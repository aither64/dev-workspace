package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/aither64/codex-web/codex"
)

type codexLimitsSnapshot struct {
	Windows      []codex.RateLimitWindow      `json:"windows"`
	UpdatedAt    time.Time                    `json:"updatedAt"`
	Credits      *codex.CreditsSnapshot       `json:"credits,omitempty"`
	ResetCredits *codex.RateLimitResetCredits `json:"rateLimitResetCredits,omitempty"`
	AccountScope string                       `json:"accountScope,omitempty"`
	CanReset     bool                         `json:"canReset"`
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
	generation := s.codexLimitsGeneration
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
			call.result = s.codexLimitsSnapshot(limits)
		}
	}
	cancel()

	s.codexLimitsMu.Lock()
	if call.err == nil && generation == s.codexLimitsGeneration {
		s.codexLimitsCache = call.result
	}
	if s.codexLimitsWait == call {
		s.codexLimitsWait = nil
	}
	close(call.done)
	s.codexLimitsMu.Unlock()
	return call.result, call.err
}

type resetCreditConsumer interface {
	ConsumeRateLimitResetCredit(context.Context, string, string) (codex.ResetCreditResult, error)
}

func accountLimitsScope(limits codex.AccountRateLimits) string {
	if limits.AccountID == nil || *limits.AccountID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(*limits.AccountID))
	return hex.EncodeToString(digest[:])
}

func (s *Server) codexLimitsSnapshot(limits codex.AccountRateLimits) codexLimitsSnapshot {
	_, supported := s.config.Codex.(resetCreditConsumer)
	scope := accountLimitsScope(limits)
	return codexLimitsSnapshot{Windows: mainCodexWindows(limits), UpdatedAt: time.Now().UTC(),
		Credits: mainCodexBucket(limits).Credits, ResetCredits: limits.ResetCredits,
		AccountScope: scope, CanReset: supported && scope != ""}
}

func (s *Server) invalidateCodexLimits() {
	s.codexLimitsMu.Lock()
	s.codexLimitsGeneration++
	s.codexLimitsCache = codexLimitsSnapshot{}
	// A read that began before redemption must not become the post-reset read.
	s.codexLimitsWait = nil
	s.codexLimitsMu.Unlock()
}

func (s *Server) consumeCodexReset(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		IdempotencyKey string `json:"idempotencyKey"`
		CreditID       string `json:"creditId"`
		AccountScope   string `json:"accountScope"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if !queueClientMessageIDPattern.MatchString(body.IdempotencyKey) ||
		!messageDigestPattern.MatchString(body.AccountScope) || len(body.CreditID) > 512 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid reset attempt"})
		return
	}
	consumer, ok := s.config.Codex.(resetCreditConsumer)
	if !ok {
		s.writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "Reset credits are unavailable"})
		return
	}
	s.codexResetMu.Lock()
	defer s.codexResetMu.Unlock()
	ctx, cancel := context.WithTimeout(s.operationContext, 35*time.Second)
	defer cancel()
	// Check the live account before every attempt, including a retained retry.
	limits, err := s.config.Codex.ReadAccountRateLimits(ctx)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The current account could not be verified"})
		return
	}
	if accountLimitsScope(limits) != body.AccountScope {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The Codex account changed. The saved reset attempt was not sent."})
		return
	}
	result, err := consumer.ConsumeRateLimitResetCredit(ctx, body.IdempotencyKey, body.CreditID)
	// Even a lost response may have consumed the credit. Retire pre-attempt reads.
	s.invalidateCodexLimits()
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The reset outcome could not be confirmed. Retry the same attempt."})
		return
	}
	switch result.Outcome {
	case "reset", "alreadyRedeemed", "nothingToReset", "noCredit":
		s.writeJSON(w, http.StatusOK, result)
	default:
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The reset outcome could not be confirmed. Retry the same attempt."})
	}
}

func mainCodexBucket(limits codex.AccountRateLimits) codex.RateLimitSnapshot {
	snapshot, found := limits.RateLimitsByLimitID["codex"]
	if !found {
		snapshot = limits.RateLimits
		if snapshot.LimitID != "" && snapshot.LimitID != "codex" {
			return codex.RateLimitSnapshot{}
		}
	}
	return snapshot
}

func mainCodexWindows(limits codex.AccountRateLimits) []codex.RateLimitWindow {
	snapshot := mainCodexBucket(limits)
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
