package monitoroperation

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// NewPiOperation freezes the real monitor launch brief. The caller supplies
// its already reserved random utility incarnation, never a roster address.
func NewPiOperation(req StartRequest, owner PiOwnerRecord, policy PiUtilityPolicyRecord, incarnation string, at time.Time) (PiOperation, error) {
	legacyEnvelope := initialLaunch(Operation{ID: req.ID, Source: req.Source, Brief: req.Brief, CancellationScope: req.CancellationScope}).Envelope
	op := PiOperation{Schema: 2, Backend: "pi", BackendContract: 1, ID: req.ID, Workspace: owner.Session.Workspace, Source: req.Source, Brief: req.Brief, CancellationScope: req.CancellationScope, RequestDigest: digest(req), CreatedAt: at, Events: []PiEventRecord{}, Owner: owner, UtilityPolicy: policy,
		Launch: PiLaunchRecord{State: "reserved", Utility: PiUtilityRecord{Session: owner.Session, OperationID: req.ID, Incarnation: incarnation}, CreationID: derivedID(req.ID, "utility"), MessageID: derivedID(req.ID, "launch"), Envelope: legacyEnvelope, Settings: policy.Settings}}
	return op, op.Validate()
}

// AddPiEvent publishes the existing immutable slot only. Report durability is
// intentionally separate from native callback/resource drain and lead delivery.
func AddPiEvent(op *PiOperation, req ReportRequest, at time.Time) error {
	if err := validateReport(req); err != nil {
		return err
	}
	for _, e := range op.Events {
		if e.Report.Slot == req.Slot {
			if reflect.DeepEqual(e.Report, req) {
				return nil
			}
			return piruntime.ErrConflict
		}
	}
	if req.Slot == "escalation" {
		for _, e := range op.Events {
			if e.Report.Slot == "terminal" {
				return piruntime.ErrConflict
			}
		}
	}
	id := derivedID(op.ID, req.Slot)
	body, err := json.Marshal(struct {
		OperationID string        `json:"operationId"`
		Source      Source        `json:"source"`
		Report      ReportRequest `json:"report"`
	}{op.ID, op.Source, req})
	if err != nil {
		return err
	}
	status := "pending"
	if op.CanceledAt != nil || op.Disposition != "" {
		status = "canceled"
	}
	op.Events = append(op.Events, PiEventRecord{ID: id, CreatedAt: at, Report: req, Delivery: PiEventDelivery{Status: status, MessageID: id, Context: "monitor:" + op.ID + ":" + id, Envelope: "Monitoring operation outcome (runtime-owned):\n\n" + string(body)}})
	return op.Validate()
}

type PiUtilityDomainGrant struct {
	Runtime                       piruntime.RuntimeRef
	Utility                       PiUtilityRecord
	CreationID, LaunchOperationID string
	Settings                      piruntime.EffectiveSettings
	Input                         piruntime.SendInput
}

// PiUtilityEffectVerifier is lock-safe central current package/session/input
// authority. It runs under writer admission without a monitor file lock.
type PiUtilityEffectVerifier interface {
	VerifyPiUtilityEffect(context.Context, piruntime.RuntimeRef, PiOperation, string) error
}
type PiDomainAuthority struct {
	Store     *Store
	Admission PiTeamWriteAdmission
	Verifier  PiUtilityEffectVerifier
}

func (a PiDomainAuthority) WithUtility(ctx context.Context, runtime piruntime.RuntimeRef, utility PiUtilityRecord, action string, run func(PiUtilityDomainGrant) error) error {
	if a.Store == nil || a.Verifier == nil || run == nil {
		return piruntime.ErrUnavailable
	}
	switch action {
	case "create", "load", "bind", "send", "unload":
	default:
		return piruntime.ErrScope
	}
	return WithPiRecordWrite(ctx, a.Admission, utility.Session, runtime.Generation, func() error {
		op, err := a.Store.Get(ctx, utility.OperationID)
		if err != nil {
			return err
		}
		if op.Pi == nil || op.Pi.Launch.Utility != utility || runtime.Session != utility.Session.Ref() || op.Pi.CanceledAt != nil && action != "unload" {
			return piruntime.ErrScope
		}
		p := *op.Pi
		if p.Launch.State == "terminated" || p.Settlement != nil && p.Settlement.State == "settled" {
			return piruntime.ErrHeld
		}
		if err := a.Verifier.VerifyPiUtilityEffect(ctx, runtime, p, action); err != nil {
			return err
		}
		if action == "create" {
			// Save monotone create intent before the native callback, then close
			// the short file lock so callbacks cannot reenter a locked store.
			tx, err := a.Store.Lock(ctx, p.ID)
			if err != nil {
				return err
			}
			if tx.Operation.Pi == nil || !reflect.DeepEqual(*tx.Operation.Pi, p) {
				tx.Close()
				return piruntime.ErrConflict
			}
			tx.Operation.Pi.Launch.CreateAttempted = true
			tx.Operation.Pi.Launch.State = "creating"
			tx.piWrite = true
			err = tx.Save()
			tx.Close()
			if err != nil {
				return err
			}
		}
		if action == "send" && (p.Launch.Input == nil || p.Launch.Outcome != "" || p.Launch.State != "submitting") {
			return piruntime.ErrHeld
		}
		return run(PiUtilityDomainGrant{Runtime: runtime, Utility: utility, CreationID: p.Launch.CreationID, LaunchOperationID: p.Launch.MessageID, Settings: p.Launch.Settings.Runtime(), Input: piruntime.SendInput{Message: p.Launch.Envelope, ActionContext: "monitor-launch:" + p.ID}})
	})
}

type PiReportService struct {
	Store         *Store
	Admission     PiTeamWriteAdmission
	ReadAdmission PiUtilityReadAdmission
}

type PiUtilityReadAdmission interface {
	WithPiUtilityRead(context.Context, PiUtilityRecord, string, string, func() error) error
}

// Status is observational and remains available to an authorized exact utility
// even when its original operation is held. No delivery/idle gate or wakeup.
func (s PiReportService) Status(ctx context.Context, caller PiUtilityRecord, inputOperationID, generation string) (PiOperation, error) {
	if s.Store == nil || s.ReadAdmission == nil {
		return PiOperation{}, piruntime.ErrUnavailable
	}
	if caller.Validate(true) != nil || !validID(inputOperationID) || !piUtilityHex.MatchString(generation) {
		return PiOperation{}, piruntime.ErrScope
	}
	var result PiOperation
	called := false
	var readErr error
	err := s.ReadAdmission.WithPiUtilityRead(ctx, caller, inputOperationID, generation, func() error {
		if called {
			return piruntime.ErrConflict
		}
		called = true
		op, err := s.Store.Get(ctx, caller.OperationID)
		if err != nil {
			readErr = err
			return err
		}
		if op.Pi == nil || op.Pi.Launch.Utility != caller || op.Pi.Launch.Input == nil || op.Pi.Launch.Input.OperationID != inputOperationID || op.Pi.Launch.Generation != generation {
			readErr = piruntime.ErrScope
			return readErr
		}
		result = *op.Pi
		return nil
	})
	if readErr != nil {
		return PiOperation{}, readErr
	}
	if !called && err == nil {
		err = piruntime.ErrUnavailable
	}
	return result, err
}

func (s PiReportService) Report(ctx context.Context, scope PiSessionRecord, generation string, caller PiUtilityRecord, inputOperationID string, req ReportRequest, at time.Time) (string, error) {
	if s.Store == nil {
		return "", piruntime.ErrUnavailable
	}
	err := s.Store.UpdatePi(ctx, s.Admission, scope, generation, caller.OperationID, func(op *PiOperation) error {
		if op.Launch.Utility != caller || caller.Validate(true) != nil || op.Launch.Generation != generation || op.Launch.Outcome != "accepted" || op.Launch.Input == nil || op.Launch.Input.OperationID != inputOperationID {
			return piruntime.ErrScope
		}
		return AddPiEvent(op, req, at)
	})
	if err != nil {
		return "", err
	}
	return derivedID(caller.OperationID, req.Slot), nil
}

type PiSettlementObservation struct {
	SourceTerminal, InputKnown, ResourcesDrained, CallbacksDrained, ToolsDrained, UtilityExited, CompleteInventory bool
	SourceEvidence, ToolEvidence                                                                                   []string
}
type PiSettlementVerifier interface {
	ObservePiSettlement(context.Context, PiOperation) (PiSettlementObservation, error)
}

func (s PiReportService) ReconcileSettlement(ctx context.Context, scope PiSessionRecord, generation, id string, proof PiSettlementVerifier, at time.Time) error {
	if s.Store == nil || proof == nil {
		return piruntime.ErrUnavailable
	}
	return WithPiRecordWrite(ctx, s.Admission, scope, generation, func() error {
		// Observe under current runtime admission but without a monitor file lock.
		// The full snapshot is compared after the observation before publication.
		snapshot, err := s.Store.Get(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.Pi == nil || snapshot.Pi.Owner.Session != scope {
			return piruntime.ErrScope
		}
		observed, err := proof.ObservePiSettlement(ctx, *snapshot.Pi)
		if err != nil {
			return err
		}
		tx, err := s.Store.Lock(ctx, id)
		if err != nil {
			return err
		}
		defer tx.Close()
		op := tx.Operation.Pi
		if op == nil {
			return piruntime.ErrScope
		}
		if !reflect.DeepEqual(op, snapshot.Pi) {
			return piruntime.ErrConflict
		}
		complete := observed.SourceTerminal && observed.InputKnown && observed.ResourcesDrained && observed.CallbacksDrained && observed.ToolsDrained && observed.UtilityExited && observed.CompleteInventory && op.Launch.Outcome == "accepted" && op.Launch.TerminatedAt != nil
		terminal := ""
		for _, e := range op.Events {
			if e.Report.Slot == "terminal" {
				terminal = e.ID
			}
		}
		if terminal == "" {
			complete = false
		}
		state := "incomplete"
		if complete {
			state = "settled"
		}
		op.Settlement = &PiSettlementRecord{State: state, SourceEvidence: observed.SourceEvidence, ToolEvidence: observed.ToolEvidence, ReportEventID: terminal, ObservedGeneration: generation, ObservedAt: at}
		tx.piWrite = true
		return tx.Save()
	})
}

// CancelPi revokes continuation only. It never invokes unload/abort, changes an
// input receipt, declares settlement, or drops a pending command obligation.
func (s PiReportService) CancelPi(ctx context.Context, scope PiSessionRecord, generation, id string, at time.Time) error {
	if s.Store == nil {
		return piruntime.ErrUnavailable
	}
	return s.Store.UpdatePi(ctx, s.Admission, scope, generation, id, func(op *PiOperation) error {
		if op.CanceledAt == nil {
			op.CanceledAt = &at
		}
		for i := range op.Events {
			d := &op.Events[i].Delivery
			if d.PiDelivery == nil && d.Status != "accepted" {
				d.Status = "canceled"
			}
		}
		return nil
	})
}

// ReserveUtilityInput stores the exact P3-produced ref before actual Send.
// UtilitySubmissionDigest construction stays in P3; presentation text is never
// used here to invent a receipt digest. A retained ref/placeholder native ID
// cannot satisfy this explicit utility arm.
func (s PiReportService) ReserveUtilityInput(ctx context.Context, scope PiSessionRecord, generation, id string, input PiUtilityInputRecord) error {
	if s.Store == nil {
		return piruntime.ErrUnavailable
	}
	return s.Store.UpdatePi(ctx, s.Admission, scope, generation, id, func(op *PiOperation) error {
		if input.Validate() != nil || input.Utility != op.Launch.Utility || input.OperationID != op.Launch.MessageID || input.Action != "send" || !op.Launch.CreateAttempted {
			return piruntime.ErrScope
		}
		if op.Launch.Input != nil {
			if !reflect.DeepEqual(op.Launch.Input, &input) {
				return piruntime.ErrConflict
			}
			return nil
		}
		if op.Launch.State != "initialized" {
			return piruntime.ErrHeld
		}
		op.Launch.Input = &input
		op.Launch.State = "submitting"
		op.Launch.Generation = generation
		return nil
	})
}
