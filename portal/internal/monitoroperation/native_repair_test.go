package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

type repairConfiguredClient struct {
	*workspacecodex.Client
	active        *atomic.Bool
	proofs        int
	discards      int
	afterDiscard  func()
	afterProof    func()
	beforeDiscard func()
	proofOverride func() (bool, error)
	discardError  error
	sendOverride  error
	lastSendErr   error
}

func (c *repairConfiguredClient) SendWithOptions(ctx context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
	if c.sendOverride != nil {
		return codex.SendReceipt{}, c.sendOverride
	}
	receipt, err := c.Client.SendWithOptions(ctx, thread, text, id, action, options)
	c.lastSendErr = err
	return receipt, err
}

func (c *repairConfiguredClient) ReadActivity(_ context.Context, thread string) (codex.ActivitySnapshot, error) {
	state := "idle"
	if c.active.Load() {
		state = "working"
	}
	return codex.ActivitySnapshot{ThreadID: thread, CurrentState: state}, nil
}
func (c *repairConfiguredClient) IsPreparedSendWithOptions(thread, text, id, action string, options codex.TurnOptions) (bool, error) {
	c.proofs++
	if c.proofOverride != nil {
		return c.proofOverride()
	}
	prepared, err := c.Client.IsPreparedSendWithOptions(thread, text, id, action, options)
	if c.afterProof != nil {
		c.afterProof()
	}
	return prepared, err
}
func (c *repairConfiguredClient) DiscardPreparedSendWithOptions(thread, text, id, action string, options codex.TurnOptions) (bool, error) {
	c.discards++
	if c.beforeDiscard != nil {
		c.beforeDiscard()
	}
	if c.discardError != nil {
		return false, c.discardError
	}
	retired, err := c.Client.DiscardPreparedSendWithOptions(thread, text, id, action, options)
	if retired && err == nil && c.afterDiscard != nil {
		c.afterDiscard()
	}
	return retired, err
}

type monitorRepairFixture struct {
	service   *DeliveryService
	native    *NativeDelivery
	client    *repairConfiguredClient
	open      func() *repairConfiguredClient
	op        Operation
	steers    atomic.Int32
	starts    atomic.Int32
	accepted  atomic.Int32
	steerCode atomic.Int32
	held      atomic.Bool
}

func configuredMonitorRepair(t *testing.T, outcome string) *monitorRepairFixture {
	t.Helper()
	t.Setenv("DEV_WORKSPACE_NAME", "")
	socket, listener := shortMonitorSocket(t)
	epoch, err := session.SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	s, _, op := deliveryFixtureSocket(t, true, socket, epoch)
	f := &monitorRepairFixture{service: s, op: op}
	f.steerCode.Store(-32600)
	if outcome == "unknown" {
		f.steerCode.Store(-32000)
	}
	if outcome == "accepted" {
		f.steerCode.Store(0)
	}
	cwd := filepath.Join(op.Workspace, "work", op.Owner.Slug)
	state, authority := t.TempDir(), filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(authority, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"plan.md": "Fixture\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: lead\n  socket_path: %s\n  client_version: 0.165.0\n", op.Owner.Slug, socket)} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Find(op.Workspace, op.Owner.Slug); err != nil {
		t.Fatalf("fixture session identity: %v", err)
	}
	rollout := filepath.Join(state, "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0600); err != nil {
		t.Fatal(err)
	}
	authorityRecord := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: op.Owner.Slug, Workspace: op.Workspace, TmuxSocket: filepath.Join(filepath.Dir(socket), "tmux"), TmuxSessionID: "$1", CodexThreadID: "lead", CodexSocketPath: socket, CodexClientVersion: "0.165.0"}
	data, _ := json.Marshal(authorityRecord)
	if err := os.WriteFile(filepath.Join(authority, op.Owner.Slug+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := session.LoadRuntimeAuthority(authority, op.Owner.Slug, op.Workspace); err != nil {
		t.Fatalf("fixture runtime authority: %v", err)
	}
	if err := (session.RecoveryStore{Root: state, Workspace: op.Workspace}).Set(context.Background(), op.Owner.Slug, "lead", socket, true, true); err != nil {
		t.Fatal(err)
	}
	var active, queued atomic.Bool
	active.Store(true)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]any
			if json.Unmarshal(data, &request) != nil || request["id"] == nil {
				continue
			}
			params, _ := request["params"].(map[string]any)
			response := map[string]any{"id": request["id"], "result": map[string]any{}}
			switch request["method"] {
			case "initialize":
				response["result"] = map[string]any{"userAgent": "configured-monitor-fixture"}
			case "thread/read":
				status := "idle"
				if active.Load() {
					status = "active"
				}
				response["result"] = map[string]any{"thread": map[string]any{"id": "lead", "cwd": cwd, "path": rollout, "model": "saved-model", "reasoningEffort": "xhigh", "modelProvider": "fixture", "source": "vscode", "ephemeral": false, "historyMode": "paginated", "status": map[string]any{"type": status}}}
			case "thread/turns/list":
				turns := []any{}
				if active.Load() {
					turns = append(turns, map[string]any{"id": "busy-turn", "status": "inProgress"})
				}
				response["result"] = map[string]any{"data": turns}
			case "thread/items/list":
				response["result"] = map[string]any{"data": []any{}, "nextCursor": nil}
			case "thread/resume", "thread/settings/update":
				if outcome == "absent" && request["method"] == "thread/resume" {
					delete(response, "result")
					response["error"] = map[string]any{"code": -32600, "message": "fixture resume refused before preparation"}
				}
			case "thread/queue/add":
				queued.Store(true)
				response["result"] = map[string]any{"queuedSubmission": map[string]any{"id": "queued-original", "clientUserMessageId": params["clientUserMessageId"], "input": params["input"]}}
			case "thread/queue/list":
				rows := []any{}
				if queued.Load() {
					rows = append(rows, map[string]any{"id": "queued-original", "clientUserMessageId": op.Events[0].Delivery.MessageID, "input": []any{map[string]any{"type": "text", "text": op.Events[0].Delivery.Envelope}}})
				}
				response["result"] = map[string]any{"data": rows, "nextCursor": nil}
			case "turn/steer", "turn/start":
				input, _ := params["input"].([]any)
				if params["threadId"] != "lead" || params["clientUserMessageId"] != op.Events[0].Delivery.MessageID || len(input) != 1 {
					t.Error("configured dispatch changed the event identity")
					return
				}
				item, _ := input[0].(map[string]any)
				if item["text"] != op.Events[0].Delivery.Envelope {
					t.Error("configured dispatch changed the event body")
					return
				}
				if request["method"] == "turn/steer" {
					f.steers.Add(1)
					if params["expectedTurnId"] != "busy-turn" || params["model"] != nil || params["effort"] != nil {
						t.Error("steer lost its active-turn contract")
						return
					}
					if f.steerCode.Load() == 0 {
						f.accepted.Add(1)
						response["result"] = map[string]any{"turnId": "busy-turn"}
					} else {
						active.Store(false) // The selected turn ends before admission.
						delete(response, "result")
						response["error"] = map[string]any{"code": f.steerCode.Load(), "message": "no active turn to steer"}
					}
				} else {
					f.starts.Add(1)
					f.accepted.Add(1)
					if params["model"] != "saved-model" || params["effort"] != "xhigh" {
						t.Error("idle routing changed saved options")
						return
					}
					response["result"] = map[string]any{"turn": map[string]any{"id": "accepted-idle-turn"}}
				}
			default:
				delete(response, "result")
				response["error"] = map[string]any{"code": -32601, "message": fmt.Sprint(request["method"])}
			}
			encoded, _ := json.Marshal(response)
			if conn.Write(r.Context(), websocket.MessageText, encoded) != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	options := codex.ClientOptions{SubmissionDatabasePath: filepath.Join(state, "receipts.sqlite"), SubmissionLedgerPath: filepath.Join(state, "receipts.json"), PreserveThreadInstructionsOnResume: true}
	f.open = func() *repairConfiguredClient {
		client := workspacecodex.NewWithOptions(socket, op.Workspace, options)
		client.RecoveryRoot = state
		client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return codex.ThreadPolicy{}, nil }
		t.Cleanup(client.Close)
		return &repairConfiguredClient{Client: client, active: &active}
	}
	f.client = f.open()
	f.native = &NativeDelivery{Workspace: op.Workspace, StateRoot: state, AuthorityDir: authority, Socket: socket, Client: f.client,
		Admission: func(Owner) error {
			if f.held.Load() {
				return errors.New("fixture readiness hold")
			}
			return nil
		},
		PrepareLead: func(context.Context, Owner) (codex.TurnOptions, error) {
			return codex.TurnOptions{Model: "saved-model", ReasoningEffort: "xhigh"}, nil
		},
	}
	s.Transport = f.native
	return f
}

func TestMonitorConfiguredOutcomeRepairsUnsentRouteOnLaterScan(t *testing.T) {
	for _, scenario := range []string{"later-scan", "crash-after-discard", "revoked-after-discard", "held-after-discard"} {
		t.Run(scenario, func(t *testing.T) {
			f := configuredMonitorRepair(t, "rejected")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if scenario == "crash-after-discard" {
				f.client.afterDiscard = func() { panic("crash after committed discard") }
				func() {
					defer func() {
						if got := recover(); got != "crash after committed discard" {
							t.Fatalf("unexpected crash: %v", got)
						}
					}()
					_ = f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID)
				}()
				f.client.afterDiscard = nil
			} else if err := f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID); err != nil {
				t.Fatal(err)
			}
			saved, err := f.service.Store.Get(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "held"
			if scenario == "crash-after-discard" {
				want = "submitting"
			}
			if saved.Events[0].Delivery.Status != want || f.steers.Load() != 1 || f.starts.Load() != 0 || f.client.proofs != 1 || f.client.discards != 1 {
				t.Fatalf("repair did not retire once without inline resend: %+v steer=%d start=%d proofs=%d discard=%d sendError=%v", saved.Events[0].Delivery, f.steers.Load(), f.starts.Load(), f.client.proofs, f.client.discards, f.client.lastSendErr)
			}
			before := saved.Events[0].Delivery
			scope, err := f.client.RetryScope(ctx, "lead")
			if err != nil {
				t.Fatal(err)
			}
			managed, err := f.client.RetryStore().Load(scope)
			if err != nil {
				t.Fatal(err)
			}
			if len(managed.Snapshots) != 1 || managed.Snapshots[0].ID != before.MessageID {
				t.Fatal("managed admission lost the original input")
			}
			originalSnapshot := managed.Snapshots[0]
			if scenario == "held-after-discard" {
				f.held.Store(true)
				if err := f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID); err != nil {
					t.Fatal(err)
				}
				if f.starts.Load() != 0 {
					t.Fatal("next scan bypassed current readiness")
				}
				f.held.Store(false)
			}
			if scenario == "revoked-after-discard" {
				tx, err := f.service.Store.Lock(ctx, f.op.ID)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				tx.Operation.CanceledAt = &now
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
			}
			f.client.Close()
			f.client = f.open()
			f.native.Client = f.client
			for i := 0; i < 2; i++ {
				if err := f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			saved, err = f.service.Store.Get(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			after := saved.Events[0].Delivery
			if before.MessageID != after.MessageID || before.Envelope != after.Envelope || before.Context != after.Context || !reflect.DeepEqual(before.Options, after.Options) {
				t.Fatal("re-admission changed the immutable input")
			}
			if scenario == "revoked-after-discard" {
				if after.Status != "canceled" || f.starts.Load() != 0 {
					t.Fatal("revoked continuation was re-admitted")
				}
				return
			}
			if after.Status != "accepted" || after.Receipt.TurnID != "accepted-idle-turn" || f.starts.Load() != 1 || f.accepted.Load() != 1 || f.client.discards != 0 {
				t.Fatal("later scan did not accept the original input exactly once")
			}
			managed, err = f.client.RetryStore().Load(scope)
			if err != nil {
				t.Fatal(err)
			}
			if len(managed.Snapshots) != 1 || managed.Snapshots[0].ID != originalSnapshot.ID || managed.Snapshots[0].TextDigest != originalSnapshot.TextDigest || !reflect.DeepEqual(managed.Snapshots[0].Options, originalSnapshot.Options) {
				t.Fatal("re-admission replaced the managed snapshot")
			}
			f.client.Close()
			f.client = f.open()
			f.native.Client = f.client
			if receipt, found, err := f.native.Reconcile(ctx, f.op.Owner, saved.Events[0]); err != nil || !found || !reflect.DeepEqual(&receipt, after.Receipt) {
				t.Fatalf("accepted repaired input lost its original restart receipt: %+v %t %v", receipt, found, err)
			}
			if err := f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID); err != nil || f.accepted.Load() != 1 {
				t.Fatal("accepted repaired input replayed after restart", err)
			}
		})
	}
}

func TestMonitorConfiguredOutcomeNeverResetsUnknownOrAccepted(t *testing.T) {
	for _, outcome := range []string{"unknown", "accepted", "queued"} {
		t.Run(outcome, func(t *testing.T) {
			f := configuredMonitorRepair(t, outcome)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if outcome == "queued" {
				d := f.op.Events[0].Delivery
				if _, err := f.client.QueueWithPolicy(ctx, "lead", d.Envelope, d.MessageID, codex.ThreadPolicy{}); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			saved, err := f.service.Store.Get(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "submitting"
			if outcome == "accepted" {
				want = "accepted"
			}
			wantSteers := int32(1)
			if outcome == "queued" {
				wantSteers = 0
			}
			if saved.Events[0].Delivery.Status != want || f.client.proofs != 0 || f.client.discards != 0 || f.steers.Load() != wantSteers || f.starts.Load() != 0 {
				t.Fatal("unknown or accepted input entered routing repair")
			}
		})
	}
}

func TestMonitorConfiguredOutcomeRequiresPreparedProofAndCurrentAdmission(t *testing.T) {
	for _, scenario := range []string{"absent", "tuple-mismatch", "proof-error", "discard-error", "readiness-hold", "changed-to-accepted", "changed-to-submitting", "joined-unknown"} {
		t.Run(scenario, func(t *testing.T) {
			outcome := "rejected"
			if scenario == "absent" {
				outcome = "absent"
			}
			f := configuredMonitorRepair(t, outcome)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			d := f.op.Events[0].Delivery
			switch scenario {
			case "tuple-mismatch":
				f.client.proofOverride = func() (bool, error) {
					return f.client.Client.IsPreparedSendWithOptions("lead", d.Envelope, d.MessageID, d.Context+":different", codex.TurnOptions{Model: "saved-model", ReasoningEffort: "xhigh"})
				}
			case "proof-error":
				f.client.proofOverride = func() (bool, error) { return false, errors.New("fixture proof storage error") }
			case "discard-error":
				f.client.discardError = errors.New("fixture discard storage error")
			case "readiness-hold":
				f.client.afterProof = func() { f.held.Store(true) }
			case "changed-to-accepted", "changed-to-submitting":
				f.client.beforeDiscard = func() {
					f.client.active.Store(true)
					f.steerCode.Store(0)
					if scenario == "changed-to-submitting" {
						f.steerCode.Store(-32000)
					}
					_, err := f.client.Client.Client.SendWithOptions(ctx, "lead", d.Envelope, d.MessageID, d.Context, codex.TurnOptions{Model: "saved-model", ReasoningEffort: "xhigh"})
					if scenario == "changed-to-accepted" && err != nil {
						t.Fatal(err)
					}
					var unknown *codex.UnknownSendOutcomeError
					if scenario == "changed-to-submitting" && !errors.As(err, &unknown) {
						t.Fatalf("intervening submission was not uncertain: %v", err)
					}
				}
			case "joined-unknown":
				f.client.sendOverride = errors.Join(&codex.MessageNotSentError{Err: errors.New("original unsent")}, &codex.UnknownSendOutcomeError{Err: errors.New("later uncertain storage")})
			}
			if err := f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID); err != nil {
				t.Fatal(err)
			}
			saved, err := f.service.Store.Get(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "submitting"
			if scenario == "readiness-hold" {
				want = "held"
			}
			if saved.Events[0].Delivery.Status != want || f.starts.Load() != 0 {
				t.Fatalf("uncertain/held repair became a resend: %s starts=%d", saved.Events[0].Delivery.Status, f.starts.Load())
			}
			wantDiscards := 0
			if scenario == "discard-error" || scenario == "changed-to-accepted" || scenario == "changed-to-submitting" {
				wantDiscards = 1
			}
			if f.client.discards != wantDiscards || scenario == "joined-unknown" && f.client.proofs != 0 {
				t.Fatal("repair lacked its positive exact-prepared authority")
			}
			if scenario == "readiness-hold" {
				options := *saved.Events[0].Delivery.Options
				if prepared, err := f.client.Client.IsPreparedSendWithOptions("lead", d.Envelope, d.MessageID, d.Context, options); err != nil || !prepared {
					t.Fatal("readiness hold removed the original preparation")
				}
			}
			if scenario == "changed-to-accepted" || scenario == "changed-to-submitting" {
				f.client.beforeDiscard = nil
				if err := f.service.Dispatch(ctx, f.op.ID, f.op.Events[0].ID); err != nil {
					t.Fatal(err)
				}
				saved, err = f.service.Store.Get(ctx, f.op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "changed-to-accepted" && (saved.Events[0].Delivery.Status != "accepted" || saved.Events[0].Delivery.Receipt.TurnID != "busy-turn") {
					t.Fatal("intervening accepted receipt was reset")
				}
				if scenario == "changed-to-submitting" && saved.Events[0].Delivery.Status != "submitting" {
					t.Fatal("intervening unknown input was reset")
				}
				if f.client.proofs != 1 || f.client.discards != 1 || f.steers.Load() != 2 || f.starts.Load() != 0 {
					t.Fatal("intervening submission was replayed")
				}
			}
		})
	}
}
