package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestRepositoryReviewDurableCommitRestoreAndRegistrationScope(t *testing.T) {
	s, _, worktree, base := reviewWebFixture(t)
	query := "?repository=" + repository.ReviewID("project")
	endpoint := "/api/sessions/example/repository-"
	history := reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	review := reviewString(t, history["review"])
	var original reviewHistoryResponse
	decodeReview(t, history, &original)
	if original.History.Commits[0].Message != "feature\n\nBody\n" {
		t.Fatalf("full message=%q", original.History.Commits[0].Message)
	}
	oldHead := original.Pair.Head
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("newer\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "commit", "-am", "new branch head")
	newest := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	fresh, err := New(s.config)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	restore := endpoint + "comparison" + query + "&review=" + review + "&commit=" + oldHead
	response := reviewRequest(t, fresh, "GET", restore, "", 200)
	var comparison reviewComparisonResponse
	decodeReview(t, response, &comparison)
	if comparison.Review != review || comparison.HistoryHead != oldHead || comparison.Pair.Head != oldHead || comparison.Pair.Base != base || comparison.Commit == nil || comparison.Commit.Message != "feature\n\nBody\n" || comparison.Preview == nil || comparison.Preview.Content.After.Text != "after\n" {
		t.Fatalf("restored=%#v", comparison)
	}
	if comparison.Stats.Files != 1 || comparison.Stats.Additions != 1 || comparison.Stats.Deletions != 1 || comparison.Stats.BinaryFiles != 0 {
		t.Fatalf("stats=%#v", comparison.Stats)
	}
	firstSnapshot := comparison.Snapshot
	// Reconstruct the link even after all transient handles were evicted.
	fresh.reviews().mu.Lock()
	clear(fresh.reviews().snapshots)
	fresh.reviews().mu.Unlock()
	response = reviewRequest(t, fresh, "GET", restore+"&file="+comparison.Files[0].ID, "", 200)
	decodeReview(t, response, &comparison)
	if comparison.Snapshot == firstSnapshot || comparison.Preview.File != comparison.Files[0].ID {
		t.Fatal("did not reconstruct the selected file")
	}
	// Parents can cross the comparison base, but newer commits cannot enter an old link.
	reviewRequest(t, fresh, "GET", endpoint+"comparison"+query+"&review="+review+"&commit="+newest, "", 422)
	rootResponse := reviewRequest(t, fresh, "GET", endpoint+"comparison"+query+"&review="+review+"&commit="+base, "", 200)
	var root reviewComparisonResponse
	decodeReview(t, rootResponse, &root)
	if root.Commit == nil || root.Commit.SHA != base || root.Commit.Parents == nil || len(root.Commit.Parents) != 0 || root.HistoryHead != oldHead || root.Stats.Files != 1 || root.Preview == nil || root.Preview.Content.Before.Kind != "absent" || root.Preview.Content.After.Text != "before\n" {
		t.Fatalf("root parent comparison=%#v", root)
	}
	reviewRequest(t, fresh, "GET", endpoint+"comparison"+query+"&review="+review+"&commit=HEAD", "", 400)
	reviewRequest(t, fresh, "GET", restore+"&file="+repository.ReviewID("not-issued"), "", 404)
	reviewRequest(t, fresh, "GET", "/api/sessions/other/repository-comparison"+query+"&review="+review, "", 404)
	// Removing an active worktree does not remove immutable committed objects.
	runWebGit(t, "-C", worktree, "worktree", "remove", worktree)
	reviewRequest(t, fresh, "GET", restore, "", 200)
	manifest := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "branch: feature", "branch: replaced", 1))
	if err := os.WriteFile(manifest, data, 0644); err != nil {
		t.Fatal(err)
	}
	reviewRequest(t, fresh, "GET", restore, "", 404)
}

func decodeReview(t *testing.T, value any, target any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryReviewBatchesAndDirectRegistrationAvoidDiscovery(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(t.TempDir(), "commands")
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$REVIEW_GIT_LOG\"\nexec \"$REVIEW_REAL_GIT\" \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_REAL_GIT", realGit)
	t.Setenv("REVIEW_GIT_LOG", commands)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	id := repository.ReviewID("project")
	query := "?repository=" + id
	endpoint := "/api/sessions/example/repository-"
	history := reviewRequest(t, s, "GET", endpoint+"histories"+query, "", 200)
	var histories []reviewHistoryResponse
	decodeReview(t, history["repositories"], &histories)
	if len(histories) != 1 || histories[0].Repository != id || histories[0].Review == "" {
		t.Fatalf("histories=%#v", histories)
	}
	state := reviewRequest(t, s, "GET", endpoint+"states"+query, "", 200)
	var states []map[string]string
	decodeReview(t, state["repositories"], &states)
	if len(states) != 1 || states[0]["head"] != histories[0].Pair.Head {
		t.Fatalf("states=%#v", states)
	}
	comparison := reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&review="+histories[0].Review, "", 200)
	var compared reviewComparisonResponse
	decodeReview(t, comparison, &compared)
	file := reviewRequest(t, s, "GET", endpoint+"files"+query+"&snapshot="+compared.Snapshot+"&file="+compared.Files[0].ID, "", 200)
	var content []struct {
		File    string
		Content repository.ReviewContent
	}
	decodeReview(t, file["files"], &content)
	if len(content) != 1 || content[0].Content.After.Text != "after\n" {
		t.Fatalf("file batch=%#v", content)
	}
	if data, err := os.ReadFile(commands); err != nil || strings.Contains(string(data), "worktree list") {
		t.Fatalf("registered reads ran discovery: %s, %v", data, err)
	}
	for _, operation := range []string{"histories", "states"} {
		reviewRequest(t, s, "GET", endpoint+operation+query+"&repository="+id, "", 400)
	}
	reviewRequest(t, s, "GET", endpoint+"histories"+query+strings.Repeat("&repository="+id, 8), "", 400)
	reviewRequest(t, s, "GET", endpoint+"files"+query+"&snapshot="+compared.Snapshot+strings.Repeat("&file="+compared.Files[0].ID, 5), "", 400)
	reviewRequest(t, s, "GET", endpoint+"files"+query+"&snapshot="+compared.Snapshot+"&file="+repository.ReviewID("foreign"), "", 404)
	partial := reviewRequest(t, s, "GET", endpoint+"states"+query+"&repository="+repository.ReviewID("missing"), "", 200)
	decodeReview(t, partial["repositories"], &states)
	if len(states) != 2 || states[0]["head"] == "" || states[1]["error"] == "" {
		t.Fatalf("partial batch=%#v", states)
	}
}

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

func TestRepositoryReviewDescriptorSurvivesArchiveButNotReplacement(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	query := "?repository=" + repository.ReviewID("project")
	history := reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 200)
	review := reviewString(t, history["review"])
	var original reviewHistoryResponse
	decodeReview(t, history, &original)
	source := filepath.Join(s.config.Workspace, "work", "example")
	target := filepath.Join(s.config.Workspace, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("schema: 1\nslug: example\nfinalized_at: '2026-09-12T12:00:00Z'\nrepositories:\n  - name: project\n    project: project\n    branch: feature\n    default_branch: master\n    initial_base_sha: %s\n    final_head_sha: %s\n", original.Pair.Base, original.Pair.Head)
	if err := os.WriteFile(filepath.Join(target, "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, target, "complete")
	if _, err := session.Find(s.config.Workspace, "example"); err != nil {
		t.Fatal(err)
	}
	restore := "/api/sessions/example/repository-comparison" + query + "&review=" + review
	reviewRequest(t, s, "GET", restore, "", 200)
	// A replacement directory with identical tracking bytes has a new ownership
	// identity. Keep the old directory alive so the test cannot reuse its inode.
	if err := os.Rename(target, target+"-previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, target, "complete")
	reviewRequest(t, s, "GET", restore, "", 404)
}

func TestRepositoryReviewMissingSavedObjectsIsExplicit(t *testing.T) {
	s, bare, _, _ := reviewWebFixture(t)
	query := "?repository=" + repository.ReviewID("project")
	history := reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 200)
	var original reviewHistoryResponse
	decodeReview(t, history, &original)
	if err := os.Remove(filepath.Join(bare, "objects", original.Pair.Head[:2], original.Pair.Head[2:])); err != nil {
		t.Fatal(err)
	}
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-comparison"+query+"&review="+original.Review, "", 422)
}

func TestRepositoryReviewIdenticalPairDoesNotRewriteSavedRecord(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	query := "?repository=" + repository.ReviewID("project")
	history := reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 200)
	var original reviewHistoryResponse
	decodeReview(t, history, &original)
	path := s.reviews().comparisonPath("example", repository.ReviewID("project"), original.Pair.Head)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query+"&snapshot="+original.Snapshot, "", 200)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged saved pair was rewritten")
	}
}
