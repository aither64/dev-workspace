package piruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Reuse the owning receipt/input fixture. Only identity replies travel through
// the actual workerProcess typed decoder; no SDK or native process is launched.
type inputMetadataWriter func([]byte) (int, error)

func (write inputMetadataWriter) Write(data []byte) (int, error) { return write(data) }
func (write inputMetadataWriter) Close() error                   { return nil }

type inputMetadataWorker struct {
	*inputFaultWorker
	endpoint                   *workerProcess
	proofChange                func(*workerIdentityProof)
	rawProof                   []byte
	replyError                 string
	dropReply                  bool
	identityError, commitError error
	onCommit                   func(context.Context)
	onMetadata                 func(context.Context)
	counts                     map[string]int
}

func (w *inputMetadataWorker) Call(ctx context.Context, op string, input, result any) error {
	w.counts[op]++
	if op == "input_commit_v1" {
		if w.onCommit != nil {
			w.onCommit(ctx)
		}
		if w.commitError != nil {
			return w.commitError
		}
	}
	if op == "identity_v1" || op == "snapshot" {
		if w.onMetadata != nil {
			w.onMetadata(ctx)
		}
	}
	if op == "identity_v1" {
		if w.identityError != nil {
			return w.identityError
		}
		return w.endpoint.Call(ctx, op, input, result)
	}
	return w.inputFaultWorker.Call(ctx, op, input, result)
}
func inputMetadataFixture(t *testing.T) (*supervisor, *conversation, *inputMetadataWorker, agentbackend.ExecutionHandle, *liveInputReceipt) {
	t.Helper()
	s, c, _, handle := policySupervisorFixture(t)
	s.launch.AppStorePath = "/nix/store/00000000000000000000000000000000-retained"
	s.conversationIdentityContract = func(app string) (bool, error) {
		if app != s.launch.AppStorePath {
			t.Fatal("not frozen launch", app)
		}
		return true, nil
	}
	c.record.Instructions = "exact saved role"
	snapshot := workerSnapshot{NativeID: handle.Conversation.NativeID, HistoryFile: filepath.Join(c.root, "history", "original.jsonl"), Model: c.record.Settings.Model, Provider: c.record.Settings.Provider, Thinking: c.record.Settings.Thinking, InstructionsDigest: instructionsDigest(c.record.Instructions)}
	c.record.HistoryFile = snapshot.HistoryFile
	w := &inputMetadataWorker{inputFaultWorker: &inputFaultWorker{snapshotWorker: snapshotWorker{snapshot: snapshot}, release: make(chan struct{}), literal: "original input"}, counts: map[string]int{}}
	close(w.release)
	host := &workerHost{closed: make(chan struct{}), admission: &hostAdmission{}}
	w.endpoint = &workerProcess{host: host, handle: retainedWorkerIdentity(handle), instructionsDigest: snapshot.InstructionsDigest, pending: map[string]chan workerReply{}}
	host.input = inputMetadataWriter(func(data []byte) (int, error) {
		var request struct {
			Schema   int
			ID       string
			Identity workerIdentity
			Op       string
		}
		if err := json.Unmarshal(data, &request); err != nil || request.Schema != 1 || request.Op != "identity_v1" || !equalJSON(request.Identity, w.endpoint.handle) || !uuidPattern.MatchString(request.ID) {
			t.Fatal("identity frame changed", string(data), err)
		}
		if !w.dropReply {
			a := w.snapshot
			proof := workerIdentityProof{Version: 1, NativeID: a.NativeID, HistoryFile: a.HistoryFile, Persisted: a.Persisted, Busy: a.Busy, Held: a.Held, Model: a.Model, Provider: a.Provider, Thinking: a.Thinking, InstructionsDigest: a.InstructionsDigest, InputDelivery: 1}
			if w.proofChange != nil {
				w.proofChange(&proof)
			}
			body, _ := json.Marshal(proof)
			if w.rawProof != nil {
				body = w.rawProof
			}
			w.endpoint.mu.Lock()
			reply := w.endpoint.pending[request.ID]
			w.endpoint.mu.Unlock()
			reply <- workerReply{Schema: 1, ID: request.ID, Identity: request.Identity, Body: body, Error: w.replyError}
		}
		return len(data), nil
	})
	c.worker = w
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	input := SendInput{Message: w.literal, ActionContext: inputDeliveryPrefix + "steer"}
	ref := SubmissionRef{Conversation: handle.Conversation, OperationID: strings.Repeat("9", 32), Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, c.record.Settings)
	record := fromRetained(SubmissionStatus{Schema: 1, Ref: ref, Phase: "dispatching", Generation: s.ref.Generation, Input: input, Settings: c.record.Settings, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err := c.receipts.saveOwned(record); err != nil {
		t.Fatal(err)
	}
	live := &liveInputReceipt{ref: record.ref, worker: w}
	c.receipts.liveInputs = map[string]*liveInputReceipt{ref.OperationID: live}
	return s, c, w, handle, live
}

func TestPostInputMetadataCommitAndFrozenPairing(t *testing.T) {
	for _, mode := range []string{"scalar", "legacy", "contract_error", "scalar_error", "replaced", "removed", "commit_error"} {
		t.Run(mode, func(t *testing.T) {
			s, c, w, h, live := inputMetadataFixture(t)
			reads := 0
			frozen := s.launch.AppStorePath
			s.metadata.Protocol = 2 // Protocol/topology does not imply scalar support.
			s.conversationIdentityContract = func(app string) (bool, error) {
				reads++
				if app != frozen {
					t.Fatal("installed/current package used", app)
				}
				if mode == "contract_error" {
					return false, ErrScope
				}
				return mode != "legacy", nil
			}
			if mode == "scalar_error" {
				w.replyError = "unresolved"
			}
			if mode == "commit_error" {
				w.commitError = ErrHeld
			}
			receiptPath := filepath.Join(c.receipts.root, live.ref.id()+".json")
			var original []byte
			var commitCtx context.Context
			w.onCommit = func(ctx context.Context) {
				saved, err := c.receipts.loadOwned(live.ref.id())
				if err != nil || saved.Phase != "Accepted" || saved.Witness == nil || !saved.ref.equal(live.ref) {
					t.Fatal("commit preceded exact persisted witness", saved, err)
				}
				original, err = os.ReadFile(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				commitCtx = ctx
				if mode == "replaced" {
					c.worker = &snapshotWorker{}
				}
				if mode == "removed" {
					c.worker = nil
				}
			}
			w.onMetadata = func(ctx context.Context) {
				if w.counts["input_commit_v1"] != 1 || ctx != commitCtx {
					t.Fatal("observation preceded commit or changed its budget")
				}
			}
			s.finishInput(c, live)
			wantIdentity, wantSnapshot, wantReads := 1, 0, 1
			if mode == "legacy" {
				wantIdentity, wantSnapshot = 0, 1
			}
			if mode == "contract_error" {
				wantIdentity = 0
			}
			if mode == "replaced" || mode == "removed" || mode == "commit_error" {
				wantIdentity, wantReads = 0, 0
			}
			if w.counts["identity_v1"] != wantIdentity || w.counts["snapshot"] != wantSnapshot || reads != wantReads {
				t.Fatal("post-input path/fallback changed", w.counts, reads)
			}
			wantHeld := mode == "scalar_error" || mode == "contract_error" || mode == "commit_error"
			if w.held != wantHeld {
				t.Fatal("post-acceptance error/hold changed", mode, w.held)
			}
			after, _ := os.ReadFile(receiptPath)
			if len(original) == 0 || !bytes.Equal(original, after) {
				t.Fatal("post-input path rewrote original Accepted bytes")
			}
			retry, err := s.inputDelivery(context.Background(), h, backend.SendRequest{Message: w.literal, ClientUserMessageID: live.ref.id(), Delivery: "steer", Retry: true})
			if err != nil || retry.Phase != "Accepted" || retry.Ref != *live.ref.retained || w.enqueues != 0 {
				t.Fatal("post-input failure replayed or lost original", retry, err, w.enqueues)
			}
		})
	}
}

func TestPostInputMetadataSnapshotParity(t *testing.T) {
	for _, state := range []string{"idle", "unpersisted", "busy", "worker_held", "receipt_held", "callback_busy", "callback_unknown", "pending_settings", "pending_policy", "held_reason"} {
		t.Run(state, func(t *testing.T) {
			s, c, w, _, live := inputMetadataFixture(t)
			delete(c.receipts.liveInputs, live.ref.id())
			// Remove the unrelated in-flight seed before comparing state projection.
			if err := os.Remove(filepath.Join(c.receipts.root, live.ref.id()+".json")); err != nil {
				t.Fatal(err)
			}
			w.snapshot.Thinking = "high"
			w.snapshot.HistoryFile = filepath.Join(c.root, "history", "actual-refreshed.jsonl")
			w.snapshot.Persisted = true
			switch state {
			case "unpersisted":
				w.snapshot.Persisted = false
			case "busy":
				w.snapshot.Busy = true
			case "worker_held":
				w.snapshot.Held = true
			case "receipt_held":
				ref := *live.ref.retained
				record := fromRetained(SubmissionStatus{Schema: 1, Ref: ref, Phase: "Unknown", Generation: s.ref.Generation, Settings: c.record.Settings, Input: SendInput{Message: w.literal, ActionContext: inputDeliveryPrefix + "steer"}})
				if err := c.receipts.saveOwned(record); err != nil {
					t.Fatal(err)
				}
			case "callback_busy":
				c.callbackCount = 1
			case "callback_unknown":
				c.callbackUnknown = true
			case "pending_settings":
				copy := c.record.Settings
				c.record.PendingSettings = &copy
			case "pending_policy":
				copy := *c.record.ToolPolicy
				c.record.PendingToolPolicy = &copy
			case "held_reason":
				c.record.HeldReason = "retained unresolved work"
			}
			before := c.record
			c.mu.Lock()
			scalar, scalarErr := s.observeInputMetadata(context.Background(), c)
			scalarRecord := c.record
			c.record = before
			snapshot, snapshotErr := s.observeState(context.Background(), c)
			snapshotRecord := c.record
			c.mu.Unlock()
			if scalarErr != nil || snapshotErr != nil || scalar != snapshot || !reflect.DeepEqual(scalarRecord, snapshotRecord) {
				t.Fatal("scalar/full metadata semantics differ", scalar, snapshot, scalarErr, snapshotErr)
			}
			expected := "idle"
			if state == "busy" || state == "callback_busy" {
				expected = "busy"
			}
			if strings.Contains(state, "held") || strings.Contains(state, "pending") || state == "callback_unknown" {
				expected = "held"
			}
			if scalar.State != expected || scalar.Generation != s.ref.Generation || scalar.Settings.Thinking != "high" || scalar.Persisted != w.snapshot.Persisted || scalarRecord.HistoryFile != w.snapshot.HistoryFile || scalarRecord.InitialSettings != before.InitialSettings || scalarRecord.Settings.Revision != before.Settings.Revision || scalarRecord.Instructions != before.Instructions || scalarRecord.HistoryRef != before.HistoryRef || w.held {
				t.Fatal("metadata refresh or held precedence changed", scalar, scalarRecord)
			}
		})
	}
}

func TestPostInputMetadataFailuresHoldWithoutFallbackOrReplay(t *testing.T) {
	for _, fault := range []string{"native_id", "model", "provider", "history_path", "instructions", "malformed", "worker_error", "canceled_call", "mapping_save", "corrupt_receipt"} {
		t.Run(fault, func(t *testing.T) {
			s, c, w, _, live := inputMetadataFixture(t)
			w.proofChange = func(proof *workerIdentityProof) {
				switch fault {
				case "native_id":
					proof.NativeID = "foreign"
				case "model":
					proof.Model = "foreign"
				case "provider":
					proof.Provider = "foreign"
				case "history_path":
					proof.HistoryFile = filepath.Join(t.TempDir(), "foreign.jsonl")
				case "instructions":
					proof.InstructionsDigest = strings.Repeat("f", 64)
				}
			}
			if fault == "malformed" {
				w.rawProof = []byte(`{"version":1}`)
			}
			if fault == "worker_error" {
				w.replyError = "unresolved"
			}
			if fault == "canceled_call" {
				w.identityError = context.Canceled
			}
			var original []byte
			w.onCommit = func(context.Context) {
				path := filepath.Join(c.receipts.root, live.ref.id()+".json")
				var err error
				original, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "mapping_save" {
					path = filepath.Join(c.root, "conversation.json")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "corrupt_receipt" {
					if err := writePrivateBytes(filepath.Join(c.receipts.root, strings.Repeat("8", 32)+".json"), []byte("{invalid")); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.finishInput(c, live)
			after, err := os.ReadFile(filepath.Join(c.receipts.root, live.ref.id()+".json"))
			if err != nil || !bytes.Equal(original, after) || !w.held || w.counts["identity_v1"] != 1 || w.counts["snapshot"] != 0 || w.counts["input_hold_v1"] != 1 || w.enqueues != 0 {
				t.Fatal("scalar error fell back/lost receipt/replayed", fault, w.counts, err)
			}
		})
	}
	// Exercise real workerProcess cancellation, not only a supplied error reply.
	s, c, w, _, _ := inputMetadataFixture(t)
	w.dropReply = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.mu.Lock()
	_, err := s.observeInputMetadata(ctx, c)
	c.mu.Unlock()
	if !errors.Is(err, context.Canceled) || w.counts["identity_v1"] != 1 || w.counts["snapshot"] != 0 {
		t.Fatal("canceled scalar call fell back", err, w.counts)
	}
}
