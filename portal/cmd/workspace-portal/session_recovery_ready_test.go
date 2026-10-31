package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestTerminalRecoveryKeepsUnresolvedSendsAndDoesNotDiscoverOrActivateNeighbors(t *testing.T) {
	for _, scenario := range []string{"ready", "wrong cwd", "wrong source", "missing rollout", "lifecycle", "authority", "created start", "changed start", "unfinished creation"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, stateRoot, home, authority := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.Chmod(authority, 0700); err != nil {
				t.Fatal(err)
			}
			const slug = "retained"
			const thread = "00000000-0000-7000-8000-000000000001"
			directory := filepath.Join(workspace, "work", slug)
			rollout := filepath.Join(home, "sessions", "rollout-2026-10-30T12-00-00-"+thread+".jsonl")
			for _, p := range []string{directory, filepath.Dir(rollout), filepath.Join(home, "archived_sessions")} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "missing rollout" {
				if err := os.WriteFile(rollout, []byte("retained history\n"), 0600); err != nil {
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
						return nil
					}
					if request["method"] != "thread/read" {
						return fmt.Errorf("terminal restoration attempted %v", request["method"])
					}
					params := request["params"].(map[string]any)
					if params["threadId"] != thread {
						return fmt.Errorf("restoration read an unretained thread")
					}
					cwd, source := directory, "vscode"
					if scenario == "wrong cwd" {
						cwd += "-foreign"
					}
					if scenario == "wrong source" {
						source = "exec"
					}
					result := map[string]any{"thread": map[string]any{"id": thread, "cwd": cwd, "source": source, "path": rollout, "status": map[string]any{"type": "notLoaded"}}}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.165.0\n", slug, thread, socket)
			if scenario == "created start" || scenario == "changed start" {
				manifest += "creation:\n  state: ready\n"
			}
			if scenario == "unfinished creation" {
				manifest += "creation:\n  state: creating\n"
			}
			for name, body := range map[string]string{"plan.md": "# Retained work\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": manifest} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "lifecycle" {
				dir := filepath.Join(workspace, "worktrees", ".locks")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, slug+".archive-cleanup.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "authority" {
				if err := os.WriteFile(filepath.Join(authority, slug+".json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
			client := newCodexClient(socket, workspace, stateRoot)
			defer client.Close()
			const text = "Preserve this original pending input."
			if err := client.PrepareSend(thread, text, "pending-input", "", false); err != nil {
				t.Fatal(err)
			}
			before, found, err := client.OriginalSendOptions(thread, text, "pending-input", "")
			if err != nil || !found {
				t.Fatal("missing pending preparation", err)
			}
			args := []string{"require-recovery-ready", "--workspace", workspace, "--session-slug", slug, "--socket", socket, "--user-state-root", stateRoot, "--authority-dir", authority, "--codex-home", home}
			if scenario == "created start" || scenario == "changed start" {
				dir := filepath.Join(workspace, "worktrees", ".locks")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				token := strings.Repeat("a", 64)
				data, _ := json.Marshal(map[string]any{"schema": 1, "slug": slug, "state": "creating", "tmux_identity": token})
				if err := os.WriteFile(filepath.Join(dir, slug+".start.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "changed start" {
					token = strings.Repeat("b", 64)
				}
				args = append(args, "--expected-start-tmux-identity", token)
			}
			err = sessionCommand(args)
			if (err == nil) != (scenario == "ready" || scenario == "created start") {
				t.Fatalf("recovery result: %v", err)
			}
			after, found, readErr := client.OriginalSendOptions(thread, text, "pending-input", "")
			beforeJSON, _ := json.Marshal(before)
			afterJSON, _ := json.Marshal(after)
			if readErr != nil || !found || string(beforeJSON) != string(afterJSON) {
				t.Fatal("restoration changed pending input", readErr)
			}
			if err := client.RequireSubmissionAttemptsKnown(context.Background(), thread); err == nil {
				t.Fatal("restoration erased unresolved-send refusal")
			}
		})
	}
}
