package piruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// LifecycleCallback retains identities and digests only. It is a conservative
// possible-forwarding set, never a delivery result or a permission to resume.
type LifecycleCallback struct {
	Conversation                   agentbackend.ConversationRef
	Input                          SubmissionRef
	Origin                         NativeOriginRef
	ArgumentsSHA256, MessageID, To string
}

// TerminalCallbacks joins the complete retained linear history to its Accepted
// input witnesses and the existing native-origin reader. It starts no work.
func (m *Manager) TerminalCallbacks(ctx context.Context, ref RuntimeRef, members []LifecycleMember, inspect bool, attach func(context.Context, RuntimeRef, HostCallbackConnector) error) ([]LifecycleCallback, error) {
	records, err := m.records(ref.Session)
	if err != nil {
		return nil, err
	}
	if records.Requests[records.CurrentRequestID].Runtime != ref {
		return nil, ErrScope
	}
	directory, err := stateDirectory(m.config.UserStateRoot, ref.Session)
	if err != nil {
		return nil, err
	}
	slots := make([]LifecycleSlot, 0, len(members))
	for _, member := range members {
		slots = append(slots, member.Slot)
	}
	currentMembers, err := m.CaptureLifecycleMembers(ctx, ref, slots)
	if err != nil || !reflect.DeepEqual(currentMembers, members) {
		return nil, errors.Join(ErrConflict, err)
	}
	if inspect {
		if attach == nil {
			return nil, fmt.Errorf("terminal callback attachment: %w", ErrUnavailable)
		}
		if err := attach(ctx, ref, m); err != nil {
			return nil, fmt.Errorf("terminal callback attachment: %w", err)
		}
	}
	bound := map[string]agentbackend.ConversationRef{}
	result := []LifecycleCallback{}
	for _, captured := range members {
		// Capture validated unloaded creating ancestors too. They retain the
		// ordinary all-owner settlement checks below, outside this live set.
		if captured.Conversation == nil || captured.Generation != ref.Generation {
			continue
		}
		member := *captured.Conversation
		root := filepath.Join(directory, "members", member.Member, member.Incarnation)
		if member.Session != ref.Session || bound[root].NativeID != "" {
			return nil, ErrScope
		}
		bound[root] = member
		var record conversationRecord
		if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
			return nil, err
		}
		if validateConversationRecord(record, member, root) != nil || record.Generation != ref.Generation || record.PendingSettings != nil || record.PendingToolPolicy != nil || (record.HeldReason != "" && record.HeldReason != "forwarded callback outcome unknown") {
			return nil, ErrHeld
		}
		engine := receiptEngine{root: filepath.Join(root, "submissions")}
		held, err := engine.held()
		if err != nil || held {
			return nil, ErrHeld
		}
		if !record.Persisted {
			if err := m.emptyLifecycleMember(ctx, ref, captured, record, root, inspect); err != nil {
				return nil, err
			}
			continue
		}
		calls, err := m.captureHistoryCallbacks(ctx, ref, record, records, inspect, false, m)
		if err != nil {
			return nil, err
		}
		for _, call := range calls {
			result = append(result, call.LifecycleCallback)
		}

	}
	// All other owners, utilities, registered resources and unknown receipts retain
	// their ordinary refusal. Only the captured conversation callback holds differ.
	if err := m.terminalLifecycleOtherOwners(ctx, records, bound); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) terminalLifecycleOtherOwners(ctx context.Context, records runtimeRecords, bound map[string]agentbackend.ConversationRef) error {
	directory, err := stateDirectory(m.config.UserStateRoot, records.Session)
	if err != nil {
		return err
	}
	for _, kind := range []string{"members", "utilities"} {
		owners, err := settledDirectory(filepath.Join(directory, kind))
		if err != nil {
			return err
		}
		for _, owner := range owners {
			incs, err := settledDirectory(filepath.Join(directory, kind, owner.Name()))
			if err != nil {
				return err
			}
			for _, inc := range incs {
				root := filepath.Join(directory, kind, owner.Name(), inc.Name())
				var record conversationRecord
				if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
					return err
				}
				if expected, ok := bound[root]; ok {
					if kind != "members" || record.Conversation != expected {
						return ErrScope
					}
					continue
				}
				if record.Loaded || record.HeldReason != "" || record.PendingSettings != nil || record.PendingToolPolicy != nil {
					return ErrHeld
				}
				held, err := (&receiptEngine{root: filepath.Join(root, "submissions")}).held()
				if err != nil || held {
					return ErrHeld
				}
				if kind == "utilities" {
					if record.Utility == nil || record.Utility.Session != records.Session || record.Utility.OperationID != owner.Name() || record.Utility.Incarnation != inc.Name() || validateUtilityRecord(record, *record.Utility, root) != nil {
						return ErrScope
					}
				} else if record.Conversation.Session != records.Session || record.Conversation.Member != owner.Name() || record.Conversation.Incarnation != inc.Name() || validateConversationRecord(record, record.Conversation, root) != nil {
					return ErrScope
				}
			}
		}
	}
	resources, err := settledDirectory(filepath.Join(directory, "resources"))
	if err != nil {
		return err
	}
	for _, entry := range resources {
		var record resourceRecord
		if err := readPrivate(filepath.Join(directory, "resources", entry.Name()), &record); err != nil {
			return err
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Name() != id+".json" || !operationPattern.MatchString(id) || record.Schema != 1 || record.Status.Handle.ResourceID != id || record.Request.ResourceID != id || record.Status.Handle.OperationID != record.Request.OperationID {
			return ErrScope
		}
		original, ok := records.Requests[record.Status.Handle.Runtime.RequestID]
		if !ok || original.Runtime != record.Status.Handle.Runtime || record.Request.Digest != ResourceDigest(original.Runtime, record.Request) {
			return ErrScope
		}
		if record.Status.Phase != "stopped" || record.Status.ExitCode == nil {
			return ErrHeld
		}
	}
	return ctx.Err()
}

// The sealed lifecycle capture may witness a positively initialized creating
// member with no submitted input. A persistent recheck alone is not settlement;
// the terminal-delete owner must separately establish exact Stop/drain proof.
func (m *Manager) emptyLifecycleMember(ctx context.Context, runtime RuntimeRef, saved LifecycleMember, record conversationRecord, root string, live bool) error {
	if saved.Conversation == nil || saved.Settings == nil || saved.Generation != runtime.Generation || saved.Slot.State != "creating" || !operationPattern.MatchString(saved.Slot.InitialInputID) || saved.Slot.CreationID != record.CreationID || saved.Slot.Member != record.Conversation.Member || saved.Slot.Incarnation != record.Conversation.Incarnation || saved.Slot.NativeID != "" && saved.Slot.NativeID != record.Conversation.NativeID || *saved.Conversation != record.Conversation || saved.HistoryRef != record.HistoryRef || *saved.Settings != record.Settings || saved.Slot.InitialSettings != record.InitialSettings || record.Settings != record.InitialSettings || record.Fork != nil || record.Persisted || record.HeldReason != "" || record.PendingSettings != nil || record.PendingToolPolicy != nil || record.Generation != runtime.Generation || validateConversationRecord(record, *saved.Conversation, root) != nil || record.HistoryFile == "" {
		return ErrHeld
	}
	entries, err := os.ReadDir(filepath.Join(root, "submissions"))
	if err != nil || len(entries) != 0 {
		return errors.Join(ErrHeld, err)
	}
	if _, err := os.Lstat(record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrHeld, err)
	}
	if !live {
		return nil
	}
	if !record.Loaded {
		return ErrHeld
	}
	handle := agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: runtime.Generation}
	proof, err := m.VerifyCreation(ctx, handle, record.CreationID, record.HistoryRef, nil)
	if err != nil || proof.Conversation != record.Conversation || proof.Generation != runtime.Generation || proof.CreationID != record.CreationID || proof.HistoryRef != record.HistoryRef || proof.Persisted || proof.Bootstrap != nil || proof.InstructionsDigest != instructionsDigest(record.Instructions) {
		return fmt.Errorf("empty creating member creation proof: %w", errors.Join(ErrHeld, err))
	}
	var snapshot workerSnapshot
	if err := m.handleCall(ctx, handle, "snapshot", conversationRequest{}, &snapshot); err != nil || snapshot.NativeID != record.Conversation.NativeID || snapshot.HistoryFile != record.HistoryFile || snapshot.Persisted || snapshot.Busy || snapshot.Held || snapshot.Model != record.Settings.Model || snapshot.Provider != record.Settings.Provider || snapshot.Thinking != record.Settings.Thinking || snapshot.InstructionsDigest != instructionsDigest(record.Instructions) {
		return fmt.Errorf("empty creating member snapshot proof: %w", errors.Join(ErrHeld, err))
	}
	observation, err := m.ObserveWorkflow(ctx, handle)
	if err != nil || !observation.Loaded || !observation.Idle || observation.Pending || observation.Held || observation.Settings != record.Settings || observation.Retained == nil || record.ToolPolicy == nil || !sameToolPolicy(*observation.Retained, *record.ToolPolicy) {
		return fmt.Errorf("empty creating member workflow proof: %w", errors.Join(ErrHeld, err))
	}
	var current conversationRecord
	if err := readPrivate(filepath.Join(root, "conversation.json"), &current); err != nil || !reflect.DeepEqual(current, record) {
		return errors.Join(ErrConflict, err)
	}
	// The live calls grant no durable mutation. Recheck absence and receipts
	// after them so a changed mapping/input cannot enter the sealed capture.
	return m.emptyLifecycleMember(ctx, runtime, saved, current, root, false)
}

// RetireTerminalCallbacks is used only by the original force-delete journal.
// It preserves Loaded/Held/Unknown audit and cannot be used by Upgrade or Load.
func (m *Manager) RetireTerminalCallbacks(ctx context.Context, ref RuntimeRef, members []LifecycleMember, saved []LifecycleCallback, attach func(context.Context, RuntimeRef, HostCallbackConnector) error) (RuntimeStatus, error) {
	result := RuntimeStatus{Runtime: ref, Phase: "unknown", ObservedAt: time.Now().UTC()}
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
	if current.Runtime != ref {
		return result, ErrScope
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil {
		return result, fmt.Errorf("terminal callback runtime proof: %w", err)
	}
	if status.Phase == "stopped" {
		if !validTerminalReceipt(current, status.Observation) {
			return result, ErrHeld
		}
		if err := m.config.verifyDrain(status.Observation); err != nil {
			return result, err
		}
	}
	// A stopping retry carries the original sealed capture. This read only
	// rechecks its persistent predicates; it never counts as terminal settlement.
	actual, err := m.TerminalCallbacks(ctx, ref, members, status.Phase == "ready", attach)
	if err != nil || len(saved) == 0 || !reflect.DeepEqual(actual, saved) {
		return result, errors.Join(ErrHeld, err)
	}
	if status.Phase != "stopped" {
		if status.Phase != "ready" && !status.Observation.StopRequested {
			return result, ErrHeld
		}
		stopper, ok := m.config.Host.(hostStopper)
		if !ok {
			return result, ErrUnavailable
		}
		observed, err := stopper.Stop(ctx, ref, status.Observation)
		if err != nil {
			return result, err
		}
		status, err = m.inspectObservation(ctx, current, observed)
		if err != nil {
			return result, err
		}
	}
	if !validTerminalReceipt(current, status.Observation) {
		return status, ErrHeld
	}
	if err := m.config.verifyDrain(status.Observation); err != nil {
		return status, err
	}
	final, err := m.TerminalCallbacks(ctx, ref, members, false, nil)
	if err != nil || !reflect.DeepEqual(final, saved) {
		return status, errors.Join(ErrHeld, err)
	}
	return status, nil
}
