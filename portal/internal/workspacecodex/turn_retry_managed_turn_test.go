package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

func TestRetryManagedTurnAcceptedVolumeRemainsEligible(t *testing.T) {
	r := &retryRetentionWire{}
	w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
	for i := 0; i < 205; i++ {
		retryRetentionSend(t, w, i)
	}
	saved, err := w.client.RetryStore().Load(w.scope)
	if err != nil || saved.Handled == "manual" || saved.ManagedTurn == nil || saved.ManagedTurn.Count != 205 || len(saved.Snapshots) != 0 {
		t.Fatal("accepted input volume consumed the active turn", saved.Handled, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.client.RetryBackend().Prove(ctx, w.scope, &saved, "manual"); err != nil {
		t.Fatal("accepted managed inputs lost execution proof", err)
	}
}

func advanceManagedTest(t *testing.T, w *retrySettingsWire) error {
	t.Helper()
	tx, err := w.client.RetryStore().Lock(context.Background(), w.scope)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	return (retryBackend{w.client}).AdvanceProof(context.Background(), w.scope, tx, w.turn)
}

func TestRetryManagedTurnLargeRepeatedContext(t *testing.T) {
	r := &retryRetentionWire{}
	w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
	w.policy.DeveloperInstructions = strings.Repeat("retained role ", 4000)
	if err := w.client.ActivateThread(context.Background(), w.thread, w.policy); err != nil {
		t.Fatal(err)
	}
	options := codex.TurnOptions{AdditionalContext: map[string]codex.AdditionalContextEntry{"application": {Kind: "application", Value: strings.Repeat("context ", 5000)}}}
	firstSize := 0
	for i := 0; i < 65; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		if _, err := w.client.SendWithOptions(context.Background(), w.thread, fmt.Sprint("input ", i), id, "", options); err != nil {
			t.Fatal(i, err)
		}
		saved, err := w.client.RetryStore().Load(w.scope)
		if err != nil || saved.ManagedTurn == nil || saved.ManagedTurn.Count != uint64(i+1) || len(saved.Snapshots) != 0 || saved.Handled != "" {
			t.Fatal("accepted input was not compacted", i, err)
		}
		data, _ := json.Marshal(saved)
		if i == 0 {
			firstSize = len(data)
		}
		if len(data) > firstSize+30 {
			t.Fatal("repeated instructions/context grew retained evidence", i, len(data), firstSize)
		}
		if i == 64 || i == 73 || i == 199 {
			t.Logf("accepted=%d snapshots=%d bytes=%d", i+1, len(saved.Snapshots), len(data))
		}
	}
	w.mu.Lock()
	w.status, w.loaded = "failed", "systemError"
	w.mu.Unlock()
	now := time.Now()
	controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "test", Capacity: true, Cybersecurity: true}, Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Clock: func() time.Time { return now }}
	if err := controller.Step(context.Background(), w.scope); err != nil {
		t.Fatal(err)
	}
	saved, _ := controller.Store.Load(w.scope)
	if saved.Pending == nil || saved.Pending.Snapshot.Options.Model != w.model || saved.Pending.Snapshot.Options.ReasoningEffort != w.effort || saved.Pending.Snapshot.Mode != "default" || !reflect.DeepEqual(saved.Pending.Snapshot.Options.AdditionalContext, options.AdditionalContext) || !reflect.DeepEqual(saved.Pending.Snapshot.Options.ThreadPolicy, w.policy) {
		t.Fatal("large accepted turn lost exact original settings/context")
	}
}

func TestRetryManagedTurnMoreThan256PagesAndRestart(t *testing.T) {
	r := &retryRetentionWire{}
	w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
	retryRetentionSend(t, w, 0)
	w.mu.Lock()
	w.retryPageLimit = 1
	for i := 0; i < 260; i++ {
		w.items = append(w.items, map[string]any{"turnId": w.turn, "item": map[string]any{"id": fmt.Sprint("tool-", i), "type": "agentMessage", "text": "bounded native progress"}})
	}
	w.mu.Unlock()
	w.client.forgetRetryReader(w.thread)
	w.client.retryProofPageBudget = 1
	completed := false
	for i := 0; i < 600; i++ {
		if i == 130 {
			retryRetentionRestart(t, w)
			w.client.retryProofPageBudget = 1
		}
		err := advanceManagedTest(t, w)
		if err == nil {
			completed = true
			break
		}
		if !errors.Is(err, turnretry.ErrProofIncomplete) {
			t.Fatal(i, err)
		}
		saved, _ := w.client.RetryStore().Load(w.scope)
		if saved.Handled != "" || saved.ManagedTurn.Count != 1 || len(saved.Snapshots) != 0 {
			t.Fatal("incomplete pages consumed or grew authority")
		}
	}
	if !completed || w.retryPageCalls <= 256 {
		t.Fatal("incremental proof did not finish beyond old cutoff", w.retryPageCalls)
	}
	saved, _ := w.client.RetryStore().Load(w.scope)
	if err := w.client.RetryBackend().Prove(context.Background(), w.scope, &saved, w.turn); err != nil {
		t.Fatal("complete replay lost proof", err)
	}
}

func TestRetryManagedTurnPrefixAndUnknownInputRefuse(t *testing.T) {
	for _, changed := range []string{"text", "order", "truncated", "unknown", "wrong-turn"} {
		t.Run(changed, func(t *testing.T) {
			r := &retryRetentionWire{}
			w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
			retryRetentionSend(t, w, 0)
			retryRetentionSend(t, w, 1)
			w.mu.Lock()
			switch changed {
			case "text":
				w.items[0].(map[string]any)["item"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "changed"}}
			case "order":
				w.items[0], w.items[1] = w.items[1], w.items[0]
			case "truncated":
				w.items = w.items[:1]
			case "unknown":
				w.items = append(w.items, retrySettingsInput(w.turn, "foreign", "unmanaged input")...)
			case "wrong-turn":
				w.items[0].(map[string]any)["turnId"] = "different"
			}
			w.mu.Unlock()
			retryRetentionRestart(t, w)
			if err := advanceManagedTest(t, w); !errors.Is(err, turnretry.ErrMissingProof) {
				t.Fatal("changed/unknown native prefix accepted", err)
			}
		})
	}
}

func TestRetryManagedTurnMalformedCursorAndFailedSave(t *testing.T) {
	t.Run("cursor", func(t *testing.T) {
		r := &retryRetentionWire{}
		w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
		retryRetentionSend(t, w, 0)
		w.mu.Lock()
		w.items = append(w.items, map[string]any{"turnId": w.turn, "item": map[string]any{"id": "next", "type": "agentMessage"}})
		w.retryPageLimit, w.retryNextCursor = 1, "retry-page:1"
		w.mu.Unlock()
		w.client.retryProofPageBudget = 1
		w.client.forgetRetryReader(w.thread)
		if err := advanceManagedTest(t, w); !errors.Is(err, turnretry.ErrProofIncomplete) {
			t.Fatal(err)
		}
		if err := advanceManagedTest(t, w); !errors.Is(err, turnretry.ErrMissingProof) {
			t.Fatal("repeated native cursor accepted", err)
		}
	})
	t.Run("save", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission fault requires unprivileged test user")
		}
		r := &retryRetentionWire{}
		w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
		r.lostID = "lost-fold"
		if _, err := w.client.Send(context.Background(), w.thread, "accepted before lost reply", r.lostID, ""); err == nil {
			t.Fatal("fixture lost reply not reproduced")
		}
		r.mu.Lock()
		r.hideHistory = false
		r.mu.Unlock()
		before, _ := w.client.RetryStore().Load(w.scope)
		tx, err := w.client.RetryStore().Lock(context.Background(), w.scope)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Close()
		if err := os.Chmod(w.recordDirectory, 0500); err != nil {
			t.Fatal(err)
		}
		err = (retryBackend{w.client}).AdvanceProof(context.Background(), w.scope, tx, w.turn)
		if restore := os.Chmod(w.recordDirectory, 0700); restore != nil {
			t.Fatal(restore)
		}
		tx.Close()
		if err == nil {
			t.Fatal("fold save fault did not occur")
		}
		after, _ := w.client.RetryStore().Load(w.scope)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("failed save consumed durable source evidence")
		}
		w.client.retryProofMu.Lock()
		_, cached := w.client.retryProofReaders[w.thread]
		w.client.retryProofMu.Unlock()
		if cached {
			t.Fatal("failed fold save published a cursor")
		}
		retryRetentionRestart(t, w)
		if err := advanceManagedTest(t, w); err != nil {
			t.Fatal(err)
		}
		if _, err := w.client.Send(context.Background(), w.thread, "accepted before lost reply", r.lostID, ""); err != nil || r.count() != 1 {
			t.Fatal("exact folded receipt replay failed or resent", err)
		}
	})
}

func TestRetryManagedTurnAlreadyHandledVolumeAndWarmSettings(t *testing.T) {
	for _, settings := range []bool{false, true} {
		t.Run(fmt.Sprint("settings=", settings), func(t *testing.T) {
			r := &retryRetentionWire{}
			w := newRetrySettingsWireWithServer(t, false, "default", r.serve)
			w.policy.DeveloperInstructions = strings.Repeat("retained role ", 4000)
			if err := w.client.ActivateThread(context.Background(), w.thread, w.policy); err != nil {
				t.Fatal(err)
			}
			if settings {
				model := w.model // An explicit intent, without changing the model.
				if _, err := w.client.UpdateThreadSettings(context.Background(), w.thread, codex.ThreadSettingsUpdate{Model: &model}); err != nil {
					t.Fatal(err)
				}
			} else {
				tx, _ := w.client.RetryStore().Lock(context.Background(), w.scope)
				tx.State.Handled = w.turn
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
			}
			w.mu.Lock()
			w.items = retrySettingsInput(w.turn, "missing-legacy-admission", "already handled input")
			w.mu.Unlock()
			for i := 0; i < 74; i++ {
				retryRetentionSend(t, w, i)
				if i == 36 {
					retryRetentionRestart(t, w)
				}
			}
			saved, err := w.client.RetryStore().Load(w.scope)
			if err != nil || saved.Handled != "manual" || saved.ManagedTurn != nil || len(saved.Snapshots) > 64 || saved.Binding == nil {
				t.Fatal("handled active turn refused bounded input or acquired authority", err)
			}
			data, _ := json.Marshal(saved)
			if len(data) >= turnretry.MaxBytes {
				t.Fatal("handled byte-pressure retirement exceeded budget")
			}
			w.mu.Lock()
			w.status, w.loaded = "failed", "systemError"
			w.mu.Unlock()
			controller := turnretry.Controller{Store: w.client.RetryStore(), Backend: w.client.RetryBackend(), Policy: turnretry.Decision{Revision: "test", Capacity: true, Cybersecurity: true}}
			if err := controller.Step(context.Background(), w.scope); err != nil || r.count() != 74 {
				t.Fatal("handled turn was rearmed", err)
			}
		})
	}
}
