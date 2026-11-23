package web

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"slices"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type piSettingsRuntime interface {
	piproviders.Configurer
	piruntime.WorkflowObserver
	ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
}

func (s *Server) piSettingsAvailable() bool {
	if s.piWorkflows == nil || s.config.PiCreation == nil || s.config.PiCreation.providers == nil {
		return false
	}
	_, ok := s.config.PiCreation.Runtime.(piSettingsRuntime)
	return ok
}

// Catalog reads use the same current conversation authority without creating a
// desired policy or changing the worker. A busy conversation remains inspectable.
func (a *piAdmittedConversation) Models(ctx context.Context, id backend.Identity) (models []backend.Model, err error) {
	if id != piruntime.BackendIdentity(a.handle) || !a.server.piSettingsAvailable() {
		return nil, piruntime.ErrUnavailable
	}
	runtime := a.server.config.PiCreation.Runtime.(piSettingsRuntime)
	catalog, canResample := runtime.(piruntime.CatalogWorkflowObserver)
	// The shared settings POST also calls Models for validation, with its
	// mutation lease. Only the separately admitted catalog GET can resample.
	canResample = canResample && a.lease == nil
	read := func(ctx context.Context, recheck func(context.Context) error) error {
		var observation piruntime.WorkflowObservation
		var err error
		if canResample {
			observation, err = catalog.ObserveCatalogWorkflow(ctx, a.handle, recheck)
		} else {
			observation, err = runtime.ObserveWorkflow(ctx, a.handle)
		}
		if err != nil || observation.Handle != a.handle || !observation.Loaded {
			return errors.Join(piruntime.ErrUnavailable, err)
		}
		p, accounts, provider, err := a.settingsCatalog(ctx, observation.Settings)
		if err != nil {
			return err
		}
		options := []backend.Model{}
		for _, model := range p.configuration.Catalog.Models {
			if model.ProviderID != provider {
				continue
			}
			option := backend.Model{ID: model.ID, Model: model.ID, DisplayName: model.Label, SupportedReasoningEfforts: []backend.ReasoningEffortOption{}}
			for _, thinking := range model.ThinkingLevels {
				target := piproviders.Target{ProviderID: provider, AccountRef: observation.Settings.AccountRef, ModelID: model.ID, Thinking: thinking}
				if a.supportedSettingsTarget(p, accounts, target) == nil {
					option.SupportedReasoningEfforts = append(option.SupportedReasoningEfforts, backend.ReasoningEffortOption{ReasoningEffort: thinking})
				}
			}
			if len(option.SupportedReasoningEfforts) != 0 {
				option.DefaultReasoningEffort = option.SupportedReasoningEfforts[0].ReasoningEffort
				options = append(options, option)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		models = options
		return nil
	}
	if a.lease != nil {
		err = a.lease.run(ctx, "read", "conversation.read", func(ctx context.Context) error { return read(ctx, nil) })
	} else {
		err = a.server.withPiSessionRead(ctx, a.handle.Conversation.Session, func(current piSessionAdmission) error {
			if current.Authority.ValidateHandle(current.Binding, a.handle) != nil {
				return agentbackend.ErrStaleExecution
			}
			if !canResample {
				return read(ctx, nil)
			}
			access, err := a.server.piSavedAccessLocked(current, a.handle.Conversation)
			if err != nil {
				return err
			}
			return read(ctx, func(ctx context.Context) error {
				// The original shared transition and runtime locks remain held.
				if err := a.server.requireCurrentHostProfile(); err != nil {
					return err
				}
				latest, err := a.server.piSessionAdmissionLockedContext(ctx, a.handle.Conversation.Session)
				if err != nil {
					return err
				}
				if latest.Authority != current.Authority || !reflect.DeepEqual(latest.Binding, current.Binding) || latest.Authority.ValidateHandle(latest.Binding, a.handle) != nil {
					return agentbackend.ErrStaleExecution
				}
				now, err := a.server.piSavedAccessLocked(latest, a.handle.Conversation)
				if err != nil || now != access {
					return agentbackend.ErrStaleExecution
				}
				return ctx.Err()
			})
		})
	}
	return
}

func (a *piAdmittedConversation) Modes(ctx context.Context, id backend.Identity) ([]backend.CollaborationMode, error) {
	return []backend.CollaborationMode{}, a.Verify(ctx, id)
}

func (a *piAdmittedConversation) settingsCatalog(ctx context.Context, current piruntime.EffectiveSettings) (*piProviderComposition, []piruntime.AccountBinding, string, error) {
	p := a.server.config.PiCreation.providers
	runtime := a.server.config.PiCreation.Runtime.(piSettingsRuntime)
	start, err := runtime.ReadStart(ctx, a.handle.Conversation.Session)
	if err != nil || start.Session != a.handle.Conversation.Session {
		return nil, nil, "", errors.Join(piruntime.ErrUnavailable, err)
	}
	frozen, err := piproviders.LoadInstalled(start.AppStorePath)
	if err != nil {
		return nil, nil, "", err
	}
	// A newer portal cannot offer choices absent from the CURRENT worker's
	// immutable package. Keep current policy restrictions as the other bound.
	scoped := *p
	scoped.configuration = copyPiPackage(p.configuration)
	scoped.configuration.Catalog.Models = nil
	for _, model := range p.configuration.Catalog.Models {
		for _, native := range frozen.Catalog.Models {
			if model.ID != native.ID || model.ProviderID != native.ProviderID {
				continue
			}
			model.ThinkingLevels = slices.DeleteFunc(slices.Clone(model.ThinkingLevels), func(level string) bool { return !slices.Contains(native.ThinkingLevels, level) })
			if len(model.ThinkingLevels) != 0 {
				scoped.configuration.Catalog.Models = append(scoped.configuration.Catalog.Models, model)
			}
		}
	}
	scoped.configuration.Catalog.Routes = slices.DeleteFunc(scoped.configuration.Catalog.Routes, func(route piproviders.RouteGate) bool {
		return !slices.ContainsFunc(scoped.configuration.Catalog.Models, func(model piproviders.Model) bool {
			return model.ProviderID == route.ProviderID && model.ID == route.ModelID
		})
	})
	p = &scoped
	accounts, err := piBuiltinAccounts(frozen.Catalog)
	if err != nil {
		return nil, nil, "", err
	}
	for _, provider := range p.configuration.Catalog.Providers {
		if provider.NativeID == current.Provider {
			if provider.RuntimeConfigRef != "builtin" {
				return nil, nil, "", piproviders.ErrUnavailableRoute
			}
			for _, binding := range accounts {
				if binding.Provider == current.Provider && binding.AccountRef == current.AccountRef {
					return p, accounts, provider.ID, nil
				}
			}
			return nil, nil, "", piproviders.ErrUnavailableRoute
		}
	}
	return nil, nil, "", piproviders.ErrUnavailableRoute
}

func (a *piAdmittedConversation) supportedSettingsTarget(p *piProviderComposition, accounts []piruntime.AccountBinding, target piproviders.Target) error {
	selection := piproviders.Selection{Chain: []piproviders.Target{target}, FallbackOn: []piproviders.FailureCategory{}}
	policy := piproviders.Policy{Session: a.handle.Conversation.Session, Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: selection}, Overrides: []piproviders.Override{}}
	if err := p.validatePolicy(policy); err != nil {
		return err
	}
	return requirePiSupportedTargets(p.configuration.Catalog, selection.Chain, accounts)
}

func piEffectiveSettings(settings piruntime.EffectiveSettings) backend.Settings {
	return backend.Settings{Model: settings.Model, ModelProvider: settings.Provider, ReasoningEffort: settings.Thinking}
}

func (a *piAdmittedConversation) Update(ctx context.Context, id backend.Identity, input backend.SettingsUpdate) (result backend.Settings, err error) {
	if id != piruntime.BackendIdentity(a.handle) || a.lease == nil || !a.server.piSettingsAvailable() || input.CollaborationMode != nil || input.Model == nil || input.ReasoningEffort == nil {
		return result, piruntime.ErrScope
	}
	err = a.lease.run(ctx, "mutation", "conversation.settings", func(ctx context.Context) error {
		grant, err := workflowGrant(ctx, a.handle.Conversation, "mutation")
		if err != nil || grant.current.Handle != a.handle {
			return agentbackend.ErrStaleExecution
		}
		observed, err := a.server.piWorkflows.freshPolicy(ctx, a.handle, true)
		if err != nil {
			return err
		}
		current := observed.Settings
		if current.Model == *input.Model && current.Thinking == *input.ReasoningEffort {
			result = piEffectiveSettings(current)
			return nil
		}
		if current.Revision == 0 || current.Revision == math.MaxUint64 {
			return piruntime.ErrHeld
		}
		p, accounts, provider, err := a.settingsCatalog(ctx, current)
		if err != nil {
			return err
		}
		target := piproviders.Target{ProviderID: provider, AccountRef: current.AccountRef, ModelID: *input.Model, Thinking: *input.ReasoningEffort}
		if err := a.supportedSettingsTarget(p, accounts, target); err != nil {
			return err
		}
		root, err := piruntime.SessionStateDirectory(a.server.config.UserStateRoot, a.handle.Conversation.Session)
		if err != nil {
			return err
		}
		store := piproviders.Store{Root: root, Catalog: p.configuration.Catalog, Session: a.handle.Conversation.Session}
		policy, err := store.Load()
		if errors.Is(err, os.ErrNotExist) {
			// Only the unchanged immutable initial selection may seed missing
			// desired state. A missing store never repairs unexplained Configure.
			initial := grant.current.intent.ProviderSelection
			matched := false
			for _, member := range initial.Members {
				if member.Address == a.handle.Conversation.Member && member.Incarnation == a.handle.Conversation.Incarnation && member.Settings.Effective() == current {
					matched = true
				}
			}
			policy = initial.Policy.Public()
			if !matched {
				rosters, loadErr := teamruntime.NewStore(a.server.config.UserStateRoot, a.server.config.Workspace)
				if loadErr != nil {
					return loadErr
				}
				roster, loadErr := rosters.LoadPi(a.handle.Conversation.Session.Slug)
				if loadErr != nil {
					return loadErr
				}
				for _, member := range roster.Members {
					if member.Address == a.handle.Conversation.Member && member.Incarnation == a.handle.Conversation.Incarnation && member.Addition != nil && member.Addition.Selection.Settings.Effective() == current {
						// Retain every original default/override; seed only this
						// addition's frozen member selection in the new store.
						for _, override := range member.Addition.Policy.Overrides {
							if override.Member == (piproviders.MemberKey{Address: member.Address, Incarnation: member.Incarnation}) {
								policy.Overrides = append(append([]piproviders.Override{}, policy.Overrides...), override)
								matched = true
							}
						}
					}
				}
			}
			if !matched || p.validatePolicy(policy) != nil {
				return piruntime.ErrHeld
			}
			if err = store.CompareAndSwap(0, policy); err != nil {
				_, readErr := store.Load()
				return errors.Join(err, readErr)
			}
		} else if err != nil {
			return err
		}
		key := piproviders.MemberKey{Address: a.handle.Conversation.Member, Incarnation: a.handle.Conversation.Incarnation}
		resolved, err := piproviders.Resolve(p.configuration.Catalog, policy, key, current.Revision+1)
		if err == nil && resolved.Source == "session_default" && key.Address != "lead" {
			rosters, loadErr := teamruntime.NewStore(a.server.config.UserStateRoot, a.server.config.Workspace)
			if loadErr != nil {
				return loadErr
			}
			roster, loadErr := rosters.LoadPi(a.handle.Conversation.Session.Slug)
			if loadErr != nil {
				return loadErr
			}
			for _, member := range roster.Members {
				if member.Address == key.Address && member.Incarnation == key.Incarnation && member.Addition != nil {
					selected, _ := piMemberDesiredPolicy(policy, key, &member)
					resolved, err = piproviders.Resolve(p.configuration.Catalog, selected, key, current.Revision+1)
				}
			}
		}
		if err != nil || resolved.Settings.Provider != current.Provider || resolved.Settings.AccountRef != current.AccountRef {
			return errors.Join(piproviders.ErrStalePolicy, err)
		}
		selection := piproviders.Selection{Chain: append([]piproviders.Target{}, resolved.Chain...), Budget: resolved.Budget, FallbackOn: append([]piproviders.FailureCategory{}, resolved.FallbackOn...)}
		selection.Chain[0] = target
		next, err := piproviders.WithOverride(policy, key, &selection)
		if err != nil || p.validatePolicy(next) != nil || requirePiSupportedTargets(p.configuration.Catalog, selection.Chain, accounts) != nil {
			return errors.Join(piproviders.ErrInvalidPolicy, err)
		}
		if next.Revision != policy.Revision {
			if err := store.CompareAndSwap(policy.Revision, next); err != nil {
				_, readErr := store.Load()
				return errors.Join(err, readErr)
			}
		}
		// CAS releases its own lock before any native call. Re-resolve the
		// exact saved desired revision; uncertainty never dispatches Configure.
		saved, err := store.Load()
		if err != nil || !reflect.DeepEqual(saved, next) {
			return errors.Join(piproviders.ErrStalePolicy, err)
		}
		resolved, err = piproviders.Resolve(p.configuration.Catalog, saved, key, current.Revision+1)
		if err != nil {
			return err
		}
		applied, err := piproviders.Apply(ctx, a.server.config.PiCreation.Runtime.(piSettingsRuntime), a.handle, current, resolved)
		if err != nil || applied.Outcome != "applied" || applied.Effective != resolved.Settings {
			return errors.Join(piruntime.ErrHeld, err)
		}
		result = piEffectiveSettings(applied.Effective)
		return nil
	})
	return
}

var _ backend.SettingsClient = (*piAdmittedConversation)(nil)

// Explicit shared overrides own later Configure selections. Without one, an
// added member retains its frozen Add policy rather than the shared default.
func piMemberDesiredPolicy(policy piproviders.Policy, key piproviders.MemberKey, member *teamruntime.PiRosterMember) (piproviders.Policy, bool) {
	for _, override := range policy.Overrides {
		if override.Member == key {
			return policy, false
		}
	}
	if member != nil && member.Addition != nil {
		return member.Addition.Policy, true
	}
	return policy, false
}
