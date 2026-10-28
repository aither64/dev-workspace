package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestTrackingDirectorySharesSlotsAndDeadline(t *testing.T) {
	workspace := t.TempDir()
	summary := &session.Summary{Manifest: session.Manifest{Slug: "files"}, Workspace: workspace, Root: "work"}
	if err := os.MkdirAll(filepath.Join(workspace, "work", "files"), 0755); err != nil {
		t.Fatal(err)
	}
	reader := ReviewReader{Workspace: workspace, Jobs: make(chan struct{}, 4), Timeout: 10 * time.Millisecond}
	for i := 0; i < cap(reader.Jobs); i++ {
		reader.Jobs <- struct{}{}
	}
	if _, err := reader.TrackingDirectory(context.Background(), summary, "", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting slot deadline: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.TrackingDirectory(ctx, summary, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting slot cancellation: %v", err)
	}
	for len(reader.Jobs) > 0 {
		<-reader.Jobs
	}
	if page, err := reader.TrackingDirectory(context.Background(), summary, "", ""); err != nil || page.Entries == nil || len(reader.Jobs) != 0 {
		t.Fatalf("successful slot release: %#v %v", page, err)
	}
	if _, err := reader.TrackingDirectory(context.Background(), summary, "missing", ""); err == nil || len(reader.Jobs) != 0 {
		t.Fatalf("failed slot release: %v", err)
	}
	if _, err := reader.TrackingDirectory(context.Background(), &session.Summary{}, "", ""); err == nil {
		t.Fatal("accepted foreign root")
	}
}
