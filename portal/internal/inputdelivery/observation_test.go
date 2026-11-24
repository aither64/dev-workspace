package inputdelivery

import (
	"context"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func TestPendingObservationWindowHasFixedClaimAnchor(t *testing.T) {
	r := Record{DispatchAtMS: 10000}
	r.Status.State = "pending"
	now, count := int64(10000), 0
	for now < 11000 {
		next := nextObservation(r, now)
		if next-now > 200 || next > 11000 {
			t.Fatal(now, next)
		}
		now = next
		count++
	}
	if count != 5 || nextObservation(r, now) != 13000 {
		t.Fatal("window renewed", count, now)
	}
	if nextObservation(r, 10950) != 11000 {
		t.Fatal("lookup crossed window")
	}
	for _, state := range []string{"unknown", "dispatching", "not_sent"} {
		r.Status.State = state
		if nextObservation(r, 10100) != 12100 {
			t.Fatal("unproved fast polling", state)
		}
	}
	r.Status.State = "pending"
	if nextObservation(r, 9999) != 11999 {
		t.Fatal("future claim renewed window")
	}
}

func TestPendingObservationRestartDoesNotRenewClaimWindow(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	s, e := Open(ctx, root, workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	r, e := s.Admit(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	r = claimRecord(r)
	r.DispatchAtMS = time.Now().Add(-2 * time.Second).UnixMilli()
	if won, e := s.Update(ctx, r, "queued"); e != nil || !won {
		t.Fatal(won, e)
	}
	r.Status.State = "pending"
	if won, e := s.Update(ctx, r, "dispatching"); e != nil || !won {
		t.Fatal(won, e)
	}
	s.Close(ctx)
	s, e = Open(ctx, root, workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close(ctx)
	r, e = s.Get(ctx, a.Scope, a.Ref.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	anchor := r.DispatchAtMS
	f := &dispatchFixture{lookupResult: Result{State: "pending"}}
	d := &Dispatcher{ctx: ctx, store: s, driver: f}
	before := time.Now().UnixMilli()
	d.process(r)
	r, e = s.Get(ctx, a.Scope, a.Ref.OperationID)
	if e != nil || r.DispatchAtMS != anchor || r.NextAttemptMS < before+2000 || f.lookups != 1 || f.dispatches != 0 {
		t.Fatal(r, e, f)
	}
}

func TestPendingDueOrderLetsLaterSteerAndPeerProgress(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	s, e := Open(ctx, t.TempDir(), workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close(ctx)
	now := time.Now().UnixMilli()
	var originalScope string
	for i, id := range []string{"11111111111111111111111111111111", "22222222222222222222222222222222"} {
		a := fixtureAdmission(workspace, id)
		if i == 0 {
			a.Request.Delivery = "followUp"
			a.Input.ActionContext = "pi-delivery-v1:followUp"
			a.Ref.Digest = piruntime.SubmissionDigest(a.Ref, a.Input, a.Selection.Settings)
		}
		r, e := s.Admit(ctx, a)
		if e != nil {
			t.Fatal(e)
		}
		originalScope = a.Scope
		r = claimRecord(r)
		if won, e := s.Update(ctx, r, "queued"); e != nil || !won {
			t.Fatal(won, e)
		}
		r.Status.State = "pending"
		r.NextAttemptMS = now - 50 - int64(i)*50
		if won, e := s.Update(ctx, r, "dispatching"); e != nil || !won {
			t.Fatal(won, e)
		}
	}
	peer := fixtureAdmission(workspace, "33333333333333333333333333333333")
	peer.Ref.Conversation.Member = "implementer0"
	peer.Selection.Handle.Conversation = peer.Ref.Conversation
	peer.Scope = ConversationScope(peer.Ref.Conversation)
	peer.Request.Selection = Digest(peer.Selection)
	peer.Ref.Digest = piruntime.SubmissionDigest(peer.Ref, peer.Input, peer.Selection.Settings)
	if _, e = s.Admit(ctx, peer); e != nil {
		t.Fatal(e)
	}
	rows, e := s.Due(ctx, "", 4)
	if e != nil || len(rows) != 2 {
		t.Fatal(rows, e)
	}
	for _, r := range rows {
		if r.Admission.Scope == originalScope && r.Status.ClientUserMessageID != "22222222222222222222222222222222" {
			t.Fatal("old followUp monopolized pending lookup", r.Status)
		}
	}
	first, e := s.Due(ctx, "", 1)
	if e != nil || len(first) != 1 {
		t.Fatal(first, e)
	}
	next, e := s.Due(ctx, first[0].Admission.Scope, 1)
	if e != nil || len(next) != 1 || next[0].Admission.Scope == first[0].Admission.Scope {
		t.Fatal("peer starved", next, e)
	}
}
