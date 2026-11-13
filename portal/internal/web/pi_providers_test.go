package web

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func piProviderPackageFixture(t *testing.T) piproviders.PackageConfiguration {
	t.Helper()
	catalog := piproviders.Catalog{Schema: 1,
		Providers: []piproviders.Provider{{ID: "a", Label: "A", NativeID: "native-a", RuntimeConfigRef: "builtin"}, {ID: "b", Label: "B", NativeID: "native-b", RuntimeConfigRef: "builtin"}, {ID: "c", Label: "Noninitial", NativeID: "native-c", RuntimeConfigRef: "builtin"}, {ID: "other", Label: "Unsupported", NativeID: "native-other", RuntimeConfigRef: "other-config"}, {ID: "daybreak", Label: "Unverified", NativeID: "native-daybreak", RuntimeConfigRef: "builtin"}},
		Accounts:  []piproviders.Account{}, Models: []piproviders.Model{}, Routes: []piproviders.RouteGate{}}
	for _, provider := range catalog.Providers {
		account := "account-" + provider.ID
		catalog.Accounts = append(catalog.Accounts, piproviders.Account{Ref: account, ProviderID: provider.ID, CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: provider.NativeID, AuthKind: "api_key"})
		models := []string{"package-model", "saved-lead", "saved-member"}
		for _, model := range models {
			catalog.Models = append(catalog.Models, piproviders.Model{Kind: "physical", ProviderID: provider.ID, ID: model, Label: model, ThinkingLevels: []string{"off", "high"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture-model"})
			state := "available"
			if provider.ID == "daybreak" {
				state = "unverified"
			}
			catalog.Routes = append(catalog.Routes, piproviders.RouteGate{ProviderID: provider.ID, AccountRef: account, ModelID: model, State: state, ReasonCode: "fixture", EvidenceRef: "fixture-route"})
		}
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err = piproviders.DecodeCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	selection := piproviders.Selection{Chain: []piproviders.Target{{ProviderID: "a", AccountRef: "account-a", ModelID: "package-model", Thinking: "high"}, {ProviderID: "b", AccountRef: "account-b", ModelID: "package-model", Thinking: "off"}}, Budget: piproviders.Budget{MaxExtraAttempts: 1, MaxHops: 1}, FallbackOn: []piproviders.FailureCategory{piproviders.Overload}}
	return piproviders.PackageConfiguration{Catalog: catalog, Default: selection}
}
func piProviderTeamFixture() (agentbackend.SessionRef, []session.PiReservedConversation, teamruntime.Preset) {
	ref := agentbackend.SessionRef{Workspace: "/fixture", Slug: "example", Incarnation: strings.Repeat("a", 64)}
	conversations := []session.PiReservedConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64)}, {Address: "implementer0", Incarnation: strings.Repeat("c", 64)}}
	preset := teamruntime.Preset{ID: "delegated", Name: "Fixture", Description: "Fixture team", Roles: []string{"lead", "implementer0"}, CatalogDigest: strings.Repeat("2", 64), TeamDigest: strings.Repeat("3", 64), LeadModel: "saved-lead", LeadEffort: "high", LeadInstructions: "Retained lead instructions.", Members: []teamruntime.MemberSpec{{Role: "implementer", Address: "implementer0", Model: "saved-member", Effort: "off", Behavior: "implementer", Access: "workspace_write", Purpose: "implementation", Instructions: "Retained member instructions."}}}
	return ref, conversations, preset
}
func piProviderServicesFixture(t *testing.T) (*PiCreationServices, piproviders.PackageConfiguration) {
	t.Helper()
	configuration := piProviderPackageFixture(t)
	services, err := newPiSelectionServices(Config{Workspace: "/fixture", UserStateRoot: "/fixture/state", PiAuthDirectory: "/fixture/auth", PiHostStateRoot: "/fixture/host", PiProviderConfig: &configuration})
	if err != nil {
		t.Fatal(err)
	}
	return services, configuration
}
func TestPiProviderCompositionUsesFullBuiltinUnionAndSavedTeam(t *testing.T) {
	services, configuration := piProviderServicesFixture(t)
	if services.available() || services.Runtime != nil || services.StartRequest != nil {
		t.Fatal("catalog composition enabled a native capability")
	}
	if services.AuthDirectory != "/fixture/auth" || services.HostStateRoot != "/fixture/host" || services.UserStateRoot != "/fixture/state" {
		t.Fatal("explicit startup paths were replaced")
	}
	want := []piruntime.AccountBinding{{Provider: "native-a", AccountRef: "account-a"}, {Provider: "native-b", AccountRef: "account-b"}, {Provider: "native-c", AccountRef: "account-c"}}
	if !reflect.DeepEqual(services.AccountBindings, want) {
		t.Fatalf("startup union: %#v", services.AccountBindings)
	}
	ref, conversations, preset := piProviderTeamFixture()
	before, _ := json.Marshal(preset)
	policy, err := services.Selection(context.Background(), ref, conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(preset)
	if string(before) != string(after) {
		t.Fatal("saved team/access/instructions changed")
	}
	if policy.Revision != 1 || policy.Default.Revision != 1 || len(policy.Overrides) != 2 || !reflect.DeepEqual(policy.Default.Selection, configuration.Default) {
		t.Fatalf("initial policy: %#v", policy)
	}
	for index, override := range policy.Overrides {
		model, thinking := preset.LeadModel, preset.LeadEffort
		if index > 0 {
			model, thinking = preset.Members[index-1].Model, preset.Members[index-1].Effort
		}
		if override.Member.Address != conversations[index].Address || override.Member.Incarnation != conversations[index].Incarnation || override.Value.Revision != 1 || override.Value.Selection.Chain[0].ModelID != model || override.Value.Selection.Chain[0].Thinking != thinking || override.Value.Selection.Chain[0].ProviderID != "a" || override.Value.Selection.Chain[0].AccountRef != "account-a" || !reflect.DeepEqual(override.Value.Selection.Chain[1:], configuration.Default.Chain[1:]) || override.Value.Selection.Budget != configuration.Default.Budget {
			t.Fatalf("saved selection changed: %#v", override)
		}
	}
	frozen, err := session.FreezePiCreationSelection(services.Catalog, policy, conversations)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.CatalogDigest != configuration.Catalog.Digest || frozen.Members[0].Settings.Model != "saved-lead" || frozen.Members[1].Settings.Model != "saved-member" {
		t.Fatal("creation freeze lost exact selections")
	}
	view, err := services.providers.publicSelection(policy, policy.Overrides[0].Member)
	if err != nil || view.Requested.ModelID != "saved-lead" || view.Effective != nil || view.Physical != nil {
		t.Fatalf("unproved public observation: %#v %v", view, err)
	}
}
func TestPiProviderCompositionRejectsWholeUnsupportedOrUnverifiedChain(t *testing.T) {
	for _, provider := range []string{"other", "daybreak"} {
		t.Run(provider, func(t *testing.T) {
			configuration := piProviderPackageFixture(t)
			configuration.Default.Chain[1] = piproviders.Target{ProviderID: provider, AccountRef: "account-" + provider, ModelID: "package-model", Thinking: "high"}
			if ValidatePiProviderConfiguration(configuration) == nil {
				t.Fatal("selected unavailable fallback was shortened/accepted")
			}
		})
	}
	services, _ := piProviderServicesFixture(t)
	ref, conversations, preset := piProviderTeamFixture()
	preset.Members[0].Model = "unknown"
	if _, err := services.Selection(context.Background(), ref, conversations, preset); err == nil {
		t.Fatal("unmapped saved model was inferred")
	}
	_, conversations, preset = piProviderTeamFixture()
	conversations[1].Incarnation = conversations[0].Incarnation
	if _, err := services.Selection(context.Background(), ref, conversations, preset); err == nil {
		t.Fatal("duplicate reserved incarnation accepted")
	}
}
func TestPiProviderPublicSelectionRequiresPolicyAndFrozenMap(t *testing.T) {
	services, _ := piProviderServicesFixture(t)
	ref, conversations, preset := piProviderTeamFixture()
	policy, err := services.Selection(context.Background(), ref, conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	member := policy.Overrides[0].Member
	// An available raw label for unsupported configuration never grants a view.
	for i := range policy.Overrides {
		policy.Overrides[i].Value.Selection.Chain[0] = piproviders.Target{ProviderID: "other", AccountRef: "account-other", ModelID: "saved-lead", Thinking: "high"}
	}
	if _, err = services.providers.publicSelection(policy, member); err == nil {
		t.Fatal("raw available route bypassed runtime support")
	}
	policy, err = services.Selection(context.Background(), ref, conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	services.providers.accounts = services.providers.accounts[:1]
	if _, err = services.providers.publicSelection(policy, member); err == nil {
		t.Fatal("selected fallback missing from frozen account map accepted")
	}
}
func TestPiProviderCompositionFreezesInputAndRetainedReplay(t *testing.T) {
	services, configuration := piProviderServicesFixture(t)
	ref, conversations, preset := piProviderTeamFixture()
	policy, err := services.Selection(context.Background(), ref, conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := session.FreezePiCreationSelection(services.Catalog, policy, conversations)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Default.Chain[0].Thinking = "off"
	configuration.Catalog.Providers[0].RuntimeConfigRef = "changed"
	again, err := services.Selection(context.Background(), ref, conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy, again) {
		t.Fatal("external config mutation changed frozen constructor")
	}
	changedDefault := copyPiPackage(services.providers.configuration)
	changedDefault.Default.Chain[0].Thinking = "off"
	next, err := newPiProviderComposition(changedDefault, ref.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = frozen.Validate(next.configuration.Catalog, ref, conversations); err != nil {
		t.Fatal(err)
	}
	if frozen.Policy.Default.Selection.Chain[0].Thinking != "high" {
		t.Fatal("retry resolved latest default")
	}
	changedCatalog := copyPiPackage(next.configuration)
	changedCatalog.Catalog.Digest = strings.Repeat("f", 64)
	if err = frozen.Validate(changedCatalog.Catalog, ref, conversations); err == nil {
		t.Fatal("stale catalog replaced retained digest")
	}
}
func TestPiProviderMissingPathsAndMalformedConfigLeaveCodexUsable(t *testing.T) {
	services, err := NewPiCreationServices(Config{})
	if err != nil || services != nil {
		t.Fatal("missing config invented Pi defaults")
	}
	configuration := piProviderPackageFixture(t)
	for _, paths := range []Config{{Workspace: "/fixture", UserStateRoot: "/state", PiHostStateRoot: "/host"}, {Workspace: "/fixture", UserStateRoot: "/state", PiHostStateRoot: "relative", PiAuthDirectory: "/auth"}} {
		paths.PiProviderConfig = &configuration
		if _, err := NewPiCreationServices(paths); !errors.Is(err, piruntime.ErrUnavailable) {
			t.Fatalf("invalid Pi paths: %v", err)
		}
	}
	s := newTestServer(t)
	s.config.Codex = &browserContractCodex{}
	codex := s.config.Codex
	config := s.config
	config.PiProviderConfig = &configuration
	config.PiAuthDirectory = "/auth"
	config.PiHostStateRoot = "/host"
	configuration.Default.Chain[0].ModelID = "unknown"
	configurePiCreation(&config)
	if config.PiCreation != nil || config.PiProviderConfig != nil || config.PiProviderConfigError == nil || config.Codex != codex {
		t.Fatal("malformed config affected Codex or returned partial Pi service")
	}
	s.config = config
	if s.piCreationAvailability() == nil {
		t.Fatal("malformed Pi configuration had no explicit refusal")
	}
}
