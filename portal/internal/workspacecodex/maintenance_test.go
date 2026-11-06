package workspacecodex

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
)

func TestMaintenanceHoldsManagedAdmissionsButPreservesOriginalReceiptAndQueue(t *testing.T) {
	ctx := context.Background()
	w := newRetrySettingsWire(t, false, "default")
	queue, err := w.client.QueueWithPolicy(ctx, w.thread, "accepted queued work", "saved-queue", w.policy)
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.status, w.loaded, w.explicitInput = "completed", "idle", "accepted source input"
	w.items = []any{}
	w.bound = false
	w.mu.Unlock()
	options := codex.TurnOptions{Model: w.model, ReasoningEffort: w.effort}
	receipt, err := w.client.SendWithOptions(ctx, w.thread, "accepted source input", "source-input", "fixture", options)
	if err != nil {
		t.Fatal(err)
	}
	before, err := w.client.RetryStore().Load(w.scope)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(w.client.RecoveryRoot, "profile")
	w.client.HostProfile = profile
	store := maintenance.Store{Root: w.client.RecoveryRoot, Profile: profile}
	op := maintenance.Operation{Schema: maintenance.Schema, OperationID: "11111111-1111-4111-8111-111111111111", Revision: 1, Phase: "pausing", Profile: profile, SourceProfileToken: "token", SourcePackage: "/source", CandidatePackage: "/candidate", PlansDigest: "plans", Services: []maintenance.Service{{Workspace: w.scope.Workspace, Unit: "native.service", Native: "/native", PID: 123, ProcessIdentity: "pid", Socket: w.scope.Socket, SocketEpoch: "epoch"}}, Participants: []maintenance.Participant{}, Blockers: []string{}}
	if err = store.Create(ctx, op); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func() error{
		func() error {
			_, err := w.client.SendWithOptions(ctx, w.thread, "new input", "new-input", "fixture", options)
			return err
		},
		func() error {
			_, err := w.client.QueueWithPolicy(ctx, w.thread, "new queue", "new-queue", w.policy)
			return err
		},
		func() error { return w.client.ActivateThread(ctx, w.thread, w.policy) },
	} {
		if err = run(); err == nil || !strings.Contains(err.Error(), "maintenance") {
			t.Fatal("new effects admitted", err)
		}
	}
	recovered, found, err := w.client.ReconcileSendWithOptions(ctx, w.thread, "accepted source input", "source-input", "fixture", options)
	if err != nil || !found || !reflect.DeepEqual(recovered, receipt) {
		t.Fatal("original receipt held or changed", recovered, found, err)
	}
	after, err := w.client.RetryStore().Load(w.scope)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("hold rewrote managed proof", err)
	}
	queued, err := w.client.ListQueue(ctx, w.thread)
	if err != nil || len(queued) != 1 || !reflect.DeepEqual(queued[0], queue) {
		t.Fatal("accepted queue changed", queued, err)
	}
	if w.client.allowImplicitResume(w.thread) {
		t.Fatal("maintenance implicitly resumed thread")
	}
}
