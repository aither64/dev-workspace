package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLifecycleOperationStoreIsPrivateVersionedAndSurvivesRestart(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(t.TempDir(), "operations")
	store, err := newLifecycleOperationStore(workspace, directory)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(store.directory)) != directory {
		t.Fatalf("operation state directory = %s, want it below %s", store.directory, directory)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := lifecycleOperation{
		Slug: "example", Kind: "archive", State: "running", Phase: "starting",
		StartedAt: now, UpdatedAt: now, Redirect: "/",
		Options: lifecycleOperationOptions{Mode: "complete"},
	}
	if err := store.save(map[string]lifecycleOperation{"example": operation}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("operation state mode = %s", info.Mode())
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var payload lifecycleOperationStorePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Schema != lifecycleOperationStoreSchema || payload.Workspace != workspace {
		t.Fatalf("operation state identity = %#v", payload)
	}

	restarted, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if restarted["example"].State != "paused" {
		t.Fatalf("restarted operation = %#v", restarted["example"])
	}
	if matches, err := filepath.Glob(filepath.Join(store.directory, ".lifecycle-operations-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary operation states = %v, %v", matches, err)
	}
}

func TestLifecycleOperationStoreRejectsUnsafeOrForeignState(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(t.TempDir(), "operations")
	store, err := newLifecycleOperationStore(workspace, directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, []byte(`{"schema":1,"workspace":"foreign","operations":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("unsafe operation state error = %v", err)
	}
	if err := os.Chmod(store.path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil || !strings.Contains(err.Error(), "wrong workspace") {
		t.Fatalf("foreign operation state error = %v", err)
	}
}

func TestLifecycleOperationStoreReservesSpaceForRunningFailures(t *testing.T) {
	store, err := newLifecycleOperationStore(t.TempDir(), filepath.Join(t.TempDir(), "operations"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operations := make(map[string]lifecycleOperation)
	for index := 0; ; index++ {
		slug := fmt.Sprintf("history-%03d", index)
		operations[slug] = lifecycleOperation{
			Slug: slug, Kind: "archive", State: "failed", Phase: "starting",
			StartedAt: now, UpdatedAt: now,
			Error: strings.Repeat("x", maxLifecycleOperationErrorBytes), Redirect: "/",
			Options: lifecycleOperationOptions{Mode: "complete"},
		}
		started := lifecycleOperation{
			Slug: "new-operation", Kind: "delete", State: "running", Phase: "starting",
			StartedAt: now, UpdatedAt: now, Redirect: "/",
		}
		if _, encodeErr := store.encode(mapWithOperation(operations, started)); encodeErr != nil {
			delete(operations, slug)
			if reserveErr := store.canPersistTerminalOutcomes(
				operations, started.Slug, started,
			); reserveErr == nil {
				t.Fatal("terminal outcome was not reserved at the store-size boundary")
			}
			return
		}
		if index >= maxLifecycleOperationRecords {
			t.Fatal("unable to construct a terminal-capacity boundary")
		}
	}
}

func mapWithOperation(
	operations map[string]lifecycleOperation, operation lifecycleOperation,
) map[string]lifecycleOperation {
	result := make(map[string]lifecycleOperation, len(operations)+1)
	for slug, existing := range operations {
		result[slug] = existing
	}
	result[operation.Slug] = operation
	return result
}

func TestIndexLifecycleOperationsAreIndependentOfSessionCards(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	server.operationMu.Lock()
	err := server.replaceLifecycleOperationLocked("missing-session", lifecycleOperation{
		Slug: "missing-session", Kind: "archive", State: "failed", Phase: "prepared",
		StartedAt: now, UpdatedAt: now, Error: "merge proof failed", Redirect: "/",
		Options: lifecycleOperationOptions{Mode: "complete"},
	})
	server.operationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status", nil))
	var payload struct {
		Sessions   []indexSessionStatus `json:"sessions"`
		Operations []lifecycleOperation `json:"operations"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(payload.Sessions) != 0 || len(payload.Operations) != 1 {
		t.Fatalf("index status = %d %#v", response.Code, payload)
	}
	if payload.Operations[0].Slug != "missing-session" || payload.Operations[0].State != "failed" {
		t.Fatalf("index operation = %#v", payload.Operations[0])
	}
}

func TestLifecycleOperationReconcilesExternalSuccess(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		kind      string
		tracking  string
		lifecycle string
		manifest  string
	}{
		{
			name: "archive", kind: "archive", tracking: "archive", lifecycle: "complete",
			manifest: "schema: 1\nslug: example\nfinalized_at: \"2026-09-09T10:06:13Z\"\n",
		},
		{
			name: "revive", kind: "revive", tracking: "work", lifecycle: "active",
			manifest: "schema: 1\nslug: example\n",
		},
		{name: "delete", kind: "delete"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServer(t)
			if testCase.tracking != "" {
				directory := filepath.Join(server.config.Workspace, testCase.tracking, "example")
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(testCase.manifest), 0o644); err != nil {
					t.Fatal(err)
				}
				writeWebTrackingFiles(t, directory, testCase.lifecycle)
			}
			now := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
			operationID := strings.Repeat("a", 64)
			options := lifecycleOperationOptions{JournalID: operationID}
			if testCase.kind == "archive" {
				options.Mode = "complete"
			}
			if testCase.tracking != "" {
				// The receipt must bind the exact directory/root whose terminal
				// state is being reconciled, not just the requested mode.
				options.TargetID = deletionTargetForTest(t, server, "example")
				options.TargetLocation = testCase.tracking
			}
			version := 0
			if testCase.kind != "delete" {
				version = 2
			}
			server.operationMu.Lock()
			err := server.replaceLifecycleOperationLocked("example", lifecycleOperation{
				Slug: "example", Kind: testCase.kind, State: "failed", Phase: "prepared",
				StartedAt: now, UpdatedAt: now, Error: "old failure",
				Redirect: operationRedirect("example", testCase.kind), Options: options, TargetIdentityVersion: version,
			})
			server.operationMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if testCase.kind == "delete" {
				writeCompletedRemovalMarker(t, server, "example", operationID, time.Now().UTC())
			}
			stored := server.operations["example"]
			succeeded, successErr := server.lifecycleOperationSucceeded("example", stored)
			if successErr != nil || !succeeded {
				t.Fatalf("authoritative success = %t, %v", succeeded, successErr)
			}
			// Wait for the bounded background owner pass before reading its cache.
			refreshTestIndexStatus(t, server, httptest.NewRecorder())
			response := httptest.NewRecorder()
			server.lifecycleStatus(response, "example")
			var operation lifecycleOperation
			if err := json.Unmarshal(response.Body.Bytes(), &operation); err != nil {
				t.Fatal(err)
			}
			if operation.State != "complete" || operation.Phase != "complete" || operation.Error != "" {
				t.Fatalf("reconciled operation = %#v", operation)
			}
		})
	}
}

func TestNewDeleteJournalNeverInheritsBrowserReceiptIdentity(t *testing.T) {
	for _, trackingPresent := range []bool{true, false} {
		name := "tracking-moved"
		if trackingPresent {
			name = "tracking-present"
		}
		t.Run(name, func(t *testing.T) {
			server := newTestServer(t)
			tracking := filepath.Join(server.config.Workspace, "work", "example")
			if err := os.MkdirAll(tracking, 0o755); err != nil {
				t.Fatal(err)
			}
			writeWebTrackingFiles(t, tracking, "active")
			if err := os.WriteFile(
				filepath.Join(tracking, "portal.yml"),
				[]byte("schema: 1\nslug: example\ncodex:\n  thread_id: current-thread\nrepositories: []\n"),
				0o644,
			); err != nil {
				t.Fatal(err)
			}
			if !trackingPresent {
				if err := os.RemoveAll(tracking); err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			journal := fmt.Sprintf(
				`{"schema":1,"slug":"example","workspace":%q,"phase":"validated","force":false,"operation_id":%q}`,
				server.config.Workspace, strings.Repeat("b", 64),
			)
			if err := os.WriteFile(
				filepath.Join(root, "example.removal.json"), []byte(journal), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			server.operationMu.Lock()
			if err := server.replaceLifecycleOperationLocked("example", lifecycleOperation{
				Slug: "example", Kind: "delete", State: "complete", Phase: "complete",
				StartedAt: now, UpdatedAt: now, Redirect: "/",
				Options: lifecycleOperationOptions{
					TargetID: strings.Repeat("a", 64), DeletedThreadID: "old-thread",
				},
			}); err != nil {
				server.operationMu.Unlock()
				t.Fatal(err)
			}
			server.operationMu.Unlock()

			operation, exists, err := server.lifecycleOperationForSlug("example")
			if err != nil || !exists || operation.State != "paused" {
				t.Fatalf("reconciled delete journal = %#v, %t, %v", operation, exists, err)
			}
			if operation.Options.TargetID != "" ||
				operation.Options.DeletedThreadID != "" {
				t.Fatalf("external deletion inherited browser identity = %#v", operation.Options)
			}
			if operation.Options.JournalID != strings.Repeat("b", 64) ||
				!operation.Options.JournalExpected {
				t.Fatalf("external deletion journal identity = %#v", operation.Options)
			}
		})
	}
}

func TestMatchingDeleteJournalPreservesTheAcceptedBrowserIdentity(t *testing.T) {
	server := newTestServer(t)
	tracking := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(tracking, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, tracking, "active")
	if err := os.WriteFile(
		filepath.Join(tracking, "portal.yml"),
		[]byte("schema: 1\nslug: example\ncodex:\n  thread_id: deleted-thread\nrepositories: []\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	targetID := deletionTargetForTest(t, server, "example")
	journalID := strings.Repeat("a", 64)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	server.operationMu.Lock()
	if err := server.replaceLifecycleOperationLocked("example", lifecycleOperation{
		Slug: "example", Kind: "delete", State: "running", Phase: "starting",
		StartedAt: now, UpdatedAt: now, Redirect: "/",
		Options: lifecycleOperationOptions{
			TargetID: targetID, DeletedThreadID: "deleted-thread",
			JournalID: journalID,
		},
	}); err != nil {
		server.operationMu.Unlock()
		t.Fatal(err)
	}
	server.operationMu.Unlock()

	// Rewriting the manifest changes the tracking directory ctime. The accepted
	// page identity is a stale-request guard, not the durable operation identity.
	if err := os.WriteFile(filepath.Join(tracking, "portal.yml.tmp"), []byte("updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := fmt.Sprintf(
		`{"schema":1,"slug":"example","workspace":%q,"phase":"validated","force":false,"operation_id":%q}`,
		server.config.Workspace, journalID,
	)
	if err := os.WriteFile(filepath.Join(root, "example.removal.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}

	operation, exists, err := server.lifecycleOperationForSlug("example")
	if err != nil || !exists {
		t.Fatalf("matching journal reconciliation = %#v, %t, %v", operation, exists, err)
	}
	if operation.Options.TargetID != targetID ||
		operation.Options.DeletedThreadID != "deleted-thread" ||
		operation.Options.JournalID != journalID ||
		!operation.Options.JournalExpected {
		t.Fatalf("accepted deletion receipt was not preserved = %#v", operation.Options)
	}
}

func TestLifecycleOperationDoesNotInferDeletionFromAMissingManifest(t *testing.T) {
	server := newTestServer(t)
	tracking := filepath.Join(server.config.Workspace, "work", "example")
	if err := os.MkdirAll(tracking, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	operation := lifecycleOperation{
		Slug: "example", Kind: "delete", State: "failed", Phase: "starting",
		StartedAt: now, UpdatedAt: now, Redirect: "/",
		Options: lifecycleOperationOptions{JournalID: strings.Repeat("a", 64)},
	}
	succeeded, err := server.lifecycleOperationSucceeded("example", operation)
	if err != nil || succeeded {
		t.Fatalf("missing manifest deletion proof = %t, %v", succeeded, err)
	}
}

func TestLifecycleOperationArchiveSuccessMatchesRequestedMode(t *testing.T) {
	server := newTestServer(t)
	directory := filepath.Join(server.config.Workspace, "archive", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "portal.yml"),
		[]byte("schema: 1\nslug: example\nfinalized_at: \"2026-09-09T10:06:13Z\"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "abandoned")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	succeeded, err := server.lifecycleOperationSucceeded("example", lifecycleOperation{
		Slug: "example", Kind: "archive", State: "failed", Phase: "starting",
		StartedAt: now, UpdatedAt: now, Redirect: "/",
		Options: lifecycleOperationOptions{Mode: "complete"},
	})
	if err != nil || succeeded {
		t.Fatalf("mismatched archive mode proof = %t, %v", succeeded, err)
	}
}

func writeCompletedRemovalMarker(
	t *testing.T, server *Server, slug, operationID string, removedAt time.Time,
) {
	t.Helper()
	digest := sha256.Sum256([]byte(server.config.Workspace))
	workspaceID := filepath.Base(server.config.Workspace) + "-" + hex.EncodeToString(digest[:8])
	root := filepath.Join(server.config.UserStateRoot, "removed", workspaceID)
	directory := filepath.Join(root, "20260909T100613.000000Z-"+slug+"-123")
	if err := os.MkdirAll(filepath.Join(directory, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"schema": 1, "slug": slug, "workspace": server.config.Workspace,
		"state": "removed", "phase": "removed", "tracking": "work",
		"recovery": directory, "operation_id": operationID,
		"removed_at": removedAt.Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "recovery.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedLifecycleOperationExpiresButFailuresPersist(t *testing.T) {
	server := newTestServer(t)
	old := time.Now().UTC().Add(-lifecycleOperationSuccessRetention - time.Minute).Format(time.RFC3339Nano)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	server.operationMu.Lock()
	for slug, operation := range map[string]lifecycleOperation{
		"old-success": {
			Slug: "old-success", Kind: "delete", State: "complete", Phase: "complete",
			StartedAt: old, UpdatedAt: old, Redirect: "/",
		},
		"old-failure": {
			Slug: "old-failure", Kind: "archive", State: "failed", Phase: "prepared",
			StartedAt: old, UpdatedAt: old, Error: "still useful", Redirect: "/",
			Options: lifecycleOperationOptions{Mode: "complete"},
		},
		"new-success": {
			Slug: "new-success", Kind: "delete", State: "complete", Phase: "complete",
			StartedAt: now, UpdatedAt: now, Redirect: "/",
		},
	} {
		if err := server.replaceLifecycleOperationLocked(slug, operation); err != nil {
			server.operationMu.Unlock()
			t.Fatal(err)
		}
	}
	server.operationMu.Unlock()
	if _, _, err := server.lifecycleOperationForSlug("old-success"); err != nil {
		t.Fatal(err)
	}
	operations, err := server.lifecycleOperations()
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]string)
	for _, operation := range operations {
		states[operation.Slug] = operation.State
	}
	if _, exists := states["old-success"]; exists || states["old-failure"] != "failed" || states["new-success"] != "complete" {
		t.Fatalf("retained lifecycle operations = %#v", states)
	}
}

func TestLifecycleOperationDismissalRejectsRunningAndRemovesTerminalState(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := lifecycleOperation{
		Slug: "example", Kind: "archive", State: "running", Phase: "starting",
		StartedAt: now, UpdatedAt: now, Redirect: "/", ReceiptID: strings.Repeat("a", 64),
		Options: lifecycleOperationOptions{Mode: "complete", JournalID: strings.Repeat("b", 64)},
	}
	server.operationMu.Lock()
	if err := server.replaceLifecycleOperationLocked("example", operation); err != nil {
		server.operationMu.Unlock()
		t.Fatal(err)
	}
	server.operationMu.Unlock()
	response := httptest.NewRecorder()
	server.dismissLifecycleOperation(response, httptest.NewRequest(
		http.MethodDelete, "/", strings.NewReader(fmt.Sprintf(
			`{"receiptId":%q}`, operation.ReceiptID,
		)),
	), "example")
	if response.Code != http.StatusConflict {
		t.Fatalf("running dismissal = %d %q", response.Code, response.Body.String())
	}
	server.operationMu.Lock()
	operation.State = "failed"
	operation.Error = "failed"
	if err := server.replaceLifecycleOperationLocked("example", operation); err != nil {
		server.operationMu.Unlock()
		t.Fatal(err)
	}
	server.operationMu.Unlock()
	response = httptest.NewRecorder()
	server.dismissLifecycleOperation(response, httptest.NewRequest(
		http.MethodDelete, "/", strings.NewReader(fmt.Sprintf(
			`{"receiptId":%q}`, operation.ReceiptID,
		)),
	), "example")
	if response.Code != http.StatusNoContent {
		t.Fatalf("failed dismissal = %d %q", response.Code, response.Body.String())
	}
	server.operationMu.Lock()
	_, exists := server.operations["example"]
	server.operationMu.Unlock()
	if exists {
		t.Fatal("dismissed operation remains in memory")
	}
	loaded, err := server.operationStore.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("dismissed operation remains persisted: %#v", loaded)
	}
}

func TestLifecycleOperationDismissalCannotRemoveAReplacementReceipt(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := func(receipt string) lifecycleOperation {
		return lifecycleOperation{
			Slug: "example", Kind: "archive", State: "failed", Phase: "prepared",
			StartedAt: now, UpdatedAt: now, Redirect: "/", ReceiptID: receipt,
			Error: "failed",
			Options: lifecycleOperationOptions{
				Mode: "complete", JournalID: strings.Repeat("c", 64),
			},
		}
	}
	server.operationMu.Lock()
	if err := server.replaceLifecycleOperationLocked(
		"example", operation(strings.Repeat("b", 64)),
	); err != nil {
		server.operationMu.Unlock()
		t.Fatal(err)
	}
	server.operationMu.Unlock()

	response := httptest.NewRecorder()
	server.dismissLifecycleOperation(response, httptest.NewRequest(
		http.MethodDelete, "/", strings.NewReader(fmt.Sprintf(
			`{"receiptId":%q}`, strings.Repeat("a", 64),
		)),
	), "example")
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "operation changed") {
		t.Fatalf("stale dismissal = %d %q", response.Code, response.Body.String())
	}
	server.operationMu.Lock()
	current, exists := server.operations["example"]
	server.operationMu.Unlock()
	if !exists || current.ReceiptID != strings.Repeat("b", 64) {
		t.Fatalf("replacement operation was dismissed: %#v", current)
	}
}

func TestLifecycleOperationRetryUsesPersistedOptions(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		kind      string
		tracking  string
		lifecycle string
		manifest  string
		options   lifecycleOperationOptions
		expected  string
	}{
		{
			name: "abandoned archive", kind: "archive", tracking: "work", lifecycle: "active",
			manifest: "schema: 1\nslug: example\n",
			options: lifecycleOperationOptions{
				Mode: "abandoned", JournalID: strings.Repeat("a", 64),
			},
			expected: "archive\nexample\n--as-is\n--portal-authorized\n--abandoned\n" +
				"--portal-operation-id\n" + strings.Repeat("a", 64) + "\n",
		},
		{
			name: "forced delete", kind: "delete", tracking: "work", lifecycle: "active",
			manifest: "schema: 1\nslug: example\ncodex:\n  thread_id: deleted-thread\n",
			options: lifecycleOperationOptions{
				Force: true, DeletedThreadID: "deleted-thread",
				JournalID: strings.Repeat("a", 64),
			},
			expected: "delete\nexample\n--as-is\n--portal-authorized\n--force\n" +
				"--portal-operation-id\n" + strings.Repeat("a", 64) + "\n",
		},
		{
			name: "abandoned revive", kind: "revive", tracking: "archive", lifecycle: "abandoned",
			manifest: "schema: 1\nslug: example\nfinalized_at: \"2026-09-09T10:06:13Z\"\n",
			options: lifecycleOperationOptions{
				AllowAbandoned: true, JournalID: strings.Repeat("a", 64),
			},
			expected: "revive\nexample\n--as-is\n--portal-authorized\n--allow-abandoned\n" +
				"--portal-operation-id\n" + strings.Repeat("a", 64) + "\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServer(t)
			directory := filepath.Join(server.config.Workspace, testCase.tracking, "example")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(testCase.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			writeWebTrackingFiles(t, directory, testCase.lifecycle)

			arguments := filepath.Join(t.TempDir(), "arguments")
			helper := filepath.Join(t.TempDir(), "dev-session")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGUMENTS\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ARGUMENTS", arguments)
			server.config.DevSession = fixtureDevSessionCommand(t, server.config.Workspace, helper)
			now := time.Now().UTC().Format(time.RFC3339Nano)
			options := testCase.options
			options.TargetID = deletionTargetForTest(t, server, "example")
			options.TargetLocation = testCase.tracking
			operation := lifecycleOperation{
				Slug: "example", Kind: testCase.kind, State: "failed", Phase: "prepared",
				StartedAt: now, UpdatedAt: now, Error: "retry me",
				Redirect:  operationRedirect("example", testCase.kind),
				ReceiptID: strings.Repeat("d", 64), Options: options, TargetIdentityVersion: 2,
			}
			server.operationMu.Lock()
			if err := server.replaceLifecycleOperationLocked("example", operation); err != nil {
				server.operationMu.Unlock()
				t.Fatal(err)
			}
			loaded, err := server.operationStore.load()
			if err == nil {
				server.operations = loaded
			}
			server.operationMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}

			response := httptest.NewRecorder()
			requestBody := fmt.Sprintf(
				`{"receiptId":%q,"journalId":%q}`,
				operation.ReceiptID, options.JournalID,
			)
			server.retryLifecycleOperation(response, httptest.NewRequest(
				http.MethodPost, "/", strings.NewReader(requestBody),
			), "example")
			if response.Code != http.StatusAccepted {
				t.Fatalf("retry response = %d %q", response.Code, response.Body.String())
			}
			if operation := waitLifecycleOperation(t, server, "example"); operation.State != "complete" {
				t.Fatalf("retry operation = %#v", operation)
			}
			data, err := os.ReadFile(arguments)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != testCase.expected {
				t.Fatalf("retry arguments = %q", data)
			}
		})
	}
}

func TestLifecycleOperationErrorsAreBoundedAndHideCommandInvocation(t *testing.T) {
	err := commandFailure(
		"archive session", "", "useful explanation\nerror: command failed: /nix/store/secret/bin/dev-session "+strings.Repeat("x", 8192),
		errors.New("exit status 1"),
	)
	if len(err.Error()) > maxLifecycleOperationErrorBytes || !strings.Contains(err.Error(), "useful explanation") ||
		strings.Contains(err.Error(), "/nix/store/secret") {
		t.Fatalf("lifecycle error = %q", err)
	}
}

func TestLifecycleOperationStoreMissingFileIsEmpty(t *testing.T) {
	store, err := newLifecycleOperationStore(t.TempDir(), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	operations, err := store.load()
	if err != nil || len(operations) != 0 {
		t.Fatalf("missing operation state = %#v, %v", operations, err)
	}
	if _, err := os.Stat(store.path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("operation store was created by a read: %v", err)
	}
}

func TestLifecycleSnapshotGETDoesNotWaitForGenerationOrReceiptMutex(t *testing.T) {
	server := newTestServer(t)
	path := filepath.Join(t.TempDir(), "transition.lock")
	owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	server.config.TransitionLock = path
	now := time.Now().UTC()
	operation := lifecycleOperation{Slug: "example", Kind: "archive", State: "running", Phase: "tracking_committed", ReceiptID: strings.Repeat("a", 64)}
	server.publishLifecycleSnapshot("example", operation, now)
	server.operationMu.Lock()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/example/operation", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		server.operationMu.Unlock()
		t.Fatal("snapshot GET waited for mutation or generation proof")
	}
	server.operationMu.Unlock()
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "tracking_committed") {
		t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
	}
}

func TestIndexProgressOnlyReadsPublishedSnapshots(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	server.indexStatusCache = cachedIndexStatus{created: now, authoritative: true, statuses: []indexSessionStatus{{Slug: "example"}}}
	server.publishLifecycleSnapshot("example", lifecycleOperation{Slug: "example", Kind: "archive", State: "running"}, now)
	// These unavailable owner paths must be irrelevant to a cache-only read.
	if err := os.RemoveAll(server.config.Workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(server.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/index-status?progress=1", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authoritative":true`) || !strings.Contains(response.Body.String(), `"state":"running"`) {
		t.Fatalf("progress = %d %s", response.Code, response.Body.String())
	}
	server.displayRefreshMu.Lock()
	pending := server.displayRefreshPending || server.displayFullPending
	server.displayRefreshMu.Unlock()
	if pending {
		t.Fatal("progress-only GET scheduled a proof pass")
	}
}

func TestDisplayGenerationContentionKeepsReceiptUnchanged(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := lifecycleOperation{Slug: "example", Kind: "archive", State: "paused", Phase: "prepared", StartedAt: now, UpdatedAt: now, Redirect: "/", Options: lifecycleOperationOptions{Mode: "complete"}}
	server.operationMu.Lock()
	err := server.replaceLifecycleOperationLocked("example", operation)
	server.operationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(server.operationStore.path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "transition.lock")
	owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	server.config.TransitionLock = path
	started := time.Now()
	err = server.refreshDisplaySnapshots(context.Background(), true)
	if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("display contention = %v", err)
	}
	after, err := os.ReadFile(server.operationStore.path)
	if err != nil {
		t.Fatal(err)
	}
	current, _, _ := server.captureLifecycleOperation("example")
	if string(before) != string(after) || current != operation {
		t.Fatal("display contention changed the durable outcome")
	}
	if time.Since(started) > time.Second || server.indexStatusRetryAt.Before(started.Add(29*time.Second)) {
		t.Fatal("display contention did not return promptly with error backoff")
	}
}

func TestLifecycleReconciliationCannotOverwriteRetryOrResurrectDismissedReceipt(t *testing.T) {
	for _, change := range []string{"retry", "dismiss", "replace-after-dismiss"} {
		t.Run(change, func(t *testing.T) {
			server := newTestServer(t)
			now := time.Now().UTC().Format(time.RFC3339Nano)
			original := lifecycleOperation{Slug: "example", Kind: "archive", State: "failed", Phase: "starting", StartedAt: now, UpdatedAt: now, Redirect: "/", ReceiptID: strings.Repeat("a", 64), Attempt: 1, Options: lifecycleOperationOptions{Mode: "complete"}}
			server.operationMu.Lock()
			err := server.replaceLifecycleOperationLocked("example", original)
			server.operationMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			captured, existed, revision := server.captureLifecycleOperation("example")
			// A slow pass has already proposed an outcome for its captured attempt.
			proposed := captured
			proposed.State = "complete"
			server.operationMu.Lock()
			switch change {
			case "retry":
				original.Attempt++
				original.State = "running"
				err = server.replaceLifecycleOperationLocked("example", original)
			default:
				err = server.removeLifecycleOperationLocked("example")
			}
			if err == nil && change == "replace-after-dismiss" {
				original.ReceiptID = strings.Repeat("b", 64)
				err = server.replaceLifecycleOperationLocked("example", original)
			}
			server.operationMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if accepted, err := server.acceptLifecycleReconciliation("example", captured, existed, revision, proposed, true, proposed); err != nil || accepted {
				t.Fatalf("stale reconciliation accepted = %t, error = %v", accepted, err)
			}
			current, exists, _ := server.captureLifecycleOperation("example")
			if change == "dismiss" {
				if exists {
					t.Fatal("dismissed receipt was resurrected")
				}
			} else if current != original {
				t.Fatalf("new receipt overwritten: %#v", current)
			}
		})
	}
}

func TestLifecycleOperationTimeoutIncludesBothLockWaits(t *testing.T) {
	for _, gate := range []string{"generation", "session"} {
		t.Run(gate, func(t *testing.T) {
			server := newTestServer(t)
			path := filepath.Join(t.TempDir(), "transition.lock")
			owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			server.config.TransitionLock = path
			if gate == "generation" {
				if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
					t.Fatal(err)
				}
			} else {
				lock := server.messageLock("example")
				if err := lock.Lock(context.Background()); err != nil {
					t.Fatal(err)
				}
				defer lock.Unlock()
			}
			started := time.Now()
			err = server.executeLifecycleOperation(context.Background(), 25*time.Millisecond, "example", "archive", nil, lifecycleOperationOptions{})
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
				t.Fatalf("%s wait = %v after %s", gate, err, time.Since(started))
			}
		})
	}
}

func TestFastLifecycleRefreshSkipsFailedAndCompletedReceiptsAndTargetReads(t *testing.T) {
	server := newTestServer(t)
	checked := time.Now().UTC().Add(-time.Hour)
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for slug, state := range map[string]string{"running": "running", "failed": "failed", "completed": "complete"} {
		operation := lifecycleOperation{Slug: slug, Kind: "archive", State: state, Phase: "starting", StartedAt: checked.Format(time.RFC3339Nano), UpdatedAt: checked.Format(time.RFC3339Nano), Redirect: "/", Options: lifecycleOperationOptions{Mode: "complete"}}
		server.operationMu.Lock()
		err := server.replaceLifecycleOperationLocked(slug, operation)
		server.operationMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		// Retain a positively sampled target. Fast progress must keep that view,
		// rather than replace it with a new filesystem-derived sample every second.
		operation.CurrentTarget = &lifecycleTargetSnapshot{Slug: slug, TargetID: strings.Repeat("a", 64), TargetIdentityVersion: 2}
		server.publishLifecycleSnapshot(slug, operation, checked)
		if state != "running" {
			if err := os.WriteFile(filepath.Join(root, slug+".archive.json"), []byte("invalid journal"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := server.refreshDisplaySnapshots(context.Background(), false); err != nil {
		t.Fatalf("fast pass inspected terminal receipt evidence: %v", err)
	}
	server.operationSnapshotMu.RLock()
	running := server.operationSnapshots["running"]
	failed, completed := server.operationSnapshots["failed"], server.operationSnapshots["completed"]
	server.operationSnapshotMu.RUnlock()
	if !running.checkedAt.After(checked) || running.operation.CurrentTarget == nil {
		t.Fatal("running progress lost its published target")
	}
	if failed.checkedAt != checked || completed.checkedAt != checked {
		t.Fatal("fast pass refreshed nonrunning rows")
	}
	if err := server.refreshDisplaySnapshots(context.Background(), true); err == nil {
		t.Fatal("ordinary full discovery ignored malformed journal evidence")
	}
}
