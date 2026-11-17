package piruntime

// An input receipt retains its original execution generation. Only the bound
// current worker or its proven runtime ancestors may present that receipt.
func (s *supervisor) admitReceiptGeneration(generation string) error {
	if !hexPattern.MatchString(generation) {
		return ErrScope
	}
	if generation == s.ref.Generation {
		return nil
	}
	records, err := s.workflowRuntimeHistory()
	if err != nil {
		return err
	}
	for id := records.CurrentRequestID; id != ""; {
		record := records.Requests[id]
		if record.Runtime.Generation == generation {
			return nil
		}
		id = record.Start.ReplacesRequestID
	}
	return ErrScope
}
