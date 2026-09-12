package web

import (
	"os/exec"
	"testing"
)

func TestRepositoryBrowserNavigationContracts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for browser contracts")
	}
	output, err := exec.Command(node, "repository_review_browser_test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("repository browser contracts: %v\n%s", err, output)
	}
}
