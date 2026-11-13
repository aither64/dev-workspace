package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestBackendGuardRefusesPiBeforeLegacyNativeAndCreationEffects(t *testing.T) {
	s := newTestServer(t)
	s.config.VerifyThread = func(context.Context, string, string) error {
		t.Fatal("Pi evidence reached Codex verification")
		return nil
	}
	slug := "2026-11-12-pi"
	digest := sha256.Sum256([]byte(s.config.Workspace))
	path := filepath.Join(s.config.UserStateRoot, "session-backends", hex.EncodeToString(digest[:]), slug+".json")
	binding := session.BackendBinding{
		Schema: 1, Backend: agentbackend.Pi, BackendContract: 1,
		Workspace: s.config.Workspace, Slug: slug, SessionIncarnation: strings.Repeat("a", 64),
		CreationReceiptID: "original", Revision: 1, State: "ready",
		Conversations: []session.BackendConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64), State: "ready", NativeID: "native-pi"}},
	}
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []bool{false, true} {
		_, err := s.resolveConversation(context.Background(), conversation.ResolveRequest{ID: slug, Mutation: mutation, Operation: "message"})
		if !errors.Is(err, agentbackend.ErrBindingConflict) {
			t.Fatalf("Pi routed to legacy resolver: %v", err)
		}
	}
	var summary session.Summary
	summary.Slug = slug
	summary.Codex = session.Codex{ThreadID: "forged-codex", SocketPath: s.config.CodexSocket}
	s.normalizeInteractivity(context.Background(), &summary)
	if summary.Interactive || summary.RecoveryReady || !summary.ExecutionHeld || summary.Codex.ThreadID != "" {
		t.Fatalf("Pi granted legacy permission: %#v", summary)
	}
	if s.recoverRuntimeSession(context.Background(), summary, time.Time{}, make(chan struct{}, 1)) {
		t.Fatal("Pi scheduled Codex recovery")
	}
	receipt := creationReceipt{Schema: 3, Request: creationRequest{Slug: slug}}
	for _, check := range []func() error{
		func() error { _, err := s.installCreationLocked(receipt.Request, "", "", false); return err },
		func() error { return s.initializeCreation(&receipt) },
		func() error { return s.proveCreation(receipt) },
		func() error { return s.retireCreation(receipt) },
		func() error { return s.publishRecovery(context.Background(), slug, "root", "root", "epoch") },
	} {
		if err := check(); !errors.Is(err, agentbackend.ErrBindingConflict) {
			t.Fatalf("Pi reached legacy writer: %v", err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("Pi bytes changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.config.Workspace, "work", slug)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy creation changed tracking: %v", err)
	}
}

func TestBackendGuardRefusesSchema4BeforeBindingAndAfterReceiptRetirement(t *testing.T) {
	s := newTestServer(t)
	slug := "2026-11-12-pi"
	for _, path := range []string{s.creationPath(slug), filepath.Join(s.config.Workspace, "worktrees", ".locks", slug+".creation.json")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data := []byte(`{"schema":4,"state":"ready"}`)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.requireCodexSessionBackend(slug); !errors.Is(err, agentbackend.ErrUnsupportedBinding) {
			t.Fatalf("schema-4 integer became accepted state: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, data) {
			t.Fatalf("retained fence changed: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
