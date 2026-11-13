package piruntime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestNativeProviderSwitchKeepsIdentityHistoryAndOtherMember(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	m.config.AccountBindings = []AccountBinding{{Provider: "p3-alternate", AccountRef: "alternate-account"}, {Provider: "p3-fake", AccountRef: "fake-account"}}
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	var handles []agentbackend.ExecutionHandle
	for i, member := range []string{"lead", "implementer1"} {
		state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: member, Incarnation: strings.Repeat(string(rune('b'+i)), 64), CreationID: []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"}[i], Settings: settingsFixture()})
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation})
	}
	handle := handles[0]
	if result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "one original input", ClientUserMessageID: "44444444-4444-4444-8444-444444444444"}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	waitIdle(t, m, ctx, handle)
	c, _ := h.s.member(handle.Conversation)
	file := c.record.HistoryFile
	target := settingsFixture()
	target.Provider = "p3-alternate"
	target.AccountRef = "alternate-account"
	target.Revision = 2
	for _, bad := range []EffectiveSettings{func() EffectiveSettings { x := target; x.AccountRef = "fake-account"; return x }(), func() EffectiveSettings { x := target; x.Provider = "p3-fake"; return x }(), func() EffectiveSettings { x := target; x.Revision = 4; return x }()} {
		result, err := m.Configure(ctx, handle, bad)
		if result.Outcome != "rejected" {
			t.Fatal(result, err)
		}
	}
	bindings := h.s.config.AccountBindings
	for _, missing := range [][]AccountBinding{{bindings[0]}, {bindings[1]}} {
		h.s.config.AccountBindings = missing
		result, err := m.Configure(ctx, handle, target)
		if !errors.Is(err, ErrUnavailable) || result.Outcome != "rejected" {
			t.Fatal(result, err)
		}
	}
	h.s.config.AccountBindings = bindings
	result, err := m.Configure(ctx, handle, target)
	if err != nil || result.Outcome != "applied" || result.Effective != target {
		t.Fatal(result, err)
	}
	if duplicate, err := m.Configure(ctx, handle, target); err != nil || duplicate != result {
		t.Fatal(duplicate, err)
	}
	if c.record.HistoryFile != file || c.record.Conversation != handle.Conversation || c.record.Settings != target || c.record.PendingSettings != nil {
		t.Fatal("private mapping changed identity or lost settings")
	}
	other, err := m.Load(ctx, handles[1])
	if err != nil || other.Settings != settingsFixture() {
		t.Fatal("other member settings changed", other, err)
	}
	ops, err := m.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := ops.Read.Read(ctx, BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatal("switch submitted another input", users)
	}
	stop, err := m.Unload(ctx, handle)
	if err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
	reloaded, err := m.Load(ctx, handle)
	if err != nil || reloaded.Settings != target || reloaded.Conversation != handle.Conversation {
		t.Fatal(reloaded, err)
	}
	for _, bound := range handles {
		if _, err := m.Unload(ctx, bound); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigureLostNativeReplyRetainsPendingAndNoFurtherEffect(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	m.config.AccountBindings = []AccountBinding{{Provider: "p3-alternate", AccountRef: "alternate-account"}, {Provider: "p3-fake", AccountRef: "fake-account"}}
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	w := &snapshotWorker{configureError: true}
	h.s.start = func(_ context.Context, _ workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, _ func()) (worker, error) {
		w.snapshot = workerSnapshot{NativeID: handle.Conversation.NativeID, HistoryFile: filepath.Join(config.HistoryDir, "empty.jsonl"), Provider: config.Settings.Provider, Model: config.Settings.Model, Thinking: config.Settings.Thinking}
		return w, nil
	}
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	target := state.Settings
	target.Provider = "p3-alternate"
	target.AccountRef = "alternate-account"
	target.Revision++
	result, err := m.Configure(ctx, handle, target)
	if err == nil || result.Outcome != "unknown" {
		t.Fatal(result, err)
	}
	c, _ := h.s.member(handle.Conversation)
	var disk conversationRecord
	if err := readPrivate(filepath.Join(c.root, "conversation.json"), &disk); err != nil {
		t.Fatal(err)
	}
	if disk.PendingSettings == nil || *disk.PendingSettings != target || disk.Settings != state.Settings || !w.snapshot.Held {
		t.Fatal("partial mutation lost its hold", disk)
	}
	if _, err := m.Configure(ctx, handle, target); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if _, err := m.Load(ctx, handle); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	if result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "must not run", ClientUserMessageID: "33333333-3333-4333-8333-333333333333"}); err == nil || result.Outcome != backend.Unknown {
		t.Fatal(result, err)
	}
	if w.configures != 1 || w.sends != 0 {
		t.Fatal("pending change repeated or sent input", w.configures, w.sends)
	}
}
