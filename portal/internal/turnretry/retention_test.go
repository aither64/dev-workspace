package turnretry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
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

func TestEvidenceRetainsVolumeAndDeduplicatesSettings(t *testing.T) {
	store, scope := Store{Root: t.TempDir(), Workspace: "/workspace"}, fixtureScope()
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 220; i++ {
		p := retentionSnapshot(fmt.Sprintf("input-%03d", i), "active")
		p.CommonInstructions = strings.Repeat("retained settings ", 1000)
		p.Digest, _ = p.Hash()
		if err := tx.State.AdmitSnapshot(p, "active"); err != nil {
			t.Fatal(err)
		}
	}
	if tx.State.Handled != "" {
		t.Fatal("volume suppressed active work")
	}
	data, _ := json.Marshal(tx.State)
	if len(data) <= MaxBytes {
		t.Fatal("fixture must exceed former aggregate byte limit")
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	saved, err := store.Load(scope)
	if err != nil || len(saved.Snapshots) != snapshotReadBatch {
		t.Fatal("unbounded load", len(saved.Snapshots), err)
	}
	count, err := saved.SnapshotCount()
	if err != nil || count != 220 {
		t.Fatal(count, err)
	}
	p, err := saved.Snapshot("input-219")
	if err != nil || p == nil || p.CommonInstructions == "" {
		t.Fatal("indexed lookup lost input", err)
	}
	if len(saved.Snapshots) != snapshotReadBatch+1 {
		t.Fatal("lookup materialized history")
	}
	db, err := openEvidence(store.databasePath(scope), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT count(*) FROM retry_settings").Scan(&count); err != nil || count != 1 {
		t.Fatal("settings not deduplicated", count, err)
	}
}

func TestEvidencePartialSaveAndStaleRevision(t *testing.T) {
	store, scope := Store{Root: t.TempDir(), Workspace: "/workspace"}, fixtureScope()
	tx, _ := store.Lock(context.Background(), scope)
	fillRetention(&tx.State, "active", 200)
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	stale, _ := store.Load(scope)
	tx, _ = store.Lock(context.Background(), scope)
	p, err := tx.State.Snapshot("input-199")
	if err != nil || p == nil {
		t.Fatal(err)
	}
	p.Turn = "accepted"
	if err := tx.State.AddSnapshot(*p); err != nil {
		t.Fatal(err)
	}
	removed := tx.State.Snapshots[0].ID
	tx.State.Snapshots = tx.State.Snapshots[1:]
	// A deleted loaded row cannot be resurrected by a fallback lookup.
	if p, err := tx.State.Snapshot(removed); err != nil || p != nil {
		t.Fatal("resurrected local deletion", err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	tx, _ = store.Lock(context.Background(), scope)
	defer tx.Close()
	count, _ := tx.State.SnapshotCount()
	if count != 199 {
		t.Fatal("partial save deleted unseen rows", count)
	}
	p, _ = tx.State.Snapshot("input-199")
	if p == nil || p.Turn != "accepted" {
		t.Fatal("indexed update lost")
	}
	tx.State = stale
	if err := tx.Save(); err == nil {
		t.Fatal("stale state overwrote newer revision")
	}
}

func TestEvidenceAcceptedReceiptFailureAndRecovery(t *testing.T) {
	c, b, now := controllerFixture(t)
	scope := fixtureScope()
	tx, _ := c.Store.Lock(context.Background(), scope)
	tx.State.Enroll(Observation{})
	tx.State.Boundary = b.boundary
	fillRetention(&tx.State, "", 200)
	id, _ := NewID()
	tx.State.Pending = &Pending{ID: id, Failure: "failed", Action: "exact", Snapshot: proof("failed"), ChargedAtMS: now.UnixMilli()}
	original := tx.State.Pending.Snapshot
	tx.State.Cyber = []Charge{{ID: id, AtMS: now.UnixMilli()}}
	tx.State.Class, tx.State.Phase = "cybersecurity", "submitting"
	if err := tx.State.PreflightAccepted(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	if err := tx.State.Accepted("accepted", *now); err != nil {
		t.Fatal(err)
	}
	directory := c.Store.directory() + "/" + scope.Slug
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	if err := tx.Save(); err == nil {
		t.Fatal("receipt persistence failure hidden")
	}
	_ = os.Chmod(directory, 0700)
	tx.Close()
	s, _ := c.Store.Load(scope)
	if !s.Uncertain() || s.Pending.ID != id || s.Pending.ResultTurn != "" {
		t.Fatal("lost uncertain identity")
	}
	b.accepted, b.never = "accepted", false
	for i := 0; i < 2; i++ {
		if err := c.ReconcileOnly(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
	}
	s, _ = c.Store.Load(scope)
	count, _ := s.SnapshotCount()
	if s.Pending.ResultTurn != "accepted" || !reflect.DeepEqual(s.Pending.Snapshot, original) || count != 201 || !s.Cyber[0].Accepted || b.sends != 0 {
		t.Fatal("lost exact acceptance", count)
	}
}
