package workspacecodex

import (
	"context"
	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"strings"
	"testing"
	"time"
)

func TestTransitionProofRefusesOwnedRetryWithoutChangingIt(t *testing.T) {
	transport := &retryRetentionWire{}
	wire := newRetrySettingsWireWithServer(t, true, "default", transport.serve)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := wire.client.SendWithOptions(ctx, wire.thread, "Manual assignment", "00000000-0000-4000-8000-000000009101", "team:example:lead:observer0", codex.TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	wire.status, wire.loaded, wire.failureCode = "failed", "idle", "serverOverloaded"
	wire.mu.Unlock()
	controller := turnretry.Controller{Store: wire.client.RetryStore(), Backend: wire.client.RetryBackend(), Policy: turnretry.Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}, Clock: time.Now, Random: func() float64 { return .5 }}
	if err := controller.Step(ctx, wire.scope); err != nil {
		t.Fatal(err)
	}
	before, err := wire.client.RetryStore().Load(wire.scope)
	if err != nil || !before.Owned() {
		t.Fatal("no scheduled retry", err)
	}
	cold := NewTransitionClient(wire.client.socket, wire.client.workspace, codex.ClientOptions{})
	defer cold.Close()
	cold.RecoveryRoot = wire.client.RecoveryRoot
	cold.RetryPolicyResolver = wire.client.RetryPolicyResolver
	if err := cold.RequireTransitionStorage(ctx, wire.thread); err == nil || !strings.Contains(err.Error(), "pending automatic retry") {
		t.Fatal("retry crossed transition", err)
	}
	after, err := wire.client.RetryStore().Load(wire.scope)
	if err != nil || after.Pending.ID != before.Pending.ID || after.Phase != before.Phase {
		t.Fatal("retry changed", err)
	}
}
