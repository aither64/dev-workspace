package web

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
)

func TestNewSessionSettingsBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 to run the Playwright creation settings regression")
	}
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
	httpServer := httptest.NewUnstartedServer(nil)
	server.config.BaseURL = "https://" + httpServer.Listener.Addr().String()
	httpServer.Config.Handler = server.Handler()
	httpServer.StartTLS()
	defer httpServer.Close()
	command := exec.Command("node", "creation_settings_browser_test.cjs", httpServer.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("creation settings browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
