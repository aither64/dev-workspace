package web

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type providerWatchRefresher interface {
	RefreshThreadSubscription(context.Context, string) error
}

func (s *Server) providerThreadResumeAllowed(thread string) bool {
	state, err := s.providerWatchState()
	if errors.Is(err, os.ErrNotExist) {
		return s.requireProviderAdmission() == nil
	}
	if err != nil {
		return false
	}
	if state.Schema == 3 {
		owner, err := s.recoveryStore().ThreadOwner(thread)
		if err != nil || owner.Archived || s.requireCurrentHostProfileFor(owner.Slug) != nil || session.RequireRetainedReady(owner, s.config.CodexSocket) != nil || !s.recoveryStore().AllowsImplicitResume(s.config.CodexSocket, thread) {
			return false
		}
		store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if err != nil {
			return false
		}
		if current, err := store.ResolveConversation(owner.Slug, owner.Codex.ThreadID, "lead", owner.Codex.ThreadID); err == nil && current.ThreadID == thread {
			return true
		}
		roster, err := store.Load(owner.Slug, owner.Codex.ThreadID)
		if err != nil {
			return false
		}
		for _, member := range roster.Members {
			if member.State == "ready" && member.RemovedAt == nil && member.RetireIntent == "" && member.Thread != "" {
				if current, err := store.ResolveConversation(owner.Slug, owner.Codex.ThreadID, member.Address, member.Thread); err == nil && current.ThreadID == thread {
					return true
				}
			}
		}
		return false
	}
	if !state.Operation.Reserved() {
		return true
	}
	owner, err := s.recoveryStore().ThreadOwner(thread)
	return err == nil && !state.BlocksSession(owner.Slug, owner.Codex.ThreadID)
}

// Watch recovery is a local read purpose; it grants no execution permission.
func (s *Server) providerWatchState() (codexproviders.State, error) {
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return codexproviders.State{}, err
	}
	state, err := codexproviders.ReadBindingState(s.config.UserStateRoot, s.config.WorkspaceName, s.config.Workspace, s.config.CodexSocket, catalog)
	if err != nil || state.Schema != 3 {
		return state, err
	}
	identity, err := readHostProfileIdentity(s.config.HostProfile)
	selected, selectedErr := filepath.EvalSymlinks(s.config.HostProfile)
	if err != nil || selectedErr != nil || identity != s.hostProfile || selected != s.config.PackageRoot {
		return state, errors.New("watch recovery package generation changed")
	}
	data, err := os.ReadFile(filepath.Join(selected, "share", "workspace-portal", "runtime-contract.json"))
	var contract map[string]json.RawMessage
	if err != nil || session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || string(contract["codexProviderBindingStateSchema"]) != "3" || string(contract["codexProviderForkTransitionPolicy"]) != "1" || string(contract["codexRuntimeAuthoritySchema"]) != "3" {
		return state, errors.New("watch recovery package cannot read bindings")
	}
	return state, nil
}

// Native archival closes connection-local subscriptions. Rejoin the portal's
// existing watches after the worker has restored effective routing and work.
// Session locks and execution gates also serialize a subsequent switch.
func (s *Server) startProviderWatchRecovery() {
	if s.config.WorkspaceName == "" || s.config.AuthorityDir == "" {
		return
	}
	refresher, ok := s.config.Codex.(providerWatchRefresher)
	if !ok {
		return
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.operationContext.Done():
				return
			case <-ticker.C:
			}
			state, err := s.providerWatchState()
			if err != nil {
				continue
			}
			markers, _ := filepath.Glob(filepath.Join(s.config.UserStateRoot, "providers", s.config.WorkspaceName, "*.watch.json"))
			for _, markerPath := range markers {
				markerInfo, err := os.Lstat(markerPath)
				if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm() != 0600 || markerInfo.Size() > 4096 {
					continue
				}
				markerData, err := os.ReadFile(markerPath)
				var marker struct {
					Schema    int    `json:"schema"`
					Operation string `json:"operation"`
					Workspace string `json:"workspace"`
					Socket    string `json:"socket"`
				}
				if err != nil || rejectDuplicateCreationJSONKeys(markerData) != nil || json.Unmarshal(markerData, &marker) != nil || marker.Schema != 1 || marker.Workspace != s.config.Workspace || marker.Socket != s.config.CodexSocket || !queueClientMessageIDPattern.MatchString(marker.Operation) || filepath.Base(markerPath) != marker.Operation+".watch.json" {
					continue
				}
				path := filepath.Join(filepath.Dir(markerPath), marker.Operation+".snapshot.json")
				info, err := os.Lstat(path)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16*1024*1024 {
					continue
				}
				data, err := os.ReadFile(path)
				var snapshot struct {
					Schema    int
					Operation string `json:"operation"`
					Workspace string `json:"workspace"`
					Threads   []struct {
						ID, Slug, Cwd, MemberAddress, MonitorOperationID string
					} `json:"threads"`
				}
				if err != nil || rejectDuplicateCreationJSONKeys(data) != nil || json.Unmarshal(data, &snapshot) != nil || snapshot.Schema != 1 || snapshot.Threads == nil || snapshot.Operation != marker.Operation || snapshot.Workspace != s.config.Workspace || state.Schema == 3 && (state.Operation == nil || state.Operation.ID != marker.Operation || state.Operation.Phase != "applied") {
					continue
				}
				complete := true
				for _, thread := range snapshot.Threads {
					if !session.ValidSlug(thread.Slug) {
						complete = false
						break
					}
					ctx, stop := context.WithTimeout(s.operationContext, 5*time.Second)
					unlockTransition, err := s.lockTransitionContext(ctx)
					if err != nil {
						stop()
						complete = false
						continue
					}
					lock, err := session.LockRuntimeSharedContext(ctx, s.config.AuthorityDir, thread.Slug)
					if err != nil {
						unlockTransition()
						stop()
						complete = false
						continue
					}
					target := thread.ID
					var unlockTeam func()
					if state.Schema == 3 {
						err = s.requireCurrentHostProfileFor(thread.Slug)
						store, storeErr := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
						if err == nil {
							err = storeErr
						}
						if err == nil {
							unlockTeam, err = store.LockOperation(ctx, thread.Slug)
						}
						if err == nil {
							currentState, stateErr := s.providerWatchState()
							if stateErr != nil || currentState.Schema != 3 || currentState.Operation == nil || currentState.Operation.ID != marker.Operation || currentState.Operation.Phase != "applied" {
								err = errors.New("saved watch operation changed")
							} else {
								err = s.requireCurrentHostProfileFor(thread.Slug)
							}
						}
						if err == nil {
							summary, findErr := session.Find(s.config.Workspace, thread.Slug)
							err = findErr
							if err == nil && (summary.Archived || thread.Cwd != filepath.Join(s.config.Workspace, "work", thread.Slug) || thread.MonitorOperationID != "") {
								err = errors.New("saved watch owner changed")
							}
							member := thread.MemberAddress
							if member == "" {
								member = "lead"
								if err == nil && thread.ID != summary.Codex.ThreadID {
									err = errors.New("saved watch original root changed")
								}
							} else if err == nil {
								row, memberErr := readyConversationMember(store, thread.Slug, summary.Codex.ThreadID, member)
								if memberErr != nil || row.Thread != thread.ID {
									err = errors.New("saved watch original member changed")
								}
							}
							if err == nil {
								current, resolveErr := store.ResolveConversation(thread.Slug, summary.Codex.ThreadID, member, thread.ID)
								target, err = current.ThreadID, resolveErr
							}
						}
					} else if summary, findErr := session.Find(s.config.Workspace, thread.Slug); findErr != nil || summary.Archived {
						err = errors.New("saved watch session unavailable")
					}
					if err == nil {
						err = refresher.RefreshThreadSubscription(ctx, target)
					}
					if err == nil && s.activity != nil {
						if observer, ok := s.activity.observer.(providerWatchRefresher); ok {
							err = observer.RefreshThreadSubscription(ctx, target)
						}
					}
					stop()
					if unlockTeam != nil {
						unlockTeam()
					}
					lock.Close()
					unlockTransition()
					if err != nil {
						complete = false
					}
				}
				if complete {
					_ = os.Remove(markerPath)
				}
			}
		}
	}()
}
