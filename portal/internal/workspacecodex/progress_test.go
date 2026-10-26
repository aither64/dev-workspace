package workspacecodex

import (
	"context"
	"fmt"
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
	if _, err := wire.client.SendProgressCheck(ctx, wire.thread, "Automatic progress check", "00000000-0000-4000-8000-000000009102", "progress:example:check", codex.TurnOptions{Model: wire.model, ReasoningEffort: wire.effort, ThreadPolicy: wire.policy}); err == nil {
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
