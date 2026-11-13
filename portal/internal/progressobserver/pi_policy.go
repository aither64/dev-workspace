package progressobserver

import (
	"fmt"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// PiActivity is a current owner-verified observation, not an activity cache or
// a new scheduler. Observer activity is explicitly excluded from development.
type PiActivity struct {
	Handle      agentbackend.ExecutionHandle
	State       string
	ObservedAt  time.Time
	Known, Held bool
	Observer    bool
}

// PiDue retains State.Due's existing timer/pause/pending behavior. The owner
// supplies its freshness bound and complete set of currently relevant handles.
// Missing/stale activity refuses a wakeup rather than treating it as idle/work.
func PiDue(state State, now time.Time, maxAge time.Duration, expected []agentbackend.ExecutionHandle, activity []PiActivity) (bool, error) {
	if maxAge <= 0 || len(expected) != len(activity) {
		return false, fmt.Errorf("Pi observer activity unavailable")
	}
	seen := make(map[agentbackend.ExecutionHandle]bool)
	active := false
	for _, subject := range activity {
		if subject.Handle.Conversation.Backend != agentbackend.Pi || subject.Handle.Generation == "" || seen[subject.Handle] ||
			!subject.Known || subject.Held || subject.ObservedAt.After(now) || now.Sub(subject.ObservedAt) > maxAge {
			return false, fmt.Errorf("Pi observer activity unavailable")
		}
		found := false
		for _, handle := range expected {
			found = found || handle == subject.Handle
		}
		if !found || (subject.State != "idle" && subject.State != "working" && subject.State != "waiting") {
			return false, fmt.Errorf("Pi observer activity unavailable")
		}
		seen[subject.Handle] = true
		active = active || (!subject.Observer && subject.State == "working")
	}
	return state.Due(now, active), nil
}
