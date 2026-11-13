package teamruntime

import (
	"context"
	"errors"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

var (
	ErrPiUnavailable = piruntime.ErrUnavailable
	ErrPiHeld        = piruntime.ErrHeld
	ErrPiConflict    = piruntime.ErrConflict
	ErrPiPolicy      = errors.New("Pi team policy refused")
)

// PiCaller is captured by the host's authenticated worker channel. Never decode
// it from model JSON. A handle is identity evidence, not authorization.
type PiCaller struct {
	Runtime piruntime.RuntimeRef
	Handle  agentbackend.ExecutionHandle
}

type PiMessage struct{ ID, To, Text string }

type PiPreparedDelivery struct {
	Target   agentbackend.ExecutionHandle
	Settings piruntime.EffectiveSettings
	Request  backend.SendRequest
}

// PiAdmission owns current session/package/team admission through the callback,
// revalidating caller, target, holds, settings and support immediately before it.
// Unavailable implementations must not invoke it. Caller holds no team lock.
type PiAdmission interface {
	WithDelivery(context.Context, PiCaller, PiMessage, func(PiPreparedDelivery) error) error
}

// PiNative is P3's trusted Bind/Send shim. It owns receipts and never derives a
// new submission identity from a runtime generation or a visible-text digest.
type PiNative interface {
	Deliver(context.Context, agentbackend.ExecutionHandle, backend.SendRequest) (backend.SendResult, error)
	Lookup(context.Context, piruntime.SubmissionRef) (piruntime.SubmissionStatus, error)
}

type PiDisposition string

const (
	PiHeld        PiDisposition = "held"
	PiUnavailable PiDisposition = "unavailable"
	PiAttempted   PiDisposition = "attempted"
)

type PiDeliveryResult struct {
	Disposition PiDisposition `json:"disposition"`
	// Native preserves the exact return value even when contradictory/invalid.
	Native *backend.SendResult `json:"native,omitempty"`
	// Outcome is the conservative interpretation, separate from the raw value.
	Outcome backend.SendOutcome `json:"outcome,omitempty"`
}

type PiDelivery struct {
	Admission PiAdmission
	Native    PiNative
}

// PiParticipant is a read-only projection under admission ownership, never a
// second roster or cache. Persisted includes P2's materialization/ready proof.
type PiParticipant struct {
	Handle                  agentbackend.ExecutionHandle
	State, Activity         string
	Loaded, Held, Persisted bool
	Policy                  agentteams.PiPolicy
	Settings                piruntime.EffectiveSettings
	SettingsApplied         bool
}
