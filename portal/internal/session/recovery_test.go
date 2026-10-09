package session

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverySurvivesRuntimeLossWithoutExecutionPermission(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "codex.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	store := RecoveryStore{Root: filepath.Join(root, "state"), Workspace: filepath.Join(root, "workspace")}
	ctx := context.Background()
	if err := store.Set(ctx, "example", "root", socket, true, true); err != nil {
		t.Fatal(err)
	}
	old, err := store.Load("example")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := SocketIdentity(socket)
	if err != nil || !old.Active("root", epoch) {
		t.Fatal("live root lost permission")
	}
	listener.Close()
	listener, err = net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	epoch, err = SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	if old.Active("root", epoch) || !old.Automatic {
		t.Fatal("restart either executed work or lost restoration intent")
	}
	if err := store.Set(ctx, "example", "root", socket, true, false); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Load("example")
	if err != nil || restored.Active("root", epoch) {
		t.Fatal("runtime restore activated Codex")
	}
	if err := store.Set(ctx, "example", "other-root", socket, true, false); err == nil {
		t.Fatal("recovery replaced the exact root")
	}
	if err := store.Set(ctx, "example", "root", socket, false, false); err != nil {
		t.Fatal(err)
	}
	stopped, err := store.Load("example")
	if err != nil || stopped.Automatic {
		t.Fatal("stop retained automatic restoration")
	}
}

func TestRecoveryPortalRestartPreservesActiveRootAndMemberHold(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "codex.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	store := RecoveryStore{Root: filepath.Join(root, "state"), Workspace: root}
	if err := store.Set(context.Background(), "example", "root", socket, true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "example", "root", socket, true, false); err != nil {
		t.Fatal(err)
	}
	record, err := store.Load("example")
	if err != nil {
		t.Fatal(err)
	}
	epoch, _ := SocketIdentity(socket)
	if !record.Active("root", epoch) || record.Active("member", epoch) {
		t.Fatal("portal restart changed execution permissions")
	}
	if err := store.Retire(context.Background(), "example", "root", socket, false); err != nil {
		t.Fatal(err)
	}
	record, _ = store.Load("example")
	if record.Automatic || len(record.ActiveThreads) != 0 {
		t.Fatal("archive retained execution intent")
	}
	if err := store.Retire(context.Background(), "example", "root", socket, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("example"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete retained sidecar: %v", err)
	}
}

func TestRecoveryRefusesForeignOrUnversionedState(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "codex.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	store := RecoveryStore{Root: filepath.Join(root, "state"), Workspace: root}
	if err := store.Set(context.Background(), "example", "root", socket, true, false); err != nil {
		t.Fatal(err)
	}
	path, _ := store.path("example")
	for _, body := range []string{`{"schema":2}`, `{"schema":1,"workspace":"foreign"}`, `{"schema":1,"unknown":true}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load("example"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
