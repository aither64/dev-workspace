package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestLifecycleCodexTargetsStrictShapeAndExactRevisionEvidence(t *testing.T) {
	good := `{"incarnation":"` + strings.Repeat("a", 64) + `","revision":18446744073709551614,"threadId":"current-root","members":[{"member":"implementer0","originalThreadId":"origin-member","threadId":"current-member"}]}`
	targets, err := DecodeCodexTargets([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if targets.Revision != ^uint64(0)-1 {
		t.Fatal("revision lost integer precision", targets.Revision)
	}
	copy := targets.Clone()
	*copy.Members[0].ThreadID = "other"
	if *targets.Members[0].ThreadID != "current-member" || SameCodexTargets(&targets, copy) {
		t.Fatal("capture aliases its copy")
	}
	for _, bad := range []string{"null", `{}`, strings.Replace(good, `"revision":18446744073709551614`, `"revision":1.0`, 1), strings.Replace(good, `"members":[`, `"members":null,"ignored":[`, 1), strings.Replace(good, `"threadId":"current-root"`, `"threadId":""`, 1), strings.Replace(good, `"originalThreadId":"origin-member",`, "", 1), strings.Replace(good, `"threadId":"current-member"`, `"threadId":"current-root"`, 1), strings.Replace(good, `"revision":18446744073709551614`, `"revision":2,"revision":3`, 1)} {
		if _, err := DecodeCodexTargets([]byte(bad)); err == nil {
			t.Fatal("malformed capture admitted", bad)
		}
	}
	unstarted := strings.Replace(strings.Replace(good, `"originalThreadId":"origin-member"`, `"originalThreadId":null`, 1), `"threadId":"current-member"`, `"threadId":null`, 1)
	nullTargets, err := DecodeCodexTargets([]byte(unstarted))
	if err != nil || nullTargets.Validate(false) == nil {
		t.Fatal("unstarted tuple acquired ordinary target permission", err)
	}
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "bound.archive.json")
	write := func(value CodexTargets, phase string) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"schema": 2, "workspace": workspace, "slug": "bound", "operation_id": strings.Repeat("b", 64), "phase": phase, "mode": "complete", "retained_thread_id": "origin-root", "codex_targets": value})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(targets, "prepared")
	before, err := PendingLifecycleProgress(workspace, "bound")
	if err != nil {
		t.Fatal(err)
	}
	write(targets, "quiesced")
	after, err := PendingLifecycleProgress(workspace, "bound")
	if err != nil || before.Evidence != after.Evidence || !SameCodexTargets(before.CodexTargets, after.CodexTargets) {
		t.Fatal("phase altered capture evidence", err)
	}
	targets.Revision++
	write(targets, "quiesced")
	changed, err := PendingLifecycleProgress(workspace, "bound")
	if err != nil || changed.Evidence == after.Evidence {
		t.Fatal("adjacent uint64 revisions shared evidence", err)
	}
}

func TestLifecycleNullCaptureHasOnlyItsOwnedDeletionProjection(t *testing.T) {
	id := strings.Repeat("b", 64)
	capture := `{"incarnation":"` + strings.Repeat("a", 64) + `","revision":18446744073709551615,"threadId":"native-root","members":[{"member":"implementer0","originalThreadId":null,"threadId":null}]}`
	valid := `{"operation_id":"` + id + `","codex_targets":` + capture + `}`
	command, err := DecodeLifecycleCommandInput([]byte(valid))
	if err != nil || command.OperationID != id || command.Targets == nil || command.Targets.Revision != ^uint64(0) || command.Targets.Validate(false) == nil {
		t.Fatal("null command lost deletion-only capture", command, err)
	}
	absent, err := DecodeLifecycleCommandInput([]byte(`{"operation_id":"` + id + `"}`))
	if err != nil || absent.Targets != nil {
		t.Fatal("accepted absence gained a capture", absent, err)
	}
	for _, bad := range []string{
		`null`, `[]`, `{}`, `{"operation_id":null}`, `{"operation_id":"` + strings.ToUpper(id) + `"}`,
		strings.Replace(valid, capture, "null", 1), strings.Replace(valid, `"threadId":null`, `"threadId":"member"`, 1),
		strings.Replace(valid, `"originalThreadId":null,`, "", 1), strings.Replace(valid, `"member":`, `"member":"other","member":`, 1),
		strings.Replace(valid, `"operation_id":`, `"operation_id":"`+id+`","operation_id":`, 1),
		valid + `{}`, valid + strings.Repeat(" ", 64<<10), valid + string([]byte{0xff}),
		`{"operation_id":"` + id + `","other":null}`, `{"operation_id":"` + id + `","other":{"codex_targets":` + capture + `}}`,
		`{"operation_id":"` + id + `","other":` + strings.Repeat("[", 66) + `1` + strings.Repeat("]", 66) + `}`,
	} {
		if _, err := DecodeLifecycleCommandInput([]byte(bad)); err == nil {
			t.Fatal("invalid command gained a null exception", bad[:min(len(bad), 200)])
		}
	}
	workspace := t.TempDir()
	locks := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(locks, 0700); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"delete", "archive", "revive"} {
		suffix := operation
		if operation == "delete" {
			suffix = "removal"
		}
		path := filepath.Join(locks, "example."+suffix+".json")
		body := map[string]any{"schema": 1, "workspace": workspace, "slug": "example", "operation_id": id, "phase": "prepared", "force": true, "codex_targets": command.Targets}
		if operation == "archive" {
			body["mode"] = "archived"
		}
		data, _ := json.Marshal(body)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		progress, err := PendingLifecycleProgress(workspace, "example")
		if operation == "delete" {
			if err != nil || progress == nil || !SameCodexTargets(progress.CodexTargets, command.Targets) {
				t.Fatal("delete progress lost null tuple", progress, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "no exact paired identity") {
			t.Fatal("nondeletion progress accepted null tuple", operation, progress)
		}
		if after, err := os.ReadFile(path); err != nil || string(after) != string(data) {
			t.Fatal("progress projection mutated its owner", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{"ordinary":null}`, `{"ordinary":{"codex_targets":` + capture + `}}`, `{"pi":{},"codex_targets":` + capture + `}`} {
		if _, _, err := decodeLifecycleObject([]byte(body), 64<<10); err == nil {
			t.Fatal("record gained an unrelated null exception", body)
		}
	}
}

func TestPendingLifecycleRecognizesOneOwningOperation(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJournal := func(suffix string) {
		t.Helper()
		path := filepath.Join(root, "example"+suffix)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeJournal(".revive.json")
	operation, err := PendingLifecycle(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if operation != "revive" {
		t.Fatalf("expected revive, got %q", operation)
	}
}

func TestPendingLifecycleProgressReadsTheValidatedJournalPhase(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "example.archive.json")
	journalID := strings.Repeat("a", 64)
	payload := `{"schema":2,"slug":"example","workspace":"` + workspace + `","phase":"clusters_released","mode":"complete","operation_id":"` + journalID + `"}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	progress, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Operation != "archive" || progress.Phase != "clusters_released" ||
		progress.Mode != "complete" || progress.JournalID != journalID || progress.UpdatedAt.IsZero() {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestPendingLifecycleProgressReadsReviveOperationIdentity(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	journalID := strings.Repeat("b", 64)
	payload := `{"schema":3,"slug":"example","workspace":"` + workspace +
		`","phase":"prepared","operation_id":"` + journalID + `"}`
	if err := os.WriteFile(
		filepath.Join(root, "example.revive.json"), []byte(payload), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	progress, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Operation != "revive" || progress.Phase != "prepared" ||
		progress.JournalID != journalID {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestPendingLifecycleProgressBindsImmutableEvidenceAcrossPhaseWrites(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "example.archive.json")
	write := func(phase, thread string) {
		t.Helper()
		payload := fmt.Sprintf(`{"schema":2,"slug":"example","workspace":%q,"operation_id":%q,"mode":"complete","phase":%q,"retained_thread_id":%s,"target_tracking_sha256":%q}`,
			workspace, strings.Repeat("a", 64), phase, thread, strings.Repeat("b", 64))
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("prepared", `"root-a"`)
	before, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	write("tracking_committed", `"root-a"`)
	after, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if before.Evidence == "" || before.Evidence != after.Evidence || !after.RootRecorded || after.RetainedThreadID == nil || *after.RetainedThreadID != "root-a" {
		t.Fatalf("phase write changed journal ownership: %#v, %#v", before, after)
	}
	write("tracking_committed", `"root-b"`)
	changed, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Evidence == after.Evidence {
		t.Fatal("root change retained journal evidence")
	}
	write("tracking_committed", `null`)
	absent, err := PendingLifecycleProgress(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !absent.RootRecorded || absent.RetainedThreadID != nil {
		t.Fatalf("root absence = %#v", absent)
	}
	write("tracking_committed", `{}`)
	if _, err := PendingLifecycleProgress(workspace, "example"); err == nil {
		t.Fatal("malformed retained root was accepted")
	}
}

func TestPendingLifecycleProgressToleratesAtomicJournalReplacement(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "example.archive.json")
	payload := []byte(`{"schema":2,"slug":"example","workspace":"` + workspace + `","phase":"prepared","mode":"complete","operation_id":"` + strings.Repeat("a", 64) + `"}`)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	errorsSeen := make(chan error, 4)
	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				progress, err := PendingLifecycleProgress(workspace, "example")
				if err != nil {
					errorsSeen <- err
					return
				}
				if progress != nil && progress.Operation != "archive" {
					errorsSeen <- errors.New("wrong lifecycle operation")
					return
				}
			}
		}()
	}
	for index := range 500 {
		temporary := filepath.Join(root, fmt.Sprintf("journal-%d.tmp", index))
		if err := os.WriteFile(temporary, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, path); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	readers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
}

func TestPendingLifecyclesIncludesJournalOnlyDeletion(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `{"schema":1,"slug":"example","workspace":"` + workspace + `","phase":"tracking_preserved","force":false,"operation_id":"` + strings.Repeat("a", 64) + `"}`
	if err := os.WriteFile(filepath.Join(root, "example.removal.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	pending, err := PendingLifecycles(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if pending["example"].Operation != "delete" || pending["example"].Phase != "tracking_preserved" ||
		pending["example"].JournalID != strings.Repeat("a", 64) {
		t.Fatalf("pending lifecycles = %#v", pending)
	}
}

func TestPendingLifecycleProgressRejectsAnInvalidOperationIdentity(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `{"schema":1,"slug":"example","workspace":"` + workspace + `","phase":"prepared","force":false,"operation_id":"NOT-AN-ID"}`
	if err := os.WriteFile(filepath.Join(root, "example.removal.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingLifecycleProgress(workspace, "example"); err == nil ||
		!strings.Contains(err.Error(), "operation identity") {
		t.Fatalf("invalid operation identity error = %v", err)
	}
}

func TestPendingLifecycleProgressRejectsFIFOWithoutBlocking(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "example.removal.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err := PendingLifecycleProgress(workspace, "example")
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("FIFO lifecycle journal error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO lifecycle journal blocked for %s", elapsed)
	}
}

func TestPendingLifecycleProgressRejectsAnInvalidOrForeignPhase(t *testing.T) {
	for _, payload := range []string{
		`{"slug":"other","workspace":"WORKSPACE","phase":"prepared","mode":"complete","operation_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"slug":"example","workspace":"WORKSPACE","phase":"invented","mode":"complete","operation_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"slug":"example","workspace":"WORKSPACE","phase":"prepared","mode":"invented","operation_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	} {
		workspace := t.TempDir()
		root := filepath.Join(workspace, "worktrees", ".locks")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		payload = strings.ReplaceAll(payload, "WORKSPACE", workspace)
		if err := os.WriteFile(filepath.Join(root, "example.archive.json"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := PendingLifecycleProgress(workspace, "example"); err == nil {
			t.Fatal("invalid progress was accepted")
		}
	}
}

func TestPendingLifecycleFailsClosedForConflictingOrUnsafeState(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".archive.json", ".removal.json"} {
		if err := os.WriteFile(filepath.Join(root, "example"+suffix), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PendingLifecycle(workspace, "example"); err == nil {
		t.Fatal("expected conflicting journals to fail closed")
	}

	if err := os.Remove(filepath.Join(root, "example.removal.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "example.archive.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingLifecycle(workspace, "example"); err == nil {
		t.Fatal("expected an unsafe journal to fail closed")
	}
}
