package turnretry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func retentionSnapshot(id, turn string) Snapshot {
	p := proof(turn)
	p.ID, p.Origin, p.Proven = id, "steer", false
	p.Digest, _ = p.Hash()
	return p
}

func fillRetention(s *State, turn string, n int) {
	for i := 0; i < n; i++ {
		s.Snapshots = append(s.Snapshots, retentionSnapshot(fmt.Sprint("input-", i), turn))
	}
}

func TestRetentionAcceptedOnlyAtomicSuppression(t *testing.T) {
	s := New(fixtureScope())
	s.Enroll(Observation{})
	fillRetention(&s, "active", MaxSnapshots-1)
	if err := s.AddSnapshot(proof("active")); err != nil {
		t.Fatal(err)
	}
	unknown := retentionSnapshot("unknown", "")
	s.Snapshots[1] = unknown
	before := s
	if err := s.AdmitSnapshot(retentionSnapshot("new", ""), "active"); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshots) > MaxSnapshots || s.Handled != "active" || s.Proof("active") != nil || s.Epoch != before.Epoch || s.Enabled != before.Enabled || !reflect.DeepEqual(s.Cyber, before.Cyber) {
		t.Fatal("retirement lost suppression or unrelated authority", s)
	}
	if !reflect.DeepEqual(s.Snapshots[0], unknown) {
		t.Fatal("unbound admission was retired", s.Snapshots[0])
	}
	s.SetEnabled(false, Observation{"active", "inProgress", ""})
	s.SetEnabled(true, Observation{"active", "inProgress", ""})
	if s.Handled != "active" {
		t.Fatal("reenabling resurrected truncated active turn")
	}
	if err := s.InvalidateSettings("active"); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(Observation{"active", "failed", "cyberPolicy"}, time.Now(), 0, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil || s.Pending != nil {
		t.Fatal("settings resurrected truncated turn", err)
	}
}

func TestRetentionProtectedAndByteBudgets(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprint("bound=", bound), func(t *testing.T) {
			s := New(fixtureScope())
			turn := ""
			if bound {
				turn = "source"
			}
			fillRetention(&s, turn, MaxSnapshots)
			if bound {
				id, _ := NewID()
				s.Pending = &Pending{ID: id, Failure: turn, Action: "exact", Snapshot: proof(turn)}
			}
			before, _ := json.Marshal(s)
			if err := s.AdmitSnapshot(retentionSnapshot("new", ""), turn); !errors.Is(err, ErrSnapshotCapacity) {
				t.Fatal("protected full record admitted work", err)
			}
			after, _ := json.Marshal(s)
			if string(before) != string(after) {
				t.Fatal("failed candidate changed valid state")
			}
		})
	}
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint("bytes/accepted=", accepted), func(t *testing.T) {
			s := New(fixtureScope())
			turn := ""
			if accepted {
				turn = "active"
			}
			for i := 0; i < 5; i++ {
				p := retentionSnapshot(fmt.Sprint(i), turn)
				p.CommonInstructions = strings.Repeat("x", 200000)
				p.Digest, _ = p.Hash()
				s.Snapshots = append(s.Snapshots, p)
			}
			p := retentionSnapshot("new", "")
			p.CommonInstructions = strings.Repeat("x", 100000)
			p.Digest, _ = p.Hash()
			err := s.AdmitSnapshot(p, "active")
			if accepted && err != nil || !accepted && !errors.Is(err, ErrSnapshotCapacity) {
				t.Fatal("encoded byte budget did not preserve accepted-only boundary", err)
			}
			if accepted && (len(s.Snapshots) != 5 || s.Handled != "active") {
				t.Fatal("byte pressure did not retire exactly one accepted contributor")
			}
			data, _ := json.Marshal(s)
			if len(data)+1 > MaxBytes {
				t.Fatal("candidate exceeded record budget")
			}
		})
	}
}

func TestRetentionAcceptedAtProtectedCapConverges(t *testing.T) {
	c, b, now := controllerFixture(t)
	scope := fixtureScope()
	tx, err := c.Store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Enroll(Observation{})
	tx.State.Boundary = b.boundary
	fillRetention(&tx.State, "", MaxSnapshots)
	id, _ := NewID()
	tx.State.Pending = &Pending{ID: id, Failure: "failure", Action: "exact", Snapshot: proof("failure"), ChargedAtMS: now.UnixMilli()}
	tx.State.Cyber = []Charge{{ID: id, AtMS: now.UnixMilli()}}
	tx.State.Class, tx.State.Phase = "cybersecurity", "submitting"
	if err := tx.State.PreflightAccepted(); !errors.Is(err, ErrSnapshotCapacity) {
		t.Fatal("fresh dispatch fit without a result slot", err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	original := tx.State.Pending.Snapshot
	tx.Close()
	b.accepted, b.never = "accepted", false
	*now = now.Add(10 * time.Minute)
	for _, status := range []string{"inProgress", "failed", "failed"} {
		b.o = Observation{"accepted", status, "cyberPolicy"}
		if err := c.Step(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
		s, err := c.Store.Load(scope)
		if err != nil || s.Phase != "blocked" || !s.Cancelled || s.Handled != "accepted" || s.Pending.ID != id || s.Pending.ResultTurn != "accepted" || !reflect.DeepEqual(s.Pending.Snapshot, original) || len(s.Snapshots) != MaxSnapshots || s.Proof("accepted") != nil || b.sends != 0 {
			t.Fatal("known receipt did not converge at protected cap", s, err)
		}
		if len(s.Cyber) != 1 || !s.Cyber[0].Accepted || s.Cyber[0].AtMS != now.UnixMilli() {
			t.Fatal("receipt charge lost or reanchored", s.Cyber)
		}
	}
	tx, _ = c.Store.Lock(context.Background(), scope)
	tx.State.Snapshots = tx.State.Snapshots[:1]
	if err := tx.State.Accepted("accepted", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if tx.State.Phase != "blocked" || len(tx.State.Snapshots) != 1 || tx.State.Cyber[0].AtMS != now.UnixMilli() {
		t.Fatal("free space resurrected settled result")
	}
	if err := tx.State.Accepted("conflict", *now); err == nil {
		t.Fatal("conflicting result accepted")
	}
	tx.Close()
}

func TestRetentionFreshAutomaticPreflightAndReceiptSaveFailure(t *testing.T) {
	c, b, now := controllerFixture(t)
	scope := fixtureScope()
	tx, _ := c.Store.Lock(context.Background(), scope)
	tx.State.Enroll(Observation{})
	tx.State.Boundary = b.boundary
	fillRetention(&tx.State, "", MaxSnapshots-1)
	if err := tx.State.AddSnapshot(proof("failed")); err != nil {
		t.Fatal(err)
	}
	if err := tx.State.Observe(Observation{"failed", "failed", "cyberPolicy"}, *now, 0, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	originalPending := *tx.State.Pending
	originalCyber := append([]Charge{}, tx.State.Cyber...)
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	b.o = Observation{"failed", "failed", "cyberPolicy"}
	if err := c.Step(context.Background(), scope); !errors.Is(err, ErrSnapshotCapacity) {
		t.Fatal("full protected automatic result slot was not refused", err)
	}
	s, _ := c.Store.Load(scope)
	if b.sends != 0 || s.Pending == nil || !reflect.DeepEqual(*s.Pending, originalPending) || !reflect.DeepEqual(s.Cyber, originalCyber) || s.Phase != "blocked" {
		t.Fatal("preflight consumed quota or submitted", s)
	}
	for i := 0; i < 2; i++ {
		restarted := Controller{Policy: Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: c.Store, Backend: b, Clock: c.Clock}
		if err := restarted.Step(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
		s, err := c.Store.Load(scope)
		if err != nil || s.Pending == nil || !reflect.DeepEqual(*s.Pending, originalPending) || !reflect.DeepEqual(s.Cyber, originalCyber) || s.Phase != "blocked" || s.Uncertain() || s.Pending.ResultTurn != "" || s.Pending.ChargedAtMS != originalPending.ChargedAtMS || s.Handled != "failed" || b.sends != 0 {
			t.Fatal("blocked preflight lost its tuple or rearmed after reload", s, err)
		}
	}
	// A persistence failure after native acceptance leaves the original durable
	// uncertain tuple for exact retry, not a fabricated successful settlement.
	tx, _ = c.Store.Lock(context.Background(), scope)
	id, _ := NewID()
	tx.State.Pending = &Pending{ID: id, Failure: "failed", Action: "exact", Snapshot: proof("failed")}
	tx.State.Class, tx.State.Phase = "capacity", "submitting"
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	if err := tx.State.Accepted("accepted", *now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.Store.directory()+"/"+scope.Slug, 0500); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); err == nil {
		t.Fatal("receipt persistence failure hidden")
	}
	if err := os.Chmod(c.Store.directory()+"/"+scope.Slug, 0700); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	s, _ = c.Store.Load(scope)
	if !s.Uncertain() || s.Pending.ID != id || s.Pending.ResultTurn != "" {
		t.Fatal("failed receipt save overwrote recovery identity")
	}
	b.accepted = "accepted"
	if err := c.ReconcileOnly(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if b.sends != 0 {
		t.Fatal("receipt recovery submitted a new turn")
	}
}

func TestRetentionProofAndAcceptedReceiptByteBudget(t *testing.T) {
	s := New(fixtureScope())
	for i := 0; i < 4; i++ {
		p := retentionSnapshot(fmt.Sprint(i), "")
		p.CommonInstructions = strings.Repeat("x", 200000)
		p.Digest, _ = p.Hash()
		s.Snapshots = append(s.Snapshots, p)
	}
	p := proof("source")
	p.CommonInstructions = strings.Repeat("x", 200000)
	p.Digest, _ = p.Hash()
	before, _ := json.Marshal(s)
	if err := s.RetainProof(p); !errors.Is(err, ErrSnapshotCapacity) {
		t.Fatal("aggregate did not reserve its pending copy", err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("refused aggregate changed protected admissions")
	}
	id, _ := NewID()
	s.Pending = &Pending{ID: id, Failure: "source", Action: "exact", Snapshot: p}
	s.Phase = "reconciling"
	if err := validate(s.Scope, s); err != nil {
		t.Fatal(err)
	}
	if err := s.PreflightAccepted(); !errors.Is(err, ErrSnapshotCapacity) {
		t.Fatal("automatic result was not preflighted against encoded bytes", err)
	}
	if err := s.Accepted("accepted", time.Now()); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(s)
	if len(data)+1 > MaxBytes || len(s.Snapshots) != 4 || s.Phase != "blocked" || s.Pending.ResultTurn != "accepted" || !reflect.DeepEqual(s.Pending.Snapshot, p) {
		t.Fatal("byte pressure lost exact accepted settlement")
	}
}
