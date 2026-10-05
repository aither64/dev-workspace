package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// Workspace diagnostics read cached CLI state only. This endpoint must never
// substitute scan (including dry-run), fetch refs or observe live conversations.
func (s *Server) autoArchiveWorkspaceAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use GET."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	transition, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Workspace is busy. Try again shortly."})
		return
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	stdout, _, err := s.runDevSessionWithTransition(ctx, 10*time.Second, transition,
		"auto-archive", "status", "--json")
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Cached archival status is unavailable."})
		return
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result["schema"] != float64(1) || result["workspace"] != s.config.Workspace {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Automatic archival returned an invalid workspace result."})
		return
	}
	if _, ok := result["sessions"].([]any); !ok {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Automatic archival returned an invalid session list."})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// The CLI owns policy and sidecar writes. Passing the existing transition lock
// keeps a hold change ordered with archive and package operations.
func (s *Server) autoArchiveAPI(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use GET or POST."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	mode := unix.LOCK_SH
	if r.Method == http.MethodPost {
		mode = unix.LOCK_EX
	}
	transition, unlock, err := s.acquireTransitionContext(ctx, mode)
	if err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Workspace is busy. Try again shortly."})
		return
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	action := "status"
	confirmedTargetID := ""
	if r.Method == http.MethodPost {
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var body struct {
			Hold     *bool  `json:"hold"`
			TargetID string `json:"targetId"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		if body.Hold == nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Choose whether to keep the session open."})
			return
		}
		var targetErr error
		confirmedTargetID, targetErr = validateLifecycleTarget(summary, body.TargetID, "Keep open")
		if targetErr != nil {
			s.writeLifecycleError(w, targetErr)
			return
		}
		if summary.Archived {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This session is already archived."})
			return
		}
		action = "release"
		if *body.Hold {
			action = "hold"
		}
	}
	if r.Method == http.MethodPost {
		if err := s.revalidateLifecycleTarget(slug, "Keep open", confirmedTargetID); err != nil {
			s.writeLifecycleError(w, err)
			return
		}
	}
	var stdout, stderr string
	if r.Method == http.MethodPost {
		stdout, stderr, err = s.runDevSessionWithTransition(ctx, 10*time.Second, transition,
			"auto-archive", action, slug, "--as-is", "--json")
	} else {
		stdout, stderr, err = s.runDevSessionWithTransition(ctx, 10*time.Second, transition, "auto-archive", action, slug, "--as-is", "--json")
	}
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": commandFailure("update automatic archival", stdout, stderr, err).Error()})
		return
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result["schema"] != float64(1) ||
		result["workspace"] != s.config.Workspace || result["slug"] != slug {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Automatic archival returned an invalid result."})
		return
	}
	result["currentTarget"] = s.lifecycleSnapshot(slug)
	s.writeJSON(w, http.StatusOK, result)
}
