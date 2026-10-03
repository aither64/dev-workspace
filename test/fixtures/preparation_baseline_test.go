//go:build preparation_compatibility

package uploads

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Copied only into the exact 924c0ec baseline by the isolated fixture runner.
func TestPreparationBaselineFixture(t *testing.T) {
	root := os.Getenv("PREPARATION_FIXTURE_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("missing isolated fixture directory")
	}
	encoded, err := os.ReadFile(filepath.Join(root, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Workspace, Directory, ScopeID, FileID, RequestID, ReceiptID string }
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	store, err := New(fixture.Directory, fixture.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	store.MinFreeBytes = 0
	backend := &Backend{Store: store, ScopeID: fixture.ScopeID}
	ctx := context.Background()
	if file, err := backend.Status(ctx, fixture.FileID); err != nil || file.State != "ready" {
		t.Fatalf("baseline strict reader: %#v %v", file, err)
	}
	store.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if err := store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	content, err := backend.Open(ctx, fixture.FileID)
	if err != nil {
		t.Fatal("baseline collector removed pending pre-slug input", err)
	}
	data, err := io.ReadAll(content.File)
	content.File.Close()
	if err != nil || string(data) != "data" {
		t.Fatal("baseline selected bytes changed")
	}
	status(t, backend.Delete(ctx, fixture.FileID, true), 409)
	_, err = backend.Append(ctx, fixture.FileID, 0, hash([]byte("data")), bytes.NewReader([]byte("data")))
	status(t, err, 409) // ready selected bytes remain immutable in the baseline
	if _, err := backend.Complete(ctx, fixture.FileID); err != nil {
		t.Fatal(err)
	}
	// Old writers do not know the new exclusive owner: Create/Append/Complete
	// and another initial Prepare still succeed. The new recovery must refuse
	// that competing state and retain all selected files and evidence.
	create(t, backend, "old-writer.txt", []byte("old"))
	if _, err := backend.Prepare(ctx, "initial", "old-writer", "competing text", []string{fixture.FileID}); err != nil {
		t.Fatal(err)
	}
}
