package piquestions

import (
	"context"
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// NativeOriginVerifier binds the optional runtime reader to the content-aware
// capture/delivery port. Its caller supplies ordinary admission and the current
// generation for the unchanged durable conversation and publication identity.
type NativeOriginVerifier struct {
	Inspector piruntime.NativeOriginInspector
}

func (v NativeOriginVerifier) Verify(ctx context.Context, current Caller, record QuestionRecord) error {
	if ValidateCaller(current) != nil || ValidateCaller(record.Caller) != nil || !sameCaller(current, record.Caller) {
		return ErrConflict
	}
	if v.Inspector == nil {
		return ErrUnavailable
	}
	origin := piruntime.NativeOriginRef{
		AnchorEntryID: record.Caller.Lineage.AnchorEntryID, SelectionToken: record.Caller.Lineage.SelectionToken,
		NativeToolCallID: record.Caller.NativeToolCallID, Tool: "request_user_input_async",
	}
	observed, err := v.Inspector.InspectNativeOrigin(ctx, current.Handle, origin)
	if errors.Is(err, piruntime.ErrScope) || errors.Is(err, piruntime.ErrConflict) {
		return ErrConflict
	}
	if errors.Is(err, piruntime.ErrUnavailable) {
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	if observed.Handle != current.Handle || observed.Origin != origin {
		return ErrConflict
	}
	return VerifyNativeArguments(record, observed.Arguments)
}
