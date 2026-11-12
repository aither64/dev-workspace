//go:build pi_native_fixture

package web

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

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
