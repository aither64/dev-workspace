package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func newPiScheduledFixture(t *testing.T) (*piWorkflowComposition, *piObservedFixture, agentbackend.ConversationRef) {
	t.Helper()
	s, receipt, native := newPiCreationFixtureForSlug(t, "2026-11-23-scheduled")
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	fake := &piObservedFixture{piCreationRuntimeFixture: native, idle: true, current: native.runtime}
	s.config.PiCreation.Runtime = fake
	w := &piWorkflowComposition{server: s, runtime: fake, tools: []string{"publish_plan", "request_user_input_async"}}
	w.observations.initialize()
	s.piWorkflows = w
	ref := native.state.Conversation
	ready := make(chan struct{})
	close(ready)
	w.observations.attachments[ref.Session] = &piWorkflowAttachment{runtime: native.runtime, attachment: "attachment", sequence: 2, initial: 1, connected: true, readyClosed: true, ready: ready, finished: make(chan struct{}), cancel: func() {}}
	w.observations.counters[ref] = 0
	return w, fake, ref
}

func mustPiForeground(t *testing.T, s *Server, ref agentbackend.SessionRef) func() {
	t.Helper()
	release, err := s.acquirePiForeground(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func TestPiWorkflowEmptyDrainAndReconciliationPreserveFreshResult(t *testing.T) {
	w, fake, ref := newPiScheduledFixture(t)
	ctx := context.Background()
	questions, _ := w.stores(ref)
	// First exercise a missing store, then a complete persisted Accepted record.
	for _, state := range []string{"empty", "Accepted"} {
		if state == "Accepted" {
			caller := piquestions.Caller{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: fake.runtime.Generation}, Lineage: piquestions.LineageRef{AnchorEntryID: "anchor", SelectionToken: "linear-v1:" + strings.Repeat("e", 64)}, NativeToolCallID: "question"}
			q, err := questions.Publish(ctx, caller, piquestions.PublishInput{Questions: []piquestions.Question{{Title: "Question"}}})
			if err != nil {
				t.Fatal(err)
			}
			in := piquestions.ResponseInput{ResponseID: piquestions.UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []piquestions.Answer{{QuestionID: "0", Text: "answer"}}}
			if _, err := questions.AcceptResponse(ctx, ref, in); err != nil {
				t.Fatal(err)
			}
			file := piquestions.StateFile{Root: w.server.config.UserStateRoot, Kind: "pi-questions", Name: "questions.json", Ref: ref}
			var record piquestions.Record
			if err := file.Update(ctx, &record, func() error {
				record.Questions[0].Responses[0].Delivery.Phase = "Accepted"
				record.Questions[0].Responses[0].Delivery.ReceiptRef = "original"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.refresh(ctx, ref); err != nil {
			t.Fatal(err)
		}
		entry := w.observations.entries[ref]
		observed := fake.observations
		lock, err := session.LockRuntimeSharedContext(ctx, w.server.config.AuthorityDir, ref.Session.Slug)
		if err != nil {
			t.Fatal(err)
		}
		budget, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		err = w.drain(budget, ref)
		cancel()
		lock.Close()
		if err != nil {
			t.Fatalf("%s drain requested exclusive ownership: %v", state, err)
		}
		for range 3 {
			if err := w.reconcileOriginal(ctx, ref); err != nil {
				t.Fatal(err)
			}
		}
		if fake.observations != observed || fake.checked != 0 {
			t.Fatal("no-op work called native proof or Send")
		}
		current, present := w.observations.entries[ref]
		if !present || current.counter != entry.counter {
			t.Fatal("no-op work revoked fresh result")
		}
		if _, pending := w.observations.requested[ref]; pending {
			t.Fatal("no-op work manufactured demand")
		}
	}
}

func TestPiWorkflowFailedDemandBackoffAndRecovery(t *testing.T) {
	w, fake, ref := newPiScheduledFixture(t)
	now := time.Unix(1000, 0)
	w.observations.now = func() time.Time { return now }
	fake.before = func() {
		for range 50 {
			w.requestObservation(ref, false)
		}
		w.invalidateRef(ref, true)
	}
	w.requestObservation(ref, true)
	for attempt, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		w.refreshRequested(context.Background())
		if fake.observations != attempt+1 {
			t.Fatal("unexpected proof count", fake.observations)
		}
		deadline := now.Add(delay)
		for range 100 {
			w.requestObservation(ref, false)
			w.invalidateRef(ref, true)
			w.refreshRequested(context.Background())
		}
		if fake.observations != attempt+1 || !w.observations.refreshes[ref].retryAt.Equal(deadline) {
			t.Fatal("GET/event churn reset failure backoff", fake.observations, w.observations.refreshes[ref].retryAt)
		}
		now = deadline
	}
	fake.before = nil
	w.refreshRequested(context.Background())
	if _, present := w.observations.entries[ref]; !present {
		t.Fatal("background proof did not recover")
	}
	if w.observations.refreshes[ref].failures != 0 {
		t.Fatal("success retained backoff")
	}
	if _, pending := w.observations.requested[ref]; pending {
		t.Fatal("empty delivery retained demand after recovery")
	}
}

func TestPiWorkflowForegroundCancelsReadsAndYieldsBetweenConversations(t *testing.T) {
	w, fake, ref := newPiScheduledFixture(t)
	var release func()
	fake.before = func() { release = mustPiForeground(t, w.server, ref.Session) }
	w.requestObservation(ref, true)
	w.refreshRequested(context.Background())
	if release == nil {
		t.Fatal("fixture did not reach native proof")
	}
	for range 20 {
		w.refreshRequested(context.Background())
	}
	if fake.observations != 1 || fake.checked != 0 {
		t.Fatal("background started while foreground queued")
	}
	if _, present := w.observations.entries[ref]; present {
		t.Fatal("canceled proof published")
	}
	if !w.observations.requested[ref] {
		t.Fatal("foreground yield lost delivery demand")
	}
	fake.before = nil
	release()
	release()
	w.refreshRequested(context.Background())
	if fake.observations != 2 {
		t.Fatal("background did not resume after release")
	}
	if len(w.observations.foreground) != 0 {
		t.Fatal("foreground release was not idempotent")
	}

	// The round-robin chooser does not let repeated demand for the first ref
	// overtake an already pending sibling, or block a different session.
	other := ref
	other.Member, other.Incarnation = "implementer0", strings.Repeat("f", 64)
	w.requestObservation(ref, false)
	w.requestObservation(other, false)
	first, _, _, cancel, ok := w.nextRefresh(context.Background())
	if !ok {
		t.Fatal("missing first attempt")
	}
	cancel()
	w.requestObservation(first, true)
	w.finishRefresh(first, nil, false, false)
	next, _, _, cancel, ok := w.nextRefresh(context.Background())
	if !ok || next == first {
		t.Fatal("one conversation starved its sibling")
	}
	cancel()
	w.finishRefresh(next, nil, false, false)
	foreign := ref
	foreign.Session.Slug = "2026-11-23-other"
	w.requestObservation(foreign, false)
	release = mustPiForeground(t, w.server, ref.Session)
	got, _, _, cancel, ok := w.nextRefresh(context.Background())
	if !ok || got != foreign {
		t.Fatal("foreground session blocked another session")
	}
	cancel()
	w.finishRefresh(got, nil, false, false)
	release()
}

func TestPiWorkflowForegroundResolverReleasesOnFailureAndTargetClose(t *testing.T) {
	w, fake, ref := newPiScheduledFixture(t)
	ctx := context.WithValue(context.Background(), piConversationScopeKey{}, piWorkflowNamespace(ref))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := w.server.resolvePiBackend(canceled, conversation.ResolveRequest{ID: ref.Session.Slug, Mutation: true, Operation: "message"})
	if !errors.Is(err, context.Canceled) || len(w.observations.foreground) != 0 {
		t.Fatal("failed resolver leaked foreground", err)
	}
	// Use the ordinary neutral fixture for a successful mutation target.
	w.server.config.PiCreation.Runtime = &piNeutralFixture{piCreationRuntimeFixture: fake.piCreationRuntimeFixture}
	target, err := w.server.resolvePiBackend(ctx, conversation.ResolveRequest{ID: ref.Session.Slug, Mutation: true, Operation: "message"})
	if err != nil {
		t.Fatal(err)
	}
	if w.observations.foreground[ref.Session] != 1 {
		t.Fatal("foreground ended before target release")
	}
	target.Release()
	target.Release()
	if len(w.observations.foreground) != 0 {
		t.Fatal("target release leaked foreground")
	}
}

type piBackgroundAdmissionWait struct {
	context.Context
	waiting chan struct{}
}

func (c piBackgroundAdmissionWait) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestPiWorkflowForegroundCancelsWaitingDrainButPreservesAdmittedEffect(t *testing.T) {
	w, fake, ref := newPiScheduledFixture(t)
	ctx := context.Background()
	questions, _ := w.stores(ref)
	caller := piquestions.Caller{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: fake.runtime.Generation}, Lineage: piquestions.LineageRef{AnchorEntryID: "anchor", SelectionToken: "linear-v1:" + strings.Repeat("e", 64)}, NativeToolCallID: "question"}
	q, err := questions.Publish(ctx, caller, piquestions.PublishInput{Questions: []piquestions.Question{{Title: "Question"}}})
	if err != nil {
		t.Fatal(err)
	}
	in := piquestions.ResponseInput{ResponseID: piquestions.UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []piquestions.Answer{{QuestionID: "0", Text: "answer"}}}
	if _, err := questions.AcceptResponse(ctx, ref, in); err != nil {
		t.Fatal(err)
	}
	lock, err := session.LockRuntimeSharedContext(ctx, w.server.config.AuthorityDir, ref.Session.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	w.requestObservation(ref, true)
	_, _, readCtx, cancel, ok := w.nextRefresh(ctx)
	if !ok {
		t.Fatal("missing pending drain")
	}
	defer cancel()
	waiting := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() { result <- w.drain(piBackgroundAdmissionWait{readCtx, waiting}, ref) }()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("pending drain did not wait for exclusive admission")
	}
	release := mustPiForeground(t, w.server, ref.Session)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("foreground did not cancel waiting drain", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground waited behind background drain")
	}
	if fake.checked != 0 {
		t.Fatal("waiting drain sent input")
	}
	release()
	w.finishRefresh(ref, nil, true, true)
	lock.Close()
	_, _, readCtx, cancel, ok = w.nextRefresh(ctx)
	if !ok {
		t.Fatal("foreground cancellation lost real delivery")
	}
	defer cancel()
	if err := w.withMutation("pi-questions.respond")(readCtx, ref, func(admitted context.Context) error {
		return w.backgroundEffects(admitted, ref, func(effect context.Context) error {
			release := mustPiForeground(t, w.server, ref.Session)
			defer release()
			if effect.Err() != nil {
				t.Fatal("foreground canceled admitted effect continuation")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	w.finishRefresh(ref, nil, false, false)
}

func TestPiWorkflowForegroundBurstReservesOneDuePassAtomically(t *testing.T) {
	for _, outcome := range []error{nil, piquestions.ErrUnavailable, context.DeadlineExceeded} {
		t.Run(strings.ReplaceAll(strings.TrimSpace("result "+fmt.Sprint(outcome)), " ", "-"), func(t *testing.T) {
			w, _, ref := newPiScheduledFixture(t)
			now := time.Unix(1000, 0)
			w.observations.now = func() time.Time { return now }
			w.requestObservation(ref, true)
			type grant struct {
				release func()
				err     error
			}
			results := make(chan grant, 16)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for range 16 {
				go func() { release, err := w.server.acquirePiForeground(ctx, ref.Session); results <- grant{release, err} }()
			}
			var releases []func()
			for range 8 {
				select {
				case result := <-results:
					if result.err != nil {
						t.Fatal(result.err)
					}
					releases = append(releases, result.release)
				case <-time.After(2 * time.Second):
					t.Fatal("foreground burst did not progress")
				}
			}
			cancel()
			for range 8 {
				result := <-results
				if result.err == nil {
					result.release()
					t.Fatal("concurrent caller slipped past reserved pass")
				}
				if !errors.Is(result.err, context.Canceled) {
					t.Fatal(result.err)
				}
			}
			if _, _, _, _, ok := w.nextRefresh(context.Background()); ok {
				t.Fatal("background preempted already granted foreground")
			}
			for _, release := range releases {
				release()
				release()
			}
			got, drain, readCtx, stop, ok := w.nextRefresh(context.Background())
			if !ok || got != ref || !drain || !w.observations.refreshes[ref].reserved {
				t.Fatal("due background did not receive reserved turn")
			}
			defer stop()
			wait, stopWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
			if release, err := w.server.acquirePiForeground(wait, ref.Session); !errors.Is(err, context.DeadlineExceeded) {
				if release != nil {
					release()
				}
				t.Fatal("new foreground bypassed active reservation", err)
			}
			stopWait()
			if readCtx.Err() != nil {
				t.Fatal("foreground canceled reserved pass")
			}
			w.finishRefresh(ref, outcome, outcome != nil, false)
			if len(w.observations.reserved) != 0 || len(w.observations.foreground) != 0 {
				t.Fatal("reservation or canceled registration leaked")
			}
			// Failure releases the gate too, but its backed-off retry must not
			// reserve another pass while an ordinary foreground burst proceeds.
			for range 16 {
				release := mustPiForeground(t, w.server, ref.Session)
				release()
			}
			if len(w.observations.reserved) != 0 {
				t.Fatal("backed-off or completed work reserved foreground")
			}
		})
	}
}

func TestPiWorkflowParentBudgetExpiryBacksOffDeliveryDemand(t *testing.T) {
	w, fake, ref := newPiScheduledFixture(t)
	now := time.Unix(1000, 0)
	w.observations.now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	fake.before = func() { <-ctx.Done() }
	w.requestObservation(ref, true)
	w.refreshRequested(ctx)
	if !w.observations.refreshes[ref].retryAt.Equal(now.Add(time.Second)) || !w.observations.requested[ref] {
		t.Fatal("parent expiry lost delivery demand or skipped failure backoff")
	}
	observed := fake.observations
	w.refreshRequested(context.Background())
	if fake.observations != observed {
		t.Fatal("expired pass retried before its backoff")
	}
}

func TestPiWorkflowForegroundWaitEndsWithPortalShutdown(t *testing.T) {
	w, _, ref := newPiScheduledFixture(t)
	w.requestObservation(ref, false)
	var releases []func()
	for range 8 {
		releases = append(releases, mustPiForeground(t, w.server, ref.Session))
	}
	result := make(chan error, 1)
	go func() {
		release, err := w.server.acquirePiForeground(context.Background(), ref.Session)
		if release != nil {
			release()
		}
		result <- err
	}()
	w.server.Close()
	select {
	case err := <-result:
		if !errors.Is(err, piruntime.ErrUnavailable) {
			t.Fatal("shutdown left foreground admission available", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown stranded a foreground scheduling waiter")
	}
	for _, release := range releases {
		release()
	}
	if len(w.observations.foreground) != 0 {
		t.Fatal("shutdown waiter leaked a registration")
	}
}
