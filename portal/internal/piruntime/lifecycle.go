package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// VerifyLifecycleTerminal is read-only eligibility for an explicit candidate
// deletion recovery. Ordinary callback finalization retains its saved audit.
func (m *Manager) VerifyLifecycleTerminal(ctx context.Context, ref RuntimeRef, saved HostObservation) error {
	directory, err := stateDirectory(m.config.UserStateRoot, ref.Session)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Join(directory, "runtime.lock")); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.Join(ErrHeld, err)
	}
	unlock, err := runtimeLock(directory)
	if err != nil {
		return err
	}
	defer unlock()
	records, err := m.records(ref.Session)
	if err != nil {
		return err
	}
	current := records.Requests[records.CurrentRequestID]
	if current.Runtime != ref || current.Phase != "starting" || !validTerminalReceipt(current, saved) {
		return ErrScope
	}
	if err := m.assertRuntimeSettled(ctx, records); err != nil {
		return err
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil || status.Phase != "stopped" || !validTerminalReceipt(current, status.Observation) || !reflect.DeepEqual(status.Observation, saved) {
		return errors.Join(ErrHeld, err)
	}
	return m.config.verifyDrain(status.Observation)
}

// Retire ends only the captured current runtime. The caller owns admitted
// lifecycle/session EX and retains this exact tuple in its original operation.
// It does not reserve a successor or remove any retained state. Success means
// stopped with positive current drain; pending/uncertain Stop stays retryable.
func (m *Manager) Retire(ctx context.Context, ref RuntimeRef, members []agentbackend.ConversationRef, ensureAttachment func(context.Context, RuntimeRef, HostCallbackConnector) error) (RuntimeStatus, error) {
	result := RuntimeStatus{Runtime: ref, Phase: "unknown", ObservedAt: time.Now().UTC()}
	if ensureAttachment == nil {
		return result, ErrScope
	}
	directory, err := stateDirectory(m.config.UserStateRoot, ref.Session)
	if err != nil {
		return result, err
	}
	unlock, err := runtimeLock(directory)
	if err != nil {
		return result, err
	}
	defer unlock()
	records, err := m.records(ref.Session)
	if err != nil {
		return result, err
	}
	current := records.Requests[records.CurrentRequestID]
	if current.Runtime != ref || current.Phase != "starting" {
		return result, ErrScope
	}
	// Reuse ordinary bound-member admission, with no Unknown retirement exception.
	request := UpgradeRequest{Start: current.Start, ExpectedGeneration: ref.Generation, Members: members}
	_, captured, err := m.upgradeMembers(ctx, request, records)
	if err != nil {
		return result, err
	}
	stopper, ok := m.config.Host.(hostStopper)
	if !ok {
		return result, ErrUnavailable
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil {
		return result, err
	}
	result = status
	if status.Phase == "stopped" {
		// A terminal unit is not authority to clear a crashed loaded owner.
		if err := m.assertRuntimeSettled(ctx, records); err != nil {
			return result, err
		}
		if !validTerminalReceipt(current, status.Observation) {
			return result, ErrHeld
		}
		if err := m.config.verifyDrain(status.Observation); err != nil {
			return result, err
		}
		return result, nil
	}
	loaded := false
	for _, member := range captured {
		loaded = loaded || member.record.Loaded
	}
	if loaded {
		if status.Phase != "ready" {
			return result, ErrHeld
		}
		if err := m.ensureLifecycleAttachment(ctx, ref, members, ensureAttachment); err != nil {
			return result, err
		}
		// Validate every loaded member before disposing of the first one. The shared
		// Unload fence also rechecks live callbacks and sibling work at each detach.
		for _, member := range captured {
			record := member.record
			if !record.Loaded {
				continue
			}
			handle := agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: ref.Generation}
			var snapshot workerSnapshot
			if err := m.callRaw(ctx, ref, "snapshot", conversationRequest{Handle: handle}, &snapshot); err != nil {
				return result, err
			}
			if snapshot.Busy || snapshot.Held || snapshot.NativeID != record.Conversation.NativeID || snapshot.HistoryFile != record.HistoryFile || snapshot.Persisted != record.Persisted || snapshot.Model != record.Settings.Model || snapshot.Provider != record.Settings.Provider || snapshot.Thinking != record.Settings.Thinking || snapshot.InstructionsDigest != instructionsDigest(record.Instructions) {
				return result, ErrHeld
			}
			observation, err := m.ObserveWorkflow(ctx, handle)
			if err != nil {
				return result, err
			}
			if !observation.Loaded || !observation.Idle || observation.Pending || observation.Held || observation.Settings != record.Settings || observation.Retained == nil || record.ToolPolicy == nil || !sameToolPolicy(*observation.Retained, *record.ToolPolicy) {
				return result, ErrHeld
			}
		}
		for _, member := range captured {
			if !member.record.Loaded {
				continue
			}
			handle := agentbackend.ExecutionHandle{Conversation: member.record.Conversation, Generation: ref.Generation}
			stopped, err := m.Unload(ctx, handle)
			if err != nil {
				return result, err
			}
			physical := stopped.Outcome == "stopped" && stopped.VerifiedExit
			// This marker is the owning conversation codec's persisted protocol2 fact.
			detached := member.record.WorkerTopology == "shared-session-v1" && stopped.Outcome == "detached" && stopped.VerifiedDetach && !stopped.VerifiedExit
			if !physical && !detached {
				return result, ErrHeld
			}
		}
	}
	fresh, err := m.records(ref.Session)
	if err != nil {
		return result, err
	}
	if fresh.Requests[fresh.CurrentRequestID].Runtime != ref {
		return result, ErrScope
	}
	if err := m.assertRuntimeSettled(ctx, fresh); err != nil {
		return result, err
	}
	for _, member := range captured {
		root := filepath.Join(directory, "members", member.record.Conversation.Member, member.record.Conversation.Incarnation)
		var now conversationRecord
		if err := readPrivate(filepath.Join(root, "conversation.json"), &now); err != nil {
			return result, err
		}
		if now.Loaded || now.Generation != ref.Generation || !sameUpgradeMember(member.record, now) {
			return result, ErrConflict
		}
	}
	// No launch.lock is held here: the exact host Stop owns it. A previous Stop
	// can already be in progress, but only its original boot/invocation may recur.
	status, err = m.Inspect(ctx, ref)
	if err != nil {
		return result, err
	}
	if status.Phase == "stopped" {
		if !validTerminalReceipt(current, status.Observation) {
			return status, ErrHeld
		}
		if err := m.config.verifyDrain(status.Observation); err != nil {
			return status, err
		}
		return status, nil
	}
	if status.Phase != "ready" && !status.Observation.StopRequested {
		return status, ErrHeld
	}
	if !sameExecution(result.Observation, status.Observation) && !status.Observation.StopRequested {
		return status, ErrScope
	}
	observed, err := stopper.Stop(ctx, ref, status.Observation)
	if err != nil {
		return RuntimeStatus{Runtime: ref, Phase: "unknown", Observation: observed, ObservedAt: time.Now().UTC()}, err
	}
	result, err = m.inspectObservation(ctx, current, observed)
	if err != nil {
		return result, err
	}
	if !validTerminalReceipt(current, observed) {
		return result, ErrHeld
	}
	if err := m.config.verifyDrain(observed); err != nil {
		return result, err
	}
	return result, nil
}
