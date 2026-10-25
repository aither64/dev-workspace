package web

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStartupCancellationPreservesAcceptedAttachments(t *testing.T) {
	server := newTestServer(t)
	scope, file := preparationReadyFile(t, server)
	record := fixturePreparation(t, server, 1, "paused")
	record.Snapshot.Input.UploadScope = scope.ID
	record.Snapshot.Input.Attachments = []string{file.ID}
	goal, err := server.uploadStore.ClaimPreparation(context.Background(), record.RequestID, scope.ID, record.Snapshot.Input.RawPrompt, []string{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	record.Snapshot.Goal = goal
	record.InputDigest = inputPreparationDigest(record.Snapshot.Input)
	record.SnapshotDigest = preparationDigest(record.Snapshot)
	if err := server.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	path := server.preparationPath(record.RequestID, false)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenFile(filepath.Join(server.uploadStore.Directory, "catalog.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	// Reconciliation reaches the accepted attachment claim after transition
	// admission. Cancellation must not turn catalog contention into lost ownership.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := server.reconcilePreparations(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("attachment claim cancellation = %v, record state %s", err, server.preparations[record.RequestID].State)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("accepted preparation changed on cancellation: %v", err)
	}
	// The constructor's earlier upload-retention wait must likewise return
	// cancellation without publishing an application or modifying the record.
	startup, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	application, err := NewContext(startup, server.config)
	if application != nil {
		application.Close()
		t.Fatal("canceled constructor published an application")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("constructor cancellation = %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("constructor changed accepted preparation: %v", err)
	}
}
