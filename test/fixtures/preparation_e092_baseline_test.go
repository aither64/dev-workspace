//go:build preparation_compatibility

package web

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Installed only in the exact e092 archive by the existing compatibility runner.
// All decoding and identity checks are the actual predecessor implementation.
func TestPreparationE092ReaderFixture(t *testing.T) {
	root := os.Getenv("PREPARATION_FIXTURE_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("missing isolated fixture directory")
	}
	for _, name := range []string{"codex-reader.json", "pi-unfinished-reader.json", "pi-compact-reader.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var identity struct {
				Workspace string `json:"workspace"`
			}
			if err := json.Unmarshal(before, &identity); err != nil {
				t.Fatal(err)
			}
			record, err := readPreparation(path, identity.Workspace)
			if name == "codex-reader.json" {
				if err != nil {
					t.Fatal("original Codex unreadable", err)
				}
				if record.Schema != 1 || record.InputVersion != 1 || record.InputDigest != inputPreparationDigest(record.Snapshot.Input) || record.SnapshotDigest != preparationDigest(record.Snapshot) {
					t.Fatal("Codex identity changed")
				}
				encoded, err := json.Marshal(record)
				if err != nil || !bytes.Equal(before, encoded) {
					t.Fatal("Codex old/new bytes differ", err)
				}
			} else if err == nil {
				t.Fatal("e092 accepted incompatible Pi preparation")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("old reader changed original evidence", err)
			}
		})
	}
}
