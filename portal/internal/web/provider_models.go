package web

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type providerModelsCall struct {
	done     chan struct{}
	models   []codex.Model
	err      error
	at       time.Time
	producer string
}

func readCorrelation(ctx context.Context) string {
	if trace, _ := ctx.Value(readTraceKey{}).(*readTrace); trace != nil {
		return trace.correlation
	}
	return "unmeasured"
}

func catalogOutcome(err error) (string, int) {
	if err == nil {
		return "ok", 0
	}
	var diagnostic *codexproviders.ModelsReadError
	if errors.As(err, &diagnostic) {
		return diagnostic.Diagnostic()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline", -1
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", -1
	}
	return "unavailable", -1
}

// Unlike the sampled HTTP record, every shared producer gets one completion
// record, even when its HTTP waiter has already departed.
func (s *Server) catalogRead(correlation, producer, stage, outcome string, elapsed time.Duration, exitCode int) {
	switch stage {
	case "runtime", "catalog", "shared_native", "wait", "helper":
	default:
		return
	}
	switch outcome {
	case "ok", "unavailable", "resolution", "identity", "deadline", "canceled", "start", "exit", "output_limit", "invalid", "unknown":
	default:
		return
	}
	code := "unknown"
	for _, id := range []*string{&correlation, &producer} {
		if *id != "unmeasured" && (len(*id) != 17 || (*id)[0] != 'r' || strings.Trim((*id)[1:], "0123456789abcdef") != "") {
			*id = "unmeasured"
		}
	}
	if exitCode >= 0 {
		code = strconv.Itoa(exitCode)
	}
	s.config.Logger.Printf("portal_models_read correlation=%s producer=%s stage=%s outcome=%s elapsed_ms=%.2f exit_code=%s",
		correlation, producer, stage, outcome, float64(elapsed)/float64(time.Millisecond), code)
}

// Native model/list has no provider argument. Model-only private servers have
// no threads or inference; cached reads never restart the shared App Server.
func (s *Server) modelsForSession(ctx context.Context, slug, selected string) ([]codex.Model, error) {
	started := time.Now()
	correlation := readCorrelation(ctx)
	resolutionError := func(err error) ([]codex.Model, error) {
		s.catalogRead(correlation, "unmeasured", "catalog", "resolution", time.Since(started), -1)
		return nil, err
	}
	catalog, state, err := s.providerState()
	if errors.Is(err, os.ErrNotExist) && selected == "" {
		models, err := s.config.Codex.ListModels(ctx)
		outcome, _ := catalogOutcome(err)
		s.catalogRead(correlation, "unmeasured", "shared_native", outcome, time.Since(started), -1)
		return models, err
	}
	if err != nil {
		return resolutionError(err)
	}
	if slug != "" {
		if !session.ValidSlug(slug) {
			return resolutionError(errors.New("invalid model catalog session"))
		}
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			return resolutionError(err)
		}
		if selected == "" {
			selected = state.Provider(slug, summary.Codex.ThreadID)
		}
	}
	if selected == "" {
		selected = state.Active
	}
	if _, err := catalog.Find(selected); err != nil {
		return resolutionError(err)
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
			s.catalogRead(correlation, "unmeasured", "runtime", "unavailable", time.Since(started), -1)
			return nil, errors.New("portal is stopping")
		}
		s.operationWG.Add(1)
		s.operationMu.Unlock()
		call = &providerModelsCall{done: make(chan struct{}), producer: correlation}
		s.providerModels[key] = call
		if len(s.providerModels) > 64 {
			s.providerModels = map[string]*providerModelsCall{key: call}
		}
		go func(call *providerModelsCall, provider string) {
			defer s.operationWG.Done()
			ctx, stop := context.WithTimeout(s.operationContext, 20*time.Second)
			defer stop()
			started := time.Now()
			call.models, call.err = codexproviders.ReadModels(ctx, s.config.HostProfile, s.config.Workspace, s.config.WorkspaceName, provider)
			call.at = time.Now()
			close(call.done)
			outcome, exitCode := catalogOutcome(call.err)
			s.catalogRead(call.producer, call.producer, "helper", outcome, time.Since(started), exitCode)
		}(call, selected)
	}
	s.codexLimitsMu.Unlock()
	select {
	case <-ctx.Done():
		outcome, exitCode := catalogOutcome(ctx.Err())
		s.catalogRead(correlation, call.producer, "wait", outcome, time.Since(started), exitCode)
		return nil, ctx.Err()
	case <-call.done:
		if call.err != nil {
			outcome, exitCode := catalogOutcome(call.err)
			s.catalogRead(correlation, call.producer, "wait", outcome, time.Since(started), exitCode)
		}
		return call.models, call.err
	}
}
