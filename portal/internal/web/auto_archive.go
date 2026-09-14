package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

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
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	action := "status"
	if r.Method == http.MethodPost {
		if summary.Archived {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This session is already archived."})
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
		if _, err := validateLifecycleTarget(summary, body.TargetID, "archive"); err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		action = "release"
		if *body.Hold {
			action = "hold"
		}
	}
	var stdout, stderr string
	if r.Method == http.MethodPost {
		stdout, stderr, err = s.runDevSessionWithTransition(ctx, 10*time.Second, transition,
			"auto-archive", action, slug, "--as-is", "--json")
	} else {
		stdout, stderr, err = s.runDevSession(ctx, 10*time.Second, "auto-archive", action, slug, "--as-is", "--json")
	}
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": commandFailure("update automatic archival", stdout, stderr, err).Error()})
		return
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result["slug"] != slug {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Automatic archival returned an invalid result."})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
