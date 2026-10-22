package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

func TestTrackingFilesConfinedWithoutGit(t *testing.T) {
	workspace := t.TempDir()
	summary := &session.Summary{Manifest: session.Manifest{Slug: "files"}, Workspace: workspace, Root: "work"}
	root := filepath.Join(workspace, "work", "files")
	for name, text := range map[string]string{
		"ignored.txt": "original\r\n", ".gitignore": "*\n", "nested/unregistered.sh": "passive",
		".git/config": "git metadata", "nested/.GIT/config": "nested git metadata",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"link", "linked-dir"} {
		if err := os.Symlink(root, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	reader := ReviewReader{Workspace: workspace}
	files, err := reader.TrackingFiles(context.Background(), summary)
	if err != nil {
		t.Fatal(err)
	}
	want := []TrackingFile{{Path: ".gitignore"}, {Path: "ignored.txt"}, {Path: "nested/unregistered.sh"}}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("listing=%#v", files)
	}
	file, err := reader.TrackingSource(context.Background(), summary, "ignored.txt")
	if err != nil || file.Content.Text != "original\r\n" {
		t.Fatalf("read=%#v/%v", file, err)
	}
	for _, name := range []string{".git/config", "nested/.GIT/config", "linked-dir/ignored.txt", "link", "pipe", "../files/ignored.txt"} {
		if _, err := reader.TrackingSource(context.Background(), summary, name); err == nil {
			t.Fatalf("unsafe file admitted: %s", name)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if files, err := reader.TrackingFiles(canceled, summary); !errors.Is(err, context.Canceled) || files != nil {
		t.Fatalf("canceled listing=%v/%v", files, err)
	}
	if _, err := reader.TrackingSource(canceled, summary, "ignored.txt"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	foreign := *summary
	foreign.Workspace = t.TempDir()
	if _, err := reader.TrackingFiles(context.Background(), &foreign); err == nil {
		t.Fatal("foreign workspace accepted")
	}
}
