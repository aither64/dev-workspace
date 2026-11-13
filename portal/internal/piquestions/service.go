package piquestions

import (
	"context"
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Dispatch is an injected P3 adapter. Prepare freezes the owning submission ref
// and digest. Send and Lookup return only the original receipt's phase; they do
// not mint another operation or retry NotSubmitted. Nil ports mean unsupported.
type Dispatch struct {
	Current      func(context.Context, Caller) (Caller, error)
	VerifyOrigin func(context.Context, Caller, QuestionRecord) error
	Ready        func(context.Context, Caller) (bool, error)
	Prepare      func(context.Context, Caller, Delivery) (string, error)
	Send         func(context.Context, Caller, Delivery) (string, error)
	Lookup       func(context.Context, Caller, Delivery) (string, error)
}
type Service struct {
	Store    *FileStore
	Dispatch Dispatch
	// Admit holds the ordinary transition/runtime/team/message admission while fn
	// runs. Store locks are always released before any native call.
	Admit func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error
}

func knownPhase(p string) bool { return p == "Accepted" || p == "NotSubmitted" || p == "Unknown" }
func (s *Service) DeliverNext(ctx context.Context, ref agentbackend.ConversationRef) error {
	if s.Store == nil || s.Admit == nil || s.Dispatch.Current == nil || s.Dispatch.Lookup == nil {
		return ErrUnavailable
	}
	return s.Admit(ctx, ref, func(ctx context.Context) error {
		r := fresh(ref)
		if err := s.Store.file(ref).Read(ctx, &r); err != nil {
			return err
		}
		if err := validRecord(r, ref); err != nil {
			return err
		}
		var selected *ResponseRecord
		var question *QuestionRecord
		for i := range r.Questions {
			q := &r.Questions[i]
			for j := range q.Responses {
				a := &q.Responses[j]
				if a.Delivery.Phase != "Accepted" && (selected == nil || a.Sequence < selected.Sequence) {
					selected = a
					question = q
				}
			}
		}
		if selected == nil {
			return nil
		}
		// All incomplete handoffs are observational. Absence, terminal refusal or
		// uncertainty never makes this a new send opportunity.
		caller, err := s.Dispatch.Current(ctx, question.Caller)
		if err != nil {
			return err
		}
		if ValidateCaller(caller) != nil || !sameCaller(caller, question.Caller) {
			return ErrConflict
		}
		if err = s.Store.validate(ctx, ref, &caller); err != nil {
			return err
		}
		if selected.Delivery.Phase != "pending" {
			if selected.Delivery.ReceiptRef == "" {
				return ErrUnavailable
			}
			phase, err := s.Dispatch.Lookup(ctx, caller, selected.Delivery)
			if err != nil {
				return err
			}
			if !knownPhase(phase) {
				phase = "Unknown"
			}
			return s.Store.setDelivery(ctx, ref, selected.Input.ResponseID, func(d *Delivery) error { d.Phase = phase; return nil })
		}
		if s.Dispatch.VerifyOrigin == nil || s.Dispatch.Ready == nil || s.Dispatch.Prepare == nil || s.Dispatch.Send == nil {
			return ErrUnavailable
		}
		if err = s.Dispatch.VerifyOrigin(ctx, caller, *question); err != nil {
			return err
		}
		if question.Closed {
			return ErrConflict
		}
		ready, err := s.Dispatch.Ready(ctx, caller)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
		receipt, err := s.Dispatch.Prepare(ctx, caller, selected.Delivery)
		if err != nil {
			return err
		}
		if receipt == "" {
			return ErrUnavailable
		}
		delivery := selected.Delivery
		delivery.Phase = "handoff"
		delivery.ReceiptRef = receipt
		if err = s.Store.setDelivery(ctx, ref, selected.Input.ResponseID, func(d *Delivery) error {
			if d.Phase != "pending" {
				return ErrConflict
			}
			*d = delivery
			return nil
		}); err != nil {
			return err
		}
		phase, sendErr := s.Dispatch.Send(ctx, caller, delivery)
		if sendErr != nil || !knownPhase(phase) {
			phase = "Unknown"
		}
		err = s.Store.setDelivery(ctx, ref, selected.Input.ResponseID, func(d *Delivery) error { d.Phase = phase; return nil })
		return errors.Join(sendErr, err)
	})
}
func (s *FileStore) setDelivery(ctx context.Context, ref agentbackend.ConversationRef, id string, fn func(*Delivery) error) error {
	r := fresh(ref)
	return s.file(ref).Update(ctx, &r, func() error {
		if err := validRecord(r, ref); err != nil {
			return err
		}
		for i := range r.Questions {
			for j := range r.Questions[i].Responses {
				a := &r.Questions[i].Responses[j]
				if a.Input.ResponseID == id {
					if err := fn(&a.Delivery); err != nil {
						return err
					}
					r.Revision++
					return nil
				}
			}
		}
		return ErrNotFound
	})
}
