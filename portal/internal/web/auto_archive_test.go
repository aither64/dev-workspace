package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestAutoArchiveStatusAndHoldValidateSessionAndOrigin(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"portal.yml": "schema: 1\nslug: example\n",
		"plan.md":    "# Plan\n", "state.md": "---\nlifecycle: active\n---\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := session.Find(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	target, err := lifecycleTargetIdentity(summary)
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "dev-session")
	script := `#!/bin/sh
test "$1" = auto-archive || exit 4
test "$3" = example || exit 5
test "$4" = --as-is || exit 6
test "$5" = --json || exit 7
case "$2" in
  status) printf '%s\n' '{"slug":"example","enabled":true,"hold":false}' ;;
  hold) printf '%s\n' '{"slug":"example","hold":true}' ;;
  release) printf '%s\n' '{"slug":"example","hold":false}' ;;
  *) exit 8 ;;
esac
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = command
	for _, test := range []struct {
		method, payload, origin string
		code                    int
	}{
		{http.MethodGet, "", "", 200},
		{http.MethodPost, `{"hold":true,"targetId":"` + target + `"}`, server.config.BaseURL, 200},
		{http.MethodPost, `{"hold":false,"targetId":"` + target + `"}`, server.config.BaseURL, 200},
		{http.MethodPost, `{"hold":true,"targetId":"stale"}`, server.config.BaseURL, 409},
		{http.MethodPost, `{"targetId":"` + target + `"}`, server.config.BaseURL, 400},
		{http.MethodPost, `{"hold":true,"targetId":"` + target + `"}`, "https://foreign.example", 403},
	} {
		request := httptest.NewRequest(test.method, "/api/sessions/example/auto-archive", strings.NewReader(test.payload))
		request.Header.Set("Origin", test.origin)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != test.code {
			t.Fatalf("%s %s = %d: %s", test.method, test.payload, response.Code, response.Body.String())
		}
		if test.code == 200 && !strings.Contains(response.Body.String(), `"slug":"example"`) {
			t.Fatalf("invalid result: %s", response.Body.String())
		}
	}
}
