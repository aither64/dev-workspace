package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
)

func TestExplicitArchiveCommandSavesQueueInSelectedPrivateStorage(t *testing.T) {
	workspace, stateRoot := t.TempDir(), t.TempDir()
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", t.TempDir())
	t.Setenv("DEV_WORKSPACES_STATE", t.TempDir()) // The selected flag must win.
	cwd := filepath.Join(workspace, "work", "example")
	thread := "11111111-1111-7111-8111-111111111111"
	queued := true
	var heldPath string
	socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
		if err := threadCommandHandshake(connection); err != nil {
			return err
		}
		for {
			request, err := threadCommandReadObject(connection)
			if err != nil {
				return nil
			}
			var result any = map[string]any{}
			switch request["method"] {
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": thread, "cwd": cwd, "source": "vscode", "path": nil}}
			case "thread/queue/list":
				rows := []any{}
				if queued {
					rows = append(rows, map[string]any{"id": "queue-1", "clientUserMessageId": "message-1", "input": []any{map[string]any{"type": "text", "text": "retain this prompt"}}})
				}
				result = map[string]any{"data": rows}
			case "thread/queue/delete":
				paths, _ := filepath.Glob(filepath.Join(stateRoot, "archived-prompts", "*", "example", "*.json"))
				if len(paths) != 1 {
					return fmt.Errorf("selected private storage missing before deletion: %v", paths)
				}
				heldPath, queued = paths[0], false
			case "thread/goal/get":
				result = map[string]any{"goal": nil}
			case "thread/loaded/list":
				result = map[string]any{"data": []any{}}
			default:
				return fmt.Errorf("unexpected archive command RPC: %v", request["method"])
			}
			if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
				return err
			}
		}
	})
	if err := threadCommand([]string{"quiesce-explicit", "--thread-id", thread, "--cwd", cwd,
		"--socket", socket, "--workspace", workspace, "--user-state-root", stateRoot}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(heldPath)
	if err != nil {
		t.Fatal(err)
	}
	var held struct {
		Thread  string                     `json:"thread_id"`
		Cwd     string                     `json:"cwd"`
		Entries map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &held); err != nil {
		t.Fatal(err)
	}
	if queued || held.Thread != thread || held.Cwd != cwd || len(held.Entries["queue-1"]) == 0 {
		t.Fatalf("queue was not retained with its selected identity: %s", data)
	}
}
