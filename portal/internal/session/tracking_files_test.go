package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestTrackingFilesAliasOnlyAfterLiteralMissing(t *testing.T) {
	summary := &Summary{Manifest: Manifest{Slug: "files"}, Workspace: t.TempDir(), Root: "work"}
	root := filepath.Join(summary.Workspace, "work", "files")
	write := func(name, text string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("report.txt", "short")
	write("another.txt", "second")
	write("work/files/report.txt", "literal")
	read := func(path, canonical, content string) {
		t.Helper()
		file, _, got, err := OpenTrackingFile(summary, path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil || got != canonical || string(data) != content {
			t.Fatalf("%s→%s/%q/%v", path, got, data, err)
		}
	}
	read("work/files/report.txt", "work/files/report.txt", "literal")
	read("archive/files/report.txt", "report.txt", "short")
	literal := filepath.Join(root, "work/files/report.txt")
	if err := os.Remove(literal); err != nil {
		t.Fatal(err)
	}
	read("work/files/report.txt", "report.txt", "short")
	if err := os.Symlink(filepath.Join(root, "report.txt"), literal); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := OpenTrackingFile(summary, "work/files/report.txt"); err == nil {
		t.Fatal("symlink literal fell through")
	}
	if err := os.Remove(literal); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(literal, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := OpenTrackingFile(summary, "work/files/report.txt"); err == nil {
		t.Fatal("directory literal fell through")
	}
	if files, err := ListTrackingFiles(context.Background(), summary, 1, 4096); !errors.Is(err, ErrTrackingFilesLimit) || files != nil {
		t.Fatalf("partial limit=%v/%v", files, err)
	}
	if files, err := ListTrackingFiles(context.Background(), summary, 100, 8); !errors.Is(err, ErrTrackingFilesLimit) || files != nil {
		t.Fatalf("metadata limit=%v/%v", files, err)
	}
}
