package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/coder/websocket"
)

func TestResolveNewThreadSettingsOwnsWorkspaceDefaults(t *testing.T) {
	models := []codex.Model{
		{
			Model: DefaultNewThreadModel, DisplayName: "GPT-6 Astra",
			DefaultReasoningEffort: "medium",
			SupportedReasoningEfforts: []codex.ReasoningEffortOption{
				{ReasoningEffort: "medium"}, {ReasoningEffort: "xhigh"},
			},
		},
		{
			Model: "bounded", DisplayName: "Bounded", DefaultReasoningEffort: "high",
			SupportedReasoningEfforts: []codex.ReasoningEffortOption{
				{ReasoningEffort: "medium"}, {ReasoningEffort: "high"},
			},
		},
	}

	settings, err := ResolveNewThreadSettings(models, codex.ThreadSettings{})
	if err != nil || settings.Model != DefaultNewThreadModel || settings.ReasoningEffort != "xhigh" {
		t.Fatalf("default settings = %#v, %v", settings, err)
	}
	settings, err = ResolveNewThreadSettings(models, codex.ThreadSettings{Model: "bounded"})
	if err != nil || settings.Model != "bounded" || settings.ReasoningEffort != "high" {
		t.Fatalf("bounded settings = %#v, %v", settings, err)
	}
}

func TestNewWithOptionsRetainsWorkspaceNonblockingInputPolicy(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		return writeObject(connection, map[string]any{
			"id": "input-1", "method": "item/tool/requestUserInput",
			"params": map[string]any{
				"threadId": "thread-1", "turnId": "turn-1", "itemId": "item-1",
				"isBlocking": false, "autoResolutionMs": 5,
				"questions": []any{map[string]any{
					"id": "choice", "header": "Choice", "question": "Choose",
				}},
			},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(client.Prompts("thread-1")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	prompts := client.Prompts("thread-1")
	if len(prompts) != 1 {
		t.Fatalf("nonblocking prompts = %#v", prompts)
	}
	prompt := prompts[0]
	if prompt.IsBlocking ||
		prompt.AutoResolutionVisibleAtMS < time.Now().Add(40*time.Second).UnixMilli() ||
		prompt.AutoResolutionAtMS < time.Now().Add(100*time.Second).UnixMilli() ||
		prompt.AutoResolutionVisibleAtMS >= prompt.AutoResolutionAtMS {
		t.Fatalf("workspace nonblocking prompt deadline = %#v", prompt)
	}
}

func TestListThreadActivityAcceptsOnlyWorkspaceThreads(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil || request["method"] != "thread/read" {
			return fmt.Errorf("expected thread/read: %#v, %v", request, err)
		}
		return writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"thread": map[string]any{
				"id": "thread-1", "cwd": "/workspace/work/one",
				"source": "vscode", "updatedAt": int64(100),
			}},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	activities, err := client.ListThreadActivity(ctx, []ThreadActivity{{
		ID: "thread-1", Cwd: "/workspace/work/one",
	}})
	if err != nil || len(activities) != 1 || activities[0].UpdatedAt.Unix() != 100 {
		t.Fatalf("activities = %#v, %v", activities, err)
	}
}

func TestRetireThreadInterruptsAnActiveTurnBeforeArchiving(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index, expected := range []string{
			"thread/list", "thread/read", "thread/read", "thread/turns/list",
			"thread/turns/list", "turn/interrupt", "thread/turns/list", "thread/archive",
		} {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			if request["method"] != expected {
				return fmt.Errorf("request %d = %v, want %s", index, request["method"], expected)
			}
			params, _ := request["params"].(map[string]any)
			if index > 0 && params["threadId"] != "thread-1" {
				return fmt.Errorf("wrong retirement thread: %#v", request)
			}
			var result any = map[string]any{}
			switch index {
			case 0:
				result = map[string]any{"data": []any{map[string]any{
					"id": "thread-1", "cwd": "/workspace/work/example", "source": "vscode",
				}}}
			case 1:
				result = map[string]any{"thread": map[string]any{
					"id": "thread-1", "cwd": "/workspace/work/example", "source": "vscode",
				}}
			case 2:
				result = map[string]any{"thread": map[string]any{
					"id": "thread-1", "cwd": "/workspace/work/example", "source": "vscode",
					"path": rollout,
				}}
			case 3, 4:
				result = map[string]any{"data": []any{map[string]any{
					"id": "turn-1", "status": "inProgress",
				}}}
			case 5:
				if params["turnId"] != "turn-1" {
					return fmt.Errorf("wrong interrupted turn: %#v", request)
				}
			case 6:
				result = map[string]any{"data": []any{map[string]any{
					"id": "turn-1", "status": "interrupted",
				}}}
			}
			if err := writeObject(connection, map[string]any{
				"id": request["id"], "result": result,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.RetireThread(ctx, "thread-1", "/workspace/work/example", true); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverForkThreadResumesMatchingPersistedFork(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index := 0; index < 3; index++ {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			params, _ := request["params"].(map[string]any)
			switch index {
			case 0:
				if request["method"] != "thread/list" || params["cwd"] != "/workspace/work/fork" {
					return fmt.Errorf("invalid recovery lookup: %#v", request)
				}
				err = writeObject(connection, map[string]any{"id": request["id"], "result": map[string]any{
					"data": []any{map[string]any{
						"id": "thread-fork", "cwd": "/workspace/work/fork", "forkedFromId": "thread-source",
					}},
				}})
			case 1:
				if request["method"] != "thread/turns/list" || params["threadId"] != "thread-fork" {
					return fmt.Errorf("invalid fork idle check: %#v", request)
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"data": []any{}},
				})
			case 2:
				if request["method"] != "thread/resume" || params["threadId"] != "thread-fork" ||
					params["cwd"] != "/workspace/work/fork" {
					return fmt.Errorf("invalid recovered fork resume: %#v", request)
				}
				err = writeObject(connection, map[string]any{"id": request["id"], "result": map[string]any{
					"thread": map[string]any{"id": "thread-fork", "cwd": "/workspace/work/fork"},
				}})
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := client.RecoverForkThread(
		ctx, "thread-source", "/workspace/work/fork",
		map[string]string{"VPSFREE_DEV_SESSION_WORKSPACE": "/workspace"}, codex.ThreadSettings{},
	)
	if err != nil || id != "thread-fork" {
		t.Fatalf("recovered fork = %q, %v", id, err)
	}
}

func TestRecoverArchivedThreadUnarchivesAndResumesExactIdentity(t *testing.T) {
	threadID := "thread-archived"
	cwd := "/workspace/work/revived"
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index, expected := range []string{"thread/list", "thread/list", "thread/unarchive", "thread/resume"} {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			if request["method"] != expected {
				return fmt.Errorf("request %d = %#v, want %s", index, request, expected)
			}
			params := request["params"].(map[string]any)
			switch index {
			case 0, 1:
				if params["cwd"] != cwd || params["archived"] != (index == 1) {
					return fmt.Errorf("thread lookup %d = %#v", index, params)
				}
				data := []any{}
				if index == 1 {
					data = append(data,
						map[string]any{"id": "older-thread", "cwd": cwd, "source": "vscode"},
						map[string]any{"id": threadID, "cwd": cwd, "source": "vscode"},
					)
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"data": data},
				})
			case 2:
				if params["threadId"] != threadID {
					return fmt.Errorf("unarchive params = %#v", params)
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"thread": map[string]any{
						"id": threadID, "cwd": cwd, "source": "vscode",
					}},
				})
			case 3:
				if params["threadId"] != threadID || params["cwd"] != cwd {
					return fmt.Errorf("resume params = %#v", params)
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"thread": map[string]any{
						"id": threadID, "cwd": cwd,
					}},
				})
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := client.RecoverArchivedThread(
		ctx, threadID, cwd, map[string]string{"VPSFREE_DEV_SESSION_WORKSPACE": "/workspace"},
	)
	if err != nil || id != threadID {
		t.Fatalf("recovered thread = %q, %v", id, err)
	}
}

func TestRecoverArchivedThreadRetryResumesAlreadyActiveIdentity(t *testing.T) {
	threadID := "thread-active"
	cwd := "/workspace/work/revived"
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index, expected := range []string{"thread/list", "thread/list", "thread/resume"} {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			if request["method"] != expected {
				return fmt.Errorf("request %d = %#v, want %s", index, request, expected)
			}
			params := request["params"].(map[string]any)
			if index < 2 {
				data := []any{}
				if index == 0 {
					data = append(data, map[string]any{"id": threadID, "cwd": cwd, "source": "vscode"})
				} else {
					data = append(data,
						map[string]any{"id": "older-thread-1", "cwd": cwd, "source": "vscode"},
						map[string]any{"id": "older-thread-2", "cwd": cwd, "source": "vscode"},
					)
				}
				if err := writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"data": data},
				}); err != nil {
					return err
				}
				continue
			}
			if params["threadId"] != threadID || params["cwd"] != cwd {
				return fmt.Errorf("resume params = %#v", params)
			}
			return writeObject(connection, map[string]any{
				"id": request["id"], "result": map[string]any{"thread": map[string]any{
					"id": threadID, "cwd": cwd,
				}},
			})
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := client.RecoverArchivedThread(ctx, threadID, cwd, nil)
	if err != nil || id != threadID {
		t.Fatalf("retried recovery = %q, %v", id, err)
	}
}

func TestRecoverArchivedThreadRejectsAnotherDirectoryIdentity(t *testing.T) {
	cwd := "/workspace/work/revived"
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index := 0; index < 2; index++ {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			data := []any{}
			if index == 0 {
				data = append(data, map[string]any{"id": "thread-other", "cwd": cwd, "source": "vscode"})
			}
			if err := writeObject(connection, map[string]any{
				"id": request["id"], "result": map[string]any{"data": data},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.RecoverArchivedThread(ctx, "thread-expected", cwd, nil)
	if err == nil || !strings.Contains(err.Error(), "another active Codex thread") {
		t.Fatalf("identity error = %v", err)
	}
}

func TestRecoverCreatingThreadIgnoresLoadedStructuredSources(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index := 0; index < 4; index++ {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			switch index {
			case 0:
				if request["method"] != "thread/loaded/list" {
					return fmt.Errorf("expected thread/loaded/list, got %v", request["method"])
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{
						"data": []any{"thread-subagent"}, "nextCursor": nil,
					},
				})
			case 1:
				if request["method"] != "thread/read" {
					return fmt.Errorf("expected thread/read, got %v", request["method"])
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"thread": map[string]any{
						"id": "thread-subagent", "cwd": "/workspace/work/other",
						"source": map[string]any{"subAgent": map[string]any{"threadSpawn": "agent"}},
					}},
				})
			case 2:
				if request["method"] != "thread/list" {
					return fmt.Errorf("expected thread/list, got %v", request["method"])
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"data": []any{}},
				})
			case 3:
				if request["method"] != "thread/start" {
					return fmt.Errorf("expected thread/start, got %v", request["method"])
				}
				err = writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"thread": map[string]any{
						"id": "thread-new", "cwd": "/workspace/work/example",
					}},
				})
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := client.RecoverCreatingThread(
		ctx, "", "/workspace/work/example",
		map[string]string{"VPSFREE_DEV_SESSION_WORKSPACE": "/workspace"},
	)
	if err != nil || id != "thread-new" {
		t.Fatalf("structured unrelated source recovery = %q, %v", id, err)
	}
}

func TestRetireThreadRefusesAmbiguousCwdLookup(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil {
			return err
		}
		return writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{
				"data": []any{
					map[string]any{"id": "thread-1", "cwd": "/workspace/work/example"},
					map[string]any{"id": "thread-2", "cwd": "/workspace/work/example"},
				},
			},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.RetireThread(ctx, "", "/workspace/work/example", false); err == nil ||
		!strings.Contains(err.Error(), "ambiguous retirement") {
		t.Fatalf("ambiguous retirement error = %v", err)
	}
}

func TestRetireThreadRefusesAnotherActiveThreadBesideTheExpectedOne(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil {
			return err
		}
		return writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{
				"data": []any{
					map[string]any{
						"id": "thread-orphan", "cwd": "/workspace/work/example", "source": "vscode",
					},
					map[string]any{
						"id": "thread-expected", "cwd": "/workspace/work/example", "source": "vscode",
					},
				},
			},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.RetireThread(
		ctx, "thread-expected", "/workspace/work/example", true,
	); err == nil || !strings.Contains(err.Error(), "ambiguous retirement") {
		t.Fatalf("active sibling retirement error = %v", err)
	}
}

func TestRetireThreadTreatsMissingCwdCandidateAsAlreadyRetired(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil {
			return err
		}
		params := request["params"].(map[string]any)
		if request["method"] != "thread/list" || params["archived"] != false {
			return fmt.Errorf("request = %#v", request)
		}
		return writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"data": []any{}},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.RetireThread(ctx, "", "/workspace/work/example", false); err != nil {
		t.Fatal(err)
	}
}

func TestRetireThreadRecoversPersistedIdentityAmongArchivedCwdCandidates(t *testing.T) {
	cwd := "/workspace/work/example"
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index := 0; index < 2; index++ {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			params := request["params"].(map[string]any)
			if request["method"] != "thread/list" || params["archived"] != (index == 1) {
				return fmt.Errorf("request %d = %#v", index, request)
			}
			data := []any{}
			if index == 1 {
				data = []any{
					map[string]any{"id": "thread-old", "cwd": cwd, "source": "vscode"},
					map[string]any{"id": "thread-1", "cwd": cwd, "source": "vscode"},
				}
			}
			if err := writeObject(connection, map[string]any{
				"id": request["id"], "result": map[string]any{"data": data},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	if err := client.RecordThreadOperationAttempt(cwd, "thread-1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.RetireThread(ctx, "", cwd, false); err != nil {
		t.Fatal(err)
	}
	threadID, err := client.ThreadOperationAttempt(cwd)
	if err != nil || threadID != "" {
		t.Fatalf("retirement marker after cleanup = %q, %v", threadID, err)
	}
}

func TestRetireThreadArchivesAFreshThreadWithoutARollout(t *testing.T) {
	threadID := "thread-fresh"
	cwd := "/workspace/work/fresh"
	rollout := filepath.Join(t.TempDir(), "missing-rollout.jsonl")
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		for index, expected := range []string{
			"thread/list", "thread/read", "thread/read", "thread/archive",
		} {
			request, err := readObject(connection)
			if err != nil {
				return err
			}
			if request["method"] != expected {
				return fmt.Errorf("request %d = %#v, want %s", index, request, expected)
			}
			if index == 0 {
				if err := writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"data": []any{map[string]any{
						"id": threadID, "cwd": cwd, "source": "vscode",
					}}},
				}); err != nil {
					return err
				}
				continue
			}
			if index == 1 || index == 2 {
				if err := writeObject(connection, map[string]any{
					"id": request["id"], "result": map[string]any{"thread": map[string]any{
						"id": threadID, "cwd": cwd, "source": "vscode", "status": map[string]any{"type": "idle"},
						"historyMode": "paginated", "preview": "", "forkedFromId": "",
						"ephemeral": false, "path": rollout, "turns": []any{},
					}},
				}); err != nil {
					return err
				}
				continue
			}
			if err := writeObject(connection, map[string]any{
				"id": request["id"], "result": map[string]any{},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.RetireThread(ctx, threadID, cwd, false); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverCreatingThreadResumesPersistedOwnerWithRuntimeConfiguration(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("materialized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"VPSFREE_DEV_SESSION_SLUG":            "example",
		"VPSFREE_DEV_SESSION_WORKSPACE":       "/workspace",
		"VPSFREE_DEV_SESSION_WORK_DIR":        "/workspace/work/example",
		"VPSFREE_DEV_SESSION_WORKTREES_DIR":   "/workspace/worktrees/example",
		"VPSFREE_DEV_SESSION_PORTAL_BASE_URL": "https://workspace.example",
		"VPSFREE_DEV_SESSION_URL":             "https://workspace.example/example/",
	}
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil || request["method"] != "thread/loaded/list" {
			return fmt.Errorf("expected persisted thread/loaded/list: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"data": []any{}, "nextCursor": nil},
		}); err != nil {
			return err
		}
		request, err = readObject(connection)
		if err != nil || request["method"] != "thread/list" {
			return fmt.Errorf("expected persisted thread/list: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"data": []any{map[string]any{
				"id": "thread-original", "cwd": "/workspace/work/example",
			}}},
		}); err != nil {
			return err
		}
		request, err = readObject(connection)
		if err != nil || request["method"] != "thread/read" {
			return fmt.Errorf("expected persisted thread/read: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{
				"thread": freshThreadMetadata("thread-original", "/workspace/work/example", rollout),
			},
		}); err != nil {
			return err
		}
		request, err = readObject(connection)
		if err != nil || request["method"] != "thread/resume" {
			return fmt.Errorf("expected persisted thread/resume: %v", err)
		}
		params := request["params"].(map[string]any)
		if params["threadId"] != "thread-original" ||
			params["cwd"] != "/workspace/work/example" || params["excludeTurns"] != true {
			return fmt.Errorf("invalid persisted resume params: %#v", params)
		}
		config := params["config"].(map[string]any)
		if _, ok := config["model"]; ok {
			return fmt.Errorf("persisted thread model was overwritten: %#v", config)
		}
		if _, ok := config["model_reasoning_effort"]; ok {
			return fmt.Errorf("persisted thread reasoning was overwritten: %#v", config)
		}
		policy := config["shell_environment_policy"].(map[string]any)
		set := policy["set"].(map[string]any)
		if set["VPSFREE_DEV_SESSION_PORTAL_BASE_URL"] != "https://workspace.example" {
			return fmt.Errorf("persisted thread runtime environment was not refreshed: %#v", set)
		}
		return writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"thread": map[string]any{
				"id": "thread-original", "cwd": "/workspace/work/example",
			}},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resolverCalled := false
	id, err := client.RecoverCreatingThreadWithSettingsResolver(
		ctx, "thread-original", "/workspace/work/example", environment,
		func() (codex.ThreadSettings, error) {
			resolverCalled = true
			return codex.ThreadSettings{}, errors.New("the old model is no longer in the catalog")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if id != "thread-original" {
		t.Fatalf("persisted thread id = %q", id)
	}
	if resolverCalled {
		t.Fatal("materialized recovery resolved replacement settings")
	}
}

func TestRecoverCreatingThreadReplacesPersistedOwnerMissingAfterRestart(t *testing.T) {
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil || request["method"] != "thread/loaded/list" {
			return fmt.Errorf("expected restart thread/loaded/list: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"data": []any{}, "nextCursor": nil},
		}); err != nil {
			return err
		}
		request, err = readObject(connection)
		if err != nil || request["method"] != "thread/list" {
			return fmt.Errorf("expected restart thread/list: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"data": []any{}},
		}); err != nil {
			return err
		}
		request, err = readObject(connection)
		if err != nil || request["method"] != "thread/start" {
			return fmt.Errorf("expected replacement thread/start: %v", err)
		}
		return writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"thread": map[string]any{
				"id": "thread-replacement", "cwd": "/workspace/work/example",
			}},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := client.RecoverCreatingThread(
		ctx, "thread-vanished", "/workspace/work/example", map[string]string{
			"VPSFREE_DEV_SESSION_WORKSPACE": "/workspace",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if id != "thread-replacement" {
		t.Fatalf("replacement thread id = %q", id)
	}
}

func TestRecoverCreatingThreadRefusesAmbiguousDirectoryCandidates(t *testing.T) {
	cwd := "/workspace/work/example"
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		loaded, err := readObject(connection)
		if err != nil || loaded["method"] != "thread/loaded/list" {
			return fmt.Errorf("expected thread/loaded/list: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": loaded["id"], "result": map[string]any{"data": []any{}, "nextCursor": nil},
		}); err != nil {
			return err
		}
		listed, err := readObject(connection)
		if err != nil || listed["method"] != "thread/list" {
			return fmt.Errorf("expected thread/list: %v", err)
		}
		return writeObject(connection, map[string]any{
			"id": listed["id"], "result": map[string]any{"data": []any{
				map[string]any{"id": "thread-1", "cwd": cwd, "source": "vscode"},
				map[string]any{"id": "thread-2", "cwd": cwd, "source": "vscode"},
			}},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.RecoverCreatingThread(ctx, "", cwd, nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous recovery") {
		t.Fatalf("ambiguous recovery error = %v", err)
	}
}

func TestRecoverCreatingThreadRejectsDifferentMaterializedCandidate(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("materialized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil || request["method"] != "thread/loaded/list" {
			return fmt.Errorf("expected thread/loaded/list: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"data": []any{}, "nextCursor": nil},
		}); err != nil {
			return err
		}
		request, err = readObject(connection)
		if err != nil || request["method"] != "thread/list" {
			return fmt.Errorf("expected thread/list: %v", err)
		}
		if err := writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{"data": []any{map[string]any{
				"id": "thread-unrelated", "cwd": "/workspace/work/example",
			}}},
		}); err != nil {
			return err
		}
		request, err = readObject(connection)
		if err != nil || request["method"] != "thread/read" {
			return fmt.Errorf("expected thread/read: %v", err)
		}
		return writeObject(connection, map[string]any{
			"id": request["id"], "result": map[string]any{
				"thread": freshThreadMetadata("thread-unrelated", "/workspace/work/example", rollout),
			},
		})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.RecoverCreatingThread(
		ctx,
		"thread-vanished",
		"/workspace/work/example",
		map[string]string{"VPSFREE_DEV_SESSION_WORKSPACE": "/workspace"},
	)
	if err == nil || !strings.Contains(err.Error(), "different materialized") {
		t.Fatalf("different materialized candidate result = %v", err)
	}
}

func freshThreadMetadata(threadID, cwd string, path any) map[string]any {
	return map[string]any{
		"id": threadID, "cwd": cwd, "path": path, "preview": "", "source": "vscode",
		"ephemeral": false, "historyMode": "paginated", "status": map[string]any{"type": "idle"},
		"turns": []any{},
	}
}

func handshake(connection *websocket.Conn) error {
	request, err := readObject(connection)
	if err != nil {
		return err
	}
	params, _ := request["params"].(map[string]any)
	capabilities, _ := params["capabilities"].(map[string]any)
	if capabilities["experimentalApi"] != true {
		return errors.New("initialize did not enable the experimental API")
	}
	clientInfo, _ := params["clientInfo"].(map[string]any)
	if clientInfo["name"] != "codex-web" || clientInfo["version"] != "0.1.0" ||
		clientInfo["Name"] != nil {
		return fmt.Errorf("initialize used invalid clientInfo: %#v", clientInfo)
	}
	if err := writeObject(connection, map[string]any{
		"id": request["id"], "result": map[string]any{"userAgent": "codex-cli/99.0.0"},
	}); err != nil {
		return err
	}
	_, err = readObject(connection)
	return err
}

func serveUnixWebsocket(t *testing.T, handler func(*websocket.Conn) error) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "workspacecodex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "app-server.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	errorsChannel := make(chan error, 8)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			errorsChannel <- err
			return
		}
		connection.SetReadLimit(8 << 20)
		go func() {
			if err := handler(connection); err != nil {
				errorsChannel <- err
			}
		}()
	})}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errorsChannel <- err
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-errorsChannel:
			t.Errorf("fake App Server: %v", err)
		default:
		}
	})
	return socket
}

func readObject(connection *websocket.Conn) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := connection.Read(ctx)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	err = json.Unmarshal(data, &value)
	return value, err
}

func writeObject(connection *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return connection.Write(ctx, websocket.MessageText, data)
}
