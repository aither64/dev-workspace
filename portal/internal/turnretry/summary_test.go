package turnretry

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

type checkpointBackend struct {
	fakeBackend
	receipts    map[string]string
	afterNative func()
}

func (b *checkpointBackend) Reconcile(_ context.Context, _ Scope, p Pending) (codex.SendReceipt, bool, error) {
	turn, ok := b.receipts[p.ID]
	return codex.SendReceipt{TurnID: turn}, ok, nil
}
func (b *checkpointBackend) Continue(_ context.Context, _ Scope, p Pending) (codex.SendReceipt, error) {
	b.sends++
	turn := "accepted-" + p.ID
	b.receipts[p.ID] = turn
	if b.afterNative != nil {
		b.afterNative()
	}
	return codex.SendReceipt{TurnID: turn}, nil
}

func TestSummaryReceiptFirstFailureAndRestartBeforeReplacingPending(t *testing.T) {
	ctx := context.Background()
	store, identity := policyFixture(t)
	createPolicy(t, store, identity)
	backend := &checkpointBackend{fakeBackend: fakeBackend{boundary: identity.Incarnation, never: true}, receipts: map[string]string{}}
	now := time.Unix(1000, 0)
	ledger := Store{Root: store.Root, Workspace: store.Workspace}
	scope := fixtureScope()
	tx, err := ledger.Lock(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Boundary = identity.Incarnation
	tx.State.Enroll(Observation{})
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	c := Controller{Store: ledger, Backend: backend, Policy: SessionPolicy{Store: store, Scope: identity}, Clock: func() time.Time { return now }}
	backend.o = Observation{Turn: "failed", Status: "failed", Code: "cyberPolicy"}
	backend.afterNative = func() {
		if err := os.Chmod(store.directory(), 0500); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Step(ctx, scope); err != nil {
		t.Fatal("summary failure rolled back accepted result", err)
	}
	state, err := ledger.Load(scope)
	if err != nil || state.Phase != "running" || state.Pending.ResultTurn == "" || backend.sends != 1 {
		t.Fatal("ledger acceptance missing", state, err)
	}
	original := *state.Pending
	charges := append([]Charge{}, state.Cyber...)
	backend.o = Observation{Turn: state.Pending.ResultTurn, Status: "failed", Code: "cyberPolicy"}
	if err := c.Step(ctx, scope); err == nil {
		t.Fatal("uncheckpointed accepted Pending replaced")
	}
	after, _ := ledger.Load(scope)
	if backend.sends != 1 || !reflect.DeepEqual(original, *after.Pending) || !reflect.DeepEqual(charges, after.Cyber) {
		t.Fatal("summary failure changed protected receipt or charge")
	}
	if err := os.Chmod(store.directory(), 0700); err != nil {
		t.Fatal(err)
	}
	backend.afterNative = nil
	// A restarted controller first counts the old exact ID and only then may
	// replace it and accept another. Quota remains its independent ledger.
	restarted := Controller{Store: ledger, Backend: backend, Policy: SessionPolicy{Store: store, Scope: identity}, Clock: c.Clock}
	if err := restarted.Step(ctx, scope); err != nil {
		t.Fatal(err)
	}
	p, err := store.Load(identity)
	if err != nil {
		t.Fatal(err)
	}
	summary := p.Thread("root").Summary
	if backend.sends != 2 || summary.ConfirmedStarts == nil || *summary.ConfirmedStarts != 2 || summary.CountQuality != "exact" {
		t.Fatal("restart count is not receipt-exact", backend.sends, summary)
	}
	before := *summary
	if err := restarted.Step(ctx, scope); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Load(identity)
	if *p.Thread("root").Summary.ConfirmedStarts != *before.ConfirmedStarts || backend.sends != 2 {
		t.Fatal("poll counted another start")
	}
}
