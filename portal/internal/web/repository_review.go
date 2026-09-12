package web

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type repositoryReviewSnapshot struct {
	ID      string
	Slug    string
	Repo    repository.ReviewRepository
	Pair    repository.ReviewPair
	Created time.Time
	mu      sync.Mutex
	Commits map[string]repository.ReviewCommit
	Branch  bool
}
type repositoryReviewService struct {
	reader        repository.ReviewReader
	directory     string
	mu            sync.Mutex
	snapshots     map[string]*repositoryReviewSnapshot
	requests      chan struct{}
	discoveryMu   sync.Mutex
	discovery     map[string][]session.Repository
	discoveryErr  error
	discoveryAt   time.Time
	discoveryWait chan struct{}
	lifetime      context.Context
	cacheMu       sync.Mutex
	cache         map[string]*list.Element
	cacheLRU      *list.List
	cacheBytes    int
	calls         map[string]*reviewCacheCall
}

func (s *Server) reviews() *repositoryReviewService {
	s.reviewOnce.Do(func() {
		s.reviewService = &repositoryReviewService{lifetime: s.operationContext, reader: repository.ReviewReader{Workspace: s.config.Workspace, Jobs: make(chan struct{}, 4)}, directory: filepath.Join(s.operationStore.directory, "repository-comparisons"), snapshots: make(map[string]*repositoryReviewSnapshot), requests: make(chan struct{}, 4)}
		s.reviewService.initializeCache()
	})
	return s.reviewService
}
func (s *repositoryReviewService) put(snapshot *repositoryReviewSnapshot) error {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	snapshot.ID = hex.EncodeToString(token[:])
	snapshot.Created = time.Now()
	snapshot.Commits = make(map[string]repository.ReviewCommit)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Retain immutable open views through status refreshes. Bound process memory;
	// evicted views report an explicit reload message, never switch to a new head.
	if len(s.snapshots) >= 32 {
		var oldest *repositoryReviewSnapshot
		for _, item := range s.snapshots {
			if oldest == nil || item.Created.Before(oldest.Created) {
				oldest = item
			}
		}
		delete(s.snapshots, oldest.ID)
	}
	s.snapshots[snapshot.ID] = snapshot
	return nil
}
func (s *repositoryReviewService) get(id, slug, repo string) *repositoryReviewSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snapshots[id]
	if snap == nil || snap.Slug != slug || snap.Repo.ID != repo {
		return nil
	}
	return snap
}
func reviewToken(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Server) reviewRegistration(ctx context.Context, summary *session.Summary, id string) (session.Repository, bool) {
	if !reviewToken(id) {
		return session.Repository{}, false
	}
	// Explicit registrations do not depend on unrelated repositories. Resolve
	// verifies their canonical identity when opening or refreshing a new view.
	for _, item := range summary.Repositories {
		if repository.ReviewID(item.Name) == id {
			return item, true
		}
	}
	if summary.Archived {
		return session.Repository{}, false
	}
	service := s.reviews()
	service.discoveryMu.Lock()
	cached := !service.discoveryAt.IsZero() && time.Since(service.discoveryAt) < 5*time.Second
	service.discoveryMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		discovered, err := s.discoverRepositories(ctx, attempt > 0)
		if err != nil {
			return session.Repository{}, false
		}
		merged, err := session.MergeActiveRepositories(summary.Repositories, discovered[summary.Slug])
		if err != nil {
			return session.Repository{}, false
		}
		for _, item := range merged {
			if repository.ReviewID(item.Name) == id {
				// A cached discovery entry is not authorization for a removed or retargeted
				// worktree. Check only this worktree, preserving discovered-only visibility.
				if _, err := service.reader.Resolve(ctx, summary.Slug, item, false); err != nil {
					return session.Repository{}, false
				}
				return item, true
			}
		}
		if !cached {
			break
		}
	}
	return session.Repository{}, false
}
func (s *Server) repositoryReviewAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary, parts []string) bool {
	if len(parts) != 2 {
		return false
	}
	operation := parts[1]
	if operation != "repository-history" && operation != "repository-comparison" && operation != "repository-file" && operation != "repository-state" {
		return false
	}
	method := http.MethodGet
	if operation == "repository-comparison" {
		method = http.MethodPost
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Unsupported review request method"})
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	service := s.reviews()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	select {
	case service.requests <- struct{}{}:
		defer func() { <-service.requests }()
	case <-ctx.Done():
		return true
	}
	repoID := r.URL.Query().Get("repository")
	registration, ok := s.reviewRegistration(ctx, summary, repoID)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "Repository is not registered in this session"})
		return true
	}
	fail := func(err error) {
		s.config.Logger.Printf("repository review %s/%s: %v", summary.Slug, registration.Name, err)
		status, message := reviewErrorMessage(err)
		s.writeJSON(w, status, map[string]string{"error": message})
	}
	if operation == "repository-state" {
		repo, err := service.reader.Resolve(ctx, summary.Slug, registration, summary.Archived)
		if err != nil {
			fail(err)
			return true
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"head": repo.Head})
		return true
	}
	snapshotID := r.URL.Query().Get("snapshot")
	snapshot := service.get(snapshotID, summary.Slug, repoID)
	if operation == "repository-comparison" {
		var body struct {
			Snapshot string `json:"snapshot"`
			Commit   string `json:"commit,omitempty"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid comparison request"})
			return true
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid comparison request"})
			return true
		}
		snapshot = service.get(body.Snapshot, summary.Slug, repoID)
		if snapshot == nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This review expired. Reload the repository history to open it again."})
			return true
		}
		if body.Commit != "" {
			snapshot.mu.Lock()
			commit, found := snapshot.Commits[body.Commit]
			snapshot.mu.Unlock()
			if !found {
				s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Commit was not listed in this review"})
				return true
			}
			pair, err := service.reader.CommitPair(ctx, snapshot.Repo, commit)
			if err != nil {
				fail(err)
				return true
			}
			snapshot = &repositoryReviewSnapshot{Slug: summary.Slug, Repo: snapshot.Repo, Pair: pair}
			if err := service.put(snapshot); err != nil {
				fail(err)
				return true
			}
		}
		files, err := service.comparisonFiles(ctx, snapshot)
		if err != nil {
			fail(err)
			return true
		}
		if snapshot.Branch {
			if err := service.saveComparison(summary.Slug, repoID, snapshot.Pair); err != nil {
				fail(err)
				return true
			}
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot.ID, "pair": snapshot.Pair, "files": files, "name": registration.Name})
		return true
	}
	if operation == "repository-file" {
		if snapshot == nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This review expired. Reload the repository history to open it again."})
			return true
		}
		content, err := service.fileContent(ctx, snapshot, r.URL.Query().Get("file"))
		if err != nil {
			fail(err)
			return true
		}
		s.writeJSON(w, http.StatusOK, content)
		return true
	}
	page := 0
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 2000 {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid history page"})
			return true
		}
		page = parsed
	}
	if snapshot == nil {
		if snapshotID != "" {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This review expired. Reload the repository history to open it again."})
			return true
		}
		repo, err := service.reader.Resolve(ctx, summary.Slug, registration, summary.Archived)
		if err != nil {
			fail(err)
			return true
		}
		saved, err := service.loadComparison(summary.Slug, repoID, repo.Head)
		if err != nil {
			fail(err)
			return true
		}
		pair, err := service.reader.Pair(ctx, repo, saved)
		if err != nil {
			fail(err)
			return true
		}
		snapshot = &repositoryReviewSnapshot{Slug: summary.Slug, Repo: repo, Pair: pair, Branch: true}
		if err := service.put(snapshot); err != nil {
			fail(err)
			return true
		}
	}
	history, err := cachedReview(ctx, service, fmt.Sprintf("history\x00%s\x00%s\x00%s\x00%s\x00%d", snapshot.Repo.Directory, snapshot.Repo.GitHub, snapshot.Pair.Base, snapshot.Pair.Head, page), func(ctx context.Context) (repository.ReviewHistory, error) {
		return service.reader.History(ctx, snapshot.Repo, snapshot.Pair, page)
	})
	if err != nil {
		fail(err)
		return true
	}
	if snapshot.Branch {
		if err := service.saveComparison(summary.Slug, repoID, snapshot.Pair); err != nil {
			fail(err)
			return true
		}
	}
	snapshot.mu.Lock()
	if len(snapshot.Commits)+len(history.Commits) > 2000 {
		clear(snapshot.Commits)
	}
	for _, commit := range history.Commits {
		snapshot.Commits[commit.ID] = repository.ReviewCommit{SHA: commit.SHA, Parents: commit.Parents}
	}
	snapshot.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot.ID, "pair": snapshot.Pair, "history": history})
	return true
}

type reviewAPIError struct {
	status  int
	message string
}

func (e *reviewAPIError) Error() string { return e.message }
func reviewError(status int, message string) error {
	return &reviewAPIError{status: status, message: message}
}
func reviewErrorMessage(err error) (int, string) {
	var api *reviewAPIError
	if errors.As(err, &api) {
		return api.status, api.message
	}
	if errors.Is(err, repository.ErrReviewLimit) {
		return http.StatusUnprocessableEntity, "This comparison exceeds the review limit. Review a smaller commit locally."
	}
	return http.StatusUnprocessableEntity, "This comparison is unavailable. Check that its repository and Git objects are still present locally."
}

func (service *repositoryReviewService) comparisonFiles(ctx context.Context, snapshot *repositoryReviewSnapshot) ([]repository.ReviewFile, error) {
	return cachedReview(ctx, service, "files\x00"+snapshot.Repo.Directory+"\x00"+snapshot.Pair.Base+"\x00"+snapshot.Pair.Head, func(ctx context.Context) ([]repository.ReviewFile, error) {
		return service.reader.Files(ctx, snapshot.Repo, snapshot.Pair)
	})
}

func (service *repositoryReviewService) fileContent(ctx context.Context, snapshot *repositoryReviewSnapshot, id string) (repository.ReviewContent, error) {
	files, err := service.comparisonFiles(ctx, snapshot)
	if err != nil {
		return repository.ReviewContent{}, err
	}
	for _, file := range files {
		if file.ID == id {
			key := "content\x00" + snapshot.Repo.Directory + "\x00" + file.OldObject + "\x00" + file.NewObject + "\x00" + file.OldMode + "\x00" + file.NewMode
			return cachedReview(ctx, service, key, func(ctx context.Context) (repository.ReviewContent, error) {
				return service.reader.Content(ctx, snapshot.Repo, file)
			})
		}
	}
	return repository.ReviewContent{}, reviewError(404, "File is not part of this comparison")
}
