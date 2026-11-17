//go:build pi_native_fixture

package web

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// BindPiRetainedCatalogNativeFixture installs the real current policy validator
// while retaining the explicitly supplied fake-host Manager execution boundary.
func (s *Server) BindPiRetainedCatalogNativeFixture() error {
	configuration, err := piproviders.LoadInstalled(s.config.PackageRoot)
	if err != nil {
		return err
	}
	if s.config.PiCreation == nil || configuration.Catalog.Digest != s.config.PiCreation.Catalog.Digest {
		return piruntime.ErrScope
	}
	composition, err := newPiProviderComposition(configuration, s.config.Workspace)
	if err != nil {
		return err
	}
	s.config.PiCreation.providers = composition
	return nil
}

func (s *Server) ProvePiRetainedCreationNativeFixture(ctx context.Context, intent session.PiCreationIntent) error {
	_, _, _, err := s.proveRetainedPiRootLocked(ctx, intent.Slug, intent.ReceiptID)
	return err
}

// RetainPiPreparationV2NativeFixture completes the existing retained-ready
// fixture's old preparation mapping with the owning version-2 digest codec.
// It is controlled pre-root evidence, not a claim of public naming execution.
func (s *Server) RetainPiPreparationV2NativeFixture(intent session.PiCreationIntent, goal string) (string, error) {
	if intent.Workspace != s.config.Workspace || len(intent.Slug) < 12 {
		return "", piruntime.ErrScope
	}
	input := preparationInput{Backend: "pi", RawPrompt: goal, Date: intent.Slug[:10], CustomName: strings.TrimPrefix(intent.Slug, intent.Slug[:11]),
		Attachments: []string{}, Team: preparationValue{true, intent.DirectTeam.ID}, Catalog: preparationValue{true, intent.DirectTeam.CatalogDigest},
		Model: preparationValue{true, intent.DirectTeam.LeadModel}, Effort: preparationValue{true, intent.DirectTeam.LeadEffort}}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := sessionPreparation{Schema: 2, InputVersion: 2, Workspace: intent.Workspace, RequestID: intent.InitialInputID,
		InputDigest: versionedPreparationDigest(input, 2), ReceiptID: intent.ReceiptID, Attempt: 1, State: "terminal", Phase: "initializing",
		StartedAt: now, UpdatedAt: now, Slug: intent.Slug, Epoch: intent.DeletionHistorySHA256, Terminal: "ready"}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := s.savePreparation(record); err != nil {
		return "", err
	}
	return s.preparationPath(record.RequestID, true), nil
}

// BindPiWorkflowNativeFixture supplies only the typed installed-catalog boundary
// for the finite external SDK fixture. NewContext still constructs the real
// routes, admission and workflows from its public, same-Manager PiCreation input.
// Call before publishing fixture sessions, exposing the server or queuing demand.
// Production builds have no such method; raw LoadInstalled has separate evidence.
func (s *Server) BindPiWorkflowNativeFixture(ctx context.Context, installed agentteams.Installed, runtime piruntime.RuntimeRef, attach bool) error {
	if !installed.Managed || installed.Catalog == nil || installed.Catalog.Validate() != nil {
		return errors.New("native fixture requires a validated typed catalog")
	}
	data, err := json.Marshal(installed.Catalog)
	if err != nil {
		return err
	}
	var catalog agentteams.Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return err
	}
	if err := catalog.Validate(); err != nil {
		return err
	}
	detached := installed
	detached.Catalog = &catalog
	bind := func() error {
		s.operationMu.Lock()
		defer s.operationMu.Unlock()
		if s.closing || len(s.creations) != 0 || s.config.Codex != nil {
			return errors.New("native fixture catalog must bind before startup work or publication")
		}
		// The display worker is already running after NewContext. It captures its
		// optional workflow pointer with this same mutex. An old captured pointer has
		// no demand/attachment to execute, verified below under its short map mutex.
		s.displayRefreshMu.Lock()
		defer s.displayRefreshMu.Unlock()
		if old := s.piWorkflows; old != nil {
			old.observations.mu.Lock()
			active := len(old.observations.entries) != 0 || len(old.observations.counters) != 0 || len(old.observations.requested) != 0 || len(old.observations.attachments) != 0 || len(old.observations.listeners) != 0
			old.observations.mu.Unlock()
			if active {
				return errors.New("native fixture workflow already has work")
			}
		}
		previous := s.installedTeams
		s.installedTeams = &detached
		workflows := newPiWorkflowComposition(s)
		if workflows == nil {
			s.installedTeams = previous
			return errors.New("native fixture workflow ports or package guards are unavailable")
		}
		s.piWorkflows = workflows
		return nil
	}
	if err := bind(); err != nil {
		return err
	}
	reader, ok := s.config.PiCreation.Runtime.(piruntime.CurrentRuntimeReader)
	if !ok || runtime.Session.Workspace != s.config.Workspace {
		return piruntime.ErrScope
	}
	current, err := reader.ReadCurrentRuntime(ctx, runtime.Session)
	if err != nil || current != runtime {
		return errors.Join(piruntime.ErrScope, err)
	}
	if !attach {
		return nil
	}
	// No binder mutex survives the actual attachment/initial ACK wait. The
	// fixture caller owns its real disposable root runtime EX through Send ACK.
	return s.piWorkflows.ensureAttachment(ctx, runtime)
}
