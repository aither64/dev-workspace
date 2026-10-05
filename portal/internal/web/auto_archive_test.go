package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestWorkspaceAutoArchiveReadsCachedEnvelopeWithoutScan(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	command := filepath.Join(t.TempDir(), "dev-session")
	payload, err := json.Marshal(map[string]any{"schema": 1, "workspace": server.config.Workspace,
		"policy":   map[string]any{"enabled": false, "epoch": nil},
		"sessions": []any{map[string]any{"slug": "malformed", "repair_needed": true}, map[string]any{"slug": "one", "activity_known": true}},
		"counts":   map[string]any{"total": 2}})
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$*\" = 'auto-archive status --json' ] || exit 7\nprintf '%s\\n' '" + strings.ReplaceAll(string(payload), "'", "'\"'\"'") + "'\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = command
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/auto-archive", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"repair_needed":true`) || !strings.Contains(response.Body.String(), `"slug":"one"`) {
		t.Fatalf("workspace status=%d: %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/api/auto-archive", strings.NewReader(`{}`))
	request.Header.Set("Origin", server.config.BaseURL)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != 405 {
		t.Fatalf("workspace mutation=%d", response.Code)
	}
}

func TestAutoArchiveStatusAndHoldValidateSessionAndOrigin(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
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
`
	for _, action := range []string{"status", "hold", "release"} {
		payload, err := json.Marshal(map[string]any{"schema": 1, "workspace": server.config.Workspace,
			"slug": "example", "enabled": true, "hold": action == "hold"})
		if err != nil {
			t.Fatal(err)
		}
		script += "  " + action + ") printf '%s\\n' '" + strings.ReplaceAll(string(payload), "'", "'\"'\"'") + "' ;;\n"
	}
	script += "  *) exit 8 ;;\nesac\n"
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
	for name, payload := range map[string]map[string]any{
		"missing_schema":    {"workspace": server.config.Workspace, "slug": "example"},
		"wrong_schema":      {"schema": 2, "workspace": server.config.Workspace, "slug": "example"},
		"missing_workspace": {"schema": 1, "slug": "example"},
		"wrong_workspace":   {"schema": 1, "workspace": "/foreign/workspace", "slug": "example"},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' '"+strings.ReplaceAll(string(encoded), "'", "'\"'\"'")+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, body := range []string{"", `{"hold":true,"targetId":"` + target + `"}`, `{"hold":false,"targetId":"` + target + `"}`} {
				method := http.MethodPost
				if body == "" {
					method = http.MethodGet
				}
				request := httptest.NewRequest(method, "/api/sessions/example/auto-archive", strings.NewReader(body))
				request.Header.Set("Origin", server.config.BaseURL)
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusBadGateway {
					t.Fatalf("%s %s accepted invalid envelope: %d: %s", method, body, response.Code, response.Body.String())
				}
			}
		})
	}
}
