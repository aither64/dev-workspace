package communication

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piReadGate struct {
	input monitoroperation.PiInputRecord
}

func (g piReadGate) WithPiInputRead(ctx context.Context, input monitoroperation.PiInputRecord, read func() error) error {
	if input != g.input {
		return piruntime.ErrScope
	}
	return read()
}

type piLookupOnly struct {
	status piruntime.SubmissionStatus
	calls  int
	err    error
}

func (f *piLookupOnly) Lookup(_ context.Context, ref piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	f.calls++
	if ref != f.status.Ref {
		return piruntime.SubmissionStatus{}, piruntime.ErrScope
	}
	return f.status, f.err
}
func TestPiCommunicationLostReplyOriginalLookupAndNegativeHoldNeverSend(t *testing.T) {
	for _, phase := range []string{"Accepted", "Unknown", "NotSubmitted"} {
		t.Run(phase, func(t *testing.T) {
			store, _ := NewStore(t.TempDir(), "/workspace")
			scope := monitoroperation.PiSessionRecord{Workspace: "/workspace", Slug: "2026-11-12-recipient", Incarnation: strings.Repeat("a", 64)}
			generation := strings.Repeat("b", 64)
			gate := piCommunicationGate{scope.Ref(), generation}
			target := PiEndpoint{Backend: "pi", Session: scope, LeadIncarnation: strings.Repeat("c", 64), NativeID: "target-native"}
			from := target
			from.Session.Slug = "2026-11-12-source"
			from.NativeID = "source-native"
			id, grantID := strings.Repeat("d", 32), strings.Repeat("e", 32)
			conversation := monitoroperation.PiConversationRecord{Session: scope, Backend: "pi", Member: "lead", Incarnation: target.LeadIncarnation, NativeID: target.NativeID}
			input := monitoroperation.PiInputRecord{Conversation: conversation, OperationID: id, Action: "send", Digest: strings.Repeat("f", 64)}
			settings := monitoroperation.PiSettingsRecord{Provider: "fixture", AccountRef: "account", Model: "model", Thinking: "low", PolicyRevision: strings.Repeat("1", 64), Revision: 1}
			message := PiMessageRecord{ID: id, GrantID: grantID, From: EndpointRecord{Pi: &from}, To: EndpointRecord{Pi: &target}, Text: "original message", CreatedAt: time.Now().UTC(), Status: "submitting", Envelope: "frozen original envelope", PiDelivery: &monitoroperation.PiDeliveryRecord{Settings: settings, Input: input}}
			if err := store.UpdatePi(context.Background(), gate, scope, generation, func(s *PiCommunicationState) error {
				s.Grants = append(s.Grants, PiGrantRecord{ID: grantID, A: message.From, B: message.To, ApprovedBy: from.Session.Slug, Approval: "exact approved pair", CreatedAt: message.CreatedAt})
				s.Messages = append(s.Messages, message)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			native := &piLookupOnly{status: piruntime.SubmissionStatus{Schema: 1, Ref: input.Ref(), Phase: phase, Settings: settings.Runtime(), Input: piruntime.SendInput{Message: message.Envelope, ActionContext: "communication:" + grantID}, Generation: generation}}
			if phase == "Accepted" {
				native.status.Witness = &piruntime.NativeWitness{SessionID: target.NativeID, EntryID: "native-entry", EffectiveDigest: piruntime.MessageDigest(message.Envelope)}
			} else if phase == "NotSubmitted" {
				native.status.Refusal = "busy"
			}
			for i := 0; i < 2; i++ {
				if err := store.ReconcilePiMessage(context.Background(), gate, piReadGate{input}, scope, generation, id, native); err != nil {
					t.Fatal(err)
				}
			}
			current, err := store.read()
			if err != nil {
				t.Fatal(err)
			}
			m := current.Pi.Messages[0]
			if m.PiDelivery.Input != input || m.PiDelivery.Settings != settings || native.calls != 2 {
				t.Fatal("lookup replaced original receipt/settings")
			}
			if phase == "Accepted" && m.Status != "delivered" || phase != "Accepted" && m.Status != "held" {
				t.Fatal("outcome misrepresented", m.Status)
			}
			if err := store.RevokePiGrant(context.Background(), gate, scope, generation, grantID, "revoked", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			current, _ = store.read()
			if current.Pi.Messages[0].PiDelivery.Input != input || current.Pi.Messages[0].Status == "canceled" {
				t.Fatal("revocation erased accepted/unknown/negative obligation")
			}
			if phase == "NotSubmitted" {
				native.status.Phase = "Accepted"
				native.status.Witness = &piruntime.NativeWitness{SessionID: target.NativeID, EntryID: "other-entry", EffectiveDigest: piruntime.MessageDigest(message.Envelope)}
				if err := store.ReconcilePiMessage(context.Background(), gate, piReadGate{input}, scope, generation, id, native); !errors.Is(err, piruntime.ErrConflict) {
					t.Fatal("terminal negative was recycled", err)
				}
			}
		})
	}
}
