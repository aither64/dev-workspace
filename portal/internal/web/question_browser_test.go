package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
)

// Run with PORTAL_BROWSER_TEST=1 and Nix-provided Node, Playwright modules and
// PLAYWRIGHT_BROWSERS_PATH. The browser controls API responses, while the actual
// server supplies the session template, CSP, scripts, styles and upload service.
func TestQuestionBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 to run the Playwright question regression")
	}
	server := newTestServer(t)
	defer server.Close()
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n" +
		"  thread_id: thread-1\n  socket_path: /run/dev-workspace-codex/app-server.sock\n" +
		"  client_version: 0.152.1\ncreation:\n  state: ready\n  initial_goal_sent: true\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	writeWebRuntimeAuthority(t, server, "example")
	events := make(chan struct{}, 1)
	server.config.Codex = &browserContractCodex{emptyPrompts: true, events: events, transcript: codex.Transcript{
		ThreadID: "thread-1", Status: "active", CollaborationMode: "plan",
	}}
	httpServer := httptest.NewUnstartedServer(nil)
	server.config.BaseURL = "https://" + httpServer.Listener.Addr().String()
	var err error
	server.conversation, err = conversation.NewHandler(conversation.Options{
		AllowedOrigins: []string{server.config.BaseURL}, BasePath: "/codex",
		Shutdown: server.stopping, Resolver: conversation.ResolverFunc(server.resolveConversation),
	})
	if err != nil {
		t.Fatal(err)
	}
	server.uploadHandler, err = conversation.NewUploadHandler(conversation.UploadOptions{
		BasePath: "/uploads", AllowedOrigins: []string{server.config.BaseURL}, Resolve: server.resolveUploads,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	httpServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/download" {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Disposition", `attachment; filename="fixture.txt"`)
			_, _ = w.Write([]byte("Artifact fixture\n"))
			return
		}
		if r.URL.Path == "/fixture/refresh" {
			select {
			case events <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		handler.ServeHTTP(w, r)
	})
	httpServer.StartTLS()
	defer httpServer.Close()
	for _, script := range []string{"question_browser_test.cjs", "page_lifecycle_browser_test.cjs", "presentation_browser_test.cjs", "team_settings_browser_test.cjs"} {
		t.Run(script, func(t *testing.T) {
			command := exec.Command("node", script, httpServer.URL)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", script, err, output)
			} else {
				t.Log(string(output))
			}
		})
	}
}
