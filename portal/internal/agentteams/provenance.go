package agentteams

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

const maxCreationJournalBytes = 64 * 1024

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// CreationJournal is the strict common representation of dev-session's
// creation journal. Schema-2 is recognized only so an old ready root can
// remain accessible; its virtual binding no longer authorizes any operation.
type CreationJournal struct {
	Schema       int
	Slug         string
	State        string
	RunCodex     bool
	GoalSHA256   *string
	Model        *string
	Effort       *string
	TMUXIdentity string
}

// ReadCreationJournal reads the one private per-session creation journal. An
// absent file is not an error; every present file is exact-schema validated.
func ReadCreationJournal(workspace, slug string) (CreationJournal, bool, error) {
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || !session.ValidSlug(slug) {
		return CreationJournal{}, false, errors.New("invalid creation journal authority")
	}
	path := filepath.Join(workspace, "worktrees", ".locks", slug+".creation.json")
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return CreationJournal{}, false, nil
	}
	if err != nil {
		return CreationJournal{}, false, fmt.Errorf("inspect creation journal: %w", err)
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > maxCreationJournalBytes {
		return CreationJournal{}, false, errors.New("creation journal is not a private bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CreationJournal{}, false, fmt.Errorf("read creation journal: %w", err)
	}
	journal, err := DecodeCreationJournal(data, slug)
	if err != nil {
		return CreationJournal{}, false, err
	}
	return journal, true, nil
}

// DecodeCreationJournal rejects duplicate, unknown, cross-schema, and
// cross-state fields. It is shared by lifecycle classification and portal
// receipt recovery so a managed journal cannot gain two interpretations.
func DecodeCreationJournal(data []byte, slug string) (CreationJournal, error) {
	if len(data) == 0 || len(data) > maxCreationJournalBytes {
		return CreationJournal{}, errors.New("creation journal exceeds bounds")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return CreationJournal{}, fmt.Errorf("invalid creation journal: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := decodeStrict(data, &raw); err != nil || raw == nil {
		return CreationJournal{}, errors.New("invalid creation journal object")
	}
	var header struct {
		Schema int    `json:"schema"`
		Slug   string `json:"slug"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.Slug != slug || (header.Schema != 1 && header.Schema != 2 && header.Schema != 3) || (header.State != "creating" && header.State != "ready") {
		return CreationJournal{}, errors.New("creation journal has an invalid schema, slug, or state")
	}
	common := []string{"schema", "slug", "goal_sha256", "run_codex", "state"}
	allowed := append([]string(nil), common...)
	if header.Schema == 2 {
		allowed = append(allowed, "model", "effort", "agent_team_binding", "agent_team_binding_digest")
	} else if header.Schema == 3 {
		allowed = append(allowed, "model", "effort", "direct_team")
	} else {
		for _, field := range []string{"model", "effort", "preserve_tracking", "tracking_origin", "tracking_plan_sha256", "tracking_state_sha256"} {
			if _, ok := raw[field]; ok {
				allowed = append(allowed, field)
			}
		}
	}
	if header.State == "creating" {
		allowed = append(allowed, "tmux_identity")
	}
	if len(raw) != len(allowed) {
		return CreationJournal{}, errors.New("creation journal has missing or unknown fields")
	}
	for _, field := range allowed {
		if _, ok := raw[field]; !ok {
			return CreationJournal{}, errors.New("creation journal has missing or unknown fields")
		}
	}
	for _, field := range []string{"model", "effort"} {
		if value, ok := raw[field]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return CreationJournal{}, errors.New("creation journal has invalid model settings")
		}
	}
	runCodex := bytes.TrimSpace(raw["run_codex"])
	if !bytes.Equal(runCodex, []byte("true")) && !bytes.Equal(runCodex, []byte("false")) {
		return CreationJournal{}, errors.New("creation journal has an invalid run_codex value")
	}
	var wire struct {
		Schema         int             `json:"schema"`
		Slug           string          `json:"slug"`
		GoalSHA256     *string         `json:"goal_sha256"`
		RunCodex       bool            `json:"run_codex"`
		State          string          `json:"state"`
		Model          *string         `json:"model"`
		Effort         *string         `json:"effort"`
		Binding        string          `json:"agent_team_binding"`
		BindingDigest  string          `json:"agent_team_binding_digest"`
		DirectTeam     json.RawMessage `json:"direct_team"`
		TMUXIdentity   string          `json:"tmux_identity"`
		Preserve       bool            `json:"preserve_tracking"`
		TrackingOrigin string          `json:"tracking_origin"`
		TrackingPlan   string          `json:"tracking_plan_sha256"`
		TrackingState  string          `json:"tracking_state_sha256"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return CreationJournal{}, errors.New("creation journal has invalid values")
	}
	if wire.GoalSHA256 != nil && !isDigest(*wire.GoalSHA256) {
		return CreationJournal{}, errors.New("creation journal has an invalid goal digest")
	}
	if (wire.Model != nil && !nonempty(*wire.Model, session.MaxAgentTeamPersistedScalarBytes)) ||
		(wire.Effort != nil && !nonempty(*wire.Effort, session.MaxAgentTeamPersistedScalarBytes)) {
		return CreationJournal{}, errors.New("creation journal has invalid model settings")
	}
	if header.State == "creating" && !isDigest(wire.TMUXIdentity) {
		return CreationJournal{}, errors.New("creation journal has an invalid tmux identity")
	}
	if header.State == "ready" && wire.TMUXIdentity != "" {
		return CreationJournal{}, errors.New("ready creation journal retains a tmux identity")
	}
	if header.Schema == 1 {
		trackingFields := []string{"tracking_origin", "tracking_plan_sha256", "tracking_state_sha256"}
		trackingPresent := 0
		for _, field := range trackingFields {
			if _, ok := raw[field]; ok {
				trackingPresent++
			}
		}
		_, preserveTracking := raw["preserve_tracking"]
		if preserveTracking != (trackingPresent == len(trackingFields)) ||
			(preserveTracking && (!wire.Preserve || !wire.RunCodex || wire.GoalSHA256 == nil ||
				(wire.TrackingOrigin != "revived" && wire.TrackingOrigin != "retained") || !isDigest(wire.TrackingPlan) || !isDigest(wire.TrackingState))) {
			return CreationJournal{}, errors.New("creation journal has invalid preserved tracking")
		}
		return CreationJournal{Schema: wire.Schema, Slug: wire.Slug, State: wire.State, RunCodex: wire.RunCodex, GoalSHA256: wire.GoalSHA256, Model: wire.Model, Effort: wire.Effort, TMUXIdentity: wire.TMUXIdentity}, nil
	}
	if header.Schema == 3 {
		if !wire.RunCodex || wire.GoalSHA256 == nil || wire.Model == nil || wire.Effort == nil ||
			validateDirectCreationTeam(wire.DirectTeam, *wire.Model, *wire.Effort) != nil {
			return CreationJournal{}, errors.New("direct creation journal has an invalid team snapshot")
		}
		return CreationJournal{Schema: wire.Schema, Slug: wire.Slug, State: wire.State, RunCodex: wire.RunCodex, GoalSHA256: wire.GoalSHA256, Model: wire.Model, Effort: wire.Effort, TMUXIdentity: wire.TMUXIdentity}, nil
	}
	if !wire.RunCodex || wire.GoalSHA256 == nil || wire.Model == nil || wire.Effort == nil ||
		len(wire.Binding) < 5 || len(wire.Binding) > 32*1024 || !isDigest(wire.BindingDigest) ||
		!strings.HasPrefix(wire.Binding, "v1.") || !strings.HasSuffix(wire.Binding, "."+wire.BindingDigest) {
		return CreationJournal{}, errors.New("managed creation journal has incomplete values")
	}
	return CreationJournal{Schema: wire.Schema, Slug: wire.Slug, State: wire.State, RunCodex: wire.RunCodex, GoalSHA256: wire.GoalSHA256, Model: wire.Model, Effort: wire.Effort, TMUXIdentity: wire.TMUXIdentity}, nil
}

// Keep this reader independent of teamruntime: registration and lifecycle
// provenance must be able to inspect a direct journal without importing the
// mutable roster service. The snapshot is validated before it can be treated
// as an unmanaged session.
func validateDirectCreationTeam(data json.RawMessage, model, effort string) error {
	var raw map[string]json.RawMessage
	if err := decodeStrict(data, &raw); err != nil || raw == nil || len(raw) != 9 {
		return errors.New("invalid direct team object")
	}
	for _, key := range []string{"id", "name", "description", "roles", "catalogDigest", "teamDigest", "leadModel", "leadEffort", "members"} {
		if _, ok := raw[key]; !ok {
			return errors.New("incomplete direct team object")
		}
	}
	var team struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Description   string   `json:"description"`
		CatalogDigest string   `json:"catalogDigest"`
		TeamDigest    string   `json:"teamDigest"`
		LeadModel     string   `json:"leadModel"`
		LeadEffort    string   `json:"leadEffort"`
		Roles         []string `json:"roles"`
		Members       []struct {
			Role            string `json:"role"`
			Address         string `json:"address"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoningEffort"`
			Behavior        string `json:"behavior"`
			Access          string `json:"access"`
		} `json:"members"`
	}
	var rawMembers []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw["members"]), []byte("null")) ||
		decodeStrict(raw["members"], &rawMembers) != nil {
		return errors.New("invalid direct team members")
	}
	if err := json.Unmarshal(data, &team); err != nil || !identifier(team.ID) || !directCreationScalar(team.Name, 256) ||
		!directCreationScalar(team.Description, 4096) || !isDigest(team.CatalogDigest) || !isDigest(team.TeamDigest) ||
		!directCreationScalar(team.LeadModel, session.MaxAgentTeamPersistedScalarBytes) ||
		!directCreationScalar(team.LeadEffort, session.MaxAgentTeamPersistedScalarBytes) ||
		team.LeadModel != model || team.LeadEffort != effort || len(team.Roles) != len(team.Members)+1 ||
		len(team.Roles) > 64 || team.Roles[0] != "lead" {
		return errors.New("invalid direct team values")
	}
	seen := make(map[string]bool, len(team.Members))
	for index, member := range team.Members {
		var fields map[string]json.RawMessage
		if err := decodeStrict(rawMembers[index], &fields); err != nil || fields == nil || len(fields) != 6 {
			return errors.New("invalid direct team member object")
		}
		for _, key := range []string{"role", "address", "model", "reasoningEffort", "behavior", "access"} {
			if _, ok := fields[key]; !ok {
				return errors.New("incomplete direct team member object")
			}
		}
		if !identifier(member.Role) || member.Role == "lead" || member.Address != member.Role+"0" ||
			!directCreationScalar(member.Model, session.MaxAgentTeamPersistedScalarBytes) ||
			!directCreationScalar(member.ReasoningEffort, session.MaxAgentTeamPersistedScalarBytes) ||
			!validDirectCreationPolicy(member.Role, member.Behavior, member.Access) ||
			team.Roles[index+1] != member.Address || seen[member.Address] {
			return errors.New("invalid direct team member")
		}
		seen[member.Address] = true
	}
	return nil
}

func validDirectCreationPolicy(role, behavior, access string) bool {
	switch role {
	case "architect":
		return behavior == "designer" && access == "read_only"
	case "implementer":
		return behavior == "implementer" && access == "workspace_write"
	case "reviewer":
		return behavior == "reviewer" && access == "read_only"
	default:
		return false
	}
}

func directCreationScalar(value string, limit int) bool {
	return nonempty(value, limit) && !bytes.ContainsAny([]byte(value), "\x00\r\n")
}

// ClassifySession performs no mutation. Malformed, incomplete, or inconsistent
// durable managed provenance is deliberately classified as corrupt so callers
// can refuse lifecycle work without silently treating it as legacy.
