package session

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCatalogAdmissionRechecksRecoveryAndSocket(t *testing.T) {
	workspace := t.TempDir()
	socket := filepath.Join(t.TempDir(), "codex.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { listener.Close() }()
	directory := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("schema: 1\nslug: example\ncodex:\n  thread_id: root\n  socket_path: %s\n  client_version: 0.160.0\ncreation:\n  state: ready\nrepositories: []\nartifacts: []\n", socket)
	if err := os.WriteFile(filepath.Join(directory, ManifestName), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	writeTrackingFiles(t, directory, "active")
	store := RecoveryStore{Root: t.TempDir(), Workspace: workspace}
	if err := store.Set(context.Background(), "example", "root", socket, true, true); err != nil {
		t.Fatal(err)
	}
	release := StartCatalog(workspace)
	defer release()
	if !store.AllowsImplicitResume(socket, "root") {
		t.Fatal("valid exact owner refused")
	}
	if err := store.Set(context.Background(), "other", "root", socket, true, true); err != nil {
		t.Fatal(err)
	}
	if store.AllowsImplicitResume(socket, "root") {
		t.Fatal("new duplicate recovery owner used cached permission")
	}
	if err := store.Retire(context.Background(), "other", "root", socket, true); err != nil {
		t.Fatal(err)
	}
	if !store.AllowsImplicitResume(socket, "root") {
		t.Fatal("retired duplicate owner still denied permission")
	}
	// A cached reverse lookup is never sufficient after execution is withdrawn.
	if err := store.Set(context.Background(), "example", "root", socket, false, false); err != nil {
		t.Fatal(err)
	}
	if store.AllowsImplicitResume(socket, "root") {
		t.Fatal("stale recovery permission allowed")
	}
	if err := store.Set(context.Background(), "example", "root", socket, true, true); err != nil {
		t.Fatal(err)
	}
	if !store.AllowsImplicitResume(socket, "root") {
		t.Fatal("restored owner refused")
	}
	listener.Close()
	listener, err = net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if store.AllowsImplicitResume(socket, "root") {
		t.Fatal("replacement socket inherited permission")
	}
}

func TestCatalogRevalidatesAndCopiesRecords(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	release := StartCatalog(workspace)
	defer release()
	original, err := Find(workspace, fixtureSlug)
	if err != nil {
		t.Fatal(err)
	}
	original.Artifacts[0].Label = "caller changed cached data"
	again, err := Find(workspace, fixtureSlug)
	if err != nil || again.Artifacts[0].Label == original.Artifacts[0].Label {
		t.Fatalf("shared mutable summary: %v", err)
	}
	directory := filepath.Join(workspace, "work", fixtureSlug)
	state := filepath.Join(directory, "state.md")
	// Atomic replacements with the same length and timestamp still invalidate.
	info, _ := os.Stat(state)
	replacement := filepath.Join(directory, "new-state")
	if err := os.WriteFile(replacement, []byte("---\nlifecycle: abandoned\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, state); err != nil {
		t.Fatal(err)
	}
	updated, err := Find(workspace, fixtureSlug)
	if err != nil || updated.Lifecycle != "abandoned" {
		t.Fatalf("stale lifecycle: %#v %v", updated, err)
	}
	if err := os.WriteFile(state, []byte("malformed"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Find(workspace, fixtureSlug); err == nil {
		t.Fatal("malformed replacement used cached permission")
	}
}

func TestCatalogConcurrentListAndDirectDeletion(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	release := StartCatalog(workspace)
	defer release()
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := List(workspace); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c := activeCatalog(workspace)
	c.mu.Lock()
	entries := len(c.files)
	c.mu.Unlock()
	if entries != 1 {
		t.Fatalf("cached files: %d", entries)
	}
	if err := os.Remove(filepath.Join(workspace, "work", fixtureSlug, ManifestName)); err != nil {
		t.Fatal(err)
	}
	// List is allowed a discovery interval. Direct reads are never TTL permission.
	if _, err := Find(workspace, fixtureSlug); !os.IsNotExist(err) {
		t.Fatalf("deleted owner accepted: %v", err)
	}
}

func TestCatalogDoesNotHideDuplicateRoot(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	release := StartCatalog(workspace)
	defer release()
	if _, err := Find(workspace, fixtureSlug); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(workspace, "archive", fixtureSlug)
	if err := os.MkdirAll(destination, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(workspace, "work", fixtureSlug)
	for _, name := range []string{ManifestName, "state.md", "plan.md"} {
		body, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == ManifestName {
			body = append(body, []byte("\nfinalized_at: 2026-10-15T20:00:00Z\n")...)
		}
		if err := os.WriteFile(filepath.Join(destination, name), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Find(workspace, fixtureSlug); err == nil {
		t.Fatal("duplicate root hidden by cache")
	}
}
