package turnretry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestSessionPolicyInheritanceClassBoundariesAndUnknownSuppression(t *testing.T) {
	store, scope := policyFixture(t)
	ctx := context.Background()
	state := New(fixtureScope())
	state.Enabled = false
	state.Boundary = scope.Incarnation
	state.Enroll(Observation{})
	active := Observation{Turn: "active", Status: "inProgress"}
	p, err := store.Update(ctx, scope, true, func(p *Policy) error {
		return PreparePolicy(p, []PolicyThread{{State: state, Before: active, After: active}}, true, true, "", true)
	})
	if err != nil || p.Capacity || p.Cybersecurity {
		t.Fatal("legacy opt-out not inherited", p, err)
	}
	before, _ := json.Marshal(state)
	p, err = store.Update(ctx, scope, false, func(p *Policy) error {
		return PreparePolicy(p, []PolicyThread{{State: state, Before: active, After: active}}, true, false, p.Revision, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	authority := SessionPolicy{Store: store, Scope: scope}
	decision, err := authority.Decide(ctx, &state)
	if err != nil || !decision.Eligible(&state, Observation{Turn: "active", Status: "failed", Code: "serverOverloaded"}) || decision.Eligible(&state, Observation{Turn: "active", Status: "failed", Code: "cyberPolicy"}) {
		t.Fatal("independent class enable failed", decision, err)
	}
	after, _ := json.Marshal(state)
	if !bytes.Equal(before, after) {
		t.Fatal("session policy rewrote ledger")
	}
	state.AddSnapshot(proof("active"))
	if err := state.Observe(Observation{Turn: "active", Status: "failed", Code: "serverOverloaded"}, time.Unix(1000, 0), 0, decision); err != nil {
		t.Fatal(err)
	}
	if err := state.Begin(time.UnixMilli(state.DeadlineMS), decision); err != nil {
		t.Fatal("legacy Enabled overrode explicit session decision", err)
	}
	original := *state.Pending
	terminal := Observation{Turn: "active", Status: "failed", Code: "serverOverloaded"}
	p, err = store.Update(ctx, scope, false, func(p *Policy) error {
		return PreparePolicy(p, []PolicyThread{{State: state, Before: terminal, After: terminal}}, false, false, p.Revision, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err = store.Update(ctx, scope, false, func(p *Policy) error {
		return PreparePolicy(p, []PolicyThread{{State: state, Before: terminal, After: terminal}}, true, true, p.Revision, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted := SessionPolicy{Store: store, Scope: scope}
	decision, err = restarted.Decide(ctx, &state)
	if err != nil || decision.Allows(&state) || len(decision.Suppressed) != 1 || decision.Suppressed[0].Pending != original.ID {
		t.Fatal("off/on revived unknown", decision, err)
	}
	if !reflect.DeepEqual(original, *state.Pending) {
		t.Fatal("policy mutated exact pending")
	}
	if err := state.ReserveUnsubmitted(time.Unix(2000, 0), decision); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatal("restart reservation bypassed gate", err)
	}
	// A different independent active turn may fail after the enable boundary.
	if !decision.Eligible(&state, Observation{Turn: "independent", Status: "failed", Code: "cyberPolicy"}) {
		t.Fatal("new independent failure excluded")
	}
	if decision.Eligible(&state, terminal) {
		t.Fatal("terminal enable baseline revived")
	}
}

func TestSessionPolicyTurnoverStaleAndFutureMember(t *testing.T) {
	store, scope := policyFixture(t)
	ctx := context.Background()
	p := createPolicy(t, store, scope)
	state := New(fixtureScope())
	state.Enroll(Observation{Turn: "before-manual", Status: "completed"})
	state.Boundary = scope.Incarnation
	state.Scope.Thread = "future-member"
	authority := SessionPolicy{Store: store, Scope: scope}
	decision, err := authority.Decide(ctx, &state)
	if err != nil || decision.CapacityBaseline.Turn != "before-manual" {
		t.Fatal("new member consumed first failure", decision, err)
	}
	if !decision.Eligible(&state, Observation{Turn: "first-manual", Status: "failed", Code: "serverOverloaded"}) {
		t.Fatal("eligible first failure discarded")
	}
	other := state
	other.Scope.Thread = "unproved"
	other.Enrolled = false
	if _, err := authority.Decide(ctx, &other); err == nil {
		t.Fatal("unproved new member enrolled")
	}
	for _, candidate := range []struct {
		revision      string
		before, after Observation
	}{
		{"stale", Observation{}, Observation{}},
		{p.Revision, Observation{Turn: "a", Status: "inProgress"}, Observation{Turn: "b", Status: "inProgress"}},
		{p.Revision, Observation{Turn: "a", Status: "inProgress"}, Observation{Turn: "a", Status: "failed"}},
	} {
		before, _ := os.ReadFile(store.path(scope))
		_, err := store.Update(ctx, scope, false, func(p *Policy) error {
			return PreparePolicy(p, []PolicyThread{{State: state, Before: candidate.before, After: candidate.after}}, false, true, candidate.revision, false)
		})
		if err == nil {
			t.Fatal("stale/turnover inventory committed")
		}
		after, _ := os.ReadFile(store.path(scope))
		if !bytes.Equal(before, after) {
			t.Fatal("rejected policy partially committed")
		}
	}
}

func TestControllerRequiresExplicitPolicyIncludingPositiveRestart(t *testing.T) {
	c, b, now := controllerFixture(t)
	scope := fixtureScope()
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	b.o = Observation{Turn: "failed", Status: "failed", Code: "cyberPolicy"}
	b.unknown = true
	if err := c.Step(context.Background(), scope); err == nil {
		t.Fatal("fixture must be uncertain")
	}
	state, _ := c.Store.Load(scope)
	original := *state.Pending
	b.unknown = false
	b.never = true
	c.Policy = nil
	if err := c.Step(context.Background(), scope); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatal("missing decision sent", err)
	}
	state, _ = c.Store.Load(scope)
	if b.sends != 1 || !reflect.DeepEqual(original, *state.Pending) {
		t.Fatal("missing policy changed intent")
	}
	c.Policy = Decision{Revision: "off"}
	*now = now.Add(time.Minute)
	if err := c.Step(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	state, _ = c.Store.Load(scope)
	if b.sends != 1 || !state.Cancelled || state.Pending == nil || state.Pending.ID != original.ID {
		t.Fatal("positive never-submitted restart escaped off", state)
	}
	// Exact native acceptance remains usable with no policy at all.
	tx, _ := c.Store.Lock(context.Background(), scope)
	tx.State.Phase = "reconciling"
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	b.accepted = "accepted"
	c.Policy = nil
	if err := c.ReconcileOnly(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	state, _ = c.Store.Load(scope)
	if state.Pending.ResultTurn != "accepted" || b.sends != 1 {
		t.Fatal("receipt-only recovery depended on policy")
	}
}

func TestSummaryAcceptanceOnceRestartFrozenTimingAndManualGap(t *testing.T) {
	store, scope := policyFixture(t)
	ctx := context.Background()
	createPolicy(t, store, scope)
	a := SessionPolicy{Store: store, Scope: scope}
	state := New(fixtureScope())
	state.Boundary = scope.Incarnation
	state.Enroll(Observation{})
	decision, _ := a.Decide(ctx, &state)
	now := time.Unix(1000, 0)
	state.AddSnapshot(proof("failed"))
	if err := state.Observe(Observation{Turn: "failed", Status: "failed", Code: "cyberPolicy"}, now, 0, decision); err != nil {
		t.Fatal(err)
	}
	if err := a.Checkpoint(ctx, &state, nil, now, true); err != nil {
		t.Fatal(err)
	}
	if err := state.Begin(now, decision); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := a.Checkpoint(ctx, &state, nil, now, false); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := store.Load(scope)
	if *p.Thread("root").Summary.ConfirmedStarts != 0 {
		t.Fatal("reservation counted as accepted")
	}
	if err := state.Accepted("accepted", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := a.Checkpoint(ctx, &state, nil, now, false); err != nil {
			t.Fatal(err)
		}
	}
	p, _ = store.Load(scope)
	summary := p.Thread("root").Summary
	if *summary.ConfirmedStarts != 1 || summary.CountQuality != "exact" || summary.LastConfirmed != state.Pending.ID {
		t.Fatal("duplicate receipt counted", summary)
	}
	finished := Observation{Turn: "accepted", Status: "completed"}
	if err := a.Checkpoint(ctx, &state, &finished, now.Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}
	if err := a.Checkpoint(ctx, &state, &finished, now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Load(scope)
	summary = p.Thread("root").Summary
	if summary.Phase != "terminal" || *summary.FinishedAtMS != now.Add(time.Minute).UnixMilli() {
		t.Fatal("terminal timing moved", summary)
	}
	// A later accepted result lost before an optional checkpoint cannot remain
	// labelled exact after a manual admission retired its authoritative Pending.
	_, err := store.Update(ctx, scope, false, func(p *Policy) error {
		p.Thread("root").Summary.PendingID = "00000000-0000-4000-8000-000000000092"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state.Cancel("manual_input")
	if err := a.Checkpoint(ctx, &state, nil, now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Load(scope)
	if p.Thread("root").Summary.CountQuality != "partial" {
		t.Fatal("missed checkpoint fabricated exact count")
	}
}

func TestPolicyCommittedOffBeforeLedgerCancellationRestart(t *testing.T) {
	c, b, now := controllerFixture(t)
	ctx := context.Background()
	scope := fixtureScope()
	store := PolicyStore{Root: c.Store.Root, Workspace: scope.Workspace}
	identity := PolicyScope{Workspace: scope.Workspace, Slug: scope.Slug, Root: scope.Root, Incarnation: b.boundary}
	createPolicy(t, store, identity)
	c.Policy = SessionPolicy{Store: store, Scope: identity}
	if err := c.Step(ctx, scope); err != nil {
		t.Fatal(err)
	}
	b.o = Observation{Turn: "failed", Status: "failed", Code: "cyberPolicy"}
	b.unknown = true
	if err := c.Step(ctx, scope); err == nil {
		t.Fatal("fixture must remain unknown")
	}
	state, _ := c.Store.Load(scope)
	original := *state.Pending
	_, err := store.Update(ctx, identity, false, func(p *Policy) error {
		return PreparePolicy(p, []PolicyThread{{State: state, Before: b.o, After: b.o}}, false, false, p.Revision, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Crash immediately after the one atomic preference commit, before any
	// lazy ledger cancellation. The exact unresolved tuple is still original.
	retained, _ := c.Store.Load(scope)
	if retained.Cancelled || !reflect.DeepEqual(original, *retained.Pending) {
		t.Fatal("policy commit rewrote ledger")
	}
	restarted := Controller{Store: c.Store, Backend: b, Policy: SessionPolicy{Store: store, Scope: identity}, Clock: func() time.Time { return *now }}
	if err := restarted.Step(ctx, scope); err == nil {
		t.Fatal("unknown outcome hidden")
	}
	if b.sends != 1 {
		t.Fatal("unknown off attempt repeated")
	}
	b.accepted = "accepted"
	b.unknown = false
	if err := restarted.Step(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Step(ctx, scope); err != nil {
		t.Fatal(err)
	}
	retained, _ = c.Store.Load(scope)
	if b.sends != 1 || !retained.Cancelled || retained.Handled != "accepted" {
		t.Fatal("exact off settlement revived chain", retained)
	}
	p, err := store.Load(identity)
	if err != nil {
		t.Fatal(err)
	}
	if p.Thread("root").Summary.ConfirmedStarts == nil || *p.Thread("root").Summary.ConfirmedStarts != 1 {
		t.Fatal("off settlement lost exact accepted count", p)
	}
}

func TestSessionDecisionCrossClassFailureKeepsChainAndTerminatesDisabledClass(t *testing.T) {
	for _, code := range []string{"cyberPolicy", "serverOverloaded", "unsupported"} {
		s := New(fixtureScope())
		s.Enroll(Observation{})
		now := time.Unix(1000, 0)
		d := Decision{Revision: "policy", Capacity: true, Cybersecurity: true}
		observe(t, &s, "failure", "serverOverloaded", now)
		if err := s.Begin(now.Add(time.Minute), d); err != nil {
			t.Fatal(err)
		}
		if err := s.Accepted("accepted", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.AddSnapshot(proof("accepted")); err != nil {
			t.Fatal(err)
		}
		chain := s.Chain
		original := *s.Pending
		d.Cybersecurity = false
		if err := s.Observe(Observation{Turn: "accepted", Status: "failed", Code: code}, now.Add(2*time.Minute), 0, d); err != nil {
			t.Fatal(err)
		}
		if s.Chain != chain {
			t.Fatal("cross-class observation changed chain")
		}
		switch code {
		case "cyberPolicy":
			if s.Phase != "cancelled" || s.Handled != "accepted" {
				t.Fatal("disabled cross-class failure remained running", s)
			}
			if s.Pending == nil || !reflect.DeepEqual(original, *s.Pending) {
				t.Fatal("disabled terminal completion lost exact tuple")
			}
		case "serverOverloaded":
			if s.Phase != "scheduled" || s.Class != "capacity" {
				t.Fatal("enabled class did not continue", s)
			}
		default:
			if s.Phase != "blocked" || s.Handled != "accepted" {
				t.Fatal("unsupported failure remained running", s)
			}
			if s.Pending == nil || !reflect.DeepEqual(original, *s.Pending) {
				t.Fatal("unsupported terminal completion lost exact tuple")
			}
		}
	}
}

func TestSessionDecisionUnknownNeverBecomesAbsentOnClassChange(t *testing.T) {
	for _, code := range []string{"cyberPolicy", "serverOverloaded", "unsupported"} {
		s := New(fixtureScope())
		s.Enroll(Observation{})
		now := time.Unix(1000, 0)
		observe(t, &s, "failed", "cyberPolicy", now)
		if err := s.Begin(now, Decision{Revision: "on", Capacity: true, Cybersecurity: true}); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(s)
		if err := s.Observe(Observation{Turn: "other", Status: "failed", Code: code}, now, 0, Decision{Revision: "off"}); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(s)
		if !s.Uncertain() || !bytes.Equal(before, after) {
			t.Fatal("class change erased or inferred uncertain identity", s)
		}
	}
}
