package turnretry

import (
	"context"
	"time"
)

func integer(v int64) *int64 { return &v }

func (a SessionPolicy) Checkpoint(ctx context.Context, s *State, observation *Observation, now time.Time, fresh bool) error {
	if s.Scope.Workspace != a.Scope.Workspace || s.Scope.Slug != a.Scope.Slug || s.Scope.Root != a.Scope.Root || s.Boundary != a.Scope.Incarnation {
		return ErrPolicyUnavailable
	}
	if s.Chain == "" || s.Class == "" {
		return nil
	}
	_, err := a.Store.Update(ctx, a.Scope, false, func(p *Policy) error {
		e := p.Thread(s.Scope.Thread)
		if e == nil {
			return ErrPolicyUnavailable
		}
		if s.Pending != nil && s.Pending.ResultTurn != "" {
			for i := range e.Suppressed {
				if e.Suppressed[i].Chain == s.Chain && e.Suppressed[i].Pending == s.Pending.ID {
					e.Suppressed[i].Result = s.Pending.ResultTurn
				}
			}
		}
		v := e.Summary
		if v == nil || v.Chain != s.Chain {
			if !fresh && (s.Phase == "idle" || s.Phase == "cancelled") && s.Pending == nil {
				return nil
			}
			// Existing chain adoption has no evidenced original start or total.
			// A newly saved schedule supplies both checkpoints from this point.
			v = &Summary{Chain: s.Chain, Class: s.Class, CountQuality: "unavailable", TimingQuality: "unavailable"}
			if fresh {
				v.ConfirmedStarts, v.CountQuality, v.StartedAtMS, v.TimingQuality = integer(0), "exact", integer(max(now.UnixMilli(), s.ClockMS)), "exact"
			}
			e.Summary = v
		}
		v.Class, v.Reason = s.Class, s.Reason
		if v.PendingID != "" && v.LastConfirmed != v.PendingID && (s.Pending == nil || s.Pending.ID != v.PendingID) && v.CountQuality == "exact" {
			// Ordinary delivery may retire a Pending after a summary-write gap.
			// It must not consult this sidecar to do so. Retain the known lower
			// bound while making the lost execution checkpoint explicit.
			v.CountQuality = "partial"
		}
		if s.Pending != nil {
			v.PendingID = s.Pending.ID
			if s.Pending.ResultTurn != "" && v.LastConfirmed != s.Pending.ID {
				if v.ConfirmedStarts != nil {
					if *v.ConfirmedStarts >= maxJSONInteger {
						return ErrPolicyUnavailable
					}
					v.ConfirmedStarts = integer(*v.ConfirmedStarts + 1)
				} else {
					v.ConfirmedStarts, v.CountQuality = integer(1), "partial"
				}
				v.LastConfirmed = s.Pending.ID
			}
		}
		v.Phase, v.Outcome = s.Phase, ""
		switch s.Phase {
		case "cancelled":
			v.Phase, v.Outcome = "terminal", "stopped"
			if s.Reason == "superseded" || s.Reason == "manual_input" || s.Reason == "activated" || s.Reason == "queue_started" {
				v.Outcome = "superseded"
			}
		case "exhausted":
			v.Phase, v.Outcome = "terminal", "exhausted"
		case "blocked":
			v.Phase, v.Outcome = "terminal", "blocked"
		case "idle":
			v.Phase, v.Outcome = "terminal", "success"
		}
		if observation != nil && s.Pending != nil && s.Pending.ResultTurn != "" {
			o := *observation
			if o.Turn == s.Pending.ResultTurn && o.Status != "inProgress" && o.Status != "failed" {
				v.Phase, v.Outcome = "terminal", "success"
			} else if o.Turn != "" && o.Turn != s.Pending.ResultTurn {
				v.Phase, v.Outcome = "terminal", "superseded"
			}
		}
		if s.Cancelled && v.Outcome == "" {
			v.Phase, v.Outcome = "terminal", "stopped"
		}
		if v.Phase == "terminal" && v.FinishedAtMS == nil {
			v.FinishedAtMS = integer(max(now.UnixMilli(), s.ClockMS))
		}
		return nil
	})
	return err
}
