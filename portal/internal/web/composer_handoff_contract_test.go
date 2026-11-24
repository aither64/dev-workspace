package web

import (
	"os/exec"
	"testing"
)

func TestComposerHandoffRetainsOriginalIntent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required")
	}
	if output, err := exec.Command(node, "composer_handoff_contract_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("composer handoff: %v\n%s", err, output)
	}
}

func TestPortalDeliveryComposerPreservesSelectionAndDraft(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required")
	}
	if output, err := exec.Command(node, "portal_delivery_composer_contract_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("portal delivery composer: %v\n%s", err, output)
	}
}
