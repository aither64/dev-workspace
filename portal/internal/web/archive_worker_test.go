package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveWorkerQueuedStartIsNotTerminal(t *testing.T) {
	directory := t.TempDir()
	statusPath := filepath.Join(directory, "status")
	for name, script := range map[string]string{
		"systemctl":  "#!/bin/sh\ncat \"$ARCHIVE_TEST_STATUS\"\n",
		"journalctl": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARCHIVE_TEST_STATUS", statusPath)
	server := &Server{config: Config{Workspace: directory, WorkspaceName: "example", HostProfile: directory, PackageRoot: directory}, operationContext: context.Background()}
	operation := lifecycleOperation{Slug: "example", Kind: "archive", State: "running", ReceiptID: strings.Repeat("a", 64), Attempt: 2}
	for _, step := range []struct {
		name, properties, want string
	}{
		{"queued", "ActiveState=inactive\nSubState=dead\nJob=123\nExecMainCode=0\nExecMainStatus=0\n", "running"},
		{"starting", "ActiveState=activating\nSubState=start-pre\nJob=123\n", "running"},
		{"executing", "ActiveState=active\nSubState=running\nJob=\n", "running"},
		{"completed", "ActiveState=active\nSubState=exited\nJob=\nExecMainCode=1\nExecMainStatus=0\n", "complete"},
		{"cancelled before exec", "ActiveState=inactive\nSubState=dead\nJob=\nExecMainCode=0\nExecMainStatus=0\n", "failed"},
		{"pre-exec failed", "ActiveState=failed\nSubState=failed\nJob=\nExecMainCode=0\nExecMainStatus=0\n", "failed"},
	} {
		t.Run(step.name, func(t *testing.T) {
			if err := os.WriteFile(statusPath, []byte("LoadState=loaded\n"+step.properties), 0o600); err != nil {
				t.Fatal(err)
			}
			reconciled, exists, err := server.proposeLifecycleOperation("example", operation, true, nil, nil, time.Now())
			if err != nil || !exists || reconciled.State != step.want {
				t.Fatalf("reconcile %s: %#v, exists %v, error %v", step.name, reconciled, exists, err)
			}
			if step.want == "running" && reconciled.FinishedAt != "" {
				t.Fatalf("uncompleted worker has finish time: %#v", reconciled)
			}
		})
	}
}

func TestArchiveWorkerContinuesAcrossPortalCancellation(t *testing.T) {
	directory := t.TempDir()
	statusPath := filepath.Join(directory, "status")
	launchPath := filepath.Join(directory, "launch")
	for name, script := range map[string]string{
		"systemd-run": "#!/bin/sh\nprintf '%s\\n' \"$@\" >\"$ARCHIVE_TEST_LAUNCH.tmp\"\nmv \"$ARCHIVE_TEST_LAUNCH.tmp\" \"$ARCHIVE_TEST_LAUNCH\"\n",
		"systemctl":   "#!/bin/sh\ncat \"$ARCHIVE_TEST_STATUS\"\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARCHIVE_TEST_STATUS", statusPath)
	t.Setenv("ARCHIVE_TEST_LAUNCH", launchPath)
	if err := os.WriteFile(statusPath, []byte("LoadState=loaded\nActiveState=active\nSubState=running\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	operation := lifecycleOperation{Slug: "example", Kind: "archive", State: "running", ReceiptID: strings.Repeat("a", 64), Attempt: 1}
	server := &Server{config: Config{Workspace: directory, WorkspaceName: "example", HostProfile: directory, PackageRoot: directory}, operations: map[string]lifecycleOperation{"example": operation}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.runSupervisedArchive(ctx, "example", []string{"archive", "example", "--as-is"}) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(launchPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("archive worker was not launched")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel portal wait: %v", err)
	}
	launch, _ := os.ReadFile(launchPath)
	if !strings.Contains(string(launch), "TimeoutStartSec=infinity") || strings.Contains(string(launch), "--wait") {
		t.Fatalf("worker lifetime is coupled to portal: %s", launch)
	}
	store, err := newLifecycleOperationStore(directory, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	operation.State = "failed"
	operation.Phase = "starting"
	operation.StartedAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	operation.UpdatedAt = operation.StartedAt
	operation.FinishedAt = operation.StartedAt
	operation.Options.Mode = "archived"
	operation.Redirect = "/"
	if err := store.save(map[string]lifecycleOperation{"example": operation}); err != nil {
		t.Fatal(err)
	}
	retained, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	server.operationContext = context.Background()
	reconciled, _, err := server.proposeLifecycleOperation("example", retained["example"], true, nil, nil, time.Now())
	if err != nil || reconciled.State != "running" || reconciled.FinishedAt != "" {
		t.Fatalf("stored receipt after observer restart: %#v, %v", reconciled, err)
	}
	if err := os.WriteFile(statusPath, []byte("LoadState=loaded\nActiveState=active\nSubState=exited\nExecMainStatus=0\nExecMainExitTimestamp=@1792174000.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := server.archiveWorker(context.Background(), operation)
	if err != nil || status == nil || status.State != "complete" {
		t.Fatalf("recover worker result after portal cancellation: %#v, %v", status, err)
	}
	reconciled, _, err = server.proposeLifecycleOperation("example", reconciled, true, nil, nil, time.Now())
	wantFinish := time.Unix(1792174000, 500000000).UTC().Format(time.RFC3339Nano)
	if err != nil || reconciled.State != "complete" || reconciled.FinishedAt != wantFinish {
		t.Fatalf("worker final receipt timestamp: %#v, %v", reconciled, err)
	}
	if err := store.save(map[string]lifecycleOperation{"example": reconciled}); err != nil {
		t.Fatal(err)
	}
	retained, err = store.load()
	if err != nil || retained["example"].FinishedAt != wantFinish {
		t.Fatalf("retained worker finish time: %#v, %v", retained, err)
	}
}

func TestCommandFailurePreservesDeadlineOverSuccessfulFetchOutput(t *testing.T) {
	err := commandFailure("archive session", "", "From github.com:example/workspace\n * branch master -> FETCH_HEAD", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "FETCH_HEAD") {
		t.Fatalf("deadline hidden by fetch output: %v", err)
	}
}

func TestLifecycleDisplayIncludesProgressForItsExactArchiveOperation(t *testing.T) {
	server := newTestServer(t)
	operation := lifecycleOperation{Slug: "example", Kind: "archive", State: "running", Options: lifecycleOperationOptions{JournalID: strings.Repeat("a", 64)}}
	directory := filepath.Join(server.config.UserStateRoot, "archive-progress", fmt.Sprintf("%x", sha256.Sum256([]byte(server.config.Workspace))))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"schema":1,"workspace":%q,"slug":"example","operation_id":%q,"stage":"Waiting for session access"}`, server.config.Workspace, operation.Options.JournalID)
	if err := os.WriteFile(filepath.Join(directory, "example.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	view := server.lifecycleOperationView("example", operation)
	if view.Progress == nil || view.Progress.Stage != "Waiting for session access" {
		t.Fatalf("waiting progress missing from display: %#v", view)
	}
	operation.Options.JournalID = strings.Repeat("b", 64)
	if view := server.lifecycleOperationView("example", operation); view.Progress != nil {
		t.Fatalf("display adopted another operation's progress: %#v", view)
	}
}
