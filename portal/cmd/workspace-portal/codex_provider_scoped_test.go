package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

func TestScopedProviderSwitchKeepsOtherSessionsAndRecoversLostCloseReply(t *testing.T) {
	for _, closeFault := range []string{"none", "lost-reply", "after-shutdown"} {
		for _, goalEnd := range []string{"active", "during-close", "before-close"} {
			t.Run(closeFault+"-goal-"+goalEnd, func(t *testing.T) {
				goalFinishes := goalEnd != "active"
				workspace := t.TempDir()
				slug := "2026-10-18-scope"
				cwd := filepath.Join(workspace, "work", slug)
				authority := filepath.Join(workspace, "authority")
				if err := os.MkdirAll(authority, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(workspace, "switch.json")
				operation := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
				snapshot := scopedProviderSnapshot{providerInventory: providerInventory{Schema: 1, Workspace: workspace, Threads: []providerThread{}, Loaded: []string{"busy", "idle"}, Blockers: []string{}}, Operation: operation, Target: "openai", Sessions: []string{slug}, Intents: map[string]*scopedThreadIntent{}}
				original := func(id, provider string) map[string]any {
					return map[string]any{"thread": map[string]any{"id": id, "canAcceptDirectInput": true}, "cwd": cwd, "model": "fixture-model", "modelProvider": provider, "reasoningEffort": "low", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "dangerFullAccess"}, "runtimeWorkspaceRoots": []any{workspace}, "activePermissionProfile": nil, "serviceTier": nil}
				}
				for _, id := range []string{"busy", "idle"} {
					snapshot.Threads = append(snapshot.Threads, providerThread{ID: id, Slug: slug, Cwd: cwd, Settings: codex.ThreadSettings{Model: "fixture-model", ReasoningEffort: "low"}, Original: original(id, "pool")})
					snapshot.Intents[id] = &scopedThreadIntent{Active: id == "busy", GoalActive: id == "busy", GoalOwned: id == "busy", PauseTurnID: id + "-turn", Continuation: operation}
				}
				if err := providerAtomicJSON(path, snapshot); err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				archived := map[string]bool{}
				stopped := map[string]bool{}
				continuations := 0
				providers := map[string]string{"busy": "pool", "idle": "pool", "unrelated": "pool", "child": "pool"}
				closes := map[string]int{}
				goalActive := true
				lost := false
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
						id, _ := params["threadId"].(string)
						if id == "unrelated" || id == "child" {
							return fmt.Errorf("unrelated session was touched")
						}
						mu.Lock()
						result := any(map[string]any{})
						injected := false
						switch request["method"] {
						case "thread/read":
							if params["excludeTurns"] != true {
								mu.Unlock()
								return fmt.Errorf("full transcript requested for a scoped check")
							}
							rollout := filepath.Join(workspace, "sessions", id+".jsonl")
							if archived[id] {
								rollout = filepath.Join(workspace, "archived_sessions", id+".jsonl")
							}
							result = map[string]any{"thread": map[string]any{"id": id, "path": rollout, "cwd": cwd, "source": "vscode", "model": "fixture-model", "reasoningEffort": "low", "modelProvider": providers[id], "status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true}}
						case "thread/archive":
							closes[id]++
							if goalFinishes && id == "busy" {
								goalActive = false
							}
							stopped[id] = true
							if closeFault != "after-shutdown" || lost {
								archived[id] = true
							}
							if closeFault != "none" && !lost {
								lost = true
								injected = true
							}
						case "thread/unarchive":
							archived[id] = false
						case "thread/resume":
							if archived[id] {
								mu.Unlock()
								return fmt.Errorf("archived thread resumed")
							}
							if provider, _ := params["modelProvider"].(string); provider != "" {
								providers[id] = provider
							}
							result = original(id, providers[id])
						case "thread/turns/list":
							status := "completed"
							if id == "busy" {
								status = "inProgress"
								if stopped[id] {
									status = "interrupted"
								}
							}
							turnID := id + "-turn"
							if id == "busy" && goalEnd == "before-close" {
								goalActive, turnID = false, "new-ordinary-turn"
							}
							result = map[string]any{"data": []any{map[string]any{"id": turnID, "status": status}}}
						case "turn/start":
							continuations++
							result = map[string]any{"turn": map[string]any{"id": "continued-turn"}}
						case "thread/goal/get":
							if archived[id] {
								mu.Unlock()
								return fmt.Errorf("goal lookup cannot read unloaded archived history %s", id)
							}
							status := "complete"
							if goalActive {
								status = "active"
							}
							result = map[string]any{"goal": nil}
							if id == "busy" {
								result = map[string]any{"goal": map[string]any{"threadId": id, "status": status}}
							}
						default:
							mu.Unlock()
							return fmt.Errorf("unexpected scoped request %v", request["method"])
						}
						mu.Unlock()
						reply := map[string]any{"id": request["id"], "result": result}
						if injected {
							delete(reply, "result")
							reply["error"] = map[string]any{"code": -32000, "message": "synthetic lost close reply"}
						}
						if err := threadCommandWriteObject(connection, reply); err != nil {
							return err
						}
					}
				})
				client := &workspacecodex.Client{Client: codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})}
				defer client.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := os.MkdirAll(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{"state.md": "---\nlifecycle: active\n---\n", "plan.md": "Scoped switch fixture\n"} {
					if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
				manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: busy\n  socket_path: %s\n  client_version: 0.163.0\n", slug, socket)
				if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
					t.Fatal(err)
				}
				roots, _ := json.Marshal(map[string]string{slug: "busy"})
				if _, err := session.Find(workspace, slug); err != nil {
					t.Fatal(err)
				}
				run := func(action string) error {
					return scopedProviderCommand(ctx, client, action, workspace, workspace, socket, workspace, authority, path, "openai", operation, slug, string(roots))
				}
				err := run("scoped-switch")
				if closeFault != "none" {
					if err == nil {
						t.Fatal("injected lost reply succeeded")
					}
					err = run("scoped-switch")
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := run("scoped-continue"); err != nil {
					t.Fatal(err)
				}
				if err := run("scoped-continue"); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				wantedCloses := 1
				if closeFault == "after-shutdown" {
					wantedCloses = 2
				}
				wantedContinuations := 0
				if goalEnd == "before-close" {
					wantedContinuations = 1
				}
				if continuations != wantedContinuations {
					t.Fatalf("continuations=%d, want %d", continuations, wantedContinuations)
				}
				if providers["busy"] != "openai" || providers["idle"] != "openai" || providers["unrelated"] != "pool" || providers["child"] != "pool" || closes["child"] != 0 || goalActive == goalFinishes || closes["busy"] != wantedCloses || closes["idle"] != 1 || archived["busy"] || archived["idle"] {
					t.Fatalf("providers=%v closes=%v archived=%v goal=%v", providers, closes, archived, goalActive)
				}
				var saved scopedProviderSnapshot
				data, _ := os.ReadFile(path)
				if json.Unmarshal(data, &saved) != nil || !saved.Intents["busy"].Continued || !saved.Intents["idle"].Continued {
					t.Fatal("continuation receipt missing")
				}
			})
		}
	}
}
