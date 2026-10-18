package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestProviderPreparationObservesCancellationWithoutNativeActivity(t *testing.T) {
	directory := t.TempDir()
	operation := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	path := filepath.Join(directory, "state.json")
	state := map[string]any{"operation": map[string]any{"id": operation, "strategy": "scoped-v1", "phase": "applying"}}
	if err := providerAtomicJSON(path, state); err != nil {
		t.Fatal(err)
	}
	ctx, stop := providerPreparationContext(context.Background(), filepath.Join(directory, operation+".snapshot.json"), operation)
	defer stop()
	if ctx.Err() != nil {
		t.Fatal("preparation canceled before the request")
	}
	if err := providerAtomicJSON(filepath.Join(directory, operation+".control.json"), map[string]any{"operation": operation, "cancelRequested": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("blocked preparation did not observe cancellation")
	}
	if context.Cause(ctx) == nil || context.Cause(ctx).Error() != "provider preparation canceled" {
		t.Fatalf("wrong cancellation: %v", context.Cause(ctx))
	}
}
