// Package agentteams validates immutable package-time agent-team data and
// persisted session state. It publishes retained state only through the
// explicit CreateRetained boundary and does not reconcile team members.
package agentteams

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

const (
	CatalogSchemaVersion        = 4
	PackageMetadataSchema       = 1
	maxCatalogBytes             = 4 * 1024 * 1024
	maxPackageMetadataBytes     = 1024 * 1024
	packageMetadataRelativePath = "share/dev-workspace/package.json"
	defaultCatalogRelativePath  = "share/dev-workspace/agent-teams.json"
	agentTeamsGeneratorIdentity = "dev-workspace-nix-agent-teams"
	agentTeamsCanonicalization  = "nix-builtins-toJSON-attrset-v1"
	nativeAdapterIdentity       = "codex-custom-agent-toml"
	nativeConfigurationPathRoot = "share/dev-workspace/agent-teams"
)

var supportedEfforts = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true,
	"xhigh": true, "max": true, "ultra": true,
}

type GeneratorIdentity struct {
	Identity         string `json:"identity"`
	Version          int    `json:"version"`
	Canonicalization string `json:"canonicalization"`
}

type NativeAdapterIdentity struct {
	Identity string `json:"identity"`
	Version  int    `json:"version"`
}

type NativeIdentity struct {
	Generator      GeneratorIdentity     `json:"generator"`
	NativeAdapter  NativeAdapterIdentity `json:"native_adapter"`
	BehaviorDigest string                `json:"behavior_digest"`
}

type Capacity struct {
	RequiredNativeChildThreads int `json:"required_native_child_threads"`
}

type WorkPolicy struct {
	Default              string   `json:"default"`
	Simple               string   `json:"simple"`
	Allowed              []string `json:"allowed"`
	SimpleRequiresReason bool     `json:"simple_requires_reason"`
	Followup             string   `json:"followup"`
}

type WorkPolicies struct {
	Design         WorkPolicy `json:"design"`
	Implementation WorkPolicy `json:"implementation"`
}

type Role struct {
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	Behavior       string   `json:"behavior"`
	Purpose        string   `json:"purpose"`
	Instructions   string   `json:"instructions"`
	Lifetime       string   `json:"lifetime"`
	AllowedEfforts []string `json:"allowed_efforts"`
	Access         string   `json:"access"`
	FreshContext   bool     `json:"fresh_context"`
}

type Lifecycle struct {
	Startup       string `json:"startup"`
	Communication string `json:"communication"`
	ReviewerReuse string `json:"reviewer_reuse"`
}

type Routing struct {
	DesignSimpleEffort      *string `json:"design_simple_effort,omitempty"`
	ImplementerSimpleEffort *string `json:"implementer_simple_effort,omitempty"`
	LowRiskReviewRole       *string `json:"low_risk_review_role,omitempty"`
}

type Team struct {
	Description   string          `json:"description"`
	DesignOwner   string          `json:"design_owner"`
	Lifecycle     Lifecycle       `json:"lifecycle"`
	MaxOpenAgents int             `json:"max_open_agents"`
	Mode          string          `json:"mode"`
	Roles         map[string]Role `json:"roles"`
	Routing       Routing         `json:"routing"`
	ServicePolicy string          `json:"service_policy"`
	TeamDigest    string          `json:"team_digest"`
}

type VerificationWatcher struct {
	Model         string   `json:"model"`
	Effort        string   `json:"effort"`
	Behavior      string   `json:"behavior"`
	Lifetime      string   `json:"lifetime"`
	Access        string   `json:"access"`
	Startup       string   `json:"startup"`
	MaxConcurrent int      `json:"max_concurrent"`
	RequiredFor   []string `json:"required_for"`
}

type Utilities struct {
	VerificationWatcher VerificationWatcher `json:"verification_watcher"`
}

type NativeRoleConfig struct {
	Kind     string         `json:"kind"`
	Team     string         `json:"team"`
	Role     string         `json:"role"`
	Effort   string         `json:"effort"`
	Identity NativeIdentity `json:"identity"`
	Name     string         `json:"name"`
	Path     string         `json:"path"`
}

type NativeUtilityConfig struct {
	Kind     string         `json:"kind"`
	Utility  string         `json:"utility"`
	Effort   string         `json:"effort"`
	Identity NativeIdentity `json:"identity"`
	Name     string         `json:"name"`
	Path     string         `json:"path"`
}

type NativeAgentConfigs struct {
	Adapter   NativeAdapterIdentity `json:"adapter"`
	Roles     []NativeRoleConfig    `json:"roles"`
	Utilities []NativeUtilityConfig `json:"utilities"`
}

type Catalog struct {
	SchemaVersion          int                `json:"schema_version"`
	DefaultTeam            string             `json:"default_team"`
	DefaultDevelopmentTeam *string            `json:"default_development_team"`
	Capacity               Capacity           `json:"capacity"`
	WorkPolicy             WorkPolicies       `json:"work_policy"`
	Teams                  map[string]Team    `json:"teams"`
	Utilities              Utilities          `json:"utilities"`
	CatalogDigest          string             `json:"catalog_digest"`
	ModelInventory         []string           `json:"model_inventory"`
	NativeAgentConfigs     NativeAgentConfigs `json:"native_agent_configs"`
}

type CatalogReference struct {
	Path          string `json:"path"`
	Digest        string `json:"digest"`
	SchemaVersion int    `json:"schema_version"`
}

type NativeCapacity struct {
	ConfigKey     string `json:"config_key"`
	RequiredValue int    `json:"required_value"`
}

type AgentTeamsMetadata struct {
	Managed              bool                  `json:"managed"`
	MetadataSchema       int                   `json:"metadata_schema,omitempty"`
	Catalog              CatalogReference      `json:"catalog,omitempty"`
	Capacity             Capacity              `json:"capacity,omitempty"`
	NativeCapacity       NativeCapacity        `json:"native_capacity,omitempty"`
	Generator            GeneratorIdentity     `json:"generator,omitempty"`
	NativeAdapter        NativeAdapterIdentity `json:"native_adapter,omitempty"`
	NativeRoleConfigs    []NativeRoleConfig    `json:"native_role_configs,omitempty"`
	NativeUtilityConfigs []NativeUtilityConfig `json:"native_utility_configs,omitempty"`
}

type PackageMetadata struct {
	Schema                       int                `json:"schema"`
	ActivationEnvironmentAliases []string           `json:"activationEnvironmentAliases"`
	RouterSocket                 string             `json:"routerSocket"`
	UserNamespace                string             `json:"userNamespace"`
	AgentTeams                   AgentTeamsMetadata `json:"agent_teams"`
}

type CatalogPin struct {
	PackagePath    string                `json:"package_path"`
	CatalogPath    string                `json:"catalog_path"`
	CatalogDigest  string                `json:"catalog_digest"`
	SchemaVersion  int                   `json:"schema_version"`
	MetadataSchema int                   `json:"metadata_schema"`
	Generator      GeneratorIdentity     `json:"generator"`
	NativeAdapter  NativeAdapterIdentity `json:"native_adapter"`
	Capacity       Capacity              `json:"capacity"`
}

type Installed struct {
	Managed     bool
	Metadata    PackageMetadata
	Catalog     *Catalog
	packageRoot string
}

func (catalog Catalog) Team(name string) (Team, bool) {
	team, ok := catalog.Teams[name]
	return team, ok
}

func (catalog Catalog) RoleVariant(team, role, effort string) (NativeRoleConfig, bool) {
	for _, variant := range catalog.NativeAgentConfigs.Roles {
		if variant.Team == team && variant.Role == role && variant.Effort == effort {
			return variant, true
		}
	}
	return NativeRoleConfig{}, false
}

func LoadInstalled(packageRoot string) (Installed, error) {
	metadataData, err := readPackageFile(packageRoot, packageMetadataRelativePath, maxPackageMetadataBytes)
	if err != nil {
		return Installed{}, fmt.Errorf("read package metadata: %w", err)
	}
	metadata, err := decodePackageMetadata(metadataData)
	if err != nil {
		return Installed{}, fmt.Errorf("load package metadata: %w", err)
	}
	installed := Installed{Managed: metadata.AgentTeams.Managed, Metadata: metadata, packageRoot: packageRoot}
	if !installed.Managed {
		return installed, nil
	}
	catalogData, err := readPackageFile(packageRoot, metadata.AgentTeams.Catalog.Path, maxCatalogBytes)
	if err != nil {
		return Installed{}, fmt.Errorf("read package catalog: %w", err)
	}
	catalog, err := decodeCatalog(catalogData)
	if err != nil {
		return Installed{}, fmt.Errorf("load package catalog: %w", err)
	}
	if err := validateMetadataCatalog(metadata.AgentTeams, catalog); err != nil {
		return Installed{}, err
	}
	for _, variant := range catalog.NativeAgentConfigs.Roles {
		if _, err := readPackageFile(packageRoot, variant.Path, 64*1024); err != nil {
			return Installed{}, fmt.Errorf("read native role %s: %w", variant.Name, err)
		}
	}
	for _, variant := range catalog.NativeAgentConfigs.Utilities {
		if _, err := readPackageFile(packageRoot, variant.Path, 64*1024); err != nil {
			return Installed{}, fmt.Errorf("read native utility %s: %w", variant.Name, err)
		}
	}
	installed.Catalog = &catalog
	return installed, nil
}

func LoadPinned(packageRoot string, pin CatalogPin) (Catalog, error) {
	if err := pin.Validate(); err != nil {
		return Catalog{}, fmt.Errorf("validate catalog pin: %w", err)
	}
	if !nixStoreRoot(packageRoot) || packageRoot != pin.PackagePath {
		return Catalog{}, errors.New("pinned package root is not the canonical store path")
	}
	installed, err := LoadInstalled(packageRoot)
	if err != nil {
		return Catalog{}, err
	}
	if !installed.Managed || installed.Catalog == nil {
		return Catalog{}, errors.New("pinned package is unmanaged")
	}
	if pin.PackagePath != packageRoot || pin.CatalogPath != installed.Metadata.AgentTeams.Catalog.Path ||
		pin.CatalogDigest != installed.Catalog.CatalogDigest || pin.SchemaVersion != installed.Catalog.SchemaVersion ||
		pin.MetadataSchema != installed.Metadata.AgentTeams.MetadataSchema ||
		pin.Capacity != installed.Catalog.Capacity || pin.Generator != installed.Metadata.AgentTeams.Generator ||
		pin.NativeAdapter != installed.Metadata.AgentTeams.NativeAdapter {
		return Catalog{}, errors.New("pinned package does not match installed catalog")
	}
	return *installed.Catalog, nil
}

func (pin CatalogPin) Validate() error {
	if !nixStoreRoot(pin.PackagePath) || pin.CatalogPath != defaultCatalogRelativePath || !isDigest(pin.CatalogDigest) ||
		pin.SchemaVersion != CatalogSchemaVersion || pin.MetadataSchema != PackageMetadataSchema ||
		pin.Capacity.RequiredNativeChildThreads < 1 || pin.Capacity.RequiredNativeChildThreads > 64 || !validGenerator(pin.Generator) || !validAdapter(pin.NativeAdapter) {
		return errors.New("invalid catalog pin")
	}
	return nil
}

func decodeCatalog(data []byte) (Catalog, error) {
	if len(data) > maxCatalogBytes {
		return Catalog{}, errors.New("catalog exceeds 4 MiB")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Catalog{}, err
	}
	if err := validateCatalogShape(data); err != nil {
		return Catalog{}, err
	}
	var catalog Catalog
	if err := decodeStrict(data, &catalog); err != nil {
		return Catalog{}, err
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	if digest, err := catalogDigest(data); err != nil || digest != catalog.CatalogDigest {
		if err != nil {
			return Catalog{}, err
		}
		return Catalog{}, errors.New("catalog digest does not match canonical source")
	}
	if err := validateTeamDigests(data, catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func decodePackageMetadata(data []byte) (PackageMetadata, error) {
	if len(data) > maxPackageMetadataBytes {
		return PackageMetadata{}, errors.New("package metadata exceeds 1 MiB")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return PackageMetadata{}, err
	}
	if err := validateMetadataShape(data); err != nil {
		return PackageMetadata{}, err
	}
	var metadata PackageMetadata
	if err := decodeStrict(data, &metadata); err != nil {
		return PackageMetadata{}, err
	}
	if metadata.Schema != PackageMetadataSchema {
		return PackageMetadata{}, errors.New("unsupported package metadata schema")
	}
	if err := metadata.AgentTeams.Validate(); err != nil {
		return PackageMetadata{}, err
	}
	return metadata, nil
}

func (metadata AgentTeamsMetadata) Validate() error {
	if !metadata.Managed {
		if metadata.MetadataSchema != 0 || metadata.Catalog != (CatalogReference{}) ||
			metadata.Capacity != (Capacity{}) || metadata.NativeCapacity != (NativeCapacity{}) ||
			metadata.Generator != (GeneratorIdentity{}) || metadata.NativeAdapter != (NativeAdapterIdentity{}) ||
			len(metadata.NativeRoleConfigs) != 0 || len(metadata.NativeUtilityConfigs) != 0 {
			return errors.New("unmanaged package metadata contains managed fields")
		}
		return nil
	}
	if metadata.MetadataSchema != 1 || metadata.Catalog.Path != defaultCatalogRelativePath ||
		metadata.Catalog.SchemaVersion != CatalogSchemaVersion || !isDigest(metadata.Catalog.Digest) ||
		metadata.Capacity.RequiredNativeChildThreads < 1 || metadata.Capacity.RequiredNativeChildThreads > 64 || metadata.NativeCapacity.ConfigKey != "agents.max_concurrent_threads_per_session" ||
		metadata.NativeCapacity.RequiredValue != metadata.Capacity.RequiredNativeChildThreads ||
		!validGenerator(metadata.Generator) || !validAdapter(metadata.NativeAdapter) {
		return errors.New("invalid managed package metadata")
	}
	return nil
}

func (catalog Catalog) Validate() error {
	if catalog.SchemaVersion != CatalogSchemaVersion || !identifier(catalog.DefaultTeam) ||
		catalog.Capacity.RequiredNativeChildThreads < 1 || catalog.Capacity.RequiredNativeChildThreads > 64 || !isDigest(catalog.CatalogDigest) ||
		len(catalog.Teams) == 0 || !validWorkPolicy(catalog.WorkPolicy.Design) ||
		!validWorkPolicy(catalog.WorkPolicy.Implementation) || !validAdapter(catalog.NativeAgentConfigs.Adapter) {
		return errors.New("invalid catalog")
	}
	if _, ok := catalog.Teams[catalog.DefaultTeam]; !ok {
		return errors.New("catalog default team is absent")
	}
	if catalog.DefaultDevelopmentTeam != nil {
		team, ok := catalog.Teams[*catalog.DefaultDevelopmentTeam]
		if !ok || team.Mode != "development" {
			return errors.New("catalog development default is invalid")
		}
	}
	for name, team := range catalog.Teams {
		if !identifier(name) || !validTeam(name, team, catalog.Capacity, catalog.WorkPolicy) {
			return fmt.Errorf("invalid catalog team %q", name)
		}
	}
	if !validWatcher(catalog.Utilities.VerificationWatcher) {
		return errors.New("invalid verification watcher")
	}
	models := make([]string, 0)
	for _, team := range catalog.Teams {
		for _, role := range team.Roles {
			models = append(models, role.Model)
		}
	}
	models = append(models, catalog.Utilities.VerificationWatcher.Model)
	models = sortedUnique(models)
	if !reflect.DeepEqual(models, catalog.ModelInventory) {
		return errors.New("catalog model inventory is not derived and sorted")
	}
	return validateVariants(catalog)
}

func validateMetadataCatalog(metadata AgentTeamsMetadata, catalog Catalog) error {
	if metadata.Catalog.Digest != catalog.CatalogDigest || metadata.Catalog.SchemaVersion != catalog.SchemaVersion ||
		metadata.Capacity != catalog.Capacity || metadata.NativeAdapter != catalog.NativeAgentConfigs.Adapter ||
		!reflect.DeepEqual(metadata.NativeRoleConfigs, catalog.NativeAgentConfigs.Roles) ||
		!reflect.DeepEqual(metadata.NativeUtilityConfigs, catalog.NativeAgentConfigs.Utilities) {
		return errors.New("package metadata does not match catalog")
	}
	return nil
}

func validateVariants(catalog Catalog) error {
	roles := catalog.NativeAgentConfigs.Roles
	if len(roles) == 0 || len(catalog.NativeAgentConfigs.Utilities) != 1 {
		return errors.New("catalog has incomplete native configurations")
	}
	seenNames := make(map[string]bool)
	want := make(map[string]bool)
	for teamName, team := range catalog.Teams {
		for roleName, role := range team.Roles {
			for _, effort := range role.AllowedEfforts {
				want[teamName+"\x00"+roleName+"\x00"+effort] = true
			}
		}
	}
	for _, variant := range roles {
		key := variant.Team + "\x00" + variant.Role + "\x00" + variant.Effort
		role, teamOK := catalog.Teams[variant.Team].Roles[variant.Role]
		if !teamOK || !want[key] || variant.Kind != "role" || !identifier(variant.Team) || !identifier(variant.Role) ||
			!supportedEfforts[variant.Effort] || !validNativeIdentity(variant.Identity) ||
			variant.Path != expectedRolePath(variant) || !nativeName(variant.Name) || seenNames[variant.Name] {
			return errors.New("invalid native role variant")
		}
		if variant.Identity.BehaviorDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(role.Instructions))) {
			return errors.New("native role instructions differ from the catalog")
		}
		if variant.Name != expectedRoleName(catalog.CatalogDigest, variant) {
			return errors.New("native role name does not match immutable identity")
		}
		seenNames[variant.Name] = true
		delete(want, key)
	}
	if len(want) != 0 {
		return errors.New("catalog lacks native role variants")
	}
	utility := catalog.NativeAgentConfigs.Utilities[0]
	watcher := catalog.Utilities.VerificationWatcher
	if utility.Kind != "utility" || utility.Utility != "verification_watcher" || utility.Effort != watcher.Effort ||
		!validNativeIdentity(utility.Identity) || utility.Path != expectedUtilityPath(utility) || !nativeName(utility.Name) ||
		seenNames[utility.Name] || utility.Name != expectedUtilityName(catalog.CatalogDigest, utility) {
		return errors.New("invalid native utility variant")
	}
	return nil
}

func validTeam(name string, team Team, capacity Capacity, policies WorkPolicies) bool {
	if !nonempty(team.Description, 4096) || (team.Mode != "solo" && team.Mode != "development") ||
		(team.ServicePolicy != "unmanaged" && team.ServicePolicy != "non_priority") || !identifier(team.DesignOwner) ||
		!validLifecycle(team.Lifecycle) || !isDigest(team.TeamDigest) || len(team.Roles) == 0 || len(team.Roles) > 8 {
		return false
	}
	lead, exists := team.Roles["team_lead"]
	if !exists || lead.Behavior != "team_lead" || !validRole(lead) ||
		(team.Roles["designer"].Behavior != "" && team.Roles["architect"].Behavior != "") {
		return false
	}
	for roleName, role := range team.Roles {
		if !roleNameIdentifier(roleName) || !validRole(role) ||
			roleName == "lead" ||
			(roleName != "team_lead" && role.Purpose == "lead") ||
			(role.Purpose == "implementation" && role.Access != "workspace_write") ||
			(role.Purpose == "review" && role.Access != "read_only") ||
			(roleName == "designer" && role.Purpose != "design") ||
			(roleName == "implementer" && role.Purpose != "implementation") ||
			(roleName == "reviewer" && role.Purpose != "review") {
			return false
		}
	}
	owner, ok := team.Roles[team.DesignOwner]
	if !ok || (team.DesignOwner == "team_lead" && owner.Purpose != "lead") ||
		(team.DesignOwner != "team_lead" && owner.Purpose != "design") {
		return false
	}
	if team.Mode == "solo" {
		return len(team.Roles) == 1 && team.DesignOwner == "team_lead" && team.MaxOpenAgents == 0 &&
			team.Routing == (Routing{}) && supportsPolicy(lead, policies.Design) && supportsPolicy(lead, policies.Implementation)
	}
	hasImplementation, hasReviewer := false, false
	for _, role := range team.Roles {
		if role.Purpose == "implementation" && matchesPolicy(role, policies.Implementation) {
			hasImplementation = true
		}
		if role.Purpose == "review" && role.FreshContext {
			hasReviewer = true
		}
	}
	if !matchesPolicy(owner, policies.Design) || !hasImplementation || !hasReviewer || team.MaxOpenAgents < 1 ||
		team.MaxOpenAgents > capacity.RequiredNativeChildThreads || team.Routing.DesignSimpleEffort == nil ||
		team.Routing.ImplementerSimpleEffort == nil || *team.Routing.DesignSimpleEffort != policies.Design.Simple ||
		*team.Routing.ImplementerSimpleEffort != policies.Implementation.Simple {
		return false
	}
	for _, role := range team.Roles {
		if role.Purpose == "review" && !role.FreshContext {
			return false
		}
	}
	if team.Routing.LowRiskReviewRole != nil {
		role, ok := team.Roles[*team.Routing.LowRiskReviewRole]
		if !ok || role.Purpose != "review" || !role.FreshContext {
			return false
		}
	}
	return true
}

func validWorkPolicy(policy WorkPolicy) bool {
	return supportedEfforts[policy.Default] && supportedEfforts[policy.Simple] && policy.Followup == "retain" &&
		validEffortList(policy.Allowed) && contains(policy.Allowed, policy.Default) && contains(policy.Allowed, policy.Simple)
}

func validRole(role Role) bool {
	return nonempty(role.Model, session.MaxAgentTeamPersistedScalarBytes) && supportedEfforts[role.Effort] && validEffortList(role.AllowedEfforts) &&
		contains(role.AllowedEfforts, role.Effort) && contains([]string{"team_lead", "designer", "implementer", "reviewer", "general"}, role.Behavior) &&
		validPurpose(role.Purpose, role.Behavior) && nonempty(role.Instructions, 4096) && !strings.ContainsRune(role.Instructions, '\x00') &&
		contains([]string{"session", "initiative", "work_unit", "review_cycle"}, role.Lifetime) &&
		contains([]string{"read_only", "workspace_write"}, role.Access)
}

func validPurpose(purpose, behavior string) bool {
	return map[string]string{"team_lead": "lead", "designer": "design", "implementer": "implementation", "reviewer": "review", "general": "general"}[behavior] == purpose && purpose != ""
}

func validLifecycle(lifecycle Lifecycle) bool {
	return lifecycle.Startup == "on_demand" && lifecycle.Communication == "lead_mediated" && lifecycle.ReviewerReuse == "same_change"
}

func validWatcher(watcher VerificationWatcher) bool {
	return nonempty(watcher.Model, session.MaxAgentTeamPersistedScalarBytes) && supportedEfforts[watcher.Effort] && watcher.Behavior == "verification_watcher" &&
		watcher.Lifetime == "operation" && contains([]string{"read_only", "workspace_write"}, watcher.Access) &&
		watcher.Startup == "on_demand" && watcher.MaxConcurrent > 0 && validIdentifierList(watcher.RequiredFor)
}

func validGenerator(generator GeneratorIdentity) bool {
	return generator.Identity == agentTeamsGeneratorIdentity && generator.Version == 1 && generator.Canonicalization == agentTeamsCanonicalization
}

func validAdapter(adapter NativeAdapterIdentity) bool {
	return adapter.Identity == nativeAdapterIdentity && adapter.Version == 1
}

func validNativeIdentity(identity NativeIdentity) bool {
	return validGenerator(identity.Generator) && validAdapter(identity.NativeAdapter) && isDigest(identity.BehaviorDigest)
}

func matchesPolicy(role Role, policy WorkPolicy) bool {
	return role.Effort == policy.Default && reflect.DeepEqual(role.AllowedEfforts, policy.Allowed)
}

func supportsPolicy(role Role, policy WorkPolicy) bool {
	return contains(role.AllowedEfforts, policy.Default) && contains(role.AllowedEfforts, policy.Simple)
}

func expectedRoleName(digest string, variant NativeRoleConfig) string {
	return nativeVariantName(map[string]any{"catalog_digest": digest, "kind": "role", "identity": variant.Identity, "team": variant.Team, "effort": variant.Effort, "role": variant.Role})
}

func expectedUtilityName(digest string, variant NativeUtilityConfig) string {
	return nativeVariantName(map[string]any{"catalog_digest": digest, "kind": "utility", "identity": variant.Identity, "utility": variant.Utility, "effort": variant.Effort})
}

func expectedRolePath(variant NativeRoleConfig) string {
	return nativeConfigurationPathRoot + "/" + variant.Team + "/" + variant.Role + "-" + variant.Effort + ".toml"
}

func expectedUtilityPath(variant NativeUtilityConfig) string {
	return nativeConfigurationPathRoot + "/utilities/" + variant.Utility + "-" + variant.Effort + ".toml"
}

func nativeVariantName(input any) string {
	encoded, err := canonicalJSON(input)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "dw_" + hex.EncodeToString(digest[:])[:48]
}

func catalogDigest(data []byte) (string, error) {
	root, err := decodeRawJSON(data)
	if err != nil {
		return "", err
	}
	object, ok := root.(map[string]any)
	if !ok {
		return "", errors.New("catalog must be an object")
	}
	delete(object, "catalog_digest")
	delete(object, "model_inventory")
	delete(object, "native_agent_configs")
	teams, ok := object["teams"].(map[string]any)
	if !ok {
		return "", errors.New("catalog teams are invalid")
	}
	for _, rawTeam := range teams {
		team, ok := rawTeam.(map[string]any)
		if !ok {
			return "", errors.New("catalog team is invalid")
		}
		delete(team, "team_digest")
	}
	return digestJSON(object)
}

func validateTeamDigests(data []byte, catalog Catalog) error {
	root, err := decodeRawJSON(data)
	if err != nil {
		return err
	}
	object := root.(map[string]any)
	teams := object["teams"].(map[string]any)
	for name, rawTeam := range teams {
		team := rawTeam.(map[string]any)
		delete(team, "team_digest")
		want, err := digestJSON(map[string]any{
			"schema_version": object["schema_version"], "capacity": object["capacity"],
			"work_policy": object["work_policy"], "team_name": name, "team": team,
		})
		if err != nil || catalog.Teams[name].TeamDigest != want {
			if err != nil {
				return err
			}
			return fmt.Errorf("team digest does not match canonical source for %q", name)
		}
	}
	return nil
}

func digestJSON(value any) (string, error) {
	encoded, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeRawJSON(encoded)
	if err != nil {
		return nil, err
	}
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(decoded); err != nil {
		return nil, err
	}
	result := bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))
	return nixJSONSeparators(result), nil
}

// Nix builtins.toJSON emits literal U+2028/U+2029 while encoding/json escapes
// them even with SetEscapeHTML(false). Decode only an odd trailing backslash in
// each run: an even run is a literal JSON-escaped backslash and must remain so.
func nixJSONSeparators(data []byte) []byte {
	var result bytes.Buffer
	for offset := 0; offset < len(data); {
		if data[offset] != '\\' {
			result.WriteByte(data[offset])
			offset++
			continue
		}
		start := offset
		for offset < len(data) && data[offset] == '\\' {
			offset++
		}
		if (offset-start)%2 == 1 && offset+5 <= len(data) && string(data[offset:offset+5]) == "u2028" {
			result.Write(data[start : offset-1])
			result.WriteString("\u2028")
			offset += 5
			continue
		}
		if (offset-start)%2 == 1 && offset+5 <= len(data) && string(data[offset:offset+5]) == "u2029" {
			result.Write(data[start : offset-1])
			result.WriteString("\u2029")
			offset += 5
			continue
		}
		result.Write(data[start:offset])
	}
	return result.Bytes()
}

func decodeRawJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON has trailing data")
	}
	return value, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON has trailing data")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token := token.(type) {
		case json.Delim:
			switch token {
			case '{':
				seen := make(map[string]bool)
				for decoder.More() {
					key, err := decoder.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return errors.New("JSON object contains a duplicate key")
					}
					seen[name] = true
					if err := walk(); err != nil {
						return err
					}
				}
				_, err := decoder.Token()
				return err
			case '[':
				for decoder.More() {
					if err := walk(); err != nil {
						return err
					}
				}
				_, err := decoder.Token()
				return err
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON has trailing data")
	}
	return nil
}

func strictObject(raw json.RawMessage, fields ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("expected JSON object")
	}
	if len(object) != len(fields) {
		return nil, errors.New("JSON object has missing or unknown fields")
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return nil, errors.New("JSON object has missing or unknown fields")
		}
	}
	return object, nil
}

func objectWithOnly(raw json.RawMessage, fields ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("expected JSON object")
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	for field := range object {
		if !allowed[field] {
			return nil, errors.New("JSON object has an unknown field")
		}
	}
	return object, nil
}

func validateCatalogShape(data []byte) error {
	root, err := strictObject(data, "schema_version", "default_team", "default_development_team", "capacity", "work_policy", "teams", "utilities", "catalog_digest", "model_inventory", "native_agent_configs")
	if err != nil {
		return err
	}
	if _, err := strictObject(root["capacity"], "required_native_child_threads"); err != nil {
		return err
	}
	policies, err := strictObject(root["work_policy"], "design", "implementation")
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if _, err := strictObject(policy, "default", "simple", "allowed", "simple_requires_reason", "followup"); err != nil {
			return err
		}
	}
	var teams map[string]json.RawMessage
	if err := json.Unmarshal(root["teams"], &teams); err != nil || len(teams) == 0 {
		return errors.New("catalog teams must be a nonempty object")
	}
	for _, rawTeam := range teams {
		team, err := strictObject(rawTeam, "description", "design_owner", "lifecycle", "max_open_agents", "mode", "roles", "routing", "service_policy", "team_digest")
		if err != nil {
			return err
		}
		if _, err := strictObject(team["lifecycle"], "startup", "communication", "reviewer_reuse"); err != nil {
			return err
		}
		routing, err := objectWithOnly(team["routing"], "design_simple_effort", "implementer_simple_effort", "low_risk_review_role")
		if err != nil {
			return err
		}
		for _, value := range routing {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return errors.New("routing values must be absent, not null")
			}
		}
		var roles map[string]json.RawMessage
		if err := json.Unmarshal(team["roles"], &roles); err != nil || len(roles) == 0 {
			return errors.New("team roles must be a nonempty object")
		}
		for _, role := range roles {
			if _, err := strictObject(role, "model", "effort", "behavior", "purpose", "instructions", "lifetime", "allowed_efforts", "access", "fresh_context"); err != nil {
				return err
			}
		}
	}
	utilities, err := strictObject(root["utilities"], "verification_watcher")
	if err != nil {
		return err
	}
	if _, err := strictObject(utilities["verification_watcher"], "model", "effort", "behavior", "lifetime", "access", "startup", "max_concurrent", "required_for"); err != nil {
		return err
	}
	native, err := strictObject(root["native_agent_configs"], "adapter", "roles", "utilities")
	if err != nil {
		return err
	}
	if _, err := strictObject(native["adapter"], "identity", "version"); err != nil {
		return err
	}
	var roles []json.RawMessage
	if err := json.Unmarshal(native["roles"], &roles); err != nil {
		return err
	}
	for _, role := range roles {
		if err := validateNativeRoleShape(role); err != nil {
			return err
		}
	}
	var utility []json.RawMessage
	if err := json.Unmarshal(native["utilities"], &utility); err != nil {
		return err
	}
	for _, item := range utility {
		if err := validateNativeUtilityShape(item); err != nil {
			return err
		}
	}
	return nil
}

func validateNativeRoleShape(raw json.RawMessage) error {
	role, err := strictObject(raw, "kind", "team", "effort", "identity", "name", "path", "role")
	if err != nil {
		return err
	}
	return validateNativeIdentityShape(role["identity"])
}

func validateNativeUtilityShape(raw json.RawMessage) error {
	utility, err := strictObject(raw, "kind", "utility", "effort", "identity", "name", "path")
	if err != nil {
		return err
	}
	return validateNativeIdentityShape(utility["identity"])
}

func validateNativeIdentityShape(raw json.RawMessage) error {
	identity, err := strictObject(raw, "generator", "native_adapter", "behavior_digest")
	if err != nil {
		return err
	}
	if _, err := strictObject(identity["generator"], "identity", "version", "canonicalization"); err != nil {
		return err
	}
	if _, err := strictObject(identity["native_adapter"], "identity", "version"); err != nil {
		return err
	}
	return nil
}

func validateMetadataShape(data []byte) error {
	root, err := strictObject(data, "schema", "activationEnvironmentAliases", "routerSocket", "userNamespace", "agent_teams")
	if err != nil {
		return err
	}
	var union map[string]json.RawMessage
	if err := json.Unmarshal(root["agent_teams"], &union); err != nil || union == nil {
		return errors.New("agent team metadata must be an object")
	}
	managed, ok := union["managed"]
	if !ok {
		return errors.New("agent team metadata has no managed field")
	}
	var enabled bool
	if err := json.Unmarshal(managed, &enabled); err != nil {
		return errors.New("agent team metadata managed is invalid")
	}
	if !enabled {
		_, err := strictObject(root["agent_teams"], "managed")
		return err
	}
	agents, err := strictObject(root["agent_teams"], "managed", "metadata_schema", "catalog", "capacity", "native_capacity", "generator", "native_adapter", "native_role_configs", "native_utility_configs")
	if err != nil {
		return err
	}
	if _, err := strictObject(agents["catalog"], "path", "digest", "schema_version"); err != nil {
		return err
	}
	if _, err := strictObject(agents["capacity"], "required_native_child_threads"); err != nil {
		return err
	}
	if _, err := strictObject(agents["native_capacity"], "config_key", "required_value"); err != nil {
		return err
	}
	if _, err := strictObject(agents["generator"], "identity", "version", "canonicalization"); err != nil {
		return err
	}
	if _, err := strictObject(agents["native_adapter"], "identity", "version"); err != nil {
		return err
	}
	var roles []json.RawMessage
	if err := json.Unmarshal(agents["native_role_configs"], &roles); err != nil {
		return err
	}
	for _, role := range roles {
		if err := validateNativeRoleShape(role); err != nil {
			return err
		}
	}
	var utilities []json.RawMessage
	if err := json.Unmarshal(agents["native_utility_configs"], &utilities); err != nil {
		return err
	}
	for _, utility := range utilities {
		if err := validateNativeUtilityShape(utility); err != nil {
			return err
		}
	}
	return nil
}

func readPackageFile(root, relative string, limit int) ([]byte, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !cleanPackagePath(relative) {
		return nil, errors.New("unsafe package path")
	}
	path := filepath.Join(root, relative)
	parent := filepath.Dir(path)
	relativeParent, err := filepath.Rel(root, parent)
	if err != nil || relativeParent == "." || strings.HasPrefix(relativeParent, ".."+string(filepath.Separator)) {
		return nil, errors.New("unsafe package parent path")
	}
	directory := root
	for _, component := range strings.Split(relativeParent, string(filepath.Separator)) {
		info, err := os.Lstat(directory)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("package directory is unsafe")
		}
		directory = filepath.Join(directory, component)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("package directory is unsafe")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return nil, errors.New("invalid package file descriptor")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() > int64(limit) {
		return nil, errors.New("package file is not a bounded immutable regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("package file exceeds size limit")
	}
	return data, nil
}

func cleanPackagePath(path string) bool {
	return path != "" && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != "." && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

// nixStoreRoot recognizes the only package-root form that can be persisted in
// a state pin. LoadInstalled intentionally does not require it so tests and
// inspection tools can load a package staged outside the store.
func nixStoreRoot(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, "/nix/store/") {
		return false
	}
	name := strings.TrimPrefix(path, "/nix/store/")
	if len(name) < 34 || name[32] != '-' || strings.Contains(name, "/") || strings.ContainsAny(name, "\x00\r\n") {
		return false
	}
	for _, character := range name[:32] {
		if !strings.ContainsRune("0123456789abcdfghijklmnpqrsvwxyz", character) {
			return false
		}
	}
	for _, character := range name[33:] {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return nonempty(name[33:], 4096)
}

func cleanNativeConfigurationPath(path string) bool {
	return cleanPackagePath(path) && strings.HasPrefix(path, nativeConfigurationPathRoot+"/") && strings.HasSuffix(path, ".toml")
}

func identifier(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_') {
			return false
		}
	}
	return true
}

func roleNameIdentifier(value string) bool {
	if value == "team_lead" {
		return true
	}
	if len(value) == 0 || len(value) > 32 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func nativeName(value string) bool {
	return len(value) == 51 && strings.HasPrefix(value, "dw_") && isLowerHex(value[3:], 48)
}
func isDigest(value string) bool { return isLowerHex(value, 64) }
func isLowerHex(value string, length int) bool {
	return len(value) == length && strings.Trim(value, "0123456789abcdef") == ""
}
func nonempty(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value)
}
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func validEffortList(values []string) bool {
	return len(values) > 0 && uniqueStrings(values) && allEfforts(values)
}
func validIdentifierList(values []string) bool {
	if len(values) == 0 || !uniqueStrings(values) {
		return false
	}
	for _, value := range values {
		if !identifier(value) {
			return false
		}
	}
	return true
}
func allEfforts(values []string) bool {
	for _, value := range values {
		if !supportedEfforts[value] {
			return false
		}
	}
	return true
}
func uniqueStrings(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
func sortedUnique(values []string) []string {
	seen := make(map[string]bool)
	for _, value := range values {
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
