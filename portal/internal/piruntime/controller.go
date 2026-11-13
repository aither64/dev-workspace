package piruntime

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func (m *Manager) call(ctx context.Context, ref RuntimeRef, operation string, body, result any) error {
	status, err := m.Inspect(ctx, ref)
	if err != nil {
		return err
	}
	if status.Phase != "ready" {
		return ErrHeld
	}
	return m.callRaw(ctx, ref, operation, body, result)
}
func (m *Manager) handleCall(ctx context.Context, handle agentbackend.ExecutionHandle, operation string, body conversationRequest, result any) error {
	if err := validateHandle(handle); err != nil {
		return err
	}
	if _, err := sessionDirectory(handle.Conversation.Session); err != nil {
		return err
	}
	record, err := m.record(handle.Conversation.Session)
	if err != nil {
		return err
	}
	if record.Runtime.Generation != handle.Generation {
		return ErrScope
	}
	body.Handle = handle
	return m.call(ctx, record.Runtime, operation, body, result)
}
func (m *Manager) Create(ctx context.Context, ref RuntimeRef, request CreateRequest) (ConversationState, error) {
	var result ConversationState
	if ref.Session != request.Session {
		return result, ErrScope
	}
	err := m.call(ctx, ref, "create", request, &result)
	return result, err
}
func (m *Manager) Load(ctx context.Context, handle agentbackend.ExecutionHandle) (ConversationState, error) {
	var result ConversationState
	err := m.handleCall(ctx, handle, "load", conversationRequest{}, &result)
	return result, err
}
func (m *Manager) Lookup(ctx context.Context, ref SubmissionRef) (SubmissionStatus, error) {
	var result SubmissionStatus
	if !operationPattern.MatchString(ref.OperationID) || ref.Action != "send" || !hexPattern.MatchString(ref.Digest) {
		return result, ErrScope
	}

	directory, err := stateDirectory(m.config.UserStateRoot, ref.Conversation.Session)
	if err != nil {
		return result, err
	}
	if err := validateHandle(agentbackend.ExecutionHandle{Conversation: ref.Conversation, Generation: strings.Repeat("0", 64)}); err != nil {
		return result, err
	}
	root := filepath.Join(directory, "members", ref.Conversation.Member, ref.Conversation.Incarnation)
	var mapping conversationRecord
	if err := readPrivate(filepath.Join(root, "conversation.json"), &mapping); err != nil {
		return result, err
	}
	if err := validateConversationRecord(mapping, ref.Conversation, root); err != nil {
		return result, err
	}
	return (&receiptEngine{root: filepath.Join(root, "submissions")}).lookup(ref)

}
func (m *Manager) Configure(ctx context.Context, handle agentbackend.ExecutionHandle, settings EffectiveSettings) (SettingsResult, error) {
	var result SettingsResult
	err := m.handleCall(ctx, handle, "configure", conversationRequest{Settings: settings}, &result)
	return result, err
}
func (m *Manager) Unload(ctx context.Context, handle agentbackend.ExecutionHandle) (StopResult, error) {
	var result StopResult
	err := m.handleCall(ctx, handle, "unload", conversationRequest{}, &result)
	return result, err
}
func (m *Manager) VerifyCreation(ctx context.Context, handle agentbackend.ExecutionHandle, creationID, historyRef string, bootstrap *SubmissionRef) (CreationProof, error) {
	var result CreationProof
	err := m.handleCall(ctx, handle, "verify_creation", conversationRequest{CreationID: creationID, HistoryRef: historyRef, Bootstrap: bootstrap}, &result)
	return result, err
}
func (m *Manager) Resources(ctx context.Context, ref RuntimeRef) (ResourceSnapshot, error) {
	var result ResourceSnapshot
	err := m.call(ctx, ref, "resources", struct{}{}, &result)
	return result, err
}
func (m *Manager) StartResource(ctx context.Context, ref RuntimeRef, request ResourceRequest) (ResourceStatus, error) {
	var result ResourceStatus
	err := m.call(ctx, ref, "start_resource", request, &result)
	return result, err
}
func (m *Manager) InspectResource(ctx context.Context, handle ResourceHandle) (ResourceStatus, error) {
	var result ResourceStatus
	err := m.call(ctx, handle.Runtime, "inspect_resource", handle, &result)
	return result, err
}
func (m *Manager) StopResource(ctx context.Context, handle ResourceHandle) (StopResult, error) {
	var result StopResult
	err := m.call(ctx, handle.Runtime, "stop_resource", handle, &result)
	return result, err
}
func (m *Manager) Bind(ctx context.Context, handle agentbackend.ExecutionHandle) (backend.Operations, error) {
	if err := validateHandle(handle); err != nil {
		return backend.Operations{}, err
	}
	var snapshot workerSnapshot
	if err := m.handleCall(ctx, handle, "snapshot", conversationRequest{}, &snapshot); err != nil {
		return backend.Operations{}, err
	}
	if snapshot.NativeID != handle.Conversation.NativeID {
		return backend.Operations{}, ErrScope
	}
	adapter := &conversationAdapter{manager: m, handle: handle}
	return backend.Operations{Verify: adapter, Read: adapter, Page: adapter, Send: adapter, Interrupt: adapter, Watch: adapter}, nil
}

// Deliver is the team/queue adapter's single trusted ordinary input path.
func (m *Manager) Deliver(ctx context.Context, handle agentbackend.ExecutionHandle, request backend.SendRequest) (backend.SendResult, error) {
	operations, err := m.Bind(ctx, handle)
	if err != nil {
		return backend.SendResult{}, err
	}
	return operations.Send.Send(ctx, BackendIdentity(handle), request)
}

var _ Controller = (*Manager)(nil)
var _ CreationVerifier = (*Manager)(nil)
