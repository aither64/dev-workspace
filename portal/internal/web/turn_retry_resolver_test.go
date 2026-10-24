package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

type resolverRetryClient struct {
	*workspacecodex.Client
	cwd string
}

func (c *resolverRetryClient) ProveArchivedRootThread(context.Context, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}
func (c *resolverRetryClient) ProveArchivedThread(context.Context, string, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}
func (c *resolverRetryClient) ProveMaterializedActiveThread(context.Context, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}
func (c *resolverRetryClient) HeadlessThreadMaterialized(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (c *resolverRetryClient) ListThreads(context.Context, codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error) {
	return []codex.ThreadMetadata{{ID: "thread-1", Cwd: c.cwd, Source: "vscode"}, {ID: "member-thread", Cwd: c.cwd, Source: "vscode"}}, nil, nil
}
func (c *resolverRetryClient) LoadedThreadIDs(context.Context) ([]string, error) {
	return []string{"thread-1", "member-thread"}, nil
}

type retryWire struct {
	mu     sync.Mutex
	cwd    string
	expose bool
	starts int
	items  map[string][]any
	latest map[string]string
}

func (w *retryWire) serve(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(data, &req) != nil {
				return
			}
			if req["id"] == nil {
				continue
			}
			params, _ := req["params"].(map[string]any)
			thread, _ := params["threadId"].(string)
			w.mu.Lock()
			var result any
			var failure any
			switch req["method"] {
			case "initialize":
				result = map[string]any{"userAgent": "codex/fixture"}
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": thread, "cwd": w.cwd, "source": "vscode", "modelProvider": "fixture", "status": map[string]any{"type": "idle"}}}
			case "thread/turns/list":
				turn := w.latest[thread]
				if turn == "" {
					turn = "failed"
				}
				status := "failed"
				if turn != "failed" {
					status = "completed"
				}
				result = map[string]any{"data": []any{map[string]any{"id": turn, "status": status, "error": map[string]any{"codexErrorInfo": "cyberPolicy", "message": "synthetic"}}}}
			case "thread/queue/list":
				result = map[string]any{"data": []any{}}
			case "thread/items/list":
				items := []any{}
				if w.expose {
					items = w.items[thread]
					if items == nil {
						items = []any{}
					}
				}
				result = map[string]any{"data": items, "nextCursor": nil}
			case "turn/start":
				w.starts++
				id, _ := params["clientUserMessageId"].(string)
				turn := "accepted-" + thread
				w.items[thread] = []any{map[string]any{"turnId": turn, "item": map[string]any{"id": "input-" + id, "type": "userMessage", "clientId": id, "content": []any{map[string]any{"type": "text", "text": turnretry.Continuation}}}}}
				w.latest[thread] = turn
				result = map[string]any{"turn": map[string]any{}} // Native acceptance, lost usable reply; history not published yet.
			default:
				failure = map[string]any{"code": -32000, "message": fmt.Sprint("unexpected native mutation/read: ", req["method"])}
				t.Errorf("unexpected native RPC %v", req["method"])
			}
			w.mu.Unlock()
			reply := map[string]any{"id": req["id"]}
			if failure != nil {
				reply["error"] = failure
			} else {
				reply["result"] = result
			}
			out, _ := json.Marshal(reply)
			if err := conn.Write(context.Background(), websocket.MessageText, out); err != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
}

func TestTurnRetryResolverRecoversExactNativeLedgerBeforeOrdinaryGate(t *testing.T) {
	for _, address := range []string{"", "implementer0"} {
		for _, mode := range []string{"prepared", "absent", "submitting", "cancelled", "cold", "stopped", "foreign", "other-member", "unknown", "generation"} {
			t.Run(address+"/"+mode, func(t *testing.T) {
				s := newTestServer(t)
				cwd := filepath.Join(s.config.Workspace, "work", "example")
				if err := os.MkdirAll(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				dir, err := os.MkdirTemp("/tmp", "retry-resolver-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				socket := filepath.Join(dir, "native.sock")
				s.config.CodexSocket = socket
				manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: thread-1\n  socket_path: " + socket + "\n  client_version: 0.152.1\n"
				if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
					t.Fatal(err)
				}
				writeWebTrackingFiles(t, cwd, "active")
				writeWebRuntimeAuthority(t, s, "example")
				wire := &retryWire{cwd: cwd, items: map[string][]any{}, latest: map[string]string{}}
				wire.serve(t, socket)
				ledger := filepath.Join(s.config.UserStateRoot, "native.json")
				makeClient := func() *resolverRetryClient {
					c := workspacecodex.NewWithOptions(socket, s.config.Workspace, codex.ClientOptions{SubmissionLedgerPath: ledger})
					c.RecoveryRoot = s.config.UserStateRoot
					c.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return codex.ThreadPolicy{}, nil }
					return &resolverRetryClient{c, cwd}
				}
				c := makeClient()
				defer func() { c.Close() }()
				s.config.Codex = c
				service, err := s.teamService("example")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := service.Store.Update(context.Background(), "example", "thread-1", true, func(r *teamruntime.Roster) error {
					r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Thread: "member-thread", Model: "gpt-6.1-sol", Effort: "xhigh", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				thread, id := "thread-1", "example"
				if address != "" {
					thread, id = "member-thread", teamConversationID("example", address)
				}
				scope, err := c.RetryScope(context.Background(), thread)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := c.RetryStore().Lock(context.Background(), scope)
				if err != nil {
					t.Fatal(err)
				}
				p := turnretry.Snapshot{Epoch: 1, Version: 1, ID: "managed", Turn: "failed", Proven: true, Provider: "fixture", Mode: "default", Options: codex.TurnOptions{Model: "gpt-6.1-sol", ReasoningEffort: "xhigh"}}
				p.Digest, _ = p.Hash()
				tx.State.Enroll(turnretry.Observation{})
				tx.State.AddSnapshot(p)
				tx.State.Boundary, _ = session.ObservationIdentity(s.config.Workspace, "example", cwd, "thread-1")
				if err := tx.State.Observe(turnretry.Observation{Turn: "failed", Status: "failed", Code: "cyberPolicy"}, time.Now(), 0, turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
					t.Fatal(err)
				}
				if err := tx.State.Begin(time.Now(), turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
					t.Fatal(err)
				}
				pending := *tx.State.Pending
				if mode == "cancelled" {
					tx.State.Cancel("cancelled")
				}
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
				if mode != "absent" {
					if err := c.PrepareSendWithOptions(thread, turnretry.Continuation, pending.ID, pending.Action, false, pending.Snapshot.Options); err != nil {
						t.Fatal(err)
					}
				}
				accepted := mode != "prepared" && mode != "absent"
				if accepted {
					_, err := c.Client.Client.ContinueFailedTurn(context.Background(), thread, turnretry.Continuation, pending.ID, pending.Action, "failed", pending.Snapshot.Options)
					var unknown *codex.UnknownSendOutcomeError
					if !errors.As(err, &unknown) {
						t.Fatal("fixture did not leave native submitting", err)
					}
				}
				if mode == "foreign" || mode == "other-member" {
					foreign := thread
					if mode == "other-member" {
						foreign = "thread-1"
						if thread == foreign {
							foreign = "member-thread"
						}
					}
					if err := c.PrepareSendWithOptions(foreign, "manual unknown", "foreign-manual", "manual", false, codex.TurnOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "cold" {
					s.config.RecoverSessions = true
					if err := s.recoveryStore().Set(context.Background(), "example", "thread-1", socket, false, false); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "stopped" {
					s.config.Tmux = "/bin/false"
				}
				if accepted {
					if target, err := s.resolveConversation(context.Background(), conversation.ResolveRequest{ID: id, Operation: "automatic-retry", Mutation: true}); err == nil {
						target.Release()
						t.Fatal("fixture bypassed original unresolved gate")
					}
				}
				c.Close()
				c = makeClient()
				s.config.Codex = c
				wire.mu.Lock()
				wire.expose = mode != "unknown"
				wire.mu.Unlock()
				if mode == "generation" {
					if err := os.Remove(s.config.HostProfile); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(t.TempDir(), s.config.HostProfile); err != nil {
						t.Fatal(err)
					}
				}
				_ = s.stepTurnRetry(context.Background(), c, id)
				saved, err := c.RetryStore().Load(scope)
				if err != nil {
					t.Fatal(err)
				}
				if accepted && mode != "unknown" && mode != "generation" {
					found := false
					for _, p := range saved.Snapshots {
						if p.ID == pending.ID && p.Turn == "accepted-"+thread {
							found = true
						}
					}
					if !found {
						t.Fatal("resolver prevented exact receipt reconciliation", saved)
					}
				}
				if (mode == "unknown" || mode == "generation") && (!saved.Uncertain() || saved.Pending.ResultTurn != "") {
					t.Fatal("unknown/generation hold cleared", saved)
				}
				if (saved.Pending != nil && saved.Pending.ID != pending.ID) || saved.Attempt != 1 {
					t.Fatal("recovery changed stable intent")
				}
				wire.mu.Lock()
				starts := wire.starts
				wire.mu.Unlock()
				expected := 0
				if accepted {
					expected = 1
				}
				if starts != expected {
					t.Fatal("recovery submitted another turn", starts)
				}
				if mode == "foreign" || mode == "other-member" {
					if target, err := s.resolveConversation(context.Background(), conversation.ResolveRequest{ID: id, Operation: "automatic-retry", Mutation: true}); err == nil {
						target.Release()
						t.Fatal("foreign unknown hold was bypassed")
					}
				}
				if mode == "prepared" {
					if err := c.RequireSubmissionAttemptsKnown(context.Background(), thread); err != nil {
						t.Fatal("exact preparation not retired", err)
					}
				}
			})
		}
	}
}
