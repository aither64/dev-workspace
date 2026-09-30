package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func expiredWorktreeReview() error {
	return reviewError(http.StatusConflict, "This snapshot has expired. Capture the changes again.")
}

type worktreeReservation struct {
	owner  *repositoryReviewService
	active bool
}

// A capture may admit up to 64 MiB before its exact size is known. Reserve
// that capacity without holding the service lock while Git and files are read.
func (s *repositoryReviewService) reserveWorktree() (*worktreeReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.worktreeBytes+s.reservedWorktreeBytes+repository.MaxWorktreeSnapshotBytes > repository.MaxWorktreeStoreBytes {
		if !s.evictIdleSnapshotLocked(true) {
			return nil, repository.ErrReviewLimit
		}
	}
	s.reservedWorktreeBytes += repository.MaxWorktreeSnapshotBytes
	return &worktreeReservation{owner: s, active: true}, nil
}

func (s *repositoryReviewService) releaseWorktreeReservation(reservation *worktreeReservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reservation != nil && reservation.owner == s && reservation.active {
		s.reservedWorktreeBytes -= repository.MaxWorktreeSnapshotBytes
		reservation.active = false
	}
}

func (s *repositoryReviewService) evictIdleSnapshotLocked(requireBytes bool) bool {
	var oldest *repositoryReviewSnapshot
	for _, item := range s.snapshots {
		if item.Readers == 0 && (!requireBytes || item.Worktree != nil) && (oldest == nil || item.Created.Before(oldest.Created)) {
			oldest = item
		}
	}
	if oldest == nil {
		return false
	}
	delete(s.snapshots, oldest.ID)
	if oldest.Worktree != nil {
		s.worktreeBytes -= oldest.Worktree.RawBytes
	}
	return true
}

func (s *repositoryReviewService) putWorktree(snapshot *repositoryReviewSnapshot, reservation *worktreeReservation) error {
	if reservation == nil || snapshot.Worktree == nil || snapshot.Worktree.RawBytes < 0 || snapshot.Worktree.RawBytes > repository.MaxWorktreeSnapshotBytes {
		return repository.ErrReviewLimit
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	snapshot.ID = hex.EncodeToString(token[:])
	snapshot.Created = time.Now()
	snapshot.Readers = 1 // Capture's response owns the first read lease.
	s.mu.Lock()
	defer s.mu.Unlock()
	if reservation.owner != s || !reservation.active {
		return repository.ErrReviewLimit
	}
	reserved := s.reservedWorktreeBytes - repository.MaxWorktreeSnapshotBytes
	for s.worktreeBytes+reserved+snapshot.Worktree.RawBytes > repository.MaxWorktreeStoreBytes {
		if !s.evictIdleSnapshotLocked(true) {
			return repository.ErrReviewLimit
		}
	}
	for len(s.snapshots) >= 32 {
		if !s.evictIdleSnapshotLocked(false) {
			return repository.ErrReviewLimit
		}
	}
	s.reservedWorktreeBytes -= repository.MaxWorktreeSnapshotBytes
	reservation.active = false
	s.snapshots[snapshot.ID] = snapshot
	s.worktreeBytes += snapshot.Worktree.RawBytes
	return nil
}

func (s *repositoryReviewService) leaseWorktree(snapshot *repositoryReviewSnapshot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots[snapshot.ID] != snapshot || snapshot.Worktree == nil {
		return false
	}
	snapshot.Readers++
	snapshot.Created = time.Now()
	return true
}

func (s *repositoryReviewService) releaseWorktree(snapshot *repositoryReviewSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot.Readers > 0 {
		snapshot.Readers--
	}
}

func (s *Server) captureWorktreeReview(ctx context.Context, summary *session.Summary, registration session.Repository, scope, kind string) (*repositoryReviewSnapshot, error) {
	if summary.Archived {
		return nil, reviewError(http.StatusConflict, "Archived sessions have committed comparisons only.")
	}
	service := s.reviews()
	repo, err := service.reader.Resolve(ctx, summary.Slug, registration, false)
	if err != nil {
		return nil, err
	}
	reservation, err := service.reserveWorktree()
	if err != nil {
		return nil, reviewError(http.StatusUnprocessableEntity, "Snapshot storage is full. Try again shortly.")
	}
	defer service.releaseWorktreeReservation(reservation)
	capture, err := service.reader.CaptureWorktree(ctx, repo, kind)
	if err != nil {
		if errors.Is(err, repository.ErrWorktreeChanged) {
			return nil, reviewError(http.StatusConflict, err.Error())
		}
		if errors.Is(err, repository.ErrWorktreeState) {
			return nil, reviewError(http.StatusUnprocessableEntity, err.Error())
		}
		if errors.Is(err, repository.ErrReviewLimit) {
			return nil, reviewError(http.StatusUnprocessableEntity, "This snapshot exceeds a review limit for files, index data, or previews.")
		}
		return nil, err
	}
	current, err := service.reader.Resolve(ctx, summary.Slug, registration, false)
	if err != nil || current.Head != capture.SourceHead || current.Worktree != repo.Worktree {
		return nil, reviewError(http.StatusConflict, "The registered worktree changed during capture. Try again.")
	}
	rootID, err := repository.WorktreeRootID(current.Worktree)
	if err != nil || rootID != capture.RootID {
		return nil, reviewError(http.StatusConflict, "The registered worktree changed during capture. Try again.")
	}
	label := "HEAD → Index"
	if kind == "unstaged" {
		label = "Index → Working tree"
	}
	snapshot := &repositoryReviewSnapshot{Slug: summary.Slug, Scope: scope, Repo: repo,
		Pair: repository.ReviewPair{Base: capture.SourceHead, Head: capture.SourceHead, BaseLabel: label}, Worktree: &capture}
	if err := service.putWorktree(snapshot, reservation); err != nil {
		if errors.Is(err, repository.ErrReviewLimit) {
			return nil, reviewError(http.StatusUnprocessableEntity, "Snapshot storage is full. Try again shortly.")
		}
		return nil, err
	}
	return snapshot, nil
}

func (s *Server) authorizeWorktreeSnapshot(ctx context.Context, summary *session.Summary, registration session.Repository, snapshot *repositoryReviewSnapshot) error {
	if summary.Archived || snapshot.Worktree == nil {
		return expiredWorktreeReview()
	}
	current, err := s.reviews().reader.Resolve(ctx, summary.Slug, registration, false)
	if err != nil || current.Worktree != snapshot.Repo.Worktree || current.Directory != snapshot.Repo.Directory {
		return expiredWorktreeReview()
	}
	rootID, err := repository.WorktreeRootID(current.Worktree)
	if err != nil || rootID != snapshot.Worktree.RootID {
		return expiredWorktreeReview()
	}
	return nil
}
