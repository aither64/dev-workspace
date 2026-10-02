package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/creationprogress"
)

func progressReceipt(server *Server) creationReceipt {
	return creationReceipt{Schema: 1, Workspace: server.config.Workspace,
		ReceiptID: strings.Repeat("a", 64), Attempt: 1, State: "running", Validated: true,
		Request: creationRequest{Kind: "new", Slug: "2026-10-02-progress", Goal: "Original request"},
		Goal:    "Original request", Model: "captured-model", Effort: "high", Phase: "Starting"}
}

func TestCreationProgressMergesOnlyCurrentRunningAttempt(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	receipt := progressReceipt(server)
	server.creations[receipt.Request.Slug] = receipt
	observer := server.creationProgressObserver(receipt)
	current := receipt
	current.Model = "newly-persisted-model"
	server.creations[receipt.Request.Slug] = current
	event := creationprogress.Event{Stage: "team_member", Event: "begin", Member: "reviewer0"}
	observer(event)
	updated := server.creations[receipt.Request.Slug]
	if updated.Model != current.Model || updated.Goal != current.Goal || updated.State != "running" || !updated.Validated || !strings.Contains(updated.Phase, "reviewer0") {
		t.Fatalf("progress changed receipt authority: %#v", updated)
	}
	observer(event)
	if server.creations[receipt.Request.Slug].UpdatedAt != updated.UpdatedAt {
		t.Fatal("duplicate progress rewrote receipt")
	}
	for _, state := range []string{"paused", "failed", "ready", "cancelled", "conflict"} {
		current.State, current.Phase = state, "retained terminal phase"
		server.creations[receipt.Request.Slug] = current
		observer(event)
		if server.creations[receipt.Request.Slug].Phase != current.Phase {
			t.Fatalf("updated %s receipt", state)
		}
	}
	current.State, current.Attempt = "running", 2
	server.creations[receipt.Request.Slug] = current
	observer(event)
	if server.creations[receipt.Request.Slug].Phase != current.Phase {
		t.Fatal("stale attempt updated phase")
	}
	current.Attempt, current.ReceiptID = 1, strings.Repeat("b", 64)
	server.creations[receipt.Request.Slug] = current
	observer(event)
	if server.creations[receipt.Request.Slug].Phase != current.Phase {
		t.Fatal("stale receipt updated phase")
	}
}

func TestCreationCommandStreamsBeforeExitWithoutReadyEvidence(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	receipt := progressReceipt(server)
	server.creations[receipt.Request.Slug] = receipt
	release := filepath.Join(t.TempDir(), "release")
	helper := filepath.Join(t.TempDir(), "creation-helper")
	script := "#!/bin/sh\n" +
		"test \"$DEV_WORKSPACE_CREATION_PROGRESS\" = 1 || exit 2\n" +
		"printf 'diagnostic before\\n' >&2\n" +
		"printf '\\036DEV_WORKSPACE_CREATION_PRO' >&2\n" +
		"printf 'GRESS/1 {\"stage\":\"team_member\",\"event\":\"begin\",\"elapsedMs\":0,\"member\":\"architect0\"}\\n' >&2\n" +
		"while [ ! -f \"$PROGRESS_TEST_RELEASE\" ]; do sleep 0.01; done\n" +
		"printf '\\036DEV_WORKSPACE_CREATION_PROGRESS/1 {\"stage\":\"evidence\",\"event\":\"finish\",\"elapsedMs\":1}\\n' >&2\n" +
		"printf 'diagnostic after\\n' >&2\n" +
		"printf '{\"slug\":\"2026-10-02-progress\"}\\n'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROGRESS_TEST_RELEASE", release)
	server.config.DevSession = helper
	observed := make(chan struct{}, 1)
	observer := server.creationProgressObserver(receipt)
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		stdout, stderr, err := server.runCreationCommand(ctx, 3*time.Second, nil, func(event creationprogress.Event) {
			observer(event)
			if event.Stage == "team_member" {
				observed <- struct{}{}
			}
		})
		if err == nil && (strings.TrimSpace(stdout) != `{"slug":"2026-10-02-progress"}` || stderr != "diagnostic before\ndiagnostic after\n") {
			err = fmt.Errorf("stdout=%q stderr=%q", stdout, stderr)
		}
		result <- err
	}()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("no live nested stage before helper exit")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/"+receipt.Request.Slug+"/creation", nil))
	var status creationStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("status %s: %v", response.Body, err)
	}
	if status.State != "running" || !strings.Contains(status.Phase, "architect0") {
		t.Fatalf("live status = %#v", status)
	}
	select {
	case err := <-result:
		t.Fatalf("command completed before release: %v", err)
	default:
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	server.operationMu.Lock()
	current := server.creations[receipt.Request.Slug]
	server.operationMu.Unlock()
	if current.State != "running" || server.proveCreation(current) == nil {
		t.Fatal("progress supplied ready authority")
	}
}

func TestCreationCommandPreservesFailureAndCancellation(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf 'real diagnostic\\n' >&2\nprintf '\\036DEV_WORKSPACE_CREATION_PROGRESS/1 bad\\n' >&2\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = helper
	_, stderr, err := server.runCreationCommand(context.Background(), time.Second, nil, func(creationprogress.Event) { t.Error("invalid frame reached callback") })
	if err == nil || !strings.Contains(stderr, "real diagnostic") {
		t.Fatalf("failure = %q, %v", stderr, err)
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err = server.runCreationCommand(context.Background(), 50*time.Millisecond, nil, nil)
	if err == nil {
		t.Fatal("cancelled creation command succeeded")
	}
}
