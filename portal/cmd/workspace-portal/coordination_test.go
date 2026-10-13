package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

func TestCoordinationNotificationRetainsSettingsAndDeduplicatesNativeDelivery(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%v", active), func(t *testing.T) {
			workspace, state := t.TempDir(), t.TempDir()
			t.Setenv("DEV_WORKSPACE_CODEX_HOME", t.TempDir())
			const slug = "2026-10-13-coordination"
			const thread = "11111111-1111-7111-8111-111111111111"
			cwd := filepath.Join(workspace, "work", slug)
			rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(rollout, nil, 0600); err != nil {
				t.Fatal(err)
			}
			sends := 0
			socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
				if err := threadCommandHandshake(connection); err != nil {
					return err
				}
				for {
					request, err := threadCommandReadObject(connection)
					if err != nil {
						return nil
					}
					params, _ := request["params"].(map[string]any)
					var result any = map[string]any{}
					switch request["method"] {
					case "thread/read":
						metadata := threadCommandFreshMetadata(thread, cwd, rollout)
						metadata["model"], metadata["reasoningEffort"] = "retained-model", "xhigh"
						result = map[string]any{"thread": metadata}
					case "thread/resume":
						if params["threadId"] != thread || !strings.Contains(fmt.Sprint(params["developerInstructions"]), slug) {
							return fmt.Errorf("resume lost owned policy: %v", params)
						}
					case "thread/turns/list":
						turns := []any{}
						if active {
							turns = append(turns, map[string]any{"id": "active-turn", "status": "inProgress"})
						}
						result = map[string]any{"data": turns}
					case "turn/start", "turn/steer":
						want := "turn/start"
						if active {
							want = "turn/steer"
						}
						if request["method"] != want || params["threadId"] != thread || params["clientUserMessageId"] != "coordination-event-1" {
							return fmt.Errorf("wrong native delivery: %v", request)
						}
						if !active && (params["model"] != "retained-model" || params["effort"] != "xhigh") {
							return fmt.Errorf("start lost settings: %v", params)
						}
						if active && (params["expectedTurnId"] != "active-turn" || params["model"] != nil || params["effort"] != nil) {
							return fmt.Errorf("steer changed active settings: %v", params)
						}
						sends++
						result = map[string]any{"turn": map[string]any{"id": "new-turn"}, "turnId": "active-turn"}
					default:
						return fmt.Errorf("unexpected notification RPC: %v", request["method"])
					}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.160.0\n", slug, thread, socket)
			for name, content := range map[string]string{"portal.yml": manifest, "state.md": "---\nlifecycle: active\n---\n", "plan.md": "Coordination test\n"} {
				if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			request := map[string]any{"schema": 1, "action": "prepare", "workspace": workspace, "slug": slug, "socket": socket, "thread": thread, "stateRoot": state}
			call := func() ([]byte, error) {
				path := filepath.Join(t.TempDir(), "request.json")
				data, _ := json.Marshal(request)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				previous := os.Stdout
				os.Stdout = w
				err = coordinationNotify([]string{"--request", path})
				os.Stdout = previous
				w.Close()
				output, _ := io.ReadAll(r)
				r.Close()
				return output, err
			}
			output, err := call()
			if err != nil {
				t.Fatal(err)
			}
			var prepared struct {
				Options codex.TurnOptions `json:"options"`
			}
			if err := json.Unmarshal(output, &prepared); err != nil {
				t.Fatal(err)
			}
			if prepared.Options.Model != "retained-model" || prepared.Options.ReasoningEffort != "xhigh" || prepared.Options.ThreadPolicy.DeveloperInstructions != workspacecodex.LeadThreadPolicy(slug, workspace, "").DeveloperInstructions {
				t.Fatalf("wrong prepared options: %s", output)
			}
			request["action"], request["options"] = "send", prepared.Options
			request["text"], request["messageId"], request["actionContext"] = "operation complete", "coordination-event-1", "coordination:owner:event"
			for i := 0; i < 2; i++ {
				if _, err := call(); err != nil {
					t.Fatal(err)
				}
			}
			if sends != 1 {
				t.Fatalf("native sends=%d, want one", sends)
			}
			request["thread"] = "22222222-2222-7222-8222-222222222222"
			if _, err := call(); err == nil {
				t.Fatal("replacement owner accepted")
			}
		})
	}
}
