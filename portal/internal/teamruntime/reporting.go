package teamruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
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
		roster, err := store.reportingRoster(slug)
		if err != nil {
			return Reporter{}, fmt.Errorf("validate reporting roster: %w", err)
		}
		if roster == nil { // Fully validated Pi rosters have no Codex native callers.
			continue
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

// Decode one bounded snapshot with its owning family codec before excluding Pi.
// Arbitrary decode failures must still obstruct discovery rather than disappear.
func (store *Store) reportingRoster(slug string) (*Roster, error) {
	file, err := os.Open(store.path(slug))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRosterBytes+1))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := monitoroperation.DecodeMixedPiRecord(data, &fields, maxRosterBytes); err != nil {
		return nil, err
	}
	record, err := DecodeRosterRecord(data)
	if err != nil {
		return nil, err
	}
	if record.Pi != nil {
		stat, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || record.Pi.Workspace != store.workspace || record.Pi.Slug != slug {
			return nil, ErrPiPolicy
		}
		return nil, nil
	}
	if _, present := fields["backend"]; present {
		return nil, ErrPiPolicy
	}
	if record.Legacy == nil || record.Legacy.RootThreadID == "" {
		return nil, errors.New("team roster has no lead")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(record.Legacy); err != nil {
		return nil, err
	}
	if err := record.Legacy.Validate(store.workspace, slug, record.Legacy.RootThreadID); err != nil {
		return nil, err
	}
	return record.Legacy, nil
}
