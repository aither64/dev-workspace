package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piActivityReadFixture struct {
	*piAcceptanceReadFixture
	readBindings, activities int
}

func (f *piActivityReadFixture) BindRead(ctx context.Context, handle agentbackend.ExecutionHandle) (backend.Operations, bool, error) {
	f.readBindings++
	ops, err := f.Bind(ctx, handle)
	ops.Activity = f
	return ops, false, err
}
func (f *piActivityReadFixture) ReadActivity(_ context.Context, id backend.Identity) (backend.ActivitySnapshot, error) {
	f.activities++
	return backend.ActivitySnapshot{ThreadID: id.ConversationID, CurrentState: "working", WorkingMS: 123, ObservedAtMS: 1700000000000, CoverageComplete: false}, nil
}

func TestPiAdmittedActivityHTTPUsesOneReadBinding(t *testing.T) {
	slug := "2026-11-18-activity"
	s, receipt, native := newPiCreationFixtureForSlug(t, slug)
	reader := &piActivityReadFixture{piAcceptanceReadFixture: &piAcceptanceReadFixture{piCreationRuntimeFixture: native}}
	s.config.PiCreation.Runtime = reader
	s.piWorkflows = newPiWorkflowComposition(s)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	initialBinds := reader.binds
	dispatcher, err := s.piConversationHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected Codex fallback") }))
	if err != nil {
		t.Fatal(err)
	}
	s.conversation = dispatcher
	request := httptest.NewRequest("GET", "/api/sessions/"+slug+"/activity", nil)
	request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(native.state.Conversation))
	request.Header.Set("Origin", s.trustedOrigins[0])
	response := httptest.NewRecorder()
	s.sessionAPI(response, request)
	var activity backend.ActivitySnapshot
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &activity) != nil || activity.CurrentState != "working" || activity.WorkingMS != 123 {
		t.Fatalf("admitted activity HTTP %d %s", response.Code, response.Body.String())
	}
	if reader.readBindings != 1 || reader.binds-initialBinds != 1 || reader.verifies != 1 || reader.activities != 1 {
		t.Fatalf("bindings=%d/%d verify=%d activity=%d", reader.readBindings, reader.binds, reader.verifies, reader.activities)
	}
	if reader.reads != 0 {
		t.Fatal("activity read fetched transcript")
	}
	request.Header.Set("X-Workspace-Conversation-Scope", "pi-workflow-v1:"+strings.Repeat("f", 64))
	response = httptest.NewRecorder()
	s.sessionAPI(response, request)
	if response.Code == 200 || reader.readBindings != 1 {
		t.Fatal("stale scope entered native read")
	}
}

type noPollPiWatch struct{ calls int }

func (w *noPollPiWatch) Subscribe(context.Context, backend.Identity) (<-chan struct{}, func(), error) {
	w.calls++
	return nil, func() {}, piruntime.ErrUnavailable
}

func TestPiConnectedWatchUsesInvalidationAndClosesOnLoss(t *testing.T) {
	for _, boundary := range []string{"disconnect", "replacement", "cancel"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/fixture", Slug: "2026-11-18-fixture", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "11111111-1111-4111-8111-111111111111"}
			handle := agentbackend.ExecutionHandle{Conversation: ref, Generation: strings.Repeat("c", 64)}
			if piruntime.BackendIdentity(handle) == (backend.Identity{}) {
				t.Fatal("invalid native identity fixture")
			}
			owner := &piWorkflowComposition{server: &Server{displayRefreshWake: make(chan struct{}, 1)}}
			owner.observations.initialize()
			attempt := &piWorkflowAttachment{runtime: piruntime.RuntimeRef{Session: ref.Session, Generation: handle.Generation}, connected: true, readyClosed: true}
			owner.observations.attachments[ref.Session] = attempt
			native := &noPollPiWatch{}
			for _, unavailable := range []string{"absent", "unready", "lost", "generation"} {
				owner.observations.attachments[ref.Session] = attempt
				attempt.readyClosed, attempt.lost, attempt.runtime.Generation = true, false, handle.Generation
				switch unavailable {
				case "absent":
					delete(owner.observations.attachments, ref.Session)
				case "unready":
					attempt.readyClosed = false
				case "lost":
					attempt.lost = true
				case "generation":
					attempt.runtime.Generation = strings.Repeat("d", 64)
				}
				if _, _, err := (piWorkflowWatch{owner: owner, ref: ref, native: native, currentEvents: true}).Subscribe(ctx, piruntime.BackendIdentity(handle)); err == nil || native.calls != 0 {
					t.Fatal("current capability fell back to native polling", unavailable)
				}
			}
			owner.observations.attachments[ref.Session] = attempt
			attempt.readyClosed, attempt.lost, attempt.runtime.Generation = true, false, handle.Generation
			events, release, err := (piWorkflowWatch{owner: owner, ref: ref, native: native, currentEvents: true}).Subscribe(ctx, piruntime.BackendIdentity(handle))
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			select {
			case <-events:
			case <-ctx.Done():
				t.Fatal("registration did not request initial refresh")
			}
			owner.invalidateRef(ref, false)
			select {
			case <-events:
			case <-ctx.Done():
				t.Fatal("post-persistence invalidation lost")
			}
			if native.calls != 0 {
				t.Fatal("connected watch started full-history polling")
			}
			if _, _, err := (piWorkflowWatch{owner: owner, ref: ref, native: native, currentEvents: false}).Subscribe(ctx, piruntime.BackendIdentity(handle)); err == nil || native.calls != 1 {
				t.Fatal("legacy worker lost its native subscription")
			}
			native.calls = 0
			owner.observations.mu.Lock()
			if boundary == "replacement" {
				owner.observations.attachments[ref.Session] = &piWorkflowAttachment{}
			} else if boundary == "disconnect" {
				attempt.connected = false
				attempt.lost = true
			}
			owner.observations.mu.Unlock()
			if boundary == "cancel" {
				cancel()
			} else {
				owner.invalidateRef(ref, false)
			}
			for {
				select {
				case _, open := <-events:
					if !open {
						return
					}
				case <-ctx.Done():
					if boundary != "cancel" {
						t.Fatal("lost attachment did not close stream")
					}
					// Cancellation is joined through the same stream close.
					for range events {
					}
					return
				}
			}
		})
	}
}
