package workspacecodex

import (
	"strings"
	"testing"
)

func TestLeadPolicyBindsSessionWithoutReplacingFrozenRoleInstructions(t *testing.T) {
	const slug = "2026-09-24-example"
	const workspace = "/workspace"
	const roleInstructions = "Coordinate the assigned work."
	policy := LeadThreadPolicy(slug, workspace, roleInstructions)
	for _, expected := range []string{slug, workspace, "dev-session current", roleInstructions,
		"DEV_SESSION_SLUG", "DEV_SESSION_WORKSPACE", "workspace-host coordination guide", "explicitly release ownership", "queued claims notify the session",
		"Independent feature work needs no repository claim", "push feature branches", "only after integration approval and preparation are complete",
		"before CI, records or cleanup", "Never hold a repository claim while waiting", "separate machine reservations"} {
		if !strings.Contains(policy.DeveloperInstructions, expected) {
			t.Errorf("lead policy missing %q", expected)
		}
	}
	if strings.Contains(policy.DeveloperInstructions, "claims for catalog-covered mutations") {
		t.Fatal("technical binding still requires claims for independent development")
	}
	_, retained, found := strings.Cut(policy.DeveloperInstructions, "\n\n")
	if !found || retained != roleInstructions {
		t.Fatalf("binding rewrote frozen instructions: %q", retained)
	}
	if LeadThreadPolicy("", "", roleInstructions).DeveloperInstructions != roleInstructions {
		t.Fatal("unbound policy rewrote frozen instructions")
	}
	if strings.Contains(LeadThreadPolicy("", "", "").DeveloperInstructions, slug) {
		t.Fatal("legacy policy unexpectedly has a session binding")
	}
}

func TestFallbackLeadDelegatesExecutionAndDistinguishesMissingMembers(t *testing.T) {
	text := LeadThreadPolicy("", "", "").DeveloperInstructions
	for _, expected := range []string{"complete execution packages", "dependent implementation", "acceptance criteria", "short coordination steps", "no implementation member, implement yourself", "busy, unready or inaccessible"} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q", expected)
		}
	}
	if strings.Contains(text, "Keep short or dependent steps yourself") {
		t.Fatal("broad execution exception remains")
	}
}

func TestCoordinationPolicyRequiresDeployedMachineIntegrationAcceptanceAndRelease(t *testing.T) {
	for _, role := range []string{"", "Retain this saved role exactly."} {
		policy := LeadThreadPolicy("2026-10-21-example", "/workspace", role)
		for _, required := range []string{
			"Retain every deployed physical machine", "deployed feature lineage is integrated into approved remote defaults",
			"user accepts the resulting UI/behavior on that machine", "user explicitly directs its release",
			"Idle time, review, feedback, redeployment, disconnects and turn completion never authorize release",
			"missing evidence keeps the machine held", "Integration needs separate explicit approval",
			"exact system/profile store paths", "complete source/input graphs", "repository/target and machine aliases",
			"fresh remote ancestry evidence", "reviewed old/new base/head ranges", "equivalence and migration evidence",
			"do not call a nonancestor deployed commit literally merged", "substantive successor needs redeployment and renewed user acceptance",
			"Record the accepted environment/result", "review or CI is not user acceptance",
			"exact claim owned by this session", "verify all three conditions", "verify its resulting state",
			"Legacy automatic lifecycle release is implementation behavior", "original recovery contracts",
			"Independent feature work needs no repository claim", "before CI, records or cleanup",
		} {
			if !strings.Contains(policy.DeveloperInstructions, required) {
				t.Errorf("generated policy missing %q", required)
			}
		}
		if role != "" && !strings.HasSuffix(policy.DeveloperInstructions, "\n\n"+role) {
			t.Fatal("technical policy rewrote saved role")
		}
	}
}
