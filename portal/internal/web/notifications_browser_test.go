package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Real templates/CSP/worker route, with synthetic subscription and delivery.
// This proves plumbing, not provider acceptance or an operating-system display.
func TestNotificationsBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 for real browser notification plumbing")
	}
	n := testNotifications(t)
	prepareInteractiveConversation(t, n.server, "example")
	fixture := httptest.NewUnstartedServer(nil)
	n.server.config.BaseURL = "https://" + fixture.Listener.Addr().String()
	n.server.trustedOrigins = []string{n.server.config.BaseURL}
	summary, err := session.Find(n.server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := session.ObservationIdentity(n.server.config.Workspace, "example", filepath.Join(n.server.config.Workspace, "work", "example"), summary.Codex.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	event := attentionEvent{Version: 1, ID: strings.Repeat("b", 64), Identity: identity, Revision: 1, Slug: "example", Kind: "input", Member: "implementer0", Route: "/example/?member=implementer0#codex", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	handler := n.server.Handler()
	var workerStage atomic.Int32
	workerStage.Store(2)
	fixture.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/fixture/worker-stage/") {
			if r.Method != http.MethodPost || r.Header.Get("Origin") != n.server.config.BaseURL {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			switch r.URL.Path {
			case "/fixture/worker-stage/old":
				workerStage.Store(0)
			case "/fixture/worker-stage/waiting":
				workerStage.Store(1)
			case "/fixture/worker-stage/current":
				workerStage.Store(2)
			default:
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n.server.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		if r.URL.Path == "/notification-worker.js" && workerStage.Load() < 2 {
			// Fixture predecessors deliberately have neither skipWaiting nor an
			// activation-message handler. Only the real successor can take over.
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Service-Worker-Allowed", "/")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(`self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));` +
				strings.Repeat("\n// predecessor", int(workerStage.Load())+1)))
			return
		}
		if r.URL.Path == "/fixture/subscription" {
			n.server.writeJSON(w, http.StatusOK, testPushSubscription(t))
			return
		}
		if r.URL.Path == "/api/sessions/example/attention" && r.Method == http.MethodGet {
			n.server.writeJSON(w, http.StatusOK, map[string]any{"version": 1, "identity": identity, "rootThread": "thread-1", "enabled": true, "fresh": true, "event": event, "revision": 1})
			return
		}
		if r.URL.Path == "/api/sessions/example/coordination" {
			n.server.writeJSON(w, http.StatusOK, map[string]any{"reservationHolders": []any{}})
			return
		}
		handler.ServeHTTP(w, r)
	})
	fixture.StartTLS()
	defer fixture.Close()
	command := exec.Command("node", "notifications_browser_test.cjs", fixture.URL)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("notification browser plumbing: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
