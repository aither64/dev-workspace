package piruntime

import (
	"context"
	"path/filepath"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type UtilityRef struct {
	Session                            agentbackend.SessionRef
	OperationID, Incarnation, NativeID string
}
type UtilityExecutionHandle struct {
	Utility    UtilityRef
	Generation string
}
type UtilityCreateRequest struct {
	Utility    UtilityRef
	CreationID string
	Settings   EffectiveSettings
}
type UtilityState struct {
	Utility           UtilityRef
	Generation, State string
	Persisted         bool
	Settings          EffectiveSettings
}
type UtilitySubmissionRef struct {
	Utility                     UtilityRef
	OperationID, Action, Digest string
}
type UtilitySubmissionStatus struct {
	Schema               int
	OwnerKind            string
	Ref                  UtilitySubmissionRef
	Phase, Generation    string
	Settings             EffectiveSettings
	Input                SendInput
	Witness              *NativeWitness
	Refusal              string
	CreatedAt, UpdatedAt time.Time
}
type UtilityController interface {
	CreateUtility(context.Context, RuntimeRef, UtilityCreateRequest) (UtilityState, error)
	LoadUtility(context.Context, UtilityExecutionHandle) (UtilityState, error)
	BindUtility(context.Context, UtilityExecutionHandle) (backend.Operations, error)
	LookupUtility(context.Context, UtilitySubmissionRef) (UtilitySubmissionStatus, error)
	UnloadUtility(context.Context, UtilityExecutionHandle) (StopResult, error)
}
type UtilityGrant struct {
	Runtime                       RuntimeRef
	Utility                       UtilityRef
	CreationID, LaunchOperationID string
	Settings                      EffectiveSettings
	Input                         SendInput
}
type UtilityAuthority interface {
	WithUtility(context.Context, RuntimeRef, UtilityRef, string, func(UtilityGrant) error) error
}

func UtilitySubmissionDigest(ref UtilitySubmissionRef, input SendInput, settings EffectiveSettings) string {
	ref.Digest = ""
	return digestValue(struct {
		Version   int
		OwnerKind string
		Ref       UtilitySubmissionRef
		Input     SendInput
		Settings  EffectiveSettings
	}{2, "utility", ref, input, settings})
}
func validateUtility(ref UtilityRef, nativeRequired bool) error {
	if validateSession(ref.Session) != nil || !operationPattern.MatchString(ref.OperationID) || !hexPattern.MatchString(ref.Incarnation) || (nativeRequired && !uuidPattern.MatchString(ref.NativeID)) || (ref.NativeID != "" && !uuidPattern.MatchString(ref.NativeID)) {
		return ErrScope
	}
	return nil
}
func UtilityIdentity(handle UtilityExecutionHandle) backend.Identity {
	if validateUtility(handle.Utility, true) != nil || !hexPattern.MatchString(handle.Generation) {
		return backend.Identity{}
	}
	ref := handle.Utility
	return backend.Identity{BackendID: "pi", ScopeKey: "pi-utility:" + digestValue(struct {
		Version int
		Kind    string
		Utility UtilityRef
	}{1, "utility", ref}), ConversationID: ref.NativeID, Directory: filepath.Join(ref.Session.Workspace, "work", ref.Session.Slug), Generation: handle.Generation}
}

// Read/recovery authorization is distinct from effect admission and may inspect
// the exact original receipt while the operation or worker remains held/busy.
type UtilityReadAuthority interface {
	WithUtilityRead(context.Context, RuntimeRef, UtilitySubmissionRef, func() error) error
}
