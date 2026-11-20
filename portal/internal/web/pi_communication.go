package web

import (
	"context"
	"errors"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// Service owns sorted runtime pair locks before these callbacks. Reuse only
// locked lead/creation/policy proofs, never the same-session team delivery API.
type communicationPiTransport struct{ server *Server }

func (p communicationPiTransport) current(ctx context.Context, e communication.EndpointRecord) (piSessionAdmission, error) {
	if p.server.config.PiCreation == nil || (piTeamService{p.server}).runtime() == nil {
		return piSessionAdmission{}, piruntime.ErrUnavailable
	}
	if e.Validate() != nil || e.Pi == nil || e.Pi.Session.Workspace != p.server.config.Workspace {
		return piSessionAdmission{}, piruntime.ErrScope
	}
	current, err := p.server.piSessionAdmissionLockedContext(ctx, e.Pi.Session.Ref())
	if err != nil {
		return current, err
	}
	lead, err := current.Binding.Resolve("lead")
	if err != nil || lead != e.Conversation() {
		return current, errors.Join(agentbackend.ErrBindingConflict, err)
	}
	return current, nil
}

func (p communicationPiTransport) Prepare(ctx context.Context, e communication.EndpointRecord) (communication.PiPreparation, error) {
	current, err := p.current(ctx, e)
	if err != nil {
		return communication.PiPreparation{}, err
	}
	handle := agentbackend.ExecutionHandle{Conversation: e.Conversation(), Generation: current.Authority.Generation}
	if _, err := p.server.piSavedAccessLocked(current, handle.Conversation); err != nil {
		return communication.PiPreparation{}, err
	}
	participant, err := (piTeamService{p.server}).participant(ctx, current, teamruntime.PiRoster{}, handle, "", false, true)
	if err != nil || participant.Policy.Validate() != nil || participant.State != "ready" || !participant.Persisted || participant.Held || !participant.Loaded || !participant.SettingsApplied ||
		(participant.Activity != "idle" && participant.Activity != "working" && participant.Activity != "streaming") {
		return communication.PiPreparation{}, errors.Join(piruntime.ErrHeld, err)
	}
	runtime := (piTeamService{p.server}).runtime()
	operations, err := runtime.Bind(ctx, handle)
	if err != nil || operations.Activity == nil {
		return communication.PiPreparation{}, errors.Join(piruntime.ErrUnavailable, err)
	}
	activity, err := operations.Activity.ReadActivity(ctx, piruntime.BackendIdentity(handle))
	if err != nil || activity.ThreadID != handle.Conversation.NativeID || activity.Stale || activity.WaitReason != "" || len(activity.BlockingRequests) != 0 {
		return communication.PiPreparation{}, errors.Join(piruntime.ErrHeld, err)
	}
	settings := participant.Settings
	return communication.PiPreparation{Generation: handle.Generation, Settings: monitoroperation.PiSettingsRecord{Provider: settings.Provider, AccountRef: settings.AccountRef, Model: settings.Model, Thinking: settings.Thinking, PolicyRevision: settings.PolicyRevision, Revision: settings.Revision}}, nil
}

func (p communicationPiTransport) Send(ctx context.Context, message communication.PiMessageRecord) (backend.SendResult, piruntime.SubmissionStatus, error) {
	prepared, err := p.Prepare(ctx, message.To)
	if err != nil || message.PiDelivery == nil || prepared.Settings != message.PiDelivery.Settings || prepared.Generation != message.PiDelivery.Generation {
		return backend.SendResult{}, piruntime.SubmissionStatus{}, errors.Join(piruntime.ErrHeld, err)
	}
	runtime := (piTeamService{p.server}).runtime()
	handle := agentbackend.ExecutionHandle{Conversation: message.To.Conversation(), Generation: prepared.Generation}
	operations, err := runtime.Bind(ctx, handle)
	if err != nil || operations.Send == nil {
		return backend.SendResult{}, piruntime.SubmissionStatus{}, errors.Join(piruntime.ErrUnavailable, err)
	}
	result, err := operations.Send.Send(ctx, piruntime.BackendIdentity(handle), backend.SendRequest{ClientUserMessageID: message.ID, Message: message.Envelope, Delivery: "steer", ActionContext: "pi-delivery-v1:steer"})
	if err != nil || result.Outcome == backend.Pending {
		return result, piruntime.SubmissionStatus{}, err
	}
	status, lookupErr := runtime.Lookup(ctx, message.PiDelivery.Input.Ref())
	return result, status, lookupErr
}

func (p communicationPiTransport) Lookup(ctx context.Context, message communication.PiMessageRecord) (piruntime.SubmissionStatus, error) {
	if message.PiDelivery == nil {
		return piruntime.SubmissionStatus{}, piruntime.ErrScope
	}
	if _, err := p.current(ctx, message.To); err != nil {
		return piruntime.SubmissionStatus{}, err
	}
	return (piTeamService{p.server}).runtime().Lookup(ctx, message.PiDelivery.Input.Ref())
}
