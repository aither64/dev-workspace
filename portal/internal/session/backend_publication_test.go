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
)

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
