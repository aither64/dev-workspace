package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

func (s *Server) rejectLegacyRetry(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || session.TurnRetrySessionPolicy() != 1 {
		return false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	legacy := len(parts) == 5 && parts[0] == "codex" && parts[1] == "conversations" && parts[3] == "retry" && (parts[4] == "enabled" || parts[4] == "cancel")
	legacy = legacy || len(parts) == 5 && parts[0] == "api" && parts[1] == "sessions" && parts[3] == "retry" && (parts[4] == "enabled" || parts[4] == "cancel")
	if !legacy {
		return false
	}
	if !s.validMutation(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "request origin is invalid"})
		return true
	}
	s.writeJSON(w, http.StatusConflict, map[string]string{"code": "retry_reload_required", "error": "Reload the page to use the current retry controls."})
	return true
}

func policyScope(summary *session.Summary) (turnretry.PolicyScope, error) {
	identity, err := session.ObservationIdentity(summary.Workspace, summary.Slug, filepath.Join(summary.Workspace, summary.Root, summary.Slug), summary.Codex.ThreadID)
	return turnretry.PolicyScope{Workspace: summary.Workspace, Slug: summary.Slug, Root: summary.Codex.ThreadID, Incarnation: identity}, err
}

func policyStore(client turnRetryClient) turnretry.PolicyStore {
	store := client.RetryStore()
	return turnretry.PolicyStore{Root: store.Root, Workspace: store.Workspace}
}

type retryPolicyRequest struct {
	Capacity         *bool  `json:"capacity"`
	Cybersecurity    *bool  `json:"cybersecurity"`
	ExpectedRevision string `json:"expectedRevision"`
}

func (s *Server) retryPolicyAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary) {
	client, ok := s.config.Codex.(turnRetryClient)
	if !ok || !client.RetryAvailable() || session.TurnRetrySessionPolicy() != 1 {
		s.retryPolicyError(w, turnretry.ErrPolicyUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		scope, err := policyScope(summary)
		if err != nil {
			s.retryPolicyError(w, err)
			return
		}
		p, err := policyStore(client).Load(scope)
		if err != nil {
			s.retryPolicyError(w, err)
			return
		}
		s.writeRetryPolicy(w, p)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body retryPolicyRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || body.Capacity == nil || body.Cybersecurity == nil || body.ExpectedRevision == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Both retry preferences and their revision are required."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	p, err := s.transactRetryPolicy(ctx, client, summary.Slug, &body)
	if err != nil {
		s.retryPolicyError(w, err)
		return
	}
	s.writeRetryPolicy(w, p)
}

func (s *Server) writeRetryPolicy(w http.ResponseWriter, p turnretry.Policy) {
	s.writeJSON(w, http.StatusOK, map[string]any{"revision": p.Revision, "capacity": p.Capacity, "cybersecurity": p.Cybersecurity})
}
func (s *Server) retryPolicyError(w http.ResponseWriter, err error) {
	if errors.Is(err, turnretry.ErrPolicyStale) {
		s.writeJSON(w, http.StatusConflict, map[string]string{"code": "retry_action_stale", "error": "Retry preferences changed. Reload and try again."})
		return
	}
	s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "retry_activity_unavailable", "error": "Retry preferences are unavailable. Try again later."})
}

// This separate transaction takes EX directly, never through the ordinary SH
// resolver. Native observations finish before the short sidecar checkpoint.
func (s *Server) transactRetryPolicy(ctx context.Context, client turnRetryClient, slug string, request *retryPolicyRequest) (turnretry.Policy, error) {
	_, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return turnretry.Policy{}, err
	}
	defer release()
	if err := s.requireCurrentHostProfile(); err != nil {
		return turnretry.Policy{}, err
	}
	runtime, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, slug)
	if err != nil {
		return turnretry.Policy{}, err
	}
	defer runtime.Close()
	summary, scope, authority, err := s.retryPolicyAuthority(ctx, slug)
	if err != nil {
		return turnretry.Policy{}, err
	}
	service, err := s.teamService(slug)
	if err != nil {
		return turnretry.Policy{}, err
	}
	unlock, err := service.Store.LockOperation(ctx, slug)
	if err != nil {
		return turnretry.Policy{}, err
	}
	defer unlock()
	roster, err := s.loadTeamRoster(summary)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return turnretry.Policy{}, err
	}
	store := policyStore(client)
	current, loadErr := store.Load(scope)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return turnretry.Policy{}, loadErr
	}
	if loadErr == nil && (request == nil || current.EqualChoices(*request.Capacity, *request.Cybersecurity)) {
		return current, nil
	}
	if request != nil && loadErr == nil && request.ExpectedRevision != current.Revision {
		return turnretry.Policy{}, turnretry.ErrPolicyStale
	}
	threads, err := s.retryPolicyInventory(ctx, client, summary, scope, service, roster)
	if err != nil {
		return turnretry.Policy{}, err
	}
	again, afterScope, afterAuthority, err := s.retryPolicyAuthority(ctx, slug)
	if err != nil || afterScope != scope || afterAuthority != authority || again.Codex != summary.Codex || again.Root != summary.Root {
		return turnretry.Policy{}, turnretry.ErrPolicyUnavailable
	}
	afterRoster, err := s.loadTeamRoster(again)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return turnretry.Policy{}, err
	}
	if !reflect.DeepEqual(roster, afterRoster) {
		return turnretry.Policy{}, turnretry.ErrPolicyUnavailable
	}
	if request != nil && errors.Is(loadErr, os.ErrNotExist) {
		return turnretry.Policy{}, turnretry.ErrPolicyUnavailable
	}
	return store.Update(ctx, scope, request == nil, func(p *turnretry.Policy) error {
		if request == nil {
			// Another portal may have bootstrapped while this worker waited EX.
			if loadErr == nil {
				return nil
			}
			return turnretry.PreparePolicy(p, threads, true, true, "", true)
		}
		return turnretry.PreparePolicy(p, threads, *request.Capacity, *request.Cybersecurity, request.ExpectedRevision, false)
	})
}

func (s *Server) retryPolicyAuthority(ctx context.Context, slug string) (*session.Summary, turnretry.PolicyScope, session.RuntimeAuthority, error) {
	var empty turnretry.PolicyScope
	var noAuthority session.RuntimeAuthority
	if err := s.requireCurrentHostProfile(); err != nil {
		return nil, empty, noAuthority, err
	}
	if err := s.requireProviderAdmission(slug); err != nil {
		return nil, empty, noAuthority, err
	}
	if owner, err := session.PendingLifecycle(s.config.Workspace, slug); err != nil || owner != "" {
		return nil, empty, noAuthority, turnretry.ErrPolicyUnavailable
	}
	if receipt, ok := s.currentCreation(slug); ok && receipt.blocksSession() {
		return nil, empty, noAuthority, turnretry.ErrPolicyUnavailable
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil || summary.Archived || summary.Lifecycle != "active" || session.RequireRetainedReady(summary, s.config.CodexSocket) != nil {
		return nil, empty, noAuthority, turnretry.ErrPolicyUnavailable
	}
	if err := RequireObservationReceipts(summary, s.config.UserStateRoot, "", ""); err != nil {
		return nil, empty, noAuthority, err
	}
	authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, slug, s.config.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != summary.Codex.ThreadID || authority.CodexSocketPath != summary.Codex.SocketPath || authority.CodexClientVersion != summary.Codex.ClientVersion {
		return nil, empty, noAuthority, turnretry.ErrPolicyUnavailable
	}
	if err := authority.VerifyTmux(ctx, s.config.Tmux); err != nil {
		return nil, empty, noAuthority, err
	}
	scope, err := policyScope(summary)
	if err != nil {
		return nil, empty, noAuthority, err
	}
	if s.config.VerifyThread == nil {
		return nil, empty, noAuthority, turnretry.ErrPolicyUnavailable
	}
	if err := s.config.VerifyThread(ctx, scope.Root, filepath.Join(scope.Workspace, "work", slug)); err != nil {
		return nil, empty, noAuthority, err
	}
	return summary, scope, authority, nil
}

func (s *Server) retryPolicyInventory(ctx context.Context, client turnRetryClient, summary *session.Summary, scope turnretry.PolicyScope, service teamruntime.Service, roster *teamruntime.Roster) ([]turnretry.PolicyThread, error) {
	type target struct{ thread, id, project string }
	targets := []target{{thread: scope.Root, id: scope.Slug}}
	if roster != nil {
		for _, m := range roster.Members {
			if m.State == "removed" || m.RemovedAt != nil {
				continue
			}
			if m.Thread == "" {
				return nil, turnretry.ErrPolicyUnavailable
			}
			targets = append(targets, target{m.Thread, teamConversationID(scope.Slug, m.Address), m.ProjectID})
		}
	}
	if len(targets) > turnretry.MaxPolicyThreads {
		return nil, turnretry.ErrPolicyUnavailable
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].thread < targets[j].thread })
	result := make([]turnretry.PolicyThread, 0, len(targets))
	for i, target := range targets {
		if i > 0 && targets[i-1].thread == target.thread {
			return nil, turnretry.ErrPolicyUnavailable
		}
		if target.thread != scope.Root {
			state, err := service.Client.ProveArchivedThread(ctx, target.thread, filepath.Join(scope.Workspace, "work", scope.Slug), target.project)
			if err != nil || state != workspacecodex.ArchiveActive && state != workspacecodex.ArchiveArchived {
				return nil, turnretry.ErrPolicyUnavailable
			}
		}
		actual, err := client.RetryScope(ctx, target.thread)
		expected := turnretry.Scope{Workspace: scope.Workspace, Slug: scope.Slug, Root: scope.Root, Thread: target.thread, Socket: s.config.CodexSocket}
		if err != nil || actual != expected {
			return nil, turnretry.ErrPolicyUnavailable
		}
		lock := s.messageLock(target.id)
		if err := lock.Lock(ctx); err != nil {
			return nil, err
		}
		// Load never creates a ledger merely to change optional preferences.
		tx, err := client.RetryStore().Inspect(ctx, expected)
		if err != nil {
			lock.Unlock()
			return nil, err
		}
		state := tx.State
		before, err := client.RetryBackend().Observe(ctx, expected)
		if err != nil {
			tx.Close()
			lock.Unlock()
			return nil, err
		}
		after, err := client.RetryBackend().Observe(ctx, expected)
		tx.Close()
		lock.Unlock()
		if err != nil || before != after {
			return nil, turnretry.ErrPolicyUnavailable
		}
		result = append(result, turnretry.PolicyThread{State: state, Before: before, After: after})
	}
	return result, nil
}
