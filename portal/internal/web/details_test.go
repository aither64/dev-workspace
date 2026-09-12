package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionDetailsDiscoversLateWorktreesAndCuratedArtifacts(t *testing.T) {
	server := newTestServer(t)
	tracking := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(tracking, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\n"
	writeManifest := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(manifest)
	writeWebTrackingFiles(t, tracking, "active")
	expireDiscovery := func() {
		server.reviews().discoveryMu.Lock()
		server.reviews().discoveryAt = time.Time{}
		server.reviews().discoveryMu.Unlock()
	}
	// Details must work before a session has a Codex thread.
	readDetails := func() map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/example/details", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("details = %d: %s", response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("session details can be cached by the browser")
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	before := readDetails()
	if before["repositoryCount"] != float64(0) || before["artifactCount"] != float64(2) {
		t.Fatalf("initial details = %#v", before)
	}

	repository := filepath.Join(server.config.Workspace, "repos", "late.git")
	if err := os.MkdirAll(filepath.Dir(repository), 0o755); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "init", "--bare", "--initial-branch=master", repository)
	runWebGit(t, "--git-dir="+repository, "config", "remote.origin.url", "git@github.com:example-org/late.git")
	seed := t.TempDir()
	runWebGit(t, "init", "--initial-branch=master", seed)
	runWebGit(t, "-C", seed, "config", "user.email", "test@example.invalid")
	runWebGit(t, "-C", seed, "config", "user.name", "Test")
	runWebGit(t, "-C", seed, "commit", "--allow-empty", "-m", "seed")
	head := strings.TrimSpace(webGitOutput(t, "-C", seed, "rev-parse", "HEAD"))
	runWebGit(t, "--git-dir="+repository, "fetch", seed, head+":refs/heads/example")
	worktree := filepath.Join(server.config.Workspace, "worktrees", "example", "late")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "--git-dir="+repository, "worktree", "add", worktree, "example")
	server.repository.GH = filepath.Join(t.TempDir(), "unavailable-gh")
	writeManifest(manifest + "artifacts:\n  - label: Late report\n    path: report.md\n")
	for _, file := range []string{"report.md", "private.md"} {
		if err := os.WriteFile(filepath.Join(tracking, file), []byte("report\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// New worktrees become visible after the shared five-second discovery cache
	// expires. Advance it explicitly instead of making this test sleep.
	expireDiscovery()
	after := readDetails()
	if after["repositoryCount"] != float64(1) || after["artifactCount"] != float64(3) {
		t.Fatalf("refreshed details = %#v", after)
	}
	if html := after["repositoriesHTML"].(string); !strings.Contains(html, "<h3>late</h3>") || !strings.Contains(html, "<code>example</code>") {
		t.Fatalf("new worktree missing from repository section: %s", html)
	}
	if html := after["artifactsHTML"].(string); !strings.Contains(html, `data-artifact-path="report.md"`) || strings.Contains(html, "private.md") {
		t.Fatalf("curated artifact section = %s", html)
	}
	// An unrelated broken canonical repository must not hide verified worktrees
	// or prevent independently curated artifacts from being refreshed.
	if err := os.MkdirAll(filepath.Join(server.config.Workspace, "repos", "broken.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	expireDiscovery()
	partial := readDetails()
	if partial["repositoryCount"] != float64(1) || partial["artifactCount"] != float64(3) ||
		!strings.Contains(partial["repositoriesHTML"].(string), "Some live worktrees could not be verified") ||
		!strings.Contains(partial["repositoriesHTML"].(string), "<h3>late</h3>") {
		t.Fatalf("partial discovery lost available details: %#v", partial)
	}

	// Removing a curated entry is visible even while repository discovery warns.
	writeManifest(manifest)
	if result := readDetails(); result["artifactCount"] != float64(2) || strings.Contains(result["artifactsHTML"].(string), "report.md") {
		t.Fatalf("removed artifact still offered: %#v", result)
	}
}

func TestSessionDetailsMissingSession(t *testing.T) {
	response := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/missing/details", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing session details = %d", response.Code)
	}
}
