package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type piSettingsFixture struct {
	*piObservedFixture
	configures    int
	outcome       string
	pending, held bool
}

func (f *piSettingsFixture) Bind(_ context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	if h.Conversation != f.state.Conversation || h.Generation != f.state.Generation {
		return backend.Operations{}, piruntime.ErrScope
	}
	return backend.Operations{Verify: f, Read: f, Send: f.piCreationRuntimeFixture}, nil
}
func (f *piSettingsFixture) Verify(_ context.Context, id backend.Identity) error {
	if id != piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: f.state.Conversation, Generation: f.state.Generation}) {
		return piruntime.ErrScope
	}
	return nil
}
func (f *piSettingsFixture) Read(_ context.Context, _ backend.Identity) (backend.Transcript, error) {
	return backend.Transcript{ThreadID: f.state.Conversation.NativeID, Status: "idle", Model: f.state.Settings.Model, ReasoningEffort: f.state.Settings.Thinking, Entries: []backend.TranscriptEntry{}}, nil
}

func (f *piSettingsFixture) ObserveWorkflow(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.WorkflowObservation, error) {
	o, err := f.piObservedFixture.ObserveWorkflow(ctx, h)
	o.Pending, o.Held = f.pending, f.held
	return o, err
}
func (f *piSettingsFixture) Configure(_ context.Context, _ agentbackend.ExecutionHandle, settings piruntime.EffectiveSettings) (piruntime.SettingsResult, error) {
	f.configures++
	result := piruntime.SettingsResult{Requested: settings, Effective: f.state.Settings, Outcome: f.outcome}
	if f.outcome == "applied" {
		f.state.Settings = settings
		result.Effective = settings
	}
	return result, nil
}

func piSettingsHTTPFixture(t *testing.T) (*Server, *piSettingsFixture, http.Handler, string, string) {
	t.Helper()
	s, receipt, native := newPiCreationFixtureForSlug(t, "2026-11-18-settings-fixture")
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	f := &piSettingsFixture{piObservedFixture: &piObservedFixture{piCreationRuntimeFixture: native, idle: true, current: native.runtime}, outcome: "applied"}
	s.config.PiCreation.Runtime = f
	s.piWorkflows.runtime = f
	ready := make(chan struct{})
	close(ready)
	s.piWorkflows.observations.attachments[native.state.Conversation.Session] = &piWorkflowAttachment{runtime: native.runtime, attachment: "attachment", sequence: 2, initial: 1, connected: true, readyClosed: true, ready: ready, finished: make(chan struct{}), cancel: func() {}}
	c := piCreationCatalogFixture()
	c.Providers[0].RuntimeConfigRef = "builtin"
	c.Models = append(c.Models, piproviders.Model{Kind: "physical", ProviderID: "host", ID: "other-model", Label: "Other model", ThinkingLevels: []string{"low", "high"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture"})
	c.Routes = append(c.Routes, piproviders.RouteGate{ProviderID: "host", AccountRef: "account", ModelID: "other-model", State: "available", ReasonCode: "verified", EvidenceRef: "fixture"})
	p := piproviders.PackageConfiguration{Catalog: c, Default: receipt.Pi.Selection.Policy.Default.Selection}
	composition, err := newPiProviderComposition(p, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	s.config.PiCreation.providers = composition
	data, _ := json.Marshal(map[string]any{"schema": 1, "catalog": c, "default": p.Default})
	if err := os.MkdirAll(filepath.Join(s.config.PackageRoot, "share/dev-workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.config.PackageRoot, "share/dev-workspace/pi-providers.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	handler, err := s.piConversationHandler(http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	root, err := piruntime.SessionStateDirectory(s.config.UserStateRoot, native.state.Conversation.Session)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, handler, piWorkflowNamespace(native.state.Conversation), filepath.Join(root, "provider-policy.json")
}

func TestPiSettingsHTTPUsesActualPairAndExplicitPolicySave(t *testing.T) {
	s, f, handler, scope, policyPath := piSettingsHTTPFixture(t)
	request := func(method, operation, body, selectedScope string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/codex/conversations/"+f.state.Conversation.Session.Slug+"/"+operation, strings.NewReader(body))
		r.Header.Set("X-Workspace-Conversation-Scope", selectedScope)
		if method == "POST" {
			r.Header.Set("Origin", s.config.BaseURL)
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	before := f.state.Settings
	creates, sends := f.creates, f.sends
	models := request("GET", "models", "", scope)
	if models.Code != 200 || !strings.Contains(models.Body.String(), `"other-model"`) || !strings.Contains(models.Body.String(), `"low"`) {
		t.Fatal(models.Code, models.Body.String())
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) || f.configures != 0 {
		t.Fatal("GET wrote desired/native state", err)
	}
	for _, body := range []string{`{"model":"foreign","reasoningEffort":"high"}`, `{"model":"other-model","reasoningEffort":"max"}`, `{"model":"other-model","reasoningEffort":"low","collaborationMode":"plan"}`} {
		if w := request("POST", "settings", body, scope); w.Code == 200 {
			t.Fatal("invalid pair/mode accepted", body)
		}
	}
	if w := request("POST", "settings", `{"model":"other-model","reasoningEffort":"low"}`, ""); w.Code == 200 {
		t.Fatal("missing scope accepted")
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) || f.configures != 0 {
		t.Fatal("refusal wrote policy", err)
	}
	for _, state := range []string{"active", "pending", "held"} {
		f.idle = state != "active"
		f.pending = state == "pending"
		f.held = state == "held"
		if w := request("POST", "settings", `{"model":"other-model","reasoningEffort":"low"}`, scope); w.Code == 200 {
			t.Fatal("busy accepted", state)
		}
	}
	f.idle = true
	f.pending = false
	f.held = false
	if w := request("POST", "settings", `{"model":"model","reasoningEffort":"high"}`, scope); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) || f.configures != 0 {
		t.Fatal("actual no-op wrote policy", err)
	}
	w := request("POST", "settings", `{"model":"other-model","reasoningEffort":"low"}`, scope)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"other-model"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.configures != 1 || f.state.Settings.Revision != before.Revision+1 || f.state.Settings.PolicyRevision == before.PolicyRevision || f.state.Settings.AccountRef != before.AccountRef || f.state.Settings.Provider != before.Provider {
		t.Fatal("wrong effective provenance", f.state.Settings)
	}
	root := filepath.Dir(policyPath)
	store := piproviders.Store{Root: root, Catalog: s.config.PiCreation.providers.configuration.Catalog, Session: f.state.Conversation.Session}
	policy, err := store.Load()
	if err != nil || policy.Revision != 2 || len(policy.Overrides) != 1 {
		t.Fatal(policy, err)
	}
	if policy.Default.Selection.Chain[0].ModelID != "model" || policy.Overrides[0].Value.Selection.Chain[0].ModelID != "other-model" {
		t.Fatal("changed default instead of exact member", policy)
	}
	if w := request("GET", "models", "", scope); w.Code != 200 || f.configures != 1 {
		t.Fatal("read reapplied desired", w.Code)
	}
	if f.creates != creates || f.sends != sends {
		t.Fatal("settings created/sent input")
	}
}

type piCatalogObserverFixture struct {
	*piSettingsFixture
	catalogReads, ordinaryReads, rechecks int
	fault                                 string
}

func (f *piCatalogObserverFixture) ObserveWorkflow(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.WorkflowObservation, error) {
	f.ordinaryReads++
	return f.piSettingsFixture.ObserveWorkflow(ctx, h)
}

func (f *piCatalogObserverFixture) ObserveCatalogWorkflow(ctx context.Context, h agentbackend.ExecutionHandle, recheck func(context.Context) error) (piruntime.WorkflowObservation, error) {
	f.catalogReads++
	if f.fault == "unavailable" {
		return piruntime.WorkflowObservation{}, piruntime.ErrUnavailable
	}
	// The real private resample is tested through the runtime's Unix
	// socket. This fixture checks how HTTP invokes it and binds rechecks.
	if f.fault == "stale" {
		f.current.Generation = strings.Repeat("f", 64)
	}
	f.rechecks++
	if err := recheck(ctx); err != nil {
		return piruntime.WorkflowObservation{}, err
	}
	return f.piSettingsFixture.ObserveWorkflow(ctx, h)
}

func TestPiSettingsModelsOnlyOptInToCatalogObservation(t *testing.T) {
	s, base, handler, scope, path := piSettingsHTTPFixture(t)
	f := &piCatalogObserverFixture{piSettingsFixture: base}
	s.config.PiCreation.Runtime, s.piWorkflows.runtime = f, f
	p := s.config.PiCreation.providers.configuration
	for i := range p.Catalog.Models {
		if p.Catalog.Models[i].ID == "model" {
			p.Catalog.Models[i].ThinkingLevels = []string{"high", "xhigh"}
		}
	}
	data, err := json.Marshal(map[string]any{"schema": 1, "catalog": p.Catalog, "default": p.Default})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.config.PackageRoot, "share/dev-workspace/pi-providers.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	request := func(method, op, selectedScope, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/codex/conversations/"+f.state.Conversation.Session.Slug+"/"+op, strings.NewReader(body))
		r.Header.Set("X-Workspace-Conversation-Scope", selectedScope)
		if method == http.MethodPost {
			r.Header.Set("Origin", s.config.BaseURL)
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	unchanged := func() {
		t.Helper()
		if _, err := os.Stat(path); !os.IsNotExist(err) || f.configures != 0 || f.state.Settings.Thinking != "high" {
			t.Fatal("unexpected settings effect", err)
		}
	}
	creates, sends := f.creates, f.sends
	f.idle = false
	if w := request("GET", "models", scope, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"xhigh"`) || f.catalogReads != 1 || f.ordinaryReads != 0 || f.rechecks != 1 {
		t.Fatal("active catalog request did not return the current model options", w.Code, w.Body.String())
	}
	for _, fault := range []string{"unavailable", "stale"} {
		f.fault = fault
		if w := request("GET", "models", scope, ""); w.Code != 503 || strings.Contains(w.Body.String(), `"xhigh"`) || f.ordinaryReads != 0 {
			t.Fatal("failed catalog path returned options or fell back to ordinary observe", fault, w.Code, w.Body.String())
		}
		f.current = f.runtime
	}
	f.fault = ""
	if w := request("GET", "models", scope+"-wrong", ""); w.Code == 200 || f.catalogReads != 3 || f.rechecks != 2 {
		t.Fatal("wrong scope reached catalog", w.Code)
	}
	unchanged()
	body := `{"model":"model","reasoningEffort":"xhigh"}`
	for _, mode := range []string{"active", "pending", "held", "wrong-scope"} {
		f.idle = mode != "active"
		f.pending, f.held = mode == "pending", mode == "held"
		selectedScope := scope
		if mode == "wrong-scope" {
			selectedScope += "-wrong"
		}
		if w := request("POST", "settings", selectedScope, body); w.Code == 200 || f.catalogReads != 3 {
			t.Fatal("settings mutation used catalog resample or was not refused", mode, w.Code)
		}
		unchanged()
	}
	if w := request("POST", "settings", scope, body); w.Code != 200 || f.catalogReads != 3 || f.ordinaryReads == 0 || f.configures != 1 || f.state.Settings.Thinking != "xhigh" || f.creates != creates || f.sends != sends {
		t.Fatal("idle explicit settings save changed behavior", w.Code, w.Body.String())
	}
}

func TestPiSettingsModelsIntersectCurrentWorkerPackage(t *testing.T) {
	s, f, handler, scope, policyPath := piSettingsHTTPFixture(t)
	portal := s.config.PiCreation.providers
	newModel := portal.configuration.Catalog.Models[1]
	newModel.ID = "portal-only-model"
	portal.configuration.Catalog.Models = append(portal.configuration.Catalog.Models, newModel)
	route := portal.configuration.Catalog.Routes[1]
	route.ModelID = newModel.ID
	portal.configuration.Catalog.Routes = append(portal.configuration.Catalog.Routes, route)
	r := httptest.NewRequest(http.MethodGet, "/codex/conversations/"+f.state.Conversation.Session.Slug+"/models", nil)
	r.Header.Set("X-Workspace-Conversation-Scope", scope)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), newModel.ID) || !strings.Contains(w.Body.String(), "other-model") {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.configures != 0 {
		t.Fatal("read configured worker")
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) {
		t.Fatal("read created desired policy", err)
	}
}

func TestPiSettingsHTTPUncertainDesiredNeverBecomesActual(t *testing.T) {
	for _, outcome := range []string{"rejected", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			s, f, h, scope, policyPath := piSettingsHTTPFixture(t)
			f.outcome = outcome
			before := f.state.Settings
			r := httptest.NewRequest("POST", "/codex/conversations/"+f.state.Conversation.Session.Slug+"/settings", strings.NewReader(`{"model":"other-model","reasoningEffort":"low"}`))
			r.Header.Set("Origin", s.config.BaseURL)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Workspace-Conversation-Scope", scope)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code == 200 || f.configures != 1 || f.state.Settings != before {
				t.Fatal(w.Code, f.configures, f.state.Settings)
			}
			if _, err := os.Stat(policyPath); err != nil {
				t.Fatal("desired policy not retained", err)
			}
			r = httptest.NewRequest("GET", "/codex/conversations/"+f.state.Conversation.Session.Slug+"/models", nil)
			r.Header.Set("X-Workspace-Conversation-Scope", scope)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || f.configures != 1 || f.state.Settings != before {
				t.Fatal("read retried desired", w.Code, f.configures)
			}
		})
	}
}

func TestPiSettingsMissingPolicyCannotRepairChangedRuntime(t *testing.T) {
	s, f, h, scope, path := piSettingsHTTPFixture(t)
	f.state.Settings.Revision++
	r := httptest.NewRequest("POST", "/codex/conversations/"+f.state.Conversation.Session.Slug+"/settings", strings.NewReader(`{"model":"other-model","reasoningEffort":"low"}`))
	r.Header.Set("Origin", s.config.BaseURL)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Workspace-Conversation-Scope", scope)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 200 || f.configures != 0 {
		t.Fatal("missing desired repaired unexplained runtime", w.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing desired was initialized", err)
	}
}
