package web

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
	"reflect"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// piProviderComposition retains trusted immutable-package input and its complete
// supported canonical account union. It performs no native/auth operation.
type piProviderComposition struct {
	configuration piproviders.PackageConfiguration
	accounts      []piruntime.AccountBinding
	workspace     string
}

func copyPiSelection(selection piproviders.Selection) piproviders.Selection {
	selection.Chain = append([]piproviders.Target{}, selection.Chain...)
	selection.FallbackOn = append([]piproviders.FailureCategory{}, selection.FallbackOn...)
	return selection
}
func copyPiPackage(configuration piproviders.PackageConfiguration) piproviders.PackageConfiguration {
	catalog := configuration.Catalog
	catalog.Providers = append([]piproviders.Provider{}, catalog.Providers...)
	catalog.Accounts = append([]piproviders.Account{}, catalog.Accounts...)
	catalog.Routes = append([]piproviders.RouteGate{}, catalog.Routes...)
	catalog.Models = append([]piproviders.Model{}, catalog.Models...)
	for i := range catalog.Models {
		catalog.Models[i].ThinkingLevels = append([]string{}, catalog.Models[i].ThinkingLevels...)
		catalog.Models[i].Inputs = append([]string{}, catalog.Models[i].Inputs...)
	}
	// Catalog.Digest remains the loader's exact raw nested byte hash. Cloning
	// typed values neither computes another digest nor rewrites its input bytes.
	return piproviders.PackageConfiguration{Catalog: catalog, Default: copyPiSelection(configuration.Default)}
}
func piBuiltinAccounts(catalog piproviders.Catalog) ([]piruntime.AccountBinding, error) {
	bindings, err := piproviders.SelectableRuntimeAccounts(catalog, piruntime.CanonicalCredentialRef)
	if err != nil {
		return nil, err
	}
	supported := map[string]bool{}
	for _, provider := range catalog.Providers {
		if provider.RuntimeConfigRef == "builtin" {
			supported[provider.NativeID] = true
		}
	}
	result := []piruntime.AccountBinding{}
	for _, binding := range bindings {
		if supported[binding.Provider] {
			result = append(result, binding)
		}
	}
	if len(result) == 0 {
		return nil, piproviders.ErrUnavailableRoute
	}
	return result, nil
}
func requirePiSupportedTargets(catalog piproviders.Catalog, targets []piproviders.Target, accounts []piruntime.AccountBinding) error {
	supported := map[string]bool{}
	for _, provider := range catalog.Providers {
		supported[provider.ID] = provider.RuntimeConfigRef == "builtin"
	}
	for _, target := range targets {
		if !supported[target.ProviderID] {
			return fmt.Errorf("Pi provider runtime configuration is unsupported: %w", piproviders.ErrUnavailableRoute)
		}
	}
	projected, err := piproviders.ProjectRuntimeAccounts(catalog, targets, piruntime.CanonicalCredentialRef)
	if err != nil {
		return err
	}
	current := map[string]string{}
	for _, binding := range accounts {
		current[binding.Provider] = binding.AccountRef
	}
	for _, binding := range projected {
		if current[binding.Provider] != binding.AccountRef {
			return piproviders.ErrUnavailableRoute
		}
	}
	return nil
}
func newPiProviderComposition(configuration piproviders.PackageConfiguration, workspace string) (*piProviderComposition, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	frozen := copyPiPackage(configuration)
	accounts, err := piBuiltinAccounts(frozen.Catalog)
	if err != nil {
		return nil, err
	}
	if err = requirePiSupportedTargets(frozen.Catalog, frozen.Default.Chain, accounts); err != nil {
		return nil, err
	}
	return &piProviderComposition{configuration: frozen, accounts: accounts, workspace: workspace}, nil
}

// ValidatePiProviderConfiguration is the package-install check shared with
// serve composition. It needs neither a fabricated session nor runtime/auth.
func ValidatePiProviderConfiguration(configuration piproviders.PackageConfiguration) error {
	_, err := newPiProviderComposition(configuration, "")
	return err
}
func explicitPiPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

// newPiSelectionServices freezes pure provider inputs without a runtime.
func newPiSelectionServices(config Config) (*PiCreationServices, error) {
	if config.PiProviderConfig == nil {
		return nil, nil
	}
	composition, err := newPiProviderComposition(*config.PiProviderConfig, config.Workspace)
	if err != nil {
		return nil, err
	}
	if !explicitPiPath(config.Workspace) || !explicitPiPath(config.UserStateRoot) || !explicitPiPath(config.PiAuthDirectory) || !explicitPiPath(config.PiHostStateRoot) {
		return nil, fmt.Errorf("explicit Pi state, auth and host paths are required: %w", piruntime.ErrUnavailable)
	}
	services := &PiCreationServices{Catalog: copyPiPackage(composition.configuration).Catalog,
		Selection: composition.initialPolicy, AccountBindings: append([]piruntime.AccountBinding{}, composition.accounts...),
		UserStateRoot: config.UserStateRoot, HostStateRoot: config.PiHostStateRoot, AuthDirectory: config.PiAuthDirectory,
		providers: composition}
	return services, nil
}

// piManagerConfiguration deliberately has no production Host, Dial or helper
// override. NewManager resolves the installed host helper and Unix transport.
func piManagerConfiguration(config Config, accounts []piruntime.AccountBinding) piruntime.Config {
	return piruntime.Config{UserStateRoot: config.UserStateRoot, HostStateRoot: config.PiHostStateRoot,
		AuthDirectory: config.PiAuthDirectory, AccountBindings: append([]piruntime.AccountBinding{}, accounts...)}
}

// NewPiCreationServices constructs the real Manager without Ensure, SDK loading
// or native launch. Failed Pi configuration leaves the legacy server available.
func NewPiCreationServices(config Config) (*PiCreationServices, error) {
	services, err := newPiSelectionServices(config)
	if err != nil || services == nil {
		return services, err
	}
	profile, err := readHostProfileIdentity(config.HostProfile)
	if err != nil {
		return nil, err
	}
	return newPiCreationManager(config, profile, services)
}
func newPiCreationManager(config Config, profile hostProfileIdentity, services *PiCreationServices) (*PiCreationServices, error) {
	if err := validatePiPackage(config, profile); err != nil {
		return nil, err
	}
	manager, err := piruntime.NewManager(piManagerConfiguration(config, services.AccountBindings))
	if err != nil {
		return nil, err
	}
	services.Runtime = manager
	services.StartRequest = func(intent session.PiCreationIntent, revision uint64) (piruntime.StartRequest, error) {
		// Called under the existing creation admission, after transition/creation/
		// runtime waits. Recheck the captured profile and static package each time.
		if err := validatePiPackage(config, profile); err != nil {
			return piruntime.StartRequest{}, err
		}
		if err := intent.Validate(services.Catalog); err != nil || intent.Workspace != config.Workspace {
			return piruntime.StartRequest{}, agentbackend.ErrBindingConflict
		}
		binding, err := (session.BackendBindingStore{Root: config.UserStateRoot, Workspace: config.Workspace}).Load(intent.Slug)
		if err != nil {
			return piruntime.StartRequest{}, err
		}
		if binding.Revision != revision || intent.ValidateBinding(binding) != nil {
			return piruntime.StartRequest{}, agentbackend.ErrBindingConflict
		}
		journal, err := readPiCreationJournal(piJournalPath(intent.Workspace, intent.Slug))
		if err != nil {
			return piruntime.StartRequest{}, err
		}
		authority, authorityErr := session.LoadPiRuntimeAuthority(config.AuthorityDir, intent.Slug, intent.Workspace)
		if authorityErr != nil && !errors.Is(authorityErr, agentbackend.ErrNotBound) {
			return piruntime.StartRequest{}, authorityErr
		}
		if authorityErr == nil && (authority.SessionIncarnation != intent.SessionIncarnation || authority.BindingRevision > binding.Revision || (journal.NativeGeneration != "" && authority.Generation != journal.NativeGeneration)) {
			return piruntime.StartRequest{}, agentbackend.ErrBindingConflict
		}
		saved, readErr := manager.ReadStart(context.Background(), intent.SessionRef())
		return piOriginalStart(config, profile, intent, binding, journal, authorityErr == nil, saved, readErr)
	}
	return services, nil
}

func piProfileToken(profile hostProfileIdentity) string {
	data, _ := json.Marshal([]any{1, profile.device, profile.inode, profile.ctimeSec,
		profile.ctimeNSec, profile.mtimeSec, profile.mtimeNSec, profile.target})
	digest := sha256.Sum256(data)
	return "pi-profile-v1:" + hex.EncodeToString(digest[:])
}

func validatePiPackage(config Config, profile hostProfileIdentity) error {
	current, err := readHostProfileIdentity(config.HostProfile)
	if err != nil || current != profile {
		return fmt.Errorf("Pi workspace profile changed: %w", piruntime.ErrHeld)
	}
	root, err := filepath.EvalSymlinks(config.PackageRoot)
	if err != nil || root != config.PackageRoot {
		return fmt.Errorf("Pi package root must be resolved: %w", piruntime.ErrUnavailable)
	}
	selected, err := filepath.EvalSymlinks(config.HostProfile)
	if err != nil || selected != root {
		return fmt.Errorf("Pi profile does not select the admitted package: %w", piruntime.ErrHeld)
	}
	// Reuse the existing package/catalog contract; the registration marker path
	// and co-location are admitted by NewContext, not a new Pi marker codec.
	if _, err := agentteams.LoadInstalled(root); err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(root, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return fmt.Errorf("Pi runtime contract is unavailable: %w", piruntime.ErrUnavailable)
	}
	if err := rejectDuplicateCreationJSONKeys(data); err != nil {
		return err
	}
	var contract map[string]json.RawMessage
	if err := json.Unmarshal(data, &contract); err != nil || contract == nil {
		return piruntime.ErrUnavailable
	}
	for field, expected := range map[string]int{"sessionBackendBindingSchema": 1, "sessionBackendBindingPolicy": 1, "runtimeAuthoritySchema": 2, "piCreationSchema": 4} {
		var actual int
		if json.Unmarshal(contract[field], &actual) != nil || actual != expected {
			return fmt.Errorf("Pi runtime contract lacks %s: %w", field, piruntime.ErrUnavailable)
		}
	}
	return piruntime.ValidatePackage(root)
}

// piOriginalStart accepts only the exact saved initial launch. Its historical
// binding revision never becomes the current authority revision.
func piOriginalStart(config Config, profile hostProfileIdentity, intent session.PiCreationIntent, binding session.BackendBinding, journal session.PiCreationJournal, authorityPresent bool, saved piruntime.StartRequest, readErr error) (piruntime.StartRequest, error) {
	if intent.ValidateBinding(binding) != nil || !reflect.DeepEqual(journal.PiCreationIntent, intent) {
		return piruntime.StartRequest{}, agentbackend.ErrBindingConflict
	}
	expected := piruntime.StartRequest{Session: intent.SessionRef(), BindingRevision: 1,
		RequestID: intent.InitialInputID, AppStorePath: config.PackageRoot,
		ProfilePath: config.HostProfile, ProfileGeneration: piProfileToken(profile)}
	if readErr == nil {
		if saved != expected || saved.BindingRevision > binding.Revision {
			return piruntime.StartRequest{}, piruntime.ErrConflict
		}
		return saved, nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return piruntime.StartRequest{}, readErr
	}
	if !reflect.DeepEqual(binding, intent.Binding()) || journal.State != "creating" || journal.NativeCreateStarted || journal.NativeGeneration != "" || journal.Native != nil || authorityPresent {
		return piruntime.StartRequest{}, piruntime.ErrHeld
	}
	return expected, nil
}

func (p *piProviderComposition) validatePolicy(policy piproviders.Policy) error {
	if policy.Session.Workspace != p.workspace {
		return piproviders.ErrInvalidPolicy
	}
	if err := piproviders.ValidatePolicy(p.configuration.Catalog, policy); err != nil {
		return err
	}
	if err := requirePiSupportedTargets(p.configuration.Catalog, policy.Default.Selection.Chain, p.accounts); err != nil {
		return err
	}
	for _, override := range policy.Overrides {
		if err := requirePiSupportedTargets(p.configuration.Catalog, override.Value.Selection.Chain, p.accounts); err != nil {
			return err
		}
	}
	return nil
}
func (p *piProviderComposition) initialPolicy(ctx context.Context, ref agentbackend.SessionRef, conversations []session.PiReservedConversation, preset teamruntime.Preset) (piproviders.Policy, error) {
	if err := ctx.Err(); err != nil {
		return piproviders.Policy{}, err
	}
	if ref.Workspace != p.workspace || validDirectTeamPreset(preset) != nil || len(conversations) != len(preset.Members)+1 || len(conversations) == 0 || conversations[0].Address != "lead" {
		return piproviders.Policy{}, piproviders.ErrInvalidPolicy
	}
	policy := piproviders.Policy{Session: ref, Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: copyPiSelection(p.configuration.Default)}, Overrides: []piproviders.Override{}}
	incarnations := map[string]bool{}
	for index, conversation := range conversations {
		model, thinking := preset.LeadModel, preset.LeadEffort
		if index > 0 {
			member := preset.Members[index-1]
			if member.Address != conversation.Address {
				return piproviders.Policy{}, piproviders.ErrInvalidPolicy
			}
			model, thinking = member.Model, member.Effort
		}
		if !messageDigestPattern.MatchString(conversation.Incarnation) || incarnations[conversation.Incarnation] {
			return piproviders.Policy{}, piproviders.ErrInvalidPolicy
		}
		incarnations[conversation.Incarnation] = true
		selection := copyPiSelection(p.configuration.Default)
		// Only the primary's exact saved model/thinking changes. Provider/account,
		// complete fallback order, categories and budget retain package intent.
		selection.Chain[0].ModelID = model
		selection.Chain[0].Thinking = thinking
		policy.Overrides = append(policy.Overrides, piproviders.Override{Member: piproviders.MemberKey{Address: conversation.Address, Incarnation: conversation.Incarnation}, Value: piproviders.VersionedSelection{Revision: 1, Selection: selection}})
	}
	if err := p.validatePolicy(policy); err != nil {
		return piproviders.Policy{}, err
	}
	for _, conversation := range conversations {
		if _, err := piproviders.Resolve(p.configuration.Catalog, policy, piproviders.MemberKey{Address: conversation.Address, Incarnation: conversation.Incarnation}, 1); err != nil {
			return piproviders.Policy{}, err
		}
	}
	return policy, nil
}

// publicSelection exposes only an exact policy-resolved target after complete
// support/map checks. Raw Catalog.Public route labels cannot authorize it.
// Effective/physical observations remain unknown; no operation is registered.
func (p *piProviderComposition) publicSelection(policy piproviders.Policy, member piproviders.MemberKey) (piproviders.SettingsView, error) {
	if err := p.validatePolicy(policy); err != nil {
		return piproviders.SettingsView{}, err
	}
	reserved := false
	for _, override := range policy.Overrides {
		if override.Member == member {
			reserved = true
			break
		}
	}
	if !reserved {
		return piproviders.SettingsView{}, piproviders.ErrInvalidPolicy
	}
	resolved, err := piproviders.Resolve(p.configuration.Catalog, policy, member, 1)
	if err != nil {
		return piproviders.SettingsView{}, err
	}
	return resolved.View(nil, nil), nil
}
func (s *Server) piCreationAvailability() error {
	if s.config.PiProviderConfigError != nil {
		return fmt.Errorf("Pi provider configuration refused: %w", s.config.PiProviderConfigError)
	}
	if !s.config.PiCreation.available() {
		return piruntime.ErrUnavailable
	}
	return nil
}
func configurePiCreation(config *Config) {
	if config.PiProviderConfigError == nil && config.PiProviderConfig != nil {
		frozen := copyPiPackage(*config.PiProviderConfig)
		config.PiProviderConfig = &frozen
		services, err := newPiSelectionServices(*config)
		if err != nil {
			config.PiProviderConfigError = err
		} else {
			config.PiCreation = services
		}
	}
	if config.PiProviderConfigError != nil {
		config.PiProviderConfig = nil
		config.PiCreation = nil
		config.Logger.Printf("Pi provider configuration refused: %v", config.PiProviderConfigError)
	}
}
