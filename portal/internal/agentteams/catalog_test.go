package agentteams

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ptrString(value string) *string { return &value }

func catalogFixture(t *testing.T) Catalog {
	t.Helper()
	identity := NativeIdentity{
		Generator:      GeneratorIdentity{Identity: agentTeamsGeneratorIdentity, Version: 1, Canonicalization: agentTeamsCanonicalization},
		NativeAdapter:  NativeAdapterIdentity{Identity: nativeAdapterIdentity, Version: 1},
		BehaviorDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("Lead work."))),
	}
	utilityIdentity := identity
	utilityIdentity.BehaviorDigest = strings.Repeat("b", 64)
	policy := WorkPolicy{Default: "high", Simple: "high", Allowed: []string{"high"}, SimpleRequiresReason: true, Followup: "retain"}
	catalog := Catalog{
		SchemaVersion: CatalogSchemaVersion,
		DefaultTeam:   "solo",
		Capacity:      Capacity{RequiredNativeChildThreads: 1},
		WorkPolicy:    WorkPolicies{Design: policy, Implementation: policy},
		Teams: map[string]Team{"solo": {
			Description: "A neutral solo team.", DesignOwner: "team_lead", MaxOpenAgents: 0, Mode: "solo", ServicePolicy: "unmanaged",
			Lifecycle: Lifecycle{Startup: "on_demand", Communication: "lead_mediated", ReviewerReuse: "same_change"},
			Roles:     map[string]Role{"team_lead": {Model: "model-lead", Effort: "high", Behavior: "team_lead", Purpose: "lead", Instructions: "Lead work.", Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "workspace_write"}},
		}},
		Utilities:          Utilities{VerificationWatcher: VerificationWatcher{Model: "model-watcher", Effort: "low", Behavior: "verification_watcher", Lifetime: "operation", Access: "workspace_write", Startup: "on_demand", MaxConcurrent: 1, RequiredFor: []string{"long_check"}}},
		ModelInventory:     []string{"model-lead", "model-watcher"},
		NativeAgentConfigs: NativeAgentConfigs{Adapter: NativeAdapterIdentity{Identity: nativeAdapterIdentity, Version: 1}},
	}
	data := marshalFixture(t, catalog)
	catalog.Teams["solo"] = withFixtureTeamDigest(t, catalog.Teams["solo"], data, "solo")
	data = marshalFixture(t, catalog)
	digest, err := catalogDigest(data)
	if err != nil {
		t.Fatal(err)
	}
	catalog.CatalogDigest = digest
	role := NativeRoleConfig{Kind: "role", Team: "solo", Role: "team_lead", Effort: "high", Identity: identity, Path: "share/dev-workspace/agent-teams/solo/team_lead-high.toml"}
	role.Name = expectedRoleName(catalog.CatalogDigest, role)
	utility := NativeUtilityConfig{Kind: "utility", Utility: "verification_watcher", Effort: "low", Identity: utilityIdentity, Path: "share/dev-workspace/agent-teams/utilities/verification_watcher-low.toml"}
	utility.Name = expectedUtilityName(catalog.CatalogDigest, utility)
	catalog.NativeAgentConfigs.Roles = []NativeRoleConfig{role}
	catalog.NativeAgentConfigs.Utilities = []NativeUtilityConfig{utility}
	return catalog
}

func withFixtureTeamDigest(t *testing.T, team Team, data []byte, name string) Team {
	t.Helper()
	root, err := decodeRawJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	object := root.(map[string]any)
	rawTeam := object["teams"].(map[string]any)[name].(map[string]any)
	delete(rawTeam, "team_digest")
	digest, err := digestJSON(map[string]any{"schema_version": object["schema_version"], "capacity": object["capacity"], "work_policy": object["work_policy"], "team_name": name, "team": rawTeam})
	if err != nil {
		t.Fatal(err)
	}
	team.TeamDigest = digest
	return team
}

func marshalFixture(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeCatalogFixture(t *testing.T, managed bool) (string, Catalog) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "share/dev-workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := PackageMetadata{Schema: PackageMetadataSchema, ActivationEnvironmentAliases: []string{"DEV_WORKSPACES"}, RouterSocket: "/run/dev-workspaces/router.sock", UserNamespace: "dev-workspaces"}
	catalog := Catalog{}
	var metadataData []byte
	if managed {
		catalog = catalogFixture(t)
		metadata.AgentTeams = AgentTeamsMetadata{
			Managed: true, MetadataSchema: 1,
			Catalog:  CatalogReference{Path: defaultCatalogRelativePath, Digest: catalog.CatalogDigest, SchemaVersion: CatalogSchemaVersion},
			Capacity: catalog.Capacity, NativeCapacity: NativeCapacity{ConfigKey: "agents.max_concurrent_threads_per_session", RequiredValue: catalog.Capacity.RequiredNativeChildThreads},
			Generator:     GeneratorIdentity{Identity: agentTeamsGeneratorIdentity, Version: 1, Canonicalization: agentTeamsCanonicalization},
			NativeAdapter: catalog.NativeAgentConfigs.Adapter, NativeRoleConfigs: catalog.NativeAgentConfigs.Roles, NativeUtilityConfigs: catalog.NativeAgentConfigs.Utilities,
		}
		if err := os.WriteFile(filepath.Join(root, defaultCatalogRelativePath), marshalFixture(t, catalog), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, role := range catalog.NativeAgentConfigs.Roles {
			path := filepath.Join(root, role.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("name = \""+role.Name+"\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, utility := range catalog.NativeAgentConfigs.Utilities {
			path := filepath.Join(root, utility.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("name = \""+utility.Name+"\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		metadataData = marshalFixture(t, metadata)
	} else {
		metadataData = marshalFixture(t, map[string]any{
			"schema": metadata.Schema, "activationEnvironmentAliases": metadata.ActivationEnvironmentAliases,
			"routerSocket": metadata.RouterSocket, "userNamespace": metadata.UserNamespace,
			"agent_teams": map[string]bool{"managed": false},
		})
	}
	if err := os.WriteFile(filepath.Join(root, packageMetadataRelativePath), metadataData, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, catalog
}

func TestLoadInstalledCatalogAndPin(t *testing.T) {
	root, want := writeCatalogFixture(t, true)
	installed, err := LoadInstalled(root)
	if err != nil {
		t.Fatal(err)
	}
	if !installed.Managed || installed.Catalog == nil || installed.Catalog.CatalogDigest != want.CatalogDigest {
		t.Fatalf("installed catalog = %#v", installed)
	}
	if _, ok := installed.Catalog.Team("solo"); !ok {
		t.Fatal("team lookup failed")
	}
	if variant, ok := installed.Catalog.RoleVariant("solo", "team_lead", "high"); !ok || variant.Name == "" {
		t.Fatal("role variant lookup failed")
	}
	// LoadInstalled accepts a staged fixture, while a persisted catalog pin
	// deliberately requires a canonical /nix/store root.
	pin := CatalogPin{PackagePath: "/nix/store/00000000000000000000000000000000-dev-workspace", CatalogPath: defaultCatalogRelativePath, CatalogDigest: want.CatalogDigest, SchemaVersion: CatalogSchemaVersion, MetadataSchema: PackageMetadataSchema, Generator: installed.Metadata.AgentTeams.Generator, NativeAdapter: installed.Metadata.AgentTeams.NativeAdapter, Capacity: want.Capacity}
	if err := pin.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPinned(root, pin); err == nil {
		t.Fatal("temporary fixture was accepted as a persisted package pin")
	}
}

func TestLoadInstalledUnmanagedAndRejectsUnsafeNativeFile(t *testing.T) {
	root, _ := writeCatalogFixture(t, false)
	installed, err := LoadInstalled(root)
	if err != nil || installed.Managed || installed.Catalog != nil {
		t.Fatalf("unmanaged package = %#v, %v", installed, err)
	}
	root, catalog := writeCatalogFixture(t, true)
	path := filepath.Join(root, catalog.NativeAgentConfigs.Roles[0].Path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, packageMetadataRelativePath), path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalled(root); err == nil {
		t.Fatal("native symlink was accepted")
	}
}

func TestLoadInstalledRejectsPackageSymlinksAndSizeCaps(t *testing.T) {
	root, _ := writeCatalogFixture(t, false)
	metadataPath := filepath.Join(root, packageMetadataRelativePath)
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), metadataPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalled(root); err == nil {
		t.Fatal("package metadata symlink was accepted")
	}

	root, _ = writeCatalogFixture(t, false)
	if err := os.WriteFile(filepath.Join(root, packageMetadataRelativePath), []byte(strings.Repeat("x", maxPackageMetadataBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalled(root); err == nil {
		t.Fatal("oversized package metadata was accepted")
	}

	root, _ = writeCatalogFixture(t, true)
	if err := os.WriteFile(filepath.Join(root, defaultCatalogRelativePath), []byte(strings.Repeat("x", maxCatalogBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalled(root); err == nil {
		t.Fatal("oversized catalog was accepted")
	}
}

func TestCatalogStrictJSONRejectsDuplicatesAndDigestChanges(t *testing.T) {
	catalog := catalogFixture(t)
	data := marshalFixture(t, catalog)
	if _, err := decodeCatalog(append(data[:1], append([]byte(`"schema_version":3,"schema_version":3,`), data[1:]...)...)); err == nil {
		t.Fatal("duplicate catalog key was accepted")
	}
	catalog.Teams["solo"] = Team{}
	if _, err := decodeCatalog(marshalFixture(t, catalog)); err == nil {
		t.Fatal("tampered catalog was accepted")
	}
}

func TestCanonicalJSONMatchesNixSpecialCharacterFixture(t *testing.T) {
	got, err := canonicalJSON(map[string]any{
		"ascii":      "<>&",
		"literal":    "\\u2028\\u2029",
		"non_ascii":  "é",
		"separators": "\u2028\u2029",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Literal output of:
	// nix eval --impure --raw --expr 'builtins.toJSON { ascii = "<>&"; literal = "\\u2028\\u2029"; non_ascii = "é"; separators = "  "; }'
	const want = `{"ascii":"<>&","literal":"\\u2028\\u2029","non_ascii":"é","separators":"  "}`
	if string(got) != want {
		t.Fatalf("Nix canonical JSON = %q, want %q", got, want)
	}
}

func TestLegacyCatalogSchemaRejected(t *testing.T) {
	// The old package catalog cannot supply frozen role instructions. Existing
	// session receipts and rosters remain readable, but new package catalogs
	// must carry the complete schema-4 policy.
	const fixture = `{"capacity":{"required_native_child_threads":1},"catalog_digest":"8726f5f556d30a9292fe27483539c473ea32702ff853a5616e8d82706e2e7c25","default_development_team":null,"default_team":"solo","model_inventory":["model-<>&é","watcher-<>&é"],"native_agent_configs":{"adapter":{"identity":"codex-custom-agent-toml","version":1},"roles":[{"effort":"high","identity":{"behavior_digest":"f9203d5f97672c28bd89e56beacf8406bd80bc03617752836a7080220421a674","generator":{"canonicalization":"nix-builtins-toJSON-attrset-v1","identity":"dev-workspace-nix-agent-teams","version":1},"native_adapter":{"identity":"codex-custom-agent-toml","version":1}},"kind":"role","name":"dw_76635e4136ba7ffbd2d59c3f412482aa2544bde741392841","path":"share/dev-workspace/agent-teams/solo/team_lead-high.toml","role":"team_lead","team":"solo"}],"utilities":[{"effort":"low","identity":{"behavior_digest":"40e97c6355a385f81a0bdcc354486b3a6f05d6bb1ef459f7b61dfafd3098a90c","generator":{"canonicalization":"nix-builtins-toJSON-attrset-v1","identity":"dev-workspace-nix-agent-teams","version":1},"native_adapter":{"identity":"codex-custom-agent-toml","version":1}},"kind":"utility","name":"dw_a92c058f263afc4e6ddb553dbe0e61429719cd1a10b11deb","path":"share/dev-workspace/agent-teams/utilities/verification_watcher-low.toml","utility":"verification_watcher"}]},"schema_version":3,"teams":{"solo":{"description":"Nix <>& é   ","design_owner":"team_lead","lifecycle":{"communication":"lead_mediated","reviewer_reuse":"same_change","startup":"on_demand"},"max_open_agents":0,"mode":"solo","roles":{"team_lead":{"access":"workspace_write","allowed_efforts":["high"],"behavior":"team_lead","effort":"high","fresh_context":false,"lifetime":"session","model":"model-<>&é"}},"routing":{},"service_policy":"unmanaged","team_digest":"92f8ad296b34d6abb047a35491aa27db6c925fae29eba4e8b4492a6b6444acf3"}},"utilities":{"verification_watcher":{"access":"workspace_write","behavior":"verification_watcher","effort":"low","lifetime":"operation","max_concurrent":1,"model":"watcher-<>&é","required_for":["long_check"],"startup":"on_demand"}},"work_policy":{"design":{"allowed":["high"],"default":"high","followup":"retain","simple":"high","simple_requires_reason":true},"implementation":{"allowed":["high"],"default":"high","followup":"retain","simple":"high","simple_requires_reason":true}}}`
	if _, err := decodeCatalog([]byte(fixture)); err == nil {
		t.Fatal("schema-3 package catalog was accepted without frozen role instructions")
	}
}

func TestCatalogRejectsRefinedTeamAndRoutingContracts(t *testing.T) {
	catalog := catalogFixture(t)
	team := catalog.Teams["solo"]
	team.DesignOwner = "reviewer"
	if validTeam("solo", team, catalog.Capacity, catalog.WorkPolicy) {
		t.Fatal("reviewer design owner was accepted")
	}
	development := Team{
		Description: "development", DesignOwner: "team_lead", MaxOpenAgents: 1, Mode: "development", ServicePolicy: "non_priority",
		Lifecycle: Lifecycle{Startup: "on_demand", Communication: "lead_mediated", ReviewerReuse: "same_change"}, TeamDigest: strings.Repeat("a", 64),
		Roles: map[string]Role{
			"team_lead":   {Model: "lead", Effort: "high", Behavior: "team_lead", Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "workspace_write"},
			"implementer": {Model: "implementer", Effort: "high", Behavior: "reviewer", Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "workspace_write"},
			"reviewer":    {Model: "reviewer", Effort: "high", Behavior: "reviewer", Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "read_only", FreshContext: true},
		},
		Routing: Routing{DesignSimpleEffort: ptrString("high"), ImplementerSimpleEffort: ptrString("high")},
	}
	if validTeam("development", development, catalog.Capacity, catalog.WorkPolicy) {
		t.Fatal("implementer key with non-implementer behavior was accepted")
	}
	if err := validateCatalogShape([]byte(`{"schema_version":3,"default_team":"solo","default_development_team":null,"capacity":{"required_native_child_threads":1},"work_policy":{"design":{"default":"high","simple":"high","allowed":["high"],"simple_requires_reason":true,"followup":"retain"},"implementation":{"default":"high","simple":"high","allowed":["high"],"simple_requires_reason":true,"followup":"retain"}},"teams":{"solo":{"description":"x","design_owner":"team_lead","lifecycle":{"startup":"on_demand","communication":"lead_mediated","reviewer_reuse":"same_change"},"max_open_agents":0,"mode":"solo","roles":{"team_lead":{"model":"model","effort":"high","behavior":"team_lead","lifetime":"session","allowed_efforts":["high"],"access":"workspace_write","fresh_context":false}},"routing":{"design_simple_effort":null},"service_policy":"unmanaged","team_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},"utilities":{"verification_watcher":{"model":"watcher","effort":"low","behavior":"verification_watcher","lifetime":"operation","access":"workspace_write","startup":"on_demand","max_concurrent":1,"required_for":["long_check"]}},"catalog_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","model_inventory":["model","watcher"],"native_agent_configs":{"adapter":{"identity":"codex-custom-agent-toml","version":1},"roles":[],"utilities":[]}}`)); err == nil {
		t.Fatal("explicit null routing value was accepted")
	}
	variant := catalog.NativeAgentConfigs.Roles[0]
	variant.Path = "share/dev-workspace/agent-teams/elsewhere.toml"
	catalog.NativeAgentConfigs.Roles[0] = variant
	if err := validateVariants(catalog); err == nil {
		t.Fatal("nondeterministic role path was accepted")
	}
}

func TestDevelopmentTeamAcceptsPurposeBasedCustomRoles(t *testing.T) {
	catalog := catalogFixture(t)
	team := Team{
		Description: "Purpose-based team", DesignOwner: "architect", MaxOpenAgents: 3,
		Mode: "development", ServicePolicy: "non_priority", TeamDigest: strings.Repeat("a", 64),
		Lifecycle: Lifecycle{Startup: "on_demand", Communication: "lead_mediated", ReviewerReuse: "same_change"},
		Roles: map[string]Role{
			"team_lead": {Model: "model", Effort: "high", Behavior: "team_lead", Purpose: "lead", Instructions: "Lead.",
				Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "workspace_write"},
			"architect": {Model: "model", Effort: "high", Behavior: "designer", Purpose: "design", Instructions: "Design.",
				Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "read_only"},
			"coder": {Model: "model", Effort: "high", Behavior: "implementer", Purpose: "implementation", Instructions: "Implement.",
				Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "workspace_write"},
			"auditor": {Model: "model", Effort: "high", Behavior: "reviewer", Purpose: "review", Instructions: "Review.",
				Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "read_only", FreshContext: true},
		},
		Routing: Routing{DesignSimpleEffort: ptrString("high"), ImplementerSimpleEffort: ptrString("high")},
	}
	if !validTeam("custom", team, Capacity{RequiredNativeChildThreads: 3}, catalog.WorkPolicy) {
		t.Fatal("purpose-based role names were rejected")
	}
	team.Roles["bad_name"] = team.Roles["architect"]
	if validTeam("custom", team, Capacity{RequiredNativeChildThreads: 3}, catalog.WorkPolicy) {
		t.Fatal("non-addressable role name was accepted")
	}
	delete(team.Roles, "bad_name")
	team.Roles["lead"] = team.Roles["architect"]
	if validTeam("custom", team, Capacity{RequiredNativeChildThreads: 3}, catalog.WorkPolicy) {
		t.Fatal("reserved lead member name was accepted")
	}
	delete(team.Roles, "lead")
	team.Roles["designer"] = team.Roles["architect"]
	if validTeam("custom", team, Capacity{RequiredNativeChildThreads: 3}, catalog.WorkPolicy) {
		t.Fatal("duplicate architect address projection was accepted")
	}
	delete(team.Roles, "designer")
	reviewer := team.Roles["auditor"]
	reviewer.Access = "workspace_write"
	team.Roles["auditor"] = reviewer
	if validTeam("custom", team, Capacity{RequiredNativeChildThreads: 3}, catalog.WorkPolicy) {
		t.Fatal("review role with write access was accepted")
	}
}

func TestCatalogPinRequiresCanonicalStoreBasename(t *testing.T) {
	for _, path := range []string{
		"/nix/store/example-dev-workspace",
		"/nix/store/00000000000000000000000000000000",
		"/nix/store/0000000000000000000000000000000e-dev-workspace",
		"/nix/store/00000000000000000000000000000000-dev/workspace",
		"/nix/store/00000000000000000000000000000000-dev\x01workspace",
	} {
		if nixStoreRoot(path) {
			t.Fatalf("invalid store root accepted: %q", path)
		}
	}
	if !nixStoreRoot("/nix/store/00000000000000000000000000000000-dev-workspace") {
		t.Fatal("canonical store root was rejected")
	}
}

func TestManagedCapacityIsBoundedInEveryPersistedContract(t *testing.T) {
	catalog := catalogFixture(t)
	catalog.Capacity.RequiredNativeChildThreads = 65
	if err := catalog.Validate(); err == nil {
		t.Fatal("catalog accepted native child capacity above 64")
	}

	metadata := managedInstalledFixture(t).Metadata.AgentTeams
	metadata.Capacity.RequiredNativeChildThreads = 65
	metadata.NativeCapacity.RequiredValue = 65
	if err := metadata.Validate(); err == nil {
		t.Fatal("managed package metadata accepted native child capacity above 64")
	}
}
