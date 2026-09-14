package repository

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewGitRanges(t *testing.T) {
	var reported struct {
		Before, After        string
		Diff                 ReviewDiff
		Additions, Deletions int64
	}
	data, err := os.ReadFile("../../review-ui/fixtures/oauth2.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reported); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ name, before, after string }{
		{"reported", reported.Before, reported.After},
		{"unicode", "same\r\n😀 old\r\nlast", "same\r\n😀 new\r\nlast\n"},
		{"empty", "", ""}, {"add", "", "new\n"}, {"remove", "old\n", ""},
		{"final-newline", "same", "same\n"}, {"lone-cr", "a\rb\n", "a\rc\n"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			f, reader, repo := reviewFixture(t)
			name := filepath.Join(f.worktree, "source")
			for _, value := range []string{fixture.before, fixture.after} {
				if err := os.WriteFile(name, []byte(value), 0644); err != nil {
					t.Fatal(err)
				}
				runGit(t, "-C", f.worktree, "add", "source")
				runGit(t, "-C", f.worktree, "commit", "--allow-empty", "-m", "source")
			}
			head := gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
			base := gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD^")
			files, err := reader.Files(context.Background(), repo, ReviewPair{Base: base, Head: head})
			if err != nil {
				t.Fatal(err)
			}
			if fixture.before == fixture.after {
				if len(files) != 0 {
					t.Fatal(files)
				}
				return
			}
			if len(files) != 1 {
				t.Fatal(files)
			}
			content, err := reader.Content(context.Background(), repo, files[0])
			if err != nil || content.Diff == nil || content.DiffError != "" {
				t.Fatalf("diff=%#v error=%v", content, err)
			}
			var added, removed int64
			for _, change := range content.Diff.Changes {
				added += int64(change.NewLines)
				removed += int64(change.OldLines)
			}
			if added != *files[0].Additions || removed != *files[0].Deletions {
				t.Fatalf("counts: +%d -%d", added, removed)
			}
			if fixture.name == "reported" {
				actual, _ := json.Marshal(content.Diff)
				expected, _ := json.Marshal(reported.Diff)
				if string(actual) != string(expected) || added != 116 || removed != 6 {
					t.Fatalf("reported ranges: %s +%d -%d", actual, added, removed)
				}
			}
		})
	}
}

func TestLargeReviewDiffBoundary(t *testing.T) {
	for _, count := range []int64{1999, 2000, 2001} {
		added, removed := count-1, int64(1)
		file := ReviewFile{Additions: &added, Deletions: &removed}
		if file.LargeDiff() != (count > 2000) {
			t.Fatalf("boundary %d", count)
		}
	}
	if (ReviewFile{}).LargeDiff() {
		t.Fatal("binary metadata considered large")
	}
}

func TestReviewRejectsInconsistentStats(t *testing.T) {
	_, reader, repo := reviewFixture(t)
	count := int64(2)
	_, err := reader.fileDiff(context.Background(), repo, ReviewFile{Additions: &count}, ReviewBlob{Kind: "absent"}, ReviewBlob{Text: "one\n"})
	if err == nil || !strings.Contains(err.Error(), "statistics") {
		t.Fatalf("err=%v", err)
	}
}
