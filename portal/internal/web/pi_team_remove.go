package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func (s *Server) removePiMember(ctx context.Context, r *http.Request, binding session.BackendBinding, request teamRequestBody) (teamruntime.PiRoster, error) {
	var roster teamruntime.PiRoster
	if request.Action != "remove" || request.Address == "lead" || request.Address == "" || request.Model != "" || request.Effort != "" || request.Role != "" || request.Preset != "" {
		return roster, piruntime.ErrScope
	}
	ref := agentbackend.SessionRef{Workspace: binding.Workspace, Slug: binding.Slug, Incarnation: binding.SessionIncarnation}
	current, release, err := s.acquirePiSessionAdmission(ctx, ref, true)
	if err != nil {
		return roster, err
	}
	defer release()
	data, e := os.ReadFile(filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json"))
	var contract struct {
		Team session.PiTeamRecordsContract `json:"piTeamRecords"`
	}
	if e != nil || json.Unmarshal(data, &contract) != nil || contract.Team.Roster < 5 {
		return roster, piruntime.ErrUnavailable
	}
	lead, err := current.Binding.Resolve("lead")
	if err != nil {
		return roster, err
	}
	scope, err := piWorkflowScopeHeader(r)
	if err != nil || scope != piWorkflowNamespace(lead) {
		return roster, agentbackend.ErrBindingConflict
	}
	if expected := r.Header.Get("X-Workspace-Pi-Generation"); expected != current.Authority.Generation {
		return roster, agentbackend.ErrStaleExecution
	}
	if access, e := s.piSavedAccessLocked(current, lead); e != nil || workflowPermission(access, "conversation.message") != nil {
		return roster, errors.Join(piruntime.ErrHeld, e)
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return roster, err
	}
	roster, err = store.LoadPi(binding.Slug)
	if err != nil {
		return roster, err
	}
	index := -1
	for n, m := range roster.Members {
		if m.Address == request.Address {
			index = n
		}
	}
	if index < 0 {
		return roster, piruntime.ErrScope
	}
	target := roster.Members[index]
	if target.State == "removed" {
		return roster, nil
	}
	bound := current.Binding.Conversations[index+1]
	if bound.Address != target.Address || bound.Incarnation != target.Incarnation {
		return roster, agentbackend.ErrBindingConflict
	}
	if target.State == "creating" {
		if target.InitialInputID != "" || target.Fork != nil || bound.State != "creating" || bound.NativeID != "" {
			return roster, piruntime.ErrHeld
		}
		root, e := piruntime.SessionStateDirectory(s.config.UserStateRoot, ref)
		if e != nil {
			return roster, e
		}
		if _, e = os.Lstat(filepath.Join(root, "members", target.Address, target.Incarnation, "conversation.json")); !errors.Is(e, os.ErrNotExist) {
			return roster, piruntime.ErrHeld
		}
	} else {
		if target.State != "ready" && target.State != "archived" {
			return roster, piruntime.ErrHeld
		}
		native, ok := s.config.PiCreation.Runtime.(interface {
			Unload(context.Context, agentbackend.ExecutionHandle) (piruntime.StopResult, error)
		})
		if !ok {
			return roster, piruntime.ErrUnavailable
		}
		actual, e := current.Binding.Resolve(target.Address)
		if e != nil {
			return roster, e
		}
		stopped, e := native.Unload(ctx, agentbackend.ExecutionHandle{Conversation: actual, Generation: current.Authority.Generation})
		if e != nil {
			return roster, e
		}
		if !((stopped.VerifiedDetach && stopped.Outcome == "detached") || (stopped.VerifiedExit && stopped.Outcome == "stopped")) {
			return roster, piruntime.ErrHeld
		}
	}
	next := roster
	next.Members = append([]teamruntime.PiRosterMember{}, roster.Members...)
	next.Schema = 5
	next.Revision++
	next.UpdatedAt = time.Now().UTC()
	next.Members[index].State = "removed"
	next.Members[index].RemovedAt = &next.UpdatedAt
	proofs, err := (piTeamService{s}).readyProofs(ctx, current, next)
	if err != nil {
		return roster, err
	}
	verifier := &piRosterWriteAdmission{server: s, snapshot: current, proofs: proofs}
	verifier.active.Store(true)
	defer verifier.active.Store(false)
	admission := piCreationRosterAdmission{ref: ref, generation: current.Authority.Generation, validate: s.requireCurrentHostProfile}
	if err = s.piCreationPoint("remove_detached"); err != nil {
		return roster, err
	}
	if err = store.RemovePi(ctx, admission, current.Authority.Generation, roster, next, target.Address, verifier); err != nil {
		return roster, err
	}
	if s.piWorkflows != nil {
		if actual, e := current.Binding.Resolve(target.Address); e == nil {
			s.piWorkflows.invalidateRef(actual, false)
		}
	}
	return next, nil
}
