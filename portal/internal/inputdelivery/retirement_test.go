package inputdelivery

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func claimRecord(r Record) Record {
	r.Status.State = "dispatching"
	r.DispatchAtMS = time.Now().UnixMilli()
	r.DispatchRuntime = r.Admission.Selection.Runtime
	r.DispatchHandle = r.Admission.Selection.Handle
	r.DispatchGeneration = r.DispatchHandle.Generation
	return r
}

func TestRetiredUnknownReleasesOnlyItsOrderingBarrierAndCapacity(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	s, e := Open(ctx, root, workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	a := fixtureAdmission(workspace, fmt.Sprintf("%032x", 1))
	for i := 1; i <= 110; i++ {
		a = fixtureAdmission(workspace, fmt.Sprintf("%032x", i))
		r, e := s.Admit(ctx, a)
		if e != nil {
			t.Fatal(i, e)
		}
		r = claimRecord(r)
		if won, e := s.Update(ctx, r, "queued"); e != nil || !won {
			t.Fatal(won, e)
		}
		r.Status.State = "unknown"
		if won, e := s.Update(ctx, r, "dispatching"); e != nil || !won {
			t.Fatal(won, e)
		}
		original := Digest(r.Admission)
		r.Status.Retired = true
		if won, e := s.Update(ctx, r, "unknown"); e != nil || !won {
			t.Fatal(won, e)
		}
		if Digest(r.Admission) != original || r.Status.State != "unknown" || r.Status.Receipt != nil {
			t.Fatal("changed original", r)
		}
		r.Status.Retired = false
		if won, e := s.Update(ctx, r, "unknown"); e != nil || won {
			t.Fatal("unretired", won, e)
		}
	}
	for i := 111; i <= 130; i++ {
		if _, e = s.Admit(ctx, fixtureAdmission(workspace, fmt.Sprintf("%032x", i))); e != nil {
			t.Fatal(i, e)
		}
	}
	if _, e = s.Admit(ctx, fixtureAdmission(workspace, fmt.Sprintf("%032x", 131))); e != ErrCapacity {
		t.Fatal(e)
	}
	rows, e := s.List(ctx, a.Scope)
	if e != nil || len(rows) != 120 {
		t.Fatal(len(rows), e)
	}
	due, e := s.Due(ctx, "", 4)
	if e != nil || len(due) != 1 || due[0].Status.Sequence != 111 {
		t.Fatal(due, e)
	}
	s.Close(ctx)
	s, e = Open(ctx, root, workspace, testGate)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close(ctx)
	old, e := s.Get(ctx, a.Scope, fmt.Sprintf("%032x", 1))
	if e != nil || !old.Status.Retired || old.Status.State != "unknown" {
		t.Fatal(old, e)
	}
	r := claimRecord(due[0])
	if won, e := s.Update(ctx, r, "queued"); e != nil || !won {
		t.Fatal(won, e)
	}
	r.Status.State = "unknown"
	if won, e := s.Update(ctx, r, "dispatching"); e != nil || !won {
		t.Fatal(won, e)
	}
	next, e := s.Get(ctx, a.Scope, fmt.Sprintf("%032x", 112))
	if e != nil {
		t.Fatal(e)
	}
	next = claimRecord(next)
	if won, e := s.Update(ctx, next, "queued"); e != nil || won {
		t.Fatal("ordinary unknown did not block", won, e)
	}
	r.Status.Retired = true
	if won, e := s.Update(ctx, r, "unknown"); e != nil || !won {
		t.Fatal(won, e)
	}
	if won, e := s.Update(ctx, next, "queued"); e != nil || !won {
		t.Fatal("retired barrier remained", won, e)
	}
}

func TestDispatcherRetirementRequiresSuccessfulLookupNotDispatch(t *testing.T) {
	ctx := context.Background()
	s, e := Open(ctx, t.TempDir(), t.TempDir(), testGate)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close(ctx)
	a := fixtureAdmission(s.workspace, "11111111111111111111111111111111")
	r, e := s.Admit(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	f := &dispatchFixture{after: fmt.Errorf("lost reply")}
	d := &Dispatcher{ctx: ctx, store: s, driver: f}
	d.process(r)
	r, e = s.Get(ctx, a.Scope, a.Ref.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	f.lookupResult = Result{State: "unknown", RetiredUnknown: true}
	d.process(r)
	r, e = s.Get(ctx, a.Scope, a.Ref.OperationID)
	if e != nil || !r.Status.Retired || r.Status.State != "unknown" || f.dispatches != 1 || f.lookups != 1 {
		t.Fatal(r, e, f)
	}
	if due, e := s.Due(ctx, "", 4); e != nil || len(due) != 0 {
		t.Fatal(due, e)
	}
}
