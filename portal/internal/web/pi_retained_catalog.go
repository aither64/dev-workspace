package web

import (
	"bytes"
	"context"
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
	start, runtime, err := reader.ReadInitialStart(ctx, intent.SessionRef(), intent.InitialInputID)
	if err != nil || start.Session != intent.SessionRef() || runtime.Session != intent.SessionRef() || start.RequestID != intent.InitialInputID || runtime.RequestID != intent.InitialInputID || start.ReplacesRequestID != "" || start.BindingRevision != 1 || runtime.Generation != journal.NativeGeneration {
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
	if !bytes.Equal(a, b) || s.config.PiCreation.providers == nil {
		return agentbackend.ErrBindingConflict
	}
	return s.config.PiCreation.providers.validatePolicy(intent.ProviderSelection.Policy.Public())
}

func piCatalogCapabilitiesCompatible(a, b piproviders.PackageConfiguration) bool {
	if a.Catalog.Schema != b.Catalog.Schema || !reflect.DeepEqual(a.Catalog.Providers, b.Catalog.Providers) || !reflect.DeepEqual(a.Catalog.Accounts, b.Catalog.Accounts) || !reflect.DeepEqual(a.Catalog.Routes, b.Catalog.Routes) || !reflect.DeepEqual(a.Default, b.Default) || len(a.Catalog.Models) != len(b.Catalog.Models) {
		return false
	}
	for i, original := range a.Catalog.Models {
		current := b.Catalog.Models[i]
		for _, input := range original.Inputs {
			if !slices.Contains(current.Inputs, input) {
				return false
			}
		}
		// Compare owned copies without changing either catalog or its digest.
		original.Inputs, current.Inputs = nil, nil
		original.EvidenceRef, current.EvidenceRef = "", ""
		if !reflect.DeepEqual(original, current) {
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
