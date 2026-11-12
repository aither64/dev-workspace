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
