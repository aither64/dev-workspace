package agentteams

import (
	"reflect"
	"testing"
)

func TestPiPolicyPreservesRetainedReviewerAndSavedAccess(t *testing.T) {
	role := Role{Model: "retained-model", Effort: "xhigh", Behavior: "reviewer", Purpose: "review",
		Instructions: "Review independently. Do not edit.", Access: "read_only"}
	before := role
	p, err := ProjectPiPolicy(role, "full_access")
	if err != nil || p.Model != role.Model || p.Effort != role.Effort || p.Instructions != role.Instructions ||
		p.SavedAccess != "read_only" || p.ExecutionAccess != "full_access" || !reflect.DeepEqual(role, before) {
		t.Fatalf("policy changed retained settings: %+v %v", p, err)
	}
	for _, mutate := range []func(*Role){
		func(r *Role) { r.Access = "workspace_write" },
		func(r *Role) { r.Purpose = "implementation" },
		func(r *Role) { r.Model = "" },
		func(r *Role) { r.Instructions = "bad\x00instruction" },
	} {
		bad := role
		mutate(&bad)
		if _, err := ProjectPiPolicy(bad, "full_access"); err == nil {
			t.Fatalf("accepted invalid policy: %+v", bad)
		}
	}
}
