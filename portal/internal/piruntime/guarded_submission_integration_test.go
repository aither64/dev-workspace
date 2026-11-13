package piruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type checkedQuestionPublication struct {
	*p6Publication
	mu         sync.Mutex
	initial    uint64
	attachment string
	ready      chan struct{}
	once       sync.Once
	events     chan struct{}
	failures   []string
}

func (h *checkedQuestionPublication) HandleHostCall(ctx context.Context, call piruntime.HostCall) (piruntime.HostReply, error) {
	reply, err := h.p6Publication.HandleHostCall(ctx, call)
	if err != nil {
		h.mu.Lock()
		h.failures = append(h.failures, fmt.Sprintf("tool=%s input=%s err=%v", call.Tool, call.InputOperationID, err))
		h.mu.Unlock()
	}
	return reply, err
}
func (h *checkedQuestionPublication) InvalidateWorkflow(event piruntime.WorkflowInvalidation) {
	h.mu.Lock()
	if event.Connected {
		if h.attachment == "" {
			h.attachment = event.AttachmentID
			h.initial = event.Sequence
		} else if h.attachment == event.AttachmentID && event.Sequence > h.initial {
			h.once.Do(func() { close(h.ready) })
		}
	}
	h.mu.Unlock()
	select {
	case h.events <- struct{}{}:
	default:
	}
}
func checkedQuestionIdle(ctx context.Context, m *piruntime.Manager, handle agentbackend.ExecutionHandle, events <-chan struct{}) (piruntime.WorkflowObservation, error) {
	for {
		observation, err := m.ObserveWorkflow(ctx, handle)
		if err == nil && observation.Idle {
			return observation, nil
		}
		select {
		case <-ctx.Done():
			return observation, ctx.Err()
		case <-events:
		}
	}
}
func TestCheckedSubmissionDurableQuestionHandoffUsesCurrentSettingsAndOriginalLookup(t *testing.T) {
	f := piruntime.NewP6NativeFixture(t)
	ctx := f.Context
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	validate := func(_ context.Context, ref agentbackend.ConversationRef, caller *piquestions.Caller) error {
		if ref.Session != f.Runtime.Session || piquestions.ValidateRef(ref) != nil || caller != nil && caller.Handle.Conversation != ref {
			return piquestions.ErrConflict
		}
		return nil
	}
	questions := &piquestions.FileStore{Root: root, Validate: validate}
	plans := &piplan.FileStore{Root: root, Validate: validate}
	initial := piruntime.ToolPolicyTarget{Mode: "plan", Revision: 1, Active: []string{"publish_plan", "read", "request_user_input_async"}, RegisteredCeiling: []string{"publish_plan", "read", "request_user_input_async", "write"}}
	initial.Digest = piruntime.ToolPolicyDigest(initial)
	publication := &p6Publication{fixture: f, questions: questions, plans: plans, initial: initial}
	handler := &checkedQuestionPublication{p6Publication: publication, ready: make(chan struct{}), events: make(chan struct{}, 1)}
	f.Attach(handler)
	select {
	case <-handler.ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	state, err := f.Manager.Create(ctx, f.Runtime, piruntime.CreateRequest{Session: f.Runtime.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: f.Settings, ToolPolicy: &initial})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(piruntime.CheckedSubmissionFixtureDiagnostic(f.Manager, handle))
			handler.mu.Lock()
			failures := append([]string{}, handler.failures...)
			handler.mu.Unlock()
			t.Log("bounded handler failures", failures)
		}
	})
	if result, err := f.Manager.Deliver(ctx, handle, backend.SendRequest{Message: "publish optional question and exact plan", ClientUserMessageID: strings.Repeat("4", 32)}); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	f.Idle(handle)
	publication.mu.Lock()
	question := publication.question
	publication.mu.Unlock()
	if !piquestions.ValidID(question.ID) {
		t.Fatal(question)
	}
	var admission sync.Mutex
	admit := func(ctx context.Context, ref agentbackend.ConversationRef, fn func(context.Context) error) error {
		if ref != handle.Conversation {
			return piquestions.ErrConflict
		}
		admission.Lock()
		defer admission.Unlock()
		return fn(ctx)
	}
	current := func(_ context.Context, caller piquestions.Caller) (piquestions.Caller, error) {
		if caller.Handle.Conversation != handle.Conversation {
			return caller, piquestions.ErrConflict
		}
		caller.Handle = handle
		return caller, nil
	}
	origin := piquestions.NativeOriginVerifier{Inspector: f.Manager}
	admitted := piquestions.AdmittedStore{Store: questions, Admit: admit, Current: current, VerifyOrigin: origin.Verify}
	response := piquestions.ResponseInput{ResponseID: piquestions.UUID(), QuestionID: question.ID, QuestionRevision: question.Revision, Answers: []piquestions.Answer{{QuestionID: "0", ChoiceID: "1", Text: "/literal answer é\n"}}}
	if _, err := admitted.AcceptResponse(ctx, handle.Conversation, response); err != nil {
		t.Fatal(err)
	}
	settings := f.Settings
	settings.Revision++
	if configured, err := f.Manager.Configure(ctx, handle, settings); err != nil || configured.Outcome != "applied" {
		t.Fatal(configured, err)
	}
	if _, err := checkedQuestionIdle(ctx, f.Manager, handle, handler.events); err != nil {
		t.Fatal(err)
	}
	prepares, sends, lookups := 0, 0, 0
	var frozen piruntime.SubmissionRef
	var opaque string
	var originalInput piruntime.SendInput
	service := piquestions.Service{Store: questions, Admit: admit, Dispatch: piquestions.Dispatch{
		Current: current, VerifyOrigin: origin.Verify,
		Ready: func(ctx context.Context, caller piquestions.Caller) (bool, error) {
			seen, err := f.Manager.ObserveWorkflow(ctx, caller.Handle)
			return err == nil && seen.Idle && !seen.Pending && !seen.Held, err
		},
		Prepare: func(ctx context.Context, caller piquestions.Caller, delivery piquestions.Delivery) (string, error) {
			prepares++
			seen, err := f.Manager.ObserveWorkflow(ctx, caller.Handle)
			if err != nil || !seen.Idle || seen.Settings != settings {
				return "", piruntime.ErrUnavailable
			}
			originalInput = piruntime.SendInput{Message: delivery.Message, ActionContext: delivery.ActionContext}
			frozen = nativeRef(caller, delivery.OperationID, delivery.Message, delivery.ActionContext, seen.Settings)
			data, err := json.Marshal(frozen)
			opaque = string(data)
			return opaque, err
		},
		Send: func(ctx context.Context, caller piquestions.Caller, delivery piquestions.Delivery) (string, error) {
			sends++
			// The real store's handoff must already be durable before native dispatch.
			saved, err := questions.Response(ctx, caller.Handle.Conversation, response.ResponseID)
			if err != nil || saved.DeliveryPhase != "handoff" || delivery.ReceiptRef != opaque {
				return "Unknown", piquestions.ErrConflict
			}
			var ref piruntime.SubmissionRef
			if json.Unmarshal([]byte(delivery.ReceiptRef), &ref) != nil || ref != frozen {
				return "Unknown", piquestions.ErrConflict
			}
			status, err := f.Manager.SendCheckedSubmission(ctx, caller.Handle, ref, originalInput)
			if err != nil {
				return "Unknown", err
			}
			if status.Phase != "Accepted" || status.Ref != frozen || status.Input != originalInput || status.Settings != settings || status.Witness == nil {
				return "Unknown", piquestions.ErrConflict
			}
			return "Unknown", errors.New("fixture lost domain ACK after checked receipt")
		},
		Lookup: func(ctx context.Context, caller piquestions.Caller, delivery piquestions.Delivery) (string, error) {
			lookups++
			var ref piruntime.SubmissionRef
			if json.Unmarshal([]byte(delivery.ReceiptRef), &ref) != nil || ref != frozen || delivery.ReceiptRef != opaque || delivery.OperationID != frozen.OperationID || delivery.Message != originalInput.Message || delivery.ActionContext != originalInput.ActionContext {
				return "Unknown", piquestions.ErrConflict
			}
			status, err := f.Manager.Lookup(ctx, ref)
			if err != nil {
				return "Unknown", err
			}
			if status.Ref != frozen || status.Input != originalInput || status.Settings.Revision != 2 || status.Witness == nil {
				return "Unknown", piquestions.ErrConflict
			}
			return status.Phase, nil
		},
	}}
	if err := service.DeliverNext(ctx, handle.Conversation); err == nil {
		t.Fatal("lost domain ACK reported delivered")
	}
	f.Idle(handle)
	// A new current setting does not permit re-preparation of this handoff.
	settings.Revision++
	if configured, err := f.Manager.Configure(ctx, handle, settings); err != nil || configured.Outcome != "applied" {
		t.Fatal(configured, err)
	}
	service.Dispatch.VerifyOrigin = nil
	service.Dispatch.Prepare = func(context.Context, piquestions.Caller, piquestions.Delivery) (string, error) {
		t.Fatal("reprepared saved handoff")
		return "", nil
	}
	service.Dispatch.Send = func(context.Context, piquestions.Caller, piquestions.Delivery) (string, error) {
		t.Fatal("resent saved handoff")
		return "", nil
	}
	if err := service.DeliverNext(ctx, handle.Conversation); err != nil {
		t.Fatal(err)
	}
	stored, err := questions.Response(ctx, handle.Conversation, response.ResponseID)
	if err != nil || stored.DeliveryPhase != "Accepted" || prepares != 1 || sends != 1 || lookups != 1 {
		t.Fatal(stored, err, prepares, sends, lookups)
	}
	ops, err := f.Manager.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := ops.Read.Read(ctx, piruntime.BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "user" {
			users++
		}
	}
	if users != 2 {
		t.Fatal("expected bootstrap plus exactly one original answer", users)
	}
	if stopped, err := f.Manager.Unload(ctx, handle); err != nil || !stopped.VerifiedExit {
		t.Fatal(stopped, err)
	}
}
