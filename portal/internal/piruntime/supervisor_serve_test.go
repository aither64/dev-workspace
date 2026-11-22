package piruntime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestSupervisorServeShutdown(t *testing.T) {
	m := &Manager{config: Config{HostStateRoot: t.TempDir(), Dial: func(ctx context.Context, path string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	s := &supervisor{ref: RuntimeRef{InstanceID: newUUID()}}
	instance := m.instance(s.ref)
	if err := os.MkdirAll(instance, 0700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	check, stopCheck := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCheck()
	s.inputTasks.Add(1)
	release := sync.OnceFunc(s.inputTasks.Done)
	var serveErr error
	done := make(chan struct{})
	go func() {
		serveErr = s.serve(ctx, instance)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not exit during fixture cleanup")
		}
	})

	for {
		var got Handshake
		if err := m.callRaw(check, s.ref, "handshake", nil, &got); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("supervisor exited before serving: %v", serveErr)
		case <-check.Done():
			t.Fatal("supervisor did not answer its readiness handshake")
		case <-time.After(time.Millisecond):
		}
	}

	cancel() // Models cancellation by the signal-owned service lifetime.
	for {
		_, err := os.Lstat(filepath.Join(instance, "control.sock"))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-check.Done():
			t.Fatal("shutdown did not close the control listener")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-done:
		t.Fatalf("supervisor exited with tracked input still pending: %v", serveErr)
	case <-time.After(10 * time.Millisecond):
	}
	release()
	select {
	case <-done:
		if serveErr != nil {
			t.Fatalf("supervisor shutdown after readiness: %v", serveErr)
		}
	case <-check.Done():
		t.Fatal("supervisor did not exit after the input drained")
	}
}

func TestSupervisorServeStartupErrors(t *testing.T) {
	t.Run("invalid launch", func(t *testing.T) {
		if err := Supervise(t.Context(), "launch.json", "", "", ""); !errors.Is(err, ErrScope) {
			t.Fatalf("invalid launch: got %v, want ErrScope", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := (&supervisor{}).serve(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled startup: got %v, want context.Canceled", err)
		}
	})
	t.Run("occupied", func(t *testing.T) {
		instance := t.TempDir()
		if err := os.WriteFile(filepath.Join(instance, "control.sock"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := (&supervisor{}).serve(context.Background(), instance); !errors.Is(err, ErrHeld) {
			t.Fatalf("occupied socket on startup: got %v, want ErrHeld", err)
		}
	})
}

type cancelOnAcceptError struct {
	*net.UnixListener
	cancel context.CancelFunc
}

func (l cancelOnAcceptError) AcceptUnix() (*net.UnixConn, error) {
	conn, err := l.UnixListener.AcceptUnix()
	if err != nil {
		l.cancel() // Deliver cancellation after the independent failure, before the loop sees it.
	}
	return conn, err
}

func TestSupervisorServeAcceptErrors(t *testing.T) {
	t.Run("foreign close", func(t *testing.T) {
		listener := newTestControlListener(t)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := (&supervisor{}).acceptControl(ctx, cancelOnAcceptError{listener, cancel}); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("foreign close with cancellation: got %v, want net.ErrClosed", err)
		}
	})
	t.Run("accept timeout", func(t *testing.T) {
		listener := newTestControlListener(t)
		if err := listener.SetDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := (&supervisor{}).acceptControl(ctx, cancelOnAcceptError{listener, cancel}); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Accept timeout with cancellation: got %v, want os.ErrDeadlineExceeded", err)
		}
	})
	t.Run("lifetime deadline", func(t *testing.T) {
		listener := newTestControlListener(t)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if err := (&supervisor{}).acceptControl(ctx, listener); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lifetime deadline: got %v, want context.DeadlineExceeded", err)
		}
		if err := listener.Close(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("lifetime deadline did not close the listener: %v", err)
		}
	})
}

func newTestControlListener(t *testing.T) *net.UnixListener {
	t.Helper()
	// Use a short path independent of the subtest name and $TMPDIR.
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
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}
