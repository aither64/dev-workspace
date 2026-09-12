package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewFileStatsAndFullCommitMessage(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	for name, content := range map[string]string{"delete.txt": "one\ntwo\n", "modify.txt": "a\nb\n", "mode.txt": "unchanged\n", "typed": "before\n"} {
		if err := os.WriteFile(filepath.Join(f.worktree, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, "-C", f.worktree, "add", ".")
	runGit(t, "-C", f.worktree, "commit", "-m", "stats base")
	base := gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	unusual := "renamed\twith\nnewlines"
	runGit(t, "-C", f.worktree, "mv", "README", unusual)
	if err := os.Remove(filepath.Join(f.worktree, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "modify.txt"), []byte("a\nc\nd\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "added\tname\n.txt"), []byte("new first\nnew second\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "binary"), []byte{0, 1, 2}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.worktree, "mode.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.worktree, "typed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(f.worktree, "typed")); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "add", "-A")
	message := "subject first line\ncontinued subject\n\nBody paragraph.\n\n  Indented detail.\n\n"
	file := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(file, []byte(message), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "commit", "--cleanup=verbatim", "-F", file)
	head := gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	pair := ReviewPair{Base: base, Head: head}
	files, err := reader.Files(context.Background(), repo, pair)
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]ReviewFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	expectations := map[string]struct {
		status         string
		added, deleted int64
	}{"delete.txt": {"D", 0, 2}, "modify.txt": {"M", 2, 1}, "mode.txt": {"M", 0, 0}, unusual: {"R100", 0, 0}, "added\tname\n.txt": {"A", 2, 0}, "typed": {"T", 1, 1}}
	for path, want := range expectations {
		file, ok := byPath[path]
		if !ok || file.Status != want.status || file.Additions == nil || file.Deletions == nil || *file.Additions != want.added || *file.Deletions != want.deleted {
			t.Errorf("%q=%#v, want %#v", path, file, want)
		}
	}
	if file, ok := byPath["binary"]; !ok || file.Additions != nil || file.Deletions != nil {
		t.Fatalf("binary stats=%#v", file)
	}
	stats := FileStats(files)
	if stats.Files != 7 || stats.Additions != 5 || stats.Deletions != 4 || stats.BinaryFiles != 1 {
		t.Fatalf("totals=%#v", stats)
	}
	history, err := reader.History(context.Background(), repo, pair, 0)
	if err != nil || len(history.Commits) != 1 || history.Commits[0].Message != message {
		t.Fatalf("full message=%#v, %v", history, err)
	}
	commit, err := reader.ComparisonCommit(context.Background(), repo, pair, head)
	if err != nil || commit.Message != message {
		t.Fatalf("restored full message=%q, %v", commit.Message, err)
	}
}

func TestReviewStatsRejectMismatchedAndIncompleteRecords(t *testing.T) {
	oid := strings.Repeat("a", 40)
	raw := ":100644 100644 " + oid + " " + oid + " M\x00path\x00"
	for _, stats := range []string{"", "1\t2\tother\x00", "1\t2\tpath\x001\t2\tpath\x00", "-\t2\tpath\x00", "-1\t2\tpath\x00", "1\t2\t\x00old\x00", "1\t2\tpath"} {
		if _, err := parseReviewFiles([]byte(raw + stats)); err == nil {
			t.Errorf("accepted malformed numstat %q", stats)
		}
	}
	if files, err := parseReviewFiles(nil); err != nil || len(files) != 0 {
		t.Fatalf("empty diff=%#v %v", files, err)
	}
}

func TestReviewGitBudgetDeadlineIncludesAdmission(t *testing.T) {
	f, reader, _ := reviewFixture(t)
	reader.Jobs = make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		reader.Jobs <- struct{}{}
	}
	reader.Timeout = 20 * time.Millisecond
	started := time.Now()
	_, err := reader.git(context.Background(), f.commonDir, 1024, "rev-parse", "HEAD")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("queued read did not honor deadline: %v", err)
	}
	<-reader.Jobs
	reader.Timeout = time.Second
	if _, err := reader.git(context.Background(), f.commonDir, 1024, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
}
