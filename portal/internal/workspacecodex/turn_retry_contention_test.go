package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/coder/websocket"
)

// Independent clients share only the production durable admission/ledger paths,
// not a websocket or codex-web's per-client queueUpdateLock. RPC barriers wait
// outside the fake native-state mutex so that mutex cannot supply exclusion.
type retryContentionWire struct {
	armed, hideHistory atomic.Bool
	owner              string
	entered, release   chan struct{}
	once               sync.Once
	mu                 sync.Mutex
	automatic, steers  int
	manualMutations    int
}

func (b *retryContentionWire) counts() (int, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.automatic, b.steers, b.manualMutations
}

func (b *retryContentionWire) serve(w *retrySettingsWire, conn *websocket.Conn) error {
	defer conn.CloseNow()
	if err := handshake(conn); err != nil {
		return err
	}
	for {
		req, err := readObject(conn)
		if err != nil {
			return nil
		}
		p, _ := req["params"].(map[string]any)
		method, _ := req["method"].(string)
		text := ""
		if input, ok := p["input"].([]any); ok && len(input) != 0 {
			text, _ = input[0].(map[string]any)["text"].(string)
		}
		auto := text == turnretry.Continuation
		armed := b.armed.Load()
		if armed && (strings.HasPrefix(method, "turn/") || method == "thread/resume" || method == "thread/settings/update" || method == "thread/queue/add") {
			if !auto && b.owner != "manual" {
				saved, err := w.client.RetryStore().Load(w.scope)
				accepted := false
				for _, snapshot := range saved.Snapshots {
					accepted = accepted || (snapshot.Origin == "automatic" && snapshot.Turn == "automatic" && snapshot.TextDigest == textHash(turnretry.Continuation))
				}
				if err != nil || !accepted || saved.Uncertain() {
					w.t.Error("manual mutation preceded durable automatic receipt", saved, err)
				}
			}
			b.mu.Lock()
			if auto {
				b.automatic++
				if method == "turn/steer" {
					b.steers++
				}
			} else {
				b.manualMutations++
			}
			b.mu.Unlock()
		}
		w.mu.Lock()
		var result any
		failure := ""
		var notify any
		switch {
		case armed && method == "thread/items/list" && b.hideHistory.Load():
			result = map[string]any{"data": []any{}, "nextCursor": nil}
		case armed && method == "thread/resume":
			// This is the ordinary explicit manual path, which may apply its
			// authorized policy. Automatic continuation must never take it.
			config, _ := p["config"].(map[string]any)
			servers, _ := config["mcp_servers"].(map[string]any)
			server, _ := servers["team"].(map[string]any)
			if server["command"] != w.policy.MCPServer.Command || p["developerInstructions"] != "common lifecycle\n\n"+w.policy.DeveloperInstructions {
				w.t.Error("manual resume lost authorized role/MCP", p)
			}
			result = map[string]any{"thread": map[string]any{"id": w.thread}}
		case armed && (method == "turn/start" || method == "turn/steer") && !auto:
			if method == "turn/start" {
				w.turn = "manual-independent"
				w.items = nil
			} else if p["expectedTurnId"] != w.turn {
				w.t.Error("manual steer lost exact active turn", p)
			}
			w.status, w.loaded = "inProgress", "active"
			w.items = append(w.items, retrySettingsInput(w.turn, p["clientUserMessageId"].(string), text)...)
			result = map[string]any{"turn": map[string]any{"id": w.turn}}
			if method == "turn/steer" {
				result = map[string]any{"turnId": w.turn}
			}
		default:
			result, failure, notify = w.reply(req)
		}
		w.mu.Unlock()
		// Native has processed input, but its receipt has not reached the
		// caller. The production admission lock must remain held here.
		if armed && ((b.owner == "manual" && !auto && (method == "turn/start" || method == "thread/queue/add")) || (b.owner != "manual" && auto)) {
			b.once.Do(func() { close(b.entered) })
			<-b.release
			if b.owner == "unknown" {
				b.hideHistory.Store(true)
				result = map[string]any{"turn": map[string]any{}}
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

func TestRetryManagedInputContendsWithController(t *testing.T) {
	for _, member := range []bool{false, true} {
		for _, writer := range []string{"send", "queue"} {
			for _, owner := range []string{"manual", "automatic", "unknown"} {
				t.Run(fmt.Sprintf("member=%t/%s/%s", member, writer, owner), func(t *testing.T) {
					barrier := &retryContentionWire{owner: owner, entered: make(chan struct{}), release: make(chan struct{})}
					var release sync.Once
					defer release.Do(func() { close(barrier.release) })
					w := newRetrySettingsWireWithServer(t, member, "default", barrier.serve)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := w.client.QueueWithPolicy(ctx, w.thread, "managed source", "source-id", w.policy); err != nil {
						t.Fatal(err)
					}
					w.mu.Lock()
					w.turn, w.status, w.loaded = "failed-source", "failed", "systemError"
					w.queue = nil
					w.items = retrySettingsInput(w.turn, "source-id", "managed source")
					w.writeRollout(w.turn)
					w.mu.Unlock()
					now := time.Now()
					controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Clock: func() time.Time { return now }}
					if err := controller.Step(ctx, w.scope); err != nil {
						t.Fatal(err)
					}
					scheduled, err := controller.Store.Load(w.scope)
					if err != nil || scheduled.Pending == nil || scheduled.Phase != "scheduled" {
						t.Fatal("real managed failure did not schedule", scheduled, err)
					}
					now = time.UnixMilli(scheduled.DeadlineMS)
					manual := NewWithOptions(w.scope.Socket, w.scope.Workspace, codex.ClientOptions{DeveloperInstructions: "common lifecycle", PreserveThreadInstructionsOnResume: true, SubmissionLedgerPath: w.client.submissionLedgerPath})
					defer manual.Close()
					manual.RecoveryRoot = w.client.RecoveryRoot
					attempted := make(chan struct{}, 1)
					manual.RetryPolicyResolver = func(ctx context.Context, thread string) (codex.ThreadPolicy, error) {
						select {
						case attempted <- struct{}{}:
						default:
						}
						return w.client.RetryPolicyResolver(ctx, thread)
					}
					if manual.Client == w.client.Client || manual.RetryStore() != controller.Store {
						t.Fatal("contention must use independent clients and one authoritative store")
					}
					input := func(ctx context.Context) error {
						if writer == "queue" {
							entry, err := manual.QueueWithPolicy(ctx, w.thread, "independent manual input", "manual-id", w.policy)
							if err == nil && entry.ClientUserMessageID != "manual-id" {
								return errors.New("manual queue lost client identity")
							}
							return err
						}
						receipt, err := manual.SendWithOptions(ctx, w.thread, "independent manual input", "manual-id", "manual-action", codex.TurnOptions{Model: w.model, ReasoningEffort: w.effort, ThreadPolicy: w.policy})
						if err == nil && (receipt.TurnID == "" || receipt.Steered != (owner != "manual")) {
							return errors.New("manual send changed ordinary idle/steer semantics")
						}
						return err
					}
					barrier.armed.Store(true)
					first := make(chan error, 1)
					if owner == "manual" {
						go func() { first <- input(ctx) }()
					} else {
						go func() { first <- controller.Step(ctx, w.scope) }()
					}
					select {
					case <-barrier.entered:
					case err := <-first:
						t.Fatal("owner never reached native receipt barrier", err)
					case <-ctx.Done():
						t.Fatal("owner never reached native receipt barrier", ctx.Err())
					}
					_, _, beforeManual := barrier.counts()
					if owner != "manual" && beforeManual != 0 {
						t.Fatal("automatic owner changed native settings before dispatch", beforeManual)
					}
					contend, stop := context.WithTimeout(ctx, 60*time.Millisecond)
					if owner == "manual" {
						err = controller.Step(contend, w.scope)
					} else {
						err = input(contend)
					}
					stop()
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("competing production path bypassed the admission fence", err)
					}
					auto, steers, mutations := barrier.counts()
					if steers != 0 || mutations != beforeManual || (owner == "manual" && auto != 0) || (owner != "manual" && auto != 1) {
						t.Fatalf("input escaped receipt critical section: automatic=%d steers=%d manual=%d/%d", auto, steers, mutations, beforeManual)
					}
					var waiting chan error
					if owner != "manual" {
						<-attempted // The deadline-limited contention probe above.
						waiting = make(chan error, 1)
						go func() { waiting <- input(ctx) }()
						select {
						case <-attempted:
						case <-ctx.Done():
							t.Fatal("managed waiter did not enter production admission")
						}
					}
					release.Do(func() { close(barrier.release) })
					err = <-first
					if owner == "unknown" {
						var unknown *codex.UnknownSendOutcomeError
						if !errors.As(err, &unknown) {
							t.Fatal("lost actual synthetic receipt was not uncertain", err)
						}
						saved, loadErr := controller.Store.Load(w.scope)
						if loadErr != nil || !saved.Uncertain() || saved.Pending.ID != scheduled.Pending.ID {
							t.Fatal("uncertainty lost exact intent", saved, loadErr)
						}
						assertRetrySubmitting(t, w.client.submissionLedgerPath, w.thread, *saved.Pending)
						if err := <-waiting; err == nil || !strings.Contains(err.Error(), "requires reconciliation") {
							t.Fatal("unknown automatic outcome did not hold managed input", err)
						}
						auto, steers, mutations = barrier.counts()
						if auto != 1 || steers != 0 || mutations != beforeManual {
							t.Fatal("uncertain input reached native", auto, steers, mutations)
						}
						after, _ := controller.Store.Load(w.scope)
						if !after.Uncertain() || !reflect.DeepEqual(after.Pending, saved.Pending) {
							t.Fatal("manual hold rewrote uncertain identity")
						}
						barrier.hideHistory.Store(false)
						if err := controller.ReconcileOnly(ctx, w.scope); err != nil {
							t.Fatal("exact receipt reconciliation", err)
						}
					} else if err != nil {
						t.Fatal("fence owner failed", err)
					}
					if owner == "manual" {
						if err := controller.Step(ctx, w.scope); err != nil {
							t.Fatal(err)
						}
						saved, _ := controller.Store.Load(w.scope)
						if saved.Pending != nil || !saved.Cancelled {
							t.Fatal("manual admission did not cancel/supersede pending retry", saved)
						}
					} else if owner == "unknown" {
						saved, _ := controller.Store.Load(w.scope)
						if saved.Pending == nil || saved.Pending.ResultTurn != "automatic" {
							t.Fatal("receipt handling did not persist before unlocking", saved)
						}
						if err := input(ctx); err != nil {
							t.Fatal("ordinary managed input failed after exact receipt", err)
						}
					} else if err := <-waiting; err != nil {
						t.Fatal("managed waiter failed after dispatch/receipt critical section", err)
					}
					auto, steers, _ = barrier.counts()
					want := 1
					if owner == "manual" {
						want = 0
					}
					if auto != want || steers != 0 {
						t.Fatal("automatic input dispatched or steered twice", auto, steers)
					}
				})
			}
		}
	}
}

// Inspect the real generic writer's ledger, without manufacturing acceptance.
func assertRetrySubmitting(t *testing.T, path, thread string, pending turnretry.Pending) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		Schema int `json:"schema"`
		Sends  map[string]map[string]struct {
			State, Context, Digest string
			Turn                   string `json:"turnId"`
		} `json:"sends"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	attempt := ledger.Sends[thread][pending.ID]
	if ledger.Schema != 3 || attempt.State != "submitting" || attempt.Context != pending.Action || attempt.Digest != textHash(turnretry.Continuation) || attempt.Turn != "" {
		t.Fatal("missing exact native submitting attempt", ledger.Schema, attempt)
	}
}
