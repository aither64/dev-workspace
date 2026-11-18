package teamruntime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// PiAdditionReservation is immutable provenance for an explicit append. It does
// not modify the original creation selection, nor grant a native input receipt.
type PiAdditionReservation struct {
	RequestID             string                            `json:"requestId"`
	OriginalSolo          bool                              `json:"originalSolo,omitempty"`
	PayloadDigest         string                            `json:"payloadDigest"`
	RootCreationID        string                            `json:"rootCreationId"`
	PriorRosterRevision   uint64                            `json:"priorRosterRevision"`
	PriorBindingRevision  uint64                            `json:"priorBindingRevision"`
	Generation            string                            `json:"generation"`
	PackageRoot           string                            `json:"packageRoot"`
	ProviderCatalogDigest string                            `json:"providerCatalogDigest"`
	Policy                piproviders.Policy                `json:"policy"`
	Selection             session.PiCreationMemberSelection `json:"selection"`
	ToolPolicy            piruntime.ToolPolicyTarget        `json:"toolPolicy"`
}

func (a PiAdditionReservation) Validate(r PiRoster, m PiRosterMember) error {
	s := a.Selection
	if len(a.RequestID) != 36 || !messageIDPattern.MatchString(a.RequestID) || !digestPattern.MatchString(a.PayloadDigest) || !digestPattern.MatchString(a.RootCreationID) || m.CreationID == a.RootCreationID || (a.PriorRosterRevision == 0) != a.OriginalSolo || a.OriginalSolo && r.PresetID != "solo" || a.PriorRosterRevision >= r.Revision || a.PriorBindingRevision == 0 || a.PriorBindingRevision == math.MaxUint64 || !digestPattern.MatchString(a.Generation) || !filepath.IsAbs(a.PackageRoot) || filepath.Clean(a.PackageRoot) != a.PackageRoot || !digestPattern.MatchString(a.ProviderCatalogDigest) || a.Policy.Session != r.Scope().Ref() || s.Address != m.Address || s.Incarnation != m.Incarnation || s.PolicyDigest != s.Settings.PolicyRevision || !validPiSettings(s.Settings.Effective()) || s.Settings.Model != m.Model || s.Settings.Thinking != m.Effort || piruntime.ValidateToolPolicy(a.ToolPolicy) != nil {
		return ErrPiPolicy
	}
	return nil
}

// NextPiAddress includes retired members: an address/index is never recycled.
func NextPiAddress(members []PiRosterMember, role string) (string, uint64, error) {
	if !rolePattern.MatchString(role) || role == "lead" || role == "team_lead" {
		return "", 0, ErrPiPolicy
	}
	var next uint64
	occupied := map[string]bool{}
	for _, m := range members {
		occupied[m.Address] = true
		if m.Role == role && m.Index >= next {
			if m.Index == math.MaxUint64 {
				return "", 0, ErrPiConflict
			}
			next = m.Index + 1
		}
	}
	for occupied[fmt.Sprintf("%s%d", role, next)] {
		if next == math.MaxUint64 {
			return "", 0, ErrPiConflict
		}
		next++
	}
	return fmt.Sprintf("%s%d", role, next), next, nil
}

// AppendPi reserves metadata only. Its caller owns transition SH/runtime EX and
// performs the binding/authority CAS after this short existing roster lock ends.
func (s *Store) AppendPi(ctx context.Context, admission monitoroperation.PiTeamWriteAdmission, generation string, previous *PiRoster, next PiRoster) (PiRoster, error) {
	if next.Validate() != nil || next.Schema != 3 || len(next.Members) == 0 {
		return PiRoster{}, ErrPiPolicy
	}
	member := next.Members[len(next.Members)-1]
	if member.State != "creating" || member.InitialInputID != "" || member.Addition == nil {
		return PiRoster{}, ErrPiPolicy
	}
	if previous == nil {
		if next.Revision != 1 || len(next.Members) != 1 || !member.Addition.OriginalSolo || member.Addition.PriorRosterRevision != 0 {
			return PiRoster{}, ErrPiConflict
		}
	} else {
		if previous.Revision == math.MaxUint64 || next.Revision != previous.Revision+1 || len(next.Members) != len(previous.Members)+1 || !reflect.DeepEqual(next.Members[:len(previous.Members)], previous.Members) {
			return PiRoster{}, ErrPiConflict
		}
		unchanged := next
		unchanged.Schema = previous.Schema
		unchanged.Revision = previous.Revision
		unchanged.UpdatedAt = previous.UpdatedAt
		unchanged.Members = previous.Members
		if !reflect.DeepEqual(unchanged, *previous) {
			return PiRoster{}, ErrPiConflict
		}
	}
	err := monitoroperation.WithPiRecordWrite(ctx, admission, next.Scope(), generation, func() error {
		return s.withOperationLock(ctx, next.Slug, func() error {
			actual, err := s.LoadPi(next.Slug)
			if previous == nil {
				if !errors.Is(err, os.ErrNotExist) {
					return ErrPiConflict
				}
			} else if err != nil {
				return err
			} else if !reflect.DeepEqual(actual, *previous) {
				return ErrPiConflict
			}
			return monitoroperation.WritePiRecord(s.path(next.Slug), next, maxRosterBytes)
		})
	})
	return next, err
}
