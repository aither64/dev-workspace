package web

import (
	"context"
	"errors"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRepositoryReviewDiscoveredWorktreeCacheAndRemoval(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	manifest := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
	if err := os.WriteFile(manifest, []byte("schema: 1\nslug: example\n"), 0644); err != nil {
		t.Fatal(err)
	}
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	registration, ok := s.reviewRegistration(context.Background(), summary, repository.ReviewID("project"))
	if !ok || registration.Branch != "feature" {
		t.Fatalf("missing discovered worktree: %#v", registration)
	}
	first := s.reviews().discoveryAt
	if _, ok := s.reviewRegistration(context.Background(), summary, repository.ReviewID("project")); !ok {
		t.Fatal("cached worktree was lost")
	}
	if !s.reviews().discoveryAt.Equal(first) {
		t.Fatal("discovery was not cached")
	}
	runWebGit(t, "-C", worktree, "worktree", "remove", worktree)
	if _, ok := s.reviewRegistration(context.Background(), summary, repository.ReviewID("project")); ok {
		t.Fatal("removed discovered worktree was still authorized")
	}
}

func TestRepositoryReviewRequestCancellationIncludesAdmission(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	for i := 0; i < cap(s.reviews().requests); i++ {
		s.reviews().requests <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/sessions/example/repository-state?repository="+repository.ReviewID("project"), nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request admission ignored cancellation")
	}
}

func TestRepositoryReviewCacheCoalescesAndCancelsLastWaiter(t *testing.T) {
	service := &repositoryReviewService{}
	service.initializeCache()
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	read := func(ctx context.Context) (string, error) {
		if reads.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return "same immutable object", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := cachedReview(firstCtx, service, "same", read); first <- err }()
	<-started
	go func() {
		value, err := cachedReview(context.Background(), service, "same", read)
		if err == nil && value != "same immutable object" {
			err = errors.New("wrong cache value")
		}
		second <- err
	}()
	waitReviewCondition(t, func() bool {
		service.cacheMu.Lock()
		defer service.cacheMu.Unlock()
		return service.calls["same"] != nil && service.calls["same"].waiters == 2
	})
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first=%v", err)
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	_, err := cachedReview(context.Background(), service, "same", read)
	if err != nil || reads.Load() != 1 {
		t.Fatalf("read count=%d, err=%v", reads.Load(), err)
	}
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cachedReview(ctx, service, "cancel", func(ctx context.Context) (string, error) { <-ctx.Done(); close(stopped); return "", ctx.Err() })
		done <- err
	}()
	waitReviewCondition(t, func() bool {
		service.cacheMu.Lock()
		defer service.cacheMu.Unlock()
		return service.calls["cancel"] != nil
	})
	cancel()
	<-done
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("orphaned cache work did not stop")
	}
	if value, err := cachedReview(context.Background(), service, "cancel", func(context.Context) (string, error) { return "retry", nil }); err != nil || value != "retry" {
		t.Fatalf("retry=%q %v", value, err)
	}
}

func waitReviewCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !condition() {
		select {
		case <-deadline:
			t.Fatal("condition timed out")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRepositoryReviewCacheBoundsAndEvicts(t *testing.T) {
	service := &repositoryReviewService{}
	service.initializeCache()
	value := strings.Repeat("x", 1024*1024)
	for i := 0; i < 40; i++ {
		_, err := cachedReview(context.Background(), service, fmt.Sprint(i), func(context.Context) (string, error) { return value, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	service.cacheMu.Lock()
	if service.cacheBytes > repositoryReviewCacheBytes || service.cacheLRU.Len() >= 40 || service.cache["0"] != nil || service.cache["39"] == nil {
		t.Fatalf("bytes=%d entries=%d", service.cacheBytes, service.cacheLRU.Len())
	}
	service.cacheMu.Unlock()
	var reads int
	_, err := cachedReview(context.Background(), service, "0", func(context.Context) (string, error) { reads++; return "evicted", nil })
	if err != nil || reads != 1 {
		t.Fatalf("evicted read=%d %v", reads, err)
	}
}

func TestRepositoryReviewConcurrentCacheCallers(t *testing.T) {
	service := &repositoryReviewService{}
	service.initializeCache()
	var wait sync.WaitGroup
	var reads atomic.Int32
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := cachedReview(context.Background(), service, "shared", func(context.Context) (string, error) { reads.Add(1); return "value", nil })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if reads.Load() != 1 {
		t.Fatalf("duplicate reads=%d", reads.Load())
	}
}
