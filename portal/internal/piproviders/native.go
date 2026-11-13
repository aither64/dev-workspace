package piproviders

import (
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// CompileNativePolicy compiles exact resolved intent, not a shortened or newly
// selected chain. The caller admits the native conversation and frozen startup
// map; compilation neither grants authority nor registers routing with a worker.
func CompileNativePolicy(c Catalog, r Resolved, conversation agentbackend.ConversationRef, frozen []piruntime.AccountBinding) (piruntime.NativeRoutingPolicy, error) {
	if !r.hasProvenance() || r.CatalogDigest != c.Digest {
		return piruntime.NativeRoutingPolicy{}, ErrInvalidPolicy
	}
	if conversation.Backend != agentbackend.Pi || conversation.Session != r.Session || conversation.Member != r.Member.Address || conversation.Incarnation != r.Member.Incarnation || !textOK(conversation.NativeID) {
		return piruntime.NativeRoutingPolicy{}, ErrStalePolicy
	}
	if err := ValidateCatalog(c); err != nil {
		return piruntime.NativeRoutingPolicy{}, err
	}
	selection := Selection{Chain: r.Chain, Budget: r.Budget, FallbackOn: r.FallbackOn}
	if err := validateSelection(c, VersionedSelection{Revision: 1, Selection: selection}); err != nil {
		return piruntime.NativeRoutingPolicy{}, err
	}
	required, err := ProjectRuntimeAccounts(c, r.Chain, piruntime.CanonicalCredentialRef)
	if err != nil {
		return piruntime.NativeRoutingPolicy{}, err
	}
	if len(frozen) == 0 || len(frozen) > 64 {
		return piruntime.NativeRoutingPolicy{}, ErrUnavailableRoute
	}
	accounts := map[string]string{}
	previous := ""
	for _, binding := range frozen {
		if !identifier.MatchString(binding.Provider) || !identifier.MatchString(binding.AccountRef) || previous != "" && previous >= binding.Provider {
			return piruntime.NativeRoutingPolicy{}, ErrUnavailableRoute
		}
		accounts[binding.Provider] = binding.AccountRef
		previous = binding.Provider
	}
	for _, binding := range required {
		if accounts[binding.Provider] != binding.AccountRef {
			return piruntime.NativeRoutingPolicy{}, ErrUnavailableRoute
		}
	}
	policy := piruntime.NativeRoutingPolicy{Conversation: conversation, Settings: r.Settings, CatalogDigest: r.CatalogDigest, PolicyDigest: r.PolicyDigest, MaxExtraAttempts: r.Budget.MaxExtraAttempts, MaxHops: r.Budget.MaxHops}
	for _, target := range r.Chain {
		provider, _, _ := findTarget(c, target)
		policy.Targets = append(policy.Targets, piruntime.NativeTarget{Provider: provider.NativeID, AccountRef: target.AccountRef, Model: target.ModelID, Thinking: target.Thinking})
	}
	// Explicit virtual policies encode no enabled categories as [], not null.
	// The originating policy digest already canonicalizes empty categories to [].
	policy.FallbackOn = make([]string, 0, len(r.FallbackOn))
	for _, category := range r.FallbackOn {
		policy.FallbackOn = append(policy.FallbackOn, string(category))
	}
	if err := validateNativePolicy(policy); err != nil {
		return piruntime.NativeRoutingPolicy{}, err
	}
	return policy, nil
}

func validateNativePolicy(p piruntime.NativeRoutingPolicy) error {
	if err := piruntime.ValidateNativeRoutingPolicy(p); err != nil {
		return errors.Join(ErrInvalidPolicy, err)
	}
	return nil
}

// PlanNativeRetry decides only a native same-input next hop from P3-certified
// observations. It cannot send, charge history, change settings or prove those
// observations itself. P3 rechecks the active receipt/generation and persists the
// cumulative charge before a worker may use an allowed target.
func PlanNativeRetry(policy piruntime.NativeRoutingPolicy, e piruntime.NativeRetryEvidence) (piruntime.NativeRetryDecision, error) {
	d := piruntime.NativeRetryDecision{Outcome: "held", Reason: "invalid_evidence", Counters: e.Counters}
	if err := validateNativePolicy(policy); err != nil {
		return d, err
	}
	if err := piruntime.ValidateNativeRetryEvidence(e); err != nil {
		d.Reason = "input_evidence_invalid"
		return d, nil
	}
	if e.Submission.Conversation != policy.Conversation || e.PolicyDigest != policy.PolicyDigest || !textOK(e.Generation) || !textOK(e.Submission.OperationID) || !textOK(e.Submission.Action) || !digestPattern.MatchString(e.Submission.Digest) {
		d.Reason = "input_identity_unproved"
		return d, nil
	}
	if !e.Accepted || !e.NativeRetry || !textOK(e.UserEntryID) || !textOK(e.StateEntryID) || !textOK(e.FailedEntryID) || e.UserEntryID == e.StateEntryID || e.UserEntryID == e.FailedEntryID || e.StateEntryID == e.FailedEntryID {
		d.Reason = "native_witness_unproved"
		return d, nil
	}
	if e.Cancelled {
		d.Outcome, d.Reason = "stop", "cancelled"
		return d, nil
	}
	if !e.FailedResponseEmpty || !e.FailedToolsAbsent || !e.PriorEffectsComplete || !e.NoPendingEffects || !e.LinearHistory {
		d.Reason = "output_effect_or_history_unproved"
		return d, nil
	}
	counters := e.Counters
	if counters.ChainIndex >= uint32(len(policy.Targets)) || counters.ExtraAttempts != counters.ChainIndex || counters.Hops != counters.ChainIndex || counters.ExtraAttempts > policy.MaxExtraAttempts || counters.Hops > policy.MaxHops || e.Actual != policy.Targets[counters.ChainIndex] {
		d.Reason = "target_or_counters_unproved"
		return d, nil
	}
	if !optionalText(e.FailureCategory) || !optionalText(e.FailureCode) || !optionalText(e.FailureEvidenceRef) || e.HTTPStatus != 0 && (e.HTTPStatus < 100 || e.HTTPStatus > 599) {
		return d, ErrInvalidPolicy
	}
	category := FailureCategory(e.FailureCategory)
	if category == Unknown || !validCategory(category) || category == Transport || !textOK(e.FailureCode) || !textOK(e.FailureEvidenceRef) {
		d.Reason = "failure_source_unproved"
		return d, nil
	}
	eligible := category == RateLimit && e.FailureCode == "rate_limit_exceeded" && e.HTTPStatus == 429 || category == Overload && e.FailureCode == "overloaded" && e.HTTPStatus == 503
	if (category == RateLimit || category == Overload) && !eligible {
		d.Reason = "failure_source_unproved"
		return d, nil
	}
	if !eligible {
		d.Outcome, d.Reason = "stop", "nonretryable_failure"
		return d, nil
	}
	enabled := false
	for _, allowed := range policy.FallbackOn {
		if allowed == e.FailureCategory {
			enabled = true
		}
	}
	if !enabled {
		d.Outcome, d.Reason = "stop", "fallback_disabled"
		return d, nil
	}
	if counters.ExtraAttempts >= policy.MaxExtraAttempts || counters.Hops >= policy.MaxHops || counters.ChainIndex+1 >= uint32(len(policy.Targets)) {
		d.Outcome, d.Reason = "stop", "finite_budget"
		return d, nil
	}
	target := policy.Targets[counters.ChainIndex+1]
	d.Outcome, d.Reason, d.Target = "allow", "next_native_target", &target
	d.Counters = piruntime.NativeRetryCounters{ChainIndex: counters.ChainIndex + 1, ExtraAttempts: counters.ExtraAttempts + 1, Hops: counters.Hops + 1}
	return d, nil
}
