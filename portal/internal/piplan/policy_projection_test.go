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
	observation := PolicyObservation{Current: c.Handle, Observed: c.Handle, CurrentBindingRevision: 2, ObservedBindingRevision: 2, Retained: policy, Proof: piruntime.ToolPolicyProof{Policy: policy, Generation: c.Handle.Generation, Active: names, Callable: names, Registered: names, Applied: true}, Idle: true, Eligible: true, Callbacks: 1, LinearLineage: 1, GuardPolicy: 1, GuardConversationSchema: 2, QuestionsReleased: true, PlanPublicationReleased: true, ImplementationReleased: true, ModeReleased: true, BoundHostTools: []string{"publish_plan", "request_user_input_async"}, ProposalCaller: &c}
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
	observation.ProposalCaller = &stale
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
