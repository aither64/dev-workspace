package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	ID          string
	Slug        string
	Repo        repository.ReviewRepository
	Pair        repository.ReviewPair
	Created     time.Time
	mu          sync.Mutex
	Commits     map[string]repository.ReviewCommit
	Files       []repository.ReviewFile
	FilesLoaded bool
	Branch      bool
}
type repositoryReviewService struct {
	reader    repository.ReviewReader
	directory string
	mu        sync.Mutex
	snapshots map[string]*repositoryReviewSnapshot
	slots     chan struct{}
}

func (s *Server) reviews() *repositoryReviewService {
	s.reviewOnce.Do(func() {
		s.reviewService = &repositoryReviewService{reader: repository.ReviewReader{Workspace: s.config.Workspace}, directory: filepath.Join(s.operationStore.directory, "repository-comparisons"), snapshots: make(map[string]*repositoryReviewSnapshot), slots: make(chan struct{}, 4)}
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
func (s *Server) reviewRegistration(ctx context.Context, summary *session.Summary, id string) (session.Repository, bool) {
	repositories := summary.Repositories
	if !summary.Archived {
		if discovered, err := session.ActiveRepositoriesContext(ctx, s.config.Workspace, summary.Slug, repositories); err == nil {
			repositories = discovered
		}
	}
	for _, item := range repositories {
		if repository.ReviewID(item.Name) == id {
			return item, true
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
	select {
	case service.slots <- struct{}{}:
		defer func() { <-service.slots }()
	case <-r.Context().Done():
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	repoID := r.URL.Query().Get("repository")
	registration, ok := s.reviewRegistration(ctx, summary, repoID)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "Repository is not registered in this session"})
		return true
	}
	fail := func(err error) {
		s.config.Logger.Printf("repository review %s/%s: %v", summary.Slug, registration.Name, err)
		message := "Local repository review is unavailable. Check that the registered worktree and Git objects are present."
		if errors.Is(err, repository.ErrReviewLimit) {
			message = "This comparison exceeds the review limit. Review a smaller commit locally."
		}
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": message})
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
		snapshot.mu.Lock()
		defer snapshot.mu.Unlock()
		if !snapshot.FilesLoaded {
			files, err := service.reader.Files(ctx, snapshot.Repo, snapshot.Pair)
			if err != nil {
				fail(err)
				return true
			}
			snapshot.Files = files
			snapshot.FilesLoaded = true
		}
		if snapshot.Branch {
			if err := service.saveComparison(summary.Slug, repoID, snapshot.Pair); err != nil {
				fail(err)
				return true
			}
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot.ID, "pair": snapshot.Pair, "files": snapshot.Files, "name": registration.Name})
		return true
	}
	if operation == "repository-file" {
		if snapshot == nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "This review expired. Reload the repository history to open it again."})
			return true
		}
		snapshot.mu.Lock()
		var selected *repository.ReviewFile
		for _, file := range snapshot.Files {
			if file.ID == r.URL.Query().Get("file") {
				copy := file
				selected = &copy
				break
			}
		}
		snapshot.mu.Unlock()
		if selected == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "File is not part of this comparison"})
			return true
		}
		content, err := service.reader.Content(ctx, snapshot.Repo, *selected)
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
	history, err := service.reader.History(ctx, snapshot.Repo, snapshot.Pair, page)
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
