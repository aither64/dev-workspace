package agentteams

import (
	"strings"
	"testing"
)

func managedInstalledFixture(t *testing.T) Installed {
	t.Helper()
	catalog := catalogFixture(t)
	return Installed{Managed: true, Catalog: &catalog, packageRoot: "/nix/store/00000000000000000000000000000000-dev-workspace", Metadata: PackageMetadata{AgentTeams: AgentTeamsMetadata{
		Managed: true, MetadataSchema: PackageMetadataSchema,
		Catalog:  CatalogReference{Path: defaultCatalogRelativePath, Digest: catalog.CatalogDigest, SchemaVersion: CatalogSchemaVersion},
		Capacity: catalog.Capacity, NativeCapacity: NativeCapacity{ConfigKey: "agents.max_concurrent_threads_per_session", RequiredValue: catalog.Capacity.RequiredNativeChildThreads},
		Generator: GeneratorIdentity{Identity: agentTeamsGeneratorIdentity, Version: 1, Canonicalization: agentTeamsCanonicalization}, NativeAdapter: catalog.NativeAgentConfigs.Adapter,
		NativeRoleConfigs: catalog.NativeAgentConfigs.Roles, NativeUtilityConfigs: catalog.NativeAgentConfigs.Utilities,
	}}}
}

func TestRegistrationPlanUsesOnlyCurrentCatalogIdentity(t *testing.T) {
	firstRoot := "/nix/store/00000000000000000000000000000000-dev-workspace"
	secondRoot := "/nix/store/11111111111111111111111111111111-dev-workspace"
	installed := managedInstalledFixture(t)
	build := func(root string) (RegistrationPlan, error) {
		current := installed
		current.packageRoot = root
		return buildRegistrationPlan(root, func(string) (Installed, error) { return current, nil })
	}
	first, err := build(firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := build(secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first.Schema != 1 || first.Policy != RegistrationPolicyVersion || first.Digest != second.Digest ||
		len(first.Argv) != 0 || first.Argv == nil || first.RequiredNativeChildThreads != 0 ||
		len(first.States) != 0 || first.States == nil {
		t.Fatalf("direct team registration plan = %#v", first)
	}
	changed := installed
	changed.Catalog = &Catalog{}
	if _, err := buildRegistrationPlan(firstRoot, func(string) (Installed, error) {
		changed.packageRoot = firstRoot
		return changed, nil
	}); err == nil {
		t.Fatal("registration accepted an invalid installed catalog")
	}
	other := installed
	other.packageRoot = firstRoot
	other.Catalog = new(Catalog)
	*other.Catalog = *installed.Catalog
	other.Catalog.CatalogDigest = strings.Repeat("b", 64)
	if _, err := buildRegistrationPlan(firstRoot, func(string) (Installed, error) { return other, nil }); err == nil {
		t.Fatal("registration accepted a catalog that disagrees with package metadata")
	}
}

func TestRegistrationPlanRejectsWrongPackageRoot(t *testing.T) {
	root := "/nix/store/00000000000000000000000000000000-dev-workspace"
	if _, err := buildRegistrationPlan(t.TempDir(), func(string) (Installed, error) { return Installed{}, nil }); err == nil {
		t.Fatal("registration accepted a non-store package root")
	}
	installed := managedInstalledFixture(t)
	installed.packageRoot = "/nix/store/11111111111111111111111111111111-dev-workspace"
	if _, err := buildRegistrationPlan(root, func(string) (Installed, error) { return installed, nil }); err == nil {
		t.Fatal("registration accepted a package loaded from another root")
	}
}

func TestRegistrationPlanSupportsUnmanagedPackage(t *testing.T) {
	root := "/nix/store/00000000000000000000000000000000-dev-workspace"
	plan, err := buildRegistrationPlan(root, func(string) (Installed, error) {
		return Installed{packageRoot: root}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Digest == "" || len(plan.Argv) != 0 || len(plan.States) != 0 || plan.RequiredNativeChildThreads != 0 {
		t.Fatalf("unmanaged package invented native registration: %#v", plan)
	}
}

func TestWorkspaceReportingRegistrationKeepsOnlyOwnedStartupOverrides(t *testing.T) {
	root := "/nix/store/00000000000000000000000000000000-dev-workspace"
	base, err := buildRegistrationPlan(root, func(string) (Installed, error) { return Installed{packageRoot: root}, nil })
	if err != nil {
		t.Fatal(err)
	}
	plan, err := base.WithWorkspaceReporting(root, "/state", "/work tree", "/custom/profile")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Argv) != 10 || plan.Digest == base.Digest {
		t.Fatalf("reporting launch %#v", plan)
	}
	for i := 0; i < len(plan.Argv); i += 2 {
		if plan.Argv[i] != "-c" || !strings.HasPrefix(plan.Argv[i+1], "mcp_servers.team_reporting.") {
			t.Fatalf("unowned override %#v", plan.Argv)
		}
	}
	other, err := base.WithWorkspaceReporting(root, "/state", "/work tree", "/different/profile")
	if err != nil || other.Digest == plan.Digest {
		t.Fatal("custom profile missing from semantic launch")
	}
	for _, path := range []string{"relative", "/unclean/../path", "/bad\x00path"} {
		if _, err := base.WithWorkspaceReporting(root, path, "/workspace", "/profile"); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
