package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

type initialRecoveryHost struct {
	m                                *Manager
	attempts, nativeStarts, inspects int
	ref                              RuntimeRef
	observation                      HostObservation
	inspectErr                       error
}

func (h *initialRecoveryHost) Start(_ context.Context, ref RuntimeRef) (HostObservation, error) {
	h.attempts++
	if h.attempts == 1 {
		return HostObservation{}, ErrUnavailable
	} // sudo refused before helper intent
	if ref != h.ref {
		return HostObservation{}, ErrScope
	}
	// The helper must be able to take its own launch lock.
	f, err := os.OpenFile(filepath.Join(h.m.instance(ref), "launch.lock"), os.O_RDWR, 0)
	if err != nil {
		return HostObservation{}, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return HostObservation{}, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	h.nativeStarts++
	r, _, err := h.m.recordFor(ref)
	if err != nil {
		return HostObservation{}, err
	}
	h.observation = HostObservation{LaunchRequest: launchFor(r.Start, ref), Digest: ref.Digest, Outcome: "starting", Phase: "starting"}
	h.inspectErr = nil
	return h.observation, nil
}
func (h *initialRecoveryHost) Inspect(_ context.Context, ref RuntimeRef) (HostObservation, error) {
	h.inspects++
	if ref != h.ref {
		return HostObservation{}, ErrScope
	}
	return h.observation, h.inspectErr
}

func initialRecoveryFixture(t *testing.T) (*Manager, *initialRecoveryHost, runtimeRecord, map[string][]byte) {
	t.Helper()
	m, reserved, directory := currentRuntimeFixture(t)
	h := &initialRecoveryHost{m: m, inspectErr: ErrHostIntentAbsent}
	m.config.Host, m.config.retainPackage = h, func(context.Context, string, string) error { return nil }
	// The pure Manager fixture has no installed application; strict real A/B
	// metadata admission is exercised separately with immutable package inputs.
	m.config.validatePackage = func(string) error { return nil }
	status, err := m.Ensure(context.Background(), reserved.Start)
	if !errors.Is(err, ErrUnavailable) || h.attempts != 1 || h.nativeStarts != 0 {
		t.Fatal(status, err, h)
	}
	h.ref = status.Runtime
	r, err := m.record(reserved.Start.Session)
	if err != nil || r.Phase != "starting" {
		t.Fatal(r, err)
	}
	files := map[string][]byte{}
	for _, path := range []string{filepath.Join(directory, "runtime.json"), filepath.Join(m.instance(h.ref), "launch.json"), filepath.Join(m.instance(h.ref), "requests", h.ref.RequestID, "runtime-config.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = data
	}
	return m, h, r, files
}

func TestResumeInitialStartRetainsFrozenOriginalAndOrdinaryReadsNeverStart(t *testing.T) {
	m, h, r, files := initialRecoveryFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := m.Ensure(context.Background(), r.Start); !errors.Is(err, ErrHostIntentAbsent) {
			t.Fatal(err)
		}
	}
	if h.attempts != 1 {
		t.Fatal("ordinary Ensure restarted", h.attempts)
	}
	reopened, err := NewManager(m.config)
	if err != nil {
		t.Fatal(err)
	}
	status, err := reopened.ResumeInitialStart(context.Background(), r.Start)
	if err != nil || status.Runtime != r.Runtime || status.Phase != "starting" || h.attempts != 2 || h.nativeStarts != 1 {
		t.Fatal(status, err, h)
	}
	for i := 0; i < 2; i++ {
		if _, err := reopened.ResumeInitialStart(context.Background(), r.Start); err != nil {
			t.Fatal(err)
		}
	}
	if h.attempts != 2 || h.nativeStarts != 1 {
		t.Fatal("existing intent restarted", h)
	}
	for p, before := range files {
		after, _ := os.ReadFile(p)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("frozen input changed", p)
		}
	}
}

func TestResumeInitialStartRefusesUncertainIntentOrChangedFrozenInputs(t *testing.T) {
	for _, fault := range []string{"unavailable", "cancel", "launch", "config", "missing", "symlink", "mode", "root", "start", "preparing", "history", "missing-lock", "lock-symlink", "config-parent-symlink", "package-refusal"} {
		t.Run(fault, func(t *testing.T) {
			m, h, r, files := initialRecoveryFixture(t)
			ctx := context.Background()
			start := r.Start
			launch := filepath.Join(m.instance(h.ref), "launch.json")
			switch fault {
			case "unavailable":
				h.inspectErr = ErrUnavailable
			case "package-refusal":
				m.config.validatePackage = func(string) error { return ErrUnavailable }
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "missing-lock":
				d, _ := stateDirectory(m.config.UserStateRoot, start.Session)
				if err := os.Remove(filepath.Join(d, "runtime.lock")); err != nil {
					t.Fatal(err)
				}
			case "lock-symlink":
				d, _ := stateDirectory(m.config.UserStateRoot, start.Session)
				p := filepath.Join(d, "runtime.lock")
				if err := os.Rename(p, p+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p+"-saved", p); err != nil {
					t.Fatal(err)
				}
			case "config-parent-symlink":
				p := filepath.Join(m.instance(h.ref), "requests", h.ref.RequestID)
				if err := os.Rename(p, p+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p+"-saved", p); err != nil {
					t.Fatal(err)
				}
			case "launch":
				os.WriteFile(launch, append(files[launch], ' '), 0600)
			case "config":
				os.WriteFile(filepath.Join(m.instance(h.ref), "requests", h.ref.RequestID, "runtime-config.json"), []byte("{}"), 0600)
			case "missing":
				os.Remove(launch)
			case "symlink":
				os.Rename(launch, launch+"-saved")
				os.Symlink(launch+"-saved", launch)
			case "mode":
				os.Chmod(launch, 0644)
			case "root":
				p := filepath.Join(m.instance(h.ref), "package-root")
				os.Remove(p)
				os.Symlink(start.AppStorePath+"-other", p)
			case "start":
				start.ProfileGeneration += "-changed"
			case "preparing":
				r.Phase = "preparing"
				d, _ := stateDirectory(m.config.UserStateRoot, start.Session)
				writePrivate(filepath.Join(d, "runtime.json"), r)
			case "history":
				d, _ := stateDirectory(m.config.UserStateRoot, start.Session)
				writePrivate(filepath.Join(d, "runtime.json"), runtimeRecords{Schema: 2, Session: start.Session, InstanceID: r.Runtime.InstanceID, CurrentRequestID: r.Runtime.RequestID, Requests: map[string]runtimeRecord{r.Runtime.RequestID: r}})
			}
			if _, err := m.ResumeInitialStart(ctx, start); err == nil {
				t.Fatal("fault admitted", fault)
			}
			if h.attempts != 1 || h.nativeStarts != 0 {
				t.Fatal("refusal started native", h)
			}
			if fault == "missing-lock" {
				d, _ := stateDirectory(m.config.UserStateRoot, start.Session)
				if _, err := os.Lstat(filepath.Join(d, "runtime.lock")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("resume repaired missing lock", err)
				}
			}
		})
	}
}
