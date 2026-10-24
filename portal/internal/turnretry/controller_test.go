package turnretry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

type fakeBackend struct {
	o                Observation
	boundary         string
	hold             bool
	sends            int
	accepted         string
	unknown          bool
	never            bool
	observed         int
	duringEnrollment bool
}

func (f *fakeBackend) Observe(context.Context, Scope) (Observation, error) {
	f.observed++
	if f.duringEnrollment && f.observed == 2 {
		return Observation{"initial", "failed", "serverOverloaded"}, nil
	}
	return f.o, nil
}
func (f *fakeBackend) Boundary(context.Context, Scope) (string, error) { return f.boundary, nil }
func (f *fakeBackend) Prove(_ context.Context, _ Scope, s *State, turn string) error {
	s.AddSnapshot(proof(turn))
	return nil
}
func (f *fakeBackend) Check(context.Context, Scope, Snapshot) error {
	if f.hold {
		return errors.New("execution held")
	}
	return nil
}
func (f *fakeBackend) Reconcile(context.Context, Scope, Pending) (codex.SendReceipt, bool, error) {
	if f.accepted != "" {
		return codex.SendReceipt{TurnID: f.accepted}, true, nil
	}
	if f.unknown && f.sends > 0 {
		return codex.SendReceipt{}, false, &codex.UnknownSendOutcomeError{Err: errors.New("lost response")}
	}
	return codex.SendReceipt{}, false, nil
}
func (f *fakeBackend) Continue(context.Context, Scope, Pending) (codex.SendReceipt, error) {
	f.sends++
	if f.unknown {
		return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: errors.New("lost response")}
	}
	f.accepted = "continued"
	return codex.SendReceipt{TurnID: f.accepted}, nil
}
func (f *fakeBackend) NeverSubmitted(context.Context, Scope, Pending) (bool, error) {
	return f.never, nil
}

func controllerFixture(t *testing.T) (Controller, *fakeBackend, *time.Time) {
	t.Helper()
	now := time.Unix(1000, 0)
	backend := &fakeBackend{boundary: "incarnation", never: true}
	c := Controller{Policy: Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: Store{Root: t.TempDir(), Workspace: "/workspace"}, Backend: backend, Clock: func() time.Time { return now }}
	return c, backend, &now
}

func TestControllerRestartReconcilesAcceptedBeforeNewerFailureAndCancellation(t *testing.T) {
	c, b, now := controllerFixture(t)
	scope := fixtureScope()
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	b.o = Observation{"failed", "failed", "cyberPolicy"}
	b.unknown = true
	if err := c.Step(context.Background(), scope); err == nil {
		t.Fatal("unknown outcome hidden")
	}
	s, _ := c.Store.Load(scope)
	if !s.Uncertain() || s.Attempt != 1 || len(s.Cyber) != 1 {
		t.Fatalf("lost uncertain reservation: %+v", s)
	}
	id := s.Pending.ID
	tx, _ := c.Store.Lock(context.Background(), scope)
	tx.State.Cancel("manual_input")
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	b.o = Observation{"newer", "failed", "cyberPolicy"}
	*now = now.Add(time.Second)
	if err := c.Step(context.Background(), scope); err == nil {
		t.Fatal("unknown outcome accepted")
	}
	if b.sends != 1 {
		t.Fatal("uncertain resend")
	}
	b.accepted = "continued"
	b.unknown = false
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	s, _ = c.Store.Load(scope)
	if s.Phase != "cancelled" || s.Pending.ID != id || s.Pending.ResultTurn != "continued" || b.sends != 1 {
		t.Fatalf("receipt/cancel lost: %+v", s)
	}
}

func TestControllerColdHoldDuplicatePollAndOverdueCapacity(t *testing.T) {
	c, b, now := controllerFixture(t)
	scope := fixtureScope()
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	b.o = Observation{"failed", "failed", "serverOverloaded"}
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	s, _ := c.Store.Load(scope)
	id, deadline := s.Pending.ID, s.DeadlineMS
	*now = now.Add(time.Hour)
	b.hold = true
	if err := c.Step(context.Background(), scope); err == nil {
		t.Fatal("cold hold ignored")
	}
	if b.sends != 0 {
		t.Fatal("held send")
	}
	s, _ = c.Store.Load(scope)
	if s.Pending.ID != id || s.DeadlineMS != deadline {
		t.Fatal("held schedule changed")
	}
	b.hold = false
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if b.sends != 1 {
		t.Fatal("overdue burst")
	}
	b.o = Observation{"continued", "inProgress", ""}
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if b.sends != 1 {
		t.Fatal("duplicate poll dispatched")
	}
}

func TestControllerEnrollmentCompletionRaceAndIncarnationFence(t *testing.T) {
	c, b, _ := controllerFixture(t)
	scope := fixtureScope()
	b.o = Observation{"initial", "inProgress", ""}
	b.duringEnrollment = true
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	s, _ := c.Store.Load(scope)
	if s.BaselineActive || s.Baseline != "initial" {
		t.Fatal("uncertain enrollment completion admitted")
	}
	b.duringEnrollment = false
	b.o = Observation{"initial", "failed", "serverOverloaded"}
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if b.sends != 0 {
		t.Fatal("old failure restarted")
	}
	b.boundary = "replacement"
	if err := c.Step(context.Background(), scope); err == nil {
		t.Fatal("incarnation replacement admitted")
	}
}
