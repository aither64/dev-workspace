package web

import (
	"os/exec"
	"testing"
)

func TestAddMemberCapturesAndRetainsSelection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required")
	}
	if output, err := exec.Command(node, "team_add_contract_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("add member: %v\n%s", err, output)
	}
}
