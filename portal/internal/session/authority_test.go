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
		TmuxIdentity:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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
		"%3", record.TmuxIdentity,
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

func TestRuntimeAuthorityRejectsMismatchedAndMalformedLiveTmuxIdentities(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	record := RuntimeAuthority{
		Schema: 1, State: "ready", Slug: "example", Workspace: "/srv/workspace",
		TmuxSocket: "/run/workspace/tmux.sock", TmuxSessionID: "$7",
		TmuxIdentity: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	write := func(authority RuntimeAuthority, liveIdentity string) error {
		data, err := json.Marshal(authority)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(directory, "example.json"), data, 0o600); err != nil {
			return err
		}
		loaded, err := LoadRuntimeAuthority(directory, "example", "/srv/workspace")
		if err != nil {
			return err
		}
		tmux := filepath.Join(t.TempDir(), "tmux")
		line := strings.Join([]string{
			"$7", "example", "1", "example", "/srv/workspace", "example",
			"/run/workspace/tmux.sock", "", "", "", "", liveIdentity,
		}, "\t")
		if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf '%s\\n' '"+line+"'\n"), 0o755); err != nil {
			return err
		}
		return loaded.VerifyTmux(context.Background(), tmux)
	}

	if err := write(record, strings.Repeat("b", 64)); err == nil {
		t.Fatal("mismatched live tmux identity was accepted")
	}
	legacy := record
	legacy.TmuxIdentity = ""
	if err := write(legacy, "malformed"); err == nil {
		t.Fatal("malformed live tmux identity was accepted for a legacy authority")
	}
	if err := write(legacy, ""); err != nil {
		t.Fatalf("legacy authority with an empty live identity was rejected: %v", err)
	}
	renamed := record
	renamed.TmuxSocket = "/run/renamed/tmux.sock"
	if err := write(renamed, record.TmuxIdentity); err != nil {
		t.Fatalf("identity-bound authority rejected a renamed live socket: %v", err)
	}
	renamed.TmuxIdentity = ""
	if err := write(renamed, record.TmuxIdentity); err == nil {
		t.Fatal("legacy authority accepted a mismatched reported socket path")
	}
}

func TestRuntimeAuthoritySharedCorpus(t *testing.T) {
	fixtures := filepath.Join("..", "..", "..", "test", "fixtures")
	manifestData, err := os.ReadFile(filepath.Join(fixtures, "runtime-authority-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Schema  int      `json:"schema"`
		Valid   []string `json:"valid"`
		Invalid []string `json:"invalid"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Schema != 1 || len(manifest.Valid) == 0 || len(manifest.Invalid) == 0 {
		t.Fatalf("invalid authority corpus manifest: %#v", manifest)
	}
	seen := make(map[string]bool)
	for _, group := range []struct {
		names []string
		ok    bool
	}{{manifest.Valid, true}, {manifest.Invalid, false}} {
		for _, name := range group.names {
			if seen[name] || filepath.Base(name) != name || filepath.Ext(name) != ".json" {
				t.Fatalf("invalid or duplicate authority corpus entry %q", name)
			}
			seen[name] = true
			fixture := filepath.Join(fixtures, name)
			t.Run(name, func(t *testing.T) {
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
				if (err == nil) != group.ok {
					t.Fatalf("accepted = %t, want %t: %v", err == nil, group.ok, err)
				}
			})
		}
	}
	entries, err := filepath.Glob(filepath.Join(fixtures, "runtime-authority-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := filepath.Base(entry)
		if name == "runtime-authority-corpus.json" {
			continue
		}
		if !seen[name] {
			t.Fatalf("authority corpus manifest omits fixture %q", name)
		}
		delete(seen, name)
	}
	if len(seen) != 0 {
		t.Fatalf("authority corpus manifest names missing fixtures: %#v", seen)
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
