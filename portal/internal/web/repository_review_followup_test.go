package web

import (
	"context"
	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"net/http/httptest"
	"os"
	"path/filepath"
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
