package piproviders

// PlanNext is a pure diagnostic planner. Even its candidate is not permission
// to redispatch the terminal NotSubmitted receipt, change its frozen settings,
// mint another operation ID, or send through a private worker hook.
func PlanNext(r Resolved, e AttemptEvidence) (FallbackDecision, error) {
	d := FallbackDecision{Outcome: "held", Reason: "invalid_evidence", ExtraAttempts: e.ExtraAttempts, Hops: e.Hops}
	f := e.Failure
	if !validCategory(f.Category) || !failureStage(f.Stage) || f.HTTPStatus != 0 && (f.HTTPStatus < 100 || f.HTTPStatus > 599) || !optionalText(f.Code) || !optionalText(f.EvidenceKind) || !optionalText(f.EvidenceRef) || !optionalText(e.NativeRequestID) || !optionalText(e.NativeEntryID) {
		return d, ErrInvalidPolicy
	}
	c := e.Submission.Conversation
	if e.PolicyDigest != r.PolicyDigest || c.Session != r.Session || c.Backend != "pi" || c.Member != r.Member.Address || c.Incarnation != r.Member.Incarnation || c.NativeID == "" || !textOK(e.Submission.OperationID) || !textOK(e.Submission.Action) || !digestPattern.MatchString(e.Submission.Digest) || len(r.Chain) == 0 || len(r.Chain) > 8 || e.ChainIndex >= uint32(len(r.Chain)) || e.Ordinal == 0 || e.Ordinal != e.ExtraAttempts+1 || e.Hops != e.ChainIndex || e.Hops > e.ExtraAttempts {
		return d, ErrStalePolicy
	}
	if r.Budget.MaxExtraAttempts > 7 || r.Budget.MaxHops > 7 || r.Budget.MaxHops > r.Budget.MaxExtraAttempts || e.ExtraAttempts > r.Budget.MaxExtraAttempts || e.Hops > r.Budget.MaxHops {
		return d, ErrInvalidPolicy
	}
	seen := map[[3]string]bool{}
	for _, t := range r.Chain {
		k := [3]string{t.ProviderID, t.AccountRef, t.ModelID}
		if seen[k] {
			return d, ErrInvalidPolicy
		}
		seen[k] = true
	}
	if e.Actual != nil && *e.Actual != r.Chain[e.ChainIndex] {
		return d, ErrStalePolicy
	}
	switch e.SubmissionOutcome {
	case "Accepted":
		d.Outcome = "stop"
		d.Reason = "accepted_no_redispatch"
		return d, nil
	case "Unknown":
		d.Reason = "submission_unknown"
		return d, nil
	case "NotSubmitted":
	default:
		return d, ErrInvalidPolicy
	}
	if e.PartialOutput {
		d.Reason = "partial_output"
		return d, nil
	}
	if e.NativeEntryID != "" {
		d.Reason = "native_entry_present"
		return d, nil
	}
	if e.Effects != "none" {
		switch e.Effects {
		case "possible", "observed", "":
			d.Reason = "effects_unresolved"
			return d, nil
		default:
			return d, ErrInvalidPolicy
		}
	}
	if f.Category == TextRefusal || f.Category == Cancelled {
		d.Outcome = "stop"
		d.Reason = "terminal_response"
		return d, nil
	}
	if f.Category == RefreshStorage || f.Category == Authentication || f.Category == StructuredPolicy || f.Category == Unknown {
		d.Reason = "category_held"
		return d, nil
	}
	if e.Actual == nil || f.EvidenceKind != "p3_not_submitted" || !textOK(f.EvidenceRef) || f.ObservedAt.IsZero() {
		d.Reason = "missing_positive_proof"
		return d, nil
	}
	// Exact adapter evidence is narrower than native text retry heuristics. HTTP
	// 429 alone cannot distinguish a transient limit from exhausted quota.
	known := false
	switch f.Category {
	case RateLimit:
		known = f.Stage == "provider_response" && f.Code == "rate_limit_exceeded" && f.HTTPStatus == 429
	case Overload:
		known = f.Stage == "provider_response" && f.Code == "overloaded" && f.HTTPStatus == 503
	case Transport:
		known = f.Stage == "transport" && f.HTTPStatus == 0 && (f.Code == "connection_refused" || f.Code == "dns_unavailable")
	case ModelUnavailable:
		known = f.Stage == "selection" && f.Code == "model_unavailable" && f.HTTPStatus == 0
	case Configuration:
		known = f.Stage == "selection" && f.Code == "configuration_unavailable" && f.HTTPStatus == 0
	}
	if !known {
		d.Reason = "unclassified_failure"
		return d, nil
	}
	enabled := false
	for _, k := range r.FallbackOn {
		if k == f.Category {
			enabled = true
		}
	}
	if !enabled {
		d.Outcome = "stop"
		d.Reason = "fallback_disabled"
		return d, nil
	}
	if e.ExtraAttempts >= r.Budget.MaxExtraAttempts || e.Hops >= r.Budget.MaxHops || e.ChainIndex+1 >= uint32(len(r.Chain)) {
		d.Outcome = "exhausted"
		d.Reason = "finite_budget"
		return d, nil
	}
	next := r.Chain[e.ChainIndex+1]
	d.Outcome = "candidate_execution_unavailable"
	d.Reason = "attempt_contract_unavailable"
	d.Next = &next
	d.ExtraAttempts++
	d.Hops++
	return d, nil
}

func optionalText(s string) bool { return s == "" || textOK(s) }
func failureStage(s string) bool {
	switch s {
	case "selection", "configure", "transport", "provider_response", "tool":
		return true
	}
	return false
}
