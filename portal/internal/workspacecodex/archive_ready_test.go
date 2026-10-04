package workspacecodex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/coder/websocket"
)

func TestArchiveDiscoverySendsTheCompleteSelectedSourceKindEnum(t *testing.T) {
	cwd := "/workspace/work/one"
	socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
		if err := handshake(connection); err != nil {
			return err
		}
		request, err := readObject(connection)
		if err != nil || request["method"] != "thread/list" {
			return fmt.Errorf("archive discovery request = %#v, %v", request, err)
		}
		params, ok := request["params"].(map[string]any)
		wantSources := []any{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview",
			"subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}
		if !ok || params["cwd"] != cwd || params["archived"] != false ||
			params["limit"] != float64(3) || !reflect.DeepEqual(params["sourceKinds"], wantSources) {
			return fmt.Errorf("archive discovery lost the complete source filter: %#v", params)
		}
		return writeObject(connection, map[string]any{"id": request["id"], "result": map[string]any{
			"data": []any{map[string]any{"id": "unknown-exec", "cwd": cwd, "source": "exec"}},
		}})
	})
	client := NewWithOptions(socket, "/workspace", codex.ClientOptions{})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	archived := false
	threads, next, err := client.ListThreads(ctx, codex.ThreadListOptions{
		Cwd: cwd, Archived: &archived, Limit: 3, SourceKinds: ArchiveDiscoverySourceKinds(),
	})
	if err != nil || next != nil || len(threads) != 1 || threads[0].ID != "unknown-exec" || threads[0].Source != "exec" {
		t.Fatalf("noninteractive archive discovery = %#v, %v, %v", threads, next, err)
	}
}

func TestRequireRootArchiveReadyUsesReadOnlyActiveAndArchivedProof(t *testing.T) {
	for _, scenario := range []string{"active", "fresh", "archived", "busy", "active queue", "fresh queue", "active queue error", "foreign source", "root state drift"} {
		t.Run(scenario, func(t *testing.T) {
			identity, reader, archivedPath := archiveProofFixture(t)
			metadata := reader.metadata
			metadata.Source = "vscode"
			metadata.Status = map[string]any{"type": "notLoaded"}
			wantState := ArchiveArchived
			if strings.HasPrefix(scenario, "active") || scenario == "busy" {
				wantState = ArchiveActive
				activePath := filepath.Join(identity.CodexHome, "sessions", filepath.Base(archivedPath))
				if err := os.MkdirAll(filepath.Dir(activePath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(activePath, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				metadata.Path = &activePath
			}
			if strings.HasPrefix(scenario, "fresh") {
				metadata.Path = nil
				wantState = ArchiveFresh
			}
			if scenario == "foreign source" {
				metadata.Source = "cli"
			}
			var queueCalls atomic.Int32
			socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
				defer connection.CloseNow()
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
						result = map[string]any{"thread": metadata}
					case "thread/loaded/list":
						result = map[string]any{"data": []any{}, "nextCursor": nil}
					case "thread/turns/list":
						status := "completed"
						if scenario == "busy" {
							status = "inProgress"
						}
						result = map[string]any{"data": []any{map[string]any{"id": "turn-one", "status": status}}}
					case "thread/queue/list":
						queueCalls.Add(1)
						if scenario == "archived" || scenario == "active queue error" {
							if err := writeObject(connection, map[string]any{"id": request["id"], "error": map[string]any{
								"code": -32600, "message": "cold archived thread queue is unavailable",
							}}); err != nil {
								return err
							}
							continue
						}
						queue := []any{}
						if strings.HasSuffix(scenario, "queue") {
							queue = append(queue, map[string]any{"id": "queue-one", "clientUserMessageId": "message-one", "input": []any{}})
						}
						result = map[string]any{"data": queue}
					default:
						return fmt.Errorf("archive preflight attempted unexpected method %v", request["method"])
					}
					if err := writeObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			client := NewWithOptions(socket, filepath.Dir(filepath.Dir(identity.Cwd)), codex.ClientOptions{})
			client.CodexHome = identity.CodexHome
			if scenario == "root state drift" {
				proofs := 0
				client.archiveProof = func(context.Context, string, string, string) (ArchiveState, error) {
					proofs++
					if proofs == 2 {
						return ArchiveActive, nil
					}
					return ArchiveArchived, nil
				}
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			state, err := client.RequireRootArchiveReady(ctx, identity.ThreadID, identity.Cwd)
			if scenario == "active" || scenario == "fresh" || scenario == "archived" {
				if err != nil || state != wantState {
					t.Fatalf("archive-ready proof = %v, %v", state, err)
				}
			} else if err == nil || state != ArchiveUnknown {
				t.Fatalf("unsafe root passed archive-ready proof: %v, %v", state, err)
			}
			if scenario == "archived" && queueCalls.Load() != 0 {
				t.Fatal("cold archived proof called queue/list")
			}
			if strings.Contains(scenario, "queue") && queueCalls.Load() != 1 {
				t.Fatal("ordinary active/fresh proof lost its queue check")
			}
			if _, err := os.Stat(archivedPath); err != nil {
				t.Fatal("preflight mutated the retained rollout")
			}
		})
	}
}

func TestRequireArchiveThreadIdleProvesColdArchivedMembersAndRefusesUncertainState(t *testing.T) {
	for _, scenario := range []string{
		"empty", "completed", "failed", "interrupted", "multipage", "omitted cursor",
		"loaded idle", "loaded active", "loaded despite status", "loaded on recheck", "target on later page",
		"missing loaded data", "null loaded data", "malformed loaded data", "empty loaded id", "empty cursor", "repeated cursor", "loaded failure", "later page failure",
		"status missing", "status null", "status string", "status unknown", "status system error", "status active", "status contradictory", "status drift during file proof",
		"source cli", "source internal", "source object", "source missing", "wrong cwd", "wrong id",
		"turn active", "turn unknown", "turn missing id", "turn null data", "turn failure", "turn on recheck",
		"pending prompt", "unresolved submission", "unresolved queued submission", "unresolved deletion", "project drift", "file replacement", "file write", "missing archive", "path drift",
	} {
		t.Run(scenario, func(t *testing.T) {
			identity, reader, archivedPath := archiveProofFixture(t)
			metadata := reader.metadata
			metadata.Source = "vscode"
			metadata.Status = map[string]any{"type": "notLoaded"}
			var metadataReads, loadedPasses, loadedCalls, turns, queueCalls atomic.Int32
			var preparingAttempts atomic.Bool
			preparingAttempts.Store(true)
			socket := serveUnixWebsocket(t, func(connection *websocket.Conn) error {
				defer connection.CloseNow()
				if err := handshake(connection); err != nil {
					return err
				}
				for {
					request, err := readObject(connection)
					if err != nil {
						return nil
					}
					params, ok := request["params"].(map[string]any)
					if !ok {
						return fmt.Errorf("missing archive read params: %#v", request)
					}
					var result any
					switch request["method"] {
					case "thread/read":
						metadataReads.Add(1)
						if len(params) != 1 || params["threadId"] != identity.ThreadID {
							return fmt.Errorf("archive metadata request = %#v", params)
						}
						row := map[string]any{"id": metadata.ID, "cwd": metadata.Cwd, "projectId": metadata.ProjectID,
							"path": metadata.Path, "source": metadata.Source, "status": metadata.Status}
						switch scenario {
						case "loaded idle":
							row["status"] = map[string]any{"type": "idle"}
						case "loaded active", "status active":
							row["status"] = map[string]any{"type": "active", "activeFlags": []any{}}
						case "status missing":
							delete(row, "status")
						case "status null":
							row["status"] = nil
						case "status string":
							row["status"] = "notLoaded"
						case "status unknown":
							row["status"] = map[string]any{"type": "futureStatus"}
						case "status system error":
							row["status"] = map[string]any{"type": "systemError"}
						case "status contradictory":
							row["status"] = map[string]any{"type": "notLoaded", "activeFlags": []any{"waitingOnApproval"}}
						case "status drift during file proof":
							if metadataReads.Load() >= 4 {
								row["status"] = map[string]any{"type": "idle"}
							}
						case "source cli":
							row["source"] = "cli"
						case "source internal":
							row["source"] = "subAgent"
						case "source object":
							row["source"] = map[string]any{"subAgent": "review"}
						case "source missing":
							delete(row, "source")
						case "wrong cwd":
							row["cwd"] = "/foreign"
						case "wrong id":
							row["id"] = proofProjectID
						case "project drift":
							if turns.Load() > 0 {
								row["projectId"] = proofThreadID
							}
						case "path drift":
							if turns.Load() > 0 {
								row["path"] = filepath.Join(identity.CodexHome, "sessions", filepath.Base(archivedPath))
							}
						}
						result = map[string]any{"thread": row}
					case "thread/loaded/list":
						loadedCalls.Add(1)
						cursor, _ := params["cursor"].(string)
						wantParams := map[string]any{"limit": float64(100)}
						if cursor != "" {
							wantParams["cursor"] = cursor
						}
						if !reflect.DeepEqual(params, wantParams) {
							return fmt.Errorf("loaded list lost public selector: %#v", params)
						}
						if cursor == "" {
							loadedPasses.Add(1)
						} else if cursor != "cursor-one" {
							return fmt.Errorf("loaded list lost opaque cursor: %#v", params)
						}
						page := map[string]any{"data": []any{}, "nextCursor": nil}
						switch scenario {
						case "loaded idle", "loaded active", "loaded despite status":
							page["data"] = []any{identity.ThreadID}
						case "loaded on recheck":
							if loadedPasses.Load() == 2 {
								page["data"] = []any{identity.ThreadID}
							}
						case "missing loaded data":
							delete(page, "data")
						case "null loaded data":
							page["data"] = nil
						case "malformed loaded data":
							page["data"] = []any{42}
						case "loaded failure":
							if err := writeObject(connection, map[string]any{"id": request["id"], "error": map[string]any{"code": -32603, "message": "loaded list unavailable"}}); err != nil {
								return err
							}
							continue
						case "empty loaded id":
							page["data"] = []any{""}
						case "empty cursor":
							page["nextCursor"] = ""
						case "omitted cursor":
							delete(page, "nextCursor")
						case "multipage", "target on later page", "repeated cursor", "later page failure":
							if cursor == "" || scenario == "repeated cursor" {
								page["data"], page["nextCursor"] = []any{proofProjectID}, "cursor-one"
								if scenario == "multipage" && loadedPasses.Load() == 2 {
									page["data"] = []any{"33333333-3333-7333-8333-333333333333"}
								}
							} else if scenario == "target on later page" {
								page["data"] = []any{identity.ThreadID}
							} else if scenario == "later page failure" {
								if err := writeObject(connection, map[string]any{"id": request["id"], "error": map[string]any{"code": -32603, "message": "loaded page unavailable"}}); err != nil {
									return err
								}
								continue
							}
						}
						if scenario == "pending prompt" && loadedPasses.Load() == 1 {
							if err := writeObject(connection, map[string]any{
								"id": "input-one", "method": "item/tool/requestUserInput", "params": map[string]any{
									"threadId": identity.ThreadID, "turnId": "turn-one", "itemId": "item-one",
									"questions": []any{map[string]any{"id": "choice", "header": "Choice", "question": "Continue?"}},
								},
							}); err != nil {
								return err
							}
						}
						result = page
					case "thread/turns/list":
						if !reflect.DeepEqual(params, map[string]any{"threadId": identity.ThreadID, "limit": float64(1), "sortDirection": "desc", "itemsView": "notLoaded"}) {
							return fmt.Errorf("latest turn lost public selector: %#v", params)
						}
						turns.Add(1)
						status := "completed"
						if scenario == "failed" || scenario == "interrupted" {
							status = scenario
						} else if scenario == "turn active" || scenario == "turn on recheck" && turns.Load() == 2 {
							status = "inProgress"
						} else if scenario == "turn unknown" {
							status = "futureStatus"
						}
						turn := map[string]any{"id": "turn-one", "status": status}
						data := []any{turn}
						if scenario == "empty" {
							data = []any{}
						} else if scenario == "turn missing id" {
							delete(turn, "id")
						}
						result = map[string]any{"data": data}
						if scenario == "turn null data" {
							result = map[string]any{"data": nil}
						} else if scenario == "turn failure" {
							if err := writeObject(connection, map[string]any{"id": request["id"], "error": map[string]any{"code": -32603, "message": "history unavailable"}}); err != nil {
								return err
							}
							continue
						}
						if turns.Load() == 1 {
							switch scenario {
							case "file replacement":
								bytes, err := os.ReadFile(archivedPath)
								if err != nil {
									return err
								}
								replacement := archivedPath + ".replacement"
								if err := os.WriteFile(replacement, bytes, 0o600); err != nil {
									return err
								}
								if err := os.Rename(replacement, archivedPath); err != nil {
									return err
								}
							case "file write":
								file, err := os.OpenFile(archivedPath, os.O_APPEND|os.O_WRONLY, 0)
								if err != nil {
									return err
								}
								_, err = file.WriteString("{}\n")
								closeErr := file.Close()
								if err != nil {
									return err
								}
								if closeErr != nil {
									return closeErr
								}
							case "missing archive":
								if err := os.Remove(archivedPath); err != nil {
									return err
								}
							}
						}
					case "thread/items/list":
						result = map[string]any{"data": []any{}, "nextCursor": nil}
					case "thread/queue/list":
						if preparingAttempts.Load() && scenario == "unresolved deletion" {
							result = map[string]any{"data": []any{map[string]any{
								"id": "queue-one", "clientUserMessageId": "client-one",
								"input": []any{map[string]any{"type": "text", "text": "pending"}},
							}}, "nextCursor": nil}
							break
						}
						queueCalls.Add(1)
						if err := writeObject(connection, map[string]any{"id": request["id"], "error": map[string]any{"code": -32600, "message": "cold archived thread queue is unavailable"}}); err != nil {
							return err
						}
						continue
					case "thread/queue/add":
						if !preparingAttempts.Load() || scenario != "unresolved queued submission" {
							return fmt.Errorf("archive proof attempted queue mutation")
						}
						result = map[string]any{"queuedSubmission": map[string]any{
							"id": "queue-one", "clientUserMessageId": "client-one",
							"input": []any{map[string]any{"type": "text", "text": "pending"}},
						}}
					case "thread/queue/delete":
						if !preparingAttempts.Load() || scenario != "unresolved deletion" {
							return fmt.Errorf("archive proof attempted queue deletion")
						}
						if err := writeObject(connection, map[string]any{"id": request["id"], "error": map[string]any{"code": -32603, "message": "lost deletion acknowledgement"}}); err != nil {
							return err
						}
						continue
					default:
						return fmt.Errorf("archive idle proof attempted mutation/unexpected RPC %v", request["method"])
					}
					if err := writeObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			client := NewWithOptions(socket, filepath.Dir(filepath.Dir(identity.Cwd)), codex.ClientOptions{
				SubmissionLedgerPath: filepath.Join(t.TempDir(), "attempts.json"),
			})
			client.CodexHome = identity.CodexHome
			defer client.Close()
			if scenario == "unresolved submission" {
				if err := client.PrepareSendWithOptions(identity.ThreadID, "pending", "client-one", "", false, codex.TurnOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Establish prior attempts through public client APIs in this fake
			// fixture, then freeze its queue interface before read-only proof.
			if scenario == "unresolved queued submission" {
				if _, err := client.Queue(ctx, identity.ThreadID, "pending", "client-one"); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "unresolved deletion" {
				if err := client.DeleteQueueEntry(ctx, identity.ThreadID, "queue-one"); err == nil {
					t.Fatal("fixture unexpectedly acknowledged queue deletion")
				}
			}
			preparingAttempts.Store(false)
			// The member caller's recorded project proof is independent of the
			// archived/unloaded helper's mandatory vscode and idle checks.
			state, err := client.ProveArchivedThread(ctx, identity.ThreadID, identity.Cwd, identity.ProjectID)
			if err == nil {
				err = client.RequireArchiveThreadIdle(ctx, identity.ThreadID, identity.Cwd, state)
			}
			wantSuccess := scenario == "empty" || scenario == "completed" || scenario == "failed" || scenario == "interrupted" || scenario == "multipage" || scenario == "omitted cursor"
			if wantSuccess && err != nil || !wantSuccess && err == nil {
				t.Fatalf("archived %s proof = %v", scenario, err)
			}
			if queueCalls.Load() != 0 {
				t.Fatal("archived proof called queue/list instead of positive unloaded proof")
			}
			if wantSuccess && (loadedPasses.Load() != 2 || turns.Load() != 2) {
				t.Fatalf("proof lost bracketing reads: loaded=%d turns=%d", loadedPasses.Load(), turns.Load())
			}
			if scenario == "multipage" && loadedCalls.Load() != 4 {
				t.Fatalf("loaded absence skipped a page: calls=%d", loadedCalls.Load())
			}
		})
	}
}

func TestRequireRootArchiveReadyRejectsUnknownState(t *testing.T) {
	client := NewWithOptions("", "/workspace", codex.ClientOptions{})
	defer client.Close()
	client.archiveProof = func(context.Context, string, string, string) (ArchiveState, error) { return ArchiveUnknown, nil }
	if state, err := client.RequireRootArchiveReady(context.Background(), proofThreadID, "/workspace/work/one"); err == nil || state != ArchiveUnknown {
		t.Fatalf("unknown root state passed: %v, %v", state, err)
	}
}
