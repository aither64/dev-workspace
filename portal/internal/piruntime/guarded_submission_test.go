package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type guardedReceiptWorker struct {
	base       *workflowWorker
	dispatches int
}

func (w *guardedReceiptWorker) Call(ctx context.Context, operation string, input, out any) error {
	if operation != "send" {
		return w.base.Call(ctx, operation, input, out)
	}
	w.dispatches++
	request := input.(map[string]string)
	w.base.base.snapshot.Persisted = true
	result := nativeResult{Outcome: "Accepted", Witness: &workerWitness{SessionID: w.base.base.snapshot.NativeID, EntryID: "original-entry", EffectiveMessage: request["message"]}}
	data, _ := json.Marshal(result)
	return json.Unmarshal(data, out)
}
func (w *guardedReceiptWorker) Wait(ctx context.Context) error { return w.base.Wait(ctx) }
func (w *guardedReceiptWorker) PID() (int, string)             { return w.base.PID() }
func guardedFixture(t *testing.T) (*supervisor, *conversation, *guardedReceiptWorker, agentbackend.ExecutionHandle, *hostAttachment) {
	t.Helper()
	s, c, base, handle, a := workflowSupervisorFixture(t)
	base.base.snapshot.HistoryFile = filepath.Join(c.root, "history", "original.jsonl")
	w := &guardedReceiptWorker{base: base}
	c.worker = w
	return s, c, w, handle, a
}
func guardedRef(handle agentbackend.ExecutionHandle, input SendInput, settings EffectiveSettings) SubmissionRef {
	ref := SubmissionRef{Conversation: handle.Conversation, OperationID: strings.Repeat("f", 32), Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, settings)
	return ref
}
func guardedInvoke(t *testing.T, s *supervisor, a *hostAttachment, ctx context.Context, request checkedSubmissionRequest) (SubmissionStatus, error) {
	t.Helper()
	type reply struct {
		status SubmissionStatus
		err    error
	}
	done := make(chan reply, 1)
	data, _ := json.Marshal(request)
	go func() {
		value, err := s.invoke(ctx, "send_checked", data)
		status, _ := value.(SubmissionStatus)
		done <- reply{status, err}
	}()
	for {
		frame := workflowFrame(t, a)
		if frame.RequiresACK {
			acknowledgeWorkflow(t, s, a, frame)
			break
		}
	}
	result := <-done
	return result.status, result.err
}
func assertNoGuardedReceipt(t *testing.T, c *conversation, w *guardedReceiptWorker) {
	t.Helper()
	entries, err := os.ReadDir(c.receipts.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 || w.dispatches != 0 {
		t.Fatal("refusal wrote intent/dispatched", len(entries), w.dispatches)
	}
}
func TestCheckedSubmissionSettingsAreAtomicAndChangedPreviewCannotDispatch(t *testing.T) {
	s, c, w, handle, a := guardedFixture(t)
	ctx := context.Background()
	input := SendInput{Message: "exact original\ninput", ActionContext: "domain:original"}
	preview, err := s.observeWorkflow(ctx, handle)
	if err != nil || preview.Settings != c.record.Settings {
		t.Fatal(preview, err)
	}
	ref := guardedRef(handle, input, preview.Settings)
	domainRef, _ := json.Marshal(ref)
	assertNoGuardedReceipt(t, c, w)
	// Represent a completed Configure between host preview and checked handoff.
	c.mu.Lock()
	c.record.Settings.Revision++
	c.mu.Unlock()
	s.workflowChanged()
	if _, err := guardedInvoke(t, s, a, ctx, checkedSubmissionRequest{Handle: handle, Ref: ref, Input: input}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale settings accepted", err)
	}
	assertNoGuardedReceipt(t, c, w)
	after, _ := json.Marshal(ref)
	if string(after) != string(domainRef) {
		t.Fatal("sender replaced frozen ref")
	}
	current, err := s.observeWorkflow(ctx, handle)
	if err != nil || current.Settings.Revision != preview.Settings.Revision+1 {
		t.Fatal(current, err)
	}
}
func TestCheckedSubmissionMismatchAndPendingRefuseBeforeReceipt(t *testing.T) {
	for _, fault := range []string{"scope", "action", "operation", "digest-shape", "digest", "message", "context", "retry", "generation", "pending-settings", "pending-policy", "held", "cold", "policy-proof"} {
		t.Run(fault, func(t *testing.T) {
			s, c, w, handle, a := guardedFixture(t)
			input := SendInput{Message: "frozen", ActionContext: "domain"}
			ref := guardedRef(handle, input, c.record.Settings)
			switch fault {
			case "scope":
				ref.Conversation.Member = "implementer9"
			case "action":
				ref.Action = "fork"
			case "operation":
				ref.OperationID = "new-model-operation"
			case "digest-shape":
				ref.Digest = "wrong"
			case "digest":
				ref.Digest = strings.Repeat("e", 64)
			case "message":
				input.Message += "changed"
			case "context":
				input.ActionContext += "changed"
			case "retry":
				input.Retry = true
			case "generation":
				handle.Generation = strings.Repeat("d", 64)
			case "pending-settings":
				pending := c.record.Settings
				pending.Revision++
				c.record.PendingSettings = &pending
			case "pending-policy":
				pending := cloneToolPolicy(*c.record.ToolPolicy)
				pending.Revision++
				c.record.PendingToolPolicy = &pending
			case "held":
				c.record.HeldReason = "original uncertainty"
			case "cold":
				c.worker = nil
			case "policy-proof":
				w.base.base.holds++
			}
			if _, err := guardedInvoke(t, s, a, context.Background(), checkedSubmissionRequest{Handle: handle, Ref: ref, Input: input}); err == nil {
				t.Fatal("mismatch/unavailable state admitted")
			}
			assertNoGuardedReceipt(t, c, w)
		})
	}
}
func TestCheckedSubmissionKeepsOriginalTerminalAndUnknownLookupWithoutResend(t *testing.T) {
	for _, phase := range []string{"prepared", "dispatching", "Unknown", "NotSubmitted", "Accepted"} {
		t.Run(phase, func(t *testing.T) {
			s, c, w, handle, a := guardedFixture(t)
			input := SendInput{Message: "one original"}
			ref := guardedRef(handle, input, c.record.Settings)
			original := SubmissionStatus{Schema: 1, Ref: ref, Phase: phase, Generation: handle.Generation, Settings: c.record.Settings, Input: input, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if phase == "Accepted" {
				original.Witness = &NativeWitness{SessionID: handle.Conversation.NativeID, EntryID: "original-user", EffectiveDigest: MessageDigest(input.Message)}
			}
			if phase == "NotSubmitted" {
				original.Refusal = "positive no dispatch"
			}
			if err := c.receipts.save(original); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(c.receipts.root, ref.OperationID+".json")
			before, _ := os.ReadFile(path)
			status, err := guardedInvoke(t, s, a, context.Background(), checkedSubmissionRequest{Handle: handle, Ref: ref, Input: input})
			if err != nil {
				t.Fatal(status, err)
			}
			if status.Ref != ref || status.Input != input || w.dispatches != 0 {
				t.Fatal("original receipt was replaced/resubmitted", status)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("replay rewrote original receipt")
			}
			looked, err := c.receipts.lookup(ref)
			if err != nil || looked.Ref != ref || w.dispatches != 0 {
				t.Fatal(looked, err)
			}
		})
	}
}
func TestCheckedSubmissionPreACKCancellationHasZeroReceiptEffect(t *testing.T) {
	s, c, w, handle, a := guardedFixture(t)
	input := SendInput{Message: "original"}
	ref := guardedRef(handle, input, c.record.Settings)
	ctx, cancel := context.WithCancel(context.Background())
	data, _ := json.Marshal(checkedSubmissionRequest{Handle: handle, Ref: ref, Input: input})
	done := make(chan error, 1)
	go func() { _, err := s.invoke(ctx, "send_checked", data); done <- err }()
	frame := workflowFrame(t, a)
	if !frame.RequiresACK {
		t.Fatal(frame)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled checked send admitted")
	}
	assertNoGuardedReceipt(t, c, w)
	frame.Event = "workflow_invalidated_ack"
	frame.RequiresACK = false
	if s.acceptWorkflowACK(a, frame) {
		t.Fatal("late ACK revived checked send")
	}
}

func TestCheckedSubmissionRealControlCancellationJoinsWithoutReceipt(t *testing.T) {
	s, c, w, handle, a := guardedFixture(t)
	m, joined := workflowControlFixture(t, s)
	input := SendInput{Message: "one frozen input"}
	ref := guardedRef(handle, input, c.record.Settings)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.callRaw(ctx, s.ref, "send_checked", checkedSubmissionRequest{Handle: handle, Ref: ref, Input: input}, nil)
	}()
	frame := workflowFrame(t, a)
	if !frame.RequiresACK {
		t.Fatal(frame)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("caller cancellation accepted")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("ordinary monitor did not join")
	}
	assertNoGuardedReceipt(t, c, w)
}

func TestActualCheckedSubmissionCurrentSettingsLostACKAndOriginalReceipt(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "lost-ack"}[lost], func(t *testing.T) {
			m, host, start, ctx := supervisorFixture(t)
			runtime, err := m.Ensure(ctx, start)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan struct{}, 1)
			handler := &workflowFixtureHandler{callbackFixtureHandler: callbackFixtureHandler{tools: []string{"report_to_lead"}}, invalidated: func(WorkflowInvalidation) {
				select {
				case events <- struct{}{}:
				default:
				}
			}}
			attachFixture(t, m, host.s, ctx, runtime.Runtime, handler)
			awaitWorkflowAttachment(t, host.s, ctx)
			policy := policyTarget("default", 1, []string{"report_to_lead"}, []string{"report_to_lead"})
			creation := strings.Repeat("c", 64)
			state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: creation, Settings: settingsFixture(), ToolPolicy: &policy})
			if err != nil {
				t.Fatal(err)
			}
			handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
			initial := awaitIdleWorkflow(t, m, ctx, handle, events)
			input := SendInput{Message: "one exact domain handoff", ActionContext: "question:original-domain-id"}
			stale := guardedRef(handle, input, initial.Settings)
			staleJSON, _ := json.Marshal(stale)
			settings := initial.Settings
			settings.Revision++
			changed, err := m.Configure(ctx, handle, settings)
			if err != nil || changed.Outcome != "applied" {
				t.Fatal(changed, err)
			}
			if _, err := m.SendCheckedSubmission(ctx, handle, stale, input); !errors.Is(err, ErrConflict) {
				t.Fatal("saved stale preview dispatched", err)
			}
			c, err := host.s.member(handle.Conversation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(c.receipts.root, stale.OperationID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("stale preparation wrote native receipt", err)
			}
			unchanged, _ := json.Marshal(stale)
			if string(unchanged) != string(staleJSON) {
				t.Fatal("stale ref was replaced")
			}
			preview := awaitIdleWorkflow(t, m, ctx, handle, events)
			if preview.Settings != settings || preview.Settings == initial.Settings {
				t.Fatal("initial settings substituted for current", preview.Settings)
			}
			ref := SubmissionRef{Conversation: handle.Conversation, OperationID: strings.Repeat("a", 32), Action: "send"}
			ref.Digest = SubmissionDigest(ref, input, preview.Settings)
			// This immutable string stands for the domain's existing opaque ReceiptRef.
			domainReceiptRef, _ := json.Marshal(ref)
			preparations := 1
			var admission sync.RWMutex
			admission.Lock()
			locked := true
			defer func() {
				if locked {
					admission.Unlock()
				}
			}()
			callbacks := make(chan struct{}, 2)
			handler.check = func(callCtx context.Context, call HostCall) error {
				callbacks <- struct{}{}
				admission.RLock()
				defer admission.RUnlock()
				if call.InputOperationID != ref.OperationID || call.Subject.Conversation == nil || call.Subject.Conversation.Conversation != ref.Conversation {
					return ErrScope
				}
				status, err := m.Lookup(callCtx, ref)
				if err != nil || status.Phase != "Accepted" || status.Witness == nil {
					return ErrHeld
				}
				return nil
			}
			originalDial := m.config.Dial
			if lost {
				var dialMu sync.Mutex
				var current net.Conn
				m.config.Dial = func(callCtx context.Context, path string) (net.Conn, error) {
					connection, err := originalDial(callCtx, path)
					if err == nil {
						dialMu.Lock()
						current = connection
						dialMu.Unlock()
					}
					return connection, err
				}
				c.receipts.checkpoint = func(point string) error {
					if point == "receipt" {
						dialMu.Lock()
						connection := current
						dialMu.Unlock()
						if connection == nil {
							return ErrUnavailable
						}
						_ = connection.Close()
					}
					return nil
				}
			}
			status, sendErr := m.SendCheckedSubmission(ctx, handle, ref, input)
			m.config.Dial = originalDial
			c.receipts.checkpoint = nil
			if lost {
				if sendErr == nil {
					t.Fatal("owned socket ACK loss reported success")
				}
			} else if sendErr != nil || status.Phase != "Accepted" || status.Ref != ref || status.Input != input || status.Witness == nil {
				t.Fatal(status, sendErr)
			}
			original, err := m.Lookup(ctx, ref)
			if err != nil || original.Phase != "Accepted" || original.Witness == nil || original.Settings != preview.Settings {
				t.Fatal(original, err)
			}
			select {
			case <-callbacks:
			case <-ctx.Done():
				t.Fatal("checked Send waited for SH callback", ctx.Err())
			}
			proof, err := m.VerifyCreation(ctx, handle, creation, state.HistoryRef, &ref)
			if err != nil || proof.Bootstrap == nil {
				t.Fatal("original witness blocked by callback", proof, err)
			}
			admission.Unlock()
			locked = false
			awaitIdleWorkflow(t, m, ctx, handle, events)
			path := filepath.Join(c.receipts.root, ref.OperationID+".json")
			before, _ := os.ReadFile(path)
			settings.Revision++
			configured, err := m.Configure(ctx, handle, settings)
			if err != nil || configured.Outcome != "applied" {
				t.Fatal(configured, err)
			}
			// Recovery consumes only the saved full ref, despite current-settings change.
			var saved SubmissionRef
			if err := json.Unmarshal(domainReceiptRef, &saved); err != nil {
				t.Fatal(err)
			}
			recovered, err := m.Lookup(ctx, saved)
			if err != nil || recovered.Ref != ref || recovered.Phase != "Accepted" || recovered.Settings != preview.Settings || preparations != 1 {
				t.Fatal(recovered, err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("lookup after settings change rewrote original receipt")
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
				if entry.Kind == "userMessage" {
					users++
				}
			}
			if users != 1 {
				t.Fatal("recovery prepared or sent another native input", users)
			}
			if stop, err := m.Unload(ctx, handle); err != nil || !stop.VerifiedExit {
				t.Fatal(stop, err)
			}
		})
	}
}

// Test-only bounded diagnostics for the existing external native fixture. This
// is not a production constructor or a second runtime/protocol harness.
func CheckedSubmissionFixtureDiagnostic(m *Manager, handle agentbackend.ExecutionHandle) string {
	host, ok := m.config.Host.(*fakeHost)
	if !ok || host.s == nil {
		return "fixture host unavailable"
	}
	s := host.s
	s.workflow.mu.Lock()
	a := s.workflow.attachment
	ready := false
	queued, acks := 0, 0
	if a != nil {
		ready = a.workflowReady
		queued = len(a.workflowEvents)
		acks = len(a.workflowACKs)
	}
	seq, pending, overflow := s.workflow.sequence, s.workflow.pending, s.workflow.overflow
	s.workflow.mu.Unlock()
	c, err := s.member(handle.Conversation)
	if err != nil {
		return fmt.Sprintf("seq=%d pending=%d overflow=%v ready=%v queue=%d acks=%d member=%v", seq, pending, overflow, ready, queued, acks, err)
	}
	c.activeMu.Lock()
	callbacks, unknown := c.callbackCount, c.callbackUnknown
	c.activeMu.Unlock()
	state := "conversation lock busy"
	if c.mu.TryLock() {
		state = fmt.Sprintf("held=%q loaded=%v persisted=%v pendingSettings=%v pendingPolicy=%v", c.record.HeldReason, c.record.Loaded, c.record.Persisted, c.record.PendingSettings != nil, c.record.PendingToolPolicy != nil)
		c.mu.Unlock()
	}
	return fmt.Sprintf("seq=%d pending=%d overflow=%v ready=%v queue=%d acks=%d callbacks=%d callbackUnknown=%v %s", seq, pending, overflow, ready, queued, acks, callbacks, unknown, state)
}
