package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPiRuntimeUpgradeRouteRefusesClientAuthorityBeforeRuntime(t *testing.T) {
	s := newTestServer(t)
	data, err := json.Marshal(map[string]string{"workspace": s.config.Workspace,
		"requestId": "77777777-7777-4777-8777-777777777777", "expectedGeneration": strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, tc := range []struct {
		name, method, origin, contentType, query, body string
		status                                         int
	}{
		{"read", "GET", s.config.BaseURL, "application/json", "", body, http.StatusMethodNotAllowed},
		{"missing-origin", "POST", "", "application/json", "", body, http.StatusForbidden},
		{"foreign-origin", "POST", "https://foreign.invalid", "application/json", "", body, http.StatusForbidden},
		{"media", "POST", s.config.BaseURL, "text/plain", "", body, http.StatusBadRequest},
		{"target", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"appStorePath":"/client-target"}`, http.StatusBadRequest},
		{"duplicate", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"requestId":"77777777-7777-4777-8777-777777777777"}`, http.StatusBadRequest},
		{"compact-uuid", "POST", s.config.BaseURL, "application/json", "", strings.ReplaceAll(body, "77777777-7777-4777-8777-777777777777", "77777777777747778777777777777777"), http.StatusBadRequest},
		{"generation", "POST", s.config.BaseURL, "application/json", "", strings.ReplaceAll(body, strings.Repeat("a", 64), "unknown"), http.StatusBadRequest},
		{"workspace", "POST", s.config.BaseURL, "application/json", "", strings.ReplaceAll(body, s.config.Workspace, "/foreign"), http.StatusBadRequest},
		{"query", "POST", s.config.BaseURL, "application/json", "?target=other", body, http.StatusBadRequest},
		{"partial-retirement", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"retireUnknownInput":"77777777-7777-4777-8777-777777777777"}`, http.StatusBadRequest},
		{"wrong-retirement-member", "POST", s.config.BaseURL, "application/json", "", strings.TrimSuffix(body, "}") + `,"retireUnknownInput":"77777777-7777-4777-8777-777777777777","inputDigest":"` + strings.Repeat("b", 64) + `","member":"../other"}`, http.StatusBadRequest},
		{"unavailable-runtime", "POST", s.config.BaseURL, "application/json", "", body, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, s.config.BaseURL+"/api/sessions/example/pi-runtime-upgrade"+tc.query, strings.NewReader(tc.body))
			request.Header.Set("Origin", tc.origin)
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			s.sessionAPI(response, request)
			if response.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
		})
	}
}
