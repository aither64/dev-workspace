package web

import (
	"os/exec"
	"testing"
)

func TestSubmissionNoticeFollowsOriginalAttempt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required")
	}
	if output, err := exec.Command(node, "submission_notice_contract_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("submission notice: %v\n%s", err, output)
	}
}
