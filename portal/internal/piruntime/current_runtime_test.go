package piruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type currentRuntimeNoHost struct{ t *testing.T }

func (h currentRuntimeNoHost) Start(context.Context, RuntimeRef) (HostObservation, error) {
	h.t.Error("retained reader invoked native Start")
	return HostObservation{}, ErrUnavailable
}
func (h currentRuntimeNoHost) Inspect(context.Context, RuntimeRef) (HostObservation, error) {
	h.t.Error("retained reader invoked native Inspect")
	return HostObservation{}, ErrUnavailable
}

// Build records with the owning reservation/codec, without a native supervisor,
// worker, credentials or a second runtime transport harness.
func currentRuntimeFixture(t *testing.T) (*Manager, runtimeRecord, string) {
	t.Helper()
	m, err := NewManager(Config{UserStateRoot: t.TempDir(), HostStateRoot: t.TempDir(), AuthDirectory: t.TempDir(),
		AccountBindings: []AccountBinding{{Provider: "p3-fake", AccountRef: "fake-account"}}, Host: currentRuntimeNoHost{t},
		Dial: func(context.Context, string) (net.Conn, error) {
			t.Error("retained reader dialed runtime")
			return nil, ErrUnavailable
		}})
	if err != nil {
		t.Fatal(err)
	}
	start := StartRequest{Session: agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}, BindingRevision: 1, RequestID: newUUID(), AppStorePath: "/nix/store/00000000000000000000000000000000-fixture", ProfilePath: "/owned-profile", ProfileGeneration: "1"}
	record, err := m.reserveRuntimeRecord(start, newUUID())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := stateDirectory(m.config.UserStateRoot, start.Session)
	if err != nil {
		t.Fatal(err)
	}
	return m, record, directory
}

func TestCurrentRuntimeReaderReturnsLiteralInitialAndCurrentSuccessorWithoutEffects(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial-schema1", true: "successor-schema2"}[successor], func(t *testing.T) {
			m, first, directory := currentRuntimeFixture(t)
			current := first
			var saved any = first
			if successor {
				first.Phase = "starting"
				var err error
				current, err = m.reserveRuntimeRecord(nextStart(first.Start, newUUID()), first.Runtime.InstanceID)
				if err != nil {
					t.Fatal(err)
				}
				saved = runtimeRecords{Schema: 2, Session: first.Runtime.Session, InstanceID: first.Runtime.InstanceID, CurrentRequestID: current.Runtime.RequestID, Requests: map[string]runtimeRecord{first.Runtime.RequestID: first, current.Runtime.RequestID: current}}
			}
			// A reserved current request is identity, even without native readiness.
			if current.Phase != "preparing" {
				t.Fatal(current.Phase)
			}
			path := filepath.Join(directory, "runtime.json")
			if err := writePrivate(path, saved); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			info, _ := os.Stat(path)
			// A new Manager recovers exact identity without Ensure or native calls.
			reopened, err := NewManager(m.config)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := os.OpenFile(filepath.Join(directory, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			got, err := reopened.ReadCurrentRuntime(context.Background(), current.Runtime.Session)
			if err != nil || got != current.Runtime || successor && got == first.Runtime {
				t.Fatal("not literal current ref", got, current.Runtime, err)
			}
			start, err := reopened.ReadStart(context.Background(), current.Runtime.Session)
			if err != nil || start != current.Start {
				t.Fatal("ReadStart changed", start, err)
			}
			after, err := os.ReadFile(path)
			afterInfo, statErr := os.Stat(path)
			entries, readErr := os.ReadDir(directory)
			if err != nil || statErr != nil || readErr != nil || !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) || len(entries) != 2 {
				t.Fatal("read changed retained state", err, statErr, readErr, entries)
			}
		})
	}
}

func TestCurrentRuntimeReaderPreservesReadStartMissingArtifactClassification(t *testing.T) {
	for _, artifact := range []string{"absent", "empty", "empty-lock", "lock-content", "lock-mode", "lock-symlink", "members", "ordinary-file"} {
		t.Run(artifact, func(t *testing.T) {
			m, record, directory := currentRuntimeFixture(t)
			if artifact != "absent" {
				if err := makePrivateDirectory(directory); err != nil {
					t.Fatal(err)
				}
			}
			lock := filepath.Join(directory, "runtime.lock")
			switch artifact {
			case "empty-lock", "lock-content", "lock-mode":
				var content []byte
				if artifact == "lock-content" {
					content = []byte("unfinished")
				}
				if err := os.WriteFile(lock, content, 0600); err != nil {
					t.Fatal(err)
				}
				if artifact == "lock-mode" {
					if err := os.Chmod(lock, 0644); err != nil {
						t.Fatal(err)
					}
				}
			case "lock-symlink":
				if err := os.Symlink("missing", lock); err != nil {
					t.Fatal(err)
				}
			case "members":
				if err := os.Mkdir(filepath.Join(directory, "members"), 0700); err != nil {
					t.Fatal(err)
				}
			case "ordinary-file":
				if err := os.Remove(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			want := ErrHeld
			if artifact == "absent" || artifact == "empty" || artifact == "empty-lock" {
				want = os.ErrNotExist
			}
			_, readErr := m.ReadCurrentRuntime(context.Background(), record.Runtime.Session)
			_, startErr := m.ReadStart(context.Background(), record.Runtime.Session)
			matches := errors.Is(readErr, want) && errors.Is(startErr, want)
			if artifact == "ordinary-file" {
				matches = readErr != nil && startErr != nil && !errors.Is(readErr, os.ErrNotExist) && !errors.Is(startErr, os.ErrNotExist)
			}
			if !matches || readErr.Error() != startErr.Error() {
				t.Fatal("read-port classifier diverged", artifact, readErr, startErr)
			}
			if _, err := os.Lstat(filepath.Join(directory, "runtime.json")); err == nil {
				t.Fatal("read repaired missing runtime record")
			}
			if artifact == "absent" {
				if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("read created state", err)
				}
			}
		})
	}
}

func TestCurrentRuntimeReaderRejectsMalformedAndDisconnectedCurrentHistory(t *testing.T) {
	for _, fault := range []string{"malformed", "scope", "current", "disconnected", "config", "digest", "generation", "unknown-field"} {
		t.Run(fault, func(t *testing.T) {
			m, first, directory := currentRuntimeFixture(t)
			first.Phase = "starting"
			next, err := m.reserveRuntimeRecord(nextStart(first.Start, newUUID()), first.Runtime.InstanceID)
			if err != nil {
				t.Fatal(err)
			}
			records := runtimeRecords{Schema: 2, Session: first.Runtime.Session, InstanceID: first.Runtime.InstanceID, CurrentRequestID: next.Runtime.RequestID, Requests: map[string]runtimeRecord{first.Runtime.RequestID: first, next.Runtime.RequestID: next}}
			switch fault {
			case "scope":
				records.Session.Incarnation = strings.Repeat("b", 64)
			case "current":
				records.CurrentRequestID = newUUID()
			case "disconnected":
				root := next.Start
				root.ReplacesRequestID = ""
				next, err = m.reserveRuntimeRecord(root, first.Runtime.InstanceID)
				if err != nil {
					t.Fatal(err)
				}
			case "config":
				m.config.AuthDirectory += "-different"
			case "digest":
				next.Runtime.Digest = strings.Repeat("e", 64)
			case "generation":
				next.Runtime.Generation = first.Runtime.Generation
			}
			records.Requests[next.Runtime.RequestID] = next
			data, err := json.Marshal(records)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "malformed" {
				data = []byte(`{"Schema":`)
			} else if fault == "unknown-field" {
				data = append([]byte(`{"Unexpected":true,`), data[1:]...)
			}
			path := filepath.Join(directory, "runtime.json")
			if err := writePrivateBytes(path, data); err != nil {
				t.Fatal(err)
			}
			_, readErr := m.ReadCurrentRuntime(context.Background(), first.Runtime.Session)
			_, startErr := m.ReadStart(context.Background(), first.Runtime.Session)
			if readErr == nil || startErr == nil || errors.Is(readErr, os.ErrNotExist) || readErr.Error() != startErr.Error() {
				t.Fatal("invalid current history returned or classified absent", readErr, startErr)
			}
			after, _ := os.ReadFile(path)
			entries, err := os.ReadDir(directory)
			if !bytes.Equal(data, after) || err != nil || len(entries) != 1 {
				t.Fatal("read changed damaged record", err, entries)
			}
		})
	}
}

func TestCurrentRuntimeReaderCanceledReadHasNoEffects(t *testing.T) {
	m, record, directory := currentRuntimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.ReadCurrentRuntime(ctx, record.Runtime.Session); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := m.ReadStart(ctx, record.Runtime.Session); !errors.Is(err, context.Canceled) {
		t.Fatal("ReadStart cancellation changed", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled read created state", err)
	}
}

func captureUnavailableDiagnostic(t *testing.T, call func()) string {
	t.Helper()
	var output bytes.Buffer
	writer, prefix, flags := log.Writer(), log.Prefix(), log.Flags()
	log.SetOutput(&output)
	log.SetPrefix("")
	log.SetFlags(0)
	defer func() { log.SetOutput(writer); log.SetPrefix(prefix); log.SetFlags(flags) }()
	call()
	return output.String()
}

func TestNativeHostInspectUnavailableDiagnostic(t *testing.T) {
	ref := RuntimeRef{InstanceID: "11111111-1111-4111-8111-111111111111", RequestID: "22222222-2222-4222-8222-222222222222"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output := captureUnavailableDiagnostic(t, func() {
		result, err := (nativeHost{path: "/never-start-canceled-helper"}).request(ctx, "inspect", ref)
		if err != ErrUnavailable || !errors.Is(err, ErrUnavailable) || err.Error() != "Pi runtime operation unavailable" || !reflect.DeepEqual(result, HostObservation{}) {
			t.Fatalf("original canceled inspect refusal changed: %#v %v", result, err)
		}
	})
	t.Logf("OLD_BOUNDARY canceled original request; no helper start; same sentinel; diagnostic=%q", output)
	if strings.Count(output, "pi_runtime_unavailable stage=host_inspect_command") != 1 {
		t.Fatalf("expected one host inspect diagnostic, got %q", output)
	}
	for _, field := range []string{"instance=" + ref.InstanceID, "request=" + ref.RequestID, "context=canceled", "process=context"} {
		if !strings.Contains(output, field) {
			t.Fatal("missing bounded field", field, output)
		}
	}
	if len(output) > 512 || strings.Count(output, "\n") != 1 || strings.Contains(output, "never-start") {
		t.Fatal("unbounded or unsafe diagnostic", output)
	}
	testHostInspectDiagnosticDetails(t, ref)
}

type unavailableFailingWriter struct {
	calls      int
	panicWrite bool
}

func (w *unavailableFailingWriter) Write([]byte) (int, error) {
	w.calls++
	if w.panicWrite {
		panic("SECRET_WRITER")
	}
	return 0, errors.New("SECRET_WRITER")
}

func testHostInspectDiagnosticDetails(t *testing.T, ref RuntimeRef) {
	marker := "SECRET_HOST_" + strings.Repeat("x", 1024) + "\nforged=1"
	exitStatus, signalStatus := syscall.WaitStatus(7<<8), syscall.WaitStatus(syscall.SIGTERM)
	for _, test := range []struct {
		name        string
		ctxErr, err error
		status      *syscall.WaitStatus
		fields      []string
	}{
		{"synthetic_exit", nil, &exec.ExitError{Stderr: []byte(marker)}, &exitStatus, []string{"context=none", "process=exit", "exit=7"}},
		{"synthetic_signal", context.Canceled, &exec.ExitError{Stderr: []byte(marker)}, &signalStatus, []string{"context=canceled", "process=signal", "signal=15"}},
		{"synthetic_start_or_io", nil, &os.PathError{Op: marker, Path: marker, Err: syscall.EACCES}, nil, []string{"context=none", "process=start_or_io"}},
		{"synthetic_wrapped_context", context.DeadlineExceeded, fmt.Errorf("%s: %w", marker, context.DeadlineExceeded), nil, []string{"context=deadline", "process=context"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			hostile := ref
			hostile.InstanceID = marker
			hostile.RequestID = marker
			line := hostInspectUnavailableLine(hostile, test.ctxErr, test.err, test.status)
			for _, field := range append(test.fields, "instance=invalid", "request=invalid") {
				if !strings.Contains(line, field) {
					t.Fatal("missing safe field", field, line)
				}
			}
			if strings.Contains(line, "SECRET") || strings.Contains(line, "forged") || strings.ContainsAny(line, "\n\r") || len(line)+1 > 512 {
				t.Fatal("unsafe/unbounded classification", line)
			}
		})
	}
	t.Run("actual_expired_context", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		line := captureUnavailableDiagnostic(t, func() {
			_, err := (nativeHost{path: marker}).request(ctx, "inspect", ref)
			if err != ErrUnavailable || !errors.Is(err, ErrUnavailable) {
				t.Fatal("deadline changed refusal", err)
			}
		})
		if !strings.Contains(line, "context=deadline process=context") || strings.Contains(line, "SECRET") {
			t.Fatal(line)
		}
	})
	t.Run("actual_start_failure_silent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		line := captureUnavailableDiagnostic(t, func() {
			_, err := (nativeHost{path: marker}).request(ctx, "start", ref)
			if err != ErrUnavailable {
				t.Fatal(err)
			}
		})
		if line != "" {
			t.Fatal("non-inspect command logged", line)
		}
	})
	for _, panicWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("actual_writer_failure_panic_%t", panicWrite), func(t *testing.T) {
			writer := &unavailableFailingWriter{panicWrite: panicWrite}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			captureUnavailableDiagnostic(t, func() {
				log.SetOutput(writer)
				_, err := (nativeHost{path: marker}).request(ctx, "inspect", ref)
				if err != ErrUnavailable || !errors.Is(err, ErrUnavailable) || err.Error() != "Pi runtime operation unavailable" {
					t.Fatal("writer changed original refusal", err)
				}
			})
			if writer.calls != 1 {
				t.Fatal("writer retried", writer.calls)
			}
		})
	}
}
