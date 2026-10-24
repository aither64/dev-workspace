package workspacecodex

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

// Use actual managed admission, native-style execution proof and the real
// backend/controller/continuation composition, not a status-predicate mock.
func TestRetryLoadedTerminalStatusController(t *testing.T) {
	for _, status := range []string{"idle", "systemError"} {
		for _, code := range []string{"serverOverloaded", "cyberPolicy"} {
			for _, hold := range []string{"", "cold", "active", "unknown-status", "newer", "completed", "in-progress", "unknown-code", "binding", "provider", "queue", "prompt", "ledger"} {
				t.Run(fmt.Sprintf("%s/%s/%s", status, code, hold), func(t *testing.T) {
					w := newRetrySettingsWire(t, false, "default")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := w.client.QueueWithPolicy(ctx, w.thread, "managed task", "managed-client", w.policy); err != nil {
						t.Fatal(err)
					}
					w.mu.Lock()
					w.turn, w.status, w.loaded, w.failureCode = "failed-source", "failed", status, code
					w.queue = nil // Native execution consumed the admitted queue entry.
					w.items = retrySettingsInput(w.turn, "managed-client", "managed task")
					w.writeRollout(w.turn)
					resumes, settings, policies := w.resumes, w.settings, w.policyUpdates
					w.mu.Unlock()
					now := time.Now()
					controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Clock: func() time.Time { return now }}
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
