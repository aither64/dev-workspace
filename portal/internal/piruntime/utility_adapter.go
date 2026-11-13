package piruntime

import (
	"context"

	"github.com/aither64/codex-web/conversation/backend"
)

type utilityReadRequest struct {
	Handle   UtilityExecutionHandle
	Original UtilitySubmissionRef
}
type utilityAdapter struct {
	manager  *Manager
	handle   UtilityExecutionHandle
	original UtilitySubmissionRef
}

func (a *utilityAdapter) snapshot(ctx context.Context, identity backend.Identity) (workerSnapshot, error) {
	var snapshot workerSnapshot
	if identity != UtilityIdentity(a.handle) {
		return snapshot, ErrScope
	}
	err := a.manager.utilityRead(ctx, a.original, func(runtime RuntimeRef, _ *conversation) error {
		if runtime.Generation != a.handle.Generation {
			return ErrScope
		}
		return a.manager.call(ctx, runtime, "utility_snapshot", utilityReadRequest{Handle: a.handle, Original: a.original}, &snapshot)
	})
	if err == nil && snapshot.NativeID != a.handle.Utility.NativeID {
		return workerSnapshot{}, ErrScope
	}
	return snapshot, err
}
func (a *utilityAdapter) Verify(ctx context.Context, identity backend.Identity) error {
	_, err := a.snapshot(ctx, identity)
	return err
}
func (a *utilityAdapter) Read(ctx context.Context, identity backend.Identity) (backend.Transcript, error) {
	snapshot, err := a.snapshot(ctx, identity)
	if err != nil {
		return backend.Transcript{}, err
	}
	return projectTranscript(snapshot), nil
}
func (a *utilityAdapter) Send(ctx context.Context, identity backend.Identity, request backend.SendRequest) (backend.SendResult, error) {
	if identity != UtilityIdentity(a.handle) {
		return backend.SendResult{Outcome: backend.NotSubmitted}, ErrScope
	}
	runtime, err := a.manager.utilityRuntime(a.handle)
	if err != nil {
		return backend.SendResult{Outcome: backend.Unknown}, err
	}
	var status UtilitySubmissionStatus
	err = a.manager.withUtility(ctx, runtime, a.handle.Utility, "send", func(grant UtilityGrant) error {
		if utilitySubmission(grant) != a.original || request != (backend.SendRequest{Message: grant.Input.Message, ClientUserMessageID: grant.LaunchOperationID, ActionContext: grant.Input.ActionContext, Retry: grant.Input.Retry}) {
			return ErrConflict
		}
		if err := a.manager.call(ctx, runtime, "utility_send", utilityRequest{Handle: a.handle, Grant: grant, Send: &request}, &status); err != nil {
			return err
		}
		if status.Schema != 2 || status.OwnerKind != "utility" || status.Ref != a.original || status.Input != grant.Input || status.Settings != grant.Settings {
			return ErrScope
		}
		return nil
	})
	if err != nil {
		return backend.SendResult{Outcome: backend.Unknown}, err
	}
	switch status.Phase {
	case "Accepted":
		if status.Witness == nil || status.Witness.SessionID != a.handle.Utility.NativeID || status.Witness.EntryID == "" || status.Witness.EffectiveDigest != MessageDigest(request.Message) {
			return backend.SendResult{Outcome: backend.Unknown}, ErrScope
		}
		return backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: a.original.OperationID, TurnID: status.Witness.EntryID}}, nil
	case "NotSubmitted":
		if status.Refusal == "" {
			return backend.SendResult{Outcome: backend.Unknown}, ErrScope
		}
		return backend.SendResult{Outcome: backend.NotSubmitted}, nil
	default:
		return backend.SendResult{Outcome: backend.Unknown}, nil
	}
}

func (s *supervisor) utilitySnapshot(ctx context.Context, request utilityReadRequest) (workerSnapshot, error) {
	var result workerSnapshot
	if request.Handle.Generation != s.ref.Generation || request.Original.Utility != request.Handle.Utility || !operationPattern.MatchString(request.Original.OperationID) || request.Original.Action != "send" || !hexPattern.MatchString(request.Original.Digest) {
		return result, ErrScope
	}
	c, err := s.utility(request.Handle.Utility)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.worker == nil || c.record.Generation != request.Handle.Generation {
		return result, ErrUnavailable
	}
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &result); err != nil {
		return result, err
	}
	if result.NativeID != request.Handle.Utility.NativeID {
		return workerSnapshot{}, ErrScope
	}
	c.activeMu.Lock()
	result.Held = result.Held || c.callbackUnknown || c.record.HeldReason != ""
	result.Busy = result.Busy || c.callbackCount != 0
	c.activeMu.Unlock()
	return result, nil
}
