package piproviders

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func nativeFixture(t *testing.T) (Catalog, Resolved, agentbackend.ConversationRef, []piruntime.AccountBinding) {
	t.Helper()
	c, p, m := canonicalFixture()
	r, err := Resolve(c, p, m, 1)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := SelectableRuntimeAccounts(c, piruntime.CanonicalCredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	conversation := handle(r).Conversation
	conversation.NativeID = "22222222-2222-4222-8222-222222222222"
	return c, r, conversation, bindings
}

func compileNativeFixture(t *testing.T) piruntime.NativeRoutingPolicy {
	t.Helper()
	c, r, conversation, bindings := nativeFixture(t)
	policy, err := CompileNativePolicy(c, r, conversation, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func nativeEvidence(p piruntime.NativeRoutingPolicy) piruntime.NativeRetryEvidence {
	return piruntime.NativeRetryEvidence{
		Submission: piruntime.SubmissionRef{Conversation: p.Conversation, OperationID: "11111111-1111-4111-8111-111111111111", Action: "send", Digest: strings.Repeat("b", 64)}, Generation: strings.Repeat("c", 64), PolicyDigest: p.PolicyDigest,
		UserEntryID: "user-entry", StateEntryID: "routing-state-entry", FailedEntryID: "failed-entry", Actual: p.Targets[0], FailureCategory: string(Overload), FailureCode: "overloaded", FailureEvidenceRef: "certified-native-response", HTTPStatus: 503,
		Accepted: true, NativeRetry: true, FailedResponseEmpty: true, FailedToolsAbsent: true, PriorEffectsComplete: true, NoPendingEffects: true, LinearHistory: true,
	}
}

func TestCompileNativePolicyExactOriginalIntentAndOwnedSlices(t *testing.T) {
	c, r, conversation, bindings := nativeFixture(t)
	before, _ := json.Marshal(r)
	policy, err := CompileNativePolicy(c, r, conversation, bindings)
	if err != nil {
		t.Fatal(err)
	}
	want := []piruntime.NativeTarget{{Provider: "native-a", AccountRef: "account-a", Model: "model-a", Thinking: "medium"}, {Provider: "native-b", AccountRef: "account-b", Model: "model-b", Thinking: "medium"}}
	if !reflect.DeepEqual(policy.Targets, want) || policy.Conversation != conversation || policy.Settings != r.Settings || policy.CatalogDigest != c.Digest || policy.PolicyDigest != r.PolicyDigest || policy.MaxExtraAttempts != r.Budget.MaxExtraAttempts || policy.MaxHops != r.Budget.MaxHops {
		t.Fatalf("changed compiled intent %+v", policy)
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) || !r.hasProvenance() {
		t.Fatal("compiler mutated resolved intent")
	}
	policy.Targets[0].Model = "mutated"
	policy.FallbackOn[0] = string(Unknown)
	bindings[0].AccountRef = "mutated"
	if r.Chain[0].ModelID != "model-a" || r.FallbackOn[0] != RateLimit || !r.hasProvenance() {
		t.Fatal("compiled slices alias source")
	}
	// Reset caller-owned map: recompiling must not reuse mutable previous output.
	_, _, _, bindings = nativeFixture(t)
	again, err := CompileNativePolicy(c, r, conversation, bindings)
	if err != nil || !reflect.DeepEqual(again.Targets, want) {
		t.Fatal("compiler retained mutated output")
	}
}

func TestCompileNativePolicyEmptyCategoriesPreserveIntentAndWireArray(t *testing.T) {
	for _, categories := range [][]FailureCategory{nil, make([]FailureCategory, 0, 1)} {
		c, p, member := canonicalFixture()
		p.Default.Selection.FallbackOn = categories
		beforePolicy, _ := json.Marshal(p)
		digestBytes := policyBytes(c, "session_default", p.Default)
		if !bytes.HasSuffix(digestBytes, []byte(",[]]")) {
			t.Fatalf("owning digest must already encode empty categories as []: %s", digestBytes)
		}
		r, err := Resolve(c, p, member, 1)
		if err != nil {
			t.Fatal(err)
		}
		beforeResolved, _ := json.Marshal(r)
		conversation := handle(r).Conversation
		conversation.NativeID = "22222222-2222-4222-8222-222222222222"
		bindings, err := SelectableRuntimeAccounts(c, piruntime.CanonicalCredentialRef)
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := CompileNativePolicy(c, r, conversation, bindings)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(compiled)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		if compiled.FallbackOn == nil || string(fields["FallbackOn"]) != "[]" {
			t.Fatalf("virtual projection requires exact FallbackOn array: %s", wire)
		}
		wantTargets := []piruntime.NativeTarget{{Provider: "native-a", AccountRef: "account-a", Model: "model-a", Thinking: "medium"}, {Provider: "native-b", AccountRef: "account-b", Model: "model-b", Thinking: "medium"}}
		if !reflect.DeepEqual(compiled.Targets, wantTargets) || compiled.Settings != r.Settings || compiled.PolicyDigest != r.PolicyDigest || compiled.CatalogDigest != r.CatalogDigest || compiled.MaxExtraAttempts != r.Budget.MaxExtraAttempts || compiled.MaxHops != r.Budget.MaxHops {
			t.Fatalf("empty categories changed original compiled intent: %+v", compiled)
		}
		decision, err := PlanNativeRetry(compiled, nativeEvidence(compiled))
		if err != nil || decision.Outcome != "stop" || decision.Reason != "fallback_disabled" || decision.Target != nil || decision.Counters != (piruntime.NativeRetryCounters{}) {
			t.Fatalf("empty categories authorized a hop: %+v %v", decision, err)
		}
		again, err := CompileNativePolicy(c, r, conversation, bindings)
		if err != nil || !reflect.DeepEqual(again, compiled) {
			t.Fatalf("recompile changed projection: %+v %v", again, err)
		}
		compiled.FallbackOn = append(compiled.FallbackOn, string(Overload))
		compiled.Targets[0].Model = "changed"
		afterPolicy, _ := json.Marshal(p)
		afterResolved, _ := json.Marshal(r)
		if len(again.FallbackOn) != 0 || again.FallbackOn == nil || again.Targets[0].Model != "model-a" || r.FallbackOn != nil || !r.hasProvenance() || !bytes.Equal(beforePolicy, afterPolicy) || !bytes.Equal(beforeResolved, afterResolved) || !bytes.Equal(digestBytes, policyBytes(c, "session_default", p.Default)) {
			t.Fatal("compiled projection aliases or changes original policy/digest/provenance")
		}
	}
}

func TestCompileNativePolicyRefusesUnprovedScopeCatalogAndMappings(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Catalog, *Resolved, *agentbackend.ConversationRef, *[]piruntime.AccountBinding)
		want error
	}{
		{"missing_provenance", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			r.provenance = nil
		}, ErrInvalidPolicy},
		{"changed_chain", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			r.Chain = r.Chain[:1]
		}, ErrInvalidPolicy},
		{"catalog_digest", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			c.Digest = strings.Repeat("d", 64)
		}, ErrInvalidPolicy},
		{"native_id", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			ref.NativeID = ""
		}, ErrStalePolicy},
		{"session", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			ref.Session.Incarnation = strings.Repeat("e", 64)
		}, ErrStalePolicy},
		{"member", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			ref.Member = "lead"
		}, ErrStalePolicy},
		{"incarnation", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			ref.Incarnation = strings.Repeat("f", 64)
		}, ErrStalePolicy},
		{"backend", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			ref.Backend = agentbackend.Codex
		}, ErrStalePolicy},
		{"fallback_unavailable", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			c.Routes[1].State = "unverified"
		}, ErrUnavailableRoute},
		{"fallback_store", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			c.Accounts[1].CredentialRef = "other-store"
		}, ErrUnavailableRoute},
		{"fallback_key", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			c.Accounts[1].CredentialKey = "other-key"
		}, ErrUnavailableRoute},
		{"missing_frozen_map", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			*b = nil
		}, ErrUnavailableRoute},
		{"missing_fallback_binding", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			*b = (*b)[:1]
		}, ErrUnavailableRoute},
		{"different_account", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			(*b)[1].AccountRef = "other-account"
		}, ErrUnavailableRoute},
		{"unsorted_map", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			(*b)[0], (*b)[1] = (*b)[1], (*b)[0]
		}, ErrUnavailableRoute},
		{"duplicate_map", func(c *Catalog, r *Resolved, ref *agentbackend.ConversationRef, b *[]piruntime.AccountBinding) {
			*b = append((*b)[:1], (*b)[0])
		}, ErrUnavailableRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r, ref, bindings := nativeFixture(t)
			tc.edit(&c, &r, &ref, &bindings)
			got, err := CompileNativePolicy(c, r, ref, bindings)
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(got, piruntime.NativeRoutingPolicy{}) {
				t.Fatalf("want %v got %+v %v", tc.want, got, err)
			}
		})
	}
}

func TestPlanNativeRetryAllowsOnlyCertifiedNextTarget(t *testing.T) {
	policy := compileNativeFixture(t)
	e := nativeEvidence(policy)
	beforePolicy, _ := json.Marshal(policy)
	beforeEvidence, _ := json.Marshal(e)
	d, err := PlanNativeRetry(policy, e)
	if err != nil || d.Outcome != "allow" || d.Target == nil || *d.Target != policy.Targets[1] || d.Counters != (piruntime.NativeRetryCounters{ChainIndex: 1, ExtraAttempts: 1, Hops: 1}) {
		t.Fatalf("next hop %+v %v", d, err)
	}
	afterPolicy, _ := json.Marshal(policy)
	afterEvidence, _ := json.Marshal(e)
	if string(beforePolicy) != string(afterPolicy) || string(beforeEvidence) != string(afterEvidence) {
		t.Fatal("decision mutated policy/evidence")
	}
	d.Target.Model = "mutated"
	if policy.Targets[1].Model != "model-b" {
		t.Fatal("decision target aliases policy")
	}
	e.FailureCategory = string(RateLimit)
	e.FailureCode = "rate_limit_exceeded"
	e.HTTPStatus = 429
	if d, err = PlanNativeRetry(policy, e); err != nil || d.Outcome != "allow" {
		t.Fatalf("exact rate limit %+v %v", d, err)
	}
}

func TestPlanNativeRetryHoldsMissingOrInconsistentProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*piruntime.NativeRetryEvidence)
	}{
		{"not_accepted", func(e *piruntime.NativeRetryEvidence) { e.Accepted = false }},
		{"not_native_retry", func(e *piruntime.NativeRetryEvidence) { e.NativeRetry = false }},
		{"wrong_conversation", func(e *piruntime.NativeRetryEvidence) { e.Submission.Conversation.NativeID = "other" }},
		{"wrong_member", func(e *piruntime.NativeRetryEvidence) { e.Submission.Conversation.Member = "lead" }},
		{"wrong_policy", func(e *piruntime.NativeRetryEvidence) { e.PolicyDigest = strings.Repeat("d", 64) }},
		{"missing_generation", func(e *piruntime.NativeRetryEvidence) { e.Generation = "" }},
		{"missing_digest", func(e *piruntime.NativeRetryEvidence) { e.Submission.Digest = "" }},
		{"missing_user", func(e *piruntime.NativeRetryEvidence) { e.UserEntryID = "" }},
		{"missing_state", func(e *piruntime.NativeRetryEvidence) { e.StateEntryID = "" }},
		{"missing_failed", func(e *piruntime.NativeRetryEvidence) { e.FailedEntryID = "" }},
		{"shared_entry", func(e *piruntime.NativeRetryEvidence) { e.StateEntryID = e.FailedEntryID }},
		{"partial_output", func(e *piruntime.NativeRetryEvidence) { e.FailedResponseEmpty = false }},
		{"failed_tool", func(e *piruntime.NativeRetryEvidence) { e.FailedToolsAbsent = false }},
		{"prior_effect", func(e *piruntime.NativeRetryEvidence) { e.PriorEffectsComplete = false }},
		{"pending_effect", func(e *piruntime.NativeRetryEvidence) { e.NoPendingEffects = false }},
		{"nonlinear", func(e *piruntime.NativeRetryEvidence) { e.LinearHistory = false }},
		{"wrong_actual", func(e *piruntime.NativeRetryEvidence) { e.Actual.AccountRef = "other" }},
		{"counter_reset", func(e *piruntime.NativeRetryEvidence) { e.Counters.Hops = 1 }},
		{"counter_skip", func(e *piruntime.NativeRetryEvidence) {
			e.Counters = piruntime.NativeRetryCounters{ChainIndex: 2, ExtraAttempts: 2, Hops: 2}
		}},
		{"missing_failure_source", func(e *piruntime.NativeRetryEvidence) { e.FailureEvidenceRef = "" }},
		{"status_only", func(e *piruntime.NativeRetryEvidence) { e.FailureCode = "" }},
		{"wrong_status", func(e *piruntime.NativeRetryEvidence) { e.HTTPStatus = 429 }},
		{"text_classifier_only", func(e *piruntime.NativeRetryEvidence) { e.FailureCode = "provider busy" }},
		{"unknown", func(e *piruntime.NativeRetryEvidence) { e.FailureCategory = string(Unknown) }},
		{"transport_uncertainty", func(e *piruntime.NativeRetryEvidence) {
			e.FailureCategory = string(Transport)
			e.FailureCode = "timeout"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := compileNativeFixture(t)
			e := nativeEvidence(p)
			tc.edit(&e)
			d, err := PlanNativeRetry(p, e)
			if err != nil || d.Outcome != "held" || d.Target != nil || d.Counters != e.Counters {
				t.Fatalf("unsafe next target %+v %v", d, err)
			}
		})
	}
}

func TestPlanNativeRetryStopsDisabledNonretryableCancelledOrExhausted(t *testing.T) {
	for _, category := range []FailureCategory{Configuration, Authentication, RefreshStorage, ModelUnavailable, QuotaBilling, StructuredPolicy, ContextLimit, TextRefusal, Cancelled} {
		t.Run(string(category), func(t *testing.T) {
			p := compileNativeFixture(t)
			e := nativeEvidence(p)
			e.FailureCategory = string(category)
			e.FailureCode = "nonretryable"
			d, err := PlanNativeRetry(p, e)
			if err != nil || d.Outcome != "stop" || d.Target != nil {
				t.Fatalf("nonretryable %+v %v", d, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*piruntime.NativeRoutingPolicy, *piruntime.NativeRetryEvidence)
	}{
		{"disabled", func(p *piruntime.NativeRoutingPolicy, e *piruntime.NativeRetryEvidence) { p.FallbackOn = nil }},
		{"cancelled", func(p *piruntime.NativeRoutingPolicy, e *piruntime.NativeRetryEvidence) { e.Cancelled = true }},
		{"zero_budget", func(p *piruntime.NativeRoutingPolicy, e *piruntime.NativeRetryEvidence) {
			p.MaxExtraAttempts = 0
			p.MaxHops = 0
		}},
		{"finite_chain", func(p *piruntime.NativeRoutingPolicy, e *piruntime.NativeRetryEvidence) {
			e.Counters = piruntime.NativeRetryCounters{ChainIndex: 1, ExtraAttempts: 1, Hops: 1}
			e.Actual = p.Targets[1]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := compileNativeFixture(t)
			e := nativeEvidence(p)
			tc.edit(&p, &e)
			d, err := PlanNativeRetry(p, e)
			if err != nil || d.Outcome != "stop" || d.Target != nil || d.Counters != e.Counters {
				t.Fatalf("terminal changed counters/target %+v %v", d, err)
			}
		})
	}
}

func TestNativeRetryBudgetPersistsAcrossSuccessfulContinuation(t *testing.T) {
	p := compileNativeFixture(t)
	p.Targets = append(p.Targets, piruntime.NativeTarget{Provider: "native-b", AccountRef: "account-b", Model: "model-c", Thinking: "medium"})
	p.MaxExtraAttempts = 1
	p.MaxHops = 1
	e := nativeEvidence(p)
	first, err := PlanNativeRetry(p, e)
	if err != nil || first.Outcome != "allow" {
		t.Fatal(err)
	}
	// The successful B response/tool prefix does not make a new operation or
	// budget. P3 certifies completion and supplies the retained charged counters.
	e.Counters = first.Counters
	e.Actual = *first.Target
	e.FailedEntryID = "later-failure"
	e.StateEntryID = "charged-state"
	last, err := PlanNativeRetry(p, e)
	if err != nil || last.Outcome != "stop" || last.Target != nil || last.Counters != first.Counters {
		t.Fatalf("budget reset before C %+v %v", last, err)
	}
}

func TestNativePolicyMalformedOrCyclicNeverAllows(t *testing.T) {
	for _, edit := range []func(*piruntime.NativeRoutingPolicy){func(p *piruntime.NativeRoutingPolicy) { p.Settings.Revision = 0 }, func(p *piruntime.NativeRoutingPolicy) { p.Settings.PolicyRevision = "wrong" }, func(p *piruntime.NativeRoutingPolicy) { p.Targets = append(p.Targets, p.Targets[0]) }, func(p *piruntime.NativeRoutingPolicy) { p.MaxHops = 8 }, func(p *piruntime.NativeRoutingPolicy) { p.FallbackOn = append(p.FallbackOn, p.FallbackOn[0]) }} {
		p := compileNativeFixture(t)
		e := nativeEvidence(p)
		edit(&p)
		d, err := PlanNativeRetry(p, e)
		if !errors.Is(err, ErrInvalidPolicy) || d.Outcome != "held" || d.Target != nil {
			t.Fatalf("malformed policy %+v %v", d, err)
		}
	}
}
