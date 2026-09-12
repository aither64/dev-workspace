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
	ID, Review, Slug, Scope, CommitSHA string
	Repo                               repository.ReviewRepository
	Pair                               repository.ReviewPair
	Created                            time.Time
	mu                                 sync.Mutex
	Commits                            map[string]string
	Branch                             bool
}
type repositoryReviewService struct {
	reader        repository.ReviewReader
	lifetime      context.Context
	directory     string
	mu            sync.Mutex
	snapshots     map[string]*repositoryReviewSnapshot
	requests      chan struct{}
	cacheMu       sync.Mutex
	cache         map[string]*list.Element
	cacheLRU      *list.List
	cacheBytes    int
	calls         map[string]*reviewCacheCall
	discoveryMu   sync.Mutex
	discovery     map[string][]session.Repository
	discoveryErr  error
	discoveryAt   time.Time
	discoveryWait chan struct{}
}

func (s *Server) reviews() *repositoryReviewService {
	s.reviewOnce.Do(func() {
		service := &repositoryReviewService{lifetime: s.operationContext, reader: repository.ReviewReader{Workspace: s.config.Workspace, Jobs: make(chan struct{}, 4)}, directory: filepath.Join(s.operationStore.directory, "repository-comparisons"), snapshots: make(map[string]*repositoryReviewSnapshot), requests: make(chan struct{}, 4)}
		service.initializeCache()
		s.reviewService = service
	})
	return s.reviewService
}
func (s *repositoryReviewService) put(snapshot *repositoryReviewSnapshot) (*repositoryReviewSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.snapshots {
		if existing.Review == snapshot.Review && existing.Scope == snapshot.Scope && existing.CommitSHA == snapshot.CommitSHA {
			existing.Created = time.Now()
			return existing, nil
		}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	snapshot.ID = hex.EncodeToString(token[:])
	snapshot.Created = time.Now()
	snapshot.Commits = make(map[string]string)
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
	return snapshot, nil
}
func (s *repositoryReviewService) get(id, slug, repo, scope string) *repositoryReviewSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.snapshots[id]
	if snapshot == nil || snapshot.Slug != slug || snapshot.Repo.ID != repo || snapshot.Scope != scope {
		return nil
	}
	snapshot.Created = time.Now()
	return snapshot
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
func (s *Server) writeReviewError(w http.ResponseWriter, summary *session.Summary, err error) {
	status, message := reviewErrorMessage(err)
	s.config.Logger.Printf("repository review %s: %v", summary.Slug, err)
	s.writeJSON(w, status, map[string]string{"error": message})
}

type reviewHistoryResponse struct {
	Repository string                   `json:"repository,omitempty"`
	Review     string                   `json:"review"`
	Snapshot   string                   `json:"snapshot"`
	Pair       repository.ReviewPair    `json:"pair"`
	History    repository.ReviewHistory `json:"history"`
}
type reviewComparisonResponse struct {
	Review      string                   `json:"review"`
	Snapshot    string                   `json:"snapshot"`
	HistoryHead string                   `json:"historyHead"`
	Pair        repository.ReviewPair    `json:"pair"`
	Name        string                   `json:"name"`
	Commit      *repository.ReviewCommit `json:"commit"`
	Stats       repository.ReviewStats   `json:"stats"`
	Files       []repository.ReviewFile  `json:"files"`
}

func (s *Server) repositoryReviewAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary, parts []string) bool {
	if len(parts) != 2 {
		return false
	}
	operation := parts[1]
	switch operation {
	case "repository-history", "repository-comparison", "repository-file", "repository-state":
	default:
		return false
	}
	allow := "GET"
	if operation == "repository-comparison" {
		allow = "GET, POST"
	}
	if r.Method != http.MethodGet && !(operation == "repository-comparison" && r.Method == http.MethodPost) {
		w.Header().Set("Allow", allow)
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Unsupported review request method"})
		return true
	}
	// A GET without a durable review ID was not a comparison operation before
	// durable links; retain its old method response for existing clients.
	if operation == "repository-comparison" && r.Method == http.MethodGet && r.URL.Query().Get("review") == "" {
		w.Header().Set("Allow", "POST")
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "A comparison link requires a review ID."})
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	select {
	case s.reviews().requests <- struct{}{}:
		defer func() { <-s.reviews().requests }()
	case <-ctx.Done():
		s.writeReviewError(w, summary, ctx.Err())
		return true
	}
	repoID := r.URL.Query().Get("repository")
	registration, ok := s.reviewRegistration(ctx, summary, repoID)
	if !ok {
		s.writeReviewError(w, summary, reviewError(404, "Repository is not registered in this session"))
		return true
	}
	scope, err := repositoryReviewScope(summary, registration)
	if err != nil {
		s.writeReviewError(w, summary, err)
		return true
	}
	service := s.reviews()
	if operation == "repository-state" {
		repo, err := service.reader.Resolve(ctx, summary.Slug, registration, summary.Archived)
		if err != nil {
			s.writeReviewError(w, summary, err)
		} else {
			s.writeJSON(w, 200, map[string]string{"head": repo.Head})
		}
		return true
	}
	if operation == "repository-history" {
		page := 0
		if raw := r.URL.Query().Get("page"); raw != "" {
			parsed, e := strconv.Atoi(raw)
			if e != nil || parsed < 0 || parsed > 2000 {
				s.writeReviewError(w, summary, reviewError(400, "Invalid history page"))
				return true
			}
			page = parsed
		}
		payload, err := s.reviewHistory(ctx, summary, registration, scope, r.URL.Query().Get("snapshot"), page)
		if err != nil {
			s.writeReviewError(w, summary, err)
		} else {
			s.writeJSON(w, 200, payload)
		}
		return true
	}
	snapshot := service.get(r.URL.Query().Get("snapshot"), summary.Slug, repoID, scope)
	if operation == "repository-file" {
		if snapshot == nil {
			s.writeReviewError(w, summary, reviewError(409, "This review expired. Reopen its comparison link."))
			return true
		}
		content, err := service.fileContent(ctx, snapshot, r.URL.Query().Get("file"))
		if err != nil {
			s.writeReviewError(w, summary, err)
		} else {
			s.writeJSON(w, 200, content)
		}
		return true
	}
	var commitSHA, fileID string
	if r.Method == http.MethodPost {
		var body struct {
			Snapshot string `json:"snapshot"`
			Commit   string `json:"commit,omitempty"`
			File     string `json:"file,omitempty"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			s.writeReviewError(w, summary, reviewError(400, "Invalid comparison request"))
			return true
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			s.writeReviewError(w, summary, reviewError(400, "Invalid comparison request"))
			return true
		}
		snapshot = service.get(body.Snapshot, summary.Slug, repoID, scope)
		if snapshot == nil {
			s.writeReviewError(w, summary, reviewError(409, "This review expired. Reload the repository history to open it again."))
			return true
		}
		if body.Commit != "" {
			snapshot.mu.Lock()
			commitSHA = snapshot.Commits[body.Commit]
			snapshot.mu.Unlock()
			if commitSHA == "" {
				s.writeReviewError(w, summary, reviewError(400, "Commit was not listed in this review"))
				return true
			}
		} else {
			commitSHA = snapshot.CommitSHA
		}
		fileID = body.File
	} else {
		snapshot, err = s.restoreReview(ctx, summary, registration, scope, r.URL.Query().Get("review"))
		if err != nil {
			s.writeReviewError(w, summary, err)
			return true
		}
		commitSHA = r.URL.Query().Get("commit")
		fileID = r.URL.Query().Get("file")
	}
	payload, err := s.reviewComparison(ctx, summary, registration, snapshot, commitSHA, fileID)
	if err != nil {
		s.writeReviewError(w, summary, err)
	} else {
		s.writeJSON(w, 200, payload)
	}
	return true
}

func (s *Server) reviewHistory(ctx context.Context, summary *session.Summary, registration session.Repository, scope, snapshotID string, page int) (reviewHistoryResponse, error) {
	service := s.reviews()
	repoID := repository.ReviewID(registration.Name)
	snapshot := service.get(snapshotID, summary.Slug, repoID, scope)
	if snapshot == nil {
		if snapshotID != "" {
			return reviewHistoryResponse{}, reviewError(409, "This review expired. Reload the repository history to open it again.")
		}
		repo, err := service.reader.Resolve(ctx, summary.Slug, registration, summary.Archived)
		if err != nil {
			return reviewHistoryResponse{}, err
		}
		saved, err := service.loadComparison(summary.Slug, repoID, repo.Head)
		if err != nil {
			return reviewHistoryResponse{}, err
		}
		pair, err := service.reader.Pair(ctx, repo, saved)
		if err != nil {
			return reviewHistoryResponse{}, err
		}
		review, err := service.saveReview(summary.Slug, repoID, scope, pair)
		if err != nil {
			return reviewHistoryResponse{}, err
		}
		snapshot, err = service.put(&repositoryReviewSnapshot{Review: review, Slug: summary.Slug, Scope: scope, Repo: repo, Pair: pair, Branch: true})
		if err != nil {
			return reviewHistoryResponse{}, err
		}
	}
	history, err := cachedReview(ctx, service, fmt.Sprintf("history\x00%s\x00%s\x00%s\x00%s\x00%d", snapshot.Repo.Directory, snapshot.Repo.GitHub, snapshot.Pair.Base, snapshot.Pair.Head, page), func(ctx context.Context) (repository.ReviewHistory, error) {
		return service.reader.History(ctx, snapshot.Repo, snapshot.Pair, page)
	})
	if err != nil {
		return reviewHistoryResponse{}, err
	}
	if snapshot.Branch {
		if err := service.saveComparison(summary.Slug, repoID, snapshot.Pair); err != nil {
			return reviewHistoryResponse{}, err
		}
	}
	snapshot.mu.Lock()
	if len(snapshot.Commits)+len(history.Commits) > 2000 {
		clear(snapshot.Commits)
	}
	for _, commit := range history.Commits {
		snapshot.Commits[commit.ID] = commit.SHA
	}
	snapshot.mu.Unlock()
	return reviewHistoryResponse{Review: snapshot.Review, Snapshot: snapshot.ID, Pair: snapshot.Pair, History: history}, nil
}

func (s *Server) restoreReview(ctx context.Context, summary *session.Summary, registration session.Repository, scope, id string) (*repositoryReviewSnapshot, error) {
	service := s.reviews()
	saved, err := service.loadReview(summary.Slug, repository.ReviewID(registration.Name), scope, id)
	if err != nil {
		return nil, err
	}
	// Verify saved objects even if a matching memory cache survives a local prune.
	repo, err := service.reader.Restore(ctx, summary.Slug, registration, saved.Pair)
	if err != nil {
		return nil, err
	}
	return service.put(&repositoryReviewSnapshot{Review: id, Slug: summary.Slug, Scope: scope, Repo: repo, Pair: saved.Pair, Branch: true})
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

func (s *Server) reviewComparison(ctx context.Context, summary *session.Summary, registration session.Repository, snapshot *repositoryReviewSnapshot, commitSHA, fileID string) (reviewComparisonResponse, error) {
	service := s.reviews()
	historyHead := snapshot.Pair.Head
	var commit *repository.ReviewCommit
	if commitSHA != "" {
		if !repositoryObjectID(commitSHA) {
			return reviewComparisonResponse{}, reviewError(400, "Invalid comparison commit")
		}
		// Membership is against the durable branch pair, never a moving branch ref.
		saved, err := service.loadReview(summary.Slug, snapshot.Repo.ID, snapshot.Scope, snapshot.Review)
		if err != nil {
			return reviewComparisonResponse{}, err
		}
		historyHead = saved.Pair.Head
		item, err := cachedReview(ctx, service, "commit\x00"+snapshot.Repo.Directory+"\x00"+snapshot.Repo.GitHub+"\x00"+saved.Pair.Base+"\x00"+saved.Pair.Head+"\x00"+commitSHA, func(ctx context.Context) (repository.ReviewCommit, error) {
			return service.reader.ComparisonCommit(ctx, snapshot.Repo, saved.Pair, commitSHA)
		})
		if err != nil {
			return reviewComparisonResponse{}, err
		}
		commit = &item
		pair, err := service.reader.CommitPair(ctx, snapshot.Repo, item)
		if err != nil {
			return reviewComparisonResponse{}, err
		}
		snapshot, err = service.put(&repositoryReviewSnapshot{Review: snapshot.Review, Slug: summary.Slug, Scope: snapshot.Scope, Repo: snapshot.Repo, Pair: pair, CommitSHA: item.SHA})
		if err != nil {
			return reviewComparisonResponse{}, err
		}
	}
	files, err := service.comparisonFiles(ctx, snapshot)
	if err != nil {
		return reviewComparisonResponse{}, err
	}
	if snapshot.Branch {
		if err := service.saveComparison(summary.Slug, snapshot.Repo.ID, snapshot.Pair); err != nil {
			return reviewComparisonResponse{}, err
		}
	}
	response := reviewComparisonResponse{Review: snapshot.Review, Snapshot: snapshot.ID, HistoryHead: historyHead, Pair: snapshot.Pair, Name: registration.Name, Commit: commit, Stats: repository.FileStats(files), Files: files}
	if fileID != "" {
		found := false
		for _, file := range files {
			if file.ID == fileID {
				found = true
				break
			}
		}
		if !found {
			return response, reviewError(404, "File is not part of this comparison")
		}
	}
	return response, nil
}
