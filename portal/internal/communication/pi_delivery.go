package communication

import (
	"context"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// PiGrantProjection is read-only evidence from the existing exact-pair grant
// owner. It is not a new grant or codec. The current owner cannot yet produce
// TypedEndpointsSupported for persisted Pi endpoints.
type PiGrantProjection struct {
	ID                      string
	A, B                    agentbackend.ConversationRef
	Revoked, RecipientHeld  bool
	TypedEndpointsSupported bool
}

// WithPiDeliveryPolicy is called inside the existing communication owner's
// admission/grant locks, with a fresh projection for this exact effect. It
// creates no mailbox, resumes no recipient and never guesses a Codex identity.
func WithPiDeliveryPolicy(ctx context.Context, caller teamruntime.PiCaller, target agentbackend.ExecutionHandle, grant PiGrantProjection, deliver func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	from, to := caller.Handle.Conversation, target.Conversation
	if teamruntime.ValidatePiCaller(caller) != nil || from.Member != "lead" || to.Member != "lead" ||
		teamruntime.ValidatePiHandle(target) != nil || from.Session == to.Session || !validIDForPiGrant(grant.ID) {
		return teamruntime.ErrPiPolicy
	}
	if !((grant.A == from && grant.B == to) || (grant.A == to && grant.B == from)) || grant.Revoked {
		return teamruntime.ErrPiPolicy
	}
	if grant.RecipientHeld {
		return teamruntime.ErrPiHeld
	}
	if !grant.TypedEndpointsSupported || deliver == nil {
		return teamruntime.ErrPiUnavailable
	}
	return deliver()
}

func validIDForPiGrant(id string) bool { return messageIDPattern.MatchString(id) }
