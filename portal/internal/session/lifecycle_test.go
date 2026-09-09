package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPendingLifecycleRecognizesOneOwningOperation(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJournal := func(suffix string) {
		t.Helper()
		path := filepath.Join(root, "example"+suffix)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeJournal(".revive.json")
	operation, err := PendingLifecycle(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if operation != "revive" {
		t.Fatalf("expected revive, got %q", operation)
	}
}

func TestPendingLifecycleProgressReadsTheValidatedJournalPhase(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "example.archive.json")
	payload := `{"schema":2,"slug":"example","workspace":"` + workspace + `","phase":"clusters_released","mode":"complete"}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	progress, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Operation != "archive" || progress.Phase != "clusters_released" ||
		progress.Mode != "complete" || progress.UpdatedAt.IsZero() {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestPendingLifecycleProgressToleratesAtomicJournalReplacement(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "example.archive.json")
	payload := []byte(`{"schema":2,"slug":"example","workspace":"` + workspace + `","phase":"prepared","mode":"complete"}`)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	errorsSeen := make(chan error, 4)
	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				progress, err := PendingLifecycleProgress(workspace, "example")
				if err != nil {
					errorsSeen <- err
					return
				}
				if progress != nil && progress.Operation != "archive" {
					errorsSeen <- errors.New("wrong lifecycle operation")
					return
				}
			}
		}()
	}
	for index := range 500 {
		temporary := filepath.Join(root, fmt.Sprintf("journal-%d.tmp", index))
		if err := os.WriteFile(temporary, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, path); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	readers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
}

func TestPendingLifecyclesIncludesJournalOnlyDeletion(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `{"schema":1,"slug":"example","workspace":"` + workspace + `","phase":"tracking_preserved","force":false}`
	if err := os.WriteFile(filepath.Join(root, "example.removal.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	pending, err := PendingLifecycles(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if pending["example"].Operation != "delete" || pending["example"].Phase != "tracking_preserved" {
		t.Fatalf("pending lifecycles = %#v", pending)
	}
}

func TestPendingLifecycleProgressRejectsFIFOWithoutBlocking(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "example.removal.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err := PendingLifecycleProgress(workspace, "example")
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("FIFO lifecycle journal error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO lifecycle journal blocked for %s", elapsed)
	}
}

func TestPendingLifecycleProgressRejectsAnInvalidOrForeignPhase(t *testing.T) {
	for _, payload := range []string{
		`{"slug":"other","workspace":"WORKSPACE","phase":"prepared","mode":"complete"}`,
		`{"slug":"example","workspace":"WORKSPACE","phase":"invented","mode":"complete"}`,
		`{"slug":"example","workspace":"WORKSPACE","phase":"prepared","mode":"invented"}`,
	} {
		workspace := t.TempDir()
		root := filepath.Join(workspace, "worktrees", ".locks")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		payload = strings.ReplaceAll(payload, "WORKSPACE", workspace)
		if err := os.WriteFile(filepath.Join(root, "example.archive.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := PendingLifecycleProgress(workspace, "example"); err == nil {
			t.Fatal("invalid progress was accepted")
		}
	}
}

func TestPendingLifecycleFailsClosedForConflictingOrUnsafeState(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".archive.json", ".removal.json"} {
		if err := os.WriteFile(filepath.Join(root, "example"+suffix), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PendingLifecycle(workspace, "example"); err == nil {
		t.Fatal("expected conflicting journals to fail closed")
	}

	if err := os.Remove(filepath.Join(root, "example.removal.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "example.archive.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingLifecycle(workspace, "example"); err == nil {
		t.Fatal("expected an unsafe journal to fail closed")
	}
}
