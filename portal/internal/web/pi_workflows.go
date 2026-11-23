package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/pihosttools"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
)

// All optional ports must come from the same retained Runtime value. Merely
// implementing them does not satisfy package guards or current admission.
type piWorkflowRuntime interface {
	PiCreationRuntime
	piruntime.HostCallbackConnector
	piruntime.NativeOriginInspector
	piruntime.ToolPolicyController
	piruntime.WorkflowObserver
	piruntime.CheckedSubmissionSender
}

type piWorkflowComposition struct {
	server       *Server
	runtime      piWorkflowRuntime
	tools        []string
	observations piWorkflowObservations
}

func newPiWorkflowComposition(s *Server) *piWorkflowComposition {
	if s.config.PiCreation == nil || s.requirePiWorkflowGuards() != nil {
		return nil
	}
	runtime, ok := s.config.PiCreation.Runtime.(piWorkflowRuntime)
	if !ok {
		return nil
	}
	w := &piWorkflowComposition{server: s, runtime: runtime, tools: []string{"publish_plan", "request_user_input_async"}}
	if _, ok := runtime.(piTeamRuntime); ok {
		w.tools = append(w.tools, "report_to_lead", "team_message")
	}
	slices.Sort(w.tools)
	w.observations.initialize()
	return w
}

type piWorkflowGrant struct {
	current   piConversationAdmission
	kind      string
	operation string
	active    atomic.Bool
}
type piWorkflowGrantKey struct{}
type piConversationScopeKey struct{}

func validPiConversationScope(value string) bool {
	return strings.HasPrefix(value, "pi-workflow-v1:") && messageDigestPattern.MatchString(strings.TrimPrefix(value, "pi-workflow-v1:"))
}

func piWorkflowScopeHeader(r *http.Request) (string, error) {
	values := r.Header.Values("X-Workspace-Conversation-Scope")
	if len(values) != 1 || !validPiConversationScope(values[0]) {
		return "", agentbackend.ErrBindingConflict
	}
	return values[0], nil
}

func checkExpectedPiScope(ctx context.Context, ref agentbackend.ConversationRef) error {
	scope, _ := ctx.Value(piConversationScopeKey{}).(string)
	if scope == "" || scope != piWorkflowNamespace(ref) {
		return agentbackend.ErrBindingConflict
	}
	return nil
}

type piExpectedGenerationKey struct{}

func scopedPiConversationRequest(r *http.Request, events bool) (*http.Request, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, agentbackend.ErrBindingConflict
	}
	headers, queries := r.Header.Values("X-Workspace-Conversation-Scope"), query["conversationScope"]
	if len(headers) > 1 || len(queries) > 1 || len(queries) > 0 && !events {
		return nil, agentbackend.ErrBindingConflict
	}
	scope := ""
	if len(headers) == 1 {
		scope = headers[0]
	}
	if len(queries) == 1 {
		if len(headers) == 1 && scope != queries[0] {
			return nil, agentbackend.ErrBindingConflict
		}
		scope = queries[0]
	}
	if !validPiConversationScope(scope) {
		return nil, agentbackend.ErrBindingConflict
	}
	request := r.Clone(context.WithValue(r.Context(), piConversationScopeKey{}, scope))
	if values := r.Header.Values("X-Workspace-Pi-Generation"); len(values) != 0 {
		if len(values) != 1 || !messageDigestPattern.MatchString(values[0]) {
			return nil, agentbackend.ErrStaleExecution
		}
		request = request.WithContext(context.WithValue(request.Context(), piExpectedGenerationKey{}, values[0]))
	}
	if len(queries) > 0 {
		requestURL := *r.URL
		query.Del("conversationScope")
		requestURL.RawQuery = query.Encode()
		request.URL = &requestURL
	}
	return request, nil
}

func withPiWorkflowGrant(ctx context.Context, current piConversationAdmission, kind, operation string, run func(context.Context) error) error {
	grant := &piWorkflowGrant{current: current, kind: kind, operation: operation}
	grant.active.Store(true)
	defer grant.active.Store(false)
	return run(context.WithValue(ctx, piWorkflowGrantKey{}, grant))
}

func workflowGrant(ctx context.Context, ref agentbackend.ConversationRef, kinds ...string) (*piWorkflowGrant, error) {
	grant, _ := ctx.Value(piWorkflowGrantKey{}).(*piWorkflowGrant)
	if grant == nil || !grant.active.Load() || grant.current.Handle.Conversation != ref || !slices.Contains(kinds, grant.kind) || ctx.Err() != nil {
		return nil, piquestions.ErrUnavailable
	}
	return grant, nil
}

func (w *piWorkflowComposition) withRead(ctx context.Context, ref agentbackend.ConversationRef, run func(context.Context) error) error {
	if w == nil || piquestions.ValidateRef(ref) != nil || run == nil {
		return piquestions.ErrUnavailable
	}
	return w.server.withPiSessionRead(ctx, ref.Session, func(current piSessionAdmission) error {
		if err := w.server.requirePiWorkflowGuards(); err != nil {
			return err
		}
		resolved, err := current.Binding.Resolve(ref.Member)
		if err != nil || resolved != ref {
			return agentbackend.ErrBindingConflict
		}
		access, _ := w.server.piSavedAccessLocked(current, ref)
		return withPiWorkflowGrant(ctx, piConversationAdmission{piSessionAdmission: current, Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: current.Authority.Generation}, BindingRevision: current.Binding.Revision, SavedAccess: access}, "read", "workflow.read", run)
	})
}

func isPiWorkflowOperation(operation string) bool {
	return operation == "pi-plan" || operation == "pi-questions"
}
func validPiWorkflowPath(parts []string) bool {
	if len(parts) == 2 {
		return true
	}
	if len(parts) == 3 {
		return parts[1] == "pi-plan" && (parts[2] == "acceptances" || parts[2] == "mode") || parts[1] == "pi-questions" && parts[2] == "responses"
	}
	return len(parts) == 4 && piquestions.ValidID(parts[3]) && (parts[1] == "pi-plan" && parts[2] == "acceptances" || parts[1] == "pi-questions" && parts[2] == "responses")
}

func (s *Server) piWorkflowAPI(w http.ResponseWriter, r *http.Request, parts []string) {
	if s.piWorkflows == nil {
		workflowError(w, piquestions.ErrUnavailable)
		return
	}
	scope, err := piWorkflowScopeHeader(r)
	if err != nil {
		workflowError(w, err)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), piConversationScopeKey{}, scope))
	resolve := func(ctx context.Context, r *http.Request, operation string) (ref agentbackend.ConversationRef, err error) {
		select {
		case <-s.stopping:
			return ref, piquestions.ErrUnavailable
		default:
		}
		query := r.URL.Query()
		if len(query) > 1 || len(query["member"]) > 1 {
			return ref, piquestions.ErrInvalid
		}
		for key := range query {
			if key != "member" {
				return ref, piquestions.ErrInvalid
			}
		}
		member := query.Get("member")
		if member == "" {
			member = "lead"
		}
		summary, err := session.Find(s.config.Workspace, parts[0])
		if err != nil {
			return ref, err
		}
		kind, binding, err := s.workflowPageBackend(summary)
		if err != nil || kind != agentbackend.Pi {
			return ref, agentbackend.ErrBindingConflict
		}
		ref, err = binding.Resolve(member)
		if err != nil {
			return ref, err
		}
		err = s.withPiSessionRead(ctx, ref.Session, func(current piSessionAdmission) error {
			if err := s.requirePiWorkflowGuards(); err != nil {
				return err
			}
			actual, err := current.Binding.Resolve(member)
			if err != nil || actual != ref {
				return agentbackend.ErrBindingConflict
			}
			if err := checkExpectedPiScope(ctx, actual); err != nil {
				return err
			}
			if operation == "pi-plan.read" || operation == "pi-questions.read" {
				return nil
			}
			access, err := s.piSavedAccessLocked(current, ref)
			if err != nil {
				return err
			}
			return workflowPermission(access, operation)
		})
		return ref, err
	}
	origin := s.config.BaseURL
	if candidate := r.Header.Get("Origin"); slices.Contains(s.trustedOrigins, candidate) {
		origin = candidate
	}
	if parts[1] == "pi-questions" {
		(PiQuestionHandler{Origin: origin, Resolve: resolve, Store: piWorkflowQuestionHTTP{s.piWorkflows}}).ServeHTTP(w, r)
	} else {
		(PiPlanHandler{Origin: origin, Resolve: resolve, Store: piWorkflowPlanHTTP{s.piWorkflows}, Modes: s.piWorkflows, RenderTextMarkdown: s.renderTextMarkdown}).ServeHTTP(w, r)
	}
}

func (w *piWorkflowComposition) planAdapter() *piplan.ToolPolicyAdapter {
	return &piplan.ToolPolicyAdapter{Controller: w.runtime, Admit: w.withMutation("pi-plan.accept"), Current: w.currentCaller, VerifyOrigin: (piplan.NativeOriginVerifier{Inspector: w.runtime}).Verify,
		Eligible: func(ctx context.Context, c piplan.Caller) (bool, error) {
			_, err := w.freshPolicy(ctx, c.Handle, true)
			return err == nil, err
		},
		ComposeDefault: func(ctx context.Context, c piplan.Caller, revision uint64) (piruntime.ToolPolicyTarget, error) {
			grant, err := workflowGrant(ctx, c.Handle.Conversation, "mutation")
			if err != nil || grant.current.Handle != c.Handle {
				return piruntime.ToolPolicyTarget{}, piquestions.ErrConflict
			}
			observed, err := w.freshPolicy(ctx, c.Handle, true)
			if err != nil {
				return piruntime.ToolPolicyTarget{}, err
			}
			return composePiWorkflowPolicy(grant.current.SavedAccess, "default", revision, observed.HostTools)
		}}
}

func (w *piWorkflowComposition) SetMode(ctx context.Context, ref agentbackend.ConversationRef, input piplan.ModeInput) (piplan.ModeResult, error) {
	adapter := &piplan.ModeAdapter{Controller: w.runtime, Admit: w.withMutation("pi-plan.mode"),
		Resolve: func(ctx context.Context, ref agentbackend.ConversationRef) (agentbackend.ExecutionHandle, error) {
			grant, err := workflowGrant(ctx, ref, "mutation")
			if err != nil || grant.operation != "pi-plan.mode" {
				return agentbackend.ExecutionHandle{}, piquestions.ErrUnavailable
			}
			return grant.current.Handle, nil
		},
		Eligible: func(ctx context.Context, handle agentbackend.ExecutionHandle) (bool, error) {
			_, err := w.freshPolicy(ctx, handle, true)
			return err == nil, err
		},
		Compose: func(ctx context.Context, handle agentbackend.ExecutionHandle, mode string, revision uint64) (piruntime.ToolPolicyTarget, error) {
			grant, err := workflowGrant(ctx, handle.Conversation, "mutation")
			if err != nil || grant.current.Handle != handle {
				return piruntime.ToolPolicyTarget{}, piquestions.ErrConflict
			}
			observed, err := w.freshPolicy(ctx, handle, true)
			if err != nil {
				return piruntime.ToolPolicyTarget{}, err
			}
			return composePiWorkflowPolicy(grant.current.SavedAccess, mode, revision, observed.HostTools)
		}}
	result, err := adapter.SetMode(ctx, ref, input)
	w.requestObservation(ref, false)
	return result, err
}

// The opaque domain field contains only P3's full original receipt reference.
// Message and action context remain in their existing owning domain record.
func workflowReceipt(ref piruntime.SubmissionRef) (string, error) {
	data, err := json.Marshal(ref)
	if err != nil || len(data) > 4000 {
		return "", piquestions.ErrInvalid
	}
	return "pi-submission-v1:" + string(data), nil
}

func parseWorkflowReceipt(value string, caller piquestions.Caller, operation string) (piruntime.SubmissionRef, error) {
	var ref piruntime.SubmissionRef
	data, valid := strings.CutPrefix(value, "pi-submission-v1:")
	if !valid || len(value) > 4096 || piquestions.Decode([]byte(data), &ref) != nil || ref.Conversation != caller.Handle.Conversation || ref.OperationID != operation || !piquestions.ValidID(operation) || ref.Action != "send" || !messageDigestPattern.MatchString(ref.Digest) {
		return ref, piquestions.ErrConflict
	}
	return ref, nil
}

func (w *piWorkflowComposition) prepare(ctx context.Context, caller piquestions.Caller, operation, message, actionContext string) (string, error) {
	grant, err := workflowGrant(ctx, caller.Handle.Conversation, "mutation")
	if err != nil || grant.current.Handle != caller.Handle || !piquestions.ValidID(operation) {
		return "", piquestions.ErrConflict
	}
	observation, err := w.freshPolicy(ctx, caller.Handle, true)
	if err != nil {
		return "", err
	}
	// ObserveWorkflow's owning decoder validates complete current Settings; no
	// initial-creation settings or earlier display snapshot can substitute.
	if observation.Settings.Provider == "" || observation.Settings.Model == "" || observation.Settings.PolicyRevision == "" || observation.Settings.Revision == 0 {
		return "", piruntime.ErrUnavailable
	}
	input := piruntime.SendInput{Message: message, ActionContext: actionContext}
	ref := piruntime.SubmissionRef{Conversation: caller.Handle.Conversation, OperationID: operation, Action: "send"}
	ref.Digest = piruntime.SubmissionDigest(ref, input, observation.Settings)
	return workflowReceipt(ref)
}

func workflowReceiptPhase(status piruntime.SubmissionStatus, ref piruntime.SubmissionRef, input piruntime.SendInput) (string, error) {
	if status.Schema != 1 || status.Ref != ref || status.Input != input || !messageDigestPattern.MatchString(status.Generation) || piruntime.SubmissionDigest(ref, input, status.Settings) != ref.Digest {
		return "Unknown", piruntime.ErrScope
	}
	switch status.Phase {
	case "Accepted":
		if status.Witness == nil || status.Witness.SessionID != ref.Conversation.NativeID || status.Witness.EntryID == "" || status.Witness.EffectiveDigest != piruntime.MessageDigest(input.Message) {
			return "Unknown", piruntime.ErrHeld
		}
	case "NotSubmitted":
		if status.Refusal == "" {
			return "Unknown", piruntime.ErrHeld
		}
	case "Unknown":
	default:
		return "Unknown", piruntime.ErrHeld
	}
	return status.Phase, nil
}

func (w *piWorkflowComposition) submit(ctx context.Context, caller piquestions.Caller, operation, message, actionContext, receipt string, lookup bool) (string, error) {
	grant, err := workflowGrant(ctx, caller.Handle.Conversation, "read", "mutation")
	if err != nil || grant.current.Handle != caller.Handle || (!lookup && grant.kind != "mutation") {
		return "Unknown", piquestions.ErrUnavailable
	}
	ref, err := parseWorkflowReceipt(receipt, caller, operation)
	if err != nil {
		return "Unknown", err
	}
	input := piruntime.SendInput{Message: message, ActionContext: actionContext}
	var status piruntime.SubmissionStatus
	if lookup {
		status, err = w.runtime.Lookup(ctx, ref)
	} else {
		// The owning domain has persisted handoff before this one invocation.
		// All revisits use Lookup, including absence and lost acknowledgement.
		status, err = w.runtime.SendCheckedSubmission(ctx, caller.Handle, ref, input)
	}
	if err != nil {
		return "Unknown", err
	}
	return workflowReceiptPhase(status, ref, input)
}

func (w *piWorkflowComposition) questionService(ref agentbackend.ConversationRef, observational bool) *piquestions.Service {
	questions, _ := w.stores(ref)
	service := &piquestions.Service{Store: questions, Admit: w.withRead, Dispatch: piquestions.Dispatch{Current: w.currentCaller,
		Lookup: func(ctx context.Context, caller piquestions.Caller, d piquestions.Delivery) (string, error) {
			return w.submit(ctx, caller, d.OperationID, d.Message, d.ActionContext, d.ReceiptRef, true)
		}}}
	if observational {
		return service
	}
	service.Admit = w.withMutation("pi-questions.respond")
	service.Dispatch.VerifyOrigin = (piquestions.NativeOriginVerifier{Inspector: w.runtime}).Verify
	service.Dispatch.Ready = func(ctx context.Context, caller piquestions.Caller) (bool, error) {
		_, err := w.freshPolicy(ctx, caller.Handle, true)
		return err == nil, err
	}
	service.Dispatch.Prepare = func(ctx context.Context, caller piquestions.Caller, d piquestions.Delivery) (string, error) {
		return w.prepare(ctx, caller, d.OperationID, d.Message, d.ActionContext)
	}
	service.Dispatch.Send = func(ctx context.Context, caller piquestions.Caller, d piquestions.Delivery) (string, error) {
		return w.submit(ctx, caller, d.OperationID, d.Message, d.ActionContext, d.ReceiptRef, false)
	}
	return service
}

func (w *piWorkflowComposition) planService(ref agentbackend.ConversationRef, observational bool) (*piplan.Service, error) {
	_, plans := w.stores(ref)
	ports := piplan.Ports{Admit: w.withRead, Current: w.currentCaller, Lookup: func(ctx context.Context, c piplan.Caller, a piplan.Acceptance) (string, error) {
		return w.submit(ctx, c, a.OperationID, a.Message, a.ActionContext, a.ReceiptRef, true)
	}}
	if observational {
		return &piplan.Service{Store: plans, Ports: ports}, nil
	}
	adapter := w.planAdapter()
	adapter.Admit = w.withMutation("pi-plan.advance")
	ports.Prepare = func(ctx context.Context, c piplan.Caller, a piplan.Acceptance) (string, error) {
		// Reprove the exact frozen Default target after Apply or a lost Apply ACK.
		if ok, err := adapter.ObserveDefault(ctx, c, a.PolicyRevision); err != nil || !ok {
			return "", errors.Join(piquestions.ErrUnavailable, err)
		}
		return w.prepare(ctx, c, a.OperationID, a.Message, a.ActionContext)
	}
	ports.Send = func(ctx context.Context, c piplan.Caller, a piplan.Acceptance) (string, error) {
		return w.submit(ctx, c, a.OperationID, a.Message, a.ActionContext, a.ReceiptRef, false)
	}
	return adapter.BindService(plans, ports)
}

func (w *piWorkflowComposition) drain(ctx context.Context, ref agentbackend.ConversationRef) {
	_ = w.questionService(ref, false).DeliverNext(ctx, ref)
	var snapshot piplan.Snapshot
	if err := w.withRead(ctx, ref, func(ctx context.Context) error {
		_, plans := w.stores(ref)
		var err error
		snapshot, err = plans.Snapshot(ctx, ref)
		return err
	}); err != nil {
		return
	}
	service, err := w.planService(ref, false)
	if err != nil {
		return
	}
	for _, a := range snapshot.Acceptances {
		if a.Phase != "accepted" {
			_ = service.Advance(ctx, ref, a.AcceptanceID)
		}
	}
}

func (w *piWorkflowComposition) reconcileOriginal(ctx context.Context, ref agentbackend.ConversationRef) {
	w.invalidateRef(ref, false)
	_ = w.questionService(ref, true).DeliverNext(ctx, ref)
	var snapshot piplan.Snapshot
	if err := w.withRead(ctx, ref, func(ctx context.Context) error {
		_, plans := w.stores(ref)
		var err error
		snapshot, err = plans.Snapshot(ctx, ref)
		return err
	}); err != nil {
		return
	}
	service, _ := w.planService(ref, true)
	for _, a := range snapshot.Acceptances {
		if a.Phase != "accepted" {
			_ = service.Advance(ctx, ref, a.AcceptanceID)
		}
	}
}

type piWorkflowQuestionHTTP struct{ owner *piWorkflowComposition }

func (h piWorkflowQuestionHTTP) Publish(context.Context, piquestions.Caller, piquestions.PublishInput) (piquestions.QuestionView, error) {
	return piquestions.QuestionView{}, piquestions.ErrUnavailable
}
func (h piWorkflowQuestionHTTP) Close(context.Context, piquestions.Caller, string, uint64) (piquestions.QuestionView, error) {
	return piquestions.QuestionView{}, piquestions.ErrUnavailable
}
func (h piWorkflowQuestionHTTP) admitted(ref agentbackend.ConversationRef) piquestions.AdmittedStore {
	questions, _ := h.owner.stores(ref)
	return piquestions.AdmittedStore{Store: questions, Admit: h.owner.withMutation("pi-questions.respond"), Current: h.owner.currentCaller, VerifyOrigin: (piquestions.NativeOriginVerifier{Inspector: h.owner.runtime}).Verify, Observations: h.owner.questionObservations}
}
func (h piWorkflowQuestionHTTP) Snapshot(ctx context.Context, ref agentbackend.ConversationRef) (result piquestions.Snapshot, err error) {
	err = h.owner.withRead(ctx, ref, func(ctx context.Context) error { var e error; result, e = h.admitted(ref).Snapshot(ctx, ref); return e })
	return
}
func (h piWorkflowQuestionHTTP) AcceptResponse(ctx context.Context, ref agentbackend.ConversationRef, in piquestions.ResponseInput) (piquestions.ResponseView, error) {
	result, err := h.admitted(ref).AcceptResponse(ctx, ref, in)
	if err != nil {
		return result, err
	}
	// Authenticate the final original receipt read under current read ownership,
	// including the domain's duplicate-before-effect path.
	result, err = h.Response(ctx, ref, in.ResponseID)
	if err == nil {
		h.owner.requestObservation(ref, true)
	}
	return result, err
}
func (h piWorkflowQuestionHTTP) Response(ctx context.Context, ref agentbackend.ConversationRef, id string) (result piquestions.ResponseView, err error) {
	err = h.owner.withRead(ctx, ref, func(ctx context.Context) error {
		var e error
		result, e = h.admitted(ref).Response(ctx, ref, id)
		return e
	})
	return
}

type piWorkflowPlanHTTP struct{ owner *piWorkflowComposition }

func (h piWorkflowPlanHTTP) admitted(ref agentbackend.ConversationRef) piplan.PolicyStore {
	_, plans := h.owner.stores(ref)
	return piplan.PolicyStore{Store: plans, Adapter: h.owner.planAdapter(), Observations: h.owner.policyObservation}
}
func (h piWorkflowPlanHTTP) Snapshot(ctx context.Context, ref agentbackend.ConversationRef) (result piplan.Snapshot, err error) {
	err = h.owner.withRead(ctx, ref, func(ctx context.Context) error { var e error; result, e = h.admitted(ref).Snapshot(ctx, ref); return e })
	return
}
func (h piWorkflowPlanHTTP) Accept(ctx context.Context, ref agentbackend.ConversationRef, in piplan.AcceptanceInput) (piplan.AcceptanceView, error) {
	result, err := h.admitted(ref).Accept(ctx, ref, in)
	if err != nil {
		return result, err
	}
	result, err = h.LookupAcceptance(ctx, ref, in.AcceptanceID)
	if err == nil {
		h.owner.requestObservation(ref, true)
	}
	return result, err
}
func (h piWorkflowPlanHTTP) LookupAcceptance(ctx context.Context, ref agentbackend.ConversationRef, id string) (result piplan.AcceptanceView, err error) {
	err = h.owner.withRead(ctx, ref, func(ctx context.Context) error {
		var e error
		result, e = h.admitted(ref).LookupAcceptance(ctx, ref, id)
		return e
	})
	return
}

func (w *piWorkflowComposition) withRecord(ctx context.Context, handle agentbackend.ExecutionHandle, operation string, run func(context.Context) error) error {
	if w == nil || run == nil {
		return piquestions.ErrUnavailable
	}
	return w.server.withPiSessionRecordWrite(ctx, handle.Conversation.Session, handle.Generation, func(current piSessionAdmission) error {
		if err := w.server.requirePiWorkflowGuards(); err != nil {
			return err
		}
		if current.Authority.ValidateHandle(current.Binding, handle) != nil {
			return agentbackend.ErrStaleExecution
		}
		w.invalidateRef(handle.Conversation, true)
		return withPiWorkflowGrant(ctx, piConversationAdmission{piSessionAdmission: current, Handle: handle, BindingRevision: current.Binding.Revision}, "record", operation, run)
	})
}

func (w *piWorkflowComposition) withMutation(operation string) func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error {
	return func(ctx context.Context, ref agentbackend.ConversationRef, run func(context.Context) error) error {
		if w == nil || run == nil {
			return piquestions.ErrUnavailable
		}
		return w.server.withPiConversationMutation(ctx, ref, func(current piConversationAdmission) error {
			if err := w.server.requirePiWorkflowGuards(); err != nil {
				return err
			}
			if err := workflowPermission(current.SavedAccess, operation); err != nil {
				return err
			}
			w.invalidateRef(ref, true)
			return withPiWorkflowGrant(ctx, current, "mutation", operation, run)
		})
	}
}

func workflowPermission(access, operation string) error {
	if access != "read_only" && access != "workspace_write" {
		return piquestions.ErrUnavailable
	}
	switch operation {
	case "pi-questions.respond", "pi-plan.mode", "conversation.message", "conversation.interrupt", "conversation.uploads", "conversation.settings":
		return nil
	case "pi-plan.accept", "pi-plan.advance":
		if access == "workspace_write" {
			return nil
		}
	}
	return piquestions.ErrUnavailable
}

func (w *piWorkflowComposition) currentCaller(ctx context.Context, caller piquestions.Caller) (piquestions.Caller, error) {
	grant, err := workflowGrant(ctx, caller.Handle.Conversation, "read", "record", "mutation")
	if err != nil || piquestions.ValidateCaller(caller) != nil {
		return piquestions.Caller{}, piquestions.ErrConflict
	}
	caller.Handle = grant.current.Handle
	return caller, nil
}

// Each store gets an immutable host-resolved scope closure. Its validator runs
// inside short domain locks as well as observational pre-reads; it never acquires
// authority or calls native code. Actual writes/effects enter the ports above.
func (w *piWorkflowComposition) stores(ref agentbackend.ConversationRef) (*piquestions.FileStore, *piplan.FileStore) {
	validate := func(_ context.Context, actual agentbackend.ConversationRef, caller *piquestions.Caller) error {
		if piquestions.ValidateRef(ref) != nil || ref.Session.Workspace != w.server.config.Workspace || actual != ref || (caller != nil && (piquestions.ValidateCaller(*caller) != nil || caller.Handle.Conversation != ref)) {
			return piquestions.ErrConflict
		}
		return nil
	}
	return &piquestions.FileStore{Root: w.server.config.UserStateRoot, Validate: validate}, &piplan.FileStore{Root: w.server.config.UserStateRoot, Validate: validate}
}

type piWorkflowHostHandler struct {
	owner   *piWorkflowComposition
	attempt *piWorkflowAttachment
}

func (h *piWorkflowHostHandler) HostTools() []string { return slices.Clone(h.owner.tools) }
func (h *piWorkflowHostHandler) InvalidateWorkflow(event piruntime.WorkflowInvalidation) {
	h.owner.invalidateRuntime(h.attempt, event)
}

func (h *piWorkflowHostHandler) HandleHostCall(ctx context.Context, call piruntime.HostCall) (reply piruntime.HostReply, err error) {
	reply.Outcome, reply.Code = "refused", "scope"
	rejected := func(reason string) {
		if logger := h.owner.server.config.Logger; logger != nil {
			logger.Printf("Pi workflow callback rejected: %s", reason)
		}
	}
	if call.Runtime != h.attempt.runtime || call.Subject.Kind != "conversation" || call.Subject.Conversation == nil || call.Subject.Utility != nil || !slices.Contains(h.owner.tools, call.Tool) || !piPublicationOperationID.MatchString(call.InputOperationID) {
		rejected("subject or original input identity")
		return reply, piruntime.ErrScope
	}
	digest := sha256.Sum256(call.Arguments)
	if call.ArgumentsSHA256 != hex.EncodeToString(digest[:]) {
		rejected("argument digest")
		return reply, piruntime.ErrScope
	}
	handle := *call.Subject.Conversation
	if handle.Conversation.Session != call.Runtime.Session || handle.Generation != call.Runtime.Generation {
		rejected("runtime recipient")
		return reply, piruntime.ErrScope
	}
	caller := piquestions.Caller{Handle: handle, Lineage: piquestions.LineageRef{AnchorEntryID: call.AnchorEntryID, SelectionToken: call.SelectionToken}, NativeToolCallID: call.NativeToolCallID}
	if piquestions.ValidateCaller(caller) != nil {
		rejected("publication caller")
		return reply, piquestions.ErrInvalid
	}
	if call.Tool == "report_to_lead" || call.Tool == "team_message" {
		if err := (piTeamService{h.owner.server}).ensureCallerReady(ctx, teamruntime.PiCaller{Runtime: call.Runtime, Handle: handle}, call.InputOperationID); err != nil {
			h.owner.server.config.Logger.Printf("Pi %s caller publication unavailable for %s/%s: %v", call.Tool, handle.Conversation.Session.Slug, handle.Conversation.Member, err)
			return reply, err
		}
		tools, bindErr := pihosttools.BindTeamTools(teamruntime.PiCaller{Runtime: call.Runtime, Handle: handle}, piTeamService{h.owner.server})
		if bindErr != nil {
			return reply, bindErr
		}
		value, callErr := tools.Call(ctx, call.Tool, call.Arguments)
		var known *piTeamPreSendRefusal
		if ctx.Err() == nil && errors.As(callErr, &known) {
			body, err := json.Marshal(struct {
				teamruntime.PiDeliveryResult
				Stage           string `json:"stage"`
				Reason          string `json:"reason"`
				Materialization string `json:"materialization"`
			}{value, known.Stage, known.Reason, known.Materialization})
			if err != nil {
				return reply, err
			}
			h.owner.server.config.Logger.Printf("Pi %s callback member=%s operation=%s stage=%s outcome=%s materialization=%s", call.Tool, handle.Conversation.Member, call.InputOperationID, known.Stage, known.Reason, known.Materialization)
			return piruntime.HostReply{Outcome: "result", Body: body}, nil
		}
		if callErr != nil {
			h.owner.server.config.Logger.Printf("Pi %s callback unavailable for %s/%s: %v", call.Tool, handle.Conversation.Session.Slug, handle.Conversation.Member, callErr)
		}
		body, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return reply, marshalErr
		}
		// Delivery preserves Unknown/Pending rather than claiming a failed host
		// transport was a pre-effect refusal. No record or team lock spans it.
		if value.Disposition == teamruntime.PiAttempted {
			return piruntime.HostReply{Outcome: "result", Body: body}, nil
		}
		return piruntime.HostReply{Outcome: "result", Body: body}, callErr
	}
	// Manager's authenticated callback engine has already proved original
	// Accepted input and native origin. Domain locks see only the pure scope
	// validator and exact native arguments, never a native reinspection.
	err = h.owner.withRecord(ctx, handle, call.Tool, func(ctx context.Context) error {
		questions, plans := h.owner.stores(handle.Conversation)
		var value any
		switch call.Tool {
		case "request_user_input_async":
			var arguments struct {
				Questions []struct {
					Title   string   `json:"title"`
					Options []string `json:"options,omitempty"`
				} `json:"questions"`
			}
			if err := piquestions.DecodeOriginArguments(call.Arguments, &arguments); err != nil {
				return err
			}
			input := piquestions.PublishInput{Questions: []piquestions.Question{}}
			for _, q := range arguments.Questions {
				question := piquestions.Question{Title: q.Title, Options: []piquestions.Option{}}
				for _, option := range q.Options {
					question.Options = append(question.Options, piquestions.Option{Label: option})
				}
				input.Questions = append(input.Questions, question)
			}
			published, err := questions.Publish(ctx, caller, input)
			if err != nil {
				return err
			}
			value = published
		case "publish_plan":
			var arguments struct {
				Text string `json:"text"`
			}
			if err := piquestions.DecodeOriginArguments(call.Arguments, &arguments); err != nil {
				return err
			}
			published, err := plans.PublishProposal(ctx, caller, arguments.Text, piplan.NativeOrigin{EntryID: call.AnchorEntryID, Complete: true})
			if err != nil {
				return err
			}
			value = published
		default:
			return piquestions.ErrUnavailable
		}
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		reply = piruntime.HostReply{Outcome: "result", Body: body}
		return nil
	})
	if err != nil {
		h.owner.server.config.Logger.Printf("Pi %s callback unavailable for %s/%s: %v", call.Tool, handle.Conversation.Session.Slug, handle.Conversation.Member, err)
		reply = piruntime.HostReply{Outcome: "refused", Code: "unavailable"}
	}
	return reply, err
}

func (w *piWorkflowComposition) ensureAttachment(ctx context.Context, runtime piruntime.RuntimeRef) error {
	if w == nil {
		return piquestions.ErrUnavailable
	}
	return w.ensureAttachmentWithConnector(ctx, runtime, w.runtime)
}

func (w *piWorkflowComposition) ensureAttachmentWithConnector(ctx context.Context, runtime piruntime.RuntimeRef, connector piruntime.HostCallbackConnector) error {
	if w == nil || runtime.Session.Workspace != w.server.config.Workspace || !messageDigestPattern.MatchString(runtime.Generation) {
		return piquestions.ErrUnavailable
	}
	if connector == nil {
		return piquestions.ErrUnavailable
	}
	o := &w.observations
	// A failed or previous-generation connection owns its goroutine until joined.
	// Never replace it while it can still deliver callbacks or invalidations.
	for {
		o.mu.Lock()
		prior := o.attachments[runtime.Session]
		if prior == nil || (prior.runtime == runtime && !prior.lost) {
			o.mu.Unlock()
			break
		}
		prior.cancel()
		o.mu.Unlock()
		select {
		case <-prior.finished:
			o.mu.Lock()
			if o.attachments[runtime.Session] == prior {
				delete(o.attachments, runtime.Session)
			}
			o.mu.Unlock()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	o.mu.Lock()
	attempt := o.attachments[runtime.Session]
	if attempt != nil && attempt.runtime != runtime {
		attempt.cancel()
		select {
		case <-attempt.finished:
			attempt = nil
		default:
			o.mu.Unlock()
			return piruntime.ErrHeld
		}
	}
	if attempt != nil {
		select {
		case <-attempt.finished:
			attempt = nil
		default:
		}
	}
	if attempt != nil && attempt.lost {
		attempt.cancel()
		o.mu.Unlock()
		return piruntime.ErrUnavailable
	}
	if attempt == nil {
		w.server.operationMu.Lock()
		if w.server.closing || w.server.operationContext.Err() != nil {
			w.server.operationMu.Unlock()
			o.mu.Unlock()
			return piquestions.ErrUnavailable
		}
		attachmentCtx, cancel := context.WithCancel(w.server.operationContext)
		attempt = &piWorkflowAttachment{runtime: runtime, ready: make(chan struct{}), finished: make(chan struct{}), cancel: cancel}
		o.attachments[runtime.Session] = attempt
		handler := &piWorkflowHostHandler{owner: w, attempt: attempt}
		w.server.operationWG.Add(1)
		w.server.operationMu.Unlock()
		go func() {
			defer w.server.operationWG.Done()
			err := connector.AttachHost(attachmentCtx, runtime, handler)
			o.mu.Lock()
			attempt.connected = false
			attempt.lost = true
			attempt.err = err
			close(attempt.finished)
			for ref := range o.counters {
				if ref.Session == runtime.Session {
					o.invalidateLocked(ref, false)
				}
			}
			o.mu.Unlock()
			w.wake()
		}()
	}
	o.mu.Unlock()
	select {
	case <-attempt.ready:
		o.mu.Lock()
		available := attempt.connected && o.attachments[runtime.Session] == attempt && attempt.sequence > attempt.initial
		o.mu.Unlock()
		if available {
			return nil
		}
		return piquestions.ErrUnavailable
	case <-attempt.finished:
		return piquestions.ErrUnavailable
	case <-ctx.Done():
		attempt.cancel()
		o.mu.Lock()
		attempt.connected = false
		attempt.lost = true
		for ref := range o.counters {
			if ref.Session == runtime.Session {
				o.invalidateLocked(ref, false)
			}
		}
		o.mu.Unlock()
		return ctx.Err()
	}
}

type piWorkflowDescriptor struct {
	Backend   string `json:"backend"`
	BasePath  string `json:"basePath"`
	Member    string `json:"member"`
	Namespace string `json:"namespace"`
}

func piWorkflowNamespace(ref agentbackend.ConversationRef) string {
	// Browser storage follows durable workspace/session/member scope. Native
	// history paths, native IDs and execution generation are not hash inputs.
	data, _ := json.Marshal([]string{"pi-workflow-v1", string(ref.Backend), ref.Session.Workspace, ref.Session.Slug, ref.Session.Incarnation, ref.Member, ref.Incarnation})
	digest := sha256.Sum256(data)
	return "pi-workflow-v1:" + hex.EncodeToString(digest[:])
}

// PiConversationScope is the same durable identity used by the installed page
// and private CLI transport; it grants no admission by itself.
func PiConversationScope(ref agentbackend.ConversationRef) string { return piWorkflowNamespace(ref) }

func (w *piWorkflowComposition) validWorkerHostTools(tools []string) bool {
	if !slices.Contains(tools, "publish_plan") || !slices.Contains(tools, "request_user_input_async") {
		return false
	}
	for i, name := range tools {
		if !slices.Contains(w.tools, name) || i > 0 && tools[i-1] >= name {
			return false
		}
	}
	return true
}

func composePiWorkflowPolicy(access, mode string, revision uint64, hostTools []string) (piruntime.ToolPolicyTarget, error) {
	if (access != "read_only" && access != "workspace_write") || (mode != "plan" && mode != "default") {
		return piruntime.ToolPolicyTarget{}, piquestions.ErrUnavailable
	}
	ceiling := []string{"find", "grep", "ls", "read"}
	if access == "workspace_write" {
		ceiling = append(ceiling, "bash", "edit", "write")
	}
	for _, name := range hostTools {
		if name != "publish_plan" && name != "request_user_input_async" && name != "report_to_lead" && name != "team_message" {
			return piruntime.ToolPolicyTarget{}, piquestions.ErrUnavailable
		}
		ceiling = append(ceiling, name)
	}
	slices.Sort(ceiling)
	if len(slices.Compact(slices.Clone(ceiling))) != len(ceiling) {
		return piruntime.ToolPolicyTarget{}, piquestions.ErrConflict
	}
	active := slices.Clone(ceiling)
	if mode == "plan" {
		active = slices.DeleteFunc(active, func(name string) bool {
			return name == "bash" || name == "edit" || name == "write" || name == "report_to_lead" || name == "team_message"
		})
	}
	target := piruntime.ToolPolicyTarget{Mode: mode, Revision: revision, Active: active, RegisteredCeiling: ceiling}
	target.Digest = piruntime.ToolPolicyDigest(target)
	return target, piruntime.ValidateToolPolicy(target)
}

func (s *Server) initialPiWorkflowPolicy(intent session.PiCreationIntent, member string) (*piruntime.ToolPolicyTarget, error) {
	if s.piWorkflows == nil || intent.ValidateShape() != nil {
		return nil, piquestions.ErrUnavailable
	}
	access := ""
	if member == "lead" && intent.Fork != nil {
		access = intent.Fork.LeadAccess
	} else if member == "lead" {
		if s.installedTeams == nil || s.installedTeams.Catalog == nil || s.installedTeams.Catalog.CatalogDigest != intent.DirectTeam.CatalogDigest {
			return nil, piquestions.ErrUnavailable
		}
		team, ok := s.installedTeams.Catalog.Team(intent.DirectTeam.ID)
		role, found := team.Roles["team_lead"]
		if !ok || !found || !s.piSourceTeamMatches(intent) {
			return nil, piquestions.ErrUnavailable
		}
		access = role.Access
	} else {
		for _, saved := range intent.DirectTeam.Members {
			if saved.Address == member {
				access = saved.Access
			}
		}
	}
	target, err := composePiWorkflowPolicy(access, "default", 1, s.piWorkflows.tools)
	if err != nil {
		return nil, err
	}
	return &target, nil
}

func (s *Server) requirePiWorkflowGuards() error {
	file, err := os.Open(filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return err
	}
	if _, err := session.DecodePiWorkflowContract(data); err != nil {
		return err
	}
	var contract struct {
		PiTeamRecords json.RawMessage `json:"piTeamRecords"`
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		return err
	}
	_, err = session.DecodePiTeamRecordsContract(contract.PiTeamRecords)
	return err
}

// Callers retain transition admission and recheck this after creation/runtime
// waits. Empty historical roles do not acquire a new package capability gate.
func (s *Server) requirePiRoleInstructions(original string) error {
	if original == "" {
		return nil
	}
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(s.config.PackageRoot)
	if err != nil || root != s.config.PackageRoot {
		return piruntime.ErrUnavailable
	}
	selected, err := filepath.EvalSymlinks(s.config.HostProfile)
	if err != nil || selected != root {
		return piruntime.ErrHeld
	}
	file, err := os.Open(filepath.Join(root, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return err
	}
	_, err = session.DecodePiRoleInstructionsContract(data)
	return err
}

// Backend selection is independent of optional workflow support. In particular,
// a Pi descriptor failure can never normalize that conversation to Codex.
func (s *Server) workflowPageBackend(summary *session.Summary) (agentbackend.Kind, *session.BackendBinding, error) {
	if summary == nil || !session.ValidSlug(summary.Slug) {
		return "", nil, agentbackend.ErrNotBound
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(summary.Slug)
	if err == nil {
		if summary.Codex.ThreadID != "" || summary.Manifest.Backend == nil || summary.Manifest.Backend.Kind != "pi" || summary.Manifest.Backend.Contract != 1 || summary.Manifest.Backend.SessionIncarnation != binding.SessionIncarnation {
			return "", nil, agentbackend.ErrBindingConflict
		}
		return agentbackend.Pi, &binding, nil
	}
	if !errors.Is(err, agentbackend.ErrNotBound) {
		return "", nil, err
	}
	if err := s.requireCodexSessionBackend(summary.Slug); err != nil {
		return "", nil, err
	}
	if _, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, summary.Slug, s.config.Workspace); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	if summary.Manifest.Schema != 1 && summary.Manifest.Schema != 2 && summary.Manifest.Schema != 3 {
		return "", nil, agentbackend.ErrUnsupportedBinding
	}
	return agentbackend.Codex, nil, nil
}

func (s *Server) setWorkflowPageData(data *pageData) {
	kind, binding, err := s.workflowPageBackend(data.Session)
	if err != nil {
		return
	}
	data.SessionBackend = string(kind)
	data.HasConversation = kind == agentbackend.Codex && data.Session.Codex.ThreadID != ""
	if kind != agentbackend.Pi {
		data.ConversationReady, data.ConversationInteractive, data.ConversationHeld = data.Session.RecoveryReady, data.Session.Interactive, data.Session.ExecutionHeld
		return
	}
	member := data.SelectedMember
	if member == "" {
		member = "lead"
	}
	ref, err := binding.Resolve(member)
	if err != nil {
		return
	}
	data.HasConversation = true
	data.ConversationScope = piWorkflowNamespace(ref)
	if s.piWorkflows == nil || s.requirePiWorkflowGuards() != nil {
		return
	}
	if _, ok := s.config.PiCreation.Runtime.(piWorkflowRuntime); !ok {
		return
	}
	// Ordinary JSON remains a string so html/template escapes the attribute.
	descriptor, err := json.Marshal(piWorkflowDescriptor{Backend: "pi", BasePath: "/api/sessions/" + data.Session.Slug, Member: member, Namespace: piWorkflowNamespace(ref)})
	if err == nil {
		data.PiWorkflow = string(descriptor)
	}
}

func (s *Server) configurePiSessionPage(ctx context.Context, data *pageData, members []string) error {
	if len(members) > 1 {
		return agentbackend.ErrBindingConflict
	}
	member := "lead"
	if len(members) == 1 && members[0] != "" {
		member = members[0]
	}
	kind, binding, err := s.workflowPageBackend(data.Session)
	if err != nil || kind != agentbackend.Pi {
		return agentbackend.ErrBindingConflict
	}
	ref, err := binding.Resolve(member)
	if err != nil {
		return err
	}
	data.SelectedMember, data.ConversationID = member, data.Session.Slug
	data.ConversationScope = piWorkflowNamespace(ref)
	if member != "lead" {
		data.ConversationID = teamConversationID(data.Session.Slug, member)
	}
	data.ConversationHeld = true
	// Shared page structure does not give Pi the legacy fork/team/settings tools.
	data.Session.Interactive, data.Session.RecoveryReady = false, false
	return s.withPiSessionRead(ctx, ref.Session, func(current piSessionAdmission) error {
		actual, err := current.Binding.Resolve(member)
		if err != nil || actual != ref {
			return agentbackend.ErrBindingConflict
		}
		if expected, _ := ctx.Value(piConversationScopeKey{}).(string); expected != "" && expected != piWorkflowNamespace(actual) {
			return agentbackend.ErrBindingConflict
		}
		data.SelectedThreadID = actual.NativeID
		lead, err := current.Binding.Resolve("lead")
		if err != nil {
			return err
		}
		data.PiTeamScope = piWorkflowNamespace(lead)
		data.TeamRoles = teamRoles(s.directTeamPresets())
		data.TeamRoleDefaults = s.roleDefaults(nil, data.TeamRoles)
		for i, defaults := range data.TeamRoleDefaults {
			projected, err := teamruntime.ProjectPiPreset(teamruntime.Preset{CatalogDigest: current.intent.DirectTeam.CatalogDigest, TeamDigest: current.intent.DirectTeam.TeamDigest, Members: []teamruntime.MemberSpec{{Role: defaults.Role, Model: defaults.Model, Effort: defaults.Effort}}})
			if err == nil {
				data.TeamRoleDefaults[i].Model = projected.Members[0].Model
				data.TeamRoleDefaults[i].Effort = projected.Members[0].Effort
			}
		}
		data.ForkAvailable = s.requirePiForkContract() == nil
		data.ForkInheritsSettings = true
		data.ConversationReady = true
		data.ConversationHeld = false
		store, storeErr := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if storeErr == nil {
			roster, rosterErr := store.LoadPi(data.Session.Slug)
			if rosterErr == nil && roster.Scope().Ref() == ref.Session {
				data.PiTeam = &roster
			} else if errors.Is(rosterErr, os.ErrNotExist) {
				data.PiDerivedSolo, _ = derivedPiSolo(current)
			}
		}
		v := teamManagementView{Description: "The lead uses this session's main conversation. Each other member has a separate conversation.", SettingsHelp: "Save changes when the session is idle."}
		leadAccess, _ := s.piSavedAccessLocked(current, lead)
		leadModel, leadEffort := current.intent.Model, current.intent.Effort
		if data.PiTeam != nil {
			leadModel, leadEffort = data.PiTeam.LeadModel, data.PiTeam.LeadEffort
		}
		v.Rows = append(v.Rows, teamMemberView{Address: "lead", State: "ready", StateLabel: "root", Thread: lead.NativeID, Stats: true, Model: leadModel, Effort: leadEffort, Access: leadAccess, Scope: piWorkflowNamespace(lead), Generation: current.Authority.Generation, CanSettings: !data.Session.Archived})
		if data.PiTeam != nil {
			for _, m := range data.PiTeam.Members {
				row := teamMemberView{Address: m.Address, State: m.State, StateLabel: m.State, Model: m.Model, Effort: m.Effort, Access: m.Access, CanRemove: !data.Session.Archived && (m.State == "ready" || m.State == "creating" || m.State == "archived"), RemoveReason: "", SettingsReason: "This member is not ready."}
				if m.State == "creating" && m.InitialInputID == "" {
					row.StateLabel = "Awaiting first assignment"
				}
				if m.State == "ready" {
					if actual, e := current.Binding.Resolve(m.Address); e == nil {
						row.Thread = actual.NativeID
						row.Stats = true
						row.Scope = piWorkflowNamespace(actual)
						row.Generation = current.Authority.Generation
						row.URL = "?member=" + m.Address + "#codex"
						row.CanSettings = !data.Session.Archived
						row.SettingsReason = ""
					}
				}
				if m.State == "removed" {
					v.Removed = append(v.Removed, row)
				} else {
					v.Rows = append(v.Rows, row)
				}
			}
		}
		data.TeamView = &v
		data.TeamCount = countSnapshot{Known: data.PiTeamList() != nil, Value: len(v.Rows), ObservedAt: time.Now().UTC().Format(time.RFC3339)}
		access, accessErr := s.piSavedAccessLocked(current, ref)
		data.ConversationInteractive = s.config.PiCreation != nil && s.config.PiCreation.Runtime != nil && accessErr == nil && workflowPermission(access, "conversation.message") == nil
		data.TeamView.CanAdd = data.ConversationInteractive
		for _, saved := range current.Binding.Conversations {
			if saved.Address == "lead" {
				continue
			}
			if _, err := current.Binding.Resolve(saved.Address); err == nil && s.piCurrentMemberActive(data.PiTeam, saved.Address) {
				data.ReadyMembers = append(data.ReadyMembers, teamruntime.Member{Address: saved.Address})
			}
		}
		if s.piWorkflows != nil {
			return withPiWorkflowGrant(ctx, piConversationAdmission{piSessionAdmission: current, Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: current.Authority.Generation}, BindingRevision: current.Binding.Revision, SavedAccess: access}, "read", "workflow.read", func(ctx context.Context) error {
				_, available := s.piWorkflows.cached(ctx, ref)
				if !available {
					s.piWorkflows.requestObservation(ref, false)
				}
				return nil
			})
		}
		return nil
	})
}

var _ piWorkflowRuntime = (*piruntime.Manager)(nil)

func (s *Server) piConversationHandler(legacy http.Handler) (http.Handler, error) {
	pi, err := conversation.NewBackendHandler(conversation.BackendOptions{Resolver: conversation.BackendResolverFunc(s.resolvePiBackend), AllowedOrigins: s.trustedOrigins, BasePath: "/codex", MaxMessageBytes: session.MaxMessageBytes, Shutdown: s.stopping, Logger: s.config.Logger})
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, conversationPath := strings.CutPrefix(r.URL.EscapedPath(), "/codex/conversations/")
		if !conversationPath {
			legacy.ServeHTTP(w, r)
			return
		}
		id, _, _ := strings.Cut(path, "/")
		id, err := url.PathUnescape(id)
		slug, _, valid := parseTeamConversationID(id)
		if err != nil || !valid {
			http.NotFound(w, r)
			return
		}
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		kind, _, err := s.workflowPageBackend(summary)
		if err != nil {
			s.writeError(w, r, http.StatusConflict, "Conversation backend is unavailable.")
			return
		}
		switch kind {
		case agentbackend.Codex:
			legacy.ServeHTTP(w, r)
		case agentbackend.Pi:
			_, operation, _ := strings.Cut(path, "/")
			request, err := scopedPiConversationRequest(r, r.Method == http.MethodGet && operation == "events")
			if err != nil {
				s.writeError(w, r, http.StatusConflict, "The selected Pi conversation changed. Reload the session.")
				return
			}
			pi.ServeHTTP(w, request)
		default:
			s.writeError(w, r, http.StatusServiceUnavailable, "Conversation backend is unavailable.")
		}
	}), nil
}

func (s *Server) resolvePiBackend(ctx context.Context, request conversation.ResolveRequest) (target conversation.BackendTarget, err error) {
	slug, member, valid := parseTeamConversationID(request.ID)
	if !valid || s.config.PiCreation == nil || s.config.PiCreation.Runtime == nil {
		return target, piruntime.ErrUnavailable
	}
	if member == "" {
		member = "lead"
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		return target, err
	}
	kind, binding, err := s.workflowPageBackend(summary)
	if err != nil || kind != agentbackend.Pi {
		return target, agentbackend.ErrBindingConflict
	}
	ref, err := binding.Resolve(member)
	if err != nil {
		return target, err
	}
	var initialRead *piConversationLease
	build := func(current piSessionAdmission, lease *piConversationLease) error {
		if generation, _ := ctx.Value(piExpectedGenerationKey{}).(string); generation != "" && generation != current.Authority.Generation {
			return agentbackend.ErrStaleExecution
		}
		resolved, err := current.Binding.Resolve(member)
		if err != nil || resolved != ref {
			return agentbackend.ErrBindingConflict
		}
		if err := checkExpectedPiScope(ctx, resolved); err != nil {
			return err
		}
		handle := agentbackend.ExecutionHandle{Conversation: ref, Generation: current.Authority.Generation}
		var operations backend.Operations
		inputDelivery, readBound := false, false
		if binder, ok := s.config.PiCreation.Runtime.(interface {
			BindRead(context.Context, agentbackend.ExecutionHandle) (backend.Operations, bool, error)
		}); ok && lease == nil {
			operations, inputDelivery, err = binder.BindRead(ctx, handle)
			readBound = true
		} else {
			operations, err = s.config.PiCreation.Runtime.Bind(ctx, handle)
		}
		if err != nil || operations.Verify == nil {
			return errors.Join(piruntime.ErrUnavailable, err)
		}
		adapter := &piAdmittedConversation{server: s, handle: handle, lease: lease, scope: piWorkflowNamespace(ref), initialRead: initialRead, readVerification: initialRead != nil}
		if lease != nil && request.Method == http.MethodPost && request.Operation == "message" {
			// Reuse the operation for this invocation, not a verification result.
			// Verify still checks the worker afresh, and Send binds again.
			adapter.resolvedMessageVerifier = operations.Verify
		}
		if readBound {
			adapter.readOperations = &operations
		}
		wrapped := backend.Operations{Verify: adapter}
		if _, ok := s.config.PiCreation.Runtime.(piProviderRuntime); ok {
			wrapped.ProviderInfo = adapter
		}
		if operations.Read != nil {
			wrapped.Read = adapter
		}
		if operations.Page != nil {
			wrapped.Page = adapter
		}
		if operations.Watch != nil {
			wrapped.Watch = adapter
		}
		if operations.Activity != nil {
			wrapped.Activity = adapter
		}
		access, accessErr := s.piSavedAccessLocked(current, ref)
		effects := accessErr == nil && (access == "read_only" || access == "workspace_write")
		if effects && operations.Send != nil {
			wrapped.Send = adapter
		}
		if effects && operations.Interrupt != nil {
			wrapped.Interrupt = adapter
		}
		if effects && s.piSettingsAvailable() {
			wrapped.Settings = adapter
		}
		if provider, ok := s.config.PiCreation.Runtime.(interface {
			InputDelivery(context.Context, agentbackend.ExecutionHandle) (bool, error)
		}); ok && !readBound {
			var err error
			inputDelivery, err = provider.InputDelivery(ctx, handle)
			if err != nil {
				return err
			}
		}
		target = conversation.BackendTarget{Identity: piruntime.BackendIdentity(handle), Operations: wrapped, Capabilities: conversation.Capabilities{Settings: wrapped.Settings != nil, InputDelivery: effects && inputDelivery, Read: wrapped.Read != nil, Send: wrapped.Send != nil, Interrupt: wrapped.Interrupt != nil, EventStream: wrapped.Watch != nil}, PresentTranscript: s.presentPiTranscript}
		if s.uploadStore != nil {
			files, err := s.piSessionUploads(ctx, current, ref, !effects)
			if err != nil {
				return err
			}
			target.Attachments = files
		}
		if lease != nil {
			target.MutationLock = s.messageLock(request.ID)
			target.Release = lease.close
		} else if initialRead != nil {
			target.Release = adapter.releaseReadVerification
		}
		return nil
	}
	if request.Mutation {
		if request.Operation != "message" && request.Operation != "interrupt" && request.Operation != "uploads" && request.Operation != "settings" {
			return target, piruntime.ErrUnavailable
		}
		current, release, err := s.acquirePiSessionAdmission(ctx, ref.Session, request.Operation != "uploads")
		if err != nil {
			return target, err
		}
		admission, err := s.projectPiConversationAdmission(current, ref)
		if err != nil {
			release()
			return target, err
		}
		if err := workflowPermission(admission.SavedAccess, "conversation."+request.Operation); err != nil {
			release()
			return target, err
		}
		lease := &piConversationLease{server: s, current: admission, release: release}
		lease.active.Store(true)
		if err := build(current, lease); err != nil {
			lease.close()
			return conversation.BackendTarget{}, err
		}
		return target, nil
	}
	current, release, err := s.acquirePiSessionAdmission(ctx, ref.Session, false)
	if err != nil {
		return target, err
	}
	admission, err := s.projectPiConversationAdmission(current, ref)
	if err != nil {
		release()
		return target, err
	}
	initialRead = &piConversationLease{server: s, current: admission, release: release}
	initialRead.active.Store(true)
	if err := build(current, nil); err != nil {
		initialRead.close()
		return conversation.BackendTarget{}, err
	}
	return target, nil
}

type piConversationLease struct {
	server  *Server
	current piConversationAdmission
	active  atomic.Bool
	once    sync.Once
	release func()
}

func (l *piConversationLease) close() { l.once.Do(func() { l.active.Store(false); l.release() }) }
func (l *piConversationLease) run(ctx context.Context, kind, operation string, effect func(context.Context) error) error {
	// Only the context passed by C controls its detached receipt deadline.
	if !l.active.Load() || ctx.Err() != nil {
		return piruntime.ErrUnavailable
	}
	if err := l.server.requireCurrentHostProfile(); err != nil {
		return err
	}
	current, err := l.server.piSessionAdmissionLocked(l.current.Handle.Conversation.Session)
	if err != nil {
		return err
	}
	access, err := l.server.piSavedAccessLocked(current, l.current.Handle.Conversation)
	if err != nil || access != l.current.SavedAccess || !reflect.DeepEqual(current.Binding, l.current.Binding) || current.Authority != l.current.Authority {
		return agentbackend.ErrStaleExecution
	}
	if kind == "mutation" {
		if err := workflowPermission(access, operation); err != nil {
			return err
		}
		if l.server.piWorkflows != nil {
			reader, ok := l.server.config.PiCreation.Runtime.(piruntime.CurrentRuntimeReader)
			if !ok {
				return piruntime.ErrUnavailable
			}
			runtime, err := reader.ReadCurrentRuntime(ctx, l.current.Handle.Conversation.Session)
			if err != nil || runtime.Generation != l.current.Handle.Generation || runtime.Session != l.current.Handle.Conversation.Session {
				return errors.Join(agentbackend.ErrStaleExecution, err)
			}
			// Reloaded portals must complete their ordinary attachment ACK before a
			// native mutation. The ACK clears only the short observation map and does
			// not reacquire this invocation's session runtime lock.
			if err := l.server.piWorkflows.ensureAttachment(ctx, runtime); err != nil {
				return err
			}
		}
		l.server.piWorkflows.invalidateRef(l.current.Handle.Conversation, true)
	}
	return withPiWorkflowGrant(ctx, l.current, kind, operation, effect)
}

type piAdmittedConversation struct {
	server                  *Server
	handle                  agentbackend.ExecutionHandle
	lease                   *piConversationLease
	scope                   string
	resolvedMessageVerifier backend.Verifier
	readOperations          *backend.Operations
	verificationMu          sync.Mutex
	initialRead             *piConversationLease
	readVerification        bool
}

func (a *piAdmittedConversation) read(ctx context.Context, id backend.Identity, run func(backend.Operations) error) error {
	if id != piruntime.BackendIdentity(a.handle) || a.scope != piWorkflowNamespace(a.handle.Conversation) {
		return piruntime.ErrScope
	}
	if a.lease != nil {
		return a.lease.run(ctx, "read", "conversation.read", func(ctx context.Context) error {
			operations, err := a.server.config.PiCreation.Runtime.Bind(ctx, a.handle)
			if err != nil {
				return err
			}
			return run(operations)
		})
	}
	return a.server.withPiSessionRead(ctx, a.handle.Conversation.Session, func(current piSessionAdmission) error {
		if current.Authority.ValidateHandle(current.Binding, a.handle) != nil {
			return agentbackend.ErrStaleExecution
		}
		if a.readOperations != nil {
			return run(*a.readOperations)
		}
		operations, err := a.server.config.PiCreation.Runtime.Bind(ctx, a.handle)
		if err != nil {
			return err
		}
		return run(operations)
	})
}

// Only initial read verification owns the resolution grant. Cleanup may abandon
// it, but cannot release its locks while the bounded verifier is still running.
func (a *piAdmittedConversation) releaseReadVerification() {
	a.verificationMu.Lock()
	defer a.verificationMu.Unlock()
	if a.initialRead != nil {
		a.initialRead.close()
		a.initialRead = nil
	}
}
func (a *piAdmittedConversation) Verify(ctx context.Context, id backend.Identity) error {
	verify := func(ops backend.Operations) error {
		if ops.Verify == nil {
			return piruntime.ErrUnavailable
		}
		return ops.Verify.Verify(ctx, id)
	}
	if verifier := a.resolvedMessageVerifier; verifier != nil {
		if id != piruntime.BackendIdentity(a.handle) || a.scope != piWorkflowNamespace(a.handle.Conversation) {
			return piruntime.ErrScope
		}
		return a.lease.run(ctx, "read", "conversation.read", func(ctx context.Context) error {
			return verifier.Verify(ctx, id)
		})
	}
	if !a.readVerification {
		return a.read(ctx, id, verify)
	}
	a.verificationMu.Lock()
	defer a.verificationMu.Unlock()
	grant := a.initialRead
	a.initialRead = nil
	if grant == nil {
		return piruntime.ErrUnavailable
	}
	defer grant.close()
	if id != piruntime.BackendIdentity(a.handle) || a.scope != piWorkflowNamespace(a.handle.Conversation) {
		return piruntime.ErrScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.server.requireCurrentHostProfile(); err != nil {
		return err
	}
	current, err := a.server.piSessionAdmissionLockedContext(ctx, a.handle.Conversation.Session)
	if err != nil {
		return err
	}
	access, err := a.server.piSavedAccessLocked(current, a.handle.Conversation)
	if err != nil || access != grant.current.SavedAccess || !reflect.DeepEqual(current.Binding, grant.current.Binding) || current.Authority != grant.current.Authority || current.Authority.ValidateHandle(current.Binding, a.handle) != nil {
		return agentbackend.ErrStaleExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ops := a.readOperations
	if ops == nil {
		bound, err := a.server.config.PiCreation.Runtime.Bind(ctx, a.handle)
		if err != nil {
			return err
		}
		ops = &bound
	}
	if err := verify(*ops); err != nil {
		return err
	}
	return ctx.Err()
}
func (a *piAdmittedConversation) Read(ctx context.Context, id backend.Identity) (result backend.Transcript, err error) {
	err = a.read(ctx, id, func(ops backend.Operations) error {
		if ops.Read == nil {
			return piruntime.ErrUnavailable
		}
		var e error
		result, e = ops.Read.Read(ctx, id)
		if e == nil {
			e = a.observeAttachments(ctx, &result)
		}
		return e
	})
	return
}
func (a *piAdmittedConversation) ReadPage(ctx context.Context, id backend.Identity, cursor backend.HistoryCursor) (result backend.TranscriptPage, err error) {
	err = a.read(ctx, id, func(ops backend.Operations) error {
		if ops.Page == nil {
			return piruntime.ErrUnavailable
		}
		var e error
		result, e = ops.Page.ReadPage(ctx, id, cursor)
		if e == nil {
			e = a.observeAttachments(ctx, &result.Transcript)
		}
		return e
	})
	return
}
func (a *piAdmittedConversation) ReadActivity(ctx context.Context, id backend.Identity) (result backend.ActivitySnapshot, err error) {
	err = a.read(ctx, id, func(ops backend.Operations) error {
		if ops.Activity == nil {
			return piruntime.ErrUnavailable
		}
		var e error
		result, e = ops.Activity.ReadActivity(ctx, id)
		return e
	})
	return
}
func (a *piAdmittedConversation) Send(ctx context.Context, id backend.Identity, input backend.SendRequest) (result backend.SendResult, err error) {
	result.Outcome = backend.NotSubmitted
	if id != piruntime.BackendIdentity(a.handle) || a.server.config.PiCreation == nil || a.server.config.PiCreation.Runtime == nil {
		return result, piruntime.ErrUnavailable
	}
	if a.lease == nil {
		return result, piruntime.ErrUnavailable
	}
	err = a.lease.run(ctx, "mutation", "conversation.message", func(ctx context.Context) error {
		grant, err := workflowGrant(ctx, a.handle.Conversation, "mutation")
		if err != nil || grant.current.Handle != a.handle {
			return agentbackend.ErrStaleExecution
		}
		ops, err := a.server.config.PiCreation.Runtime.Bind(ctx, a.handle)
		if err != nil || ops.Send == nil {
			return errors.Join(piruntime.ErrUnavailable, err)
		}
		result, err = ops.Send.Send(ctx, id, input)
		return err
	})
	return
}
func (a *piAdmittedConversation) Interrupt(ctx context.Context, id backend.Identity) error {
	if id != piruntime.BackendIdentity(a.handle) || a.server.config.PiCreation == nil || a.server.config.PiCreation.Runtime == nil {
		return piruntime.ErrUnavailable
	}
	if a.lease == nil {
		return piruntime.ErrUnavailable
	}
	return a.lease.run(ctx, "mutation", "conversation.interrupt", func(ctx context.Context) error {
		grant, err := workflowGrant(ctx, a.handle.Conversation, "mutation")
		if err != nil || grant.current.Handle != a.handle {
			return agentbackend.ErrStaleExecution
		}
		ops, err := a.server.config.PiCreation.Runtime.Bind(ctx, a.handle)
		if err != nil || ops.Interrupt == nil {
			return errors.Join(piruntime.ErrUnavailable, err)
		}
		return ops.Interrupt.Interrupt(ctx, id)
	})
}
func (a *piAdmittedConversation) Subscribe(ctx context.Context, id backend.Identity) (events <-chan struct{}, release func(), err error) {
	var native backend.Watcher
	currentEvents := false
	err = a.read(ctx, id, func(ops backend.Operations) error {
		if ops.Verify == nil {
			return piruntime.ErrUnavailable
		}
		if err := ops.Verify.Verify(ctx, id); err != nil {
			return err
		}
		native = ops.Watch
		// piActivity:1 also supplies the post-append notification boundary.
		// Older workers retain their existing native polling subscription.
		currentEvents = ops.Activity != nil
		return nil
	})
	if err != nil || native == nil {
		return nil, nil, errors.Join(piruntime.ErrUnavailable, err)
	}
	// No runtime/transition lock survives for the event-stream lifetime.
	if a.server.piWorkflows != nil {
		return (piWorkflowWatch{owner: a.server.piWorkflows, ref: a.handle.Conversation, native: native, currentEvents: currentEvents}).Subscribe(ctx, id)
	}
	if currentEvents {
		return nil, nil, piruntime.ErrUnavailable
	}
	return native.Subscribe(ctx, id)
}

// Pi uses the portal's existing Markdown renderer and sanitizer. Text remains
// the canonical native input/output, independently of presentation metadata.
func (s *Server) presentPiTranscript(transcript *backend.Transcript) {
	for i := range transcript.Entries {
		entry := &transcript.Entries[i]
		if entry.Kind == "userMessage" && entry.Communication == nil {
			if kind, from, to := nativeMessageCommunication(entry.Text); kind != "" {
				entry.Communication = &backend.TranscriptCommunication{Kind: kind, From: from, To: to}
			}
		}
		switch entry.Kind {
		case "userMessage", "agentMessage", "reasoning", "plan":
			text := entry.Text
			if entry.Kind == "userMessage" && entry.DisplayText != nil {
				text = *entry.DisplayText
			}
			entry.HTML = string(s.renderTextMarkdown(text))
		}
	}
}

func (a *piAdmittedConversation) observeAttachments(ctx context.Context, transcript *backend.Transcript) error {
	if a.server.uploadStore == nil {
		return nil
	}
	return a.server.withPiSessionRead(ctx, a.handle.Conversation.Session, func(current piSessionAdmission) error {
		if current.Authority.ValidateHandle(current.Binding, a.handle) != nil {
			return agentbackend.ErrStaleExecution
		}
		access, err := a.server.piSavedAccessLocked(current, a.handle.Conversation)
		if err != nil {
			return err
		}
		files, err := a.server.piSessionUploads(ctx, current, a.handle.Conversation, workflowPermission(access, "conversation.uploads") != nil)
		if err != nil {
			return err
		}
		return files.ObserveNeutralTranscript(ctx, transcript)
	})
}

func piUploadThread(ref agentbackend.ConversationRef) string { return "pi:" + piWorkflowNamespace(ref) }
func (s *Server) piSessionUploads(ctx context.Context, current piSessionAdmission, ref agentbackend.ConversationRef, readOnly bool) (*uploads.Backend, error) {
	if current.Binding.SessionIncarnation != ref.Session.Incarnation {
		return nil, agentbackend.ErrBindingConflict
	}
	epoch, err := session.CompletedRemovalHistory(s.config.Workspace, ref.Session.Slug, s.config.UserStateRoot)
	if err != nil {
		return nil, err
	}
	thread := piUploadThread(ref)
	if ref.Member == "lead" {
		if err := s.uploadStore.AdoptInitial(ctx, current.intent.GoalSHA256, ref.Session.Slug, thread, epoch); err != nil {
			return nil, err
		}
	}
	scope, err := s.uploadStore.SessionScope(ctx, ref.Session.Slug, thread, epoch)
	if err != nil {
		return nil, err
	}
	id := ref.Session.Slug
	if ref.Member != "lead" {
		id += "~" + ref.Member
	}
	return &uploads.Backend{Store: s.uploadStore, ScopeID: scope.ID, BaseURL: "/uploads/s-" + id, ReadOnly: readOnly, InitialInputID: current.intent.InitialInputID, ConversationScope: piWorkflowNamespace(ref), CheckIdle: s.uploadReferencesIdle, LockReferences: s.lockUploadReferences}, nil
}

func (s *Server) piCurrentMemberActive(roster *teamruntime.PiRoster, address string) bool {
	if roster == nil {
		return false
	}
	for _, m := range roster.Members {
		if m.Address == address {
			return m.State == "ready"
		}
	}
	return false
}
