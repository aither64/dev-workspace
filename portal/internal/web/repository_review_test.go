package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if restored.Base != base || restored.Head != pair.Head || !strings.Contains(restored.BaseLabel, "Saved") {
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

func TestStatusObservationPreservesComparisonWithoutOpeningHistory(t *testing.T) {
	s, bare, worktree, base := reviewWebFixture(t)
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + repository.ReviewID("project")
	reviewRequest(t, s, "GET", "/api/sessions/example/details", "", 200)
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", head)
	history := reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	var pair repository.ReviewPair
	if err := json.Unmarshal(history["pair"], &pair); err != nil {
		t.Fatal(err)
	}
	if pair.Base != base || pair.Head != head || pair.Warning != "" {
		t.Fatalf("observed pair: %#v", pair)
	}
}

func TestCaptureComparisonRecoversMergedHeadAndRejectsWrongIdentity(t *testing.T) {
	s, bare, worktree, base := reviewWebFixture(t)
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", head)
	capture := func(base, head string) (repository.ReviewPair, error) {
		return CaptureRepositoryComparison(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", base, head)
	}
	if _, err := capture("", ""); err == nil {
		t.Fatal("guessed historical base")
	}
	if _, err := capture(head, base); err == nil {
		t.Fatal("accepted a different registered head")
	}
	if _, err := capture("missing", head); err == nil {
		t.Fatal("accepted an invalid base")
	}
	pair, err := capture(base, head)
	if err != nil {
		t.Fatal(err)
	}
	// A stale browser fallback may finish after the explicit capture.
	if err := s.reviews().saveComparison(context.Background(), "example", repository.ReviewID("project"), repository.ReviewPair{Base: base, Head: head, Warning: "fallback", BaseLabel: "fallback"}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.reviews().loadComparison("example", repository.ReviewID("project"), head)
	if err != nil || saved == nil || *saved != pair {
		t.Fatalf("capture overwritten: %#v %v", saved, err)
	}
	history := reviewRequest(t, s, "GET", "/api/sessions/example/repository-history?repository="+repository.ReviewID("project"), "", 200)
	if err := json.Unmarshal(history["pair"], &pair); err != nil {
		t.Fatal(err)
	}
	if pair.Base != base || pair.Head != head || pair.Warning != "" {
		t.Fatalf("recovered: %#v", pair)
	}
}

func TestComparisonCaptureKeepsFirstExactPairAndWaitsForConcurrentWriter(t *testing.T) {
	s, _, worktree, base := reviewWebFixture(t)
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	service := s.reviews()
	repo := repository.ReviewID("project")
	pair := repository.ReviewPair{Base: base, Head: head, BaseLabel: "Original exact capture"}
	if err := service.saveComparison(context.Background(), "example", repo, pair); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(service.comparisonPath("example", repo, head)+".lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := service.saveComparison(ctx, "example", repo, pair); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended save: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- service.saveComparison(context.Background(), "example", repo, pair) }()
	unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer did not resume")
	}
	relabeled := pair
	relabeled.BaseLabel = "Later label"
	if err := service.saveComparison(context.Background(), "example", repo, relabeled); err != nil {
		t.Fatal(err)
	}
	conflict := pair
	conflict.Base = strings.Repeat("1", 40)
	if err := service.saveComparison(context.Background(), "example", repo, conflict); err == nil {
		t.Fatal("replaced an exact pair")
	}
	saved, err := service.loadComparison("example", repo, head)
	if err != nil || saved == nil || *saved != pair {
		t.Fatalf("changed exact capture: %#v %v", saved, err)
	}
}

func TestComparisonObservationFailureDoesNotHideRepositoryStatus(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	service := s.reviews()
	if err := os.MkdirAll(filepath.Dir(service.directory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.directory, []byte("unavailable store"), 0600); err != nil {
		t.Fatal(err)
	}
	details := reviewRequest(t, s, "GET", "/api/sessions/example/details", "", 200)
	if !strings.Contains(string(details["repositoriesHTML"]), "project") {
		t.Fatal("repository status disappeared")
	}
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-history?repository="+repository.ReviewID("project"), "", 422)
	// The readable state endpoint is independent of comparison persistence.
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-state?repository="+repository.ReviewID("project"), "", 200)
}
