package teamruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piAdmissionFunc func(context.Context, PiCaller, PiMessage, func(PiPreparedDelivery) error) error

func (f piAdmissionFunc) WithDelivery(ctx context.Context, c PiCaller, m PiMessage, fn func(PiPreparedDelivery) error) error {
	return f(ctx, c, m, fn)
}

type piNativeFake struct {
	calls   int
	request backend.SendRequest
	result  backend.SendResult
	err     error
}

func (f *piNativeFake) Deliver(_ context.Context, _ agentbackend.ExecutionHandle, r backend.SendRequest) (backend.SendResult, error) {
	f.calls++
	f.request = r
	return f.result, f.err
}
func (f *piNativeFake) Lookup(context.Context, piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	return piruntime.SubmissionStatus{}, ErrPiUnavailable
}

func piFixture() (PiCaller, PiMessage, PiParticipant, PiParticipant) {
	session := agentbackend.SessionRef{Workspace: "/workspace", Slug: "2026-11-12-fixture", Incarnation: strings.Repeat("a", 64)}
	from := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: session, Backend: agentbackend.Pi,
		Member: "implementer0", Incarnation: strings.Repeat("b", 64), NativeID: "native-from"}, Generation: strings.Repeat("c", 64)}
	target := from
	target.Conversation.Member, target.Conversation.NativeID = "lead", "native-lead"
	caller := PiCaller{Handle: from, Runtime: piruntime.RuntimeRef{Session: session, Generation: from.Generation,
		InstanceID: "11111111-1111-4111-8111-111111111111", RequestID: "22222222-2222-4222-8222-222222222222", Digest: strings.Repeat("d", 64)}}
	policy := agentteams.PiPolicy{Model: "saved-model", Effort: "xhigh", Behavior: "implementer", Purpose: "implementation",
		Instructions: "Saved instructions.", SavedAccess: "workspace_write", ExecutionAccess: "full_access"}
	participant := PiParticipant{Handle: from, State: "ready", Activity: "working", Loaded: true, Persisted: true,
		Policy: policy, SettingsApplied: true, Settings: piruntime.EffectiveSettings{Provider: "fixture", AccountRef: "account",
			Model: "saved-model", Thinking: "xhigh", PolicyRevision: strings.Repeat("e", 64), Revision: 1}}
	recipient := participant
	recipient.Handle, recipient.Activity = target, "idle"
	return caller, PiMessage{ID: "33333333-3333-4333-8333-333333333333", To: "lead", Text: "  Results ready.  "}, participant, recipient
}

func piPrepared(c PiCaller, m PiMessage, target PiParticipant) PiPreparedDelivery {
	return PiPreparedDelivery{Target: target.Handle, Settings: target.Settings,
		Request: backend.SendRequest{ClientUserMessageID: m.ID, Message: PiEnvelope(c, m), ActionContext: PiActionContext(c, m)}}
}

func TestPiDeliveryRefusesWithoutAuthorityOrNativeEffect(t *testing.T) {
	c, m, from, target := piFixture()
	native := &piNativeFake{}
	for _, state := range []string{"creating", "retiring", "removed", "ready"} {
		t.Run(state, func(t *testing.T) {
			recipient := target
			recipient.State = state
			if state == "ready" {
				recipient.Activity = "busy"
			}
			admission := piAdmissionFunc(func(ctx context.Context, got PiCaller, message PiMessage, fn func(PiPreparedDelivery) error) error {
				if err := CheckPiDelivery(got, from, recipient); err != nil {
					return err
				}
				return fn(piPrepared(got, message, recipient))
			})
			result, err := (PiDelivery{Admission: admission, Native: native}).Deliver(context.Background(), c, m)
			if !errors.Is(err, ErrPiHeld) || result.Disposition != PiHeld || result.Native != nil {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
	result, err := (PiDelivery{Native: native}).Deliver(context.Background(), c, m)
	if !errors.Is(err, ErrPiUnavailable) || result.Disposition != PiUnavailable || native.calls != 0 {
		t.Fatalf("%+v %v calls=%d", result, err, native.calls)
	}
}

func TestPiExplicitReportMakesOneWitnessedAttempt(t *testing.T) {
	c, m, from, target := piFixture()
	native := &piNativeFake{result: backend.SendResult{Outcome: backend.Accepted,
		Receipt: backend.SendReceipt{ClientUserMessageID: m.ID, TurnID: "native-entry"}}}
	admission := piAdmissionFunc(func(ctx context.Context, got PiCaller, message PiMessage, fn func(PiPreparedDelivery) error) error {
		if err := CheckPiDelivery(got, from, target); err != nil {
			return err
		}
		return fn(piPrepared(got, message, target))
	})
	delivery := PiDelivery{Admission: admission, Native: native}
	if native.calls != 0 {
		t.Fatal("constructor forwarded a final answer")
	}
	result, err := delivery.Report(context.Background(), c, m.ID, m.Text)
	if err != nil || result.Disposition != PiAttempted || result.Outcome != backend.Accepted || native.calls != 1 {
		t.Fatalf("%+v %v calls=%d", result, err, native.calls)
	}
	if native.request.ClientUserMessageID != m.ID || native.request.ActionContext != "team:2026-11-12-fixture:implementer0:lead" || native.request.Message != "Team assignment from implementer0 to lead:\n\nResults ready." {
		t.Fatalf("wrong frozen request: %+v", native.request)
	}
}

func TestPiDuplicateAdmissionCallbackCannotSendTwice(t *testing.T) {
	c, m, _, target := piFixture()
	native := &piNativeFake{result: backend.SendResult{Outcome: backend.Unknown}}
	admission := piAdmissionFunc(func(ctx context.Context, got PiCaller, message PiMessage, fn func(PiPreparedDelivery) error) error {
		if err := fn(piPrepared(got, message, target)); err != nil {
			return err
		}
		return fn(piPrepared(got, message, target))
	})
	r, err := (PiDelivery{Admission: admission, Native: native}).Deliver(context.Background(), c, m)
	if native.calls != 1 || !errors.Is(err, ErrPiConflict) || r.Disposition != PiAttempted || r.Outcome != backend.Unknown {
		t.Fatalf("%+v %v calls=%d", r, err, native.calls)
	}
}

// This is P3's immutable-receipt contract as a fake, not a P5 ledger. The
// adapter must preserve IDs/envelopes even when a caller repeats a tool call.
type piReceiptNative struct {
	request *backend.SendRequest
	result  backend.SendResult
	effects int
}

func (f *piReceiptNative) Deliver(_ context.Context, _ agentbackend.ExecutionHandle, request backend.SendRequest) (backend.SendResult, error) {
	if f.request != nil {
		if *f.request != request {
			return backend.SendResult{Outcome: backend.Unknown}, ErrPiConflict
		}
		return f.result, nil
	}
	saved := request
	f.request = &saved
	if f.result.Outcome == backend.Accepted {
		f.effects++
	}
	return f.result, nil
}
func (f *piReceiptNative) Lookup(context.Context, piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	return piruntime.SubmissionStatus{}, ErrPiUnavailable
}

func TestPiOriginalIDsAndTerminalOutcomesAreNeverReplaced(t *testing.T) {
	for _, id := range []string{"33333333-3333-4333-8333-333333333333", "33333333333343338333333333333333"} {
		for _, outcome := range []backend.SendOutcome{backend.Accepted, backend.Unknown, backend.NotSubmitted} {
			c, m, _, target := piFixture()
			m.ID = id
			native := &piReceiptNative{result: backend.SendResult{Outcome: outcome}}
			if outcome == backend.Accepted {
				native.result.Receipt = backend.SendReceipt{ClientUserMessageID: id, TurnID: "persisted-native-entry"}
			}
			admission := piAdmissionFunc(func(ctx context.Context, c PiCaller, m PiMessage, fn func(PiPreparedDelivery) error) error {
				return fn(piPrepared(c, m, target))
			})
			d := PiDelivery{Admission: admission, Native: native}
			for i := 0; i < 2; i++ {
				r, err := d.Deliver(context.Background(), c, m)
				if err != nil || r.Outcome != outcome || r.Native == nil || native.request.ClientUserMessageID != id {
					t.Fatalf("%s %s: %+v %v", id, outcome, r, err)
				}
			}
			wantEffects := 0
			if outcome == backend.Accepted {
				wantEffects = 1
			}
			if native.effects != wantEffects {
				t.Fatal("adapter replaced an original operation")
			}
			m.Text = "changed full envelope"
			if _, err := d.Deliver(context.Background(), c, m); !errors.Is(err, ErrPiConflict) {
				t.Fatal("changed envelope did not preserve native conflict")
			}
			if native.request.ClientUserMessageID != id || native.effects != wantEffects {
				t.Fatal("conflict minted a replacement")
			}
		}
	}
}
