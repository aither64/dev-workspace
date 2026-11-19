package web

import (
	"context"
	"errors"
	"os"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// Faults surround actual address-scoped fixture effects, not callback results.
type piCallbackTeamFixture struct {
	*piAddedTeamFixture
	failure   string
	cancel    context.CancelFunc
	sendCalls int
}

func (f *piCallbackTeamFixture) Create(ctx context.Context, r piruntime.RuntimeRef, q piruntime.CreateRequest) (piruntime.ConversationState, error) {
	state, err := f.piAddedTeamFixture.Create(ctx, r, q)
	if err == nil && f.failure == "create_unknown" {
		return state, errors.New("fixture lost Create acknowledgment")
	}
	return state, err
}
func (f *piCallbackTeamFixture) VerifyCreation(ctx context.Context, h agentbackend.ExecutionHandle, id, history string, input *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	proof, err := f.piAddedTeamFixture.VerifyCreation(ctx, h, id, history, input)
	if h.Conversation.Member != "lead" {
		if f.failure == "proof_unknown" {
			return proof, piruntime.ErrHeld
		}
		if f.failure == "cancel_after_create" {
			f.cancel()
		}
	}
	return proof, err
}
func (f *piCallbackTeamFixture) InspectToolPolicy(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
	if h.Conversation.Member != "lead" && (f.failure == "policy_held" || f.failure == "cancel_after_create") {
		return piruntime.ToolPolicyProof{}, piruntime.ErrHeld
	}
	return f.piAddedTeamFixture.InspectToolPolicy(ctx, h)
}
func (f *piCallbackTeamFixture) Bind(ctx context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	ops, err := f.piAddedTeamFixture.Bind(ctx, h)
	if err == nil && h.Conversation.Member != "lead" {
		ops.Send = piCallbackSend{f, ops.Send}
	}
	return ops, err
}

type piCallbackSend struct {
	owner  *piCallbackTeamFixture
	sender backend.Sender
}

func (s piCallbackSend) Send(ctx context.Context, id backend.Identity, q backend.SendRequest) (backend.SendResult, error) {
	s.owner.sendCalls++
	if s.owner.failure == "send_unknown" {
		return backend.SendResult{Outcome: backend.Unknown}, errors.New("fixture lost Send acknowledgment")
	}
	return s.sender.Send(ctx, id, q)
}

// Address-scoped extension of the existing isolated creation/settings fixture.
// These fake ports prove adapter joins, not native SDK execution.
type piAddedTeamFixture struct {
	*piSettingsFixture
	slots    map[string]*piSettingsFixture
	creation map[string]string
	receipts map[string]piruntime.SubmissionStatus
}

func (f *piAddedTeamFixture) slot(member string) *piSettingsFixture {
	if member == "lead" {
		return f.piSettingsFixture
	}
	return f.slots[member]
}
func (f *piAddedTeamFixture) Create(ctx context.Context, r piruntime.RuntimeRef, q piruntime.CreateRequest) (piruntime.ConversationState, error) {
	if old := f.slot(q.Member); old != nil {
		return old.state, nil
	}
	native := &piCreationRuntimeFixture{outcome: "Accepted", runtime: r, start: f.start}
	state, err := native.Create(ctx, r, q)
	if err != nil {
		return state, err
	}
	native.state.Conversation.NativeID = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	f.slots[q.Member] = &piSettingsFixture{piObservedFixture: &piObservedFixture{piCreationRuntimeFixture: native, idle: true, current: r}}
	f.creation[q.Member] = q.CreationID
	return native.state, nil
}
func (f *piAddedTeamFixture) ReadMemberAdmission(_ context.Context, r agentbackend.ConversationRef, creation, input string) (piruntime.ConversationState, piruntime.SubmissionStatus, error) {
	x := f.slot(r.Member)
	if x == nil {
		return piruntime.ConversationState{}, piruntime.SubmissionStatus{}, os.ErrNotExist
	}
	if creation != f.creation[r.Member] || r.Incarnation != x.state.Conversation.Incarnation || r.NativeID != "" && r.NativeID != x.state.Conversation.NativeID {
		return piruntime.ConversationState{}, piruntime.SubmissionStatus{}, piruntime.ErrScope
	}
	if x.submission != nil && x.submission.Ref.OperationID == input {
		return x.state, *x.submission, nil
	}
	if receipt, ok := f.receipts[input]; ok && receipt.Ref.Conversation == x.state.Conversation {
		return x.state, receipt, nil
	}
	return x.state, piruntime.SubmissionStatus{}, os.ErrNotExist
}
func (f *piAddedTeamFixture) VerifyCreation(ctx context.Context, h agentbackend.ExecutionHandle, id, history string, input *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	x := f.slot(h.Conversation.Member)
	if x == nil {
		return piruntime.CreationProof{}, piruntime.ErrScope
	}
	return x.VerifyCreation(ctx, h, id, history, input)
}
func (f *piAddedTeamFixture) InspectToolPolicy(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
	x := f.slot(h.Conversation.Member)
	if x == nil {
		return piruntime.ToolPolicyProof{}, piruntime.ErrScope
	}
	return x.InspectToolPolicy(ctx, h)
}
func (f *piAddedTeamFixture) Bind(ctx context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	x := f.slot(h.Conversation.Member)
	if x == nil {
		return backend.Operations{}, piruntime.ErrScope
	}
	ops, err := x.Bind(ctx, h)
	ops.Send = piAddedSend{f, x}
	return ops, err
}
func (f *piAddedTeamFixture) Lookup(ctx context.Context, r piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	if receipt, ok := f.receipts[r.OperationID]; ok {
		if receipt.Ref != r {
			return receipt, piruntime.ErrConflict
		}
		return receipt, nil
	}
	return f.piSettingsFixture.Lookup(ctx, r)
}

type piAddedSend struct {
	owner *piAddedTeamFixture
	slot  *piSettingsFixture
}

func (s piAddedSend) Send(ctx context.Context, id backend.Identity, q backend.SendRequest) (backend.SendResult, error) {
	if receipt, ok := s.owner.receipts[q.ClientUserMessageID]; ok {
		if receipt.Input.Message != q.Message || receipt.Input.ActionContext != q.ActionContext {
			return backend.SendResult{}, piruntime.ErrConflict
		}
		return backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: q.ClientUserMessageID, TurnID: receipt.Witness.EntryID}}, nil
	}
	original := s.slot.submission
	_, err := s.slot.piCreationRuntimeFixture.Send(ctx, id, q)
	if err != nil {
		return backend.SendResult{}, err
	}
	receipt := *s.slot.submission
	s.owner.receipts[q.ClientUserMessageID] = receipt
	if original != nil {
		s.slot.submission = original
	}
	s.slot.state.Persisted = true
	return backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: q.ClientUserMessageID, TurnID: receipt.Witness.EntryID}}, nil
}

type piAddUnavailableAttachment struct{ *piSettingsFixture }

func (piAddUnavailableAttachment) AttachHost(context.Context, piruntime.RuntimeRef, piruntime.HostCallbackHandler) error {
	return piruntime.ErrUnavailable
}
