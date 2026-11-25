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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
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

// Real R1 state/authority, operation codec, locks and SDK/UnixWS delivery.
// Utility creation/bootstrap and raw activity observations remain doubles.
func TestBoundMonitorCapturesCurrentLeadAndDeliversToSavedChild(t *testing.T) {
	service, utility, request, _ := operationFixture(t)
	workspace := service.Store.workspace
	stateRoot := filepath.Dir(filepath.Dir(service.Store.directory))
	slug := "monitor-test"
	oldSocket, listener := shortMonitorSocket(t)
	socket := filepath.Join(filepath.Dir(oldSocket), "registered", "app-server.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldSocket, socket); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"portal.yml": fmt.Sprintf("schema: 6\nslug: %s\nrepositories: []\ncodex:\n  thread_id: origin-root\n  socket_path: %s\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\n", slug, socket), "plan.md": "Monitor current lead control\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	authorityDir := filepath.Join(filepath.Dir(socket), "authority")
	if err := os.MkdirAll(authorityDir, 0700); err != nil {
		t.Fatal(err)
	}
	inc := strings.Repeat("a", 64)
	operation := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	state := codexproviders.State{Schema: 3, Workspace: "registered", Root: workspace, Socket: socket, Active: "openai", BindingRevision: 2, Bindings: map[string]codexproviders.SessionBinding{slug: {RootThreadID: "origin-root", Incarnation: inc, Revision: 2, Conversations: map[string]codexproviders.ConversationBinding{"origin-root": {Member: "lead", ThreadID: "current-child", Provider: "openai", OperationID: operation, EvidenceDigest: strings.Repeat("c", 64)}}}}}
	authority := session.RuntimeAuthority{Schema: 3, State: "ready", Slug: slug, Workspace: workspace, TmuxSocket: "/run/tmux.sock", TmuxSessionID: "$1", TmuxIdentity: inc, CodexThreadID: "origin-root", CodexActiveThreadID: "current-child", CodexBindingRevision: 2, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
	statePath, err := codexproviders.StatePath(stateRoot, "registered")
	if err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]any{statePath: state, filepath.Join(authorityDir, slug+".json"): authority} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(value)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalog)
	recovery := session.RecoveryStore{Root: stateRoot, Workspace: workspace}
	if err := recovery.Set(context.Background(), slug, "origin-root", socket, true, false); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Update(context.Background(), slug, func(r *session.Recovery) error { r.ActiveThreads = []string{"current-child"}; return nil }); err != nil {
		t.Fatal(err)
	}
	var busy atomic.Bool
	busy.Store(true)
	var mutations atomic.Int32
	failures := make(chan error, 8)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			failures <- err
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
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
			result := map[string]any{}
			switch req["method"] {
			case "initialize":
				result["userAgent"] = "bound-monitor-fixture"
			case "thread/read":
				if params["threadId"] != "current-child" {
					failures <- errors.New("ancestor/member reached native identity read")
					return
				}
				result["thread"] = map[string]any{"id": "current-child", "cwd": cwd, "source": "vscode", "model": "saved-model", "modelProvider": "openai", "reasoningEffort": "high", "path": "/fixture/rollout.jsonl"}
			case "thread/turns/list":
				turns := []any{}
				if busy.Load() {
					turns = append(turns, map[string]any{"id": "originating-turn", "status": "inProgress"})
				}
				result["data"] = turns
			case "thread/items/list":
				result["data"] = []any{}
				result["nextCursor"] = nil
			case "thread/resume":
			case "turn/start":
				if params["threadId"] != "current-child" {
					failures <- errors.New("monitor delivered to ancestor")
					return
				}
				mutations.Add(1)
				result["turn"] = map[string]any{"id": "delivered-turn"}
			default:
				failures <- fmt.Errorf("unexpected bound monitor method %v", req["method"])
				return
			}
			response, _ := json.Marshal(map[string]any{"id": req["id"], "result": result})
			if err := conn.Write(r.Context(), websocket.MessageText, response); err != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	client := codex.NewWithOptions(socket, codex.ClientOptions{SubmissionLedgerPath: filepath.Join(stateRoot, "lead-receipts.json")})
	defer client.Close()
	native := NativeAuthority{Workspace: workspace, StateRoot: stateRoot, AuthorityDir: authorityDir, Socket: socket, Client: client}
	service.ResolveLead, service.Resolve, service.LockSession, service.Admit, service.Activate = native.ResolveLead, native.Resolve, native.LockSession, native.Admit, native.Activate
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, caller := range []string{"origin-root", "member-child"} {
		if _, err := service.Start(ctx, caller, request); err == nil {
			t.Fatal("ancestor/member started monitor")
		}
	}
	if utility.creates != 0 || utility.starts != 0 {
		t.Fatal("denied caller created utility")
	}
	op, err := service.Start(ctx, "current-child", request)
	if err != nil {
		t.Fatal(err)
	}
	if op.Owner.RootThreadID != "origin-root" || op.Owner.LeadTarget() != "current-child" || op.Owner.RuntimeSchema != 3 || op.Owner.OriginTurnID != "originating-turn" {
		t.Fatal("current capture changed logical root", op.Owner)
	}
	read, err := service.Store.Get(ctx, op.ID)
	if err != nil || !reflect.DeepEqual(read.Owner, op.Owner) {
		t.Fatal("saved owner roundtrip differs", read.Owner, err)
	}
	if _, err := service.Store.Inventory(ctx, slug, "origin-root"); err != nil {
		t.Fatal("original inventory lost", err)
	}
	busy.Store(false)
	delivery := NativeDelivery{Workspace: workspace, StateRoot: stateRoot, AuthorityDir: authorityDir, Socket: socket, Client: &deliveryNativeFixture{DeliveryClient: client, metadata: codex.ThreadMetadata{ID: "current-child", Cwd: cwd}, activity: codex.ActivitySnapshot{ThreadID: "current-child", CurrentState: "idle"}}, PrepareLead: func(context.Context, Owner) (codex.TurnOptions, error) {
		return codex.TurnOptions{Model: "saved-model", ReasoningEffort: "high"}, nil
	}}
	if reason, err := delivery.Ready(ctx, op.Owner); reason != "" || err != nil {
		t.Fatal("current lead held", reason, err)
	}
	event := Event{Delivery: Delivery{MessageID: "current-event", Envelope: "exact saved event", Context: "monitor:exact:event", Options: &codex.TurnOptions{Model: "saved-model", ReasoningEffort: "high"}}}
	receipt, err := delivery.Send(ctx, op.Owner, event)
	if err != nil || receipt.TurnID != "delivered-turn" || mutations.Load() != 1 {
		t.Fatal("current child delivery failed", receipt, err)
	}
	if reconciled, found, err := delivery.Reconcile(ctx, op.Owner, event); err != nil || !found || reconciled != receipt || mutations.Load() != 1 {
		t.Fatal("saved delivery replayed", reconciled, found, err)
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
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
