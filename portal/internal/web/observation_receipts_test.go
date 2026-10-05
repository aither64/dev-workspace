package web

import (
	"encoding/json"
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

func TestCreationlessRetainedObservationRequiresActualReceiptAbsence(t *testing.T) {
	for _, residue := range []string{"none", "completed creation", "rotated creation", "start", "fork", "browser"} {
		t.Run(residue, func(t *testing.T) {
			workspace, stateRoot := t.TempDir(), t.TempDir()
			directory := filepath.Join(workspace, "work", "example")
			if err := os.MkdirAll(directory, 0755); err != nil {
				t.Fatal(err)
			}
			writeWebTrackingFiles(t, directory, "active")
			manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root-exact\n  socket_path: /run/codex.sock\n  client_version: 0.160.0\n"
			if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0644); err != nil {
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
			path := ""
			switch residue {
			case "completed creation":
				path = filepath.Join(store.directory, "creations", "example.json")
			case "rotated creation":
				path = filepath.Join(store.directory, "creations", "example.old.json")
			case "start", "fork":
				path = filepath.Join(workspace, "worktrees", ".locks", "example."+residue+".json")
			case "browser":
				now := time.Now().UTC().Format(time.RFC3339Nano)
				target, err := lifecycleTargetIdentity(summary)
				if err != nil {
					t.Fatal(err)
				}
				operation := lifecycleOperation{Slug: "example", Kind: "archive", State: "failed", Phase: "starting", StartedAt: now, UpdatedAt: now, Redirect: "/", ReceiptID: strings.Repeat("a", 64), TargetIdentityVersion: 2, Options: lifecycleOperationOptions{JournalID: strings.Repeat("b", 64), Mode: "complete", TargetID: target, TargetLocation: "work"}}
				if err := store.save(map[string]lifecycleOperation{"example": operation}); err != nil {
					t.Fatal(err)
				}
			}
			if path != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"state":"ready"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := RequireObservationReceipts(summary, stateRoot, "", ""); (err == nil) != (residue == "none") {
				t.Fatalf("receipt absence for %s: %v", residue, err)
			}
		})
	}
}

func TestCreationlessObservationMatchesOnlyItsStartAndReviveOwners(t *testing.T) {
	for _, scenario := range []string{"start", "revive", "both", "no context", "wrong token", "missing start", "wrong phase", "wrong root", "missing revive", "browser", "foreign browser", "changed target", "creation", "fork", "cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, stateRoot := t.TempDir(), t.TempDir()
			directory := filepath.Join(workspace, "work", "example")
			locks := filepath.Join(workspace, "worktrees", ".locks")
			for _, path := range []string{directory, locks} {
				if err := os.MkdirAll(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			writeWebTrackingFiles(t, directory, "active")
			manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root-exact\n  socket_path: /run/codex.sock\n  client_version: 0.160.0\n"
			if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0644); err != nil {
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
			token, id := strings.Repeat("a", 64), strings.Repeat("b", 64)
			context := ObservationRetainedContext{StartTmuxIdentity: token, ReviveOperationID: id}
			allowed := scenario == "start" || scenario == "revive" || scenario == "both" || scenario == "browser"
			write := func(path string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "revive" && scenario != "missing start" {
				write(filepath.Join(locks, "example.start.json"), map[string]any{"schema": 1, "slug": "example", "state": "creating", "tmux_identity": token})
			}
			if scenario == "start" || scenario == "wrong token" || scenario == "missing start" {
				context.ReviveOperationID = ""
			} else if scenario != "missing revive" {
				phase, root := "runtime_starting", "root-exact"
				if scenario == "wrong phase" {
					phase = "tracking_committed"
				}
				if scenario == "wrong root" {
					root = "replacement"
				}
				// Ruby owns full schema-3 projection validation. This fixture
				// exercises only the existing read-only progress/evidence owner.
				write(filepath.Join(locks, "example.revive.json"), map[string]any{"schema": 3, "workspace": workspace, "slug": "example", "phase": phase, "operation_id": id, "retained_thread_id": root, "legacy_without_portal": false})
			}
			if scenario == "revive" {
				context.StartTmuxIdentity = ""
			}
			if scenario == "no context" {
				context = ObservationRetainedContext{}
			}
			if scenario == "wrong token" {
				context.StartTmuxIdentity = strings.Repeat("c", 64)
			}
			if scenario == "creation" {
				write(filepath.Join(store.directory, "creations", "example.old.json"), map[string]any{"state": "ready"})
			}
			if scenario == "fork" || scenario == "cleanup" {
				kind := "fork"
				if scenario == "cleanup" {
					kind = "archive-cleanup"
				}
				write(filepath.Join(locks, "example."+kind+".json"), map[string]any{"schema": 1})
			}
			if scenario == "browser" || scenario == "foreign browser" || scenario == "changed target" {
				progress, err := session.PendingLifecycleProgress(workspace, "example")
				if err != nil {
					t.Fatal(err)
				}
				target, err := lifecycleTargetIdentityAt(summary, "archive")
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "changed target" {
					target = strings.Repeat("d", 64)
				}
				now := time.Now().UTC().Format(time.RFC3339Nano)
				operation := lifecycleOperation{Slug: "example", Kind: "revive", State: "running", Phase: "runtime_starting", StartedAt: now, UpdatedAt: now, Redirect: "/example/", ReceiptID: strings.Repeat("e", 64), TargetIdentityVersion: 2, Options: lifecycleOperationOptions{JournalID: id, JournalExpected: true, JournalEvidence: progress.Evidence, TargetID: target, TargetLocation: "archive"}}
				if scenario == "foreign browser" {
					operation.Options.JournalID = strings.Repeat("f", 64)
				}
				if err := store.save(map[string]lifecycleOperation{"example": operation}); err != nil {
					t.Fatal(err)
				}
			}
			if err := RequireObservationReceipts(summary, stateRoot, "", "", context); (err == nil) != allowed {
				t.Fatalf("%s owner match: %v", scenario, err)
			}
		})
	}
	for _, context := range []ObservationRetainedContext{{StartTmuxIdentity: "bad"}, {ReviveOperationID: "bad"}, {StartTmuxIdentity: strings.Repeat("a", 64)}} {
		if err := ValidateObservationContext(strings.Repeat("b", 64), "complete", context); err == nil {
			t.Fatalf("mixed or malformed context accepted: %#v", context)
		}
	}
}
