package web

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"slices"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piInitialStartReader interface {
	ReadInitialStart(context.Context, agentbackend.SessionRef, string) (piruntime.StartRequest, piruntime.RuntimeRef, error)
}

// Called only after the owning ready journal, intent and binding identity join.
// Original policy digests belong to the original catalog, not today's metadata.
func (s *Server) validatePiReadyCatalog(ctx context.Context, intent session.PiCreationIntent, journal session.PiCreationJournal) error {
	if s.config.PiCreation == nil {
		return piruntime.ErrUnavailable
	}
	if intent.ProviderSelection.CatalogDigest == s.config.PiCreation.Catalog.Digest {
		return intent.Validate(s.config.PiCreation.Catalog)
	}
	reader, ok := s.config.PiCreation.Runtime.(piInitialStartReader)
	if !ok || journal.State != "ready" || journal.Native == nil || !reflect.DeepEqual(journal.PiCreationIntent, intent) {
		return agentbackend.ErrBindingConflict
	}
	start, runtime, err := reader.ReadInitialStart(ctx, intent.SessionRef(), intent.RuntimeRequestID())
	if err != nil || start.Session != intent.SessionRef() || runtime.Session != intent.SessionRef() || start.RequestID != intent.RuntimeRequestID() || runtime.RequestID != intent.RuntimeRequestID() || start.ReplacesRequestID != "" || start.BindingRevision != 1 || runtime.Generation != journal.NativeGeneration {
		return agentbackend.ErrBindingConflict
	}
	if err := validatePiPackageRoot(start.AppStorePath); err != nil {
		return err
	}
	original, err := piproviders.LoadInstalled(start.AppStorePath)
	if err != nil {
		return err
	}
	if err := intent.Validate(original.Catalog); err != nil {
		return err
	}
	current, err := piproviders.LoadInstalled(s.config.PackageRoot)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current.Catalog, s.config.PiCreation.Catalog) || !piCatalogCapabilitiesCompatible(original, current) {
		return agentbackend.ErrBindingConflict
	}
	a, err := piRetainedModels(start.AppStorePath)
	if err != nil {
		return err
	}
	b, err := piRetainedModels(s.config.PackageRoot)
	if err != nil {
		return err
	}
	if !piRetainedModelCapabilitiesCompatible(a, b) || s.config.PiCreation.providers == nil {
		return agentbackend.ErrBindingConflict
	}
	return s.config.PiCreation.providers.validatePolicy(intent.ProviderSelection.Policy.Public())
}

func piCatalogCapabilitiesCompatible(a, b piproviders.PackageConfiguration) bool {
	if a.Catalog.Schema != b.Catalog.Schema || !reflect.DeepEqual(a.Catalog.Providers, b.Catalog.Providers) || !reflect.DeepEqual(a.Catalog.Accounts, b.Catalog.Accounts) || !reflect.DeepEqual(a.Default, b.Default) {
		return false
	}
	for _, route := range a.Catalog.Routes {
		if !slices.ContainsFunc(b.Catalog.Routes, func(current piproviders.RouteGate) bool { return reflect.DeepEqual(route, current) }) {
			return false
		}
	}
	for _, original := range a.Catalog.Models {
		index := slices.IndexFunc(b.Catalog.Models, func(current piproviders.Model) bool {
			return current.ID == original.ID && current.ProviderID == original.ProviderID
		})
		if index < 0 {
			return false
		}
		current := b.Catalog.Models[index]
		for _, input := range original.Inputs {
			if !slices.Contains(current.Inputs, input) {
				return false
			}
		}
		for _, level := range original.ThinkingLevels {
			if !slices.Contains(current.ThinkingLevels, level) {
				return false
			}
		}
		// Compare owned copies without changing either catalog or its digest.
		original.Inputs, current.Inputs = nil, nil
		original.ThinkingLevels, current.ThinkingLevels = nil, nil
		original.EvidenceRef, current.EvidenceRef = "", ""
		if !reflect.DeepEqual(original, current) {
			return false
		}
	}
	return true
}

// Both inputs have already passed the owning strict installed reader. Compare
// existing native semantics, allowing additional physical models and exact overrides.
func piRetainedModelCapabilitiesCompatible(a, b []byte) bool {
	decode := func(data []byte) (map[string]json.RawMessage, []piruntime.InstalledPhysicalModel, error) {
		var config struct {
			Providers map[string]map[string]json.RawMessage `json:"providers"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, nil, err
		}
		provider := config.Providers["openai"]
		var models []piruntime.InstalledPhysicalModel
		if raw, ok := provider["models"]; ok {
			if err := json.Unmarshal(raw, &models); err != nil {
				return nil, nil, err
			}
		}
		delete(provider, "models")
		return provider, models, nil
	}
	x, old, err := decode(a)
	if err != nil {
		return false
	}
	y, added, err := decode(b)
	if err != nil || len(x) != len(y) {
		return false
	}
	for key, raw := range x {
		var left, right any
		if json.Unmarshal(raw, &left) != nil || json.Unmarshal(y[key], &right) != nil {
			return false
		}
		if key == "modelOverrides" {
			// The strict reader validated both maps. Preserve every old override;
			// only new exact model keys can be added inside the existing property.
			oldOverrides, oldOK := left.(map[string]any)
			newOverrides, newOK := right.(map[string]any)
			if !oldOK || !newOK {
				return false
			}
			for id, value := range oldOverrides {
				if !reflect.DeepEqual(value, newOverrides[id]) {
					return false
				}
			}
		} else if !reflect.DeepEqual(left, right) {
			return false
		}
	}
	for _, model := range old {
		if !slices.ContainsFunc(added, func(current piruntime.InstalledPhysicalModel) bool { return reflect.DeepEqual(model, current) }) {
			return false
		}
	}
	return true
}

func piRetainedModels(root string) ([]byte, error) {
	config, err := piruntime.LoadInstalledModelConfiguration(root)
	if err != nil || config.Path == "" {
		return nil, piruntime.ErrScope
	}
	// Match the installed model loader's regular-file and bounded-read rules.
	f, err := os.Open(config.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, piruntime.ErrScope
	}
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, piruntime.ErrScope
	}
	return data, nil
}
