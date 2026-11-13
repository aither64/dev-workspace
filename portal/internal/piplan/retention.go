package piplan

import "github.com/aither64/dev-workspace/portal/internal/piquestions"

// DecodeRetainedRecord returns full detached retained evidence, including empty
// and held records, without live policy/native access or StateFile locks.
func DecodeRetainedRecord(data []byte) (Record, error) {
	var record Record
	if err := piquestions.Decode(data, &record); err != nil {
		return Record{}, err
	}
	if err := piquestions.ValidateRef(record.Conversation); err != nil {
		return Record{}, err
	}
	if err := validateRecord(record, record.Conversation); err != nil {
		return Record{}, err
	}
	return record, nil
}
