package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProgressStatsExposeExpansionBeforeAndAfterCommit(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.worktree, path), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "Client usage\n")
	runGit(t, "-C", f.worktree, "add", "README.md")
	write("environment.rb", strings.Repeat("inventory entry\n", 1307))
	write("Gemfile.lock", strings.Repeat("dependency\n", 500))
	write("binary", "\x00data")
	files, err := reader.ProgressFiles(context.Background(), repo, f.baseHead)
	if err != nil {
		t.Fatal(err)
	}
	before := FileStats(files)
	if before.Files != 4 || before.Additions != 1808 || before.BinaryFiles != 1 {
		t.Fatal(before)
	}
	for _, file := range files {
		if file.Path == "environment.rb" && ProgressCategory(file.Path) != "implementation/other" {
			t.Fatal("handwritten expansion hidden")
		}
		if file.Path == "Gemfile.lock" && ProgressCategory(file.Path) != "lock/dependency" {
			t.Fatal("legitimate lock growth misclassified")
		}
	}
	runGit(t, "-C", f.worktree, "add", ".")
	runGit(t, "-C", f.worktree, "commit", "-m", "fixture")
	files, err = reader.ProgressFiles(context.Background(), repo, f.baseHead)
	if err != nil || FileStats(files) != before {
		t.Fatal("commit hid or doubled work", FileStats(files), err)
	}
	write("environment.rb", "small conventional fix\n")
	files, err = reader.ProgressFiles(context.Background(), repo, f.baseHead)
	if err != nil || FileStats(files).Additions != 502 {
		t.Fatal("net simplification was not visible", FileStats(files), err)
	}
}

func TestProgressOversizedUntrackedEvidenceIsExplicit(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	if err := os.WriteFile(filepath.Join(f.worktree, "large"), []byte(strings.Repeat("x", MaxReviewBlobBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := reader.ProgressFiles(context.Background(), repo, f.baseHead)
	if err != nil || !FileStats(files).LineCountsIncomplete {
		t.Fatal(files, err)
	}
}
