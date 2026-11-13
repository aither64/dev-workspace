package piruntime

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Workflow observations are ephemeral display proof, never effect authority or
// receipt identity. A new runtime generation starts with no observations.
type WorkflowObservation struct {
	Settings                    EffectiveSettings
	Handle                      agentbackend.ExecutionHandle
	AttachmentID                string
	Sequence                    uint64
	Retained                    *ToolPolicyState
	Proof                       ToolPolicyProof
	Loaded, Pending, Held, Idle bool
	HostTools                   []string
}
type WorkflowObserver interface {
	ObserveWorkflow(context.Context, agentbackend.ExecutionHandle) (WorkflowObservation, error)
}
type WorkflowInvalidation struct {
	Runtime      RuntimeRef
	AttachmentID string
	Sequence     uint64
	Connected    bool
}
type HostInvalidationHandler interface{ InvalidateWorkflow(WorkflowInvalidation) }

const workflowACKTimeout = 5 * time.Second
const workflowQueueLimit = 128

type workflowACK struct {
	sequence uint64
	received chan struct{}
}
type workflowState struct {
	mu                 sync.Mutex
	sequence, pending  uint64
	required, overflow bool
	attachment         *hostAttachment
}
type workflowPermit struct {
	attachment *hostAttachment
	active     bool
}
type workflowPermitKey struct{}

// Called with workflow.mu held. Enqueueing never waits for an attachment writer
// or host handler: worker stdout must remain free to deliver input witnesses.
func (s *supervisor) workflowAdvanceLocked(ack bool) (string, chan struct{}, error) {
	state := &s.workflow
	if state.overflow || state.sequence == math.MaxUint64 {
		state.overflow = true
		if a := state.attachment; a != nil {
			a.workflowReady = false
			_ = a.connection.Close()
		}
		return "", nil, ErrHeld
	}
	state.sequence++
	a := state.attachment
	if a == nil || !a.workflow || !a.workflowReady {
		if ack {
			return "", nil, ErrUnavailable
		}
		return "", nil, nil
	}
	if ack && len(a.workflowACKs) >= workflowQueueLimit {
		a.workflowReady = false
		_ = a.connection.Close()
		return "", nil, ErrUnavailable
	}
	id := newUUID()
	frame := hostFrame{Schema: 1, AttachmentID: a.id, Runtime: s.ref, Event: "workflow_invalidated", ID: id, Sequence: state.sequence, RequiresACK: ack}
	var waiter chan struct{}
	if ack {
		waiter = make(chan struct{})
		a.workflowACKs[id] = workflowACK{state.sequence, waiter}
	}
	select {
	case a.workflowEvents <- frame:
		return id, waiter, nil
	default:
		delete(a.workflowACKs, id)
		a.workflowReady = false
		_ = a.connection.Close()
		return "", nil, ErrUnavailable
	}
}
func (s *supervisor) workflowChanged() {
	s.workflow.mu.Lock()
	defer s.workflow.mu.Unlock()
	if s.workflow.required {
		_, _, _ = s.workflowAdvanceLocked(false)
	}
}

// The only ACK wait is before ordinary effect locks. Completion is notification
// only, so it cannot wait on a host handler holding a domain/runtime lock.
func (s *supervisor) beginWorkflowMutation(ctx context.Context) (context.Context, func(), error) {
	state := &s.workflow
	state.mu.Lock()
	if !state.required {
		state.mu.Unlock()
		return ctx, func() {}, nil
	}
	a := state.attachment
	if state.overflow || state.pending == math.MaxUint64 || a == nil || !a.workflowReady {
		state.mu.Unlock()
		return ctx, nil, ErrUnavailable
	}
	state.pending++
	permit := &workflowPermit{attachment: a, active: true}
	id, waiter, err := s.workflowAdvanceLocked(true)
	state.mu.Unlock()
	finish := func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		if !permit.active {
			return
		}
		permit.active = false
		state.pending--
		_, _, _ = s.workflowAdvanceLocked(false)
	}
	if err != nil {
		finish()
		return ctx, nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, workflowACKTimeout)
	defer cancel()
	select {
	case <-waiter:
	case <-waitCtx.Done():
		err = waitCtx.Err()
	case <-a.closed:
		err = ErrUnavailable
	}
	state.mu.Lock()
	delete(a.workflowACKs, id)
	if state.attachment != a || !a.workflowReady || state.overflow {
		err = ErrUnavailable
	}
	state.mu.Unlock()
	if err != nil {
		finish()
		return ctx, nil, err
	}
	return context.WithValue(ctx, workflowPermitKey{}, permit), finish, nil
}
func (s *supervisor) workflowCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.workflow.mu.Lock()
	defer s.workflow.mu.Unlock()
	if !s.workflow.required {
		return nil
	}
	permit, _ := ctx.Value(workflowPermitKey{}).(*workflowPermit)
	if s.workflow.overflow || permit == nil || !permit.active || permit.attachment != s.workflow.attachment || !permit.attachment.workflowReady {
		return ErrUnavailable
	}
	return nil
}
func workflowMutates(operation string) bool {
	return slices.Contains([]string{"send_checked", "create", "load", "send", "configure", "unload", "abort", "tool_policy_apply", "utility_create", "utility_load", "utility_bind", "utility_send", "utility_unload", "start_resource", "stop_resource"}, operation)
}

// Every nullable count/flag must be present in the synchronous worker reply.
// Missing old-worker support cannot default to an idle observation.
type workerWorkflowObservation struct {
	NativeID    string           `json:"nativeId"`
	Model       string           `json:"model"`
	Provider    string           `json:"provider"`
	Thinking    string           `json:"thinking"`
	Proof       *ToolPolicyProof `json:"proof"`
	Pending     *bool            `json:"pending"`
	Streaming   *bool            `json:"streaming"`
	HostCalls   *uint64          `json:"hostCalls"`
	Running     *bool            `json:"running"`
	ActiveCalls *uint64          `json:"activeCalls"`
	Held        *bool            `json:"held"`
	HostTools   []string         `json:"hostTools"`
}

func (s *supervisor) observeWorkflow(ctx context.Context, handle agentbackend.ExecutionHandle) (WorkflowObservation, error) {
	result := WorkflowObservation{Handle: handle}
	if err := s.verifyHandle(handle); err != nil {
		return result, err
	}
	s.workflow.mu.Lock()
	a, sequence := s.workflow.attachment, s.workflow.sequence
	available := s.workflow.required && !s.workflow.overflow && s.workflow.pending == 0 && a != nil && a.workflowReady
	s.workflow.mu.Unlock()
	if !available {
		return result, ErrUnavailable
	}
	c, err := s.member(handle.Conversation)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.record.Schema != 2 || c.record.Utility != nil || c.record.OwnerKind != "" || c.record.ToolPolicy == nil || c.record.Conversation != handle.Conversation || c.record.Generation != handle.Generation {
		return result, ErrUnavailable
	}
	policy := cloneToolPolicy(*c.record.ToolPolicy)
	if ValidateToolPolicy(policy) != nil {
		return result, ErrHeld
	}
	result.Settings = c.record.Settings
	if err := validateSettings(result.Settings); err != nil {
		return WorkflowObservation{}, err
	}
	result.Retained = &policy
	result.AttachmentID = a.id
	result.Sequence = sequence
	result.Loaded = c.record.Loaded && c.worker != nil
	result.Pending = c.record.PendingSettings != nil || c.record.PendingToolPolicy != nil
	result.Held = c.record.HeldReason != ""
	held, err := c.receipts.held()
	if err != nil {
		return result, err
	}
	result.Held = result.Held || held
	c.activeMu.Lock()
	callbacks, unknown := c.callbackCount, c.callbackUnknown
	c.activeMu.Unlock()
	result.Held = result.Held || unknown
	if !result.Loaded {
		return result, ErrUnavailable
	}
	var native workerWorkflowObservation
	if err := c.worker.Call(ctx, "workflow_observe", struct{}{}, &native); err != nil {
		return result, err
	}
	if native.Proof == nil || native.Pending == nil || native.Streaming == nil || native.HostCalls == nil || native.Running == nil || native.ActiveCalls == nil || native.Held == nil || native.HostTools == nil || !hostToolsValid(native.HostTools) || !slices.Equal(native.HostTools, c.hostTools) || native.NativeID != handle.Conversation.NativeID || native.Model != c.record.Settings.Model || native.Provider != c.record.Settings.Provider || native.Thinking != c.record.Settings.Thinking {
		return result, ErrUnavailable
	}
	result.Pending = result.Pending || *native.Pending
	result.Proof = *native.Proof
	result.Held = result.Held || *native.Held
	if ValidateToolPolicyProof(result.Proof, policy, handle.Generation) != nil {
		return result, ErrHeld
	}
	result.HostTools = []string{}
	for _, name := range c.hostTools {
		if !slices.Contains(a.tools, name) {
			return result, ErrUnavailable
		}
		result.HostTools = append(result.HostTools, name)
	}
	check := func() (bool, error) { return s.unexpectedChildrenFor(c.record.WorkerPID) }
	if s.childrenCheck != nil {
		check = s.childrenCheck
	}
	unexpected, err := check()
	if err != nil {
		return result, err
	}
	resourcesIdle, err := s.workflowResourcesIdle(ctx)
	if err != nil {
		return result, err
	}
	result.Idle = !result.Pending && !result.Held && !*native.Pending && !*native.Streaming && *native.HostCalls == 0 && !*native.Running && *native.ActiveCalls == 0 && callbacks == 0 && !unexpected && resourcesIdle
	s.workflow.mu.Lock()
	defer s.workflow.mu.Unlock()
	if s.workflow.overflow || s.workflow.pending != 0 || s.workflow.sequence != sequence || s.workflow.attachment != a || !a.workflowReady {
		return WorkflowObservation{}, ErrUnavailable
	}
	return result, nil
}
func (s *supervisor) workflowResourcesIdle(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.stateRoot, "resources"))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if len(entries) > 4096 {
		return false, ErrHeld
	}
	var history *runtimeRecords
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var record resourceRecord
		if err := readPrivate(filepath.Join(s.stateRoot, "resources", entry.Name()), &record); err != nil {
			return false, err
		}
		handle := record.Status.Handle
		if record.Schema != 1 || !operationPattern.MatchString(handle.ResourceID) || !operationPattern.MatchString(handle.OperationID) || entry.Name() != handle.ResourceID+".json" || record.Request.ResourceID != handle.ResourceID || record.Request.OperationID != handle.OperationID || record.Request.Digest != ResourceDigest(handle.Runtime, record.Request) {
			return false, ErrScope
		}
		if handle.Runtime != s.ref {
			if history == nil {
				records, err := s.workflowRuntimeHistory()
				if err != nil {
					return false, err
				}
				history = &records
			}
			original, ok := history.Requests[handle.Runtime.RequestID]
			if !ok || original.Runtime != handle.Runtime || record.Status.Phase != "stopped" || record.Status.ExitCode == nil {
				return false, ErrHeld
			}
		}
		if record.Status.Phase == "stopped" && record.Status.ExitCode == nil {
			return false, ErrHeld
		}
		if record.Status.Phase != "stopped" && record.Status.Phase != "rejected" {
			return false, nil
		}
	}
	return true, nil
}

// Historical terminal rows use the same strict immutable startup/history proof
// as explicit succession. An old numeric process ID is not a new native proof.
func (s *supervisor) workflowRuntimeHistory() (runtimeRecords, error) {
	if !s.reclaimControl || s.verifiedInstanceDirectory == "" {
		return runtimeRecords{}, ErrUnavailable
	}
	if err := s.validateSuccessorStartup(s.verifiedInstanceDirectory); err != nil {
		return runtimeRecords{}, err
	}
	m := Manager{config: Config{UserStateRoot: s.config.UserStateRoot, HostStateRoot: filepath.Dir(filepath.Dir(s.verifiedInstanceDirectory)), AuthDirectory: s.config.AuthDirectory, AccountBindings: s.config.AccountBindings}}
	records, err := m.records(s.ref.Session)
	if err != nil {
		return runtimeRecords{}, err
	}
	if records.Requests[records.CurrentRequestID].Runtime != s.ref {
		return runtimeRecords{}, ErrScope
	}
	return records, nil
}

func (m *Manager) ObserveWorkflow(ctx context.Context, handle agentbackend.ExecutionHandle) (WorkflowObservation, error) {
	var result WorkflowObservation
	if err := m.toolPolicyCall(ctx, handle, "workflow_observe", toolPolicyRequest{}, &result); err != nil {
		return result, err
	}
	if validateSettings(result.Settings) != nil || result.Handle != handle || !uuidPattern.MatchString(result.AttachmentID) || result.Sequence == 0 || result.Retained == nil || ValidateToolPolicyProof(result.Proof, *result.Retained, handle.Generation) != nil || !hostToolsValid(result.HostTools) || (result.Idle && (!result.Loaded || result.Pending || result.Held)) {
		return WorkflowObservation{}, ErrScope
	}
	return result, nil
}

var _ WorkflowObserver = (*Manager)(nil)

func (s *supervisor) acceptWorkflowACK(a *hostAttachment, frame hostFrame) bool {
	s.workflow.mu.Lock()
	defer s.workflow.mu.Unlock()
	if s.workflow.attachment != a || !a.workflow || frame.Schema != 1 || frame.Runtime != s.ref || frame.AttachmentID != a.id || frame.Event != "workflow_invalidated_ack" || !uuidPattern.MatchString(frame.ID) || frame.Call != nil || frame.Reply != nil || frame.RequiresACK || frame.Sequence == 0 {
		return false
	}
	if !a.workflowReady && frame.ID == a.setupID && frame.Sequence == a.setupSequence {
		a.workflowReady = true
		_, _, err := s.workflowAdvanceLocked(false)
		return err == nil
	}
	waiter, ok := a.workflowACKs[frame.ID]
	if !a.workflowReady || !ok || waiter.sequence != frame.Sequence {
		return false
	}
	delete(a.workflowACKs, frame.ID)
	close(waiter.received)
	return true
}
func (s *supervisor) writeWorkflowEvents(a *hostAttachment) {
	for {
		select {
		case <-a.closed:
			return
		case frame := <-a.workflowEvents:
			data, err := marshalWire(frame)
			if err != nil {
				_ = a.connection.Close()
				return
			}
			a.writeMu.Lock()
			_ = a.connection.SetWriteDeadline(time.Now().Add(workflowACKTimeout))
			_, err = a.connection.Write(data)
			_ = a.connection.SetWriteDeadline(time.Time{})
			a.writeMu.Unlock()
			if err != nil {
				_ = a.connection.Close()
				return
			}
		}
	}
}
