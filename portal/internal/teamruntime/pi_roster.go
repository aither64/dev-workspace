package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"golang.org/x/sys/unix"
)

type PiRosterMember struct {
	Address             string     `json:"address"`
	Role                string     `json:"role"`
	Index               uint64     `json:"index"`
	Model               string     `json:"model,omitempty"`
	Effort              string     `json:"reasoningEffort,omitempty"`
	Behavior            string     `json:"behavior,omitempty"`
	Purpose             string     `json:"purpose,omitempty"`
	Instructions        string     `json:"instructions,omitempty"`
	Access              string     `json:"access,omitempty"`
	PolicyCatalogDigest string     `json:"policyCatalogDigest,omitempty"`
	RetireIntent        string     `json:"retireIntent,omitempty"`
	State               string     `json:"state"`
	AddedAt             time.Time  `json:"addedAt"`
	RemovedAt           *time.Time `json:"removedAt,omitempty"`
	Incarnation         string     `json:"incarnation"`
	CreationID          string     `json:"creationId"`
	InitialInputID      string     `json:"initialInputId,omitempty"`
}
type PiRoster struct {
	Schema             int              `json:"schema"`
	Workspace          string           `json:"workspace"`
	Slug               string           `json:"slug"`
	Revision           uint64           `json:"revision"`
	CreatedAt          time.Time        `json:"createdAt"`
	UpdatedAt          time.Time        `json:"updatedAt"`
	PresetID           string           `json:"presetId,omitempty"`
	CatalogDigest      string           `json:"catalogDigest,omitempty"`
	TeamDigest         string           `json:"teamDigest,omitempty"`
	LeadModel          string           `json:"leadModel,omitempty"`
	LeadEffort         string           `json:"leadEffort,omitempty"`
	LeadInstructions   string           `json:"leadInstructions,omitempty"`
	Members            []PiRosterMember `json:"members"`
	Backend            string           `json:"backend"`
	BackendContract    int              `json:"backendContract"`
	SessionIncarnation string           `json:"sessionIncarnation"`
	LeadIncarnation    string           `json:"leadIncarnation"`
}

func (r PiRoster) Scope() monitoroperation.PiSessionRecord {
	return monitoroperation.PiSessionRecord{Workspace: r.Workspace, Slug: r.Slug, Incarnation: r.SessionIncarnation}
}
func (r PiRoster) Validate() error {
	if r.Schema != 2 || r.Backend != "pi" || r.BackendContract != 1 || r.Scope().Validate() != nil || !digestPattern.MatchString(r.LeadIncarnation) || r.Revision == 0 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.Members == nil {
		return ErrPiPolicy
	}
	// Reuse the legacy policy validation without giving it fabricated native IDs.
	if r.PresetID != "" && (!digestPattern.MatchString(r.CatalogDigest) || !digestPattern.MatchString(r.TeamDigest) || r.LeadModel == "" || r.LeadEffort == "" || !validRetainedInstructions(r.LeadInstructions)) {
		return ErrPiPolicy
	}
	seen := map[string]bool{}
	for _, m := range r.Members {
		if !rolePattern.MatchString(m.Role) || m.Address != fmt.Sprintf("%s%d", m.Role, m.Index) || seen[m.Address] || !digestPattern.MatchString(m.Incarnation) || !digestPattern.MatchString(m.CreationID) || m.AddedAt.IsZero() ||
			(m.State != "creating" && m.State != "ready" && m.State != "archived" && m.State != "removed") || m.RetireIntent != "" ||
			(m.InitialInputID != "" && !messageIDPattern.MatchString(m.InitialInputID)) || validateMemberPolicy(m.Role, m.Behavior, m.Purpose, m.Instructions, m.Access) != nil {
			return ErrPiPolicy
		}
		seen[m.Address] = true
		if m.State == "ready" && m.InitialInputID == "" {
			return ErrPiHeld
		}
	}
	return nil
}
func DecodePiRoster(data []byte) (PiRoster, error) {
	var r PiRoster
	err := monitoroperation.DecodePiRecord(data, &r, maxRosterBytes)
	if err == nil {
		err = r.Validate()
	}
	return r, err
}
func (store *Store) LoadPi(slug string) (PiRoster, error) {
	var r PiRoster
	err := monitoroperation.ReadPiRecord(store.path(slug), &r, maxRosterBytes)
	if err == nil {
		err = r.Validate()
	}
	if err == nil && (r.Workspace != store.workspace || r.Slug != slug) {
		err = ErrPiPolicy
	}
	return r, err
}

// SavePi never invokes native creation or materialization. P2's separately
// admitted publication supplies Ready; this codec only preserves its reference.
type PiRosterReadyVerifier interface {
	VerifyPiRosterReady(context.Context, PiRoster) error
}

func (store *Store) SavePi(ctx context.Context, admission monitoroperation.PiTeamWriteAdmission, generation string, expected uint64, next PiRoster, ready ...PiRosterReadyVerifier) error {
	if next.Validate() != nil || next.Workspace != store.workspace {
		return ErrPiPolicy
	}
	return monitoroperation.WithPiRecordWrite(ctx, admission, next.Scope(), generation, func() error {
		for _, member := range next.Members {
			if member.State == "ready" {
				if len(ready) != 1 || ready[0] == nil {
					return ErrPiUnavailable
				}
				// Read-only current P2 proof under outer runtime admission, before
				// either roster lock. It must not reacquire transition/runtime locks.
				if err := ready[0].VerifyPiRosterReady(ctx, next); err != nil {
					return err
				}
				break
			}
		}
		return store.withOperationLock(ctx, next.Slug, func() error {
			lock, err := os.OpenFile(store.lockPath(next.Slug), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return err
			}
			defer lock.Close()
			if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
				return err
			}
			previous, err := store.LoadPi(next.Slug)
			if errors.Is(err, os.ErrNotExist) {
				if expected != 0 || next.Revision != 1 {
					return piruntime.ErrConflict
				}
			} else if err != nil {
				return err
			} else {
				if previous.Revision != expected || next.Revision != expected+1 || previous.Scope() != next.Scope() || previous.LeadIncarnation != next.LeadIncarnation || !previous.CreatedAt.Equal(next.CreatedAt) || len(previous.Members) != len(next.Members) {
					return piruntime.ErrConflict
				}
				for i, old := range previous.Members {
					m := next.Members[i]
					oldState, oldInput := old.State, old.InitialInputID
					old.State, old.InitialInputID = m.State, m.InitialInputID
					if !reflect.DeepEqual(old, m) || (oldInput != "" && oldInput != m.InitialInputID) || (oldState == "ready" && m.State != "ready") {
						return piruntime.ErrConflict
					}
				}
			}
			return monitoroperation.WritePiRecord(store.path(next.Slug), next, maxRosterBytes)
		})
	})
}

// RosterRecord is the existing-path codec union. Legacy bytes are returned
// verbatim until a legacy writer changes them; Pi is never decoded as Codex.
type RosterRecord struct {
	Legacy   *Roster
	Pi       *PiRoster
	original json.RawMessage
}

func DecodeRosterRecord(data []byte) (RosterRecord, error) {
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return RosterRecord{}, err
	}
	if header.Schema == 2 {
		r, err := DecodePiRoster(data)
		return RosterRecord{Pi: &r}, err
	}
	var r Roster
	if header.Schema != 1 {
		return RosterRecord{}, ErrPiPolicy
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return RosterRecord{}, err
	}
	if err := r.Validate(r.Workspace, r.Slug, r.RootThreadID); err != nil {
		return RosterRecord{}, err
	}
	return RosterRecord{Legacy: &r, original: append([]byte(nil), data...)}, nil
}
func (r RosterRecord) MarshalJSON() ([]byte, error) {
	if r.Pi != nil && r.Legacy == nil {
		return json.Marshal(r.Pi)
	}
	if r.Legacy != nil && r.Pi == nil {
		if r.original != nil {
			var original Roster
			if json.Unmarshal(r.original, &original) == nil && reflect.DeepEqual(original, *r.Legacy) {
				return r.original, nil
			}
		}
		return json.Marshal(r.Legacy)
	}
	return nil, ErrPiPolicy
}
