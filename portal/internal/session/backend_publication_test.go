package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func TestPiRuntimeUpgradeAuthorityRejectsUnprovenGenerationWithoutWriting(t *testing.T) {
	binding := backendBindingFixture()
	authority := backendAuthorityFixture()
	directory := filepath.Join(t.TempDir(), "authority")
	if err := PublishPiRuntimeAuthority(directory, binding, authority); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, binding.Slug+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishPiRuntimeUpgradeAuthority(directory, binding, authority, piruntime.UpgradeProof{}); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatal("unproven successor published", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed successor proof changed authority", err)
	}
	changed := authority
	changed.Generation = strings.Repeat("f", 64)
	if err := PublishPiRuntimeAuthority(directory, binding, changed); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatal("ordinary publisher admitted successor", err)
	}
}

func TestBackendPublicationCASAndInterruptedProjection(t *testing.T) {
	ctx := context.Background()
	store := BackendBindingStore{Root: t.TempDir(), Workspace: "/srv/workspace"}
	reserved := backendBindingFixture()
	reserved.State = "creating"
	for i := range reserved.Conversations {
		reserved.Conversations[i].State, reserved.Conversations[i].NativeID = "creating", ""
	}
	created, err := store.Create(ctx, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := store.Create(ctx, reserved); err != nil || !reflect.DeepEqual(created, replay) {
		t.Fatalf("creation replay changed identity: %#v %v", replay, err)
	}
	conflict := reserved
	conflict.CreationReceiptID = "different-owner"
	if _, err := store.Create(ctx, conflict); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("foreign create: %v", err)
	}
	authorityDirectory := filepath.Join(t.TempDir(), "authority")
	authority := backendAuthorityFixture()
	authority.State = "starting"
	if err := PublishPiRuntimeAuthority(authorityDirectory, created, authority); err != nil {
		t.Fatal(err)
	}
	ready := created
	ready.Conversations = append([]BackendConversation(nil), created.Conversations...)
	ready.State, ready.Revision = "ready", 2
	ready.Conversations[0].State, ready.Conversations[0].NativeID = "ready", "native-lead"
	committed, err := store.CompareAndSwap(ctx, 1, ready)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Conversations[1].State != "creating" {
		t.Fatal("root readiness invented a ready member")
	}
	// Simulate the crash boundary after binding publication and before live
	// projection. The mismatch must keep dispatch held after reopening stores.
	reopened, err := store.Load(reserved.Slug)
	if err != nil {
		t.Fatal(err)
	}
	live, err := LoadPiRuntimeAuthority(authorityDirectory, reserved.Slug, store.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.ValidateBinding(reopened); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("partial publication dispatched: %v", err)
	}
	if _, err := store.CompareAndSwap(ctx, 1, ready); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	authority.BindingRevision, authority.State = 2, "ready"
	if err := PublishPiRuntimeAuthority(authorityDirectory, reopened, authority); err != nil {
		t.Fatal(err)
	}
	if err := PublishPiRuntimeAuthority(authorityDirectory, reopened, authority); err != nil {
		t.Fatalf("projection replay: %v", err)
	}
	live, err = LoadPiRuntimeAuthority(authorityDirectory, reserved.Slug, store.Workspace)
	if err != nil || live.ValidateBinding(reopened) != nil {
		t.Fatalf("projection not reconciled: %v", err)
	}
	stale := authority
	stale.Generation = strings.Repeat("f", 64)
	if err := PublishPiRuntimeAuthority(authorityDirectory, reopened, stale); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("generation silently replaced: %v", err)
	}
}

func TestBackendPublicationRefusesMalformedAndLegacyState(t *testing.T) {
	store := BackendBindingStore{Root: t.TempDir(), Workspace: "/srv/workspace"}
	reserved := backendBindingFixture()
	reserved.State = "creating"
	for i := range reserved.Conversations {
		reserved.Conversations[i].State, reserved.Conversations[i].NativeID = "creating", ""
	}
	directory, err := store.publicationDirectory(reserved.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, reserved.Slug+".json")
	bytes := []byte(`{"schema":9}`)
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), reserved); err == nil {
		t.Fatal("malformed binding repaired")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(bytes) {
		t.Fatal("malformed authority overwritten")
	}
	authorityDirectory := t.TempDir()
	legacy := []byte(`{"schema":1,"state":"ready","slug":"example","workspace":"/srv/workspace","tmux_socket":"/run/tmux.sock","tmux_session_id":"$1"}`)
	authorityPath := filepath.Join(authorityDirectory, reserved.Slug+".json")
	if err := os.WriteFile(authorityPath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := PublishPiRuntimeAuthority(authorityDirectory, backendBindingFixture(), backendAuthorityFixture()); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("legacy authority adopted: %v", err)
	}
	after, _ = os.ReadFile(authorityPath)
	if string(after) != string(legacy) {
		t.Fatal("legacy authority overwritten")
	}
}

func TestPiRuntimeUpgradeProjectionPreservesRemovedAndUnstartedBindings(t *testing.T) {
	b := BackendBinding{Conversations: []BackendConversation{{Address: "lead", Incarnation: strings.Repeat("a", 64), NativeID: "lead", State: "ready"}, {Address: "implementer0", Incarnation: strings.Repeat("b", 64), State: "creating"}, {Address: "reviewer0", Incarnation: strings.Repeat("c", 64), NativeID: "reviewer", State: "ready"}}}
	rows := []PiRuntimeUpgradeMember{{Address: "implementer0", Incarnation: strings.Repeat("b", 64), State: "creating"}, {Address: "reviewer0", Incarnation: strings.Repeat("c", 64), State: "removed"}}
	// A real binding fixture supplies the owning scope/record shape below.
	b.Schema = 1
	b.Backend = agentbackend.Pi
	b.BackendContract = 1
	b.Workspace = t.TempDir()
	b.Slug = "2026-11-19-projection"
	b.SessionIncarnation = strings.Repeat("d", 64)
	b.CreationReceiptID = strings.Repeat("e", 64)
	b.Revision = 3
	b.State = "ready"
	for _, schema := range []int{2, 3, 4, 5} {
		p, e := ProjectPiRuntimeUpgrade(b, schema, 4, rows)
		if e != nil || len(p.Members()) != 1 || p.Members()[0].Member != "lead" {
			t.Fatal(schema, p, e)
		}
	}
	for _, change := range []string{"missing", "incarnation", "pending", "mapped"} {
		r := append([]PiRuntimeUpgradeMember{}, rows...)
		bound := b
		bound.Conversations = append([]BackendConversation{}, b.Conversations...)
		switch change {
		case "missing":
			r = r[:1]
		case "incarnation":
			r[1].Incarnation = strings.Repeat("f", 64)
		case "pending":
			r[0].InitialInputID = "11111111-1111-4111-8111-111111111111"
		case "mapped":
			bound.Conversations[1].NativeID = "partial"
		}
		if _, e := ProjectPiRuntimeUpgrade(bound, 5, 4, r); e == nil {
			t.Fatal("unsafe projection", change)
		}
	}
}

func TestPiTeamConflictProjectionKeepsReservedCreatingSeparate(t *testing.T) {
	binding := backendBindingFixture()
	binding.Conversations[1].State = "creating"
	binding.Conversations[1].NativeID = ""
	rows := []PiRuntimeUpgradeMember{{Address: binding.Conversations[1].Address, Incarnation: binding.Conversations[1].Incarnation, State: "creating", InitialInputID: "11111111-1111-4111-8111-111111111111"}}
	if _, err := ProjectPiRuntimeUpgrade(binding, 5, 2, rows); err == nil {
		t.Fatal("ordinary upgrade adopted recovery exception")
	}
	p, err := ProjectPiRuntimeTeamConflictUpgrade(binding, 5, 2, rows)
	if err != nil || len(p.Members()) != 1 || len(p.retainedCreating) != 1 || !reflect.DeepEqual(p.binding, binding) {
		t.Fatal(p, err)
	}
	for _, fault := range []string{"fork", "mapped", "ready", "wrong-scope"} {
		b := binding
		b.Conversations = append([]BackendConversation{}, binding.Conversations...)
		r := append([]PiRuntimeUpgradeMember{}, rows...)
		switch fault {
		case "fork":
			r[0].Fork = true
		case "mapped":
			b.Conversations[1].NativeID = "partial"
		case "ready":
			b.Conversations[1].State = "ready"
		case "wrong-scope":
			r[0].Incarnation = strings.Repeat("f", 64)
		}
		if _, err := ProjectPiRuntimeTeamConflictUpgrade(b, 5, 2, r); err == nil {
			t.Fatal("unsafe retained empty", fault)
		}
	}
}
