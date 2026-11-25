package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func providerTestCatalog(t *testing.T, s *Server, endpoint string) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "private-token")
	if err := os.WriteFile(token, []byte("test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog := codexproviders.Catalog{Schema: 1, InitialProvider: "pool", Providers: []codexproviders.Provider{
		{ID: "openai", Label: "Subscription", Kind: "subscription"},
		{ID: "pool", Label: "Pool", Kind: "codex-lb", BaseURL: endpoint + "/v1", TokenFile: token},
	}}
	data, _ := json.Marshal(catalog)
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", path)
	s.config.WorkspaceName = "test-workspace"
	s.config.UserStateRoot = t.TempDir()
}

func TestProviderPreviewUsesIndependentLimitsAndHidesPrivateCatalog(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("invalid trusted upstream request")
		}
		_, _ = w.Write([]byte(`{"limits":[],"account_pool_usage":{"primary":75,"secondary":null}}`))
	}))
	defer upstream.Close()
	s := newTestServer(t)
	defer s.Close()
	providerTestCatalog(t, s, upstream.URL)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/codex-providers", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"active":"pool"`) {
		t.Fatalf("catalog unavailable: %d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{upstream.URL, "private-token", "test-secret", "tokenFile", "baseURL"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("private provider catalog exposed")
		}
	}
	snapshot, err := s.loadProviderLimits(context.Background(), "pool")
	if err != nil || len(snapshot.Windows) != 1 || snapshot.Windows[0].UsedPercent != 25 || snapshot.CanReset {
		t.Fatalf("unexpected pool limits: %#v %v", snapshot, err)
	}
	if _, err := s.loadProviderLimits(context.Background(), "pool"); err != nil || upstreamCalls != 1 {
		t.Fatal("provider cache did not coalesce reads")
	}
	view, err := s.providerView()
	if err != nil || view.Active != "pool" {
		t.Fatal("preview changed active selection")
	}
	for _, action := range []string{"switch", "cancel"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/codex-providers/"+action, strings.NewReader(`{}`))
		request.Header.Set("Origin", "https://foreign.example.test")
		s.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("foreign origin accepted: %d", response.Code)
		}
	}
}

func TestRootConversationProviderUsesScopedRegisteredHandler(t *testing.T) {
	for _, test := range []struct {
		provider, label string
		used            [2]int
	}{
		{"openai", "Subscription", [2]int{7, 42}},
		{"pool", "Pool", [2]int{25, 50}},
	} {
		t.Run(test.provider, func(t *testing.T) {
			poolReads, subscriptionReads := 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				poolReads++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("invalid trusted upstream request")
				}
				_, _ = w.Write([]byte(`{"limits":[],"account_pool_usage":{"primary":75,"secondary":50}}`))
			}))
			defer upstream.Close()
			s := newTestServer(t)
			defer s.Close()
			providerTestCatalog(t, s, upstream.URL)
			catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
			if err != nil {
				t.Fatal(err)
			}
			catalog.InitialProvider = test.provider
			catalogBytes, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(codexproviders.CatalogPath(), catalogBytes, 0644); err != nil {
				t.Fatal(err)
			}
			s.config.Codex = &limitsCodex{browserContractCodex: &browserContractCodex{}, read: func(context.Context) (codex.AccountRateLimits, error) {
				subscriptionReads++
				return codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{LimitID: "codex", Primary: limitWindow(10080, 42), Secondary: limitWindow(300, 7)}}, nil
			}}
			prepareInteractiveConversation(t, s, "example")
			request := func(method, path, origin string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, nil)
				r.Header.Set("Origin", origin)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				return w
			}
			alias := "/api/sessions/example/provider"
			canonical := "/codex/conversations/example/provider"
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				if w := request(method, alias, s.config.BaseURL); w.Code != http.StatusNotFound {
					t.Fatalf("%s provider alias = %d", method, w.Code)
				}
			}
			if w := request(http.MethodGet, alias, "https://foreign.example.test"); w.Code != http.StatusForbidden {
				t.Fatalf("foreign provider read = %d", w.Code)
			}
			if poolReads != 0 || subscriptionReads != 0 {
				t.Fatal("refused request reached a provider")
			}
			for _, path := range []string{alias, canonical} {
				w := request(http.MethodGet, path, "")
				var info backend.ProviderInfo
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil || info.ThreadID != "thread-1" || info.Generation != "" || info.ProviderID != test.provider || info.ProviderLabel != test.label || info.Usage.State != "available" || info.Usage.Metrics == nil || len(info.Usage.Metrics.Windows) != 2 {
					t.Fatalf("scoped provider %s = %d %s", path, w.Code, w.Body.String())
				}
				for i, duration := range []int64{300, 10080} {
					window := info.Usage.Metrics.Windows[i]
					if window.UsedPercent == nil || *window.UsedPercent != test.used[i] || window.WindowDurationMins == nil || *window.WindowDurationMins != duration {
						t.Fatalf("wrong scoped window %d: %#v", i, window)
					}
				}
				for _, private := range []string{upstream.URL, "private-token", "test-secret", "tokenFile", "baseURL"} {
					if strings.Contains(w.Body.String(), private) {
						t.Fatal("private provider catalog exposed")
					}
				}
			}
			if test.provider == "openai" && (subscriptionReads != 1 || poolReads != 0) || test.provider == "pool" && (poolReads != 1 || subscriptionReads != 0) {
				t.Fatalf("provider reads crossed selection: subscription=%d pool=%d", subscriptionReads, poolReads)
			}
			manifest := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
			data, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, []byte(strings.Replace(string(data), "thread-1", "wrong-thread", 1)), 0644); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{alias, canonical} {
				if w := request(http.MethodGet, path, ""); w.Code != http.StatusNotFound {
					t.Fatalf("changed thread accepted by %s: %d", path, w.Code)
				}
			}
			statePath, err := codexproviders.StatePath(s.config.UserStateRoot, s.config.WorkspaceName)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatal("provider read wrote selection state", err)
			}
			if data, err := os.ReadFile(codexproviders.CatalogPath()); err != nil || string(data) != string(catalogBytes) {
				t.Fatal("provider read changed catalog", err)
			}
		})
	}
}

func TestRootSubscriptionProviderPreservesCreditStates(t *testing.T) {
	finite, zero := "17.25", "0"
	for _, tc := range []struct {
		name        string
		credits     *codex.CreditsSnapshot
		unavailable bool
	}{
		{"finite", &codex.CreditsSnapshot{HasCredits: true, Balance: &finite}, false},
		{"zero_false_flag", &codex.CreditsSnapshot{Balance: &zero}, false},
		{"unlimited", &codex.CreditsSnapshot{HasCredits: true, Unlimited: true, Balance: &zero}, false},
		{"nil_balance", &codex.CreditsSnapshot{HasCredits: true}, false},
		{"nil_balance_false_flag", &codex.CreditsSnapshot{}, false},
		{"absent", nil, false},
		{"unavailable", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			providerTestCatalog(t, s, "https://unused.example.test")
			catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
			if err != nil {
				t.Fatal(err)
			}
			catalog.InitialProvider = "openai"
			data, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(codexproviders.CatalogPath(), data, 0644); err != nil {
				t.Fatal(err)
			}
			s.config.Codex = &limitsCodex{browserContractCodex: &browserContractCodex{}, read: func(context.Context) (codex.AccountRateLimits, error) {
				if tc.unavailable {
					return codex.AccountRateLimits{}, errors.New("private fixture failure")
				}
				return codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{LimitID: "codex", Primary: limitWindow(300, 7), Credits: tc.credits}}, nil
			}}
			prepareInteractiveConversation(t, s, "example")
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/example/provider", nil))
			var got backend.ProviderInfo
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil || got.ThreadID != "thread-1" || got.Generation != "" || got.ProviderID != "openai" {
				t.Fatal("scoped credit response identity")
			}
			if tc.unavailable {
				if got.Usage.State != "unavailable" || got.Usage.Metrics != nil || strings.Contains(response.Body.String(), "private") {
					t.Fatal("unavailable read invented credits or leaked failure")
				}
				return
			}
			if got.Usage.State != "available" || got.Usage.Metrics == nil || len(got.Usage.Metrics.Windows) != 1 {
				t.Fatal("credit projection changed usage windows")
			}
			var want *backend.ProviderCredits
			if tc.credits != nil {
				want = &backend.ProviderCredits{HasCredits: tc.credits.HasCredits, Unlimited: tc.credits.Unlimited, Balance: tc.credits.Balance}
			}
			if !reflect.DeepEqual(got.Usage.Metrics.Credits, want) || strings.Contains(response.Body.String(), `"credits"`) != (want != nil) {
				t.Fatal("native credit flags, exact balance or absence lost")
			}
		})
	}
}

func TestProviderReservationSurvivesCatalogRemoval(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	providerTestCatalog(t, s, "http://127.0.0.1:1")
	path, _ := codexproviders.StatePath(s.config.UserStateRoot, s.config.WorkspaceName)
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	state := codexproviders.State{Schema: 1, Workspace: s.config.WorkspaceName, Root: s.config.Workspace, Socket: s.config.CodexSocket, Active: "pool", Operation: &codexproviders.Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "pool", CatalogDigest: strings.Repeat("a", 64), Phase: "recovery-required"}}
	data, _ := json.Marshal(state)
	_ = os.WriteFile(path, data, 0600)
	if s.requireProviderAdmission() == nil {
		t.Fatal("reserved writes admitted")
	}
	if err := os.Remove(codexproviders.CatalogPath()); err != nil {
		t.Fatal(err)
	}
	if s.requireProviderAdmission() == nil {
		t.Fatal("removed catalog hid existing reservation")
	}
}

func TestProviderBindingCannotUseLegacyMissingTrackingAdmission(t *testing.T) {
	s, _, _, _, statePath := boundRecoveryFixture(t)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.config.Workspace, "work", "example")); err != nil {
		t.Fatal(err)
	}
	if err := s.requireProviderAdmission("example"); err == nil {
		t.Fatal("missing marked tracking obtained legacy admission")
	}
	after, err := os.ReadFile(statePath)
	if err != nil || string(after) != string(before) {
		t.Fatal("missing tracking changed provider state", err)
	}
}

func TestScopedProviderPauseDoesNotLeaveAutomaticRecoveryFailed(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	directory := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"state.md": "---\nlifecycle: active\n---\n", "plan.md": "Scoped switch fixture\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: thread-1\n  socket_path: " + s.config.CodexSocket + "\n  client_version: 0.163.0\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Find(s.config.Workspace, "example"); err != nil {
		t.Fatal(err)
	}
	providerTestCatalog(t, s, "http://127.0.0.1:1")
	path, _ := codexproviders.StatePath(s.config.UserStateRoot, s.config.WorkspaceName)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	state := codexproviders.State{Schema: 2, Workspace: s.config.WorkspaceName, Root: s.config.Workspace, Socket: s.config.CodexSocket, Active: "pool", Operation: &codexproviders.Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "openai", CatalogDigest: strings.Repeat("a", 64), Phase: "applying", Strategy: "scoped-v1", Session: "example", Stage: "preparing", Sessions: []string{"example"}, Roots: map[string]string{"example": "thread-1"}}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	s.restoreRuntime(context.Background(), "example")
	s.recoveryMu.Lock()
	status := s.recoveryOperations["example"]
	s.recoveryMu.Unlock()
	if status.State == "failed" || status.State == "recovering" {
		t.Fatalf("provider pause poisoned ordinary recovery: %#v", status)
	}
}

func TestCreationProviderSurvivesPreparationAndReceiptRestart(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	receipt := managedCreationReceiptForRestart(t, s, "new")
	receipt.Request.Provider = "openai"
	data, _ := json.Marshal(receipt)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if err := validateCreationReceiptShape(raw); err != nil {
		t.Fatal(err)
	}
	request := preparationCreationRequest(sessionPreparation{Slug: receipt.Request.Slug, Snapshot: &preparationSnapshot{Goal: receipt.Request.Goal, Input: preparationInput{Provider: "openai"}}})
	if request.Provider != "openai" {
		t.Fatal("provider selection lost at preparation handoff")
	}
	a, b := receipt.Request, receipt.Request
	b.Provider = "pool"
	if preparationDigest(a) == preparationDigest(b) {
		t.Fatal("provider is absent from accepted creation identity")
	}
}

func TestProviderTerminalTimingAndControlProjection(t *testing.T) {
	s := newTestServer(t)
	providerTestCatalog(t, s, "http://127.0.0.1:1")
	path, _ := codexproviders.StatePath(s.config.UserStateRoot, s.config.WorkspaceName)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	o := &codexproviders.Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "pool", CatalogDigest: strings.Repeat("a", 64), Phase: "failed", Strategy: "scoped-v1", Stage: "preparing", CreatedAt: "2026-10-18T17:16:58Z", UpdatedAt: "2026-10-18T17:17:04Z"}
	state := codexproviders.State{Schema: 2, Workspace: s.config.WorkspaceName, Root: s.config.Workspace, Socket: s.config.CodexSocket, Active: "pool", Operation: o}
	save := func() {
		data, _ := json.Marshal(state)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	progress := `{"operation":"` + o.ID + `","stage":"capturing-settings","threadLabel":"architect0","updatedAt":"2026-10-18T17:17:02Z","reason":"cold_permissions_unsupported"}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), o.ID+".snapshot.json.progress"), []byte(progress), 0600); err != nil {
		t.Fatal(err)
	}
	view, err := s.providerView()
	if err != nil {
		t.Fatal(err)
	}
	if view.Operation.FinishedAt != o.UpdatedAt || view.Operation.CanCancel || view.Operation.ThreadLabel != "architect0" {
		t.Fatalf("wrong terminal view: %#v", view.Operation)
	}
	o.Phase = "applying"
	save()
	view, err = s.providerView()
	if err != nil || !view.Operation.CanCancel {
		t.Fatalf("preparation cancellation: %#v %v", view.Operation, err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), o.ID+".control.json"), []byte(`{"operation":"`+o.ID+`","cancelRequested":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	view, err = s.providerView()
	if err != nil || view.Operation.CanCancel || !view.Operation.CancelRequested {
		t.Fatalf("pending cancellation: %#v %v", view.Operation, err)
	}
	o.Stage = "switching"
	save()
	view, err = s.providerView()
	if err != nil || view.Operation.CanCancel {
		t.Fatalf("cancel fence: %#v %v", view.Operation, err)
	}
}

func TestAcceptedForkModelsKeepDestinationPurposeAcrossWarmCacheAndSavedRecovery(t *testing.T) {
	server, _, _, _, statePath := boundRecoveryFixture(t)
	defer server.Close()
	selected, err := filepath.EvalSymlinks(server.config.HostProfile)
	if err != nil {
		t.Fatal(err)
	}
	server.config.PackageRoot, server.config.WorkspaceName = selected, "registered"
	write := func(path string, value any) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	contractPath := filepath.Join(selected, "share/workspace-portal/runtime-contract.json")
	contract := map[string]int{"codexProviderBindingStateSchema": 3, "codexProviderForkTransitionPolicy": 1, "codexRuntimeAuthoritySchema": 3}
	write(contractPath, contract)
	catalogPath := os.Getenv("DEV_WORKSPACES_CODEX_PROVIDERS")
	write(catalogPath, map[string]any{"schema": 1, "initialProvider": "openai", "providers": []any{map[string]string{"id": "openai", "label": "OpenAI", "kind": "subscription"}, map[string]string{"id": "pool", "label": "Pool", "kind": "codex-lb", "baseURL": "https://pool.test/v1", "tokenFile": "/not-read-token"}}})
	trace := filepath.Join(t.TempDir(), "models.trace")
	host := filepath.Join(selected, "bin/workspace-host")
	if err := os.MkdirAll(filepath.Dir(host), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + trace + "'\ncat <<'JSON'\n" + `[{"model":"saved-model","displayName":"Saved","supportedReasoningEfforts":[{"reasoningEffort":"high","description":"High"}]}]` + "\nJSON\n"
	if err := os.WriteFile(host, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/codex-providers?session=example", "/api/models?session=example", "/api/models?session=example&provider=pool"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !json.Valid(response.Body.Bytes()) {
			t.Fatal("ordinary marked provider/model route", path, response.Code, response.Body.String())
		}
	}
	contract["codexRuntimeAuthoritySchema"] = 2
	write(contractPath, contract)
	if _, err := server.modelsForSession(context.Background(), "example", "pool"); err == nil {
		t.Fatal("ordinary model warm cache bypassed package proof")
	}
	contract["codexRuntimeAuthoritySchema"] = 3
	write(contractPath, contract)
	source, identity, capture, err := server.creationForkSource(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := session.CompletedRemovalHistory(server.config.Workspace, "destination", server.config.UserStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, Request: creationRequest{Kind: "fork", Slug: "destination", Source: "example", SourceThreadID: source.Codex.ThreadID, SourceIdentity: identity, SourceFork: capture, Model: "saved-model", Effort: "high", Provider: "pool"}, ReceiptID: strings.Repeat("f", 64), DeletionHistorySHA256: epoch, Attempt: 1, State: "running", Phase: "accepted", StartedAt: "2026-11-25T12:00:00Z", UpdatedAt: "2026-11-25T12:00:00Z"}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	server.creations["destination"] = receipt
	assertModels := func() {
		t.Helper()
		models, err := server.modelsForAcceptedFork(context.Background(), receipt)
		if err != nil || len(models) != 1 || models[0].Model != "saved-model" {
			t.Fatal("accepted destination model lookup", models, err)
		}
	}
	assertModels()
	receipt.Validated, receipt.Model, receipt.Effort = true, "saved-model", "high"
	if err := server.updateCreation(receipt); err != nil {
		t.Fatal(err)
	}
	writeCreationBinding(t, server, receipt)
	journalPath := filepath.Join(server.config.Workspace, "worktrees", ".locks", "destination.fork.json")
	journal := map[string]any{"schema": 2, "slug": "destination", "source_slug": "example", "source_thread_id": source.Codex.ThreadID, "state": "creating", "tmux_identity": strings.Repeat("d", 64), "model": "saved-model", "effort": "high", "requested_model": "saved-model", "requested_effort": "high", "provider": "pool", "source_fork": capture, "root_fork": map[string]string{"state": "known", "threadId": "saved-root"}}
	write(journalPath, journal)
	var state codexproviders.State
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.Sessions = map[string]codexproviders.Selection{"destination": {Provider: "pool", CreationID: strings.Repeat("d", 64), RootThreadID: "saved-root"}}
	binding := state.Bindings["example"]
	binding.Revision = 3
	row := binding.Conversations["thread-1"]
	row.ThreadID = "later-source"
	binding.Conversations["thread-1"] = row
	state.BindingRevision = 3
	state.Bindings["example"] = binding
	write(statePath, state)
	archive := filepath.Join(server.config.Workspace, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(server.config.Workspace, "work", "example"), archive); err != nil {
		t.Fatal(err)
	}
	assertModels() // known recovery never resolves today's source or its authority
	if err := os.RemoveAll(archive); err != nil {
		t.Fatal(err)
	}
	assertModels()
	for _, fault := range []string{"capability", "attempt", "receipt", "provider", "journal", "fence"} {
		t.Run(fault, func(t *testing.T) {
			candidate := receipt
			switch fault {
			case "capability":
				contract["codexRuntimeAuthoritySchema"] = 2
				write(contractPath, contract)
				defer func() { contract["codexRuntimeAuthoritySchema"] = 3; write(contractPath, contract) }()
			case "attempt":
				candidate.Attempt++
			case "receipt":
				candidate.ReceiptID = strings.Repeat("e", 64)
			case "provider":
				candidate.Request.Provider = "openai"
			case "journal":
				write(journalPath, map[string]any{})
				defer write(journalPath, journal)
			case "fence":
				state.Operation = &codexproviders.Operation{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Target: "pool", CatalogDigest: strings.Repeat("e", 64), Phase: "applying"}
				write(statePath, state)
				defer func() { state.Operation = nil; write(statePath, state) }()
			}
			if _, err := server.modelsForAcceptedFork(context.Background(), candidate); err == nil {
				t.Fatal("wrong proof reached warm cache")
			}
		})
	}
	journal["root_fork"] = map[string]string{"state": "intent"}
	write(journalPath, journal)
	selection := state.Sessions["destination"]
	selection.RootThreadID = ""
	state.Sessions["destination"] = selection
	write(statePath, state)
	if _, err := server.modelsForAcceptedFork(context.Background(), receipt); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("intent did not stop before model lookup", err)
	}
	// Status persistence before any journal does not grant validation or effects.
	pending := receipt
	pending.Request.Slug = "failed-before-journal"
	pending.ReceiptID = strings.Repeat("9", 64)
	pending.Validated = false
	pending.Model = ""
	pending.Effort = ""
	pending.State = "running"
	if err := server.saveCreation(pending); err != nil {
		t.Fatal(err)
	}
	server.creations[pending.Request.Slug] = pending
	pending.State, pending.Error = "failed", "original admission failed"
	if err := server.updateCreation(pending); err != nil {
		t.Fatal("failure recording depended on absent source", err)
	}
	var saved creationReceipt
	if err := readCreationJSON(server.creationPath(pending.Request.Slug), &saved); err != nil || saved.Validated || saved.State != "failed" {
		t.Fatal(saved, err)
	}
	out, err := os.ReadFile(trace)
	if err != nil || strings.TrimSpace(string(out)) != "provider models registered openai\nprovider models registered pool" {
		t.Fatal("warm-cache admission launched another provider", string(out), err)
	}
}
