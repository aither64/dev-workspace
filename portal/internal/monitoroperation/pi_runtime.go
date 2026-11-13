package monitoroperation

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func (u PiUtilityRecord) Ref() piruntime.UtilityRef {
	return piruntime.UtilityRef{Session: u.Session.Ref(), OperationID: u.OperationID, Incarnation: u.Incarnation, NativeID: u.NativeID}
}
func utilityRecord(u piruntime.UtilityRef) PiUtilityRecord {
	return PiUtilityRecord{Session: PiSessionRecord{Workspace: u.Session.Workspace, Slug: u.Session.Slug, Incarnation: u.Session.Incarnation}, OperationID: u.OperationID, Incarnation: u.Incarnation, NativeID: u.NativeID}
}
func (r PiUtilityInputRecord) Ref() piruntime.UtilitySubmissionRef {
	return piruntime.UtilitySubmissionRef{Utility: r.Utility.Ref(), OperationID: r.OperationID, Action: r.Action, Digest: r.Digest}
}
func utilityInput(p PiOperation) piruntime.SendInput {
	return piruntime.SendInput{Message: p.Launch.Envelope, ActionContext: "monitor-launch:" + p.ID}
}

// PiRuntimeAdapter is composed with the same Manager as its effect/read
// authority. Controller methods enter admission themselves; callers must not
// hold transition/runtime or domain locks while invoking them.
// Central supplies ReadAdmission independently from Domain's effect verifier.
type PiRuntimeAdapter struct {
	Domain        PiDomainAuthority
	Controller    piruntime.UtilityController
	ReadAdmission piruntime.UtilityReadAuthority
}

var _ piruntime.UtilityAuthority = (*PiRuntimeAdapter)(nil)
var _ piruntime.UtilityReadAuthority = (*PiRuntimeAdapter)(nil)

func (a *PiRuntimeAdapter) WithUtility(ctx context.Context, runtime piruntime.RuntimeRef, utility piruntime.UtilityRef, action string, run func(piruntime.UtilityGrant) error) error {
	if a == nil || run == nil {
		return piruntime.ErrUnavailable
	}
	return a.Domain.WithUtility(ctx, runtime, utilityRecord(utility), action, func(g PiUtilityDomainGrant) error {
		return run(piruntime.UtilityGrant{Runtime: g.Runtime, Utility: g.Utility.Ref(), CreationID: g.CreationID, LaunchOperationID: g.LaunchOperationID, Settings: g.Settings, Input: g.Input})
	})
}

// WithUtilityRead validates the original saved ref under current read authority.
// Its stable digest/ref is independent of the current execution generation.
// Neither this callback nor Lookup is an effect or a current-input inference.
func (a *PiRuntimeAdapter) WithUtilityRead(ctx context.Context, runtime piruntime.RuntimeRef, ref piruntime.UtilitySubmissionRef, read func() error) error {
	if a == nil || a.Domain.Store == nil || a.ReadAdmission == nil || read == nil {
		return piruntime.ErrUnavailable
	}
	if runtime.Session != ref.Utility.Session || utilityRecord(ref.Utility).Validate(true) != nil || !piUtilityHex.MatchString(runtime.Generation) {
		return piruntime.ErrScope
	}
	called := false
	var readErr error
	err := a.ReadAdmission.WithUtilityRead(ctx, runtime, ref, func() error {
		if called {
			readErr = piruntime.ErrConflict
			return readErr
		}
		called = true
		if readErr = ctx.Err(); readErr != nil {
			return readErr
		}
		op, err := a.Domain.Store.Get(ctx, ref.Utility.OperationID)
		if err != nil {
			readErr = err
		} else if op.Pi == nil || op.Pi.Launch.Input == nil || op.Pi.Launch.Input.Ref() != ref ||
			ref.Digest != piruntime.UtilitySubmissionDigest(ref, utilityInput(*op.Pi), op.Pi.Launch.Settings.Runtime()) {
			readErr = piruntime.ErrScope
		} else {
			readErr = read()
		}
		return readErr
	})
	if !called && err == nil {
		return piruntime.ErrUnavailable
	}
	return errors.Join(err, readErr)
}

func (a *PiRuntimeAdapter) snapshot(ctx context.Context, runtime piruntime.RuntimeRef, id string) (PiOperation, error) {
	if a == nil || a.Domain.Store == nil || a.Domain.Verifier == nil || a.Controller == nil {
		return PiOperation{}, piruntime.ErrUnavailable
	}
	scope := PiSessionRecord{Workspace: runtime.Session.Workspace, Slug: runtime.Session.Slug, Incarnation: runtime.Session.Incarnation}
	var result PiOperation
	err := WithPiRecordWrite(ctx, a.Domain.Admission, scope, runtime.Generation, func() error {
		op, err := a.Domain.Store.Get(ctx, id)
		if err != nil {
			return err
		}
		if op.Pi == nil || op.Pi.Owner.Session.Ref() != runtime.Session {
			return piruntime.ErrScope
		}
		result = *op.Pi
		return nil
	})
	return result, err
}

// Launch admits only the existing operation's first real input. Once input is
// reserved, every later call is Lookup-only, even if Bind/Send never replied.
func (a *PiRuntimeAdapter) Launch(ctx context.Context, runtime piruntime.RuntimeRef, id string) error {
	p, err := a.snapshot(ctx, runtime, id)
	if err != nil {
		return err
	}
	if p.Launch.Input != nil {
		return a.ReconcileLaunch(ctx, runtime, id)
	}
	if a.ReadAdmission == nil {
		return piruntime.ErrUnavailable
	}
	if p.CanceledAt != nil || p.Disposition != "" || p.Launch.TerminatedAt != nil {
		return piruntime.ErrHeld
	}
	if p.Launch.Utility.NativeID == "" {
		// The durable claim serializes concurrent Launch calls, but is released
		// before Manager enters its own per-effect admission. A lost creation
		// response is held; this layer has no mapping-repair or replacement API.
		err = WithPiRecordWrite(ctx, a.Domain.Admission, p.Owner.Session, runtime.Generation, func() error {
			if err := a.Domain.Verifier.VerifyPiUtilityEffect(ctx, runtime, p, "create"); err != nil {
				return err
			}
			tx, err := a.Domain.Store.Lock(ctx, id)
			if err != nil {
				return err
			}
			defer tx.Close()
			current := tx.Operation.Pi
			if current == nil || !reflect.DeepEqual(current, &p) {
				return piruntime.ErrConflict
			}
			if current.Launch.CreateAttempted || current.Launch.State != "reserved" {
				return piruntime.ErrHeld
			}
			current.Launch.CreateAttempted, current.Launch.State = true, "creating"
			tx.piWrite = true
			return tx.Save()
		})
		if err != nil {
			return err
		}
		state, err := a.Controller.CreateUtility(ctx, runtime, piruntime.UtilityCreateRequest{Utility: p.Launch.Utility.Ref(), CreationID: p.Launch.CreationID, Settings: p.Launch.Settings.Runtime()})
		if err != nil {
			return err
		}
		u := utilityRecord(state.Utility)
		reserved := u
		reserved.NativeID = ""
		identity := piruntime.UtilityIdentity(piruntime.UtilityExecutionHandle{Utility: state.Utility, Generation: runtime.Generation})
		if reserved != p.Launch.Utility || identity.BackendID != "pi" || state.Generation != runtime.Generation || state.Settings != p.Launch.Settings.Runtime() || state.State != "initialized" || state.Persisted {
			return piruntime.ErrConflict
		}
		err = a.Domain.Store.UpdatePi(ctx, a.Domain.Admission, p.Owner.Session, runtime.Generation, id, func(current *PiOperation) error {
			if current.Launch.Utility != p.Launch.Utility || !current.Launch.CreateAttempted || current.Launch.State != "creating" || current.CanceledAt != nil || current.Disposition != "" {
				return piruntime.ErrConflict
			}
			current.Launch.Utility, current.Launch.State = u, "initialized"
			return nil
		})
		if err != nil {
			return err
		}
		p.Launch.Utility, p.Launch.State, p.Launch.CreateAttempted = u, "initialized", true
	}
	ref := piruntime.UtilitySubmissionRef{Utility: p.Launch.Utility.Ref(), OperationID: p.Launch.MessageID, Action: "send"}
	ref.Digest = piruntime.UtilitySubmissionDigest(ref, utilityInput(p), p.Launch.Settings.Runtime())
	input := PiUtilityInputRecord{Utility: p.Launch.Utility, OperationID: ref.OperationID, Action: ref.Action, Digest: ref.Digest}
	service := PiReportService{Store: a.Domain.Store, Admission: a.Domain.Admission}
	reserved, err := service.reserveUtilityInput(ctx, p.Owner.Session, runtime.Generation, id, input)
	if err != nil {
		return err
	}
	if !reserved {
		return a.ReconcileLaunch(ctx, runtime, id)
	}
	handle := piruntime.UtilityExecutionHandle{Utility: ref.Utility, Generation: runtime.Generation}
	ops, err := a.Controller.BindUtility(ctx, handle)
	if err != nil {
		return err
	}
	if ops.Verify == nil || ops.Send == nil {
		return piruntime.ErrUnavailable
	}
	identity := piruntime.UtilityIdentity(handle)
	if err = ops.Verify.Verify(ctx, identity); err != nil {
		return err
	}
	_, sendErr := ops.Send.Send(ctx, identity, backend.SendRequest{Message: p.Launch.Envelope, ClientUserMessageID: ref.OperationID, ActionContext: utilityInput(p).ActionContext})
	// A Send result is not the durable native witness. Lookup also recovers a
	// positive persisted receipt when Send's reply was lost, without resending.
	return errors.Join(sendErr, a.reconcileLaunch(ctx, runtime, id, sendErr == nil))
}

// ReconcileLaunch invokes only the original receipt lookup. Manager owns its
// separate read gate; no effect admission is held across that Controller call.
func (a *PiRuntimeAdapter) ReconcileLaunch(ctx context.Context, runtime piruntime.RuntimeRef, id string) error {
	return a.reconcileLaunch(ctx, runtime, id, false)
}

func (a *PiRuntimeAdapter) reconcileLaunch(ctx context.Context, runtime piruntime.RuntimeRef, id string, currentSend bool) error {
	if a == nil || a.Domain.Store == nil || a.Controller == nil {
		return piruntime.ErrUnavailable
	}
	saved, err := a.Domain.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if saved.Pi == nil || saved.Pi.Owner.Session.Ref() != runtime.Session || saved.Pi.Launch.Input == nil {
		return piruntime.ErrScope
	}
	p := *saved.Pi
	ref := p.Launch.Input.Ref()
	status, lookupErr := a.Controller.LookupUtility(ctx, ref)
	outcome := "unknown"
	if lookupErr == nil && status.Schema == 2 && status.OwnerKind == "utility" && status.Ref == ref && status.Generation == p.Launch.Generation && status.Settings == p.Launch.Settings.Runtime() && status.Input == utilityInput(p) && ref.Digest == piruntime.UtilitySubmissionDigest(ref, status.Input, status.Settings) {
		switch status.Phase {
		case "Accepted":
			if status.Witness != nil && status.Witness.SessionID == ref.Utility.NativeID && status.Witness.EntryID != "" && status.Witness.EffectiveDigest == piruntime.MessageDigest(status.Input.Message) && status.Refusal == "" {
				outcome = "accepted"
			}
		case "NotSubmitted":
			if status.Refusal != "" && status.Witness == nil {
				outcome = "not_submitted"
			}
		}
	}
	writeErr := a.Domain.Store.UpdatePi(ctx, a.Domain.Admission, p.Owner.Session, runtime.Generation, id, func(current *PiOperation) error {
		if !reflect.DeepEqual(current, &p) {
			return piruntime.ErrConflict
		}
		if p.Launch.Outcome == "accepted" || p.Launch.Outcome == "not_submitted" {
			if outcome != "unknown" && p.Launch.Outcome != outcome {
				return piruntime.ErrConflict
			}
			return nil
		}
		current.Launch.Outcome, current.Launch.State = outcome, "held"
		if current.Launch.TerminatedAt != nil {
			current.Launch.State = "terminated"
		} else if outcome == "accepted" && currentSend && runtime.Generation == current.Launch.Generation && current.CanceledAt == nil && current.Disposition == "" {
			current.Launch.State = "running"
		}
		return nil
	})
	if outcome == "unknown" && lookupErr == nil {
		lookupErr = piruntime.ErrHeld
	}
	return errors.Join(lookupErr, writeErr)
}

// Report can race the original Send reply/domain projection. The authenticated
// bridge already proves the original input Accepted; recover that same durable
// receipt without waiting for Send or holding an effect/store lock, then publish
// only after rechecking the captured utility/input in the short domain write.
func (a *PiRuntimeAdapter) Report(ctx context.Context, runtime piruntime.RuntimeRef, utility piruntime.UtilityRef, inputID string, req ReportRequest, at time.Time) (string, error) {
	if a == nil || a.Domain.Store == nil {
		return "", piruntime.ErrUnavailable
	}
	if err := validateReport(req); err != nil {
		return "", err
	}
	op, err := a.Domain.Store.Get(ctx, utility.OperationID)
	if err != nil {
		return "", err
	}
	if op.Pi == nil || utility.Session != runtime.Session || op.Pi.Launch.Utility.Ref() != utility || op.Pi.Launch.Input == nil || op.Pi.Launch.Input.OperationID != inputID || op.Pi.Launch.Generation != runtime.Generation {
		return "", piruntime.ErrScope
	}
	if err := a.ReconcileLaunch(ctx, runtime, utility.OperationID); err != nil {
		return "", err
	}
	return (PiReportService{Store: a.Domain.Store, Admission: a.Domain.Admission}).Report(ctx, op.Pi.Owner.Session, runtime.Generation, utilityRecord(utility), inputID, req, at)
}

// Unload is an explicit authorized utility effect, never a side effect of
// cancellation or report publication. Verified exit alone does not settle it.
func (a *PiRuntimeAdapter) Unload(ctx context.Context, runtime piruntime.RuntimeRef, id string, at time.Time) error {
	if at.IsZero() {
		return piruntime.ErrScope
	}
	p, err := a.snapshot(ctx, runtime, id)
	if err != nil {
		return err
	}
	if p.Launch.Utility.Validate(true) != nil {
		return piruntime.ErrScope
	}
	stopped, err := a.Controller.UnloadUtility(ctx, piruntime.UtilityExecutionHandle{Utility: p.Launch.Utility.Ref(), Generation: runtime.Generation})
	if err != nil {
		return err
	}
	if !stopped.VerifiedExit || stopped.Outcome != "stopped" {
		return piruntime.ErrHeld
	}
	return a.Domain.Store.UpdatePi(ctx, a.Domain.Admission, p.Owner.Session, runtime.Generation, id, func(current *PiOperation) error {
		if current.Launch.Utility != p.Launch.Utility {
			return piruntime.ErrConflict
		}
		if current.Launch.TerminatedAt == nil {
			current.Launch.TerminatedAt, current.Launch.Termination, current.Launch.State = &at, "verified_exit", "terminated"
		}
		return nil
	})
}
