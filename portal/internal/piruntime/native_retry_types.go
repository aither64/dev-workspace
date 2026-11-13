package piruntime

import (
	"context"
	"slices"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// These data-only contracts do not register a router or implement permission,
// charging, native evidence, retries or another submission operation.
type NativeTarget struct{ Provider, AccountRef, Model, Thinking string }
type NativeRoutingPolicy struct {
	Conversation                agentbackend.ConversationRef
	Settings                    EffectiveSettings
	CatalogDigest, PolicyDigest string
	Targets                     []NativeTarget
	MaxExtraAttempts, MaxHops   uint32
	FallbackOn                  []string
}
type NativeRoutingSelection struct{ Provider, Model, Thinking, PolicyDigest string }
type NativeRoutingResult struct {
	Outcome   string
	Selection NativeRoutingSelection
}
type NativeRetryCounters struct{ ChainIndex, ExtraAttempts, Hops uint32 }
type NativeRetryEvidence struct {
	Submission                                                       SubmissionRef
	Generation, PolicyDigest                                         string
	UserEntryID, StateEntryID, FailedEntryID                         string
	Counters                                                         NativeRetryCounters
	Actual                                                           NativeTarget
	FailureCategory, FailureCode, FailureEvidenceRef                 string
	HTTPStatus                                                       int
	Accepted, NativeRetry, FailedResponseEmpty, FailedToolsAbsent    bool
	PriorEffectsComplete, NoPendingEffects, LinearHistory, Cancelled bool
}
type NativeRetryDecision struct {
	Outcome, Reason string
	Target          *NativeTarget
	Counters        NativeRetryCounters
}
type NativeRetryRequest struct {
	Runtime  RuntimeRef
	Handle   agentbackend.ExecutionHandle
	Evidence NativeRetryEvidence
}
type NativeRetryHandler interface {
	PrepareNativeRetry(context.Context, NativeRetryRequest) (NativeRetryDecision, error)
}

func ValidateNativeTarget(target NativeTarget) error {
	if !accountIdentifier.MatchString(target.Provider) || target.Provider == "dev-workspace-router" || !accountIdentifier.MatchString(target.AccountRef) || target.Model == "" || len(target.Model) > 4096 || !validString(target.Model) {
		return ErrScope
	}
	switch target.Thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return nil
	default:
		return ErrScope
	}
}
func nativeFailureCategory(value string) bool {
	return slices.Contains([]string{"configuration", "authentication", "refresh_storage", "model_unavailable", "rate_limit", "overload", "quota_billing", "transport", "structured_policy", "context_limit", "text_refusal", "cancelled", "unknown"}, value)
}
func ValidateNativeRoutingPolicy(policy NativeRoutingPolicy) error {
	if validateHandle(agentbackend.ExecutionHandle{Conversation: policy.Conversation, Generation: strings.Repeat("0", 64)}) != nil || validateSettings(policy.Settings) != nil || !hexPattern.MatchString(policy.CatalogDigest) || !hexPattern.MatchString(policy.PolicyDigest) || policy.Settings.PolicyRevision != policy.PolicyDigest || len(policy.Targets) < 1 || len(policy.Targets) > 8 || policy.MaxExtraAttempts > 7 || policy.MaxHops > 7 || policy.MaxHops > policy.MaxExtraAttempts || len(policy.FallbackOn) > 13 {
		return ErrScope
	}
	seen := map[[3]string]bool{}
	for _, target := range policy.Targets {
		if ValidateNativeTarget(target) != nil {
			return ErrScope
		}
		key := [3]string{target.Provider, target.AccountRef, target.Model}
		if seen[key] {
			return ErrConflict
		}
		seen[key] = true
	}
	primary := policy.Targets[0]
	if primary != (NativeTarget{policy.Settings.Provider, policy.Settings.AccountRef, policy.Settings.Model, policy.Settings.Thinking}) {
		return ErrScope
	}
	categories := map[string]bool{}
	for _, category := range policy.FallbackOn {
		if !nativeFailureCategory(category) || categories[category] {
			return ErrScope
		}
		categories[category] = true
	}
	return nil
}
func ValidateNativeRetryEvidence(evidence NativeRetryEvidence) error {
	if validateHandle(agentbackend.ExecutionHandle{Conversation: evidence.Submission.Conversation, Generation: evidence.Generation}) != nil || !operationPattern.MatchString(evidence.Submission.OperationID) || evidence.Submission.Action != "send" || !hexPattern.MatchString(evidence.Submission.Digest) || !hexPattern.MatchString(evidence.PolicyDigest) || evidence.Counters.ChainIndex > 7 || evidence.Counters.ExtraAttempts > 7 || evidence.Counters.Hops > 7 || evidence.HTTPStatus < 0 || (evidence.HTTPStatus != 0 && (evidence.HTTPStatus < 100 || evidence.HTTPStatus > 599)) {
		return ErrScope
	}
	for _, value := range []string{evidence.UserEntryID, evidence.StateEntryID, evidence.FailedEntryID, evidence.FailureCode, evidence.FailureEvidenceRef} {
		if len(value) > 4096 || !validString(value) {
			return ErrScope
		}
	}
	if evidence.FailureCategory != "" && !nativeFailureCategory(evidence.FailureCategory) {
		return ErrScope
	}
	if evidence.Actual != (NativeTarget{}) && ValidateNativeTarget(evidence.Actual) != nil {
		return ErrScope
	}
	return nil
}
