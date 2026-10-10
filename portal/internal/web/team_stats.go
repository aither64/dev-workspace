package web

import (
	"errors"
	"net/http"
	"os"
	"sync"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type teamActivity struct {
	Address  string                  `json:"address"`
	State    string                  `json:"state"`
	Snapshot *codex.ActivitySnapshot `json:"snapshot,omitempty"`
}

func (s *Server) teamStatsAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	roster, err := s.loadTeamRoster(summary)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	rows := []teamActivity{}
	var mu sync.Mutex
	var workers sync.WaitGroup
	read := func(address, thread, state string) {
		defer workers.Done()
		row := teamActivity{Address: address, State: state}
		if thread != "" {
			if state == "removed" || summary.Archived || s.recoveryHeld(summary.Slug, thread) {
				var snapshot codex.ActivitySnapshot
				if s.loadThreadRecord("activity-final", thread, &snapshot) == nil && snapshot.ThreadID == thread {
					if snapshot.CurrentState == "waiting" {
						snapshot.WaitingMS += snapshot.OpenWaitingMS
					}
					snapshot.OpenWaitingMS = 0
					snapshot.CurrentState = "idle"
					snapshot.StateSinceMS = 0
					row.Snapshot = &snapshot
				}
				if state != "removed" {
					row.State = "stopped"
				}
			} else if target, err := s.resolveConversation(r.Context(), conversation.ResolveRequest{ID: func() string {
				if address == "lead" {
					return summary.Slug
				}
				return teamConversationID(summary.Slug, address)
			}(), Operation: "activity", Method: http.MethodGet}); err == nil {
				if !target.Capabilities.Send {
					row.State = "stopped"
				}
				if target.Activity != nil && target.Client.VerifyThread(r.Context(), target.ThreadID, target.Directory) == nil {
					if snapshot, err := target.Activity.ReadActivity(r.Context(), target.ThreadID); err == nil {
						row.Snapshot = &snapshot
					}
				}
				target.Release()
			}
		}
		mu.Lock()
		rows = append(rows, row)
		mu.Unlock()
	}
	workers.Add(1)
	go read("lead", summary.Codex.ThreadID, "ready")
	if roster != nil {
		for _, member := range roster.Members {
			workers.Add(1)
			go read(member.Address, member.Thread, member.State)
		}
	}
	workers.Wait()
	s.writeJSON(w, http.StatusOK, map[string]any{"members": rows})
}
