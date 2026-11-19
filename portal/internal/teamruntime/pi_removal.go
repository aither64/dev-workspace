package teamruntime

import (
	"context"
	"reflect"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
)

// RemovePi publishes only a tombstone after the caller proves native detachment
// or an untouched reservation. It does not relax the ordinary SavePi transition.
func (s *Store) RemovePi(ctx context.Context, admission monitoroperation.PiTeamWriteAdmission, generation string, previous, next PiRoster, address string, verifier PiRosterReadyVerifier) error {
	if previous.Validate() != nil || next.Validate() != nil || next.Schema != 5 || next.Revision != previous.Revision+1 || len(next.Members) != len(previous.Members) || verifier == nil {
		return ErrPiPolicy
	}
	unchanged := next
	unchanged.Schema, unchanged.Revision, unchanged.UpdatedAt = previous.Schema, previous.Revision, previous.UpdatedAt
	unchanged.Members = append([]PiRosterMember{}, next.Members...)
	found := false
	for n, m := range previous.Members {
		if m.Address == address {
			if found || m.State == "removed" || next.Members[n].State != "removed" || next.Members[n].RemovedAt == nil || next.Members[n].RemovedAt.IsZero() {
				return ErrPiPolicy
			}
			found = true
			unchanged.Members[n].State = m.State
			unchanged.Members[n].RemovedAt = m.RemovedAt
		}
	}
	if !found || !reflect.DeepEqual(unchanged, previous) {
		return ErrPiConflict
	}
	return monitoroperation.WithPiRecordWrite(ctx, admission, next.Scope(), generation, func() error {
		if err := verifier.VerifyPiRosterReady(ctx, next); err != nil {
			return err
		}
		return s.withOperationLock(ctx, next.Slug, func() error {
			current, err := s.LoadPi(next.Slug)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, previous) {
				return ErrPiConflict
			}
			return monitoroperation.WritePiRecord(s.path(next.Slug), next, maxRosterBytes)
		})
	})
}
