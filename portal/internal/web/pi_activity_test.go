package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
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

// The existing retained Team fixture supplies identity/admission; this port only
// controls the optional read operation, never creation or a native worker.
type piTeamActivityFixture struct {
	*piForkRuntimeFixture
	handles                     map[string]agentbackend.ExecutionHandle
	binds, verifies, activities int
	unsupported, unavailable    string
	cancel                      context.CancelFunc
}

func (f *piTeamActivityFixture) BindRead(_ context.Context, h agentbackend.ExecutionHandle) (backend.Operations, bool, error) {
	if h != f.handles[h.Conversation.Member] {
		return backend.Operations{}, false, piruntime.ErrScope
	}
	f.binds++
	port := piTeamActivityPort{f, h}
	ops := backend.Operations{Verify: port}
	if h.Conversation.Member != f.unsupported {
		ops.Activity = port
	}
	return ops, false, nil
}

type piTeamActivityPort struct {
	owner  *piTeamActivityFixture
	handle agentbackend.ExecutionHandle
}

func (p piTeamActivityPort) Verify(_ context.Context, id backend.Identity) error {
	p.owner.verifies++
	if id != piruntime.BackendIdentity(p.handle) {
		return piruntime.ErrScope
	}
	return nil
}
func (p piTeamActivityPort) ReadActivity(ctx context.Context, id backend.Identity) (backend.ActivitySnapshot, error) {
	if id != piruntime.BackendIdentity(p.handle) {
		return backend.ActivitySnapshot{}, piruntime.ErrScope
	}
	p.owner.activities++
	if p.owner.cancel != nil {
		p.owner.cancel()
		return backend.ActivitySnapshot{}, ctx.Err()
	}
	if p.handle.Conversation.Member == p.owner.unavailable {
		return backend.ActivitySnapshot{}, piruntime.ErrUnavailable
	}
	v := backend.ActivitySnapshot{ThreadID: id.ConversationID, CurrentState: "idle", CountsComplete: true,
		ObservedAtMS: 1700000120000, StateSinceMS: 1700000000000, IdleMS: 120000,
		CoverageReason: "Observed since worker attachment"}
	if p.handle.Conversation.Member == "lead" {
		v.SentMessages, v.ReceivedMessages, v.TotalToolCalls = 3, 2, 7
		v.WorkingMS, v.WaitingMS = 20000, 4000
	}
	return v, nil
}
func TestPiTeamActivityUsesAdmittedMembers(t *testing.T) {
	s, receipt, native := newPiForkFixture(t, true)
	creates, sends := native.creates, native.sends
	store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, err := store.Load(receipt.Request.Slug)
	if err != nil {
		t.Fatal(err)
	}
	f := &piTeamActivityFixture{piForkRuntimeFixture: native, handles: map[string]agentbackend.ExecutionHandle{}}
	for _, saved := range binding.Conversations {
		if ref, err := binding.Resolve(saved.Address); err == nil {
			f.handles[saved.Address] = agentbackend.ExecutionHandle{Conversation: ref, Generation: native.source.Generation}
		}
	}
	s.config.PiCreation.Runtime = f
	lead := f.handles["lead"]
	read := func(ctx context.Context, scope, generation string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/sessions/"+binding.Slug+"/team-stats", nil).WithContext(ctx)
		r.Header.Set("Origin", s.trustedOrigins[0])
		r.Header.Set("X-Workspace-Conversation-Scope", scope)
		r.Header.Set("X-Workspace-Pi-Generation", generation)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	decode := func(w *httptest.ResponseRecorder) []teamActivity {
		t.Helper()
		var payload struct {
			Members []teamActivity `json:"members"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &payload) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return payload.Members
	}
	rows := decode(read(context.Background(), piWorkflowNamespace(lead.Conversation), lead.Generation))
	if len(rows) != 4 || rows[0].Address != "lead" || rows[1].Address != "implementer0" || rows[2].Address != "reviewer0" || rows[3].Address != "architect0" {
		t.Fatal("roster order/states changed", rows)
	}
	if rows[0].Snapshot == nil || rows[0].Snapshot.SentMessages != 3 || rows[0].Snapshot.ReceivedMessages != 2 || rows[0].Snapshot.TotalToolCalls != 7 || rows[0].Snapshot.WorkingMS != 20000 || rows[0].Snapshot.CoverageComplete || !rows[0].Snapshot.CountsComplete || rows[2].Snapshot == nil || rows[2].Snapshot.SentMessages != 0 || rows[1].State != "creating" || rows[1].Snapshot != nil || rows[3].State != "removed" || rows[3].Snapshot != nil {
		t.Fatal("metrics or unavailable states changed", rows)
	}
	if f.binds != 2 || f.verifies != 2 || f.activities != 2 {
		t.Fatal("wrong activity reads", f.binds, f.verifies, f.activities)
	}
	before := f.binds
	for _, bad := range []struct{ scope, generation string }{
		{"pi-workflow-v1:" + strings.Repeat("f", 64), lead.Generation},
		{piWorkflowNamespace(lead.Conversation), strings.Repeat("f", 64)},
	} {
		if w := read(context.Background(), bad.scope, bad.generation); w.Code == 200 || f.binds != before {
			t.Fatal("stale page reached native read", w.Code)
		}
	}
	member := f.handles["reviewer0"]
	oldMember := member.Conversation
	oldMember.Incarnation = strings.Repeat("e", 64)
	if w := read(context.Background(), piWorkflowNamespace(oldMember), member.Generation); w.Code == 200 || f.binds != before {
		t.Fatal("replaced source reached native read")
	}
	// A materialized member page may observe its Team, using its own scope.
	decode(read(context.Background(), piWorkflowNamespace(member.Conversation), member.Generation))
	f.unsupported = "reviewer0"
	rows = decode(read(context.Background(), piWorkflowNamespace(lead.Conversation), lead.Generation))
	if rows[0].Snapshot == nil || rows[2].Snapshot != nil || rows[2].Error != "Activity not supported" {
		t.Fatal("unsupported erased peer data", rows)
	}
	f.unsupported, f.unavailable = "", "reviewer0"
	rows = decode(read(context.Background(), piWorkflowNamespace(lead.Conversation), lead.Generation))
	if rows[0].Snapshot == nil || rows[2].Snapshot != nil || rows[2].Error != "Statistics unavailable" {
		t.Fatal("unavailable erased peer data", rows)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	if w := read(ctx, piWorkflowNamespace(lead.Conversation), lead.Generation); w.Code == 200 || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("canceled read succeeded")
	}
	f.cancel, f.unavailable = nil, ""
	ctx2, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	decode(read(ctx2, piWorkflowNamespace(lead.Conversation), lead.Generation))
	if native.creates != creates || native.sends != sends {
		t.Fatal("read performed creation/send", native.creates, native.sends)
	}
	if len(native.copies) != 0 {
		t.Fatal("read forked a member")
	}
	// Existing templates must produce metric carriers for actual Pi identities.
	summary, err := session.Find(s.config.Workspace, binding.Slug)
	if err != nil {
		t.Fatal(err)
	}
	d := pageData{Session: summary}
	if err := s.configurePiSessionPage(context.Background(), &d, nil); err != nil {
		t.Fatal(err)
	}
	var html strings.Builder
	if err := s.templates.ExecuteTemplate(&html, "member-status", d); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"lead", "reviewer0"} {
		if !strings.Contains(html.String(), `data-team-stats="`+address+`"`) {
			t.Fatal("missing Pi metric cells", address)
		}
	}
	if strings.Contains(html.String(), `data-team-stats="implementer0"`) {
		t.Fatal("unstarted member advertised measured statistics")
	}
}
