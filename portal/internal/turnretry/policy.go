package turnretry

import (
	"context"
	"errors"
	"time"
)

var ErrPolicyStale = errors.New("automatic retry policy changed")

// Decision is explicit session authority, captured under the caller's runtime
// fence and admission lock. The deployed Enabled bit is inheritance input only.
type Decision struct {
	Revision                                string
	Capacity, Cybersecurity                 bool
	CapacityBaseline, CybersecurityBaseline Enrollment
	Suppressed                              []Suppression
}

// A fixed decision can be used by an already fenced internal caller. Production
// session workers use SessionPolicy so checkpoints remain durable.
func (d Decision) Decide(context.Context, *State) (Decision, error) {
	if d.Revision == "" {
		return Decision{}, ErrPolicyUnavailable
	}
	return d, nil
}
func (d Decision) Checkpoint(context.Context, *State, *Observation, time.Time, bool) error {
	return nil
}

func (d Decision) Enabled(class string) bool {
	return d.Revision != "" && (class == "capacity" && d.Capacity || class == "cybersecurity" && d.Cybersecurity)
}

func (d Decision) Allows(s *State) bool {
	if !d.Enabled(s.Class) || s.Cancelled {
		return false
	}
	for _, v := range d.Suppressed {
		if v.Chain == s.Chain || s.Pending != nil && v.Pending != "" && v.Pending == s.Pending.ID {
			return false
		}
	}
	return true
}

func (d Decision) Eligible(s *State, o Observation) bool {
	class := Class(o.Code)
	if !d.Enabled(class) {
		return false
	}
	baseline := d.CapacityBaseline
	if class == "cybersecurity" {
		baseline = d.CybersecurityBaseline
	}
	if o.Turn == baseline.Turn && !baseline.Active {
		return false
	}
	for _, v := range d.Suppressed {
		if o.Turn == v.Chain || v.Result != "" && o.Turn == v.Result {
			return false
		}
		if s.Pending != nil && s.Pending.ResultTurn == o.Turn && (v.Chain == s.Chain || v.Pending == s.Pending.ID) {
			return false
		}
	}
	return true
}

type PolicyAuthority interface {
	Decide(context.Context, *State) (Decision, error)
	Checkpoint(context.Context, *State, *Observation, time.Time, bool) error
}

// SessionPolicy does no native I/O. Its short RMW is always inside the caller's
// runtime/conversation/admission fences, and cannot grant delivery authority.
type SessionPolicy struct {
	Store PolicyStore
	Scope PolicyScope
}

func (a SessionPolicy) Decide(ctx context.Context, s *State) (Decision, error) {
	if s.Scope.Workspace != a.Scope.Workspace || s.Scope.Slug != a.Scope.Slug || s.Scope.Root != a.Scope.Root || s.Boundary != a.Scope.Incarnation {
		return Decision{}, ErrPolicyUnavailable
	}
	p, err := a.Store.Load(a.Scope)
	if err != nil {
		return Decision{}, err
	}
	entry := p.Thread(s.Scope.Thread)
	if entry == nil {
		// Manual admission already enrolled this exact incarnation before its
		// first native send. Use that watermark, not today's terminal failure.
		if !s.Enrolled {
			return Decision{}, ErrPolicyUnavailable
		}
		p, err = a.Store.Update(ctx, a.Scope, false, func(p *Policy) error {
			if p.Thread(s.Scope.Thread) != nil {
				return nil
			}
			if len(p.Threads) >= MaxPolicyThreads {
				return ErrPolicyUnavailable
			}
			baseline := Enrollment{Turn: s.Baseline, Active: s.BaselineActive}
			p.Threads = append(p.Threads, ThreadPolicy{Thread: s.Scope.Thread, Capacity: baseline, Cybersecurity: baseline, Suppressed: []Suppression{}})
			return nil
		})
		if err != nil {
			return Decision{}, err
		}
		entry = p.Thread(s.Scope.Thread)
	}
	return Decision{Revision: p.Revision, Capacity: p.Capacity, Cybersecurity: p.Cybersecurity, CapacityBaseline: entry.Capacity, CybersecurityBaseline: entry.Cybersecurity, Suppressed: append([]Suppression{}, entry.Suppressed...)}, nil
}

type PolicyThread struct {
	State         State
	Before, After Observation
}

// PreparePolicy runs only after two complete, stable cold observations under
// runtime EX. No ledger is rewritten by bootstrap or preference changes.
func PreparePolicy(p *Policy, threads []PolicyThread, capacity, cybersecurity bool, revision string, bootstrap bool) error {
	if len(threads) == 0 || len(threads) > MaxPolicyThreads {
		return ErrPolicyUnavailable
	}
	seen := map[string]bool{}
	for _, v := range threads {
		if v.After.Turn == "" && v.After.Status != "" || v.After.Turn != "" && !oneOf(v.After.Status, "inProgress", "completed", "failed", "interrupted") {
			return ErrPolicyUnavailable
		}
		if v.Before != v.After || v.State.Scope.Root != p.Scope.Root || v.State.Scope.Slug != p.Scope.Slug || v.State.Scope.Workspace != p.Scope.Workspace || seen[v.State.Scope.Thread] {
			return ErrPolicyUnavailable
		}
		seen[v.State.Scope.Thread] = true
		if v.State.Boundary != "" && v.State.Boundary != p.Scope.Incarnation {
			return ErrPolicyUnavailable
		}
		if bootstrap && !v.State.Enabled {
			capacity, cybersecurity = false, false
		}
	}
	if !bootstrap && p.EqualChoices(capacity, cybersecurity) {
		return nil
	}
	if !bootstrap && p.Revision != revision {
		return ErrPolicyStale
	}
	oldCapacity, oldCyber := p.Capacity, p.Cybersecurity
	for _, v := range threads {
		s := &v.State
		entry := p.Thread(s.Scope.Thread)
		if entry == nil {
			if len(p.Threads) >= MaxPolicyThreads {
				return ErrPolicyUnavailable
			}
			p.Threads = append(p.Threads, ThreadPolicy{Thread: s.Scope.Thread, Suppressed: []Suppression{}})
			entry = p.Thread(s.Scope.Thread)
		}
		baseline := Enrollment{Turn: v.After.Turn, Active: v.After.Status == "inProgress"}
		if bootstrap || oldCapacity != capacity {
			entry.Capacity = baseline
		}
		if bootstrap || oldCyber != cybersecurity {
			entry.Cybersecurity = baseline
		}
		// An exactly owned inherited chain remains eligible only in its enabled
		// class. Unowned failures at bootstrap remain consumed by the baseline.
		if bootstrap && s.Chain != "" && s.Pending != nil && !s.Cancelled && (s.Class == "capacity" && capacity || s.Class == "cybersecurity" && cybersecurity) {
			if s.Class == "capacity" {
				entry.Capacity = Enrollment{Turn: s.Baseline, Active: s.BaselineActive}
			} else {
				entry.Cybersecurity = Enrollment{Turn: s.Baseline, Active: s.BaselineActive}
			}
		}
		if s.Chain != "" && s.Pending != nil && (s.Class == "capacity" && !capacity || s.Class == "cybersecurity" && !cybersecurity) {
			kept := entry.Suppressed[:0]
			for _, old := range entry.Suppressed {
				// The schema-1 admission owner cannot replace an unresolved chain.
				// A different admitted chain therefore makes old identities obsolete.
				if old.Chain == s.Chain || old.Pending == s.Pending.ID {
					kept = append(kept, old)
				}
			}
			entry.Suppressed = kept
			stop := Suppression{Chain: s.Chain, Pending: s.Pending.ID, Result: s.Pending.ResultTurn}
			found := false
			for i := range entry.Suppressed {
				if entry.Suppressed[i].Chain == stop.Chain {
					entry.Suppressed[i], found = stop, true
				}
			}
			if !found {
				// Admission cannot create a new chain while an old unknown remains.
				// Keep any protected identity; bounds refusal beats evidence loss.
				if len(entry.Suppressed) >= 2 {
					return ErrPolicyUnavailable
				}
				entry.Suppressed = append(entry.Suppressed, stop)
			}
		}
	}
	if !bootstrap {
		next, err := NewID()
		if err != nil {
			return err
		}
		p.Revision = next
	}
	p.Capacity, p.Cybersecurity = capacity, cybersecurity
	return nil
}

// ActionToken is a comparison identity, never authentication or dispatch proof.
func ActionToken(scope PolicyScope, thread, chain, revision, operation, pending string) string {
	return hash(scope.Incarnation + "\x00" + thread + "\x00" + chain + "\x00" + revision + "\x00" + operation + "\x00" + pending)
}

func Stop(s *State) bool {
	if s.Cancelled {
		return false
	}
	if s.Pending != nil {
		s.Handled = s.Pending.Failure
		if s.Pending.ResultTurn != "" {
			s.Handled = s.Pending.ResultTurn
		}
	}
	pending := s.Pending
	s.Cancel("cancelled")
	// Keep the owning tuple available for exact receipt/count recovery. It is
	// not permission to execute after cancellation, even if positively absent.
	s.Pending = pending
	return true
}

func RetryNow(s *State, now time.Time) bool {
	if s.Cancelled || s.Phase != "scheduled" || s.Pending == nil || s.DeadlineMS <= max(now.UnixMilli(), s.ClockMS) {
		return false
	}
	s.DeadlineMS = s.Now(now)
	return true
}
