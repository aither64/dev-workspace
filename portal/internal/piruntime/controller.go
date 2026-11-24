package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
func (m *Manager) InspectNativeOrigin(ctx context.Context, handle agentbackend.ExecutionHandle, origin NativeOriginRef) (NativeOriginObservation, error) {
	var result NativeOriginObservation
	if err := validateNativeOrigin(origin); err != nil {
		return result, err
	}
	err := m.handleCall(ctx, handle, "inspect_origin", conversationRequest{Origin: &origin}, &result)
	if err == nil && (result.Handle != handle || result.Origin != origin || len(result.Arguments) > callbackLimit || !json.Valid(result.Arguments)) {
		return NativeOriginObservation{}, ErrScope
	}
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
	if err := m.verifyConversation(ctx, handle); err != nil {
		return backend.Operations{}, err
	}
	adapter := &conversationAdapter{manager: m, handle: handle}
	ops := backend.Operations{Verify: adapter, Read: adapter, Page: adapter, Send: adapter, Interrupt: adapter, Watch: adapter}
	available, err := m.activityAvailable(handle)
	if err != nil {
		return backend.Operations{}, err
	}
	if available {
		ops.Activity = adapter
	}
	return ops, nil
}

// A capability comes from the exact CURRENT frozen package. A positive claim
// always uses a fresh live request; operation/proof failure never falls back.
func (m *Manager) readConversationIdentity(ctx context.Context, handle agentbackend.ExecutionHandle) (workerIdentityProof, bool, error) {
	var proof workerIdentityProof
	if err := validateHandle(handle); err != nil {
		return proof, false, err
	}
	current, err := m.record(handle.Conversation.Session)
	if err != nil {
		return proof, false, err
	}
	if current.Runtime.Generation != handle.Generation {
		return proof, false, ErrScope
	}
	light, err := m.config.conversationIdentityContract(current.Start.AppStorePath)
	if err != nil || !light {
		return proof, light, err
	}
	selection, err := m.ReadInputSelection(ctx, handle)
	if err != nil {
		return proof, true, err
	}
	if selection.Runtime != current.Runtime {
		return proof, true, ErrConflict
	}
	if err := m.call(ctx, current.Runtime, "identity_v1", conversationRequest{Handle: handle}, &proof); err != nil {
		return proof, true, err
	}
	directory, err := stateDirectory(m.config.UserStateRoot, handle.Conversation.Session)
	if err != nil {
		return proof, true, err
	}
	history := filepath.Join(directory, "members", handle.Conversation.Member, handle.Conversation.Incarnation, "history")
	if proof.NativeID != handle.Conversation.NativeID || !filepath.IsAbs(proof.HistoryFile) || filepath.Clean(proof.HistoryFile) != proof.HistoryFile || filepath.Dir(proof.HistoryFile) != history || proof.Model != selection.Settings.Model || proof.Provider != selection.Settings.Provider || proof.Thinking != selection.Settings.Thinking || proof.InstructionsDigest != selection.InstructionsDigest {
		return workerIdentityProof{}, true, ErrScope
	}
	latest, err := m.ReadInputSelection(ctx, handle)
	if err != nil {
		return proof, true, err
	}
	if !reflect.DeepEqual(latest, selection) {
		return proof, true, ErrConflict
	}
	proof.Held = proof.Held || selection.Held || selection.PendingSettings || selection.PendingPolicy
	return proof, true, nil
}

func (m *Manager) verifyConversation(ctx context.Context, handle agentbackend.ExecutionHandle) error {
	if proof, light, err := m.readConversationIdentity(ctx, handle); err != nil || light {
		if err == nil && proof.Held {
			return ErrHeld
		}
		return err
	}
	// Retained workers keep the exact legacy snapshot path and its known
	// history-linear cost. A new portal never upgrades the retained process.
	var snapshot workerSnapshot
	if err := m.handleCall(ctx, handle, "snapshot", conversationRequest{}, &snapshot); err != nil {
		return err
	}
	if snapshot.NativeID != handle.Conversation.NativeID {
		return ErrScope
	}
	return nil
}

// BindRead belongs to one resolved HTTP read, never a mutation or later refresh.
// Identity/admission remains fresh; only the immutable read result is reused.
func (m *Manager) BindRead(ctx context.Context, handle agentbackend.ExecutionHandle) (backend.Operations, bool, error) {
	if err := validateHandle(handle); err != nil {
		return backend.Operations{}, false, err
	}
	record, err := m.record(handle.Conversation.Session)
	if err != nil {
		return backend.Operations{}, false, err
	}
	if record.Runtime.Generation != handle.Generation {
		return backend.Operations{}, false, ErrScope
	}
	inputDelivery, err := m.InputDelivery(ctx, handle)
	if err != nil {
		return backend.Operations{}, false, err
	}
	adapter := &conversationAdapter{manager: m, handle: handle, readRuntime: &record.Runtime, inputDelivery: &inputDelivery}
	ops := backend.Operations{Verify: adapter, Read: adapter, Page: adapter, Watch: adapter}
	available, err := m.activityAvailable(handle)
	if err != nil {
		return backend.Operations{}, false, err
	}
	if available {
		ops.Activity = adapter
	}
	return ops, inputDelivery, nil
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
var _ NativeOriginInspector = (*Manager)(nil)

// InputSelection is a local storage observation, never live execution proof.
// Policy absence is explicit and must not be replaced with a default policy.
type InputSelection struct {
	Runtime                        RuntimeRef
	Handle                         agentbackend.ExecutionHandle
	Settings                       EffectiveSettings
	Policy                         *ToolPolicyTarget
	InstructionsDigest             string
	CheckedInput, Held             bool
	PendingSettings, PendingPolicy bool
}

func decodeConversationCapability(data []byte, name string) (bool, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields, 1<<20); err != nil || fields == nil {
		return false, ErrScope
	}
	raw, exists := fields[name]
	if !exists {
		return false, nil
	}
	var version int
	if strictDecode(raw, &version, 32) != nil || version != 1 {
		return false, ErrScope
	}
	return true, nil
}

func readConversationCapability(app, name string) (bool, error) {
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
	return decodeConversationCapability(data, name)
}

func readConversationIdentityContract(app string) (bool, error) {
	return readConversationCapability(app, "piConversationIdentity")
}
func readCheckedInputContract(app string) (bool, error) {
	return readConversationCapability(app, "piCheckedInput")
}

// ReadInputSelection performs atomic-file observations only. It neither waits
// for native runtime ownership nor turns a stored selection into live proof.
func (m *Manager) ReadInputSelection(ctx context.Context, handle agentbackend.ExecutionHandle) (InputSelection, error) {
	var result InputSelection
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validateHandle(handle); err != nil {
		return result, err
	}
	current, err := m.record(handle.Conversation.Session)
	if err != nil {
		return result, err
	}
	if current.Runtime.Generation != handle.Generation {
		return result, ErrScope
	}
	directory, err := stateDirectory(m.config.UserStateRoot, handle.Conversation.Session)
	if err != nil {
		return result, err
	}
	root := filepath.Join(directory, "members", handle.Conversation.Member, handle.Conversation.Incarnation)
	var record conversationRecord
	if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
		return result, err
	}
	if err := validateConversationRecord(record, handle.Conversation, root); err != nil {
		return result, err
	}
	if record.Generation != handle.Generation {
		if err := m.receiptGeneration(handle.Conversation.Session, record.Generation); err != nil {
			return result, err
		}
		return result, ErrUnavailable // legitimate successor has not loaded this owner
	}
	if record.PendingSettings != nil && (validateSettings(*record.PendingSettings) != nil || record.PendingSettings.Revision != record.Settings.Revision+1) {
		return result, ErrScope
	}
	if !record.Loaded {
		return result, ErrUnavailable
	}
	supported, err := m.config.checkedInputContract(current.Start.AppStorePath)
	if err != nil {
		return result, err
	}
	if supported {
		identity, identityErr := m.config.conversationIdentityContract(current.Start.AppStorePath)
		delivery, deliveryErr := m.config.inputDeliveryContract(current.Start.AppStorePath)
		if identityErr != nil || deliveryErr != nil || !identity || !delivery {
			return result, errors.Join(ErrScope, identityErr, deliveryErr)
		}
	}
	result = InputSelection{Runtime: current.Runtime, Handle: handle, Settings: record.Settings, InstructionsDigest: instructionsDigest(record.Instructions), CheckedInput: supported, Held: record.HeldReason != "", PendingSettings: record.PendingSettings != nil, PendingPolicy: record.PendingToolPolicy != nil}
	if record.ToolPolicy != nil {
		policy := cloneToolPolicy(*record.ToolPolicy)
		result.Policy = &policy
	}
	latest, err := m.record(handle.Conversation.Session)
	if err != nil || latest.Runtime != current.Runtime {
		return InputSelection{}, errors.Join(ErrConflict, err)
	}
	if err := ctx.Err(); err != nil {
		return InputSelection{}, err
	}
	return result, nil
}

func (m *Manager) receiptGeneration(ref agentbackend.SessionRef, generation string) error {
	records, err := m.records(ref)
	if err != nil {
		return err
	}
	for id := records.CurrentRequestID; id != ""; id = records.Requests[id].Start.ReplacesRequestID {
		if records.Requests[id].Runtime.Generation == generation {
			return nil
		}
	}
	return ErrScope
}
