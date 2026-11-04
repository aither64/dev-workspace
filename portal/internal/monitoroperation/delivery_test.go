package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

type deliveryFixtureTransport struct {
	store        *Store
	t            *testing.T
	held         string
	sendErr      error
	reconcileErr error
	found        bool
	queueOnly    bool
	sends        int
	prepares     int
	reconciles   int
	sent         Event
}

func (f *deliveryFixtureTransport) Ready(context.Context, Owner) (string, error) { return f.held, nil }
func (f *deliveryFixtureTransport) Prepare(context.Context, Owner) (codex.TurnOptions, error) {
	f.prepares++
	return codex.TurnOptions{Model: "retained-model", ReasoningEffort: "xhigh"}, nil
}
func (f *deliveryFixtureTransport) Send(_ context.Context, _ Owner, event Event) (codex.SendReceipt, error) {
	f.sends++
	f.sent = event
	// Check the actual durable boundary without reacquiring the transaction lock.
	path, _ := f.store.path(fixtureOperationID)
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	var saved Operation
	if err := json.Unmarshal(data, &saved); err != nil {
		f.t.Fatal(err)
	}
	if saved.Events[0].Delivery.Status != "submitting" || saved.Events[0].Delivery.Options == nil {
		f.t.Fatal("native send preceded durable options/submission intent")
	}
	return f.receipt(event), f.sendErr
}
func (f *deliveryFixtureTransport) receipt(event Event) codex.SendReceipt {
	r := codex.SendReceipt{ClientUserMessageID: event.Delivery.MessageID, TurnID: "lead-turn"}
	if f.queueOnly {
		r.TurnID, r.QueuedSubmissionID = "", "queued-input"
	}
	return r
}
func (f *deliveryFixtureTransport) Reconcile(_ context.Context, _ Owner, event Event) (codex.SendReceipt, bool, error) {
	f.reconciles++
	if !reflect.DeepEqual(event.Delivery.Options, f.sent.Delivery.Options) || event.Delivery.MessageID != f.sent.Delivery.MessageID || event.Delivery.Envelope != f.sent.Delivery.Envelope || event.Delivery.Context != f.sent.Delivery.Context {
		f.t.Fatal("reconciliation changed the immutable request")
	}
	return f.receipt(event), f.found, f.reconcileErr
}

const fixtureOperationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func deliveryFixture(t *testing.T, withEvent bool) (*DeliveryService, *deliveryFixtureTransport, Operation) {
	return deliveryFixtureSocket(t, withEvent, "", "origin-epoch")
}

func deliveryFixtureSocket(t *testing.T, withEvent bool, socket, epoch string) (*DeliveryService, *deliveryFixtureTransport, Operation) {
	t.Helper()
	workspace, state := t.TempDir(), t.TempDir()
	store, err := NewStore(state, workspace)
	if err != nil {
		t.Fatal(err)
	}
	req := StartRequest{ID: fixtureOperationID, Brief: "Observe the exact check", Source: Source{Kind: "local", Argv: []string{"check", "exact"}, Cwd: workspace, Revision: "source-head", InputIdentity: "input-digest", InvocationID: fixtureOperationID}}
	if socket == "" {
		socket = workspace + "/native.sock"
	}
	owner := Owner{Slug: "2026-11-03-delivery", RootThreadID: "lead", Incarnation: "incarnation", OriginTurnID: "origin", Socket: socket, SocketEpoch: epoch, RuntimeSchema: 1}
	op := Operation{Schema: Schema, ID: req.ID, Workspace: workspace, Owner: owner, Source: req.Source, Brief: req.Brief, RequestDigest: digest(req), CreatedAt: time.Now().UTC(),
		UtilityPolicy: UtilityPolicy{PackageRoot: workspace, CatalogDigest: strings.Repeat("a", 64), NativeName: "watcher", Model: "gpt-6-luna", Effort: "low", Instructions: "Observe only", Sandbox: "danger-full-access", ApprovalPolicy: "never", MaxConcurrent: 1},
		Launch:        Launch{State: "running", ProjectID: "utility-project", ThreadID: "utility", CreateAttempted: true, BootstrapAttempted: true, BootstrapMarker: "bootstrap", MessageID: derivedID(req.ID, "launch"), Envelope: "Observe the exact check"},
		Events:        []Event{},
	}
	op.Launch.Receipt = &codex.SendReceipt{ClientUserMessageID: op.Launch.MessageID, TurnID: "utility-turn"}
	if withEvent {
		exit := 0
		if err := AddEvent(&op, ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exit}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.reserve(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	f := &deliveryFixtureTransport{store: store, t: t}
	s := &DeliveryService{Store: store, Resolve: func(string) (Owner, error) { return owner, nil }, LockSession: func(context.Context, string) (func(), error) { return func() {}, nil }, Transport: f}
	return s, f, op
}

func TestMonitorDeliverySameIDSurvivesRestartAndLostReply(t *testing.T) {
	s, f, op := deliveryFixture(t, true)
	f.sendErr = &codex.UnknownSendOutcomeError{Err: errors.New("lost reply")}
	id := op.Events[0].ID
	if err := s.Dispatch(context.Background(), op.ID, id); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Store.Get(context.Background(), op.ID)
	if first.Events[0].Delivery.Status != "submitting" {
		t.Fatal("unknown result was not retained")
	}
	// A restarted worker opens the same files and reconciles the original input.
	s.Store = &Store{directory: s.Store.directory, workspace: s.Store.workspace}
	f.found = true
	if err := s.Dispatch(context.Background(), op.ID, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(context.Background(), op.ID, id); err != nil {
		t.Fatal(err)
	}
	last, _ := s.Store.Get(context.Background(), op.ID)
	if f.sends != 1 || f.prepares != 1 || f.reconciles != 1 || last.Events[0].Delivery.Status != "accepted" || last.Events[0].Delivery.Receipt.TurnID != "lead-turn" {
		t.Fatalf("restart duplicated or lost the original send: sends=%d prepares=%d reconciles=%d event=%+v", f.sends, f.prepares, f.reconciles, last.Events[0])
	}
}

func TestMonitorDeliveryHoldsThenUsesExplicitlyAdmittedEpoch(t *testing.T) {
	s, f, op := deliveryFixture(t, true)
	f.held = "Session is stopped"
	if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	if f.sends != 0 || f.prepares != 0 {
		t.Fatal("held lead was activated")
	}
	f.held = ""
	current := op.Owner
	current.SocketEpoch = "explicit-new-epoch"
	s.Resolve = func(string) (Owner, error) { return current, nil }
	if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.Get(context.Background(), op.ID)
	if saved.Owner.SocketEpoch != "origin-epoch" || saved.Events[0].Delivery.AdmittedEpoch != current.SocketEpoch || f.sends != 1 {
		t.Fatal("origin identity or explicit admission was lost")
	}
}

func TestMonitorDeliveryRevocationAndUncertainty(t *testing.T) {
	for _, scenario := range []string{"unsent", "accepted-lost-reply", "unknown", "definitely-unsent"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, op := deliveryFixture(t, true)
			if scenario != "unsent" {
				f.sendErr = &codex.UnknownSendOutcomeError{Err: errors.New("lost reply")}
				if scenario == "definitely-unsent" {
					f.sendErr = &codex.MessageNotSentError{Err: errors.New("no writer"), Retryable: true}
				}
				if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := s.Store.Lock(context.Background(), op.ID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			tx.Operation.CanceledAt = &now
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			f.found = scenario == "accepted-lost-reply"
			if scenario == "unknown" {
				f.reconcileErr = &codex.UnknownSendOutcomeError{Err: errors.New("not visible")}
			}
			if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
				t.Fatal(err)
			}
			saved, _ := s.Store.Get(context.Background(), op.ID)
			want := "canceled"
			if scenario == "accepted-lost-reply" {
				want = "accepted"
			}
			if scenario == "unknown" {
				want = "submitting"
			}
			if saved.Events[0].Delivery.Status != want {
				t.Fatalf("status=%s want %s", saved.Events[0].Delivery.Status, want)
			}
			wantSends := 1
			if scenario == "unsent" {
				wantSends = 0
			}
			if f.sends != wantSends {
				t.Fatalf("revocation sent new input: %d", f.sends)
			}
		})
	}
}

func TestMonitorDeliveryDoesNotReplayUnknownOrQueueOnlyReceipt(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "queued"}[queued], func(t *testing.T) {
			s, f, op := deliveryFixture(t, true)
			f.queueOnly = queued
			if !queued {
				f.sendErr = &codex.UnknownSendOutcomeError{Err: errors.New("lost")}
				f.reconcileErr = f.sendErr
			}
			f.found = queued
			for i := 0; i < 4; i++ {
				if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			saved, _ := s.Store.Get(context.Background(), op.ID)
			if f.sends != 1 || len(saved.Events) != 1 || saved.Events[0].Delivery.Status != "submitting" {
				t.Fatal("unknown or queued receipt became another send/accepted turn")
			}
		})
	}
}

func TestMonitorDeliveryReplacementAndConcurrentDispatch(t *testing.T) {
	s, f, op := deliveryFixture(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.sends != 1 {
		t.Fatalf("concurrent dispatch sent %d inputs", f.sends)
	}
	s, f, op = deliveryFixture(t, true)
	owner := op.Owner
	owner.Incarnation = "replacement"
	s.Resolve = func(string) (Owner, error) { return owner, nil }
	if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.Get(context.Background(), op.ID)
	if f.sends != 0 || saved.Events[0].Delivery.Status != "canceled" {
		t.Fatal("replacement owner received stale completion")
	}
}

type terminalFixture struct {
	status string
	calls  int
}

func (f *terminalFixture) ObserveUtility(context.Context, Operation) (string, bool, error) {
	f.calls++
	return f.status, f.status != "", nil
}

func TestMonitorObservationRequiresTerminationAndNeverInfersSuccess(t *testing.T) {
	for _, withReport := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-report", true: "reported"}[withReport], func(t *testing.T) {
			s, _, op := deliveryFixture(t, withReport)
			observer := &terminalFixture{}
			s.Observer = observer
			if err := s.Observe(context.Background(), op.ID); err != nil {
				t.Fatal(err)
			}
			saved, _ := s.Store.Get(context.Background(), op.ID)
			if saved.Launch.TerminatedAt != nil {
				t.Fatal("silence released utility capacity")
			}
			observer.status = "completed"
			for i := 0; i < 2; i++ {
				if err := s.Observe(context.Background(), op.ID); err != nil {
					t.Fatal(err)
				}
			}
			saved, _ = s.Store.Get(context.Background(), op.ID)
			want := "incomplete"
			if withReport {
				want = "passed"
			}
			if saved.Launch.TerminatedAt == nil || len(saved.Events) != 1 || saved.Events[0].Report.Outcome != want || observer.calls != 2 {
				t.Fatal("terminal observation lost/replaced an outcome or created a replay loop")
			}
		})
	}
}

func TestMonitorStatusReconciliationHasNoActivationSideEffects(t *testing.T) {
	s, f, op := deliveryFixture(t, true)
	f.sendErr = &codex.UnknownSendOutcomeError{Err: errors.New("lost reply")}
	if err := s.Dispatch(context.Background(), op.ID, op.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.Lock(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	now := time.Now().UTC()
	tx.Operation.CanceledAt = &now
	f.reconcileErr = &codex.UnknownSendOutcomeError{Err: errors.New("history is not visible")}
	if err := ReconcileMonitorEvents(context.Background(), tx.Operation, f); err == nil || tx.Operation.Events[0].Delivery.Status != "submitting" {
		t.Fatal("uncertain canceled input was discarded")
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	f.reconcileErr, f.found = nil, true
	if err := ReconcileMonitorEvents(context.Background(), tx.Operation, f); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	if tx.Operation.Events[0].Delivery.Status != "accepted" || f.sends != 1 || f.prepares != 1 {
		t.Fatal("receipt recovery activated or replaced canceled work")
	}
}
