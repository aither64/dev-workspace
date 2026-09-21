// Package teamruntime owns the small, durable roster that connects a
// development session to independent Codex threads.  It deliberately does not
// store message bodies: each member's Codex thread is the message history.
package teamruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"golang.org/x/sys/unix"
)

const schema = 1

var rolePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var messageIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// Roster is the durable identity of an independent team.  Lead is always the
// session's normal root conversation and therefore has no separate member
// record.
type Roster struct {
	Schema        int                 `json:"schema"`
	Workspace     string              `json:"workspace"`
	Slug          string              `json:"slug"`
	RootThreadID  string              `json:"rootThreadId"`
	Revision      uint64              `json:"revision"`
	CreatedAt     time.Time           `json:"createdAt"`
	UpdatedAt     time.Time           `json:"updatedAt"`
	PresetID      string              `json:"presetId,omitempty"`
	CatalogDigest string              `json:"catalogDigest,omitempty"`
	TeamDigest    string              `json:"teamDigest,omitempty"`
	LeadModel     string              `json:"leadModel,omitempty"`
	LeadEffort    string              `json:"leadEffort,omitempty"`
	ForkSource    *ForkSourceSnapshot `json:"forkSource,omitempty"`
	Members       []Member            `json:"members"`
}

// ForkSourceSnapshot is the immutable source roster selection used by every
// retry of a destination fork. The digest also detects later source mutations
// whenever the source roster remains readable.
type ForkSourceSnapshot struct {
	Workspace    string             `json:"workspace"`
	Slug         string             `json:"slug"`
	RootThreadID string             `json:"rootThreadId"`
	Revision     uint64             `json:"revision"`
	Digest       string             `json:"digest"`
	Members      []ForkSourceMember `json:"members"`
}

type ForkSourceMember struct {
	Address             string `json:"address"`
	Role                string `json:"role"`
	Index               uint64 `json:"index"`
	Thread              string `json:"threadId"`
	Model               string `json:"model"`
	Effort              string `json:"reasoningEffort"`
	Behavior            string `json:"behavior"`
	Access              string `json:"access"`
	PolicyCatalogDigest string `json:"policyCatalogDigest,omitempty"`
	State               string `json:"state"`
}

type Member struct {
	Address             string     `json:"address"`
	Role                string     `json:"role"`
	Index               uint64     `json:"index"`
	Thread              string     `json:"threadId"`
	Model               string     `json:"model,omitempty"`
	Effort              string     `json:"reasoningEffort,omitempty"`
	Behavior            string     `json:"behavior,omitempty"`
	Access              string     `json:"access,omitempty"`
	PolicyCatalogDigest string     `json:"policyCatalogDigest,omitempty"`
	ProjectID           string     `json:"projectId,omitempty"`
	CreateAttempted     bool       `json:"createAttempted,omitempty"`
	State               string     `json:"state"`
	AddedAt             time.Time  `json:"addedAt"`
	RemovedAt           *time.Time `json:"removedAt,omitempty"`
}

type Preset struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Description   string       `json:"description"`
	Roles         []string     `json:"roles"`
	CatalogDigest string       `json:"catalogDigest,omitempty"`
	TeamDigest    string       `json:"teamDigest,omitempty"`
	LeadModel     string       `json:"leadModel,omitempty"`
	LeadEffort    string       `json:"leadEffort,omitempty"`
	Members       []MemberSpec `json:"members"`
}

type MemberSpec struct {
	Role     string `json:"role"`
	Address  string `json:"address"`
	Model    string `json:"model"`
	Effort   string `json:"reasoningEffort"`
	Behavior string `json:"behavior"`
	Access   string `json:"access"`
}

// These are the App Server equivalents of nix/agent-teams.nix roleInstructions.
// The pinned catalog selects a behavior; a role/access mismatch fails closed.
var behaviorInstructions = map[string]string{
	"designer":    "Develop and assess the technical design. Do not edit application source.",
	"implementer": "Implement the assigned change and keep unrelated files untouched.",
	"reviewer":    "Independently review the assigned change for correctness, security, and verification gaps. Do not edit application source.",
}

func memberPolicy(behavior, access string) (codex.ThreadPolicy, error) {
	instructions := behaviorInstructions[behavior]
	if instructions == "" {
		return codex.ThreadPolicy{}, fmt.Errorf("unknown team member behavior %q", behavior)
	}
	var sandbox string
	switch access {
	case "read_only":
		sandbox = "read-only"
	case "workspace_write":
		sandbox = "workspace-write"
	default:
		return codex.ThreadPolicy{}, fmt.Errorf("unknown team member access %q", access)
	}
	return codex.ThreadPolicy{DeveloperInstructions: instructions, Sandbox: sandbox}, nil
}

func policyForRole(role, behavior, access string) (codex.ThreadPolicy, error) {
	expected := map[string]struct{ behavior, access string }{
		"architect":   {"designer", "read_only"},
		"implementer": {"implementer", "workspace_write"},
		"reviewer":    {"reviewer", "read_only"},
	}[role]
	if expected.behavior == "" || behavior != expected.behavior || access != expected.access {
		return codex.ThreadPolicy{}, fmt.Errorf("team role %s has unsupported behavior or access", role)
	}
	return memberPolicy(behavior, access)
}

func validateCatalogInstructions(catalog *agentteams.Catalog, teamID, roleName string, role agentteams.Role) error {
	if len(catalog.NativeAgentConfigs.Roles) == 0 {
		// Hand-constructed catalog fixtures have no generated native variants.
		return nil
	}
	policy, err := memberPolicy(role.Behavior, role.Access)
	if err != nil {
		return err
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(policy.DeveloperInstructions)))
	for _, variant := range catalog.NativeAgentConfigs.Roles {
		if variant.Team == teamID && variant.Role == roleName && variant.Effort == role.Effort {
			if variant.Identity.BehaviorDigest != want {
				return fmt.Errorf("catalog instructions for %s/%s differ from this runtime", teamID, roleName)
			}
			return nil
		}
	}
	return fmt.Errorf("catalog has no instructions for %s/%s", teamID, roleName)
}

func catalogRole(catalog *agentteams.Catalog, role string, managed bool) (agentteams.Role, error) {
	if catalog == nil {
		if managed {
			return agentteams.Role{}, errors.New("installed team catalog is required to add a member")
		}
		// Unmanaged installations have no pinned catalog. Keep their supported
		// direct roles explicit, rather than creating an ungoverned thread.
		switch role {
		case "architect":
			return agentteams.Role{Behavior: "designer", Access: "read_only"}, nil
		case "implementer":
			return agentteams.Role{Behavior: "implementer", Access: "workspace_write"}, nil
		case "reviewer":
			return agentteams.Role{Behavior: "reviewer", Access: "read_only"}, nil
		default:
			return agentteams.Role{}, errors.New("team role has no policy; choose architect, implementer, or reviewer")
		}
	}
	if !digestPattern.MatchString(catalog.CatalogDigest) {
		return agentteams.Role{}, errors.New("installed team catalog has no valid digest")
	}
	name := role
	if name == "architect" {
		name = "designer"
	}
	var found *agentteams.Role
	for teamID, team := range catalog.Teams {
		candidate, ok := team.Roles[name]
		if !ok || name == "team_lead" {
			continue
		}
		if found != nil && (found.Behavior != candidate.Behavior || found.Access != candidate.Access) {
			return agentteams.Role{}, fmt.Errorf("team role %s has conflicting catalog policies", role)
		}
		if err := validateCatalogInstructions(catalog, teamID, name, candidate); err != nil {
			return agentteams.Role{}, err
		}
		copy := candidate
		found = &copy
	}
	if found == nil {
		return agentteams.Role{}, fmt.Errorf("team role %s has no pinned catalog policy", role)
	}
	if _, err := policyForRole(role, found.Behavior, found.Access); err != nil {
		return agentteams.Role{}, err
	}
	return *found, nil
}

// PresetsFromCatalog projects the installed site policy into real persistent
// threads. The old managed catalog is an immutable source of settings, not a
// virtual-team dispatcher. Its designer role has the user-facing address
// `architect0` in the direct roster.
func PresetsFromCatalog(catalog agentteams.Catalog) ([]Preset, error) {
	if catalog.CatalogDigest == "" {
		return nil, errors.New("installed team catalog has no digest")
	}
	ids := make([]string, 0, len(catalog.Teams))
	for id := range catalog.Teams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	presets := make([]Preset, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, errors.New("installed team catalog has an empty team ID")
		}
		team := catalog.Teams[id]
		lead, ok := team.Roles["team_lead"]
		if !ok || lead.Model == "" || lead.Effort == "" || team.TeamDigest == "" {
			return nil, fmt.Errorf("team %s has incomplete lead settings", id)
		}
		name := map[string]string{"solo": "Solo", "delegated": "Full team", "lead_designed": "Lead-designed team"}[id]
		if name == "" {
			name = strings.ReplaceAll(id, "_", " ")
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		preset := Preset{ID: id, Name: name, Description: team.Description, Members: []MemberSpec{},
			CatalogDigest: catalog.CatalogDigest, TeamDigest: team.TeamDigest,
			LeadModel: lead.Model, LeadEffort: lead.Effort,
			Roles: []string{"lead"}}
		roleNames := make([]string, 0, len(team.Roles))
		for roleName := range team.Roles {
			if roleName != "team_lead" {
				roleNames = append(roleNames, roleName)
			}
		}
		sort.Strings(roleNames)
		for _, roleName := range roleNames {
			role := team.Roles[roleName]
			addressRole := roleName
			if addressRole == "designer" {
				addressRole = "architect"
			}
			if !rolePattern.MatchString(addressRole) || role.Model == "" || role.Effort == "" {
				return nil, fmt.Errorf("team %s has invalid direct member %s", id, roleName)
			}
			if _, err := policyForRole(addressRole, role.Behavior, role.Access); err != nil {
				return nil, fmt.Errorf("team %s member %s: %w", id, roleName, err)
			}
			if err := validateCatalogInstructions(&catalog, id, roleName, role); err != nil {
				return nil, err
			}
			preset.Members = append(preset.Members, MemberSpec{Role: addressRole,
				Address: addressRole + "0", Model: role.Model, Effort: role.Effort,
				Behavior: role.Behavior, Access: role.Access})
			preset.Roles = append(preset.Roles, addressRole+"0")
		}
		presets = append(presets, preset)
	}
	return presets, nil
}

func FindCatalogPreset(catalog agentteams.Catalog, id string) (Preset, error) {
	presets, err := PresetsFromCatalog(catalog)
	if err != nil {
		return Preset{}, err
	}
	for _, preset := range presets {
		if preset.ID == id {
			return preset, nil
		}
	}
	return Preset{}, errors.New("unknown team preset")
}

func Presets() []Preset {
	return []Preset{
		{ID: "solo", Name: "Solo", Description: "lead", Roles: nil, Members: []MemberSpec{}},
		{ID: "delivery", Name: "Delivery", Description: "lead · implementer0 · reviewer0", Roles: []string{"implementer", "reviewer"}, Members: []MemberSpec{}},
		{ID: "full", Name: "Full team", Description: "lead · architect0 · implementer0 · reviewer0", Roles: []string{"architect", "implementer", "reviewer"}, Members: []MemberSpec{}},
	}
}

func FindPreset(id string) (Preset, bool) {
	for _, preset := range Presets() {
		if preset.ID == id {
			return preset, true
		}
	}
	return Preset{}, false
}

func (roster Roster) Validate(workspace, slug, rootThreadID string) error {
	if roster.Schema != schema || roster.Workspace != workspace || roster.Slug != slug ||
		roster.RootThreadID != rootThreadID || roster.Revision == 0 || !session.ValidSlug(slug) {
		return errors.New("team roster has the wrong session identity")
	}
	if roster.PresetID != "" {
		if !session.ValidSlug(roster.PresetID) || !digestPattern.MatchString(roster.CatalogDigest) ||
			!digestPattern.MatchString(roster.TeamDigest) || roster.LeadModel == "" || roster.LeadEffort == "" {
			return errors.New("team roster has an invalid preset identity")
		}
	} else if roster.CatalogDigest != "" || roster.TeamDigest != "" || roster.LeadModel != "" || roster.LeadEffort != "" {
		return errors.New("team roster has settings without a preset")
	}
	if roster.ForkSource != nil {
		source := roster.ForkSource
		if source.Workspace != workspace || !session.ValidSlug(source.Slug) || source.Slug == slug ||
			source.RootThreadID == "" || source.Revision == 0 || !digestPattern.MatchString(source.Digest) {
			return errors.New("team roster has an invalid fork source snapshot")
		}
	}
	seen := make(map[string]struct{}, len(roster.Members))
	for _, member := range roster.Members {
		if !rolePattern.MatchString(member.Role) || member.Address != fmt.Sprintf("%s%d", member.Role, member.Index) ||
			(member.State != "creating" && member.State != "ready" && member.State != "archived" && member.State != "removed") || member.AddedAt.IsZero() ||
			(member.Thread == "" && member.State != "creating" && member.State != "removed") {
			return errors.New("team roster contains an invalid member")
		}
		if _, found := seen[member.Address]; found {
			return errors.New("team roster contains a duplicate member address")
		}
		seen[member.Address] = struct{}{}
		if member.Behavior != "" || member.Access != "" {
			if _, err := policyForRole(member.Role, member.Behavior, member.Access); err != nil {
				return fmt.Errorf("team roster member %s policy: %w", member.Address, err)
			}
		}
		if member.PolicyCatalogDigest != "" && !digestPattern.MatchString(member.PolicyCatalogDigest) {
			return errors.New("team roster member has an invalid policy catalog digest")
		}
		if member.PolicyCatalogDigest != "" && (member.Behavior == "" || member.Access == "") {
			return errors.New("team roster member has a catalog digest without policy")
		}
		if member.ProjectID != "" && member.ProjectID != memberProjectID(workspace, slug, rootThreadID, member.Address) {
			return errors.New("team roster member has the wrong project identity")
		}
		if member.CreateAttempted && member.ProjectID == "" && roster.ForkSource == nil {
			return errors.New("team roster member has an attempt without a project identity")
		}
	}
	return nil
}

func memberProjectID(workspace, slug, rootThreadID, address string) string {
	data, _ := json.Marshal([]string{workspace, slug, rootThreadID, address})
	digest := sha256.Sum256(data)
	return "dev-workspace-member:" + hex.EncodeToString(digest[:])
}

func snapshotSource(source *Roster) (*ForkSourceSnapshot, error) {
	data, err := json.Marshal(source)
	if err != nil {
		return nil, fmt.Errorf("encode fork source roster: %w", err)
	}
	digest := sha256.Sum256(data)
	snapshot := &ForkSourceSnapshot{Workspace: source.Workspace, Slug: source.Slug,
		RootThreadID: source.RootThreadID, Revision: source.Revision,
		Digest: hex.EncodeToString(digest[:]), Members: make([]ForkSourceMember, 0, len(source.Members))}
	for _, member := range source.Members {
		snapshot.Members = append(snapshot.Members, ForkSourceMember{
			Address: member.Address, Role: member.Role, Index: member.Index,
			Thread: member.Thread, Model: member.Model, Effort: member.Effort,
			Behavior: member.Behavior, Access: member.Access,
			PolicyCatalogDigest: member.PolicyCatalogDigest, State: member.State,
		})
	}
	return snapshot, nil
}

type Store struct {
	directory string
	workspace string
}

func NewStore(stateRoot, workspace string) (*Store, error) {
	if stateRoot == "" || workspace == "" || !filepath.IsAbs(stateRoot) || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, errors.New("team roster requires canonical absolute state and workspace paths")
	}
	return &Store{directory: userstate.WorkspaceDirectory(stateRoot, "codex-teams", workspace), workspace: workspace}, nil
}

func (store *Store) path(slug string) string     { return filepath.Join(store.directory, slug+".json") }
func (store *Store) lockPath(slug string) string { return filepath.Join(store.directory, slug+".lock") }
func (store *Store) operationLockPath(slug string) string {
	return filepath.Join(store.directory, slug+".operation.lock")
}

// withOperationLock spans App Server calls as well as local journal writes.
// It is distinct from the short roster file lock used by Update.
func (store *Store) withOperationLock(ctx context.Context, slug string, run func() error) error {
	if ctx == nil || !session.ValidSlug(slug) {
		return errors.New("invalid team operation")
	}
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(store.directory, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(store.operationLockPath(slug), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			return run()
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (store *Store) Load(slug, rootThreadID string) (*Roster, error) {
	if !session.ValidSlug(slug) {
		return nil, errors.New("invalid session slug")
	}
	file, err := os.Open(store.path(slug))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 256*1024+1))
	if err != nil {
		return nil, fmt.Errorf("read team roster: %w", err)
	}
	if len(data) > 256*1024 {
		return nil, errors.New("team roster exceeds 256 KiB")
	}
	var roster Roster
	if err := json.Unmarshal(data, &roster); err != nil {
		return nil, fmt.Errorf("decode team roster: %w", err)
	}
	if err := roster.Validate(store.workspace, slug, rootThreadID); err != nil {
		return nil, err
	}
	return &roster, nil
}

func (store *Store) Update(ctx context.Context, slug, rootThreadID string, create bool, mutate func(*Roster) error) (*Roster, error) {
	if ctx == nil || !session.ValidSlug(slug) || rootThreadID == "" || mutate == nil {
		return nil, errors.New("invalid team roster update")
	}
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		return nil, fmt.Errorf("create team roster directory: %w", err)
	}
	if err := os.Chmod(store.directory, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(store.lockPath(slug), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return nil, err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roster, err := store.Load(slug, rootThreadID)
	if errors.Is(err, os.ErrNotExist) && create {
		now := time.Now().UTC()
		roster = &Roster{Schema: schema, Workspace: store.workspace, Slug: slug, RootThreadID: rootThreadID, Revision: 1, CreatedAt: now, UpdatedAt: now}
	} else if err != nil {
		return nil, err
	}
	if err := mutate(roster); err != nil {
		return nil, err
	}
	roster.UpdatedAt = time.Now().UTC()
	roster.Revision++
	if err := roster.Validate(store.workspace, slug, rootThreadID); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(roster, "", "  ")
	if err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(store.directory, ".team-*.tmp")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return nil, err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, store.path(slug)); err != nil {
		return nil, err
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return nil, err
	}
	return roster, nil
}

type Client interface {
	StartThreadWithSettings(context.Context, string, map[string]string, codex.ThreadSettings) (string, error)
	ListThreads(context.Context, codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error)
	ResumeThreadWithSettings(context.Context, string, string, map[string]string, codex.ThreadSettings) (string, error)
	ForkThread(context.Context, string, string, map[string]string, codex.ThreadSettings) (string, error)
	ArchiveThread(context.Context, string) error
	RequireThreadIdle(context.Context, string, string) error
	VerifyThread(context.Context, string, string) error
	ActiveTurnID(context.Context, string) (string, error)
	Interrupt(context.Context, string) error
	RequireThreadTurnsIdle(context.Context, string) error
	UnarchiveThread(context.Context, string) (codex.ThreadMetadata, error)
	SetName(context.Context, string, string) error
	SendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
}

type Service struct {
	Store     *Store
	Client    Client
	Workspace string
	Catalog   *agentteams.Catalog
}

func (service Service) Add(ctx context.Context, slug, rootThreadID, cwd string, environment map[string]string, role, model, effort string) (Member, error) {
	if service.Store == nil || service.Client == nil || !rolePattern.MatchString(role) || role == "lead" {
		return Member{}, errors.New("invalid team member request")
	}
	var pending Member
	err := service.Store.withOperationLock(ctx, slug, func() error {
		_, err := service.Store.Update(ctx, slug, rootThreadID, true, func(roster *Roster) error {
			policyRole, err := catalogRole(service.Catalog, role, roster.PresetID != "")
			if err != nil {
				return err
			}
			var next uint64
			for _, member := range roster.Members {
				if member.Role == role && member.Index >= next {
					next = member.Index + 1
				}
			}
			var catalogDigest string
			if service.Catalog != nil {
				catalogDigest = service.Catalog.CatalogDigest
			}
			pending = Member{Address: fmt.Sprintf("%s%d", role, next), Role: role, Index: next,
				Model: model, Effort: effort, Behavior: policyRole.Behavior, Access: policyRole.Access,
				PolicyCatalogDigest: catalogDigest,
				State:               "creating", AddedAt: time.Now().UTC()}
			pending.ProjectID = memberProjectID(service.Store.workspace, slug, rootThreadID, pending.Address)
			roster.Members = append(roster.Members, pending)
			return nil
		})
		return err
	})
	if err != nil {
		return Member{}, err
	}
	return service.RetryCreating(ctx, slug, rootThreadID, cwd, environment, pending.Address)
}

func (service Service) ApplyPreset(ctx context.Context, slug, rootThreadID, cwd string, environment map[string]string, preset string, model, effort string) ([]Member, error) {
	selected, ok := FindPreset(preset)
	if !ok {
		return nil, errors.New("unknown team preset")
	}
	roster, err := service.Store.Load(slug, rootThreadID)
	if err == nil && len(roster.Members) > 0 {
		return nil, errors.New("a preset can only create an empty team")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(selected.Roles) == 0 {
		return nil, nil
	}
	if roster == nil {
		if _, err := service.Store.Update(ctx, slug, rootThreadID, true, func(*Roster) error { return nil }); err != nil {
			return nil, err
		}
	}
	created := make([]Member, 0, len(selected.Roles))
	for _, role := range selected.Roles {
		member, err := service.Add(ctx, slug, rootThreadID, cwd, environment, role, model, effort)
		if err != nil {
			return created, err
		}
		created = append(created, member)
	}
	return created, nil
}

// ApplyPresetSpec reserves every member before starting any thread. A retry of
// the same immutable selection resumes only its unfinished members; it never
// silently appends a changed preset to a team that the user has edited.
func (service Service) ApplyPresetSpec(ctx context.Context, slug, rootThreadID, cwd string, environment map[string]string, preset Preset) (*Roster, error) {
	if service.Store == nil || service.Client == nil || preset.ID == "" || preset.CatalogDigest == "" ||
		preset.TeamDigest == "" || preset.LeadModel == "" || preset.LeadEffort == "" {
		return nil, errors.New("team preset is incomplete")
	}
	for _, member := range preset.Members {
		if !rolePattern.MatchString(member.Role) || member.Role == "lead" ||
			member.Address != member.Role+"0" || member.Model == "" || member.Effort == "" {
			return nil, errors.New("team preset contains an invalid member")
		}
		if _, err := policyForRole(member.Role, member.Behavior, member.Access); err != nil {
			return nil, fmt.Errorf("team preset member %s: %w", member.Address, err)
		}
	}
	err := service.Store.withOperationLock(ctx, slug, func() error {
		_, err := service.Store.Update(ctx, slug, rootThreadID, true, func(roster *Roster) error {
			if roster.PresetID == "" {
				if len(roster.Members) != 0 {
					return errors.New("a preset can only create an empty team")
				}
				roster.PresetID, roster.CatalogDigest, roster.TeamDigest = preset.ID, preset.CatalogDigest, preset.TeamDigest
				roster.LeadModel, roster.LeadEffort = preset.LeadModel, preset.LeadEffort
				now := time.Now().UTC()
				for _, member := range preset.Members {
					roster.Members = append(roster.Members, Member{Address: member.Address, Role: member.Role,
						Index: 0, Model: member.Model, Effort: member.Effort,
						Behavior: member.Behavior, Access: member.Access, PolicyCatalogDigest: preset.CatalogDigest,
						ProjectID: memberProjectID(service.Store.workspace, slug, rootThreadID, member.Address),
						State:     "creating", AddedAt: now})
				}
				return nil
			}
			if roster.PresetID != preset.ID || roster.CatalogDigest != preset.CatalogDigest ||
				roster.TeamDigest != preset.TeamDigest || roster.LeadModel != preset.LeadModel ||
				roster.LeadEffort != preset.LeadEffort || len(roster.Members) != len(preset.Members) {
				return errors.New("team preset differs from the retained roster")
			}
			for index, spec := range preset.Members {
				member := roster.Members[index]
				if member.Address != spec.Address || member.Role != spec.Role || member.Model != spec.Model ||
					member.Effort != spec.Effort || member.Behavior != spec.Behavior || member.Access != spec.Access ||
					(member.State != "creating" && member.State != "ready") {
					return errors.New("team preset differs from the retained roster")
				}
			}
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, member := range preset.Members {
		if _, err := service.RetryCreating(ctx, slug, rootThreadID, cwd, environment, member.Address); err != nil {
			return nil, err
		}
	}
	return service.Store.Load(slug, rootThreadID)
}

// findStartedMember is deliberately read-only. A zero-result list may race a
// start whose response was lost, so it never authorizes a second start.
func (service Service) findStartedMember(ctx context.Context, projectID, cwd string) (string, error) {
	if projectID == "" || cwd == "" {
		return "", errors.New("member thread recovery requires project identity and cwd")
	}
	archived := false
	var matches []string
	cursor := ""
	seenCursors := map[string]bool{}
	for {
		threads, next, err := service.Client.ListThreads(ctx, codex.ThreadListOptions{
			ProjectID: projectID, Cwd: cwd, Archived: &archived, Limit: 100, Cursor: cursor,
		})
		if err != nil {
			return "", fmt.Errorf("list member threads: %w", err)
		}
		for _, thread := range threads {
			if thread.ID == "" || thread.Cwd != cwd || thread.ProjectID == nil || *thread.ProjectID != projectID || thread.ForkedFromID != "" {
				return "", errors.New("member thread listing contains an unexpected identity")
			}
			matches = append(matches, thread.ID)
		}
		if len(matches) > 1 {
			return "", errors.New("member thread recovery is ambiguous: multiple threads have the project identity")
		}
		if next == nil || *next == "" {
			break
		}
		if seenCursors[*next] {
			return "", errors.New("member thread listing repeated a cursor")
		}
		seenCursors[*next] = true
		cursor = *next
	}
	if len(matches) == 0 {
		return "", errors.New("member thread start outcome is unknown; retry after App Server lists the project identity")
	}
	return matches[0], nil
}

// RetryCreating completes a reserved member without allocating another
// address or repeating an uncertain thread/start request.
func (service Service) RetryCreating(ctx context.Context, slug, rootThreadID, cwd string, environment map[string]string, address string) (Member, error) {
	if service.Store == nil || service.Client == nil {
		return Member{}, errors.New("team runtime is unavailable")
	}
	var result Member
	err := service.Store.withOperationLock(ctx, slug, func() error {
		var err error
		result, err = service.retryCreatingLocked(ctx, slug, rootThreadID, cwd, environment, address)
		return err
	})
	return result, err
}

func (service Service) retryCreatingLocked(ctx context.Context, slug, rootThreadID, cwd string, environment map[string]string, address string) (Member, error) {
	roster, err := service.Store.Load(slug, rootThreadID)
	if err != nil {
		return Member{}, err
	}
	var pending *Member
	for index := range roster.Members {
		if roster.Members[index].Address == address {
			pending = &roster.Members[index]
			break
		}
	}
	if pending == nil {
		return Member{}, errors.New("unknown team member")
	}
	if pending.State == "ready" {
		return *pending, nil
	}
	if pending.State != "creating" {
		return Member{}, errors.New("team member cannot be resumed")
	}
	threadID := pending.Thread
	policy, err := policyForRole(pending.Role, pending.Behavior, pending.Access)
	if err != nil {
		return Member{}, fmt.Errorf("member %s has no retained policy: %w", address, err)
	}
	memberEnvironment := make(map[string]string, len(environment)+1)
	for key, value := range environment {
		memberEnvironment[key] = value
	}
	memberEnvironment["DEV_SESSION_MEMBER_ADDRESS"] = address
	if threadID == "" {
		if pending.ProjectID == "" {
			return Member{}, errors.New("member has no durable project identity; creation outcome is unknown")
		}
		if !pending.CreateAttempted {
			if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
				for index := range updated.Members {
					member := &updated.Members[index]
					if member.Address == address && member.State == "creating" && member.Thread == "" && !member.CreateAttempted && member.ProjectID == pending.ProjectID {
						member.CreateAttempted = true
						return nil
					}
				}
				return errors.New("member start reservation changed")
			}); err != nil {
				return Member{}, err
			}
			threadID, err = service.Client.StartThreadWithSettings(ctx, cwd, memberEnvironment,
				codex.ThreadSettings{Model: pending.Model, ReasoningEffort: pending.Effort, Policy: policy, ProjectID: pending.ProjectID})
			if err != nil {
				threadID, err = service.findStartedMember(ctx, pending.ProjectID, cwd)
				if err != nil {
					return Member{}, fmt.Errorf("start %s thread outcome is uncertain: %w", address, err)
				}
			}
		} else {
			threadID, err = service.findStartedMember(ctx, pending.ProjectID, cwd)
			if err != nil {
				return Member{}, fmt.Errorf("recover %s thread: %w", address, err)
			}
		}
		if threadID == "" {
			return Member{}, errors.New("member start returned an empty thread identity")
		}
		if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			for index := range updated.Members {
				if updated.Members[index].Address == address && updated.Members[index].State == "creating" && updated.Members[index].Thread == "" && updated.Members[index].CreateAttempted && updated.Members[index].ProjectID == pending.ProjectID {
					updated.Members[index].Thread = threadID
					return nil
				}
			}
			return errors.New("team member creation was interrupted")
		}); err != nil {
			return Member{}, err
		}
	} else {
		if _, err := service.Client.ResumeThreadWithSettings(ctx, threadID, cwd, memberEnvironment,
			codex.ThreadSettings{Model: pending.Model, ReasoningEffort: pending.Effort, Policy: policy}); err != nil {
			return Member{}, fmt.Errorf("resume %s thread: %w", address, err)
		}
	}
	if err := service.Client.SetName(ctx, threadID, slug+" "+address); err != nil {
		return Member{}, fmt.Errorf("name %s thread: %w", address, err)
	}
	updated, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
		for index := range roster.Members {
			if roster.Members[index].Address == address && roster.Members[index].State == "creating" && roster.Members[index].Thread == threadID {
				roster.Members[index].State = "ready"
				return nil
			}
		}
		return errors.New("team member creation was interrupted")
	})
	if err != nil {
		return Member{}, err
	}
	for _, member := range updated.Members {
		if member.Address == address {
			return member, nil
		}
	}
	return Member{}, errors.New("team member disappeared")
}

func (service Service) Remove(ctx context.Context, slug, rootThreadID, address string) error {
	if service.Store == nil || service.Client == nil {
		return errors.New("team runtime is unavailable")
	}
	return service.Store.withOperationLock(ctx, slug, func() error {
		return service.removeLocked(ctx, slug, rootThreadID, address)
	})
}

func (service Service) removeLocked(ctx context.Context, slug, rootThreadID, address string) error {
	roster, err := service.Store.Load(slug, rootThreadID)
	if err != nil {
		return err
	}
	for _, member := range roster.Members {
		if member.Address != address {
			continue
		}
		if member.State == "removed" {
			return nil
		}
		if member.State == "creating" && member.Thread == "" && (member.CreateAttempted || member.ProjectID == "") {
			return errors.New("member thread start outcome is unknown; reconcile it before removal")
		}
		// A creation that failed before App Server returned a thread is still a
		// durable reservation. Retiring it preserves the non-reuse guarantee
		// without attempting an invalid App Server operation.
		if member.Thread != "" {
			if err := service.Client.RequireThreadIdle(ctx, member.Thread, filepath.Join(service.Store.workspace, "work", slug)); err != nil {
				return fmt.Errorf("member %s is not idle: %w", address, err)
			}
			if err := service.Client.ArchiveThread(ctx, member.Thread); err != nil {
				return fmt.Errorf("archive %s: %w", address, err)
			}
		}
		_, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			for index := range updated.Members {
				if updated.Members[index].Address == address {
					now := time.Now().UTC()
					updated.Members[index].State = "removed"
					updated.Members[index].RemovedAt = &now
					return nil
				}
			}
			return errors.New("team member disappeared")
		})
		return err
	}
	return errors.New("unknown team member")
}

// ArchiveAll and ReviveAll mirror the root session lifecycle for every
// independent member. They are deliberately idempotent so a lifecycle journal
// can resume after an interrupted transition.
func (service Service) RequireIdleAll(ctx context.Context, slug, rootThreadID string) error {
	if service.Store == nil || service.Client == nil {
		return errors.New("team runtime is unavailable")
	}
	return service.Store.withOperationLock(ctx, slug, func() error {
		return service.requireIdleAllLocked(ctx, slug, rootThreadID)
	})
}

func (service Service) requireIdleAllLocked(ctx context.Context, slug, rootThreadID string) error {
	roster, err := service.Store.Load(slug, rootThreadID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cwd := filepath.Join(service.Store.workspace, "work", slug)
	for _, member := range roster.Members {
		if member.State == "archived" || member.State == "removed" {
			continue
		}
		if member.State != "ready" || member.Thread == "" {
			return fmt.Errorf("member %s has no confirmed idle thread", member.Address)
		}
		archived, err := service.archivedMemberThread(ctx, member.Thread, cwd)
		if err != nil {
			return fmt.Errorf("inspect archived member %s: %w", member.Address, err)
		}
		if archived {
			continue
		}
		if err := service.Client.RequireThreadIdle(ctx, member.Thread, cwd); err != nil {
			return fmt.Errorf("member %s is not idle: %w", member.Address, err)
		}
	}
	return nil
}

func (service Service) ArchiveAll(ctx context.Context, slug, rootThreadID string) error {
	if service.Store == nil || service.Client == nil {
		return errors.New("team runtime is unavailable")
	}
	return service.Store.withOperationLock(ctx, slug, func() error {
		return service.archiveAllLocked(ctx, slug, rootThreadID, false)
	})
}

// RetireAll is reserved for a confirmed forced session deletion. Its bounded
// context covers interruption, the wait for each turn to stop, and archival.
func (service Service) RetireAll(ctx context.Context, slug, rootThreadID string) error {
	if service.Store == nil || service.Client == nil || ctx == nil {
		return errors.New("team runtime is unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	return service.Store.withOperationLock(bounded, slug, func() error {
		return service.archiveAllLocked(bounded, slug, rootThreadID, true)
	})
}

func (service Service) archiveAllLocked(ctx context.Context, slug, rootThreadID string, force bool) error {
	if !force {
		if err := service.requireIdleAllLocked(ctx, slug, rootThreadID); err != nil {
			return err
		}
	}
	roster, err := service.Store.Load(slug, rootThreadID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cwd := filepath.Join(service.Store.workspace, "work", slug)
	archivedMembers := make(map[string]bool, len(roster.Members))
	for _, member := range roster.Members {
		if member.State == "archived" || member.State == "removed" {
			continue
		}
		if member.Thread == "" || (member.State != "ready" && (!force || member.State != "creating")) {
			return fmt.Errorf("member %s has no confirmed thread; finish or resolve creation before archive", member.Address)
		}
		archived, err := service.archivedMemberThread(ctx, member.Thread, cwd)
		if err != nil {
			return fmt.Errorf("inspect archived member %s: %w", member.Address, err)
		}
		archivedMembers[member.Address] = archived
		if force && !archived {
			if err := service.Client.VerifyThread(ctx, member.Thread, cwd); err != nil {
				return fmt.Errorf("verify member %s thread: %w", member.Address, err)
			}
		}
	}
	for _, member := range roster.Members {
		if member.State == "archived" || member.State == "removed" {
			continue
		}
		if force && !archivedMembers[member.Address] {
			activeTurn, err := service.Client.ActiveTurnID(ctx, member.Thread)
			if err != nil {
				return fmt.Errorf("inspect active turn for %s: %w", member.Address, err)
			}
			if activeTurn != "" {
				if err := service.Client.Interrupt(ctx, member.Thread); err != nil {
					return fmt.Errorf("interrupt %s: %w", member.Address, err)
				}
			}
			for {
				if err := service.Client.RequireThreadTurnsIdle(ctx, member.Thread); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("wait for interrupted member %s: %w", member.Address, ctx.Err())
				case <-time.After(25 * time.Millisecond):
				}
			}
		}
		if !archivedMembers[member.Address] {
			if err := service.Client.ArchiveThread(ctx, member.Thread); err != nil {
				archived, lookupErr := service.archivedMemberThread(ctx, member.Thread, cwd)
				if lookupErr != nil {
					return fmt.Errorf("archive %s: %w", member.Address, errors.Join(err, lookupErr))
				}
				if !archived {
					return fmt.Errorf("archive %s: %w", member.Address, err)
				}
			}
		}
		if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			for index := range updated.Members {
				if updated.Members[index].Address == member.Address {
					updated.Members[index].State = "archived"
					return nil
				}
			}
			return errors.New("team member disappeared")
		}); err != nil {
			return err
		}
	}
	return nil
}

// archivedMemberThread finds only the roster's exact archived thread. A
// completed App Server archive can precede the durable roster update, and
// thread/archive cannot safely be repeated against that archived thread.
func (service Service) archivedMemberThread(ctx context.Context, threadID, cwd string) (bool, error) {
	archived := true
	seenCursors := make(map[string]bool)
	var cursor string
	found := false
	for {
		threads, next, err := service.Client.ListThreads(ctx, codex.ThreadListOptions{
			Cwd: cwd, Archived: &archived, Limit: 100, SortDirection: "asc", Cursor: cursor,
		})
		if err != nil {
			return false, err
		}
		for _, thread := range threads {
			if thread.ID != threadID {
				continue
			}
			if thread.Cwd != cwd {
				return false, errors.New("archived member thread has the wrong working directory")
			}
			if found {
				return false, errors.New("archived member thread has an ambiguous identity")
			}
			found = true
		}
		if next == nil {
			return found, nil
		}
		if *next == "" {
			return false, errors.New("archived member listing returned an empty cursor")
		}
		if seenCursors[*next] {
			return false, errors.New("archived member listing repeated a cursor")
		}
		seenCursors[*next] = true
		cursor = *next
	}
}

func (service Service) ReviveAll(ctx context.Context, slug, rootThreadID string) error {
	roster, err := service.Store.Load(slug, rootThreadID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, member := range roster.Members {
		if member.State != "archived" || member.Thread == "" {
			continue
		}
		if _, err := service.Client.UnarchiveThread(ctx, member.Thread); err != nil {
			return fmt.Errorf("revive %s: %w", member.Address, err)
		}
		if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			for index := range updated.Members {
				if updated.Members[index].Address == member.Address {
					updated.Members[index].State = "ready"
					updated.Members[index].RemovedAt = nil
					return nil
				}
			}
			return errors.New("team member disappeared")
		}); err != nil {
			return err
		}
	}
	return nil
}

// Configure changes the settings retained for the member's next assignment.
// App Server applies model and effort when that next idle turn starts.
func (service Service) Configure(ctx context.Context, slug, rootThreadID, address, model, effort string) (*Roster, error) {
	if address == "lead" {
		return nil, errors.New("configure lead through the conversation controls")
	}
	if service.Store == nil {
		return nil, errors.New("team runtime is unavailable")
	}
	var result *Roster
	err := service.Store.withOperationLock(ctx, slug, func() error {
		var err error
		result, err = service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
			for index := range roster.Members {
				if roster.Members[index].Address == address && roster.Members[index].State == "ready" {
					roster.Members[index].Model, roster.Members[index].Effort = model, effort
					return nil
				}
			}
			return errors.New("team member is unavailable")
		})
		return err
	})
	return result, err
}

func (service Service) Assign(ctx context.Context, slug, rootThreadID, from, to, message, model, effort, messageID string) (codex.SendReceipt, error) {
	if service.Store == nil || service.Client == nil {
		return codex.SendReceipt{}, errors.New("team runtime is unavailable")
	}
	if !messageIDPattern.MatchString(messageID) {
		return codex.SendReceipt{}, errors.New("assignment requires a stable lowercase UUID or 32-hex message ID")
	}
	var receipt codex.SendReceipt
	err := service.Store.withOperationLock(ctx, slug, func() error {
		var sendErr error
		receipt, sendErr = service.assignLocked(ctx, slug, rootThreadID, from, to, message, model, effort, messageID)
		return sendErr
	})
	return receipt, err
}

func (service Service) assignLocked(ctx context.Context, slug, rootThreadID, from, to, message, model, effort, messageID string) (codex.SendReceipt, error) {
	roster, err := service.Store.Load(slug, rootThreadID)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	if from != "lead" {
		found := false
		for _, member := range roster.Members {
			if member.Address == from && member.State == "ready" {
				found = true
				break
			}
		}
		if !found {
			return codex.SendReceipt{}, errors.New("sender is not an active member of this team")
		}
	}
	var target *Member
	if to == "lead" {
		target = &Member{Address: "lead", Thread: rootThreadID, State: "ready"}
	} else {
		for index := range roster.Members {
			if roster.Members[index].Address == to {
				target = &roster.Members[index]
				break
			}
		}
	}
	if target == nil || target.State != "ready" {
		return codex.SendReceipt{}, errors.New("team member is unavailable")
	}
	if strings.TrimSpace(message) == "" {
		return codex.SendReceipt{}, errors.New("assignment is empty")
	}
	if model == "" {
		model = target.Model
	}
	if effort == "" {
		effort = target.Effort
	}
	var policy codex.ThreadPolicy
	if to != "lead" {
		policy, err = policyForRole(target.Role, target.Behavior, target.Access)
		if err != nil {
			return codex.SendReceipt{}, fmt.Errorf("member %s has no retained policy: %w", to, err)
		}
	}
	envelope := fmt.Sprintf("Team assignment from %s to %s:\n\n%s\n\nReport the result or a blocking question to lead with `dev-session team assign $DEV_SESSION_SLUG --from %s --to lead --message '...'`.", from, target.Address, strings.TrimSpace(message), target.Address)
	return service.Client.SendWithOptions(ctx, target.Thread, envelope, messageID, "team:"+slug+":"+from+":"+to, codex.TurnOptions{Model: model, ReasoningEffort: effort, ThreadPolicy: policy})
}

func (service Service) Fork(ctx context.Context, source *Roster, slug, rootThreadID, cwd string, environment map[string]string) (*Roster, error) {
	if service.Store == nil || service.Client == nil || source == nil {
		return nil, errors.New("fork requires a source roster and team runtime")
	}
	var result *Roster
	err := service.Store.withOperationLock(ctx, slug, func() error {
		var err error
		result, err = service.forkLocked(ctx, source, slug, rootThreadID, cwd, environment)
		return err
	})
	return result, err
}

func (service Service) forkLocked(ctx context.Context, source *Roster, slug, rootThreadID, cwd string, environment map[string]string) (*Roster, error) {
	if err := source.Validate(service.Store.workspace, source.Slug, source.RootThreadID); err != nil {
		return nil, fmt.Errorf("invalid fork source: %w", err)
	}
	if source.Slug == slug || source.RootThreadID == rootThreadID || cwd == "" {
		return nil, errors.New("fork source and destination must be distinct")
	}
	// A creating source member has not acquired its final thread identity yet.
	// Reject the whole fork before reserving any destination member.
	for _, member := range source.Members {
		if member.State == "creating" {
			return nil, fmt.Errorf("fork source member %s is still creating", member.Address)
		}
	}
	requested, err := snapshotSource(source)
	if err != nil {
		return nil, err
	}
	if live, err := service.Store.Load(source.Slug, source.RootThreadID); err == nil {
		current, err := snapshotSource(live)
		if err != nil {
			return nil, err
		}
		if current.Digest != requested.Digest {
			return nil, errors.New("fork source roster changed since it was selected")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read fork source roster: %w", err)
	}
	destination, err := service.Store.Load(slug, rootThreadID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if destination == nil {
		destination, err = service.Store.Update(ctx, slug, rootThreadID, true, func(roster *Roster) error {
			roster.PresetID, roster.CatalogDigest, roster.TeamDigest = source.PresetID, source.CatalogDigest, source.TeamDigest
			roster.LeadModel, roster.LeadEffort = source.LeadModel, source.LeadEffort
			roster.ForkSource = requested
			for _, old := range source.Members {
				member := old
				member.Thread, member.ProjectID, member.CreateAttempted = "", "", false
				if member.State != "removed" {
					member.State, member.RemovedAt = "creating", nil
				}
				member.AddedAt = time.Now().UTC()
				roster.Members = append(roster.Members, member)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if destination.ForkSource == nil || destination.ForkSource.Digest != requested.Digest ||
		destination.ForkSource.Slug != requested.Slug || destination.ForkSource.RootThreadID != requested.RootThreadID ||
		len(destination.Members) != len(requested.Members) {
		return nil, errors.New("destination team roster differs from its frozen fork source")
	}
	for index, old := range destination.ForkSource.Members {
		member := destination.Members[index]
		if member.Address != old.Address || member.Role != old.Role || member.Index != old.Index ||
			member.Model != old.Model || member.Effort != old.Effort || member.Behavior != old.Behavior ||
			member.Access != old.Access || member.PolicyCatalogDigest != old.PolicyCatalogDigest ||
			(old.State == "removed" && member.State != "removed") ||
			(old.State != "removed" && member.State != "creating" && member.State != "ready") {
			return nil, errors.New("destination team roster differs from its frozen fork source")
		}
		if old.State == "removed" || member.State == "ready" {
			continue
		}
		if old.Thread == "" {
			return nil, fmt.Errorf("fork source member %s has no thread", old.Address)
		}
		threadID := member.Thread
		if threadID == "" {
			// thread/fork cannot carry a deterministic projectId. A cwd and
			// forkedFromId match could belong to an older destination session,
			// so an uncertain result must be resolved outside this retry path.
			if member.CreateAttempted {
				return nil, fmt.Errorf("fork %s outcome is unknown; App Server does not provide a unique fork retry identity", old.Address)
			}
			policy, err := policyForRole(old.Role, old.Behavior, old.Access)
			if err != nil {
				return nil, fmt.Errorf("fork %s without retained policy: %w", old.Address, err)
			}
			if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
				candidate := &roster.Members[index]
				if candidate.Address != old.Address || candidate.State != "creating" || candidate.Thread != "" || candidate.CreateAttempted {
					return errors.New("fork reservation changed")
				}
				candidate.CreateAttempted = true
				return nil
			}); err != nil {
				return nil, err
			}
			memberEnvironment := make(map[string]string, len(environment)+1)
			for key, value := range environment {
				memberEnvironment[key] = value
			}
			memberEnvironment["DEV_SESSION_MEMBER_ADDRESS"] = old.Address
			threadID, err = service.Client.ForkThread(ctx, old.Thread, cwd, memberEnvironment,
				codex.ThreadSettings{Model: old.Model, ReasoningEffort: old.Effort, Policy: policy})
			if err != nil {
				return nil, fmt.Errorf("fork %s outcome is unknown; retry requires manual reconciliation: %w", old.Address, err)
			}
			if threadID == "" {
				return nil, errors.New("fork returned an empty thread identity")
			}
			if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
				candidate := &roster.Members[index]
				if candidate.Address != old.Address || candidate.State != "creating" || candidate.Thread != "" || !candidate.CreateAttempted {
					return errors.New("fork result binding changed")
				}
				candidate.Thread = threadID
				return nil
			}); err != nil {
				return nil, err
			}
		}
		if err := service.Client.SetName(ctx, threadID, slug+" "+old.Address); err != nil {
			return nil, fmt.Errorf("name %s thread: %w", old.Address, err)
		}
		if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
			candidate := &roster.Members[index]
			if candidate.Address != old.Address || candidate.State != "creating" || candidate.Thread != threadID {
				return errors.New("fork completion changed")
			}
			candidate.State = "ready"
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return service.Store.Load(slug, rootThreadID)
}

func SortedMembers(roster *Roster) []Member {
	if roster == nil {
		return nil
	}
	members := append([]Member(nil), roster.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Address < members[j].Address })
	return members
}
