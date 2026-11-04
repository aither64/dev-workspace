package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type monitorPortalClient struct {
	communicationTestClient
	settings codex.ThreadSettings
}

func (c monitorPortalClient) ReadThreadSettings(context.Context, string) (codex.ThreadSettings, error) {
	return c.settings, nil
}
func (monitorPortalClient) ReadTurnState(context.Context, string) (codex.TurnState, error) {
	return codex.TurnState{}, errors.New("unexpected utility observation")
}
func (monitorPortalClient) IsPreparedSendWithOptions(string, string, string, string, codex.TurnOptions) (bool, error) {
	return false, errors.New("unexpected monitor preparation proof")
}
func (monitorPortalClient) DiscardPreparedSendWithOptions(string, string, string, string, codex.TurnOptions) (bool, error) {
	return false, errors.New("unexpected monitor preparation discard")
}

func TestMonitorPortalPreparesCurrentRetainedLeadSettings(t *testing.T) {
	s := newTestServer(t)
	s.config.Codex = monitorPortalClient{settings: codex.ThreadSettings{Model: "saved-model", ReasoningEffort: "xhigh"}}
	const slug, root = "2026-11-03-monitor", "retained-lead"
	dir := filepath.Join(s.config.Workspace, "work", slug)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"plan.md": "Monitoring fixture\n", "state.md": "---\nlifecycle: active\n---\n",
		"portal.yml": fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n", slug, root),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), slug, root, true, func(r *teamruntime.Roster) error {
		r.PresetID, r.CatalogDigest, r.TeamDigest = "solo", strings.Repeat("a", 64), strings.Repeat("b", 64)
		r.LeadModel, r.LeadEffort = "older-model", "high"
		r.LeadInstructions = "Preserve the current lead's task and constraints."
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	owner := monitoroperation.Owner{Slug: slug, RootThreadID: root}
	options, err := s.prepareMonitorLead(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if options.Model != "saved-model" || options.ReasoningEffort != "xhigh" || !strings.Contains(options.ThreadPolicy.DeveloperInstructions, "Preserve the current lead's task and constraints.") {
		t.Fatal("monitor completion changed the lead's settings or instructions")
	}
	service, err := s.monitorDeliveryService()
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := service.LockSession(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if lock, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, slug); err == nil {
		lock.Close()
		unlock()
		t.Fatal("monitor dispatch did not exclude lifecycle admission")
	}
	unlock()
	lock, err := session.LockRuntimeExclusiveContext(context.Background(), s.config.AuthorityDir, slug)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestMonitorPortalWorkerClosesWithoutBrowserOrNativeActivation(t *testing.T) {
	s := newTestServer(t)
	s.config.Codex = monitorPortalClient{}
	s.startMonitorOperations()
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor worker ignored portal cancellation")
	}
}

type monitorWorkerLaunchClient struct {
	monitoroperation.LaunchClient
	projects map[string]string
	threads  map[string]codex.ThreadMetadata
}

func (c *monitorWorkerLaunchClient) CreateProject(_ context.Context, name, key string) (codex.ProjectMetadata, error) {
	c.projects[key] = name
	return codex.ProjectMetadata{ID: key, Name: name}, nil
}
func (c *monitorWorkerLaunchClient) ReadProject(_ context.Context, id string) (codex.ProjectMetadata, error) {
	return codex.ProjectMetadata{ID: id, Name: c.projects[id]}, nil
}
func (c *monitorWorkerLaunchClient) StartThreadWithSettings(_ context.Context, cwd string, _ map[string]string, settings codex.ThreadSettings) (string, error) {
	id, project := "utility-"+settings.ProjectID, settings.ProjectID
	c.threads[id] = codex.ThreadMetadata{ID: id, Cwd: cwd, ProjectID: &project}
	return id, nil
}
func (c *monitorWorkerLaunchClient) ReadThreadMetadata(_ context.Context, id string, _ bool) (codex.ThreadMetadata, error) {
	return c.threads[id], nil
}
func (*monitorWorkerLaunchClient) BootstrapHeadlessThread(context.Context, string, string, string, string) error {
	return nil
}
func (*monitorWorkerLaunchClient) SendWithOptions(_ context.Context, thread, _, id, _ string, _ codex.TurnOptions) (codex.SendReceipt, error) {
	return codex.SendReceipt{ClientUserMessageID: id, TurnID: "turn-" + thread}, nil
}
func (*monitorWorkerLaunchClient) ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{}, false, nil
}

type monitorWorkerTransport struct {
	blockedSlug     string
	blocked         bool
	blockedErr      error
	blockedDeadline time.Time
	ready           []string
	prepared        []string
	sends           int
	sentSlug        string
	sentEventID     string
	sentMessageID   string
}

func (f *monitorWorkerTransport) Ready(ctx context.Context, owner monitoroperation.Owner) (string, error) {
	f.ready = append(f.ready, owner.Slug)
	if owner.Slug == f.blockedSlug {
		f.blocked = true
		f.blockedDeadline, _ = ctx.Deadline()
		<-ctx.Done()
		f.blockedErr = ctx.Err()
		return "", f.blockedErr
	}
	return "", nil
}
func (f *monitorWorkerTransport) Prepare(_ context.Context, owner monitoroperation.Owner) (codex.TurnOptions, error) {
	f.prepared = append(f.prepared, owner.Slug)
	return codex.TurnOptions{}, nil
}
func (f *monitorWorkerTransport) Send(_ context.Context, owner monitoroperation.Owner, event monitoroperation.Event) (codex.SendReceipt, error) {
	f.sends++
	f.sentSlug, f.sentEventID, f.sentMessageID = owner.Slug, event.ID, event.Delivery.MessageID
	return codex.SendReceipt{ClientUserMessageID: event.Delivery.MessageID, TurnID: "accepted-lead-turn"}, nil
}
func (*monitorWorkerTransport) Reconcile(context.Context, monitoroperation.Owner, monitoroperation.Event) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{}, false, errors.New("unexpected receipt reconciliation")
}

type monitorWorkerObserver struct {
	blockedOperation string
	blocked          bool
	blockedErr       error
	blockedDeadline  time.Time
	observed         []string
}

func (f *monitorWorkerObserver) ObserveUtility(ctx context.Context, op monitoroperation.Operation) (string, bool, error) {
	f.observed = append(f.observed, op.Owner.Slug)
	if op.ID == f.blockedOperation {
		f.blocked = true
		f.blockedDeadline, _ = ctx.Deadline()
		<-ctx.Done()
		f.blockedErr = ctx.Err()
		return "", false, f.blockedErr
	}
	return "", false, nil
}

func TestMonitorPortalWorkerDeliversPastBlockedOperation(t *testing.T) {
	for _, blockedStage := range []string{"observation", "dispatch"} {
		t.Run(blockedStage, func(t *testing.T) {
			s := newTestServer(t)
			store, err := monitoroperation.NewStore(s.config.UserStateRoot, s.config.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			owners := map[string]monitoroperation.Owner{}
			client := &monitorWorkerLaunchClient{projects: map[string]string{}, threads: map[string]codex.ThreadMetadata{}}
			launch := &monitoroperation.Service{Store: store, Client: client,
				ResolveLead: func(_ context.Context, caller string) (monitoroperation.Owner, error) { return owners[caller], nil },
				Resolve:     func(slug string) (monitoroperation.Owner, error) { return owners[slug], nil },
				LockSession: func(context.Context, string) (func(), error) { return func() {}, nil },
				Policy: func(context.Context, monitoroperation.Owner) (monitoroperation.UtilityPolicy, error) {
					return monitoroperation.UtilityPolicy{PackageRoot: s.config.Workspace, CatalogDigest: strings.Repeat("a", 64), NativeName: "watcher", Model: "gpt-6-luna", Effort: "low", Instructions: "Observe only", Sandbox: "danger-full-access", ApprovalPolicy: "never", MaxConcurrent: 1}, nil
				},
				Admit:    func(context.Context, monitoroperation.Owner) error { return nil },
				Activate: func(context.Context, monitoroperation.Owner, string) error { return nil },
			}
			var ops []monitoroperation.Operation
			for _, letter := range []string{"a", "b"} {
				owner := monitoroperation.Owner{Slug: "fixture-" + letter, RootThreadID: "lead-" + letter, Incarnation: "incarnation-" + letter, OriginTurnID: "origin", Socket: filepath.Join(s.config.Workspace, "native.sock"), SocketEpoch: "epoch", RuntimeSchema: 1}
				owners[owner.Slug], owners[owner.RootThreadID] = owner, owner
				id := strings.Repeat(letter, 32)
				op, err := launch.Start(context.Background(), owner.RootThreadID, monitoroperation.StartRequest{ID: id, Brief: "Observe this disposable fixture", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture-check"}, Cwd: s.config.Workspace, InputIdentity: "fixture-input", Revision: "fixture-revision", InvocationID: id}})
				if err != nil {
					t.Fatal(err)
				}
				if letter == "b" || blockedStage == "dispatch" {
					exit := 0
					op, err = launch.Report(context.Background(), op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exit})
					if err != nil {
						t.Fatal(err)
					}
				}
				ops = append(ops, op)
			}
			transport, observer := &monitorWorkerTransport{}, &monitorWorkerObserver{}
			if blockedStage == "observation" {
				observer.blockedOperation = ops[0].ID
			} else {
				transport.blockedSlug = ops[0].Owner.Slug
			}
			service := &monitoroperation.DeliveryService{Store: store, Resolve: launch.Resolve, LockSession: launch.LockSession, Transport: transport, Observer: observer}
			nextOperation, nextEvent := 0, 0
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			logPassOnFailure := func(pass string, started time.Time, batchErr error, savedA, savedB monitoroperation.Operation, errA, errB error) {
				statuses := [2][]string{}
				for i, op := range []monitoroperation.Operation{savedA, savedB} {
					for _, event := range op.Events {
						statuses[i] = append(statuses[i], event.Delivery.Status)
					}
				}
				details := fmt.Sprintf("pass=%s batchErr=%v parentErr=%v elapsed=%s operations=%d events=%d ready=%v prepared=%v observed=%v sends=%d sent=%s/%s/%s blockedErr=%v blockedDeadline=%s observerErr=%v observerDeadline=%s A=%v AreadErr=%v B=%v BreadErr=%v", pass, batchErr, ctx.Err(), time.Since(started), nextOperation, nextEvent, transport.ready, transport.prepared, observer.observed, transport.sends, transport.sentSlug, transport.sentEventID, transport.sentMessageID, transport.blockedErr, transport.blockedDeadline, observer.blockedErr, observer.blockedDeadline, statuses[0], errA, statuses[1], errB)
				t.Cleanup(func() {
					if t.Failed() {
						t.Log(details)
					}
				})
			}
			checkObservationCursor := func(before int, visits []string) {
				if len(visits) > len(ops) || nextOperation != before+len(visits) {
					t.Fatalf("observation cursor does not match callbacks: before=%d after=%d visits=%v", before, nextOperation, visits)
				}
				for i, slug := range visits {
					if slug != ops[(before+i)%len(ops)].Owner.Slug {
						t.Fatalf("observation order changed: before=%d visits=%v", before, visits)
					}
				}
			}
			if blockedStage == "observation" {
				started := time.Now()
				batchErr := s.runMonitorOperations(ctx, service, &nextOperation, &nextEvent, 250*time.Millisecond, 50*time.Millisecond)
				saved, err := store.Get(ctx, ops[1].ID)
				logPassOnFailure("observation", started, batchErr, monitoroperation.Operation{}, saved, nil, err)
				if err != nil {
					t.Fatal(err)
				}
				if !observer.blocked || !errors.Is(observer.blockedErr, context.DeadlineExceeded) {
					t.Fatal("fixture did not cancel the blocked observer")
				}
				if len(saved.Events) != 1 || saved.Events[0].Delivery.Status != "accepted" || saved.Events[0].Delivery.MessageID != ops[1].Events[0].Delivery.MessageID || transport.sends != 1 {
					t.Fatalf("blocked observation starved or duplicated persisted outcome B, sends=%d", transport.sends)
				}
				if nextOperation != 2 || nextEvent != 1 || !reflect.DeepEqual(transport.ready, []string{ops[1].Owner.Slug}) {
					t.Fatalf("cursor did not advance past blocked item: operations=%d events=%d", nextOperation, nextEvent)
				}
				checkObservationCursor(0, observer.observed)
				return
			}

			// The worker retains its cursors across bounded passes. Expire A's
			// delivery batch so the next pass must start with untouched B.
			started := time.Now()
			batchErr := s.runMonitorOperations(ctx, service, &nextOperation, &nextEvent, 50*time.Millisecond, 50*time.Millisecond)
			saved, err := store.Get(ctx, ops[1].ID)
			savedA, errA := store.Get(ctx, ops[0].ID)
			logPassOnFailure("dispatch1", started, batchErr, savedA, saved, errA, err)
			if batchErr != nil || ctx.Err() != nil {
				t.Fatalf("dispatch pass1 failed: batchErr=%v parentErr=%v", batchErr, ctx.Err())
			}
			if err != nil || errA != nil {
				t.Fatalf("dispatch pass1 snapshot failed: A=%v B=%v", errA, err)
			}
			if !transport.blocked || !errors.Is(transport.blockedErr, context.DeadlineExceeded) || !reflect.DeepEqual(transport.ready, []string{ops[0].Owner.Slug}) || len(transport.prepared) != 0 || transport.sends != 0 || nextEvent != 1 {
				t.Fatal("dispatch pass1 did not yield after blocked A without attempting B")
			}
			if len(savedA.Events) != 1 || savedA.Events[0].Delivery.Status != "held" || len(saved.Events) != 1 || saved.Events[0].Delivery.Status != "pending" || !reflect.DeepEqual(saved.Events, ops[1].Events) {
				t.Fatal("dispatch pass1 did not persist held A and untouched original B")
			}
			checkObservationCursor(0, observer.observed)

			beforeOperation, beforeEvent := nextOperation, nextEvent
			beforeReady, beforeObserved := len(transport.ready), len(observer.observed)
			started = time.Now()
			batchErr = s.runMonitorOperations(ctx, service, &nextOperation, &nextEvent, 250*time.Millisecond, 50*time.Millisecond)
			final, finalErr := store.Get(ctx, ops[1].ID)
			logPassOnFailure("dispatch2", started, batchErr, savedA, final, errA, finalErr)
			if batchErr != nil || ctx.Err() != nil {
				t.Fatalf("dispatch pass2 failed: batchErr=%v parentErr=%v", batchErr, ctx.Err())
			}
			ready := transport.ready[beforeReady:]
			if len(ready) == 0 || len(ready) > 2 || ready[0] != ops[1].Owner.Slug || (len(ready) == 2 && ready[1] != ops[0].Owner.Slug) || nextEvent != beforeEvent+len(ready) {
				t.Fatalf("dispatch pass2 did not resume B first with retained cursor: before=%d after=%d ready=%v", beforeEvent, nextEvent, ready)
			}
			if !errors.Is(transport.blockedErr, context.DeadlineExceeded) || !reflect.DeepEqual(transport.prepared, []string{ops[1].Owner.Slug}) || transport.sends != 1 || transport.sentSlug != ops[1].Owner.Slug || transport.sentEventID != ops[1].Events[0].ID || transport.sentMessageID != ops[1].Events[0].Delivery.MessageID {
				t.Fatal("dispatch pass2 did not send B's original event exactly once without sending A")
			}
			if finalErr != nil {
				t.Fatal(finalErr)
			}
			if len(final.Events) != 1 || final.Events[0].Delivery.Status != "accepted" || final.Events[0].Delivery.MessageID != saved.Events[0].Delivery.MessageID || final.Events[0].Delivery.Receipt == nil || final.Events[0].Delivery.Receipt.ClientUserMessageID != saved.Events[0].Delivery.MessageID || final.Events[0].Delivery.Receipt.TurnID != "accepted-lead-turn" {
				t.Fatal("dispatch pass2 did not persist the original B acceptance receipt")
			}
			checkObservationCursor(beforeOperation, observer.observed[beforeObserved:])
		})
	}
}
