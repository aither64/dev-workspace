package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveFailurePage(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.config.VerifyThread = func(_ context.Context, id, cwd string) error {
		if id != "thread-1" || cwd != filepath.Join(server.config.Workspace, "work", "example") {
			return fmt.Errorf("unexpected fixture conversation %q in %q", id, cwd)
		}
		return nil
	}
	directory := filepath.Join(server.config.Workspace, "archive", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\nfinalized_at: '2026-09-16T13:02:05Z'\ncodex:\n  thread_id: thread-1\n  socket_path: /run/dev-workspace-codex/app-server.sock\n  client_version: 0.152.1\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "complete")
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"schema":2,"slug":"example","workspace":%q,"phase":"tracking_committed","mode":"complete","operation_id":%q}`, server.config.Workspace, strings.Repeat("a", 64))
	if err := os.WriteFile(filepath.Join(root, "example.archive.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/example/", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `data-thread-id="thread-1"`) || !strings.Contains(response.Body.String(), "Archiving is unfinished.") || strings.Contains(response.Body.String(), `id="message-form"`) {
		t.Fatalf("pending archive page = %d: %s", response.Code, response.Body.String())
	}
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		return
	}
	httpServer := httptest.NewTLSServer(server.Handler())
	defer httpServer.Close()
	command := exec.Command("node", "archive_failure_browser_test.cjs", httpServer.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
