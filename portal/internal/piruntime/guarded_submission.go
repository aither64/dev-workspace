package piruntime

import (
	"context"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// CheckedSubmissionSender consumes a ref already frozen in the owning domain
// handoff. It does not prepare a receipt, authorize a policy or replace a digest.
type CheckedSubmissionSender interface {
	SendCheckedSubmission(context.Context, agentbackend.ExecutionHandle, SubmissionRef, SendInput) (SubmissionStatus, error)
}
type checkedSubmissionRequest struct {
	Handle agentbackend.ExecutionHandle
	Ref    SubmissionRef
	Input  SendInput
}

func validateCheckedSubmission(handle agentbackend.ExecutionHandle, ref SubmissionRef, input SendInput) error {
	if validateHandle(handle) != nil || ref.Conversation != handle.Conversation || ref.Action != "send" || !operationPattern.MatchString(ref.OperationID) || !hexPattern.MatchString(ref.Digest) || !utf8.ValidString(input.Message) || !utf8.ValidString(input.ActionContext) {
		return ErrScope
	}
	return nil
}
func (m *Manager) SendCheckedSubmission(ctx context.Context, handle agentbackend.ExecutionHandle, ref SubmissionRef, input SendInput) (SubmissionStatus, error) {
	var result SubmissionStatus
	if err := validateCheckedSubmission(handle, ref, input); err != nil {
		return result, err
	}
	if _, err := sessionDirectory(handle.Conversation.Session); err != nil {
		return result, err
	}
	record, err := m.record(handle.Conversation.Session)
	if err != nil {
		return result, err
	}
	if record.Runtime.Generation != handle.Generation {
		return result, ErrScope
	}
	if err := m.call(ctx, record.Runtime, "send_checked", checkedSubmissionRequest{Handle: handle, Ref: ref, Input: input}, &result); err != nil {
		return result, err
	}
	if result.Schema != 1 || result.Ref != ref || result.Input != input || !hexPattern.MatchString(result.Generation) || validateSettings(result.Settings) != nil || SubmissionDigest(ref, input, result.Settings) != ref.Digest {
		return result, ErrScope
	}
	switch result.Phase {
	case "Accepted":
		witness := result.Witness
		if witness == nil || witness.SessionID != ref.Conversation.NativeID || witness.EntryID == "" || witness.EffectiveDigest != MessageDigest(input.Message) {
			return result, ErrHeld
		}
	case "NotSubmitted":
		if result.Refusal == "" {
			return result, ErrHeld
		}
	case "Unknown":
	default:
		return result, ErrHeld
	}
	return result, nil
}
func (s *supervisor) sendCheckedSubmission(ctx context.Context, request checkedSubmissionRequest) (SubmissionStatus, error) {
	if err := s.verifyHandle(request.Handle); err != nil {
		return SubmissionStatus{}, err
	}
	if err := validateCheckedSubmission(request.Handle, request.Ref, request.Input); err != nil {
		return SubmissionStatus{}, err
	}
	c, err := s.member(request.Handle.Conversation)
	if err != nil {
		return SubmissionStatus{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.workflowCheck(ctx); err != nil {
		return SubmissionStatus{}, err
	}
	if c.record.Generation != request.Handle.Generation || c.record.Conversation != request.Ref.Conversation {
		return SubmissionStatus{}, ErrScope
	}
	if SubmissionDigest(request.Ref, request.Input, c.record.Settings) != request.Ref.Digest {
		return SubmissionStatus{}, ErrConflict
	}
	status, err := s.sendOwned(ctx, c, receiptReference{retained: &request.Ref}, request.Input)
	return status.retained(), err
}

var _ CheckedSubmissionSender = (*Manager)(nil)
