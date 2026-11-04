package monitoroperation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type lifecycleFixtureClient struct {
	*launchFixtureClient
	state       workspacecodex.ArchiveState
	idleError   error
	foreign     bool
	archives    int
	lostArchive bool
	fresh       bool
	prepared    bool
}

func (f *lifecycleFixtureClient) ReadThreadMetadata(ctx context.Context, thread string, cold bool) (codex.ThreadMetadata, error) {
	metadata, err := f.launchFixtureClient.ReadThreadMetadata(ctx, thread, cold)
	metadata.Source = "vscode"
	metadata.Status = map[string]any{"type": "idle"}
	if f.fresh {
		metadata.ProjectID = nil
	}
	if f.foreign {
		project := "foreign"
		metadata.ProjectID = &project
	}
	return metadata, err
}
func (f *lifecycleFixtureClient) HeadlessThreadMaterialized(context.Context, string, string, string) (bool, error) {
	return !f.fresh, nil
}
func (f *lifecycleFixtureClient) IsPreparedSendWithOptions(thread, text, id, action string, options codex.TurnOptions) (bool, error) {
	op := f.durable()
	return f.prepared && thread == op.Launch.ThreadID && text == op.Launch.Envelope && id == op.Launch.MessageID &&
		action == "monitor-launch:"+op.ID && reflect.DeepEqual(options, op.Launch.Options), nil
}
func (f *lifecycleFixtureClient) ObserveThreadIdentityExceptPreparedSend(_ context.Context, thread, cwd, project string, state workspacecodex.ArchiveState, prepared workspacecodex.PreparedSend) (workspacecodex.ThreadObservation, error) {
	op := f.durable()
	if thread != op.Launch.ThreadID || project != op.Launch.ProjectID || prepared.ID != op.Launch.MessageID || prepared.Text != op.Launch.Envelope || prepared.Action != "monitor-launch:"+op.ID || !reflect.DeepEqual(prepared.Options, op.Launch.Options) {
		return workspacecodex.ThreadObservation{}, errors.New("wrong prepared exception")
	}
	return workspacecodex.ThreadObservation{ActivityKnown: true, Idle: true}, nil
}
func (f *lifecycleFixtureClient) ProveArchivedThread(context.Context, string, string, string) (workspacecodex.ArchiveState, error) {
	return f.state, nil
}
func (f *lifecycleFixtureClient) RequireArchiveThreadIdle(context.Context, string, string, workspacecodex.ArchiveState) error {
	return f.idleError
}
func (f *lifecycleFixtureClient) ArchiveThread(context.Context, string) error {
	f.archives++
	f.state = workspacecodex.ArchiveArchived
	if f.lostArchive {
		return errors.New("lost archive acknowledgement")
	}
	return nil
}

func lifecycleFixture(t *testing.T) (*Service, *lifecycleFixtureClient, Lifecycle, Operation) {
	t.Helper()
	service, native, request, _ := operationFixture(t)
	op, err := service.Start(context.Background(), "lead", request)
	if err != nil {
		t.Fatal(err)
	}
	client := &lifecycleFixtureClient{launchFixtureClient: native, state: workspacecodex.ArchiveActive}
	lifecycle := Lifecycle{Store: service.Store, Client: client}
	service.CheckResolution = lifecycle.ProveQuiescent
	return service, client, lifecycle, op
}

func TestMonitorLifecycleResolutionRequiresNativeProofAndPreservesOriginalOutcome(t *testing.T) {
	ctx := context.Background()
	service, client, lifecycle, op := lifecycleFixture(t)
	report := ReportRequest{Slot: "terminal", Outcome: "incomplete", StillRunning: "exact-command", Detail: "lost command status"}
	if _, err := service.Report(ctx, op.Launch.ThreadID, report); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RequireIdle(ctx, op.Owner.Slug, op.Owner.RootThreadID, false); err == nil {
		t.Fatal("external effects were silently treated as complete")
	}
	resolution := ResolutionRequest{OperationID: op.ID, ResolutionID: strings.Repeat("a", 32), Evidence: []string{"exact invocation receipt"}, Detail: "verified command exited"}
	client.idleError = errors.New("utility is still active")
	if _, err := service.ResolveEffects(ctx, "lead", resolution); err == nil {
		t.Fatal("resolution accepted active utility")
	}
	client.idleError = nil
	resolved, err := service.ResolveEffects(ctx, "lead", resolution)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resolution == nil || !reflect.DeepEqual(resolved.Events[0].Report, report) || client.archives != 0 || client.starts != 1 || client.sends != 1 {
		t.Fatal("resolution changed original outcome or executed native effects", resolved)
	}
	if err := lifecycle.RequireIdle(ctx, op.Owner.Slug, op.Owner.RootThreadID, false); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorLifecycleArchivesOnlyExactIdleUtilityAndRecoversLostReply(t *testing.T) {
	ctx := context.Background()
	service, client, lifecycle, op := lifecycleFixture(t)
	zero := 0
	if _, err := service.Report(ctx, op.Launch.ThreadID, ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &zero}); err != nil {
		t.Fatal(err)
	}
	client.foreign = true
	if err := lifecycle.Archive(ctx, op.Owner.Slug, op.Owner.RootThreadID); err == nil || client.archives != 0 {
		t.Fatal("archived an unproved project")
	}
	client.foreign = false
	client.lostArchive = true
	if err := lifecycle.Archive(ctx, op.Owner.Slug, op.Owner.RootThreadID); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Archive(ctx, op.Owner.Slug, op.Owner.RootThreadID); err != nil {
		t.Fatal(err)
	}
	retained, err := service.Store.Get(ctx, op.ID)
	if err != nil || retained.Disposition != "archived" || retained.CanceledAt == nil || client.archives != 1 || client.starts != 1 || client.sends != 1 {
		t.Fatal("retirement was not exact and idempotent", retained, err)
	}
	if err := lifecycle.RequireArchived(ctx, op.Owner.Slug, op.Owner.RootThreadID); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorLifecycleUnknownLaunchAndSubmissionCannotBeResolved(t *testing.T) {
	_, client, lifecycle, op := lifecycleFixture(t)
	for _, change := range []func(*Operation){
		func(op *Operation) { op.Launch.ThreadID = ""; op.Launch.CreateAttempted = true },
		func(op *Operation) { op.Launch.Receipt = nil; op.Launch.State = "submitting" },
		func(op *Operation) { op.Events = []Event{{Delivery: Delivery{Status: "submitting"}}} },
	} {
		candidate := cloneOperation(op)
		change(&candidate)
		if err := lifecycle.ProveQuiescent(context.Background(), candidate); err == nil {
			t.Fatal("unknown native outcome passed resolution proof")
		}
	}
	if client.archives != 0 || client.starts != 1 || client.sends != 1 {
		t.Fatal("proof performed native mutations")
	}
}

func TestMonitorLifecycleTerminalResultSupersedesHistoricalEscalation(t *testing.T) {
	ctx := context.Background()
	service, _, lifecycle, op := lifecycleFixture(t)
	if _, err := service.Report(ctx, op.Launch.ThreadID, ReportRequest{Slot: "escalation", Outcome: "incomplete", StillRunning: "exact CI run", Detail: "operator guidance required"}); err != nil {
		t.Fatal(err)
	}
	exit := 7
	if _, err := service.Report(ctx, op.Launch.ThreadID, ReportRequest{Slot: "terminal", Outcome: "failed", ExitStatus: &exit}); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RequireIdle(ctx, op.Owner.Slug, op.Owner.RootThreadID, false); err != nil {
		t.Fatal("authoritative completion did not supersede historical running identity", err)
	}
}

func TestMonitorLifecycleCanceledPreparationUsesOnlyExactReadOnlyException(t *testing.T) {
	_, client, lifecycle, op := lifecycleFixture(t)
	now := time.Now().UTC()
	op.CanceledAt = &now
	op.Launch.State, op.Launch.Receipt = "unsubmitted", nil
	client.prepared = true
	client.idleError = errors.New("ordinary idle refuses a prepared attempt")
	if err := lifecycle.ProveQuiescent(context.Background(), op); err != nil {
		t.Fatal("exact canceled preparation did not retain its observation exception", err)
	}
	op.Launch.MessageID = strings.Repeat("f", 32)
	if err := lifecycle.ProveQuiescent(context.Background(), op); err == nil {
		t.Fatal("different preparation bypassed ordinary idle proof")
	}
	if client.archives != 0 || client.starts != 1 || client.sends != 1 {
		t.Fatal("preparation observation changed native state")
	}
}

func TestMonitorLifecycleCanceledBeforeBootstrapRequiresExactFreshProof(t *testing.T) {
	_, client, lifecycle, op := lifecycleFixture(t)
	now := time.Now().UTC()
	op.CanceledAt = &now
	op.Launch.State, op.Launch.Receipt, op.Launch.BootstrapAttempted = "creating", nil, false
	client.fresh = true
	if err := lifecycle.ProveQuiescent(context.Background(), op); err != nil {
		t.Fatal("exact original fresh utility did not prove quiescent", err)
	}
	op.Launch.BootstrapAttempted = true
	if err := lifecycle.ProveQuiescent(context.Background(), op); err == nil {
		t.Fatal("bootstrap uncertainty adopted missing project metadata")
	}
	if client.archives != 0 || client.starts != 1 || client.sends != 1 {
		t.Fatal("fresh proof recycled, archived, or restarted utility")
	}
}
