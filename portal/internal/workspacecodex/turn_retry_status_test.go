package workspacecodex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

const wrappedCapacityMessage = "stream disconnected before completion: Codex is at capacity, please try again later"

// Use actual managed admission, native-style execution proof and the real
// backend/controller/continuation composition, not a status-predicate mock.
func TestRetryLoadedTerminalStatusController(t *testing.T) {
	for _, status := range []string{"idle", "systemError"} {
		for _, failure := range []struct{ name, code, message string }{
			{"typed capacity", "serverOverloaded", "synthetic terminal error"},
			{"cybersecurity", "cyberPolicy", "synthetic terminal error"},
			{"wrapped capacity", "other", wrappedCapacityMessage},
		} {
			for _, hold := range []string{"", "cold", "active", "unknown-status", "newer", "completed", "in-progress", "unknown-code", "binding", "provider", "queue", "prompt", "ledger", "disabled", "handled", "baseline"} {
				t.Run(fmt.Sprintf("%s/%s/%s", status, failure.name, hold), func(t *testing.T) {
					w := newRetrySettingsWire(t, false, "default")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := w.client.QueueWithPolicy(ctx, w.thread, "managed task", "managed-client", w.policy); err != nil {
						t.Fatal(err)
					}
					w.mu.Lock()
					w.turn, w.status, w.loaded, w.failureCode = "failed-source", "failed", status, failure.code
					w.failureMessage = failure.message
					w.queue = nil // Native execution consumed the admitted queue entry.
					w.items = retrySettingsInput(w.turn, "managed-client", "managed task")
					if failure.code == "other" {
						w.items = append(w.items,
							map[string]any{"turnId": w.turn, "item": map[string]any{"id": "partial-answer", "type": "agentMessage", "text": "Already completed part of the work"}},
							map[string]any{"turnId": w.turn, "item": map[string]any{"id": "completed-command", "type": "commandExecution", "command": "synthetic command", "status": "completed", "aggregatedOutput": "saved result"}})
					}
					sourceHistory, _ := json.Marshal(w.items)
					w.writeRollout(w.turn)
					resumes, settings, policies := w.resumes, w.settings, w.policyUpdates
					w.mu.Unlock()
					now := time.Now()
					controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Clock: func() time.Time { return now }}
					if failure.code == "other" {
						raw, err := w.client.Client.ReadTurnState(ctx, w.thread)
						if err != nil || raw.TerminalFailure == nil || raw.TerminalFailure.Code != "other" || raw.TerminalFailure.Message != wrappedCapacityMessage {
							t.Fatal("native failure was rewritten or lost", raw, err)
						}
						observed, err := controller.Backend.Observe(ctx, w.scope)
						if err != nil || observed.Code != "serverOverloaded" || observed.Turn != "failed-source" {
							t.Fatal("exact native capacity did not reach retry policy", observed, err)
						}
					}
					if hold == "disabled" {
						controller.Policy = turnretry.Decision{Revision: "disabled-policy", Capacity: false, Cybersecurity: false}
					}
					if hold == "handled" || hold == "baseline" {
						tx, err := controller.Store.Lock(ctx, w.scope)
						if err != nil {
							t.Fatal(err)
						}
						if hold == "handled" {
							tx.State.Handled = "failed-source"
						} else {
							tx.State.Baseline, tx.State.BaselineActive = "failed-source", false
						}
						if err := tx.Save(); err != nil {
							t.Fatal(err)
						}
						tx.Close()
					}
					if hold == "newer" {
						tx, err := controller.Store.Lock(ctx, w.scope)
						if err != nil {
							t.Fatal(err)
						}
						if err := controller.Backend.Prove(ctx, w.scope, &tx.State, "failed-source"); err != nil {
							t.Fatal(err)
						}
						proof := *tx.State.Proof("failed-source")
						if err := tx.Save(); err != nil {
							t.Fatal(err)
						}
						tx.Close()
						w.mu.Lock()
						w.turn = "new-independent-turn"
						w.mu.Unlock()
						if err := controller.Backend.Check(ctx, w.scope, proof); err == nil {
							t.Fatal("newer turn passed source identity check")
						}
					}
					// First pass schedules capacity; cyber may dispatch immediately.
					// Install holds before observing either class.
					w.mu.Lock()
					switch hold {
					case "cold":
						w.loaded = "notLoaded"
					case "active":
						w.loaded = "active"
					case "unknown-status":
						w.loaded = "futureStatus"
					case "completed":
						w.status = "completed"
					case "in-progress":
						w.status, w.loaded = "inProgress", "active"
					case "unknown-code":
						w.failureCode = "unknownFailure"
					case "provider":
						w.provider = "different-provider"
					case "queue":
						w.queue = []codex.QueueEntry{{ID: "competing", ClientUserMessageID: "competing-client", Text: "new user input"}}
					case "prompt":
						w.pendingQuestion = true
					}
					w.mu.Unlock()
					if hold == "binding" {
						tx, err := controller.Store.Lock(ctx, w.scope)
						if err != nil {
							t.Fatal(err)
						}
						tx.State.Binding = nil
						if err := tx.Save(); err != nil {
							t.Fatal(err)
						}
						tx.Close()
					}
					if hold == "ledger" {
						if err := w.client.Client.PrepareSendWithOptions(w.thread, "other uncertain input", "foreign-id", "other-action", false, codex.TurnOptions{}); err != nil {
							t.Fatal(err)
						}
					}
					firstErr := controller.Step(ctx, w.scope)
					saved, err := controller.Store.Load(w.scope)
					if err != nil {
						t.Fatal(err)
					}
					var dispatchErr error
					if saved.Phase == "scheduled" && saved.Pending != nil {
						if hold == "" && failure.code == "other" && (saved.Class != "capacity" || saved.DeadlineMS != now.Add(15*time.Second).UnixMilli()) {
							t.Fatal("wrapped capacity bypassed the existing backoff", saved)
						}
						// Check must agree for both native loaded statuses while the
						// exact historical proof and ordinary authority still match.
						if hold == "" && w.client.RetryBackend().Check(ctx, w.scope, saved.Pending.Snapshot) != nil {
							t.Fatal("loaded terminal source lost proven binding")
						}
						now = time.UnixMilli(saved.DeadlineMS)
						dispatchErr = controller.Step(ctx, w.scope)
					}
					w.mu.Lock()
					starts := w.starts
					if w.resumes != resumes || w.settings != settings || w.policyUpdates != policies {
						t.Error("automatic check/dispatch mutated loaded settings")
					}
					w.mu.Unlock()
					if hold == "" {
						if firstErr != nil || dispatchErr != nil || starts != 1 {
							t.Fatalf("loaded failed continuation: starts=%d observe=%v dispatch=%v", starts, firstErr, dispatchErr)
						}
						saved, err = controller.Store.Load(w.scope)
						if err != nil || saved.Pending == nil || saved.Pending.ResultTurn != "automatic" || saved.Phase != "running" {
							t.Fatal("native receipt not bound", saved, err)
						}
						if failure.code == "other" {
							w.mu.Lock()
							retained, _ := json.Marshal(w.items[1:])
							w.mu.Unlock()
							if string(retained) != string(sourceHistory) {
								t.Fatal("continuation changed prior partial output or tool results")
							}
							if err := controller.Step(ctx, w.scope); err != nil {
								t.Fatal(err)
							}
							w.mu.Lock()
							if w.starts != 1 {
								t.Error("duplicate poll repeated the original task")
							}
							w.mu.Unlock()
						}
					} else if starts != 0 {
						t.Fatalf("hold %s admitted automatic native input", hold)
					}
					if hold == "ledger" && w.client.RequireSubmissionAttemptsKnown(ctx, w.thread) == nil {
						t.Fatal("foreign ledger hold was erased")
					}
					if hold == "queue" {
						w.mu.Lock()
						if len(w.queue) != 1 {
							t.Error("native queue hold was changed")
						}
						w.mu.Unlock()
					}
					if hold == "prompt" && len(w.client.Prompts(w.thread)) == 0 {
						t.Fatal("pending request hold was not established")
					}
				})
			}
		}
	}
}

func TestRetryCapacityObservationRejectsNearMessages(t *testing.T) {
	for _, test := range []struct{ code, message string }{
		{"other", "synthetic terminal error"},
		{"other", "stream disconnected before completion: connection reset"},
		{"other", "stream disconnected before completion: " + wrappedCapacityMessage},
		{"other", wrappedCapacityMessage + "."},
		{"other", " " + wrappedCapacityMessage},
		{"other", strings.ToLower(wrappedCapacityMessage)},
		{"invalidRequest", wrappedCapacityMessage},
		{"unknownFailure", wrappedCapacityMessage},
		{"", wrappedCapacityMessage},
	} {
		t.Run(test.code+"/"+test.message, func(t *testing.T) {
			w := newRetrySettingsWire(t, false, "default")
			w.mu.Lock()
			w.turn, w.status, w.loaded = "failed-source", "failed", "systemError"
			w.failureCode, w.failureMessage = test.code, test.message
			w.mu.Unlock()
			o, err := w.client.RetryBackend().Observe(context.Background(), w.scope)
			if err != nil || o.Code != test.code || turnretry.Class(o.Code) != "" {
				t.Fatal("unsupported native error gained retry classification", o, err)
			}
		})
	}
}
