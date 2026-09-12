package web

import (
	"context"
	"net/http"
	"sync"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func reviewBatchIDs(values []string, limit int) bool {
	if len(values) == 0 || len(values) > limit {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, id := range values {
		if !reviewToken(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// Batch workers share the reader's process budget. Request goroutines do not
// reserve a process slot while waiting for their own nested Git commands.
func runReviewBatch(ctx context.Context, count int, run func(int)) {
	var wait sync.WaitGroup
	work := make(chan int)
	for i := 0; i < min(count, 4); i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range work {
				run(index)
			}
		}()
	}
	for i := 0; i < count; i++ {
		select {
		case work <- i:
		case <-ctx.Done():
			close(work)
			wait.Wait()
			return
		}
	}
	close(work)
	wait.Wait()
}

func (s *Server) repositoryReviewBatch(w http.ResponseWriter, r *http.Request, summary *session.Summary, operation string) {
	ids := r.URL.Query()["repository"]
	limit := 8
	if operation == "repository-states" {
		limit = 32
	}
	if !reviewBatchIDs(ids, limit) {
		s.writeReviewError(w, summary, reviewError(400, "Invalid repository batch"))
		return
	}
	result := make([]map[string]any, len(ids))
	for index, id := range ids {
		result[index] = map[string]any{"repository": id, "error": "The repository request was cancelled."}
	}
	runReviewBatch(r.Context(), len(ids), func(index int) {
		id := ids[index]
		registration, ok := s.reviewRegistration(r.Context(), summary, id)
		if !ok {
			result[index] = map[string]any{"repository": id, "error": "Repository is not registered in this session"}
			return
		}
		scope, err := repositoryReviewScope(summary, registration)
		if err == nil && operation == "repository-histories" {
			payload, readErr := s.reviewHistory(r.Context(), summary, registration, scope, "", 0)
			err = readErr
			if err == nil {
				result[index] = map[string]any{"repository": id, "review": payload.Review, "snapshot": payload.Snapshot, "pair": payload.Pair, "history": payload.History}
				return
			}
		} else if err == nil {
			repo, readErr := s.reviews().reader.Resolve(r.Context(), summary.Slug, registration, summary.Archived)
			err = readErr
			if err == nil {
				result[index] = map[string]any{"repository": id, "head": repo.Head}
				return
			}
		}
		_, message := reviewErrorMessage(err)
		result[index] = map[string]any{"repository": id, "error": message}
	})
	s.writeJSON(w, 200, map[string]any{"repositories": result})
}

func (s *Server) repositoryFilesBatch(w http.ResponseWriter, r *http.Request, summary *session.Summary, snapshot *repositoryReviewSnapshot) {
	ids := r.URL.Query()["file"]
	if !reviewBatchIDs(ids, 4) {
		s.writeReviewError(w, summary, reviewError(400, "Invalid file batch"))
		return
	}
	files, err := s.reviews().comparisonFiles(r.Context(), snapshot)
	if err != nil {
		s.writeReviewError(w, summary, err)
		return
	}
	allowed := make(map[string]bool, len(files))
	for _, file := range files {
		allowed[file.ID] = true
	}
	for _, id := range ids {
		if !allowed[id] {
			s.writeReviewError(w, summary, reviewError(404, "File is not part of this comparison"))
			return
		}
	}
	type fileResponse struct {
		File    string                    `json:"file"`
		Content *repository.ReviewContent `json:"content,omitempty"`
		Error   string                    `json:"error,omitempty"`
	}
	result := make([]fileResponse, len(ids))
	for index, id := range ids {
		result[index] = fileResponse{File: id, Error: "The file request was cancelled."}
	}
	runReviewBatch(r.Context(), len(ids), func(index int) {
		content, err := s.reviews().fileContent(r.Context(), snapshot, ids[index])
		if err != nil {
			_, message := reviewErrorMessage(err)
			result[index] = fileResponse{File: ids[index], Error: message}
		} else {
			result[index] = fileResponse{File: ids[index], Content: &content}
		}
	})
	s.writeJSON(w, 200, map[string]any{"files": result})
}
