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
	for _, key := range []string{"turnRetryStateSchema", "turnRetryManagedTurnProof", "turnRetrySessionPolicy"} {
		var declared int
		if err := json.Unmarshal(fields[key], &declared); err != nil || declared != 1 {
			t.Fatalf("%s must be exact integer1: %s (%v)", key, fields[key], err)
		}
	}
	if TurnRetrySessionPolicy() != 1 {
		t.Fatal("portal policy availability differs from candidate guard")
	}
}

func TestMaintenanceHeldCapabilityKeepsReaderPolicies(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(runtimeContractJSON, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"maintenanceHeldCompletionPolicy", "maintenanceStateSchema", "maintenancePolicy"} {
		var value int
		if err := json.Unmarshal(fields[key], &value); err != nil || value != map[string]int{"maintenanceHeldCompletionPolicy": 1, "maintenanceStateSchema": 2, "maintenancePolicy": 2}[key] {
			t.Fatalf("%s has incorrect maintenance contract: %s %v", key, fields[key], err)
		}
	}
}
