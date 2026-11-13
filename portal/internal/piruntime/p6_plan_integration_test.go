package piruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

const nativePlanText = "Plan é\r\n  exact bytes\n"

type p6Publication struct {
	fixture   piruntime.P6NativeFixture
	questions *piquestions.FileStore
	plans     *piplan.FileStore
	initial   piruntime.ToolPolicyTarget
	mu        sync.Mutex
	question  piquestions.QuestionView
	proposal  piplan.ProposalView
}

func (*p6Publication) HostTools() []string {
	return []string{"publish_plan", "request_user_input_async"}
}
func (h *p6Publication) HandleHostCall(ctx context.Context, call piruntime.HostCall) (piruntime.HostReply, error) {
	if call.Subject.Kind != "conversation" || call.Subject.Conversation == nil || call.Subject.Utility != nil {
		return piruntime.HostReply{}, piruntime.ErrScope
	}
	caller := piquestions.Caller{Handle: *call.Subject.Conversation,
		Lineage: piquestions.LineageRef{AnchorEntryID: call.AnchorEntryID, SelectionToken: call.SelectionToken}, NativeToolCallID: call.NativeToolCallID}
	ref := nativeRef(caller, strings.Repeat("4", 32), "publish optional question and exact plan", "", h.fixture.Settings)
	status, err := h.fixture.Manager.Lookup(ctx, ref)
	if err != nil || call.InputOperationID != ref.OperationID || status.Phase != "Accepted" || status.Witness == nil {
		return piruntime.HostReply{}, piruntime.ErrHeld
	}
	var value any
	switch call.Tool {
	case "request_user_input_async":
		var args struct {
			Questions []struct {
				Title   string   `json:"title"`
				Options []string `json:"options,omitempty"`
			} `json:"questions"`
		}
		if err := piquestions.DecodeOriginArguments(call.Arguments, &args); err != nil {
			return piruntime.HostReply{}, err
		}
		in := piquestions.PublishInput{Questions: []piquestions.Question{}}
		for _, item := range args.Questions {
			q := piquestions.Question{Title: item.Title}
			for _, label := range item.Options {
				q.Options = append(q.Options, piquestions.Option{Label: label})
			}
			in.Questions = append(in.Questions, q)
		}
		q, err := h.questions.Publish(ctx, caller, in)
		if err != nil {
			return piruntime.HostReply{}, err
		}
		h.mu.Lock()
		h.question = q
		h.mu.Unlock()
		value = map[string]any{"id": q.ID, "revision": q.Revision, "status": "pending"}
	case "publish_plan":
		// This real callback is active while Inspect proves the installed policy.
		proof, err := h.fixture.Manager.InspectToolPolicy(ctx, caller.Handle)
		if err != nil || piruntime.ValidateToolPolicyProof(proof, h.initial, caller.Handle.Generation) != nil {
			return piruntime.HostReply{}, piruntime.ErrHeld
		}
		var args struct {
			Text string `json:"text"`
		}
		if err := piquestions.DecodeOriginArguments(call.Arguments, &args); err != nil {
			return piruntime.HostReply{}, err
		}
		p, err := h.plans.PublishProposal(ctx, caller, args.Text, piplan.NativeOrigin{EntryID: call.AnchorEntryID, Complete: true})
		if err != nil {
			return piruntime.HostReply{}, err
		}
		h.mu.Lock()
		h.proposal = p
		h.mu.Unlock()
		value = map[string]any{"planId": p.PlanID, "revision": p.Revision, "textSha256": p.TextSHA256}
	default:
		return piruntime.HostReply{}, piruntime.ErrUnavailable
	}
	body, err := json.Marshal(value)
	return piruntime.HostReply{Outcome: "result", Body: body}, err
}

func nativeRef(c piquestions.Caller, id, message, actionContext string, settings piruntime.EffectiveSettings) piruntime.SubmissionRef {
	ref := piruntime.SubmissionRef{Conversation: c.Handle.Conversation, OperationID: id, Action: "send"}
	ref.Digest = piruntime.SubmissionDigest(ref, piruntime.SendInput{Message: message, ActionContext: actionContext}, settings)
	return ref
}

type lostModeAck struct {
	manager *piruntime.Manager
	calls   int
}

func (c *lostModeAck) InspectToolPolicy(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
	return c.manager.InspectToolPolicy(ctx, h)
}
func (c *lostModeAck) ApplyToolPolicy(ctx context.Context, h agentbackend.ExecutionHandle, expected uint64, target piruntime.ToolPolicyTarget) (piruntime.ToolPolicyResult, error) {
	c.calls++
	result, err := c.manager.ApplyToolPolicy(ctx, h, expected, target)
	if err == nil && result.Outcome == "applied" {
		return result, errors.New("fixture lost mode ACK after completion")
	}
	return result, err
}

func TestP6NativeQuestionAndPlanUseOriginalReceiptsAfterLostAcks(t *testing.T) {
	f := piruntime.NewP6NativeFixture(t)
	ctx := f.Context
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	validate := func(_ context.Context, ref agentbackend.ConversationRef, c *piquestions.Caller) error {
		if ref.Session != f.Runtime.Session || piquestions.ValidateRef(ref) != nil || c != nil && c.Handle.Conversation != ref {
			return piquestions.ErrConflict
		}
		return nil
	}
	questions := &piquestions.FileStore{Root: root, Validate: validate}
	plans := &piplan.FileStore{Root: root, Validate: validate}
	initial := piruntime.ToolPolicyTarget{Mode: "plan", Revision: 1,
		Active:            []string{"publish_plan", "read", "request_user_input_async"},
		RegisteredCeiling: []string{"publish_plan", "read", "request_user_input_async", "write"}}
	initial.Digest = piruntime.ToolPolicyDigest(initial)
	handler := &p6Publication{fixture: f, questions: questions, plans: plans, initial: initial}
	f.Attach(handler)
	state, err := f.Manager.Create(ctx, f.Runtime, piruntime.CreateRequest{Session: f.Runtime.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: f.Settings, ToolPolicy: &initial})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	first, err := f.Manager.Deliver(ctx, handle, backend.SendRequest{Message: "publish optional question and exact plan", ClientUserMessageID: strings.Repeat("4", 32)})
	if err != nil || first.Outcome != backend.Accepted {
		t.Fatal(first, err)
	}
	f.Idle(handle)
	handler.mu.Lock()
	q, p := handler.question, handler.proposal
	handler.mu.Unlock()
	if !piquestions.ValidID(q.ID) || !piquestions.ValidID(p.PlanID) || p.Text != nativePlanText || p.TextSHA256 != piplan.Digest(nativePlanText) {
		t.Fatal(q, p)
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
	current := func(_ context.Context, c piquestions.Caller) (piquestions.Caller, error) {
		if c.Handle.Conversation != handle.Conversation {
			return c, piquestions.ErrConflict
		}
		c.Handle = handle
		return c, nil
	}
	eligible := func(ctx context.Context, c piquestions.Caller) (bool, error) {
		ops, err := f.Manager.Bind(ctx, c.Handle)
		if err != nil {
			return false, err
		}
		view, err := ops.Read.Read(ctx, piruntime.BackendIdentity(c.Handle))
		return view.Status == "idle", err
	}
	qOrigin := piquestions.NativeOriginVerifier{Inspector: f.Manager}
	admitted := piquestions.AdmittedStore{Store: questions, Admit: admit, Current: current, VerifyOrigin: qOrigin.Verify}
	response := piquestions.ResponseInput{ResponseID: piquestions.UUID(), QuestionID: q.ID, QuestionRevision: q.Revision, Answers: []piquestions.Answer{{QuestionID: "0", ChoiceID: "1", Text: "/literal answer é\n"}}}
	if _, err := admitted.AcceptResponse(ctx, handle.Conversation, response); err != nil {
		t.Fatal(err)
	}
	questionSends := 0
	var originalQuestion piquestions.Delivery
	questionService := piquestions.Service{Store: questions, Admit: admit, Dispatch: piquestions.Dispatch{Current: current, VerifyOrigin: qOrigin.Verify, Ready: eligible,
		Prepare: func(_ context.Context, c piquestions.Caller, d piquestions.Delivery) (string, error) {
			return nativeRef(c, d.OperationID, d.Message, d.ActionContext, f.Settings).Digest, nil
		},
		Send: func(ctx context.Context, c piquestions.Caller, d piquestions.Delivery) (string, error) {
			questionSends++
			originalQuestion = d
			_, data, found := strings.Cut(d.Message, "\n")
			var payload struct {
				ResponseID string               `json:"responseId"`
				Answers    []piquestions.Answer `json:"answers"`
			}
			if !found || json.Unmarshal([]byte(data), &payload) != nil || payload.ResponseID != response.ResponseID || len(payload.Answers) != 1 || payload.Answers[0] != response.Answers[0] || strings.HasPrefix(d.Message, "/") {
				return "Unknown", piquestions.ErrConflict
			}
			if d.OperationID != response.ResponseID || d.ActionContext != fmt.Sprintf("question-answer:%s:%d:%s", q.ID, q.Revision, response.ResponseID) || d.ReceiptRef != nativeRef(c, d.OperationID, d.Message, d.ActionContext, f.Settings).Digest {
				return "Unknown", piquestions.ErrConflict
			}
			result, err := f.Manager.Deliver(ctx, c.Handle, backend.SendRequest{Message: d.Message, ClientUserMessageID: d.OperationID, ActionContext: d.ActionContext})
			if err == nil && result.Outcome == backend.Accepted {
				return "Unknown", errors.New("fixture lost answer ACK")
			}
			return "Unknown", err
		}, Lookup: func(ctx context.Context, c piquestions.Caller, d piquestions.Delivery) (string, error) {
			ref := nativeRef(c, d.OperationID, d.Message, d.ActionContext, f.Settings)
			if d.OperationID != originalQuestion.OperationID || d.Message != originalQuestion.Message || d.ActionContext != originalQuestion.ActionContext || d.ReceiptRef != originalQuestion.ReceiptRef || ref.Digest != d.ReceiptRef {
				return "Unknown", piquestions.ErrConflict
			}
			status, err := f.Manager.Lookup(ctx, ref)
			if err == nil && (status.Witness == nil || status.Input.Message != d.Message || status.Input.ActionContext != d.ActionContext || status.Witness.EffectiveDigest != piruntime.MessageDigest(d.Message)) {
				return "Unknown", piquestions.ErrConflict
			}
			return status.Phase, err
		}}}
	if err := questionService.DeliverNext(ctx, handle.Conversation); err == nil {
		t.Fatal("lost question ACK succeeded")
	}
	f.Idle(handle)
	questionService.Dispatch.VerifyOrigin = nil
	if err := questionService.DeliverNext(ctx, handle.Conversation); err != nil || questionSends != 1 {
		t.Fatal(err, questionSends)
	}
	if delivered, err := questions.Response(ctx, handle.Conversation, response.ResponseID); err != nil || delivered.DeliveryPhase != "Accepted" {
		t.Fatal(delivered, err)
	}
	mode := &lostModeAck{manager: f.Manager}
	pOrigin := piplan.NativeOriginVerifier{Inspector: f.Manager}
	adapter := &piplan.ToolPolicyAdapter{Controller: mode, Current: current, VerifyOrigin: pOrigin.Verify, Admit: admit, Eligible: eligible,
		ComposeDefault: func(_ context.Context, _ piplan.Caller, revision uint64) (piruntime.ToolPolicyTarget, error) {
			target := initial
			target.Mode, target.Revision = "default", revision
			target.Active = []string{"read", "request_user_input_async", "write"}
			target.Digest = piruntime.ToolPolicyDigest(target)
			return target, nil
		}}
	store := piplan.PolicyStore{Store: plans, Adapter: adapter}
	input := piplan.AcceptanceInput{AcceptanceID: piquestions.UUID(), PlanID: p.PlanID, Revision: p.Revision, TextSHA256: p.TextSHA256}
	accepted, err := store.Accept(ctx, handle.Conversation, input)
	if err != nil {
		t.Fatal(err)
	}
	planSends, prepares := 0, 0
	var originalPlan piplan.Acceptance
	service, err := adapter.BindService(plans, piplan.Ports{
		Prepare: func(_ context.Context, c piplan.Caller, a piplan.Acceptance) (string, error) {
			prepares++
			return nativeRef(c, a.OperationID, a.Message, a.ActionContext, f.Settings).Digest, nil
		},
		Send: func(ctx context.Context, c piplan.Caller, a piplan.Acceptance) (string, error) {
			planSends++
			originalPlan = a
			_, data, found := strings.Cut(a.Message, "\n")
			var payload struct {
				PlanID string `json:"planId"`
				Digest string `json:"textSha256"`
				Text   string `json:"text"`
			}
			if !found || json.Unmarshal([]byte(data), &payload) != nil || payload.PlanID != p.PlanID || payload.Digest != p.TextSHA256 || payload.Text != nativePlanText {
				return "Unknown", piquestions.ErrConflict
			}
			if a.OperationID != accepted.OperationID || a.ActionContext != fmt.Sprintf("plan:%s:%s", p.PlanID, p.TextSHA256) || a.TextSHA256 != p.TextSHA256 || a.ReceiptRef != nativeRef(c, a.OperationID, a.Message, a.ActionContext, f.Settings).Digest {
				return "Unknown", piquestions.ErrConflict
			}
			result, err := f.Manager.Deliver(ctx, c.Handle, backend.SendRequest{Message: a.Message, ClientUserMessageID: a.OperationID, ActionContext: a.ActionContext})
			if err == nil && result.Outcome == backend.Accepted {
				return "Unknown", errors.New("fixture lost implementation ACK")
			}
			return "Unknown", err
		}, Lookup: func(ctx context.Context, c piplan.Caller, a piplan.Acceptance) (string, error) {
			ref := nativeRef(c, a.OperationID, a.Message, a.ActionContext, f.Settings)
			if a.OperationID != originalPlan.OperationID || a.Message != originalPlan.Message || a.ReceiptRef != originalPlan.ReceiptRef || a.ActionContext != originalPlan.ActionContext || ref.Digest != a.ReceiptRef {
				return "Unknown", piquestions.ErrConflict
			}
			status, err := f.Manager.Lookup(ctx, ref)
			if err == nil && (status.Witness == nil || status.Input.Message != a.Message || status.Input.ActionContext != a.ActionContext || status.Witness.EffectiveDigest != piruntime.MessageDigest(a.Message)) {
				return "Unknown", piquestions.ErrConflict
			}
			return status.Phase, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Advance(ctx, handle.Conversation, accepted.AcceptanceID); err == nil || mode.calls != 1 || prepares != 0 || planSends != 0 {
		t.Fatal("lost mode ACK advanced", err, mode.calls, prepares, planSends)
	}
	if err := service.Advance(ctx, handle.Conversation, accepted.AcceptanceID); err == nil || mode.calls != 1 || prepares != 1 || planSends != 1 {
		t.Fatal("original send ACK loss", err, mode.calls, prepares, planSends)
	}
	f.Idle(handle)
	service.Ports.VerifyOrigin = nil
	if err := service.Advance(ctx, handle.Conversation, accepted.AcceptanceID); err != nil || mode.calls != 1 || prepares != 1 || planSends != 1 {
		t.Fatal("reconciliation repeated an effect", err, mode.calls, prepares, planSends)
	}
	store.Adapter = nil
	if duplicate, err := store.Accept(ctx, handle.Conversation, input); err != nil || duplicate.OperationID != accepted.OperationID || duplicate.Phase != "accepted" {
		t.Fatal(duplicate, err)
	}
	path := filepath.Join(f.Runtime.Session.Workspace, "work", f.Runtime.Session.Slug, "owned-p6-implementation")
	if data, err := os.ReadFile(path); err != nil || string(data) != "accepted plan implemented" {
		t.Fatal(string(data), err)
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
	if users != 3 {
		t.Fatal("native inputs were replaced or resent", users)
	}
	if stop, err := f.Manager.Unload(ctx, handle); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
}
