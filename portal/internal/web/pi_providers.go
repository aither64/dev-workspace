package web

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
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

// NewPiCreationServices freezes production catalog/selection inputs. Runtime and
// StartRequest are retained only from separately admitted existing ports; nil
// remains unavailable. Neither interface possession nor this constructor proves
// matching native configuration or permits launch/dispatch.
func NewPiCreationServices(config Config) (*PiCreationServices, error) {
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
	if config.PiCreation != nil {
		services.Runtime = config.PiCreation.Runtime
		services.StartRequest = config.PiCreation.StartRequest
	}
	return services, nil
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
		services, err := NewPiCreationServices(*config)
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
