package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reviewFixture(t *testing.T) (repositoryFixture, ReviewReader, ReviewRepository) {
	t.Helper()
	f := newRepositoryFixture(t)
	item := activeRepository()
	item.InitialBaseSHA = f.baseHead
	runGit(t, "--git-dir="+f.commonDir, "update-ref", "refs/remotes/origin/master", f.baseHead)
	reader := ReviewReader{Workspace: f.workspace}
	repo, err := reader.Resolve(context.Background(), testSlug, item, false)
	if err != nil {
		t.Fatal(err)
	}
	return f, reader, repo
}
func TestReviewKeepsUnpushedHistoryAndSavedComparisonAfterIntegration(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	head := commitInWorktree(t, f.worktree, "unpushed")
	repo.Head = head
	pair, err := reader.Pair(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pair.Base != f.baseHead || !strings.Contains(pair.BaseLabel, "origin/master") {
		t.Fatalf("pair = %#v", pair)
	}
	history, err := reader.History(context.Background(), repo, pair, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Commits) != 1 || history.Commits[0].SHA != head {
		t.Fatalf("history = %#v", history)
	}
	runGit(t, "--git-dir="+f.commonDir, "update-ref", "refs/remotes/origin/master", head)
	integrated, err := reader.Pair(context.Background(), repo, &pair)
	if err != nil {
		t.Fatal(err)
	}
	if integrated.Base != pair.Base || !strings.Contains(integrated.BaseLabel, "Last viewed") {
		t.Fatalf("integrated = %#v", integrated)
	}
	fallback, err := reader.Pair(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Base != f.baseHead || !strings.Contains(fallback.BaseLabel, "fallback") || !strings.Contains(fallback.Warning, "rebase") {
		t.Fatalf("fallback = %#v", fallback)
	}
	runGit(t, "--git-dir="+f.commonDir, "worktree", "remove", f.worktree)
	item := activeRepository()
	item.InitialBaseSHA = f.baseHead
	item.FinalHeadSHA = head
	archived, err := reader.Resolve(context.Background(), testSlug, item, true)
	if err != nil {
		t.Fatal(err)
	}
	archivedPair, err := reader.Pair(context.Background(), archived, &pair)
	if err != nil || archivedPair.Base != f.baseHead {
		t.Fatalf("archived = %#v, %v", archivedPair, err)
	}
	if _, err := reader.Resolve(context.Background(), testSlug, item, false); err == nil {
		t.Fatal("missing active worktree accepted")
	}
}
func TestReviewMergeBaseFollowsRebaseAndImmutablePair(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	oldHead := commitInWorktree(t, f.worktree, "feature-change")
	repo.Head = oldHead
	before, err := reader.Pair(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	upstream := commitFrom(t, f, f.baseHead, "upstream")
	runGit(t, "--git-dir="+f.commonDir, "update-ref", "refs/remotes/origin/master", upstream)
	runGit(t, "-C", f.worktree, "rebase", upstream)
	item := activeRepository()
	item.InitialBaseSHA = f.baseHead
	current, err := reader.Resolve(context.Background(), testSlug, item, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reader.Pair(context.Background(), current, &before)
	if err != nil {
		t.Fatal(err)
	}
	if after.Base != upstream || after.Head == oldHead {
		t.Fatalf("rebased pair = %#v", after)
	}
	oldHistory, err := reader.History(context.Background(), repo, before, 0)
	if err != nil || len(oldHistory.Commits) != 1 || oldHistory.Commits[0].SHA != oldHead {
		t.Fatalf("immutable history = %#v, %v", oldHistory, err)
	}
	currentHistory, err := reader.History(context.Background(), current, after, 0)
	if err != nil || len(currentHistory.Commits) != 1 || currentHistory.Commits[0].Subject != "feature-change" {
		t.Fatalf("rebased history = %#v, %v", currentHistory, err)
	}
}
func TestReviewMetadataUnusualPathsAndBlobLimits(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	renamed := "renamed\twith\nnewlines"
	runGit(t, "-C", f.worktree, "mv", "README", renamed)
	files := map[string][]byte{"binary": {0, 1, 2}, "huge": []byte(strings.Repeat("x", MaxReviewBlobBytes+1)), "many-lines": []byte(strings.Repeat("\n", MaxReviewLines+1)), "no-newline": []byte("final line"), "invalid-utf8": {0xff}}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(f.worktree, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../a-file-never-read", filepath.Join(f.worktree, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "add", "--all")
	runGit(t, "-C", f.worktree, "update-index", "--add", "--cacheinfo", "160000,"+f.baseHead+",module")
	runGit(t, "-C", f.worktree, "commit", "-m", "metadata\n\nDetailed commit body.")
	repo.Head = gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	marker := filepath.Join(t.TempDir(), "external-diff")
	runGit(t, "-C", f.worktree, "config", "diff.external", "touch "+marker)
	pair := ReviewPair{Base: f.baseHead, Head: repo.Head}
	changed, err := reader.Files(context.Background(), repo, pair)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("external diff was run")
	}
	found := make(map[string]ReviewContent)
	for _, file := range changed {
		content, err := reader.Content(context.Background(), repo, file)
		if err != nil {
			t.Fatalf("%q: %v", file.Path, err)
		}
		found[file.Path] = content
		if file.Path == renamed && (file.OldPath != "README" || file.Status != "R100") {
			t.Fatalf("rename = %#v", file)
		}
	}
	if found[renamed].After.Text != "base\n" || !found["binary"].After.Binary || !found["invalid-utf8"].After.Binary || !found["huge"].After.Limited || !found["many-lines"].After.Limited || !found["no-newline"].After.MissingNewline || found["link"].After.Text != "../a-file-never-read" || found["link"].After.Kind != "symlink" || found["module"].After.Kind != "submodule" {
		t.Fatalf("unexpected metadata: %#v", found)
	}
	history, err := reader.History(context.Background(), repo, pair, 0)
	if err != nil || history.Commits[0].Body != "Detailed commit body." {
		t.Fatalf("body = %#v, %v", history, err)
	}
}
func TestReviewCommitPaginationAndRootDiff(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	for i := 0; i < 52; i++ {
		runGit(t, "-C", f.worktree, "commit", "--allow-empty", "-m", "commit")
	}
	repo.Head = gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	pair := ReviewPair{Base: f.baseHead, Head: repo.Head}
	first, err := reader.History(context.Background(), repo, pair, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.History(context.Background(), repo, pair, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Commits) != 50 || !first.HasMore || len(second.Commits) != 2 || second.HasMore || first.Commits[0].SHA != repo.Head {
		t.Fatalf("page counts = %d/%d", len(first.Commits), len(second.Commits))
	}
	root, err := reader.CommitPair(context.Background(), repo, ReviewCommit{SHA: f.baseHead})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := reader.Files(context.Background(), repo, root)
	if err != nil || len(changed) != 1 || changed[0].Status != "A" {
		t.Fatalf("root = %#v, %v", changed, err)
	}
}
func TestReviewRejectsWrongBranchAndBoundsGit(t *testing.T) {
	f, reader, _ := reviewFixture(t)
	item := activeRepository()
	item.Name = "../other"
	if _, err := reader.Resolve(context.Background(), testSlug, item, false); err == nil {
		t.Fatal("untrusted repository name accepted")
	}
	item = activeRepository()
	runGit(t, "-C", f.worktree, "checkout", "--detach")
	if _, err := reader.Resolve(context.Background(), testSlug, item, false); err == nil {
		t.Fatal("wrong feature branch accepted")
	}
	if _, err := reader.git(context.Background(), f.commonDir, 1, "log", "--all"); !errors.Is(err, ErrReviewLimit) {
		t.Fatalf("bounded output = %v", err)
	}
	reader.Timeout = time.Nanosecond
	if _, err := reader.git(context.Background(), f.commonDir, 1024, "rev-parse", "HEAD"); err == nil {
		t.Fatal("deadline ignored")
	}
}

func TestReviewKeepsModifiedRenamesAboveGitDefaultLimit(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	runGit(t, "-C", f.worktree, "config", "gc.auto", "0")
	runGit(t, "-C", f.worktree, "config", "maintenance.auto", "false")
	const count = 1001
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("before-%04d", i)
		content := strings.Repeat(fmt.Sprintf("unique file %04d content\n", i), 20)
		if err := os.WriteFile(filepath.Join(f.worktree, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, "-C", f.worktree, "add", ".")
	runGit(t, "-C", f.worktree, "commit", "-m", "add rename candidates")
	base := gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	for i := 0; i < count; i++ {
		oldPath := filepath.Join(f.worktree, fmt.Sprintf("before-%04d", i))
		content, err := os.ReadFile(oldPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(oldPath); err != nil {
			t.Fatal(err)
		}
		newPath := filepath.Join(f.worktree, fmt.Sprintf("after-%04d", i))
		if err := os.WriteFile(newPath, append(content, []byte("a changed line\n")...), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, "-C", f.worktree, "add", "-A")
	runGit(t, "-C", f.worktree, "commit", "-m", "rename and edit candidates")
	head := gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	files, err := reader.Files(context.Background(), repo, ReviewPair{Base: base, Head: head})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != count {
		t.Fatalf("got %d changes, want %d renames", len(files), count)
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Status, "R") || file.OldPath == "" {
			t.Fatalf("lost modified rename: %#v", file)
		}
	}
}
