package workspacecodex

import (
	"context"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

func TestProgressSubmissionCannotEnterOrdinaryRetry(t *testing.T) {
	for _, code := range []string{"serverOverloaded", "cyberPolicy"} {
		t.Run(code, func(t *testing.T) {
			transport := &retryRetentionWire{}
			wire := newRetrySettingsWireWithServer(t, true, "default", transport.serve)
			wire.mu.Lock()
			wire.status, wire.loaded = "completed", "idle"
			wire.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id := "00000000-0000-4000-8000-000000009001"
			receipt, err := wire.client.SendProgressCheck(ctx, wire.thread, "Scheduled progress check", id, "progress:example:check", codex.TurnOptions{Model: wire.model, ReasoningEffort: wire.effort, ThreadPolicy: wire.policy})
			if err != nil || receipt.TurnID == "" {
				t.Fatal(receipt, err)
			}
			saved, err := wire.client.RetryStore().Load(wire.scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, snapshot := range saved.Snapshots {
				if snapshot.ID == id {
					t.Fatal("scheduled check acquired retry admission")
				}
			}
			wire.mu.Lock()
			wire.status, wire.loaded, wire.failureCode = "failed", "idle", code
			wire.mu.Unlock()
			now := time.Now()
			controller := turnretry.Controller{Store: wire.client.RetryStore(), Backend: wire.client.RetryBackend(), Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Clock: func() time.Time { return now }}
			for i := 0; i < 3; i++ {
				_ = controller.Step(ctx, wire.scope)
				now = now.Add(5 * time.Minute)
			}
			if transport.count() != 1 {
				t.Fatal("ordinary retry started scheduled inference", transport.count())
			}
			// Explicit user assignments keep the established managed admission contract.
			manualID := "00000000-0000-4000-8000-000000009002"
			if _, err := wire.client.SendWithOptions(ctx, wire.thread, fmt.Sprint("Manual observer assignment ", code), manualID, "team:example:lead:observer0", codex.TurnOptions{}); err != nil {
				t.Fatal(err)
			}
			saved, err = wire.client.RetryStore().Load(wire.scope)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, snapshot := range saved.Snapshots {
				found = found || snapshot.ID == manualID
			}
			if !found {
				t.Fatal("manual assignment lost normal retry admission")
			}
		})
	}
}

func TestProgressPreservesPendingManualObserverRetry(t *testing.T) {
	transport := &retryRetentionWire{}
	wire := newRetrySettingsWireWithServer(t, true, "default", transport.serve)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Admit a real manual assignment and fail it into ordinary capacity backoff.
	if _, err := wire.client.SendWithOptions(ctx, wire.thread, "Manual observer analysis", "00000000-0000-4000-8000-000000009101", "team:example:lead:observer0", codex.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	wire.status, wire.loaded, wire.failureCode = "failed", "idle", "serverOverloaded"
	wire.mu.Unlock()
	now := time.Now()
	controller := turnretry.Controller{Store: wire.client.RetryStore(), Backend: wire.client.RetryBackend(), Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Clock: func() time.Time { return now }, Random: func() float64 { return .5 }}
	if err := controller.Step(ctx, wire.scope); err != nil {
		t.Fatal(err)
	}
	before, err := wire.client.RetryStore().Load(wire.scope)
	if err != nil || before.Pending == nil || !before.Owned() {
		t.Fatal("manual retry was not scheduled", before, err)
	}
	count := transport.count()
	if err := wire.client.RefreshProgressPolicy(ctx, wire.thread, wire.cwd, codex.ThreadSettings{Policy: wire.policy}); !errors.Is(err, ErrProgressCheckDeferred) {
		t.Fatal("refresh displaced manual retry", err)
	}
	if _, err := wire.client.SendProgressCheck(ctx, wire.thread, "Automatic progress check", "00000000-0000-4000-8000-000000009102", "progress:example:check", codex.TurnOptions{Model: wire.model, ReasoningEffort: wire.effort, ThreadPolicy: wire.policy}); !errors.Is(err, ErrProgressCheckDeferred) {
		t.Fatal("progress displaced manual retry")
	}
	after, err := wire.client.RetryStore().Load(wire.scope)
	if err != nil {
		t.Fatal(err)
	}
	if transport.count() != count || after.Pending == nil || after.Pending.ID != before.Pending.ID || after.DeadlineMS != before.DeadlineMS || after.Phase != before.Phase {
		t.Fatal("progress changed manual retry", before, after)
	}
}

// Model native warm-policy retention, so a plain resume cannot satisfy this
// regression. The live rollout check separately exercises the real native tools.
func TestProgressRefreshUpdatesIdlePolicyAndPreservesManualWork(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprint("queued=", queued), func(t *testing.T) {
			effective := ""
			injections := 0
			w := newRetrySettingsWireWithServer(t, true, "default", func(w *retrySettingsWire, conn *websocket.Conn) error {
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
					var result, notify any
					failure := ""
					params, _ := req["params"].(map[string]any)
					switch req["method"] {
					case "thread/resume":
						if !w.bound || w.loaded == "notLoaded" {
							effective, _ = params["developerInstructions"].(string)
						}
						w.bound = true
						w.loaded = "idle"
						result = map[string]any{"thread": map[string]any{"id": w.thread}}
					case "thread/inject_items":
						injections++
						item := params["items"].([]any)[0].(map[string]any)
						if item["role"] != "developer" || params["threadId"] != w.thread {
							t.Error("wrong policy item identity", params)
						}
						effective = item["content"].([]any)[0].(map[string]any)["text"].(string)
						result = map[string]any{}
					default:
						result, failure, notify = w.reply(req)
					}
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
			})
			w.mu.Lock()
			original := effective
			w.status, w.loaded = "completed", "idle"
			if queued {
				w.queue = []codex.QueueEntry{{ID: "manual", ClientUserMessageID: "00000000-0000-4000-8000-000000001234", Text: "Manual inspection"}}
			}
			w.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			next := codex.ThreadSettings{Model: w.model, ReasoningEffort: w.effort, Policy: codex.ThreadPolicy{DeveloperInstructions: "Use tools for targeted inspection.", Sandbox: "danger-full-access", ApprovalPolicy: "never"}}
			if err := w.client.Client.ActivateThreadWithSettings(ctx, w.thread, next); err != nil {
				t.Fatal(err)
			}
			w.mu.Lock()
			retained := effective
			w.mu.Unlock()
			if retained != original {
				t.Fatal("fixture did not reproduce warm retention")
			}
			err := w.client.RefreshProgressPolicy(ctx, w.thread, w.cwd, next)
			w.mu.Lock()
			defer w.mu.Unlock()
			if queued {
				if err == nil || injections != 0 || effective != original || len(w.queue) != 1 {
					t.Fatal("refresh displaced manual work", err, injections)
				}
			} else if err != nil || injections != 1 || effective != next.Policy.DeveloperInstructions || w.thread != "member" || w.turn != "manual" {
				t.Fatal("refresh did not apply the policy on the same thread", err, injections, effective)
			}
		})
	}
}
