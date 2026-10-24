package turnretry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

func fixtureScope() Scope {
	return Scope{Workspace: "/workspace", Slug: "2026-10-22-test", Root: "root", Thread: "root", Socket: "/socket"}
}
func proof(turn string) Snapshot {
	p := Snapshot{Epoch: 1, Version: 1, ID: "manual-" + turn, Turn: turn, Origin: "browser", Provider: "openai", Mode: "default", Options: codex.TurnOptions{Model: "model", ReasoningEffort: "high"}, Proven: true}
	p.Digest, _ = p.Hash()
	return p
}
func observe(t *testing.T, s *State, turn, code string, now time.Time) {
	t.Helper()
	s.AddSnapshot(proof(turn))
	if err := s.Observe(Observation{turn, "failed", code}, now, 0, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
}

func TestNewFailuresAndActiveEnrollment(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(fixtureScope())
	observe(t, &s, "old", "serverOverloaded", now)
	if s.Phase != "idle" {
		t.Fatal("old failure restarted")
	}
	observe(t, &s, "new", "serverOverloaded", now)
	if s.Phase != "scheduled" || s.DeadlineMS != now.Add(15*time.Second).UnixMilli() {
		t.Fatalf("new failure: %+v", s)
	}
	id := s.Pending.ID
	observe(t, &s, "new", "serverOverloaded", now.Add(time.Second))
	if s.Pending.ID != id {
		t.Fatal("duplicate notification changed identity")
	}
	active := New(fixtureScope())
	active.Enroll(Observation{"active", "inProgress", ""})
	observe(t, &active, "active", "cyberPolicy", now)
	if active.Phase != "scheduled" {
		t.Fatal("active enrollment failed completion excluded")
	}
}

func TestBackoffAcrossAcceptedChainAndRestart(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(fixtureScope())
	s.Enroll(Observation{})
	for i, delay := range []time.Duration{15, 30, 60, 120, 240, 300, 300, 300} {
		turn := string(rune('a' + i))
		observe(t, &s, turn, "serverOverloaded", now)
		if got := time.Duration(s.DeadlineMS-now.UnixMilli()) * time.Millisecond; got != delay*time.Second {
			t.Fatalf("attempt %d: %v", i, got)
		}
		now = time.UnixMilli(s.DeadlineMS)
		if err := s.Begin(now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
			t.Fatal(err)
		}
		if err := s.Accepted(string(rune('a'+i+1)), now); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(0); i < 12; i++ {
		low, high := CapacityDelay(i, 0), CapacityDelay(i, 1)
		if high > 300*time.Second || high-low > min(3*time.Second, low/10) {
			t.Fatal("jitter escaped cap")
		}
	}
}

func TestCyberPossibleSendsExhaustionAndClock(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(fixtureScope())
	s.Enroll(Observation{})
	for i := 0; i < 30; i++ {
		turn := string(rune(100 + i))
		observe(t, &s, turn, "cyberPolicy", now)
		if err := s.Begin(now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
			t.Fatal(err)
		}
		// A crash after Begin consumes one possible send. Reconciliation, even
		// after a backwards wall-clock jump, cannot reserve a new slot.
		if err := s.Observe(Observation{turn, "failed", "cyberPolicy"}, now.Add(-time.Hour), 0, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
			t.Fatal(err)
		}
		if s.CyberUsed(s.Now(now.Add(-time.Hour))) != i+1 {
			t.Fatal("lost uncertain charge")
		}
		if err := s.Accepted(string(rune(101+i)), now); err != nil {
			t.Fatal(err)
		}
	}
	observe(t, &s, string(rune(130)), "cyberPolicy", now)
	if s.Phase != "exhausted" || s.Pending != nil {
		t.Fatal("31st send permitted")
	}
	observe(t, &s, string(rune(130)), "cyberPolicy", now.Add(10*time.Minute))
	if s.Phase != "exhausted" {
		t.Fatal("timer rearmed exhausted failure")
	}
	if s.CyberUsed(now.UnixMilli()+300000) != 0 {
		t.Fatal("rolling window lower boundary included")
	}
}

func TestCancelDisableAndUnknownOutcome(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(fixtureScope())
	s.Enroll(Observation{})
	observe(t, &s, "failed", "cyberPolicy", now)
	s.Cancel("manual_input")
	if err := s.Begin(now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err == nil {
		t.Fatal("cancelled dispatch allowed")
	}
	observe(t, &s, "failed", "cyberPolicy", now)
	if s.Pending != nil {
		t.Fatal("cancel rearmed same failure")
	}
	observe(t, &s, "second", "cyberPolicy", now)
	if err := s.Begin(now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	id := s.Pending.ID
	s.Cancel("manual_input")
	s.Phase = "reconciling"
	if s.Pending.ID != id || !s.Uncertain() || !s.Cancelled {
		t.Fatal("uncertain cancel lost identity")
	}
	if err := s.Accepted("result", now); err != nil {
		t.Fatal(err)
	}
	if s.Phase != "cancelled" {
		t.Fatal("cancel lost after acceptance")
	}
	s.SetEnabled(false, Observation{"result", "failed", "cyberPolicy"})
	s.SetEnabled(true, Observation{"result", "failed", "cyberPolicy"})
	observe(t, &s, "result", "cyberPolicy", now)
	if s.Pending != nil {
		t.Fatal("reenable restarted old task")
	}
}

func TestNoTextClassifierOrGuessedSettings(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, code := range []string{"server_overloaded", "429", "rateLimit", "CyberPolicy", "capacity"} {
		s := New(fixtureScope())
		s.Enroll(Observation{})
		observe(t, &s, "fail", code, now)
		if s.Phase != "idle" {
			t.Fatalf("accepted %s", code)
		}
	}
	s := New(fixtureScope())
	s.Enroll(Observation{})
	if err := s.Observe(Observation{"unknown-origin", "failed", "serverOverloaded"}, now, 0, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	if s.Phase != "blocked" || s.Reason != "settings_unproven" {
		t.Fatal("missing proof guessed")
	}
}

func TestPrivateStoreRestartAndAdmissionExclusion(t *testing.T) {
	store := Store{Root: t.TempDir(), Workspace: "/workspace"}
	scope := fixtureScope()
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Enroll(Observation{})
	observe(t, &tx.State, "failed", "serverOverloaded", time.Unix(1000, 0))
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := store.Lock(ctx, scope); err == nil {
		t.Fatal("independent descriptor bypassed admission")
	}
	id, deadline := tx.State.Pending.ID, tx.State.DeadlineMS
	tx.Close()
	again, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.State.Pending.ID != id || again.State.DeadlineMS != deadline {
		t.Fatal("restart lost saved schedule")
	}
	info, _ := os.Stat(store.path(scope))
	if info.Mode().Perm() != 0600 {
		t.Fatal("public state")
	}
}

func TestCorruptUnknownVersionAndScopeFailClosed(t *testing.T) {
	store := Store{Root: t.TempDir(), Workspace: "/workspace"}
	scope := fixtureScope()
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	data, _ := os.ReadFile(store.path(scope))
	for _, bad := range []string{strings.Replace(string(data), `"schema":1`, `"schema":2`, 1), strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(string(data), `"root":"root"`, `"root":"foreign"`, 1), string(data) + `{}`} {
		if err := os.WriteFile(store.path(scope), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Lock(context.Background(), scope); err == nil {
			t.Fatal("corrupt state admitted")
		}
	}
	if filepath.Ext(store.path(scope)) != ".json" {
		t.Fatal("fixture path")
	}
}

func TestAdmissionFenceAcrossProcesses(t *testing.T) {
	if root := os.Getenv("TURN_RETRY_CHILD_ROOT"); root != "" {
		store := Store{Root: root, Workspace: "/workspace"}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if tx, err := store.Lock(ctx, fixtureScope()); err == nil {
			tx.Close()
			t.Fatal("child process bypassed admission fence")
		}
		return
	}
	store := Store{Root: t.TempDir(), Workspace: "/workspace"}
	tx, err := store.Lock(context.Background(), fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestAdmissionFenceAcrossProcesses$", "-test.count=1")
	command.Env = append(os.Environ(), "TURN_RETRY_CHILD_ROOT="+store.Root)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("independent process: %s %v", data, err)
	}
}

func TestLifecycleCancellationPreservesUncertainIdentityAndCancelsOtherThreads(t *testing.T) {
	store := Store{Root: t.TempDir(), Workspace: "/workspace"}
	scope := fixtureScope()
	now := time.Unix(1000, 0)
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Enroll(Observation{})
	observe(t, &tx.State, "fail", "cyberPolicy", now)
	if err := tx.State.Begin(now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	id := tx.State.Pending.ID
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	second := scope
	second.Thread = "member"
	tx, err = store.Lock(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Enroll(Observation{})
	observe(t, &tx.State, "member-fail", "serverOverloaded", now)
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	if err := store.CancelSession(context.Background(), scope.Slug, scope.Root, scope.Socket); err == nil {
		t.Fatal("uncertain lifecycle completion admitted")
	}
	a, _ := store.Load(scope)
	b, _ := store.Load(second)
	if !a.Cancelled || !a.Uncertain() || a.Pending.ID != id || b.Phase != "cancelled" || b.Pending != nil {
		t.Fatal("lifecycle cancellation lost work or stopped at first uncertainty")
	}
}
