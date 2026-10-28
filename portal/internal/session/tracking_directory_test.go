package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func directoryFixture(t *testing.T) (*Summary, string) {
	t.Helper()
	summary := &Summary{Manifest: Manifest{Slug: "files"}, Workspace: t.TempDir(), Root: "work"}
	root := filepath.Join(summary.Workspace, "work", "files")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	return summary, root
}

func directoryWrite(t *testing.T, root, path string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, path), nil, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestTrackingDirectoryLazyAndConfined(t *testing.T) {
	summary, root := directoryFixture(t)
	for _, path := range []string{"bulk", "empty", ".git", "unopened"} {
		if err := os.Mkdir(filepath.Join(root, path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5001; i++ {
		directoryWrite(t, filepath.Join(root, "bulk"), fmt.Sprintf("%05d", i))
	}
	directoryWrite(t, root, "root.md")
	directoryWrite(t, root, ".ignored")
	if err := os.Symlink("bulk", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "unopened"), 0000); err != nil {
		t.Fatal(err)
	}
	large, err := os.Create(filepath.Join(root, "sparse.log"))
	if err != nil {
		t.Fatal(err)
	}
	err = large.Truncate(1 << 40)
	large.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, archived := range []bool{false, true} {
		if archived {
			archive := filepath.Join(summary.Workspace, "archive")
			if err := os.Mkdir(archive, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(root, filepath.Join(archive, "files")); err != nil {
				t.Fatal(err)
			}
			summary.Root, summary.Archived = "archive", true
		}
		page, err := ListTrackingDirectory(context.Background(), summary, "", "")
		want := []TrackingDirectoryEntry{{"bulk", "directory"}, {"empty", "directory"}, {"unopened", "directory"}, {".ignored", "file"}, {"root.md", "file"}, {"sparse.log", "file"}}
		if err != nil || !reflect.DeepEqual(page.Entries, want) || page.NextCursor != nil {
			t.Fatalf("root = %#v, %v", page, err)
		}
		empty, err := ListTrackingDirectory(context.Background(), summary, "empty", "")
		if err != nil || empty.Entries == nil || len(empty.Entries) != 0 {
			t.Fatalf("empty = %#v, %v", empty, err)
		}
		for _, path := range []string{"../other", "a/../bulk", "a//b", "bulk/", ".git", "x/.GiT/y", "\\bulk", "/bulk", "a\nb", "\xff"} {
			if _, err := ListTrackingDirectory(context.Background(), summary, path, ""); !errors.Is(err, ErrTrackingDirectoryInput) {
				t.Fatalf("accepted %q: %v", path, err)
			}
		}
		if _, err := ListTrackingDirectory(context.Background(), summary, "link", ""); err == nil {
			t.Fatal("followed directory symlink")
		}
	}
}

func TestTrackingDirectoryAlphabeticalPages(t *testing.T) {
	for _, count := range []int{0, 200, 201, 5001} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			summary, root := directoryFixture(t)
			want := []TrackingDirectoryEntry{}
			for i := count - 1; i >= 0; i-- {
				name := fmt.Sprintf("%05d", i)
				directoryWrite(t, root, name)
				want = append(want, TrackingDirectoryEntry{name, "file"})
			}
			if count > 201 {
				for _, name := range []string{"z-dir", "A-dir", "č-dir"} {
					if err := os.Mkdir(filepath.Join(root, name), 0755); err != nil {
						t.Fatal(err)
					}
					want = append(want, TrackingDirectoryEntry{name, "directory"})
				}
				for _, name := range []string{"quote\"<>&", "Ž-file", "a-file"} {
					directoryWrite(t, root, name)
					want = append(want, TrackingDirectoryEntry{name, "file"})
				}
			}
			sortDirectoryEntries(want)
			got := []TrackingDirectoryEntry{}
			cursor := ""
			for {
				page, err := ListTrackingDirectory(context.Background(), summary, "", cursor)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(page)
				if len(page.Entries) > 200 || len(encoded)+1 > TrackingDirectoryMaxBytes {
					t.Fatal("page exceeded resource bound")
				}
				got = append(got, page.Entries...)
				if page.NextCursor == nil {
					break
				}
				if len(page.Entries) == 0 || *page.NextCursor == cursor || len(*page.NextCursor) > trackingCursorMaxBytes {
					t.Fatal("nonadvancing cursor")
				}
				cursor = *page.NextCursor
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pages contain %d entries, want %d", len(got), len(want))
			}
		})
	}
}

func sortDirectoryEntries(entries []TrackingDirectoryEntry) {
	// Independent expected order: directory first, then Go's UTF-8 byte order.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].Path < entries[j].Path
	})
}

func TestTrackingDirectoryByteBudgetAndCompactCursor(t *testing.T) {
	summary, root := directoryFixture(t)
	parts := make([]string, 14)
	for i := range parts {
		parts[i] = strings.Repeat("p", 180)
	}
	path := strings.Join(parts, "/")
	dir := filepath.Join(root, path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 201; i++ {
		directoryWrite(t, dir, fmt.Sprintf("%03d", i))
	}
	page, err := ListTrackingDirectory(context.Background(), summary, path, "")
	if err != nil || len(page.Entries) == 0 || len(page.Entries) >= 200 || page.NextCursor == nil {
		t.Fatalf("byte-limited page = %d / %v", len(page.Entries), err)
	}
	encoded, _ := json.Marshal(page)
	if len(encoded)+1 > TrackingDirectoryMaxBytes || len(*page.NextCursor) > 4096 {
		t.Fatal("response/cursor too large")
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(*page.NextCursor)
	if strings.Contains(string(decoded), path) {
		t.Fatal("cursor repeats parent path")
	}
	next, err := ListTrackingDirectory(context.Background(), summary, path, *page.NextCursor)
	if err != nil || next.Entries[0].Path <= page.Entries[len(page.Entries)-1].Path {
		t.Fatalf("byte budget skipped or repeated entries: %v", err)
	}
}

func TestTrackingDirectoryCursorValidationAndChanges(t *testing.T) {
	summary, root := directoryFixture(t)
	for i := 0; i < 201; i++ {
		directoryWrite(t, root, fmt.Sprintf("%03d", i))
	}
	page, err := ListTrackingDirectory(context.Background(), summary, "", "")
	if err != nil || page.NextCursor == nil {
		t.Fatal(err)
	}
	cursor := *page.NextCursor
	data, _ := base64.RawURLEncoding.DecodeString(cursor)
	for _, malformed := range []string{"!", strings.Repeat("a", 4097), "{}", string(data) + "{}", strings.Replace(string(data), `"v":1`, `"v":1,"v":1`, 1), strings.Replace(string(data), `"v":1`, `"v":2`, 1), strings.Replace(string(data), `"name":"199"`, `"name":"../199"`, 1), strings.Replace(string(data), `"v":1`, `"extra":1,"v":1`, 1)} {
		encoded := malformed
		if strings.HasPrefix(malformed, "{") {
			encoded = base64.RawURLEncoding.EncodeToString([]byte(malformed))
		}
		if _, err := ListTrackingDirectory(context.Background(), summary, "", encoded); !errors.Is(err, ErrTrackingDirectoryInput) {
			t.Fatalf("accepted cursor %q: %v", malformed, err)
		}
	}
	// Changing bytes inside an existing file does not change directory generation.
	if err := os.WriteFile(filepath.Join(root, "000"), []byte("content edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListTrackingDirectory(context.Background(), summary, "", cursor); err != nil {
		t.Fatalf("content edit invalidated page: %v", err)
	}
	for _, mutation := range []func(){
		func() { directoryWrite(t, root, "added") },
		func() {
			if err := os.Remove(filepath.Join(root, "added")); err != nil {
				t.Fatal(err)
			}
		},
		func() {
			if err := os.Rename(filepath.Join(root, "000"), filepath.Join(root, "renamed")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		page, err = ListTrackingDirectory(context.Background(), summary, "", "")
		if err != nil {
			t.Fatal(err)
		}
		mutation()
		if _, err := ListTrackingDirectory(context.Background(), summary, "", *page.NextCursor); !errors.Is(err, ErrTrackingDirectoryChanged) {
			t.Fatalf("mutation not detected: %v", err)
		}
	}
	other, _ := directoryFixture(t)
	if _, err := ListTrackingDirectory(context.Background(), other, "", cursor); !errors.Is(err, ErrTrackingDirectoryChanged) {
		t.Fatalf("cross-workspace cursor: %v", err)
	}
	archive := filepath.Join(summary.Workspace, "archive")
	if err := os.Mkdir(archive, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(archive, "files")); err != nil {
		t.Fatal(err)
	}
	summary.Root, summary.Archived = "archive", true
	if _, err := ListTrackingDirectory(context.Background(), summary, "", cursor); !errors.Is(err, ErrTrackingDirectoryChanged) {
		t.Fatalf("archive transition: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ListTrackingDirectory(ctx, summary, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// Exercise cancellation and filesystem mutation at a deterministic request
// checkpoint, without racing a background writer or adding scanner test hooks.
type directoryCheckpointContext struct {
	context.Context
	checkpoint func() error
}

func (c directoryCheckpointContext) Err() error { return c.checkpoint() }

func TestTrackingDirectoryScanChangesAndDescriptorClosure(t *testing.T) {
	for _, mutation := range []string{"rename", "replace", "symlink", "cancel", "deadline"} {
		t.Run(mutation, func(t *testing.T) {
			summary, root := directoryFixture(t)
			for i := 0; i < 201; i++ {
				directoryWrite(t, root, fmt.Sprintf("%03d", i))
			}
			checks := 0
			ctx := directoryCheckpointContext{Context: context.Background(), checkpoint: func() error {
				checks++
				if checks < 10 {
					return nil
				}
				if mutation == "cancel" {
					return context.Canceled
				}
				if mutation == "deadline" {
					return context.DeadlineExceeded
				}
				if checks != 10 {
					return nil
				}
				if mutation == "rename" {
					if err := os.Rename(filepath.Join(root, "000"), filepath.Join(root, "renamed")); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(root, root+"-old"); err != nil {
						t.Fatal(err)
					}
					var err error
					if mutation == "replace" {
						err = os.Mkdir(root, 0755)
					} else {
						err = os.Symlink(root+"-old", root)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}}
			_, err := ListTrackingDirectory(ctx, summary, "", "")
			want := ErrTrackingDirectoryChanged
			if mutation == "cancel" {
				want = context.Canceled
			}
			if mutation == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("scan returned %v, want %v", err, want)
			}
			fds, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			for _, fd := range fds {
				target, _ := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
				if strings.HasPrefix(target, summary.Workspace) {
					t.Fatalf("leaked request descriptor: %s", target)
				}
			}
		})
	}
}
