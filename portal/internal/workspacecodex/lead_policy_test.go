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
