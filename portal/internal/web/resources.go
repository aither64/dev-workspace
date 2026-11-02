package web

import (
	"fmt"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/cluster"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// A display card is not proof of resource ownership. Keep certainty with the
// snapshot that produced its page so unavailable placeholders never count.
type countSnapshot struct {
	Value      int    `json:"value"`
	Known      bool   `json:"known"`
	Stale      bool   `json:"stale"`
	ObservedAt string `json:"observedAt,omitempty"`
}

func (c countSnapshot) Display() string {
	if !c.Known {
		return "?"
	}
	if c.Stale {
		return fmt.Sprintf("%d~", c.Value)
	}
	return fmt.Sprint(c.Value)
}
func (c countSnapshot) Hidden() bool { return c.Known && !c.Stale && c.Value == 0 }
func (c countSnapshot) Description(kind string) string {
	if !c.Known {
		return kind + ": count unavailable"
	}
	if c.Stale {
		return fmt.Sprintf("%s: %d, last known count (%s)", kind, c.Value, c.ObservedAt)
	}
	return fmt.Sprintf("%s: %d", kind, c.Value)
}
func clusterCount(statuses []cluster.Status) countSnapshot {
	count := countSnapshot{Known: true, ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, status := range statuses {
		if !status.PresenceKnown {
			count.Known = false
		}
		if status.Found {
			count.Value++
		}
		if status.Notice != "" {
			count.Stale = true
		}
		if !status.ObservedAt.IsZero() && status.ObservedAt.Format(time.RFC3339) < count.ObservedAt {
			count.ObservedAt = status.ObservedAt.Format(time.RFC3339)
		}
	}
	return count
}
func teamCount(roster *teamruntime.Roster, statusError string) countSnapshot {
	count := countSnapshot{Known: statusError == "", Value: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	if roster != nil {
		for _, member := range roster.Members {
			if member.State != "removed" {
				count.Value++
			}
		}
	}
	return count
}
