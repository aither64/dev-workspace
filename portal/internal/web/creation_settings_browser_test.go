package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestNewSessionSettingsBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 to run the Playwright creation settings regression")
	}
	server := creationSettingsTestServer(t)
	httpServer := httptest.NewUnstartedServer(nil)
	server.config.BaseURL = "https://" + httpServer.Listener.Addr().String()
	server.trustedOrigins = []string{server.config.BaseURL}
	handler := server.Handler()
	httpServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/session" {
			server.render(w, "session", pageData{Session: &session.Summary{
				Manifest: session.Manifest{Slug: "fixture", Sensitive: r.URL.Query().Get("sensitive") == "true"},
				Archived: r.URL.Query().Get("archived") == "true",
			}})
			return
		}
		handler.ServeHTTP(w, r)
	})
	httpServer.StartTLS()
	defer httpServer.Close()
	command := exec.Command("node", "creation_settings_browser_test.cjs", httpServer.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("creation settings browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	server.operationMu.Lock()
	defer server.operationMu.Unlock()
	for _, provider := range []string{"", "pool", "openai"} {
		label := provider
		if label == "" {
			label = "inherited"
		}
		found := false
		for _, record := range server.preparations {
			if record.Snapshot != nil && record.Snapshot.Input.RawPrompt == "Unmocked provider admission: "+label && record.Snapshot.Input.Provider == provider {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("browser provider %q was not durably admitted", provider)
		}
	}

}

func creationSettingsTestServer(t *testing.T) *Server {
	server := newTestServer(t)
	server.installedTeams = &agentteams.Installed{Managed: true, Catalog: &agentteams.Catalog{
		CatalogDigest: strings.Repeat("a", 64), DefaultTeam: "lead_reviewed",
		Teams: map[string]agentteams.Team{
			"lead_reviewed": {Description: "Lead implements and reviewer reviews.", TeamDigest: strings.Repeat("b", 64), Roles: map[string]agentteams.Role{
				"team_lead": {Model: "model-lead", Effort: "xhigh", Purpose: "lead", Instructions: "Design and implement the work."},
				"reviewer":  {Model: "model-review", Effort: "xhigh", Behavior: "reviewer", Purpose: "review", Access: "read_only", Instructions: "Review the work."},
			}},
			"solo": {Description: "A single lead.", TeamDigest: strings.Repeat("c", 64), Roles: map[string]agentteams.Role{
				"team_lead": {Model: "model-solo", Effort: "high", Purpose: "lead", Instructions: "Own the work."},
			}},
		},
	}}
	providerTestCatalog(t, server, "http://127.0.0.1:1")
	// Admission is real; naming is held so no native session is created.
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return server
}
