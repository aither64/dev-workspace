package web

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

type turnRetryClient interface {
	RetryAvailable() bool
	RetryStore() turnretry.Store
	RetryScope(context.Context, string) (turnretry.Scope, error)
	RetryBackend() turnretry.Backend
}

type retryProvider struct {
	server *Server
	client turnRetryClient
	scope  turnretry.Scope
}

func retryView(state turnretry.State, now time.Time) conversation.RetryStatus {
	clock := max(now.UnixMilli(), state.ClockMS)
	status := conversation.RetryStatus{Enabled: state.Enabled, Class: state.Class, Phase: state.Phase, Attempt: int(min(state.Attempt, uint64(1<<53))), ServerNowMS: now.UnixMilli(), CyberUsed: state.CyberUsed(clock), CyberLimit: turnretry.CyberLimit}
	if state.DeadlineMS > 0 {
		v := state.DeadlineMS
		status.NextAttemptAtMS = &v
	}
	// Internal reason codes never become user-visible protocol text.
	status.Reason = map[string]string{
		"settings_unproven": "The failed turn's settings could not be verified. Continue manually.",
		"cyber_limit":       "Automatic retries stopped after 30 attempts within 5 minutes. Continue manually to try again.",
		"outcome_unknown":   "Waiting to confirm whether the retry started.",
		"admission_held":    "Automatic retry is waiting for the conversation to become available.",
		"dispatch_refused":  "The conversation changed before the retry could start. Continue manually.",
		"identity_changed":  "The session changed. Continue manually.",
		"binding_changed":   "Settings changed. Continue manually.",
		"settings_changed":  "Settings changed. Continue manually.",
		"manual_input":      "New input cancelled the pending retry.",
		"superseded":        "New input cancelled the pending retry.",
		"disabled":          "Automatic retry is off.",
		"cancelled":         "Automatic retry cancelled.",
		"lifecycle":         "Automatic retry cancelled.",
		"interrupted":       "Automatic retry cancelled.",
		"activated":         "New input cancelled the pending retry.",
		"queue_started":     "New input cancelled the pending retry.",
	}[state.Reason]
	return status
}

func (p retryProvider) ReadRetry(_ context.Context, thread string) (conversation.RetryStatus, error) {
	if thread != p.scope.Thread {
		return conversation.RetryStatus{}, errors.New("retry target changed")
	}
	state, err := p.client.RetryStore().Load(p.scope)
	if errors.Is(err, os.ErrNotExist) {
		state := turnretry.New(p.scope)
		state.Phase, state.Reason = "blocked", "admission_held"
		return retryView(state, time.Now()), nil
	}
	if err != nil {
		return conversation.RetryStatus{Enabled: false, Phase: "blocked", ServerNowMS: time.Now().UnixMilli(), CyberLimit: turnretry.CyberLimit, Reason: "Retry state is unavailable. Continue manually."}, nil
	}
	return retryView(state, time.Now()), nil
}
func (p retryProvider) SetRetryEnabled(context.Context, string, bool) (conversation.RetryStatus, error) {
	return conversation.RetryStatus{}, activityError(conversation.RetryReloadRequired)
}
func (p retryProvider) CancelRetry(context.Context, string) (conversation.RetryStatus, error) {
	return conversation.RetryStatus{}, activityError(conversation.RetryReloadRequired)
}

// Retain a reservation from enqueue until completion. A slow conversation
// cannot occupy both workers through repeated scheduler ticks.
type retryJobs struct {
	mu     sync.Mutex
	active map[string]bool
	jobs   chan string
}

func (j *retryJobs) enqueue(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.active[id] {
		return true
	}
	select {
	case j.jobs <- id:
		j.active[id] = true
		return true
	default:
		return false
	}
}

func (j *retryJobs) done(id string) {
	j.mu.Lock()
	delete(j.active, id)
	j.mu.Unlock()
}

func (s *Server) startTurnRetries() {
	client, ok := s.config.Codex.(turnRetryClient)
	if !ok || !client.RetryAvailable() || !s.config.RecoverSessions || s.config.UserStateRoot == "" {
		return
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ctx := s.operationContext
		queue := retryJobs{jobs: make(chan string, 2), active: make(map[string]bool)}
		var workers sync.WaitGroup
		var diagnostics sync.Mutex
		lastFailure := map[string]time.Time{}
		for i := 0; i < 2; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for id := range queue.jobs {
					attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
					err := s.stepTurnRetry(attempt, client, id)
					diagnostics.Lock()
					if err != nil && time.Since(lastFailure[id]) > time.Minute {
						s.config.Logger.Printf("automatic retry observation or admission unavailable for %s", id)
						lastFailure[id] = time.Now()
					} else if err == nil {
						delete(lastFailure, id)
					}
					diagnostics.Unlock()
					cancel()
					queue.done(id)
				}
			}()
		}
		defer workers.Wait()
		defer close(queue.jobs)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		next := 0
		for {
			ids := []string{}
			summaries, err := session.List(s.config.Workspace)
			if err == nil {
				for _, summary := range summaries {
					if summary.Archived || session.RequireRetainedReady(&summary, s.config.CodexSocket) != nil {
						continue
					}
					ids = append(ids, summary.Slug)
					roster, err := s.loadTeamRoster(&summary)
					if err != nil {
						continue
					}
					if roster != nil {
						for _, member := range roster.Members {
							if member.State == "ready" && member.RetireIntent == "" && member.Thread != "" {
								ids = append(ids, teamConversationID(summary.Slug, member.Address))
							}
						}
					}
				}
			}
			if len(ids) > 0 {
				start := next
				for count := 0; count < len(ids); count++ {
					index := (start + count) % len(ids)
					if queue.enqueue(ids[index]) {
						next = (index + 1) % len(ids)
					} else {
						count = len(ids)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) stepTurnRetry(ctx context.Context, client turnRetryClient, id string) error {
	if err := s.reconcileTurnRetry(ctx, client, id); err != nil {
		return err
	}
	slug, _, valid := parseTeamConversationID(id)
	if !valid {
		return turnretry.ErrPolicyUnavailable
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		return err
	}
	policyIdentity, err := policyScope(summary)
	if err != nil {
		return err
	}
	if _, err := policyStore(client).Load(policyIdentity); errors.Is(err, os.ErrNotExist) {
		// No SH is held here. Bootstrap takes its own bounded EX transaction.
		bootstrap, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := s.transactRetryPolicy(bootstrap, client, slug, nil)
		cancel()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Reacquire ordinary admission in full after the reconciliation-only
	// transaction. No permission from that transaction authorizes execution.
	target, err := s.resolveConversation(ctx, conversation.ResolveRequest{ID: id, Operation: "automatic-retry", Method: http.MethodPost, Mutation: true})
	if err != nil {
		return err
	}
	defer target.Release()
	if err := target.MutationLock.Lock(ctx); err != nil {
		return err
	}
	defer target.MutationLock.Unlock()
	scope, err := client.RetryScope(ctx, target.ThreadID)
	if err != nil {
		return err
	}
	controller := turnretry.Controller{Store: client.RetryStore(), Backend: client.RetryBackend(), Random: rand.Float64, Policy: turnretry.SessionPolicy{Store: policyStore(client), Scope: policyIdentity}}
	return controller.Step(ctx, scope)
}

// Reconciliation has no execution capability. In particular, the semantic
// ordinary-mutation proof must not prevent its own unresolved exact receipt
// from becoming known. All fresh work still uses resolveConversation above.
func (s *Server) reconcileTurnRetry(ctx context.Context, client turnRetryClient, id string) error {
	slug, address, valid := parseTeamConversationID(id)
	if !valid {
		return errors.New("invalid session identity")
	}
	_, unlock, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	runtime, err := session.LockRuntimeShared(s.config.AuthorityDir, slug)
	if err != nil {
		return err
	}
	defer runtime.Close()
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		return err
	}
	if summary.Archived || summary.Lifecycle != "active" || session.RequireRetainedReady(summary, s.config.CodexSocket) != nil {
		return errors.New("retry session is unavailable")
	}
	if receipt, ok := s.currentCreation(slug); ok && receipt.blocksSession() {
		return errors.New("session initialization has not finished")
	}
	if err := RequireObservationReceipts(summary, s.config.UserStateRoot, "", ""); err != nil {
		return err
	}
	service, err := s.teamService(slug)
	if err != nil {
		return err
	}
	operation, err := service.Store.LockOperation(ctx, slug)
	if err != nil {
		return err
	}
	defer operation()
	thread, project := summary.Codex.ThreadID, ""
	if address != "" {
		member, err := readyConversationMember(service.Store, slug, thread, address)
		if err != nil {
			return err
		}
		thread, project = member.Thread, member.ProjectID
	}
	conversation := s.messageLock(id)
	if err := conversation.Lock(ctx); err != nil {
		return err
	}
	defer conversation.Unlock()
	scope, err := client.RetryScope(ctx, thread)
	if err != nil {
		return err
	}
	expected := turnretry.Scope{Workspace: s.config.Workspace, Slug: slug, Root: summary.Codex.ThreadID, Thread: thread, Socket: s.config.CodexSocket}
	if scope != expected {
		return errors.New("retry target changed")
	}
	state, err := client.RetryStore().Load(scope)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !state.Uncertain() {
		return nil
	}
	authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, slug, s.config.Workspace)
	if err != nil || authority.CodexThreadID != scope.Root || authority.CodexSocketPath != scope.Socket || authority.CodexClientVersion != summary.Codex.ClientVersion {
		return errors.New("retained runtime authority changed")
	}
	// Cold identity reads are allowed here; tmux/execution permission remains
	// a prerequisite of the subsequent strict dispatch admission.
	if s.config.VerifyThread == nil {
		return errors.New("conversation identity is unavailable")
	}
	cwd := filepath.Join(scope.Workspace, "work", slug)
	if err := s.config.VerifyThread(ctx, scope.Root, cwd); err != nil {
		return err
	}
	if address != "" {
		archive, err := service.Client.ProveArchivedThread(ctx, thread, cwd, project)
		if err != nil {
			return err
		}
		if archive != workspacecodex.ArchiveActive && archive != workspacecodex.ArchiveArchived {
			return errors.New("retained member identity is unavailable")
		}
	}
	controller := turnretry.Controller{Store: client.RetryStore(), Backend: client.RetryBackend()}
	return controller.ReconcileOnly(ctx, scope)
}
