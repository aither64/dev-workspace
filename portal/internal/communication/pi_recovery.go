package communication

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type PiInputReadAdmission interface {
	WithPiInputRead(context.Context, monitoroperation.PiInputRecord, func() error) error
}
type PiInputLookup interface {
	Lookup(context.Context, piruntime.SubmissionRef) (piruntime.SubmissionStatus, error)
}

// ReconcilePiMessage has no Send/Load/Configure port. It reads only the saved
// original ref under read/recovery authorization, including held/busy inputs.
func (s *Store) ReconcilePiMessage(ctx context.Context, writer monitoroperation.PiTeamWriteAdmission, reader PiInputReadAdmission, scope monitoroperation.PiSessionRecord, generation, id string, native PiInputLookup) error {
	if reader == nil || native == nil {
		return piruntime.ErrUnavailable
	}
	var original PiMessageRecord
	err := s.locked(ctx, false, func(st *state) error {
		if st.Pi == nil {
			return piruntime.ErrScope
		}
		for _, m := range st.Pi.Messages {
			if m.ID == id {
				original = m
				return nil
			}
		}
		return piruntime.ErrScope
	})
	if err != nil {
		return err
	}
	if original.To.Pi == nil || original.To.Pi.Session != scope || original.PiDelivery == nil {
		return piruntime.ErrHeld
	}
	var status piruntime.SubmissionStatus
	var lookupErr error
	called := false
	err = reader.WithPiInputRead(ctx, original.PiDelivery.Input, func() error {
		if called {
			return piruntime.ErrConflict
		}
		called = true
		status, lookupErr = native.Lookup(ctx, original.PiDelivery.Input.Ref())
		return lookupErr
	})
	if !called {
		return errors.Join(piruntime.ErrUnavailable, err)
	}
	outcome := "unknown"
	if err == nil && lookupErr == nil && status.Schema == 1 && status.Ref == original.PiDelivery.Input.Ref() && status.Settings == original.PiDelivery.Settings.Runtime() && status.Input.Message == original.Envelope && status.Input.ActionContext == "communication:"+original.GrantID && !status.Input.Retry {
		switch status.Phase {
		case "Accepted":
			if status.Witness != nil && status.Witness.SessionID == status.Ref.Conversation.NativeID && status.Witness.EntryID != "" && status.Witness.EffectiveDigest == piruntime.MessageDigest(status.Input.Message) {
				outcome = "accepted"
			}
		case "NotSubmitted":
			if status.Refusal != "" {
				outcome = "not_submitted"
			}
		}
	}
	writeErr := s.UpdatePi(ctx, writer, scope, generation, func(st *PiCommunicationState) error {
		for i := range st.Messages {
			m := &st.Messages[i]
			if m.ID != id {
				continue
			}
			if !reflect.DeepEqual(*m, original) {
				return piruntime.ErrConflict
			}
			m.PiDelivery.Outcome = outcome
			m.Status = "held"
			if outcome == "accepted" {
				m.Status = "delivered"
			}
			return nil
		}
		return piruntime.ErrScope
	})
	return errors.Join(err, lookupErr, writeErr)
}

func (s *Store) RevokePiGrant(ctx context.Context, writer monitoroperation.PiTeamWriteAdmission, scope monitoroperation.PiSessionRecord, generation, id, reason string, at time.Time) error {
	return s.UpdatePi(ctx, writer, scope, generation, func(st *PiCommunicationState) error {
		found := false
		for i := range st.Grants {
			g := &st.Grants[i]
			if g.ID == id {
				if g.A.Pi == nil && g.B.Pi == nil {
					return piruntime.ErrScope
				}
				if g.RevokedAt == nil {
					g.RevokedAt = &at
					g.Reason = reason
				}
				found = true
			}
		}
		if !found {
			return piruntime.ErrScope
		}
		for i := range st.Messages {
			m := &st.Messages[i]
			if m.GrantID == id && m.PiDelivery == nil && m.Status != "delivered" && m.To.Pi != nil {
				m.Status = "canceled"
			}
		}
		return nil
	})
}
