package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestNewSessionSettingsBrowser(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 to run the Playwright creation settings regression")
	}
	server := creationSettingsTestServer(t)
	server.installedTeams.Catalog.Teams["development"] = agentteams.Team{
		Description: "Retained development team.", TeamDigest: strings.Repeat("d", 64),
		Roles: map[string]agentteams.Role{
			"team_lead": {Model: "gpt-5.5", Effort: "xhigh", Purpose: "lead", Instructions: "Own the work."},
		},
	}
	catalogPath := os.Getenv("DEV_WORKSPACES_CODEX_PROVIDERS")
	catalogData, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var catalog codexproviders.Catalog
	if err := json.Unmarshal(catalogData, &catalog); err != nil {
		t.Fatal(err)
	}
	alternate := catalog.Providers[1]
	alternate.ID, alternate.Label = "alternate", "Alternate"
	catalog.Providers = append(catalog.Providers, alternate)
	catalogData, err = json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, catalogData, 0600); err != nil {
		t.Fatal(err)
	}
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
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("creation settings browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	server.operationMu.Lock()
	defer server.operationMu.Unlock()
	admissions := 0
	modes := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "reload-admission ") {
			continue
		}
		var observed struct {
			RequestID string `json:"requestId"`
			ReceiptID string `json:"receiptId"`
			Mode      string `json:"mode"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "reload-admission ")), &observed); err != nil {
			t.Fatal(err)
		}
		if (observed.Mode != "ordinary" && observed.Mode != "held-catalog") || modes[observed.Mode] {
			t.Fatalf("unexpected or repeated reload case: %+v", observed)
		}
		modes[observed.Mode] = true
		record, found := server.preparations[observed.RequestID]
		if !found || record.Snapshot == nil || record.ReceiptID != observed.ReceiptID || record.RequestID != observed.RequestID {
			t.Fatalf("reload admission identity missing: %+v", observed)
		}
		input := record.Snapshot.Input
		if input.RawPrompt != "Reload effort admission: "+observed.Mode || input.Provider != "alternate" ||
			input.Team != (preparationValue{Present: true, Value: "development"}) ||
			input.Catalog != (preparationValue{Present: true, Value: strings.Repeat("a", 64)}) ||
			input.Model != (preparationValue{Present: true, Value: "gpt-5.5"}) ||
			input.Effort != (preparationValue{Present: true, Value: "high"}) {
			t.Fatalf("reload admission snapshot changed: mode=%s input=%+v", observed.Mode, input)
		}
		preset := record.Snapshot.Preset
		if preset == nil || preset.ID != "development" || preset.LeadModel != "gpt-5.5" || preset.LeadEffort != "high" {
			t.Fatalf("reload admission resolved lead changed: mode=%s preset=%+v", observed.Mode, preset)
		}
		admissions++
	}
	if admissions != 2 {
		t.Fatalf("reload admission coverage: got %d, want ordinary and held catalog", admissions)
	}
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
