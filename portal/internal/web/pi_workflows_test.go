package web

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func piWorkflowGuardFixture(t *testing.T, s *Server) {
	t.Helper()
	piTeamPackageFixture(t, s)
	path := filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"piQuestionsSchema", "piPlanSchema", "hostCallbacks", "linearLineage", "workflowObservations"} {
		fields[key] = 1
	}
	fields["piToolPolicy"] = map[string]int{"policy": 1, "conversationSchema": 2}
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.requirePiWorkflowGuards(); err != nil {
		t.Fatal(err)
	}
}

// The native external package cannot import another package's *_test helpers.
// The finite batch exports this one existing validated typed fixture instead of
// copying a catalog generator or adding a production constructor override.
func TestPiWorkflowExportNativeCatalogFixture(t *testing.T) {
	path := os.Getenv("PI_CENTRAL_CATALOG_FIXTURE")
	if path == "" {
		t.Skip("only the finite native composition batch exports its typed fixture")
	}
	preset := teamruntime.Preset{ID: "delegated", Name: "Fixture", Description: "Fixture team", Roles: []string{"lead", "implementer0"}, CatalogDigest: strings.Repeat("2", 64), TeamDigest: strings.Repeat("3", 64), LeadModel: "p3-model", LeadEffort: "high", Members: []teamruntime.MemberSpec{{Role: "implementer", Address: "implementer0", Model: "p3-model", Effort: "high", Behavior: "implementer", Access: "workspace_write"}}}
	installed := piCreationTeamCatalogFixture(t, preset)
	data, err := json.Marshal(installed.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPiWorkflowInitialPolicyUsesExactAccessAndFixedCeiling(t *testing.T) {
	tools := []string{"publish_plan", "request_user_input_async"}
	for _, access := range []string{"read_only", "workspace_write"} {
		initial, err := composePiWorkflowPolicy(access, "default", 1, tools)
		if err != nil || piruntime.ValidateToolPolicy(initial) != nil || initial.Revision != 1 || initial.Mode != "default" {
			t.Fatalf("initial %s: %#v %v", access, initial, err)
		}
		plan, err := composePiWorkflowPolicy(access, "plan", 2, tools)
		if err != nil || !slices.Equal(initial.RegisteredCeiling, plan.RegisteredCeiling) || slices.Contains(plan.Active, "bash") || slices.Contains(plan.Active, "edit") || slices.Contains(plan.Active, "write") {
			t.Fatalf("Plan changed its ceiling: %#v %v", plan, err)
		}
		if slices.Contains(initial.Active, "write") != (access == "workspace_write") {
			t.Fatal("saved access inferred or ignored")
		}
		if !slices.Contains(initial.Active, "publish_plan") || !slices.Contains(plan.Active, "request_user_input_async") {
			t.Fatal("bound released factories lost")
		}
	}
	for _, args := range []struct {
		access string
		tools  []string
	}{
		{"", tools}, {"full_access", tools}, {"workspace_write", []string{"publish_plan", "publish_plan"}}, {"workspace_write", []string{"monitor_operation"}},
	} {
		if _, err := composePiWorkflowPolicy(args.access, "default", 1, args.tools); err == nil {
			t.Fatalf("unproved factory/access admitted: %#v", args)
		}
	}
}

func TestPiWorkflowDescriptorNamespaceUsesDurableScopeOnly(t *testing.T) {
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/workspace", Slug: "example", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "private-native-id"}
	namespace := piWorkflowNamespace(ref)
	again := ref
	again.NativeID = "replacement-native-id"
	if namespace != piWorkflowNamespace(again) || strings.Contains(namespace, ref.NativeID) || strings.Contains(namespace, ref.Session.Workspace) {
		t.Fatal("native identity/path leaked into durable storage")
	}
	again.Incarnation = strings.Repeat("c", 64)
	if namespace == piWorkflowNamespace(again) {
		t.Fatal("member incarnation did not isolate browser storage")
	}
	data, err := json.Marshal(piWorkflowDescriptor{Backend: "pi", BasePath: "/api/sessions/example", Member: "lead", Namespace: namespace})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 4 || fields["backend"] == nil || fields["basePath"] == nil || fields["member"] == nil || fields["namespace"] == nil {
		t.Fatal("descriptor shape changed")
	}
}

func TestPiWorkflowInitialCreateCapturesSameManagerAndExactDefault(t *testing.T) {
	s, receipt, runtime := newPiCreationFixture(t)
	if s.piWorkflows == nil || s.piWorkflows.runtime != runtime {
		t.Fatal("optional ports came from another runtime")
	}
	if runtime.creates != 0 || runtime.sends != 0 || runtime.start != nil {
		t.Fatal("composition had native effects")
	}
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	policy := runtime.initialPolicy
	expected := []string{"bash", "edit", "find", "grep", "ls", "publish_plan", "read", "request_user_input_async", "write"}
	if policy == nil || policy.Mode != "default" || policy.Revision != 1 || !slices.Equal(policy.Active, expected) || !slices.Equal(policy.RegisteredCeiling, expected) || policy.Digest != piruntime.ToolPolicyDigest(*policy) {
		t.Fatalf("first Create policy: %#v", policy)
	}
}

func TestPiWorkflowInitialCreateRefusesMissingHistoricalAccess(t *testing.T) {
	for _, change := range []func(*Server){
		func(s *Server) { s.installedTeams = nil },
		func(s *Server) { s.installedTeams.Catalog.CatalogDigest = strings.Repeat("f", 64) },
		func(s *Server) {
			team := s.installedTeams.Catalog.Teams["delegated"]
			team.TeamDigest = strings.Repeat("f", 64)
			s.installedTeams.Catalog.Teams["delegated"] = team
		},
		func(s *Server) { team := s.installedTeams.Catalog.Teams["delegated"]; delete(team.Roles, "team_lead") },
		func(s *Server) {
			team := s.installedTeams.Catalog.Teams["delegated"]
			lead := team.Roles["team_lead"]
			lead.Access = ""
			team.Roles["team_lead"] = lead
		},
	} {
		s, receipt, runtime := newPiCreationFixture(t)
		change(s)
		if err := s.initializePiCreation(receipt); err == nil || runtime.start != nil || runtime.creates != 0 || runtime.sends != 0 {
			t.Fatalf("missing historical access crossed native boundary: start=%v create=%d send=%d err=%v", runtime.start, runtime.creates, runtime.sends, err)
		}
	}
}

func TestPiWorkflowRoutesAndCarriersRefuseUnknownMemberWithoutNativeRead(t *testing.T) {
	slug := "2026-11-12-workflow"
	s, receipt, native := newPiCreationFixtureForSlug(t, slug)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	// Disable demand delivery only for this synchronous GET projection fixture.
	// The real worker producer and native composition have separate fixtures.
	s.displayRefreshWake = nil
	s.config.BaseURL = "https://portal.example"
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{Session: summary}
	if err := s.configurePiSessionPage(context.Background(), &data, nil); err != nil {
		t.Fatal(err)
	}
	s.setWorkflowPageData(&data)
	if data.SessionBackend != "pi" || data.SelectedMember != "lead" || data.SelectedThreadID != "" || data.ConversationID != slug || !data.ConversationReady || !data.ConversationInteractive || data.Session.Interactive {
		t.Fatalf("Pi/legacy view scopes: %#v", data)
	}
	var descriptor piWorkflowDescriptor
	if json.Unmarshal([]byte(data.PiWorkflow), &descriptor) != nil || descriptor.Member != "lead" || descriptor.Namespace != piWorkflowNamespace(native.state.Conversation) {
		t.Fatal("missing durable carrier")
	}
	page := httptest.NewRecorder()
	s.render(page, "session", data)
	body := html.UnescapeString(page.Body.String())
	if !strings.Contains(body, `data-session-backend="pi"`) || !strings.Contains(body, `data-thread-id=""`) || !strings.Contains(body, `data-pi-workflow="`+data.PiWorkflow+`"`) || !strings.Contains(body, `id="message-form"`) || strings.Contains(body, `id="fork-open"`) {
		t.Fatalf("carrier/form mismatch: %d", page.Code)
	}
	for _, marker := range []string{`<h2>Pi</h2>`, `aria-label="Pi conversation"`, `id="codex-mode"`, `id="codex-mode-status"`, `id="optional-questions"`, `id="plan-actions"`, `id="codex-member"`, `id="interrupt"`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("Pi conversation lost its label or shared root: %s", marker)
		}
	}
	for _, marker := range []string{`id="codex-settings-open"`, `id="codex-settings-dialog"`, `id="session-tab-team"`, `id="progress-observer"`, `id="communication"`} {
		if strings.Contains(body, marker) {
			t.Fatalf("Pi page offered an unavailable Codex control: %s", marker)
		}
	}
	if err := s.configurePiSessionPage(context.Background(), &data, []string{"missing0"}); err == nil {
		t.Fatal("unknown member fell back to lead")
	}
	if _, err := (piWorkflowPlanHTTP{s.piWorkflows}).Snapshot(context.Background(), native.state.Conversation); err != nil {
		t.Fatal("direct native-free snapshot", err)
	}
	for _, path := range []string{"/api/sessions/" + slug + "/pi-plan", "/api/sessions/" + slug + "/pi-questions"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(native.state.Conversation))
		response := httptest.NewRecorder()
		s.sessionAPI(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("native-free %s: %d %s", path, response.Code, response.Body.String())
		}
		request = httptest.NewRequest(http.MethodGet, path+"?member=missing0", nil)
		request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(native.state.Conversation))
		response = httptest.NewRecorder()
		s.sessionAPI(response, request)
		if response.Code == http.StatusOK {
			t.Fatal("route member fallback")
		}
	}
	if native.creates != 1 || native.sends != 1 {
		t.Fatal("GET created or sent native input")
	}
	for _, path := range []string{"/api/sessions/" + slug + "/pi-plan/other", "/api/sessions/" + slug + "/pi-questions/mode", "/api/sessions/" + slug + "/pi-plan/acceptances/no-id", "/api/sessions/" + slug + "/pi-plan//mode", "/api/sessions/" + slug + "/pi-plan/%6dode"} {
		value, _ := url.Parse(path)
		if _, valid := sessionAPIPath(value); valid {
			t.Fatalf("unlisted path admitted: %s", path)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+slug+"/pi-plan/mode", strings.NewReader(`{}`))
	request.Header.Set("Origin", "https://foreign.invalid")
	request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(native.state.Conversation))
	response := httptest.NewRecorder()
	s.sessionAPI(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong origin: %d", response.Code)
	}
	s.trustedOrigins = []string{s.config.BaseURL, "https://alias.example"}
	for _, operation := range []string{"pi-plan/mode", "pi-plan/acceptances", "pi-questions/responses"} {
		for _, origin := range []string{s.config.BaseURL, "https://alias.example", "https://foreign.invalid", ""} {
			request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+slug+"/"+operation, strings.NewReader(`{}`))
			request.Header.Set("Origin", origin)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(native.state.Conversation))
			response := httptest.NewRecorder()
			s.sessionAPI(response, request)
			allowed := origin == s.config.BaseURL || origin == "https://alias.example"
			if allowed && response.Code == http.StatusForbidden || !allowed && response.Code != http.StatusForbidden {
				t.Fatalf("registered-origin/CSRF fence %s %q: %d", operation, origin, response.Code)
			}
		}
	}
}

type piNeutralFixture struct {
	*piCreationRuntimeFixture
	verifies                                int
	ordinarySends, interrupts, observations int
	omitSend                                bool
	sendError                               error
}

func (f *piNeutralFixture) Bind(ctx context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	if h.Conversation != f.state.Conversation || h.Generation != f.state.Generation {
		return backend.Operations{}, piruntime.ErrScope
	}
	operations := backend.Operations{Verify: f, Interrupt: f}
	if !f.omitSend {
		operations.Send = f
	}
	return operations, nil
}
func (f *piNeutralFixture) Verify(context.Context, backend.Identity) error { f.verifies++; return nil }
func (f *piNeutralFixture) Send(context.Context, backend.Identity, backend.SendRequest) (backend.SendResult, error) {
	f.ordinarySends++
	if f.sendError != nil {
		return backend.SendResult{Outcome: backend.NotSubmitted}, f.sendError
	}
	return backend.SendResult{Outcome: backend.Accepted}, nil
}
func (f *piNeutralFixture) Interrupt(context.Context, backend.Identity) error {
	f.interrupts++
	return nil
}
func (f *piNeutralFixture) ObserveWorkflow(context.Context, agentbackend.ExecutionHandle) (piruntime.WorkflowObservation, error) {
	f.observations++
	return piruntime.WorkflowObservation{}, piruntime.ErrHeld
}

func TestPiWorkflowNeutralLeaseRetainsRealLocksAndDetachedScope(t *testing.T) {
	s, receipt, native := newPiCreationFixture(t)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	fake := &piNeutralFixture{piCreationRuntimeFixture: native}
	s.config.PiCreation.Runtime = fake
	s.piWorkflows.runtime = fake
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), piConversationScopeKey{}, piWorkflowNamespace(native.state.Conversation)))
	target, err := s.resolvePiBackend(ctx, conversation.ResolveRequest{ID: "example", Mutation: true, Operation: "message"})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Release()
	held := func() {
		t.Helper()
		wait, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer stop()
		lock, err := session.LockRuntimeSharedContext(wait, s.config.AuthorityDir, "example")
		if lock != nil {
			lock.Close()
			t.Fatal("mutation lease released early")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	held()
	if err := target.Operations.Verify.Verify(ctx, target.Identity); err != nil {
		t.Fatal(err)
	}
	if err := target.MutationLock.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	held()
	// C's admitted Send detaches browser cancellation; only its passed context
	// and explicit release control this lease, not the original context object.
	cancel()
	if err := target.Operations.Verify.Verify(context.Background(), target.Identity); err != nil {
		t.Fatal("browser cancellation revoked admitted scope", err)
	}
	wrong := target.Identity
	wrong.Generation = strings.Repeat("9", 64)
	if err := target.Operations.Verify.Verify(context.Background(), wrong); err == nil {
		t.Fatal("wrong identity used lease")
	}
	target.MutationLock.Unlock()
	target.Release()
	target.Release()
	if err := target.Operations.Verify.Verify(context.Background(), target.Identity); err == nil {
		t.Fatal("expired lease reused")
	}
	lock, err := session.LockRuntimeSharedContext(context.Background(), s.config.AuthorityDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	read, err := s.resolvePiBackend(context.WithValue(context.Background(), piConversationScopeKey{}, piWorkflowNamespace(native.state.Conversation)), conversation.ResolveRequest{ID: "example", Operation: "transcript"})
	if err != nil {
		t.Fatal(err)
	}
	if read.Release != nil || read.MutationLock != nil {
		t.Fatal("read pins mutation lease")
	}
	lock, err = session.LockRuntimeExclusiveContext(context.Background(), s.config.AuthorityDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestPiWorkflowExpectedScopeTransportsRemainPreconditions(t *testing.T) {
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/workspace", Slug: "2026-11-12-scope", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native"}
	scope := piWorkflowNamespace(ref)
	for _, test := range []struct {
		header        []string
		query         string
		events, valid bool
	}{
		{[]string{scope}, "", false, true}, {nil, "conversationScope=" + url.QueryEscape(scope) + "&cursor=saved", true, true},
		{[]string{scope}, "conversationScope=" + url.QueryEscape(scope), true, true},
		{nil, "", true, false}, {[]string{scope, scope}, "", false, false},
		{[]string{scope}, "conversationScope=" + url.QueryEscape(scope) + "&conversationScope=" + url.QueryEscape(scope), true, false},
		{[]string{scope}, "conversationScope=" + url.QueryEscape(scope), false, false},
		{[]string{""}, "conversationScope=" + url.QueryEscape(scope), true, false},
		{[]string{scope}, "conversationScope=pi-workflow-v1%3A" + strings.Repeat("f", 64), true, false},
	} {
		request := httptest.NewRequest("GET", "/codex/conversations/2026-11-12-scope/events?"+test.query, nil)
		for _, value := range test.header {
			request.Header.Add("X-Workspace-Conversation-Scope", value)
		}
		scoped, err := scopedPiConversationRequest(request, test.events)
		if (err == nil) != test.valid {
			t.Fatalf("scope %v %q: %v", test.header, test.query, err)
		}
		if err == nil {
			if err := checkExpectedPiScope(scoped.Context(), ref); err != nil {
				t.Fatal(err)
			}
			if scoped.URL.Query().Has("conversationScope") {
				t.Fatal("EventSource scope leaked into C query parser")
			}
			if strings.Contains(test.query, "cursor=saved") && scoped.URL.Query().Get("cursor") != "saved" {
				t.Fatal("unrelated C query dropped")
			}
			replacement := ref
			replacement.Incarnation = strings.Repeat("c", 64)
			if checkExpectedPiScope(scoped.Context(), replacement) == nil {
				t.Fatal("old page retargeted replacement incarnation")
			}
		}
	}
	if checkExpectedPiScope(context.Background(), ref) == nil {
		t.Fatal("missing expectation accepted")
	}
}

type piBadInitialPolicyFixture struct {
	*piCreationRuntimeFixture
	mutate      func(*piruntime.ToolPolicyProof)
	inspections int
}

func (f *piBadInitialPolicyFixture) InspectToolPolicy(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
	f.inspections++
	proof, err := f.piCreationRuntimeFixture.InspectToolPolicy(ctx, h)
	if err == nil {
		f.mutate(&proof)
	}
	return proof, err
}
func TestPiWorkflowFirstSendRequiresExactCreatedPolicyAndRetryOnlyLooksUp(t *testing.T) {
	for name, mutate := range map[string]func(*piruntime.ToolPolicyProof){
		"unapplied":        func(p *piruntime.ToolPolicyProof) { p.Applied = false },
		"wrong-generation": func(p *piruntime.ToolPolicyProof) { p.Generation = strings.Repeat("f", 64) },
		"advanced-policy":  func(p *piruntime.ToolPolicyProof) { p.Policy.Revision++ },
		"partial-registry": func(p *piruntime.ToolPolicyProof) { p.Registered = p.Registered[:len(p.Registered)-1] },
	} {
		t.Run(name, func(t *testing.T) {
			s, receipt, native := newPiCreationFixture(t)
			wrapper := &piBadInitialPolicyFixture{piCreationRuntimeFixture: native, mutate: mutate}
			s.config.PiCreation.Runtime = wrapper
			s.piWorkflows.runtime = wrapper
			if err := s.initializePiCreation(receipt); err == nil || native.creates != 1 || native.sends != 0 || wrapper.inspections != 1 {
				t.Fatalf("bad proof crossed firstSend: %v %d/%d/%d", err, native.creates, native.sends, wrapper.inspections)
			}
			if err := s.initializePiCreation(receipt); err == nil || native.creates != 1 || native.sends != 0 || wrapper.inspections != 1 {
				t.Fatalf("retry gained new proof/send opportunity: %v %d/%d/%d", err, native.creates, native.sends, wrapper.inspections)
			}
		})
	}
}

// Host-composition proof only: P3's existing schema/policy/barrier/receipt tests
// own native behavior. No layout or Default proof is invented by a Bind result.
func TestPiWorkflowOrdinaryP1IndependentOfOptionalP6AndPropagatesNativeRefusal(t *testing.T) {
	s, receipt, native := newPiCreationFixtureForSlug(t, "2026-11-12-ordinary")
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	fake := &piNeutralFixture{piCreationRuntimeFixture: native}
	s.config.PiCreation.Runtime = fake
	composition := s.piWorkflows
	composition.runtime = fake
	s.piWorkflows = nil // Keep required immutable package maps; only P6 ports absent.
	summary, err := session.Find(s.config.Workspace, receipt.Request.Slug)
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{Session: summary}
	if err := s.configurePiSessionPage(context.Background(), &data, nil); err != nil {
		t.Fatal(err)
	}
	s.setWorkflowPageData(&data)
	if !data.ConversationInteractive || data.PiWorkflow != "" || data.ConversationScope == "" {
		t.Fatal("P6 absence disabled generic conversation or fabricated descriptor")
	}
	ctx := context.WithValue(context.Background(), piConversationScopeKey{}, data.ConversationScope)
	acquire := func(operation string) conversation.BackendTarget {
		t.Helper()
		target, err := s.resolvePiBackend(ctx, conversation.ResolveRequest{ID: receipt.Request.Slug, Mutation: true, Operation: operation})
		if err != nil {
			t.Fatal(err)
		}
		if err := target.MutationLock.Lock(ctx); err != nil {
			target.Release()
			t.Fatal(err)
		}
		return target
	}
	release := func(target conversation.BackendTarget) { target.MutationLock.Unlock(); target.Release() }
	target := acquire("message")
	result, err := target.Operations.Send.Send(ctx, target.Identity, backend.SendRequest{Message: "ordinary input", ClientUserMessageID: "77777777-7777-4777-8777-777777777777"})
	release(target)
	if err != nil || result.Outcome != backend.Accepted || fake.ordinarySends != 1 || fake.observations != 0 {
		t.Fatal("ordinary P1 required P6 proof", result, err)
	}
	fake.sendError = piruntime.ErrHeld
	s.piWorkflows = composition
	ref := native.state.Conversation
	composition.observations.counters[ref] = 7
	composition.observations.entries[ref] = piWorkflowSnapshot{counter: 7}
	target = acquire("message")
	_, err = target.Operations.Send.Send(ctx, target.Identity, backend.SendRequest{Message: "held policy/sticky attachment"})
	release(target)
	if !errors.Is(err, piruntime.ErrHeld) || fake.ordinarySends != 2 || fake.observations != 0 {
		t.Fatal("native failure downgraded or P6 observation became a generic gate", err)
	}
	if _, present := composition.observations.entries[ref]; present || composition.observations.counters[ref] <= 7 {
		t.Fatal("composed observation survived ordinary effect")
	}
	fake.omitSend = true
	target = acquire("message")
	if target.Operations.Send != nil || target.Capabilities.Send {
		t.Fatal("missing actual operation advertised")
	}
	release(target)
	target = acquire("interrupt")
	err = target.Operations.Interrupt.Interrupt(ctx, target.Identity)
	release(target)
	if err != nil || fake.interrupts != 1 || fake.ordinarySends != 2 || fake.observations != 0 {
		t.Fatal("busy interrupt acquired Send/P6 prerequisites", err)
	}
}

func TestPiWorkflowDetailsAndInvalidBackendStayScopedAndNativeFree(t *testing.T) {
	slug := "2026-11-12-details"
	s, receipt, native := newPiCreationFixtureForSlug(t, slug)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	// The isolated initializer does not run the outer creation coordinator.
	// Its completed journal/authority now own the ordinary page, not the test's
	// original in-memory running-receipt overlay.
	s.operationMu.Lock()
	delete(s.creations, slug)
	s.operationMu.Unlock()
	s.piWorkflows = nil
	scope := piWorkflowNamespace(native.state.Conversation)
	request := func(path string, scopes []string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		for _, value := range scopes {
			r.Header.Add("X-Workspace-Conversation-Scope", value)
		}
		response := httptest.NewRecorder()
		s.sessionAPI(response, r)
		return response
	}
	path := "/api/sessions/" + slug + "/details"
	response := request(path, []string{scope})
	var details map[string]any
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &details) != nil {
		t.Fatalf("Pi details %d: %s", response.Code, response.Body.String())
	}
	if details["sessionBackend"] != "pi" || details["conversationScope"] != scope || details["conversationInteractive"] != true || details["conversationHeld"] != false || details["interactive"] != false {
		t.Fatalf("generic/P6/legacy flags mixed: %#v", details)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("scoped details may be cached")
	}
	for _, test := range []struct {
		path   string
		scopes []string
	}{
		{path, nil},
		{path, []string{scope, scope}},
		{path, []string{"pi-workflow-v1:" + strings.Repeat("f", 64)}},
		{path + "?member=missing0", []string{scope}},
		{path + "?member=lead&member=lead", []string{scope}},
	} {
		if response := request(test.path, test.scopes); response.Code != http.StatusConflict {
			t.Fatalf("details retargeted missing/stale selection: %s %v: %d", test.path, test.scopes, response.Code)
		}
	}
	if native.creates != 1 || native.sends != 1 {
		t.Fatal("details created or sent native input")
	}
	legacyCalls := 0
	dispatcher, err := s.piConversationHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.conversation = dispatcher
	store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, err := store.Load(slug)
	if err != nil {
		t.Fatal(err)
	}
	binding.Backend = "unknown"
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(s.config.UserStateRoot, "session-backends", "*", slug+".json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("exact fixture binding: %v %v", paths, err)
	}
	if err := os.WriteFile(paths[0], encoded, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{path, "/api/sessions/" + slug + "/thread", "/codex/conversations/" + slug + "/thread", "/" + slug + "/"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Workspace-Conversation-Scope", scope)
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, r)
		if response.Code != http.StatusConflict {
			t.Fatalf("unknown backend was normalized: %s %d", path, response.Code)
		}
	}
	if legacyCalls != 0 || native.creates != 1 || native.sends != 1 {
		t.Fatal("unknown backend entered Codex or native execution")
	}
}
