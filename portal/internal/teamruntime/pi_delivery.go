package teamruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aither64/codex-web/conversation/backend"
)

// PiEnvelope retains the existing assignment/report instruction verbatim.
func PiEnvelope(caller PiCaller, message PiMessage) string {
	envelope := fmt.Sprintf("Team assignment from %s to %s:\n\n%s", caller.Handle.Conversation.Member, message.To, strings.TrimSpace(message.Text))
	if message.To != "lead" {
		envelope += "\n\nSend results or blocking questions to lead with report_to_lead. Use a new message_id for each message; reuse it only to retry that message."
	}
	return envelope
}

func PiActionContext(caller PiCaller, message PiMessage) string {
	return "team:" + caller.Runtime.Session.Slug + ":" + caller.Handle.Conversation.Member + ":" + message.To
}

func (d PiDelivery) Deliver(ctx context.Context, caller PiCaller, message PiMessage) (PiDeliveryResult, error) {
	result := PiDeliveryResult{Disposition: PiUnavailable}
	if ValidatePiCaller(caller) != nil || ValidatePiMessage(message) != nil {
		return result, ErrPiPolicy
	}
	if d.Admission == nil || d.Native == nil {
		return result, ErrPiUnavailable
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	called := false
	var nativeErr error
	err := d.Admission.WithDelivery(ctx, caller, message, func(p PiPreparedDelivery) error {
		if called {
			return ErrPiConflict
		}
		if !validPiHandle(p.Target) || p.Target.Conversation.Session != caller.Runtime.Session ||
			p.Target.Generation != caller.Runtime.Generation || p.Target.Conversation.Member != message.To ||
			!validPiSettings(p.Settings) || p.Request.ClientUserMessageID != message.ID ||
			p.Request.Message != PiEnvelope(caller, message) || p.Request.ActionContext != PiActionContext(caller, message) {
			return ErrPiPolicy
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		called = true
		result.Disposition, result.Outcome = PiAttempted, backend.Unknown
		native, callErr := d.Native.Deliver(ctx, p.Target, p.Request)
		nativeErr, result.Native = callErr, &native
		if callErr == nil {
			switch native.Outcome {
			case backend.Accepted:
				if native.Receipt.ClientUserMessageID == message.ID && native.Receipt.TurnID != "" &&
					native.Receipt.QueuedSubmissionID == "" && !native.Receipt.Steered && !native.Retryable {
					result.Outcome = backend.Accepted
				}
			case backend.NotSubmitted:
				if native.Receipt.TurnID == "" && native.Receipt.QueuedSubmissionID == "" && !native.Receipt.Steered &&
					(native.Receipt.ClientUserMessageID == "" || native.Receipt.ClientUserMessageID == message.ID) {
					result.Outcome = backend.NotSubmitted
				}
			case backend.Unknown:
				result.Outcome = backend.Unknown
			}
		}
		return callErr
	})
	if called {
		// Admission errors after Send cannot turn an attempted effect into a
		// pre-dispatch refusal, nor replace the original native error/result.
		if err != nil {
			result.Outcome = backend.Unknown
		}
		if nativeErr == nil || errors.Is(err, nativeErr) {
			return result, err
		}
		return result, errors.Join(nativeErr, err)
	}
	if errors.Is(err, ErrPiHeld) {
		result.Disposition = PiHeld
	}
	if err == nil {
		err = ErrPiUnavailable // A skipped callback never implies delivery.
	}
	return result, err
}
