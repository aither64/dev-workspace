package session

import (
	"encoding/json"
	"errors"
)

// PiPreparationContract selects the whole package contract for pre-root Pi
// requests. It is reader compatibility, not native execution authority.
type PiPreparationContract struct {
	Role   PiRoleInstructionsContract
	Schema int
}

func DecodePiPreparationContract(data []byte) (PiPreparationContract, error) {
	role, err := DecodePiRoleInstructionsContract(data)
	if err != nil {
		return PiPreparationContract{}, err
	}
	var fields map[string]json.RawMessage
	if err := DecodePiCreationJSON(data, &fields, 64<<10); err != nil {
		return PiPreparationContract{}, err
	}
	for field, expected := range map[string]int{"sessionBackendBindingSchema": 1, "sessionBackendBindingPolicy": 1, "runtimeAuthoritySchema": 2, "piCreationSchema": 4} {
		var actual int
		if err := DecodePiCreationJSON(fields[field], &actual, 64<<10); err != nil || actual != expected {
			return PiPreparationContract{}, errors.New("unsupported Pi preparation contract")
		}
	}
	var version int
	if err := DecodePiCreationJSON(fields["piPreparationSchema"], &version, 64<<10); err != nil || (version != 2 && version != 3) {
		return PiPreparationContract{}, errors.New("unsupported Pi preparation contract")
	}
	return PiPreparationContract{Role: role, Schema: version}, nil
}
