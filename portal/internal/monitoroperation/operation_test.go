package monitoroperation

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
)

type launchFixtureClient struct {
	t                                                        *testing.T
	store                                                    *Store
	opID                                                     string
	project                                                  string
	thread                                                   string
	creates, starts, bootstraps, verifies, sends, reconciles int
	lostStart, lostBootstrap, hiddenThread, lostSend         bool
	lostProject                                              bool
	projectKeys                                              []string
	accepted                                                 bool
}

func (f *launchFixtureClient) CreateProject(_ context.Context, name, key string) (codex.ProjectMetadata, error) {
	f.creates++
	f.projectKeys = append(f.projectKeys, key)
	f.project = key
	if f.lostProject {
		return codex.ProjectMetadata{}, errors.New("lost project response")
	}
	return codex.ProjectMetadata{ID: key, Name: name}, nil
}
func (f *launchFixtureClient) ReadProject(_ context.Context, id string) (codex.ProjectMetadata, error) {
	return codex.ProjectMetadata{ID: id, Name: "monitor " + f.opID}, nil
}
func (f *launchFixtureClient) ListThreads(context.Context, codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error) {
	if f.hiddenThread {
		return nil, nil, nil
	}
	m, _ := f.ReadThreadMetadata(context.Background(), f.thread, false)
	return []codex.ThreadMetadata{m}, nil, nil
}
func (f *launchFixtureClient) StartThreadWithSettings(_ context.Context, cwd string, _ map[string]string, settings codex.ThreadSettings) (string, error) {
	f.starts++
	f.thread = fmt.Sprintf("utility-%d", f.starts)
	op := f.durable()
	if !op.Launch.CreateAttempted || op.Launch.ProjectID != settings.ProjectID || settings.Model != "gpt-6-luna" || settings.ReasoningEffort != "low" || settings.Policy.ApprovalPolicy != "never" {
		f.t.Fatal("start preceded durable intent or changed utility policy")
	}
	if f.lostStart {
		return "", errors.New("lost start response")
	}
	return f.thread, nil
}
func (f *launchFixtureClient) ReadThreadMetadata(_ context.Context, id string, _ bool) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{ID: id, Cwd: f.store.workspace + "/work/monitor-test", ProjectID: &f.project}, nil
}
func (f *launchFixtureClient) BootstrapHeadlessThread(context.Context, string, string, string, string) error {
	f.bootstraps++
	if !f.durable().Launch.BootstrapAttempted {
		f.t.Fatal("bootstrap preceded durable intent")
	}
	if f.lostBootstrap {
		return errors.New("lost bootstrap response")
	}
	return nil
}
func (f *launchFixtureClient) VerifyHeadlessBootstrap(context.Context, string, string, string, string) error {
	f.verifies++
	return nil
}
func (f *launchFixtureClient) SendWithOptions(_ context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
	f.sends++
	op := f.durable()
	if op.Launch.ActivatedEpoch != op.Owner.SocketEpoch || op.Launch.State != "submitting" || op.Launch.ThreadID != thread {
		f.t.Fatal("send preceded durable exact activation")
	}
	f.accepted = true
	if f.lostSend {
		return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: errors.New("lost send response")}
	}
	return codex.SendReceipt{ClientUserMessageID: id, TurnID: "utility-turn"}, nil
}
func (f *launchFixtureClient) ReconcileSendWithOptions(_ context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, bool, error) {
	f.reconciles++
	return codex.SendReceipt{ClientUserMessageID: id, TurnID: "utility-turn"}, f.accepted, nil
}
func (f *launchFixtureClient) durable() Operation {
	path, _ := f.store.path(f.opID)
	op, err := f.store.read(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return op
}

func operationFixture(t *testing.T) (*Service, *launchFixtureClient, StartRequest, *Owner) {
	t.Helper()
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner := &Owner{Slug: "monitor-test", RootThreadID: "lead", Incarnation: "incarnation", OriginTurnID: "origin", Socket: store.workspace + "/native.sock", SocketEpoch: "epoch", RuntimeSchema: 1}
	req := StartRequest{ID: strings.Repeat("1", 32), Brief: "Run only the exact check", Source: Source{Kind: "local", Argv: []string{"check", "--exact"}, Cwd: store.workspace, InputIdentity: "inputs", Revision: "revision", InvocationID: strings.Repeat("1", 32)}}
	f := &launchFixtureClient{t: t, store: store, opID: req.ID}
	s := &Service{Store: store, Client: f, ResolveLead: func(_ context.Context, caller string) (Owner, error) {
		if caller != owner.RootThreadID {
			return Owner{}, errors.New("wrong caller")
		}
		return *owner, nil
	}, Resolve: func(string) (Owner, error) { return *owner, nil }, LockSession: func(context.Context, string) (func(), error) { return func() {}, nil }, Policy: func(context.Context, Owner) (UtilityPolicy, error) {
		return UtilityPolicy{PackageRoot: store.workspace, CatalogDigest: strings.Repeat("a", 64), NativeName: "native-watcher", Model: "gpt-6-luna", Effort: "low", Instructions: "Observe only", Sandbox: "danger-full-access", ApprovalPolicy: "never", MaxConcurrent: 1}, nil
	}, Admit: func(context.Context, Owner) error { return nil }, Activate: func(context.Context, Owner, string) error { return nil }, CheckResolution: func(context.Context, Operation) error { return nil }}
	return s, f, req, owner
}

func TestOperationLaunchRecoversLostAcknowledgementsWithFrozenIdentity(t *testing.T) {
	for _, stage := range []string{"project", "start", "bootstrap", "send"} {
		t.Run(stage, func(t *testing.T) {
			s, f, req, _ := operationFixture(t)
			f.lostStart = stage == "start"
			f.lostProject = stage == "project"
			f.lostBootstrap = stage == "bootstrap"
			f.lostSend = stage == "send"
			first, err := s.Start(context.Background(), "lead", req)
			if stage == "start" && err != nil {
				t.Fatal(err)
			}
			if stage != "start" && err == nil {
				t.Fatal("expected uncertain acknowledgement")
			}
			if first.ID != req.ID {
				t.Fatal("lost operation identity")
			}
			// A package selection change must not replace the saved utility policy.
			s.Policy = func(context.Context, Owner) (UtilityPolicy, error) {
				t.Fatal("retry resolved a new policy")
				return UtilityPolicy{}, nil
			}
			f.lostStart, f.lostBootstrap, f.lostSend = false, false, false
			f.lostProject = false
			op, err := s.Start(context.Background(), "lead", req)
			if err != nil {
				t.Fatal(err)
			}
			if op.Launch.Receipt == nil || op.Launch.Receipt.TurnID != "utility-turn" || f.starts != 1 || f.bootstraps != 1 || f.sends != 1 {
				t.Fatalf("repeated launch effects: %#v starts=%d bootstraps=%d sends=%d", op.Launch, f.starts, f.bootstraps, f.sends)
			}
			if len(f.projectKeys) == 2 && f.projectKeys[0] != f.projectKeys[1] {
				t.Fatal("lost project acknowledgement allocated another idempotency key")
			}
		})
	}
}

func TestOperationUnknownStartRetainsCapacityAndNeverRepeats(t *testing.T) {
	s, f, req, _ := operationFixture(t)
	f.lostStart, f.hiddenThread = true, true
	for i := 0; i < 2; i++ {
		if _, err := s.Start(context.Background(), "lead", req); err == nil {
			t.Fatal("unknown start accepted")
		}
	}
	if f.starts != 1 || f.sends != 0 {
		t.Fatal("unknown start was repeated")
	}
	status, err := s.Status(context.Background(), "lead", req.ID)
	if err != nil || status.Launch.Detail == "" || f.starts != 1 || f.bootstraps != 0 || f.sends != 0 {
		t.Fatal("uncertain status hid its record or repeated work")
	}
	req.ID = strings.Repeat("2", 32)
	req.Source.InvocationID = req.ID
	if _, err := s.Start(context.Background(), "lead", req); err == nil {
		t.Fatal("uncertain utility released concurrency")
	}
	view, err := s.Store.Inventory(context.Background(), "monitor-test", "lead")
	if err != nil || len(view.Unresolved) != 1 {
		t.Fatalf("missing uncertain inventory: %#v %v", view, err)
	}
}

func TestOperationReportsAreCallerBoundAndSlotsImmutable(t *testing.T) {
	s, f, req, _ := operationFixture(t)
	op, err := s.Start(context.Background(), "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	exit := 0
	report := ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exit, Evidence: []string{"log/exact"}}
	if _, err := s.Report(context.Background(), "lead", report); err == nil {
		t.Fatal("root impersonated utility")
	}
	if _, err := s.Report(context.Background(), "foreign-utility", report); err == nil {
		t.Fatal("unknown utility accepted")
	}
	first, err := s.Report(context.Background(), op.Launch.ThreadID, report)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Report(context.Background(), op.Launch.ThreadID, report)
	if err != nil || !reflect.DeepEqual(first.Events, again.Events) {
		t.Fatal("same report did not retain its event")
	}
	report.Evidence = []string{"changed"}
	if _, err := s.Report(context.Background(), op.Launch.ThreadID, report); err == nil {
		t.Fatal("conflicting slot accepted")
	}
	if f.sends != 1 {
		t.Fatal("report executed another command")
	}
	// A final report alone does not prove the utility has stopped.
	req.ID = strings.Repeat("3", 32)
	req.Source.InvocationID = req.ID
	if _, err := s.Start(context.Background(), "lead", req); err == nil {
		t.Fatal("report alone released native capacity")
	}
}

func TestOperationCanceledStatusReconcilesWithoutNewEffects(t *testing.T) {
	s, f, req, _ := operationFixture(t)
	f.lostSend = true
	if _, err := s.Start(context.Background(), "lead", req); err == nil {
		t.Fatal("lost send reply accepted")
	}
	if _, err := s.Cancel(context.Background(), "lead", req.ID); err != nil {
		t.Fatal(err)
	}
	op, err := s.Status(context.Background(), "lead", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Launch.Receipt == nil || op.CanceledAt == nil || f.starts != 1 || f.bootstraps != 1 || f.sends != 1 {
		t.Fatal("canceled status reran native effects or lost proof")
	}
}

func TestOperationResolutionRequiresProofAndPreservesOutcome(t *testing.T) {
	s, f, req, owner := operationFixture(t)
	op, err := s.Start(context.Background(), "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.Report(context.Background(), op.Launch.ThreadID, ReportRequest{Slot: "terminal", Outcome: "incomplete", Detail: "Command effects remain unknown", StillRunning: "exact process identity"})
	if err != nil {
		t.Fatal(err)
	}
	result := ResolutionRequest{OperationID: req.ID, ResolutionID: strings.Repeat("4", 32), Evidence: []string{"exact-process-exit-proof"}, Detail: "Original command is accounted for"}
	if _, err := s.ResolveEffects(context.Background(), "foreign", result); err == nil {
		t.Fatal("wrong owner resolved operation")
	}
	s.CheckResolution = func(context.Context, Operation) error { return errors.New("native still active or receipt unknown") }
	if _, err := s.ResolveEffects(context.Background(), "lead", result); err == nil {
		t.Fatal("resolution bypassed native proof")
	}
	s.CheckResolution = func(context.Context, Operation) error { return nil }
	resolved, err := s.ResolveEffects(context.Background(), "lead", result)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resolution == nil || HasUnresolvedEffects(resolved) || resolved.Events[0].Report.Detail != op.Events[0].Report.Detail || !reflect.DeepEqual(resolved.Launch.Receipt, op.Launch.Receipt) {
		t.Fatal("resolution changed original outcome/receipt")
	}
	again, err := s.ResolveEffects(context.Background(), "lead", result)
	if err != nil || !reflect.DeepEqual(again.Resolution, resolved.Resolution) {
		t.Fatal("resolution retry changed immutable acknowledgement")
	}
	result.Detail = "different"
	if _, err := s.ResolveEffects(context.Background(), "lead", result); err == nil {
		t.Fatal("conflicting resolution accepted")
	}
	owner.Incarnation = "replacement"
	if _, err := s.ResolveEffects(context.Background(), "lead", result); err == nil {
		t.Fatal("replacement incarnation resolved original")
	}
	if f.sends != 1 || f.starts != 1 {
		t.Fatal("resolution caused native effects")
	}
}

func TestOperationStoreRejectsMutationAndKeepsArchivingOwnership(t *testing.T) {
	s, _, req, _ := operationFixture(t)
	op, err := s.Start(context.Background(), "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.Lock(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Operation.Source.Revision = "different"
	if err := tx.Save(); err == nil {
		t.Fatal("immutable source changed")
	}
	tx.Close()
	op2, err := s.Store.Get(context.Background(), req.ID)
	if err != nil || op2.Source.Revision != op.Source.Revision {
		t.Fatal("closing transaction committed an unvalidated mutation")
	}
	tx, _ = s.Store.Lock(context.Background(), req.ID)
	tx.Operation.Disposition = "archiving"
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	view, err := s.Store.Inventory(context.Background(), op.Owner.Slug, op.Owner.RootThreadID)
	if err != nil || len(view.Threads) != 1 || len(view.Unresolved) == 0 {
		t.Fatalf("archiving hid ownership: %#v %v", view, err)
	}
	path, _ := s.Store.path(req.ID)
	valid, _ := json.Marshal(op2)
	duplicate := strings.Replace(string(valid), `"schema":1`, `"schema":1,"schema":1`, 1)
	if err := os.WriteFile(path, []byte(duplicate), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Get(context.Background(), req.ID); err == nil {
		t.Fatal("duplicate state ownership keys accepted")
	}
	if err := os.WriteFile(path, []byte(`{"schema":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Inventory(context.Background(), op.Owner.Slug, op.Owner.RootThreadID); err == nil {
		t.Fatal("malformed state became an empty inventory")
	}
}

func TestOperationTerminalCompletionSupersedesEscalationEffects(t *testing.T) {
	s, _, req, _ := operationFixture(t)
	op, err := s.Start(context.Background(), "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddEvent(&op, ReportRequest{Slot: "escalation", Outcome: "incomplete", Detail: "Slow check", StillRunning: "run-attempt-1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !HasUnresolvedEffects(op) {
		t.Fatal("live escalation lost effects fence")
	}
	exit := 1
	if err := AddEvent(&op, ReportRequest{Slot: "terminal", Outcome: "failed", ExitStatus: &exit}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if HasUnresolvedEffects(op) {
		t.Fatal("historical escalation blocks authoritative terminal result")
	}
}

func TestOperationAdmissionDoesNotWaitForUnrelatedNativeTransaction(t *testing.T) {
	s, _, req, _ := operationFixture(t)
	op, err := s.Start(context.Background(), "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.Lock(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	other := cloneOperation(op)
	other.ID = strings.Repeat("5", 32)
	other.Source.InvocationID = other.ID
	other.Owner.Slug = "another-monitor"
	other.RequestDigest = digest(StartRequest{ID: other.ID, Brief: other.Brief, Source: other.Source, CancellationScope: other.CancellationScope})
	other.Launch = initialLaunch(other)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.Store.reserve(ctx, other); err != nil {
		t.Fatalf("workspace admission waited for unrelated operation RPC: %v", err)
	}
}
