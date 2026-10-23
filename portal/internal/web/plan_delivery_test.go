package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

const planDeliveryID = "00000000-0000-4000-8000-000000000071"
const deliveryPlan = "Implement the saved synthetic plan."

type planDeliveryWire struct {
	mu                                        sync.Mutex
	t                                         *testing.T
	cwd, rollout, mode, latest, status        string
	starts, updates, resumes                  int
	items                                     []any
	lost, hide, failSettings, failReceiptSave bool
	failAdmissionSave, stopAfterAdmission     bool
	client                                    *workspacecodex.Client
}

func (p *planDeliveryWire) writeMode() {
	line, _ := json.Marshal(map[string]any{"type": "turn_context", "payload": map[string]any{"turn_id": p.latest, "cwd": p.cwd, "model": "retained-model", "effort": "xhigh", "collaboration_mode": map[string]any{"mode": p.mode}}})
	if err := os.WriteFile(p.rollout, append(line, '\n'), 0600); err != nil {
		p.t.Error(err)
	}
}

func (p *planDeliveryWire) serve(socket string) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		p.t.Fatal(err)
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
			p.mu.Lock()
			var result any = map[string]any{}
			failure := ""
			var notify any
			switch req["method"] {
			case "initialize":
				result = map[string]any{"userAgent": "codex/fixture"}
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": thread, "cwd": p.cwd, "path": p.rollout, "source": "vscode", "modelProvider": "fixture", "model": "retained-model", "reasoningEffort": "xhigh", "status": map[string]any{"type": "idle"}}}
			case "thread/turns/list":
				items := []any{}
				if p.latest == "plan-turn" {
					items = append(items, map[string]any{"id": "plan-item", "type": "plan", "text": deliveryPlan})
				}
				result = map[string]any{"data": []any{map[string]any{"id": p.latest, "status": p.status, "items": items}}}
			case "thread/items/list":
				items := p.items
				if items == nil || p.hide {
					items = []any{}
				}
				result = map[string]any{"data": items, "nextCursor": nil}
			case "thread/queue/list":
				result = map[string]any{"data": []any{}}
			case "thread/queue/add":
				result = map[string]any{"queuedSubmission": map[string]any{"id": "queued", "clientUserMessageId": params["clientUserMessageId"], "input": params["input"]}}
			case "thread/resume":
				p.resumes++
				if p.stopAfterAdmission {
					scope := turnretry.Scope{Workspace: filepath.Dir(filepath.Dir(p.cwd)), Slug: "example", Root: "thread-1", Thread: thread, Socket: socket}
					state, err := p.client.RetryStore().Load(scope)
					if err != nil {
						p.t.Error(err)
					}
					for _, snapshot := range state.Snapshots {
						if snapshot.ID == planDeliveryID {
							failure = "synthetic interruption after durable admission"
						}
					}
				}
				result = map[string]any{"thread": map[string]any{"id": thread}}
			case "thread/settings/update":
				if mode, ok := params["collaborationMode"].(map[string]any); ok {
					p.updates++
					if p.failSettings {
						failure = "synthetic settings refusal"
						break
					}
					p.mode = mode["mode"].(string)
					p.writeMode()
					if p.failAdmissionSave {
						paths, _ := filepath.Glob(filepath.Join(p.client.RecoveryRoot, "turn-retries", "*", "example"))
						for _, path := range paths {
							if err := os.Chmod(path, 0500); err != nil {
								p.t.Error(err)
							}
						}
					}
					notify = map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": thread, "threadSettings": map[string]any{"model": "retained-model", "effort": "xhigh", "collaborationMode": map[string]any{"mode": p.mode}}}}
				}
			case "turn/start", "turn/steer":
				p.starts++
				text := params["input"].([]any)[0].(map[string]any)["text"].(string)
				id := params["clientUserMessageId"].(string)
				if text != "Implement the plan." && text != "manual message" {
					p.t.Error("delivery changed input", text)
				}
				p.latest, p.status = "implementation", "inProgress"
				p.items = []any{map[string]any{"turnId": p.latest, "item": map[string]any{"id": "input-" + id, "type": "userMessage", "clientId": id, "content": []any{map[string]any{"type": "text", "text": text}}}}}
				result = map[string]any{"turn": map[string]any{"id": p.latest}}
				if p.lost {
					p.hide = true
					result = map[string]any{"turn": map[string]any{}}
				}
				if p.failReceiptSave {
					paths, _ := filepath.Glob(filepath.Join(p.client.RecoveryRoot, "turn-retries", "*", "example"))
					for _, path := range paths {
						if err := os.Chmod(path, 0500); err != nil {
							p.t.Error(err)
						}
					}
				}
			default:
				failure = fmt.Sprint("unexpected plan fixture RPC: ", req["method"])
				p.t.Error(failure)
			}
			p.mu.Unlock()
			reply := map[string]any{"id": req["id"], "result": result}
			if failure != "" {
				delete(reply, "result")
				reply["error"] = map[string]any{"code": -32000, "message": failure}
			}
			out, _ := json.Marshal(reply)
			if conn.Write(context.Background(), websocket.MessageText, out) != nil {
				return
			}
			if notify != nil {
				out, _ = json.Marshal(notify)
				if conn.Write(context.Background(), websocket.MessageText, out) != nil {
					return
				}
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	p.t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
}

type planDeliveryFixture struct {
	s              *Server
	wire           *planDeliveryWire
	client         *planDeliveryClient
	ledger, socket string
}

type planDeliveryClient struct {
	*resolverRetryClient
	stateRoot, workspace string
}

func (c *planDeliveryClient) ListThreads(context.Context, codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error) {
	threads := []codex.ThreadMetadata{{ID: "thread-1", Cwd: c.cwd, Source: "vscode"}}
	store, err := teamruntime.NewStore(c.stateRoot, c.workspace)
	if err != nil {
		return nil, nil, err
	}
	roster, err := store.Load("example", "thread-1")
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if roster != nil {
		for _, member := range roster.Members {
			threads = append(threads, codex.ThreadMetadata{ID: member.Thread, Cwd: c.cwd, Source: "vscode"})
		}
	}
	return threads, nil, nil
}

func (c *planDeliveryClient) LoadedThreadIDs(ctx context.Context) ([]string, error) {
	threads, _, err := c.ListThreads(ctx, codex.ThreadListOptions{})
	ids := []string{}
	for _, thread := range threads {
		ids = append(ids, thread.ID)
	}
	return ids, err
}

func newPlanDeliveryFixture(t *testing.T) *planDeliveryFixture {
	t.Helper()
	s := newTestServer(t)
	dir, err := os.MkdirTemp("/tmp", "plan-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "native.sock")
	s.config.CodexSocket = socket
	cwd := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	w := &planDeliveryWire{t: t, cwd: cwd, rollout: filepath.Join(dir, "rollout.jsonl"), mode: "plan", latest: "plan-turn", status: "completed"}
	w.writeMode()
	w.serve(socket)
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: thread-1\n  socket_path: " + socket + "\n  client_version: 0.152.1\n"
	if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, cwd, "active")
	writeWebRuntimeAuthority(t, s, "example")
	if err := (session.RecoveryStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Set(context.Background(), "example", "thread-1", socket, true, true); err != nil {
		t.Fatal(err)
	}
	f := &planDeliveryFixture{s: s, wire: w, ledger: filepath.Join(s.config.UserStateRoot, "native.json"), socket: socket}
	f.restart(t)
	t.Cleanup(func() {
		paths, _ := filepath.Glob(filepath.Join(s.config.UserStateRoot, "turn-retries", "*", "example"))
		for _, path := range paths {
			_ = os.Chmod(path, 0700)
		}
	})
	return f
}

func (f *planDeliveryFixture) restart(t *testing.T) {
	t.Helper()
	if f.client != nil {
		f.client.Close()
	}
	c := workspacecodex.NewWithOptions(f.socket, f.s.config.Workspace, codex.ClientOptions{SubmissionLedgerPath: f.ledger, PreserveThreadInstructionsOnResume: true})
	c.RecoveryRoot = f.s.config.UserStateRoot
	c.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return codex.ThreadPolicy{}, nil }
	f.client = &planDeliveryClient{&resolverRetryClient{c, f.wire.cwd}, f.s.config.UserStateRoot, f.s.config.Workspace}
	f.s.config.Codex = f.client
	f.wire.mu.Lock()
	f.wire.client = c
	f.wire.mu.Unlock()
	t.Cleanup(c.Close)
}

func (f *planDeliveryFixture) request(ctx context.Context, id, turn, action string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"action":%q,"planContextVersion":2,"planTurnId":%q,"planSha256":%q,"clientUserMessageId":%q}`, action, turn, planDigest(deliveryPlan), id)
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/example/implement-plan", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Origin", f.s.config.BaseURL)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	return w
}

func TestPlanDeliveryManagedHTTPFreshAndPreparedRestart(t *testing.T) {
	for _, stage := range []string{"fresh", "prepared-plan", "prepared-default", "settings-refusal"} {
		t.Run(stage, func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			ctx := context.Background()
			if stage != "fresh" {
				if err := f.client.PrepareSend("thread-1", "Implement the plan.", planDeliveryID, "plan:plan-turn:"+planDigest(deliveryPlan), false); err != nil {
					t.Fatal(err)
				}
				if stage == "prepared-default" {
					mode := "default"
					if _, err := f.client.UpdateThreadSettings(ctx, "thread-1", codex.ThreadSettingsUpdate{CollaborationMode: &mode}); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "settings-refusal" {
					f.wire.mu.Lock()
					f.wire.failSettings = true
					f.wire.mu.Unlock()
					first := f.request(ctx, planDeliveryID, "plan-turn", "same")
					if first.Code < 400 {
						t.Fatal("settings refusal hidden", first.Code)
					}
					f.wire.mu.Lock()
					f.wire.failSettings = false
					f.wire.mu.Unlock()
				}
				f.restart(t)
			}
			response := f.request(ctx, planDeliveryID, "plan-turn", "same")
			if response.Code != http.StatusAccepted {
				t.Fatal(response.Code, response.Body.String())
			}
			f.wire.mu.Lock()
			starts, updates := f.wire.starts, f.wire.updates
			f.wire.mu.Unlock()
			if starts != 1 {
				t.Fatal("plan delivery did not preserve original single send", starts)
			}
			scope, err := f.client.RetryScope(ctx, "thread-1")
			if err != nil {
				t.Fatal(err)
			}
			state, err := f.client.RetryStore().Load(scope)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Snapshots) != 1 || state.Snapshots[0].Epoch != state.Epoch || state.Snapshots[0].ID != planDeliveryID || state.Snapshots[0].Turn != "implementation" {
				t.Fatal("admission used stale epoch or changed identity", state)
			}
			before := state
			f.restart(t)
			for _, action := range []string{"same", "recover"} {
				response = f.request(ctx, planDeliveryID, "plan-turn", action)
				if response.Code != http.StatusAccepted {
					t.Fatal("original receipt recovery blocked", response.Code, response.Body.String())
				}
			}
			if ack, err := f.client.AcknowledgeSends(ctx, "thread-1", []codex.SendAcknowledgement{{ClientUserMessageID: planDeliveryID, Digest: planDigest("Implement the plan.")}}); err != nil || len(ack) != 1 {
				t.Fatal(ack, err)
			}
			f.restart(t)
			response = f.request(ctx, planDeliveryID, "plan-turn", "same")
			if response.Code != http.StatusAccepted {
				t.Fatal("acknowledged receipt not recovered before stale-plan gate", response.Code, response.Body.String())
			}
			after, err := f.client.RetryStore().Load(scope)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("receipt replay recaptured settings", err)
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != starts || f.wire.updates != updates {
				t.Fatal("replay dispatched or changed mode", f.wire.starts, f.wire.updates)
			}
		})
	}
}

func TestPlanDeliveryPreparedExceptionKeepsOtherUnknownsAndIdentity(t *testing.T) {
	for _, change := range []string{"text", "action", "options", "another-root-send", "member-send", "stale-plan", "active-steering"} {
		t.Run(change, func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			text, action, options := "Implement the plan.", "plan:plan-turn:"+planDigest(deliveryPlan), (codex.TurnOptions{})
			if change == "text" {
				text = "changed"
			}
			if change == "action" {
				action = "changed"
			}
			if change == "options" {
				options.Model = "changed"
			}
			if err := f.client.PrepareSendWithOptions("thread-1", text, planDeliveryID, action, change == "active-steering", options); err != nil {
				t.Fatal(err)
			}
			if change == "another-root-send" {
				if err := f.client.PrepareSend("thread-1", "other", "other", "other", false); err != nil {
					t.Fatal(err)
				}
			}
			if change == "member-send" {
				service, err := f.s.teamService("example")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = service.Store.Update(context.Background(), "example", "thread-1", true, func(r *teamruntime.Roster) error {
					r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Thread: "member-thread", Model: "gpt-6.1-sol", Effort: "xhigh", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.client.PrepareSend("member-thread", "other", "other", "other", false); err != nil {
					t.Fatal(err)
				}
			}
			turn := "plan-turn"
			if change == "stale-plan" {
				turn = "older"
			}
			response := f.request(context.Background(), planDeliveryID, turn, "same")
			if response.Code < 400 {
				t.Fatal("invalid exception admitted", change, response.Code)
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != 0 {
				t.Fatal("invalid exception sent", change)
			}
		})
	}
}

func TestPlanDeliverySubmittingAndAcceptedSaveRecovery(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost=", lost), func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			f.wire.mu.Lock()
			f.wire.lost = lost
			f.wire.failReceiptSave = !lost
			f.wire.mu.Unlock()
			first := f.request(context.Background(), planDeliveryID, "plan-turn", "same")
			if first.Code != http.StatusServiceUnavailable {
				t.Fatal("uncertainty hidden", first.Code, first.Body.String())
			}
			paths, _ := filepath.Glob(filepath.Join(f.client.RecoveryRoot, "turn-retries", "*", "example"))
			for _, path := range paths {
				if err := os.Chmod(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			f.restart(t)
			if lost {
				unknown := f.request(context.Background(), planDeliveryID, "plan-turn", "same")
				if unknown.Code != http.StatusServiceUnavailable {
					t.Fatal("unknown was replayed", unknown.Code)
				}
			}
			f.wire.mu.Lock()
			f.wire.hide = false
			f.wire.mu.Unlock()
			recovered := f.request(context.Background(), planDeliveryID, "plan-turn", "same")
			if recovered.Code != http.StatusAccepted {
				t.Fatal(recovered.Code, recovered.Body.String())
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != 1 || f.wire.updates != 1 {
				t.Fatal("same-ID recovery repeated native or settings", f.wire.starts, f.wire.updates)
			}
		})
	}
}

func TestPlanDeliveryAdmissionPersistenceBoundaries(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint("after=", after), func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			f.wire.mu.Lock()
			f.wire.failAdmissionSave, f.wire.stopAfterAdmission = !after, after
			f.wire.mu.Unlock()
			first := f.request(context.Background(), planDeliveryID, "plan-turn", "same")
			if first.Code < 400 {
				t.Fatal("interrupted admission succeeded", first.Code)
			}
			paths, _ := filepath.Glob(filepath.Join(f.client.RecoveryRoot, "turn-retries", "*", "example"))
			for _, path := range paths {
				if err := os.Chmod(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			scope, err := f.client.RetryScope(context.Background(), "thread-1")
			if err != nil {
				t.Fatal(err)
			}
			state, err := f.client.RetryStore().Load(scope)
			if err != nil || (len(state.Snapshots) == 1) != after || len(state.Snapshots) > 1 {
				t.Fatal("interruption crossed admission boundary", state, err)
			}
			prepared, err := f.client.IsPreparedSendWithOptions("thread-1", "Implement the plan.", planDeliveryID, "plan:plan-turn:"+planDigest(deliveryPlan), codex.TurnOptions{})
			if err != nil || !prepared {
				t.Fatal("original prepared identity was lost", prepared, err)
			}
			f.wire.mu.Lock()
			if f.wire.starts != 0 || f.wire.updates != 1 {
				t.Fatal("interruption sent or repeated settings", f.wire.starts, f.wire.updates)
			}
			f.wire.failAdmissionSave, f.wire.stopAfterAdmission = false, false
			f.wire.mu.Unlock()
			f.restart(t)
			recovered := f.request(context.Background(), planDeliveryID, "plan-turn", "same")
			if recovered.Code != http.StatusAccepted {
				t.Fatal(recovered.Code, recovered.Body.String())
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != 1 || f.wire.updates != 1 {
				t.Fatal("recovery repeated delivery or settings", f.wire.starts, f.wire.updates)
			}
		})
	}
}
