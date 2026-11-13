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

func TestMonitorObserverCapabilitiesPreserveNativeContract(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(runtimeContractJSON, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"monitorOperationStateSchema", "monitorOperationPolicy", "monitorObserverStateSchema", "monitorObserverLifecyclePolicy"} {
		var value int
		if err := json.Unmarshal(fields[key], &value); err != nil || value != 1 {
			t.Fatalf("%s requires exact integer1: %s (%v)", key, fields[key], err)
		}
	}
	schema, policy := MonitorObserverContract()
	if schema != 1 || policy != 1 {
		t.Fatal("portal and candidate capabilities disagree", schema, policy)
	}
}

func TestObserverHelperCutoverPreservesReportingAndStateFormats(t *testing.T) {
	var fields map[string]int
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(runtimeContractJSON, &raw); err != nil {
		t.Fatal(err)
	}
	fields = map[string]int{"livePackageSwitchPolicy": 2, "workspaceReportingPolicy": 3, "monitorOperationStateSchema": 1, "monitorOperationPolicy": 1, "monitorObserverStateSchema": 1, "monitorObserverLifecyclePolicy": 1}
	for key, want := range fields {
		var got int
		if err := json.Unmarshal(raw[key], &got); err != nil || got != want {
			t.Fatalf("%s = %s; want integer%d (%v)", key, raw[key], want, err)
		}
	}
}
