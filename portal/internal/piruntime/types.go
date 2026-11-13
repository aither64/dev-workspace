// Package piruntime owns the private Pi supervisor and SDK-worker boundary.
// Callers must authorize these trusted identities before invoking Controller.
package piruntime

import (
	"context"
	"errors"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

var (
	ErrScope       = errors.New("Pi runtime scope mismatch")
	ErrHeld        = errors.New("Pi runtime has unresolved work")
	ErrUnavailable = errors.New("Pi runtime operation unavailable")
	ErrConflict    = errors.New("Pi runtime immutable request conflict")
)

type Controller interface {
	Ensure(context.Context, StartRequest) (RuntimeStatus, error)
	Inspect(context.Context, RuntimeRef) (RuntimeStatus, error)
	Create(context.Context, RuntimeRef, CreateRequest) (ConversationState, error)
	Load(context.Context, agentbackend.ExecutionHandle) (ConversationState, error)
	Bind(context.Context, agentbackend.ExecutionHandle) (backend.Operations, error)
	Lookup(context.Context, SubmissionRef) (SubmissionStatus, error)
	Configure(context.Context, agentbackend.ExecutionHandle, EffectiveSettings) (SettingsResult, error)
	Unload(context.Context, agentbackend.ExecutionHandle) (StopResult, error)
	Resources(context.Context, RuntimeRef) (ResourceSnapshot, error)
	StartResource(context.Context, RuntimeRef, ResourceRequest) (ResourceStatus, error)
	InspectResource(context.Context, ResourceHandle) (ResourceStatus, error)
	StopResource(context.Context, ResourceHandle) (StopResult, error)
}

type StartRequest struct {
	Session                                                 agentbackend.SessionRef
	BindingRevision                                         uint64
	RequestID, AppStorePath, ProfilePath, ProfileGeneration string
}
type RuntimeRef struct {
	Session                                   agentbackend.SessionRef
	InstanceID, RequestID, Digest, Generation string
}
type RuntimeStatus struct {
	Runtime     RuntimeRef
	Phase       string
	Observation HostObservation
	Handshake   *Handshake
	ObservedAt  time.Time
}
type CreateRequest struct {
	Session                         agentbackend.SessionRef
	Member, Incarnation, CreationID string
	Settings                        EffectiveSettings
}
type ConversationState struct {
	Conversation      agentbackend.ConversationRef
	Generation, State string
	HistoryRef        string
	Persisted         bool
	Settings          EffectiveSettings
}

// EffectiveSettings contains trusted resolved catalog references, never secrets
// or browser-selected endpoint configuration. Revision serializes local changes.
type EffectiveSettings struct {
	Provider, AccountRef, Model, Thinking, PolicyRevision string
	Revision                                              uint64
}
type SettingsResult struct {
	Requested, Effective EffectiveSettings
	Outcome              string
}
type SubmissionRef struct {
	Conversation                agentbackend.ConversationRef
	OperationID, Action, Digest string
}
type NativeWitness struct {
	SessionID, EntryID, ParentID, PreviousLeafID, EffectiveDigest string
}
type SubmissionStatus struct {
	Schema               int
	Ref                  SubmissionRef
	Phase, Generation    string
	Settings             EffectiveSettings
	Input                SendInput
	Witness              *NativeWitness
	Refusal              string
	CreatedAt, UpdatedAt time.Time
}

type SendInput struct {
	Message, ActionContext string
	Retry                  bool
}
type StopResult struct {
	Outcome, Reason string
	VerifiedExit    bool
}
type ResourceRequest struct {
	ResourceID, OperationID, Digest, Cwd string
	Argv, Environment                    []string
}
type ResourceHandle struct {
	Runtime                 RuntimeRef
	ResourceID, OperationID string
	PID, ProcessGroup       int
	PIDStart                string
}
type ResourceStatus struct {
	Handle   ResourceHandle
	Phase    string
	ExitCode *int
}
type ResourceSnapshot struct {
	ObservedAt                                 time.Time
	Cgroup                                     map[string]string
	ScratchLogicalBytes, ScratchAllocatedBytes int64
	Unavailable, Exclusions                    []string
}

// CreationVerifier is observational. It never creates, loads or repairs history.
type CreationVerifier interface {
	VerifyCreation(context.Context, agentbackend.ExecutionHandle, string, string, *SubmissionRef) (CreationProof, error)
}
type CreationProof struct {
	Conversation                       agentbackend.ConversationRef
	Generation, CreationID, HistoryRef string
	Persisted                          bool
	Bootstrap                          *SubmissionStatus
}
