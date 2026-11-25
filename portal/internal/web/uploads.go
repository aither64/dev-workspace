package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	"golang.org/x/sys/unix"
)

func (s *Server) initUploads(ctx context.Context) error {
	store, err := uploads.New(filepath.Join(s.operationStore.directory, "uploads"), s.config.Workspace)
	if err != nil {
		return err
	}
	s.uploadStore = store
	handler, err := conversation.NewUploadHandler(conversation.UploadOptions{
		BasePath: "/uploads", AllowedOrigins: s.trustedOrigins, Resolve: s.resolveUploads,
	})
	if err != nil {
		return err
	}
	s.uploadHandler = handler
	return s.retainCreationUploads(ctx)
}

func (s *Server) sessionUploads(ctx context.Context, slug, thread string, readOnly bool) (*uploads.Backend, error) {
	epoch, err := session.CompletedRemovalHistory(s.config.Workspace, slug, s.config.UserStateRoot)
	if err != nil {
		return nil, err
	}
	if summary, err := session.Find(s.config.Workspace, slug); err == nil && summary.Codex.ThreadID == thread && summary.Creation.GoalSHA256 != "" {
		if err := s.uploadStore.AdoptInitial(ctx, summary.Creation.GoalSHA256, slug, thread, epoch); err != nil {
			return nil, err
		}
	}
	scope, err := s.uploadStore.SessionScope(ctx, slug, thread, epoch)
	if err != nil {
		return nil, err
	}
	return &uploads.Backend{Store: s.uploadStore, ScopeID: scope.ID, BaseURL: "/uploads/s-" + slug, ReadOnly: readOnly, CheckIdle: s.uploadReferencesIdle, LockReferences: s.lockUploadReferences}, nil
}

func (s *Server) resolveUploads(ctx context.Context, id string, mutation bool) (conversation.UploadTarget, error) {
	unavailable := func() (conversation.UploadTarget, error) {
		return conversation.UploadTarget{}, &conversation.UploadError{Status: 404, Message: "Upload scope is unavailable"}
	}
	if strings.HasPrefix(id, "s-") {
		conversationID := strings.TrimPrefix(id, "s-")
		slug, _, valid := parseTeamConversationID(conversationID)
		if !valid {
			return unavailable()
		}
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			return unavailable()
		}
		kind, _, err := s.workflowPageBackend(summary)
		if err != nil {
			return unavailable()
		}
		if kind == agentbackend.Pi {
			target, err := s.resolvePiBackend(ctx, conversation.ResolveRequest{ID: conversationID, Operation: "uploads", Mutation: mutation})
			if err != nil {
				return unavailable()
			}
			store, ok := target.Attachments.(*uploads.Backend)
			if !ok {
				if target.Release != nil {
					target.Release()
				}
				return unavailable()
			}
			return conversation.UploadTarget{Store: store, Writable: target.Capabilities.Send, Release: target.Release}, nil
		}
		target, err := s.resolveConversation(ctx, conversation.ResolveRequest{ID: conversationID, Operation: "uploads", Mutation: mutation})
		if err != nil {
			return unavailable()
		}
		backend, err := s.sessionUploads(ctx, slug, target.ThreadID, !target.Capabilities.Send)
		if err != nil {
			if target.Release != nil {
				target.Release()
			}
			return conversation.UploadTarget{}, err
		}
		backend.BaseURL = "/uploads/s-" + conversationID
		return conversation.UploadTarget{Store: backend, Writable: target.Capabilities.Send, Release: target.Release}, nil
	}
	if !strings.HasPrefix(id, "d-") {
		return unavailable()
	}
	var release func()
	if mutation {
		var err error
		release, err = s.lockTransitionContext(ctx)
		if err != nil {
			return conversation.UploadTarget{}, err
		}
		if err = s.requireCurrentHostProfile(); err != nil {
			release()
			return conversation.UploadTarget{}, err
		}
	}
	scope, err := s.uploadStore.Scope(ctx, strings.TrimPrefix(id, "d-"))
	if err != nil || !scope.Draft || scope.Deleted {
		if release != nil {
			release()
		}
		return unavailable()
	}
	backend := &uploads.Backend{Store: s.uploadStore, ScopeID: scope.ID, BaseURL: "/uploads/" + id, CheckIdle: s.uploadReferencesIdle, LockReferences: s.lockUploadReferences}
	return conversation.UploadTarget{Store: backend, Writable: true, Release: release}, nil
}

func (s *Server) newUploadDraft(w http.ResponseWriter, r *http.Request) {
	scope, err := s.uploadStore.NewDraft(r.Context())
	if err != nil {
		s.writeError(w, r, 500, "Unable to create an upload draft")
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]string{"id": scope.ID, "url": "/uploads/d-" + scope.ID})
}

func (s *Server) prepareCreationAttachments(ctx context.Context, slug, scopeID, text string, ids []string) (string, error) {
	if len(ids) == 0 {
		return text, nil
	}
	scope, err := s.uploadStore.Scope(ctx, scopeID)
	if err != nil {
		return "", err
	}
	// An accepted creation can be replayed after its draft has been adopted. Its
	// exact immutable initial prompt is still checked by Prepare and acceptCreation.
	if scope.Slug != "" && scope.Slug != slug {
		return "", errors.New("attachment draft belongs to another session")
	}
	backend := &uploads.Backend{Store: s.uploadStore, ScopeID: scopeID}
	encoded, _ := json.Marshal([]any{text, ids})
	hash := sha256.Sum256(encoded)
	return backend.Prepare(ctx, "initial", hex.EncodeToString(hash[:]), text, ids)
}

func (s *Server) bindCreationUploads(ctx context.Context, receipt creationReceipt) error {
	if receipt.Request.SourceFork != nil {
		if _, err := s.admitAcceptedForkExecution(ctx, receipt); err != nil {
			return err
		}
		if err := s.proveCreation(receipt); err != nil {
			return err
		}
	}
	if err := s.bindCreationNotificationPreference(receipt); err != nil {
		return err
	}
	summary, err := session.Find(s.config.Workspace, receipt.Request.Slug)
	if err != nil {
		return err
	}
	epoch, err := session.CompletedRemovalHistory(s.config.Workspace, summary.Slug, s.config.UserStateRoot)
	if err != nil {
		return err
	}
	if receipt.Request.Backend == "pi" {
		binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(summary.Slug)
		if err != nil {
			return err
		}
		ref, err := binding.Resolve("lead")
		if err != nil || receipt.Pi == nil || ref.Session.Incarnation != receipt.Pi.SessionIncarnation {
			return agentbackend.ErrBindingConflict
		}
		return s.uploadStore.BindCreation(ctx, receipt.Goal, summary.Slug, piUploadThread(ref), epoch)
	}
	if receipt.Request.Kind != "fork" {
		return s.uploadStore.BindCreation(ctx, receipt.Goal, summary.Slug, summary.Codex.ThreadID, epoch)
	}
	sourceThread := receipt.Request.SourceThreadID
	if receipt.Request.SourceFork != nil {
		sourceThread = receipt.Request.SourceFork.ThreadID
	}
	hasFiles, err := s.uploadStore.HasThread(ctx, sourceThread)
	if err != nil || !hasFiles {
		return err
	}
	backend, err := s.sessionUploads(ctx, summary.Slug, summary.Codex.ThreadID, false)
	if err != nil {
		return err
	}
	target, err := s.uploadStore.Scope(ctx, backend.ScopeID)
	if err != nil {
		return err
	}
	transcript, err := s.config.Codex.ReadThread(ctx, summary.Codex.ThreadID)
	if err != nil {
		return err
	}
	return s.uploadStore.Fork(ctx, sourceThread, target, transcript.Entries)
}

func (s *Server) uploadReferencesIdle(ctx context.Context, scopes []uploads.Scope) error {
	for _, scope := range scopes {
		if strings.HasPrefix(scope.Thread, "pi:") {
			summary, err := session.Find(s.config.Workspace, scope.Slug)
			if err != nil {
				return &conversation.UploadError{Status: 409, Message: "A referencing session could not be verified"}
			}
			_, binding, err := s.workflowPageBackend(summary)
			if err != nil || binding == nil {
				return &conversation.UploadError{Status: 409, Message: "A referencing session changed"}
			}
			matched := false
			// Current Pi scopes are derived from trusted member incarnations.
			for _, member := range binding.Conversations {
				ref, e := binding.Resolve(member.Address)
				if e != nil || piUploadThread(ref) != scope.Thread {
					continue
				}
				matched = true
				err = s.withPiSessionRead(ctx, ref.Session, func(current piSessionAdmission) error {
					handle := agentbackend.ExecutionHandle{Conversation: ref, Generation: current.Authority.Generation}
					ops, e := s.config.PiCreation.Runtime.Bind(ctx, handle)
					if e != nil || ops.Read == nil {
						return &conversation.UploadError{Status: 409, Message: "Pi file use is unavailable"}
					}
					transcript, e := ops.Read.Read(ctx, piruntime.BackendIdentity(handle))
					if e != nil || transcript.Status != "idle" {
						return &conversation.UploadError{Status: 409, Message: "Wait until Pi is idle before deleting this file"}
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			if !matched {
				return &conversation.UploadError{Status: 409, Message: "A referencing Pi member changed"}
			}
			continue
		}
		if s.config.Codex == nil {
			return &conversation.UploadError{Status: 409, Message: "File use is unavailable"}
		}
		if scope.Thread == "" {
			return &conversation.UploadError{Status: 409, Message: "Session creation has not finished"}
		}
		summary, err := session.Find(s.config.Workspace, scope.Slug)
		if err != nil || summary.Codex.ThreadID != scope.Thread {
			return &conversation.UploadError{Status: 409, Message: "A referencing session could not be verified"}
		}
		pending, err := session.PendingLifecycle(s.config.Workspace, scope.Slug)
		if err != nil || pending != "" {
			return &conversation.UploadError{Status: 409, Message: "A referencing session is changing"}
		}
		transcript, err := s.config.Codex.ReadThread(ctx, scope.Thread)
		if err != nil || transcript.Status == "active" {
			return &conversation.UploadError{Status: 409, Message: fmt.Sprintf("Wait until %s is idle before deleting this file", scope.Slug)}
		}
		if !summary.Archived {
			queued, err := s.config.Codex.ListQueue(ctx, scope.Thread)
			if err != nil || len(queued) > 0 {
				return &conversation.UploadError{Status: 409, Message: fmt.Sprintf("Resolve queued input in %s before deleting this file", scope.Slug)}
			}
		}
	}
	return nil
}

func (s *Server) retainCreationUploads(ctx context.Context) error {
	s.operationMu.Lock()
	goals := map[string]uploads.Scope{}
	for _, receipt := range s.creations {
		owner := uploads.Scope{Slug: receipt.Request.Slug, Epoch: receipt.DeletionHistorySHA256, Deleted: receipt.State == "conflict"}
		for _, goal := range []string{receipt.Request.Goal, receipt.Goal} {
			if previous, exists := goals[goal]; exists && !previous.Deleted && owner.Deleted {
				continue
			}
			goals[goal] = owner
		}
	}
	s.operationMu.Unlock()
	return s.uploadStore.RetainCreations(ctx, goals)
}

func (s *Server) collectUploads(ctx context.Context) error {
	_, unlockTransition, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlockTransition()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if err := s.uploadCreationMu.Lock(ctx); err != nil {
		return err
	}
	defer s.uploadCreationMu.Unlock()
	s.operationMu.Lock()
	err = s.reconcilePreparationsLocked(ctx)
	s.operationMu.Unlock()
	if err != nil {
		return err
	}
	if err := s.retainCreationUploads(ctx); err != nil {
		return err
	}
	scopes, err := s.uploadStore.Scopes(ctx)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		if scope.Deleted || scope.Slug == "" {
			continue
		}
		lock, err := session.LockRuntimeShared(s.config.AuthorityDir, scope.Slug)
		if err != nil {
			continue
		}
		epoch, err := session.CompletedRemovalHistory(s.config.Workspace, scope.Slug, s.config.UserStateRoot)
		if err == nil && epoch != scope.Epoch {
			err = s.uploadStore.RemoveSession(ctx, scope.Slug, scope.Thread, scope.Epoch)
		}
		lock.Close()
		if err != nil {
			return err
		}
	}
	return s.uploadStore.Collect(ctx)
}

func (s *Server) startUploadCollector() {
	if !s.config.CollectUploads {
		return
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(s.operationContext, 30*time.Second)
			if err := s.collectUploads(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.Printf("collect uploads: %v", err)
			}
			cancel()
			select {
			case <-s.operationContext.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) lockUploadReferences(ctx context.Context, scopes []uploads.Scope) (func(), error) {
	slugs := map[string]bool{}
	for _, scope := range scopes {
		if scope.Slug == "" {
			continue
		}
		if strings.HasPrefix(scope.Thread, "pi:") {
			binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(scope.Slug)
			if err != nil {
				return nil, err
			}
			matched := false
			for _, member := range binding.Conversations {
				ref, err := binding.Resolve(member.Address)
				if err == nil && piUploadThread(ref) == scope.Thread {
					id := scope.Slug
					if member.Address != "lead" {
						id += "~" + member.Address
					}
					slugs[id] = true
					matched = true
				}
			}
			if !matched {
				return nil, agentbackend.ErrBindingConflict
			}
		} else {
			slugs[scope.Slug] = true
		}
	}
	ordered := []string{}
	for slug := range slugs {
		ordered = append(ordered, slug)
	}
	sort.Strings(ordered)
	locks := []conversation.MutationLocker{}
	release := func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
	for _, scope := range scopes {
		if scope.Draft {
			if err := s.uploadCreationMu.Lock(ctx); err != nil {
				return nil, err
			}
			locks = append(locks, s.uploadCreationMu)
			// Acceptance may have persisted before a failed catalog update.
			// Reconcile it under the same lock before allowing draft deletion.
			if err := s.retainCreationUploads(ctx); err != nil {
				release()
				return nil, err
			}
			break
		}
	}
	for _, slug := range ordered {
		lock := s.messageLock(slug)
		if err := lock.Lock(ctx); err != nil {
			release()
			return nil, err
		}
		locks = append(locks, lock)
	}
	return release, nil
}

// File downloads cannot set custom headers. Reuse the existing opaque scope
// query transport for that GET only; all mutations retain the header contract.
func (s *Server) serveUploads(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/uploads/s-") {
		id := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/uploads/s-"), "/", 2)[0]
		slug, _, valid := parseTeamConversationID(id)
		if valid {
			summary, err := session.Find(s.config.Workspace, slug)
			if err == nil {
				kind, _, err := s.workflowPageBackend(summary)
				if err == nil && kind == agentbackend.Pi {
					scoped, err := scopedPiConversationRequest(r, r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content"))
					if err != nil {
						http.Error(w, "File scope changed", http.StatusConflict)
						return
					}
					r = scoped
				}
			}
		}
	}
	s.uploadHandler.ServeHTTP(w, r)
}
