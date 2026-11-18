package piruntime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func readActivityContract(app string) (bool, error) {
	if !storePattern.MatchString(app) {
		return false, ErrScope
	}
	file, err := os.Open(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if strictDecode(data, &fields, 1<<20) != nil || fields == nil {
		return false, ErrScope
	}
	raw, present := fields["piActivity"]
	if !present {
		return false, nil
	}
	var version int
	if strictDecode(raw, &version, 32) != nil || version != 1 {
		return false, ErrScope
	}
	return true, nil
}

func (m *Manager) activityAvailable(handle agentbackend.ExecutionHandle) (bool, error) {
	record, err := m.record(handle.Conversation.Session)
	if err != nil {
		return false, err
	}
	if record.Runtime.Generation != handle.Generation {
		return false, ErrScope
	}
	return m.config.activityContract(record.Start.AppStorePath)
}

func (a *conversationAdapter) ReadActivity(ctx context.Context, identity backend.Identity) (backend.ActivitySnapshot, error) {
	var result backend.ActivitySnapshot
	if identity != BackendIdentity(a.handle) {
		return result, ErrScope
	}
	if a.readRuntime != nil {
		if err := a.verifyReadRuntime(); err != nil {
			return result, err
		}
	}
	available, err := a.manager.activityAvailable(a.handle)
	if err != nil {
		return result, err
	}
	if !available {
		return result, ErrUnavailable
	}
	err = a.manager.handleCall(ctx, a.handle, "activity_v1", conversationRequest{}, &result)
	if err == nil && (result.ThreadID != identity.ConversationID || result.ObservedAtMS <= 0 || result.CoverageComplete ||
		(result.CurrentState != "idle" && result.CurrentState != "working" && result.CurrentState != "waiting" && result.CurrentState != "unclassified") ||
		result.WorkingMS < 0 || result.IdleMS < 0 || result.WaitingMS < 0 || result.UnclassifiedMS < 0) {
		return backend.ActivitySnapshot{}, ErrScope
	}
	return result, err
}

func (s *supervisor) activity(ctx context.Context, handle agentbackend.ExecutionHandle) (backend.ActivitySnapshot, error) {
	var result backend.ActivitySnapshot
	c, err := s.member(handle.Conversation)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.worker == nil {
		return result, ErrUnavailable
	}
	err = c.worker.Call(ctx, "activity_v1", struct{}{}, &result)
	if err == nil && result.ThreadID != handle.Conversation.NativeID {
		return backend.ActivitySnapshot{}, ErrScope
	}
	return result, err
}
