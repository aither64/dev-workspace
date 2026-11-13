package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aither64/codex-web/codex"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type observerDeliveryFixture struct {
	store                       *Store
	t                           *testing.T
	sent                        Event
	sends, prepares, reconciles int
	found                       bool
}

func (f *observerDeliveryFixture) Ready(context.Context, Owner) (string, error) { return "", nil }
func (f *observerDeliveryFixture) Prepare(context.Context, Owner) (codex.TurnOptions, error) {
	f.prepares++
	return codex.TurnOptions{Model: "retained-model", ReasoningEffort: "xhigh"}, nil
}
func (f *observerDeliveryFixture) Send(_ context.Context, _ Owner, e Event) (codex.SendReceipt, error) {
	f.sends++
	f.sent = e
	path, _ := f.store.observerPath(strings.Split(e.Delivery.Context, ":")[1])
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	var r ObserverRecord
	if err := json.Unmarshal(data, &r); err != nil {
		f.t.Fatal(err)
	}
	if r.Events[0].Delivery.Status != "submitting" || r.Events[0].Delivery.Options == nil {
		f.t.Fatal("send preceded durable observer intent")
	}
	return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: errors.New("lost reply")}
}
func (f *observerDeliveryFixture) Reconcile(_ context.Context, _ Owner, e Event) (codex.SendReceipt, bool, error) {
	f.reconciles++
	if e.ID != f.sent.ID || e.Delivery.Envelope != f.sent.Delivery.Envelope || e.Delivery.Context != f.sent.Delivery.Context || !reflect.DeepEqual(e.Delivery.Options, f.sent.Delivery.Options) {
		f.t.Fatal("recovery changed original input")
	}
	if !f.found {
		return codex.SendReceipt{}, false, errors.New("original result unknown")
	}
	return codex.SendReceipt{ClientUserMessageID: e.ID, TurnID: "exact-lead-turn"}, true, nil
}
func TestDirectDeliveryUnknownReceiptSurvivesQuiescenceAndRecoversWithoutNewSend(t *testing.T) {
	ctx := context.Background()
	s, native, req, owner := operationFixture(t)
	req = ciRequest(req)
	s.ObserverEpoch = "worker"
	r, err := s.StartCIObservation(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCIPoll(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	terminal := CISnapshot{Status: "completed", Conclusion: "success", ObservedAt: now, SourceDigest: claim.SourceDigest}
	r, err = s.CommitCIPoll(ctx, claim, &terminal, joinedPoll(claim, now), "")
	if err != nil {
		t.Fatal(err)
	}
	f := &observerDeliveryFixture{store: s.Store, t: t}
	d := DeliveryService{Store: s.Store, Resolve: s.Resolve, LockSession: s.LockSession, Transport: f}
	refs, err := d.Pending(ctx)
	if err != nil || len(refs) != 1 {
		t.Fatal(refs, err)
	}
	if err := d.Dispatch(ctx, r.ID, r.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	l := Lifecycle{Store: s.Store, AuthorizeArchive: func(context.Context, ArchiveAuthority) error { return nil }}
	a := ArchiveAuthority{Slug: owner.Slug, RootThreadID: owner.RootThreadID, OperationID: strings.Repeat("8", 64), TargetID: "v2-target", Mode: "explicit"}
	if err := l.BeginArchiveQuiescence(ctx, a); err != nil {
		t.Fatal(err)
	}
	view, err := l.CompleteArchiveQuiescence(ctx, a)
	if err != nil || view.State != "blocked" {
		t.Fatal("unknown receipt waived", view, err)
	}
	if err := d.Dispatch(ctx, r.ID, r.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	unknown, _ := s.Store.GetObserver(ctx, r.ID)
	if unknown.Events[0].Delivery.Status != "submitting" {
		t.Fatal("unknown input declared canceled")
	}
	f.found = true
	if err := d.ReconcileReceipts(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	view, err = l.CompleteArchiveQuiescence(ctx, a)
	if err != nil || view.State != "quiescent" {
		t.Fatal(view, err)
	}
	if err := d.Dispatch(ctx, r.ID, r.Events[0].ID); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.GetObserver(ctx, r.ID)
	if saved.Phase != "detached" || saved.Events[0].Delivery.Status != "accepted" || f.sends != 1 || f.prepares != 1 || native.sends != 0 {
		t.Fatalf("lost or duplicated original input: %+v, sends=%d", saved, f.sends)
	}
}

func completedObserverFixture(t *testing.T) (*Service, ObserverRecord, *Owner) {
	t.Helper()
	ctx := context.Background()
	s, _, req, owner := operationFixture(t)
	s.ObserverEpoch = "worker"
	req = ciRequest(req)
	r, err := s.StartCIObservation(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCIPoll(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Second)
	snapshot := CISnapshot{Status: "completed", Conclusion: "success", ObservedAt: at, SourceDigest: claim.SourceDigest}
	r, err = s.CommitCIPoll(ctx, claim, &snapshot, joinedPoll(claim, at), "")
	if err != nil {
		t.Fatal(err)
	}
	return s, r, owner
}

func TestCompletedDirectObservationRetiresThroughOrdinaryRevokeWithoutArchiveBinding(t *testing.T) {
	for _, status := range []string{"pending", "held", "accepted"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			s, r, owner := completedObserverFixture(t)
			tx, err := s.Store.lockObserver(ctx, r.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			tx.record.Events[0].Delivery.Status = status
			if status == "accepted" {
				tx.record.Events[0].Delivery.Receipt = &codex.SendReceipt{ClientUserMessageID: r.Events[0].ID, TurnID: "retained-lead-turn"}
			}
			bumpObserver(tx.record, time.Now().UTC())
			err = tx.save()
			tx.close()
			if err != nil {
				t.Fatal(err)
			}
			before, _ := s.Store.GetObserver(ctx, r.ID)
			l := Lifecycle{Store: s.Store}
			if err := l.RequireIdle(ctx, owner.Slug, owner.RootThreadID, false); err != nil {
				t.Fatal("completed source cannot enter ordinary delete", err)
			}
			if local, err := l.AssessLocalSafety(ctx, owner.Slug, owner.RootThreadID); err != nil || local.State != "quiescent" {
				t.Fatal(local, err)
			}
			if err := l.RequireArchived(ctx, owner.Slug, owner.RootThreadID); err == nil {
				t.Fatal("idle result waived continuation retirement")
			}
			if err := l.Revoke(ctx, owner.Slug, owner.RootThreadID); err != nil {
				t.Fatal(err)
			}
			if err := l.Archive(ctx, owner.Slug, owner.RootThreadID); err != nil {
				t.Fatal("existing ordinary retirement path refused", err)
			}
			if err := l.RequireArchived(ctx, owner.Slug, owner.RootThreadID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ClaimCIPoll(ctx, r.ID); !errors.Is(err, ErrPollNotDue) {
				t.Fatal("terminal operation polled again", err)
			}
			f := &observerDeliveryFixture{store: s.Store, t: t}
			d := DeliveryService{Store: s.Store, Resolve: s.Resolve, LockSession: s.LockSession, Transport: f}
			if err := d.Dispatch(ctx, r.ID, r.Events[0].ID); err != nil {
				t.Fatal(err)
			}
			after, _ := s.Store.GetObserver(ctx, r.ID)
			want := "canceled"
			if status == "accepted" {
				want = "accepted"
			}
			if after.Events[0].Delivery.Status != want || !reflect.DeepEqual(before.Events[0].Delivery.Receipt, after.Events[0].Delivery.Receipt) || after.CanceledAt == nil || after.Archive != nil || after.Phase != "observing" || f.sends != 0 || f.prepares != 0 {
				t.Fatal("retirement lost receipt, woke owner or invented archive authority", after)
			}
		})
	}
}

func TestCompletedDirectRetirementKeepsUncertainAndNonterminalFences(t *testing.T) {
	for _, kind := range []string{"queued", "poll error", "missing poll", "intent", "running", "submitting", "queued receipt", "missing terminal event"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, r, owner := completedObserverFixture(t)
			path, _ := s.Store.observerPath(r.ID)
			switch kind {
			case "queued", "poll error":
				r.Remote.Status = "queued"
				r.Remote.Conclusion = ""
				r.Events = []Event{}
				if kind == "poll error" {
					r.Detail = "GitHub status request failed; last source snapshot retained"
				}
			case "missing poll":
				r.Poll = nil
			case "intent":
				r.Poll.State = "intent"
				r.Poll.Handle = ""
				r.Poll.FinishedAt = nil
				r.Poll.ExitStatus = nil
			case "running":
				r.Poll.State = "running"
				r.Poll.FinishedAt = nil
				r.Poll.ExitStatus = nil
			case "submitting":
				r.Events[0].Delivery.Status = "submitting"
			case "queued receipt":
				r.Events[0].Delivery.Status = "submitting"
				r.Events[0].Delivery.Receipt = &codex.SendReceipt{ClientUserMessageID: r.Events[0].ID, QueuedSubmissionID: "original-unknown-input"}
			case "missing terminal event":
				r.Events = []Event{}
			}
			// These retained shapes use the unchanged generic reader. None is
			// sufficient evidence for naturally completed local retirement.
			data, _ := json.Marshal(r)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.GetObserver(ctx, r.ID); err != nil {
				t.Fatal("fixture must be a retained readable shape", err)
			}
			l := Lifecycle{Store: s.Store}
			if err := l.RequireIdle(ctx, owner.Slug, owner.RootThreadID, false); err == nil {
				t.Fatal("uncertainty/nonterminal source allowed delete", kind)
			}
			if err := l.Revoke(ctx, owner.Slug, owner.RootThreadID); err != nil {
				t.Fatal(err)
			}
			if err := l.RequireIdle(ctx, owner.Slug, owner.RootThreadID, false); err == nil {
				t.Fatal("revocation fabricated local completion", kind)
			}
			after, _ := s.Store.GetObserver(ctx, r.ID)
			if !reflect.DeepEqual(after.Poll, r.Poll) || !reflect.DeepEqual(after.Remote, r.Remote) || after.Archive != nil || after.Phase != r.Phase {
				t.Fatal("revoke mutated execution/source proof", after)
			}
			if kind == "submitting" || kind == "queued receipt" {
				if after.Events[0].Delivery.Status != "submitting" || !reflect.DeepEqual(after.Events[0].Delivery.Receipt, r.Events[0].Delivery.Receipt) {
					t.Fatal("revoke discarded unknown delivery")
				}
			}
		})
	}
}
