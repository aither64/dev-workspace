package repository

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestReviewCommitParentsBeyondComparisonBase(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	ancestor := commitInWorktree(t, f.worktree, "ancestor")
	base := commitInWorktree(t, f.worktree, "comparison-base")
	first := commitInWorktree(t, f.worktree, "first-parent")
	second := commitFrom(t, f, base, "second-parent")
	runGit(t, "-C", f.worktree, "merge", "--no-ff", "-m", "merge parents", second)
	head := gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	pair := ReviewPair{Base: base, Head: head}
	unrelated := commitFrom(t, f, ancestor, "unrelated")
	// A newer branch head must not extend the old review's reachable history.
	repo.Head = commitInWorktree(t, f.worktree, "newer-head")

	for _, entry := range []struct {
		sha     string
		parents []string
	}{
		{head, []string{first, second}},
		{first, []string{base}},
		{second, []string{base}},
		{base, []string{ancestor}},
		{ancestor, []string{f.baseHead}},
		{f.baseHead, []string{}},
	} {
		commit, err := reader.ComparisonCommit(context.Background(), repo, pair, entry.sha)
		if err != nil || commit.SHA != entry.sha || !reflect.DeepEqual(commit.Parents, entry.parents) || commit.Message == "" {
			t.Fatalf("commit %s = %#v, %v", entry.sha, commit, err)
		}
		if commit.SHA == head || commit.SHA == f.baseHead {
			commitPair, err := reader.CommitPair(context.Background(), repo, commit)
			if err != nil {
				t.Fatal(err)
			}
			files, err := reader.Files(context.Background(), repo, commitPair)
			if err != nil || len(files) != 1 || files[0].Status != "A" {
				t.Fatalf("parent comparison for %s = %#v, %v", entry.sha, files, err)
			}
			if commit.SHA == head && (commitPair.Base != first || files[0].Path != "second-parent") {
				t.Fatalf("merge did not use first parent: %#v, %#v", commitPair, files)
			}
			if commit.SHA == f.baseHead && files[0].Path != "README" {
				t.Fatalf("root did not use empty tree: %#v", files)
			}
		}
	}

	history, err := reader.History(context.Background(), repo, pair, 0)
	if err != nil || len(history.Commits) != 3 {
		t.Fatalf("feature history = %#v, %v", history, err)
	}
	for _, commit := range history.Commits {
		if commit.SHA != head && commit.SHA != first && commit.SHA != second {
			t.Fatalf("ancestor leaked into feature history: %#v", commit)
		}
	}
	for _, sha := range []string{unrelated, repo.Head, "HEAD", head[:10], head + "^", "--all", strings.Repeat("0", 40), gitOutput(t, "-C", f.worktree, "rev-parse", head+"^{tree}"), gitOutput(t, "-C", f.worktree, "rev-parse", head+":README")} {
		if _, err := reader.ComparisonCommit(context.Background(), repo, pair, sha); err == nil {
			t.Errorf("accepted commit outside saved history: %q", sha)
		}
	}
}
