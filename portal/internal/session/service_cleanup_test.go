package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func servicePreludeFixture(t *testing.T, workspace, operation string) (string, map[string]any) {
	t.Helper()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	var mode, force any
	if operation == "archive" {
		mode = "archived"
	} else {
		force = false
	}
	value := map[string]any{"schema": 1, "workspace": workspace, "slug": "example", "registry_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "retained_thread_id": "root", "tracking_identity": map[string]any{"dev": 1, "ino": 2}, "operation": operation, "operation_id": strings.Repeat("a", 64), "mode": mode, "force": force, "automatic": false, "resource_ids": []string{"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}, "phase": "services_pending", "created_at": "2026-11-02T12:00:00Z", "updated_at": "2026-11-02T12:00:00Z"}
	path := filepath.Join(root, "example.services-cleanup.json")
	writeServicePrelude(t, path, value)
	return path, value
}
func writeServicePrelude(t *testing.T, path string, value map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestPendingServiceCleanupIsOneDiscoverableLifecycleOperation(t *testing.T) {
	workspace := t.TempDir()
	_, _ = servicePreludeFixture(t, workspace, "archive")
	operation, err := PendingLifecycle(workspace, "example")
	if err != nil || operation != "archive" {
		t.Fatal(operation, err)
	}
	progress, err := PendingLifecycleProgress(workspace, "example")
	if err != nil || !progress.ServicePrelude || progress.Phase != "services_pending" || progress.Evidence == "" {
		t.Fatal(progress, err)
	}
	pending, err := PendingLifecycles(workspace)
	if err != nil || len(pending) != 1 || pending["example"].JournalID != progress.JournalID {
		t.Fatal(pending, err)
	}
}
func TestServicePreludeEvidenceExcludesProgressAndAuthorizedDeleteForceUpgrade(t *testing.T) {
	workspace := t.TempDir()
	path, value := servicePreludeFixture(t, workspace, "delete")
	before, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	value["phase"] = "services_stopped"
	value["updated_at"] = "2026-11-02T12:01:00Z"
	value["force"] = true
	writeServicePrelude(t, path, value)
	after, err := PendingLifecycleProgress(workspace, "example")
	if err != nil || !after.Force || before.Evidence != after.Evidence {
		t.Fatal(after, err)
	}
	value["registry_id"] = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	writeServicePrelude(t, path, value)
	changed, err := PendingLifecycleProgress(workspace, "example")
	if err != nil || changed.Evidence == before.Evidence {
		t.Fatal(changed, err)
	}
}
func TestOrdinaryJournalTakesPrecedenceAndMustMatchServicePrelude(t *testing.T) {
	for _, operation := range []string{"archive", "delete"} {
		t.Run(operation, func(t *testing.T) {
			workspace := t.TempDir()
			_, prelude := servicePreludeFixture(t, workspace, operation)
			journal := map[string]any{"schema": 2, "workspace": workspace, "slug": "example", "operation_id": prelude["operation_id"], "phase": "prepared", "retained_thread_id": "root"}
			suffix := "archive"
			if operation == "archive" {
				journal["mode"] = "archived"
			} else {
				suffix = "removal"
				journal["force"] = true
			}
			path := filepath.Join(workspace, "worktrees", ".locks", "example."+suffix+".json")
			writeServicePrelude(t, path, journal)
			progress, err := PendingLifecycleProgress(workspace, "example")
			if err != nil || progress.ServicePrelude || progress.Phase != "prepared" || progress.ServicePreludeEvidence == "" {
				t.Fatal(progress, err)
			}
			journal["operation_id"] = strings.Repeat("b", 64)
			writeServicePrelude(t, path, journal)
			if _, err := PendingLifecycleProgress(workspace, "example"); err == nil {
				t.Fatal("accepted changed operation")
			}
			if _, err := PendingLifecycle(workspace, "example"); err == nil {
				t.Fatal("accepted conflicting admission")
			}
		})
	}
}
func TestServicePreludeRejectsMalformedOwnershipAndUnsafeFiles(t *testing.T) {
	for _, mutate := range []func(map[string]any){func(v map[string]any) { v["workspace"] = "/other" }, func(v map[string]any) { v["phase"] = "prepared" }, func(v map[string]any) { v["resource_ids"] = nil }, func(v map[string]any) { delete(v, "automatic") }, func(v map[string]any) { v["schema"] = 2 }, func(v map[string]any) { v["mode"] = "invalid" }} {
		workspace := t.TempDir()
		path, value := servicePreludeFixture(t, workspace, "archive")
		mutate(value)
		writeServicePrelude(t, path, value)
		if _, err := PendingLifecycleProgress(workspace, "example"); err == nil {
			t.Fatal("accepted invalid prelude")
		}
	}
}
