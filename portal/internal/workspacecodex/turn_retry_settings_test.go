package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/coder/websocket"
)

// This fixture drives the production wrappers and exact rollout/history reader.
// It models native warm-object retention, not a native compatibility test.
type retrySettingsWire struct {
	mu                                     sync.Mutex
	t                                      *testing.T
	client                                 *Client
	scope                                  turnretry.Scope
	cwd, rollout                           string
	thread, turn, status, loaded, provider string
	model, effort, mode                    string
	policy                                 codex.ThreadPolicy
	bound                                  bool
	queue                                  []codex.QueueEntry
	items                                  []any
	resumes, settings, policyUpdates       int
	starts                                 int
	failureCode                            string
	pendingQuestion                        bool
	boundary                               bool
	change                                 string
	recordDirectory                        string
}

func newRetrySettingsWire(t *testing.T, member bool, initialMode string) *retrySettingsWire {
	return newRetrySettingsWireWithServer(t, member, initialMode, nil)
}

func newRetrySettingsWireWithServer(t *testing.T, member bool, initialMode string, serve func(*retrySettingsWire, *websocket.Conn) error) *retrySettingsWire {
	t.Helper()
	workspace := t.TempDir()
	w := &retrySettingsWire{t: t, thread: "root", turn: "manual", status: "inProgress", loaded: "active", provider: "fixture", model: "old-model", effort: "low", mode: initialMode, failureCode: "serverOverloaded"}
	if member {
		w.thread = "member"
	}
	w.cwd = filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(w.cwd, 0700); err != nil {
		t.Fatal(err)
	}
	w.rollout = filepath.Join(t.TempDir(), "rollout.jsonl")
	w.writeRollout(w.turn)
	handler := w.serve
	if serve != nil {
		handler = func(conn *websocket.Conn) error { return serve(w, conn) }
	}
	socket := serveUnixWebsocket(t, handler)
	for name, body := range map[string]string{
		"state.md": "---\nlifecycle: active\n---\n", "plan.md": "# synthetic managed settings\n",
		"portal.yml": "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root\n  socket_path: " + socket + "\n  client_version: 0.163.0\n",
	} {
		if err := os.WriteFile(filepath.Join(w.cwd, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w.client = NewWithOptions(socket, workspace, codex.ClientOptions{DeveloperInstructions: "common lifecycle", PreserveThreadInstructionsOnResume: true, SubmissionLedgerPath: filepath.Join(t.TempDir(), "native.json")})
	t.Cleanup(func() { w.client.Close() })
	w.client.RecoveryRoot = t.TempDir()
	w.policy = codex.ThreadPolicy{DeveloperInstructions: "retained root", Sandbox: "danger-full-access", ApprovalPolicy: "never", MCPServer: &codex.ThreadMCPServer{Name: "team", Command: "/package/bin/workspace-portal", Args: []string{"team-mcp", "--slug", "example"}, Tool: "report_to_lead"}}
	if member {
		w.policy.DeveloperInstructions = "retained implementer"
	}
	w.client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		policy := w.policy
		if w.boundary {
			switch w.change {
			case "policy":
				policy.DeveloperInstructions = "changed role"
			case "mcp":
				server := *policy.MCPServer
				server.Command = "/other/package/bin/workspace-portal"
				policy.MCPServer = &server
			case "access":
				policy.Sandbox = "workspace-write"
			case "common":
				w.client.retryCommonInstructions = "changed lifecycle"
			case "authority-error":
				return policy, errors.New("synthetic authority failure")
			}
		}
		return policy, nil
	}
	w.scope = turnretry.Scope{Workspace: workspace, Slug: "example", Root: "root", Thread: w.thread, Socket: socket}
	recovery := session.RecoveryStore{Root: w.client.RecoveryRoot, Workspace: workspace}
	if err := recovery.Set(context.Background(), "example", "root", socket, true, true); err != nil {
		t.Fatal(err)
	}
	if member {
		if err := recovery.Update(context.Background(), "example", func(r *session.Recovery) error { r.ActiveThreads = append(r.ActiveThreads, "member"); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// Establish prior proof through the real explicit managed owner, including
	// the native MCP/instruction/access request, rather than seeding a digest.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.client.ActivateThread(ctx, w.thread, w.policy); err != nil {
		t.Fatal(err)
	}
	tx, err := w.client.RetryStore().Lock(ctx, w.scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Enroll(turnretry.Observation{})
	id, _ := turnretry.NewID()
	tx.State.Cyber = append(tx.State.Cyber, turnretry.Charge{ID: id, AtMS: time.Now().UnixMilli(), Accepted: true})
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	paths, err := filepath.Glob(filepath.Join(w.client.RecoveryRoot, "turn-retries", "*", "example", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal(paths, err)
	}
	w.recordDirectory = filepath.Dir(paths[0])
	t.Cleanup(func() { _ = os.Chmod(w.recordDirectory, 0700) })
	return w
}

func (w *retrySettingsWire) writeRollout(turn string) {
	w.t.Helper()
	records := []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": w.thread, "cwd": w.cwd}},
		map[string]any{"type": "turn_context", "payload": map[string]any{"turn_id": turn, "cwd": w.cwd, "model": w.model, "effort": w.effort, "collaboration_mode": map[string]any{"mode": w.mode}, "approval_policy": "never", "sandbox_policy": map[string]any{"type": "danger-full-access"}, "approvals_reviewer": "user", "permission_profile": map[string]any{"type": "disabled"}}},
	}
	var data []byte
	for _, record := range records {
		line, _ := json.Marshal(record)
		data = append(data, append(line, '\n')...)
	}
	if err := os.WriteFile(w.rollout, data, 0600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *retrySettingsWire) serve(conn *websocket.Conn) error {
	defer conn.CloseNow()
	if err := handshake(conn); err != nil {
		return err
	}
	for {
		req, err := readObject(conn)
		if err != nil {
			return nil
		}
		w.mu.Lock()
		result, failure, notify := w.reply(req)
		var question any
		if w.pendingQuestion && req["method"] == "thread/read" {
			w.pendingQuestion = false
			question = map[string]any{"id": "fixture-question", "method": "item/tool/requestUserInput", "params": map[string]any{"threadId": w.thread, "turnId": w.turn, "itemId": "question-item", "questions": []any{map[string]any{"id": "q", "header": "Fixture", "question": "Synthetic pending question", "options": nil}}}}
		}
		w.mu.Unlock()
		if question != nil {
			if err := writeObject(conn, question); err != nil {
				return nil
			}
		}
		reply := map[string]any{"id": req["id"], "result": result}
		if failure != "" {
			delete(reply, "result")
			reply["error"] = map[string]any{"code": -32000, "message": failure}
		}
		if err := writeObject(conn, reply); err != nil {
			return nil
		}
		if notify != nil {
			if err := writeObject(conn, notify); err != nil {
				return nil
			}
		}
	}
}

func (w *retrySettingsWire) reply(req map[string]any) (any, string, any) {
	p, _ := req["params"].(map[string]any)
	switch req["method"] {
	case "thread/read":
		if w.boundary && w.change == "postcheck-error" {
			return nil, "synthetic postcheck failure", nil
		}
		return map[string]any{"thread": map[string]any{"id": w.thread, "cwd": w.cwd, "source": "vscode", "path": w.rollout, "modelProvider": w.provider, "model": w.model, "reasoningEffort": w.effort, "status": map[string]any{"type": w.loaded}}}, "", nil
	case "thread/turns/list":
		return map[string]any{"data": []any{map[string]any{"id": w.turn, "status": w.status, "error": map[string]any{"codexErrorInfo": w.failureCode, "message": "synthetic terminal error"}}}}, "", nil
	case "thread/resume":
		w.resumes++
		if !w.bound {
			config, _ := p["config"].(map[string]any)
			servers, _ := config["mcp_servers"].(map[string]any)
			server, _ := servers["team"].(map[string]any)
			tools, _ := server["tools"].(map[string]any)
			tool, _ := tools["report_to_lead"].(map[string]any)
			if p["developerInstructions"] != "common lifecycle\n\n"+w.policy.DeveloperInstructions || p["approvalPolicy"] != "never" || p["sandbox"] != "danger-full-access" || server["command"] != w.policy.MCPServer.Command || fmt.Sprint(server["args"]) != "[team-mcp --slug example]" || fmt.Sprint(server["enabled_tools"]) != "[report_to_lead]" || server["required"] != true || tool["approval_mode"] != "approve" {
				w.t.Errorf("explicit owner lost inherited policy/MCP: %#v", p)
			}
			w.bound = true
		} else {
			for _, key := range []string{"config", "developerInstructions", "sandbox", "approvalPolicy", "model", "effort", "collaborationMode"} {
				if _, present := p[key]; present {
					w.t.Errorf("implicit warm settings resume changed %s: %#v", key, p)
				}
			}
		}
		return map[string]any{"thread": map[string]any{"id": w.thread}}, "", nil
	case "thread/settings/update":
		// The explicit owner and queue permission preparation also use this RPC.
		if p["model"] == nil && p["effort"] == nil && p["collaborationMode"] == nil {
			w.policyUpdates++
			return map[string]any{}, "", nil
		}
		w.settings++
		saved, err := w.client.RetryStore().Load(w.scope)
		if err != nil || saved.Epoch != 2 || saved.Binding != nil || saved.Handled != "manual" {
			w.t.Errorf("settings preceded durable invalidation: %+v %v", saved, err)
		}
		w.boundary = true
		if w.change == "rpc-failed" {
			return nil, "synthetic settings failure", nil
		}
		if model, ok := p["model"].(string); ok {
			w.model = model
		}
		if effort, ok := p["effort"].(string); ok {
			w.effort = effort
		}
		if mode, ok := p["collaborationMode"].(map[string]any); ok {
			w.mode = mode["mode"].(string)
		}
		switch w.change {
		case "completed":
			w.status, w.loaded = "completed", "idle"
		case "turnover":
			w.turn = "pre-settings-queue"
		case "idle":
			w.loaded = "idle"
		case "cold":
			w.loaded = "notLoaded"
		case "provider":
			w.provider = "changed-provider"
		case "socket":
			if err := os.Chmod(w.scope.Socket, 0600); err != nil {
				w.t.Error(err)
			}
		case "save-failed":
			if err := os.Chmod(w.recordDirectory, 0500); err != nil {
				w.t.Error(err)
			}
		}
		if w.change == "rpc-unknown" {
			return map[string]any{}, "", nil
		}
		notify := map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": w.thread, "threadSettings": map[string]any{"model": w.model, "effort": w.effort, "collaborationMode": map[string]any{"mode": w.mode}}}}
		return map[string]any{}, "", notify
	case "thread/queue/add":
		entry := codex.QueueEntry{ID: fmt.Sprint("queue-", len(w.queue)), ClientUserMessageID: p["clientUserMessageId"].(string), Text: p["input"].([]any)[0].(map[string]any)["text"].(string)}
		w.queue = append(w.queue, entry)
		return map[string]any{"queuedSubmission": map[string]any{"id": entry.ID, "clientUserMessageId": entry.ClientUserMessageID, "input": p["input"]}}, "", nil
	case "thread/items/list":
		return map[string]any{"data": w.items, "nextCursor": nil}, "", nil
	case "thread/queue/list":
		entries := []any{}
		for _, entry := range w.queue {
			entries = append(entries, map[string]any{"id": entry.ID, "clientUserMessageId": entry.ClientUserMessageID, "input": []any{map[string]any{"type": "text", "text": entry.Text}}})
		}
		return map[string]any{"data": entries, "nextCursor": nil}, "", nil
	case "turn/start":
		w.starts++
		text := p["input"].([]any)[0].(map[string]any)["text"].(string)
		if p["model"] != w.model || p["effort"] != w.effort || text != turnretry.Continuation {
			w.t.Error("automatic input changed proven settings or continuation", p)
		}
		w.turn, w.status, w.loaded = "automatic", "inProgress", "active"
		w.items = retrySettingsInput(w.turn, p["clientUserMessageId"].(string), text)
		return map[string]any{"turn": map[string]any{"id": w.turn}}, "", nil
	default:
		w.t.Errorf("unexpected native call: %v", req["method"])
		return nil, "unexpected synthetic call", nil
	}
}

func TestRetryWarmSettingsActiveQueueProductionProof(t *testing.T) {
	model, effort, plan, normal := "new-model", "xhigh", "plan", "default"
	for name, update := range map[string]codex.ThreadSettingsUpdate{"model": {Model: &model}, "effort": {ReasoningEffort: &effort}, "default-to-plan": {CollaborationMode: &plan}, "plan-to-default": {CollaborationMode: &normal}} {
		for _, member := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/member=%t", name, member), func(t *testing.T) {
				initial := "default"
				if name == "plan-to-default" {
					initial = "plan"
				}
				w := newRetrySettingsWire(t, member, initial)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				old, err := w.client.QueueWithPolicy(ctx, w.thread, "old task", "old-client", w.policy)
				if err != nil {
					t.Fatal(err)
				}
				before, err := w.client.RetryStore().Load(w.scope)
				if err != nil {
					t.Fatal(err)
				}
				result, err := w.client.UpdateThreadSettings(ctx, w.thread, update)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := w.client.RetryStore().Load(w.scope)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Binding == nil || saved.Epoch != 2 || saved.Binding.Epoch != 2 || saved.Binding.Digest == before.Binding.Digest || !reflect.DeepEqual(saved.Binding.Policy, before.Binding.Policy) || saved.Binding.CommonInstructions != before.Binding.CommonInstructions || !reflect.DeepEqual(saved.Snapshots, before.Snapshots) || !reflect.DeepEqual(saved.Cyber, before.Cyber) || !saved.Enabled || !saved.Enrolled || saved.Handled != "manual" {
					t.Fatalf("promotion changed old authority: %+v", saved)
				}
				fresh, err := w.client.QueueWithPolicy(ctx, w.thread, "new independent task", "new-client", w.policy)
				if err != nil {
					t.Fatal(err)
				}
				w.mu.Lock()
				if len(w.queue) != 2 || !reflect.DeepEqual(w.queue[0], old) || !reflect.DeepEqual(w.queue[1], fresh) || w.resumes != 2 || w.settings != 1 || !w.bound {
					t.Errorf("queue/FIFO or warm binding changed: %+v resumes=%d settings=%d", w.queue, w.resumes, w.settings)
				}
				policyUpdates := w.policyUpdates
				// A pre-settings queue failure stays manual even with a new binding.
				w.turn, w.status, w.loaded = "executed-old-queue", "failed", "idle"
				w.items = retrySettingsInput(w.turn, "old-client", old.Text)
				w.writeRollout(w.turn)
				w.mu.Unlock()
				controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Clock: func() time.Time { return time.Unix(2000000000, 0) }}
				if err := controller.Step(ctx, w.scope); !errors.Is(err, turnretry.ErrMissingProof) {
					t.Fatal("old queue acquired new-epoch authority", err)
				}
				w.mu.Lock()
				// Receipt and exact native context must prove execution with the
				// requested settings, rather than relying on enqueue defaults.
				w.model, w.effort, w.mode = result.Model, result.ReasoningEffort, result.CollaborationMode
				w.turn, w.status, w.loaded = "executed-new-queue", "failed", "idle"
				w.items = retrySettingsInput(w.turn, "new-client", fresh.Text)
				w.writeRollout(w.turn)
				w.mu.Unlock()
				if err := controller.Step(ctx, w.scope); err != nil {
					t.Fatal(err)
				}
				saved, err = w.client.RetryStore().Load(w.scope)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Phase != "scheduled" || saved.Pending == nil || saved.Pending.Snapshot.Epoch != 2 || saved.Pending.Snapshot.Options.Model != result.Model || saved.Pending.Snapshot.Options.ReasoningEffort != result.ReasoningEffort || saved.Pending.Snapshot.Mode != result.CollaborationMode || !reflect.DeepEqual(saved.Pending.Snapshot.Options.ThreadPolicy, w.policy) {
					t.Fatalf("new active queue did not schedule with exact proof: %+v", saved)
				}
				if err := w.client.RetryBackend().Check(ctx, w.scope, saved.Pending.Snapshot); err != nil {
					t.Fatal(err)
				}
				w.mu.Lock()
				if w.resumes != 2 || w.settings != 1 || w.policyUpdates != policyUpdates || len(w.queue) != 2 {
					t.Error("retry proof/scheduling mutated native binding or queue")
				}
				w.mu.Unlock()
			})
		}
	}
}

func TestRetryWarmSettingsPromotionRequiresPositiveContinuity(t *testing.T) {
	for _, change := range []string{"completed", "turnover", "idle", "cold", "pre-cold", "pre-idle", "pre-provider", "pre-policy", "pre-common", "pre-socket", "provider", "socket", "policy", "mcp", "access", "common", "authority-error", "missing-prior", "rpc-failed", "rpc-unknown", "postcheck-error", "save-failed"} {
		t.Run(change, func(t *testing.T) {
			w := newRetrySettingsWire(t, true, "default")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if change == "missing-prior" {
				tx, err := w.client.RetryStore().Lock(ctx, w.scope)
				if err != nil {
					t.Fatal(err)
				}
				tx.State.Binding = nil
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
			}
			w.mu.Lock()
			w.change = change
			if change == "pre-cold" {
				w.loaded = "notLoaded"
			}
			if change == "pre-idle" {
				w.loaded = "idle"
			}
			switch change {
			case "pre-provider":
				w.provider = "changed-provider"
			case "pre-policy":
				w.policy.DeveloperInstructions = "changed role"
			case "pre-common":
				w.client.retryCommonInstructions = "changed lifecycle"
			case "pre-socket":
				if err := os.Chmod(w.scope.Socket, 0600); err != nil {
					t.Fatal(err)
				}
			}
			w.mu.Unlock()
			callCtx := ctx
			if change == "rpc-unknown" {
				var stop context.CancelFunc
				callCtx, stop = context.WithTimeout(ctx, 150*time.Millisecond)
				defer stop()
			}
			model := "new-model"
			_, err := w.client.UpdateThreadSettings(callCtx, w.thread, codex.ThreadSettingsUpdate{Model: &model})
			if (change == "rpc-failed" || change == "rpc-unknown" || change == "save-failed") && err == nil {
				t.Fatal("failed/unknown effect or persistence reported success")
			}
			if err := os.Chmod(w.recordDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			saved, err := w.client.RetryStore().Load(w.scope)
			if err != nil || saved.Epoch != 2 || saved.Binding != nil || saved.Handled != "manual" || !saved.Enabled || !saved.Enrolled || len(saved.Cyber) != 1 {
				t.Fatalf("unproved continuity restored authority: %+v %v", saved, err)
			}
		})
	}
}

func retrySettingsInput(turn, clientID, text string) []any {
	return []any{map[string]any{"turnId": turn, "item": map[string]any{"id": "native-input", "type": "userMessage", "clientId": clientID, "content": []any{map[string]any{"type": "text", "text": text}}}}}
}

func TestRetryWarmSettingsRefusesEffectBeforeDurableBoundary(t *testing.T) {
	w := newRetrySettingsWire(t, false, "default")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := os.Chmod(w.recordDirectory, 0500); err != nil {
		t.Fatal(err)
	}
	model := "new-model"
	if _, err := w.client.UpdateThreadSettings(ctx, w.thread, codex.ThreadSettingsUpdate{Model: &model}); err == nil {
		t.Fatal("settings effect permitted without durable epoch")
	}
	if err := os.Chmod(w.recordDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	if w.resumes != 1 || w.settings != 0 {
		t.Error("failed epoch save allowed native mutation")
	}
	w.mu.Unlock()
	saved, err := w.client.RetryStore().Load(w.scope)
	if err != nil || saved.Epoch != 1 || saved.Binding == nil {
		t.Fatal("failed pre-effect save changed persisted authority", saved, err)
	}
}

func TestRetryWarmSettingsPreservesUncertainOldTuple(t *testing.T) {
	w := newRetrySettingsWire(t, true, "default")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := w.client.RetryStore().Lock(ctx, w.scope)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, snapshot := retryProofFixture()
	snapshot.Turn, snapshot.Proven, snapshot.Mode = "old-failure", true, "default"
	snapshot.Options.ThreadPolicy = tx.State.Binding.Policy
	snapshot.BindingDigest = tx.State.Binding.Digest
	snapshot.Digest, _ = snapshot.Hash()
	tx.State.AddSnapshot(snapshot)
	if err := tx.State.Observe(turnretry.Observation{Turn: snapshot.Turn, Status: "failed", Code: "serverOverloaded"}, time.Now(), 0, turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.State.Begin(time.Now().Add(15*time.Second), turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	before := *tx.State.Pending
	tx.Close()
	model := "new-model"
	if _, err := w.client.UpdateThreadSettings(ctx, w.thread, codex.ThreadSettingsUpdate{Model: &model}); err == nil {
		t.Fatal("uncertain automatic send allowed settings effect")
	}
	saved, err := w.client.RetryStore().Load(w.scope)
	if err != nil || saved.Epoch != 2 || saved.Binding != nil || !saved.Cancelled || !saved.Uncertain() || !reflect.DeepEqual(saved.Pending, &before) {
		t.Fatal("settings changed uncertain reconciliation identity", saved, err)
	}
	w.mu.Lock()
	if w.resumes != 1 || w.settings != 0 {
		t.Error("uncertainty allowed native settings effect")
	}
	w.mu.Unlock()
}
