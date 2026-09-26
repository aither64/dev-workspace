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
	"unicode/utf8"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"golang.org/x/sys/unix"
)

const (
	schema         = 1
	maxRosterBytes = 256 * 1024
)

var rolePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var projectUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var messageIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// Roster is the durable identity of an independent team.  Lead is always the
// session's normal root conversation and therefore has no separate member
// record.
type Roster struct {
	Schema           int                 `json:"schema"`
	Workspace        string              `json:"workspace"`
	Slug             string              `json:"slug"`
	RootThreadID     string              `json:"rootThreadId"`
	Revision         uint64              `json:"revision"`
	CreatedAt        time.Time           `json:"createdAt"`
	UpdatedAt        time.Time           `json:"updatedAt"`
	PresetID         string              `json:"presetId,omitempty"`
	CatalogDigest    string              `json:"catalogDigest,omitempty"`
	TeamDigest       string              `json:"teamDigest,omitempty"`
	LeadModel        string              `json:"leadModel,omitempty"`
	LeadEffort       string              `json:"leadEffort,omitempty"`
	LeadInstructions string              `json:"leadInstructions,omitempty"`
	ForkSource       *ForkSourceSnapshot `json:"forkSource,omitempty"`
	Members          []Member            `json:"members"`
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
	Purpose             string `json:"purpose,omitempty"`
	Instructions        string `json:"instructions,omitempty"`
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
	Purpose             string     `json:"purpose,omitempty"`
	Instructions        string     `json:"instructions,omitempty"`
	Access              string     `json:"access,omitempty"`
	PolicyCatalogDigest string     `json:"policyCatalogDigest,omitempty"`
	ProjectID           string     `json:"projectId,omitempty"`
	CreateAttempted     bool       `json:"createAttempted,omitempty"`
	BootstrapAttempted  bool       `json:"bootstrapAttempted,omitempty"`
	RetireIntent        string     `json:"retireIntent,omitempty"`
	State               string     `json:"state"`
	AddedAt             time.Time  `json:"addedAt"`
	RemovedAt           *time.Time `json:"removedAt,omitempty"`
}

type Preset struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	Description      string       `json:"description"`
	Roles            []string     `json:"roles"`
	CatalogDigest    string       `json:"catalogDigest,omitempty"`
	TeamDigest       string       `json:"teamDigest,omitempty"`
	LeadModel        string       `json:"leadModel,omitempty"`
	LeadEffort       string       `json:"leadEffort,omitempty"`
	LeadInstructions string       `json:"leadInstructions,omitempty"`
	Members          []MemberSpec `json:"members"`
}

func (preset Preset) MemberCount() int {
	if preset.CatalogDigest == "" && len(preset.Members) == 0 {
		return len(preset.Roles) + 1
	}
	return len(preset.Members) + 1
}

func (preset Preset) RoleSummary() string {
	counts := map[string]int{"lead": 1}
	if preset.CatalogDigest == "" && len(preset.Members) == 0 {
		for _, role := range preset.Roles {
			counts[role]++
		}
	} else {
		for _, member := range preset.Members {
			counts[member.Role]++
		}
	}
	roles := []string{"lead", "architect", "implementer", "reviewer"}
	extraRoles := make([]string, 0)
	for role := range counts {
		if role != "lead" && role != "architect" && role != "implementer" && role != "reviewer" {
			extraRoles = append(extraRoles, role)
		}
	}
	sort.Strings(extraRoles)
	roles = append(roles, extraRoles...)
	parts := make([]string, 0, len(counts))
	for _, role := range roles {
		count := counts[role]
		if count == 0 {
			continue
		}
		label := role
		if count != 1 {
			label += "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s", count, label))
	}
	return strings.Join(parts, ", ")
}

type MemberSpec struct {
	Role         string `json:"role"`
	Address      string `json:"address"`
	Model        string `json:"model"`
	Effort       string `json:"reasoningEffort"`
	Behavior     string `json:"behavior"`
	Purpose      string `json:"purpose,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Access       string `json:"access"`
}

// Legacy rosters predate retained instructions. Keep their original behavioral
// text so an installed catalog update cannot alter an existing member's job.
var legacyBehaviorInstructions = map[string]string{
	"designer":    "Develop and assess the technical design. Do not edit application source.",
	"implementer": "Implement the assigned change and keep unrelated files untouched.",
	"reviewer":    "Independently review the assigned change for correctness, security, and verification gaps. Do not edit application source.",
}

var purposeForBehavior = map[string]string{
	"designer": "design", "implementer": "implementation", "reviewer": "review", "general": "general",
}

func validateMemberPolicy(role, behavior, purpose, instructions, access string) error {
	if !rolePattern.MatchString(role) || role == "lead" || role == "team_lead" ||
		purposeForBehavior[behavior] == "" || (purpose != "" && purpose != purposeForBehavior[behavior]) ||
		!validRetainedInstructions(instructions) || (access != "read_only" && access != "workspace_write") {
		return fmt.Errorf("team role %s has invalid retained policy", role)
	}
	if purpose == "" || instructions == "" {
		legacy := map[string]string{"architect": "designer", "implementer": "implementer", "reviewer": "reviewer"}
		if purpose != "" || instructions != "" || behavior != legacy[role] || access != map[string]string{
			"architect": "read_only", "implementer": "workspace_write", "reviewer": "read_only",
		}[role] {
			return fmt.Errorf("team role %s has incomplete retained policy", role)
		}
	} else if (purpose == "implementation" && access != "workspace_write") ||
		(purpose == "review" && access != "read_only") {
		return fmt.Errorf("team role %s has incompatible access", role)
	}
	return nil
}

func validRetainedInstructions(instructions string) bool {
	return len(instructions) <= 4096 && utf8.ValidString(instructions) && !strings.ContainsRune(instructions, '\x00')
}

func memberPolicy(workspace, slug string, member Member) (codex.ThreadPolicy, error) {
	if err := validateMemberPolicy(member.Role, member.Behavior, member.Purpose, member.Instructions, member.Access); err != nil {
		return codex.ThreadPolicy{}, err
	}
	instructions := member.Instructions
	if instructions == "" {
		instructions = legacyBehaviorInstructions[member.Behavior]
	}
	var sandbox string
	switch member.Access {
	case "read_only":
		sandbox = "read-only"
	case "workspace_write":
		sandbox = "workspace-write"
	}
	if !session.ValidSlug(slug) || workspace == "" || member.Address == "" {
		return codex.ThreadPolicy{}, errors.New("member policy has incomplete session identity")
	}
	identity := fmt.Sprintf("This Codex conversation is bound to development session %q in workspace %q as team member %q. Before working on session files, run dev-session current and confirm it prints this session slug. DEV_SESSION_SLUG and DEV_SESSION_WORKSPACE must either both be absent or both match this binding. Stop and report any mismatch.\n\n", slug, workspace, member.Address)
	return codex.ThreadPolicy{DeveloperInstructions: identity + instructions, Sandbox: sandbox}, nil
}

func validateCatalogInstructions(catalog *agentteams.Catalog, teamID, roleName string, role agentteams.Role) error {
	if len(catalog.NativeAgentConfigs.Roles) == 0 {
		// Hand-constructed catalog fixtures have no generated native variants.
		return nil
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(role.Instructions)))
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

func catalogRole(catalog *agentteams.Catalog, teamID, role string, managed bool) (agentteams.Role, error) {
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
	if role == "lead" || role == "team_lead" {
		return agentteams.Role{}, errors.New("lead cannot be added as a member")
	}
	find := func(id string) (agentteams.Role, bool, error) {
		team, ok := catalog.Teams[id]
		if !ok {
			return agentteams.Role{}, false, nil
		}
		name := role
		candidate, ok := team.Roles[name]
		if !ok && name == "architect" {
			name = "designer"
			candidate, ok = team.Roles[name]
		}
		if !ok || name == "team_lead" {
			return agentteams.Role{}, false, nil
		}
		if err := validateMemberPolicy(role, candidate.Behavior, candidate.Purpose, candidate.Instructions, candidate.Access); err != nil {
			return agentteams.Role{}, false, err
		}
		if err := validateCatalogInstructions(catalog, id, name, candidate); err != nil {
			return agentteams.Role{}, false, err
		}
		return candidate, true, nil
	}
	// A retained team's own role wins. Otherwise the configured development
	// default supplies additions to older or smaller teams.
	preferred := []string{teamID}
	if catalog.DefaultDevelopmentTeam != nil {
		preferred = append(preferred, *catalog.DefaultDevelopmentTeam)
	}
	preferred = append(preferred, catalog.DefaultTeam)
	seen := map[string]bool{}
	for _, id := range preferred {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		found, ok, err := find(id)
		if err != nil {
			return agentteams.Role{}, err
		}
		if ok {
			return found, nil
		}
	}
	ids := make([]string, 0, len(catalog.Teams))
	for id := range catalog.Teams {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var found *agentteams.Role
	for _, id := range ids {
		candidate, ok, err := find(id)
		if err != nil {
			return agentteams.Role{}, err
		}
		if !ok {
			continue
		}
		if found != nil && (found.Behavior != candidate.Behavior || found.Purpose != candidate.Purpose ||
			found.Instructions != candidate.Instructions || found.Access != candidate.Access) {
			return agentteams.Role{}, fmt.Errorf("team role %s has conflicting catalog policies", role)
		}
		copy := candidate
		found = &copy
	}
	if found == nil {
		return agentteams.Role{}, fmt.Errorf("team role %s has no pinned catalog policy", role)
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
		if !ok || lead.Model == "" || lead.Effort == "" || lead.Purpose != "lead" ||
			lead.Instructions == "" || team.TeamDigest == "" {
			return nil, fmt.Errorf("team %s has incomplete lead settings", id)
		}
		if err := validateCatalogInstructions(&catalog, id, "team_lead", lead); err != nil {
			return nil, err
		}
		name := map[string]string{"solo": "Solo", "delegated": "Full team", "lead_designed": "Lead-designed team"}[id]
		if name == "" {
			name = strings.ReplaceAll(id, "_", " ")
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		preset := Preset{ID: id, Name: name, Description: team.Description, Members: []MemberSpec{},
			CatalogDigest: catalog.CatalogDigest, TeamDigest: team.TeamDigest,
			LeadModel: lead.Model, LeadEffort: lead.Effort, LeadInstructions: lead.Instructions,
			Roles: []string{"lead"}}
		roleNames := make([]string, 0, len(team.Roles))
		for roleName := range team.Roles {
			if roleName != "team_lead" {
				roleNames = append(roleNames, roleName)
			}
		}
		sort.Strings(roleNames)
		addresses := make(map[string]bool, len(roleNames))
		for _, roleName := range roleNames {
			role := team.Roles[roleName]
			addressRole := roleName
			if addressRole == "designer" {
				addressRole = "architect"
			}
			if !rolePattern.MatchString(addressRole) || addressRole == "lead" || addresses[addressRole] ||
				role.Model == "" || role.Effort == "" {
				return nil, fmt.Errorf("team %s has invalid direct member %s", id, roleName)
			}
			addresses[addressRole] = true
			if err := validateMemberPolicy(addressRole, role.Behavior, role.Purpose, role.Instructions, role.Access); err != nil {
				return nil, fmt.Errorf("team %s member %s: %w", id, roleName, err)
			}
			if err := validateCatalogInstructions(&catalog, id, roleName, role); err != nil {
				return nil, err
			}
			preset.Members = append(preset.Members, MemberSpec{Role: addressRole,
				Address: addressRole + "0", Model: role.Model, Effort: role.Effort,
				Behavior: role.Behavior, Purpose: role.Purpose, Instructions: role.Instructions, Access: role.Access})
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
			!digestPattern.MatchString(roster.TeamDigest) || roster.LeadModel == "" || roster.LeadEffort == "" ||
			!validRetainedInstructions(roster.LeadInstructions) {
			return errors.New("team roster has an invalid preset identity")
		}
	} else if roster.CatalogDigest != "" || roster.TeamDigest != "" || roster.LeadModel != "" ||
		roster.LeadEffort != "" || roster.LeadInstructions != "" {
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
	seenThreads := make(map[string]struct{}, len(roster.Members))
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
		if member.Thread != "" {
			if member.Thread == rootThreadID {
				return errors.New("team roster member uses the root thread")
			}
			if _, found := seenThreads[member.Thread]; found {
				return errors.New("team roster contains a duplicate member thread")
			}
			seenThreads[member.Thread] = struct{}{}
		}
		if member.Behavior != "" || member.Access != "" {
			if err := validateMemberPolicy(member.Role, member.Behavior, member.Purpose, member.Instructions, member.Access); err != nil {
				return fmt.Errorf("team roster member %s policy: %w", member.Address, err)
			}
		} else if member.Purpose != "" || member.Instructions != "" {
			return errors.New("team roster member has instructions without a policy")
		}
		if member.PolicyCatalogDigest != "" && !digestPattern.MatchString(member.PolicyCatalogDigest) {
			return errors.New("team roster member has an invalid policy catalog digest")
		}
		if member.PolicyCatalogDigest != "" && (member.Behavior == "" || member.Access == "") {
			return errors.New("team roster member has a catalog digest without policy")
		}
		if member.ProjectID != "" && member.ProjectID != memberProjectKey(workspace, slug, rootThreadID, member.Address) &&
			!projectUUIDPattern.MatchString(member.ProjectID) {
			return errors.New("team roster member has the wrong project identity")
		}
		if member.CreateAttempted && member.ProjectID == "" && roster.ForkSource == nil {
			return errors.New("team roster member has an attempt without a project identity")
		}
		if member.BootstrapAttempted && member.Thread == "" {
			return errors.New("team roster member has a bootstrap attempt without a thread")
		}
		if member.RetireIntent != "" && (member.RetireIntent != "replace" && member.RetireIntent != "remove" ||
			member.Thread == "" || !projectUUIDPattern.MatchString(member.ProjectID) ||
			(member.State != "creating" && member.State != "ready")) {
			return errors.New("team roster member has an invalid retirement reservation")
		}
	}
	return nil
}

// memberProjectKey is also the synthetic projectId used by the first direct-team
// release. Keep it stable so project/create replays and legacy recovery work.
func memberProjectKey(workspace, slug, rootThreadID, address string) string {
	data, _ := json.Marshal([]string{workspace, slug, rootThreadID, address})
	digest := sha256.Sum256(data)
	return "dev-workspace-member:" + hex.EncodeToString(digest[:])
}

func memberBootstrapMarker(slug, address, threadID string) string {
	return fmt.Sprintf("Internal team member initialization for %s in session %s (thread %s). No assignment has been sent yet.", address, slug, threadID)
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
			Behavior: member.Behavior, Purpose: member.Purpose, Instructions: member.Instructions, Access: member.Access,
			PolicyCatalogDigest: member.PolicyCatalogDigest, State: member.State,
		})
	}
	return snapshot, nil
}

type Store struct {
	directory string
	workspace string
	stateRoot string
}

func NewStore(stateRoot, workspace string) (*Store, error) {
	if stateRoot == "" || workspace == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot ||
		!filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, errors.New("team roster requires canonical absolute state and workspace paths")
	}
	return &Store{directory: userstate.WorkspaceDirectory(stateRoot, "codex-teams", workspace), workspace: workspace, stateRoot: stateRoot}, nil
}

func (store *Store) path(slug string) string     { return filepath.Join(store.directory, slug+".json") }
func (store *Store) lockPath(slug string) string { return filepath.Join(store.directory, slug+".lock") }
func (store *Store) operationLockPath(slug string) string {
	return filepath.Join(store.directory, slug+".operation.lock")
}

// withOperationLock spans App Server calls as well as local journal writes.
// It is distinct from the short roster file lock used by Update.
func (store *Store) withOperationLock(ctx context.Context, slug string, run func() error) error {
	release, err := store.LockOperation(ctx, slug)
	if err != nil {
		return err
	}
	defer release()
	return run()
}

// LockOperation serializes a member conversation mutation with team commands.
// The caller retains the lock across the App Server request and must release it.
func (store *Store) LockOperation(ctx context.Context, slug string) (func(), error) {
	if ctx == nil || !session.ValidSlug(slug) {
		return nil, errors.New("invalid team operation")
	}
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(store.directory, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(store.operationLockPath(slug), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			lock.Close()
			return nil, err
		}
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
				_ = lock.Close()
			}, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			lock.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, ctx.Err()
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
	data, err := io.ReadAll(io.LimitReader(file, maxRosterBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read team roster: %w", err)
	}
	if len(data) > maxRosterBytes {
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
	// The trailing newline is part of the on-disk record. Reject it here so
	// a successful update can always be loaded by the next operation.
	if len(data) >= maxRosterBytes {
		return nil, errors.New("team roster exceeds 256 KiB")
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
	CreateProject(context.Context, string, string) (codex.ProjectMetadata, error)
	ReadProject(context.Context, string) (codex.ProjectMetadata, error)
	StartThreadWithSettings(context.Context, string, map[string]string, codex.ThreadSettings) (string, error)
	ListThreads(context.Context, codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error)
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
	ResumeThreadWithSettings(context.Context, string, string, map[string]string, codex.ThreadSettings) (string, error)
	ForkThread(context.Context, string, string, map[string]string, codex.ThreadSettings) (string, error)
	ArchiveThread(context.Context, string) error
	RequireThreadIdle(context.Context, string, string) error
	RequireSubmissionAttemptsResolved(context.Context, string) error
	VerifyThread(context.Context, string, string) error
	ActiveTurnID(context.Context, string) (string, error)
	Interrupt(context.Context, string) error
	RequireThreadTurnsIdle(context.Context, string) error
	UnarchiveThread(context.Context, string) (codex.ThreadMetadata, error)
	SetName(context.Context, string, string) error
	HeadlessThreadMaterialized(context.Context, string, string, string) (bool, error)
	ForkedHeadlessThreadMaterialized(context.Context, string, string, string) (bool, error)
	BootstrapHeadlessThread(context.Context, string, string, string, string) error
	BootstrapForkedHeadlessThread(context.Context, string, string, string, string) error
	VerifyHeadlessBootstrap(context.Context, string, string, string, string) error
	VerifyForkedHeadlessBootstrap(context.Context, string, string, string, string) error
	DeleteFreshHeadlessThread(context.Context, string, string, string) error
	SendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error)
	CompactAcceptedSendOptions(string) error
	ClearRetiredThreadAttempts(string) error
}

type Service struct {
	Store     *Store
	Client    Client
	Workspace string
	Catalog   *agentteams.Catalog
}

// memberTurnPolicy binds the report tool to the exact roster identity on each
// resume before a member turn. A thread ID is unavailable during thread/start
// and thread/fork; neither operation can expose this helper before the roster
// has durably recorded that ID.
func (service Service) memberTurnPolicy(slug, rootThreadID string, member Member) (codex.ThreadPolicy, error) {
	if service.Store == nil || member.Thread == "" || !session.ValidSlug(slug) || rootThreadID == "" || member.Address == "" {
		return codex.ThreadPolicy{}, errors.New("member report tool has incomplete roster identity")
	}
	policy, err := memberPolicy(service.Store.workspace, slug, member)
	if err != nil {
		return codex.ThreadPolicy{}, err
	}
	command, err := os.Executable()
	if err != nil {
		return codex.ThreadPolicy{}, fmt.Errorf("locate member report helper: %w", err)
	}
	command, err = filepath.EvalSymlinks(command)
	if err != nil {
		return codex.ThreadPolicy{}, fmt.Errorf("locate canonical member report helper: %w", err)
	}
	if !filepath.IsAbs(command) || filepath.Clean(command) != command {
		return codex.ThreadPolicy{}, errors.New("member report helper path is not canonical and absolute")
	}
	identity := strings.Join([]string{service.Store.workspace, slug, rootThreadID, member.Address, member.Thread}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	policy.MCPServer = &codex.ThreadMCPServer{
		Name: "team_" + hex.EncodeToString(digest[:16]), Tool: "report_to_lead", Command: command,
		Args: []string{
			"team-mcp", "--user-state-root", service.Store.stateRoot,
			"--workspace", service.Store.workspace, "--session-slug", slug,
			"--root-thread-id", rootThreadID, "--member-address", member.Address,
			"--member-thread-id", member.Thread,
		},
	}
	return policy, nil
}

// MemberTurnPolicy is the same package-owned policy used by team assignments.
// Portal conversations must rebind it before every direct member turn too.
func (service Service) MemberTurnPolicy(slug, rootThreadID string, member Member) (codex.ThreadPolicy, error) {
	return service.memberTurnPolicy(slug, rootThreadID, member)
}

func (service Service) Add(ctx context.Context, slug, rootThreadID, cwd string, environment map[string]string, role, model, effort string) (Member, error) {
	if service.Store == nil || service.Client == nil || !rolePattern.MatchString(role) || role == "lead" {
		return Member{}, errors.New("invalid team member request")
	}
	var pending Member
	err := service.Store.withOperationLock(ctx, slug, func() error {
		_, err := service.Store.Update(ctx, slug, rootThreadID, true, func(roster *Roster) error {
			policyRole, err := catalogRole(service.Catalog, roster.PresetID, role, roster.PresetID != "")
			if err != nil {
				return err
			}
			var next uint64
			occupied := make(map[string]bool, len(roster.Members))
			for _, member := range roster.Members {
				occupied[member.Address] = true
				if member.Role == role && member.Index >= next {
					if member.Index == ^uint64(0) {
						return errors.New("team member index is exhausted")
					}
					next = member.Index + 1
				}
			}
			for occupied[fmt.Sprintf("%s%d", role, next)] {
				if next == ^uint64(0) {
					return errors.New("team member index is exhausted")
				}
				next++
			}
			var catalogDigest string
			if service.Catalog != nil {
				catalogDigest = service.Catalog.CatalogDigest
			}
			pending = Member{Address: fmt.Sprintf("%s%d", role, next), Role: role, Index: next,
				Model: model, Effort: effort, Behavior: policyRole.Behavior, Purpose: policyRole.Purpose,
				Instructions: policyRole.Instructions, Access: policyRole.Access,
				PolicyCatalogDigest: catalogDigest,
				State:               "creating", AddedAt: time.Now().UTC()}
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
	if service.Store == nil || service.Client == nil {
		return nil, errors.New("team runtime is unavailable")
	}
	selected, ok := FindPreset(preset)
	if !ok {
		return nil, errors.New("unknown team preset")
	}
	var expected []Member
	indices := make(map[string]uint64)
	for _, role := range selected.Roles {
		policy, err := catalogRole(service.Catalog, "", role, false)
		if err != nil {
			return nil, err
		}
		index := indices[role]
		indices[role]++
		member := Member{Address: fmt.Sprintf("%s%d", role, index), Role: role, Index: index,
			Model: model, Effort: effort, Behavior: policy.Behavior, Purpose: policy.Purpose,
			Instructions: policy.Instructions, Access: policy.Access}
		if service.Catalog != nil {
			member.PolicyCatalogDigest = service.Catalog.CatalogDigest
		}
		expected = append(expected, member)
	}
	if len(selected.Roles) == 0 {
		// Unmanaged Solo has no persisted preset identity. Serialize its empty
		// roster check, but keep it a no-op when there is no roster to reserve.
		var err error
		err = service.Store.withOperationLock(ctx, slug, func() error {
			roster, loadErr := service.Store.Load(slug, rootThreadID)
			if errors.Is(loadErr, os.ErrNotExist) {
				return nil
			}
			if loadErr != nil {
				return loadErr
			}
			if roster.PresetID != "" || len(roster.Members) != 0 {
				return errors.New("a preset can only create an empty team")
			}
			return nil
		})
		return nil, err
	}
	err := service.Store.withOperationLock(ctx, slug, func() error {
		_, updateErr := service.Store.Update(ctx, slug, rootThreadID, true, func(roster *Roster) error {
			// A partial creating prefix could come from a concurrent manual Add.
			// Only the complete atomic reservation can be retried safely.
			if roster.PresetID != "" || (len(roster.Members) != 0 && len(roster.Members) != len(expected)) {
				return errors.New("a preset can only create an empty team")
			}
			creating := false
			for index, member := range roster.Members {
				want := expected[index]
				if member.Address != want.Address || member.Role != want.Role || member.Index != want.Index ||
					member.Model != want.Model || member.Effort != want.Effort ||
					member.Behavior != want.Behavior || member.Purpose != want.Purpose ||
					member.Instructions != want.Instructions || member.Access != want.Access ||
					member.PolicyCatalogDigest != want.PolicyCatalogDigest ||
					(member.State != "creating" && member.State != "ready") {
					return errors.New("team preset differs from the retained roster")
				}
				creating = creating || member.State == "creating"
			}
			// An all-ready roster could also be a manually built team, so it
			// cannot prove a preset retry.
			if len(roster.Members) > 0 && !creating {
				return errors.New("a preset can only create an empty team")
			}
			now := time.Now().UTC()
			for _, member := range expected[len(roster.Members):] {
				member.State, member.AddedAt = "creating", now
				roster.Members = append(roster.Members, member)
			}
			return nil
		})
		return updateErr
	})
	if err != nil {
		return nil, err
	}
	created := make([]Member, 0, len(expected))
	for _, member := range expected {
		member, err := service.RetryCreating(ctx, slug, rootThreadID, cwd, environment, member.Address)
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
		if err := validateMemberPolicy(member.Role, member.Behavior, member.Purpose, member.Instructions, member.Access); err != nil {
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
				roster.LeadInstructions = preset.LeadInstructions
				now := time.Now().UTC()
				for _, member := range preset.Members {
					roster.Members = append(roster.Members, Member{Address: member.Address, Role: member.Role,
						Index: 0, Model: member.Model, Effort: member.Effort,
						Behavior: member.Behavior, Purpose: member.Purpose, Instructions: member.Instructions,
						Access: member.Access, PolicyCatalogDigest: preset.CatalogDigest,
						State: "creating", AddedAt: now})
				}
				return nil
			}
			if roster.PresetID != preset.ID || roster.CatalogDigest != preset.CatalogDigest ||
				roster.TeamDigest != preset.TeamDigest || roster.LeadModel != preset.LeadModel ||
				roster.LeadEffort != preset.LeadEffort || roster.LeadInstructions != preset.LeadInstructions ||
				len(roster.Members) != len(preset.Members) {
				return errors.New("team preset differs from the retained roster")
			}
			for index, spec := range preset.Members {
				member := roster.Members[index]
				if member.Address != spec.Address || member.Role != spec.Role || member.Model != spec.Model ||
					member.Effort != spec.Effort || member.Behavior != spec.Behavior ||
					member.Purpose != spec.Purpose || member.Instructions != spec.Instructions || member.Access != spec.Access ||
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
	matches, err := service.listMemberThreads(ctx, projectID, cwd)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", errors.New("member thread start outcome is unknown; retry after App Server lists the project identity")
	}
	return matches[0], nil
}

func (service Service) listMemberThreads(ctx context.Context, projectID, cwd string) ([]string, error) {
	if projectID == "" || cwd == "" {
		return nil, errors.New("member thread recovery requires project identity and cwd")
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
			return nil, fmt.Errorf("list member threads: %w", err)
		}
		for _, thread := range threads {
			if thread.ID == "" || thread.Cwd != cwd || thread.ProjectID == nil || *thread.ProjectID != projectID || thread.ForkedFromID != "" {
				return nil, errors.New("member thread listing contains an unexpected identity")
			}
			matches = append(matches, thread.ID)
		}
		if len(matches) > 1 {
			return nil, errors.New("member thread recovery is ambiguous: multiple threads have the project identity")
		}
		if next == nil || *next == "" {
			break
		}
		if seenCursors[*next] {
			return nil, errors.New("member thread listing repeated a cursor")
		}
		seenCursors[*next] = true
		cursor = *next
	}
	return matches, nil
}

// recycleFreshMemberLocked replaces an empty headless thread that App Server
// unloaded before it acquired a rollout. The address and project stay stable;
// only a verified no-history thread may be deleted.
func (service Service) recycleFreshMemberLocked(ctx context.Context, slug, rootThreadID, cwd string, member Member) error {
	if err := service.reserveFreshRetirementLocked(ctx, slug, rootThreadID, &member, "replace"); err != nil {
		return err
	}
	if materialized, err := service.Client.HeadlessThreadMaterialized(ctx, member.Thread, cwd, member.ProjectID); err == nil && materialized {
		marker := memberBootstrapMarker(slug, member.Address, member.Thread)
		if err := service.Client.VerifyHeadlessBootstrap(ctx, member.Thread, cwd, member.ProjectID, marker); err != nil {
			return fmt.Errorf("member rollout appeared during replacement without its exact bootstrap marker: %w", err)
		}
		_, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
			for index := range roster.Members {
				current := &roster.Members[index]
				if current.Address == member.Address && current.Thread == member.Thread && current.RetireIntent == "replace" {
					current.RetireIntent = ""
					return nil
				}
			}
			return errors.New("member bootstrap reconciliation changed")
		})
		return err
	}
	if err := service.deleteUniqueFreshMemberLocked(ctx, slug, cwd, member); err != nil {
		return err
	}
	_, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
		for index := range roster.Members {
			current := &roster.Members[index]
			if current.Address == member.Address && current.Thread == member.Thread &&
				current.ProjectID == member.ProjectID && current.State == member.State && current.RetireIntent == "replace" {
				current.Thread = ""
				current.CreateAttempted = false
				current.BootstrapAttempted = false
				current.RetireIntent = ""
				current.State = "creating"
				return nil
			}
		}
		return errors.New("member recovery reservation changed")
	})
	return err
}

func (service Service) reserveFreshRetirementLocked(ctx context.Context, slug, rootThreadID string, member *Member, intent string) error {
	if member.RetireIntent == intent {
		return nil
	}
	if member.RetireIntent != "" || (intent != "replace" && intent != "remove") {
		return errors.New("member has a conflicting retirement reservation")
	}
	_, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
		for index := range roster.Members {
			current := &roster.Members[index]
			if current.Address == member.Address && current.Thread == member.Thread &&
				current.ProjectID == member.ProjectID && current.State == member.State && current.RetireIntent == "" {
				current.RetireIntent = intent
				return nil
			}
		}
		return errors.New("member retirement reservation changed")
	})
	if err == nil {
		member.RetireIntent = intent
	}
	return err
}

func (service Service) deleteUniqueFreshMemberLocked(ctx context.Context, slug, cwd string, member Member) error {
	if member.Thread == "" || !projectUUIDPattern.MatchString(member.ProjectID) {
		return errors.New("member has no confirmed thread and project to recycle")
	}
	project, err := service.Client.ReadProject(ctx, member.ProjectID)
	if err != nil || project.ID != member.ProjectID || project.Name != slug+" "+member.Address {
		return fmt.Errorf("member project identity changed before recovery: %v", err)
	}
	matches, err := service.listMemberThreads(ctx, member.ProjectID, cwd)
	if err != nil {
		return err
	}
	// Fresh no-turn threads can be absent from thread/list while thread/read
	// still finds them. The exact thread ID, not an empty project listing,
	// determines whether retirement has completed.
	if len(matches) == 1 && matches[0] != member.Thread {
		return errors.New("member thread recovery is not a unique project match")
	}
	deleteErr := service.Client.DeleteFreshHeadlessThread(ctx, member.Thread, cwd, member.ProjectID)
	if deleteErr != nil {
		// A successful thread/delete response is authoritative even when the
		// deleted no-rollout thread later reads as "thread not loaded". An
		// uncertain response needs stronger proof: exact not-found from read.
		_, readErr := service.Client.ReadThreadMetadata(ctx, member.Thread, false)
		if !codex.IsThreadNotFound(readErr, member.Thread) {
			if readErr == nil {
				readErr = errors.New("retired member thread still exists")
			}
			return errors.Join(deleteErr, fmt.Errorf("verify retired member thread: %w", readErr))
		}
	}
	matches, err = service.listMemberThreads(ctx, member.ProjectID, cwd)
	if err != nil {
		return errors.Join(deleteErr, err)
	}
	if len(matches) != 0 {
		return errors.Join(deleteErr, errors.New("retired member thread still appears under its project"))
	}
	// A lost thread/delete response is reconciled only after thread/read
	// reports that the exact old member no longer exists. Project-scoped list
	// remains an ambiguity check, not proof of deletion for a fresh thread.
	return nil
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
	if pending.RetireIntent == "replace" {
		if err := service.recycleFreshMemberLocked(ctx, slug, rootThreadID, cwd, *pending); err != nil {
			return Member{}, fmt.Errorf("finish %s replacement: %w", address, err)
		}
		return service.retryCreatingLocked(ctx, slug, rootThreadID, cwd, environment, address)
	}
	if pending.RetireIntent != "" {
		return Member{}, errors.New("member retirement has a conflicting intent")
	}
	if pending.Thread != "" {
		materialized, err := service.Client.HeadlessThreadMaterialized(ctx, pending.Thread, cwd, pending.ProjectID)
		if err != nil {
			return Member{}, fmt.Errorf("inspect %s bootstrap: %w", address, err)
		}
		if !materialized {
			if err := service.recycleFreshMemberLocked(ctx, slug, rootThreadID, cwd, *pending); err != nil {
				return Member{}, fmt.Errorf("recover %s bootstrap: %w", address, err)
			}
			return service.retryCreatingLocked(ctx, slug, rootThreadID, cwd, environment, address)
		}
	}
	threadID := pending.Thread
	wasStarted := threadID != ""
	policy, err := memberPolicy(service.Store.workspace, slug, *pending)
	if err != nil {
		return Member{}, fmt.Errorf("member %s has no retained policy: %w", address, err)
	}
	memberEnvironment := make(map[string]string, len(environment)+1)
	for key, value := range environment {
		memberEnvironment[key] = value
	}
	memberEnvironment["DEV_SESSION_MEMBER_ADDRESS"] = address
	if threadID == "" {
		projectKey := memberProjectKey(service.Store.workspace, slug, rootThreadID, address)
		projectName := slug + " " + address
		if pending.ProjectID == projectKey {
			// The previous client passed this idempotency key as projectId. In
			// the pinned App Server, thread/start rejects a missing project
			// before allocating a thread. Only that exact missing-project
			// result proves the old attempt could not have started one.
			if _, readErr := service.Client.ReadProject(ctx, projectKey); !codex.IsProjectNotFound(readErr, projectKey) {
				if readErr == nil {
					readErr = errors.New("legacy project exists")
				}
				return Member{}, fmt.Errorf("legacy %s start needs project reconciliation: %w", address,
					readErr)
			}
			if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
				for index := range updated.Members {
					member := &updated.Members[index]
					if member.Address == address && member.State == "creating" && member.Thread == "" &&
						member.ProjectID == projectKey && member.CreateAttempted == pending.CreateAttempted {
						member.ProjectID = ""
						member.CreateAttempted = false
						return nil
					}
				}
				return errors.New("legacy member reservation changed")
			}); err != nil {
				return Member{}, err
			}
			pending.ProjectID = ""
			pending.CreateAttempted = false
		}
		project, err := service.Client.CreateProject(ctx, projectName, projectKey)
		if err != nil {
			return Member{}, fmt.Errorf("register %s project: %w", address, err)
		}
		if !projectUUIDPattern.MatchString(project.ID) || project.Name != projectName ||
			(pending.ProjectID != "" && pending.ProjectID != project.ID) {
			return Member{}, errors.New("member project registration returned a different identity")
		}
		if pending.ProjectID == "" {
			if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
				for index := range updated.Members {
					member := &updated.Members[index]
					if member.Address == address && member.State == "creating" && member.Thread == "" &&
						!member.CreateAttempted && member.ProjectID == "" {
						member.ProjectID = project.ID
						return nil
					}
				}
				return errors.New("member project reservation changed")
			}); err != nil {
				return Member{}, err
			}
			pending.ProjectID = project.ID
		}
		verified, err := service.Client.ReadProject(ctx, pending.ProjectID)
		if err != nil {
			return Member{}, fmt.Errorf("verify %s project: %w", address, err)
		}
		if verified.ID != pending.ProjectID || verified.Name != projectName {
			return Member{}, errors.New("member project lookup returned a different identity")
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
		pending.Thread = threadID
	}
	marker := memberBootstrapMarker(slug, address, threadID)
	if !pending.BootstrapAttempted {
		if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			for index := range updated.Members {
				member := &updated.Members[index]
				if member.Address == address && member.State == "creating" && member.Thread == threadID && !member.BootstrapAttempted {
					member.BootstrapAttempted = true
					return nil
				}
			}
			return errors.New("member bootstrap reservation changed")
		}); err != nil {
			return Member{}, err
		}
		if err := service.Client.BootstrapHeadlessThread(ctx, threadID, cwd, pending.ProjectID, marker); err != nil {
			return Member{}, fmt.Errorf("bootstrap %s thread outcome is uncertain: %w", address, err)
		}
	} else if err := service.Client.VerifyHeadlessBootstrap(ctx, threadID, cwd, pending.ProjectID, marker); err != nil {
		return Member{}, fmt.Errorf("verify %s bootstrap: %w", address, err)
	}
	if wasStarted {
		boundPolicy, err := service.memberTurnPolicy(slug, rootThreadID, *pending)
		if err != nil {
			return Member{}, fmt.Errorf("bind %s report tool: %w", address, err)
		}
		if _, err := service.Client.ResumeThreadWithSettings(ctx, threadID, cwd, memberEnvironment,
			codex.ThreadSettings{Model: pending.Model, ReasoningEffort: pending.Effort, Policy: boundPolicy}); err != nil {
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
			return service.clearPreviouslyRetiredMemberAttempts(ctx, slug, member, false)
		}
		if member.RetireIntent == "replace" {
			return errors.New("member replacement is pending; finish recovery before removal")
		}
		if member.State == "creating" && member.Thread == "" && (member.CreateAttempted || member.ProjectID == "") {
			return errors.New("member thread start outcome is unknown; reconcile it before removal")
		}
		// A creation that failed before App Server returned a thread is still a
		// durable reservation. Retiring it preserves the non-reuse guarantee
		// without attempting an invalid App Server operation.
		if member.Thread != "" {
			cwd := filepath.Join(service.Store.workspace, "work", slug)
			archived, err := service.archivedMemberThread(ctx, member.Thread, cwd)
			if err != nil {
				return fmt.Errorf("inspect archived member %s: %w", address, err)
			}
			if !archived {
				materialized := true
				if member.ProjectID != "" {
					materialized, err = service.Client.HeadlessThreadMaterialized(ctx, member.Thread, cwd, member.ProjectID)
					if err != nil && member.RetireIntent == "" {
						return fmt.Errorf("inspect %s thread: %w", address, err)
					}
					if err != nil {
						materialized = false
					}
				}
				if !materialized || member.RetireIntent == "remove" {
					if materialized {
						if err := service.Client.VerifyHeadlessBootstrap(ctx, member.Thread, cwd, member.ProjectID,
							memberBootstrapMarker(slug, member.Address, member.Thread)); err != nil {
							return fmt.Errorf("member rollout appeared during removal without its exact bootstrap marker: %w", err)
						}
						if err := service.Client.RequireThreadIdle(ctx, member.Thread, cwd); err != nil {
							return fmt.Errorf("member %s is not idle: %w", address, err)
						}
						if err := service.archiveMemberThread(ctx, member.Thread, cwd); err != nil {
							return fmt.Errorf("archive %s: %w", address, err)
						}
					} else {
						if err := service.reserveFreshRetirementLocked(ctx, slug, rootThreadID, &member, "remove"); err != nil {
							return fmt.Errorf("reserve retirement of %s: %w", address, err)
						}
						if err := service.deleteUniqueFreshMemberLocked(ctx, slug, cwd, member); err != nil {
							return fmt.Errorf("retire %s: %w", address, err)
						}
					}
				} else {
					if err := service.Client.RequireThreadIdle(ctx, member.Thread, cwd); err != nil {
						return fmt.Errorf("member %s is not idle: %w", address, err)
					}
					if err := service.archiveMemberThread(ctx, member.Thread, cwd); err != nil {
						return fmt.Errorf("archive %s: %w", address, err)
					}
				}
			}
			// A failed ledger cleanup leaves this member nonterminal. Retrying
			// proves the same archived or deleted thread before clearing again.
			if err := service.clearRetiredMemberAttempts(ctx, member.Thread, false); err != nil {
				return fmt.Errorf("clear retired %s attempts: %w", address, err)
			}
		}
		_, err := service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			for index := range updated.Members {
				if updated.Members[index].Address == address {
					now := time.Now().UTC()
					updated.Members[index].State = "removed"
					updated.Members[index].RemovedAt = &now
					updated.Members[index].RetireIntent = ""
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
		if member.RetireIntent != "" {
			return fmt.Errorf("member %s retirement is pending", member.Address)
		}
		archived, err := service.archivedMemberThread(ctx, member.Thread, cwd)
		if err != nil {
			return fmt.Errorf("inspect archived member %s: %w", member.Address, err)
		}
		if archived {
			if err := service.Client.RequireSubmissionAttemptsResolved(ctx, member.Thread); err != nil {
				return fmt.Errorf("member %s has unresolved submission attempts: %w", member.Address, err)
			}
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
	roster, err := service.Store.Load(slug, rootThreadID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cwd := filepath.Join(service.Store.workspace, "work", slug)
	for _, member := range roster.Members {
		if member.State != "ready" || member.ProjectID == "" {
			continue
		}
		if member.RetireIntent == "remove" {
			return fmt.Errorf("member %s removal is pending", member.Address)
		}
		archived, err := service.archivedMemberThread(ctx, member.Thread, cwd)
		if err != nil {
			return fmt.Errorf("inspect archived member %s before recovery: %w", member.Address, err)
		}
		if archived {
			continue
		}
		materialized := false
		if member.RetireIntent != "replace" {
			materialized, err = service.Client.HeadlessThreadMaterialized(ctx, member.Thread, cwd, member.ProjectID)
			if err != nil {
				return fmt.Errorf("inspect %s before archive: %w", member.Address, err)
			}
		}
		if materialized {
			continue
		}
		if err := service.recycleFreshMemberLocked(ctx, slug, rootThreadID, cwd, member); err != nil {
			return fmt.Errorf("recover %s before archive: %w", member.Address, err)
		}
		if _, err := service.retryCreatingLocked(ctx, slug, rootThreadID, cwd,
			map[string]string{"DEV_SESSION_SLUG": slug, "DEV_SESSION_WORKSPACE": service.Store.workspace, "DEV_SESSION_WORK_DIR": cwd}, member.Address); err != nil {
			return fmt.Errorf("recreate %s before archive: %w", member.Address, err)
		}
	}
	roster, err = service.Store.Load(slug, rootThreadID)
	if err != nil {
		return err
	}
	if !force {
		if err := service.requireIdleAllLocked(ctx, slug, rootThreadID); err != nil {
			return err
		}
	}
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
		if member.State == "archived" {
			if err := service.clearPreviouslyRetiredMemberAttempts(ctx, slug, member, force); err != nil {
				return fmt.Errorf("reconcile archived %s attempts: %w", member.Address, err)
			}
			continue
		}
		if member.State == "removed" {
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
			if err := service.archiveMemberThread(ctx, member.Thread, cwd); err != nil {
				return fmt.Errorf("archive %s: %w", member.Address, err)
			}
		}
		if err := service.clearRetiredMemberAttempts(ctx, member.Thread, force); err != nil {
			return fmt.Errorf("clear retired %s attempts: %w", member.Address, err)
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

// archiveMemberThread proves the exact roster thread reached archived history,
// including when App Server accepted the archive but its response was lost.
func (service Service) archiveMemberThread(ctx context.Context, threadID, cwd string) error {
	archiveErr := service.Client.ArchiveThread(ctx, threadID)
	archived, lookupErr := service.archivedMemberThread(ctx, threadID, cwd)
	if lookupErr != nil {
		return errors.Join(archiveErr, lookupErr)
	}
	if !archived {
		return errors.Join(archiveErr, errors.New("retired member thread is not archived"))
	}
	return nil
}

// Older generations could leave attempts behind after recording a terminal
// member state. A retry may clear them only with fresh proof of that exact
// thread's retirement. A missing active thread is insufficient by itself:
// archived threads are listed separately, and deleted fresh threads must also
// be absent from their dedicated project.
func (service Service) clearPreviouslyRetiredMemberAttempts(ctx context.Context, slug string, member Member, force bool) error {
	if member.Thread == "" {
		return nil
	}
	cwd := filepath.Join(service.Store.workspace, "work", slug)
	archived, err := service.archivedMemberThread(ctx, member.Thread, cwd)
	if err != nil {
		return err
	}
	if !archived {
		if member.State != "removed" || !projectUUIDPattern.MatchString(member.ProjectID) {
			return errors.New("retired member thread has no archived or deleted identity proof")
		}
		project, err := service.Client.ReadProject(ctx, member.ProjectID)
		if err != nil || project.ID != member.ProjectID || project.Name != slug+" "+member.Address {
			return fmt.Errorf("retired member project identity changed: %v", err)
		}
		matches, err := service.listMemberThreads(ctx, member.ProjectID, cwd)
		if err != nil {
			return err
		}
		if len(matches) != 0 {
			return errors.New("retired member thread still appears under its project")
		}
		_, readErr := service.Client.ReadThreadMetadata(ctx, member.Thread, false)
		if !codex.IsThreadNotFound(readErr, member.Thread) {
			if readErr == nil {
				readErr = errors.New("retired member thread still exists")
			}
			return fmt.Errorf("verify retired member thread: %w", readErr)
		}
	}
	return service.clearRetiredMemberAttempts(ctx, member.Thread, force)
}

// Forced session deletion intentionally discards unresolved attempts after
// interrupting the member. Ordinary retirement keeps their retry markers
// until the owning thread's attempts are resolved.
func (service Service) clearRetiredMemberAttempts(ctx context.Context, threadID string, force bool) error {
	if !force {
		if err := service.Client.RequireSubmissionAttemptsResolved(ctx, threadID); err != nil {
			return fmt.Errorf("unresolved member submission attempts: %w", err)
		}
	}
	return service.Client.ClearRetiredThreadAttempts(threadID)
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

// UpdateAccess changes one idle member's retained sandbox policy without
// replacing its thread, instructions, or model settings. Purpose constraints
// still apply; the next turn rebinds the saved policy through memberTurnPolicy.
func (service Service) UpdateAccess(ctx context.Context, slug, rootThreadID, address, access string) (*Roster, error) {
	if service.Store == nil || service.Client == nil {
		return nil, errors.New("team runtime is unavailable")
	}
	if access != "read_only" && access != "workspace_write" {
		return nil, errors.New("access must be read_only or workspace_write")
	}
	var result *Roster
	err := service.Store.withOperationLock(ctx, slug, func() error {
		roster, err := service.Store.Load(slug, rootThreadID)
		if err != nil {
			return err
		}
		var member *Member
		for index := range roster.Members {
			if roster.Members[index].Address == address {
				member = &roster.Members[index]
				break
			}
		}
		if member == nil || member.State != "ready" || member.RetireIntent != "" {
			return errors.New("team member is unavailable")
		}
		if err := validateMemberPolicy(member.Role, member.Behavior, member.Purpose, member.Instructions, access); err != nil {
			return fmt.Errorf("member %s access: %w", address, err)
		}
		if member.Access == access {
			result = roster
			return nil
		}
		cwd := filepath.Join(service.Store.workspace, "work", slug)
		if err := service.Client.RequireThreadIdle(ctx, member.Thread, cwd); err != nil {
			return fmt.Errorf("member %s is not idle: %w", address, err)
		}
		result, err = service.Store.Update(ctx, slug, rootThreadID, false, func(updated *Roster) error {
			if updated.Revision != roster.Revision {
				return errors.New("team roster changed during access update")
			}
			for index := range updated.Members {
				if updated.Members[index].Address == address {
					updated.Members[index].Access = access
					updated.Members[index].PolicyCatalogDigest = ""
					return nil
				}
			}
			return errors.New("team member disappeared during access update")
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
	if to != "lead" && target.ProjectID != "" {
		if target.RetireIntent == "remove" {
			return codex.SendReceipt{}, errors.New("member removal is pending")
		}
		cwd := filepath.Join(service.Store.workspace, "work", slug)
		materialized := false
		if target.RetireIntent != "replace" {
			materialized, err = service.Client.HeadlessThreadMaterialized(ctx, target.Thread, cwd, target.ProjectID)
			if err != nil {
				return codex.SendReceipt{}, fmt.Errorf("inspect %s thread: %w", to, err)
			}
		}
		if !materialized {
			if err := service.recycleFreshMemberLocked(ctx, slug, rootThreadID, cwd, *target); err != nil {
				return codex.SendReceipt{}, fmt.Errorf("recover %s thread: %w", to, err)
			}
			recovered, err := service.retryCreatingLocked(ctx, slug, rootThreadID, cwd,
				map[string]string{"DEV_SESSION_SLUG": slug, "DEV_SESSION_WORKSPACE": service.Store.workspace, "DEV_SESSION_WORK_DIR": cwd}, to)
			if err != nil {
				return codex.SendReceipt{}, fmt.Errorf("recreate %s thread: %w", to, err)
			}
			target = &recovered
		}
	}
	if model == "" {
		model = target.Model
	}
	if effort == "" {
		effort = target.Effort
	}
	var policy codex.ThreadPolicy
	if to != "lead" {
		policy, err = service.memberTurnPolicy(slug, rootThreadID, *target)
		if err != nil {
			return codex.SendReceipt{}, fmt.Errorf("member %s has no report policy: %w", to, err)
		}
	}
	envelope := fmt.Sprintf("Team assignment from %s to %s:\n\n%s", from, target.Address, strings.TrimSpace(message))
	if to != "lead" {
		envelope += "\n\nSend results or blocking questions to lead with report_to_lead. Use a new message_id for each message; reuse it only to retry that message."
	}
	// Team retries always supply their exact options. Compact only accepted
	// team attempts before SendWithOptions reserves another ledger entry; the
	// retained digests still bind a same-ID retry to its original request.
	if err := service.Client.CompactAcceptedSendOptions("team:"); err != nil {
		return codex.SendReceipt{}, fmt.Errorf("compact accepted team assignments: %w", err)
	}
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
		if member.State == "ready" && member.ProjectID != "" {
			materialized, err := service.Client.HeadlessThreadMaterialized(ctx, member.Thread,
				filepath.Join(service.Store.workspace, "work", source.Slug), member.ProjectID)
			if err != nil {
				return nil, fmt.Errorf("inspect fork source member %s: %w", member.Address, err)
			}
			if !materialized {
				return nil, fmt.Errorf("fork source member %s has no persisted rollout", member.Address)
			}
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
			roster.LeadInstructions = source.LeadInstructions
			roster.ForkSource = requested
			for _, old := range source.Members {
				member := old
				member.Thread, member.ProjectID, member.CreateAttempted, member.BootstrapAttempted = "", "", false, false
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
		destination.LeadInstructions != source.LeadInstructions ||
		len(destination.Members) != len(requested.Members) {
		return nil, errors.New("destination team roster differs from its frozen fork source")
	}
	for index, old := range destination.ForkSource.Members {
		member := destination.Members[index]
		if member.Address != old.Address || member.Role != old.Role || member.Index != old.Index ||
			member.Model != old.Model || member.Effort != old.Effort || member.Behavior != old.Behavior ||
			member.Purpose != old.Purpose || member.Instructions != old.Instructions ||
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
			policy, err := memberPolicy(service.Store.workspace, slug, member)
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
		materialized, err := service.Client.ForkedHeadlessThreadMaterialized(ctx, threadID, cwd, old.Thread)
		if err != nil {
			return nil, fmt.Errorf("inspect fork %s bootstrap: %w", old.Address, err)
		}
		if !materialized && member.BootstrapAttempted {
			return nil, fmt.Errorf("fork %s bootstrap outcome is uncertain; reconcile the exact thread before retry", old.Address)
		}
		marker := memberBootstrapMarker(slug, old.Address, threadID)
		if !member.BootstrapAttempted {
			if _, err := service.Store.Update(ctx, slug, rootThreadID, false, func(roster *Roster) error {
				candidate := &roster.Members[index]
				if candidate.Address != old.Address || candidate.State != "creating" || candidate.Thread != threadID || candidate.BootstrapAttempted {
					return errors.New("fork bootstrap reservation changed")
				}
				candidate.BootstrapAttempted = true
				return nil
			}); err != nil {
				return nil, err
			}
			if err := service.Client.BootstrapForkedHeadlessThread(ctx, threadID, cwd, old.Thread, marker); err != nil {
				return nil, fmt.Errorf("fork %s bootstrap outcome is uncertain: %w", old.Address, err)
			}
		} else if err := service.Client.VerifyForkedHeadlessBootstrap(ctx, threadID, cwd, old.Thread, marker); err != nil {
			return nil, fmt.Errorf("verify fork %s bootstrap: %w", old.Address, err)
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
