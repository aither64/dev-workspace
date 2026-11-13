package session

import (
	"encoding/json"
	"errors"
)

// PiNativeRoutingContract is a detached metadata selection, not a retained
// conversation format, live guard result or permission to use schema 3.
type PiNativeRoutingContract struct {
	Workflow     PiWorkflowContract
	Policy       int
	RecordSchema int
}

// DecodePiNativeRoutingContract requires the complete workflow contract and
// exact native-routing map from the same bounded whole runtime metadata object.
// It neither reads conversation records nor enables native guard injection.
func DecodePiNativeRoutingContract(data []byte) (PiNativeRoutingContract, error) {
	workflow, err := DecodePiWorkflowContract(data)
	if err != nil {
		return PiNativeRoutingContract{}, err
	}
	var fields map[string]json.RawMessage
	if err := DecodePiCreationJSON(data, &fields, 64<<10); err != nil {
		return PiNativeRoutingContract{}, err
	}
	var native struct {
		Policy       int `json:"policy"`
		RecordSchema int `json:"recordSchema"`
	}
	if err := DecodePiCreationJSON(fields["piNativeRouting"], &native, 64<<10); err != nil {
		return PiNativeRoutingContract{}, err
	}
	if native.Policy != 1 || native.RecordSchema != 3 {
		return PiNativeRoutingContract{}, errors.New("unsupported Pi native routing contract")
	}
	return PiNativeRoutingContract{Workflow: workflow, Policy: native.Policy, RecordSchema: native.RecordSchema}, nil
}
