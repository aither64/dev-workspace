package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestRepositoryExplicitBasePreservesLinksAndAutomaticCapture(t *testing.T) {
	s, bare, worktree, originalBase := reviewWebFixture(t)
	set := func(base string, clear bool) RepositoryReviewBaseResult {
		t.Helper()
		result, err := RepositoryReviewBase(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", base, clear)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	endpoint := "/api/sessions/example/repository-"
	query := "?repository=" + repository.ReviewID("project")
	old := reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	oldReview := reviewString(t, old["review"])
	base := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(worktree, "backport"), []byte("fix\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "add", "backport")
	runWebGit(t, "-C", worktree, "commit", "-m", "backport")
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	// Save the automatic pair for this same head before choosing a narrower base.
	automatic := reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	before, err := s.reviews().loadComparison("example", repository.ReviewID("project"), head)
	if err != nil || before.Base != originalBase {
		t.Fatalf("automatic pair: %#v %v", before, err)
	}
	if got := set(base, false); got.Base != base || got.Head != head {
		t.Fatalf("set: %#v", got)
	}
	history := reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	var pair repository.ReviewPair
	decodeReview(t, history["pair"], &pair)
	if pair.Base != base || pair.Head != head {
		t.Fatalf("explicit pair: %#v", pair)
	}
	if string(history["comparisonVersion"]) == string(automatic["comparisonVersion"]) {
		t.Fatal("base change did not change version")
	}
	var commits repository.ReviewHistory
	decodeReview(t, history["history"], &commits)
	if len(commits.Commits) != 1 || commits.Commits[0].Subject != "backport" {
		t.Fatalf("history: %#v", commits)
	}
	comparison := reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&review="+reviewString(t, history["review"]), "", 200)
	files := reviewFilesArray(t, comparison)
	if len(files) != 1 || files[0].Path != "backport" {
		t.Fatalf("files: %#v", files)
	}
	if _, err := CaptureRepositoryComparison(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", "", ""); err != nil {
		t.Fatal(err)
	}
	after, err := s.reviews().loadComparison("example", repository.ReviewID("project"), head)
	if err != nil || *before != *after {
		t.Fatalf("legacy capture changed: %#v %v", after, err)
	}
	// Historical capture remains independent of the active review-base policy.
	if captured, err := CaptureRepositoryComparison(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", originalBase, head); err != nil || captured.Base != originalBase {
		t.Fatalf("historical capture: %#v %v", captured, err)
	}
	oldComparison := reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&review="+oldReview, "", 200)
	decodeReview(t, oldComparison["pair"], &pair)
	if pair.Base != originalBase || pair.Head != base {
		t.Fatalf("old link changed: %#v", pair)
	}
	// A default-branch change must not override the selected review base.
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", head)
	history = reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	decodeReview(t, history["pair"], &pair)
	if pair.Base != base {
		t.Fatalf("integrated explicit base changed: %#v", pair)
	}
	set(head, false)
	history = reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	decodeReview(t, history["history"], &commits)
	if len(commits.Commits) != 0 {
		t.Fatal("equal base and head is not empty")
	}
	set("", true)
	history = reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	decodeReview(t, history["pair"], &pair)
	if pair.Base != originalBase {
		t.Fatalf("clear did not restore saved pair: %#v", pair)
	}
}

func TestRepositoryReviewBaseScopeArchiveAndInvalidInputs(t *testing.T) {
	s, _, worktree, base := reviewWebFixture(t)
	set := func(value string, clear bool) error {
		_, err := RepositoryReviewBase(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", value, clear)
		return err
	}
	if err := set(strings.Repeat("f", 40), false); !errors.Is(err, repository.ErrReviewBaseUnavailable) {
		t.Fatalf("missing base: %v", err)
	}
	if err := set(base, true); err == nil {
		t.Fatal("conflicting options accepted")
	}
	if err := set(base, false); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	tracking := filepath.Join(s.config.Workspace, "work", "example")
	manifest := fmt.Sprintf("schema: 1\nslug: example\nfinalized_at: '2026-09-12T12:00:00Z'\nrepositories:\n- name: project\n  project: project\n  branch: feature\n  default_branch: master\n  initial_base_sha: %s\n  final_head_sha: %s\n", base, head)
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, tracking, "archived")
	archived := filepath.Join(s.config.Workspace, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tracking, archived); err != nil {
		t.Fatal(err)
	}
	result, err := RepositoryReviewBase(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", "", false)
	if err != nil || result.Base != base {
		t.Fatalf("archived read: %#v %v", result, err)
	}
	if err := set("", true); err == nil {
		t.Fatal("archived mutation accepted")
	}
	query := "?repository=" + repository.ReviewID("project")
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 200)
	if err := os.Rename(archived, tracking); err != nil {
		t.Fatal(err)
	}
	manifest = strings.Replace(manifest, "finalized_at: '2026-09-12T12:00:00Z'\n", "", 1)
	manifest = strings.Replace(manifest, "  final_head_sha: "+head+"\n", "", 1)
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, tracking, "active")
	if result, err = RepositoryReviewBase(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", "", false); err != nil || result.Base != base {
		t.Fatalf("revived read: %#v %v", result, err)
	}
	// Replacing the directory changes the existing scope identity without relying
	// on slug uniqueness. Retain the old directory to avoid inode reuse.
	if err := os.Rename(tracking, tracking+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tracking, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, tracking, "active")
	if result, err = RepositoryReviewBase(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", "", false); err != nil || result.Base != "" {
		t.Fatalf("reused identity inherited base: %#v %v", result, err)
	}
}

func TestRepositoryReviewBaseRejectsRebasedHeadAndCorruptState(t *testing.T) {
	s, bare, worktree, originalBase := reviewWebFixture(t)
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	if _, err := RepositoryReviewBase(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", head, false); err != nil {
		t.Fatal(err)
	}
	// Move the fixture branch behind the selected base.
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/heads/feature", originalBase)
	query := "?repository=" + repository.ReviewID("project")
	payload := reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 409)
	if !strings.Contains(reviewString(t, payload["error"]), "ancestor") {
		t.Fatalf("wrong error: %s", payload["error"])
	}
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := repositoryReviewScope(summary, summary.Repositories[0])
	if err != nil {
		t.Fatal(err)
	}
	path := s.reviews().reviewBasePath("example", repository.ReviewID("project"), scope)
	if err := os.WriteFile(path, []byte(`{"schema":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	state := reviewRequest(t, s, "GET", "/api/sessions/example/repository-state"+query, "", 200)
	if _, present := state["comparisonVersion"]; present || reviewString(t, state["head"]) != originalBase {
		t.Fatalf("invalid setting hid readable HEAD: %v", state)
	}
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 409)
	if _, err := RepositoryReviewBase(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", "", true); err != nil {
		t.Fatalf("clear corrupt setting: %v", err)
	}
	reviewRequest(t, s, "GET", "/api/sessions/example/repository-history"+query, "", 200)
}

func TestReviewErrorPreservesDeadlineThroughResolution(t *testing.T) {
	s, _, _, _ := reviewWebFixture(t)
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.reviews().reader.Resolve(ctx, "example", summary.Repositories[0], false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	status, message := reviewErrorMessage(err)
	if status != 408 || !strings.Contains(message, "cancelled") {
		t.Fatalf("%d %s", status, message)
	}
	status, _ = reviewErrorMessage(fmt.Errorf("wrapped: %w", context.DeadlineExceeded))
	if status != 504 {
		t.Fatalf("deadline status %d", status)
	}
}

func TestRepositoryHistoricalCaptureInvalidatesDescendantDefault(t *testing.T) {
	s, bare, worktree, originalBase := reviewWebFixture(t)
	base := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	runWebGit(t, "-C", worktree, "commit", "--allow-empty", "-m", "feature follow-up")
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	// Advance the default beyond the integrated feature head.
	defaultHead := strings.TrimSpace(webGitOutput(t, "-C", worktree, "commit-tree", "HEAD^{tree}", "-p", head, "-m", "default follow-up"))
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", defaultHead)
	endpoint := "/api/sessions/example/repository-history?repository=" + repository.ReviewID("project")
	before := reviewRequest(t, s, "GET", endpoint, "", 200)
	var pair repository.ReviewPair
	decodeReview(t, before["pair"], &pair)
	if pair.Base != originalBase {
		t.Fatalf("initial fallback: %#v", pair)
	}
	if _, err := CaptureRepositoryComparison(context.Background(), s.config.Workspace, s.config.UserStateRoot, "example", "project", base, head); err != nil {
		t.Fatal(err)
	}
	after := reviewRequest(t, s, "GET", endpoint, "", 200)
	decodeReview(t, after["pair"], &pair)
	if pair.Base != base || string(after["comparisonVersion"]) == string(before["comparisonVersion"]) {
		t.Fatalf("historical capture left cached fallback: %#v", pair)
	}
}
