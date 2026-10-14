package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Notice preferences are separate from schema-1 execution permission, which
// preceding package generations still read and write strictly.
type recoveryNotice struct {
	Identity  string `json:"identity"`
	Epoch     string `json:"epoch"`
	ID        string `json:"id"`
	Dismissed bool   `json:"dismissed"`
}

func (s *Server) recoveryNotice(summary *session.Summary) (recoveryNotice, error) {
	identity, err := session.ObservationIdentity(s.config.Workspace, summary.Slug,
		filepath.Join(s.config.Workspace, summary.Root, summary.Slug), summary.Codex.ThreadID)
	if err != nil {
		return recoveryNotice{}, err
	}
	epoch, err := session.SocketIdentity(s.config.CodexSocket)
	if err != nil {
		return recoveryNotice{}, err
	}
	authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, summary.Slug, s.config.Workspace)
	if err != nil {
		return recoveryNotice{}, err
	}
	if authority.State != "ready" || authority.CodexThreadID != summary.Codex.ThreadID || authority.CodexSocketPath != s.config.CodexSocket {
		return recoveryNotice{}, errors.New("restored runtime identity changed")
	}
	// Existing waiting sessions need no migration or read-side writes.
	// The canonical helper assigns a new terminal identity on restoration, even
	// when the native socket survives. Portal-only restarts retain that identity.
	digest := sha256.Sum256([]byte(identity + ":" + epoch + ":" + authority.TmuxIdentity))
	baseline := recoveryNotice{Identity: identity, Epoch: epoch, ID: hex.EncodeToString(digest[:])}
	var saved recoveryNotice
	if err := s.loadThreadRecord("recovery-notice", summary.Codex.ThreadID, &saved); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.config.Logger.Printf("read recovery notice for %s: %v", summary.Slug, err)
		}
		return baseline, nil // Unreadable preferences never suppress a notice.
	}
	if saved.Identity != identity || saved.Epoch != epoch || saved.ID != baseline.ID {
		return baseline, nil
	}
	return saved, nil
}

func (s *Server) waitingRecoveryStatus(summary *session.Summary) recoveryStatus {
	status := recoveryStatus{State: "waiting"}
	notice, err := s.recoveryNotice(summary)
	if err == nil {
		status.NoticeID, status.NoticeDismissed = notice.ID, notice.Dismissed
	}
	return status
}

func (s *Server) selectedRecoveryStatus(summary *session.Summary, address string) (recoveryStatus, error) {
	status := s.recoveryStatus(summary)
	if address == "" || !summary.Interactive || status.State == "recovering" || status.State == "failed" {
		return status, nil
	}
	roster, err := s.loadTeamRoster(summary)
	if err != nil {
		return status, err
	}
	if roster != nil {
		for _, member := range roster.Members {
			if member.Address == address && member.State == "ready" && member.RetireIntent == "" {
				if s.recoveryHeld(summary.Slug, member.Thread) {
					return s.waitingRecoveryStatus(summary), nil
				}
				return recoveryStatus{State: "active"}, nil
			}
		}
	}
	return status, errors.New("The selected team member is unavailable.")
}

func (s *Server) dismissRecoveryNotice(w http.ResponseWriter, r *http.Request, initial *session.Summary, noticeID, address string) {
	lock := s.messageLock("recovery:" + initial.Slug)
	if err := lock.Lock(r.Context()); err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer lock.Unlock()
	summary, err := session.Find(s.config.Workspace, initial.Slug)
	if err != nil || summary.Codex.ThreadID != initial.Codex.ThreadID {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Session identity changed. Reload the session."})
		return
	}
	owner, err := session.PendingLifecycle(s.config.Workspace, summary.Slug)
	if err != nil || owner != "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Finish session recovery before dismissing this notice."})
		return
	}
	s.normalizeInteractivity(r.Context(), summary)
	status, err := s.selectedRecoveryStatus(summary, address)
	if err != nil || status.State != "waiting" || noticeID == "" || status.NoticeID != noticeID {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The recovery notice changed. Refresh the session before dismissing it."})
		return
	}
	notice, err := s.recoveryNotice(summary)
	if err != nil || notice.ID != noticeID {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The recovery notice changed. Refresh the session before dismissing it."})
		return
	}
	notice.Dismissed = true
	if err := s.saveThreadRecord("recovery-notice", summary.Codex.ThreadID, notice); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Unable to dismiss the recovery notice. Try again."})
		return
	}
	status.NoticeDismissed = true
	s.writeJSON(w, http.StatusOK, status)
}
