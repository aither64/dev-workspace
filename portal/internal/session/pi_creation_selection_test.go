package session

import (
	"encoding/json"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"strings"
	"testing"
)

func piSelectionFixture(t *testing.T) (piproviders.Catalog, PiCreationSelection, []PiReservedConversation) {
	t.Helper()
	c := piproviders.Catalog{Schema: 1, Digest: strings.Repeat("1", 64),
		Providers: []piproviders.Provider{{ID: "provider", Label: "Provider", NativeID: "native", RuntimeConfigRef: "runtime"}, {ID: "second", Label: "Second", NativeID: "native-second", RuntimeConfigRef: "runtime-second"}},
		Accounts:  []piproviders.Account{{Ref: "one", ProviderID: "provider", CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: "native", AuthKind: "oauth"}, {Ref: "two", ProviderID: "second", CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: "native-second", AuthKind: "api_key"}},
		Models:    []piproviders.Model{{Kind: "physical", ProviderID: "provider", ID: "model", Label: "Model", ThinkingLevels: []string{"off", "high"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture"}, {Kind: "physical", ProviderID: "second", ID: "model", Label: "Model", ThinkingLevels: []string{"off", "high"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture-second"}},
		Routes:    []piproviders.RouteGate{{ProviderID: "provider", AccountRef: "one", ModelID: "model", State: "available", ReasonCode: "verified", EvidenceRef: "fixture"}, {ProviderID: "second", AccountRef: "two", ModelID: "model", State: "available", ReasonCode: "verified", EvidenceRef: "fixture"}}}
	ref := agentbackend.SessionRef{Workspace: "/fixture", Slug: "example", Incarnation: strings.Repeat("a", 64)}
	members := []PiReservedConversation{{"lead", strings.Repeat("b", 64)}, {"implementer0", strings.Repeat("c", 64)}}
	selection := piproviders.Selection{Chain: []piproviders.Target{{ProviderID: "provider", AccountRef: "one", ModelID: "model", Thinking: "high"}, {ProviderID: "second", AccountRef: "two", ModelID: "model", Thinking: "off"}}, Budget: piproviders.Budget{MaxExtraAttempts: 1, MaxHops: 1}, FallbackOn: []piproviders.FailureCategory{piproviders.RateLimit}}
	override := piproviders.Selection{Chain: []piproviders.Target{{ProviderID: "second", AccountRef: "two", ModelID: "model", Thinking: "off"}}, FallbackOn: []piproviders.FailureCategory{}}
	p := piproviders.Policy{Session: ref, Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: selection}, Overrides: []piproviders.Override{{Member: piproviders.MemberKey{Address: members[1].Address, Incarnation: members[1].Incarnation}, Value: piproviders.VersionedSelection{Revision: 1, Selection: override}}}}
	frozen, err := FreezePiCreationSelection(c, p, members)
	if err != nil {
		t.Fatal(err)
	}
	return c, frozen, members
}
func TestPiCreationSelectionFreezesExactAccountsAndIntent(t *testing.T) {
	c, s, members := piSelectionFixture(t)
	if s.Members[0].Settings.AccountRef != "one" || s.Members[1].Settings.AccountRef != "two" || s.Members[1].Source != "member_override" || len(s.Policy.Default.Selection.Chain) != 2 {
		t.Fatalf("lost frozen intent: %#v", s)
	}
	data, _ := json.Marshal(s)
	for _, secret := range []string{"auth-one", "auth-two", "credential_key", "runtime"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("snapshot includes private catalog config %s", secret)
		}
	}
	cases := map[string]func(*PiCreationSelection){
		"account":              func(s *PiCreationSelection) { s.Members[0].Settings.AccountRef = "two" },
		"incarnation":          func(s *PiCreationSelection) { s.Members[1].Incarnation = strings.Repeat("f", 64) },
		"override-incarnation": func(s *PiCreationSelection) { s.Policy.Overrides[0].Member.Incarnation = strings.Repeat("f", 64) },
		"budget":               func(s *PiCreationSelection) { s.Policy.Default.Selection.Budget.MaxExtraAttempts = 2 },
		"chain": func(s *PiCreationSelection) {
			s.Policy.Default.Selection.Chain[0], s.Policy.Default.Selection.Chain[1] = s.Policy.Default.Selection.Chain[1], s.Policy.Default.Selection.Chain[0]
		},
		"category":        func(s *PiCreationSelection) { s.Policy.Default.Selection.FallbackOn[0] = piproviders.Overload },
		"settings-digest": func(s *PiCreationSelection) { s.Members[0].Settings.PolicyRevision = strings.Repeat("f", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var copy PiCreationSelection
			if err := DecodePiCreationJSON(data, &copy, 64<<10); err != nil {
				t.Fatal(err)
			}
			mutate(&copy)
			if copy.Validate(c, copy.Policy.Public().Session, members) == nil {
				t.Fatal("changed frozen selection accepted")
			}
		})
	}
	c.Digest = strings.Repeat("2", 64)
	if s.Validate(c, s.Policy.Public().Session, members) == nil {
		t.Fatal("latest catalog replaced frozen catalog")
	}
}
func TestPiCreationSelectionStrictWireShape(t *testing.T) {
	_, s, _ := piSelectionFixture(t)
	data, _ := json.Marshal(s)
	cases := map[string]string{
		"missing-zero":   strings.Replace(string(data), `"revision":1,`, "", 1),
		"null":           strings.Replace(string(data), `"fallback_on":[]`, `"fallback_on":null`, 1),
		"case-alias":     strings.Replace(string(data), `"schema":1`, `"Schema":1`, 1),
		"unknown":        strings.Replace(string(data), `"schema":1`, `"schema":1,"extra":true`, 1),
		"duplicate":      strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1),
		"missing-budget": strings.Replace(string(data), `"max_hops":1`, `"other":1`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var out PiCreationSelection
			if DecodePiCreationJSON([]byte(raw), &out, 64<<10) == nil {
				t.Fatal("incomplete or ambiguous selection accepted")
			}
		})
	}
}
