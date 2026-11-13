package piplan

import (
	"context"
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

func TestPlanOriginRequiresExactTextDigestAndAnchor(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	_, err := s.PublishProposal(ctx, c, " plan\r\n", NativeOrigin{EntryID: c.Lineage.AnchorEntryID, Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	r := fresh(c.Handle.Conversation)
	if err = s.file(c.Handle.Conversation).Read(ctx, &r); err != nil {
		t.Fatal(err)
	}
	p := r.Proposals[0]
	if VerifyNativeArguments(p, []byte(`{"text":" plan\r\n"}`)) != nil {
		t.Fatal("exact native plan rejected")
	}
	for _, raw := range []string{`{"text":"plan\r\n"}`, `{"text":" plan\n"}`, `{"text":" plan\r\n","extra":true}`, `{"text":null}`, `{"text":"\udc00"}`} {
		if VerifyNativeArguments(p, []byte(raw)) == nil {
			t.Fatal(raw)
		}
	}
	p.NativeOrigin.EntryID = "other"
	if VerifyNativeArguments(p, []byte(`{"text":" plan\r\n"}`)) == nil {
		t.Fatal("wrong anchor")
	}
	p = r.Proposals[0]
	p.TextSHA256 = Digest("different")
	if VerifyNativeArguments(p, []byte(`{"text":" plan\r\n"}`)) == nil {
		t.Fatal("wrong digest")
	}
}
func TestPlanDuplicateCaptureWithUnavailableOriginAndPolicy(t *testing.T) {
	ctx := context.Background()
	s, c, a, _ := adapterFixture(t)
	p, err := s.PublishProposal(ctx, c, "plan", NativeOrigin{EntryID: c.Lineage.AnchorEntryID, Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	a.VerifyOrigin = func(_ context.Context, _ Caller, p Proposal) error {
		return VerifyNativeArguments(p, []byte(`{"text":"plan"}`))
	}
	store := PolicyStore{Store: s, Adapter: a}
	in := AcceptanceInput{piquestions.UUID(), p.PlanID, 1, p.TextSHA256}
	first, err := store.Accept(ctx, c.Handle.Conversation, in)
	if err != nil {
		t.Fatal(err)
	}
	store.Adapter.Admit = func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error {
		t.Fatal("duplicate invoked action admission")
		return nil
	}
	againBefore, err := store.Accept(ctx, c.Handle.Conversation, in)
	if err != nil || againBefore.OperationID != first.OperationID {
		t.Fatal(againBefore, err)
	}
	store.Adapter = nil
	again, err := store.Accept(ctx, c.Handle.Conversation, in)
	if err != nil || again.OperationID != first.OperationID {
		t.Fatal(again, err)
	}
}

func TestMissingPlanOriginHoldsNewEffectButNotReceiptLookup(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	p, err := s.PublishProposal(ctx, c, "plan", NativeOrigin{EntryID: c.Lineage.AnchorEntryID, Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	input := AcceptanceInput{piquestions.UUID(), p.PlanID, 1, p.TextSHA256}
	a, err := s.Accept(ctx, c.Handle.Conversation, input)
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	svc := Service{Store: s, Ports: Ports{Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, Lookup: func(_ context.Context, _ Caller, v Acceptance) (string, error) {
		lookups++
		if v.OperationID != a.OperationID || v.ReceiptRef != "original" {
			t.Fatal(v)
		}
		return "NotSubmitted", nil
	}}}
	if err = svc.Advance(ctx, c.Handle.Conversation, a.AcceptanceID); !errors.Is(err, piquestions.ErrUnavailable) || lookups != 0 {
		t.Fatal(err, lookups)
	}
	if err = s.updateAcceptance(ctx, c.Handle.Conversation, a.AcceptanceID, func(v *Acceptance) error { v.Phase = "dispatch_pending"; v.ReceiptRef = "original"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = svc.Advance(ctx, c.Handle.Conversation, a.AcceptanceID); err != nil || lookups != 1 {
		t.Fatal(err, lookups)
	}
}
func TestPlanCaptureRechecksProposalAfterOriginInspection(t *testing.T) {
	ctx := context.Background()
	s, c, a, _ := adapterFixture(t)
	p, err := s.PublishProposal(ctx, c, "plan", NativeOrigin{EntryID: c.Lineage.AnchorEntryID, Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	a.VerifyOrigin = func(ctx context.Context, c Caller, p Proposal) error {
		if err := VerifyNativeArguments(p, []byte(`{"text":"plan"}`)); err != nil {
			return err
		}
		c.NativeToolCallID = "new-call"
		_, err := s.PublishProposal(ctx, c, "new plan", NativeOrigin{EntryID: c.Lineage.AnchorEntryID, Complete: true})
		return err
	}
	input := AcceptanceInput{piquestions.UUID(), p.PlanID, 1, p.TextSHA256}
	if _, err = (PolicyStore{Store: s, Adapter: a}).Accept(ctx, c.Handle.Conversation, input); !errors.Is(err, piquestions.ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.LookupAcceptance(ctx, c.Handle.Conversation, input.AcceptanceID); !errors.Is(err, piquestions.ErrNotFound) {
		t.Fatal("captured superseded plan", err)
	}
}
