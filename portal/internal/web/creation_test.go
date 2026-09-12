package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

type creationTestCodex struct {
	*browserContractCodex
	entered chan<- struct{}
	release <-chan struct{}
}

func (client *creationTestCodex) ListModels(ctx context.Context) ([]codex.Model, error) {
	if client.entered != nil {
		select {
		case client.entered <- struct{}{}:
		default:
		}
	}
	if client.release != nil {
		select {
		case <-client.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	models, _ := client.browserContractCodex.ListModels(ctx)
	return append(models, codex.Model{Model: workspacecodex.DefaultNewThreadModel, IsDefault: true,
		DefaultReasoningEffort:    workspacecodex.DefaultNewThreadReasoningEffort,
		SupportedReasoningEfforts: []codex.ReasoningEffortOption{{ReasoningEffort: workspacecodex.DefaultNewThreadReasoningEffort}}}), nil
}
func awaitCreation(t *testing.T, server *Server, slug string) creationReceipt {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		receipt, ok := server.currentCreation(slug)
		if ok && receipt.State != "running" {
			return receipt
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("creation worker did not finish")
	return creationReceipt{}
}
func postCreation(t *testing.T, server *Server, path, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Origin", server.config.BaseURL)
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}
func writeCreationBinding(t *testing.T, server *Server, receipt creationReceipt) {
	t.Helper()
	binding := map[string]any{"schema": 1, "workspace": server.config.Workspace, "slug": receipt.Request.Slug, "kind": receipt.Request.Kind, "receipt_id": receipt.ReceiptID, "deletion_history_sha256": receipt.DeletionHistorySHA256,
		"source_thread": receipt.Request.SourceThreadID, "source_identity": receipt.Request.SourceIdentity, "goal_sha256": nil, "model": receipt.Model, "effort": receipt.Effort}
	if receipt.Request.Kind != "fork" {
		binding["kind"] = "start"
		binding["goal_sha256"] = planDigest(receipt.Goal)
	}
	bindingData, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.creationEvidencePath(receipt)+".request", bindingData, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeCreationProof(t *testing.T, server *Server, receipt creationReceipt) {
	t.Helper()
	writeCreationBinding(t, server, receipt)
	slug := receipt.Request.Slug
	directory := filepath.Join(server.config.Workspace, "work", slug)
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: " + slug + "\ncodex:\n  thread_id: created-thread\ncreation:\n  state: ready\n  initial_goal_sent: true\n"
	if receipt.Request.Kind != "fork" {
		manifest += "  goal_sha256: " + planDigest(receipt.Goal) + "\n"
	}
	if receipt.Request.Kind == "fork" {
		manifest += "forked_from: " + receipt.Request.Source + "\n"
	}
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	var stat unix.Stat_t
	if err := unix.Lstat(directory, &stat); err != nil {
		t.Fatal(err)
	}
	proof := creationEvidence{Schema: 1, Workspace: server.config.Workspace, Slug: slug, ReceiptID: receipt.ReceiptID, ThreadID: "created-thread",
		SourceThreadID: receipt.Request.SourceThreadID, GoalSHA256: planDigest(receipt.Goal), Model: receipt.Model, Effort: receipt.Effort,
		TrackingDevice: stat.Dev, TrackingInode: stat.Ino}
	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.creationEvidencePath(receipt), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCreationNavigationPrecedesSlowValidationAndManifestDiscovery(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server.config.Codex = &creationTestCodex{browserContractCodex: &browserContractCodex{}, entered: entered, release: release}
	defer close(release)
	started := time.Now()
	response := postCreation(t, server, "/sessions", "creation_date=2026-09-12&name=latency&goal=Initial+request", "application/x-www-form-urlencoded")
	if response.Code != http.StatusSeeOther || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("slow acceptance: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker never reached slow validation")
	}
	slug := "2026-09-12-latency"
	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/"+slug+"/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-creation="`+slug+`"`) || strings.Contains(page.Body.String(), `id="message-form"`) {
		t.Fatalf("premanifest page: %d %s", page.Code, page.Body.String())
	}
	status := httptest.NewRecorder()
	server.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/sessions/"+slug+"/creation", nil))
	if status.Code != http.StatusOK || strings.Contains(status.Body.String(), "Initial request") || !strings.Contains(status.Body.String(), "startedAt") {
		t.Fatalf("private status: %d %s", status.Code, status.Body.String())
	}
	var stored creationReceipt
	if err := readCreationJSON(server.creationPath(slug), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Request.Goal != "Initial request" {
		t.Fatalf("lost captured goal: %#v", stored)
	}
	duplicate := postCreation(t, server, "/sessions", "creation_date=2026-09-12&name=latency&goal=Initial+request", "application/x-www-form-urlencoded")
	if duplicate.Code != http.StatusSeeOther {
		t.Fatal(duplicate.Body.String())
	}
	receipt, _ := server.currentCreation(slug)
	if receipt.ReceiptID != stored.ReceiptID || receipt.Attempt != 1 {
		t.Fatal("duplicate started a new receipt or attempt")
	}
	conflicting := postCreation(t, server, "/sessions", "creation_date=2026-09-12&name=latency&goal=Different+request", "application/x-www-form-urlencoded")
	if conflicting.Code != http.StatusConflict {
		t.Fatalf("conflicting duplicate = %d", conflicting.Code)
	}
}

func TestCreationCapturesDeletionHistoryUnderDestinationLock(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	release := make(chan struct{})
	defer close(release)
	server.config.Codex = &creationTestCodex{browserContractCodex: &browserContractCodex{}, release: release}
	request := creationRequest{Kind: "new", Slug: "after-clock-correction", Goal: "Fresh request"}
	operationID := strings.Repeat("a", 64)
	writeCompletedRemovalMarker(t, server, request.Slug, operationID, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	lock, err := os.OpenFile(filepath.Join(server.config.AuthorityDir, request.Slug+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := server.acceptCreation(request); err == nil {
		t.Fatal("accepted history while deletion owns the destination lock")
	}
	if _, err := os.Stat(server.creationPath(request.Slug)); !os.IsNotExist(err) {
		t.Fatal("locked acceptance persisted a receipt")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	receipt, err := server.acceptCreation(request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.DeletionHistorySHA256 != planDigest(operationID) {
		t.Fatal("future deletion timestamp blocked or changed fresh acceptance")
	}
	var persisted creationReceipt
	if err := readCreationJSON(server.creationPath(request.Slug), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.DeletionHistorySHA256 != receipt.DeletionHistorySHA256 {
		t.Fatal("acceptance did not freeze deletion history")
	}
	// A clock-only correction cannot change matching duplicate identity.
	writeCompletedRemovalMarker(t, server, request.Slug, operationID, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	duplicate, err := server.acceptCreation(request)
	if err != nil || duplicate.ReceiptID != receipt.ReceiptID || duplicate.DeletionHistorySHA256 != receipt.DeletionHistorySHA256 {
		t.Fatal("clock correction changed an accepted request")
	}
}

func TestCreationForkAcceptsBeforeSlowSourceVerification(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	prepareInteractiveConversation(t, server, "source")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	server.config.VerifyThread = func(ctx context.Context, _ string, _ string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	response := postCreation(t, server, "/api/sessions/source/fork", `{"name":"fork","creationDate":"2026-09-12"}`, "application/json")
	if response.Code != http.StatusAccepted {
		t.Fatalf("fork response: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("source verification not started")
	}
}

func TestCreationInterruptedReceiptPausesAndRequiresExactRetryAttempt(t *testing.T) {
	server := newTestServer(t)
	entered := make(chan struct{}, 1)
	never := make(chan struct{})
	server.config.Codex = &creationTestCodex{browserContractCodex: &browserContractCodex{}, entered: entered, release: never}
	postCreation(t, server, "/sessions", "creation_date=2026-09-12&name=interrupted&goal=Request", "application/x-www-form-urlencoded")
	<-entered
	server.Close()
	receipt, _ := server.currentCreation("2026-09-12-interrupted")
	// Simulate loss of the final outcome write at process termination.
	receipt.State = "running"
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, _ := restarted.currentCreation(receipt.Request.Slug)
	if restored.State != "paused" {
		t.Fatalf("restart resumed blindly: %#v", restored)
	}
	stale := postCreation(t, restarted, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":0}`, receipt.ReceiptID), "application/json")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale retry = %d", stale.Code)
	}
	valid := postCreation(t, restarted, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
	if valid.Code != http.StatusAccepted {
		t.Fatalf("valid retry = %d %s", valid.Code, valid.Body.String())
	}
	current, _ := restarted.currentCreation(receipt.Request.Slug)
	if current.Attempt != 2 {
		t.Fatalf("retry attempt = %d", current.Attempt)
	}
}

func TestCreationRecoveryRequiresReceiptBoundCompletionAndExactSession(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	request := creationRequest{Kind: "new", Slug: "2026-09-12-proof", Goal: "Exact request"}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""), Request: request, ReceiptID: strings.Repeat("a", 64), Attempt: 1,
		State: "paused", StartedAt: now, UpdatedAt: now, Validated: true, Goal: request.Goal, Model: "model-1", Effort: "high"}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	server.creations[request.Slug] = receipt
	writeCreationProof(t, server, receipt)
	wrong := receipt
	wrong.ReceiptID = strings.Repeat("b", 64)
	if err := server.proveCreation(wrong); err == nil {
		t.Fatal("accepted another receipt's completion")
	}
	if err := os.Remove(server.creationEvidencePath(receipt)); err != nil {
		t.Fatal(err)
	}
	unproven, _ := server.currentCreation(request.Slug)
	if unproven.State != "paused" {
		t.Fatal("manifest alone proved completion")
	}
	writeCreationProof(t, server, receipt)
	proven, _ := server.currentCreation(request.Slug)
	if proven.State != "ready" {
		t.Fatalf("completion proof not recovered: %#v", proven)
	}
	directory := filepath.Join(server.config.Workspace, "work", request.Slug)
	if err := os.Rename(directory, directory+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := server.proveCreation(receipt); err == nil {
		t.Fatal("replacement session accepted old completion")
	}
}

func TestCreationPlanSnapshotWaitsForMessageLockAndRejectsNewerPlan(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	prepareInteractiveConversation(t, server, "source")
	read := make(chan struct{}, 1)
	controller := &browserContractCodex{readEntered: read, transcript: codex.Transcript{ThreadID: "thread-1", Status: "idle", CollaborationMode: "plan",
		Model: "model-1", ReasoningEffort: "high", Entries: []codex.TranscriptEntry{{Kind: "plan", TurnID: "new-plan", TurnStatus: "completed", Text: "New plan"}}}}
	server.config.Codex = controller
	lock := server.messageLock("source")
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"action":"new","name":"plan","creationDate":"2026-09-12","planTurnId":"approved","planText":"Approved plan","planSha256":%q,"model":"model-1","reasoningEffort":"high"}`, planDigest("Approved plan"))
	response := postCreation(t, server, "/api/sessions/source/implement-plan", body, "application/json")
	if response.Code != http.StatusAccepted {
		lock.Unlock()
		t.Fatalf("plan acceptance: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-read:
		lock.Unlock()
		t.Fatal("snapshot read escaped message lock")
	case <-time.After(30 * time.Millisecond):
	}
	lock.Unlock()
	receipt := awaitCreation(t, server, "2026-09-12-plan")
	if receipt.Validated || !strings.Contains(receipt.Error, "stale") || receipt.Request.PlanText != "Approved plan" {
		t.Fatalf("stale plan was replaced: %#v", receipt)
	}
}

func TestCreationRechecksGenerationAfterWorkerTransitionLock(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.config.Codex = &creationTestCodex{browserContractCodex: &browserContractCodex{}}
	lockPath := filepath.Join(t.TempDir(), "transition.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	server.config.TransitionLock = lockPath
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	receipt, err := server.acceptCreation(creationRequest{Kind: "new", Slug: "2026-09-12-generation", Goal: "Request"})
	if err != nil {
		t.Fatal(err)
	}
	profile := server.config.HostProfile
	if err := os.Remove(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), profile); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	completed := awaitCreation(t, server, receipt.Request.Slug)
	if !strings.Contains(completed.Error, "superseded") || completed.Validated {
		t.Fatalf("old generation executed: %#v", completed)
	}
}

func TestCreationFastSourceLockDoesNotWaitForLifecycleMutation(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	prepareInteractiveConversation(t, server, "source")
	file, err := os.OpenFile(filepath.Join(server.config.AuthorityDir, "source.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response := postCreation(t, server, "/api/sessions/source/fork", `{"name":"fork","creationDate":"2026-09-12"}`, "application/json")
	if response.Code != http.StatusConflict || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("source lock blocked: %d", response.Code)
	}
}

func TestCreationWorkerPersistsSuccessAndPlanRetryUsesTheCapturedGoal(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	prepareInteractiveConversation(t, server, "source")
	plan := "Accepted plan"
	controller := &browserContractCodex{transcript: codex.Transcript{ThreadID: "thread-1", Status: "idle", CollaborationMode: "plan", Model: "model-1", ReasoningEffort: "high",
		Entries: []codex.TranscriptEntry{{Kind: "plan", TurnID: "approved", TurnStatus: "completed", Text: plan}}}}
	server.config.Codex = controller
	directory := t.TempDir()
	helper := filepath.Join(directory, "dev-session")
	started := filepath.Join(directory, "started")
	release := filepath.Join(directory, "release")
	// The first attempt fails after validation. The second waits while the test
	// models the CLI's durable completion, then returns its normal result.
	script := `#!/bin/sh
if [ ! -f "$CREATION_STARTED" ]; then
  touch "$CREATION_STARTED"
  printf 'temporary initialization failure\n' >&2
  exit 1
fi
touch "$CREATION_STARTED.second"
while [ ! -f "$CREATION_RELEASE" ]; do sleep 0.01; done
printf '{"slug":"2026-09-12-plan-retry"}\n'
`
	if err := os.WriteFile(helper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREATION_STARTED", started)
	t.Setenv("CREATION_RELEASE", release)
	server.config.DevSession = helper
	body := fmt.Sprintf(`{"action":"new","name":"plan-retry","creationDate":"2026-09-12","planTurnId":"approved","planText":%q,"planSha256":%q,"model":"model-1","reasoningEffort":"high"}`, plan, planDigest(plan))
	response := postCreation(t, server, "/api/sessions/source/implement-plan", body, "application/json")
	if response.Code != http.StatusAccepted {
		t.Fatalf("acceptance: %d %s", response.Code, response.Body.String())
	}
	failed := awaitCreation(t, server, "2026-09-12-plan-retry")
	if !failed.Validated || !strings.Contains(failed.Error, "temporary initialization failure") {
		t.Fatalf("first failure: %#v", failed)
	}
	controller.transcript.Entries = []codex.TranscriptEntry{{Kind: "plan", TurnID: "new-plan", TurnStatus: "completed", Text: "A newer unaccepted plan"}}
	retry := postCreation(t, server, "/api/sessions/2026-09-12-plan-retry/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, failed.ReceiptID), "application/json")
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s", retry.Code, retry.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started + ".second"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry CLI did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	writeCreationProof(t, server, failed)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ready := awaitCreation(t, server, "2026-09-12-plan-retry")
	if ready.State != "ready" || ready.Attempt != 2 || ready.Goal != "Implement the following approved plan from session source.\n\n"+plan {
		t.Fatalf("retry changed goal or did not complete: %#v", ready)
	}
	var stored creationReceipt
	if err := readCreationJSON(server.creationPath(ready.Request.Slug), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "ready" {
		t.Fatal("success was not durable")
	}
}

func TestCreationProofBeforeForkJournalRemovalStillOffersRetry(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""), Request: creationRequest{Kind: "fork", Slug: "2026-09-12-journal", Source: "source", SourceThreadID: "old-thread"},
		ReceiptID: strings.Repeat("c", 64), Attempt: 1, State: "paused", StartedAt: now, UpdatedAt: now, Validated: true, Model: "model-1", Effort: "high"}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	server.creations[receipt.Request.Slug] = receipt
	writeCreationProof(t, server, receipt)
	directory := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(directory, receipt.Request.Slug+".fork.json")
	if err := os.WriteFile(journal, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	paused, _ := server.currentCreation(receipt.Request.Slug)
	if paused.State != "paused" {
		t.Fatal("unfinished fork journal was hidden by completion evidence")
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	ready, _ := server.currentCreation(receipt.Request.Slug)
	if ready.State != "ready" {
		t.Fatalf("finished fork did not recover: %#v", ready)
	}
}

func TestCreationUnvalidatedRetryRefusesTheReplacementSource(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	prepareInteractiveConversation(t, server, "source")
	source, identity, err := server.creationSource("source")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""), Request: creationRequest{Kind: "fork", Slug: "2026-09-12-stale-source", Source: source.Slug, SourceThreadID: source.Codex.ThreadID, SourceIdentity: identity},
		ReceiptID: strings.Repeat("d", 64), Attempt: 1, State: "paused", StartedAt: now, UpdatedAt: now, Model: "model-1", Effort: "high"}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	server.creations[receipt.Request.Slug] = receipt
	directory := filepath.Join(server.config.Workspace, "work", "source")
	if err := os.Rename(directory, directory+"-old"); err != nil {
		t.Fatal(err)
	}
	prepareInteractiveConversation(t, server, "source")
	response := postCreation(t, server, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
	if response.Code != http.StatusAccepted {
		t.Fatalf("retry submission: %d", response.Code)
	}
	failed := awaitCreation(t, server, receipt.Request.Slug)
	if !strings.Contains(failed.Error, "replacement") {
		t.Fatalf("replacement source was used: %#v", failed)
	}
}

func TestCreationStorePreservesMaximallyEscapedCapturedMessages(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	goal := strings.Repeat("<", session.MaxMessageBytes)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
		Request:   creationRequest{Kind: "new", Slug: "2026-09-12-escaped", Goal: goal},
		ReceiptID: strings.Repeat("e", 64), Attempt: 1, State: "paused", StartedAt: now, UpdatedAt: now,
		Validated: true, Goal: goal, Model: "model-1", Effort: "high"}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	var restored creationReceipt
	if err := readCreationJSON(server.creationPath(receipt.Request.Slug), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Request.Goal != goal || restored.Goal != goal {
		t.Fatal("large accepted request was changed")
	}
}

func TestCreationFinalPersistenceFailureKeepsExplicitRetryAvailable(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server.config.Codex = &creationTestCodex{browserContractCodex: &browserContractCodex{}, entered: entered, release: release}
	receipt, err := server.acceptCreation(creationRequest{Kind: "new", Slug: "2026-09-12-save-failed", Goal: "Request"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	path := server.creationPath(receipt.Request.Slug)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	close(release)
	paused := awaitCreation(t, server, receipt.Request.Slug)
	if paused.State != "paused" || !strings.Contains(paused.Phase, "Unable to save") {
		t.Fatalf("page would wait forever after worker stopped: %#v", paused)
	}
}

func TestCreationRetiresCompletedReceiptsAcrossRestartWithoutLosingRecovery(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	pending := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
		Request:   creationRequest{Kind: "new", Slug: "pending-recovery", Goal: "original request"},
		ReceiptID: strings.Repeat("a", 64), Attempt: 3, State: "paused"}
	if err := server.saveCreation(pending); err != nil {
		t.Fatal(err)
	}
	var oldest creationReceipt
	for i := 0; i < maxCreationReceipts; i++ {
		receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
			Request:   creationRequest{Kind: "new", Slug: fmt.Sprintf("receipt-%04d", i)},
			ReceiptID: fmt.Sprintf("%064x", i+1), Attempt: 1, State: "ready"}
		if err := server.saveCreation(receipt); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = receipt
			writeCreationProof(t, server, receipt)
		}
	}
	if err := server.loadCreations(); err != nil {
		t.Fatal(err)
	}
	if len(server.creations) != maxCreationReceipts {
		t.Fatalf("loaded %d receipts", len(server.creations))
	}
	if server.creations[pending.Request.Slug].ReceiptID != pending.ReceiptID {
		t.Fatal("pending recovery was lost")
	}
	for _, path := range []string{server.creationPath(oldest.Request.Slug), server.creationEvidencePath(oldest), server.creationEvidencePath(oldest) + ".request"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("completed receipt retained at %s: %v", path, err)
		}
	}
	if _, err := server.acceptCreation(oldest.Request); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("retired receipt allowed destination replacement: %v", err)
	}
	if err := server.retireReadyCreations(1); err != nil {
		t.Fatal(err)
	}
	if len(server.creations) != maxCreationReceipts-1 {
		t.Fatal("completed history did not release a creation slot")
	}
	if err := server.loadCreations(); err != nil {
		t.Fatal(err)
	}
	replayed, err := server.acceptCreation(pending.Request)
	if err != nil || replayed.ReceiptID != pending.ReceiptID || replayed.Attempt != 3 || replayed.State != "paused" {
		t.Fatalf("pending retry identity changed: %#v, %v", replayed, err)
	}
	// A later completed deletion must also release a name whose retry-cache
	// record was already evicted, without leaving its CLI binding behind.
	if err := os.RemoveAll(filepath.Join(server.config.Workspace, "work", oldest.Request.Slug)); err != nil {
		t.Fatal(err)
	}
	request := oldest.Request
	request.Goal = "New request after deletion"
	replacement, err := server.acceptCreation(request)
	if err != nil || replacement.ReceiptID == oldest.ReceiptID {
		t.Fatalf("evicted receipt stranded deleted name: %#v, %v", replacement, err)
	}
}

// The base generation has no receipt store: its synchronous creation path can
// publish a canonical manifest after head accepted a request but before the CLI
// binding. This fixture uses that unchanged manifest/runtime format for the
// middle step, then starts a fresh head server against the same private state.
func TestCreationHeadBaseHeadBeforeCLIBindingPreservesCanonicalSession(t *testing.T) {
	server := newTestServer(t)
	entered := make(chan struct{}, 1)
	never := make(chan struct{})
	server.config.Codex = &creationTestCodex{browserContractCodex: &browserContractCodex{}, entered: entered, release: never}
	accepted, err := server.acceptCreation(creationRequest{Kind: "new", Slug: "2026-09-12-rollback", Goal: "Original accepted request"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	server.Close()
	if _, err := os.Stat(server.creationEvidencePath(accepted) + ".request"); !os.IsNotExist(err) {
		t.Fatalf("CLI binding already exists: %v", err)
	}
	// Base creates the same slug independently while ignoring the additive state.
	prepareInteractiveConversation(t, server, accepted.Request.Slug)
	server.config.Codex = &browserContractCodex{}
	restored, err := New(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	page := httptest.NewRecorder()
	restored.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/"+accepted.Request.Slug+"/", nil))
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "data-creation=") ||
		!strings.Contains(page.Body.String(), `id="message-form"`) || !strings.Contains(page.Body.String(), creationConflictMessage) {
		t.Fatalf("rolled-forward page hides canonical session: %d %s", page.Code, page.Body.String())
	}
	receipt, ok := restored.currentCreation(accepted.Request.Slug)
	if !ok || receipt.State != "conflict" || receipt.ReceiptID != accepted.ReceiptID || receipt.Attempt != 1 || receipt.Validated {
		t.Fatalf("independent session adopted as completion: %#v", receipt)
	}
	if receipt.status().CanonicalURL != "/"+accepted.Request.Slug+"/" {
		t.Fatal("conflict has no canonical destination")
	}
	retry := postCreation(t, restored, "/api/sessions/"+accepted.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
	if retry.Code != http.StatusConflict {
		t.Fatalf("conflicting request retried: %d %s", retry.Code, retry.Body.String())
	}
	sent := postCreation(t, restored, "/codex/conversations/"+accepted.Request.Slug+"/message", `{"message":"Use canonical session","clientUserMessageId":"00000000-0000-4000-8000-000000000071"}`, "application/json")
	if sent.Code != http.StatusAccepted {
		t.Fatalf("receipt blocks canonical controls: %d %s", sent.Code, sent.Body.String())
	}
	if _, _, err := restored.creationSource(accepted.Request.Slug); err != nil {
		t.Fatalf("receipt blocks canonical source: %v", err)
	}
	var stored creationReceipt
	if err := readCreationJSON(restored.creationPath(accepted.Request.Slug), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "conflict" {
		t.Fatal("conflict was not durable")
	}
	if _, err := os.Stat(restored.creationEvidencePath(receipt)); !os.IsNotExist(err) {
		t.Fatal("conflict synthesized completion evidence")
	}
}

func TestCreationHeadBaseHeadAfterBindingPreservesForeignCanonicalSession(t *testing.T) {
	for _, test := range []struct {
		name, goal, model string
		pending, conflict bool
	}{
		{name: "different base goal", goal: "Independent base request", model: "model-1", conflict: true},
		{name: "different base settings", goal: "Accepted head request", model: "model-2", conflict: true},
		{name: "matching recoverable request", goal: "Accepted head request", model: "model-1"},
		{name: "unfinished journal keeps authority", goal: "Independent base request", model: "model-1", pending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
				Request:   creationRequest{Kind: "new", Slug: "2026-09-12-bound-rollback", Goal: "Accepted head request"},
				ReceiptID: strings.Repeat("7", 64), Attempt: 1, State: "paused", Validated: true,
				Goal: "Accepted head request", Model: "model-1", Effort: "high"}
			if err := server.saveCreation(receipt); err != nil {
				t.Fatal(err)
			}
			// Head stopped after binding, before any destination journal or
			// manifest existed. Base ignores the additive binding completely.
			writeCreationBinding(t, server, receipt)
			server.Close()
			prepareInteractiveConversation(t, server, receipt.Request.Slug)
			manifestPath := filepath.Join(server.config.Workspace, "work", receipt.Request.Slug, "portal.yml")
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			manifest = append(manifest, []byte("  goal_sha256: "+planDigest(test.goal)+"\n")...)
			if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(server.config.Workspace, "worktrees", ".locks")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			journal := fmt.Sprintf(`{"schema":1,"slug":%q,"state":"ready","goal_sha256":%q,"run_codex":true,"model":%q,"effort":"high"}`, receipt.Request.Slug, planDigest(test.goal), test.model)
			if err := os.WriteFile(filepath.Join(directory, receipt.Request.Slug+".creation.json"), []byte(journal), 0600); err != nil {
				t.Fatal(err)
			}
			if test.pending {
				if err := os.WriteFile(filepath.Join(directory, receipt.Request.Slug+".start.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			server.config.Codex = &browserContractCodex{}
			restored, err := New(server.config)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			current, ok := restored.currentCreation(receipt.Request.Slug)
			if !ok || (current.State == "conflict") != test.conflict || current.State == "ready" || current.ReceiptID != receipt.ReceiptID || current.Attempt != 1 {
				t.Fatalf("incorrect reconciliation at binding boundary: %#v", current)
			}
			if test.conflict {
				page := httptest.NewRecorder()
				restored.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/"+receipt.Request.Slug+"/", nil))
				if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "data-creation=") || !strings.Contains(page.Body.String(), `id="message-form"`) {
					t.Fatal("bound conflicting receipt shadows the canonical session")
				}
				retry := postCreation(t, restored, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
				if retry.Code != http.StatusConflict {
					t.Fatal("bound conflicting receipt was retried")
				}
			}
			if _, err := os.Stat(restored.creationEvidencePath(receipt)); !os.IsNotExist(err) {
				t.Fatal("reconciliation fabricated completion evidence")
			}
			if bound, err := restored.readCreationBinding(receipt); !bound || err != nil {
				t.Fatalf("reconciliation changed the accepted CLI binding: %v", err)
			}
		})
	}
}

func TestCreationHeadBaseHeadForkBindingDoesNotAdoptMatchingCanonicalFork(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("fork-journal=%v", pending), func(t *testing.T) {
			server := newTestServer(t)
			receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
				Request:   creationRequest{Kind: "fork", Slug: "2026-09-12-bound-fork", Source: "source", SourceThreadID: "source-thread", SourceIdentity: strings.Repeat("a", 64)},
				ReceiptID: strings.Repeat("8", 64), Attempt: 1, State: "paused", Validated: true, Model: "model-1", Effort: "high"}
			if err := server.saveCreation(receipt); err != nil {
				t.Fatal(err)
			}
			writeCreationBinding(t, server, receipt)
			server.Close()
			// Base independently completes a fork with the same source and
			// settings, but cannot publish head's receipt-bound evidence.
			prepareInteractiveConversation(t, server, receipt.Request.Slug)
			manifestPath := filepath.Join(server.config.Workspace, "work", receipt.Request.Slug, "portal.yml")
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			manifest = append(manifest, []byte("forked_from: source\n")...)
			if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
				t.Fatal(err)
			}
			if pending {
				writeBoundForkRecovery(t, server, receipt)
			}
			restored, err := New(server.config)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			current, ok := restored.currentCreation(receipt.Request.Slug)
			if !ok || (current.State == "conflict") == pending || current.State == "ready" {
				t.Fatalf("matching canonical fork lost its ownership boundary: %#v", current)
			}
			if !pending {
				retry := postCreation(t, restored, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
				if retry.Code != http.StatusConflict {
					t.Fatal("binding-only fork attempted to recover an unrelated destination")
				}
			}
			if _, err := os.Stat(restored.creationEvidencePath(receipt)); !os.IsNotExist(err) {
				t.Fatal("matching canonical fork was adopted as receipt completion")
			}
		})
	}
}

func TestCreationRetiresDeletedReadySlugBeforeAcceptingAnotherRequest(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""), Request: creationRequest{Kind: "new", Slug: "2026-09-12-reuse", Goal: "Old request"},
		ReceiptID: strings.Repeat("1", 64), Attempt: 1, State: "ready", Validated: true, Goal: "Old request", Model: "model-1", Effort: "high"}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	server.creations[receipt.Request.Slug] = receipt
	writeCreationProof(t, server, receipt)
	if err := os.WriteFile(server.creationEvidencePath(receipt)+".request", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	authority := filepath.Join(server.config.AuthorityDir, receipt.Request.Slug+".json")
	if err := os.WriteFile(authority, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(server.config.Workspace, "work", receipt.Request.Slug)); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.currentCreation(receipt.Request.Slug); !ok {
		t.Fatal("retired while runtime identity still exists")
	}
	if err := os.Remove(authority); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	var removal string
	for _, journal := range session.LifecycleJournals() {
		if journal.Command == "delete" {
			removal = filepath.Join(directory, receipt.Request.Slug+"."+journal.Name+".json")
		}
	}
	if err := os.WriteFile(removal, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.currentCreation(receipt.Request.Slug); !ok {
		t.Fatal("retired while deletion remains unfinished")
	}
	if err := os.Remove(removal); err != nil {
		t.Fatal(err)
	}
	runtimeLock, err := os.OpenFile(filepath.Join(server.config.AuthorityDir, receipt.Request.Slug+".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeLock.Close()
	if err := unix.Flock(int(runtimeLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.currentCreation(receipt.Request.Slug); !ok {
		t.Fatal("retired while another CLI operation holds the runtime lock")
	}
	if err := unix.Flock(int(runtimeLock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	accepted, err := server.acceptCreation(creationRequest{Kind: "new", Slug: receipt.Request.Slug, Goal: "Different request"})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.ReceiptID == receipt.ReceiptID || accepted.Request.Goal != "Different request" || accepted.Attempt != 1 {
		t.Fatalf("stale receipt reused: %#v", accepted)
	}
	if _, err := os.Stat(server.creationEvidencePath(receipt) + ".request"); !os.IsNotExist(err) {
		t.Fatalf("retired binding leaked: %v", err)
	}
	stale := postCreation(t, server, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
	if stale.Code != http.StatusConflict {
		t.Fatalf("old receipt targets new attempt: %d", stale.Code)
	}
}

func TestCreationEvidenceGapDeletionRetiresTheExactRequest(t *testing.T) {
	for _, kind := range []string{"new", "plan", "fork"} {
		for _, state := range []string{"paused", "failed"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				server := newTestServer(t)
				started := time.Now().UTC().Add(-time.Minute)
				receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
					Request:   creationRequest{Kind: kind, Slug: "2026-09-12-deleted-" + kind, Goal: "Original goal"},
					ReceiptID: strings.Repeat("d", 64), Attempt: 1, State: state, StartedAt: started.Format(time.RFC3339Nano),
					Validated: true, Goal: "Original goal", Model: "model-1", Effort: "high"}
				if kind != "new" {
					receipt.Request.Source = "source"
					receipt.Request.SourceThreadID = "source-thread"
					receipt.Request.SourceIdentity = strings.Repeat("f", 64)
				}
				// Capture the existing deletion independently of its clock value.
				// Merely missing tracking or changing another slug cannot end it.
				writeCompletedRemovalMarker(t, server, receipt.Request.Slug, strings.Repeat("a", 64), started.Add(time.Hour))
				history, err := session.CompletedRemovalHistory(server.config.Workspace, receipt.Request.Slug, server.config.UserStateRoot)
				if err != nil {
					t.Fatal(err)
				}
				receipt.DeletionHistorySHA256 = history
				if err := server.saveCreation(receipt); err != nil {
					t.Fatal(err)
				}
				server.creations[receipt.Request.Slug] = receipt
				writeCompletedRemovalMarker(t, server, "different-slug", strings.Repeat("b", 64), time.Now())
				if current, ok := server.currentCreation(receipt.Request.Slug); !ok || current.State != state {
					t.Fatal("absence or unrelated deletion retired an incomplete request")
				}
				writeCreationProof(t, server, receipt)
				if err := os.Remove(server.creationEvidencePath(receipt)); err != nil {
					t.Fatal(err)
				}
				server.Close()
				// Exercise the existing Ruby deletion producer, including the
				// canonical tracking move and final schema-1 recovery marker.
				script := `load ARGV.fetch(0)
runner = DevSession::Runner.new(workspace: ARGV.fetch(1), env: ENV.to_h.merge('DEV_WORKSPACES_STATE' => ARGV.fetch(2), 'PATH' => ''))
slug = ARGV.fetch(3)
removal = runner.send(:prepare_removal!, slug, force: false)
%w[validated thread_retiring thread_retired clusters_released worktrees_removed runtime_retired].each { |phase| runner.send(:advance_removal!, slug, removal, phase) }
runner.send(:preserve_removed_state!, slug, removal, nil)
runner.send(:advance_removal!, slug, removal, 'tracking_preserved')
runner.send(:advance_removal!, slug, removal, 'tracking_committed')
runner.send(:finalize_removal!, slug, removal)
removal['removed_at'] = '2000-01-01T00:00:00Z'
runner.send(:write_removal_metadata!, removal.fetch('recovery'), removal)
`
				command := exec.Command("ruby", "-e", script, "../../../libexec/dev-session", server.config.Workspace, server.config.UserStateRoot, receipt.Request.Slug)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("delete fixture: %v\n%s", err, output)
				}
				lock, err := os.OpenFile(filepath.Join(server.config.AuthorityDir, receipt.Request.Slug+".lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				restored, err := New(server.config)
				if err != nil {
					t.Fatal(err)
				}
				defer restored.Close()
				if _, ok := restored.currentCreation(receipt.Request.Slug); !ok {
					t.Fatal("retired during an active destination mutation")
				}
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
					t.Fatal(err)
				}
				stale := postCreation(t, restored, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
				if stale.Code != http.StatusNotFound {
					t.Fatalf("deleted request remained retryable: %d", stale.Code)
				}
				if _, err := os.Stat(filepath.Join(server.config.Workspace, "work", receipt.Request.Slug)); !os.IsNotExist(err) {
					t.Fatal("stale retry recreated deleted tracking")
				}
				for _, path := range []string{server.creationPath(receipt.Request.Slug), server.creationEvidencePath(receipt) + ".request"} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("deleted request cache was retained")
					}
				}
				fresh, err := restored.acceptCreation(creationRequest{Kind: "new", Slug: receipt.Request.Slug, Goal: "Fresh request"})
				if err != nil {
					t.Fatal(err)
				}
				if fresh.ReceiptID == receipt.ReceiptID {
					t.Fatal("name reuse retained the deleted receipt")
				}
				stale = postCreation(t, restored, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
				if stale.Code != http.StatusConflict {
					t.Fatal("old retry targeted the new request")
				}
			})
		}
	}
}

func TestCreationReclaimsDeletedReceiptsForUnrelatedAdmissionAndStartup(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			server := newTestServer(t)
			started := time.Now().UTC().Add(-time.Minute)
			count := maxCreationReceipts
			if restart {
				count++
			}
			for i := 0; i < count; i++ {
				receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
					Request:   creationRequest{Kind: "new", Slug: fmt.Sprintf("deleted-cache-%04d", i), Goal: "Original goal"},
					ReceiptID: fmt.Sprintf("%064x", i+1), Attempt: 2, State: "paused", StartedAt: started.Format(time.RFC3339Nano)}
				if i%2 != 0 {
					receipt.State = "failed"
				}
				if i == 3 && !restart {
					receipt.State = "running"
				}
				if err := server.saveCreation(receipt); err != nil {
					t.Fatal(err)
				}
				server.creations[receipt.Request.Slug] = receipt
				if i != 0 && (i != 3 || !restart) {
					writeCompletedRemovalMarker(t, server, receipt.Request.Slug, strings.Repeat("a", 64), time.Now().UTC())
				}
			}
			lock, err := os.OpenFile(filepath.Join(server.config.AuthorityDir, "deleted-cache-0001.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			locks := filepath.Join(server.config.Workspace, "worktrees", ".locks")
			if err := os.MkdirAll(locks, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(locks, "deleted-cache-0002.fork.json"), []byte("unfinished"), 0600); err != nil {
				t.Fatal(err)
			}
			if restart {
				server.Close()
				server, err = New(server.config)
				if err != nil {
					t.Fatalf("deleted receipt history blocked startup: %v", err)
				}
				if len(server.creations) != 4 {
					t.Fatalf("startup retained %d receipts", len(server.creations))
				}
			}
			defer server.Close()
			fresh, err := server.acceptCreation(creationRequest{Kind: "new", Slug: "unrelated-new-session", Goal: "Fresh request"})
			if err != nil {
				t.Fatalf("deleted receipt history blocked unrelated admission: %v", err)
			}
			server.operationMu.Lock()
			defer server.operationMu.Unlock()
			if len(server.creations) != 5 {
				t.Fatalf("admission retained %d receipts", len(server.creations))
			}
			for i := 0; i < 4; i++ {
				receipt, ok := server.creations[fmt.Sprintf("deleted-cache-%04d", i)]
				if !ok || receipt.ReceiptID != fmt.Sprintf("%064x", i+1) || receipt.Attempt != 2 {
					t.Fatalf("reclamation discarded unresolved receipt %d", i)
				}
			}
			if fresh.Request.Slug != "unrelated-new-session" || fresh.Attempt != 1 {
				t.Fatal("unrelated request reused deleted identity")
			}
		})
	}
}

func TestCreationConflictRetentionIsBoundedAndDoesNotEvictPendingBindings(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	var first creationReceipt
	for i := 0; i < maxCreationReceipts; i++ {
		receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""), Request: creationRequest{Kind: "new", Slug: fmt.Sprintf("conflict-%04d", i)}, ReceiptID: fmt.Sprintf("%064x", i+1), Attempt: 1, State: "conflict"}
		if err := server.saveCreation(receipt); err != nil {
			t.Fatal(err)
		}
		server.creations[receipt.Request.Slug] = receipt
		if i == 0 {
			first = receipt
		}
	}
	if err := server.retireReadyCreations(1); err != nil {
		t.Fatal(err)
	}
	if len(server.creations) != maxCreationReceipts-1 {
		t.Fatal("conflicts impose a lifetime ceiling")
	}
	if _, ok := server.creations[first.Request.Slug]; ok {
		t.Fatal("oldest terminal conflict retained")
	}
	protected := server.creations["conflict-0001"]
	protected.State = "ready"
	server.creations[protected.Request.Slug] = protected
	if err := server.saveCreation(protected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.creationEvidencePath(protected)+".request", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, protected.Request.Slug+".fork.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := server.retireReadyCreations(3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(server.creationEvidencePath(protected) + ".request"); err != nil {
		t.Fatal("terminal eviction removed an unfinished journal's binding")
	}
	if err := os.Remove(filepath.Join(directory, protected.Request.Slug+".fork.json")); err != nil {
		t.Fatal(err)
	}
	journal := fmt.Sprintf(`{"schema":1,"slug":%q,"state":"creating"}`, protected.Request.Slug)
	if err := os.WriteFile(filepath.Join(directory, protected.Request.Slug+".creation.json"), []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	if err := server.retireReadyCreations(4); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(server.creationEvidencePath(protected) + ".request"); err != nil {
		t.Fatal("terminal eviction removed an unfinished initial request's binding")
	}
}

func writeBoundForkRecovery(t *testing.T, server *Server, receipt creationReceipt) string {
	t.Helper()
	writeCreationBinding(t, server, receipt)
	directory := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, receipt.Request.Slug+".fork.json")
	// The portal only needs to know that this CLI-owned journal is pending.
	// Its schema and recovery checks are exercised by the Ruby owner's tests.
	if err := os.WriteFile(path, []byte("opaque pending fork journal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreationBoundForkJournalRecoversWithoutItsSource(t *testing.T) {
	for _, sourceState := range []string{"archived", "deleted"} {
		t.Run(sourceState, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			prepareInteractiveConversation(t, server, "source")
			source, identity, err := server.creationSource("source")
			if err != nil {
				t.Fatal(err)
			}
			receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""), Request: creationRequest{Kind: "fork", Slug: "2026-09-12-recover-fork", Source: "source", SourceThreadID: source.Codex.ThreadID, SourceIdentity: identity},
				ReceiptID: strings.Repeat("3", 64), Attempt: 1, State: "paused", Validated: true, Model: "model-1", Effort: "high"}
			if err := server.saveCreation(receipt); err != nil {
				t.Fatal(err)
			}
			server.creations[receipt.Request.Slug] = receipt
			journal := writeBoundForkRecovery(t, server, receipt)
			sourceDirectory := filepath.Join(server.config.Workspace, "work", "source")
			if sourceState == "archived" {
				archive := filepath.Join(server.config.Workspace, "archive")
				if err := os.MkdirAll(archive, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(sourceDirectory, filepath.Join(archive, "source")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.RemoveAll(sourceDirectory); err != nil {
					t.Fatal(err)
				}
			}
			server.config.Codex = &browserContractCodex{}
			directory := t.TempDir()
			helper := filepath.Join(directory, "dev-session")
			marker := filepath.Join(directory, "started")
			release := filepath.Join(directory, "release")
			script := "#!/bin/sh\ntouch \"$FORK_STARTED\"\nwhile [ ! -f \"$FORK_RELEASE\" ]; do sleep 0.01; done\nrm \"$FORK_JOURNAL\"\nprintf '{\"slug\":\"2026-09-12-recover-fork\"}\\n'\n"
			if err := os.WriteFile(helper, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FORK_STARTED", marker)
			t.Setenv("FORK_RELEASE", release)
			t.Setenv("FORK_JOURNAL", journal)
			server.config.DevSession = helper
			response := postCreation(t, server, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
			if response.Code != http.StatusAccepted {
				t.Fatalf("retry: %d %s", response.Code, response.Body.String())
			}
			deadline := time.Now().Add(time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					current, _ := server.currentCreation(receipt.Request.Slug)
					t.Fatalf("journal recovery reread source: %#v", current)
				}
				time.Sleep(5 * time.Millisecond)
			}
			writeCreationProof(t, server, receipt)
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			completed := awaitCreation(t, server, receipt.Request.Slug)
			if completed.State != "ready" || completed.ReceiptID != receipt.ReceiptID {
				t.Fatalf("journal recovery failed: %#v", completed)
			}
		})
	}
}

func TestCreationValidatedPlanRetryDelegatesRecoveryWithoutItsSource(t *testing.T) {
	for _, sourceState := range []string{"archived", "deleted"} {
		t.Run(sourceState, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			prepareInteractiveConversation(t, server, "source")
			source, identity, err := server.creationSource("source")
			if err != nil {
				t.Fatal(err)
			}
			plan := "The exact accepted plan."
			receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
				Request:   creationRequest{Kind: "plan", Slug: "2026-09-12-plan-recovery", Source: "source", SourceThreadID: source.Codex.ThreadID, SourceIdentity: identity, PlanText: plan, PlanTurnID: "accepted-turn", PlanSHA256: planDigest(plan)},
				ReceiptID: strings.Repeat("9", 64), Attempt: 1, State: "paused", Validated: true, Goal: "Implement the following approved plan from session source.\n\n" + plan, Model: "model-1", Effort: "high"}
			if err := server.saveCreation(receipt); err != nil {
				t.Fatal(err)
			}
			server.creations[receipt.Request.Slug] = receipt
			// Ruby tests own the actual journal crash boundary. Here the portal
			// must forward the recorded request and await the CLI's exact proof.
			writeCreationProof(t, server, receipt)
			if err := os.Remove(server.creationEvidencePath(receipt)); err != nil {
				t.Fatal(err)
			}
			sourceDirectory := filepath.Join(server.config.Workspace, "work", "source")
			if sourceState == "archived" {
				archive := filepath.Join(server.config.Workspace, "archive")
				if err := os.MkdirAll(archive, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(sourceDirectory, filepath.Join(archive, "source")); err != nil {
					t.Fatal(err)
				}
			} else if err := os.RemoveAll(sourceDirectory); err != nil {
				t.Fatal(err)
			}
			server.config.Codex = &browserContractCodex{}
			directory := t.TempDir()
			helper := filepath.Join(directory, "dev-session")
			marker, release := filepath.Join(directory, "started"), filepath.Join(directory, "release")
			arguments, goal := filepath.Join(directory, "arguments"), filepath.Join(directory, "goal")
			script := `#!/bin/sh
printf '%s\n' "$@" >"$PLAN_ARGS"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--goal-file" ]; then cp "$2" "$PLAN_GOAL"; fi
  shift
done
touch "$PLAN_STARTED"
while [ ! -f "$PLAN_RELEASE" ]; do sleep 0.01; done
printf '{"slug":"2026-09-12-plan-recovery"}\n'
`
			if err := os.WriteFile(helper, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PLAN_ARGS", arguments)
			t.Setenv("PLAN_GOAL", goal)
			t.Setenv("PLAN_STARTED", marker)
			t.Setenv("PLAN_RELEASE", release)
			server.config.DevSession = helper
			response := postCreation(t, server, "/api/sessions/"+receipt.Request.Slug+"/creation/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receipt.ReceiptID), "application/json")
			if response.Code != http.StatusAccepted {
				t.Fatalf("retry: %d %s", response.Code, response.Body.String())
			}
			deadline := time.Now().Add(time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					current, _ := server.currentCreation(receipt.Request.Slug)
					t.Fatalf("plan recovery reread source: %#v", current)
				}
				time.Sleep(5 * time.Millisecond)
			}
			passed, err := os.ReadFile(arguments)
			if err != nil {
				t.Fatal(err)
			}
			for option, expected := range map[string]string{"--expected-source": "source", "--expected-source-thread": source.Codex.ThreadID, "--expected-source-identity": identity, "--creation-receipt-id": receipt.ReceiptID, "--model": receipt.Model, "--effort": receipt.Effort} {
				if !strings.Contains(string(passed), option+"\n"+expected+"\n") {
					t.Fatalf("CLI lost captured %s", option)
				}
			}
			delivered, err := os.ReadFile(goal)
			if err != nil || string(delivered) != receipt.Goal {
				t.Fatalf("CLI goal changed: %v", err)
			}
			writeCreationProof(t, server, receipt)
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			completed := awaitCreation(t, server, receipt.Request.Slug)
			if completed.State != "ready" || completed.Request != receipt.Request || completed.Attempt != 2 {
				t.Fatalf("delegated plan recovery failed: %#v", completed)
			}
		})
	}
}

func TestCreationHeadBaseArchiveHeadKeepsCanonicalHistoryAccessible(t *testing.T) {
	for _, kind := range []string{"new", "plan", "fork"} {
		for _, evidence := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/evidence=%v", kind, evidence), func(t *testing.T) {
				server := newTestServer(t)
				receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, DeletionHistorySHA256: planDigest(""),
					Request:   creationRequest{Kind: kind, Slug: "2026-09-12-archive-" + kind, Goal: "Exact original request"},
					ReceiptID: strings.Repeat("e", 64), Attempt: 1, State: "paused", Validated: true, Goal: "Exact original request", Model: "model-1", Effort: "high"}
				if kind != "new" {
					receipt.Request.Source = "source"
					receipt.Request.SourceThreadID = "source-thread"
					receipt.Request.SourceIdentity = strings.Repeat("f", 64)
				}
				if err := server.saveCreation(receipt); err != nil {
					t.Fatal(err)
				}
				server.Close()
				writeCreationProof(t, server, receipt)
				if !evidence {
					if err := os.Remove(server.creationEvidencePath(receipt)); err != nil {
						t.Fatal(err)
					}
				}
				// The preceding generation archives its completed canonical
				// tracking by rename, preserving thread and directory identity.
				directory := filepath.Join(server.config.Workspace, "work", receipt.Request.Slug)
				manifestPath := filepath.Join(directory, "portal.yml")
				manifest, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				manifest = append(manifest, []byte("finalized_at: '2026-09-12T12:00:00Z'\n")...)
				if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
					t.Fatal(err)
				}
				writeWebTrackingFiles(t, directory, "complete")
				archive := filepath.Join(server.config.Workspace, "archive")
				if err := os.MkdirAll(archive, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(directory, filepath.Join(archive, receipt.Request.Slug)); err != nil {
					t.Fatal(err)
				}
				locks := filepath.Join(server.config.Workspace, "worktrees", ".locks")
				if err := os.MkdirAll(locks, 0700); err != nil {
					t.Fatal(err)
				}
				var pendingPath string
				for _, journal := range session.LifecycleJournals() {
					if journal.Command == "archive" {
						pendingPath = filepath.Join(locks, receipt.Request.Slug+"."+journal.Name+".json")
					}
				}
				if err := os.WriteFile(pendingPath, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				restored, err := New(server.config)
				if err != nil {
					t.Fatal(err)
				}
				defer restored.Close()
				if current, _ := restored.currentCreation(receipt.Request.Slug); current.State != "paused" {
					t.Fatal("unfinished archive lost recovery authority")
				}
				if err := os.Remove(pendingPath); err != nil {
					t.Fatal(err)
				}
				current, ok := restored.currentCreation(receipt.Request.Slug)
				expected := "conflict"
				if evidence {
					expected = "ready"
				}
				if !ok || current.State != expected || current.ReceiptID != receipt.ReceiptID || current.Request != receipt.Request {
					t.Fatalf("archive reconciliation changed ownership: %#v", current)
				}
				page := httptest.NewRecorder()
				restored.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/"+receipt.Request.Slug+"/", nil))
				if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "data-creation=") || strings.Contains(page.Body.String(), `id="message-form"`) {
					t.Fatalf("receipt hides or reopens canonical archive: %d", page.Code)
				}
				if !evidence && (!strings.Contains(page.Body.String(), creationArchivedMessage) || strings.Contains(page.Body.String(), creationConflictMessage)) {
					t.Fatal("archive conflict has incorrect explanatory copy")
				}
				_, err = os.Stat(restored.creationEvidencePath(receipt))
				if !evidence && !os.IsNotExist(err) {
					t.Fatal("archive reconciliation synthesized completion evidence")
				}
				if evidence {
					// Exact evidence must still reject a changed retained thread.
					changed := strings.Replace(string(manifest), "created-thread", "replacement-thread", 1)
					if err := os.WriteFile(filepath.Join(archive, receipt.Request.Slug, "portal.yml"), []byte(changed), 0644); err != nil {
						t.Fatal(err)
					}
					if err := restored.proveCreation(receipt); err == nil {
						t.Fatal("archived replacement thread accepted earlier completion evidence")
					}
				} else {
					if err := os.Rename(filepath.Join(archive, receipt.Request.Slug), directory); err != nil {
						t.Fatal(err)
					}
					writeWebTrackingFiles(t, directory, "active")
					active := strings.Replace(string(manifest), "finalized_at: '2026-09-12T12:00:00Z'\n", "", 1)
					if err := os.WriteFile(manifestPath, []byte(active), 0644); err != nil {
						t.Fatal(err)
					}
					page = httptest.NewRecorder()
					restored.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/"+receipt.Request.Slug+"/", nil))
					if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "This session is archived.") || !strings.Contains(page.Body.String(), creationArchivedMessage) {
						t.Fatal("historical creation notice misreports a revived session")
					}
				}
			})
		}
	}
}
