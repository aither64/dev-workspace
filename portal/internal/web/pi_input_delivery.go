package web

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime/trace"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/inputdelivery"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// This gate covers the retained SQLite connection's complete lifetime, including
// reconnect failure and final checkpoint. It intentionally checks no runtime.
func (s *Server) inputDeliveryDatabaseGate(ctx context.Context, run func() error) error {
	_, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer release()
	return run()
}
func (s *Server) initInputDelivery(ctx context.Context) error {
	if s.config.PiCreation == nil || s.config.UserStateRoot == "" {
		return nil
	}
	if _, ok := s.config.PiCreation.Runtime.(piruntime.QueuedInputController); !ok {
		return nil
	}
	store, err := inputdelivery.Open(ctx, s.config.UserStateRoot, s.config.Workspace, s.inputDeliveryDatabaseGate)
	if err != nil {
		return err
	}
	s.inputDeliveryStore = store
	s.inputDispatcher = inputdelivery.Start(s.operationContext, store, piDeliveryDriver{s})
	return nil
}
func (s *Server) closeInputDelivery() {
	if s.inputDispatcher != nil {
		s.inputDispatcher.Close()
	}
	if s.inputDeliveryStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := s.inputDeliveryStore.Close(ctx); err != nil {
			s.config.Logger.Printf("portal input delivery database terminal close: %v", err)
		}
	}
}

type piLocalDeliveries struct {
	server   *Server
	ref      agentbackend.ConversationRef
	identity backend.Identity
}

func (s *Server) resolvePiDelivery(ctx context.Context, request conversation.ResolveRequest) (target conversation.BackendTarget, resultErr error) {
	defer trace.StartRegion(ctx, "pi.delivery.resolve").End()
	defer func() { trace.Logf(ctx, "pi.delivery.resolve.outcome", "error=%t", resultErr != nil) }()
	slug, member, ok := parseTeamConversationID(request.ID)
	if !ok {
		return conversation.BackendTarget{}, piruntime.ErrScope
	}
	if member == "" {
		member = "lead"
	}
	transition := trace.StartRegion(ctx, "pi.delivery.resolve.transition")
	file, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	transition.End()
	if err != nil {
		return conversation.BackendTarget{}, err
	}
	invocation := s.piPackageInvocation(file)
	fail := func(err error) (conversation.BackendTarget, error) {
		invocation.expire()
		release()
		return conversation.BackendTarget{}, err
	}
	if err = invocation.recheck(); err != nil {
		return fail(err)
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(slug)
	if err != nil {
		return fail(err)
	}
	ref, err := binding.Resolve(member)
	if err != nil || checkExpectedPiScope(ctx, ref) != nil {
		return fail(agentbackend.ErrBindingConflict)
	}
	ready := trace.StartRegion(ctx, "pi.delivery.resolve.ready_join")
	current, err := s.piReadySessionIdentityLocked(ref.Session)
	ready.End()
	if err != nil {
		return fail(err)
	}
	admission, err := s.projectPiConversationAdmission(current, ref)
	if err != nil || workflowPermission(admission.SavedAccess, "conversation.message") != nil {
		return fail(piruntime.ErrScope)
	}
	// Browser generation is an observation, not the stable inbox namespace.
	// Its syntax is checked by scopedPiConversationRequest. New input is bound
	// to the fresh selection; old IDs retain their original immutable request.
	identity := piruntime.BackendIdentity(admission.Handle)
	identity.ScopeKey = piWorkflowNamespace(ref)
	provider := &piLocalDeliveries{server: s, ref: ref, identity: identity}
	return conversation.BackendTarget{Identity: identity, Operations: backend.Operations{Verify: provider, Deliveries: provider}, Capabilities: conversation.Capabilities{DurableDelivery: s.inputDeliveryStore != nil},
		MutationLock: conversation.NewMutationLock(), Release: func() { invocation.expire(); release() }}, nil
}

func (p *piLocalDeliveries) local(ctx context.Context) (piConversationAdmission, error) {
	defer trace.StartRegion(ctx, "pi.delivery.local_join").End()
	current, err := p.server.piReadySessionIdentityLocked(p.ref.Session)
	if err != nil {
		return piConversationAdmission{}, err
	}
	return p.server.projectPiConversationAdmission(current, p.ref)
}
func (p *piLocalDeliveries) Verify(ctx context.Context, id backend.Identity) error {
	_, err := p.verifyCurrent(ctx, id)
	return err
}
func (p *piLocalDeliveries) verifyCurrent(ctx context.Context, id backend.Identity) (piConversationAdmission, error) {
	defer trace.StartRegion(ctx, "pi.delivery.verify").End()
	if id != p.identity {
		return piConversationAdmission{}, piruntime.ErrScope
	}
	if err := p.server.requireCurrentHostProfile(); err != nil {
		return piConversationAdmission{}, err
	}
	current, err := p.local(ctx)
	if err != nil {
		return piConversationAdmission{}, err
	}
	if current.Handle.Generation != id.Generation {
		return piConversationAdmission{}, agentbackend.ErrStaleExecution
	}
	if err := workflowPermission(current.SavedAccess, "conversation.message"); err != nil {
		return piConversationAdmission{}, err
	}
	return current, nil
}
func portalSelection(current piConversationAdmission, selected piruntime.InputSelection) inputdelivery.Selection {
	return inputdelivery.Selection{Runtime: selected.Runtime, Handle: selected.Handle, Settings: selected.Settings, Policy: selected.Policy, InstructionsDigest: selected.InstructionsDigest, Access: current.SavedAccess, BindingRevision: current.Binding.Revision}
}
func usableInputSelection(selected piruntime.InputSelection) bool {
	return selected.CheckedInput && !selected.Held && !selected.PendingSettings && !selected.PendingPolicy
}
func (p *piLocalDeliveries) selected(ctx context.Context, current piConversationAdmission) (piruntime.InputSelection, error) {
	defer trace.StartRegion(ctx, "pi.delivery.selected").End()
	controller, ok := p.server.config.PiCreation.Runtime.(piruntime.QueuedInputController)
	if !ok {
		return piruntime.InputSelection{}, piruntime.ErrUnavailable
	}
	selected, err := controller.ReadInputSelection(ctx, current.Handle)
	if err == nil && (selected.Handle != current.Handle || selected.Runtime.Session != p.ref.Session || selected.Runtime.Generation != current.Handle.Generation) {
		err = agentbackend.ErrStaleExecution
	}
	return selected, err
}
func deliveryDisposition(err error) error {
	var known *backend.DeliveryError
	if errors.As(err, &known) {
		return known
	}
	switch {
	case errors.Is(err, inputdelivery.ErrConflict):
		return &backend.DeliveryError{Code: "delivery_conflict"}
	case errors.Is(err, inputdelivery.ErrCapacity):
		return &backend.DeliveryError{Code: "delivery_capacity"}
	case errors.Is(err, inputdelivery.ErrNotFound):
		return &backend.DeliveryError{Code: "delivery_not_found"}
	default:
		return &backend.DeliveryError{Code: "delivery_unavailable"}
	}
}
func (p *piLocalDeliveries) Deliveries(ctx context.Context, id backend.Identity) (backend.DeliveryCollection, error) {
	if err := p.Verify(ctx, id); err != nil {
		return backend.DeliveryCollection{}, err
	}
	rows, err := p.server.inputDeliveryStore.List(ctx, id.ScopeKey)
	if err != nil {
		return backend.DeliveryCollection{}, deliveryDisposition(err)
	}
	result := backend.DeliveryCollection{Version: 1, Scope: id.ScopeKey, DeliveryModes: []string{}, Entries: []backend.DeliveryStatus{}}
	for _, r := range rows {
		result.Entries = append(result.Entries, r.Status)
	}
	current, err := p.local(ctx)
	if err != nil {
		return result, nil
	}
	selected, err := p.selected(ctx, current)
	if err != nil {
		return result, nil
	}
	result.Selection = inputdelivery.Digest(portalSelection(current, selected))
	result.Settings = backend.Settings{Model: selected.Settings.Model, ModelProvider: selected.Settings.Provider, ReasoningEffort: selected.Settings.Thinking}
	if selected.Policy != nil {
		result.Settings.CollaborationMode = selected.Policy.Mode
	}
	if selected.CheckedInput {
		result.DeliveryModes = []string{"steer", "followUp"}
	}
	result.CanEnqueue = usableInputSelection(selected)
	return result, nil
}
func (p *piLocalDeliveries) Delivery(ctx context.Context, id backend.Identity, clientID string) (backend.DeliveryStatus, error) {
	if err := p.Verify(ctx, id); err != nil {
		return backend.DeliveryStatus{}, err
	}
	row, err := p.server.inputDeliveryStore.Get(ctx, id.ScopeKey, clientID)
	if err != nil {
		return backend.DeliveryStatus{}, deliveryDisposition(err)
	}
	return row.Status, nil
}
func (p *piLocalDeliveries) AdmitDelivery(ctx context.Context, id backend.Identity, request backend.DeliveryRequest) (status backend.DeliveryStatus, resultErr error) {
	defer trace.StartRegion(ctx, "pi.delivery.admit").End()
	defer func() {
		if !trace.IsEnabled() {
			return
		}
		outcome := "ok"
		if resultErr != nil {
			outcome = "error"
			var known *backend.DeliveryError
			if errors.As(resultErr, &known) && known.Code == "delivery_conflict" {
				outcome = "conflict"
			}
		}
		trace.Logf(ctx, "pi.delivery.admit.outcome", "originalID=%s phase=%s outcome=%s", request.ClientUserMessageID, status.State, outcome)
	}()
	if id != p.identity || id.ScopeKey != inputdelivery.ConversationScope(p.ref) {
		return backend.DeliveryStatus{}, piruntime.ErrScope
	}
	if err := p.server.requireCurrentHostProfile(); err != nil {
		return backend.DeliveryStatus{}, err
	}
	store := p.server.inputDeliveryStore
	resolve := trace.StartRegion(ctx, "pi.delivery.admit.resolve")
	previous, resolveErr := store.Resolve(ctx, id.ScopeKey, request)
	resolve.End()
	// Resolve only reads private identities. Fresh authorization must win over
	// every captured outcome before any status/proof or upload/refusal effect.
	current, err := p.verifyCurrent(ctx, id)
	if err != nil {
		return backend.DeliveryStatus{}, err
	}
	if resolveErr == nil {
		if !inputdelivery.SameRequest(previous.Admission.Request, request) {
			return backend.DeliveryStatus{}, &backend.DeliveryError{Code: "delivery_conflict"}
		}
		return previous.Status, nil
	} else if !errors.Is(resolveErr, inputdelivery.ErrNotFound) {
		return backend.DeliveryStatus{}, deliveryDisposition(resolveErr)
	}
	selected, err := p.selected(ctx, current)
	if err != nil || !usableInputSelection(selected) {
		return backend.DeliveryStatus{}, &backend.DeliveryError{Code: "delivery_unavailable"}
	}
	selection := portalSelection(current, selected)
	if inputdelivery.Digest(selection) != request.Selection {
		return p.reject(ctx, id, request, "delivery_selection_changed")
	}
	if request.Delivery != "steer" && request.Delivery != "followUp" {
		return p.reject(ctx, id, request, "delivery_invalid")
	}
	wire, uploadScope := request.Message, ""
	if p.server.uploadStore != nil {
		uploads := trace.StartRegion(ctx, "pi.delivery.admit.uploads")
		files, e := p.server.piSessionUploads(ctx, current.piSessionAdmission, p.ref, false)
		if e != nil {
			uploads.End()
			return backend.DeliveryStatus{}, e
		}
		wire, e = files.Prepare(ctx, "send", request.ClientUserMessageID, request.Message, request.AttachmentIDs)
		uploads.End()
		if e != nil {
			return backend.DeliveryStatus{}, e
		}
		uploadScope = files.ScopeID
	} else if len(request.AttachmentIDs) != 0 {
		return p.reject(ctx, id, request, "delivery_invalid")
	}
	latest, err := p.local(ctx)
	if err != nil {
		return backend.DeliveryStatus{}, err
	}
	fresh, err := p.selected(ctx, latest)
	if err != nil {
		return backend.DeliveryStatus{}, deliveryDisposition(err)
	}
	if !usableInputSelection(fresh) || !reflect.DeepEqual(selection, portalSelection(latest, fresh)) {
		return p.reject(ctx, id, request, "delivery_selection_changed")
	}
	input := piruntime.SendInput{Message: wire, ActionContext: "pi-delivery-v1:" + request.Delivery}
	ref := piruntime.SubmissionRef{Conversation: p.ref, OperationID: request.ClientUserMessageID, Action: "send"}
	ref.Digest = piruntime.SubmissionDigest(ref, input, selection.Settings)
	admit := trace.StartRegion(ctx, "pi.delivery.admit.store")
	row, err := store.Admit(ctx, inputdelivery.Admission{Version: 1, Scope: id.ScopeKey, Request: request, Wire: wire, UploadScope: uploadScope, Selection: selection, Ref: ref, Input: input})
	admit.End()
	if err != nil {
		return backend.DeliveryStatus{}, deliveryDisposition(err)
	}
	if p.server.inputDispatcher != nil {
		p.server.inputDispatcher.Wake()
	}
	return row.Status, nil
}

func (p *piLocalDeliveries) reject(ctx context.Context, id backend.Identity, request backend.DeliveryRequest, code string) (backend.DeliveryStatus, error) {
	// Scope/auth and syntax have already passed. The same SQLite transaction
	// resolves a concurrent successful admission before committing a refusal.
	row, err := p.server.inputDeliveryStore.Reject(ctx, id.ScopeKey, p.ref, request, code)
	if err != nil {
		return backend.DeliveryStatus{}, deliveryDisposition(err)
	}
	return row.Status, nil
}

type piDeliveryDriver struct{ server *Server }

func sameLogicalDelivery(original, fresh inputdelivery.Selection) bool {
	return original.Handle.Conversation == fresh.Handle.Conversation && original.Access == fresh.Access &&
		original.Settings == fresh.Settings && original.InstructionsDigest == fresh.InstructionsDigest && reflect.DeepEqual(original.Policy, fresh.Policy)
}
func (d piDeliveryDriver) Dispatch(ctx context.Context, row inputdelivery.Record, claim func(inputdelivery.ExecutionEvidence) error) (result inputdelivery.Result, err error) {
	// NewTask allocates even with tracing disabled; only diagnostic runs need
	// a detached dispatcher task linked by its immutable original ID.
	if trace.IsEnabled() {
		var task *trace.Task
		ctx, task = trace.NewTask(ctx, "pi.delivery.dispatch")
		defer task.End()
		trace.Log(ctx, "originalID", row.Admission.Ref.OperationID)
	}
	defer trace.StartRegion(ctx, "pi.delivery.dispatch").End()
	defer func() { trace.Logf(ctx, "pi.delivery.dispatch.outcome", "phase=%s error=%t", result.State, err != nil) }()
	s, a := d.server, row.Admission
	if summary, e := session.Find(a.Ref.Conversation.Session.Workspace, a.Ref.Conversation.Session.Slug); e != nil && !errors.Is(e, os.ErrNotExist) {
		return result, e
	} else if e != nil || summary.Archived {
		return result, &inputdelivery.PreDispatchError{Terminal: true, Reason: "The selected session is no longer active.", Err: e}
	}
	if owner, e := session.PendingLifecycle(a.Ref.Conversation.Session.Workspace, a.Ref.Conversation.Session.Slug); e != nil {
		return result, e
	} else if owner != "" {
		return result, &inputdelivery.PreDispatchError{Terminal: true, Reason: "The selected session is changing lifecycle state."}
	}
	foregroundWait := trace.StartRegion(ctx, "pi.delivery.dispatch.foreground")
	foreground, err := s.acquirePiForeground(ctx, a.Ref.Conversation.Session)
	foregroundWait.End()
	if err != nil {
		return result, err
	}
	defer foreground()
	// This aggregate includes runtime-lock waiting and the owning local proof;
	// its end is not an exact runtime-lock grant timestamp.
	admissionWait := trace.StartRegion(ctx, "pi.delivery.dispatch.session_admission")
	current, release, err := s.acquirePiSessionAdmission(ctx, a.Ref.Conversation.Session, true)
	admissionWait.End()
	if err != nil {
		if errors.Is(err, agentbackend.ErrBindingConflict) {
			return result, &inputdelivery.PreDispatchError{Terminal: true, Reason: "The conversation changed. Check the selection before sending a new message.", Err: err}
		}
		return result, err
	}
	defer release()
	admission, err := s.projectPiConversationAdmission(current, a.Ref.Conversation)
	if err != nil {
		return result, &inputdelivery.PreDispatchError{Terminal: true, Reason: "The conversation changed. Check the selection before sending a new message.", Err: err}
	}
	controller, ok := s.config.PiCreation.Runtime.(piruntime.QueuedInputController)
	if !ok {
		return result, &inputdelivery.PreDispatchError{Terminal: true, Reason: "Saved delivery is unavailable for this conversation."}
	}
	selection := trace.StartRegion(ctx, "pi.delivery.dispatch.selected")
	selected, err := controller.ReadInputSelection(ctx, admission.Handle)
	selection.End()
	if err != nil {
		return result, err
	}
	if !usableInputSelection(selected) || !sameLogicalDelivery(a.Selection, portalSelection(admission, selected)) {
		return result, &inputdelivery.PreDispatchError{Terminal: true, Reason: "The conversation, access or settings changed. Review the selection before sending a new message."}
	}
	continuation := trace.StartRegion(ctx, "pi.delivery.dispatch.continuation")
	err = controller.ValidateInputRuntimeContinuation(ctx, a.Selection.Runtime, selected.Runtime)
	continuation.End()
	if err != nil {
		return result, err
	}
	lease := &piConversationLease{server: s, current: admission, release: func() {}}
	lease.active.Store(true)
	defer lease.close()
	leaseRun := trace.StartRegion(ctx, "pi.delivery.dispatch.lease")
	err = lease.run(ctx, "mutation", "conversation.message", func(ctx context.Context) error {
		defer trace.StartRegion(ctx, "pi.delivery.dispatch.effect").End()
		conversationID := a.Ref.Conversation.Session.Slug
		if a.Ref.Conversation.Member != "lead" {
			conversationID += "~" + a.Ref.Conversation.Member
		}
		lock := s.messageLock(conversationID)
		messageWait := trace.StartRegion(ctx, "pi.delivery.dispatch.message_lock")
		lockErr := lock.Lock(ctx)
		messageWait.End()
		if lockErr != nil {
			return lockErr
		}
		defer lock.Unlock()
		verify := trace.StartRegion(ctx, "pi.delivery.dispatch.bind_verify")
		ops, e := s.config.PiCreation.Runtime.Bind(ctx, admission.Handle)
		if e != nil || ops.Verify == nil {
			verify.End()
			return errors.Join(piruntime.ErrUnavailable, e)
		}
		e = ops.Verify.Verify(ctx, piruntime.BackendIdentity(admission.Handle))
		verify.End()
		if e != nil {
			return e
		}
		if s.uploadStore != nil {
			uploads := trace.StartRegion(ctx, "pi.delivery.dispatch.uploads")
			files, e := s.piSessionUploads(ctx, current, a.Ref.Conversation, false)
			if e != nil {
				uploads.End()
				return e
			}
			if files.ScopeID != a.UploadScope {
				uploads.End()
				return &inputdelivery.PreDispatchError{Terminal: true, Reason: "The file selection changed. Check the files before sending a new message."}
			}
			wire, e := files.Prepare(ctx, "send", a.Ref.OperationID, a.Request.Message, a.Request.AttachmentIDs)
			uploads.End()
			if e != nil || wire != a.Wire {
				return &inputdelivery.PreDispatchError{Terminal: true, Reason: "The saved files are unavailable. Check the files before sending a new message.", Err: e}
			}
		}
		claimRun := trace.StartRegion(ctx, "pi.delivery.dispatch.claim")
		e = claim(inputdelivery.ExecutionEvidence{Runtime: selected.Runtime, Handle: selected.Handle})
		claimRun.End()
		if e != nil {
			return e
		}
		checked := trace.StartRegion(ctx, "pi.delivery.dispatch.checked_input")
		native, e := controller.DispatchQueuedInput(ctx, piruntime.QueuedInputRequest{OriginalRuntime: a.Selection.Runtime, Selection: selected, Ref: a.Ref, Input: a.Input, Delivery: a.Request.Delivery})
		checked.End()
		if native.NotSubmitted {
			result = inputdelivery.Result{State: "not_sent", Failure: queuedRefusalMessage(native.Refusal)}
			return nil
		}
		if e != nil {
			return e
		}
		result, e = queueNativeResult(a, native.Status)
		return e
	})
	leaseRun.End()
	return result, err
}
func queuedRefusalMessage(reason string) string {
	switch reason {
	case "Settings or mode changed. Review them before sending a new message.",
		"The worker could not be verified. Check the conversation before sending a new message.",
		"The worker could not accept this message. Check the conversation before sending a new message.":
		return reason
	}
	return "The worker could not accept this message. Check the conversation before sending a new message."
}
func queueNativeResult(a inputdelivery.Admission, status piruntime.SubmissionStatus) (inputdelivery.Result, error) {
	if status.Phase == "Unknown" && status.Schema == 0 {
		return inputdelivery.Result{State: "unknown"}, nil
	}
	if status.Ref != a.Ref || status.Input != a.Input || status.Settings != a.Selection.Settings || piruntime.SubmissionDigest(status.Ref, status.Input, status.Settings) != a.Ref.Digest {
		return inputdelivery.Result{}, piruntime.ErrScope
	}
	switch status.Phase {
	case "Accepted":
		if status.Witness == nil || status.Witness.SessionID != a.Ref.Conversation.NativeID || status.Witness.EntryID == "" || status.Witness.EffectiveDigest != piruntime.MessageDigest(a.Wire) {
			return inputdelivery.Result{}, piruntime.ErrScope
		}
		return inputdelivery.Result{State: "sent", Receipt: &backend.SendReceipt{ClientUserMessageID: a.Ref.OperationID, TurnID: status.Witness.EntryID}}, nil
	case "Pending":
		return inputdelivery.Result{State: "pending"}, nil
	case "NotSubmitted":
		if status.Refusal != "" {
			return inputdelivery.Result{State: "not_sent", Failure: "The runtime did not accept this message."}, nil
		}
	}
	return inputdelivery.Result{State: "unknown"}, nil
}
func (d piDeliveryDriver) Lookup(ctx context.Context, row inputdelivery.Record) (output inputdelivery.Result, resultErr error) {
	if trace.IsEnabled() {
		var task *trace.Task
		ctx, task = trace.NewTask(ctx, "pi.delivery.lookup")
		defer task.End()
		trace.Log(ctx, "originalID", row.Admission.Ref.OperationID)
	}
	defer trace.StartRegion(ctx, "pi.delivery.lookup").End()
	defer func() {
		trace.Logf(ctx, "pi.delivery.lookup.outcome", "phase=%s error=%t", output.State, resultErr != nil)
	}()
	controller, ok := d.server.config.PiCreation.Runtime.(piruntime.QueuedInputController)
	if !ok {
		return inputdelivery.Result{}, piruntime.ErrUnavailable
	}
	// Only the saved full reference is used. A current generation or selection
	// cannot create a second input, and a missing receipt remains Unknown.
	lookup, err := controller.LookupQueuedInput(ctx, row.Admission.Ref)
	if err != nil {
		return inputdelivery.Result{}, err
	}
	result, err := queueNativeResult(row.Admission, lookup.Status)
	if lookup.RetiredUnknown {
		if err != nil || lookup.Status.Schema == 0 || lookup.Status.Phase != "Unknown" || lookup.Status.Witness != nil || result.State != "unknown" || lookup.Status.Ref != row.Admission.Ref {
			return inputdelivery.Result{}, piruntime.ErrScope
		}
		result.RetiredUnknown = true
	}
	return result, err
}
