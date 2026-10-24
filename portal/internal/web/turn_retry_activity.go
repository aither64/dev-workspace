package web

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

func activityError(code string) error { return &conversation.RetryActivityError{Code: code} }

func (p retryProvider) policy() (turnretry.PolicyScope, turnretry.Policy, error) {
	summary, err := session.Find(p.scope.Workspace, p.scope.Slug)
	if err != nil || summary.Archived || summary.Codex.ThreadID != p.scope.Root {
		return turnretry.PolicyScope{}, turnretry.Policy{}, turnretry.ErrPolicyUnavailable
	}
	scope, err := policyScope(summary)
	if err != nil {
		return scope, turnretry.Policy{}, err
	}
	policy, err := policyStore(p.client).Load(scope)
	return scope, policy, err
}

func (p retryProvider) ReadRetryActivity(_ context.Context, thread string) (conversation.RetryActivityStatus, error) {
	if thread != p.scope.Thread {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActivityUnavailable)
	}
	scope, policy, err := p.policy()
	if err != nil {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActivityUnavailable)
	}
	state, err := p.client.RetryStore().Load(p.scope)
	if errors.Is(err, os.ErrNotExist) {
		return conversation.RetryActivityStatus{ServerNowMS: time.Now().UnixMilli()}, nil
	}
	if err != nil || state.Boundary != scope.Incarnation {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActivityUnavailable)
	}
	return retryActivityView(scope, policy, state, time.Now()), nil
}

func retryActivityView(scope turnretry.PolicyScope, policy turnretry.Policy, state turnretry.State, now time.Time) conversation.RetryActivityStatus {
	result := conversation.RetryActivityStatus{ServerNowMS: now.UnixMilli()}
	entry := policy.Thread(state.Scope.Thread)
	if state.Chain == "" || entry == nil {
		return result
	}
	checkpoint := entry.Summary
	class := state.Class
	if class == "" && checkpoint != nil && checkpoint.Chain == state.Chain {
		class = checkpoint.Class
	}
	if class == "" {
		return result
	}
	if checkpoint == nil || checkpoint.Chain != state.Chain {
		if state.Phase == "idle" || state.Phase == "cancelled" {
			return result
		}
		checkpoint = &turnretry.Summary{Chain: state.Chain, Class: class, Phase: state.Phase, CountQuality: "unavailable", TimingQuality: "unavailable"}
	}
	v := &conversation.RetryActivity{ChainKey: turnretry.ActionToken(scope, state.Scope.Thread, state.Chain, "", "chain", ""), Class: class, Phase: checkpoint.Phase, ConfirmedStarts: checkpoint.ConfirmedStarts, CountQuality: checkpoint.CountQuality, StartedAtMS: checkpoint.StartedAtMS, TimingQuality: checkpoint.TimingQuality, FinishedAtMS: checkpoint.FinishedAtMS, Outcome: checkpoint.Outcome, Reason: retryView(state, now).Reason}
	if checkpoint.CountQuality == "exact" {
		acceptedGap := state.Pending != nil && state.Pending.ResultTurn != "" && checkpoint.LastConfirmed != state.Pending.ID
		retiredGap := checkpoint.PendingID != "" && checkpoint.PendingID != checkpoint.LastConfirmed && (state.Pending == nil || state.Pending.ID != checkpoint.PendingID)
		if acceptedGap || retiredGap {
			// Acceptance and ordinary retirement never depend on optional writes.
			// A readable older checkpoint cannot certify the count across that
			// identity gap. Do not reconstruct starts or repair storage on a read.
			v.ConfirmedStarts, v.CountQuality = nil, "unavailable"
		}
	}
	// Presentation cannot resurrect actions or old active state from a stale
	// optional checkpoint. Current ledger identity is always authoritative.
	switch state.Phase {
	case "scheduled", "submitting", "reconciling", "running":
		v.Phase, v.Outcome, v.FinishedAtMS = state.Phase, "", nil
	case "cancelled":
		v.Phase, v.Outcome = "terminal", "stopped"
	case "blocked":
		v.Phase, v.Outcome = "terminal", "blocked"
	case "exhausted":
		v.Phase, v.Outcome = "terminal", "exhausted"
	case "idle":
		v.Phase, v.Outcome = "terminal", "success"
	}
	if state.Cancelled && state.Phase != "blocked" && state.Phase != "exhausted" {
		v.Phase, v.Outcome = "terminal", "stopped"
		switch state.Reason {
		case "manual_input", "superseded", "activated", "queue_started":
			v.Outcome = "superseded"
		}
	}
	decision := turnretry.Decision{Revision: policy.Revision, Capacity: policy.Capacity, Cybersecurity: policy.Cybersecurity, Suppressed: entry.Suppressed}
	if state.Pending != nil && !decision.Allows(&state) && v.Phase != "terminal" {
		v.Phase, v.Outcome = "terminal", "stopped"
	}
	if v.StartedAtMS != nil {
		end := max(now.UnixMilli(), state.ClockMS)
		if v.Phase == "terminal" {
			if v.FinishedAtMS == nil {
				// A polling observation cannot invent the missed terminal time.
				v.StartedAtMS, v.ElapsedMS, v.TimingQuality = nil, nil, "unavailable"
			} else {
				end = *v.FinishedAtMS
			}
		}
		if v.StartedAtMS != nil {
			elapsed := max(int64(0), end-*v.StartedAtMS)
			v.ElapsedMS = &elapsed
		}
	}
	if state.Uncertain() {
		v.Unconfirmed = 1
	}
	if v.Phase != "terminal" {
		v.StopToken = turnretry.ActionToken(scope, state.Scope.Thread, state.Chain, policy.Revision, "stop", "")
		if state.Phase == "scheduled" && state.Pending != nil && !state.Cancelled {
			deadline := state.DeadlineMS
			v.NextAttemptAtMS = &deadline
			v.RetryNowToken = turnretry.ActionToken(scope, state.Scope.Thread, state.Chain, policy.Revision, "now", state.Pending.ID)
		}
	}
	result.Activity = v
	return result
}

func (p retryProvider) RetryNow(ctx context.Context, thread, token string) (conversation.RetryActivityStatus, error) {
	return p.activityAction(ctx, thread, token, false)
}
func (p retryProvider) StopRetry(ctx context.Context, thread, token string) (conversation.RetryActivityStatus, error) {
	return p.activityAction(ctx, thread, token, true)
}

func (p retryProvider) activityAction(ctx context.Context, thread, token string, stop bool) (conversation.RetryActivityStatus, error) {
	if thread != p.scope.Thread {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActionStale)
	}
	identity, policy, err := p.policy()
	if err != nil {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActivityUnavailable)
	}
	tx, err := p.client.RetryStore().Inspect(ctx, p.scope)
	if err != nil {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActivityUnavailable)
	}
	defer tx.Close()
	s := &tx.State
	if s.Chain == "" || s.Boundary != identity.Incarnation || policy.Thread(thread) == nil {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActionStale)
	}
	operation, pending := "stop", ""
	if !stop {
		operation = "now"
		if s.Pending == nil {
			return conversation.RetryActivityStatus{}, activityError(conversation.RetryActionStale)
		}
		pending = s.Pending.ID
	}
	if token != turnretry.ActionToken(identity, thread, s.Chain, policy.Revision, operation, pending) {
		return conversation.RetryActivityStatus{}, activityError(conversation.RetryActionStale)
	}
	if s.Uncertain() {
		controller := turnretry.Controller{Store: p.client.RetryStore(), Backend: p.client.RetryBackend()}
		// Unknown remains protected and cannot stop its own durable cancellation.
		_ = controller.ReconcileLocked(ctx, tx)
	}
	changed := false
	if stop {
		changed = turnretry.Stop(s)
	} else {
		changed = turnretry.RetryNow(s, time.Now())
	}
	if changed {
		if err := tx.Save(); err != nil {
			return conversation.RetryActivityStatus{}, activityError(conversation.RetryActivityUnavailable)
		}
	}
	// Summary errors cannot undo or fail acknowledgment of durable Stop.
	authority := turnretry.SessionPolicy{Store: policyStore(p.client), Scope: identity}
	_ = authority.Checkpoint(ctx, s, nil, time.Now(), false)
	if updated, err := policyStore(p.client).Load(identity); err == nil {
		policy = updated
	}
	return retryActivityView(identity, policy, *s, time.Now()), nil
}
