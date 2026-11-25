package piplan

import (
	"context"
	"os"
	"testing"
)

func TestLatestProposalReadIsDetachedStrictAndDoesNotCreateState(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	path, err := s.file(c.Handle.Conversation).Path()
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.LatestProposal(ctx, c.Handle.Conversation); err != nil || p != nil {
		t.Fatal(p, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created state", err)
	}
	view, err := s.PublishProposal(ctx, c, "exact text", NativeOrigin{EntryID: "anchor", Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.LatestProposal(ctx, c.Handle.Conversation)
	if err != nil || p.PlanID != view.PlanID {
		t.Fatal(p, err)
	}
	p.Text = "changed copy"
	p.Caller.NativeToolCallID = "changed copy"
	p.NativeOrigin.EntryID = "changed copy"
	again, err := s.LatestProposal(ctx, c.Handle.Conversation)
	if err != nil || again.Text != "exact text" || again.Caller != c || again.NativeOrigin.EntryID != "anchor" {
		t.Fatal(again, err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":1,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LatestProposal(ctx, c.Handle.Conversation); err == nil {
		t.Fatal("malformed state became absence")
	}
}

func TestPublicationsRetainOlderCallsAndDetachedAcceptance(t *testing.T) {
	ctx := context.Background()
	s, caller := fixture(t)
	first, err := s.PublishProposal(ctx, caller, "first original", NativeOrigin{EntryID: "anchor", Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, caller.Handle.Conversation, AcceptanceInput{AcceptanceID: "11111111-1111-4111-8111-111111111111", PlanID: first.PlanID, Revision: first.Revision, TextSHA256: first.TextSHA256}); err != nil {
		t.Fatal(err)
	}
	caller.NativeToolCallID = "new-call"
	caller.Lineage.AnchorEntryID = "new-anchor"
	if _, err := s.PublishProposal(ctx, caller, "newer original", NativeOrigin{EntryID: "new-anchor", Complete: true}); err != nil {
		t.Fatal(err)
	}
	proposals, accepts, err := s.Publications(ctx, caller.Handle.Conversation)
	if err != nil || len(proposals) != 2 || len(accepts) != 1 || proposals[0].Text != "first original" || proposals[1].Text != "newer original" {
		t.Fatal(proposals, accepts, err)
	}
	proposals[0].Text = "mutated"
	accepts[0].Message = "mutated"
	again, saved, err := s.Publications(ctx, caller.Handle.Conversation)
	if err != nil || again[0].Text != "first original" || saved[0].Message == "mutated" {
		t.Fatal("shared store state", again, saved, err)
	}
	path, _ := s.file(caller.Handle.Conversation).Path()
	os.WriteFile(path, []byte(`{"schema":1,"unknown":true}`), 0600)
	if _, _, err := s.Publications(ctx, caller.Handle.Conversation); err == nil {
		t.Fatal("corrupt publications became absence")
	}
}
