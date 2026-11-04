package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/coder/websocket"
)

type deliveryNativeFixture struct {
	DeliveryClient
	metadata codex.ThreadMetadata
	activity codex.ActivitySnapshot
	state    codex.TurnState
	cold     bool
}

func (f *deliveryNativeFixture) ReadThreadMetadata(_ context.Context, _ string, cold bool) (codex.ThreadMetadata, error) {
	f.cold = cold
	return f.metadata, nil
}
func (f *deliveryNativeFixture) ReadActivity(context.Context, string) (codex.ActivitySnapshot, error) {
	return f.activity, nil
}
func (f *deliveryNativeFixture) ReadTurnState(context.Context, string) (codex.TurnState, error) {
	return f.state, nil
}

func shortMonitorSocket(t *testing.T) (string, net.Listener) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "monitor-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return path, listener
}

func TestMonitorNativeReadinessHoldsWithoutGrantingExecution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authority := filepath.Join(root, "authority")
	if err := os.MkdirAll(authority, 0700); err != nil {
		t.Fatal(err)
	}
	socket, _ := shortMonitorSocket(t)
	epoch, err := session.SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	owner := Owner{Slug: "2026-11-03-ready", RootThreadID: "lead", Socket: socket, SocketEpoch: epoch}
	f := &deliveryNativeFixture{metadata: codex.ThreadMetadata{ID: "lead", Cwd: filepath.Join(root, "work", owner.Slug)}, activity: codex.ActivitySnapshot{ThreadID: "lead", CurrentState: "idle"}}
	n := NativeDelivery{Workspace: root, StateRoot: root, AuthorityDir: authority, Socket: socket, Client: f}
	a := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: owner.Slug, Workspace: root, TmuxSocket: filepath.Join(root, "tmux"), TmuxSessionID: "$1", CodexThreadID: owner.RootThreadID, CodexSocketPath: socket, CodexClientVersion: "0.165.0"}
	data, _ := json.Marshal(a)
	if err := os.WriteFile(filepath.Join(authority, owner.Slug+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	recovery := session.RecoveryStore{Root: root, Workspace: root}
	if err := recovery.Set(ctx, owner.Slug, owner.RootThreadID, socket, true, true); err != nil {
		t.Fatal(err)
	}
	for _, activity := range []codex.ActivitySnapshot{{ThreadID: "lead", CurrentState: "idle"}, {ThreadID: "lead", CurrentState: "working"}, {ThreadID: "lead", CurrentState: "waiting", WaitReason: "sleep"}} {
		f.activity = activity
		if reason, err := n.Ready(ctx, owner); reason != "" || err != nil {
			t.Fatalf("eligible lead held: %q %v", reason, err)
		}
	}
	for _, activity := range []codex.ActivitySnapshot{{ThreadID: "lead", CurrentState: "waiting", WaitReason: "userInput"}, {ThreadID: "lead", CurrentState: "waiting", WaitReason: "approval"}, {ThreadID: "lead", CurrentState: "idle", Stale: true}, {ThreadID: "foreign", CurrentState: "idle"}, {ThreadID: "lead", CurrentState: "unknown"}} {
		f.activity = activity
		if reason, _ := n.Ready(ctx, owner); reason == "" {
			t.Fatalf("unsafe activity admitted: %+v", activity)
		}
	}
	f.activity = codex.ActivitySnapshot{ThreadID: "lead", CurrentState: "idle"}
	n.Admission = func(Owner) error { return errors.New("provider changing") }
	if reason, _ := n.Ready(ctx, owner); reason == "" {
		t.Fatal("provider pause bypassed")
	}
	n.Admission = nil
	old := owner
	old.SocketEpoch = "old-daemon"
	if reason, _ := n.Ready(ctx, old); reason == "" {
		t.Fatal("origin epoch activated the replacement daemon")
	}
	if err := recovery.Set(ctx, owner.Slug, owner.RootThreadID, socket, false, false); err != nil {
		t.Fatal(err)
	}
	if reason, _ := n.Ready(ctx, owner); reason == "" {
		t.Fatal("stopped session activated")
	}
	saved, err := recovery.Load(owner.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Active(owner.RootThreadID, epoch) {
		t.Fatal("readiness granted execution permission")
	}
}

func TestMonitorNativeUtilityObservationIsColdAndExact(t *testing.T) {
	project := "utility-project"
	op := Operation{Owner: Owner{Slug: "fixture"}, Launch: Launch{ThreadID: "utility", ProjectID: project, Receipt: &codex.SendReceipt{TurnID: "exact-turn"}}}
	f := &deliveryNativeFixture{metadata: codex.ThreadMetadata{ID: "utility", Cwd: "/workspace/work/fixture", ProjectID: &project}, state: codex.TurnState{ThreadID: "utility", Status: "notLoaded", LatestTurnID: "exact-turn", LatestTurnStatus: "completed"}}
	n := NativeDelivery{Workspace: "/workspace", Client: f}
	for _, status := range []string{"completed", "failed", "interrupted"} {
		f.state.LatestTurnStatus = status
		if got, terminal, err := n.ObserveUtility(context.Background(), op); got != status || !terminal || err != nil || !f.cold {
			t.Fatalf("cold terminal proof failed: %s %t %v", got, terminal, err)
		}
	}
	f.state.LatestTurnID = "another-turn"
	if _, terminal, _ := n.ObserveUtility(context.Background(), op); terminal {
		t.Fatal("another turn released capacity")
	}
	f.state.LatestTurnID = "exact-turn"
	f.state.Status = "active"
	if _, terminal, _ := n.ObserveUtility(context.Background(), op); terminal {
		t.Fatal("active utility released capacity")
	}
	f.state.Status = "idle"
	wrong := "foreign-project"
	f.metadata.ProjectID = &wrong
	if _, terminal, err := n.ObserveUtility(context.Background(), op); terminal || err == nil {
		t.Fatal("foreign utility accepted")
	}
}

func TestMonitorNativeTransportStartsIdleSteersBusyAndReconcilesSameID(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%t", active), func(t *testing.T) {
			socket, listener := shortMonitorSocket(t)
			var mutations atomic.Int32
			failures := make(chan error, 8)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					failures <- err
					return
				}
				defer conn.CloseNow()
				ctx := r.Context()
				for {
					_, data, err := conn.Read(ctx)
					if err != nil {
						return
					}
					var req map[string]any
					if err := json.Unmarshal(data, &req); err != nil {
						failures <- err
						return
					}
					if req["id"] == nil {
						continue
					}
					params, _ := req["params"].(map[string]any)
					var result any = map[string]any{}
					switch req["method"] {
					case "initialize":
						result = map[string]any{"userAgent": "monitor-fixture"}
					case "thread/resume":
					case "thread/turns/list":
						turns := []any{}
						if active {
							turns = append(turns, map[string]any{"id": "busy-turn", "status": "inProgress"})
						}
						result = map[string]any{"data": turns}
					case "thread/items/list":
						result = map[string]any{"data": []any{}, "nextCursor": nil}
					case "turn/start", "turn/steer":
						want := "turn/start"
						if active {
							want = "turn/steer"
						}
						if req["method"] != want || params["threadId"] != "lead" || params["clientUserMessageId"] != "event-id" {
							failures <- fmt.Errorf("wrong native dispatch: %v", req)
							return
						}
						input, _ := params["input"].([]any)
						if len(input) != 1 {
							failures <- errors.New("native delivery lost its exact event input")
							return
						}
						item, _ := input[0].(map[string]any)
						if item["type"] != "text" || item["text"] != "exact run/attempt/outcome" {
							failures <- errors.New("native delivery changed its exact event input")
							return
						}
						if !active && (params["model"] != "saved-model" || params["effort"] != "xhigh") {
							failures <- fmt.Errorf("idle settings changed: %v", params)
							return
						}
						if active && (params["expectedTurnId"] != "busy-turn" || params["model"] != nil || params["effort"] != nil) {
							failures <- fmt.Errorf("active settings changed: %v", params)
							return
						}
						mutations.Add(1)
						result = map[string]any{"turn": map[string]any{"id": "idle-turn"}, "turnId": "busy-turn"}
					default:
						failures <- fmt.Errorf("unexpected native method %v", req["method"])
						return
					}
					response, _ := json.Marshal(map[string]any{"id": req["id"], "result": result})
					if err := conn.Write(ctx, websocket.MessageText, response); err != nil {
						return
					}
				}
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			state := t.TempDir()
			options := codex.ClientOptions{SubmissionDatabasePath: filepath.Join(state, "receipts.sqlite"), SubmissionLedgerPath: filepath.Join(state, "receipts.json")}
			client := codex.NewWithOptions(socket, options)
			native := NativeDelivery{Client: client}
			event := Event{Delivery: Delivery{MessageID: "event-id", Envelope: "exact run/attempt/outcome", Context: "monitor:operation:event", Options: &codex.TurnOptions{Model: "saved-model", ReasoningEffort: "xhigh"}}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			receipt, err := native.Send(ctx, Owner{RootThreadID: "lead"}, event)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.TurnID == "" || receipt.Steered != active {
				t.Fatalf("wrong native receipt: %+v", receipt)
			}
			client.Close()
			client = codex.NewWithOptions(socket, options)
			defer client.Close()
			native.Client = client
			if recovered, found, err := native.Reconcile(ctx, Owner{RootThreadID: "lead"}, event); err != nil || !found || recovered != receipt {
				t.Fatalf("restart lost receipt: %+v %t %v", recovered, found, err)
			}
			if mutations.Load() != 1 {
				t.Fatalf("native mutations=%d", mutations.Load())
			}
			select {
			case err := <-failures:
				t.Fatal(err)
			default:
			}
		})
	}
}
