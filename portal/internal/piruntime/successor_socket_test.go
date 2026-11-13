package piruntime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func ownedSocketFixture(t *testing.T, stale bool) (*os.File, string) {
	t.Helper()
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	path := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Cleanup(func() { _ = listener.Close() })
	}
	return directory, path
}

func TestSuccessorSocketReclaimsOnlyRefusedOwnedEntryAndBindsOnce(t *testing.T) {
	directory, path := ownedSocketFixture(t, true)
	other := filepath.Join(filepath.Dir(path), "unrelated")
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareControlSocket(context.Background(), directory, false, nil); !errors.Is(err, ErrHeld) {
		t.Fatal("initial launch removed socket", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("initial launch changed socket", err)
	}
	if err := prepareControlSocket(context.Background(), directory, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale entry retained", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal("single successor bind", err)
	}
	defer listener.Close()
	if data, err := os.ReadFile(other); err != nil || string(data) != "keep" {
		t.Fatal("unrelated entry changed", err)
	}
}

func TestSuccessorSocketRefusesLiveChangedAndAmbiguousEntries(t *testing.T) {
	directory, path := ownedSocketFixture(t, false)
	before, _ := os.Lstat(path)
	if err := prepareControlSocket(context.Background(), directory, true, nil); !errors.Is(err, ErrHeld) {
		t.Fatal("live listener replaced", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("live listener changed", err)
	}
	for _, outcome := range []error{nil, context.DeadlineExceeded, syscall.EACCES, syscall.ECONNRESET} {
		directory, path := ownedSocketFixture(t, true)
		calls := 0
		err := prepareControlSocket(context.Background(), directory, true, func(ctx context.Context, _ string) error {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 500*time.Millisecond {
				t.Fatal("unbounded socket probe")
			}
			return outcome
		})
		if err == nil || calls != 1 {
			t.Fatal("ambiguous probe admitted", outcome, err, calls)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("ambiguous socket removed", err)
		}
	}
	directory, path = ownedSocketFixture(t, true)
	err = prepareControlSocket(context.Background(), directory, true, func(context.Context, string) error {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		return syscall.ECONNREFUSED
	})
	if !errors.Is(err, ErrHeld) {
		t.Fatal("changed entry replaced", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatal("replacement removed", err)
	}
}

type foreignSocketInfo struct {
	os.FileInfo
	owner syscall.Stat_t
}

func (i foreignSocketInfo) Sys() any { return &i.owner }

func TestSuccessorSocketRefusesWrongKindOwnerAndAcceptsAbsence(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	path := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), "control.sock")
	if err := prepareControlSocket(context.Background(), directory, false, nil); err != nil {
		t.Fatal("absent initial entry", err)
	}
	if err := os.WriteFile(path, []byte("ordinary file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareControlSocket(context.Background(), directory, true, nil); !errors.Is(err, ErrScope) {
		t.Fatal("non-socket replaced", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	if err := prepareControlSocket(context.Background(), directory, true, nil); !errors.Is(err, ErrScope) {
		t.Fatal("symlink replaced", err)
	}
	_, socketPath := ownedSocketFixture(t, true)
	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	owner := *info.Sys().(*syscall.Stat_t)
	owner.Uid++
	if _, err := identifyControlSocket(foreignSocketInfo{FileInfo: info, owner: owner}); !errors.Is(err, ErrScope) {
		t.Fatal("foreign socket accepted", err)
	}
}

func TestSuccessorStartupRequiresCurrentSchema2Lineage(t *testing.T) {
	m, h, start, first, ctx := successorFixture(t)
	h.finish(t, first.Runtime)
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	second, err := m.Ensure(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	s := h.base.s
	instance := m.instance(second.Runtime)
	if err := s.validateSuccessorStartup(instance); err != nil {
		t.Fatal("valid current successor", err)
	}
	s.handshake.MainPID++
	if err := s.validateSuccessorStartup(instance); !errors.Is(err, ErrScope) {
		t.Fatal("wrong startup process", err)
	}
	s.handshake.MainPID--
	records, err := m.records(start.Session)
	if err != nil {
		t.Fatal(err)
	}
	records.CurrentRequestID = start.RequestID
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	if err := writePrivate(filepath.Join(directory, "runtime.json"), records); err != nil {
		t.Fatal(err)
	}
	if err := s.validateSuccessorStartup(instance); err == nil {
		t.Fatal("wrong current lineage accepted")
	}
}
