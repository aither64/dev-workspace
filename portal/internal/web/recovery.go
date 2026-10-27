package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type recoveryStatus struct {
	State              string `json:"state"`
	ExecutionHeld      bool   `json:"executionHeld"`
	WorkKind           string `json:"workKind,omitempty"`
	InputBlockedReason string `json:"inputBlockedReason,omitempty"`
	Error              string `json:"error,omitempty"`
	Retry              bool   `json:"-"`
	NoticeID           string `json:"noticeId,omitempty"`
	NoticeDismissed    bool   `json:"noticeDismissed,omitempty"`
}

type recoveryController interface {
	LoadedThreadIDs(context.Context) ([]string, error)
	ActivateThread(context.Context, string, codex.ThreadPolicy) error
	HasActiveGoal(context.Context, string) (bool, error)
}

func (s *Server) recoveryStore() session.RecoveryStore {
	return session.RecoveryStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
}

func (s *Server) recoveryHeld(slug, thread string) bool {
	if !s.config.RecoverSessions {
		return false
	}
	record, err := s.recoveryStore().Load(slug)
	if err != nil || record.SocketPath != s.config.CodexSocket {
		return true
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil || summary.Codex.ThreadID != record.ThreadID {
		return true
	}
	epoch, err := session.SocketIdentity(s.config.CodexSocket)
	return err != nil || !record.Active(thread, epoch)
}

func (s *Server) recoveryStatus(summary *session.Summary) recoveryStatus {
	if summary.Archived {
		return recoveryStatus{ExecutionHeld: true, State: "stopped"}
	}
	if !s.config.RecoverSessions && summary.Interactive {
		return recoveryStatus{State: "active"}
	}
	s.recoveryMu.Lock()
	operation, exists := s.recoveryOperations[summary.Slug]
	s.recoveryMu.Unlock()
	if exists && (operation.State == "recovering" || operation.State == "failed") {
		operation.ExecutionHeld = true
		return operation
	}
	if summary.Codex.ThreadID == "" {
		return recoveryStatus{ExecutionHeld: true, State: "stopped"}
	}
	if !summary.Interactive {
		return recoveryStatus{ExecutionHeld: true, State: "stopped"}
	}
	if s.recoveryHeld(summary.Slug, summary.Codex.ThreadID) {
		return s.heldRecoveryStatus(context.Background(), summary, summary.Codex.ThreadID)
	}
	return recoveryStatus{State: "active"}
}

func (s *Server) setRecoveryStatus(slug string, value recoveryStatus) {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	if s.recoveryOperations == nil {
		s.recoveryOperations = make(map[string]recoveryStatus)
	}
	s.recoveryOperations[slug] = value
}

// Admission shares the shutdown lock: Close cannot race a new WaitGroup Add.
// At most one recovery operation owns a session, including browser retries.
func (s *Server) launchRecovery(slug string, run func()) bool {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.closing || s.recoveryJobs[slug] {
		return false
	}
	if s.recoveryJobs == nil {
		s.recoveryJobs = make(map[string]bool)
	}
	s.recoveryJobs[slug] = true
	s.setRecoveryStatus(slug, recoveryStatus{ExecutionHeld: true, State: "recovering"})
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		defer func() { s.operationMu.Lock(); delete(s.recoveryJobs, slug); s.operationMu.Unlock() }()
		run()
	}()
	return true
}

func (s *Server) startRecovery() {
	if !s.config.RecoverSessions {
		return
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ctx := s.operationContext
		backoff := make(map[string]time.Time)
		slots := make(chan struct{}, 2)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			summaries, listErr := session.List(s.config.Workspace)
			if listErr != nil {
				s.config.Logger.Printf("list sessions for runtime recovery: %v", listErr)
			}
			for _, summary := range summaries {
				if s.recoverRuntimeSession(ctx, summary, backoff[summary.Slug], slots) {
					backoff[summary.Slug] = time.Now().Add(30 * time.Second)
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

// recoverRuntimeSession is one step of the existing automatic recovery sweep.
func (s *Server) recoverRuntimeSession(ctx context.Context, summary session.Summary, retryAt time.Time, slots chan struct{}) bool {
	if summary.Archived || summary.Codex.ThreadID == "" || summary.Codex.SocketPath != s.config.CodexSocket {
		return false
	}
	if s.requireProviderAdmission(summary.Slug) != nil {
		return false
	}
	s.recoveryMu.Lock()
	previous := s.recoveryOperations[summary.Slug]
	s.recoveryMu.Unlock()
	if previous.State == "recovering" || (previous.State == "failed" && !previous.Retry) {
		return false
	}
	record, err := s.recoveryStore().Load(summary.Slug)
	if errors.Is(err, os.ErrNotExist) {
		// Upgrade seeds only an exact, still-live runtime. Stopped
		// tracking never supplies an automatic-restoration intent.
		if err := s.seedLiveRecovery(ctx, &summary); err != nil {
			return false
		}
		record, err = s.recoveryStore().Load(summary.Slug)
	}
	if err != nil {
		s.setRecoveryStatus(summary.Slug, recoveryStatus{ExecutionHeld: true, State: "failed", Error: err.Error()})
		return false
	}
	if record.ThreadID != summary.Codex.ThreadID || record.SocketPath != summary.Codex.SocketPath {
		s.setRecoveryStatus(summary.Slug, recoveryStatus{ExecutionHeld: true, State: "failed", Error: "Saved recovery identity differs from this session."})
		return false
	}
	if !record.Automatic {
		return false
	}
	checked := summary
	s.normalizeInteractivity(ctx, &checked)
	if checked.Interactive {
		s.retireTemporaryRecovery(ctx, &summary, previous)
		return false
	}
	if time.Now().Before(retryAt) {
		return false
	}
	select {
	case slots <- struct{}{}:
	default:
		return false
	}
	slug := summary.Slug
	if s.launchRecovery(slug, func() { defer func() { <-slots }(); s.restoreRuntime(ctx, slug) }) {
		return true
	} else {
		<-slots
	}
	return false
}

// Only the coordinator retires a transient access failure. Fresh live-runtime
// proof removes an obsolete error, never a hold or an original submission.
func (s *Server) retireTemporaryRecovery(parent context.Context, expected *session.Summary, observed recoveryStatus) {
	if observed.State != "failed" || !observed.Retry {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	unlock, err := s.lockTransitionContext(ctx)
	if err != nil {
		return
	}
	defer unlock()
	lock, err := session.LockRuntimeSharedContext(ctx, s.config.AuthorityDir, expected.Slug)
	if err != nil {
		return
	}
	defer lock.Close()
	if s.requireCurrentHostProfile() != nil || s.requireProviderAdmission(expected.Slug) != nil {
		return
	}
	current, err := session.Find(s.config.Workspace, expected.Slug)
	if err != nil || current.Archived || current.Root != expected.Root || current.Codex != expected.Codex {
		return
	}
	if owner, err := session.PendingLifecycle(s.config.Workspace, expected.Slug); err != nil || owner != "" {
		return
	}
	record, err := s.recoveryStore().Load(expected.Slug)
	if err != nil || !record.Automatic || record.ThreadID != current.Codex.ThreadID || record.SocketPath != current.Codex.SocketPath {
		return
	}
	s.normalizeInteractivity(ctx, current)
	if !current.Interactive || ctx.Err() != nil {
		return
	}
	// Match the snapshot after I/O, using launchRecovery's lock order. A newer
	// operation, shutdown or an owning job always wins over this observation.
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	if receipt, ok := s.creations[expected.Slug]; ok && receipt.blocksSession() {
		return
	}
	if !s.closing && !s.recoveryJobs[expected.Slug] && s.recoveryOperations[expected.Slug] == observed {
		delete(s.recoveryOperations, expected.Slug)
	}
}

func (s *Server) seedLiveRecovery(parent context.Context, summary *session.Summary) error {
	controller, ok := s.config.Codex.(recoveryController)
	if !ok {
		return errors.New("recovery client is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	owner, err := session.PendingLifecycle(s.config.Workspace, summary.Slug)
	if err != nil || owner != "" {
		return errors.New("session has unfinished lifecycle work")
	}
	s.normalizeInteractivity(ctx, summary)
	if !summary.Interactive {
		return errors.New("session runtime is not live")
	}
	loaded, err := controller.LoadedThreadIDs(ctx)
	if err != nil {
		return err
	}
	epoch, err := session.SocketIdentity(s.config.CodexSocket)
	if err != nil {
		return err
	}
	active := []string{}
	if slices.Contains(loaded, summary.Codex.ThreadID) {
		active = append(active, summary.Codex.ThreadID)
	}
	roster, err := s.loadTeamRoster(summary)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if roster != nil {
		for _, member := range roster.Members {
			if member.State == "ready" && member.RetireIntent == "" && slices.Contains(loaded, member.Thread) {
				if err := s.config.VerifyThread(ctx, member.Thread, filepath.Join(s.config.Workspace, "work", summary.Slug)); err != nil {
					return err
				}
				active = append(active, member.Thread)
			}
		}
	}
	return s.recoveryStore().Update(ctx, summary.Slug, func(record *session.Recovery) error {
		if record.Schema != 0 {
			return nil
		}
		*record = session.Recovery{Schema: 1, Workspace: s.config.Workspace, Slug: summary.Slug,
			ThreadID: summary.Codex.ThreadID, SocketPath: s.config.CodexSocket, SocketIdentity: epoch,
			Automatic: true, ActiveThreads: active}
		return nil
	})
}

func (s *Server) restoreRuntime(ctx context.Context, slug string) {
	if s.requireProviderAdmission(slug) != nil {
		s.setRecoveryStatus(slug, recoveryStatus{})
		return
	}
	lock := s.messageLock("recovery:" + slug)
	if err := lock.Lock(ctx); err != nil {
		return
	}
	defer lock.Unlock()
	s.setRecoveryStatus(slug, recoveryStatus{ExecutionHeld: true, State: "recovering"})
	_, stderr, err := s.runDevSession(ctx, 90*time.Second, "resume", slug, "--as-is")
	if err != nil {
		if s.requireProviderAdmission(slug) != nil || strings.Contains(stderr, "provider transition is reserved") || strings.Contains(stderr, "this session is changing provider") {
			s.setRecoveryStatus(slug, recoveryStatus{})
			return
		}
		// An unavailable native endpoint or timeout is transient. Canonical
		// identity/lifecycle refusals need an explicit Retry after repair.
		_, socketErr := session.SocketIdentity(s.config.CodexSocket)
		var exit *exec.ExitError
		retry := socketErr != nil || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &exit) && exit.ExitCode() == 75
		if stderr != "" {
			err = fmt.Errorf("%s", stderr)
		}
		s.setRecoveryStatus(slug, recoveryStatus{ExecutionHeld: true, State: "failed", Error: err.Error(), Retry: retry})
		return
	}
	s.setRecoveryStatus(slug, recoveryStatus{ExecutionHeld: true, State: "waiting"})
}

func (s *Server) recoveryAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary, operation string) {
	if operation == "recovery" && r.Method == http.MethodGet {
		s.normalizeInteractivity(r.Context(), summary)
		s.writeJSON(w, http.StatusOK, s.recoveryStatus(summary))
		return
	}
	if r.Method != http.MethodPost || !s.config.RecoverSessions || summary.Archived || summary.Codex.ThreadID == "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This session cannot be resumed."})
		return
	}
	var request struct {
		RequestID string `json:"requestId"`
		NoticeID  string `json:"noticeId"`
		Member    string `json:"member,omitempty"`
	}
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if operation == "dismiss-recovery" {
		s.dismissRecoveryNotice(w, r, summary, request.NoticeID, request.Member)
		return
	}
	if operation == "continue" && !queueClientMessageIDPattern.MatchString(request.RequestID) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Continue requires a stable request ID."})
		return
	}
	if operation == "resume" {
		if err := s.withCurrentGeneration(func() error { return nil }); err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		s.launchRecovery(summary.Slug, func() { s.restoreRuntime(s.operationContext, summary.Slug) })
		s.writeJSON(w, http.StatusAccepted, recoveryStatus{ExecutionHeld: true, State: "recovering"})
		return
	}
	if operation != "continue" {
		http.NotFound(w, r)
		return
	}
	// Use the normal resolver's origin, generation, lifecycle, runtime and
	// retained-identity checks. Continue is an explicit send-like operation.
	target, err := s.resolveConversation(r.Context(), conversation.ResolveRequest{ID: summary.Slug, Operation: "message", Method: http.MethodPost, Mutation: true})
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer target.Release()
	if err := target.MutationLock.Lock(r.Context()); err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer target.MutationLock.Unlock()
	if !s.recoveryHeld(summary.Slug, summary.Codex.ThreadID) {
		s.writeJSON(w, http.StatusAccepted, recoveryStatus{State: "active"})
		return
	}
	acceptedID, err := s.acceptContinuation(r.Context(), summary.Slug, summary.Codex.ThreadID, request.RequestID)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if !s.launchRecovery(summary.Slug, func() {
		ctx, cancel := context.WithTimeout(s.operationContext, 90*time.Second)
		defer cancel()
		current, err := s.resolveConversation(ctx, conversation.ResolveRequest{ID: summary.Slug, Operation: "message", Method: http.MethodPost, Mutation: true})
		if err == nil {
			defer current.Release()
			err = current.MutationLock.Lock(ctx)
			if err == nil {
				defer current.MutationLock.Unlock()
				err = s.continueRecovery(ctx, summary.Slug, summary.Codex.ThreadID, acceptedID)
			}
		}
		if err != nil {
			s.setRecoveryStatus(summary.Slug, recoveryStatus{ExecutionHeld: true, State: "failed", Error: err.Error()})
		}
	}) {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Session recovery is already running. Wait for its status before retrying."})
		return
	}
	s.writeJSON(w, http.StatusAccepted, recoveryStatus{ExecutionHeld: true, State: "recovering"})
}

func (s *Server) recoveryPolicy(slug, root, address string) (codex.ThreadPolicy, error) {
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil || summary.Codex.ThreadID != root {
		return codex.ThreadPolicy{}, errors.New("retained conversation changed")
	}
	roster, err := s.loadTeamRoster(summary)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return codex.ThreadPolicy{}, err
	}
	if address != "" {
		service, err := s.teamService(slug)
		if err != nil {
			return codex.ThreadPolicy{}, err
		}
		member, err := readyConversationMember(service.Store, slug, root, address)
		if err != nil {
			return codex.ThreadPolicy{}, err
		}
		return service.MemberTurnPolicy(slug, root, member)
	}
	instructions := ""
	if roster != nil {
		instructions = roster.LeadInstructions
	}
	return workspacecodex.LeadThreadPolicy(slug, s.config.Workspace, instructions), nil
}

func (s *Server) activateRecovery(ctx context.Context, slug, root, thread, address string) error {
	if saved, err := s.coldSettings(slug, root, thread); err != nil {
		return err
	} else if saved != nil && saved.RequestID != "" {
		return errors.New("Recover the pending original input before continuing other work.")
	}
	epoch, err := session.SocketIdentity(s.config.CodexSocket)
	if err != nil {
		return err
	}
	if err := s.loadRecovery(ctx, slug, root, thread, address); err != nil {
		return err
	}
	if err := s.publishRecovery(ctx, slug, root, thread, epoch); err != nil {
		return err
	}
	if err := os.Remove(s.threadRecordPath("next-settings", thread)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Loading is separate from permission publication for cold explicit input.
func (s *Server) loadRecovery(ctx context.Context, slug, root, thread, address string) error {
	controller, ok := s.config.Codex.(recoveryController)
	if !ok {
		return errors.New("recovery client is unavailable")
	}
	policy, err := s.recoveryPolicy(slug, root, address)
	if err != nil {
		return err
	}
	record, err := s.recoveryStore().Load(slug)
	if err != nil {
		return err
	}
	if record.ThreadID != root || record.SocketPath != s.config.CodexSocket {
		return errors.New("saved recovery identity changed")
	}
	settings := codex.ThreadSettings{Policy: policy}
	if address != "" {
		service, err := s.teamService(slug)
		if err != nil {
			return err
		}
		member, err := readyConversationMember(service.Store, slug, root, address)
		if err != nil {
			return err
		}
		settings.Model, settings.ReasoningEffort = member.Model, member.Effort
	} else {
		saved, err := s.coldSettings(slug, root, thread)
		if err != nil {
			return err
		}
		if saved != nil {
			settings.Model, settings.ReasoningEffort = saved.Model, saved.Effort
		}
	}

	if settings.Model != "" {
		activator, ok := s.config.Codex.(interface {
			ActivateThreadWithSettings(context.Context, string, codex.ThreadSettings) error
		})
		if !ok {
			return errors.New("activation with saved settings is unavailable")
		}
		if err := s.validateModelSettings(ctx, settings, false, slug); err != nil {
			return err
		}
		if err := activator.ActivateThreadWithSettings(ctx, thread, settings); err != nil {
			return err
		}
	} else if err := controller.ActivateThread(ctx, thread, policy); err != nil {
		return err
	}

	return nil
}

func (s *Server) publishRecovery(ctx context.Context, slug, root, thread, epoch string) error {
	err := s.recoveryStore().Update(ctx, slug, func(record *session.Recovery) error {
		if record.ThreadID != root || record.SocketPath != s.config.CodexSocket {
			return errors.New("saved recovery identity changed")
		}
		current, err := session.SocketIdentity(s.config.CodexSocket)
		if err != nil || current != epoch {
			return errors.New("App Server changed during activation")
		}
		if record.SocketIdentity != epoch {
			record.ActiveThreads = []string{}
		}
		record.SocketIdentity = epoch
		if thread == root {
			record.Continuation = nil
		}
		if !slices.Contains(record.ActiveThreads, thread) {
			record.ActiveThreads = append(record.ActiveThreads, thread)
		}
		return nil
	})
	if err == nil {
		s.setRecoveryStatus(slug, recoveryStatus{State: "active"})
	}
	return err
}

func (s *Server) acceptContinuation(ctx context.Context, slug, root, requestID string) (string, error) {
	if saved, err := s.coldSettings(slug, root, root); err != nil {
		return "", err
	} else if saved != nil && saved.RequestID != "" {
		return "", errors.New("Recover the pending original input before continuing other work.")
	}
	epoch, err := session.SocketIdentity(s.config.CodexSocket)
	if err != nil {
		return "", err
	}
	record, err := s.recoveryStore().Load(slug)
	if err != nil {
		return "", err
	}
	if record.SocketIdentity == epoch && record.Continuation != nil {
		return record.Continuation.RequestID, nil
	}
	controller, ok := s.config.Codex.(recoveryController)
	if !ok {
		return "", errors.New("recovery client is unavailable")
	}
	queue, err := s.config.Codex.ListQueue(ctx, root)
	if err != nil {
		return "", err
	}
	goal, err := controller.HasActiveGoal(ctx, root)
	if err != nil {
		return "", err
	}
	err = s.recoveryStore().Update(ctx, slug, func(current *session.Recovery) error {
		if current.ThreadID != root || current.SocketPath != s.config.CodexSocket {
			return errors.New("saved recovery identity changed")
		}
		if now, err := session.SocketIdentity(s.config.CodexSocket); err != nil || now != epoch {
			return errors.New("App Server changed during continuation")
		}
		if current.SocketIdentity != epoch {
			current.SocketIdentity = epoch
			current.ActiveThreads = []string{}
			current.Continuation = nil
		}
		if current.Continuation == nil {
			current.Continuation = &session.RecoveryContinuation{RequestID: requestID, Prompt: len(queue) == 0 && !goal}
		}
		requestID = current.Continuation.RequestID
		return nil
	})
	return requestID, err
}

func (s *Server) continueRecovery(ctx context.Context, slug, root, requestID string) error {
	// Continue's background job reacquires admission after its decision was
	// accepted. A manual attempt can bind in that interval; check before Queue.
	if saved, err := s.coldSettings(slug, root, root); err != nil {
		return err
	} else if saved != nil && saved.RequestID != "" {
		return errors.New("Recover the pending original input before continuing other work.")
	}
	if !s.recoveryHeld(slug, root) {
		s.setRecoveryStatus(slug, recoveryStatus{State: "active"})
		return nil
	}
	record, err := s.recoveryStore().Load(slug)
	if err != nil {
		return err
	}
	if record.Continuation == nil || record.Continuation.RequestID != requestID {
		return errors.New("continuation receipt changed")
	}
	if record.Continuation.Prompt {
		_, err := s.config.Codex.Queue(ctx, root, "Continue the interrupted task from the saved conversation. Inspect the current state before repeating actions.", requestID)
		if err != nil {
			return err
		}
	}
	return s.activateRecovery(ctx, slug, root, root, "")
}

// Cold explicit sends preserve the hold through settings and exact delivery.
type recoveryConversationClient struct {
	conversation.Client
	server                      *Server
	slug, root, thread, address string
}

type recoveryPagedConversationClient struct{ recoveryConversationClient }

func (c recoveryConversationClient) DeleteQueueEntryWithCompletion(ctx context.Context, thread, id string, complete func() error) error {
	client, ok := c.Client.(conversation.QueueDeletionCompleter)
	if !ok {
		return errors.New("queue deletion recovery is unavailable")
	}
	return client.DeleteQueueEntryWithCompletion(ctx, thread, id, complete)
}

func (c recoveryConversationClient) ReconcileQueueDeletionsWithCompletion(ctx context.Context, thread string, complete func(string) error) error {
	client, ok := c.Client.(conversation.QueueDeletionCompleter)
	if !ok {
		return errors.New("queue deletion recovery is unavailable")
	}
	return client.ReconcileQueueDeletionsWithCompletion(ctx, thread, complete)
}

func (c recoveryPagedConversationClient) ReadThreadPage(ctx context.Context, thread, cursor string) (codex.TranscriptPage, error) {
	page, err := c.Client.(conversation.TranscriptPageReader).ReadThreadPage(ctx, thread, cursor)
	if err != nil {
		return page, err
	}
	saved, err := c.server.coldSettings(c.slug, c.root, thread)
	if err == nil && saved != nil {
		if saved.Model != "" {
			page.Model, page.ReasoningEffort = saved.Model, saved.Effort
		}
		if saved.Mode != "" {
			page.CollaborationMode = saved.Mode
		}
	}
	return page, err
}

func (c recoveryConversationClient) Send(ctx context.Context, thread, text, id, action string) (codex.SendReceipt, error) {
	if thread != c.thread || action != "" {
		return codex.SendReceipt{}, errors.New("invalid recovery send identity")
	}
	return c.sendCold(ctx, text, id, action, "")
}

func (c recoveryConversationClient) StartQueue(ctx context.Context, thread, id string) error {
	if thread != c.thread {
		return errors.New("invalid recovery queue identity")
	}
	entries, err := c.Client.ListQueue(ctx, thread)
	if err != nil {
		return err
	}
	if len(entries) == 0 || entries[0].ID != id {
		return errors.New("only the first queued message can be started")
	}
	return c.server.activateRecovery(ctx, c.slug, c.root, thread, c.address)
}
