package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Review bases are independent of lifecycle manifests and immutable saved pairs.
// Scope-specific paths prevent a replacement session/registration inheriting one.
type repositoryReviewBase struct {
	Schema int    `json:"schema"`
	Scope  string `json:"scope"`
	Base   string `json:"base"`
}

type RepositoryReviewBaseResult struct {
	Base string `json:"base"`
	Head string `json:"head,omitempty"`
}

func (s *repositoryReviewService) reviewBasePath(slug, repo, scope string) string {
	return filepath.Join(s.directory, "bases", repository.ReviewID(slug+"\x00"+repo), scope+".json")
}

func (s *repositoryReviewService) loadReviewBase(slug, repo, scope string) (string, error) {
	f, err := os.Open(s.reviewBasePath(slug, repo, scope))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil {
		return "", err
	}
	var record repositoryReviewBase
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if len(data) > 8192 || d.Decode(&record) != nil || d.Decode(&struct{}{}) != io.EOF ||
		record.Schema != 1 || record.Scope != scope || !repositoryObjectID(record.Base) {
		return "", reviewError(409, "The repository review-base setting is invalid. Check it with dev-session worktree review-base.")
	}
	return record.Base, nil
}

// The CLI holds the existing worktree session lock across this operation, just
// as it does for capture-comparison. Readers see either complete atomic record.
func RepositoryReviewBase(ctx context.Context, workspace, stateRoot, slug, name, base string, clear bool) (RepositoryReviewBaseResult, error) {
	var result RepositoryReviewBaseResult
	summary, err := session.Find(workspace, slug)
	if err != nil {
		return result, err
	}
	var registration *session.Repository
	for _, item := range summary.Repositories {
		if item.Name == name {
			copy := item
			registration = &copy
			break
		}
	}
	if registration == nil {
		return result, errors.New("repository is not registered in this session")
	}
	if base != "" && clear {
		return result, errors.New("--base and --clear cannot be combined")
	}
	mutation := base != "" || clear
	if mutation && summary.Archived {
		return result, errors.New("cannot change the review base of an archived session")
	}
	scope, err := repositoryReviewScope(summary, *registration)
	if err != nil {
		return result, err
	}
	store, err := newLifecycleOperationStore(workspace, stateRoot)
	if err != nil {
		return result, err
	}
	service := &repositoryReviewService{directory: filepath.Join(store.directory, "repository-comparisons"), reader: repository.ReviewReader{Workspace: workspace}}
	result.Base, err = service.loadReviewBase(slug, repository.ReviewID(name), scope)
	if !mutation || (err != nil && !clear) {
		return result, err
	}
	repo, err := service.reader.Resolve(ctx, slug, *registration, false)
	if err != nil {
		return result, err
	}
	result.Head = repo.Head
	if !clear {
		if _, err := service.reader.ExplicitPair(ctx, repo, base); err != nil {
			return result, err
		}
	}
	current, err := session.Find(workspace, slug)
	if err != nil {
		return result, err
	}
	if current.Archived {
		return result, errors.New("session changed during review-base selection; retry")
	}
	var same bool
	for _, item := range current.Repositories {
		if item.Name != name {
			continue
		}
		currentScope, scopeErr := repositoryReviewScope(current, item)
		if scopeErr != nil {
			return result, scopeErr
		}
		currentRepo, resolveErr := service.reader.Resolve(ctx, slug, item, false)
		if resolveErr != nil {
			return result, resolveErr
		}
		same = currentScope == scope && currentRepo.Head == repo.Head
	}
	if !same {
		return result, errors.New("repository changed during review-base selection; retry")
	}
	target := service.reviewBasePath(slug, repo.ID, scope)
	if clear {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		result.Base = ""
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return result, err
		}
		data, err := json.Marshal(repositoryReviewBase{Schema: 1, Scope: scope, Base: base})
		if err != nil {
			return result, err
		}
		f, err := os.CreateTemp(filepath.Dir(target), ".base-*")
		if err != nil {
			return result, err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(data); err != nil {
			f.Close()
			return result, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return result, err
		}
		if err := f.Close(); err != nil {
			return result, err
		}
		if err := os.Rename(f.Name(), target); err != nil {
			return result, err
		}
		result.Base = base
	}
	dir, err := os.Open(filepath.Dir(target))
	if clear && errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer dir.Close()
	return result, dir.Sync()
}

type reviewSelection struct {
	Base        string
	DefaultHead string
	Saved       *repository.ReviewPair
	Version     string
}

func (s *repositoryReviewService) state(ctx context.Context, slug, scope string, repo repository.ReviewRepository) map[string]string {
	result := map[string]string{"head": repo.Head}
	// HEAD remains readable when comparison persistence or selection fails.
	// History reports the comparison error; version metadata is optional.
	if selection, err := s.selection(ctx, slug, scope, repo); err == nil {
		result["comparisonVersion"] = selection.Version
	}
	return result
}

func (s *repositoryReviewService) selection(ctx context.Context, slug, scope string, repo repository.ReviewRepository) (reviewSelection, error) {
	base, err := s.loadReviewBase(slug, repo.ID, scope)
	if err != nil {
		return reviewSelection{}, err
	}
	var defaultHead string
	if base == "" {
		defaultHead, err = s.reader.DefaultHead(ctx, repo)
		if err != nil {
			return reviewSelection{}, err
		}
	}
	var saved *repository.ReviewPair
	if base == "" {
		saved, err = s.loadComparison(slug, repo.ID, repo.Head)
		if err != nil {
			return reviewSelection{}, err
		}
	}
	// Pair may use the saved comparison whenever the default contains HEAD,
	// including when the default has advanced beyond it. A historical capture
	// must invalidate a cached fallback without another ancestry walk here.
	encoded, _ := json.Marshal(saved)
	return reviewSelection{Base: base, DefaultHead: defaultHead, Saved: saved,
		Version: repository.ReviewID(scope + "\x00" + repo.Head + "\x00" + base + "\x00" + repo.DefaultRef + "\x00" + defaultHead + "\x00" + string(encoded))}, nil
}

func (s *repositoryReviewService) selectedPair(ctx context.Context, slug string, repo repository.ReviewRepository, selection reviewSelection) (repository.ReviewPair, error) {
	read := func(ctx context.Context) (repository.ReviewPair, error) {
		if selection.Base != "" {
			return s.reader.ExplicitPair(ctx, repo, selection.Base)
		}
		saved := selection.Saved
		// Pin the default to the identity used in the cache key, retaining the
		// human label. A moving default cannot put a different pair in this key.
		originalDefault := repo.DefaultRef
		repo.DefaultRef = selection.DefaultHead
		pair, err := s.reader.Pair(ctx, repo, saved)
		if err == nil && selection.DefaultHead != "" && pair.BaseLabel == "Merge base with locally available "+selection.DefaultHead {
			pair.BaseLabel = "Merge base with locally available " + strings.TrimPrefix(originalDefault, "refs/remotes/")
		}
		if err == nil && !repo.Archived && originalDefault != "" && selection.DefaultHead == "" {
			pair.Warning = strings.Replace(pair.Warning, "No saved comparison is available for this head.", "The default branch is unavailable locally.", 1)
		}
		return pair, err
	}
	// Standalone CLI instances have no cache lifetime.
	if s.lifetime == nil {
		return read(ctx)
	}
	return cachedReview(ctx, s, fmt.Sprintf("pair\x00%s\x00%s\x00%s\x00%t\x00%s", slug, repo.ID, repo.Directory, repo.Archived, selection.Version), read)
}
