package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// LifecycleSlot comes from the original creation/Add/fork reservation. NativeID
// may be absent for a creating slot; only its owning saved mapping supplies it.
type LifecycleSlot struct {
	Member, Incarnation, CreationID, InitialInputID, NativeID, State string
	InitialSettings                                                  EffectiveSettings
}
type LifecycleMember struct {
	Generation   string
	Slot         LifecycleSlot
	Conversation *agentbackend.ConversationRef `json:",omitempty"`
	Settings     *EffectiveSettings            `json:",omitempty"`
	HistoryRef   string                        `json:",omitempty"`
}

// CaptureLifecycleMembers is metadata-only. It never creates/loads a missing
// slot, and rejects native mappings not covered by the captured reservations.
func (m *Manager) CaptureLifecycleMembers(ctx context.Context, ref RuntimeRef, slots []LifecycleSlot) ([]LifecycleMember, error) {
	if len(slots) == 0 || len(slots) > 64 {
		return nil, ErrScope
	}
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
	covered := map[string]bool{}
	result := make([]LifecycleMember, 0, len(slots))
	for _, slot := range slots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if (slot.State != "ready" && slot.State != "creating") || !memberPattern.MatchString(slot.Member) || !hexPattern.MatchString(slot.Incarnation) || !creationPattern.MatchString(slot.CreationID) || slot.InitialInputID != "" && !operationPattern.MatchString(slot.InitialInputID) || validateSettings(slot.InitialSettings) != nil {
			return nil, ErrScope
		}
		root := filepath.Join(directory, "members", slot.Member, slot.Incarnation)
		if covered[root] {
			return nil, ErrScope
		}
		covered[root] = true
		member := LifecycleMember{Slot: slot}
		var record conversationRecord
		if err := readPrivate(filepath.Join(root, "conversation.json"), &record); errors.Is(err, os.ErrNotExist) {
			if slot.NativeID != "" {
				return nil, ErrConflict
			}
		} else if err != nil {
			return nil, err
		} else {
			actual := record.Conversation
			if actual.Session != ref.Session || actual.Member != slot.Member || actual.Incarnation != slot.Incarnation || slot.NativeID != "" && actual.NativeID != slot.NativeID || record.CreationID != slot.CreationID || record.InitialSettings != slot.InitialSettings || validateConversationRecord(record, actual, root) != nil {
				return nil, ErrScope
			}
			if record.Generation != ref.Generation {
				ancestor := false
				for id := records.CurrentRequestID; id != ""; id = records.Requests[id].Start.ReplacesRequestID {
					if records.Requests[id].Runtime.Generation == record.Generation {
						ancestor = true
						break
					}
				}
				if !ancestor || slot.State != "creating" || record.Loaded {
					return nil, ErrScope
				}
			}
			member.Generation = record.Generation
			member.Conversation = &actual
			settings := record.Settings
			member.Settings = &settings
			member.HistoryRef = record.HistoryRef
		}
		result = append(result, member)
	}
	owners, err := settledDirectory(filepath.Join(directory, "members"))
	if err != nil {
		return nil, err
	}
	for _, owner := range owners {
		incarnations, err := settledDirectory(filepath.Join(directory, "members", owner.Name()))
		if err != nil {
			return nil, err
		}
		for _, inc := range incarnations {
			root := filepath.Join(directory, "members", owner.Name(), inc.Name())
			if !covered[root] {
				return nil, ErrScope
			}
		}
	}
	return result, nil
}

// CheckLifecycle observes normal retirement obligations while allowing a known
// accepted active turn. Explicit archive may request its existing native abort.
func (m *Manager) CheckLifecycle(ctx context.Context, ref RuntimeRef, members []agentbackend.ConversationRef) error {
	records, err := m.records(ref.Session)
	if err != nil {
		return err
	}
	current := records.Requests[records.CurrentRequestID]
	if current.Runtime != ref {
		return ErrScope
	}
	if _, _, err := m.upgradeMembers(ctx, UpgradeRequest{Start: current.Start, ExpectedGeneration: ref.Generation, Members: members}, records); err != nil {
		return err
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil {
		return err
	}
	if status.Phase != "ready" && status.Phase != "stopped" {
		return ErrHeld
	}
	return nil
}

// InterruptLifecycle uses the existing native cancellation operation only after
// receipts and retained obligations prove a known accepted input owner. Actual
// settlement remains Retire's responsibility; abort is not drain evidence.
func (m *Manager) InterruptLifecycle(ctx context.Context, ref RuntimeRef, members []agentbackend.ConversationRef, ensure func(context.Context, RuntimeRef, HostCallbackConnector) error) error {
	if err := m.CheckLifecycle(ctx, ref, members); err != nil {
		return err
	}
	status, err := m.Inspect(ctx, ref)
	if err != nil {
		return err
	}
	if status.Phase == "stopped" {
		return nil
	}
	if ensure == nil {
		return ErrScope
	}
	if err := ensure(ctx, ref, m); err != nil {
		return err
	}
	for _, member := range members {
		root, _ := stateDirectory(m.config.UserStateRoot, ref.Session)
		var record conversationRecord
		if err := readPrivate(filepath.Join(root, "members", member.Member, member.Incarnation, "conversation.json"), &record); err != nil {
			return err
		}
		if !record.Loaded {
			continue
		}
		handle := agentbackend.ExecutionHandle{Conversation: member, Generation: ref.Generation}
		var snapshot workerSnapshot
		if err := m.handleCall(ctx, handle, "snapshot", conversationRequest{}, &snapshot); err != nil {
			return err
		}
		if snapshot.Held {
			return ErrHeld
		}
		if snapshot.Busy {
			var result StopResult
			if err := m.handleCall(ctx, handle, "abort", conversationRequest{}, &result); err != nil {
				return err
			}
		}
	}
	return nil
}
