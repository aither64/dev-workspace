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
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/coder/websocket"
)

type retryRetentionWire struct {
	mu           sync.Mutex
	sends        int
	lostID       string
	hideHistory  bool
	failSaveID   string
	stopBeforeID string
}

func (r *retryRetentionWire) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends
}

func (r *retryRetentionWire) serve(w *retrySettingsWire, conn *websocket.Conn) error {
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
		r.mu.Lock()
		method, _ := req["method"].(string)
		p, _ := req["params"].(map[string]any)
		var result, notify any
		failure := ""
		switch {
		case method == "thread/resume" && w.bound && p["developerInstructions"] != nil:
			config, _ := p["config"].(map[string]any)
			servers, _ := config["mcp_servers"].(map[string]any)
			server, _ := servers["team"].(map[string]any)
			if p["developerInstructions"] != "common lifecycle\n\n"+w.policy.DeveloperInstructions || p["sandbox"] != w.policy.Sandbox || p["approvalPolicy"] != w.policy.ApprovalPolicy || server["command"] != w.policy.MCPServer.Command {
				w.t.Error("explicit prepared send changed authorized instructions/MCP/access", p)
			}
			w.resumes++
			result = map[string]any{"thread": map[string]any{"id": w.thread}}
		case method == "thread/items/list" && (r.hideHistory || w.items == nil):
			result = map[string]any{"data": []any{}, "nextCursor": nil}
		case method == "thread/read" && r.stopBeforeID != "":
			saved, err := w.client.RetryStore().Load(w.scope)
			admitted := false
			for _, snapshot := range saved.Snapshots {
				admitted = admitted || snapshot.ID == r.stopBeforeID
			}
			if err == nil && admitted {
				failure = "synthetic interruption after durable admission"
			} else {
				result, failure, notify = w.reply(req)
			}
		case method == "turn/start" || method == "turn/steer":
			r.sends++
			id, _ := p["clientUserMessageId"].(string)
			text := p["input"].([]any)[0].(map[string]any)["text"].(string)
			if method == "turn/start" {
				w.turn, w.items = fmt.Sprint("independent-", r.sends), nil
			} else if p["expectedTurnId"] != w.turn {
				w.t.Error("manual steer lost exact turn", p)
			}
			w.status, w.loaded = "inProgress", "active"
			input := retrySettingsInput(w.turn, id, text)
			input[0].(map[string]any)["item"].(map[string]any)["id"] = "native-" + id
			w.items = append(w.items, input...)
			w.writeRollout(w.turn)
			result = map[string]any{"turn": map[string]any{"id": w.turn}}
			if method == "turn/steer" {
				result = map[string]any{"turnId": w.turn}
			}
			if id == r.lostID {
				r.hideHistory = true
				result = map[string]any{}
			}
			if id == r.failSaveID {
				if err := os.Chmod(w.recordDirectory, 0500); err != nil {
					w.t.Error(err)
				}
			}
		default:
			result, failure, notify = w.reply(req)
		}
		r.mu.Unlock()
		w.mu.Unlock()
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

func retryRetentionSend(t *testing.T, w *retrySettingsWire, n int) codex.SendReceipt {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
	receipt, err := w.client.SendWithOptions(context.Background(), w.thread, fmt.Sprint("input ", n), id, "", codex.TurnOptions{})
	if err != nil || receipt.TurnID == "" || !receipt.Steered {
		t.Fatal("managed steer did not reach native", n, receipt, err)
	}
	return receipt
}

func retryRetentionRestart(t *testing.T, w *retrySettingsWire) {
	t.Helper()
	old := w.client
	old.Close()
	w.mu.Lock()
	w.client = NewWithOptions(w.scope.Socket, w.scope.Workspace, codex.ClientOptions{DeveloperInstructions: "common lifecycle", PreserveThreadInstructionsOnResume: true, SubmissionLedgerPath: old.submissionLedgerPath})
	w.client.RecoveryRoot, w.client.RetryPolicyResolver = old.RecoveryRoot, old.RetryPolicyResolver
	w.mu.Unlock()
	t.Cleanup(func() { w.client.Close() })
}

func TestRetryRetentionManagedSteersBeyond64AndExactReplay(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(fmt.Sprint("member=", member), func(t *testing.T) {
			r := &retryRetentionWire{}
			w := newRetrySettingsWireWithServer(t, member, "default", r.serve)
			before, _ := w.client.RetryStore().Load(w.scope)
			for i := 0; i < 64; i++ {
				retryRetentionSend(t, w, i)
			}
			full, err := w.client.RetryStore().Load(w.scope)
			if err != nil || len(full.Snapshots) != 64 {
				t.Fatal("did not reproduce deployed full steer shape", len(full.Snapshots), err)
			}
			for i := 64; i < 69; i++ {
				retryRetentionSend(t, w, i)
			}
			after, err := w.client.RetryStore().Load(w.scope)
			if err != nil || len(after.Snapshots) > 64 || after.Handled != "manual" || after.Proof("manual") != nil || after.Epoch != before.Epoch || !reflect.DeepEqual(after.Binding, before.Binding) || !reflect.DeepEqual(after.Cyber, before.Cyber) || after.Enabled != before.Enabled || after.Baseline != before.Baseline {
				t.Fatal("bounded admission lost suppression or authority", after, err)
			}
			// Replaying an evicted accepted UUID must not capture current settings.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			retryRetentionRestart(t, w)
			id := "00000000-0000-4000-8000-000000000000"
			for _, changed := range []string{"text", "action", "options"} {
				text, action, options := "input 0", "", (codex.TurnOptions{})
				switch changed {
				case "text":
					text = "changed"
				case "action":
					action = "changed"
				case "options":
					options.Model = "changed"
				}
				if _, err := w.client.SendWithOptions(ctx, w.thread, text, id, action, options); err == nil || !strings.Contains(err.Error(), "message identity was reused") {
					t.Fatal("changed identity was not rejected by the exact tuple check", changed, err)
				}
			}
			if receipt, err := w.client.Send(ctx, w.thread, "input 0", id, ""); err != nil || receipt.TurnID != "manual" {
				t.Fatal("evicted original receipt unavailable", receipt, err)
			}
			ack, err := w.client.AcknowledgeSends(ctx, w.thread, []codex.SendAcknowledgement{{ClientUserMessageID: id, Digest: textHash("input 0")}})
			if err != nil || len(ack) != 1 {
				t.Fatal("browser acknowledgment failed", ack, err)
			}
			retryRetentionRestart(t, w)
			// This is the browser retry:true preflight, followed by actual Send.
			if found, err := w.client.SendAttempted(ctx, w.thread, "input 0", id, ""); err != nil || !found {
				t.Fatal("acknowledged exact history was not recovered", found, err)
			}
			if _, err := w.client.Send(ctx, w.thread, "input 0", id, ""); err != nil {
				t.Fatal(err)
			}
			final, _ := w.client.RetryStore().Load(w.scope)
			if !reflect.DeepEqual(final, after) {
				t.Fatal("accepted replay recaptured or rebound retired provenance")
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.sends != 69 {
				t.Fatal("exact replay duplicated native input", r.sends)
			}
			t.Logf("64 accepted steers -> 69 distinct accepted steers; ledger/restart/ack/history replays and changed text/action/options: native sends=%d, snapshots=%d", r.sends, len(final.Snapshots))
		})
	}
}

func TestRetryRetentionProtectedQueueFIFOAndPreparedIdentity(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(fmt.Sprint("member=", member), func(t *testing.T) {
			r := &retryRetentionWire{}
			w := newRetrySettingsWireWithServer(t, member, "default", r.serve)
			ctx := context.Background()
			for i := 0; i < 64; i++ {
				if _, err := w.client.QueueWithPolicy(ctx, w.thread, fmt.Sprint(i), fmt.Sprint("queue-", i), w.policy); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := w.client.RetryStore().Load(w.scope)
			for _, queue := range []bool{false, true} {
				var err error
				if queue {
					_, err = w.client.QueueWithPolicy(ctx, w.thread, "fresh", "fresh", w.policy)
				} else {
					_, err = w.client.Send(ctx, w.thread, "fresh", "fresh", "")
				}
				if !errors.Is(err, turnretry.ErrSnapshotCapacity) {
					t.Fatal("protected queue evidence was dropped to send", err)
				}
			}
			// Recovery of an existing queue bypasses new capture, never queue add.
			if entry, err := w.client.QueueWithPolicy(ctx, w.thread, "0", "queue-0", w.policy); err != nil || entry.ID != "queue-0" {
				t.Fatal("existing queue identity unavailable at cap", entry, err)
			}
			after, _ := w.client.RetryStore().Load(w.scope)
			if !reflect.DeepEqual(before.Snapshots, after.Snapshots) {
				t.Fatal("protected admissions changed")
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			if len(w.queue) != 64 {
				t.Fatal("queue count changed", len(w.queue))
			}
			for i, entry := range w.queue {
				if entry.ClientUserMessageID != fmt.Sprint("queue-", i) {
					t.Fatal("queue order changed", w.queue)
				}
			}
		})
	}
	t.Run("prepared first execution without admission", func(t *testing.T) {
		r := &retryRetentionWire{}
		w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
		if err := w.client.PrepareSendWithOptions(w.thread, "prepared", "prepared", "action", true, codex.TurnOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.client.Send(context.Background(), w.thread, "prepared", "prepared", "action"); err != nil {
			t.Fatal("positive exact prepared intent could not acquire first execution admission", err)
		}
		if r.count() != 1 {
			t.Fatal("prepared identity did not execute exactly once")
		}
	})
}

func TestRetryRetentionCrashAndLostReceiptRecovery(t *testing.T) {
	for _, failure := range []string{"before-save", "after-admission", "submitting", "accepted-save"} {
		t.Run(failure, func(t *testing.T) {
			r := &retryRetentionWire{}
			w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
			for i := 0; i < 64; i++ {
				retryRetentionSend(t, w, i)
			}
			ctx := context.Background()
			id, text := "fresh-uncertain", "new original input"
			r.mu.Lock()
			switch failure {
			case "before-save":
				if err := os.Chmod(w.recordDirectory, 0500); err != nil {
					t.Fatal(err)
				}
			case "after-admission":
				r.stopBeforeID = id
			case "submitting":
				r.lostID = id
			case "accepted-save":
				r.failSaveID = id
			}
			r.mu.Unlock()
			_, err := w.client.Send(ctx, w.thread, text, id, "")
			if err == nil {
				t.Fatal("injected uncertain/refused outcome was hidden")
			}
			if failure == "submitting" || failure == "accepted-save" {
				var unknown *codex.UnknownSendOutcomeError
				if !errors.As(err, &unknown) {
					t.Fatal("postnative failure was not uncertain", err)
				}
			}
			if err := os.Chmod(w.recordDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			firstSends := r.sends
			r.stopBeforeID, r.failSaveID = "", ""
			r.mu.Unlock()
			if failure == "before-save" || failure == "after-admission" {
				if firstSends != 64 {
					t.Fatal("native input preceded durable admission", firstSends)
				}
			} else if firstSends != 65 {
				t.Fatal("native accepted fixture did not submit exactly once", firstSends)
			}
			retryRetentionRestart(t, w)
			if failure == "submitting" {
				if _, err := w.client.Send(ctx, w.thread, text, id, ""); err == nil {
					t.Fatal("unknown result replayed before history")
				}
				r.mu.Lock()
				r.hideHistory = false
				r.mu.Unlock()
			}
			if receipt, err := w.client.Send(ctx, w.thread, text, id, ""); err != nil || receipt.TurnID != "manual" {
				t.Fatal("original identity did not recover", receipt, err)
			}
			state, err := w.client.RetryStore().Load(w.scope)
			data, _ := json.Marshal(state)
			if err != nil || len(state.Snapshots) > 64 || len(data)+1 > turnretry.MaxBytes || state.Handled != "manual" {
				t.Fatal("recovered state invalid or missing suppression", state, err)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.sends != 65 {
				t.Fatal("crash recovery duplicated original input", r.sends)
			}
		})
	}
}

func TestRetryRetentionCachedProofSuppressedAndNewQueueEligible(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(fmt.Sprint("member=", member), func(t *testing.T) {
			r := &retryRetentionWire{}
			w := newRetrySettingsWireWithServer(t, member, "default", r.serve)
			ctx := context.Background()
			for i := 0; i < 63; i++ {
				retryRetentionSend(t, w, i)
			}
			tx, _ := w.client.RetryStore().Lock(ctx, w.scope)
			if err := w.client.RetryBackend().Prove(ctx, w.scope, &tx.State, "manual"); err != nil {
				t.Fatal(err)
			}
			if tx.State.Proof("manual") == nil || len(tx.State.Snapshots) != 64 {
				t.Fatal("fixture did not create a complete cached aggregate at cap")
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			retryRetentionSend(t, w, 64)
			tx, _ = w.client.RetryStore().Lock(ctx, w.scope)
			if tx.State.Proof("manual") != nil || tx.State.Handled != "manual" {
				t.Fatal("retirement kept a cached aggregate")
			}
			tx.State.SetEnabled(false, turnretry.Observation{Turn: "manual", Status: "inProgress"})
			tx.State.SetEnabled(true, turnretry.Observation{Turn: "manual", Status: "inProgress"})
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			model := "new-model"
			if _, err := w.client.UpdateThreadSettings(ctx, w.thread, codex.ThreadSettingsUpdate{Model: &model}); err != nil {
				t.Fatal(err)
			}
			retryRetentionRestart(t, w)
			if _, err := w.client.QueueWithPolicy(ctx, w.thread, "independent queued work", "new-queue", w.policy); err != nil {
				t.Fatal(err)
			}
			w.mu.Lock()
			w.status, w.loaded = "failed", "systemError"
			// Deliberately partial history cannot resurrect the cached aggregate.
			w.items = w.items[len(w.items)-1:]
			w.writeRollout("manual")
			w.mu.Unlock()
			controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend()}
			if err := controller.Step(ctx, w.scope); err != nil {
				t.Fatal(err)
			}
			s, _ := controller.Store.Load(w.scope)
			if s.Pending != nil || s.Proof("manual") != nil || s.Handled != "manual" {
				t.Fatal("restart/settings/optout restored truncated-turn eligibility")
			}
			w.mu.Lock()
			w.turn, w.status, w.loaded, w.queue = "new-queue-turn", "failed", "systemError", nil
			w.items = retrySettingsInput(w.turn, "new-queue", "independent queued work")
			w.writeRollout(w.turn)
			w.mu.Unlock()
			if err := controller.Step(ctx, w.scope); err != nil {
				t.Fatal("new independent task lost complete-proof eligibility", err)
			}
			s, _ = controller.Store.Load(w.scope)
			if s.Pending == nil || s.Pending.Failure != "new-queue-turn" || s.Phase != "scheduled" || s.Pending.Snapshot.Options.Model != model {
				t.Fatal("new epoch/queue did not schedule from exact context", s)
			}
		})
	}
}

func TestRetryRetentionAggregateAndAutomaticAtCap(t *testing.T) {
	for _, count := range []int{63, 64} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := &retryRetentionWire{}
			w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
			for i := 0; i < count; i++ {
				retryRetentionSend(t, w, i)
			}
			w.mu.Lock()
			w.status, w.loaded = "failed", "systemError"
			w.mu.Unlock()
			now := time.Now()
			controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Clock: func() time.Time { return now }}
			err := controller.Step(context.Background(), w.scope)
			if count == 64 && !errors.Is(err, turnretry.ErrMissingProof) || count == 63 && err != nil {
				t.Fatal("aggregate budget returned unexpected outcome", err)
			}
			s, err := controller.Store.Load(w.scope)
			if err != nil || len(s.Snapshots) > 64 {
				t.Fatal("aggregate saved an invalid candidate", err)
			}
			if count == 63 {
				now = time.UnixMilli(s.DeadlineMS)
				if err := controller.Step(context.Background(), w.scope); !errors.Is(err, turnretry.ErrSnapshotCapacity) {
					t.Fatal("automatic receipt slot was not preflighted", err)
				}
			}
			s, err = controller.Store.Load(w.scope)
			if err != nil || s.Phase != "blocked" || s.Handled != "manual" || r.count() != count {
				t.Fatal("cap pressure dispatched or lost manual-only failure", s, err, r.count())
			}
		})
	}
}

func TestRetryRetentionUnknownAutomaticAtCapHoldsManual(t *testing.T) {
	r := &retryRetentionWire{}
	w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
	retryRetentionSend(t, w, 1)
	w.mu.Lock()
	w.status, w.loaded = "failed", "systemError"
	w.mu.Unlock()
	now := time.Now()
	controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Clock: func() time.Time { return now }}
	ctx := context.Background()
	if err := controller.Step(ctx, w.scope); err != nil {
		t.Fatal(err)
	}
	s, _ := controller.Store.Load(w.scope)
	id := s.Pending.ID
	r.mu.Lock()
	r.lostID = id
	r.mu.Unlock()
	now = time.UnixMilli(s.DeadlineMS)
	if err := controller.Step(ctx, w.scope); err == nil {
		t.Fatal("fixture did not preserve genuine submitting uncertainty")
	}
	tx, _ := controller.Store.Lock(ctx, w.scope)
	original := *tx.State.Pending
	for len(tx.State.Snapshots) < 64 {
		p := tx.State.Snapshots[0]
		p.ID, p.Turn = fmt.Sprint("protected-", len(tx.State.Snapshots)), ""
		p.Digest, _ = p.Hash()
		if err := tx.State.AddSnapshot(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	if _, err := w.client.Send(ctx, w.thread, "new work", "new-work", ""); err == nil {
		t.Fatal("unknown automatic input did not hold manual admission")
	}
	s, _ = controller.Store.Load(w.scope)
	if !s.Uncertain() || !reflect.DeepEqual(*s.Pending, original) || len(s.Snapshots) != 64 {
		t.Fatal("manual admission altered pending identity or queue evidence")
	}
	r.mu.Lock()
	r.hideHistory = false
	r.mu.Unlock()
	retryRetentionRestart(t, w)
	controller.Store, controller.Backend = w.client.RetryStore(), w.client.RetryBackend()
	if err := controller.ReconcileOnly(ctx, w.scope); err != nil {
		t.Fatal(err)
	}
	s, _ = controller.Store.Load(w.scope)
	if s.Phase != "blocked" || s.Pending.ID != id || s.Pending.ResultTurn == "" || len(s.Snapshots) != 64 || r.count() != 2 {
		t.Fatal("known accepted result did not settle without a 65th record", s, r.count())
	}
	result := s.Pending.ResultTurn
	for _, status := range []string{"inProgress", "failed", "failed"} {
		w.mu.Lock()
		w.status = status
		if status == "failed" {
			w.loaded = "systemError"
		}
		w.mu.Unlock()
		retryRetentionRestart(t, w)
		controller.Backend = w.client.RetryBackend()
		if err := controller.Step(ctx, w.scope); err != nil {
			t.Fatal(err)
		}
		s, err := controller.Store.Load(w.scope)
		if err != nil || s.Phase != "blocked" || s.Pending.ID != id || s.Pending.ResultTurn != result || !reflect.DeepEqual(s.Pending.Snapshot, original.Snapshot) || s.Handled != result || len(s.Snapshots) != 64 || r.count() != 2 {
			t.Fatal("accepted protected-cap result rearmed after restart", s, err)
		}
	}
	t.Logf("all 64 protected snapshots preserved; exact accepted automatic UUID settled/reloaded active+failed: native sends=%d (one manual source, one automatic, zero repeats)", r.count())
}
