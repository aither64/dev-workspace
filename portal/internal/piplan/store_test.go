package piplan

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

func fixture(t *testing.T) (*FileStore, Caller) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-example", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native"}
	return &FileStore{Root: root, Validate: func(context.Context, agentbackend.ConversationRef, *Caller) error { return nil }, Policy: func(context.Context, agentbackend.ConversationRef, Caller) (Policy, error) {
		return Policy{OriginalMode: "plan", Revision: "proof", Idle: true, Eligible: true, Enforced: true}, nil
	}}, Caller{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: "generation"}, Lineage: piquestions.LineageRef{AnchorEntryID: "anchor", SelectionToken: "branch"}, NativeToolCallID: "call"}
}
func TestExactPlanAndAcceptanceIdentity(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	p, err := s.PublishProposal(ctx, c, "Plan\r\n  exact bytes \n", NativeOrigin{"entry", true})
	if err != nil {
		t.Fatal(err)
	}
	if p.TextSHA256 != Digest(p.Text) || p.TextSHA256 == Digest(strings.TrimSpace(p.Text)) {
		t.Fatal(p)
	}
	in := AcceptanceInput{piquestions.UUID(), p.PlanID, p.Revision, p.TextSHA256}
	a, err := s.Accept(ctx, c.Handle.Conversation, in)
	if err != nil {
		t.Fatal(err)
	}
	c.NativeToolCallID = "new-call"
	if _, err = s.PublishProposal(ctx, c, "New plan", NativeOrigin{"new-entry", true}); err != nil {
		t.Fatal(err)
	}
	again, err := s.Accept(ctx, c.Handle.Conversation, in)
	if err != nil || again.OperationID != a.OperationID {
		t.Fatal(again, err)
	}
	in.AcceptanceID = piquestions.UUID()
	if _, err = s.Accept(ctx, c.Handle.Conversation, in); !errors.Is(err, piquestions.ErrConflict) {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || snapshot.CanSelectMode || snapshot.Proposals[0].CanAccept {
		t.Fatal(snapshot, err)
	}
}
func TestModeAckLossDoesNotApplyAgain(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	p, err := s.PublishProposal(ctx, c, "Plan", NativeOrigin{"entry", true})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Accept(ctx, c.Handle.Conversation, AcceptanceInput{piquestions.UUID(), p.PlanID, 1, p.TextSHA256})
	if err != nil {
		t.Fatal(err)
	}
	applies, observes, sends := 0, 0, 0
	svc := Service{Store: s, Ports: Ports{Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, ApplyDefault: func(context.Context, Caller, string) (bool, error) { applies++; return false, errors.New("lost") }, ObserveDefault: func(context.Context, Caller, string) (bool, error) { observes++; return true, nil }, Prepare: func(context.Context, Caller, Acceptance) (string, error) { return "original-receipt", nil }, Send: func(context.Context, Caller, Acceptance) (string, error) { sends++; return "NotSubmitted", nil }, Lookup: func(context.Context, Caller, Acceptance) (string, error) { return "NotSubmitted", nil }}}
	if err = svc.Advance(ctx, c.Handle.Conversation, a.AcceptanceID); err == nil {
		t.Fatal("mode uncertainty accepted")
	}
	for range 3 {
		if err = svc.Advance(ctx, c.Handle.Conversation, a.AcceptanceID); err != nil {
			t.Fatal(err)
		}
	}
	if applies != 1 || observes != 1 || sends != 1 {
		t.Fatal(applies, observes, sends)
	}
}
