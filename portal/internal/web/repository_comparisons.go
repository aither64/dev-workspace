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
