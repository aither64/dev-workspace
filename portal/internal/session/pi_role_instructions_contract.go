package session

import (
	"encoding/json"
	"errors"
)

// PiRoleInstructionsContract selects declared retained instruction support.
// It is detached metadata, not proof of worker application or write permission.
type PiRoleInstructionsContract struct {
	Workflow PiWorkflowContract
	Version  int
}

// DecodePiRoleInstructionsContract requires the complete workflow contract and
// exact instruction version from the same bounded whole runtime metadata object.
// Unrelated top-level fields are permitted.
func DecodePiRoleInstructionsContract(data []byte) (PiRoleInstructionsContract, error) {
	workflow, err := DecodePiWorkflowContract(data)
	if err != nil {
		return PiRoleInstructionsContract{}, err
	}
	var fields map[string]json.RawMessage
	if err := DecodePiCreationJSON(data, &fields, 64<<10); err != nil {
		return PiRoleInstructionsContract{}, err
	}
	var version int
	if err := DecodePiCreationJSON(fields["piRoleInstructions"], &version, 64<<10); err != nil {
		return PiRoleInstructionsContract{}, err
	}
	if version != 1 {
		return PiRoleInstructionsContract{}, errors.New("unsupported Pi role instructions contract")
	}
	return PiRoleInstructionsContract{Workflow: workflow, Version: version}, nil
}
