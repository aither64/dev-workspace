package inputdelivery

import (
	"context"
	"errors"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
)

type dispatchFixture struct {
	dispatches, lookups int
	before              error
	after               error
	onNative            func()
	lookupResult        Result
}

func (f *dispatchFixture) Dispatch(ctx context.Context, r Record, claim func(ExecutionEvidence) error) (Result, error) {
	if f.before != nil {
		return Result{}, f.before
	}
	if err := claim(ExecutionEvidence{Runtime: r.Admission.Selection.Runtime, Handle: r.Admission.Selection.Handle}); err != nil {
		return Result{}, err
	}
	f.dispatches++
	if f.onNative != nil {
		f.onNative()
	}
	return Result{State: "sent", Receipt: &backend.SendReceipt{ClientUserMessageID: r.Status.ClientUserMessageID, TurnID: "native-entry"}}, f.after
}
func (f *dispatchFixture) Lookup(context.Context, Record) (Result, error) {
	f.lookups++
	return f.lookupResult, nil
}

func TestDispatchRestartReconcilesOriginalWithoutReplay(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	store, err := Open(ctx, t.TempDir(), workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	r, err := store.Admit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	f := &dispatchFixture{after: errors.New("lost native reply")}
	d := &Dispatcher{ctx: ctx, store: store, driver: f}
	d.process(r)
	r, err = store.Get(ctx, a.Scope, a.Ref.OperationID)
	if err != nil || r.Status.State != "unknown" {
		t.Fatal(r, err)
	}
	d.process(r)
	if f.dispatches != 1 || f.lookups != 1 {
		t.Fatal("replayed unknown", f)
	}
	f.lookupResult = Result{State: "sent", Receipt: &backend.SendReceipt{ClientUserMessageID: a.Ref.OperationID, TurnID: "native-entry"}}
	d.process(r)
	r, err = store.Get(ctx, a.Scope, a.Ref.OperationID)
	if err != nil || r.Status.State != "sent" || f.dispatches != 1 {
		t.Fatal(r, err, f)
	}
}

func TestClaimBeforeWireCrashReopensLookupOnly(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	store, err := Open(ctx, root, workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	r, err := store.Admit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	original := Digest(r.Admission)
	r = claimRecord(r)
	// The durable claim is the crash boundary, even before a native socket call.
	if won, e := store.Update(ctx, r, "queued"); e != nil || !won {
		t.Fatal(won, e)
	}
	store.Close(ctx)
	store, err = Open(ctx, root, workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	r, err = store.Get(ctx, a.Scope, a.Ref.OperationID)
	if err != nil || r.Status.State != "dispatching" {
		t.Fatal(r, err)
	}
	f := &dispatchFixture{lookupResult: Result{State: "unknown"}}
	d := &Dispatcher{ctx: ctx, store: store, driver: f}
	d.process(r)
	r, err = store.Get(ctx, a.Scope, a.Ref.OperationID)
	if err != nil || r.Status.State != "unknown" || f.dispatches != 0 || f.lookups != 1 || Digest(r.Admission) != original {
		t.Fatal(r, err, f)
	}
}

func TestAcceptedNativeWithFailedQueueCommitUsesOriginalLookup(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	deny := false
	gate := func(ctx context.Context, run func() error) error {
		if deny {
			return errors.New("transition unavailable")
		}
		return testGate(ctx, run)
	}
	store, err := Open(ctx, t.TempDir(), workspace, gate)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	r, err := store.Admit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	f := &dispatchFixture{onNative: func() { deny = true }}
	d := &Dispatcher{ctx: ctx, store: store, driver: f}
	d.process(r)
	deny = false
	r, err = store.Get(ctx, a.Scope, a.Ref.OperationID)
	if err != nil || r.Status.State != "dispatching" {
		t.Fatal(r, err)
	}
	f.lookupResult = Result{State: "sent", Receipt: &backend.SendReceipt{ClientUserMessageID: a.Ref.OperationID, TurnID: "native-entry"}}
	d.process(r)
	r, err = store.Get(ctx, a.Scope, a.Ref.OperationID)
	if err != nil || r.Status.State != "sent" || f.dispatches != 1 || f.lookups != 1 {
		t.Fatal(r, err, f)
	}
}

func TestPositivePreEffectFailureNeverCallsNative(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	store, err := Open(ctx, t.TempDir(), workspace, testGate)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	r, err := store.Admit(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	f := &dispatchFixture{before: &PreDispatchError{Terminal: true, Reason: "The selected settings changed."}}
	d := &Dispatcher{ctx: ctx, store: store, driver: f}
	d.process(r)
	r, err = store.Get(ctx, a.Scope, a.Ref.OperationID)
	if err != nil || r.Status.State != "not_sent" || f.dispatches != 0 {
		t.Fatal(r, err, f)
	}
	due, err := store.Due(ctx, "", 4)
	if err != nil || len(due) != 0 {
		t.Fatal(due, err)
	}
}
