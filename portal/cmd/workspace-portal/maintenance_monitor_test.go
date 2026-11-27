package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	idleWait                        bool
	idleChecks, mutations           int
	reads                           map[string]int
	readHook                        func(context.Context, string, string) error
	projects, threadProjects        map[string]string
}

func (f *maintenanceUtilityClient) CreateProject(_ context.Context, name, key string) (codex.ProjectMetadata, error) {
	f.mutations++
	f.project = key
	if f.projects == nil {
		f.projects, f.threadProjects = map[string]string{}, map[string]string{}
	}
	f.projects[key] = name
	return codex.ProjectMetadata{ID: key, Name: name}, nil
}
func (f *maintenanceUtilityClient) read(ctx context.Context, stage, id string) error {
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[stage]++
	if f.readHook != nil {
		return f.readHook(ctx, stage, id)
	}
	return nil
}
func (f *maintenanceUtilityClient) ReadProject(ctx context.Context, id string) (codex.ProjectMetadata, error) {
	return codex.ProjectMetadata{ID: id, Name: f.projects[id]}, f.read(ctx, "native-project", id)
}
func (f *maintenanceUtilityClient) StartThreadWithSettings(context.Context, string, map[string]string, codex.ThreadSettings) (string, error) {
	f.mutations++
	f.thread = "utility-" + f.operation
	f.threadProjects[f.thread] = f.project
	return f.thread, nil
}
func (f *maintenanceUtilityClient) ReadThreadMetadata(ctx context.Context, id string, includeTurns bool) (codex.ThreadMetadata, error) {
	stage := "native-thread:false"
	if includeTurns {
		stage = "native-thread:true"
	}
	project := f.threadProjects[id]
	return codex.ThreadMetadata{ID: id, Cwd: f.cwd, ProjectID: &project, Source: "vscode"}, f.read(ctx, stage, id)
}
func (f *maintenanceUtilityClient) BootstrapHeadlessThread(context.Context, string, string, string, string) error {
	f.mutations++
	return nil
}
func (f *maintenanceUtilityClient) VerifyHeadlessBootstrap(ctx context.Context, id, _, _, _ string) error {
	return f.read(ctx, "native-bootstrap", id)
}
func (f *maintenanceUtilityClient) ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{}, false, nil
}
func (f *maintenanceUtilityClient) SendWithOptions(_ context.Context, _ string, _ string, id string, _ string, _ codex.TurnOptions) (codex.SendReceipt, error) {
	f.mutations++
	return codex.SendReceipt{ClientUserMessageID: id, TurnID: "utility-turn"}, nil
}
func (f *maintenanceUtilityClient) ProveArchivedThread(ctx context.Context, id, _, _ string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, f.read(ctx, "native-archive-state", id)
}
func (f *maintenanceUtilityClient) RequireArchiveThreadIdle(ctx context.Context, id string, _ string, _ workspacecodex.ArchiveState) error {
	f.idleChecks++
	if err := f.read(ctx, "native-idle", id); err != nil {
		return err
	}
	if f.idleWait {
		<-ctx.Done()
		return errors.Join(f.idleErr, ctx.Err())
	}
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
	if len(scenarios) > 1 {
		f.operation = scenarios[1]
	}
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
				if err := os.Remove(filepath.Join(summary.Workspace, "work", summary.Slug, "portal.yml")); err != nil {
					t.Fatal(err)
				}
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

func TestMaintenanceMonitorInventoryRetainsNativeDeadlineBeforeObserverFailure(t *testing.T) {
	const id = "abcdef12-1122-4122-8122-112233441122"
	_, o, service, f, op, summary := maintenanceMonitorFixture(t, "completed", id)
	zero := 0
	if _, err := service.Report(context.Background(), op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &zero}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartCIObservation(context.Background(), op.Owner.RootThreadID, monitoroperation.StartRequest{ID: strings.Repeat("2", 32), Brief: "Observe disposable source", Source: monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "example/project", WorkflowID: "12", RunID: "34", RunAttempt: 1, Commit: strings.Repeat("a", 40)}}); err != nil {
		t.Fatal(err)
	}
	private := errors.New("PRIVATE native cause /path")
	f.idleWait, f.idleErr = true, private
	before := f.mutations
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, f)
	var detail *monitoroperation.DiagnosticError
	found := errors.As(err, &detail)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, private) || !found || detail.Stage != "native-idle" || detail.MonitorID != id || detail.OwnerSlug != summary.Slug || f.idleChecks != 1 || f.mutations != before {
		t.Fatalf("lost original native deadline/identity or changed effects: detail=%+v err=%v", detail, err)
	}
	if _, storeErr := service.Store.ListObservers(ctx, summary.Slug); !errors.Is(storeErr, context.DeadlineExceeded) {
		t.Fatal("real observer listing did not refuse exhausted context", storeErr)
	}
	reason := maintenanceControlFailureReason(maintenanceMonitorBlockers([]string{"monitor-operation-unresolved"}, err))
	want := "prepare-inventory-blocked:monitor-operation-unresolved;monitor-stage=native-idle;monitor-category=deadline;monitor-id=" + id + ";owner-slug=" + summary.Slug
	if reason != want || strings.Contains(reason, "PRIVATE") || strings.Contains(reason, "/path") {
		t.Fatalf("unsafe or misattributed maintenance diagnostic: %q", reason)
	}
}

func completeMaintenanceMonitorProof(t *testing.T, service *monitoroperation.Service, f *maintenanceUtilityClient, op monitoroperation.Operation) {
	t.Helper()
	ctx := context.Background()
	zero := 0
	if _, err := service.Report(ctx, op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &zero}); err != nil {
		t.Fatal(err)
	}
	service.CheckResolution = (monitoroperation.Lifecycle{Store: service.Store, Client: f}).ProveQuiescent
	if _, err := service.ResolveEffects(ctx, op.Owner.RootThreadID, monitoroperation.ResolutionRequest{OperationID: op.ID, ResolutionID: strings.Repeat("e", 32), Evidence: []string{"disposable joined fixture"}, Detail: "Original synthetic command and native proof completed"}); err != nil {
		t.Fatal(err)
	}
}

func completeMaintenanceObserverProof(t *testing.T, service *monitoroperation.Service, owner monitoroperation.Owner) {
	t.Helper()
	ctx := context.Background()
	service.ObserverEpoch = "disposable-worker"
	record, err := service.StartCIObservation(ctx, owner.RootThreadID, monitoroperation.StartRequest{ID: strings.Repeat("3", 32), Brief: "Observe disposable source", Source: monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "example/project", WorkflowID: "12", RunID: "34", RunAttempt: 1, Commit: strings.Repeat("a", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := service.ClaimCIPoll(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	at, zero := time.Now().UTC(), 0
	joined := *claim.Poll
	joined.State, joined.FinishedAt, joined.ExitStatus = "joined", &at, &zero
	joined.Handle = "owned:" + joined.ID
	snapshot := monitoroperation.CISnapshot{Status: "completed", Conclusion: "success", ObservedAt: at, SourceDigest: claim.SourceDigest}
	if _, err := service.CommitCIPoll(ctx, claim, &snapshot, joined, ""); err != nil {
		t.Fatal(err)
	}
}

func maintenanceMonitorProofBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	err := filepath.WalkDir(filepath.Join(root, "monitor-operations"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		result[path], err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMaintenanceMonitorProofCallCounts(t *testing.T) {
	m, o, service, f, op, summary := maintenanceMonitorFixture(t)
	completeMaintenanceMonitorProof(t, service, f, op)
	completeMaintenanceObserverProof(t, service, op.Owner)
	// A valid foreign native record is decoded before the slug filter. Its
	// missing observer counterpart must not count as a decoded body.
	foreign := *service
	owner := op.Owner
	owner.Slug = "unrelated-owner"
	foreign.ResolveLead = func(context.Context, string) (monitoroperation.Owner, error) { return owner, nil }
	foreign.Resolve = func(string) (monitoroperation.Owner, error) { return owner, nil }
	f.operation = strings.Repeat("4", 32)
	cwd := f.cwd
	f.cwd = filepath.Join(m.workspace, "work", owner.Slug)
	_, err := foreign.Start(context.Background(), owner.RootThreadID, monitoroperation.StartRequest{ID: f.operation, Brief: "Unrelated disposable record", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture"}, Cwd: m.workspace, InputIdentity: "fixture", Revision: "fixture", InvocationID: f.operation}})
	f.cwd = cwd
	if err != nil {
		t.Fatal(err)
	}
	before, mutations := maintenanceMonitorProofBytes(t, m.store.Root), f.mutations
	start := service.Store.RecordReads()
	if records, err := service.Store.List(context.Background(), summary.Slug); err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	nativeReads := service.Store.RecordReads() - start
	start = service.Store.RecordReads()
	if records, err := service.Store.ListObservers(context.Background(), summary.Slug); err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	observerReads := service.Store.RecordReads() - start
	if nativeReads != 2 || observerReads != 1 {
		t.Fatalf("decoder/collision boundary changed: native=%d observer=%d", nativeReads, observerReads)
	}
	want := map[string]int{"native-project": 1, "native-thread:true": 1, "native-bootstrap": 1, "native-archive-state": 1, "native-idle": 1}
	for _, mode := range []string{"maintenance", "direct"} {
		f.reads = nil
		start = service.Store.RecordReads()
		pairs := uint64(1)
		if mode == "maintenance" {
			if _, err := maintenanceMonitorInventory(context.Background(), service.Store, summary, o.Services[0].Socket, f); err != nil {
				t.Fatal(err)
			}
		} else {
			pairs = 1
			view, err := (monitoroperation.Lifecycle{Store: service.Store, Client: f}).AssessLocalSafety(context.Background(), summary.Slug, summary.Codex.ThreadID)
			if err != nil || view.State != "quiescent" {
				t.Fatal(view, err)
			}
		}
		reads := service.Store.RecordReads() - start
		expected := nativeReads + observerReads
		if mode == "maintenance" {
			expected += observerReads
		}
		if reads != expected || !reflect.DeepEqual(f.reads, want) {
			t.Fatalf("%s: decoder=%d lifecycle=%v", mode, reads, f.reads)
		}
		t.Logf("%s: decoder=%d initial-list-pairs=%d late-observer=%t lifecycle=%v; absent collision counterparts add zero body reads", mode, reads, pairs, mode == "maintenance", f.reads)
	}
	if f.mutations != mutations || !reflect.DeepEqual(before, maintenanceMonitorProofBytes(t, m.store.Root)) {
		t.Fatal("read-only assessments changed records or native effects")
	}
}

func TestMaintenanceMonitorSharedProofBudget(t *testing.T) {
	const firstID = "11111111-1111-4111-8111-111111111111"
	const secondID = "22222222-2222-4222-8222-222222222222"
	for _, mode := range []string{"archive", "idle", "earlier-consumer", "first-nondeadline"} {
		t.Run(mode, func(t *testing.T) {
			m, o, service, f, first, summary := maintenanceMonitorFixture(t, "completed", firstID)
			completeMaintenanceMonitorProof(t, service, f, first)
			last := first
			if mode == "earlier-consumer" || mode == "first-nondeadline" {
				f.operation = secondID
				var err error
				last, err = service.Start(context.Background(), first.Owner.RootThreadID, monitoroperation.StartRequest{ID: secondID, Brief: "Second disposable proof", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture"}, Cwd: m.workspace, InputIdentity: "fixture", Revision: "fixture", InvocationID: secondID}})
				if err != nil {
					t.Fatal(err)
				}
				completeMaintenanceMonitorProof(t, service, f, last)
			}
			completeMaintenanceObserverProof(t, service, first.Owner)
			before, mutations := maintenanceMonitorProofBytes(t, m.store.Root), f.mutations
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(joined)
				select {
				case <-entered:
					<-ctx.Done()
				case <-ctx.Done():
				}
				close(release)
			}()
			gateStage, gateThread := "native-idle", last.Launch.ThreadID
			wantStage, wantID := "native-idle", first.ID
			if mode == "archive" {
				gateStage, wantStage = "native-archive-state", "native-archive-state"
			} else if mode == "earlier-consumer" {
				gateThread, wantStage, wantID = first.Launch.ThreadID, "native-project", last.ID
			} else if mode == "first-nondeadline" {
				wantStage = "native-archive-state"
			}
			private := errors.New("PRIVATE first native protocol cause")
			f.reads = nil
			f.readHook = func(callCtx context.Context, stage, id string) error {
				if mode == "first-nondeadline" && stage == "native-archive-state" && id == first.Launch.ThreadID {
					return private
				}
				if stage == gateStage && id == gateThread {
					close(entered)
					<-release
					if mode == "earlier-consumer" {
						return nil // Earlier proof completes at the shared deadline.
					}
				}
				return callCtx.Err()
			}
			_, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, f)
			<-joined
			var detail *monitoroperation.DiagnosticError
			if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &detail) || detail.Stage != wantStage || detail.MonitorID != wantID || detail.OwnerSlug != summary.Slug {
				t.Fatalf("lost first proof boundary: detail=%+v error=%v", detail, err)
			}
			if mode == "first-nondeadline" && (!errors.Is(detail, private) || errors.Is(detail, context.DeadlineExceeded)) {
				t.Fatalf("later inventory deadline replaced first native cause: %+v", detail)
			}
			reason := maintenanceMonitorReason("prepare-inventory-blocked:monitor-operation-unresolved", err)
			if !strings.Contains(reason, ";monitor-stage="+wantStage+";monitor-category=deadline;monitor-id="+wantID+";owner-slug="+summary.Slug) || strings.Contains(reason, "PRIVATE") {
				t.Fatal(reason)
			}
			select {
			case <-entered:
			default:
				t.Fatal("context expired before reaching the controlled native call")
			}
			t.Logf("%s: first-stage=%s first-id=%s joined-deadline=true lifecycle=%v", mode, detail.Stage, detail.MonitorID, f.reads)
			f.readHook = nil
			view, directErr := (monitoroperation.Lifecycle{Store: service.Store, Client: f}).AssessLocalSafety(context.Background(), summary.Slug, summary.Codex.ThreadID)
			if directErr != nil || view.State != "quiescent" || f.mutations != mutations || !reflect.DeepEqual(before, maintenanceMonitorProofBytes(t, m.store.Root)) {
				t.Fatal("released fixture failed direct proof or changed effects/bytes", view, directErr)
			}
		})
	}
}

func TestMaintenanceCompletedMonitorOwnerRetainsProofWithoutLaunchAuthority(t *testing.T) {
	ctx := context.Background()
	m, o, service, native, op, summary := maintenanceMonitorFixture(t)
	completeMaintenanceMonitorProof(t, service, native, op)
	state := filepath.Join(m.workspace, "work", summary.Slug, "state.md")
	if err := os.WriteFile(state, []byte("---\nlifecycle: complete\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := monitoroperation.IdentityFor(m.workspace, summary.Slug); err == nil {
		t.Fatal("completed session gained new monitor authority")
	}
	before := native.mutations
	if _, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, native); err != nil {
		t.Fatal("settled completed session blocked maintenance", err)
	}
	native.idleErr = errors.New("native utility still running")
	if _, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, native); err == nil {
		t.Fatal("completion replaced native idle proof")
	}
	native.idleErr = nil
	tx, err := service.Store.Lock(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Operation.Events[0].Delivery.Status = "submitting"
	tx.Operation.Events[0].Delivery.Options = &codex.TurnOptions{}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	if _, err := maintenanceMonitorInventory(ctx, service.Store, summary, o.Services[0].Socket, native); err == nil {
		t.Fatal("completion replaced receipt proof")
	}
	if native.mutations != before {
		t.Fatal("maintenance changed native state")
	}
}
