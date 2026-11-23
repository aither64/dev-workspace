package web

import (
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"sync"
	"time"

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
	refreshes   map[agentbackend.ConversationRef]*piWorkflowRefresh
	order       []agentbackend.ConversationRef
	foreground  map[agentbackend.SessionRef]int
	bursts      map[agentbackend.SessionRef]int
	reserved    map[agentbackend.SessionRef]*piWorkflowReservation
	now         func() time.Time
}

type piWorkflowReservation struct {
	ref  agentbackend.ConversationRef
	done chan struct{}
}

type piWorkflowRefresh struct {
	inflight bool
	counter  uint64
	failures uint
	retryAt  time.Time
	cancel   context.CancelFunc
	reserved bool
}

func (o *piWorkflowObservations) initialize() {
	o.entries = map[agentbackend.ConversationRef]piWorkflowSnapshot{}
	o.counters = map[agentbackend.ConversationRef]uint64{}
	o.requested = map[agentbackend.ConversationRef]bool{}
	o.listeners = map[agentbackend.ConversationRef]map[chan struct{}]struct{}{}
	o.attachments = map[agentbackend.SessionRef]*piWorkflowAttachment{}
	o.refreshes = map[agentbackend.ConversationRef]*piWorkflowRefresh{}
	o.foreground = map[agentbackend.SessionRef]int{}
	o.bursts = map[agentbackend.SessionRef]int{}
	o.reserved = map[agentbackend.SessionRef]*piWorkflowReservation{}
	o.now = time.Now
}

func (o *piWorkflowObservations) requestLocked(ref agentbackend.ConversationRef, drain bool) {
	state := o.refreshes[ref]
	if state == nil {
		state = &piWorkflowRefresh{}
		o.refreshes[ref] = state
		o.order = append(o.order, ref)
	}
	// Display readers of the same revision share this attempt, including its
	// failure. Real delivery demand and a changed revision survive the attempt.
	if !drain && state.inflight && state.counter == o.counters[ref] {
		return
	}
	o.requested[ref] = o.requested[ref] || drain
}

func (o *piWorkflowObservations) dueLocked(session agentbackend.SessionRef) (agentbackend.ConversationRef, bool) {
	for _, ref := range o.order {
		state := o.refreshes[ref]
		if _, pending := o.requested[ref]; pending && ref.Session == session && !state.inflight && !o.now().Before(state.retryAt) {
			return ref, true
		}
	}
	return agentbackend.ConversationRef{}, false
}

// acquirePiForeground grants scheduling ownership only. Budgeting and
// registration are atomic, so concurrent callers cannot slip past a reserved
// background pass. Ordinary locks and fresh checks still grant effect authority.
func (s *Server) acquirePiForeground(ctx context.Context, ref agentbackend.SessionRef) (func(), error) {
	w := s.piWorkflows
	if w == nil {
		return func() {}, ctx.Err()
	}
	o := &w.observations
	for {
		o.mu.Lock()
		select {
		case <-s.stopping:
			o.mu.Unlock()
			return nil, piruntime.ErrUnavailable
		default:
		}
		if err := ctx.Err(); err != nil {
			o.mu.Unlock()
			return nil, err
		}
		if reservation := o.reserved[ref]; reservation != nil {
			done := reservation.done
			o.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-s.stopping:
				return nil, piruntime.ErrUnavailable
			case <-done:
				continue
			}
		}
		due, pending := o.dueLocked(ref)
		if pending {
			o.bursts[ref]++
			if o.bursts[ref] == 8 {
				o.reserved[ref] = &piWorkflowReservation{ref: due, done: make(chan struct{})}
			}
		} else {
			delete(o.bursts, ref)
		}
		o.foreground[ref]++
		for conversation, state := range o.refreshes {
			if conversation.Session == ref && state.cancel != nil && !state.reserved {
				state.cancel() // only waiting/read stages install a cancel
			}
		}
		o.mu.Unlock()
		break
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			o.foreground[ref]--
			if o.foreground[ref] == 0 {
				delete(o.foreground, ref)
			}
			o.mu.Unlock()
			w.wake()
		})
	}, nil
}

func (w *piWorkflowComposition) backgroundReady(ctx context.Context, ref agentbackend.ConversationRef) bool {
	if ctx.Err() != nil {
		return false
	}
	w.observations.mu.Lock()
	defer w.observations.mu.Unlock()
	return w.observations.foreground[ref.Session] == 0
}

// Called inside ordinary exclusive admission, immediately before the domain
// starts effects. Foreground may cancel a waiting admission, but cannot cancel
// this original effect/receipt continuation once the callback begins.
func (w *piWorkflowComposition) backgroundEffects(ctx context.Context, ref agentbackend.ConversationRef, run func(context.Context) error) error {
	o := &w.observations
	o.mu.Lock()
	if err := ctx.Err(); err != nil {
		o.mu.Unlock()
		return err
	}
	if o.foreground[ref.Session] != 0 {
		o.mu.Unlock()
		return context.Canceled
	}
	if state := o.refreshes[ref]; state != nil {
		state.cancel = nil
	}
	o.mu.Unlock()
	return run(ctx)
}

func (o *piWorkflowObservations) invalidateLocked(ref agentbackend.ConversationRef, demand bool) {
	if o.counters[ref] != math.MaxUint64 {
		o.counters[ref]++
	}
	delete(o.entries, ref)
	if demand {
		o.requestLocked(ref, false)
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
				o.requestLocked(ref, true)
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
	w.observations.requestLocked(ref, drain)
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
	owner         *piWorkflowComposition
	ref           agentbackend.ConversationRef
	native        backend.Watcher
	currentEvents bool
}

func (w piWorkflowWatch) Subscribe(ctx context.Context, identity backend.Identity) (<-chan struct{}, func(), error) {
	if w.owner == nil || w.native == nil || identity.ConversationID != w.ref.NativeID {
		return nil, nil, piruntime.ErrUnavailable
	}
	ctx, cancel := context.WithCancel(ctx)
	local, merged := make(chan struct{}, 1), make(chan struct{}, 1)
	o := &w.owner.observations
	o.mu.Lock()
	// Register before selecting the connection or taking any initial native
	// observation. A change in that window must wake this reader too.
	if _, exists := o.counters[w.ref]; !exists {
		o.counters[w.ref] = 0
	}
	if o.listeners[w.ref] == nil {
		o.listeners[w.ref] = map[chan struct{}]struct{}{}
	}
	o.listeners[w.ref][local] = struct{}{}
	attempt := o.attachments[w.ref.Session]
	connected := w.currentEvents && attempt != nil && attempt.connected && attempt.readyClosed && !attempt.lost && attempt.runtime.Generation == identity.Generation
	if w.currentEvents && !connected {
		delete(o.listeners[w.ref], local)
		o.mu.Unlock()
		cancel()
		return nil, nil, piruntime.ErrUnavailable
	}
	o.mu.Unlock()
	var native <-chan struct{}
	release := func() {}
	if !connected {
		var err error
		native, release, err = w.native.Subscribe(ctx, identity)
		if err != nil {
			cancel()
			o.mu.Lock()
			delete(o.listeners[w.ref], local)
			o.mu.Unlock()
			return nil, nil, err
		}
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() { cancel(); release(); o.mu.Lock(); delete(o.listeners[w.ref], local); o.mu.Unlock() })
	}
	go func() {
		defer close(merged)
		defer cleanup()
		// Re-read once after registration; no change between the HTTP initial
		// read and listener setup can be lost. Connected streams never poll.
		merged <- struct{}{}
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
			valid := true
			if connected {
				o.mu.Lock()
				valid = o.attachments[w.ref.Session] == attempt && attempt.connected && attempt.readyClosed && !attempt.lost
				o.mu.Unlock()
			}
			select {
			case merged <- struct{}{}:
			default:
			}
			if !valid {
				return
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
	if observed.Handle != handle || observed.Retained == nil || !observed.Loaded || observed.Pending || observed.Held || (idle && !observed.Idle) || piruntime.ValidateToolPolicyProof(observed.Proof, *observed.Retained, handle.Generation) != nil || !w.validWorkerHostTools(observed.HostTools) {
		return observed, piruntime.ErrHeld
	}
	target, err := composePiWorkflowPolicy(grant.current.SavedAccess, observed.Retained.Mode, observed.Retained.Revision, observed.HostTools)
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

// Demand, invalidation and failed attempts populate requested. The existing
// display worker's tick only services that pending work; it adds no new demand.
func (w *piWorkflowComposition) refreshRequested(ctx context.Context) {
	if w == nil {
		return
	}
	ref, drain, readCtx, cancel, ok := w.nextRefresh(ctx)
	if !ok {
		return
	}
	defer cancel()
	var refreshErr error
	deliveryPending := drain
	interrupted := false
	defer func() { w.finishRefresh(ref, refreshErr, deliveryPending, interrupted) }()
	if refreshErr = w.reconcileOriginal(readCtx, ref); refreshErr == nil && w.backgroundReady(readCtx, ref) {
		refreshErr = w.attachRetained(readCtx, ref)
		if refreshErr == nil && w.backgroundReady(readCtx, ref) {
			refreshErr = w.refresh(readCtx, ref)
		}
	}
	// Yield between every read stage and before delivery. Once a drain enters
	// effects, foreground arrival must not cancel its original receipt work.
	interrupted = !w.backgroundReady(context.WithoutCancel(ctx), ref) || readCtx.Err() != nil && ctx.Err() == nil
	if ctx.Err() != nil {
		refreshErr = errors.Join(refreshErr, ctx.Err())
	}
	if interrupted {
		return
	}
	if refreshErr != nil {
		w.invalidateRef(ref, false)
	}
	if drain && w.backgroundReady(ctx, ref) {
		// Display proof is not delivery authority. Even a failed display pass
		// may service real intent through its own fresh exclusive admission.
		deliveryErr := w.drain(readCtx, ref)
		deliveryPending = deliveryErr != nil
		refreshErr = errors.Join(refreshErr, deliveryErr)
		interrupted = !w.backgroundReady(context.WithoutCancel(ctx), ref) || readCtx.Err() != nil && ctx.Err() == nil
	} else if drain {
		interrupted = ctx.Err() == nil
	}
	if refreshErr != nil {
		w.server.config.Logger.Printf("Pi workflow refresh unavailable for %s/%s: %v", ref.Session.Slug, ref.Member, refreshErr)
	}
}

// One conversation per wake bounds work and yields to the other display tasks.
// Rotating the selected ref prevents a failed or busy member starving siblings.
func (w *piWorkflowComposition) nextRefresh(ctx context.Context) (agentbackend.ConversationRef, bool, context.Context, context.CancelFunc, bool) {
	o := &w.observations
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx.Err() != nil {
		return agentbackend.ConversationRef{}, false, nil, nil, false
	}
	for i, ref := range o.order {
		drain, pending := o.requested[ref]
		state := o.refreshes[ref]
		if !pending || state.inflight || o.foreground[ref.Session] != 0 || o.now().Before(state.retryAt) {
			continue
		}
		reservation := o.reserved[ref.Session]
		if reservation != nil && reservation.ref != ref {
			continue
		}
		delete(o.requested, ref)
		state.inflight, state.counter = true, o.counters[ref]
		state.reserved = reservation != nil
		readCtx, cancel := context.WithCancel(ctx)
		state.cancel = cancel
		o.order = append(append(o.order[:i:i], o.order[i+1:]...), ref)
		return ref, drain, readCtx, cancel, true
	}
	return agentbackend.ConversationRef{}, false, nil, nil, false
}

func (w *piWorkflowComposition) finishRefresh(ref agentbackend.ConversationRef, err error, deliveryPending, interrupted bool) {
	o := &w.observations
	o.mu.Lock()
	state := o.refreshes[ref]
	state.inflight, state.cancel = false, nil
	if interrupted {
		o.requestLocked(ref, deliveryPending)
	} else if err != nil {
		if state.failures < 6 {
			state.failures++
		}
		delay := min(time.Second<<uint(state.failures-1), 30*time.Second)
		state.retryAt = o.now().Add(delay)
		o.requestLocked(ref, deliveryPending)
	} else {
		state.failures, state.retryAt = 0, time.Time{}
	}
	if state.reserved {
		reservation := o.reserved[ref.Session]
		delete(o.reserved, ref.Session)
		delete(o.bursts, ref.Session)
		state.reserved = false
		close(reservation.done)
	}
	ready := false
	for conversation := range o.requested {
		next := o.refreshes[conversation]
		if !next.inflight && o.foreground[conversation.Session] == 0 && !o.now().Before(next.retryAt) {
			ready = true
			break
		}
	}
	o.mu.Unlock()
	if ready {
		w.wake()
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
		entry.policy = piplan.PolicyObservation{Current: grant.current.Handle, Observed: grant.current.Handle, CurrentBindingRevision: grant.current.BindingRevision, ObservedBindingRevision: grant.current.BindingRevision, Retained: cloneWorkflowPolicy(*observed.Retained), Proof: observed.Proof, Pending: observed.Pending, Held: observed.Held, Idle: observed.Idle, Eligible: true, Callbacks: 1, LinearLineage: 1, GuardPolicy: 1, GuardConversationSchema: 2, QuestionsReleased: true, PlanPublicationReleased: true, ImplementationReleased: workflowPermission(grant.current.SavedAccess, "pi-plan.accept") == nil, ModeReleased: workflowPermission(grant.current.SavedAccess, "pi-plan.mode") == nil, BoundHostTools: slices.Clone(observed.HostTools)}
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
