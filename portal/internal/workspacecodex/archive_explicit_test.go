package workspacecodex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/coder/websocket"
)

func TestExplicitArchiveHoldsFullQueueBeforeDeletingAndPausesActiveWork(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(fmt.Sprint("lost_delete_reply=", lostReply), func(t *testing.T) {
			root := t.TempDir()
			rollout := filepath.Join(root, "rollout.jsonl")
			if err := os.WriteFile(rollout, []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			queued, paused, interrupted, failedReply := true, false, false, false
			var heldPath string
			socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
				if err := handshake(connection); err != nil {
					return err
				}
				for {
					request, err := readObject(connection)
					if err != nil {
						return nil
					}
					var result any = map[string]any{}
					switch request["method"] {
					case "thread/read":
						result = map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": "/workspace/work/example", "source": "vscode", "path": rollout}}
					case "thread/queue/list":
						rows := []any{}
						if queued {
							rows = append(rows, map[string]any{"id": "queue-1", "clientUserMessageId": "message-1", "input": []any{map[string]any{"type": "text", "text": "saved prompt"}, map[string]any{"type": "image", "url": "https://example.invalid/image.png"}}})
						}
						result = map[string]any{"data": rows}
					case "thread/queue/delete":
						paths, _ := filepath.Glob(filepath.Join(root, "archived-prompts", "*", "example", "*.json"))
						if len(paths) != 1 {
							return fmt.Errorf("prompt was not durable before deletion: %v", paths)
						}
						heldPath = paths[0]
						queued = false
						if lostReply && !failedReply {
							failedReply = true
							if err := writeObject(connection, map[string]any{"id": request["id"], "error": map[string]any{"code": -32000, "message": "lost delete reply"}}); err != nil {
								return err
							}
							continue
						}
						result = map[string]any{"deleted": true}
					case "thread/goal/get":
						status := "active"
						if paused {
							status = "paused"
						}
						result = map[string]any{"goal": map[string]any{"threadId": "thread-1", "status": status}}
					case "thread/goal/set":
						params := request["params"].(map[string]any)
						if params["status"] != "paused" {
							return fmt.Errorf("wrong archive goal action: %v", params)
						}
						paused = true
					case "thread/turns/list":
						status := "inProgress"
						if interrupted {
							status = "interrupted"
						}
						result = map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": status}}}
					case "turn/interrupt":
						if !paused {
							return fmt.Errorf("goal was not paused before interruption")
						}
						interrupted = true
					default:
						return fmt.Errorf("unexpected explicit archive RPC: %v", request["method"])
					}
					if err := writeObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
			client.RecoveryRoot = root
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := client.QuiesceExplicitArchive(ctx, "thread-1", "/workspace/work/example")
			if lostReply {
				if err == nil {
					t.Fatal("lost reply was not reported")
				}
				err = client.QuiesceExplicitArchive(ctx, "thread-1", "/workspace/work/example")
			}
			if err != nil {
				t.Fatal(err)
			}
			if queued || !paused || !interrupted {
				t.Fatalf("queued=%v paused=%v interrupted=%v", queued, paused, interrupted)
			}
			data, err := os.ReadFile(heldPath)
			if err != nil {
				t.Fatal(err)
			}
			var saved heldPrompts
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			var entry struct {
				Input []map[string]any `json:"input"`
			}
			if err := json.Unmarshal(saved.Entries["queue-1"], &entry); err != nil {
				t.Fatal(err)
			}
			if len(entry.Input) != 2 || entry.Input[1]["url"] != "https://example.invalid/image.png" {
				t.Fatalf("held structured input changed: %s", data)
			}
			info, _ := os.Stat(heldPath)
			if info.Mode().Perm() != 0600 {
				t.Fatalf("held prompts are not private: %v", info.Mode())
			}
		})
	}
}

func TestExplicitArchiveNativeDescendantsStopsParentsAndResamplesConcurrentSpawns(t *testing.T) {
	root := t.TempDir()
	parents := map[string]string{"child": "root", "foreign": "another-root"}
	loaded := []string{"ordinary", "foreign", "child"}
	stopped := map[string]bool{}
	var archived []string
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for {
			request, err := readObject(connection)
			if err != nil {
				return nil
			}
			params, _ := request["params"].(map[string]any)
			id, _ := params["threadId"].(string)
			var result any = map[string]any{}
			switch request["method"] {
			case "thread/loaded/list":
				if !stopped["root"] {
					return fmt.Errorf("descendants inventoried before stopping root")
				}
				result = map[string]any{"data": loaded}
			case "thread/read":
				var source any = "vscode"
				if parents[id] != "" {
					source = map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parents[id], "depth": 1}}}
				}
				result = map[string]any{"thread": map[string]any{"id": id, "cwd": "/workspace/work/example", "source": source}}
			case "thread/queue/list":
				if id == "foreign" || id == "ordinary" {
					return fmt.Errorf("foreign conversation was changed: %s", id)
				}
				if id != "root" && !stopped[parents[id]] {
					return fmt.Errorf("child stopped before parent: %s", id)
				}
				result = map[string]any{"data": []any{}}
			case "thread/goal/get":
				// A parent can spawn while its interruption is in flight.
				if id == "child" || id == "grandchild" {
					child := "grandchild"
					if id == "grandchild" {
						child = "great-grandchild"
					}
					parents[child] = id
					loaded = append(loaded, child)
				}
				stopped[id] = true
				result = map[string]any{"goal": nil}
			case "thread/archive":
				if !stopped["great-grandchild"] {
					return fmt.Errorf("retirement started before descendant shutdown finished")
				}
				archived = append(archived, id)
			default:
				return fmt.Errorf("unexpected native retirement request: %v", request["method"])
			}
			if err := writeObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
				return err
			}
		}
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	client.RecoveryRoot = root
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.QuiesceExplicitArchive(ctx, "root", "/workspace/work/example"); err != nil {
		t.Fatal(err)
	}
	if err := client.QuiesceNativeDescendants(ctx, "root", "/workspace/work/example"); err != nil {
		t.Fatal(err)
	}
	if len(archived) != 3 || archived[0] != "great-grandchild" || archived[1] != "grandchild" || archived[2] != "child" {
		t.Fatalf("wrong native ownership or retirement ordering: %v", archived)
	}
}
