package piquestions

// DecodeRetainedRecord validates the full present record without live
// authorization, filesystem access, lock creation or loss of empty identity.
func DecodeRetainedRecord(data []byte) (Record, error) {
	var record Record
	if err := Decode(data, &record); err != nil {
		return Record{}, err
	}
	if err := ValidateRef(record.Conversation); err != nil {
		return Record{}, err
	}
	if err := validRecord(record, record.Conversation); err != nil {
		return Record{}, err
	}
	return record, nil
}
