//go:build live_switch_integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

type retryNativeBarrier struct {
	atomic.Bool
	mu                              sync.Mutex
	starts, accepted, hiddenHistory int
	thread, clientID, turn, failure string
}

func (b *retryNativeBarrier) snapshot() (starts, accepted, hidden int, thread, clientID, turn, failure string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts, b.accepted, b.hiddenHistory, b.thread, b.clientID, b.turn, b.failure
}

type retryNativeStart struct{ Thread, ClientID string }

// Only an actual successful native receipt is hidden. RPC errors and missing
// receipts stay visible; neither can establish native acceptance for this test.
func (b *retryNativeBarrier) receipt(t *testing.T, start retryNativeStart, reply map[string]json.RawMessage) bool {
	var result struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	failure := string(reply["error"])
	accepted := (failure == "" || failure == "null") && json.Unmarshal(reply["result"], &result) == nil && result.Turn.ID != ""
	b.mu.Lock()
	defer b.mu.Unlock()
	if accepted {
		b.accepted++
		b.thread, b.clientID, b.turn = start.Thread, start.ClientID, result.Turn.ID
		t.Logf("native barrier accepted thread=%s client=%s turn=%s", start.Thread, start.ClientID, result.Turn.ID)
	} else {
		if failure == "" || failure == "null" {
			failure = "native turn/start returned no successful turn receipt"
		}
		b.failure = failure[:min(len(failure), 1024)]
		t.Logf("native barrier preserved rejected/missing receipt: %s", b.failure)
	}
	return accepted
}

// Hold hides receipt history and removes a successful turn/start response's
// usable receipt after native acceptance. Native state stays intact.
func retryNativeReceiptBarrier(t *testing.T, ctx context.Context, native, proxy string) *retryNativeBarrier {
	t.Helper()
	hold := &retryNativeBarrier{}
	listener, err := net.Listen("unix", proxy)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer down.CloseNow()
		down.SetReadLimit(16 << 20)
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", native)
		}}
		defer transport.CloseIdleConnections()
		up, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			t.Errorf("native proxy dial: %v", err)
			return
		}
		defer up.CloseNow()
		up.SetReadLimit(16 << 20)
		var mu sync.Mutex
		starts := map[string]retryNativeStart{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer down.CloseNow()
			for {
				kind, data, err := up.Read(ctx)
				if err != nil {
					return
				}
				var reply map[string]json.RawMessage
				if json.Unmarshal(data, &reply) == nil && reply["id"] != nil {
					mu.Lock()
					start, armed := starts[string(reply["id"])]
					delete(starts, string(reply["id"]))
					mu.Unlock()
					if armed && hold.receipt(t, start, reply) {
						reply["result"] = json.RawMessage(`{"turn":{}}`)
						data, _ = json.Marshal(reply)
					}
				}
				if err := down.Write(ctx, kind, data); err != nil {
					return
				}
			}
		}()
		defer func() { _ = up.CloseNow(); <-done }()
		for {
			kind, data, err := down.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					Thread   string `json:"threadId"`
					ClientID string `json:"clientUserMessageId"`
				} `json:"params"`
			}
			_ = json.Unmarshal(data, &req)
			if hold.Load() && req.Method == "thread/items/list" {
				hold.mu.Lock()
				hold.hiddenHistory++
				hold.mu.Unlock()
				out, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"data": []any{}, "nextCursor": nil}})
				if down.Write(ctx, websocket.MessageText, out) != nil {
					return
				}
				continue
			}
			if hold.Load() && req.Method == "turn/start" {
				mu.Lock()
				starts[string(req.ID)] = retryNativeStart{req.Params.Thread, req.Params.ClientID}
				mu.Unlock()
				hold.mu.Lock()
				hold.starts++
				hold.mu.Unlock()
			}
			if up.Write(ctx, kind, data) != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	return hold
}

func TestNativeRetryReceiptBarrierRequiresSuccessfulReceipt(t *testing.T) {
	for _, outcome := range []string{"accepted", "rpc-error", "missing-receipt"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			native := threadCommandAppServer(t, func(conn *websocket.Conn) error {
				defer conn.CloseNow()
				if err := threadCommandHandshake(conn); err != nil {
					return err
				}
				req, err := threadCommandReadObject(conn)
				if err != nil || req["method"] != "turn/start" {
					return fmt.Errorf("expected turn/start: %v %v", req, err)
				}
				reply := map[string]any{"id": req["id"], "result": map[string]any{"turn": map[string]any{}}}
				if outcome == "accepted" {
					reply["result"] = map[string]any{"turn": map[string]any{"id": "actual-native-turn"}}
				}
				if outcome == "rpc-error" {
					delete(reply, "result")
					reply["error"] = map[string]any{"code": -32600, "message": "synthetic rejection"}
				}
				if err := threadCommandWriteObject(conn, reply); err != nil {
					return err
				}
				_, _ = threadCommandReadObject(conn) // Keep transport alive until the caller closes it.
				return nil
			})
			dir, err := os.MkdirTemp("/tmp", "retry-barrier-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			proxy := filepath.Join(dir, "proxy.sock")
			barrier := retryNativeReceiptBarrier(t, ctx, native, proxy)
			barrier.Store(true)
			client := codex.New(proxy)
			defer client.Close()
			var result struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			err = client.Request(ctx, "turn/start", map[string]any{"threadId": "thread", "clientUserMessageId": "client"}, &result)
			starts, accepted, _, thread, clientID, turn, failure := barrier.snapshot()
			if starts != 1 || result.Turn.ID != "" {
				t.Fatal("unexpected native receipt barrier result", starts, result)
			}
			if outcome == "accepted" {
				if err != nil || accepted != 1 || thread != "thread" || clientID != "client" || turn != "actual-native-turn" || failure != "" {
					t.Fatal("successful native receipt not retained", err, accepted, turn, failure)
				}
			} else if accepted != 0 || failure == "" || (outcome == "rpc-error" && (err == nil || !strings.Contains(err.Error(), "synthetic rejection"))) {
				t.Fatal("native rejection was hidden or counted as acceptance", err, accepted, failure)
			}
		})
	}
}

func retryNativePortal(t *testing.T, root, workspace, stateRoot, socket, thread string, client *workspacecodex.Client) *portalweb.Server {
	t.Helper()
	// The test provider is configured solely in the private native home. The
	// portal must not inherit the host's installed provider catalog.
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(root, "absent-provider-catalog.json"))
	authorityDir := filepath.Join(root, "authority")
	if err := os.MkdirAll(authorityDir, 0700); err != nil {
		t.Fatal(err)
	}
	authority := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: "example", Workspace: workspace, TmuxSocket: filepath.Join(root, "tmux.sock"), TmuxSessionID: "$1", TmuxIdentity: strings.Repeat("a", 64), CodexThreadID: thread, CodexSocketPath: socket, CodexClientVersion: "0.163.0"}
	data, _ := json.Marshal(authority)
	if err := os.WriteFile(filepath.Join(authorityDir, "example.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(root, "tmux")
	line := strings.Join([]string{"$1", "example", "1", "example", workspace, "example", authority.TmuxSocket, thread, socket, "0.163.0", "%1", authority.TmuxIdentity}, "\t")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf '%s\\n' '"+line+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "no-lifecycle")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profile")
	if err := os.Symlink(root, profile); err != nil {
		t.Fatal(err)
	}
	transition := filepath.Join(root, "transition.lock")
	if err := os.WriteFile(transition, nil, 0600); err != nil {
		t.Fatal(err)
	}
	server, err := portalweb.New(portalweb.Config{Workspace: workspace, BaseURL: "https://fixture.local", UserStateRoot: stateRoot, AuthorityDir: authorityDir, HostProfile: profile, TransitionLock: transition, DevSession: helper, Tmux: tmux, Codex: client, CodexSocket: socket, CodexVersion: "0.163.0", VerifyThread: client.VerifyThread, RecoverSessions: true, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// Exercise the native fixture's actual portal constructor without a native
// process. Compilation alone cannot validate its secure-origin configuration.
func TestNativeRetryPortalSecureSetup(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot, socket := filepath.Join(root, "state"), filepath.Join(root, "unused.sock")
	client := newCodexClient(socket, workspace, stateRoot)
	defer client.Close()
	server := retryNativePortal(t, root, workspace, stateRoot, socket, "fixture-thread", client)
	server.Close()
}
