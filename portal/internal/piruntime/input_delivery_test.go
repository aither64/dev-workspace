package piruntime

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	conversationhttp "github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestNativeInputHTTPIdleAfterLegacyPrompt(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	transcriptContractFixture(t, m, start, `{"piTranscriptSnapshot":1,"piInputDelivery":1}`)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	attachFixture(t, m, h.s, ctx, runtime.Runtime, &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}})
	awaitWorkflowAttachment(t, h.s, ctx)
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	if result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "initial legacy input", ClientUserMessageID: strings.Repeat("1", 32)}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	waitIdle(t, m, ctx, handle)
	ops, err := m.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	httpHandler, err := conversationhttp.NewBackendHandler(conversationhttp.BackendOptions{AllowedOrigins: []string{"https://portal.test"}, Logger: log.New(io.Discard, "", 0), Resolver: conversationhttp.BackendResolverFunc(func(context.Context, conversationhttp.ResolveRequest) (conversationhttp.BackendTarget, error) {
		return conversationhttp.BackendTarget{Identity: BackendIdentity(handle), Operations: ops, Capabilities: conversationhttp.Capabilities{Read: true, Send: true, InputDelivery: true}, MutationLock: conversationhttp.NewMutationLock()}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	id := "33333333-3333-4333-8333-333333333333"
	post := func(retry bool) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"message": "idle literal ${unchanged}", "clientUserMessageId": id, "delivery": "steer", "retry": retry})
		request := httptest.NewRequest("POST", "/codex/conversations/trusted/message", strings.NewReader(string(data)))
		request.Header.Set("Origin", "https://portal.test")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		httpHandler.ServeHTTP(response, request)
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return response.Code, body
	}
	if code, body := post(false); code != 409 || body["code"] != "message_pending" {
		t.Fatal("idle enqueue", code, body)
	}
	for {
		code, body := post(true)
		if code == http.StatusAccepted {
			if body["clientUserMessageId"] != id || body["turnId"] == "" {
				t.Fatal(body)
			}
			break
		}
		if code != 409 || body["code"] != "message_pending" {
			t.Fatal("idle completion", code, body)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	waitIdle(t, m, ctx, handle)
	transcript, err := ops.Read.Read(ctx, BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	users, matched := 0, 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "userMessage" {
			users++
			if entry.ClientUserMessageID == id && entry.Text == "idle literal ${unchanged}" {
				matched++
			}
		}
	}
	if users != 2 || matched != 1 {
		t.Fatal("idle input missing or duplicated", users, matched)
	}
}

func TestNativeInputHTTPPendingWitnessAndOriginalCallbackLocks(t *testing.T) {
	m, h, start, ctx := supervisorFixture(t)
	transcriptContractFixture(t, m, start, `{"piTranscriptSnapshot":1,"piInputDelivery":1}`)
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	handler := &workflowFixtureHandler{invalidated: func(WorkflowInvalidation) {}, callbackFixtureHandler: callbackFixtureHandler{tools: []string{"report_to_lead"}, check: func(ctx context.Context, call HostCall) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}}
	attachFixture(t, m, h.s, ctx, runtime.Runtime, handler)
	awaitWorkflowAttachment(t, h.s, ctx)
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	if result, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "original", ClientUserMessageID: strings.Repeat("1", 32)}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ops, err := m.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if supported, err := m.InputDelivery(ctx, handle); err != nil || !supported {
		t.Fatal(supported, err)
	}
	httpHandler, err := conversationhttp.NewBackendHandler(conversationhttp.BackendOptions{AllowedOrigins: []string{"https://portal.test"}, Logger: log.New(io.Discard, "", 0), Resolver: conversationhttp.BackendResolverFunc(func(context.Context, conversationhttp.ResolveRequest) (conversationhttp.BackendTarget, error) {
		return conversationhttp.BackendTarget{Identity: BackendIdentity(handle), Operations: ops, Capabilities: conversationhttp.Capabilities{Read: true, Send: true, InputDelivery: true}, MutationLock: conversationhttp.NewMutationLock()}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	post := func(id, mode string, retry bool) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"message": "same literal\nAttached local files: /fixture/input.png", "clientUserMessageId": id, "delivery": mode, "retry": retry})
		request := httptest.NewRequest("POST", "/codex/conversations/trusted/message", strings.NewReader(string(data)))
		request.Header.Set("Origin", "https://portal.test")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		httpHandler.ServeHTTP(response, request)
		var result map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return response.Code, result
	}
	follow, steer := "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	for _, input := range []struct{ id, mode string }{{follow, "followUp"}, {steer, "steer"}} {
		code, body := post(input.id, input.mode, false)
		if code != 409 || body["code"] != "message_pending" || body["pending"] != true || body["clientUserMessageId"] != input.id || body["delivery"] != input.mode || body["turnId"] != nil || body["notSent"] != nil {
			t.Fatal(code, body)
		}
		code, body = post(input.id, input.mode, true)
		if code != 409 || body["code"] != "message_pending" {
			t.Fatal("enquiry lost live ownership", code, body)
		}
	}
	// No completion lock may prevent this original callback from finishing. The
	// exact running activeRef is unchanged by either queued input.
	c, _ := h.s.member(handle.Conversation)
	c.activeMu.Lock()
	active := c.activeRef.id()
	c.activeMu.Unlock()
	if active != strings.Repeat("1", 32) {
		t.Fatal("queued input replaced original callback", active)
	}
	code, body := post(steer, "followUp", true)
	if code != 409 || body["code"] != "message_delivery_unknown" {
		t.Fatal("changed mode did not conflict", code, body)
	}
	missing := "44444444-4444-4444-8444-444444444444"
	code, body = post(missing, "steer", true)
	if code != 409 || body["code"] != "message_delivery_unknown" {
		t.Fatal("missing enquiry dispatched", code, body)
	}
	if _, err := os.Stat(filepath.Join(c.receipts.root, missing+".json")); !os.IsNotExist(err) {
		t.Fatal("enquiry created intent", err)
	}
	close(release)
	for _, input := range []struct{ id, mode string }{{steer, "steer"}, {follow, "followUp"}} {
		for {
			code, body := post(input.id, input.mode, true)
			if code == http.StatusAccepted {
				if body["clientUserMessageId"] != input.id || body["turnId"] == "" {
					t.Fatal(body)
				}
				break
			}
			if code != 409 || body["code"] != "message_pending" {
				data, _ := os.ReadFile(filepath.Join(c.root, "agent/fixture-error.log"))
				t.Fatalf("completion %d %v native=%s", code, body, data)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitIdle(t, m, ctx, handle)
	handler.mu.Lock()
	calls := append([]HostCall{}, handler.calls...)
	handler.mu.Unlock()
	if len(calls) != 3 || calls[2].InputOperationID != follow {
		t.Fatal("consumed input callback lost exact receipt provenance", calls)
	}
	// Reopened receipts retain original false retry and frozen action context.
	records := map[string]SubmissionStatus{}
	for _, id := range []string{follow, steer} {
		record, err := c.receipts.load(id)
		if err != nil || record.Phase != "Accepted" || record.Input.Retry {
			t.Fatal(record, err)
		}
		records[id] = record
	}
	if required, err := retainedInputDelivery(c.receipts.root); err != nil || !required {
		t.Fatal("receipt capability requirement lost", required, err)
	}
	if records[follow].Witness.EntryID == records[steer].Witness.EntryID {
		t.Fatal("duplicate literal collapsed witnesses")
	}
	before, err := ops.Read.Read(ctx, BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, entry := range before.Entries {
		found[entry.ClientUserMessageID] = true
	}
	if !found[follow] || !found[steer] || !before.InputDelivery {
		t.Fatal("receipt projection lost", found)
	}
}

func TestInputDeliveryContractSelectsOnlyExactVersion(t *testing.T) {
	for _, fixture := range []struct {
		data  string
		want  bool
		valid bool
	}{{`{}`, false, true}, {`{"piInputDelivery":1}`, true, true}, {`{"piInputDelivery":2}`, false, false}, {`{"piInputDelivery":null}`, false, false}, {`{"piInputDelivery":1,"piInputDelivery":1}`, false, false}} {
		got, err := decodeInputDeliveryContract([]byte(fixture.data))
		if got != fixture.want || (err == nil) != fixture.valid {
			t.Fatal(fixture, got, err)
		}
	}
}

type inputFaultWorker struct {
	snapshotWorker
	release   chan struct{}
	waitError error
	ackError  error
	enqueues  int
	held      bool
	literal   string
}

func (w *inputFaultWorker) Call(ctx context.Context, operation string, input, result any) error {
	switch operation {
	case "input_enqueue_v1":
		w.mu.Lock()
		w.enqueues++
		w.literal = input.(map[string]string)["message"]
		ackError := w.ackError
		w.mu.Unlock()
		if ackError != nil {
			return ackError
		}
		data, _ := json.Marshal(nativeResult{Outcome: "Pending"})
		return json.Unmarshal(data, result)
	case "input_wait_v1":
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.release:
		}
		if w.waitError != nil {
			return w.waitError
		}
		w.mu.Lock()
		literal := w.literal
		nativeID := w.snapshot.NativeID
		w.mu.Unlock()
		data, _ := json.Marshal(nativeResult{Outcome: "Accepted", Witness: &workerWitness{SessionID: nativeID, EntryID: "one-exact-user", EffectiveMessage: literal}})
		return json.Unmarshal(data, result)
	case "input_commit_v1":
		return json.Unmarshal([]byte(`{"committed":true}`), result)
	case "input_hold_v1":
		w.mu.Lock()
		w.held = true
		w.mu.Unlock()
		return nil
	default:
		return w.snapshotWorker.Call(ctx, operation, input, result)
	}
}

func TestNativeInputReceiptFaultsNeverReplayAndRestartIsUnknown(t *testing.T) {
	for _, fault := range []string{"lost-ack", "intent", "dispatch", "native", "witness", "receipt", "shutdown", "accepted-response"} {
		t.Run(fault, func(t *testing.T) {
			m, h, start, ctx := supervisorFixture(t)
			runtime, err := m.Ensure(ctx, start)
			if err != nil {
				t.Fatal(err)
			}
			w := &inputFaultWorker{release: make(chan struct{})}
			h.s.start = func(_ context.Context, _ workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, _ func()) (worker, error) {
				w.snapshot = workerSnapshot{NativeID: handle.Conversation.NativeID, HistoryFile: filepath.Join(config.HistoryDir, "fixture.jsonl"), Model: config.Settings.Model, Provider: config.Settings.Provider, Thinking: config.Settings.Thinking}
				return w, nil
			}
			state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: "22222222-2222-4222-8222-222222222222", Settings: settingsFixture()})
			if err != nil {
				t.Fatal(err)
			}
			handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
			c, _ := h.s.member(handle.Conversation)
			if fault == "lost-ack" {
				w.ackError = ErrHeld
			}
			if fault == "shutdown" {
				w.waitError = ErrHeld
			}
			if fault != "accepted-response" && fault != "lost-ack" && fault != "shutdown" {
				c.receipts.checkpoint = func(point string) error {
					if point == fault {
						return ErrHeld
					}
					return nil
				}
			}
			request := backend.SendRequest{Message: "exact literal", ClientUserMessageID: strings.Repeat("f", 32), Delivery: "steer"}
			initial, initialErr := h.s.inputDelivery(ctx, handle, request)
			if initial.Phase == "Pending" {
				saved, err := c.receipts.load(request.ClientUserMessageID)
				if err != nil {
					t.Fatal(err)
				}
				reopened := &receiptEngine{root: c.receipts.root}
				if got := reopened.projectInput(fromRetained(saved)); got.Phase != "Unknown" {
					t.Fatal("restart invented pending", got)
				}
				close(w.release)
				h.s.inputTasks.Wait()
			} else if initialErr == nil {
				t.Fatal("fault had no disposition", initial)
			}
			request.Retry = true
			final, err := h.s.inputDelivery(ctx, handle, request)
			if err != nil {
				t.Fatal("same-ID enquiry refused immutable record", err)
			}
			want := "Unknown"
			if fault == "receipt" || fault == "accepted-response" {
				want = "Accepted"
			}
			if final.Phase != want || final.Input.Retry || final.Input.ActionContext != inputDeliveryPrefix+"steer" {
				t.Fatal(final, want)
			}
			w.mu.Lock()
			enqueues := w.enqueues
			w.mu.Unlock()
			expected := 1
			if fault == "intent" || fault == "dispatch" {
				expected = 0
			}
			if enqueues != expected {
				t.Fatal("enquiry replayed", enqueues, expected)
			}
		})
	}
}

// Frozen 283 conversationRequest/SendRequest shapes. Test the actual new
// envelope, not a substitute codec: empty Send accompanies ordinary read/load
// calls too, and retained supervisors reject every unknown nested field.
func TestInputDeliveryPreservesStrict283ConversationWire(t *testing.T) {
	type legacySend struct {
		Message, ClientUserMessageID, ActionContext string
		Retry                                       bool
	}
	type legacyRequest struct {
		Handle                 agentbackend.ExecutionHandle
		Send                   legacySend
		Settings               EffectiveSettings
		CreationID, HistoryRef string
		Bootstrap              *SubmissionRef
		Origin                 *NativeOriginRef
		Before                 *int
	}
	for _, send := range []legacySend{{}, {Message: "exact legacy", ClientUserMessageID: "44444444-4444-4444-8444-444444444444", ActionContext: "workflow", Retry: true}} {
		old := legacyRequest{Send: send, CreationID: "retained creation", HistoryRef: "retained history"}
		current := conversationRequest{Send: backend.SendRequest{Message: send.Message, ClientUserMessageID: send.ClientUserMessageID, ActionContext: send.ActionContext, Retry: send.Retry}, CreationID: old.CreationID, HistoryRef: old.HistoryRef}
		oldBytes, _ := json.Marshal(old)
		newBytes, _ := json.Marshal(current)
		if string(oldBytes) != string(newBytes) {
			t.Fatalf("legacy envelope changed: %s != %s", newBytes, oldBytes)
		}
		var oldReader legacyRequest
		if err := strictDecode(newBytes, &oldReader, recordLimit); err != nil {
			t.Fatal("retained strict reader refused new legacy request", err)
		}
		var newReader conversationRequest
		if err := strictDecode(oldBytes, &newReader, recordLimit); err != nil || newReader.Send != current.Send {
			t.Fatal("new reader refused old caller", err)
		}
	}
	explicit, _ := json.Marshal(conversationRequest{Send: backend.SendRequest{Delivery: "steer"}})
	var oldReader legacyRequest
	if err := strictDecode(explicit, &oldReader, recordLimit); err == nil {
		t.Fatal("oracle no longer enforces the retained strict boundary")
	}
	var current conversationRequest
	if err := strictDecode(explicit, &current, recordLimit); err != nil || current.Send.Delivery != "steer" {
		t.Fatal("explicit versioned request lost mode", err)
	}
}
