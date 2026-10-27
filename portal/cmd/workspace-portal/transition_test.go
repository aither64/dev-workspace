package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/coder/websocket"
)

func TestTransitionProofPreservesDormantAttemptsAndStrictLifecycle(t *testing.T) {
	for _, state := range []string{"prepared", "submitting"} {
		t.Run(state, func(t *testing.T) {
			workspace, stateRoot := t.TempDir(), t.TempDir()
			cwd := filepath.Join(workspace, "work", "example")
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "state.md"), []byte("---\nlifecycle: active\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "plan.md"), []byte("Fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte("schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n"), 0600); err != nil {
				t.Fatal(err)
			}
			socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
				if err := threadCommandHandshake(connection); err != nil {
					return err
				}
				// A cold helper must ignore both answerable and unsupported requests.
				for _, method := range []string{"item/tool/requestUserInput", "unsupported/request"} {
					if err := threadCommandWriteObject(connection, map[string]any{"id": "request-" + method, "method": method, "params": map[string]any{"threadId": "thread-1"}}); err != nil {
						return err
					}
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
					case "thread/turns/list", "thread/queue/list", "thread/items/list":
						result = map[string]any{"data": []any{}}
					default:
						return fmt.Errorf("cold proof sent a response or mutation: %#v", request)
					}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			// Build real schema-3 originals using the client, including frozen options.
			legacy := codex.NewWithOptions(socket, codex.ClientOptions{})
			options := codex.TurnOptions{Model: "gpt-6-sol"}
			if err := legacy.PrepareSendWithOptions("thread-1", "retain original", "attempt-1", "context", false, options); err != nil {
				t.Fatal(err)
			}
			legacy.Close()
			path := socket + ".submission-attempts-v3.json"
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if state == "submitting" {
				original = bytes.ReplaceAll(original, []byte(`"state":"prepared"`), []byte(`"state":"submitting"`))
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Open the selected transactional store before taking the preserved snapshot.
			selected := newTransitionClient(socket, workspace, stateRoot)
			before, err := selected.ExportSubmissionAttempts("thread-1")
			if err != nil {
				t.Fatal(err)
			}
			selected.Close()
			args := []string{"require-execution-idle", "--thread-id", "thread-1", "--cwd", cwd, "--socket", socket, "--workspace", workspace, "--user-state-root", stateRoot}
			for i := 0; i < 2; i++ { // fresh connection/reopen cannot resume or rewrite originals
				if err := threadCommand(args); err != nil {
					t.Fatal(err)
				}
			}
			reader := newTransitionClient(socket, workspace, stateRoot)
			defer reader.Close()
			after, err := reader.ExportSubmissionAttempts("thread-1")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("attempt changed: %s -> %s, %v", before, after, err)
			}
			var snapshot map[string]any
			if json.Unmarshal(after, &snapshot) != nil || !bytes.Contains(after, []byte(`"state":"`+state+`"`)) {
				t.Fatal("missing original state", string(after))
			}
			if err := reader.RequireSubmissionAttemptsResolved(context.Background(), "thread-1"); err == nil {
				t.Fatal("strict lifecycle accepted unknown receipt")
			}
			if state == "submitting" {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if _, _, err := reader.ReconcileSendWithOptions(ctx, "thread-1", "retain original", "attempt-1", "context", options); err == nil || !strings.Contains(err.Error(), "unknown") {
					t.Fatal("missing history converted uncertain original", err)
				}
				retained, err := reader.ExportSubmissionAttempts("thread-1")
				if err != nil || !bytes.Equal(before, retained) {
					t.Fatal("reconciliation changed original", string(retained), err)
				}
			}
		})
	}
}

func TestTransitionProofRejectsNativeUncertaintyAndInvalidStorage(t *testing.T) {
	for _, scenario := range []string{"idle", "notLoaded", "active", "waiting", "systemError", "unknown", "malformed", "busy turn", "queue", "wrong cwd", "invalid store", "invalid database"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, stateRoot := t.TempDir(), t.TempDir()
			cwd := filepath.Join(workspace, "work", "example")
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "state.md"), []byte("---\nlifecycle: active\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "plan.md"), []byte("Fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte("schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n"), 0600); err != nil {
				t.Fatal(err)
			}
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
						metadata := threadCommandFreshMetadata("thread-1", cwd, filepath.Join(cwd, "rollout.jsonl"))
						status := "idle"
						if scenario == "notLoaded" || scenario == "active" || scenario == "waiting" || scenario == "systemError" || scenario == "unknown" {
							status = scenario
						}
						metadata["status"] = map[string]any{"type": status}
						if scenario == "waiting" {
							metadata["status"] = map[string]any{"type": "active", "activeFlags": []any{"waitingOnUserInput"}}
						}
						if scenario == "malformed" {
							metadata["status"] = map[string]any{"type": "idle", "activeFlags": []any{"waitingOnUserInput"}}
						}
						if scenario == "wrong cwd" {
							metadata["cwd"] = "/foreign"
						}
						result = map[string]any{"thread": metadata}
					case "thread/turns/list":
						data := []any{}
						if scenario == "busy turn" {
							data = []any{map[string]any{"id": "turn-1", "status": "inProgress"}}
						}
						result = map[string]any{"data": data}
					case "thread/queue/list":
						data := []any{}
						if scenario == "queue" {
							data = []any{map[string]any{"id": "queued-1", "clientUserMessageId": "queued-message-1", "input": []any{map[string]any{"type": "text", "text": "held native work"}}}}
						}
						result = map[string]any{"data": data}
					default:
						return fmt.Errorf("unexpected cold RPC: %v", request)
					}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			if scenario == "invalid store" {
				if err := os.WriteFile(socket+".submission-attempts-v3.json", []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "invalid database" {
				writer := newCodexClient(socket, workspace, stateRoot)
				if err := writer.PrepareSend("thread-1", "pending", "attempt-1", "test", false); err != nil {
					t.Fatal(err)
				}
				writer.Close()
				paths, _ := filepath.Glob(filepath.Join(stateRoot, "submissions", "*.sqlite"))
				if len(paths) != 1 {
					t.Fatal("missing selected database")
				}
				if err := os.WriteFile(paths[0], []byte("invalid database"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := threadCommand([]string{"require-execution-idle", "--thread-id", "thread-1", "--cwd", cwd, "--socket", socket, "--workspace", workspace, "--user-state-root", stateRoot})
			if scenario == "queue" && (err == nil || err.Error() != "conversation has queued native work") {
				t.Fatalf("valid native queue did not reach the queued-work refusal: %v", err)
			}
			want := scenario == "idle" || scenario == "notLoaded"
			if (err == nil) != want {
				t.Fatalf("proof result %v, want success %t", err, want)
			}
		})
	}
}
