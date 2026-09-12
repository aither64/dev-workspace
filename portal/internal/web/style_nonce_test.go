package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestSessionStylesUseMatchingPerResponseNonce(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.render(w, "session", pageData{Session: &session.Summary{Manifest: session.Manifest{Slug: "example"}}})
	}))
	var previous string
	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/example/", nil))
		match := regexp.MustCompile(`<meta name="style-nonce" content="([a-f0-9]{48})">`).FindStringSubmatch(response.Body.String())
		if len(match) != 2 {
			t.Fatalf("no style nonce: %s", response.Body.String())
		}
		policy := response.Header().Get("Content-Security-Policy")
		if !strings.Contains(policy, "style-src 'self' 'nonce-"+match[1]+"'") || strings.Contains(policy, "unsafe-inline") || !strings.Contains(policy, "script-src 'self'") {
			t.Fatalf("editor style nonce weakened CSP: %s", policy)
		}
		if previous == match[1] {
			t.Fatal("style nonce reused")
		}
		previous = match[1]
	}
}
