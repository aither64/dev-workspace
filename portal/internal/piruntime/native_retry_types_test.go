package piruntime

import (
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestNativeRetryStructuralContractsHaveNoExecution(t *testing.T) {
	conversation := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/workspace", Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "11111111-1111-4111-8111-111111111111"}
	settings := EffectiveSettings{Provider: "provider-a", AccountRef: "account-a", Model: "model", Thinking: "off", Revision: 1, PolicyRevision: strings.Repeat("c", 64)}
	policy := NativeRoutingPolicy{Conversation: conversation, Settings: settings, CatalogDigest: strings.Repeat("d", 64), PolicyDigest: settings.PolicyRevision, Targets: []NativeTarget{{"provider-a", "account-a", "model", "off"}, {"provider-b", "account-b", "other", "high"}}, MaxExtraAttempts: 1, MaxHops: 1, FallbackOn: []string{"overload"}}
	if err := ValidateNativeRoutingPolicy(policy); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*NativeRoutingPolicy){func(p *NativeRoutingPolicy) { p.Targets = nil }, func(p *NativeRoutingPolicy) { p.PolicyDigest = "bad" }, func(p *NativeRoutingPolicy) { p.MaxHops = 8 }, func(p *NativeRoutingPolicy) { p.MaxExtraAttempts = 0 }, func(p *NativeRoutingPolicy) { p.FallbackOn = []string{"unknown-category"} }, func(p *NativeRoutingPolicy) { p.Settings.Revision = 0 }, func(p *NativeRoutingPolicy) {
		p.Targets = []NativeTarget{{"provider-a", "account-a", "model", "off"}, {"provider-a", "account-a", "model", "high"}}
	}} {
		bad := policy
		change(&bad)
		if ValidateNativeRoutingPolicy(bad) == nil {
			t.Fatal("invalid routing policy accepted")
		}
	}
	if ValidateNativeTarget(NativeTarget{"dev-workspace-router", "a", "m", "off"}) == nil {
		t.Fatal("virtual target accepted as physical")
	}
	evidence := NativeRetryEvidence{Submission: SubmissionRef{Conversation: conversation, OperationID: "22222222-2222-4222-8222-222222222222", Action: "send", Digest: strings.Repeat("e", 64)}, Generation: strings.Repeat("f", 64), PolicyDigest: policy.PolicyDigest}
	// Missing native/effect truth remains representable. This data validator
	// deliberately cannot turn false/missing observations into retry permission.
	if err := ValidateNativeRetryEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*NativeRetryEvidence){func(e *NativeRetryEvidence) { e.Generation = "bad" }, func(e *NativeRetryEvidence) { e.Submission.OperationID = "../path" }, func(e *NativeRetryEvidence) { e.Submission.Action = "retry" }, func(e *NativeRetryEvidence) { e.HTTPStatus = 99 }, func(e *NativeRetryEvidence) { e.Counters.Hops = 8 }, func(e *NativeRetryEvidence) { e.FailureCategory = "free-form" }} {
		bad := evidence
		change(&bad)
		if ValidateNativeRetryEvidence(bad) == nil {
			t.Fatal("invalid structural evidence accepted")
		}
	}
}
