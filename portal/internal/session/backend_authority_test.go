package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func backendAuthorityFixture() PiRuntimeAuthority {
	return PiRuntimeAuthority{
		Schema: 2, Backend: agentbackend.Pi, BackendContract: 1,
		Workspace: "/srv/workspace", Slug: "example", SessionIncarnation: strings.Repeat("a", 64),
		BindingRevision: 1, Generation: strings.Repeat("d", 64), State: "ready",
	}
}

func TestPiRuntimeAuthorityStrictReaderPreservesLegacyBoundary(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "authority")
	if _, err := LoadPiRuntimeAuthority(directory, "example", "/srv/workspace"); !errors.Is(err, agentbackend.ErrNotBound) {
		t.Fatalf("absent live projection: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created directory: %v", err)
	}
	path := filepath.Join(directory, "example.json")
	valid := backendJSON(t, backendAuthorityFixture())
	backendWrite(t, path, valid)
	for range 2 {
		loaded, err := LoadPiRuntimeAuthority(directory, "example", "/srv/workspace")
		if err != nil || loaded != backendAuthorityFixture() {
			t.Fatalf("load: %#v, %v", loaded, err)
		}
	}
	if _, err := LoadRuntimeAuthority(directory, "example", "/srv/workspace"); err == nil {
		t.Fatal("unchanged legacy loader accepted Pi authority")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, valid) {
		t.Fatalf("read changed bytes: %v", err)
	}
	for _, field := range []string{"schema", "backend", "backend_contract", "workspace", "slug", "session_incarnation", "binding_revision", "generation", "state"} {
		for _, operation := range []string{"missing", "null"} {
			t.Run(operation+"-"+field, func(t *testing.T) {
				data := mutateBackendJSON(t, valid, func(object map[string]any) {
					if operation == "missing" {
						delete(object, field)
					} else {
						object[field] = nil
					}
				})
				backendWrite(t, path, data)
				if _, err := LoadPiRuntimeAuthority(directory, "example", "/srv/workspace"); err == nil || errors.Is(err, agentbackend.ErrNotBound) {
					t.Fatalf("invalid projection accepted/absent: %v", err)
				}
			})
		}
	}
	for _, tc := range []struct {
		field string
		value any
		want  error
	}{
		{"schema", 1, agentbackend.ErrUnsupportedBinding}, {"backend", "other", agentbackend.ErrUnsupportedBinding},
		{"backend_contract", 2, agentbackend.ErrUnsupportedBinding}, {"workspace", "/other", nil},
		{"slug", "other", nil}, {"binding_revision", 0, nil}, {"generation", "pid-1", nil},
		{"state", "creating", nil}, {"tmux_identity", strings.Repeat("a", 64), nil}, {"SCHEMA", 2, nil},
	} {
		t.Run(tc.field, func(t *testing.T) {
			backendWrite(t, path, mutateBackendJSON(t, valid, func(object map[string]any) { object[tc.field] = tc.value }))
			_, err := LoadPiRuntimeAuthority(directory, "example", "/srv/workspace")
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPiRuntimeAuthoritySeparatesStableIdentityFromExecution(t *testing.T) {
	binding, authority := backendBindingFixture(), backendAuthorityFixture()
	ref, err := binding.Resolve("implementer2")
	if err != nil {
		t.Fatal(err)
	}
	original := agentbackend.ExecutionHandle{Conversation: ref, Generation: authority.Generation}
	if err := authority.ValidateHandle(binding, original); err != nil {
		t.Fatal(err)
	}
	restarted := authority
	restarted.Generation = strings.Repeat("e", 64)
	if err := restarted.ValidateBinding(binding); err != nil {
		t.Fatalf("restart changed durable binding: %v", err)
	}
	if err := restarted.ValidateHandle(binding, original); !errors.Is(err, agentbackend.ErrStaleExecution) {
		t.Fatalf("old generation survived restart: %v", err)
	}
	current := agentbackend.ExecutionHandle{Conversation: ref, Generation: restarted.Generation}
	if current.Conversation != original.Conversation {
		t.Fatal("restart changed receipt identity")
	}
	if err := restarted.ValidateHandle(binding, current); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*agentbackend.ExecutionHandle){
		func(h *agentbackend.ExecutionHandle) { h.Conversation.Incarnation = strings.Repeat("f", 64) },
		func(h *agentbackend.ExecutionHandle) { h.Conversation.NativeID = "different-native" },
		func(h *agentbackend.ExecutionHandle) { h.Conversation.Backend = agentbackend.Codex },
		func(h *agentbackend.ExecutionHandle) { h.Conversation.Session.Incarnation = strings.Repeat("f", 64) },
	} {
		stale := original
		change(&stale)
		if err := authority.ValidateHandle(binding, stale); !errors.Is(err, agentbackend.ErrStaleExecution) {
			t.Fatalf("stale identity accepted: %v", err)
		}
	}
	for _, change := range []func(*PiRuntimeAuthority){
		func(a *PiRuntimeAuthority) { a.Workspace = "/other" },
		func(a *PiRuntimeAuthority) { a.Slug = "other" },
		func(a *PiRuntimeAuthority) { a.SessionIncarnation = strings.Repeat("f", 64) },
		func(a *PiRuntimeAuthority) { a.BindingRevision++ },
	} {
		conflicting := authority
		change(&conflicting)
		if err := conflicting.ValidateBinding(binding); !errors.Is(err, agentbackend.ErrBindingConflict) {
			t.Fatalf("conflict accepted: %v", err)
		}
	}
	authority.State = "starting"
	if err := authority.ValidateHandle(binding, original); !errors.Is(err, agentbackend.ErrConversationNotReady) {
		t.Fatalf("starting authority dispatchable: %v", err)
	}
}
