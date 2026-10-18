package web

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

type providerWatchRefresher interface {
	RefreshThreadSubscription(context.Context, string) error
}

func (s *Server) providerThreadResumeAllowed(thread string) bool {
	_, state, err := s.providerState()
	if errors.Is(err, os.ErrNotExist) {
		return s.requireProviderAdmission() == nil
	}
	if err != nil {
		return false
	}
	if !state.Operation.Reserved() {
		return true
	}
	owner, err := s.recoveryStore().ThreadOwner(thread)
	return err == nil && !state.BlocksSession(owner.Slug, owner.Codex.ThreadID)
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
			if _, _, err := s.providerState(); err != nil {
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
				if err != nil || json.Unmarshal(markerData, &marker) != nil || marker.Schema != 1 || marker.Workspace != s.config.Workspace || marker.Socket != s.config.CodexSocket || !queueClientMessageIDPattern.MatchString(marker.Operation) || filepath.Base(markerPath) != marker.Operation+".watch.json" {
					continue
				}
				path := filepath.Join(filepath.Dir(markerPath), marker.Operation+".snapshot.json")
				info, err := os.Lstat(path)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16*1024*1024 {
					continue
				}
				data, err := os.ReadFile(path)
				var snapshot struct {
					Operation string `json:"operation"`
					Workspace string `json:"workspace"`
					Threads   []struct {
						ID   string `json:"id"`
						Slug string `json:"slug"`
					} `json:"threads"`
				}
				if err != nil || json.Unmarshal(data, &snapshot) != nil || snapshot.Operation != marker.Operation || snapshot.Workspace != s.config.Workspace {
					continue
				}
				complete := true
				for _, thread := range snapshot.Threads {
					if !session.ValidSlug(thread.Slug) {
						complete = false
						break
					}
					if summary, err := session.Find(s.config.Workspace, thread.Slug); err != nil || summary.Archived {
						continue
					}
					lock, err := session.LockRuntimeShared(s.config.AuthorityDir, thread.Slug)
					if err != nil {
						complete = false
						continue
					}
					ctx, stop := context.WithTimeout(s.operationContext, 5*time.Second)
					err = refresher.RefreshThreadSubscription(ctx, thread.ID)
					if err == nil && s.activity != nil {
						if observer, ok := s.activity.observer.(providerWatchRefresher); ok {
							err = observer.RefreshThreadSubscription(ctx, thread.ID)
						}
					}
					stop()
					lock.Close()
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
