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
		"DEV_SESSION_SLUG", "DEV_SESSION_WORKSPACE"} {
		if !strings.Contains(policy.DeveloperInstructions, expected) {
			t.Errorf("lead policy missing %q", expected)
		}
	}
	if strings.Contains(LeadThreadPolicy("", "", "").DeveloperInstructions, slug) {
		t.Fatal("legacy policy unexpectedly has a session binding")
	}
}
