package monitoroperation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type adapterGate struct {
	t       *testing.T
	runtime piruntime.RuntimeRef
	depth   int
	reads   int
	hold    bool
}

func (g *adapterGate) WithPiTeamWrite(ctx context.Context, session agentbackend.SessionRef, generation string, run func() error) error {
	if g.depth != 0 {
		g.t.Fatal("adapter reacquired outer admission")
	}
	if session != g.runtime.Session || generation != g.runtime.Generation {
		return piruntime.ErrScope
	}
	g.depth++
	defer func() { g.depth-- }()
	return run()
}
func (g *adapterGate) WithUtilityRead(ctx context.Context, runtime piruntime.RuntimeRef, ref piruntime.UtilitySubmissionRef, read func() error) error {
	if g.depth != 0 {
		g.t.Fatal("Lookup entered under effect admission")
	}
	if runtime != g.runtime || ref.Utility.Session != runtime.Session {
		return piruntime.ErrScope
	}
	g.reads++
	return read()
}
func (g *adapterGate) VerifyPiUtilityEffect(_ context.Context, runtime piruntime.RuntimeRef, op PiOperation, action string) error {
	if g.depth != 1 || runtime != g.runtime || op.Owner.Session.Ref() != runtime.Session {
		return piruntime.ErrScope
	}
	if g.hold {
		return piruntime.ErrHeld
	}
	return nil
}

// This is an interface fault fixture, not a runtime/receipt implementation.
// The actual Manager/Unix/SDK engine is verified in piruntime's owner harness.
type utilityControllerFake struct {
	t                           *testing.T
	adapter                     *PiRuntimeAdapter
	gate                        *adapterGate
	runtime                     piruntime.RuntimeRef
	utility                     piruntime.UtilityRef
	settings                    piruntime.EffectiveSettings
	input                       piruntime.SendInput
	phase                       string
	status                      piruntime.UtilitySubmissionStatus
	creates, binds, sends       int
	loads, lookups, unloads     int
	createErr, bindErr, sendErr error
	lookupErr                   error
	mutateStatus                func(*piruntime.UtilitySubmissionStatus)
	seenRef                     piruntime.UtilitySubmissionRef
	afterLookup                 func()
	mutateCreate                func(*piruntime.UtilityState)
}

func (f *utilityControllerFake) outsideAdmission() {
	if f.gate.depth != 0 {
		f.t.Fatal("Controller invoked with outer admission already held")
	}
}
func (f *utilityControllerFake) CreateUtility(ctx context.Context, runtime piruntime.RuntimeRef, req piruntime.UtilityCreateRequest) (piruntime.UtilityState, error) {
	f.outsideAdmission()
	var state piruntime.UtilityState
	err := f.adapter.WithUtility(ctx, runtime, req.Utility, "create", func(g piruntime.UtilityGrant) error {
		f.creates++
		if g.Utility != req.Utility || g.Settings != req.Settings || req.CreationID != g.CreationID || g.Input.Retry || g.Input.Message == "" {
			f.t.Fatal("create altered original grant")
		}
		op, err := f.adapter.Domain.Store.Get(ctx, req.Utility.OperationID)
		if err != nil || !op.Pi.Launch.CreateAttempted {
			f.t.Fatal("create effect preceded durable intent", err)
		}
		f.utility, f.settings, f.input = req.Utility, g.Settings, g.Input
		f.utility.NativeID = "12345678-1234-4234-8234-123456789abc"
		state = piruntime.UtilityState{Utility: f.utility, Generation: runtime.Generation, State: "initialized", Settings: req.Settings}
		if f.mutateCreate != nil {
			f.mutateCreate(&state)
		}
		return f.createErr
	})
	return state, err
}
func (f *utilityControllerFake) LoadUtility(context.Context, piruntime.UtilityExecutionHandle) (piruntime.UtilityState, error) {
	f.loads++
	return piruntime.UtilityState{}, piruntime.ErrUnavailable
}
func (f *utilityControllerFake) BindUtility(ctx context.Context, handle piruntime.UtilityExecutionHandle) (backend.Operations, error) {
	f.outsideAdmission()
	if f.bindErr != nil {
		return backend.Operations{}, f.bindErr
	}
	err := f.adapter.WithUtility(ctx, f.runtime, handle.Utility, "bind", func(g piruntime.UtilityGrant) error {
		f.binds++
		if g.Utility != f.utility || g.Settings != f.settings || g.Input != f.input || handle.Generation != f.runtime.Generation {
			return piruntime.ErrConflict
		}
		return nil
	})
	return backend.Operations{Verify: f, Send: f}, err
}
func (f *utilityControllerFake) Verify(_ context.Context, id backend.Identity) error {
	if id != piruntime.UtilityIdentity(piruntime.UtilityExecutionHandle{Utility: f.utility, Generation: f.runtime.Generation}) {
		return piruntime.ErrScope
	}
	return nil
}
func (f *utilityControllerFake) Send(ctx context.Context, id backend.Identity, req backend.SendRequest) (backend.SendResult, error) {
	f.outsideAdmission()
	err := f.adapter.WithUtility(ctx, f.runtime, f.utility, "send", func(g piruntime.UtilityGrant) error {
		if f.Verify(ctx, id) != nil || req.ClientUserMessageID != g.LaunchOperationID || req.Message != g.Input.Message || req.ActionContext != g.Input.ActionContext || req.Retry {
			return piruntime.ErrConflict
		}
		ref := piruntime.UtilitySubmissionRef{Utility: f.utility, OperationID: g.LaunchOperationID, Action: "send"}
		ref.Digest = piruntime.UtilitySubmissionDigest(ref, g.Input, g.Settings)
		op, err := f.adapter.Domain.Store.Get(ctx, f.utility.OperationID)
		if err != nil || op.Pi.Launch.Input == nil || op.Pi.Launch.Input.Ref() != ref || op.Pi.Launch.State != "submitting" {
			f.t.Fatal("Send preceded durable exact P3 input ref", err)
		}
		f.sends++
		f.status = piruntime.UtilitySubmissionStatus{Schema: 2, OwnerKind: "utility", Ref: ref, Generation: f.runtime.Generation, Phase: f.phase, Settings: g.Settings, Input: g.Input}
		if f.phase == "Accepted" {
			f.status.Witness = &piruntime.NativeWitness{SessionID: f.utility.NativeID, EntryID: "native-entry", EffectiveDigest: piruntime.MessageDigest(g.Input.Message)}
		} else if f.phase == "NotSubmitted" {
			f.status.Refusal = "before dispatch"
		}
		return f.sendErr
	})
	// This deliberately cannot establish acceptance without Lookup's witness.
	return backend.SendResult{Outcome: backend.Accepted}, err
}
func (f *utilityControllerFake) LookupUtility(ctx context.Context, ref piruntime.UtilitySubmissionRef) (piruntime.UtilitySubmissionStatus, error) {
	f.outsideAdmission()
	f.lookups++
	f.seenRef = ref
	var status piruntime.UtilitySubmissionStatus
	err := f.adapter.WithUtilityRead(ctx, f.runtime, ref, func() error {
		status = f.status
		if f.mutateStatus != nil {
			f.mutateStatus(&status)
		}
		return f.lookupErr
	})
	if err == nil && f.afterLookup != nil {
		f.afterLookup()
	}
	return status, err
}
func (f *utilityControllerFake) UnloadUtility(ctx context.Context, handle piruntime.UtilityExecutionHandle) (piruntime.StopResult, error) {
	f.outsideAdmission()
	err := f.adapter.WithUtility(ctx, f.runtime, handle.Utility, "unload", func(piruntime.UtilityGrant) error { f.unloads++; return nil })
	return piruntime.StopResult{Outcome: "stopped", VerifiedExit: err == nil}, err
}

func runtimeAdapterFixture(t *testing.T) (*PiRuntimeAdapter, *utilityControllerFake, PiOperation) {
	t.Helper()
	store, op, oldGate := piOperationFixture(t)
	runtime := piruntime.RuntimeRef{Session: op.Owner.Session.Ref(), Generation: oldGate.generation, InstanceID: "11111111-1111-4111-8111-111111111111", RequestID: "22222222-2222-4222-8222-222222222222", Digest: strings.Repeat("9", 64)}
	gate := &adapterGate{t: t, runtime: runtime}
	if _, err := store.ReservePi(context.Background(), gate, runtime.Generation, op); err != nil {
		t.Fatal(err)
	}
	a := &PiRuntimeAdapter{Domain: PiDomainAuthority{Store: store, Admission: gate, Verifier: gate}, ReadAdmission: gate}
	native := &utilityControllerFake{t: t, adapter: a, runtime: runtime, gate: gate, phase: "Accepted"}
	a.Controller = native
	return a, native, op
}

func TestPiRuntimeAdapterFreezesActualUtilityRefDigestAndSendsOnce(t *testing.T) {
	a, native, original := runtimeAdapterFixture(t)
	ctx := context.Background()
	if err := a.Launch(ctx, native.runtime, original.ID); err != nil {
		t.Fatal(err)
	}
	saved, _ := a.Domain.Store.Get(ctx, original.ID)
	ref := saved.Pi.Launch.Input.Ref()
	if saved.Pi.Launch.State != "running" || saved.Pi.Launch.Outcome != "accepted" || ref.Utility != native.utility || ref.OperationID != original.Launch.MessageID || ref.Digest != piruntime.UtilitySubmissionDigest(ref, native.input, native.settings) {
		t.Fatal("missing original utility/witness projection")
	}
	if err := a.Launch(ctx, native.runtime, original.ID); err != nil {
		t.Fatal(err)
	}
	if native.creates != 1 || native.binds != 1 || native.sends != 1 || native.loads != 0 || native.lookups != 2 || native.seenRef != ref {
		t.Fatal("replay performed another effect", native)
	}
	// Current generation authenticates reading, but does not enter stable input
	// identity or rewrite the original receipt's execution generation.
	native.runtime.Generation = strings.Repeat("8", 64)
	native.gate.runtime = native.runtime
	native.gate.hold = true
	if err := a.ReconcileLaunch(ctx, native.runtime, original.ID); err != nil {
		t.Fatal("held original receipt blocked by effect gate", err)
	}
	after, _ := a.Domain.Store.Get(ctx, original.ID)
	if after.Pi.Launch.Input.Ref() != ref || after.Pi.Launch.Generation != original.Owner.Generation || native.sends != 1 {
		t.Fatal("read reminted original ref/generation")
	}
	if err := a.WithUtilityRead(ctx, native.runtime, piruntime.UtilitySubmissionRef{Utility: ref.Utility, OperationID: ref.OperationID, Action: ref.Action, Digest: strings.Repeat("f", 64)}, func() error { t.Fatal("wrong ref entered read"); return nil }); !errors.Is(err, piruntime.ErrScope) {
		t.Fatal(err)
	}
}

func TestPiRuntimeAdapterUnknownNegativeAndLostRepliesNeverResend(t *testing.T) {
	for _, phase := range []string{"Unknown", "NotSubmitted", "Accepted"} {
		t.Run(phase, func(t *testing.T) {
			a, native, op := runtimeAdapterFixture(t)
			native.phase = phase
			native.sendErr = errors.New("lost Send reply")
			if err := a.Launch(context.Background(), native.runtime, op.ID); err == nil {
				t.Fatal("lost reply was hidden")
			}
			for i := 0; i < 2; i++ {
				_ = a.Launch(context.Background(), native.runtime, op.ID)
			}
			if native.sends != 1 || native.creates != 1 || native.binds != 1 || native.loads != 0 {
				t.Fatal("terminal/unknown replay sent", native)
			}
			if phase == "Unknown" {
				native.status.Phase = "Accepted"
				native.status.Witness = &piruntime.NativeWitness{SessionID: native.utility.NativeID, EntryID: "original-native-entry", EffectiveDigest: piruntime.MessageDigest(native.input.Message)}
				if err := a.ReconcileLaunch(context.Background(), native.runtime, op.ID); err != nil {
					t.Fatal("original positive Lookup could not resolve lost ACK", err)
				}
			}
		})
	}
	a, native, op := runtimeAdapterFixture(t)
	native.createErr = errors.New("lost Create reply")
	for i := 0; i < 2; i++ {
		if err := a.Launch(context.Background(), native.runtime, op.ID); err == nil {
			t.Fatal("lost mapping became success")
		}
	}
	if native.creates != 1 || native.sends != 0 {
		t.Fatal("lost create response created a replacement")
	}
}

func TestPiRuntimeAdapterRequiresAuthorityAndExactUtilityReceipt(t *testing.T) {
	for _, missing := range []string{"writer", "verifier", "reader", "controller"} {
		t.Run(missing, func(t *testing.T) {
			a, native, op := runtimeAdapterFixture(t)
			switch missing {
			case "writer":
				a.Domain.Admission = nil
			case "verifier":
				a.Domain.Verifier = nil
			case "reader":
				a.ReadAdmission = nil
			case "controller":
				a.Controller = nil
			}
			if err := a.Launch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrUnavailable) {
				t.Fatal(err)
			}
			if native.creates != 0 || native.sends != 0 {
				t.Fatal("missing authority caused an effect")
			}
		})
	}
	a, native, op := runtimeAdapterFixture(t)
	native.gate.hold = true
	if err := a.Launch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrHeld) || native.creates != 0 || native.sends != 0 {
		t.Fatal("held operation caused an effect", err)
	}
	native.gate.hold = false
	native.gate.runtime.Generation = strings.Repeat("f", 64)
	if err := a.Launch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrScope) || native.creates != 0 {
		t.Fatal("stale current generation caused an effect", err)
	}
	for _, field := range []string{"identity", "generation", "settings", "persisted"} {
		t.Run("create-"+field, func(t *testing.T) {
			a, native, op := runtimeAdapterFixture(t)
			native.mutateCreate = func(s *piruntime.UtilityState) {
				switch field {
				case "identity":
					s.Utility.Incarnation = strings.Repeat("f", 64)
				case "generation":
					s.Generation = strings.Repeat("f", 64)
				case "settings":
					s.Settings.Model = "different-model"
				case "persisted":
					s.Persisted = true
				}
			}
			if err := a.Launch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrConflict) || native.sends != 0 {
				t.Fatal("create reply changed original mapping or started input", err)
			}
		})
	}
	for _, field := range []string{"owner", "root", "generation", "input", "settings", "witness"} {
		t.Run(field, func(t *testing.T) {
			a, native, op := runtimeAdapterFixture(t)
			native.mutateStatus = func(s *piruntime.UtilitySubmissionStatus) {
				switch field {
				case "owner":
					s.OwnerKind, s.Schema = "retained", 1
				case "root":
					s.Ref.Utility.Incarnation = strings.Repeat("7", 64)
				case "generation":
					s.Generation = strings.Repeat("7", 64)
				case "input":
					s.Input.Message += "changed"
				case "settings":
					s.Settings.Model += "changed"
				case "witness":
					s.Witness = nil
				}
			}
			if err := a.Launch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrHeld) {
				t.Fatal("forged acceptance", err)
			}
			saved, _ := a.Domain.Store.Get(context.Background(), op.ID)
			if saved.Pi.Launch.Outcome != "unknown" || native.sends != 1 {
				t.Fatal("unproved native result became Accepted")
			}
		})
	}
}

func TestPiRuntimeAdapterOriginalTerminalLookupAndStalePublication(t *testing.T) {
	for _, terminal := range []string{"Accepted", "NotSubmitted"} {
		t.Run(terminal, func(t *testing.T) {
			a, native, op := runtimeAdapterFixture(t)
			native.phase = "Unknown"
			if err := a.Launch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrHeld) {
				t.Fatal(err)
			}
			ref := native.seenRef
			native.status.Phase = terminal
			want := "not_submitted"
			if terminal == "Accepted" {
				want = "accepted"
				native.status.Witness = &piruntime.NativeWitness{SessionID: native.utility.NativeID, EntryID: "original-entry", EffectiveDigest: piruntime.MessageDigest(native.input.Message)}
			} else {
				native.status.Refusal = "positive original before-dispatch refusal"
			}
			if err := a.ReconcileLaunch(context.Background(), native.runtime, op.ID); err != nil {
				t.Fatal(err)
			}
			saved, _ := a.Domain.Store.Get(context.Background(), op.ID)
			if saved.Pi.Launch.Outcome != want || saved.Pi.Launch.State != "held" || saved.Pi.Launch.Input.Ref() != ref || saved.Pi.Settlement != nil {
				t.Fatal("lookup inferred execution/settlement or replaced original ref")
			}
			if terminal == "Accepted" {
				native.status.Phase, native.status.Witness, native.status.Refusal = "NotSubmitted", nil, "contradictory refusal"
			} else {
				native.status.Phase, native.status.Refusal = "Accepted", ""
				native.status.Witness = &piruntime.NativeWitness{SessionID: native.utility.NativeID, EntryID: "contradictory-entry", EffectiveDigest: piruntime.MessageDigest(native.input.Message)}
			}
			if err := a.ReconcileLaunch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrConflict) {
				t.Fatal("terminal outcome changed", err)
			}
			if native.sends != 1 || native.creates != 1 || native.binds != 1 || native.loads != 0 {
				t.Fatal("lookup performed an effect")
			}
		})
	}
	a, native, op := runtimeAdapterFixture(t)
	native.phase = "Unknown"
	_ = a.Launch(context.Background(), native.runtime, op.ID)
	native.status.Phase = "NotSubmitted"
	native.status.Refusal = "positive original refusal"
	native.afterLookup = func() {
		native.afterLookup = nil
		if err := (PiReportService{Store: a.Domain.Store, Admission: a.Domain.Admission}).CancelPi(context.Background(), op.Owner.Session, native.runtime.Generation, op.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ReconcileLaunch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrConflict) {
		t.Fatal("stale tuple published", err)
	}
	saved, _ := a.Domain.Store.Get(context.Background(), op.ID)
	if saved.Pi.CanceledAt == nil || saved.Pi.Launch.Outcome != "unknown" || native.sends != 1 {
		t.Fatal("stale lookup erased cancellation/original input")
	}
}

func TestPiRuntimeAdapterLostBindAndEarlyReportUseOriginalReadOnlyRef(t *testing.T) {
	a, native, op := runtimeAdapterFixture(t)
	native.bindErr = piruntime.ErrUnavailable
	if err := a.Launch(context.Background(), native.runtime, op.ID); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatal(err)
	}
	saved, _ := a.Domain.Store.Get(context.Background(), op.ID)
	ref := saved.Pi.Launch.Input.Ref()
	native.bindErr = nil
	_ = a.Launch(context.Background(), native.runtime, op.ID)
	if native.seenRef != ref || native.sends != 0 || native.creates != 1 || native.binds != 0 || native.loads != 0 {
		t.Fatal("lost Bind was retried as capability guess")
	}

	a, native, op = runtimeAdapterFixture(t)
	native.lookupErr = errors.New("domain projection not yet available")
	if err := a.Launch(context.Background(), native.runtime, op.ID); err == nil {
		t.Fatal("missing Lookup proof became accepted")
	}
	native.lookupErr = nil
	req := ReportRequest{Slot: "terminal", Outcome: "passed", Conclusion: "success", ElapsedMillis: 1}
	at := time.Now().UTC()
	event, err := a.Report(context.Background(), native.runtime, native.utility, op.Launch.MessageID, req, at)
	if err != nil || event != derivedID(op.ID, "terminal") {
		t.Fatal("early original report waited for Send projection", err)
	}
	again, err := a.Report(context.Background(), native.runtime, native.utility, op.Launch.MessageID, req, at)
	if err != nil || again != event {
		t.Fatal("report retry changed original event", err)
	}
	reads := native.lookups
	if _, err := a.Report(context.Background(), native.runtime, native.utility, op.Launch.MessageID, ReportRequest{}, at); err == nil || native.lookups != reads {
		t.Fatal("invalid report entered native lookup")
	}
	saved, _ = a.Domain.Store.Get(context.Background(), op.ID)
	if saved.Pi.Launch.Outcome != "accepted" || saved.Pi.Launch.State != "held" || saved.Pi.Settlement != nil || native.sends != 1 || len(saved.Pi.Events) != 1 {
		t.Fatal("report receipt inferred running/drain or resent input")
	}
}

func TestPiRuntimeAdapterDurableReportAndExitStillRequireCompleteDrain(t *testing.T) {
	a, native, op := runtimeAdapterFixture(t)
	ctx := context.Background()
	if err := a.Launch(ctx, native.runtime, op.ID); err != nil {
		t.Fatal(err)
	}
	saved, _ := a.Domain.Store.Get(ctx, op.ID)
	service := PiReportService{Store: a.Domain.Store, Admission: a.Domain.Admission}
	at := time.Now().UTC()
	req := ReportRequest{Slot: "terminal", Outcome: "passed", Conclusion: "success", ElapsedMillis: 1}
	first, err := service.Report(ctx, op.Owner.Session, native.runtime.Generation, saved.Pi.Launch.Utility, op.Launch.MessageID, req, at)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Report(ctx, op.Owner.Session, native.runtime.Generation, saved.Pi.Launch.Utility, op.Launch.MessageID, req, at)
	if err != nil || first != second {
		t.Fatal("report replaced original event", err)
	}
	if err := service.CancelPi(ctx, op.Owner.Session, native.runtime.Generation, op.ID, at); err != nil {
		t.Fatal(err)
	}
	if native.unloads != 0 {
		t.Fatal("cancel stopped command")
	}
	if err := a.Unload(ctx, native.runtime, op.ID, time.Time{}); !errors.Is(err, piruntime.ErrScope) || native.unloads != 0 {
		t.Fatal("invalid unload request reached native effect", err)
	}
	if err := a.Unload(ctx, native.runtime, op.ID, at); err != nil {
		t.Fatal(err)
	}
	proof := piSettlementFake{value: PiSettlementObservation{SourceTerminal: true, InputKnown: true, ResourcesDrained: true, ToolsDrained: true, UtilityExited: true, CompleteInventory: true, SourceEvidence: []string{"original-source"}, ToolEvidence: []string{"native-tools"}}}
	if err := service.ReconcileSettlement(ctx, op.Owner.Session, native.runtime.Generation, op.ID, &proof, at); err != nil {
		t.Fatal(err)
	}
	held, _ := a.Domain.Store.Get(ctx, op.ID)
	if held.Pi.Settlement.State != "incomplete" || !operationActive(held) {
		t.Fatal("report/exit bypassed callback drain")
	}
	proof.value.CallbacksDrained = true
	if err := service.ReconcileSettlement(ctx, op.Owner.Session, native.runtime.Generation, op.ID, &proof, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	final, _ := a.Domain.Store.Get(ctx, op.ID)
	if final.Pi.Settlement.State != "settled" || operationActive(final) || !reflect.DeepEqual(final.Pi.Launch.Input, held.Pi.Launch.Input) || native.sends != 1 {
		t.Fatal("complete drain changed original input or failed to settle")
	}
}
