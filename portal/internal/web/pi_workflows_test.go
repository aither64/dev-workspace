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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
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
	preset := piCreationSoloPreset()
	preset.LeadModel = "p3-model"
	preset.LeadInstructions = "Lead the fixture.\n/path/to/role.md\nKeep the literal café role.\n"
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
			team := s.installedTeams.Catalog.Teams["solo"]
			team.TeamDigest = strings.Repeat("f", 64)
			s.installedTeams.Catalog.Teams["solo"] = team
		},
		func(s *Server) { team := s.installedTeams.Catalog.Teams["solo"]; delete(team.Roles, "team_lead") },
		func(s *Server) {
			team := s.installedTeams.Catalog.Teams["solo"]
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

// This fixture supplies synthetic transcript values; the recorded click below
// is a contract input, not a native Send or completion witness.
type piAcceptanceReadFixture struct {
	*piCreationRuntimeFixture
	mu                     sync.Mutex
	reads, binds, verifies int
}

func (f *piAcceptanceReadFixture) Bind(_ context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.binds++
	if h != (agentbackend.ExecutionHandle{Conversation: f.state.Conversation, Generation: f.state.Generation}) {
		return backend.Operations{}, piruntime.ErrScope
	}
	return backend.Operations{Verify: f, Read: f, Send: f.piCreationRuntimeFixture}, nil
}
func (f *piAcceptanceReadFixture) Verify(_ context.Context, id backend.Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	if id != piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: f.state.Conversation, Generation: f.state.Generation}) {
		return piruntime.ErrScope
	}
	return nil
}
func (f *piAcceptanceReadFixture) Read(_ context.Context, id backend.Identity) (backend.Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: f.state.Conversation, Generation: f.state.Generation}) {
		return backend.Transcript{}, piruntime.ErrScope
	}
	f.reads++
	result := backend.Transcript{ThreadID: f.state.Conversation.Session.Slug, Status: "inProgress", Model: "gpt-4o-mini", ReasoningEffort: "off", Entries: []backend.TranscriptEntry{{Kind: "userMessage", Text: "ORIGINAL_PI", TurnID: "initial"}}}
	if f.reads == 2 {
		result.Status = "idle"
		result.LatestTurnID = "followup"
		result.Entries = append(result.Entries, backend.TranscriptEntry{Kind: "userMessage", Text: "FOLLOWUP_PI", TurnID: "followup"}, backend.TranscriptEntry{Kind: "agentMessage", Text: "CREATION_REPLY_pi", TurnID: "followup"})
	}
	return result, nil
}

func TestPiAcceptanceFollowupScope(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("declared Node required:", err)
	}
	slug := "2026-11-14-followup"
	s, receipt, native := newPiCreationFixtureForSlug(t, slug)
	reader := &piAcceptanceReadFixture{piCreationRuntimeFixture: native}
	s.config.PiCreation.Runtime = reader
	s.piWorkflows = newPiWorkflowComposition(s)
	if s.piWorkflows == nil || s.piWorkflows.runtime != reader {
		t.Fatal("different runtime composition")
	}
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	s.operationMu.Lock()
	delete(s.creations, slug)
	s.operationMu.Unlock()
	if native.ensures != 1 || native.creates != 1 || native.sends != 1 {
		t.Fatalf("initializer baseline: %d/%d/%d", native.ensures, native.creates, native.sends)
	}
	scope := piWorkflowNamespace(native.state.Conversation)
	dispatcher, err := s.piConversationHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Codex fallback: %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.conversation = dispatcher
	path := "/api/sessions/" + slug + "/thread"
	var mu sync.Mutex
	requests, unexpected := 0, 0
	serve := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		values := r.Header.Values("X-Workspace-Conversation-Scope")
		t.Logf("HTTP_BEFORE_GUARD method=%s path=%s scopePresent=%t scopeMatch=%t scopeCount=%d", r.Method, r.URL.RequestURI(), len(values) > 0, len(values) == 1 && values[0] == scope, len(values))
		if r.Method != http.MethodGet || r.URL.Path != path {
			mu.Lock()
			unexpected++
			mu.Unlock()
			http.Error(w, "unexpected fixture route", 500)
			return
		}
		response := httptest.NewRecorder()
		s.sessionAPI(response, r)
		t.Logf("HTTP_AFTER_GUARD status=%d body=%s", response.Code, func() string {
			if response.Code == 200 {
				return "controlled transcript"
			}
			return strings.TrimSpace(response.Body.String())
		}())
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}
	host := httptest.NewServer(http.HandlerFunc(serve))
	defer host.Close()
	t.Logf("FIXTURE_LISTENER %s PID=%d fakeEnsure/Create/Send=1/1/1", host.URL, os.Getpid())
	reset := func() {
		reader.mu.Lock()
		reader.reads = 0
		reader.binds = 0
		reader.verifies = 0
		reader.mu.Unlock()
		mu.Lock()
		requests = 0
		mu.Unlock()
	}
	evidence := func() (int, int, int) {
		t.Helper()
		reader.mu.Lock()
		reads, binds, verifies := reader.reads, reader.binds, reader.verifies
		reader.mu.Unlock()
		mu.Lock()
		count, bad := requests, unexpected
		mu.Unlock()
		t.Logf("AFTER_CONTROL requests=%d transcriptReads=%d binds=%d verifies=%d fakeEnsure/Create/Send=%d/%d/%d unexpected=%d", count, reads, binds, verifies, native.ensures, native.creates, native.sends, bad)
		if native.ensures != 1 || native.creates != 1 || native.sends != 1 || bad != 0 {
			t.Fatal("read/refusal had native or unlisted effects")
		}
		return count, reads, binds
	}
	for _, mode := range []string{"valid_scope", "matching_reconnect", "mismatching_saved_scope", "stale_page_scope", "unscoped_get_no_leak"} {
		t.Run(mode, func(t *testing.T) {
			reset()
			input, _ := json.Marshal(map[string]string{"origin": host.URL, "slug": slug, "scope": scope, "mode": mode})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, node, "pi_acceptance_followup_contract_test.cjs", string(input))
			output, runErr := command.CombinedOutput()
			t.Logf("ACTUAL_SOURCE_CONTROL\n%s", output)
			count, reads, _ := evidence()
			if runErr != nil {
				t.Fatalf("actual helper contract: %v", runErr)
			}
			switch mode {
			case "valid_scope", "matching_reconnect":
				if count != 2 || reads != 2 {
					t.Fatalf("expected exactly two original-scope reads: %d/%d", count, reads)
				}
			case "stale_page_scope":
				if count != 1 || reads != 0 {
					t.Fatalf("stale exchange retried/read: %d/%d", count, reads)
				}
			default:
				if count != 0 || reads != 0 {
					t.Fatalf("precondition/no-leak entered server: %d/%d", count, reads)
				}
			}
		})
	}
	for _, test := range []struct {
		name    string
		headers []string
		query   string
		status  int
	}{
		{"missing", nil, "", 409}, {"malformed", []string{"malformed"}, "", 409}, {"duplicate", []string{scope, scope}, "", 409},
		{"non_event_query", []string{scope}, "?conversationScope=" + url.QueryEscape(scope), 409},
		{"stale", []string{"pi-workflow-v1:" + strings.Repeat("f", 64)}, "", 404},
	} {
		t.Run("guard_"+test.name, func(t *testing.T) {
			reset()
			r := httptest.NewRequest(http.MethodGet, path+test.query, nil)
			for _, v := range test.headers {
				r.Header.Add("X-Workspace-Conversation-Scope", v)
			}
			response := httptest.NewRecorder()
			serve(response, r)
			count, reads, binds := evidence()
			if response.Code != test.status || count != 1 || reads != 0 || binds != 0 {
				t.Fatalf("guard: status=%d requests=%d reads=%d binds=%d body=%s", response.Code, count, reads, binds, response.Body.String())
			}
			expected := "The selected Pi conversation changed. Reload the session."
			if test.status == 404 {
				expected = "conversation is unavailable"
			}
			if !strings.Contains(response.Body.String(), expected) {
				t.Fatal("wrong refusal", response.Body.String())
			}
		})
	}
}
