package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

func reviewWebFixture(t *testing.T) (*Server, string, string, string) {
	t.Helper()
	s := newTestServer(t)
	bare := filepath.Join(s.config.Workspace, "repos", "project.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0755); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "init", "--bare", "--initial-branch=master", bare)
	seed := t.TempDir()
	runWebGit(t, "init", "--initial-branch=master", seed)
	runWebGit(t, "-C", seed, "config", "user.name", "Test")
	runWebGit(t, "-C", seed, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(seed, "file"), []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", seed, "add", "file")
	runWebGit(t, "-C", seed, "commit", "-m", "base")
	base := strings.TrimSpace(webGitOutput(t, "-C", seed, "rev-parse", "HEAD"))
	runWebGit(t, "--git-dir="+bare, "fetch", seed, "HEAD:refs/heads/feature")
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", base)
	worktree := filepath.Join(s.config.Workspace, "worktrees", "example", "project")
	runWebGit(t, "--git-dir="+bare, "worktree", "add", worktree, "feature")
	runWebGit(t, "-C", worktree, "config", "user.name", "Test")
	runWebGit(t, "-C", worktree, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("after\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "feature\n\nBody")
	for _, slug := range []string{"example", "other"} {
		tracking := filepath.Join(s.config.Workspace, "work", slug)
		if err := os.MkdirAll(tracking, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories:\n  - name: project\n    project: project\n    branch: feature\n    default_branch: master\n    initial_base_sha: %s\n", slug, base)
		if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(manifest), 0644); err != nil {
			t.Fatal(err)
		}
		writeWebTrackingFiles(t, tracking, "active")
	}
	s.repository.GH = "/unavailable-gh"
	return s, bare, worktree, base
}
func reviewRequest(t *testing.T, s *Server, method, path, body string, want int) map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", s.config.BaseURL)
	s.Handler().ServeHTTP(w, req)
	if w.Code != want {
		t.Fatalf("%s %s = %d: %s", method, path, w.Code, w.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func reviewString(t *testing.T, value json.RawMessage) string {
	t.Helper()
	var result string
	if err := json.Unmarshal(value, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestRepositoryReviewHTTPImmutablePairAndOpaqueIdentities(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + repository.ReviewID("project")
	history := reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	snapshot := reviewString(t, history["snapshot"])
	var page repository.ReviewHistory
	if err := json.Unmarshal(history["history"], &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Commits) != 1 || page.Commits[0].Body != "Body" {
		t.Fatalf("local unpushed history = %#v", page)
	}
	var pair repository.ReviewPair
	if err := json.Unmarshal(history["pair"], &pair); err != nil {
		t.Fatal(err)
	}
	// A local edit and new commit must not silently change an already-open view.
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("newest\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "next")
	state := reviewRequest(t, s, "GET", endpoint+"state"+query, "", 200)
	if reviewString(t, state["head"]) == pair.Head {
		t.Fatal("head change is not observable")
	}
	comparison := reviewRequest(t, s, "POST", endpoint+"comparison"+query, fmt.Sprintf(`{"snapshot":%q}`, snapshot), 200)
	var files []repository.ReviewFile
	if err := json.Unmarshal(comparison["files"], &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %#v", files)
	}
	content := reviewRequest(t, s, "GET", endpoint+"file"+query+"&snapshot="+snapshot+"&file="+files[0].ID, "", 200)
	var after repository.ReviewBlob
	if err := json.Unmarshal(content["after"], &after); err != nil {
		t.Fatal(err)
	}
	if after.Text != "after\n" {
		t.Fatalf("immutable content = %q", after.Text)
	}
	reviewRequest(t, s, "GET", endpoint+"file"+query+"&snapshot="+snapshot+"&file=../../file", "", 404)
	reviewRequest(t, s, "POST", endpoint+"comparison"+query, fmt.Sprintf(`{"snapshot":%q,"commit":"not-issued"}`, snapshot), 400)
	reviewRequest(t, s, "GET", endpoint+"history?repository=../../project", "", 404)
	reviewRequest(t, s, "GET", endpoint+"comparison"+query, "", 405)
	reviewRequest(t, s, "GET", "/api/sessions/other/repository-history"+query+"&snapshot="+snapshot, "", 409)
	commit := reviewRequest(t, s, "POST", endpoint+"comparison"+query, fmt.Sprintf(`{"snapshot":%q,"commit":%q}`, snapshot, page.Commits[0].ID), 200)
	if reviewString(t, commit["snapshot"]) == snapshot {
		t.Fatal("commit comparison overwrote branch snapshot")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", endpoint+"comparison"+query, strings.NewReader(`{}`)))
	if w.Code != 403 {
		t.Fatalf("comparison write without origin = %d", w.Code)
	}
}
func TestRepositoryReviewSavedHistorySurvivesRestartAndIntegration(t *testing.T) {
	s, bare, _, base := reviewWebFixture(t)
	endpoint := "/api/sessions/example/repository-history?repository=" + repository.ReviewID("project")
	history := reviewRequest(t, s, "GET", endpoint, "", 200)
	var pair repository.ReviewPair
	if err := json.Unmarshal(history["pair"], &pair); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", pair.Head)
	fresh, err := New(s.config)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	saved := reviewRequest(t, fresh, "GET", endpoint, "", 200)
	var restored repository.ReviewPair
	if err := json.Unmarshal(saved["pair"], &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Base != base || restored.Head != pair.Head || !strings.Contains(restored.BaseLabel, "Last viewed") {
		t.Fatalf("saved pair = %#v", restored)
	}
	file := fresh.reviews().comparisonPath("example", repository.ReviewID("project"), pair.Head)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("comparison mode = %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(s.config.Workspace, "work", "example", "repository-comparisons.json")); !os.IsNotExist(err) {
		t.Fatal("comparison state leaked into session tracking")
	}
	if err := os.WriteFile(file, []byte(`{"schema":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	reviewRequest(t, fresh, "GET", endpoint, "", 422)
}
