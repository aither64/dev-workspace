package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func gitDirectoryInventory(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	result := make(map[string][32]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[name] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAdmitWorkingEntryBoundsRetainedPreviewsAndSharedEntries(t *testing.T) {
	working := make(map[string]workEntry)
	changed := make(map[string]struct{})
	var retained int64
	preview := strings.Repeat("x", MaxReviewBlobBytes)
	item := workEntry{blob: ReviewBlob{Kind: "file", Text: preview}}
	for i := 0; i < MaxWorktreeSnapshotBytes/MaxReviewBlobBytes; i++ {
		if err := admitWorkingEntry(working, changed, 0, &retained, fmt.Sprintf("file-%03d", i), item); err != nil {
			t.Fatal(err)
		}
	}
	if retained != MaxWorktreeSnapshotBytes || len(working) != MaxWorktreeSnapshotBytes/MaxReviewBlobBytes {
		t.Fatalf("retained working previews = %d bytes in %d entries", retained, len(working))
	}
	if err := admitWorkingEntry(working, changed, 0, &retained, "overflow", item); !errors.Is(err, ErrReviewLimit) {
		t.Fatalf("over-budget preview retained: %v", err)
	}
	if _, retainedOverLimit := working["overflow"]; retainedOverLimit || retained != MaxWorktreeSnapshotBytes {
		t.Fatal("rejected preview changed the working set")
	}
	metadata := workEntry{blob: ReviewBlob{Kind: "file", Limited: true}}
	if err := admitWorkingEntry(working, changed, 0, &retained, "large-metadata", metadata); err != nil {
		t.Fatalf("metadata-only candidate consumed preview bytes: %v", err)
	}
	if err := admitWorkingEntry(working, changed, maxReviewFiles-len(changed), &retained, "entry-overflow", metadata); !errors.Is(err, ErrReviewLimit) {
		t.Fatalf("combined changed/unverified entry limit was bypassed: %v", err)
	}
}

func TestWorktreeCaptureSeparatesLayersAndKeepsFrozenContent(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	path := filepath.Join(f.worktree, "README")
	if err := os.WriteFile(path, []byte("index\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "add", "README")
	if err := os.WriteFile(path, []byte("working\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, ".gitignore"), []byte("ignored\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "ignored"), []byte("private\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "untracked file"), []byte("new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	beforeIndex, err := os.ReadFile(filepath.Join(f.commonDir, "worktrees", filepath.Base(f.worktree), "index"))
	if err != nil {
		t.Fatal(err)
	}
	beforeGit := gitDirectoryInventory(t, f.commonDir)
	staged, err := reader.CaptureWorktree(context.Background(), repo, "staged")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeGit, gitDirectoryInventory(t, f.commonDir)) {
		t.Fatal("capture changed canonical Git files")
	}
	unstaged, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Files) != 1 || staged.Files[0].Path != "README" || staged.Contents[staged.Files[0].ID].After.Text != "index\n" {
		t.Fatalf("staged = %#v", staged)
	}
	found := map[string]ReviewContent{}
	for _, file := range unstaged.Files {
		found[file.Path] = unstaged.Contents[file.ID]
	}
	if found["README"].Before.Text != "index\n" || found["README"].After.Text != "working\n" || found["untracked file"].After.Text != "new\n" || found["ignored"].After.Kind != "" {
		t.Fatalf("unstaged = %#v", found)
	}
	if err := os.WriteFile(path, []byte("later\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if unstaged.Contents[unstaged.Files[0].ID].After.Text == "later\n" {
		t.Fatal("snapshot followed working file")
	}
	afterIndex, err := os.ReadFile(filepath.Join(f.commonDir, "worktrees", filepath.Base(f.worktree), "index"))
	if err != nil || string(afterIndex) != string(beforeIndex) {
		t.Fatalf("real index changed: %v", err)
	}
}

func TestWorktreeCaptureSkipsUnchangedLargeFilesAndFilters(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	large := filepath.Join(f.worktree, "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", MaxReviewBlobBytes+1024)), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "add", "large")
	runGit(t, "-C", f.worktree, "commit", "-m", "large")
	repo.Head = gitOutput(t, "-C", f.worktree, "rev-parse", "HEAD")
	markers := t.TempDir()
	filterMarker := filepath.Join(markers, "filter-ran")
	runGit(t, "-C", f.worktree, "config", "filter.hostile.clean", "sh -c 'touch "+filterMarker+"; cat'")
	runGit(t, "-C", f.worktree, "config", "core.fsmonitor", "sh -c 'touch "+filepath.Join(markers, "fsmonitor-ran")+"'")
	runGit(t, "-C", f.worktree, "config", "diff.external", "sh -c 'touch "+filepath.Join(markers, "external-diff-ran")+"'")
	runGit(t, "-C", f.worktree, "config", "diff.hostile.textconv", "sh -c 'touch "+filepath.Join(markers, "textconv-ran")+"'")
	if err := os.WriteFile(filepath.Join(f.worktree, ".gitattributes"), []byte("README filter=hostile diff=hostile\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "README"), []byte("edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Status and name-only diffs can skip the filter; hash-object checks it without writing an object.
	runGit(t, "-C", f.worktree, "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "hash-object", "--path=README", "README")
	if _, err := os.Stat(filterMarker); err != nil {
		t.Fatalf("Git control did not exercise the clean filter: %v", err)
	}
	if err := os.Remove(filterMarker); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fsmonitor-ran", "external-diff-ran", "textconv-ran"} {
		if err := os.Remove(filepath.Join(markers, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err = filepath.Abs(realGit)
	if err != nil {
		t.Fatal(err)
	}
	guardDir := filepath.Join(t.TempDir(), "git wrapper's files")
	if err := os.Mkdir(guardDir, 0700); err != nil {
		t.Fatal(err)
	}
	callsLog := filepath.Join(guardDir, "calls")
	violationsLog := filepath.Join(guardDir, "violations")
	shellQuote := func(path string) string {
		return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
	}
	guard := fmt.Sprintf(`#!/bin/sh
set -eu
real_git=%s
calls=%s
violations=%s
deny() {
  printf '%%s\n' "$*" >> "$violations"
  exit 97
}
inspect() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --no-pager|--no-replace-objects) shift ;;
      -c|-C)
        [ "$#" -ge 2 ] || deny "missing value for $1"
        shift 2
        ;;
      -*) deny "unknown Git prefix: $1" ;;
      *) break ;;
    esac
  done
  [ "$#" -gt 0 ] || deny "missing Git command"
  command=$1
  shift
  case "$command" in
    status|diff-files) deny "$command $*" ;;
    ls-files)
      if [ "$#" -eq 2 ] && { [ "$1" = "--stage" ] || [ "$1" = "--debug" ] || [ "$1" = "-v" ]; } && [ "$2" = "-z" ]; then
        :
      elif [ "$#" -eq 3 ] && [ "$1" = "--others" ] && [ "$2" = "--exclude-standard" ] && [ "$3" = "-z" ]; then
        :
      else
        deny "$command $*"
      fi
      ;;
  esac
  printf '%%s\n' "$command $*" >> "$calls"
}
inspect "$@"
exec "$real_git" "$@"
`, shellQuote(realGit), shellQuote(callsLog), shellQuote(violationsLog))
	if err := os.WriteFile(filepath.Join(guardDir, "git"), []byte(guard), 0700); err != nil {
		t.Fatal(err)
	}
	t.Run("guarded capture", func(t *testing.T) {
		t.Setenv("PATH", guardDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		probes := [][]string{
			{"--no-pager", "-c", "core.fsmonitor=false", "-C", f.worktree, "status", "--porcelain"},
			{"diff-files", "--name-only"},
			{"ls-files", "-m"},
			{"ls-files", "--modified"},
			{"ls-files", "-mz"},
			{"ls-files", "--stage", "-m", "-z"},
		}
		for _, args := range probes {
			command := exec.Command("git", args...)
			err := command.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 97 {
				t.Fatalf("Git guard accepted %q: %v", args, err)
			}
		}
		denied, err := os.ReadFile(violationsLog)
		if err != nil || len(strings.Split(strings.TrimSpace(string(denied)), "\n")) != len(probes) {
			t.Fatalf("Git guard did not record every rejected probe: %q: %v", denied, err)
		}
		for _, path := range []string{callsLog, violationsLog} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
		checkGuard := func(from int) int {
			t.Helper()
			if denied, err := os.ReadFile(violationsLog); err == nil {
				t.Fatalf("capture called prohibited Git command: %s", denied)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(callsLog)
			if err != nil {
				t.Fatal(err)
			}
			if from > len(calls) || !strings.Contains(string(calls[from:]), "ls-files --stage -z\n") ||
				!strings.Contains(string(calls[from:]), "ls-files --debug -z\n") {
				t.Fatalf("capture did not use guarded stage/debug reads: %q", calls[from:])
			}
			return len(calls)
		}
		capture, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"filter-ran", "fsmonitor-ran", "external-diff-ran", "textconv-ran"} {
			if _, err := os.Stat(filepath.Join(markers, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("hostile Git hook %s executed: %v", name, err)
			}
		}
		lastCall := checkGuard(0)
		for _, file := range capture.Files {
			if file.Path == "large" {
				t.Fatal("unchanged large file opened or reported")
			}
		}
		if err := os.WriteFile(large, []byte(strings.Repeat("y", MaxReviewBlobBytes+1024)), 0644); err != nil {
			t.Fatal(err)
		}
		capture, err = reader.CaptureWorktree(context.Background(), repo, "unstaged")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"filter-ran", "fsmonitor-ran", "external-diff-ran", "textconv-ran"} {
			if _, err := os.Stat(filepath.Join(markers, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("hostile Git hook %s executed: %v", name, err)
			}
		}
		checkGuard(lastCall)
		for _, file := range capture.Files {
			if file.Path == "large" {
				content := capture.Contents[file.ID]
				if !file.Limited || file.Additions != nil || file.Deletions != nil || !content.After.Limited || file.Warning == "" {
					t.Fatalf("large candidate claims exact content: %#v %#v", file, content)
				}
				stats := FileStats(capture.Files)
				if stats.LimitedFiles != 1 || !stats.LineCountsIncomplete {
					t.Fatalf("large candidate claims complete line counts: %#v", stats)
				}
				return
			}
		}
		t.Fatal("large candidate omitted")
	})
}

func TestWorktreeCaptureRacySameSizeAndIndexRefusals(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	path := filepath.Join(f.worktree, "README")
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old.ModTime(), old.ModTime()); err != nil {
		t.Fatal(err)
	}
	capture, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil || len(capture.Files) != 1 || capture.Contents[capture.Files[0].ID].After.Text != "edit\n" {
		t.Fatalf("same-size edit = %#v, %v", capture, err)
	}
	// A path with skip-worktree must fail rather than silently use stale bytes.
	runGit(t, "-C", f.worktree, "update-index", "--skip-worktree", "README")
	_, err = reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err == nil || !strings.Contains(err.Error(), "skip-worktree") {
		t.Fatalf("skip-worktree = %v", err)
	}
	runGit(t, "-C", f.worktree, "update-index", "--no-skip-worktree", "README")
	// Ensure an index-write timestamp equal to the cached mtime is conservative.
	if indexMatchesWorking(indexStamp{ctimeSec: 1, mtimeSec: 1, dev: 1, ino: 1, size: 5},
		workStamp{dev: 1, ino: 1, mode: unixRegularMode(), size: 5, ctimeSec: 1, mtimeSec: 1}, "100644", time.Unix(1, 0)) {
		t.Fatal("racy-clean stat tuple skipped")
	}
}

func TestWorktreeCaptureIntentToAddAndUnsafePaths(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	if err := os.WriteFile(filepath.Join(f.worktree, "intent"), []byte("working only\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", f.worktree, "add", "-N", "intent")
	staged, err := reader.CaptureWorktree(context.Background(), repo, "staged")
	if err != nil || len(staged.Files) != 0 {
		t.Fatalf("intent-to-add staged = %#v, %v", staged.Files, err)
	}
	unstaged, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil {
		t.Fatal(err)
	}
	if len(unstaged.Files) != 1 || unstaged.Files[0].Path != "intent" || unstaged.Contents[unstaged.Files[0].ID].After.Text != "working only\n" {
		t.Fatalf("intent-to-add unstaged = %#v", unstaged)
	}
	if _, err := parseIndexEntries([]byte("100644 "+strings.Repeat("a", 40)+" 2\tconflict\x00"), map[string]indexStamp{"conflict": {}}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("unmerged stage accepted: %v", err)
	}
	if validReviewPath("bad\xff") || validReviewPath("../escape") || !validReviewPath("space and ünicode") || !validReviewPath("line\nbreak") {
		t.Fatal("unsafe path validation")
	}
}

func TestWorktreeCaptureRetriesConcurrentWriteAndFailsClosed(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	path := filepath.Join(f.worktree, "README")
	if err := os.WriteFile(path, []byte("first\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writes := 0
	reader.captureHook = func() {
		writes++
		if writes == 1 {
			if err := os.WriteFile(path, []byte("second\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	capture, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil || writes != 2 || capture.Contents[capture.Files[0].ID].After.Text != "second\n" {
		t.Fatalf("concurrent retry = %#v, writes=%d, err=%v", capture, writes, err)
	}
	reader.captureHook = func() {
		writes++
		if err := os.WriteFile(path, []byte(strings.Repeat("x", writes)+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.CaptureWorktree(context.Background(), repo, "unstaged"); !errors.Is(err, ErrWorktreeChanged) {
		t.Fatalf("continuous writes did not fail closed: %v", err)
	}
}

func TestWorktreeCaptureDoesNotFollowSymlinksOrReadSpecialFiles(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("never serve\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.worktree, "README")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	capture, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil || len(capture.Files) != 1 {
		t.Fatalf("symlink capture = %#v, %v", capture, err)
	}
	after := capture.Contents[capture.Files[0].ID].After
	if after.Kind != "symlink" || after.Text != outside || strings.Contains(after.Text, "never serve") {
		t.Fatalf("followed symlink: %#v", after)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.CaptureWorktree(context.Background(), repo, "unstaged"); err == nil || !strings.Contains(err.Error(), "special") {
		t.Fatalf("special file accepted: %v", err)
	}
}

func TestWorktreeCaptureModeDeleteAndControlPath(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	if err := os.Chmod(filepath.Join(f.worktree, "README"), 0755); err != nil {
		t.Fatal(err)
	}
	name := "space ünicode\nname"
	if err := os.WriteFile(filepath.Join(f.worktree, name), []byte("without newline"), 0644); err != nil {
		t.Fatal(err)
	}
	capture, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]ReviewFile)
	for _, file := range capture.Files {
		files[file.Path] = file
	}
	if files["README"].OldMode != "100644" || files["README"].NewMode != "100755" || files[name].Path != name || !capture.Contents[files[name].ID].After.MissingNewline {
		t.Fatalf("mode/path = %#v", files)
	}
	if err := os.Remove(filepath.Join(f.worktree, "README")); err != nil {
		t.Fatal(err)
	}
	capture, err = reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range capture.Files {
		if file.Path == "README" && file.Status == "D" && file.NewMode == "000000" {
			return
		}
	}
	t.Fatal("tracked deletion missing")
}

func TestWorktreeCaptureSeparatesUnverifiedSubmoduleMetadata(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	runGit(t, "-C", f.worktree, "update-index", "--add", "--cacheinfo", "160000,"+f.baseHead+",module")
	if err := os.Mkdir(filepath.Join(f.worktree, "module"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "README"), []byte("ordinary change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	staged, err := reader.CaptureWorktree(context.Background(), repo, "staged")
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Files) != 1 || staged.Files[0].Path != "module" || staged.Contents[staged.Files[0].ID].After.Kind != "submodule" {
		t.Fatalf("staged gitlink = %#v", staged)
	}
	unstaged, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil {
		t.Fatal(err)
	}
	if len(unstaged.Files) != 1 || unstaged.Files[0].Path != "README" || len(unstaged.UnverifiedSubmodules) != 1 ||
		unstaged.UnverifiedSubmodules[0] != (WorktreeSubmodule{Path: "module", Mode: "160000", Object: f.baseHead}) {
		t.Fatalf("unverified gitlink leaked into changes: %#v", unstaged)
	}
	if err := os.Remove(filepath.Join(f.worktree, "README")); err != nil {
		t.Fatal(err)
	}
	if unstaged.Contents[unstaged.Files[0].ID].After.Text != "ordinary change\n" {
		t.Fatal("snapshot followed working file")
	}
	if err := os.Remove(filepath.Join(f.worktree, "module")); err != nil {
		t.Fatal(err)
	}
	missing, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil || len(missing.Files) != 1 || len(missing.UnverifiedSubmodules) != 1 {
		t.Fatalf("missing gitlink claimed as a verified change: %#v, %v", missing, err)
	}
}

func TestWorktreeCaptureBoundsUnverifiedSubmoduleMetadata(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	var input strings.Builder
	for i := 0; i <= maxReviewFiles; i++ {
		fmt.Fprintf(&input, "160000 %s\tmodules/%05d\n", f.baseHead, i)
	}
	command := exec.Command("git", "-C", f.worktree, "update-index", "--index-info")
	command.Stdin = strings.NewReader(input.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("populate gitlinks: %v: %s", err, output)
	}
	if _, err := reader.CaptureWorktree(context.Background(), repo, "unstaged"); !errors.Is(err, ErrReviewLimit) {
		t.Fatalf("unverified gitlinks exceeded the shared file limit: %v", err)
	}
}

func TestWorktreeCaptureUsesReadOnlyExpandedSplitIndex(t *testing.T) {
	f, reader, repo := reviewFixture(t)
	runGit(t, "-C", f.worktree, "update-index", "--split-index")
	if err := os.WriteFile(filepath.Join(f.worktree, "README"), []byte("changed through split index\n"), 0644); err != nil {
		t.Fatal(err)
	}
	capture, err := reader.CaptureWorktree(context.Background(), repo, "unstaged")
	if err != nil || len(capture.Files) != 1 || capture.Contents[capture.Files[0].ID].After.Text != "changed through split index\n" {
		t.Fatalf("split index capture = %#v, %v", capture, err)
	}
	if _, err := parseIndexEntries([]byte("040000 "+strings.Repeat("a", 40)+" 0\tsparse\x00"), map[string]indexStamp{"sparse": {}}); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("sparse index entry accepted: %v", err)
	}
}

func unixRegularMode() uint64 { return 0100000 | 0644 }
