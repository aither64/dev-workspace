package piplan

import (
	"context"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

// Ports require positive policy and native receipt proof. None is registered by
// this package. Mode loss/uncertainty is reconciled observationally, never retried.
type Ports struct {
	Admit          func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error
	Current        func(context.Context, Caller) (Caller, error)
	ApplyDefault   func(context.Context, Caller, string) (bool, error)
	ObserveDefault func(context.Context, Caller, string) (bool, error)
	Prepare        func(context.Context, Caller, Acceptance) (string, error)
	Send           func(context.Context, Caller, Acceptance) (string, error)
	Lookup         func(context.Context, Caller, Acceptance) (string, error)
}
type Service struct {
	Store *FileStore
	Ports Ports
}

func (s *Service) Advance(ctx context.Context, ref agentbackend.ConversationRef, id string) error {
	p := s.Ports
	if s.Store == nil || p.Admit == nil || p.Current == nil || p.ApplyDefault == nil || p.ObserveDefault == nil || p.Prepare == nil || p.Send == nil || p.Lookup == nil {
		return piquestions.ErrUnavailable
	}
	return p.Admit(ctx, ref, func(ctx context.Context) error {
		r := fresh(ref)
		if err := s.Store.file(ref).Read(ctx, &r); err != nil {
			return err
		}
		if err := validateRecord(r, ref); err != nil {
			return err
		}
		var a Acceptance
		var proposal Proposal
		for _, v := range r.Acceptances {
			if v.AcceptanceID == id {
				a = v
			}
		}
		if a.AcceptanceID == "" {
			return piquestions.ErrNotFound
		}
		for _, v := range r.Proposals {
			if v.PlanID == a.PlanID {
				proposal = v
			}
		}
		if a.Phase == "accepted" {
			return nil
		}
		caller, err := p.Current(ctx, proposal.Caller)
		if err != nil {
			return err
		}
		if piquestions.ValidateCaller(caller) != nil || !sameCaller(caller, proposal.Caller) {
			return piquestions.ErrConflict
		}
		if err = s.Store.validate(ctx, ref, &caller); err != nil {
			return err
		}
		// Dispatch intent is persisted before Send. Every revisit consults the same
		// receipt; Unknown/NotSubmitted cannot become a new implementation operation.
		if a.ReceiptRef != "" {
			phase, err := p.Lookup(ctx, caller, a)
			if err != nil {
				return err
			}
			return s.Store.updateAcceptance(ctx, ref, id, func(v *Acceptance) error {
				v.Phase = "held"
				if phase == "Accepted" {
					v.Phase = "accepted"
				}
				return nil
			})
		}
		if a.Phase == "held" {
			return piquestions.ErrUnavailable
		}
		applied := false
		if a.Phase == "prepared" {
			if s.Store.Policy == nil {
				return piquestions.ErrUnavailable
			}
			policy, err := s.Store.Policy(ctx, ref, caller)
			if err != nil {
				return err
			}
			if !policy.Idle || !policy.Eligible || !policy.Enforced || policy.Revision != a.PolicyRevision {
				return piquestions.ErrUnavailable
			}
			if err = s.Store.updateAcceptance(ctx, ref, id, func(v *Acceptance) error { v.Phase = "mode_pending"; return nil }); err != nil {
				return err
			}
			applied, err = p.ApplyDefault(ctx, caller, a.PolicyRevision)
		} else if a.Phase == "mode_pending" {
			applied, err = p.ObserveDefault(ctx, caller, a.PolicyRevision)
		} else {
			return piquestions.ErrUnavailable
		}
		if err != nil || !applied {
			return piquestions.ErrUnavailable
		}
		receipt, err := p.Prepare(ctx, caller, a)
		if err != nil {
			return err
		}
		if receipt == "" {
			return piquestions.ErrUnavailable
		}
		a.Phase = "dispatch_pending"
		a.ReceiptRef = receipt
		if err = s.Store.updateAcceptance(ctx, ref, id, func(v *Acceptance) error { *v = a; return nil }); err != nil {
			return err
		}
		phase, sendErr := p.Send(ctx, caller, a)
		if err = s.Store.updateAcceptance(ctx, ref, id, func(v *Acceptance) error {
			v.Phase = "held"
			if sendErr == nil && phase == "Accepted" {
				v.Phase = "accepted"
			}
			return nil
		}); err != nil {
			return err
		}
		return sendErr
	})
}
func (s *FileStore) updateAcceptance(ctx context.Context, ref agentbackend.ConversationRef, id string, fn func(*Acceptance) error) error {
	r := fresh(ref)
	return s.file(ref).Update(ctx, &r, func() error {
		if err := validateRecord(r, ref); err != nil {
			return err
		}
		for i := range r.Acceptances {
			if r.Acceptances[i].AcceptanceID == id {
				if err := fn(&r.Acceptances[i]); err != nil {
					return err
				}
				r.Revision++
				return nil
			}
		}
		return piquestions.ErrNotFound
	})
}
