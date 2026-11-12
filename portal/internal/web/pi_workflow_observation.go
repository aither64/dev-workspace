package web

import (
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"sync"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piWorkflowSnapshot struct {
	current      piConversationAdmission
	counter      uint64
	observation  piruntime.WorkflowObservation
	questions    []piquestions.QuestionRecord
	questionView []piquestions.QuestionObservation
	proposal     *piplan.Proposal
	policy       piplan.PolicyObservation
}

type piWorkflowAttachment struct {
	runtime     piruntime.RuntimeRef
	attachment  string
	sequence    uint64
	initial     uint64
	connected   bool
	lost        bool
	ready       chan struct{}
	finished    chan struct{}
	readyClosed bool
	err         error
	cancel      context.CancelFunc
}

type piWorkflowObservations struct {
	mu          sync.Mutex
	entries     map[agentbackend.ConversationRef]piWorkflowSnapshot
	counters    map[agentbackend.ConversationRef]uint64
	requested   map[agentbackend.ConversationRef]bool
	listeners   map[agentbackend.ConversationRef]map[chan struct{}]struct{}
	attachments map[agentbackend.SessionRef]*piWorkflowAttachment
}

func (o *piWorkflowObservations) initialize() {
	o.entries = map[agentbackend.ConversationRef]piWorkflowSnapshot{}
	o.counters = map[agentbackend.ConversationRef]uint64{}
	o.requested = map[agentbackend.ConversationRef]bool{}
	o.listeners = map[agentbackend.ConversationRef]map[chan struct{}]struct{}{}
	o.attachments = map[agentbackend.SessionRef]*piWorkflowAttachment{}
}

func (o *piWorkflowObservations) invalidateLocked(ref agentbackend.ConversationRef, demand bool) {
	if o.counters[ref] != math.MaxUint64 {
		o.counters[ref]++
	}
	delete(o.entries, ref)
	if demand {
		if _, present := o.requested[ref]; !present {
			o.requested[ref] = false
		}
	}
	for channel := range o.listeners[ref] {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

func (w *piWorkflowComposition) wake() {
	select {
	case w.server.displayRefreshWake <- struct{}{}:
	default:
	}
}

func (w *piWorkflowComposition) invalidateRef(ref agentbackend.ConversationRef, demand bool) {
	if w == nil {
		return
	}
	w.observations.mu.Lock()
	w.observations.invalidateLocked(ref, demand)
	w.observations.mu.Unlock()
	w.wake()
}

func (w *piWorkflowComposition) invalidateSession(ref agentbackend.SessionRef) {
	if w == nil {
		return
	}
	w.observations.mu.Lock()
	for conversation := range w.observations.counters {
		if conversation.Session == ref {
			w.observations.invalidateLocked(conversation, true)
		}
	}
	w.observations.mu.Unlock()
	w.wake()
}

// This callback is also the pre-effect ACK path. It only clears/signals the
// short host map, with no authority, domain, native, conversation or receipt lock.
func (w *piWorkflowComposition) invalidateRuntime(attempt *piWorkflowAttachment, event piruntime.WorkflowInvalidation) {
	o := &w.observations
	o.mu.Lock()
	if o.attachments[event.Runtime.Session] != attempt || event.Runtime != attempt.runtime || (attempt.attachment != "" && event.AttachmentID != attempt.attachment) {
		o.mu.Unlock()
		return
	}
	if event.Connected {
		if attempt.lost || event.AttachmentID == "" || event.Sequence == 0 || event.Sequence <= attempt.sequence {
			o.mu.Unlock()
			return
		}
		if attempt.attachment == "" {
			attempt.attachment, attempt.initial = event.AttachmentID, event.Sequence
		}
		attempt.sequence, attempt.connected = event.Sequence, true
		// P3 emits this later event only after its initial ACK became ready.
		if !attempt.readyClosed && event.Sequence > attempt.initial {
			attempt.readyClosed = true
			close(attempt.ready)
		}
	} else {
		attempt.connected = false
		attempt.lost = true
	}
	for ref := range o.counters {
		if ref.Session == event.Runtime.Session {
			o.invalidateLocked(ref, event.Connected)
			if event.Connected {
				o.requested[ref] = true
			}
		}
	}
	o.mu.Unlock()
	w.wake()
}

func (w *piWorkflowComposition) requestObservation(ref agentbackend.ConversationRef, drain bool) {
	w.observations.mu.Lock()
	if _, present := w.observations.counters[ref]; !present {
		w.observations.counters[ref] = 0
	}
	w.observations.requested[ref] = w.observations.requested[ref] || drain
	w.observations.mu.Unlock()
	w.wake()
}

func (w *piWorkflowComposition) cached(ctx context.Context, ref agentbackend.ConversationRef) (piWorkflowSnapshot, bool) {
	grant, err := workflowGrant(ctx, ref, "read", "mutation")
	if err != nil {
		return piWorkflowSnapshot{}, false
	}
	reader, ok := w.runtime.(piruntime.CurrentRuntimeReader)
	if !ok {
		return piWorkflowSnapshot{}, false
	}
	runtime, err := reader.ReadCurrentRuntime(ctx, ref.Session)
	if err != nil {
		return piWorkflowSnapshot{}, false
	}
	o := &w.observations
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, exists := o.entries[ref]
	attempt := o.attachments[ref.Session]
	valid := exists && entry.counter != math.MaxUint64 && entry.counter == o.counters[ref] && entry.current.Handle == grant.current.Handle && entry.current.Authority == grant.current.Authority && reflect.DeepEqual(entry.current.Binding, grant.current.Binding) && entry.current.SavedAccess == grant.current.SavedAccess && attempt != nil && attempt.runtime == runtime && attempt.connected && attempt.readyClosed && entry.observation.AttachmentID == attempt.attachment && entry.observation.Sequence == attempt.sequence
	return entry, valid
}

func cloneWorkflowPolicy(policy piruntime.ToolPolicyTarget) piruntime.ToolPolicyTarget {
	policy.Active, policy.RegisteredCeiling = slices.Clone(policy.Active), slices.Clone(policy.RegisteredCeiling)
	return policy
}

func (w *piWorkflowComposition) policyObservation(ctx context.Context, ref agentbackend.ConversationRef) (piplan.PolicyObservation, error) {
	entry, valid := w.cached(ctx, ref)
	if !valid {
		w.requestObservation(ref, false)
		return piplan.PolicyObservation{}, piquestions.ErrUnavailable
	}
	value := entry.policy
	value.Retained = cloneWorkflowPolicy(value.Retained)
	value.Proof.Policy = cloneWorkflowPolicy(value.Proof.Policy)
	value.Proof.Active, value.Proof.Callable, value.Proof.Registered = slices.Clone(value.Proof.Active), slices.Clone(value.Proof.Callable), slices.Clone(value.Proof.Registered)
	value.BoundHostTools = slices.Clone(value.BoundHostTools)
	if value.Proposal != nil {
		proposal := *value.Proposal
		value.Proposal = &proposal
	}
	return value, nil
}

func (w *piWorkflowComposition) questionObservations(ctx context.Context, ref agentbackend.ConversationRef) ([]piquestions.QuestionObservation, error) {
	entry, valid := w.cached(ctx, ref)
	if !valid {
		w.requestObservation(ref, false)
		return nil, piquestions.ErrUnavailable
	}
	values := slices.Clone(entry.questionView)
	for i := range values {
		values[i].Origin.Arguments = slices.Clone(values[i].Origin.Arguments)
	}
	return values, nil
}

type piWorkflowWatch struct {
	owner  *piWorkflowComposition
	ref    agentbackend.ConversationRef
	native backend.Watcher
}

func (w piWorkflowWatch) Subscribe(ctx context.Context, identity backend.Identity) (<-chan struct{}, func(), error) {
	if w.owner == nil || w.native == nil || identity.ConversationID != w.ref.NativeID {
		return nil, nil, piruntime.ErrUnavailable
	}
	native, release, err := w.native.Subscribe(ctx, identity)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	local, merged := make(chan struct{}, 1), make(chan struct{}, 1)
	o := &w.owner.observations
	o.mu.Lock()
	if o.listeners[w.ref] == nil {
		o.listeners[w.ref] = map[chan struct{}]struct{}{}
	}
	o.listeners[w.ref][local] = struct{}{}
	o.mu.Unlock()
	var once sync.Once
	cleanup := func() {
		once.Do(func() { cancel(); release(); o.mu.Lock(); delete(o.listeners[w.ref], local); o.mu.Unlock() })
	}
	go func() {
		defer close(merged)
		defer cleanup()
		for {
			select {
			case <-ctx.Done():
				return
			case _, open := <-native:
				if !open {
					return
				}
			case <-local:
			}
			select {
			case merged <- struct{}{}:
			default:
			}
		}
	}()
	return merged, cleanup, nil
}

var _ backend.Watcher = piWorkflowWatch{}

func (w *piWorkflowComposition) freshPolicy(ctx context.Context, handle agentbackend.ExecutionHandle, idle bool) (piruntime.WorkflowObservation, error) {
	grant, err := workflowGrant(ctx, handle.Conversation, "read", "mutation")
	if err != nil || grant.current.Handle != handle {
		return piruntime.WorkflowObservation{}, piruntime.ErrScope
	}
	observed, err := w.runtime.ObserveWorkflow(ctx, handle)
	if err != nil {
		return observed, err
	}
	if observed.Handle != handle || observed.Retained == nil || !observed.Loaded || observed.Pending || observed.Held || (idle && !observed.Idle) || piruntime.ValidateToolPolicyProof(observed.Proof, *observed.Retained, handle.Generation) != nil || !slices.Equal(observed.HostTools, w.tools) {
		return observed, piruntime.ErrHeld
	}
	target, err := composePiWorkflowPolicy(grant.current.SavedAccess, observed.Retained.Mode, observed.Retained.Revision, w.tools)
	if err != nil || target.Digest != observed.Retained.Digest || !slices.Equal(target.RegisteredCeiling, observed.Retained.RegisteredCeiling) {
		return observed, piruntime.ErrHeld
	}
	w.observations.mu.Lock()
	attempt := w.observations.attachments[handle.Conversation.Session]
	var attached piruntime.RuntimeRef
	if attempt != nil {
		attached = attempt.runtime
	}
	available := attempt != nil && attempt.connected && attempt.readyClosed && attempt.runtime.Generation == handle.Generation && attempt.attachment == observed.AttachmentID && attempt.sequence == observed.Sequence
	w.observations.mu.Unlock()
	if !available {
		return observed, piruntime.ErrUnavailable
	}
	reader, ok := w.runtime.(piruntime.CurrentRuntimeReader)
	if !ok {
		return observed, piruntime.ErrUnavailable
	}
	current, err := reader.ReadCurrentRuntime(ctx, handle.Conversation.Session)
	if err != nil || current != attached {
		return observed, errors.Join(piruntime.ErrConflict, err)
	}
	return observed, nil
}

// Only demand and native events populate requested. The existing display worker
// calls this once per wake; its lifecycle ticker never creates workflow demand.
func (w *piWorkflowComposition) refreshRequested(ctx context.Context) {
	if w == nil {
		return
	}
	w.observations.mu.Lock()
	requested := w.observations.requested
	w.observations.requested = map[agentbackend.ConversationRef]bool{}
	w.observations.mu.Unlock()
	for ref, drain := range requested {
		if ctx.Err() != nil {
			return
		}
		w.reconcileOriginal(ctx, ref)
		if err := w.attachRetained(ctx, ref); err != nil {
			w.server.config.Logger.Printf("Pi workflow attachment unavailable for %s/%s: %v", ref.Session.Slug, ref.Member, err)
			continue
		}
		if err := w.refresh(ctx, ref); err != nil {
			w.server.config.Logger.Printf("Pi workflow observation unavailable for %s/%s: %v", ref.Session.Slug, ref.Member, err)
		} else if drain {
			// Delivery runs after shared observation ownership is released. Its own
			// fresh exclusive admission decides whether an accepted intent can run.
			w.drain(ctx, ref)
		}
	}
}

func (w *piWorkflowComposition) attachRetained(ctx context.Context, ref agentbackend.ConversationRef) error {
	reader, ok := w.runtime.(piruntime.CurrentRuntimeReader)
	if !ok {
		return piruntime.ErrUnavailable
	}
	var captured piruntime.RuntimeRef
	read := func(ctx context.Context) error {
		grant, err := workflowGrant(ctx, ref, "read")
		if err != nil {
			return err
		}
		current, err := reader.ReadCurrentRuntime(ctx, ref.Session)
		if err != nil {
			return err
		}
		if current.Session != ref.Session || current.Generation != grant.current.Authority.Generation {
			return piruntime.ErrScope
		}
		if captured != (piruntime.RuntimeRef{}) && current != captured {
			return piruntime.ErrConflict
		}
		captured = current
		return nil
	}
	if err := w.withRead(ctx, ref, read); err != nil {
		return err
	}
	// Attachment owns no authority lock for its stream lifetime.
	if err := w.ensureAttachment(ctx, captured); err != nil {
		return err
	}
	return w.withRead(ctx, ref, read)
}

func (w *piWorkflowComposition) refresh(ctx context.Context, ref agentbackend.ConversationRef) error {
	return w.withRead(ctx, ref, func(ctx context.Context) error {
		grant, err := workflowGrant(ctx, ref, "read")
		if err != nil {
			return err
		}
		o := &w.observations
		o.mu.Lock()
		counter := o.counters[ref]
		o.mu.Unlock()
		if counter == math.MaxUint64 {
			return piruntime.ErrUnavailable
		}
		questions, plans := w.stores(ref)
		groups, err := questions.Questions(ctx, ref)
		if err != nil {
			return err
		}
		proposal, err := plans.LatestProposal(ctx, ref)
		if err != nil {
			return err
		}
		// All native work is outside the domain readers' short locks.
		observed, err := w.freshPolicy(ctx, grant.current.Handle, false)
		if err != nil {
			return err
		}
		entry := piWorkflowSnapshot{current: grant.current, counter: counter, observation: observed, questions: groups, proposal: proposal}
		for _, group := range groups {
			origin := piruntime.NativeOriginRef{AnchorEntryID: group.Caller.Lineage.AnchorEntryID, SelectionToken: group.Caller.Lineage.SelectionToken, NativeToolCallID: group.Caller.NativeToolCallID, Tool: "request_user_input_async"}
			proof, err := w.runtime.InspectNativeOrigin(ctx, grant.current.Handle, origin)
			if err != nil || proof.Handle != grant.current.Handle || proof.Origin != origin || piquestions.VerifyNativeArguments(group, proof.Arguments) != nil {
				continue // This publication stays visible without a capture capability.
			}
			entry.questionView = append(entry.questionView, piquestions.QuestionObservation{Current: grant.current.Handle, BindingRevision: grant.current.BindingRevision, ObservedBindingRevision: grant.current.BindingRevision, QuestionID: group.ID, Revision: group.Revision, Caller: group.Caller, Origin: proof, CaptureAllowed: workflowPermission(grant.current.SavedAccess, "pi-questions.respond") == nil, Callbacks: 1, LinearLineage: 1, GuardQuestionsSchema: 1})
		}
		entry.policy = piplan.PolicyObservation{Current: grant.current.Handle, Observed: grant.current.Handle, CurrentBindingRevision: grant.current.BindingRevision, ObservedBindingRevision: grant.current.BindingRevision, Retained: cloneWorkflowPolicy(*observed.Retained), Proof: observed.Proof, Pending: observed.Pending, Held: observed.Held, Idle: observed.Idle, Eligible: true, Callbacks: 1, LinearLineage: 1, GuardPolicy: 1, GuardConversationSchema: 2, QuestionsReleased: true, PlanPublicationReleased: true, ImplementationReleased: workflowPermission(grant.current.SavedAccess, "pi-plan.accept") == nil, ModeReleased: workflowPermission(grant.current.SavedAccess, "pi-plan.mode") == nil, BoundHostTools: slices.Clone(w.tools)}
		if proposal != nil {
			current := proposal.Caller
			current.Handle = grant.current.Handle
			if err := (piplan.NativeOriginVerifier{Inspector: w.runtime}).Verify(ctx, current, *proposal); err == nil {
				entry.policy.Proposal = &piplan.ProposalObservation{PlanID: proposal.PlanID, Revision: proposal.Revision, TextSHA256: proposal.TextSHA256, Caller: current}
			}
		}
		// Compare every captured owning record after native proof. A publication,
		// answer, access or authority change cannot revive the old capability.
		latestGroups, err := questions.Questions(ctx, ref)
		if err != nil || !reflect.DeepEqual(groups, latestGroups) {
			return piruntime.ErrConflict
		}
		latestProposal, err := plans.LatestProposal(ctx, ref)
		if err != nil || !reflect.DeepEqual(proposal, latestProposal) {
			return piruntime.ErrConflict
		}
		current, err := w.server.piSessionAdmissionLocked(ref.Session)
		if err != nil || !reflect.DeepEqual(current, grant.current.piSessionAdmission) {
			return piruntime.ErrConflict
		}
		access, err := w.server.piSavedAccessLocked(current, ref)
		if err != nil || access != grant.current.SavedAccess || w.server.requirePiWorkflowGuards() != nil || ctx.Err() != nil {
			return piruntime.ErrUnavailable
		}
		reader, ok := w.runtime.(piruntime.CurrentRuntimeReader)
		if !ok {
			return piruntime.ErrUnavailable
		}
		currentRuntime, err := reader.ReadCurrentRuntime(ctx, ref.Session)
		if err != nil {
			return err
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		attempt := o.attachments[ref.Session]
		if o.counters[ref] != counter || attempt == nil || !attempt.connected || !attempt.readyClosed || attempt.attachment != observed.AttachmentID || attempt.sequence != observed.Sequence || attempt.runtime != currentRuntime || currentRuntime.Generation != grant.current.Handle.Generation {
			return piruntime.ErrConflict
		}
		o.entries[ref] = entry
		// A GET queued while this exact unchanged pass was in flight already
		// has its result. Keep event/accepted-intent drain demand, but do not
		// immediately invalidate a newly published snapshot for another GET.
		if drain, present := o.requested[ref]; present && !drain {
			delete(o.requested, ref)
		}
		for channel := range o.listeners[ref] {
			select {
			case channel <- struct{}{}:
			default:
			}
		}
		return nil
	})
}

func (w *piWorkflowComposition) close() {
	if w == nil {
		return
	}
	o := &w.observations
	o.mu.Lock()
	defer o.mu.Unlock()
	for ref := range o.counters {
		o.invalidateLocked(ref, false)
	}
	for _, attempt := range o.attachments {
		attempt.connected = false
		attempt.cancel()
	}
}
