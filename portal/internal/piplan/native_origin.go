package piplan

import (
	"context"
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// NativeOriginVerifier is shared by acceptance capture and new implementation
// effects. Admission and current-generation resolution remain with the host.
type NativeOriginVerifier struct {
	Inspector piruntime.NativeOriginInspector
}

func (v NativeOriginVerifier) Verify(ctx context.Context, current Caller, proposal Proposal) error {
	if piquestions.ValidateCaller(current) != nil || piquestions.ValidateCaller(proposal.Caller) != nil || !sameCaller(current, proposal.Caller) || proposal.Conversation != proposal.Caller.Handle.Conversation {
		return piquestions.ErrConflict
	}
	if v.Inspector == nil {
		return piquestions.ErrUnavailable
	}
	origin := piruntime.NativeOriginRef{
		AnchorEntryID: proposal.Caller.Lineage.AnchorEntryID, SelectionToken: proposal.Caller.Lineage.SelectionToken,
		NativeToolCallID: proposal.Caller.NativeToolCallID, Tool: "publish_plan",
	}
	observed, err := v.Inspector.InspectNativeOrigin(ctx, current.Handle, origin)
	if errors.Is(err, piruntime.ErrScope) || errors.Is(err, piruntime.ErrConflict) {
		return piquestions.ErrConflict
	}
	if errors.Is(err, piruntime.ErrUnavailable) {
		return piquestions.ErrUnavailable
	}
	if err != nil {
		return err
	}
	if observed.Handle != current.Handle || observed.Origin != origin {
		return piquestions.ErrConflict
	}
	return VerifyNativeArguments(proposal, observed.Arguments)
}
