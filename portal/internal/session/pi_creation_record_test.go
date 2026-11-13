package session

import (
	"encoding/json"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"strings"
	"testing"
)

func piIntentFixture(t *testing.T) (piproviders.Catalog, PiCreationIntent) {
	t.Helper()
	c, s, members := piSelectionFixture(t)
	intent := PiCreationIntent{Schema: 4, Workspace: s.Policy.Session.Workspace, Slug: s.Policy.Session.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: strings.Repeat("d", 64), DeletionHistorySHA256: strings.Repeat("e", 64), SessionIncarnation: s.Policy.Session.Incarnation, Conversations: members, GoalSHA256: strings.Repeat("f", 64), Model: "model", Effort: "high", Provider: "native", ProviderSelection: s, InitialInputID: "11111111-1111-4111-8111-111111111111", Sensitive: false,
		DirectTeam: PiCreationTeam{ID: "delegated", Name: "Team", Description: "Fixture team", Roles: []string{"lead", "implementer0"}, CatalogDigest: strings.Repeat("2", 64), TeamDigest: strings.Repeat("3", 64), LeadModel: "model", LeadEffort: "high", Members: []PiCreationTeamMember{{Role: "implementer", Address: "implementer0", Model: "model", Effort: "off", Behavior: "implementer", Access: "workspace_write"}}}}
	if err := intent.Validate(c); err != nil {
		t.Fatal(err)
	}
	return c, intent
}
func TestPiCreationJournalStrictIntentAndNativeBoundaries(t *testing.T) {
	_, intent := piIntentFixture(t)
	creating := PiCreationJournal{PiCreationIntent: intent, State: "creating"}
	data, _ := json.Marshal(creating)
	journal, err := DecodePiCreationJournal(data)
	if err != nil {
		t.Fatal(err)
	}
	if journal.NativeCreateStarted || journal.Native != nil {
		t.Fatal("fresh intent gained native proof")
	}
	cases := map[string]func(map[string]any){
		"missing-false":        func(v map[string]any) { delete(v, "native_create_started") },
		"missing-sensitivity":  func(v map[string]any) { delete(v, "sensitive") },
		"null-native":          func(v map[string]any) { v["native"] = nil },
		"premature-generation": func(v map[string]any) { v["native_generation"] = "" },
		"premature-result":     func(v map[string]any) { v["native"] = map[string]any{"native_id": "id", "history_ref": "history"} },
		"lost-generation":      func(v map[string]any) { v["native_create_started"] = true },
		"forged-ready":         func(v map[string]any) { v["state"] = "ready" },
		"unsupported-backend":  func(v map[string]any) { v["backend"] = "codex" },
		"legacy-field":         func(v map[string]any) { v["run_codex"] = false },
		"changed-preset":       func(v map[string]any) { v["direct_team"].(map[string]any)["leadEffort"] = "off" },
		"lost-selection":       func(v map[string]any) { delete(v, "provider_selection") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			json.Unmarshal(data, &v)
			mutate(v)
			bad, _ := json.Marshal(v)
			if _, err := DecodePiCreationJournal(bad); err == nil {
				t.Fatal("invalid creation evidence accepted")
			}
		})
	}
	creating.NativeCreateStarted = true
	creating.NativeGeneration = strings.Repeat("4", 64)
	data, _ = json.Marshal(creating)
	if _, err := DecodePiCreationJournal(data); err != nil {
		t.Fatalf("Unknown create intent must remain readable: %v", err)
	}
	creating.Native = &PiCreationNative{NativeID: "native-id", HistoryRef: "opaque-locator"}
	creating.State = "ready"
	data, _ = json.Marshal(creating)
	if _, err := DecodePiCreationJournal(data); err != nil {
		t.Fatal(err)
	}
	// Shape is not readiness proof: completion additionally requires P3's exact
	// native history and original Accepted input witness at host composition.
	binding := intent.Binding()
	if err := intent.ValidateBinding(binding); err != nil {
		t.Fatal(err)
	}
	binding.Conversations[1].Incarnation = strings.Repeat("8", 64)
	if intent.ValidateBinding(binding) == nil {
		t.Fatal("changed incarnation accepted")
	}
}
