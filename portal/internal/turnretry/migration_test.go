package turnretry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEvidenceMigrationAndInterruptedCutover(t *testing.T) {
	for _, schema := range []int{1, 2} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("schema-%d/interrupted-%t", schema, interrupted), func(t *testing.T) {
				store, scope := Store{Root: t.TempDir(), Workspace: "/workspace"}, fixtureScope()
				s := New(scope)
				s.Schema = schema
				s.Enroll(Observation{})
				observe(t, &s, "failed", "cyberPolicy", time.Unix(1000, 0))
				if err := s.Begin(time.Unix(1000, 0), Decision{Revision: "test", Capacity: true, Cybersecurity: true}); err != nil {
					t.Fatal(err)
				}
				s.Cancel("interrupted")
				data, _ := json.Marshal(s)
				if err := os.MkdirAll(filepath.Dir(store.path(scope)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := atomicPrivateFile(store.path(scope), data); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Load(scope); err != nil {
					t.Fatal(err)
				}
				unchanged, _ := os.ReadFile(store.path(scope))
				if string(unchanged) != string(data) {
					t.Fatal("read migrated legacy state")
				}
				if interrupted {
					if err := atomicPrivateFile(store.path(scope)+".legacy", data); err != nil {
						t.Fatal(err)
					}
					marker, _ := json.Marshal(evidenceMarker{Schema: Schema, Scope: scope, Source: "legacy"})
					if err := atomicPrivateFile(store.path(scope), marker); err != nil {
						t.Fatal(err)
					}
					if _, err := store.Load(scope); err == nil || errors.Is(err, os.ErrNotExist) {
						t.Fatal("unfinished cutover appeared absent", err)
					}
				}
				tx, err := store.Lock(context.Background(), scope)
				if err != nil {
					t.Fatal(err)
				}
				s.Schema = Schema
				if !sameState(tx.State, s) {
					t.Fatal("migration changed cancellation or uncertain identity")
				}
				if err := tx.State.Accepted("accepted", time.Unix(1001, 0)); err != nil {
					t.Fatal(err)
				}
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
				backup, _ := os.ReadFile(store.path(scope) + ".legacy")
				if string(backup) != string(data) {
					t.Fatal("legacy recovery evidence changed")
				}
				marker, _ := os.ReadFile(store.path(scope))
				var legacy State
				if err := json.Unmarshal(marker, &legacy); err != nil || legacy.Schema <= 2 {
					t.Fatal("old strict reader could accept cutover marker")
				}
				// Crash after publishing the database but before the final marker must
				// retain new database state, rather than importing the backup again.
				var interruptedMarker evidenceMarker
				_ = json.Unmarshal(marker, &interruptedMarker)
				interruptedMarker.Ready = false
				incomplete, _ := json.Marshal(interruptedMarker)
				if err := atomicPrivateFile(store.path(scope), incomplete); err != nil {
					t.Fatal(err)
				}
				recovered, err := store.Lock(context.Background(), scope)
				if err != nil {
					t.Fatal(err)
				}
				if recovered.State.Pending.ResultTurn != "accepted" {
					t.Fatal("cutover recovery restored stale JSON")
				}
				recovered.Close()
				// Database loss after completed cutover must never restore stale JSON.
				if err := os.Remove(store.databasePath(scope)); err != nil {
					t.Fatal(err)
				}
				if tx, err := store.Lock(context.Background(), scope); err == nil {
					tx.Close()
					t.Fatal("lost database reset accepted state")
				}
			})
		}
	}
}

func TestEvidenceEmptyCutoverRecoveryAndMissingBackup(t *testing.T) {
	for _, source := range []string{"empty", "legacy"} {
		t.Run(source, func(t *testing.T) {
			store, scope := Store{Root: t.TempDir(), Workspace: "/workspace"}, fixtureScope()
			if err := os.MkdirAll(filepath.Dir(store.path(scope)), 0700); err != nil {
				t.Fatal(err)
			}
			marker, _ := json.Marshal(evidenceMarker{Schema: Schema, Scope: scope, Source: source})
			if err := atomicPrivateFile(store.path(scope), marker); err != nil {
				t.Fatal(err)
			}
			tx, err := store.Inspect(context.Background(), scope)
			if source == "legacy" {
				if err == nil {
					tx.Close()
					t.Fatal("missing backup became a fresh state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Close()
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEvidenceBindsQueuedInputsAcrossReadBatches(t *testing.T) {
	store, scope := Store{Root: t.TempDir(), Workspace: "/workspace"}, fixtureScope()
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	s := managedState()
	s.Binding.CommonInstructions = "shared instructions"
	s.Binding.Digest, _ = s.Binding.Hash()
	for i := 0; i < 150; i++ {
		p := managedAdmission(s, fmt.Sprintf("queue-%03d", i))
		p.BindingDigest, p.Origin, p.Options.AdditionalContext = "", "queue", nil
		p.Digest, _ = p.Hash()
		tx.State.Snapshots = append(tx.State.Snapshots, p)
	}
	tx.State.Binding = s.Binding
	if err := tx.BindUnboundSnapshots(*s.Binding); err != nil {
		t.Fatal(err)
	}
	if count, _ := tx.State.SnapshotCount(); count != 150 || len(tx.State.Snapshots) > snapshotReadBatch {
		t.Fatal("binding lost or materialized queue", count)
	}
	for i := 0; i < 150; i++ {
		state, err := store.Load(scope)
		if err != nil {
			t.Fatal(err)
		}
		p, err := state.Snapshot(fmt.Sprintf("queue-%03d", i))
		if err != nil || p == nil || p.BindingDigest != s.Binding.Digest {
			t.Fatal("queued binding missing", i, err)
		}
	}
}
