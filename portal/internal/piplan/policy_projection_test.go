package piplan

import (
	"context"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func TestPolicyProjectionUsesCompleteInvalidatedHostEvidenceWithoutNativeCalls(t *testing.T) {
	ctx := context.Background()
	s, c, a, ctl := adapterFixture(t)
	p, err := s.PublishProposal(ctx, c, "exact proposal", NativeOrigin{"anchor", true})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"find", "grep", "ls", "publish_plan", "read", "request_user_input_async"}
	policy := piruntime.ToolPolicyTarget{Mode: "plan", Revision: 1, Active: names, RegisteredCeiling: names}
	policy.Digest = piruntime.ToolPolicyDigest(policy)
	observation := PolicyObservation{Current: c.Handle, Observed: c.Handle, CurrentBindingRevision: 2, ObservedBindingRevision: 2, Retained: policy, Proof: piruntime.ToolPolicyProof{Policy: policy, Generation: c.Handle.Generation, Active: names, Callable: names, Registered: names, Applied: true}, Idle: true, Eligible: true, Callbacks: 1, LinearLineage: 1, GuardPolicy: 1, GuardConversationSchema: 2, QuestionsReleased: true, PlanPublicationReleased: true, ImplementationReleased: true, ModeReleased: true, BoundHostTools: []string{"publish_plan", "request_user_input_async"}, Proposal: &ProposalObservation{PlanID: p.PlanID, Revision: p.Revision, TextSHA256: p.TextSHA256, Caller: c}}
	store := PolicyStore{Store: s, Adapter: a, Observations: func(context.Context, agentbackend.ConversationRef) (PolicyObservation, error) {
		return observation, nil
	}}
	snapshot, err := store.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || !snapshot.CanSelectMode || !snapshot.Proposals[0].CanAccept || snapshot.Proposals[0].PlanID != p.PlanID {
		t.Fatal(snapshot, err)
	}
	if ctl.inspect != 0 || ctl.apply != 0 {
		t.Fatal("projection called native policy")
	}
	good := observation
	for _, change := range []func(*PolicyObservation){func(o *PolicyObservation) { o.Pending = true }, func(o *PolicyObservation) { o.Held = true }, func(o *PolicyObservation) { o.ObservedBindingRevision++ }, func(o *PolicyObservation) { o.Observed.Generation = "stale" }, func(o *PolicyObservation) { o.GuardPolicy = 0 }, func(o *PolicyObservation) { o.Callbacks = 0 }, func(o *PolicyObservation) { o.BoundHostTools = nil }, func(o *PolicyObservation) { o.Proof.Callable = []string{"bash"} }} {
		observation = good
		change(&observation)
		snapshot, err = store.Snapshot(ctx, c.Handle.Conversation)
		if err != nil || snapshot.CanSelectMode || snapshot.Proposals[0].CanAccept {
			t.Fatal(snapshot, err)
		}
	}
	observation = good
	stale := c
	stale.Lineage.SelectionToken = "changed"
	observation.Proposal = &ProposalObservation{PlanID: p.PlanID, Revision: p.Revision, TextSHA256: p.TextSHA256, Caller: stale}
	snapshot, err = store.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || !snapshot.CanSelectMode || snapshot.Proposals[0].CanAccept {
		t.Fatal(snapshot, err)
	}
	store.Observations = nil
	snapshot, err = store.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || snapshot.CanSelectMode || snapshot.Proposals[0].CanAccept || ctl.inspect != 0 {
		t.Fatal(snapshot, err)
	}
}

func TestPolicyProjectionBindsCompleteProposalTupleAndAbsentMode(t *testing.T) {
	ctx := context.Background()
	s, c, a, ctl := adapterFixture(t)
	names := []string{"find", "grep", "ls", "publish_plan", "read", "request_user_input_async"}
	policy := piruntime.ToolPolicyTarget{Mode: "plan", Revision: 1, Active: names, RegisteredCeiling: names}
	policy.Digest = piruntime.ToolPolicyDigest(policy)
	o := PolicyObservation{Current: c.Handle, Observed: c.Handle, CurrentBindingRevision: 2, ObservedBindingRevision: 2, Retained: policy, Proof: piruntime.ToolPolicyProof{Policy: policy, Generation: c.Handle.Generation, Active: names, Callable: names, Registered: names, Applied: true}, Idle: true, Eligible: true, Callbacks: 1, LinearLineage: 1, GuardPolicy: 1, GuardConversationSchema: 2, QuestionsReleased: true, PlanPublicationReleased: true, ImplementationReleased: true, ModeReleased: true, BoundHostTools: []string{"publish_plan", "request_user_input_async"}}
	store := PolicyStore{Store: s, Adapter: a, Observations: func(context.Context, agentbackend.ConversationRef) (PolicyObservation, error) { return o, nil }}
	view, err := store.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || !view.CanSelectMode || len(view.Proposals) != 0 {
		t.Fatal("no proposal should be required for manual mode", view, err)
	}
	if latest, err := s.LatestProposal(ctx, c.Handle.Conversation); err != nil || latest != nil {
		t.Fatal(latest, err)
	}
	p, err := s.PublishProposal(ctx, c, "bound proposal", NativeOrigin{EntryID: "anchor", Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	proof := ProposalObservation{PlanID: p.PlanID, Revision: p.Revision, TextSHA256: p.TextSHA256, Caller: c}
	for _, change := range []func(*ProposalObservation){
		func(p *ProposalObservation) { p.PlanID = "other" },
		func(p *ProposalObservation) { p.Revision++ },
		func(p *ProposalObservation) { p.TextSHA256 = Digest("other") },
		func(p *ProposalObservation) { p.Caller.Handle.Generation = "other" },
		func(p *ProposalObservation) { p.Caller.NativeToolCallID = "other" },
	} {
		changed := proof
		change(&changed)
		o.Proposal = &changed
		view, err := store.Snapshot(ctx, c.Handle.Conversation)
		if err != nil || !view.CanSelectMode || view.Proposals[0].CanAccept {
			t.Fatal(view, err)
		}
	}
	o.Proposal = &proof
	view, err = store.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || !view.Proposals[0].CanAccept || ctl.inspect != 0 || ctl.apply != 0 {
		t.Fatal(view, err, ctl)
	}
}
