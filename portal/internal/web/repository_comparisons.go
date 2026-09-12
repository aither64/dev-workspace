package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// Comparison records are additive private portal state, separate from the
// session manifest and lifecycle journals. Old package generations ignore them.
type savedRepositoryComparison struct {
	Schema     int                   `json:"schema"`
	Slug       string                `json:"slug"`
	Repository string                `json:"repository"`
	Pair       repository.ReviewPair `json:"pair"`
	SavedAt    time.Time             `json:"savedAt"`
}

func (s *repositoryReviewService) comparisonPath(slug, repo, head string) string {
	return filepath.Join(s.directory, repository.ReviewID(slug+"\x00"+repo), head+".json")
}
func (s *repositoryReviewService) loadComparison(slug, repo, head string) (*repository.ReviewPair, error) {
	file, err := os.Open(s.comparisonPath(slug, repo, head))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return nil, err
	}
	if len(data) > 8192 {
		return nil, errors.New("saved repository comparison is too large")
	}
	var saved savedRepositoryComparison
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("saved repository comparison has trailing data")
	}
	if saved.Schema != 1 || saved.Slug != slug || saved.Repository != repo || saved.Pair.Head != head || !repositoryObjectID(saved.Pair.Base) || !repositoryObjectID(saved.Pair.Head) || saved.SavedAt.IsZero() {
		return nil, errors.New("saved repository comparison has an unexpected identity")
	}
	return &saved.Pair, nil
}
func repositoryObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s *repositoryReviewService) saveComparison(slug, repo string, pair repository.ReviewPair) error {
	if !repositoryObjectID(pair.Base) || !repositoryObjectID(pair.Head) {
		return errors.New("invalid repository comparison")
	}
	previous, err := s.loadComparison(slug, repo, pair.Head)
	if err != nil {
		return err
	}
	if previous != nil && *previous == pair {
		return nil
	}
	target := s.comparisonPath(slug, repo, pair.Head)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(savedRepositoryComparison{Schema: 1, Slug: slug, Repository: repo, Pair: pair, SavedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".comparison-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync repository comparison: %w", err)
	}
	return nil
}

// Durable review descriptors are additive private state. They freeze a branch
// comparison without pinning objects or changing canonical session manifests.
type savedRepositoryReview struct {
	Schema     int                   `json:"schema"`
	Slug       string                `json:"slug"`
	Repository string                `json:"repository"`
	Scope      string                `json:"scope"`
	Pair       repository.ReviewPair `json:"pair"`
}

func repositoryReviewScope(summary *session.Summary, registration session.Repository) (string, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(filepath.Join(summary.Workspace, summary.Root, summary.Slug), &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", errors.New("session tracking is not a directory")
	}
	// The tracking directory survives archival by rename. Its ctime changes as
	// artifacts are added, so it is not part of the durable identity. The thread
	// and directory identity reject reuse of a deleted session's slug.
	registration.FinalHeadSHA = ""
	registration.DefaultBranch = ""
	data, err := json.Marshal(struct {
		Device     uint64
		Inode      uint64
		Thread     string
		Repository session.Repository
	}{uint64(stat.Dev), stat.Ino, summary.Codex.ThreadID, registration})
	if err != nil {
		return "", err
	}
	return repository.ReviewID(string(data)), nil
}

func (s *repositoryReviewService) reviewPath(slug, repo, id string) string {
	return filepath.Join(s.directory, "reviews", repository.ReviewID(slug+"\x00"+repo), id+".json")
}

func (s *repositoryReviewService) saveReview(slug, repo, scope string, pair repository.ReviewPair) (string, error) {
	if !session.ValidSlug(slug) || !reviewToken(repo) || !reviewToken(scope) || !repositoryObjectID(pair.Base) || !repositoryObjectID(pair.Head) {
		return "", errors.New("invalid durable review identity")
	}
	record := savedRepositoryReview{Schema: 1, Slug: slug, Repository: repo, Scope: scope, Pair: pair}
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if len(data) > 8192 {
		return "", repository.ErrReviewLimit
	}
	id := repository.ReviewID(string(data))
	path := s.reviewPath(slug, repo, id)
	if _, err := os.Stat(path); err == nil {
		_, err = s.loadReview(slug, repo, scope, id)
		return id, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".review-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return "", err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *repositoryReviewService) loadReview(slug, repo, scope, id string) (*savedRepositoryReview, error) {
	unavailable := reviewError(404, "This comparison link is unavailable or belongs to a different session or repository.")
	if !reviewToken(id) {
		return nil, unavailable
	}
	file, err := os.Open(s.reviewPath(slug, repo, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, unavailable
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return nil, err
	}
	if len(data) > 8192 {
		return nil, errors.New("durable review descriptor is too large")
	}
	var record savedRepositoryReview
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("durable review descriptor has trailing data")
	}
	canonical, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if record.Schema != 1 || record.Slug != slug || record.Repository != repo || record.Scope != scope || !repositoryObjectID(record.Pair.Base) || !repositoryObjectID(record.Pair.Head) || repository.ReviewID(string(canonical)) != id {
		return nil, unavailable
	}
	return &record, nil
}
