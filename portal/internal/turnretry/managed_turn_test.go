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

	"github.com/aither64/codex-web/codex"
)

func managedState() State {
	s := New(fixtureScope())
	b := Binding{Epoch: 1, Provider: "provider", SocketIdentity: "socket", CommonInstructions: strings.Repeat("instructions", 10000)}
	b.Digest, _ = b.Hash()
	s.Binding = &b
	return s
}

func TestManagedTurnDigestWireFixture(t *testing.T) {
	seed := ManagedTurnSeed(fixtureScope(), 1, "binding", "turn")
	if seed != "b402f66572708a807cb31f84030af5e8b36fd28d96b23703ef11088c76ae6041" || ManagedInputDigest(seed, "native-1", "client-1", hash("hello")) != "e656f11ca154f51e9d9b4bcc11a9dcdf280f939ba82b0434609bc4e1774552e1" {
		t.Fatal("managed prefix SHA256/JSON wire representation changed")
	}
}

func managedAdmission(s State, id string) Snapshot {
	p := Snapshot{Epoch: s.Epoch, Version: 1, ID: id, Origin: "steer", Provider: s.Binding.Provider, SocketIdentity: s.Binding.SocketIdentity,
		CommonInstructions: s.Binding.CommonInstructions, BindingDigest: s.Binding.Digest, TextDigest: hash("input " + id)}
	p.Options.ThreadPolicy = s.Binding.Policy
	p.Options.AdditionalContext = map[string]codex.AdditionalContextEntry{"application": {Kind: "application", Value: strings.Repeat("context", 5000)}}
	p.Digest, _ = p.Hash()
	return p
}

func TestLifecycleCancellationClearsCompactAuthority(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		t.Run(fmt.Sprint("scheduled=", scheduled), func(t *testing.T) {
			ctx, scope := context.Background(), fixtureScope()
			store := Store{Root: t.TempDir(), Workspace: scope.Workspace}
			s := managedState()
			s.Enroll(Observation{})
			p := managedAdmission(s, "accepted")
			if err := s.AdmitSnapshot(p, "active"); err != nil {
				t.Fatal(err)
			}
			if err := s.CompactManagedInput("active", "native-accepted", p.ID, p.TextDigest); err != nil {
				t.Fatal(err)
			}
			if scheduled {
				observe(t, &s, "active", "cyberPolicy", time.Unix(1000, 0))
			}
			tx, err := store.Lock(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			s.evidence = tx.State.evidence
			tx.State = s
			err = tx.Save()
			tx.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := s
			want.Cancel("lifecycle")
			want.Binding, want.ManagedTurn = nil, nil
			if err := store.CancelSession(ctx, scope.Slug, scope.Root, scope.Socket); err != nil {
				t.Fatal("compact lifecycle cancellation failed", err)
			}
			after, err := store.Load(scope)
			if err != nil || !sameState(after, want) {
				t.Fatal("cancellation changed unrelated evidence", after, want, err)
			}
		})
	}
}

func TestLifecycleCancellationRetainsCompactUncertainRecovery(t *testing.T) {
	for _, recovery := range []string{"no-backend-accepted", "unknown-accepted", "unknown-never", "unknown-unknown"} {
		t.Run(recovery, func(t *testing.T) {
			ctx, scope := context.Background(), fixtureScope()
			store := Store{Root: t.TempDir(), Workspace: scope.Workspace}
			member := scope
			member.Thread = "member"
			var before State
			for _, target := range []Scope{scope, member} {
				s := managedState()
				s.Scope = target
				s.Enroll(Observation{})
				p := managedAdmission(s, "accepted")
				if err := s.AdmitSnapshot(p, "active"); err != nil {
					t.Fatal(err)
				}
				if err := s.CompactManagedInput("active", "native-accepted", p.ID, p.TextDigest); err != nil {
					t.Fatal(err)
				}
				if target == scope {
					now := time.Unix(1000, 0)
					observe(t, &s, "active", "cyberPolicy", now)
					if err := s.Begin(now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
						t.Fatal(err)
					}
					before = s
				}
				tx, err := store.Lock(ctx, target)
				if err != nil {
					t.Fatal(err)
				}
				s.evidence = tx.State.evidence
				tx.State = s
				err = tx.Save()
				tx.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			unknown := &fakeBackend{unknown: true, sends: 1}
			var err error
			if strings.HasPrefix(recovery, "no-backend") {
				err = store.CancelSession(ctx, scope.Slug, scope.Root, scope.Socket)
			} else {
				err = store.CancelSession(ctx, scope.Slug, scope.Root, scope.Socket, unknown)
			}
			if err == nil || !strings.Contains(err.Error(), "unresolved automatic submission") {
				t.Fatal("uncertain lifecycle blocker changed", err)
			}
			after, err := store.Load(scope)
			want := before
			want.Cancel("lifecycle")
			want.Binding, want.ManagedTurn = nil, nil
			if err != nil || !sameState(after, want) || unknown.sends != 1 {
				t.Fatal("uncertain tuple, charge or cancellation changed", after, want, err)
			}
			other, err := store.Load(member)
			if err != nil || !other.Cancelled || other.Binding != nil || other.ManagedTurn != nil {
				t.Fatal("uncertain member prevented other cancellation", other, err)
			}
			store = Store{Root: store.Root, Workspace: store.Workspace}
			backend := &fakeBackend{}
			switch {
			case strings.HasSuffix(recovery, "accepted"):
				backend.accepted = "accepted-turn"
			case strings.HasSuffix(recovery, "never"):
				backend.never = true
			}
			err = store.CancelSession(ctx, scope.Slug, scope.Root, scope.Socket, backend)
			if (err != nil) != strings.HasSuffix(recovery, "unknown") {
				t.Fatal("receipt-only restart settlement changed", err)
			}
			settled, loadErr := store.Load(scope)
			if loadErr != nil || settled.Binding != nil || settled.ManagedTurn != nil || settled.Epoch != before.Epoch ||
				len(settled.Cyber) != 1 || settled.Cyber[0].ID != before.Pending.ID || backend.sends != 0 {
				t.Fatal("settlement restored authority or changed quota identity", settled, loadErr)
			}
			switch {
			case backend.accepted != "":
				if settled.Pending == nil || settled.Pending.ID != before.Pending.ID || settled.Pending.ResultTurn != backend.accepted ||
					!reflect.DeepEqual(settled.Pending.Snapshot, before.Pending.Snapshot) || !settled.Cyber[0].Accepted || settled.Handled != backend.accepted {
					t.Fatal("exact accepted receipt was lost", settled)
				}
			case backend.never:
				if settled.Pending != nil || !reflect.DeepEqual(settled.Cyber, before.Cyber) {
					t.Fatal("never-submitted settlement changed the reserved charge", settled)
				}
			default:
				if !sameState(settled, after) {
					t.Fatal("unknown restart changed recovery evidence", settled, after)
				}
			}
			err = store.CancelSession(ctx, scope.Slug, scope.Root, scope.Socket, backend)
			if (err != nil) != strings.HasSuffix(recovery, "unknown") {
				t.Fatal("repeated cancellation changed its result", err)
			}
			repeated, loadErr := store.Load(scope)
			if loadErr != nil || backend.sends != 0 || !reflect.DeepEqual(repeated.Cyber, settled.Cyber) || repeated.Binding != nil || repeated.ManagedTurn != nil {
				t.Fatal("repeated cancellation dispatched or charged again", repeated, loadErr)
			}
		})
	}
}

func TestManagedTurnCompactsAcceptedVolumeAndContextOnce(t *testing.T) {
	s := managedState()
	var firstSize int
	for i := 0; i < 220; i++ {
		p := managedAdmission(s, fmt.Sprint(i))
		if err := s.AdmitSnapshot(p, "active"); err != nil {
			t.Fatal(i, err)
		}
		if err := s.CompactManagedInput("active", "native-"+p.ID, p.ID, p.TextDigest); err != nil {
			t.Fatal(i, err)
		}
		data, _ := json.Marshal(s)
		if i == 0 {
			firstSize = len(data)
		}
		if len(data) > firstSize+20 || len(s.Snapshots) != 0 || s.ManagedTurn.Count != uint64(i+1) || s.Handled != "" {
			t.Fatal("accepted volume grew evidence or consumed authority", i, len(data), len(s.Snapshots), s.Handled)
		}
	}
	if len(s.ManagedTurn.AdditionalContext) != 1 {
		t.Fatal("repeated context was duplicated")
	}
}

func TestManagedTurnOutstandingVolumePreservesAuthority(t *testing.T) {
	s := managedState()
	s.Binding.CommonInstructions = "instructions"
	s.Binding.Digest, _ = s.Binding.Hash()
	for i := 0; i < 220; i++ {
		p := managedAdmission(s, fmt.Sprint(i))
		p.Options.AdditionalContext = nil
		p.Digest, _ = p.Hash()
		if err := s.AdmitSnapshot(p, "active"); err != nil {
			t.Fatal(err)
		}
	}
	if s.Handled != "" || len(s.Snapshots) != 220 {
		t.Fatal("volume changed authority")
	}
	for i := 0; i < 220; i++ {
		p := s.Snapshots[0]
		if err := s.CompactManagedInput("active", "native-"+p.ID, p.ID, p.TextDigest); err != nil {
			t.Fatal(err)
		}
	}
	if s.ManagedTurn.Count != 220 || len(s.Snapshots) != 0 || s.Handled != "" {
		t.Fatal("volume did not compact")
	}
}

func TestManagedTurnFoldConflictAndPendingAreAtomic(t *testing.T) {
	s := managedState()
	p := managedAdmission(s, "first")
	s.Snapshots = []Snapshot{p}
	id, _ := NewID()
	s.Pending = &Pending{ID: id, Failure: "older", Action: "saved", Snapshot: proof("older")}
	pending := *s.Pending
	if err := s.CompactManagedInput("active", "native-first", p.ID, p.TextDigest); err != nil || !reflect.DeepEqual(pending, *s.Pending) {
		t.Fatal("fold rewrote pending identity", err)
	}
	p = managedAdmission(s, "second")
	p.Options.AdditionalContext["application"] = codex.AdditionalContextEntry{Kind: "application", Value: "conflict"}
	p.Digest, _ = p.Hash()
	s.Snapshots = append(s.Snapshots, p)
	before, _ := json.Marshal(s)
	if err := s.CompactManagedInput("active", "native-second", p.ID, p.TextDigest); !errors.Is(err, ErrMissingProof) {
		t.Fatal("context conflict accepted", err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("failed fold changed prefix/source/pending")
	}
}

func TestManagedTurnSchemaOneLoadDoesNotRewriteAndSaveUpgrades(t *testing.T) {
	store, scope := Store{Root: t.TempDir(), Workspace: "/workspace"}, fixtureScope()
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	state := managedState()
	state.Schema = 1
	bytes, _ := json.Marshal(state)
	if err := os.WriteFile(store.path(scope), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	loaded, err := store.Load(scope)
	unchanged, _ := os.ReadFile(store.path(scope))
	if err != nil || loaded.Schema != 1 || string(unchanged) != string(bytes) {
		t.Fatal("reading predecessor changed its bytes", err)
	}
	tx, err = store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	if err := tx.Save(); err != nil || tx.State.Schema != Schema {
		t.Fatal("ordinary admitted save did not upgrade", err)
	}
	for _, bad := range []string{strings.Replace(string(bytes), `"schema":1`, `"schema":3`, 1), strings.Replace(string(bytes), `"schema":1`, `"schema":1,"managedTurn":null`, 1)} {
		if err := os.WriteFile(store.path(scope), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(scope); err == nil {
			t.Fatal("unsupported schema or preliminary hybrid accepted")
		}
	}
}

func TestRetireObsoleteProofsRequiresFreshNonemptyTurnAndPreservesPending(t *testing.T) {
	s := managedState()
	current, old, protected := proof("current"), proof("old"), proof("protected")
	unbound := managedAdmission(s, "waiting")
	id, _ := NewID()
	s.Pending = &Pending{ID: id, Action: "exact", Failure: protected.Turn, Snapshot: protected}
	s.Snapshots = []Snapshot{current, old, protected, unbound}
	s.Handled = "handled"
	before, _ := json.Marshal(s)
	if s.RetireObsoleteProofs("") {
		t.Fatal("empty observation retired proof")
	}
	unchanged, _ := json.Marshal(s)
	if string(before) != string(unchanged) {
		t.Fatal("empty observation changed state")
	}
	pending := *s.Pending
	if !s.RetireObsoleteProofs("current") || !reflect.DeepEqual(s.Snapshots, []Snapshot{current, protected, unbound}) || s.Handled != "handled" || !reflect.DeepEqual(*s.Pending, pending) {
		t.Fatal("fresh filter changed current/pending/unbound authority")
	}
	s.Pending = nil
	if !s.RetireObsoleteProofs("current") || !reflect.DeepEqual(s.Snapshots, []Snapshot{current, unbound}) {
		t.Fatal("released old pending proof did not retire on later fresh observation")
	}
}

func TestManagedTurnAtomicRecordAndStrictShapes(t *testing.T) {
	store, scope := Store{Root: t.TempDir(), Workspace: "/workspace"}, fixtureScope()
	tx, err := store.Lock(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	initial := managedState()
	initial.evidence = tx.State.evidence
	tx.State = initial
	p := managedAdmission(tx.State, "accepted")
	tx.State.Snapshots = []Snapshot{p}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	if err := tx.State.CompactManagedInput("active", "native", p.ID, p.TextDigest); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Load(scope)
	if before.ManagedTurn != nil || len(before.Snapshots) != 1 {
		t.Fatal("unsaved fold removed durable admissions")
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Load(scope)
	if after.ManagedTurn == nil || after.ManagedTurn.Count != 1 || len(after.Snapshots) != 0 {
		t.Fatal("prefix and source removal were not one record")
	}
	valid, _ := json.Marshal(tx.State)
	saved := tx.State
	for _, missing := range []bool{true, false} {
		tx.State = saved
		if missing {
			tx.State.Binding = nil
		} else {
			binding := *saved.Binding
			binding.Provider = "changed-provider"
			binding.Digest, _ = binding.Hash()
			tx.State.Binding = &binding
		}
		if err := tx.Save(); err == nil {
			t.Fatal("unbound or mismatched compact authority was published")
		}
		current, err := store.Load(scope)
		bytes, _ := json.Marshal(current)
		if err != nil || string(bytes) != string(valid) {
			t.Fatal("failed validation changed the atomic record", err)
		}
		if _, err := store.Load(scope); err != nil {
			t.Fatal("failed save left an unreadable compact record", err)
		}
	}
	tx.State = saved
	var null map[string]any
	_ = json.Unmarshal(valid, &null)
	null["managedTurn"] = nil
	data, _ := json.Marshal(null)
	if _, err := decodeState(scope, data); err == nil {
		t.Fatal("explicit null compact authority was accepted as legacy state")
	}
	for name, value := range map[string]any{"version": 2, "epoch": 2, "turn": "", "count": -1, "digest": "BAD", "lastUserItemId": "", "bindingDigest": "wrong", "additionalContext": nil, "unknown": true} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]any
			_ = json.Unmarshal(valid, &fields)
			fields["managedTurn"].(map[string]any)[name] = value
			bad, _ := json.Marshal(fields)
			if _, err := decodeState(scope, bad); err == nil {
				t.Fatal("malformed compact authority accepted")
			}
		})
	}
}

func sameState(a, b State) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
