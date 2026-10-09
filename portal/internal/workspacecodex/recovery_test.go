package workspacecodex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/coder/websocket"
)

func TestColdAssignmentRetryPreservesPersistedHotSendOutcome(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			workspace := t.TempDir()
			cwd := filepath.Join(workspace, "work", "example")
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			starts, resumes, queues := 0, 0, 0
			socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
				if err := handshake(connection); err != nil {
					return err
				}
				for {
					request, err := readObject(connection)
					if err != nil {
						return nil
					}
					var result any
					switch request["method"] {
					case "thread/read":
						result = map[string]any{"thread": map[string]any{"id": "member", "cwd": cwd, "source": "vscode", "status": map[string]any{"type": "notLoaded"}, "turns": []any{}}}
					case "thread/resume":
						resumes++
						result = map[string]any{"thread": map[string]any{"id": "member"}}
					case "turn/start":
						starts++
						result = map[string]any{}
						if !unknown {
							result = map[string]any{"turn": map[string]any{"id": "original-turn"}}
						}
					case "thread/queue/add":
						queues++
						result = map[string]any{}
					case "thread/turns/list", "thread/items/list", "thread/queue/list":
						result = map[string]any{"data": []any{}, "nextCursor": nil}
					default:
						return fmt.Errorf("retry unexpectedly used %v", request["method"])
					}
					if err := writeObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return nil
					}
				}
			})
			for name, body := range map[string]string{"plan.md": "# Plan\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root\n  socket_path: " + socket + "\n  client_version: 0.160.0\n"} {
				if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			state := filepath.Join(workspace, "state")
			if err := os.Mkdir(state, 0700); err != nil {
				t.Fatal(err)
			}
			options := codex.TurnOptions{Model: "saved-model", ReasoningEffort: "low"}
			clientOptions := codex.ClientOptions{SubmissionLedgerPath: filepath.Join(state, "receipts.json")}
			hot := NewWithOptions(socket, workspace, clientOptions)
			original, originalErr := hot.Client.SendWithOptions(context.Background(), "member", "original request", "same-id", "assignment", options)
			hot.Close() // The caller never acknowledges its lost browser reply.
			store := session.RecoveryStore{Root: state, Workspace: workspace}
			if err := store.Set(context.Background(), "example", "root", socket, true, false); err != nil {
				t.Fatal(err)
			}
			cold := NewWithOptions(socket, workspace, clientOptions)
			cold.RecoveryRoot = state
			defer cold.Close()
			receipt, err := cold.SendWithOptions(context.Background(), "member", "original request", "same-id", "assignment", options)
			if unknown {
				var outcome *codex.UnknownSendOutcomeError
				if !errors.As(err, &outcome) || originalErr == nil {
					t.Fatal("unknown hot send was replayed", receipt, err, originalErr)
				}
			} else if err != nil || originalErr != nil || receipt != original {
				t.Fatal("accepted hot receipt changed", receipt, original, err, originalErr)
			}
			if starts != 1 || resumes != 1 || queues != 0 || store.AllowsImplicitResume(socket, "member") {
				t.Fatal("retry submitted or activated work", starts, resumes, queues)
			}
		})
	}
}

func TestColdAssignmentReceiptRemainsKnownAfterExecutionWithoutBrowser(t *testing.T) {
	workspace := t.TempDir()
	cwd := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	queued, started, resumes := false, false, 0
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for {
			request, err := readObject(connection)
			if err != nil {
				return nil
			}
			var result any
			switch request["method"] {
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": "member", "cwd": cwd, "source": "vscode", "updatedAt": time.Now().Unix(), "status": map[string]any{"type": "notLoaded"}, "turns": []any{}}}
			case "thread/queue/add":
				queued = true
				result = map[string]any{"queuedSubmission": map[string]any{"id": "queued", "clientUserMessageId": "assignment-id", "input": []any{map[string]any{"type": "text", "text": "saved assignment"}}}}
			case "thread/queue/list":
				data := []any{}
				if queued {
					data = append(data, map[string]any{"id": "queued", "clientUserMessageId": "assignment-id", "input": []any{map[string]any{"type": "text", "text": "saved assignment"}}})
				}
				result = map[string]any{"data": data, "nextCursor": nil}
			case "thread/resume":
				resumes++
				queued, started = false, true
				result = map[string]any{"thread": map[string]any{"id": "member"}}
			case "thread/turns/list":
				data := []any{}
				if started {
					data = append(data, map[string]any{"id": "turn", "status": "completed"})
				}
				result = map[string]any{"data": data, "nextCursor": nil}
			case "thread/items/list":
				data := []any{}
				if started {
					data = append(data, map[string]any{"turnId": "turn", "item": map[string]any{"id": "item", "type": "userMessage", "clientId": "assignment-id", "content": []any{map[string]any{"type": "text", "text": "saved assignment"}}}})
				}
				result = map[string]any{"data": data, "nextCursor": nil}
			default:
				return fmt.Errorf("assignment unexpectedly used %v", request["method"])
			}
			if err := writeObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
				return nil
			}
		}
	})
	for name, body := range map[string]string{"plan.md": "# Plan\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root\n  socket_path: " + socket + "\n  client_version: 0.160.0\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(workspace, "state")
	store := session.RecoveryStore{Root: state, Workspace: workspace}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Set(ctx, "example", "root", socket, true, false); err != nil {
		t.Fatal(err)
	}
	client := NewWithOptions(socket, workspace, codex.ClientOptions{SubmissionLedgerPath: filepath.Join(state, "receipts.json")})
	client.RecoveryRoot = state
	defer client.Close()
	options := codex.TurnOptions{Model: "saved-model", ReasoningEffort: "low"}
	if _, err := client.SendWithOptions(ctx, "member", "saved assignment", "assignment-id", "assignment", options); err != nil {
		t.Fatal(err)
	}
	observation, err := client.ObserveThreadIdentity(ctx, "member", cwd, "", ArchiveActive)
	if err != nil || !observation.ActivityKnown || !observation.Idle {
		t.Fatal("assignment blocked later observation", observation, err)
	}
	if err := client.RequireSubmissionAttemptsResolved(ctx, "member"); err != nil {
		t.Fatal("assignment blocked lifecycle", err)
	}
	if _, err := client.SendWithOptions(ctx, "member", "saved assignment", "assignment-id", "assignment", options); err != nil {
		t.Fatal(err)
	}
	if resumes != 1 {
		t.Fatal("retry loaded duplicate work", resumes)
	}
	if store.AllowsImplicitResume(socket, "root") || !store.AllowsImplicitResume(socket, "member") {
		t.Fatal("assignment released another recipient")
	}
}
