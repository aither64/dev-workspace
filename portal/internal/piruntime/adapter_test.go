package piruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	conversationhttp "github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type snapshotWorker struct {
	mu             sync.Mutex
	snapshot       workerSnapshot
	sends          int
	configures     int
	configureError bool
	sendResult     *nativeResult
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
		if w.sendResult != nil {
			value = *w.sendResult
		}
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

func adapterHTTPFixture(t *testing.T) (*Manager, *fakeHost, *snapshotWorker, agentbackend.ExecutionHandle, backend.Operations, context.Context, http.Handler) {
	t.Helper()
	m, host, start, ctx := supervisorFixture(t)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	w := &snapshotWorker{}
	host.s.start = func(_ context.Context, _ workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, _ func()) (worker, error) {
		w.snapshot = workerSnapshot{NativeID: handle.Conversation.NativeID, Model: config.Settings.Model, Provider: config.Settings.Provider, Thinking: config.Settings.Thinking, HistoryFile: filepath.Join(config.HistoryDir, "fixture.jsonl")}
		return w, nil
	}
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	ops, err := m.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := conversationhttp.NewBackendHandler(conversationhttp.BackendOptions{AllowedOrigins: []string{"https://portal.test"}, Logger: log.New(io.Discard, "", 0), Resolver: conversationhttp.BackendResolverFunc(func(context.Context, conversationhttp.ResolveRequest) (conversationhttp.BackendTarget, error) {
		return conversationhttp.BackendTarget{Identity: BackendIdentity(handle), Operations: ops, Capabilities: conversationhttp.Capabilities{Read: true}}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return m, host, w, handle, ops, ctx, handler
}

func adapterHTTPRead(t *testing.T, handler http.Handler, route string, status int, code string) backend.TranscriptPage {
	t.Helper()
	request := httptest.NewRequest("GET", "/codex/conversations/trusted/"+route, nil)
	request.Header.Set("Origin", "https://portal.test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("status=%d want=%d: %s", response.Code, status, response.Body.String())
	}
	if code != "" {
		var body map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["code"] != code {
			t.Fatal(response.Body.String(), err)
		}
		return backend.TranscriptPage{}
	}
	var page backend.TranscriptPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func nativeRow(id, role, text, reason, diagnostic string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"id": id, "type": "message", "timestamp": "2026-11-12T10:00:00Z", "message": map[string]any{"role": role, "content": []any{map[string]string{"type": "text", "text": text}}, "stopReason": reason, "errorMessage": diagnostic}})
	return data
}

func TestAdapterHTTPPagingAndCursorDispositions(t *testing.T) {
	_, _, w, handle, _, _, handler := adapterHTTPFixture(t)
	for i := 0; i < 55; i++ {
		w.snapshot.Entries = append(w.snapshot.Entries, nativeRow(fmtInt(i), "user", "<b>input</b>", "stop", ""))
	}
	page := adapterHTTPRead(t, handler, "thread/page", 200, "")
	if page.ThreadID != handle.Conversation.NativeID || len(page.Entries) != 50 || !page.HasOlder || page.OlderCursor == nil || len(*page.OlderCursor) <= 32 {
		t.Fatal(page)
	}
	older := adapterHTTPRead(t, handler, "thread/page?cursor="+string(*page.OlderCursor), 200, "")
	repeat := adapterHTTPRead(t, handler, "thread/page?cursor="+string(*page.OlderCursor), 200, "")
	if !reflect.DeepEqual(older, repeat) || len(older.Entries) != 5 || older.HasOlder || older.OlderCursor != nil {
		t.Fatal(older, repeat)
	}
	for i, entry := range append(older.Entries, page.Entries...) {
		if entry.ItemID != fmtInt(i) || entry.Kind != "userMessage" || entry.HTML != "&lt;b&gt;input&lt;/b&gt;" {
			t.Fatal(i, entry)
		}
	}
	for _, query := range []string{"cursor=", "cursor=A&cursor=B", "other=A", "cursor=%GG", "cursor=A%2FB", "cursor=" + strings.Repeat("A", 5463), "cursor=" + strings.Repeat("%41", 5462) + "&", "cursor=e30"} {
		adapterHTTPRead(t, handler, "thread/page?"+query, 400, "transcript_cursor_invalid")
	}
	data, _ := base64.RawURLEncoding.DecodeString(string(*page.OlderCursor))
	var position historyCursor
	if err := json.Unmarshal(data, &position); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{string(data) + "{}", strings.Replace(string(data), `"Version":1`, `"Version":1,"Version":1`, 1), strings.Replace(string(data), `"Version":1`, `"Version":null`, 1), strings.Replace(string(data), `"End":5`, `"End":"5"`, 1)} {
		adapterHTTPRead(t, handler, "thread/page?cursor="+base64.RawURLEncoding.EncodeToString([]byte(malformed)), 400, "transcript_cursor_invalid")
	}
	encode := func(value historyCursor) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, field := range []string{"scope", "generation", "snapshot", "end"} {
		changed := position
		switch field {
		case "scope":
			changed.Scope = strings.Repeat("f", 64)
		case "generation":
			changed.Generation = strings.Repeat("f", 64)
		case "snapshot":
			changed.Snapshot = strings.Repeat("f", 64)
		case "end":
			changed.End = 56
		}
		adapterHTTPRead(t, handler, "thread/page?cursor="+encode(changed), 409, "transcript_reset_required")
	}
	for _, field := range []string{"scope", "generation", "snapshot", "version", "end"} {
		changed := position
		switch field {
		case "scope":
			changed.Scope = "invalid"
		case "generation":
			changed.Generation = "invalid"
		case "snapshot":
			changed.Snapshot = "invalid"
		case "version":
			changed.Version = 2
		case "end":
			changed.End = 0
		}
		adapterHTTPRead(t, handler, "thread/page?cursor="+encode(changed), 400, "transcript_cursor_invalid")
	}
	// Whitespace is part of the adapter's decoded JSON limit, not C's token format.
	padded := append(data, []byte(strings.Repeat(" ", 4096-len(data)))...)
	adapterHTTPRead(t, handler, "thread/page?cursor="+base64.RawURLEncoding.EncodeToString(padded), 200, "")
	adapterHTTPRead(t, handler, "thread/page?cursor="+base64.RawURLEncoding.EncodeToString(append(padded, ' ')), 400, "transcript_cursor_invalid")
	w.mu.Lock()
	w.snapshot.Entries = append(w.snapshot.Entries, nativeRow("new", "user", "new", "stop", ""))
	w.mu.Unlock()
	adapterHTTPRead(t, handler, "thread/page?cursor="+string(*page.OlderCursor), 409, "transcript_reset_required")
}

func TestAdapterHTTPMessagePresentationAndDiagnostics(t *testing.T) {
	_, _, w, _, _, _, handler := adapterHTTPFixture(t)
	for _, state := range []struct {
		busy, held bool
		want       string
	}{{false, false, "idle"}, {true, false, "active"}, {true, true, "held"}} {
		w.mu.Lock()
		w.snapshot.Busy = state.busy
		w.snapshot.Held = state.held
		w.mu.Unlock()
		for _, route := range []string{"thread", "thread/page"} {
			if got := adapterHTTPRead(t, handler, route, 200, "").Status; got != state.want {
				t.Fatal(got, state.want)
			}
		}
	}
	w.snapshot.Held = false
	w.snapshot.Entries = []json.RawMessage{nativeRow("user-1", "user", "<b>input</b>", "", ""), nativeRow("assistant-1", "assistant", "answer", "stop", ""), nativeRow("tool-1", "toolResult", "tool output", "", "")}
	w.snapshot.LiveMessage = json.RawMessage(`{"role":"assistant","content":"stream","stopReason":"pending"}`)
	page := adapterHTTPRead(t, handler, "thread/page", 200, "")
	if !reflect.DeepEqual([]string{page.Entries[0].Kind, page.Entries[1].Kind, page.Entries[2].Kind, page.Entries[3].Kind}, []string{"userMessage", "agentMessage", "toolResult", "agentMessage"}) || page.Entries[3].ItemID != "pi-stream" || page.Entries[3].TurnStatus != "inProgress" {
		t.Fatal(page)
	}
	for _, reason := range []string{"error", "aborted"} {
		for _, message := range []string{"", "<script>\"猫\" & diagnostic</script>", strings.Repeat("猫<&", 2000)} {
			w.snapshot.Entries = []json.RawMessage{nativeRow("user-1", "user", "input", "", ""), nativeRow("failed-1", "assistant", " \n\t", reason, message)}
			live, _ := json.Marshal(map[string]any{"role": "assistant", "content": "", "stopReason": reason, "errorMessage": message})
			w.snapshot.LiveMessage = live
			for _, route := range []string{"thread", "thread/page"} {
				page := adapterHTTPRead(t, handler, route, 200, "")
				if len(page.Entries) != 3 {
					t.Fatal(page)
				}
				for i, entry := range page.Entries[1:] {
					if entry.Kind != "error" || entry.Text == "" || !strings.HasPrefix(entry.Text, reason) || len(entry.Text) > 4096 || !utf8.ValidString(entry.Text) || entry.HTML != html.EscapeString(entry.Text) || entry.TurnID != "user-1" || entry.ClientUserMessageID != "" {
						t.Fatal(entry)
					}
					if i == 0 && (entry.ItemID != "failed-1" || entry.Timestamp != "2026-11-12T10:00:00Z") {
						t.Fatal(entry)
					}
					if i == 1 && (entry.ItemID != "pi-stream" || entry.TurnStatus != "inProgress") {
						t.Fatal(entry)
					}
					if len(message) > 4096 && !strings.HasSuffix(entry.Text, "…") {
						t.Fatal("truncation indicator absent")
					}
				}
			}
			w.snapshot.LiveMessage = nil
			if page := adapterHTTPRead(t, handler, "thread", 200, ""); len(page.Entries) != 2 || page.Entries[1].Kind != "error" {
				t.Fatal(page)
			}
		}
	}
	for _, reason := range []string{"pending", "stop", "length", "toolUse", "deferred", "cancelled", "unknown"} {
		w.snapshot.Entries = []json.RawMessage{nativeRow("ordinary", "assistant", "", reason, "not a failure proof")}
		if page := adapterHTTPRead(t, handler, "thread", 200, ""); len(page.Entries) != 1 || page.Entries[0].Kind != "agentMessage" {
			t.Fatal(page)
		}
	}
	w.snapshot.Entries = []json.RawMessage{nativeRow("ordinary", "assistant", "nonempty", "error", "ignored"), json.RawMessage(`{"id":"bad","type":"message","message":{"role":"assistant","content":"","stopReason":true}}`)}
	if page := adapterHTTPRead(t, handler, "thread", 200, ""); len(page.Entries) != 1 || page.Entries[0].Kind != "agentMessage" || page.Entries[0].Text != "nonempty" {
		t.Fatal(page)
	}
	w.snapshot.Entries = []json.RawMessage{json.RawMessage(`{"id":"bad-diagnostic","type":"message","message":{"role":"assistant","content":"","stopReason":"error","errorMessage":42}}`)}
	w.snapshot.LiveMessage = json.RawMessage(`{"role":"assistant","content":"","stopReason":"aborted","errorMessage":42}`)
	if page := adapterHTTPRead(t, handler, "thread", 200, ""); len(page.Entries) != 0 {
		t.Fatal("malformed typed diagnostic projected", page)
	}
}

func TestAdapterDiagnosticReadPreservesOriginalAdmission(t *testing.T) {
	for _, outcome := range []string{"Accepted", "Unknown"} {
		t.Run(outcome, func(t *testing.T) {
			_, host, w, handle, ops, ctx, handler := adapterHTTPFixture(t)
			request := backend.SendRequest{Message: "one original input", ClientUserMessageID: "0123456789abcdef0123456789abcdef"}
			w.sendResult = &nativeResult{Outcome: outcome, Witness: &workerWitness{SessionID: handle.Conversation.NativeID, EntryID: "original-user", EffectiveMessage: request.Message}}
			result, err := ops.Send.Send(ctx, BackendIdentity(handle), request)
			if err != nil || result.Outcome != map[string]backend.SendOutcome{"Accepted": backend.Accepted, "Unknown": backend.Unknown}[outcome] || w.sends != 1 {
				t.Fatal(result, err, w.sends)
			}
			member, err := host.s.member(handle.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(member.receipts.root, request.ClientUserMessageID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var original SubmissionStatus
			if err := json.Unmarshal(before, &original); err != nil {
				t.Fatal(err)
			}
			if original.Phase != outcome || original.Ref.OperationID != request.ClientUserMessageID {
				t.Fatal(original)
			}
			w.mu.Lock()
			w.snapshot.Entries = []json.RawMessage{nativeRow("original-user", "user", request.Message, "", ""), nativeRow("terminal", "assistant", "", "error", "display only")}
			w.mu.Unlock()
			page := adapterHTTPRead(t, handler, "thread/page", 200, "")
			if len(page.Entries) != 2 || page.Entries[1].Kind != "error" {
				t.Fatal(page)
			}
			looked, err := member.receipts.lookup(original.Ref)
			if err != nil || !reflect.DeepEqual(looked, original) {
				t.Fatal(looked, err)
			}
			replay, err := ops.Send.Send(ctx, BackendIdentity(handle), request)
			if err != nil || !reflect.DeepEqual(replay, result) || w.sends != 1 {
				t.Fatal("diagnostic changed admission or resent", replay, result, err, w.sends)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("read/replay changed original receipt", err)
			}
		})
	}
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
