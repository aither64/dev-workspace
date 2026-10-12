package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/coder/websocket"
)

func TestExplicitThreadlessArchiveUsesRealObserverAndPreparedReceipt(t *testing.T) {
	for _, scenario := range []string{"terminal", "absent", "automatic", "native identity", "pane identity", "changed terminal", "unaccepted context", "conversation"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, stateRoot, home := t.TempDir(), t.TempDir(), t.TempDir()
			directory := filepath.Join(workspace, "work", "example")
			if err := os.MkdirAll(directory, 0755); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]string{"plan.md": "# Plan\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": "schema: 1\nslug: example\nrepositories: []\nartifacts: []\n"} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"sessions", "archived_sessions"} {
				if err := os.Mkdir(filepath.Join(home, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			authorityDir := filepath.Join(t.TempDir(), "authority")
			if err := os.Mkdir(authorityDir, 0700); err != nil {
				t.Fatal(err)
			}
			tmuxSocket := filepath.Join(t.TempDir(), "tmux.sock")
			identity := strings.Repeat("b", 64)
			if scenario != "absent" {
				authority := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: "example", Workspace: workspace, TmuxSocket: tmuxSocket, TmuxSessionID: "$7", TmuxIdentity: identity}
				data, _ := json.Marshal(authority)
				if err := os.WriteFile(filepath.Join(authorityDir, "example.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(tmuxSocket, nil, 0600); err != nil {
					t.Fatal(err)
				}
				fields := []string{"$7", "example", "1", "example", workspace, "example", tmuxSocket, "", "", "", "", identity}
				switch scenario {
				case "native identity":
					fields[7], fields[8], fields[9] = "unexpected", "/run/codex.sock", "0.160.0"
				case "pane identity":
					fields[10] = "%3"
				case "changed terminal":
					fields[11] = strings.Repeat("c", 64)
				}
				bin := t.TempDir()
				script := "#!/bin/sh\nif [ \"$3\" = display-message ]; then printf '%s\\n' '" + strings.Join(fields, "\t") + "'; else printf '$7\\texample\\n'; fi\n"
				if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			}
			if scenario != "unaccepted context" {
				locks := filepath.Join(workspace, "worktrees", ".locks")
				if err := os.MkdirAll(locks, 0700); err != nil {
					t.Fatal(err)
				}
				// Runner owns reconstruction and validation of this prepared intent.
				if err := os.WriteFile(filepath.Join(locks, "example.archive-cleanup.json"), []byte("validated prepared intent"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
				defer connection.CloseNow()
				if err := threadCommandHandshake(connection); err != nil {
					return err
				}
				for {
					request, err := threadCommandReadObject(connection)
					if err != nil {
						return nil // The observation closes its client after the proof.
					}
					result := map[string]any{"data": []any{}, "nextCursor": nil}
					if request["method"] == "thread/list" && scenario == "conversation" {
						result["data"] = []any{map[string]any{"id": "unexpected", "cwd": directory, "source": "vscode"}}
					}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
			t.Setenv("DEV_SESSION_TMUX_SOCKET", tmuxSocket)
			mode := "archived"
			if scenario == "automatic" {
				mode = "complete"
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stdout
			os.Stdout = writer
			defer func() { os.Stdout = previous }()
			err = sessionCommand([]string{"observe", "--workspace", workspace, "--session-slug", "example", "--socket", socket, "--user-state-root", stateRoot, "--authority-dir", authorityDir, "--codex-home", home, "--expected-operation-id", strings.Repeat("a", 64), "--expected-archive-mode", mode})
			writer.Close()
			os.Stdout = previous
			data, _ := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			var result teamruntime.Observation
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			allowed := scenario == "terminal" || scenario == "absent"
			if result.ActivityKnown != allowed || result.Idle != allowed {
				t.Fatalf("observation = %s", data)
			}
		})
	}
}
