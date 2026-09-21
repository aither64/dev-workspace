package agentteams

import (
	"encoding/json"
	"errors"
)

const (
	HelperSchemaVersion   = 1
	MaxHelperRequestBytes = 4 * 1024 * 1024
)

type RegistrationRequest struct {
	Schema int `json:"schema"`
}

func DecodeRegistrationRequest(data []byte) (RegistrationRequest, error) {
	if _, err := strictHelperObject(data, "schema"); err != nil {
		return RegistrationRequest{}, err
	}
	var request RegistrationRequest
	if err := decodeStrict(data, &request); err != nil || request.Schema != HelperSchemaVersion {
		return RegistrationRequest{}, errors.New("invalid agent team registration request")
	}
	return request, nil
}

func strictHelperObject(data []byte, fields ...string) (map[string]json.RawMessage, error) {
	if len(data) == 0 || len(data) > MaxHelperRequestBytes {
		return nil, errors.New("agent team helper request exceeds bounds")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	return strictObject(data, fields...)
}
