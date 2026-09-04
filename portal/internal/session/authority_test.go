package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRuntimeAuthorityRequiresPrivateHostStateAndLiveTmuxIdentity(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	record := RuntimeAuthority{
		Schema: 1, State: "ready", Slug: "example", Workspace: "/srv/workspace",
		TmuxSocket: "/run/workspace/tmux.sock", TmuxSessionID: "$7",
		CodexThreadID: "thread-1", CodexSocketPath: "/run/workspace/codex.sock",
		CodexClientVersion: "0.152.1",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "example.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRuntimeAuthority(directory, "example", "/srv/workspace")
	if err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(t.TempDir(), "tmux")
	line := strings.Join([]string{
		"$7", "example", "1", "example", "/srv/workspace", "example",
		"/run/workspace/tmux.sock", "thread-1", "/run/workspace/codex.sock", "0.152.1",
		"%3",
	}, "\t")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf '%s\\n' '"+line+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := loaded.VerifyTmux(context.Background(), tmux); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "example.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeAuthority(directory, "example", "/srv/workspace"); err == nil {
		t.Fatal("mode-0644 runtime authority was accepted")
	}
}

func TestRuntimeAuthorityRejectsCrossMappedAndUnknownData(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"schema":1,"state":"ready","slug":"other","workspace":"/srv/workspace","tmux_socket":"/run/tmux.sock","tmux_session_id":"$1","extra":true}`
	if err := os.WriteFile(filepath.Join(directory, "example.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeAuthority(directory, "example", "/srv/workspace"); err == nil {
		t.Fatal("cross-mapped runtime authority was accepted")
	}
}

func TestRuntimeAuthoritySharedCorpus(t *testing.T) {
	fixtures := filepath.Join("..", "..", "..", "test", "fixtures")
	for _, pattern := range []struct {
		glob string
		ok   bool
	}{{"runtime-authority-valid-*.json", true}, {"runtime-authority-invalid-*.json", false}} {
		paths, err := filepath.Glob(filepath.Join(fixtures, pattern.glob))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixture glob %q: %v", pattern.glob, err)
		}
		for _, fixture := range paths {
			t.Run(filepath.Base(fixture), func(t *testing.T) {
				directory := filepath.Join(t.TempDir(), "authority")
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "example.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				_, err = LoadRuntimeAuthority(directory, "example", "/srv/workspace")
				if (err == nil) != pattern.ok {
					t.Fatalf("accepted = %t, want %t: %v", err == nil, pattern.ok, err)
				}
			})
		}
	}
}

func TestRuntimeLockUsesHostOnlySessionFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := LockRuntimeShared(directory, "example")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	path := filepath.Join(directory, "example.lock")
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		file.Close()
		t.Fatalf("exclusive lock unexpectedly acquired: %s", path)
	}
	file.Close()
}
