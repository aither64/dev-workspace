package web

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
)

func TestRepositoryReviewParentNavigationUsesFrozenHistoryAfterRestart(t *testing.T) {
	s, bare, worktree, root := reviewWebFixture(t)
	git := func(args ...string) string {
		return strings.TrimSpace(webGitOutput(t, append([]string{"-C", worktree}, args...)...))
	}
	base := git("rev-parse", "HEAD")
	rootTree := git("rev-parse", root+"^{tree}")
	baseTree := git("rev-parse", base+"^{tree}")
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", base)
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("left\n"), 0644); err != nil {
		t.Fatal(err)
	}
	firstMessage := "First parent\n\nLeft-side message."
	runWebGit(t, "-C", worktree, "commit", "-am", firstMessage)
	first := git("rev-parse", "HEAD")
	secondMessage := "Second parent\n\nRight-side message."
	second := git("commit-tree", rootTree, "-p", base, "-m", secondMessage)
	merge := git("commit-tree", baseTree, "-p", first, "-p", second, "-m", "Merge parents")
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/heads/feature", merge)
	query := "?repository=" + repository.ReviewID("project")
	endpoint := "/api/sessions/example/repository-"
	var history reviewHistoryResponse
	decodeReview(t, reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200), &history)
	if history.Pair.Base != base || history.Pair.Head != merge || len(history.History.Commits) != 3 {
		t.Fatalf("feature range=%#v", history)
	}
	for _, commit := range history.History.Commits {
		if commit.SHA == base || commit.SHA == root {
			t.Fatalf("parent navigation extended the feature list: %#v", commit)
		}
	}
	if !reflect.DeepEqual(history.History.Commits[0].Parents, []string{first, second}) {
		t.Fatalf("history parent metadata=%#v", history.History.Commits[0])
	}
	// Move both refs, then reopen the saved URL in a fresh service with no cache.
	newer := git("commit-tree", baseTree, "-p", merge, "-m", "New branch head")
	unrelated := git("commit-tree", rootTree, "-p", root, "-m", "Unrelated local commit")
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/heads/feature", newer)
	runWebGit(t, "--git-dir="+bare, "update-ref", "refs/remotes/origin/master", newer)
	fresh, err := New(s.config)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	comparisonURL := endpoint + "comparison" + query + "&review=" + history.Review
	for _, entry := range []struct {
		sha, message, before, after string
		parents                     []string
	}{
		{merge, "Merge parents\n", "left\n", "after\n", []string{first, second}},
		{first, firstMessage + "\n", "after\n", "left\n", []string{base}},
		{base, "feature\n\nBody\n", "before\n", "after\n", []string{root}},
		{root, "base\n", "", "before\n", []string{}},
		{second, secondMessage + "\n", "after\n", "before\n", []string{base}},
	} {
		var comparison reviewComparisonResponse
		decodeReview(t, reviewRequest(t, fresh, "GET", comparisonURL+"&commit="+entry.sha, "", 200), &comparison)
		if comparison.Commit == nil || comparison.Commit.SHA != entry.sha || comparison.Commit.Message != entry.message || !reflect.DeepEqual(comparison.Commit.Parents, entry.parents) || comparison.Commit.Author != "Test" || comparison.Commit.Date == "" || comparison.Review != history.Review || comparison.HistoryHead != merge || comparison.Pair.Head != entry.sha {
			t.Fatalf("commit details for %s=%#v", entry.sha, comparison)
		}
		if len(entry.parents) > 0 && comparison.Pair.Base != entry.parents[0] {
			t.Fatalf("commit did not compare its first parent: %#v", comparison.Pair)
		}
		if comparison.Stats.Files != 1 || len(comparison.Files) != 1 || comparison.Files[0].Path != "file" || comparison.Preview == nil || comparison.Preview.Content.Before.Text != entry.before || comparison.Preview.Content.After.Text != entry.after {
			t.Fatalf("parent diff for %s=%#v", entry.sha, comparison)
		}
		if entry.sha == root && (comparison.Preview.Content.Before.Kind != "absent" || comparison.Files[0].Status != "A" || comparison.Stats.Additions != 1 || comparison.Stats.Deletions != 0) {
			t.Fatalf("root diff=%#v", comparison)
		}
	}
	for _, sha := range []string{newer, unrelated} {
		reviewRequest(t, fresh, "GET", comparisonURL+"&commit="+sha, "", 422)
	}
	// A missing ancestor remains unavailable; no remote fetch repairs it.
	if err := os.Remove(filepath.Join(bare, "objects", second[:2], second[2:])); err != nil {
		t.Fatal(err)
	}
	missing, err := New(s.config)
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	reviewRequest(t, missing, "GET", comparisonURL+"&commit="+second, "", 422)
}
