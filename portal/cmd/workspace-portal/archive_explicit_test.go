package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"

	"github.com/coder/websocket"
)

func TestThreadRequireIdleUsesSelectedSubmissionStorage(t *testing.T) {
	workspace, stateRoot := t.TempDir(), t.TempDir()
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", t.TempDir())
	cwd := filepath.Join(workspace, "work", "example")
	socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
		if err := threadCommandHandshake(connection); err != nil {
			return err
		}
		for {
			request, err := threadCommandReadObject(connection)
			if err != nil {
				return nil
			}
			var result any
			switch request["method"] {
			case "thread/read":
				result = map[string]any{"thread": threadCommandFreshMetadata("thread-1", cwd, filepath.Join(cwd, "rollout.jsonl"))}
			case "thread/turns/list", "thread/queue/list":
				result = map[string]any{"data": []any{}}
			default:
				return fmt.Errorf("unexpected idle check RPC: %v", request["method"])
			}
			if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
				return err
			}
		}
	})
	writer := newCodexClient(socket, workspace, stateRoot)
	defer writer.Close()
	// A running portal migrates and fences the socket-adjacent JSON path.
	// Keep a real unresolved receipt in SQLite to prove idle checks read it.
	if err := writer.PrepareSend("thread-1", "pending", "attempt-1", "test", false); err != nil {
		t.Fatal(err)
	}
	args := []string{"require-idle", "--thread-id", "thread-1", "--cwd", cwd,
		"--socket", socket, "--workspace", workspace}
	if err := threadCommand(args); err == nil || !strings.Contains(err.Error(), `unknown field "storage"`) {
		t.Fatalf("unselected helper did not reject the legacy fence: %v", err)
	}
	args = append(args, "--user-state-root", stateRoot)
	if err := threadCommand(args); err == nil || !strings.Contains(err.Error(), "unresolved message attempt") {
		t.Fatalf("selected helper did not observe the current receipt: %v", err)
	}
	if discarded, err := writer.DiscardPreparedSend("thread-1", "pending", "attempt-1", "test"); err != nil || !discarded {
		t.Fatalf("resolve fixture receipt: %t, %v", discarded, err)
	}
	if err := threadCommand(args); err != nil {
		t.Fatalf("selected helper could not prove idle after resolution: %v", err)
	}
}

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

func TestExplicitArchiveCommandInterruptsNativeWatchers(t *testing.T) {
	for _, parentKind := range []string{"root", "member"} {
		for _, outcome := range []string{"success", "lost_reply", "uncertain_managed", "changed_parent", "changed_cwd", "changed_id"} {
			t.Run(parentKind+"/"+outcome, func(t *testing.T) {
				workspace, stateRoot, home := t.TempDir(), t.TempDir(), t.TempDir()
				t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
				t.Setenv("DEV_WORKSPACE_NAME", "")
				cwd := filepath.Join(workspace, "work", "example")
				root := "11111111-1111-7111-8111-111111111111"
				parent := root
				if parentKind == "member" {
					parent = "22222222-2222-7222-8222-222222222222"
				}
				child := "33333333-3333-7333-8333-333333333333"
				foreign := "44444444-4444-7444-8444-444444444444"
				for _, dir := range []string{cwd, filepath.Join(home, "sessions")} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				rollouts := map[string]string{}
				for _, id := range []string{parent, child} {
					rollouts[id] = filepath.Join(home, "sessions", "rollout-2026-10-28T10-00-00-"+id+".jsonl")
					if err := os.WriteFile(rollouts[id], []byte("{}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				stopped, paused := map[string]bool{}, map[string]bool{}
				queued, archived, lost := true, false, false
				childReads := 0
				var retryStore turnretry.Store
				var scope turnretry.Scope
				socket := threadCommandAppServer(t, func(conn *websocket.Conn) error {
					if err := threadCommandHandshake(conn); err != nil {
						return err
					}
					for {
						req, err := threadCommandReadObject(conn)
						if err != nil {
							return nil
						}
						params, _ := req["params"].(map[string]any)
						id, _ := params["threadId"].(string)
						method := req["method"]
						if id == foreign && method != "thread/read" {
							return fmt.Errorf("foreign child mutated: %v", method)
						}
						var result any = map[string]any{}
						switch method {
						case "thread/read":
							metadata := threadCommandFreshMetadata(id, cwd, rollouts[id])
							if id == child || id == foreign {
								owner := parent
								if id == foreign {
									owner = "another-root"
								}
								metadata["source"] = map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": owner, "depth": 1}}}
							}
							if id == child {
								childReads++
								if childReads == 2 {
									switch outcome {
									case "changed_parent":
										metadata["source"] = map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "another-root"}}}
									case "changed_cwd":
										metadata["cwd"] = filepath.Join(workspace, "work", "another")
									case "changed_id":
										metadata["id"] = foreign
									}
								}
							}
							result = map[string]any{"thread": metadata}
						case "thread/loaded/list":
							if !stopped[parent] {
								return fmt.Errorf("native discovery before parent stopped")
							}
							result = map[string]any{"data": []string{child, foreign}}
						case "thread/queue/list":
							rows := []any{}
							if id == child && queued {
								rows = append(rows, map[string]any{"id": "queue-1", "clientUserMessageId": "message-1", "input": []any{map[string]any{"type": "text", "text": "keep this"}}})
							}
							result = map[string]any{"data": rows}
						case "thread/queue/delete":
							paths, _ := filepath.Glob(filepath.Join(stateRoot, "archived-prompts", "*", "example", "*.json"))
							retained := false
							for _, path := range paths {
								data, err := os.ReadFile(path)
								if err != nil {
									return err
								}
								var record struct {
									Thread  string                     `json:"thread_id"`
									Entries map[string]json.RawMessage `json:"entries"`
								}
								if err := json.Unmarshal(data, &record); err != nil {
									return err
								}
								if record.Thread == child && len(record.Entries["queue-1"]) > 0 {
									retained = true
								}
							}
							if id != child || !retained {
								return fmt.Errorf("queue deleted before durable capture")
							}
							queued = false
						case "thread/goal/get":
							status := "active"
							if paused[id] {
								status = "paused"
							}
							result = map[string]any{"goal": map[string]any{"threadId": id, "status": status}}
						case "thread/goal/set":
							if params["status"] != "paused" {
								return fmt.Errorf("goal not paused")
							}
							paused[id] = true
						case "thread/turns/list":
							status := "inProgress"
							if stopped[id] {
								status = "interrupted"
							}
							result = map[string]any{"data": []any{map[string]any{"id": "turn-" + id, "status": status}}}
						case "turn/interrupt":
							if !paused[id] || id == child && queued {
								return fmt.Errorf("interrupt before prompt/goal capture")
							}
							if id == parent {
								saved, err := retryStore.Load(scope)
								if err != nil || !saved.Cancelled || saved.Uncertain() {
									return fmt.Errorf("managed interrupt bypassed retry cancellation: %v", err)
								}
							} else if !stopped[parent] {
								return fmt.Errorf("child interrupted before parent")
							}
							stopped[id] = true
							if id == child && outcome == "lost_reply" && !lost {
								lost = true
								if err := threadCommandWriteObject(conn, map[string]any{"id": req["id"], "error": map[string]any{"code": -32000, "message": "lost interrupt reply"}}); err != nil {
									return err
								}
								continue
							}
						case "thread/archive":
							if id != child || !stopped[child] {
								return fmt.Errorf("retirement before child shutdown")
							}
							archived = true
						default:
							return fmt.Errorf("unexpected archive RPC: %v", method)
						}
						if err := threadCommandWriteObject(conn, map[string]any{"id": req["id"], "result": result}); err != nil {
							return err
						}
					}
				})
				for name, body := range map[string]string{
					"state.md": "---\nlifecycle: active\n---\n", "plan.md": "# Archive fixture\n",
					"portal.yml": fmt.Sprintf("schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.164.0\n", root, socket),
				} {
					if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				store, err := teamruntime.NewStore(stateRoot, workspace)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(context.Background(), "example", root, true, func(roster *teamruntime.Roster) error {
					roster.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Thread: "22222222-2222-7222-8222-222222222222", State: "ready", Behavior: "implementer", Access: "workspace_write", AddedAt: time.Now()}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				retryStore = turnretry.Store{Root: stateRoot, Workspace: workspace}
				scope = turnretry.Scope{Workspace: workspace, Slug: "example", Root: root, Thread: parent, Socket: socket}
				tx, err := retryStore.Lock(context.Background(), scope)
				if err != nil {
					t.Fatal(err)
				}
				if outcome == "uncertain_managed" {
					snapshot := turnretry.Snapshot{Epoch: 1, Version: 1, ID: "input"}
					snapshot.Digest, _ = snapshot.Hash()
					tx.State.Phase = "submitting"
					tx.State.Pending = &turnretry.Pending{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Action: "retry", Failure: "failed-turn", Snapshot: snapshot}
				}
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
				args := []string{"quiesce-explicit", "--thread-id", parent, "--cwd", cwd, "--socket", socket, "--workspace", workspace, "--user-state-root", stateRoot}
				err = threadCommand(args)
				if outcome == "uncertain_managed" {
					if err == nil || !strings.Contains(err.Error(), "automatic submission requires reconciliation") || stopped[parent] || archived {
						t.Fatalf("uncertain managed work bypassed: %v", err)
					}
					return
				}
				if strings.HasPrefix(outcome, "changed_") {
					if err == nil || !strings.Contains(err.Error(), "native archive") || stopped[child] || archived || !queued || paused[child] {
						t.Fatalf("changed descendant was not fenced before mutation: %v", err)
					}
					return
				}
				if outcome == "lost_reply" {
					if err == nil || !strings.Contains(err.Error(), "lost interrupt reply") || archived {
						t.Fatalf("lost interruption reply: %v", err)
					}
					err = threadCommand(args)
				}
				if err != nil {
					t.Fatal(err)
				}
				if !archived || !stopped[child] || queued {
					t.Fatalf("incomplete shutdown: archived=%v stopped=%v queued=%v", archived, stopped, queued)
				}
				// Shutdown must not enroll a native child for managed retries.
				scope.Thread = child
				if _, err := retryStore.Load(scope); !os.IsNotExist(err) {
					t.Fatalf("native child gained retry state: %v", err)
				}
			})
		}
	}
}
