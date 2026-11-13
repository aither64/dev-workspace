package teamruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
)

func TestPiCurrentPolicyRefusalsHaveZeroNativeCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PiCaller, *PiParticipant, *PiParticipant)
	}{
		{"workspace", func(c *PiCaller, f, p *PiParticipant) { c.Runtime.Session.Workspace = "/other" }},
		{"caller incarnation", func(c *PiCaller, f, p *PiParticipant) { f.Handle.Conversation.Incarnation = strings.Repeat("f", 64) }},
		{"target session", func(c *PiCaller, f, p *PiParticipant) { p.Handle.Conversation.Session.Slug = "2026-11-12-other" }},
		{"generation", func(c *PiCaller, f, p *PiParticipant) { p.Handle.Generation = strings.Repeat("f", 64) }},
		{"caller removed", func(c *PiCaller, f, p *PiParticipant) { f.State = "removed" }},
		{"caller retiring", func(c *PiCaller, f, p *PiParticipant) { f.State = "retiring" }},
		{"recipient held", func(c *PiCaller, f, p *PiParticipant) { p.Held = true }},
		{"recipient cold", func(c *PiCaller, f, p *PiParticipant) { p.Loaded = false }},
		{"not materialized", func(c *PiCaller, f, p *PiParticipant) { p.Persisted = false }},
		{"model unavailable", func(c *PiCaller, f, p *PiParticipant) { p.SettingsApplied = false }},
		{"missing settings proof", func(c *PiCaller, f, p *PiParticipant) { p.Settings.PolicyRevision = "" }},
		{"waiting", func(c *PiCaller, f, p *PiParticipant) { p.Activity = "waiting" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m, from, target := piFixture()
			tc.mutate(&c, &from, &target)
			native := &piNativeFake{}
			admission := piAdmissionFunc(func(ctx context.Context, current PiCaller, message PiMessage, fn func(PiPreparedDelivery) error) error {
				if err := CheckPiDelivery(current, from, target); err != nil {
					return err
				}
				return fn(piPrepared(current, message, target))
			})
			r, err := (PiDelivery{Admission: admission, Native: native}).Deliver(context.Background(), c, m)
			if err == nil || native.calls != 0 || r.Native != nil {
				t.Fatalf("refusal had effect: %+v %v calls=%d", r, err, native.calls)
			}
		})
	}
}

func TestPiReplacedCallerIsRevalidatedAfterToolBinding(t *testing.T) {
	c, m, from, target := piFixture()
	native := &piNativeFake{}
	admission := piAdmissionFunc(func(ctx context.Context, current PiCaller, message PiMessage, fn func(PiPreparedDelivery) error) error {
		from.Handle.Conversation.Incarnation = strings.Repeat("f", 64)
		if err := CheckPiDelivery(current, from, target); err != nil {
			return err
		}
		return fn(piPrepared(current, message, target))
	})
	_, err := (PiDelivery{Admission: admission, Native: native}).Deliver(context.Background(), c, m)
	if !errors.Is(err, ErrPiPolicy) || native.calls != 0 {
		t.Fatalf("%v calls=%d", err, native.calls)
	}
}

func TestPiSendOutcomeIsSeparateFromRawNativeReturn(t *testing.T) {
	c, m, _, target := piFixture()
	nativeError := errors.New("native reply lost")
	for _, tc := range []struct {
		name    string
		result  backend.SendResult
		err     error
		outcome backend.SendOutcome
	}{
		{"empty", backend.SendResult{}, nil, backend.Unknown},
		{"unknown", backend.SendResult{Outcome: backend.Unknown}, nativeError, backend.Unknown},
		{"missing acceptance witness", backend.SendResult{Outcome: backend.Accepted}, nil, backend.Unknown},
		{"contradictory acceptance", backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{TurnID: "entry", ClientUserMessageID: m.ID}}, nativeError, backend.Unknown},
		{"positive refusal", backend.SendResult{Outcome: backend.NotSubmitted, Retryable: true}, nil, backend.NotSubmitted},
		{"refusal with entry", backend.SendResult{Outcome: backend.NotSubmitted, Receipt: backend.SendReceipt{TurnID: "entry"}}, nil, backend.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := &piNativeFake{result: tc.result, err: tc.err}
			admission := piAdmissionFunc(func(ctx context.Context, c PiCaller, m PiMessage, fn func(PiPreparedDelivery) error) error {
				return fn(piPrepared(c, m, target))
			})
			r, err := (PiDelivery{Admission: admission, Native: native}).Deliver(context.Background(), c, m)
			if r.Disposition != PiAttempted || r.Outcome != tc.outcome || r.Native == nil || *r.Native != tc.result || native.calls != 1 || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("%+v %v calls=%d", r, err, native.calls)
			}
		})
	}
}

func TestPiPreparationCannotChangeFrozenEnvelopeOrTarget(t *testing.T) {
	c, m, _, target := piFixture()
	for _, mutate := range []func(*PiPreparedDelivery){
		func(p *PiPreparedDelivery) { p.Request.ClientUserMessageID = "44444444444444444444444444444444" },
		func(p *PiPreparedDelivery) { p.Request.Message = "changed" },
		func(p *PiPreparedDelivery) { p.Request.ActionContext = "wrong" },
		func(p *PiPreparedDelivery) { p.Target.Conversation.Member = "reviewer0" },
	} {
		native := &piNativeFake{}
		admission := piAdmissionFunc(func(ctx context.Context, c PiCaller, m PiMessage, fn func(PiPreparedDelivery) error) error {
			p := piPrepared(c, m, target)
			mutate(&p)
			return fn(p)
		})
		if _, err := (PiDelivery{Admission: admission, Native: native}).Deliver(context.Background(), c, m); !errors.Is(err, ErrPiPolicy) || native.calls != 0 {
			t.Fatalf("%v calls=%d", err, native.calls)
		}
	}
}
