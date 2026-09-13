package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewTotalMatchesFullHistoryAcrossPageBoundaries(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	wanted := map[int]bool{0: true, 1: true, 50: true, 51: true, 101: true}
	for count := 0; count <= 101; count++ {
		if count > 0 {
			runGit(t, "-C", f.worktree, "commit", "--allow-empty", "-m", fmt.Sprintf("change %d", count))
		}
		if !wanted[count] {
			continue
		}
		head := strings.TrimSpace(gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD"))
		pair := ReviewPair{Base: f.baseHead, Head: head}
		total, err := reader.CommitCount(context.Background(), repo, pair)
		if err != nil || total != int64(count) {
			t.Fatalf("count %d: total=%d, err=%v", count, total, err)
		}
		for page := 0; page <= count/ReviewPageSize; page++ {
			history, err := reader.History(context.Background(), repo, pair, page)
			remaining := max(0, count-page*ReviewPageSize)
			if err != nil || len(history.Commits) != min(ReviewPageSize, remaining) || history.HasMore != (remaining > ReviewPageSize) {
				t.Fatalf("count %d, page %d: %#v, %v", count, page, history, err)
			}
		}
	}
}

func TestReviewTotalsDescribeNetTreesIncludingRenamesAndBinaryFiles(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	path := filepath.Join(f.worktree, "temporary")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "add", ".")
	runGit(t, "-C", f.worktree, "commit", "-m", "add temporary lines")
	runGit(t, "-C", f.worktree, "rm", "temporary")
	runGit(t, "-C", f.worktree, "mv", "README", "renamed")
	if err := os.WriteFile(filepath.Join(f.worktree, "binary"), []byte{0, 1, 2}, 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "add", ".")
	runGit(t, "-C", f.worktree, "commit", "-m", "remove temporary lines and rename")
	head := strings.TrimSpace(gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD"))
	pair := ReviewPair{Base: f.baseHead, Head: head}
	files, err := reader.Files(context.Background(), repo, pair)
	if err != nil {
		t.Fatal(err)
	}
	stats := FileStats(files)
	if stats.Files != 2 || stats.BinaryFiles != 1 || stats.Additions != 0 || stats.Deletions != 0 {
		t.Fatalf("net stats: %#v", stats)
	}
	count, err := reader.CommitCount(context.Background(), repo, pair)
	if err != nil || count != 2 {
		t.Fatalf("count=%d, err=%v", count, err)
	}
}
