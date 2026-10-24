package session

import (
	"encoding/json"
	"testing"
)

func TestTurnRetrySessionPolicyMatchesEmbeddedContract(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(runtimeContractJSON, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"turnRetryStateSchema", "turnRetrySessionPolicy"} {
		var declared int
		if err := json.Unmarshal(fields[key], &declared); err != nil || declared != 1 {
			t.Fatalf("%s must be exact integer1: %s (%v)", key, fields[key], err)
		}
	}
	if TurnRetrySessionPolicy() != 1 {
		t.Fatal("portal policy availability differs from candidate guard")
	}
}
