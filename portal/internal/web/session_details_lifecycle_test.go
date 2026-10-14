package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestSessionDetailsVerifiedPendingLifecycle(t *testing.T) {
	for _, name := range []string{"absent", "pending", "unsafe", "conflicting"} {
		t.Run(name, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if name != "absent" {
				mode := os.FileMode(0o600)
				if name == "unsafe" {
					mode = 0o644
				}
				if err := os.WriteFile(filepath.Join(root, "example.archive.json"), []byte("{}"), mode); err != nil {
					t.Fatal(err)
				}
			}
			if name == "conflicting" {
				if err := os.WriteFile(filepath.Join(root, "example.revive.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			summary := &session.Summary{Manifest: session.Manifest{Slug: "example"}, Archived: true}
			server.sessionDetails(response, httptest.NewRequest("GET", "/api/sessions/example/details", nil), summary)
			if name == "unsafe" || name == "conflicting" {
				if response.Code != 503 {
					t.Fatalf("unverified lifecycle returned %d: %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code != 200 {
				t.Fatalf("details returned %d: %s", response.Code, response.Body.String())
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			var pending string
			if err := json.Unmarshal(payload["pendingLifecycle"], &pending); err != nil {
				t.Fatalf("missing verified pendingLifecycle: %v", err)
			}
			want := ""
			if name == "pending" {
				want = "archive"
			}
			if pending != want {
				t.Fatalf("pendingLifecycle = %q, want %q", pending, want)
			}
		})
	}
}
