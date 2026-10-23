package session

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRuntimeSharedContextWaitCancelAndRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	exclusive, err := LockRuntimeExclusive(dir, "example")
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	if shared, err := LockRuntimeShared(dir, "example"); err == nil {
		shared.Close()
		t.Fatal("fail-fast API stopped refusing exclusive owner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if shared, err := LockRuntimeSharedContext(ctx, dir, "example"); !errors.Is(err, context.DeadlineExceeded) {
		if shared != nil {
			shared.Close()
		}
		t.Fatal("wait ignored its deadline", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		shared, err := LockRuntimeSharedContext(ctx, dir, "example")
		if shared != nil {
			shared.Close()
		}
		result <- err
	}()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	exclusive.Close()
	shared, err := LockRuntimeSharedContext(context.Background(), dir, "example")
	if err != nil {
		t.Fatal(err)
	}
	shared.Close()
	final, err := LockRuntimeExclusive(dir, "example")
	if err != nil {
		t.Fatal("cancelled waiter leaked its descriptor lock", err)
	}
	final.Close()
}
