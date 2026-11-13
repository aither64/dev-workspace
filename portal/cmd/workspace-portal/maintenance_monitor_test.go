package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type maintenanceUtilityClient struct {
	monitoroperation.LaunchClient
	operation, cwd, project, thread string
	idleErr                         error
	idleChecks, mutations           int
}

func (f *maintenanceUtilityClient) CreateProject(_ context.Context, name, key string) (codex.ProjectMetadata, error) {
	f.mutations++
	f.project = key
	return codex.ProjectMetadata{ID: key, Name: name}, nil
}
func (f *maintenanceUtilityClient) ReadProject(_ context.Context, id string) (codex.ProjectMetadata, error) {
	return codex.ProjectMetadata{ID: id, Name: "monitor " + f.operation}, nil
}
func (f *maintenanceUtilityClient) StartThreadWithSettings(context.Context, string, map[string]string, codex.ThreadSettings) (string, error) {
	f.mutations++
	f.thread = "utility"
	return f.thread, nil
}
func (f *maintenanceUtilityClient) ReadThreadMetadata(_ context.Context, id string, _ bool) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{ID: id, Cwd: f.cwd, ProjectID: &f.project, Source: "vscode"}, nil
}
func (f *maintenanceUtilityClient) BootstrapHeadlessThread(context.Context, string, string, string, string) error {
	f.mutations++
	return nil
}
func (f *maintenanceUtilityClient) VerifyHeadlessBootstrap(context.Context, string, string, string, string) error {
	return nil
}
func (f *maintenanceUtilityClient) ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{}, false, nil
}
func (f *maintenanceUtilityClient) SendWithOptions(_ context.Context, _ string, _ string, id string, _ string, _ codex.TurnOptions) (codex.SendReceipt, error) {
	f.mutations++
	return codex.SendReceipt{ClientUserMessageID: id, TurnID: "utility-turn"}, nil
}
func (f *maintenanceUtilityClient) ProveArchivedThread(context.Context, string, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}
func (f *maintenanceUtilityClient) RequireArchiveThreadIdle(context.Context, string, string, workspacecodex.ArchiveState) error {
	f.idleChecks++
	return f.idleErr
}
func (*maintenanceUtilityClient) ArchiveThread(context.Context, string) error {
	panic("maintenance cannot archive a utility")
}

func maintenanceMonitorFixture(t *testing.T, scenarios ...string) (maintenanceControl, maintenance.Operation, *monitoroperation.Service, *maintenanceUtilityClient, monitoroperation.Operation, session.Summary) {
	t.Helper()
	scenario := "completed"
	if len(scenarios) != 0 {
		scenario = scenarios[0]
	}
	m, o := maintenanceIdleHostFixture(t, scenario)
	summary, err := session.Find(m.workspace, "actor")
	if err != nil {
		t.Fatal(err)
	}
	store, err := monitoroperation.NewStore(m.store.Root, m.workspace)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := monitoroperation.IdentityFor(m.workspace, summary.Slug)
	if err != nil {
		t.Fatal(err)
	}
	owner.Socket, owner.SocketEpoch, owner.OriginTurnID, owner.RuntimeSchema = o.Services[0].Socket, o.Services[0].SocketEpoch, "origin", 1
	f := &maintenanceUtilityClient{operation: strings.Repeat("1", 32), cwd: filepath.Join(m.workspace, "work", "actor")}
	service := &monitoroperation.Service{Store: store, Client: f,
		ResolveLead: func(context.Context, string) (monitoroperation.Owner, error) { return owner, nil },
		Resolve:     func(string) (monitoroperation.Owner, error) { return owner, nil },
		LockSession: func(context.Context, string) (func(), error) { return func() {}, nil },
		Admit:       func(context.Context, monitoroperation.Owner) error { return nil },
		Activate:    func(context.Context, monitoroperation.Owner, string) error { return nil },
		Policy: func(context.Context, monitoroperation.Owner) (monitoroperation.UtilityPolicy, error) {
			return monitoroperation.UtilityPolicy{PackageRoot: m.workspace, CatalogDigest: strings.Repeat("a", 64), NativeName: "watcher", Model: "gpt-6-luna", Effort: "low", Instructions: "observe only", Sandbox: "danger-full-access", ApprovalPolicy: "never", MaxConcurrent: 1}, nil
		},
	}
	op, err := service.Start(context.Background(), owner.RootThreadID, monitoroperation.StartRequest{ID: f.operation, Brief: "Observe exact disposable check", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture"}, Cwd: m.workspace, InputIdentity: "fixture", Revision: "fixture", InvocationID: f.operation}})
	if err != nil {
		t.Fatal(err)
	}
	return m, o, service, f, op, *summary
}

func TestMaintenanceUtilityInventoryRequiresOriginalEffectsAndNativeProof(t *testing.T) {
	ctx := context.Background()
	m, o, service, f, op, summary := maintenanceMonitorFixture(t)
	before := f.mutations
	view, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, f)
	if err == nil || len(view.Threads) != 1 || view.Threads[0].ThreadID != op.Launch.ThreadID {
		t.Fatal("running/unreported utility granted readiness", view, err)
	}
	zero := 0
	if _, err = service.Report(ctx, op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &zero}); err != nil {
		t.Fatal(err)
	}
	f.idleErr = errors.New("native effect still active")
	if _, err = maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, f); err == nil {
		t.Fatal("terminal report replaced native proof")
	}
	f.idleErr = nil
	if _, err = maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, f); err != nil {
		t.Fatal(err)
	}
	tx, err := service.Store.Lock(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Operation.Events[0].Delivery.Status = "submitting"
	options := codex.TurnOptions{}
	tx.Operation.Events[0].Delivery.Options = &options
	if err = tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	if _, err = maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, f); err == nil {
		t.Fatal("unknown lead receipt granted readiness")
	}
	if f.mutations != before || f.idleChecks != 2 {
		t.Fatal("inventory performed effects or skipped exact proof", f.mutations, before, f.idleChecks)
	}
	// Utility remains outside the cooperative participant list and never gets a
	// notice or continuation even when its wrapper has ended.
	if err = m.inventory(ctx, &o); err == nil {
		t.Fatal("unresolved monitor allowed preparation")
	}
	if len(o.Participants) != 0 || !strings.Contains(strings.Join(o.Blockers, ","), "monitor-operation-unresolved") {
		t.Fatal(o)
	}
}

func TestMaintenanceMonitorAdmissionHoldsEffectsWithoutFencingSourceCompletion(t *testing.T) {
	ctx := context.Background()
	m, o, service, f, op, _ := maintenanceMonitorFixture(t)
	if _, err := m.store.Update(ctx, o.OperationID, 0, func(saved *maintenance.Operation) error { *saved = o; return nil }); err != nil {
		t.Fatal(err)
	}
	client := newCodexClient(o.Services[0].Socket, m.workspace, m.store.Root)
	client.HostProfile = m.store.Profile
	defer client.Close()
	production, err := newMonitorService(m.workspace, m.store.Root, filepath.Join(m.store.Root, "authority"), o.Services[0].Socket, "/unused-package", client)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []func() error{
		func() error { return production.Admit(ctx, op.Owner) },
		func() error { return production.Activate(ctx, op.Owner, op.Launch.ThreadID) },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "maintenance") {
			t.Fatal("new monitor effect not held before native authority", err)
		}
	}
	reportCtx, cancelReport := context.WithTimeout(ctx, time.Second)
	defer cancelReport()
	before := f.mutations
	zero := 0
	report, err := service.Report(reportCtx, op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Status(reportCtx, op.Owner.RootThreadID, op.ID); err != nil {
		t.Fatal(err)
	}
	if len(report.Events) != 1 || report.Events[0].Delivery.Status != "pending" || f.mutations != before {
		t.Fatal("source report activated work", report)
	}
	// Corrupt state is a fence, not an empty operation. No new effect escapes.
	if err = os.WriteFile(m.store.Path(o.OperationID), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = production.Admit(ctx, op.Owner); err == nil {
		t.Fatal("malformed maintenance state admitted launch")
	}
}
