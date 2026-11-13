package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type snapshotWorker struct {
	mu             sync.Mutex
	snapshot       workerSnapshot
	sends          int
	configures     int
	configureError bool
}

func (w *snapshotWorker) Call(_ context.Context, operation string, input, result any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var value any
	switch operation {
	case "snapshot":
		page := w.snapshot
		end := len(page.Entries)
		encoded, _ := json.Marshal(input)
		var position struct {
			Before *int `json:"before"`
		}
		_ = json.Unmarshal(encoded, &position)
		if position.Before != nil {
			end = *position.Before
		}
		if end < 0 || end > len(page.Entries) {
			return ErrScope
		}
		page.HistoryTotal = len(page.Entries)
		page.HistoryDigest = digestValue(page.Entries)
		page.HistoryStart = max(0, end-50)
		page.Entries = page.Entries[page.HistoryStart:end]
		value = page
	case "send":
		w.sends++
		value = nativeResult{Outcome: "NotSubmitted", Refusal: "busy before native call"}
	case "configure":
		w.configures++
		settings := input.(EffectiveSettings)
		w.snapshot.Model = settings.Model
		w.snapshot.Provider = settings.Provider
		w.snapshot.Thinking = settings.Thinking
		if w.configureError {
			return ErrHeld
		}
		value = SettingsResult{Requested: settings, Effective: settings, Outcome: "applied"}
	case "hold", "abort":
		w.snapshot.Held = true
		value = struct{ Idle, Held bool }{true, true}
	default:
		return ErrUnavailable
	}
	data, _ := json.Marshal(value)
	if result == nil {
		return nil
	}
	return json.Unmarshal(data, result)
}
func (w *snapshotWorker) Wait(context.Context) error { return nil }
func (w *snapshotWorker) PID() (int, string)         { return 0, "" }
func TestAdapterOpaqueCursorInvalidationAndUnavailableSlots(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	status, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	w := &snapshotWorker{}
	h.s.start = func(_ context.Context, _ workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, _ func()) (worker, error) {
		w.snapshot = workerSnapshot{NativeID: handle.Conversation.NativeID, Model: config.Settings.Model, Provider: config.Settings.Provider, Thinking: config.Settings.Thinking, HistoryFile: filepath.Join(config.HistoryDir, "fixture.jsonl")}
		for i := 0; i < 55; i++ {
			raw, _ := json.Marshal(map[string]any{"id": fmtInt(i), "type": "message", "message": map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": "<b>untrusted</b>"}}}})
			w.snapshot.Entries = append(w.snapshot.Entries, raw)
		}
		return w, nil
	}
	state, err := m.Create(ctx, status.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	identity := BackendIdentity(handle)
	ops, err := m.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	page, err := ops.Page.ReadPage(ctx, identity, "")
	if err != nil || len(page.Entries) != 50 || !page.HasOlder || page.OlderCursor == nil {
		t.Fatal(page, err)
	}
	older, err := ops.Page.ReadPage(ctx, identity, *page.OlderCursor)
	if err != nil || len(older.Entries) != 5 || older.HasOlder {
		t.Fatal(older, err)
	}
	if older.Entries[0].HTML != "&lt;b&gt;untrusted&lt;/b&gt;" {
		t.Fatal("raw markup returned")
	}
	if _, err := ops.Page.ReadPage(ctx, identity, backend.HistoryCursor("../native/path")); err == nil {
		t.Fatal("caller path accepted")
	}
	updates, cancel, err := ops.Watch.Subscribe(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	w.mu.Lock()
	w.snapshot.Entries = append(w.snapshot.Entries, json.RawMessage(`{"id":"new","type":"message","message":{"role":"user","content":"new"}}`))
	w.mu.Unlock()
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("no snapshot invalidation")
	}
	if _, err := ops.Page.ReadPage(ctx, identity, *page.OlderCursor); err == nil {
		t.Fatal("stale cursor accepted after append")
	}
	request := backend.SendRequest{Message: "input", ClientUserMessageID: "0123456789abcdef0123456789abcdef"}
	for i := 0; i < 2; i++ {
		result, err := ops.Send.Send(ctx, identity, request)
		if err != nil || result.Outcome != backend.NotSubmitted || result.Retryable {
			t.Fatal(result, err)
		}
	}
	if w.sends != 1 {
		t.Fatal("terminal refusal resent")
	}
	request.ClientUserMessageID = "../invalid"
	if _, err := ops.Send.Send(ctx, identity, request); err == nil {
		t.Fatal("invalid operation ID accepted")
	}
	wrong := identity
	wrong.ScopeKey = "another scope"
	if _, err := ops.Read.Read(ctx, wrong); !errors.Is(err, ErrScope) {
		t.Fatal(err)
	}
	if ops.Settings != nil || ops.Respond != nil || ops.QueueWrite != nil || ops.Acknowledge != nil {
		t.Fatal("unsupported slot exposed")
	}
}
