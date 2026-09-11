package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aither64/codex-web/codex"
)

type codexLimitsSnapshot struct {
	Windows   []codex.RateLimitWindow `json:"windows"`
	UpdatedAt time.Time               `json:"updatedAt"`
}

type codexLimitsCall struct {
	done   chan struct{}
	result codexLimitsSnapshot
	err    error
}

func (s *Server) codexLimits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := s.loadCodexLimits(r.Context())
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Limits unavailable"})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) loadCodexLimits(requestContext context.Context) (codexLimitsSnapshot, error) {
	s.codexLimitsMu.Lock()
	if !s.codexLimitsCache.UpdatedAt.IsZero() && time.Since(s.codexLimitsCache.UpdatedAt) < 30*time.Second {
		result := s.codexLimitsCache
		s.codexLimitsMu.Unlock()
		return result, nil
	}
	if call := s.codexLimitsWait; call != nil {
		s.codexLimitsMu.Unlock()
		select {
		case <-call.done:
			return call.result, call.err
		case <-requestContext.Done():
			return codexLimitsSnapshot{}, requestContext.Err()
		}
	}
	call := &codexLimitsCall{done: make(chan struct{})}
	s.codexLimitsWait = call
	s.codexLimitsMu.Unlock()

	// A page navigation must not cancel a read shared by other open pages.
	ctx, cancel := context.WithTimeout(s.operationContext, 10*time.Second)
	if s.config.Codex == nil {
		call.err = errors.New("Codex is unavailable")
	} else {
		var limits codex.AccountRateLimits
		limits, call.err = s.config.Codex.ReadAccountRateLimits(ctx)
		if call.err == nil {
			call.result = codexLimitsSnapshot{
				Windows: mainCodexWindows(limits), UpdatedAt: time.Now().UTC(),
			}
		}
	}
	cancel()

	s.codexLimitsMu.Lock()
	if call.err == nil {
		s.codexLimitsCache = call.result
	}
	s.codexLimitsWait = nil
	close(call.done)
	s.codexLimitsMu.Unlock()
	return call.result, call.err
}

func mainCodexWindows(limits codex.AccountRateLimits) []codex.RateLimitWindow {
	snapshot, found := limits.RateLimitsByLimitID["codex"]
	if !found {
		snapshot = limits.RateLimits
		if snapshot.LimitID != "" && snapshot.LimitID != "codex" {
			return []codex.RateLimitWindow{}
		}
	}
	windows := make([]codex.RateLimitWindow, 0, 2)
	for _, duration := range []int64{300, 10080} {
		for _, window := range []*codex.RateLimitWindow{snapshot.Primary, snapshot.Secondary} {
			if window != nil && window.WindowDurationMins != nil && *window.WindowDurationMins == duration {
				windows = append(windows, *window)
				break
			}
		}
	}
	return windows
}
