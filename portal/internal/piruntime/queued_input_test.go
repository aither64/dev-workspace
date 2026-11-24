package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func checkedRequest(selection InputSelection, id string) QueuedInputRequest {
	input := SendInput{Message: "exact original literal ${input}", ActionContext: inputDeliveryPrefix + "steer"}
	ref := SubmissionRef{Conversation: selection.Handle.Conversation, OperationID: id, Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, selection.Settings)
	return QueuedInputRequest{OriginalRuntime: selection.Runtime, Selection: selection, Ref: ref, Input: input, Delivery: "steer"}
}

func TestCheckedInputFreshTupleAndPostNativeAmbiguity(t *testing.T) {
	for _, change := range []string{"settings", "policy", "live-id", "instructions", "held", "callback_unknown", "process", "wire"} {
		t.Run(change, func(t *testing.T) {
			m, _, c, w, selection, ctx := checkedInputFixture(t)
			request := checkedRequest(selection, strings.Repeat("3", 32))
			if err := m.PreflightQueuedInput(ctx, selection.Handle); err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			switch change {
			case "settings":
				c.record.Settings.Revision++
			case "policy":
				c.record.Schema = 2
				policy := policyTarget("default", 1, []string{}, []string{})
				c.record.ToolPolicy = &policy
			case "live-id":
				w.mutateIdentity = func(proof *workerIdentityProof) { proof.NativeID = "foreign" }
			case "instructions":
				w.mutateIdentity = func(proof *workerIdentityProof) { proof.InstructionsDigest = strings.Repeat("f", 64) }
			case "held":
				w.snapshot.Held = true
			case "callback_unknown":
				c.activeMu.Lock()
				c.callbackUnknown = true
				c.activeMu.Unlock()
			case "process":
				m.config.verifyProcess = func(HostObservation) error { return ErrScope }
			case "wire":
				w.enqueueError = ErrUnavailable
			}
			if err := c.save(); err != nil {
				t.Fatal(err)
			}
			c.mu.Unlock()
			result, err := m.DispatchQueuedInput(ctx, request)
			if change == "wire" {
				if err == nil || result.NotSubmitted || w.enqueues != 1 {
					t.Fatal("post-native error claimed no submission", result, err, w.enqueues)
				}
			} else if change == "process" {
				if err == nil || result.NotSubmitted || w.enqueues != 0 {
					t.Fatal("lost fresh host proof after preflight", result, err, w.enqueues)
				}
			} else if err == nil || !result.NotSubmitted || w.enqueues != 0 {
				t.Fatal("fresh tuple mismatch reached native input", result, err, w.enqueues)
			}
		})
	}
}

func saveCheckedReceipt(t *testing.T, c *conversation, request QueuedInputRequest, phase string) {
	t.Helper()
	now := time.Now().UTC()
	status := SubmissionStatus{Schema: 1, Ref: request.Ref, Phase: phase, Generation: request.Selection.Handle.Generation, Settings: request.Selection.Settings, Input: request.Input, CreatedAt: now, UpdatedAt: now}
	if phase == "Accepted" {
		status.Witness = &NativeWitness{SessionID: request.Ref.Conversation.NativeID, EntryID: "original-entry", EffectiveDigest: MessageDigest(request.Input.Message)}
	}
	if err := c.receipts.save(status); err != nil {
		t.Fatal(err)
	}
}

func TestCheckedInputOriginalReceiptSurvivesChangedSelectionWithoutReplay(t *testing.T) {
	for _, phase := range []string{"Accepted", "Unknown"} {
		t.Run(phase, func(t *testing.T) {
			m, _, c, w, selection, ctx := checkedInputFixture(t)
			request := checkedRequest(selection, strings.Repeat("4", 32))
			saveCheckedReceipt(t, c, request, phase)
			c.mu.Lock()
			c.record.Settings.Revision++
			err := c.save()
			c.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			result, err := m.DispatchQueuedInput(ctx, request)
			if err != nil || result.NotSubmitted || result.Status.Phase != phase || result.Status.Ref != request.Ref || w.enqueues != 0 {
				t.Fatal("original receipt was reclassified/replayed", result, err)
			}
			status, err := lookupQueuedStatus(m, ctx, request.Ref)
			if err != nil || status.Phase != phase || status.Ref != request.Ref {
				t.Fatal(status, err)
			}
			changed := request
			changed.Input.Message = "different original"
			changed.Ref.Digest = SubmissionDigest(changed.Ref, changed.Input, changed.Selection.Settings)
			if result, err := m.DispatchQueuedInput(ctx, changed); !errors.Is(err, ErrConflict) || result.NotSubmitted {
				t.Fatal("changed original identity accepted", result, err)
			}
			missing := checkedRequest(selection, strings.Repeat("5", 32))
			if result, err := m.DispatchQueuedInput(ctx, missing); err == nil || !result.NotSubmitted || w.enqueues != 0 {
				t.Fatal("missing stale selection sent", result, err)
			}
		})
	}
}

func TestQueuedLookupRequiresExactLiveOwnerAndNativeWitness(t *testing.T) {
	m, host, c, w, selection, ctx := checkedInputFixture(t)
	request := checkedRequest(selection, strings.Repeat("6", 32))
	if status, err := lookupQueuedStatus(m, ctx, request.Ref); err != nil || status.Phase != "Unknown" {
		t.Fatal("missing receipt was not Unknown", status, err)
	}
	saveCheckedReceipt(t, c, request, "dispatching")
	c.receipts.liveInputs = map[string]*liveInputReceipt{request.Ref.OperationID: {ref: receiptReference{retained: &request.Ref}, worker: w}}
	if status, err := lookupQueuedStatus(m, ctx, request.Ref); err != nil || status.Phase != "Pending" {
		t.Fatal("live owner lost Pending", status, err)
	}
	c.receipts.liveInputs[request.Ref.OperationID].worker = &snapshotWorker{}
	if status, err := lookupQueuedStatus(m, ctx, request.Ref); err != nil || status.Phase != "Unknown" {
		t.Fatal("replacement reconstructed Pending", status, err)
	}
	saveCheckedReceipt(t, c, request, "Accepted")
	w.verifyHistory = false
	if status, err := lookupQueuedStatus(m, ctx, request.Ref); err == nil || status.Phase != "Unknown" {
		t.Fatal("unverified witness became Accepted", status, err)
	}
	w.verifyHistory = true
	// The old owning verifier remains an observational compatibility path.
	m.config.checkedInputContract = func(string) (bool, error) { return false, nil }
	if status, err := lookupQueuedStatus(m, ctx, request.Ref); err != nil || status.Phase != "Accepted" {
		t.Fatal("retained original proof failed", status, err)
	}
	var saved SubmissionStatus
	path := filepath.Join(c.receipts.root, request.Ref.OperationID+".json")
	if err := readPrivate(path, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Generation = strings.Repeat("e", 64)
	if err := writePrivate(path, saved); err != nil {
		t.Fatal(err)
	}
	if status, err := lookupQueuedStatus(m, ctx, request.Ref); err == nil || status.Phase != "Unknown" {
		t.Fatal("foreign receipt generation accepted", status, err)
	}
	if host.count != 1 || w.enqueues != 0 {
		t.Fatal("lookup created runtime/input")
	}
	c.mu.Lock()
	c.record.Loaded = false
	err := c.save()
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	missing := checkedRequest(selection, strings.Repeat("1", 32)).Ref
	if status, err := lookupQueuedStatus(m, ctx, missing); err != nil || status.Phase != "Unknown" {
		t.Fatal("unloaded new admission blocked independent missing receipt read", status, err)
	}
}

func TestQueuedInputLeavesLegacyWireAndNativeReceiptBytesUnchanged(t *testing.T) {
	data, err := os.ReadFile("../session/runtime-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"piConversationIdentity", "piCheckedInput"} {
		if supported, err := decodeConversationCapability(data, name); err != nil || !supported {
			t.Fatal(name, supported, err)
		}
	}
	// New operations use distinct types. The legacy strict request deliberately
	// has no Selection/expected-settings field that an old reader might ignore.
	request := checkedRequest(InputSelection{}, strings.Repeat("7", 32))
	encoded, _ := json.Marshal(request)
	var legacy conversationRequest
	if strictDecode(encoded, &legacy, recordLimit) == nil {
		t.Fatal("new checked request passed old strict reader")
	}
}

func TestInputRuntimeContinuationRequiresFullCurrentAncestryWithoutHostCalls(t *testing.T) {
	m, host, start, first, ctx := successorFixture(t)
	if err := m.ValidateInputRuntimeContinuation(ctx, first.Runtime, first.Runtime); err != nil {
		t.Fatal(err)
	}
	host.finish(t, first.Runtime)
	second, err := m.Ensure(ctx, nextStart(start, "33333333-3333-4333-8333-333333333333"))
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingSelectionHost{Host: m.config.Host}
	m.config.Host = counted
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	release, err := runtimeLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := m.ValidateInputRuntimeContinuation(ctx, first.Runtime, second.Runtime); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"digest", "generation", "instance", "request", "session", "noncurrent"} {
		original, current := first.Runtime, second.Runtime
		switch field {
		case "digest":
			original.Digest = strings.Repeat("e", 64)
		case "generation":
			original.Generation = current.Generation
		case "instance":
			original.InstanceID = current.RequestID
		case "request":
			original.RequestID = current.RequestID
		case "session":
			original.Session.Incarnation = strings.Repeat("e", 64)
		case "noncurrent":
			current = first.Runtime
		}
		if err := m.ValidateInputRuntimeContinuation(ctx, original, current); err == nil {
			t.Fatal("unproved full runtime accepted", field)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.ValidateInputRuntimeContinuation(canceled, first.Runtime, second.Runtime); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if counted.inspections.Load() != 0 || host.starts != 2 {
		t.Fatal("read-only ancestry probed/started native runtime")
	}
}

func TestCheckedInputReadySuccessorPreservesOriginalIDAndDigest(t *testing.T) {
	m, host, start, first, ctx := successorFixture(t)
	m.config.conversationIdentityContract = func(string) (bool, error) { return true, nil }
	m.config.checkedInputContract = func(string) (bool, error) { return true, nil }
	m.config.inputDeliveryContract = func(string) (bool, error) { return true, nil }
	host.base.s.verifiedInstanceDirectory = m.instance(first.Runtime)
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: newUUID(), Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	admitted, err := m.ReadInputSelection(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	request := checkedRequest(admitted, strings.Repeat("a", 32))
	if _, err := m.Unload(ctx, handle); err != nil {
		t.Fatal(err)
	}
	host.finish(t, first.Runtime)
	second, err := m.Ensure(ctx, nextStart(start, "44444444-4444-4444-8444-444444444444"))
	if err != nil {
		t.Fatal(err)
	}
	handle.Generation = second.Runtime.Generation
	if _, err := m.ReadInputSelection(ctx, handle); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unloaded successor implied readiness", err)
	}
	if _, err := m.Load(ctx, handle); err != nil {
		t.Fatal(err)
	} // explicit ordinary fixture upgrade, never queue recovery
	current, err := m.ReadInputSelection(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if current.Handle.Conversation != admitted.Handle.Conversation || current.Settings != admitted.Settings || current.InstructionsDigest != admitted.InstructionsDigest {
		t.Fatal("successor changed logical selection")
	}
	request.Selection = current
	if request.OriginalRuntime != first.Runtime || SubmissionDigest(request.Ref, request.Input, current.Settings) != request.Ref.Digest {
		t.Fatal("successor reminted original provenance/digest")
	}
	result, err := m.DispatchQueuedInput(ctx, request)
	if err != nil || result.Status.Phase != "Pending" {
		t.Fatal("ready successor rejected unchanged input", result, err)
	}
	for {
		status, err := lookupQueuedStatus(m, ctx, request.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.Phase == "Accepted" {
			if status.Ref != request.Ref || status.Generation != second.Runtime.Generation {
				t.Fatal("wrong original/execution receipt", status)
			}
			break
		}
		if status.Phase != "Pending" {
			t.Fatal(status)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if host.starts != 2 {
		t.Fatal("checked continuation implicitly started runtime")
	}
	// A stale original/current claim can never invoke a second native send.
	request.Selection = admitted
	if result, err := m.DispatchQueuedInput(ctx, request); err == nil || result.NotSubmitted {
		t.Fatal("stale owner adopted current runtime", result, err)
	}
	request.Selection = current
	if result, err := m.DispatchQueuedInput(ctx, request); err != nil || result.Status.Phase != "Accepted" {
		t.Fatal("repeat ID lost original receipt", result, err)
	}
	c, _ := host.base.s.member(handle.Conversation)
	var snapshot workerSnapshot
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, raw := range snapshot.Entries {
		var entry nativeEntry
		if json.Unmarshal(raw, &entry) == nil && entry.Type == "message" && entry.Message.Role == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatal("successor replayed original input", users)
	}
}

func TestCheckedInputRequiresWorkflowACKBeforeConversationLockOrEnqueue(t *testing.T) {
	m, host, c, w, selection, ctx := checkedInputFixture(t)
	if err := m.PreflightQueuedInput(ctx, selection.Handle); err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	blocked, release := make(chan struct{}, 1), make(chan struct{})
	attachFixture(t, m, host.s, ctx, selection.Runtime, &workflowFixtureHandler{invalidated: func(event WorkflowInvalidation) {
		if armed.Load() && event.Connected {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-release
		}
	}})
	awaitWorkflowAttachment(t, host.s, ctx)
	armed.Store(true)
	result := make(chan error, 1)
	go func() {
		_, err := m.DispatchQueuedInput(ctx, checkedRequest(selection, strings.Repeat("8", 32)))
		result <- err
	}()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !c.mu.TryLock() {
		close(release)
		t.Fatal("pre-effect ACK waited under conversation lock")
	}
	if w.enqueues != 0 {
		c.mu.Unlock()
		close(release)
		t.Fatal("native enqueue preceded invalidation ACK")
	}
	c.mu.Unlock()
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if w.enqueues != 1 {
		t.Fatal("admitted checked input never reached native path")
	}
}

func TestCheckedInputNativeWorkerPersistsExactWitnessAndNeverReplays(t *testing.T) {
	m, host, start, ctx := supervisorFixture(t)
	transcriptContractFixture(t, m, start, `{"piInputDelivery":1,"piTranscriptSnapshot":1}`)
	m.config.conversationIdentityContract = func(string) (bool, error) { return true, nil }
	m.config.checkedInputContract = func(string) (bool, error) { return true, nil }
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	host.s.verifiedInstanceDirectory = m.instance(runtime.Runtime)
	state, err := m.Create(ctx, runtime.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: newUUID(), Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	selection, err := m.ReadInputSelection(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	request := checkedRequest(selection, strings.Repeat("9", 32))
	result, err := m.DispatchQueuedInput(ctx, request)
	if err != nil || result.NotSubmitted || result.Status.Phase != "Pending" {
		t.Fatal("native enqueue acknowledgement", result, err)
	}
	var accepted SubmissionStatus
	for {
		accepted, err = lookupQueuedStatus(m, ctx, request.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if accepted.Phase == "Accepted" {
			break
		}
		if accepted.Phase != "Pending" {
			t.Fatal("healthy native input lost ownership", accepted)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if accepted.Witness == nil || accepted.Ref != request.Ref || accepted.Witness.EffectiveDigest != MessageDigest(request.Input.Message) {
		t.Fatal("wrong exact witness", accepted)
	}
	for range 2 {
		result, err := m.DispatchQueuedInput(ctx, request)
		if err != nil || result.Status.Phase != "Accepted" || result.Status.Witness.EntryID != accepted.Witness.EntryID {
			t.Fatal("same original did not retain witness", result, err)
		}
	}
	c, _ := host.s.member(state.Conversation)
	var snapshot workerSnapshot
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, raw := range snapshot.Entries {
		var entry nativeEntry
		if json.Unmarshal(raw, &entry) == nil && entry.Type == "message" && entry.Message.Role == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatal("original input replayed", users)
	}
}

// Existing receipt assertions keep exercising the unchanged native status;
// retirement disposition is checked separately through the typed lookup.
func lookupQueuedStatus(m *Manager, ctx context.Context, ref SubmissionRef) (SubmissionStatus, error) {
	result, err := m.LookupQueuedInput(ctx, ref)
	return result.Status, err
}

func TestQueuedLookupEnvelopeCannotRetireSyntheticUnknown(t *testing.T) {
	for _, raw := range []string{`{"Status":{"Phase":"Unknown"}}`, `{"Status":{"Phase":"Unknown"},"RetiredUnknown":true}`, `{"Status":{"Phase":"Unknown"},"RetiredUnknown":"false"}`, `{"Status":{"Phase":"Unknown"},"RetiredUnknown":false,"other":true}`} {
		var result QueuedInputLookupResult
		if json.Unmarshal([]byte(raw), &result) == nil {
			t.Fatal("malformed/synthetic retirement accepted", raw)
		}
	}
	data, _ := json.Marshal(QueuedInputLookupResult{Status: SubmissionStatus{Phase: "Unknown"}})
	var result QueuedInputLookupResult
	if err := json.Unmarshal(data, &result); err != nil || result.RetiredUnknown {
		t.Fatal("missing original changed retirement", result, err)
	}
	var legacy SubmissionStatus
	if strictDecode(data, &legacy, recordLimit) == nil {
		t.Fatal("new queued lookup polluted legacy status shape")
	}
}

func TestQueuedInputPreflightKeepsOneFreshProofAndFrozenErrors(t *testing.T) {
	for _, kind := range []string{"scalar", "legacy", "identity_error", "activity_error", "cancel_before", "cancel_after"} {
		t.Run(kind, func(t *testing.T) {
			m, host, _, w, selection, parent := checkedInputFixture(t)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			identities, snapshots, starts := w.identities, w.snapshots, host.count
			activityCalls := 0
			m.config.activityContract = func(app string) (bool, error) {
				activityCalls++
				current, err := m.record(selection.Handle.Conversation.Session)
				if err != nil || app != current.Start.AppStorePath {
					t.Error("preflight selected another activity contract", app, err)
				}
				if kind == "activity_error" {
					return false, ErrConflict
				}
				if kind == "cancel_after" {
					cancel()
				}
				return false, nil
			}
			wantIdentity, wantSnapshot, wantActivity := 1, 0, 1
			var wantErr error
			switch kind {
			case "legacy":
				m.config.conversationIdentityContract = func(string) (bool, error) { return false, nil }
				m.config.checkedInputContract = func(string) (bool, error) { return false, nil }
				wantIdentity, wantSnapshot = 0, 1
			case "identity_error":
				w.mutateIdentity = func(proof *workerIdentityProof) { proof.NativeID = "foreign" }
				wantErr, wantActivity = ErrScope, 0
			case "activity_error":
				wantErr = ErrConflict
			case "cancel_before":
				cancel()
				wantErr = context.Canceled
				wantIdentity, wantActivity = 0, 0
			case "cancel_after":
				wantErr = context.Canceled
			}
			err := m.PreflightQueuedInput(ctx, selection.Handle)
			if wantErr == nil && err != nil || wantErr != nil && !errors.Is(err, wantErr) {
				t.Fatal("preflight proof/error changed", kind, err)
			}
			if w.identities-identities != wantIdentity || w.snapshots-snapshots != wantSnapshot || activityCalls != wantActivity || host.count != starts || w.enqueues != 0 {
				t.Fatal("preflight duplicated/skipped proof, fell back or created effects", kind, w.identities-identities, w.snapshots-snapshots, activityCalls, host.count-starts, w.enqueues)
			}
			if kind == "scalar" {
				w.snapshot.Held = true
				if err := m.PreflightQueuedInput(ctx, selection.Handle); !errors.Is(err, ErrHeld) || w.identities-identities != 2 || w.snapshots != snapshots {
					t.Fatal("preflight retained an earlier proof", err)
				}
			}
		})
	}
}

func TestQueuedInputPreflightUnavailableNeverLoads(t *testing.T) {
	for _, kind := range []string{"unloaded", "no_worker", "preparing", "held", "callback_unknown", "stale_live", "process"} {
		t.Run(kind, func(t *testing.T) {
			m, host, c, w, selection, ctx := checkedInputFixture(t)
			record, originalWorker, observed, verify := c.record, c.worker, host.observation, m.config.verifyProcess
			starts, loads := host.count, 0
			host.s.start = func(context.Context, workerMetadata, agentbackend.ExecutionHandle, workerConfig, func()) (worker, error) {
				loads++
				return nil, ErrUnavailable
			}
			c.mu.Lock()
			switch kind {
			case "unloaded":
				c.record.Loaded = false
				c.worker = nil
			case "no_worker":
				c.worker = nil
			case "held":
				w.snapshot.Held = true
			case "callback_unknown":
				c.activeMu.Lock()
				c.callbackUnknown = true
				c.activeMu.Unlock()
			case "stale_live":
				w.mutateIdentity = func(proof *workerIdentityProof) { proof.NativeID = "foreign" }
			case "process":
				m.config.verifyProcess = func(HostObservation) error { return ErrScope }
			case "preparing":
				host.mu.Lock()
				host.observation.Outcome = "starting"
				host.observation.Phase = "starting"
				host.mu.Unlock()
			}
			if err := c.save(); err != nil {
				t.Fatal(err)
			}
			c.mu.Unlock()
			if err := m.PreflightQueuedInput(ctx, selection.Handle); err == nil {
				t.Fatal("unavailable owner passed preflight", kind)
			}
			if loads != 0 || host.count != starts || w.enqueues != 0 {
				t.Fatal("preflight loaded/started/enqueued", kind, loads, host.count-starts, w.enqueues)
			}
			c.mu.Lock()
			c.record = record
			c.worker = originalWorker
			if err := c.save(); err != nil {
				t.Fatal(err)
			}
			c.mu.Unlock()
			c.activeMu.Lock()
			c.callbackUnknown = false
			c.activeMu.Unlock()
			w.snapshot.Held = false
			w.mutateIdentity = nil
			m.config.verifyProcess = verify
			host.mu.Lock()
			host.observation = observed
			host.mu.Unlock()
			if err := m.PreflightQueuedInput(ctx, selection.Handle); err != nil || loads != 0 || host.count != starts || w.enqueues != 0 {
				t.Fatal("same owner could not recover without preflight effects", kind, err, loads)
			}
		})
	}
}
