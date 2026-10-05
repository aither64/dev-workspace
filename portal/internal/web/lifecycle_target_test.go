package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func browserTargetFixture(t *testing.T, server *Server, root, lifecycle, thread string) *session.Summary {
	t.Helper()
	directory := filepath.Join(server.config.Workspace, root, "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, lifecycle)
	manifest := "schema: 1\nslug: example\n"
	if thread != "" {
		manifest += "codex:\n  thread_id: " + thread + "\n"
	}
	if root == "archive" {
		manifest += "finalized_at: \"2026-09-09T10:06:13Z\"\n"
	}
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := session.Find(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func failedBrowserOperation(t *testing.T, summary *session.Summary, kind string) lifecycleOperation {
	t.Helper()
	identity, err := lifecycleTargetIdentity(summary)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := lifecycleOperation{Slug: summary.Slug, Kind: kind, State: "failed", Phase: "starting",
		StartedAt: now, UpdatedAt: now, Redirect: operationRedirect(summary.Slug, kind),
		ReceiptID: strings.Repeat("b", 64), TargetIdentityVersion: 2, Attempt: 1,
		Options: lifecycleOperationOptions{TargetID: identity, TargetLocation: summary.Root, JournalID: strings.Repeat("a", 64)}}
	if kind == "archive" {
		operation.Options.Mode = "abandoned"
	}
	if kind == "delete" {
		operation.Options.DeletedThreadID = summary.Codex.ThreadID
	}
	if kind == "revive" {
		operation.Options.AllowAbandoned = true
	}
	return operation
}

func saveBrowserOperation(t *testing.T, server *Server, operation lifecycleOperation) {
	t.Helper()
	server.operationMu.Lock()
	defer server.operationMu.Unlock()
	if err := server.replaceLifecycleOperationLocked(operation.Slug, operation); err != nil {
		t.Fatal(err)
	}
}

func browserLifecycleHelper(t *testing.T, server *Server) string {
	t.Helper()
	arguments := filepath.Join(t.TempDir(), "arguments")
	helper := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$BROWSER_ARGUMENTS\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER_ARGUMENTS", arguments)
	server.config.DevSession = fixtureDevSessionCommand(t, server.config.Workspace, helper)
	return arguments
}

func retryBrowserOperation(server *Server, operation lifecycleOperation) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	server.retryLifecycleOperation(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(
		`{"receiptId":%q,"journalId":%q}`, operation.ReceiptID, operation.Options.JournalID))), operation.Slug)
	return response
}

func TestLifecycleTargetV2IgnoresAdministrativeWritesAndProvesReplacement(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "work", "active", "root-a")
	before, err := lifecycleTargetIdentity(summary)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(summary.Workspace, summary.Root, summary.Slug)
	if err := os.WriteFile(filepath.Join(directory, "review.md"), []byte("evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(directory, "portal.yml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte("artifacts:\n  - label: Review\n    path: review.md\n")...)
	if err := os.WriteFile(filepath.Join(directory, ".portal.tmp"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(directory, ".portal.tmp"), filepath.Join(directory, "portal.yml")); err != nil {
		t.Fatal(err)
	}
	summary, err = session.Find(summary.Workspace, summary.Slug)
	if err != nil {
		t.Fatal(err)
	}
	after, err := lifecycleTargetIdentity(summary)
	if err != nil || after != before {
		t.Fatalf("administrative write changed target: %s != %s (%v)", after, before, err)
	}
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(summary.Workspace, alias); err != nil {
		t.Fatal(err)
	}
	copy := *summary
	copy.Workspace = alias
	canonical, err := lifecycleTargetIdentity(&copy)
	if err != nil || canonical != before {
		t.Fatalf("canonical workspace target = %s, %v", canonical, err)
	}
	summary.Codex.ThreadID = "root-b"
	changed, err := lifecycleTargetIdentity(summary)
	if err != nil || changed == before {
		t.Fatalf("root replacement retained target: %s, %v", changed, err)
	}
	summary.Codex.ThreadID = ""
	absent, err := lifecycleTargetIdentity(summary)
	if err != nil || absent == before || absent == changed {
		t.Fatalf("root absence is not explicit: %s, %v", absent, err)
	}
	summary.Codex.ThreadID = "root-a"
	if err := os.Rename(directory, directory+".old"); err != nil {
		t.Fatal(err)
	}
	summary = browserTargetFixture(t, server, "work", "active", "root-a")
	replaced, err := lifecycleTargetIdentity(summary)
	if err != nil || replaced == before {
		t.Fatalf("directory replacement retained target: %s, %v", replaced, err)
	}
}

func TestLifecycleTargetV2RetryKeepsReceiptAcrossRestartAndStatusRefresh(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "work", "active", "root-a")
	operation := failedBrowserOperation(t, summary, "archive")
	saveBrowserOperation(t, server, operation)
	loaded, err := server.operationStore.load()
	if err != nil {
		t.Fatal(err)
	}
	server.operations = loaded
	if err := os.WriteFile(filepath.Join(summary.Workspace, "work", "example", "result.md"), []byte("new artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	status := httptest.NewRecorder()
	server.lifecycleStatus(status, "example")
	var snapshot lifecycleOperation
	if err := json.Unmarshal(status.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentTarget == nil || snapshot.CurrentTarget.TargetID != operation.Options.TargetID || snapshot.CurrentTarget.TargetIdentityVersion != 2 {
		t.Fatalf("status target = %#v", snapshot)
	}
	arguments := browserLifecycleHelper(t, server)
	response := retryBrowserOperation(server, operation)
	if response.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", response.Code, response.Body.String())
	}
	server.operationWG.Wait()
	current := server.operations["example"]
	if current.ReceiptID != operation.ReceiptID || current.Attempt != 2 || current.Options.TargetID != operation.Options.TargetID || current.TargetIdentityVersion != 2 {
		t.Fatalf("retry changed receipt identity: %#v", current)
	}
	if _, err := os.Stat(arguments); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleTargetLegacyConversionRequiresPositiveExactProof(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			server := newTestServer(t)
			summary := browserTargetFixture(t, server, "work", "active", "root-a")
			operation := failedBrowserOperation(t, summary, "archive")
			legacy, err := legacyLifecycleTargetIdentity(summary)
			if err != nil {
				t.Fatal(err)
			}
			operation.TargetIdentityVersion = 0
			operation.Options.TargetID = legacy
			operation.Options.TargetLocation = ""
			saveBrowserOperation(t, server, operation)
			arguments := browserLifecycleHelper(t, server)
			if changed {
				if err := os.WriteFile(filepath.Join(summary.Workspace, "work", "example", "result.md"), []byte("artifact"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			response := retryBrowserOperation(server, operation)
			if changed {
				var result struct {
					Code          string                   `json:"code"`
					ReceiptID     string                   `json:"receiptId"`
					CurrentTarget *lifecycleTargetSnapshot `json:"currentTarget"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusConflict || result.Code != "target_changed" || result.ReceiptID != operation.ReceiptID || result.CurrentTarget == nil {
					t.Fatalf("unverifiable old receipt = %d %s", response.Code, response.Body.String())
				}
				if _, err := os.Stat(arguments); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unverifiable old target ran: %v", err)
				}
				if server.operations["example"].Options.TargetID != legacy {
					t.Fatal("unverifiable target was adopted")
				}
				return
			}
			if response.Code != http.StatusAccepted {
				t.Fatalf("positive old receipt = %d %s", response.Code, response.Body.String())
			}
			server.operationWG.Wait()
			current := server.operations["example"]
			if current.ReceiptID != operation.ReceiptID || current.TargetIdentityVersion != 2 || current.Options.TargetID != deletionTargetForTest(t, server, "example") {
				t.Fatalf("positive conversion = %#v", current)
			}
		})
	}
}

func TestLifecycleTargetFreshConfirmationSupersedesOnlyExactFailedPreJournalReceipt(t *testing.T) {
	for _, kind := range []string{"archive", "delete", "revive"} {
		t.Run(kind, func(t *testing.T) {
			server := newTestServer(t)
			root, lifecycle := "work", "active"
			if kind == "revive" {
				root, lifecycle = "archive", "abandoned"
			}
			summary := browserTargetFixture(t, server, root, lifecycle, "root-a")
			old := failedBrowserOperation(t, summary, kind)
			saveBrowserOperation(t, server, old)
			if err := os.Rename(filepath.Join(summary.Workspace, root, "example"), filepath.Join(t.TempDir(), "old")); err != nil {
				t.Fatal(err)
			}
			summary = browserTargetFixture(t, server, root, lifecycle, "root-b")
			current := deletionTargetForTest(t, server, "example")
			arguments := browserLifecycleHelper(t, server)
			start := func(receipt string) *httptest.ResponseRecorder {
				response := httptest.NewRecorder()
				body := fmt.Sprintf(`{"targetId":%q,"receiptId":%q,"mode":"abandoned"}`, current, receipt)
				if kind == "delete" {
					body = fmt.Sprintf(`{"targetId":%q,"receiptId":%q,"force":true}`, current, receipt)
				}
				if kind == "revive" {
					body = fmt.Sprintf(`{"targetId":%q,"receiptId":%q,"allowAbandoned":true}`, current, receipt)
				}
				request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				switch kind {
				case "archive":
					server.startArchive(response, request, summary)
				case "revive":
					server.startRevive(response, request, summary)
				case "delete":
					server.deleteSession(response, request, "example")
				}
				return response
			}
			stale := start(strings.Repeat("c", 64))
			if stale.Code != http.StatusConflict {
				t.Fatalf("stale supersession = %d %s", stale.Code, stale.Body.String())
			}
			if _, err := os.Stat(arguments); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale supersession ran: %v", err)
			}
			accepted := start(old.ReceiptID)
			if accepted.Code != http.StatusAccepted {
				t.Fatalf("fresh confirmation = %d %s", accepted.Code, accepted.Body.String())
			}
			server.operationWG.Wait()
			replacement := server.operations["example"]
			if replacement.ReceiptID == old.ReceiptID || replacement.Options.TargetID != current || replacement.TargetIdentityVersion != 2 {
				t.Fatalf("fresh receipt = %#v", replacement)
			}
			if kind == "delete" && !replacement.Options.Force {
				t.Fatal("force confirmation was lost")
			}
		})
	}
}

func TestLifecycleTargetCompletionRequiresSameDirectoryAndRootAcrossMove(t *testing.T) {
	for _, kind := range []string{"archive", "revive"} {
		t.Run(kind, func(t *testing.T) {
			server := newTestServer(t)
			origin, destination, initial, terminal := "work", "archive", "active", "abandoned"
			if kind == "revive" {
				origin, destination, initial, terminal = "archive", "work", "abandoned", "active"
			}
			summary := browserTargetFixture(t, server, origin, initial, "root-a")
			operation := failedBrowserOperation(t, summary, kind)
			if err := os.MkdirAll(filepath.Join(summary.Workspace, destination), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(summary.Workspace, destination, "example")
			if err := os.Rename(filepath.Join(summary.Workspace, origin, "example"), path); err != nil {
				t.Fatal(err)
			}
			browserTargetFixture(t, server, destination, terminal, "root-a")
			succeeded, err := server.lifecycleOperationSucceeded("example", operation)
			if err != nil || !succeeded {
				t.Fatalf("matching moved target = %t, %v", succeeded, err)
			}
			if err := os.Rename(path, filepath.Join(t.TempDir(), "old")); err != nil {
				t.Fatal(err)
			}
			browserTargetFixture(t, server, destination, terminal, "root-a")
			succeeded, err = server.lifecycleOperationSucceeded("example", operation)
			if err != nil || succeeded {
				t.Fatalf("replacement terminal completed old receipt = %t, %v", succeeded, err)
			}
		})
	}
}

func writeBrowserArchiveJournal(t *testing.T, server *Server, operation lifecycleOperation, phase, digest string) string {
	t.Helper()
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "example.archive.json")
	payload := fmt.Sprintf(`{"schema":2,"workspace":%q,"slug":"example","phase":%q,"mode":"abandoned","operation_id":%q,"retained_thread_id":"root-a","target_tracking_sha256":%q,"proven_heads":{},"finalized_at":"2026-09-09T10:06:13Z"}`,
		server.config.Workspace, phase, operation.Options.JournalID, digest)
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLifecycleTargetJournalOwnsMovedRetryAndRejectsChangedEvidence(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "work", "active", "root-a")
	operation := failedBrowserOperation(t, summary, "archive")
	saveBrowserOperation(t, server, operation)
	writeBrowserArchiveJournal(t, server, operation, "prepared", strings.Repeat("c", 64))
	accepted, ok, err := server.lifecycleOperationForSlug("example")
	if err != nil || !ok || !accepted.Options.JournalExpected || accepted.Options.JournalEvidence == "" {
		t.Fatalf("accepted journal = %#v, %v", accepted, err)
	}
	if err := os.MkdirAll(filepath.Join(summary.Workspace, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(summary.Workspace, "work", "example"), filepath.Join(summary.Workspace, "archive", "example")); err != nil {
		t.Fatal(err)
	}
	browserTargetFixture(t, server, "archive", "abandoned", "root-a")
	writeBrowserArchiveJournal(t, server, operation, "tracking_committed", strings.Repeat("c", 64))
	status := httptest.NewRecorder()
	server.lifecycleStatus(status, "example")
	var snapshot lifecycleOperation
	if err := json.Unmarshal(status.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Options.TargetID != operation.Options.TargetID || snapshot.CurrentTarget == nil || snapshot.CurrentTarget.TargetID == operation.Options.TargetID {
		t.Fatalf("moved operation was retargeted: %#v", snapshot)
	}
	arguments := browserLifecycleHelper(t, server)
	response := retryBrowserOperation(server, snapshot)
	if response.Code != http.StatusAccepted {
		t.Fatalf("journal retry = %d %s", response.Code, response.Body.String())
	}
	server.operationWG.Wait()
	if server.operations["example"].ReceiptID != operation.ReceiptID || server.operations["example"].Options.TargetID != operation.Options.TargetID {
		t.Fatal("journal retry replaced original identity")
	}
	if err := os.Remove(arguments); err != nil {
		t.Fatal(err)
	}
	writeBrowserArchiveJournal(t, server, operation, "tracking_committed", strings.Repeat("d", 64))
	refused := retryBrowserOperation(server, snapshot)
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "immutable evidence changed") {
		t.Fatalf("changed journal evidence = %d %s", refused.Code, refused.Body.String())
	}
	if _, err := os.Stat(arguments); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed journal ran: %v", err)
	}
}

func TestLifecycleTargetFreshConfirmationCannotReplaceAnAcceptedJournal(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "work", "active", "root-a")
	operation := failedBrowserOperation(t, summary, "archive")
	saveBrowserOperation(t, server, operation)
	writeBrowserArchiveJournal(t, server, operation, "prepared", strings.Repeat("c", 64))
	arguments := browserLifecycleHelper(t, server)
	response := httptest.NewRecorder()
	server.startArchive(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(
		`{"mode":"abandoned","targetId":%q,"receiptId":%q}`, operation.Options.TargetID, operation.ReceiptID))), summary)
	if response.Code != http.StatusConflict {
		t.Fatalf("journal supersession = %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(arguments); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal supersession ran: %v", err)
	}
}

func TestLifecycleTargetLegacyReviveMissingJournalNeverAdoptsNewRoot(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "archive", "abandoned", "")
	operation := failedBrowserOperation(t, summary, "revive")
	operation.State = "running"
	operation.Options.JournalExpected = true
	operation.Options.JournalEvidence = strings.Repeat("c", 64)
	saveBrowserOperation(t, server, operation)
	if err := os.MkdirAll(filepath.Join(summary.Workspace, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(summary.Workspace, "archive", "example"), filepath.Join(summary.Workspace, "work", "example")); err != nil {
		t.Fatal(err)
	}
	// The owning command created a root and removed its journal, but its result
	// was lost before receipt persistence. Terminal lifecycle is insufficient.
	browserTargetFixture(t, server, "work", "active", "new-root")
	loaded, err := server.operationStore.load()
	if err != nil {
		t.Fatal(err)
	}
	server.operations = loaded
	current, ok, err := server.lifecycleOperationForSlug("example")
	if err != nil || !ok || current.State != "failed" || current.Options.TargetID != operation.Options.TargetID {
		t.Fatalf("unproved legacy completion = %#v, %v", current, err)
	}
	arguments := browserLifecycleHelper(t, server)
	response := retryBrowserOperation(server, current)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "journal is no longer available") {
		t.Fatalf("missing journal retry = %d %s", response.Code, response.Body.String())
	}
	fresh := httptest.NewRecorder()
	server.startLifecycleOperation(fresh, "example", "revive", "/example/", []string{"revive", "example"},
		lifecycleOperationOptions{TargetID: deletionTargetForTest(t, server, "example"), JournalID: strings.Repeat("d", 64)}, "", current.ReceiptID)
	if fresh.Code != http.StatusConflict {
		t.Fatalf("missing expected journal supersession = %d %s", fresh.Code, fresh.Body.String())
	}
	if _, err := os.Stat(arguments); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing journal ran: %v", err)
	}
}

func TestLifecycleTargetLegacyReviveOwningCommandResultFinishesOriginalReceipt(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "archive", "abandoned", "")
	operation := failedBrowserOperation(t, summary, "revive")
	saveBrowserOperation(t, server, operation)
	if err := os.MkdirAll(filepath.Join(summary.Workspace, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(summary.Workspace, "archive", "example"), filepath.Join(summary.Workspace, "work", "example")); err != nil {
		t.Fatal(err)
	}
	browserTargetFixture(t, server, "work", "active", "")
	root := filepath.Join(summary.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "example.revive.json")
	journal := fmt.Sprintf(`{"schema":3,"slug":"example","workspace":%q,"phase":"runtime_starting","operation_id":%q,"retained_thread_id":null,"legacy_without_portal":true}`,
		summary.Workspace, operation.Options.JournalID)
	if err := os.WriteFile(journalPath, []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf 'schema: 1\\nslug: example\\ncodex:\\n  thread_id: new-root\\n' > \"$BROWSER_PORTAL\"\nrm \"$BROWSER_JOURNAL\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER_PORTAL", filepath.Join(summary.Workspace, "work", "example", "portal.yml"))
	t.Setenv("BROWSER_JOURNAL", journalPath)
	server.config.DevSession = fixtureDevSessionCommand(t, server.config.Workspace, helper)
	response := retryBrowserOperation(server, operation)
	if response.Code != http.StatusAccepted {
		t.Fatalf("owned legacy retry = %d %s", response.Code, response.Body.String())
	}
	server.operationWG.Wait()
	current := server.operations["example"]
	if current.State != "complete" || current.ReceiptID != operation.ReceiptID || current.Options.TargetID != operation.Options.TargetID {
		t.Fatalf("owning result failed or retargeted original receipt: %#v", current)
	}
}

func TestLifecycleTargetReceiptOptionalFieldsRefuseMalformedVersionsAndAttempts(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "work", "active", "root-a")
	operation := failedBrowserOperation(t, summary, "archive")
	saveBrowserOperation(t, server, operation)
	data, err := os.ReadFile(server.operationStore.path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	row := payload["operations"].([]any)[0].(map[string]any)
	for _, field := range []string{"targetIdentityVersion", "attempt"} {
		original := row[field]
		for _, malformed := range []any{nil, 0, -1, "2", 1.5} {
			row[field] = malformed
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(server.operationStore.path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := server.operationStore.load(); err == nil {
				t.Fatalf("accepted malformed %s: %#v", field, malformed)
			}
		}
		row[field] = original
	}
	row["targetIdentityVersion"] = 3
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.operationStore.path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.operationStore.load(); err == nil {
		t.Fatal("accepted unsupported target version")
	}
	delete(row, "targetIdentityVersion")
	delete(row, "attempt")
	encoded, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.operationStore.path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := server.operationStore.load()
	if err != nil || loaded["example"].TargetIdentityVersion != 0 || loaded["example"].Attempt != 0 {
		t.Fatalf("omitted predecessor fields = %#v, %v", loaded, err)
	}
}

func TestLifecycleTargetAttemptRejectsLateResultWithTheSameReceipt(t *testing.T) {
	server := newTestServer(t)
	summary := browserTargetFixture(t, server, "work", "active", "root-a")
	operation := failedBrowserOperation(t, summary, "archive")
	saveBrowserOperation(t, server, operation)
	directory := t.TempDir()
	started, release := filepath.Join(directory, "started"), filepath.Join(directory, "release")
	helper := filepath.Join(directory, "dev-session")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \"$BROWSER_STARTED\"\nwhile [ ! -e \"$BROWSER_RELEASE\" ]; do sleep 0.01; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER_STARTED", started)
	t.Setenv("BROWSER_RELEASE", release)
	server.config.DevSession = fixtureDevSessionCommand(t, server.config.Workspace, helper)
	defer func() { _ = os.WriteFile(release, nil, 0o600); server.operationWG.Wait() }()
	response := retryBrowserOperation(server, operation)
	if response.Code != http.StatusAccepted {
		t.Fatalf("attempt = %d %s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	server.operationMu.Lock()
	newer := server.operations["example"]
	newer.Attempt++
	newer.State = "failed"
	newer.Error = "newer attempt outcome"
	if err := server.replaceLifecycleOperationLocked("example", newer); err != nil {
		server.operationMu.Unlock()
		t.Fatal(err)
	}
	server.operationMu.Unlock()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server.operationWG.Wait()
	current := server.operations["example"]
	if current.ReceiptID != operation.ReceiptID || current.Attempt != newer.Attempt || current.State != "failed" || current.Error != newer.Error {
		t.Fatalf("older attempt overwrote the same receipt: %#v", current)
	}
}
