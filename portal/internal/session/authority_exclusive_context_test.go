package session

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// The lock loop consults Done only after its nonblocking flock is contended.
// This barrier proves the waiter reached that path before release/cancellation.
type exclusiveWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *exclusiveWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

type exclusiveLockResult struct {
	lock *RuntimeLock
	err  error
}

func exclusiveTestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func waitForExclusiveContention(t *testing.T, ctx *exclusiveWaitContext, result <-chan exclusiveLockResult) {
	t.Helper()
	select {
	case <-ctx.waiting:
	case got := <-result:
		if got.lock != nil {
			got.lock.Close()
		}
		t.Fatalf("exclusive waiter returned before contention: %v", got.err)
	case <-ctx.Context.Done():
		t.Fatal("exclusive waiter did not reach flock contention", ctx.Err())
	}
}

func TestRuntimeExclusiveContextWaitsForSharedAndExclusive(t *testing.T) {
	for name, acquire := range map[string]func(string, string) (*RuntimeLock, error){
		"shared": LockRuntimeShared, "exclusive": LockRuntimeExclusive,
	} {
		t.Run(name, func(t *testing.T) {
			directory := exclusiveTestDirectory(t)
			owner, err := acquire(directory, "example")
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			budget, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ctx := &exclusiveWaitContext{Context: budget, waiting: make(chan struct{})}
			result := make(chan exclusiveLockResult, 1)
			go func() {
				lock, err := LockRuntimeExclusiveContext(ctx, directory, "example")
				result <- exclusiveLockResult{lock, err}
			}()
			waitForExclusiveContention(t, ctx, result)
			select {
			case got := <-result:
				if got.lock != nil {
					got.lock.Close()
				}
				t.Fatalf("exclusive waiter passed a held %s lock: %v", name, got.err)
			default:
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-result:
				if got.err != nil || got.lock == nil {
					t.Fatal("exclusive waiter did not acquire after release", got.err)
				}
				defer got.lock.Close()
				if shared, err := LockRuntimeShared(directory, "example"); err == nil {
					shared.Close()
					t.Fatal("exclusive context wrapper granted a shared lock")
				}
				got.lock.Close()
			case <-budget.Done():
				t.Fatal("exclusive waiter ignored release", budget.Err())
			}
			final, err := LockRuntimeExclusive(directory, "example")
			if err != nil {
				t.Fatal("exclusive waiter leaked a lock", err)
			}
			final.Close()
		})
	}
}

func TestRuntimeExclusiveContextCanceledBeforeAcquisition(t *testing.T) {
	directory := exclusiveTestDirectory(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{{"canceled", canceled, context.Canceled}, {"expired", expired, context.DeadlineExceeded}} {
		t.Run(tc.name, func(t *testing.T) {
			lock, err := LockRuntimeExclusiveContext(tc.ctx, directory, "example")
			if lock != nil {
				lock.Close()
				t.Fatal("completed context received an exclusive lock")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			probe, err := LockRuntimeExclusive(directory, "example")
			if err != nil {
				t.Fatal("completed context leaked its lock", err)
			}
			probe.Close()
		})
	}
}

func TestRuntimeExclusiveContextQueuedCancellationAndDeadlineReleaseLock(t *testing.T) {
	for name, acquire := range map[string]func(string, string) (*RuntimeLock, error){
		"shared": LockRuntimeShared, "exclusive": LockRuntimeExclusive,
	} {
		for _, deadline := range []bool{false, true} {
			kind := "cancel"
			if deadline {
				kind = "deadline"
			}
			t.Run(name+"/"+kind, func(t *testing.T) {
				directory := exclusiveTestDirectory(t)
				owner, err := acquire(directory, "example")
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
				duration := 2 * time.Second
				if deadline {
					duration = 100 * time.Millisecond
				}
				budget, cancel := context.WithTimeout(context.Background(), duration)
				defer cancel()
				ctx := &exclusiveWaitContext{Context: budget, waiting: make(chan struct{})}
				result := make(chan exclusiveLockResult, 1)
				go func() {
					lock, err := LockRuntimeExclusiveContext(ctx, directory, "example")
					result <- exclusiveLockResult{lock, err}
				}()
				waitForExclusiveContention(t, ctx, result)
				want := context.Canceled
				if deadline {
					<-budget.Done()
					want = context.DeadlineExceeded
				} else {
					cancel()
				}
				select {
				case got := <-result:
					if got.lock != nil {
						got.lock.Close()
						t.Fatal("queued completed context acquired an exclusive lock")
					}
					if !errors.Is(got.err, want) {
						t.Fatalf("error = %v, want %v", got.err, want)
					}
				case <-time.After(time.Second):
					t.Fatal("queued waiter did not return after context completion")
				}
				owner.Close()
				probe, err := LockRuntimeExclusive(directory, "example")
				if err != nil {
					t.Fatal("queued completed waiter leaked its lock", err)
				}
				probe.Close()
			})
		}
	}
}

func TestRuntimeExclusiveRemainsFailFast(t *testing.T) {
	for name, acquire := range map[string]func(string, string) (*RuntimeLock, error){
		"shared": LockRuntimeShared, "exclusive": LockRuntimeExclusive,
	} {
		t.Run(name, func(t *testing.T) {
			directory := exclusiveTestDirectory(t)
			owner, err := acquire(directory, "example")
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			result := make(chan exclusiveLockResult, 1)
			go func() {
				lock, err := LockRuntimeExclusive(directory, "example")
				result <- exclusiveLockResult{lock, err}
			}()
			select {
			case got := <-result:
				if got.lock != nil {
					got.lock.Close()
					t.Fatal("fail-fast exclusive API passed a held lock")
				}
				if got.err == nil {
					t.Fatal("fail-fast exclusive API did not refuse contention")
				}
			case <-time.After(time.Second):
				t.Fatal("fail-fast exclusive API waited for the owner to release")
			}
		})
	}
}
