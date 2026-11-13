package progressobserver

import (
	"os"
	"strings"
	"testing"
)

func TestReadPolicyRefreshPreservesExtendedStockAndCustomInstructions(t *testing.T) {
	data, err := os.ReadFile("testdata/extended-stock-instructions.txt")
	if err != nil {
		t.Fatal(err)
	}
	old := string(data)
	updated, refresh := RefreshedStockInstructions(old)
	if !refresh || strings.Contains(updated, "inspect other sessions") || !strings.Contains(updated, "Read other sessions only as task-relevant reference") {
		t.Fatal("released observer did not receive the read policy")
	}
	// The previous paragraphs carry independent verification guidance.
	oldPrefix, _, _ := strings.Cut(old, "Do not force parallel work")
	if !strings.HasPrefix(updated, oldPrefix) || !strings.Contains(updated, "do not call report_to_lead") {
		t.Fatal("observer lost unrelated role instructions")
	}
	for _, retained := range []string{updated, Instructions, old + "\nCustom scope."} {
		if result, changed := RefreshedStockInstructions(retained); changed || result != retained {
			t.Fatal("current or custom policy was rewritten")
		}
	}
	if PendingV2Instructions(old) != old {
		t.Fatal("pending submission options changed")
	}
}
