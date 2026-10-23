package workspacecodex

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

func TestRetryPreparedFirstExecutionRetainsOptionsAndOldEpoch(t *testing.T) {
	for _, member := range []bool{false, true} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("member=%t/existing=%t", member, existing), func(t *testing.T) {
				r := &retryRetentionWire{}
				w := newRetrySettingsWireWithServer(t, member, "plan", r.serve)
				ctx := context.Background()
				options := codex.TurnOptions{Model: "original-model", ReasoningEffort: "xhigh", ThreadPolicy: w.policy, AdditionalContext: map[string]codex.AdditionalContextEntry{"managed": {Kind: "application", Value: "immutable context"}}}
				if err := w.client.PrepareSendWithOptions(w.thread, "prepared input", "prepared-id", "plan:original", true, options); err != nil {
					t.Fatal(err)
				}
				var oldSnapshot turnretry.Snapshot
				if existing {
					tx, err := w.client.RetryStore().Lock(ctx, w.scope)
					if err != nil {
						t.Fatal(err)
					}
					if err := w.client.captureRetry(ctx, tx, w.policy, "prepared input", "prepared-id", "steer", options); err != nil {
						t.Fatal(err)
					}
					oldSnapshot = tx.State.Snapshots[0]
					tx.Close()
				}
				model := "later-model"
				if _, err := w.client.UpdateThreadSettings(ctx, w.thread, codex.ThreadSettingsUpdate{Model: &model}); err != nil {
					t.Fatal(err)
				}
				if _, err := w.client.SendWithOptions(ctx, w.thread, "prepared input", "prepared-id", "plan:original", options); err != nil {
					t.Fatal(err)
				}
				original, found, err := w.client.OriginalSendOptions(w.thread, "prepared input", "prepared-id", "plan:original")
				if err != nil || !found || !reflect.DeepEqual(original, options) {
					t.Fatal("generic original options changed", original, err)
				}
				state, err := w.client.RetryStore().Load(w.scope)
				if err != nil || len(state.Snapshots) != 1 {
					t.Fatal(err, state)
				}
				p := state.Snapshots[0]
				if existing {
					old := oldSnapshot
					if p.Epoch != old.Epoch || p.BindingDigest != old.BindingDigest || p.Options.Model != old.Options.Model || p.Digest != old.Digest {
						t.Fatal("prepared path retrobound an old snapshot")
					}
				} else if p.Epoch != state.Epoch || !reflect.DeepEqual(p.Options, options) {
					t.Fatal("first admission lost exact authorized original options", p)
				}
				if _, err := w.client.SendWithOptions(ctx, w.thread, "prepared input", "prepared-id", "plan:original", options); err != nil || r.count() != 1 {
					t.Fatal("prepared recovery repeated delivery", err, r.count())
				}
			})
		}
	}
}
