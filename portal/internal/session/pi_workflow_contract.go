package session

import (
	"encoding/json"
	"fmt"
)

type PiToolPolicyContract struct {
	Policy             int `json:"policy"`
	ConversationSchema int `json:"conversationSchema"`
}

// PiWorkflowContract is retained workflow support, not permission or live proof.
type PiWorkflowContract struct {
	PiQuestionsSchema    int                  `json:"piQuestionsSchema"`
	PiPlanSchema         int                  `json:"piPlanSchema"`
	PiToolPolicy         PiToolPolicyContract `json:"piToolPolicy"`
	HostCallbacks        int                  `json:"hostCallbacks"`
	LinearLineage        int                  `json:"linearLineage"`
	WorkflowObservations int                  `json:"workflowObservations"`
}

// DecodePiWorkflowContract selects the complete workflow bundle from the whole
// bounded runtime metadata object. Unrelated top-level fields are permitted.
func DecodePiWorkflowContract(data []byte) (PiWorkflowContract, error) {
	var fields map[string]json.RawMessage
	if err := DecodePiCreationJSON(data, &fields, 64<<10); err != nil {
		return PiWorkflowContract{}, err
	}
	if fields == nil {
		return PiWorkflowContract{}, fmt.Errorf("runtime contract must be an object")
	}
	selected := map[string]json.RawMessage{}
	for _, name := range []string{"piQuestionsSchema", "piPlanSchema", "piToolPolicy", "hostCallbacks", "linearLineage", "workflowObservations"} {
		value, ok := fields[name]
		if !ok {
			return PiWorkflowContract{}, fmt.Errorf("runtime contract lacks %s", name)
		}
		selected[name] = value
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return PiWorkflowContract{}, err
	}
	var contract PiWorkflowContract
	if err := DecodePiCreationJSON(encoded, &contract, 64<<10); err != nil {
		return PiWorkflowContract{}, err
	}
	expected := PiWorkflowContract{PiQuestionsSchema: 1, PiPlanSchema: 1, PiToolPolicy: PiToolPolicyContract{Policy: 1, ConversationSchema: 2}, HostCallbacks: 1, LinearLineage: 1, WorkflowObservations: 1}
	if contract != expected {
		return PiWorkflowContract{}, fmt.Errorf("unsupported retained Pi workflow contract")
	}
	return contract, nil
}
