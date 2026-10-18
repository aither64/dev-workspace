package web

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type providerModelsCall struct {
	done   chan struct{}
	models []codex.Model
	err    error
	at     time.Time
}

// Native model/list has no provider argument. Model-only private servers have
// no threads or inference; cached reads never restart the shared App Server.
func (s *Server) modelsForSession(ctx context.Context, slug, selected string) ([]codex.Model, error) {
	catalog, state, err := s.providerState()
	if errors.Is(err, os.ErrNotExist) && selected == "" {
		return s.config.Codex.ListModels(ctx)
	}
	if err != nil {
		return nil, err
	}
	if slug != "" {
		if !session.ValidSlug(slug) {
			return nil, errors.New("invalid model catalog session")
		}
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			return nil, err
		}
		if selected == "" {
			selected = state.Provider(slug, summary.Codex.ThreadID)
		}
	}
	if selected == "" {
		selected = state.Active
	}
	if _, err := catalog.Find(selected); err != nil {
		return nil, err
	}
	key := catalog.Digest + "/" + selected
	s.codexLimitsMu.Lock()
	if s.providerModels == nil {
		s.providerModels = map[string]*providerModelsCall{}
	}
	call := s.providerModels[key]
	if call != nil {
		select {
		case <-call.done:
			if call.err != nil || time.Since(call.at) > 30*time.Second {
				call = nil
			}
		default:
		}
	}
	if call == nil {
		s.operationMu.Lock()
		if s.closing {
			s.operationMu.Unlock()
			s.codexLimitsMu.Unlock()
			return nil, errors.New("portal is stopping")
		}
		s.operationWG.Add(1)
		s.operationMu.Unlock()
		call = &providerModelsCall{done: make(chan struct{})}
		s.providerModels[key] = call
		if len(s.providerModels) > 64 {
			s.providerModels = map[string]*providerModelsCall{key: call}
		}
		go func(call *providerModelsCall, provider string) {
			defer s.operationWG.Done()
			ctx, stop := context.WithTimeout(s.operationContext, 20*time.Second)
			defer stop()
			call.models, call.err = codexproviders.ReadModels(ctx, s.config.HostProfile, s.config.Workspace, s.config.WorkspaceName, provider)
			call.at = time.Now()
			close(call.done)
		}(call, selected)
	}
	s.codexLimitsMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return call.models, call.err
	}
}
