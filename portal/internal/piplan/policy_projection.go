package piplan

import (
	"context"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// PolicyObservation is an immutable host-owned observation, never browser input.
// Its owner rechecks current binding and invalidates it before runtime, policy,
// authority, branch, attachment or domain changes. This is not a TTL/cache service.
type PolicyObservation struct {
	Current, Observed                                                                agentbackend.ExecutionHandle
	CurrentBindingRevision, ObservedBindingRevision                                  uint64
	Retained                                                                         piruntime.ToolPolicyState
	Proof                                                                            piruntime.ToolPolicyProof
	Pending, Held, Idle, Eligible                                                    bool
	Callbacks, LinearLineage, GuardPolicy, GuardConversationSchema                   int
	QuestionsReleased, PlanPublicationReleased, ImplementationReleased, ModeReleased bool
	BoundHostTools                                                                   []string
	ProposalCaller                                                                   *Caller
}
type PolicyObservations func(context.Context, agentbackend.ConversationRef) (PolicyObservation, error)

func contains(names []string, name string) bool {
	for _, v := range names {
		if v == name {
			return true
		}
	}
	return false
}
func (o PolicyObservation) available(ref agentbackend.ConversationRef) bool {
	if o.Current.Conversation != ref || o.Observed != o.Current || o.CurrentBindingRevision == 0 || o.ObservedBindingRevision != o.CurrentBindingRevision || !piquestions.Text(o.Current.Generation, 4096) || o.Pending || o.Held || !o.Idle || !o.Eligible || o.Callbacks != 1 || o.LinearLineage != 1 || o.GuardPolicy != 1 || o.GuardConversationSchema != 2 || !o.QuestionsReleased || !o.PlanPublicationReleased || !o.ModeReleased {
		return false
	}
	if piruntime.ValidateToolPolicyProof(o.Proof, o.Retained, o.Current.Generation) != nil {
		return false
	}
	for _, name := range []string{"find", "grep", "ls", "read", "publish_plan", "request_user_input_async"} {
		if !contains(o.Proof.Registered, name) {
			return false
		}
	}
	return contains(o.BoundHostTools, "publish_plan") && contains(o.BoundHostTools, "request_user_input_async")
}

// projectPolicy performs only local identity/proof comparisons. The required
// production observation/invalidation wiring remains with the portal/runtime.
func projectPolicy(snapshot Snapshot, record Record, ref agentbackend.ConversationRef, o PolicyObservation) Snapshot {
	if snapshot.Revision != record.Revision || !o.available(ref) {
		return snapshot
	}
	snapshot.CanSelectMode = true
	snapshot.Mode = o.Retained.Mode
	snapshot.ToolPolicyRevision = o.Retained.Revision
	snapshot.ToolPolicyDigest = o.Retained.Digest
	if !o.ImplementationReleased || o.ProposalCaller == nil || len(record.Proposals) == 0 {
		return snapshot
	}
	p := record.Proposals[len(record.Proposals)-1]
	if piquestions.ValidateCaller(*o.ProposalCaller) != nil || o.ProposalCaller.Handle != o.Current || !sameCaller(*o.ProposalCaller, p.Caller) || !p.Complete || Digest(p.Text) != p.TextSHA256 {
		return snapshot
	}
	for _, a := range record.Acceptances {
		if a.PlanID == p.PlanID {
			return snapshot
		}
	}
	for i := range snapshot.Proposals {
		v := &snapshot.Proposals[i]
		if v.PlanID == p.PlanID && v.Revision == p.Revision && v.TextSHA256 == p.TextSHA256 {
			v.CanAccept = true
		}
	}
	return snapshot
}
