package web

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piCatalogInitialFixture struct {
	*piCreationRuntimeFixture
	initial  piruntime.StartRequest
	original piruntime.RuntimeRef
	err      error
	reads    int
}

func (f *piCatalogInitialFixture) ReadInitialStart(_ context.Context, ref agentbackend.SessionRef, id string) (piruntime.StartRequest, piruntime.RuntimeRef, error) {
	f.reads++
	if ref != f.initial.Session || id != f.initial.RequestID {
		return piruntime.StartRequest{}, piruntime.RuntimeRef{}, piruntime.ErrScope
	}
	return f.initial, f.original, f.err
}

func TestPiReadyCatalogFrozenPackageProvenance(t *testing.T) {
	aRoot, bRoot := os.Getenv("PI_CATALOG_OLD"), os.Getenv("PI_CATALOG_NEW")
	if aRoot == "" || bRoot == "" {
		t.Skip("explicit immutable catalog fixture roots required")
	}
	a, err := piproviders.LoadInstalled(aRoot)
	if err != nil {
		t.Fatal(err)
	}
	b, err := piproviders.LoadInstalled(bRoot)
	if err != nil {
		t.Fatal(err)
	}
	ref := agentbackend.SessionRef{Workspace: "/fixture", Slug: "example", Incarnation: strings.Repeat("a", 64)}
	members := []session.PiReservedConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64)}}
	policy := piproviders.Policy{Session: ref, Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: a.Default}, Overrides: []piproviders.Override{}}
	policy.Default.Selection.FallbackOn = []piproviders.FailureCategory{}
	frozen, err := session.FreezePiCreationSelection(a.Catalog, policy, members)
	if err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	intent := session.PiCreationIntent{Schema: 4, Workspace: ref.Workspace, Slug: ref.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: strings.Repeat("c", 64), DeletionHistorySHA256: strings.Repeat("d", 64), SessionIncarnation: ref.Incarnation, Conversations: members, GoalSHA256: strings.Repeat("e", 64), Model: "p3-model", Effort: "high", Provider: "p3-fake", ProviderSelection: frozen, InitialInputID: id,
		DirectTeam: session.PiCreationTeam{ID: "solo", Name: "Solo", Description: "Fixture", Roles: []string{"lead"}, CatalogDigest: strings.Repeat("f", 64), TeamDigest: strings.Repeat("1", 64), LeadModel: "p3-model", LeadEffort: "high", Members: []session.PiCreationTeamMember{}}}
	if err := intent.Validate(a.Catalog); err != nil {
		t.Fatal(err)
	}
	journal := session.PiCreationJournal{PiCreationIntent: intent, State: "ready", NativeCreateStarted: true, NativeGeneration: strings.Repeat("2", 64), Native: &session.PiCreationNative{NativeID: "original", HistoryRef: strings.Repeat("3", 64)}}
	start := piruntime.StartRequest{Session: ref, BindingRevision: 1, RequestID: id, AppStorePath: aRoot}
	runtime := piruntime.RuntimeRef{Session: ref, RequestID: id, Generation: journal.NativeGeneration}
	for _, kind := range []string{"compatible", "same-catalog", "wrong-request", "foreign-session", "wrong-generation", "successor", "missing-ancestor", "missing-package", "malformed-package", "removed-input", "account", "route", "settings", "endpoint", "sampling", "missing-models", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			reader := &piCatalogInitialFixture{piCreationRuntimeFixture: &piCreationRuntimeFixture{}, initial: start, original: runtime}
			composition, err := newPiProviderComposition(b, ref.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{config: Config{PackageRoot: bRoot, PiCreation: &PiCreationServices{Catalog: b.Catalog, Runtime: reader, providers: composition}}}
			j := journal
			switch kind {
			case "same-catalog":
				s.config.PiCreation.Catalog = a.Catalog
			case "wrong-request":
				reader.original.RequestID = "22222222-2222-4222-8222-222222222222"
			case "foreign-session":
				reader.original.Session.Incarnation = strings.Repeat("4", 64)
			case "wrong-generation":
				reader.original.Generation = strings.Repeat("4", 64)
			case "successor":
				reader.initial.ReplacesRequestID = id
			case "missing-ancestor":
				reader.err = piruntime.ErrConflict
			case "missing-package":
				reader.initial.AppStorePath = filepath.Join(t.TempDir(), "missing")
			case "malformed-package":
				reader.initial.AppStorePath = t.TempDir()
			case "incomplete":
				j.State = "creating"
			case "removed-input", "account", "route", "settings", "endpoint", "sampling", "missing-models":
				root := t.TempDir()
				dir := filepath.Join(root, "share/dev-workspace")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				provider, _ := os.ReadFile(filepath.Join(bRoot, "share/dev-workspace/pi-providers.json"))
				models, _ := os.ReadFile(filepath.Join(bRoot, "share/dev-workspace/pi-models.json"))
				originalModels := append([]byte{}, models...)
				if kind == "account" || kind == "route" || kind == "settings" {
					var configuration map[string]any
					if err := json.Unmarshal(provider, &configuration); err != nil {
						t.Fatal(err)
					}
					catalog := configuration["catalog"].(map[string]any)
					switch kind {
					case "account":
						catalog["accounts"].([]any)[0].(map[string]any)["ref"] = "different-account"
						catalog["routes"].([]any)[0].(map[string]any)["account_ref"] = "different-account"
						configuration["default"].(map[string]any)["chain"].([]any)[0].(map[string]any)["account_ref"] = "different-account"
					case "route":
						catalog["routes"].([]any)[0].(map[string]any)["evidence_ref"] = "different-route-evidence"
					case "settings":
						catalog["models"].([]any)[0].(map[string]any)["thinking_levels"] = []string{"high", "off"}
						configuration["default"].(map[string]any)["chain"].([]any)[0].(map[string]any)["thinking"] = "off"
					}
					provider, err = json.Marshal(configuration)
					if err != nil {
						t.Fatal(err)
					}
				}
				if kind == "removed-input" {
					provider = []byte(strings.Replace(string(provider), `["text","image"]`, `["image"]`, 1))
				}
				if kind == "endpoint" {
					var config struct {
						Providers map[string]map[string]any `json:"providers"`
					}
					if err := json.Unmarshal(models, &config); err != nil {
						t.Fatal(err)
					}
					config.Providers["openai"]["baseUrl"] = config.Providers["openai"]["baseUrl"].(string) + "/incompatible"
					models, err = json.Marshal(config)
					if err != nil {
						t.Fatal(err)
					}
				}
				if kind == "sampling" {
					models = []byte(strings.Replace(string(models), `false`, `true`, 1))
				}
				if (kind == "endpoint" || kind == "sampling") && bytes.Equal(models, originalModels) {
					t.Fatal("model runtime negative fixture did not change its input")
				}
				if err := os.WriteFile(filepath.Join(dir, "pi-providers.json"), provider, 0600); err != nil {
					t.Fatal(err)
				}
				if kind != "missing-models" {
					if err := os.WriteFile(filepath.Join(dir, "pi-models.json"), models, 0600); err != nil {
						t.Fatal(err)
					}
				}
				current, err := piproviders.LoadInstalled(root)
				if err != nil {
					t.Fatal(err)
				}
				s.config.PackageRoot = root
				s.config.PiCreation.Catalog = current.Catalog
			}
			before, _ := json.Marshal(intent)
			err = s.validatePiReadyCatalog(context.Background(), intent, j)
			if (err == nil) != (kind == "compatible" || kind == "same-catalog") {
				t.Fatal(kind, err)
			}
			if kind == "same-catalog" && reader.reads != 0 {
				t.Fatal("same catalog gained provenance requirement")
			}
			after, _ := json.Marshal(intent)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("validator rewrote frozen intent")
			}
		})
	}
}

func TestPiReadyCatalogAllowsOnlyAdditiveInputsAndModelEvidence(t *testing.T) {
	a := piProviderPackageFixture(t)
	clone := func() piproviders.PackageConfiguration {
		data, _ := json.Marshal(a)
		var b piproviders.PackageConfiguration
		if err := json.Unmarshal(data, &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	before, _ := json.Marshal(a)
	b := clone()
	b.Catalog.Digest = "different raw catalog identity"
	b.Catalog.Models[0].Inputs = []string{"image", "text"}
	b.Catalog.Models[0].EvidenceRef = "qualified-image"
	if !piCatalogCapabilitiesCompatible(a, b) || !piCatalogCapabilitiesCompatible(a, a) {
		t.Fatal("metadata-only relation refused")
	}
	for _, test := range []struct {
		name   string
		change func(*piproviders.PackageConfiguration)
	}{
		{"schema", func(b *piproviders.PackageConfiguration) { b.Catalog.Schema++ }},
		{"provider", func(b *piproviders.PackageConfiguration) { b.Catalog.Providers[0].NativeID = "other" }},
		{"account", func(b *piproviders.PackageConfiguration) { b.Catalog.Accounts[0].CredentialKey = "other" }},
		{"model", func(b *piproviders.PackageConfiguration) { b.Catalog.Models[0].ID = "other" }},
		{"thinking", func(b *piproviders.PackageConfiguration) { b.Catalog.Models[0].ThinkingLevels = []string{"off"} }},
		{"removed-input", func(b *piproviders.PackageConfiguration) { b.Catalog.Models[0].Inputs = []string{"image"} }},
		{"tools", func(b *piproviders.PackageConfiguration) { b.Catalog.Models[0].Tools = false }},
		{"model-label", func(b *piproviders.PackageConfiguration) { b.Catalog.Models[0].Label = "other" }},
		{"route-state", func(b *piproviders.PackageConfiguration) { b.Catalog.Routes[0].State = "unverified" }},
		{"route-evidence", func(b *piproviders.PackageConfiguration) { b.Catalog.Routes[0].EvidenceRef = "other" }},
		{"default", func(b *piproviders.PackageConfiguration) { b.Default.Chain[0].Thinking = "off" }},
		{"fallback", func(b *piproviders.PackageConfiguration) { b.Default.FallbackOn = nil }},
		{"models", func(b *piproviders.PackageConfiguration) { b.Catalog.Models = b.Catalog.Models[1:] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := clone()
			test.change(&b)
			if piCatalogCapabilitiesCompatible(a, b) {
				t.Fatal("incompatible configuration admitted")
			}
		})
	}
	after, _ := json.Marshal(a)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("comparison mutated original policy/catalog")
	}
}
