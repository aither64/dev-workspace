package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type teamActivity struct {
	Address  string                    `json:"address"`
	State    string                    `json:"state"`
	Error    string                    `json:"error,omitempty"`
	Snapshot *backend.ActivitySnapshot `json:"snapshot,omitempty"`
}

func (s *Server) teamStatsAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	kind, binding, err := s.workflowPageBackend(summary)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Conversation backend is unavailable."})
		return
	}
	if kind == agentbackend.Pi {
		s.piTeamStatsAPI(w, r, *binding)
		return
	}
	roster, err := s.loadTeamRoster(summary)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	rows := make([]teamActivity, 1)
	if roster != nil {
		rows = make([]teamActivity, 1+len(roster.Members))
	}
	var workers sync.WaitGroup
	read := func(index int, address, thread, state string) {
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
					row.Snapshot = projectTeamActivity(snapshot)
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
						row.Snapshot = projectTeamActivity(snapshot)
					}
				}
				target.Release()
			}
		}
		rows[index] = row
	}
	workers.Add(1)
	go read(0, "lead", summary.Codex.ThreadID, "ready")
	if roster != nil {
		for index, member := range roster.Members {
			workers.Add(1)
			go read(index+1, member.Address, member.Thread, member.State)
		}
	}
	workers.Wait()
	s.writeJSON(w, http.StatusOK, map[string]any{"members": rows})
}

// The legacy Codex port and native adapters share one browser presentation value.
func projectTeamActivity(value codex.ActivitySnapshot) *backend.ActivitySnapshot {
	snapshot := &backend.ActivitySnapshot{
		ThreadID: value.ThreadID, CurrentTurnID: value.CurrentTurnID,
		Stale: value.Stale, Refreshing: value.Refreshing,
		Messages: value.Messages, ToolCalls: value.ToolCalls,
		SentMessages: value.SentMessages, ReceivedMessages: value.ReceivedMessages,
		TotalToolCalls: value.TotalToolCalls, CountsComplete: value.CountsComplete,
		CreatedAtMS: value.CreatedAtMS, StartedAtMS: value.StartedAtMS, CompletedAtMS: value.CompletedAtMS,
		WorkingMS: value.WorkingMS, WaitingMS: value.WaitingMS, OpenWaitingMS: value.OpenWaitingMS,
		IdleMS: value.IdleMS, BetweenTurnsMS: value.BetweenTurnsMS, UnclassifiedMS: value.UnclassifiedMS,
		WaitReason: value.WaitReason, WaitUntilMS: value.WaitUntilMS, LatestTurnStatus: value.LatestTurnStatus,
		ObservedAtMS: value.ObservedAtMS, StateSinceMS: value.StateSinceMS, CurrentState: value.CurrentState,
		CoverageStartMS: value.CoverageStartMS, CoverageComplete: value.CoverageComplete,
		CoverageReason: value.CoverageReason, Scope: value.Scope, TimingApproximate: value.TimingApproximate,
	}
	for _, request := range value.BlockingRequests {
		snapshot.BlockingRequests = append(snapshot.BlockingRequests, backend.ActivityBlockingRequest(request))
	}
	return snapshot
}

func (s *Server) piTeamStatsAPI(w http.ResponseWriter, r *http.Request, binding session.BackendBinding) {
	scoped, err := scopedPiConversationRequest(r, false)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The selected Pi conversation changed. Reload the session."})
		return
	}
	ref := agentbackend.SessionRef{Workspace: binding.Workspace, Slug: binding.Slug, Incarnation: binding.SessionIncarnation}
	var rows []teamActivity
	err = s.withPiSessionRead(scoped.Context(), ref, func(current piSessionAdmission) error {
		if expected, _ := scoped.Context().Value(piExpectedGenerationKey{}).(string); expected == "" || expected != current.Authority.Generation {
			return agentbackend.ErrStaleExecution
		}
		store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if err != nil {
			return err
		}
		roster, err := store.LoadPi(binding.Slug)
		if errors.Is(err, os.ErrNotExist) {
			if _, err = derivedPiSolo(current); err != nil {
				return err
			}
		} else if err != nil || roster.Scope().Ref() != ref {
			return agentbackend.ErrBindingConflict
		}
		// Admit the browser's actual source before deriving any peer read scope.
		sourceOK := false
		for _, saved := range current.Binding.Conversations {
			ref, err := current.Binding.Resolve(saved.Address)
			if err == nil && (ref.Member == "lead" || s.piCurrentMemberActive(&roster, ref.Member)) && checkExpectedPiScope(scoped.Context(), ref) == nil {
				sourceOK = true
				break
			}
		}
		if !sourceOK {
			return agentbackend.ErrBindingConflict
		}
		rows = []teamActivity{{Address: "lead", State: "ready"}}
		for _, member := range roster.Members {
			rows = append(rows, teamActivity{Address: member.Address, State: member.State})
		}
		for index := range rows {
			row := &rows[index]
			if row.State != "ready" {
				continue
			}
			ref, err := current.Binding.Resolve(row.Address)
			if err != nil {
				row.Error = "Statistics unavailable"
				continue
			}
			// Peer identity comes only from the admitted current roster/binding.
			ctx := context.WithValue(scoped.Context(), piConversationScopeKey{}, piWorkflowNamespace(ref))
			ctx = context.WithValue(ctx, piExpectedGenerationKey{}, current.Authority.Generation)
			id := binding.Slug
			if row.Address != "lead" {
				id = teamConversationID(binding.Slug, row.Address)
			}
			target, err := s.resolvePiBackend(ctx, conversation.ResolveRequest{ID: id, Operation: "activity", Method: http.MethodGet})
			if err != nil {
				row.Error = "Statistics unavailable"
				continue
			}
			func() {
				if target.Release != nil {
					defer target.Release()
				}
				expected := piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: ref, Generation: current.Authority.Generation})
				if target.Identity != expected || target.Operations.Verify == nil || target.Operations.Verify.Verify(ctx, expected) != nil {
					row.Error = "Statistics unavailable"
					return
				}
				if target.Operations.Activity == nil {
					row.Error = "Activity not supported"
					return
				}
				snapshot, err := target.Operations.Activity.ReadActivity(ctx, expected)
				if err != nil || snapshot.ThreadID != ref.NativeID {
					row.Error = "Statistics unavailable"
					return
				}
				row.Snapshot = &snapshot
			}()
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Team activity is unavailable. Reload the session."})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"members": rows})
}
