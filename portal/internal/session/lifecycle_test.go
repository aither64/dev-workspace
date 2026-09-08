package session

import (
	"os"
	"path/filepath"
	"testing"
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
