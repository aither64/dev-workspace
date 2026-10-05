package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestObservationArchiveContextRequiresPairedIDAndMode(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, test := range []struct {
		id, mode string
		valid    bool
	}{
		{"", "", true}, {id, "complete", true}, {id, "abandoned", true},
		{id, "", false}, {"", "complete", false}, {id, "other", false}, {"receipt", "complete", false},
	} {
		if err := ValidateObservationArchiveContext(test.id, test.mode); (err == nil) != test.valid {
			t.Fatalf("context %#v: %v", test, err)
		}
	}
}

func TestObservationReceiptOwnerBindsBrowserJournalPreparedAndMovedArchive(t *testing.T) {
	for _, scenario := range []string{"manual", "browser", "wrong id", "wrong mode", "wrong kind", "changed target", "missing journal", "journal", "changed evidence", "moved", "moved replaced", "prepared", "prepared missing expected", "invalid store", "creation residue"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, stateRoot := t.TempDir(), t.TempDir()
			directory := filepath.Join(workspace, "work", "example")
			if err := os.MkdirAll(directory, 0755); err != nil {
				t.Fatal(err)
			}
			writeWebTrackingFiles(t, directory, "active")
			if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: example\nrepositories: []\nartifacts: []\n"), 0644); err != nil {
				t.Fatal(err)
			}
			summary, err := session.Find(workspace, "example")
			if err != nil {
				t.Fatal(err)
			}
			store, err := newLifecycleOperationStore(workspace, stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			target, err := lifecycleTargetIdentity(summary)
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("a", 64)
			now := time.Now().UTC().Format(time.RFC3339Nano)
			operation := lifecycleOperation{Slug: "example", Kind: "archive", State: "running", Phase: "starting", StartedAt: now, UpdatedAt: now, Redirect: "/", ReceiptID: strings.Repeat("b", 64), TargetIdentityVersion: 2, Options: lifecycleOperationOptions{JournalID: id, Mode: "complete", TargetID: target, TargetLocation: "work"}}
			expected, mode, allowed := id, "complete", true
			withReceipt := scenario != "manual" && scenario != "prepared"
			switch scenario {
			case "manual":
				expected, mode = "", ""
			case "wrong id":
				expected = strings.Repeat("c", 64)
				allowed = false
			case "wrong mode":
				mode = "abandoned"
				allowed = false
			case "wrong kind":
				operation.Kind = "revive"
				operation.Options.Mode = ""
				allowed = false
			case "changed target":
				operation.Options.TargetID = strings.Repeat("c", 64)
				allowed = false
			case "missing journal", "prepared missing expected":
				operation.Options.JournalExpected = true
				allowed = false
			case "changed evidence", "moved replaced", "invalid store", "creation residue":
				allowed = false
			}
			if scenario == "prepared" || scenario == "prepared missing expected" {
				root := filepath.Join(workspace, "worktrees", ".locks")
				if err := os.MkdirAll(root, 0755); err != nil {
					t.Fatal(err)
				}
				// Ruby's cleanup tests own full prepared reconstruction; this
				// read-only owner does not parse or authorize this sidecar.
				if err := os.WriteFile(filepath.Join(root, "example.archive-cleanup.json"), []byte("Ruby-validated intent fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "journal" || scenario == "changed evidence" || strings.HasPrefix(scenario, "moved") {
				root := filepath.Join(workspace, "worktrees", ".locks")
				if err := os.MkdirAll(root, 0755); err != nil {
					t.Fatal(err)
				}
				data := `{"schema":2,"workspace":"` + workspace + `","slug":"example","operation_id":"` + id + `","mode":"complete","phase":"tracking_committed","retained_thread_id":null}`
				if err := os.WriteFile(filepath.Join(root, "example.archive.json"), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				progress, err := session.PendingLifecycleProgress(workspace, "example")
				if err != nil {
					t.Fatal(err)
				}
				operation.Options.JournalExpected = true
				operation.Options.JournalEvidence = progress.Evidence
				if scenario == "changed evidence" {
					operation.Options.JournalEvidence = strings.Repeat("f", 64)
				}
				if strings.HasPrefix(scenario, "moved") {
					archive := filepath.Join(workspace, "archive")
					if err := os.MkdirAll(archive, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(directory, filepath.Join(archive, "example")); err != nil {
						t.Fatal(err)
					}
					summary.Root = "archive"
					summary.Archived = true
					if scenario == "moved replaced" {
						operation.Options.TargetID = strings.Repeat("f", 64)
					}
				}
			}
			if withReceipt {
				if err := store.save(map[string]lifecycleOperation{"example": operation}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "invalid store" {
				if err := os.WriteFile(store.path, []byte(`{"schema":1,"workspace":"foreign","operations":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "creation residue" {
				if err := os.MkdirAll(filepath.Join(store.directory, "creations"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(store.directory, "creations", "example.json"), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(store.path)
			if err := RequireObservationReceipts(summary, stateRoot, expected, mode); (err == nil) != allowed {
				t.Fatalf("allowed=%v error=%v", allowed, err)
			}
			after, _ := os.ReadFile(store.path)
			if string(before) != string(after) {
				t.Fatal("observation rewrote receipt")
			}
			if scenario == "browser" && RequireObservationReceipts(summary, stateRoot, "", "") == nil {
				t.Fatal("ordinary observation ignored pending browser receipt")
			}
		})
	}
}
