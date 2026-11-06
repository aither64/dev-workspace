package workspacecodex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
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

func TestColdAssignmentAndProgressReceiptsRemainKnownAfterExecutionWithoutBrowser(t *testing.T) {
	for _, progress := range []bool{false, true} {
		for _, mode := range []string{"auto", "fresh-hidden", "initial-cancel", "invalid-ack", "lost-ack", "prior-unknown", "idle", "foreign", "active", "changed-text", "changed-id", "unknown", "lost-reconciled", "cancelled", "race-consumed", "race-unknown", "missing-history", "epoch", "permission", "maintenance"} {
			t.Run(fmt.Sprintf("progress=%t/%s", progress, mode), func(t *testing.T) {
				workspace := t.TempDir()
				cwd := filepath.Join(workspace, "work", "example")
				if err := os.MkdirAll(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				queued, started, loaded := false, false, false
				historyVisible := true
				queues, starts, resumes, loadedLists := 0, 0, 0, 0
				var client *Client
				var store session.RecoveryStore
				var socket string
				var cancelSend context.CancelFunc
				policy := codex.ThreadPolicy{DeveloperInstructions: "retained member", Sandbox: "danger-full-access", ApprovalPolicy: "never", MCPServer: &codex.ThreadMCPServer{Name: "team", Command: "/package/bin/workspace-portal", Args: []string{"team-mcp", "--slug", "example"}, Tool: "report_to_lead"}}
				socket = serveUnixWebsocket(t, func(connection *websocket.Conn) error {
					if err := handshake(connection); err != nil {
						return err
					}
					for {
						request, err := readObject(connection)
						if err != nil {
							return nil
						}
						mu.Lock()
						var result any
						failure := false
						params, _ := request["params"].(map[string]any)
						switch request["method"] {
						case "thread/read":
							status := "notLoaded"
							if loaded {
								status = "idle"
							}
							result = map[string]any{"thread": map[string]any{"id": "member", "cwd": cwd, "source": "vscode", "modelProvider": "fixture", "model": "saved-model", "reasoningEffort": "low", "updatedAt": time.Now().Unix(), "historyMode": "paginated", "status": map[string]any{"type": status}, "turns": []any{}}}
						case "thread/queue/add":
							queues++
							if params["threadId"] != "member" || params["clientUserMessageId"] != "assignment-id" || fmt.Sprint(params["input"]) != "[map[text:saved assignment type:text]]" {
								t.Error("queue changed original identity", params)
							}
							queued = mode != "invalid-ack" && mode != "lost-ack" && mode != "prior-unknown"
							result = map[string]any{"queuedSubmission": map[string]any{"id": "queued", "clientUserMessageId": "assignment-id", "input": []any{map[string]any{"type": "text", "text": "saved assignment"}}}}
							if mode == "invalid-ack" || mode == "prior-unknown" {
								result = map[string]any{}
							}
						case "thread/queue/list":
							if loaded {
								loadedLists++
								if loadedLists == 1 {
									if !store.AllowsImplicitResume(socket, "member") || store.AllowsImplicitResume(socket, "root") {
										t.Error("queue completion preceded addressed permission")
									}
									switch mode {
									case "initial-cancel":
										cancelSend()
									case "epoch":
										if err := os.Chmod(socket, 0600); err != nil {
											t.Error(err)
										}
									case "permission":
										if err := store.Update(context.Background(), "example", func(r *session.Recovery) error { r.ActiveThreads = []string{}; return nil }); err != nil {
											t.Error(err)
										}
									case "maintenance":
										m := maintenance.Store{Root: client.RecoveryRoot, Profile: client.maintenanceProfile()}
										op := maintenance.Operation{Schema: maintenance.Schema, OperationID: "11111111-1111-4111-8111-111111111111", Revision: 1, Phase: "pausing", Profile: client.maintenanceProfile(), SourceProfileToken: "token", SourcePackage: "/source", CandidatePackage: "/candidate", PlansDigest: "plans", Services: []maintenance.Service{{Workspace: workspace, Unit: "native.service", Native: "/native", PID: 123, ProcessIdentity: "pid", Socket: socket, SocketEpoch: "epoch"}}, Participants: []maintenance.Participant{}, Blockers: []string{}}
										if err := m.Create(context.Background(), op); err != nil {
											t.Error(err)
										}
									}
								}
								if loadedLists == 2 && (mode == "race-consumed" || mode == "race-unknown") {
									queued, started = false, mode == "race-consumed"
								}
							}
							data := []any{}
							if mode == "foreign" && loaded {
								data = append(data, map[string]any{"id": "foreign", "clientUserMessageId": "foreign-client", "input": []any{map[string]any{"type": "text", "text": "foreign work"}}})
							}
							if queued {
								id, text := "queued", "saved assignment"
								if loaded && mode == "changed-text" {
									text = "changed assignment"
								}
								if loaded && mode == "changed-id" {
									id = "changed-queue"
								}
								data = append(data, map[string]any{"id": id, "clientUserMessageId": "assignment-id", "input": []any{map[string]any{"type": "text", "text": text}}})
							}
							result = map[string]any{"data": data, "nextCursor": nil}
						case "thread/resume":
							resumes++
							config, _ := params["config"].(map[string]any)
							if resumes == 1 && (params["model"] != "saved-model" || config["model_reasoning_effort"] != "low") {
								t.Error("cold activation lost original settings", params)
							}
							servers, _ := config["mcp_servers"].(map[string]any)
							server, _ := servers["team"].(map[string]any)
							if params["threadId"] != "member" || params["developerInstructions"] != "common lifecycle\n\nretained member" || params["sandbox"] != "danger-full-access" || params["approvalPolicy"] != "never" || server["command"] != policy.MCPServer.Command || fmt.Sprint(server["args"]) != "[team-mcp --slug example]" {
								t.Error("cold resume lost original member policy", params)
							}
							loaded = true
							if mode == "auto" {
								queued, started = false, true
							} else if mode == "fresh-hidden" || mode == "initial-cancel" {
								queued, started, historyVisible = false, true, false
							}
							result = map[string]any{"thread": map[string]any{"id": "member"}}
						case "thread/settings/update":
							if params["approvalPolicy"] != "never" || fmt.Sprint(params["sandboxPolicy"]) != "map[type:dangerFullAccess]" {
								t.Error("queue policy was weakened", params)
							}
							result = map[string]any{}
						case "thread/queue/start":
							starts++
							if params["threadId"] != "member" || params["queuedSubmissionId"] != "queued" || !store.AllowsImplicitResume(socket, "member") || store.AllowsImplicitResume(socket, "root") {
								t.Error("queue start changed identity/permission", params)
							}
							lockCtx, lockCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
							scope := turnretry.Scope{Workspace: workspace, Slug: "example", Root: "root", Thread: "member", Socket: socket}
							tx, lockErr := client.RetryStore().Lock(lockCtx, scope)
							lockCancel()
							if lockErr == nil {
								tx.Close()
								t.Error("queue start lost held retry admission")
							}
							if mode != "unknown" && mode != "cancelled" {
								queued, started = false, true
							}
							failure = mode == "unknown" || mode == "missing-history"
							if mode == "missing-history" {
								historyVisible = false
							}
							if mode == "cancelled" {
								cancelSend()
							}
							result = map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}}
						case "thread/turns/list":
							data := []any{}
							if started {
								data = append(data, map[string]any{"id": "turn", "status": "completed"})
							} else if loaded && mode == "active" {
								data = append(data, map[string]any{"id": "foreign-turn", "status": "inProgress"})
							}
							result = map[string]any{"data": data, "nextCursor": nil}
						case "thread/items/list":
							data := []any{}
							if started && historyVisible {
								data = append(data, map[string]any{"turnId": "turn", "item": map[string]any{"id": "item", "type": "userMessage", "clientId": "assignment-id", "content": []any{map[string]any{"type": "text", "text": "saved assignment"}}}})
							}
							result = map[string]any{"data": data, "nextCursor": nil}
						default:
							mu.Unlock()
							return fmt.Errorf("assignment unexpectedly used %v", request["method"])
						}
						mu.Unlock()
						if request["method"] == "thread/queue/add" && mode == "lost-ack" {
							connection.CloseNow() // No acknowledgement or positive queue/history proof.
							return nil
						}
						if request["method"] == "thread/queue/start" && (mode == "lost-reconciled" || mode == "cancelled") {
							connection.CloseNow() // Lose the reply, preserving the observed execution state.
							return nil
						}
						reply := map[string]any{"id": request["id"], "result": result}
						if failure {
							delete(reply, "result")
							reply["error"] = map[string]any{"code": -32000, "message": "synthetic uncertain queue start"}
						}
						if err := writeObject(connection, reply); err != nil {
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
				store = session.RecoveryStore{Root: state, Workspace: workspace}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				cancelSend = cancel
				defer cancel()
				if err := store.Set(ctx, "example", "root", socket, true, false); err != nil {
					t.Fatal(err)
				}
				client = NewWithOptions(socket, workspace, codex.ClientOptions{DeveloperInstructions: "common lifecycle", PreserveThreadInstructionsOnResume: true, SubmissionLedgerPath: filepath.Join(state, "receipts.json")})
				client.RecoveryRoot = state
				client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return policy, nil }
				defer client.Close()
				options := codex.TurnOptions{Model: "saved-model", ReasoningEffort: "low", ThreadPolicy: policy}
				send, action := client.SendWithOptions, "team:example:lead:member"
				if progress {
					send, action = client.SendProgressCheck, "progress:example:check"
				}
				if mode == "prior-unknown" {
					// Create an actual earlier uncertain queue attempt, not a fresh acknowledgement.
					if err := client.Client.PrepareSendWithOptions("member", "saved assignment", "assignment-id", action, false, options); err != nil {
						t.Fatal(err)
					}
					if _, err := client.Client.QueueWithPolicy(ctx, "member", "saved assignment", "assignment-id", policy); err == nil {
						t.Fatal("earlier invalid queue acknowledgement unexpectedly succeeded")
					}
				}
				receipt, err := send(ctx, "member", "saved assignment", "assignment-id", action, options)
				mu.Lock()
				gotQueued, gotStarted, gotQueues, gotStarts, gotResumes := queued, started, queues, starts, resumes
				mu.Unlock()
				preActivation := mode == "invalid-ack" || mode == "lost-ack" || mode == "prior-unknown"
				wantError := preActivation || mode == "initial-cancel" || mode == "changed-text" || mode == "changed-id" || mode == "unknown" || mode == "cancelled" || mode == "race-unknown" || mode == "missing-history" || mode == "epoch" || mode == "permission" || mode == "maintenance"
				if wantError {
					var unsent *codex.MessageNotSentError
					if err == nil || errors.As(err, &unsent) || gotQueues != 1 || gotStarts > 1 || !preActivation && gotResumes < 1 {
						t.Fatalf("unresolved cold queue was replayed or relabelled: receipt=%+v err=%v queue=%t started=%t adds=%d starts=%d resumes=%d", receipt, err, gotQueued, gotStarted, gotQueues, gotStarts, gotResumes)
					}
					if preActivation && (gotStarts != 0 || gotResumes != 0 || gotQueued || gotStarted || store.AllowsImplicitResume(socket, "member") || store.AllowsImplicitResume(socket, "root")) {
						t.Fatal("unacknowledged prior/fresh attempt activated work", err, gotStarts, gotResumes, gotQueued, gotStarted)
					}
					if mode == "initial-cancel" && (!errors.Is(err, context.Canceled) || gotStarts != 0 || gotResumes != 1 || gotQueued || !gotStarted) {
						t.Fatal("cancelled initial absence was accepted or activated twice", err, gotStarts, gotResumes, gotQueued, gotStarted)
					}
					if mode == "missing-history" && (gotStarts != 1 || gotQueued || !gotStarted || !strings.Contains(err.Error(), "synthetic uncertain queue start") || !strings.Contains(err.Error(), "earlier queued send outcome is unknown")) {
						t.Fatal("failed start bypassed exact history proof", err, gotStarts, gotQueued, gotStarted)
					}
					if mode == "cancelled" && (!errors.Is(err, context.Canceled) || gotStarts != 1 || !gotQueued || gotStarted) {
						t.Fatal("cancelled queue start lost its unresolved outcome", err, gotStarts, gotQueued, gotStarted)
					}
				} else {
					if err != nil || receipt.ClientUserMessageID != "assignment-id" || receipt.QueuedSubmissionID != "queued" || receipt.TurnID != "" || gotQueues != 1 {
						t.Fatal("original queued receipt changed", receipt, err, gotQueues)
					}
					if (mode == "idle" || mode == "lost-reconciled") && (!gotStarted || gotQueued || gotStarts != 1) {
						t.Fatalf("fresh cold queue head was not executed: adds=%d starts=%d queued=%t materialized=%t", gotQueues, gotStarts, gotQueued, gotStarted)
					}
					if mode == "foreign" || mode == "active" {
						if !gotQueued || gotStarted || gotStarts != 0 {
							t.Fatal("foreign head or active turn was started", gotQueued, gotStarted, gotStarts)
						}
					} else if mode == "fresh-hidden" {
						if !gotStarted || gotQueued || gotStarts != 0 || gotResumes != 1 || receipt.Steered {
							t.Fatal("fresh acceptance changed execution or receipt", receipt, gotStarts, gotResumes, gotQueued, gotStarted)
						}
						// Fresh acceptance does not resolve the ledger or authorize another dispatch.
						knownErr := client.RequireSubmissionAttemptsKnown(ctx, "member")
						resolvedErr := client.RequireSubmissionAttemptsResolved(ctx, "member")
						_, replayErr := send(ctx, "member", "saved assignment", "assignment-id", action, options)
						var unknown *codex.UnknownSendOutcomeError
						if knownErr == nil || resolvedErr == nil || !errors.As(replayErr, &unknown) || !strings.Contains(replayErr.Error(), "earlier queued send outcome is unknown") {
							t.Fatal("hidden history resolved or replayed fresh acceptance", knownErr, resolvedErr, replayErr)
						}
						mu.Lock()
						if queues != gotQueues || starts != gotStarts || resumes != gotResumes {
							t.Error("hidden same-ID observation submitted or activated work", queues, starts, resumes)
						}
						historyVisible = true // Expose the original item without another native mutation.
						mu.Unlock()
						passive, found, proofErr := client.Client.ReconcileSendWithOptions(ctx, "member", "saved assignment", "assignment-id", action, options)
						if proofErr != nil || !found || passive.ClientUserMessageID != receipt.ClientUserMessageID || passive.TurnID != "" || passive.Steered {
							t.Fatal("visible original history did not passively reconcile", passive, found, proofErr)
						}
						if err := client.RequireSubmissionAttemptsKnown(ctx, "member"); err != nil {
							t.Fatal("visible original history remained unknown", err)
						}
						if err := client.RequireSubmissionAttemptsResolved(ctx, "member"); err != nil {
							t.Fatal("visible original history remained unresolved", err)
						}
						t.Logf("fresh acknowledged receipt retained while paginated history hidden; known=%v resolved=%v replay=%v; original history then passively proved", knownErr, resolvedErr, replayErr)
					} else {
						if !gotStarted || gotQueued || (mode == "auto" || mode == "race-consumed") && gotStarts != 0 {
							t.Fatal("consumed queue/history proof missing", gotQueued, gotStarted, gotStarts)
						}
						if err := client.RequireSubmissionAttemptsResolved(ctx, "member"); err != nil {
							t.Fatal("assignment blocked lifecycle", err)
						}
					}
				}
				if mode != "epoch" && mode != "permission" && mode != "maintenance" && mode != "cancelled" && mode != "initial-cancel" {
					_, replayErr := send(ctx, "member", "saved assignment", "assignment-id", action, options)
					if mode != "changed-text" && mode != "changed-id" && mode != "race-unknown" && mode != "missing-history" && !preActivation && replayErr != nil {
						t.Fatal("same-ID observation failed", replayErr)
					}
					mu.Lock()
					if queues != gotQueues || starts != gotStarts || resumes != gotResumes {
						t.Error("same-ID retry submitted or activated work", queues, starts, resumes)
					}
					mu.Unlock()
				}
				if mode != "epoch" && mode != "permission" && mode != "maintenance" && !preActivation && (store.AllowsImplicitResume(socket, "root") || !store.AllowsImplicitResume(socket, "member")) {
					t.Fatal("assignment released another recipient or rolled back published permission")
				}
				t.Logf("progress=%t mode=%s adds=%d starts=%d resumes=%d queued=%t materialized=%t err=%v", progress, mode, gotQueues, gotStarts, gotResumes, gotQueued, gotStarted, err)
			})
		}
	}
}
