package teamruntime

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Reporter is a current roster identity resolved from native MCP metadata,
// never from tool arguments. It is re-resolved under the team operation lock
// after the host CLI acquires the session lock, immediately before delivery.
type Reporter struct {
	Slug, RootThreadID string
	Member             Member
}

func (store *Store) ResolveReporter(threadID string) (Reporter, error) {
	if threadID == "" {
		return Reporter{}, errors.New("missing native report caller")
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return Reporter{}, fmt.Errorf("read reporting rosters: %w", err)
	}
	var found *Reporter
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		slug := strings.TrimSuffix(entry.Name(), ".json")
		if !session.ValidSlug(slug) || !entry.Type().IsRegular() {
			return Reporter{}, errors.New("invalid reporting roster entry")
		}
		roster, err := store.load(slug, "", true)
		if err != nil {
			return Reporter{}, fmt.Errorf("validate reporting roster: %w", err)
		}
		for _, member := range roster.Members {
			if member.Thread != threadID {
				continue
			}
			if found != nil {
				return Reporter{}, errors.New("ambiguous native report caller")
			}
			found = &Reporter{Slug: slug, RootThreadID: roster.RootThreadID, Member: member}
		}
	}
	if found == nil {
		return Reporter{}, errors.New("unknown native report caller")
	}
	member := found.Member
	if member.State != "ready" || member.RemovedAt != nil || member.RetireIntent != "" {
		return Reporter{}, errors.New("report caller is no longer ready")
	}
	summary, err := session.Find(store.workspace, found.Slug)
	if err != nil || summary.Archived || summary.Terminal || summary.Codex.ThreadID != found.RootThreadID {
		return Reporter{}, errors.New("report session or lead is no longer active")
	}
	return *found, nil
}
