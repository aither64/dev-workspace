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

func TestMaintenanceMonitorDiagnosticProjection(t *testing.T) {
	const prefix = "prepare-inventory-blocked:monitor-operation-unresolved,retained-member-transition,unowned-loaded-writer"
	const id = "abcdef12-1122-4122-8122-112233441122"
	private := errors.New("PRIVATE /peer/path prompt token")
	for _, tc := range []struct{ name, id, slug, wantID, wantSlug string }{
		{"valid", id, "2026-11-11-owner", id, "2026-11-11-owner"},
		{"unknown", "", "", "unknown", "unknown"},
		{"compact", strings.Repeat("a", 32), "actor", "unknown", "actor"},
		{"control", id + "\nPRIVATE", "actor\nPRIVATE", "unknown", "unknown"},
		{"upper-id", strings.ToUpper(id), "../PRIVATE", "unknown", "unknown"},
		{"oversized", strings.Repeat("a", 2048), strings.Repeat("a", 129), "unknown", "unknown"},
		{"bounded", id, strings.Repeat("a", 128), id, strings.Repeat("a", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := &monitoroperation.DiagnosticError{Stage: "native-idle", Category: "unavailable-proof", MonitorID: tc.id, OwnerSlug: tc.slug, Err: private}
			err := maintenanceMonitorBlockers([]string{"unowned-loaded-writer", "monitor-operation-unresolved", "retained-member-transition", "monitor-operation-unresolved"}, detail)
			reason := maintenanceControlFailureReason(err)
			want := prefix + ";monitor-stage=native-idle;monitor-category=unavailable-proof;monitor-id=" + tc.wantID + ";owner-slug=" + tc.wantSlug
			if reason != want || len(reason) > 1024 || strings.Contains(reason, "PRIVATE") || !errors.Is(err, private) || err.Error() != reason {
				t.Fatalf("unsafe diagnostic/cause: %q", reason)
			}
		})
	}
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		category := "deadline"
		if cause == context.Canceled {
			category = "cancelled"
		}
		detail := &monitoroperation.DiagnosticError{Stage: "PRIVATE", Category: "PRIVATE", MonitorID: id, OwnerSlug: "actor", Err: private, Cause: cause}
		reason := maintenanceMonitorReason(prefix, detail)
		if !strings.Contains(reason, ";monitor-stage=unknown;monitor-category="+category+";") || strings.Contains(reason, "PRIVATE") {
			t.Fatal(reason)
		}
	}
	if got := maintenanceControlFailureReason(maintenanceMonitorBlockers([]string{"retained-member-transition"}, nil)); got != "prepare-inventory-blocked:retained-member-transition" {
		t.Fatal(got)
	}
}

func TestMaintenanceMonitorInventoryRetainsTypedNativeCause(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, errors.New("PRIVATE native error /path")} {
		_, o, service, f, op, summary := maintenanceMonitorFixture(t)
		zero := 0
		if _, err := service.Report(context.Background(), op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &zero}); err != nil {
			t.Fatal(err)
		}
		before := f.mutations
		f.idleErr = cause
		_, err := maintenanceMonitorInventory(context.Background(), service.Store, summary, o.Services[0].Socket, f)
		var detail *monitoroperation.DiagnosticError
		if !errors.Is(err, cause) || !errors.As(err, &detail) || detail.Stage != "native-idle" || detail.MonitorID != op.ID || detail.OwnerSlug != summary.Slug || f.idleChecks != 1 || f.mutations != before {
			t.Fatalf("lost native boundary: %v", err)
		}
		reason := maintenanceControlFailureReason(maintenanceMonitorBlockers([]string{"monitor-operation-unresolved"}, err))
		if strings.Contains(reason, "PRIVATE") || strings.Contains(reason, "/path") {
			t.Fatal(reason)
		}
	}
}

func TestMaintenanceInventoryRetainsMonitorDiagnosticWithoutChangingBlockers(t *testing.T) {
	m, o, _, f, _, _ := maintenanceMonitorFixture(t)
	before := f.mutations
	err := m.inventory(context.Background(), &o)
	reason := maintenanceControlFailureReason(err)
	if !strings.HasPrefix(reason, "prepare-inventory-blocked:monitor-operation-unresolved;") || !strings.Contains(reason, ";monitor-stage=receipt-proof;monitor-category=uncertain-state;monitor-id=unknown;owner-slug=actor") {
		t.Fatal(reason)
	}
	if f.mutations != before || len(o.Blockers) != 1 || o.Blockers[0] != "monitor-operation-unresolved" {
		t.Fatal("diagnostic changed blockers/effects", o.Blockers)
	}
}

func TestMaintenanceMonitorPreRecordDiagnostics(t *testing.T) {
	for _, stage := range []string{"inventory-store", "owner-identity"} {
		t.Run(stage, func(t *testing.T) {
			_, o, service, f, _, summary := maintenanceMonitorFixture(t)
			ctx := context.Background()
			if stage == "inventory-store" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				summary.Workspace = t.TempDir()
			}
			before := f.mutations
			_, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, f)
			var detail *monitoroperation.DiagnosticError
			if err == nil || !errors.As(err, &detail) || detail.Stage != stage || detail.MonitorID != "" || detail.OwnerSlug != "" || f.mutations != before {
				t.Fatalf("misattributed pre-record failure: %v", err)
			}
			if stage == "inventory-store" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation", err)
			}
			reason := maintenanceMonitorReason("prepare-inventory-blocked:monitor-operation-unresolved", err)
			if !strings.HasSuffix(reason, ";monitor-id=unknown;owner-slug=unknown") || strings.Contains(reason, summary.Workspace) {
				t.Fatal(reason)
			}
		})
	}
}
