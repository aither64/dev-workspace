package turnretry

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/aither64/codex-web/codex"
)

var ErrMissingProof = errors.New("managed execution settings are unproved")

// Backend is application-owned authority. Step never acquires an outer lock:
// its caller retains generation/runtime/roster/conversation admission. Native
// methods below must not reenter the per-thread admission fence.
type Backend interface {
	Observe(context.Context, Scope) (Observation, error)
	Boundary(context.Context, Scope) (string, error)
	Prove(context.Context, Scope, *State, string) error
	Check(context.Context, Scope, Snapshot) error
	Reconcile(context.Context, Scope, Pending) (codex.SendReceipt, bool, error)
	Continue(context.Context, Scope, Pending) (codex.SendReceipt, error)
	NeverSubmitted(context.Context, Scope, Pending) (bool, error)
}

type Controller struct {
	Store   Store
	Backend Backend
	Clock   func() time.Time
	Random  func() float64
	Policy  PolicyAuthority
}

func (c Controller) now() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now()
}
func (c Controller) random() float64 {
	if c.Random != nil {
		return c.Random()
	}
	return 0
}

// ReconcileOnly recovers an existing exact intent without enrollment or
// execution. It may retire that intent's positively unsubmitted prepared
// ledger entry so ordinary admission can run again. It never calls Continue.
func (c Controller) ReconcileOnly(ctx context.Context, scope Scope) error {
	if _, err := c.Store.Load(scope); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	tx, err := c.Store.Lock(ctx, scope)
	if err != nil {
		return err
	}
	defer tx.Close()
	return c.ReconcileLocked(ctx, tx)
}

// ReconcileLocked is receipt-only under an already held admission fence. It
// never returns positive never-submitted proof as permission to dispatch.
func (c Controller) ReconcileLocked(ctx context.Context, tx *Transaction) error {
	if err := c.checkBoundary(ctx, tx); err != nil {
		return err
	}
	if !tx.State.Uncertain() {
		return nil
	}
	_, err := c.reconcile(ctx, tx)
	return err
}

func (c Controller) checkBoundary(ctx context.Context, tx *Transaction) error {
	s := &tx.State
	boundary, err := c.Backend.Boundary(ctx, s.Scope)
	if err != nil {
		return err
	}
	if s.Boundary == "" {
		s.Boundary = boundary
	} else if s.Boundary != boundary {
		s.Cancel("identity_changed")
		if err := tx.Save(); err != nil {
			return err
		}
		return errors.New("retry session incarnation changed")
	}
	return nil
}

// A false result means accepted, cancelled, or still uncertain: no fresh
// outbound attempt is permitted by this reconciliation pass.
func (c Controller) reconcile(ctx context.Context, tx *Transaction) (bool, error) {
	s := &tx.State
	p := *s.Pending
	receipt, found, err := c.Backend.Reconcile(ctx, s.Scope, p)
	if found && err == nil {
		if err := s.Accepted(receipt.TurnID, c.now()); err != nil {
			return false, err
		}
		if err := tx.Save(); err != nil {
			return false, err
		}
		// The receipt is authoritative even when optional summary storage fails.
		if c.Policy != nil {
			_ = c.Policy.Checkpoint(ctx, s, nil, c.now(), false)
		}
		return false, nil
	}
	if err == nil {
		var never bool
		never, err = c.Backend.NeverSubmitted(ctx, s.Scope, p)
		if err == nil && never {
			if s.Cancelled {
				s.Phase, s.Pending = "cancelled", nil
			}
			return s.Pending != nil, tx.Save()
		}
	}
	s.Phase, s.Reason = "reconciling", "outcome_unknown"
	if saveErr := tx.Save(); saveErr != nil {
		return false, saveErr
	}
	if err == nil {
		err = errors.New("automatic submission requires reconciliation")
	}
	return false, err
}

func (c Controller) Step(ctx context.Context, scope Scope) error {
	tx, err := c.Store.Lock(ctx, scope)
	if err != nil {
		return err
	}
	defer tx.Close()
	s := &tx.State
	if err := c.checkBoundary(ctx, tx); err != nil {
		return err
	}
	// Reconcile before current-failure/policy checks: a receipt remains valid
	// after a newer user turn, opt-out, stop or generation recovery.
	if s.Uncertain() {
		unsubmitted, err := c.reconcile(ctx, tx)
		if err != nil || !unsubmitted {
			return err
		}
		decision, err := c.decision(ctx, s)
		if err != nil {
			return err
		}
		if !decision.Allows(s) {
			s.Cancel("disabled")
			s.Phase = "cancelled"
			return tx.Save()
		}
		p := *s.Pending
		// Positive proof permits this same ID/options to be sent. Renew its
		// reservation at dispatch time; an old charge cannot cover a new call.
		if err := c.Backend.Check(ctx, scope, p.Snapshot); err != nil {
			s.Reason = "admission_held"
			if saveErr := tx.Save(); saveErr != nil {
				return saveErr
			}
			return err
		}
		if err := c.preflightAccepted(tx); err != nil {
			return err
		}
		if err := s.ReserveUnsubmitted(c.now(), decision); err != nil {
			return err
		}
		if err := tx.Save(); err != nil {
			return err
		}
		if s.Pending == nil {
			return nil
		}
		return c.dispatch(ctx, tx, *s.Pending)
	}
	decision, err := c.decision(ctx, s)
	if err != nil {
		return err
	}
	// Do not replace an accepted Pending before its idempotent count checkpoint.
	if err := c.Policy.Checkpoint(ctx, s, nil, c.now(), false); err != nil {
		return err
	}
	if s.Pending != nil && !s.Cancelled && !decision.Allows(s) {
		pending := s.Pending
		s.Cancel("disabled")
		s.Pending = pending
		if err := tx.Save(); err != nil {
			return err
		}
		_ = c.Policy.Checkpoint(ctx, s, nil, c.now(), false)
	}
	o, err := c.Backend.Observe(ctx, scope)
	if err != nil {
		return err
	}
	if !s.Enrolled {
		// Read twice around the durable baseline. A native completion racing
		// enrollment is consumed conservatively instead of becoming new work.
		s.Enroll(o)
		if err := tx.Save(); err != nil {
			return err
		}
		after, err := c.Backend.Observe(ctx, scope)
		if err != nil {
			return err
		}
		if o != after {
			s.Baseline, s.BaselineActive = after.Turn, false
		}
		return tx.Save()
	}
	if err := c.Policy.Checkpoint(ctx, s, &o, c.now(), false); err != nil {
		return err
	}
	if o.Status == "failed" && decision.Eligible(s, o) && o.Turn != s.Handled && (o.Turn != s.Baseline || s.BaselineActive) {
		if err := c.Backend.Prove(ctx, scope, s, o.Turn); err != nil {
			s.Phase, s.Reason = "blocked", "settings_unproven"
			if errors.Is(err, ErrMissingProof) {
				s.Handled = o.Turn
			}
			if saveErr := tx.Save(); saveErr != nil {
				return saveErr
			}
			return err
		}
	}
	previousChain := s.Chain
	if err := s.Observe(o, c.now(), c.random(), decision); err != nil {
		return err
	}
	if err := tx.Save(); err != nil {
		return err
	}
	if err := c.Policy.Checkpoint(ctx, s, nil, c.now(), previousChain != s.Chain); err != nil {
		return err
	}
	if s.Phase != "scheduled" || s.Pending == nil || !decision.Allows(s) || s.Now(c.now()) < s.DeadlineMS {
		return nil
	}
	if err := c.Backend.Check(ctx, scope, s.Pending.Snapshot); err != nil {
		s.Reason = "admission_held"
		if saveErr := tx.Save(); saveErr != nil {
			return saveErr
		}
		return err
	}
	unsubmitted, err := c.reconcile(ctx, tx)
	if err != nil || !unsubmitted {
		return err
	}
	if err := c.preflightAccepted(tx); err != nil {
		return err
	}
	if err := s.Begin(c.now(), decision); err != nil {
		return err
	}
	if err := tx.Save(); err != nil {
		return err
	}
	if s.Pending == nil {
		return nil
	}
	return c.dispatch(ctx, tx, *s.Pending)
}

func (c Controller) decision(ctx context.Context, s *State) (Decision, error) {
	if c.Policy == nil {
		return Decision{}, ErrPolicyUnavailable
	}
	return c.Policy.Decide(ctx, s)
}

// Called only after exact positive never-submitted proof. Unknown intents must
// remain reconciliation-only, including when the provenance record is full.
func (c Controller) preflightAccepted(tx *Transaction) error {
	err := tx.State.PreflightAccepted()
	if err != nil {
		if errors.Is(err, ErrSnapshotCapacity) {
			tx.State.Handled = tx.State.Pending.Failure
			tx.State.Phase, tx.State.Reason, tx.State.Cancelled, tx.State.DeadlineMS = "blocked", "settings_unproven", true, 0
			if saveErr := tx.Save(); saveErr != nil {
				return saveErr
			}
		}
		return err
	}
	return nil
}

func (c Controller) dispatch(ctx context.Context, tx *Transaction, p Pending) error {
	receipt, err := c.Backend.Continue(ctx, tx.State.Scope, p)
	if err == nil {
		if err := tx.State.Accepted(receipt.TurnID, c.now()); err != nil {
			return err
		}
		if err := tx.Save(); err != nil {
			return err
		}
		if c.Policy != nil {
			_ = c.Policy.Checkpoint(ctx, &tx.State, nil, c.now(), false)
		}
		return nil
	}
	var unknown *codex.UnknownSendOutcomeError
	if errors.As(err, &unknown) {
		tx.State.Phase, tx.State.Reason = "reconciling", "outcome_unknown"
	} else {
		never, proofErr := c.Backend.NeverSubmitted(ctx, tx.State.Scope, p)
		if proofErr != nil || !never {
			tx.State.Phase, tx.State.Reason = "reconciling", "outcome_unknown"
		} else {
			// A positively refused attempt is terminal for this source failure.
			// Retain its conservative quota charge; no timer retries refusal.
			tx.State.Phase, tx.State.Reason, tx.State.Pending = "blocked", "dispatch_refused", nil
		}
	}
	if saveErr := tx.Save(); saveErr != nil {
		return saveErr
	}
	return err
}
