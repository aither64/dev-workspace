package web

import (
	"os/exec"
	"testing"
)

func TestResetCreditBrowserContracts(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is supplied by the Nix check environment")
	}
	output, err := exec.Command("node", "--test", "reset_credits_browser_test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("reset browser contracts: %v\n%s", err, output)
	}
}
