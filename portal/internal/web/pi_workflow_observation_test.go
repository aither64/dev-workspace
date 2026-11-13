package web

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestPiWorkflowInvalidationACKNeedsNoAuthorityDomainOrNativeLock(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: root, Slug: "2026-11-12-observation", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native"}
	w := &piWorkflowComposition{server: &Server{displayRefreshWake: make(chan struct{}, 1)}}
	w.observations.initialize()
	attempt := &piWorkflowAttachment{runtime: piruntime.RuntimeRef{Session: ref.Session, InstanceID: "instance", RequestID: "request", Digest: strings.Repeat("c", 64), Generation: strings.Repeat("d", 64)}, ready: make(chan struct{}), finished: make(chan struct{})}
	w.observations.attachments[ref.Session] = attempt
	w.observations.counters[ref] = 0
	event := piruntime.WorkflowInvalidation{Runtime: attempt.runtime, AttachmentID: "attachment", Sequence: 1, Connected: true}
	w.invalidateRuntime(attempt, event)
	select {
	case <-attempt.ready:
		t.Fatal("initial event preceded ACK")
	default:
	}
	event.Sequence = 2
	w.invalidateRuntime(attempt, event)
	select {
	case <-attempt.ready:
	default:
		t.Fatal("later exact sequence did not acknowledge")
	}
	w.observations.entries[ref] = piWorkflowSnapshot{counter: 2}
	authority := filepath.Join(root, "authority")
	if err := os.Mkdir(authority, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := session.LockRuntimeExclusiveContext(context.Background(), authority, ref.Session.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	file := piquestions.StateFile{Root: root, Kind: "pi-questions", Name: "questions.json", Ref: ref}
	record := struct {
		Schema int `json:"schema"`
	}{Schema: 1}
	event.Sequence = 3
	if err := file.Update(context.Background(), &record, func() error { w.invalidateRuntime(attempt, event); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, present := w.observations.entries[ref]; present || w.observations.counters[ref] != 3 {
		t.Fatal("pre-effect callback did not clear exact entry")
	}
	event.Connected = false
	w.invalidateRuntime(attempt, event)
	event.Connected = true
	event.Sequence = 4
	w.invalidateRuntime(attempt, event)
	if attempt.connected {
		t.Fatal("old attachment revived after disconnect")
	}
	w.observations.counters[ref] = math.MaxUint64
	w.invalidateRef(ref, true)
	if w.observations.counters[ref] != math.MaxUint64 {
		t.Fatal("counter overflow wrapped")
	}
}

// These synthetic observations test the portal's comparison/race logic only.
// The tagged SDK fixture proves production/native composition separately.
type piObservedFixture struct {
	*piCreationRuntimeFixture
	before                func()
	idle                  bool
	origins               map[string][]byte
	current               piruntime.RuntimeRef
	observations, checked int
}

func (f *piObservedFixture) ReadCurrentRuntime(context.Context, agentbackend.SessionRef) (piruntime.RuntimeRef, error) {
	return f.current, nil
}
func (f *piObservedFixture) ObserveWorkflow(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.WorkflowObservation, error) {
	f.observations++
	proof, err := f.piCreationRuntimeFixture.InspectToolPolicy(ctx, h)
	if err != nil {
		return piruntime.WorkflowObservation{}, err
	}
	if f.before != nil {
		f.before()
	}
	return piruntime.WorkflowObservation{Settings: f.state.Settings, Handle: h, AttachmentID: "attachment", Sequence: 2, Retained: &proof.Policy, Proof: proof, Loaded: true, Idle: f.idle, HostTools: []string{"publish_plan", "request_user_input_async"}}, nil
}
func (f *piObservedFixture) InspectNativeOrigin(_ context.Context, h agentbackend.ExecutionHandle, o piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
	data, ok := f.origins[o.NativeToolCallID]
	if !ok {
		return piruntime.NativeOriginObservation{}, piruntime.ErrUnavailable
	}
	return piruntime.NativeOriginObservation{Handle: h, Origin: o, Arguments: append([]byte{}, data...)}, nil
}
func (f *piObservedFixture) SendCheckedSubmission(context.Context, agentbackend.ExecutionHandle, piruntime.SubmissionRef, piruntime.SendInput) (piruntime.SubmissionStatus, error) {
	f.checked++
	return piruntime.SubmissionStatus{}, piruntime.ErrUnavailable
}

func TestPiWorkflowObservationRaceBusyCaptureAndOriginalHandoff(t *testing.T) {
	s, receipt, native := newPiCreationFixtureForSlug(t, "2026-11-12-observed")
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	// Join the disposable background workers before exercising one deterministic
	// producer pass. No live server or native runtime is stopped by this fixture.
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	fake := &piObservedFixture{piCreationRuntimeFixture: native, idle: true, current: native.runtime, origins: map[string][]byte{"question-call": []byte(`{"questions":[{"title":"Path?","options":["One","Two"]}]}`)}}
	s.config.PiCreation.Runtime = fake
	w := &piWorkflowComposition{server: s, runtime: fake, tools: []string{"publish_plan", "request_user_input_async"}}
	w.observations.initialize()
	s.piWorkflows = w
	ref := native.state.Conversation
	ready := make(chan struct{})
	close(ready)
	w.observations.attachments[ref.Session] = &piWorkflowAttachment{runtime: native.runtime, attachment: "attachment", sequence: 2, initial: 1, connected: true, readyClosed: true, ready: ready, finished: make(chan struct{}), cancel: func() {}}
	caller := piquestions.Caller{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: native.runtime.Generation}, Lineage: piquestions.LineageRef{AnchorEntryID: "anchor", SelectionToken: "linear-v1:" + strings.Repeat("e", 64)}, NativeToolCallID: "question-call"}
	questions, _ := w.stores(ref)
	var group piquestions.QuestionView
	if err := w.withRecord(context.Background(), caller.Handle, "request_user_input_async", func(ctx context.Context) error {
		var err error
		group, err = questions.Publish(ctx, caller, piquestions.PublishInput{Questions: []piquestions.Question{{Title: "Path?", Options: []piquestions.Option{{Label: "One"}, {Label: "Two"}}}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fake.before = func() { w.invalidateRef(ref, false) }
	if err := w.refresh(context.Background(), ref); err == nil {
		t.Fatal("invalidation during native proof published old eligibility")
	}
	if _, found := w.observations.entries[ref]; found {
		t.Fatal("racing snapshot survived")
	}
	fake.before = nil
	if err := w.refresh(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	fake.before = func() { w.requestObservation(ref, false) }
	if err := w.refresh(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	fake.before = nil
	if _, queued := w.observations.requested[ref]; queued {
		t.Fatal("GET demand during a successful pass queued an immediately invalidating duplicate refresh")
	}
	fake.before = func() { w.requestObservation(ref, true) }
	if err := w.refresh(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	fake.before = nil
	if !w.observations.requested[ref] {
		t.Fatal("successful display publication dropped accepted-intent drain demand")
	}
	observations := fake.observations
	snapshot, err := (piWorkflowQuestionHTTP{w}).Snapshot(context.Background(), ref)
	if err != nil || len(snapshot.Questions) != 1 || !snapshot.Questions[0].CanRespond {
		t.Fatalf("question proof projection: %#v %v", snapshot, err)
	}
	plans, err := (piWorkflowPlanHTTP{w}).Snapshot(context.Background(), ref)
	if err != nil || !plans.CanSelectMode || fake.observations != observations {
		t.Fatal("GET did native work or lost complete mode proof", err)
	}
	fake.current.RequestID = "same-generation-replacement"
	if err := w.withRead(context.Background(), ref, func(ctx context.Context) error {
		if _, ok := w.cached(ctx, ref); ok {
			t.Fatal("same-generation runtime replacement reused observation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake.current = native.runtime
	fake.idle = false
	if err := w.refresh(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	snapshot, err = (piWorkflowQuestionHTTP{w}).Snapshot(context.Background(), ref)
	if err != nil || !snapshot.Questions[0].CanRespond {
		t.Fatal("busy capture incorrectly held", err)
	}
	input := piquestions.ResponseInput{ResponseID: "99999999-9999-4999-8999-999999999999", QuestionID: group.ID, QuestionRevision: group.Revision, Answers: []piquestions.Answer{{QuestionID: group.Questions[0].ID, ChoiceID: group.Questions[0].Options[0].ID}}}
	response, err := (piWorkflowQuestionHTTP{w}).AcceptResponse(context.Background(), ref, input)
	if err != nil || response.DeliveryPhase != "pending" {
		t.Fatalf("busy durable capture: %#v %v", response, err)
	}
	w.drain(context.Background(), ref)
	if fake.checked != 0 {
		t.Fatal("busy recipient sent an input")
	}
	fake.idle = true
	w.drain(context.Background(), ref)
	response, err = (piWorkflowQuestionHTTP{w}).Response(context.Background(), ref, input.ResponseID)
	if err != nil || response.DeliveryPhase != "Unknown" || fake.checked != 1 {
		t.Fatalf("uncertain handoff: %#v %v sends=%d", response, err, fake.checked)
	}
	w.drain(context.Background(), ref)
	if fake.checked != 1 {
		t.Fatal("Unknown handoff repeated checkedSend")
	}
}
