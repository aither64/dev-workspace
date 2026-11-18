package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func piProviderInfoFixture(t *testing.T, endpoint string) (*Server, piproviders.PackageConfiguration, string, backend.Identity) {
	t.Helper()
	s := newTestServer(t)
	t.Cleanup(s.Close)
	providerTestCatalog(t, s, endpoint)
	// The member uses the pool while the workspace's native default is OpenAI.
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		t.Fatal(err)
	}
	catalog.InitialProvider = "openai"
	catalogBytes, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(codexproviders.CatalogPath(), catalogBytes, 0644); err != nil {
		t.Fatal(err)
	}
	s.config.PiCreation = &PiCreationServices{AuthDirectory: t.TempDir()}
	if err := os.Chmod(s.config.PiCreation.AuthDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.config.PiCreation.AuthDirectory, "auth.json"), []byte(`{"openai":{"type":"api_key","key":"test-secret"},"other":{"type":"oauth","access":"synthetic-other"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	app := t.TempDir()
	dir := filepath.Join(app, "share/dev-workspace")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	models, _ := json.Marshal(map[string]any{"providers": map[string]any{"openai": map[string]any{"api": "openai-responses", "baseUrl": endpoint + "/v1", "modelOverrides": map[string]any{"model": map[string]any{"samplingParams": map[string]any{"parallel_tool_calls": false}}}}}})
	if err := os.WriteFile(filepath.Join(dir, "pi-models.json"), models, 0600); err != nil {
		t.Fatal(err)
	}
	frozen := piproviders.PackageConfiguration{Catalog: piproviders.Catalog{
		Providers: []piproviders.Provider{{ID: "frozen-route", Label: "Frozen route", NativeID: "openai", RuntimeConfigRef: "builtin"}},
		Accounts:  []piproviders.Account{{Ref: "member-account", ProviderID: "frozen-route", CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: "openai", AuthKind: "api_key"}},
	}}
	return s, frozen, app, backend.Identity{ConversationID: "member-thread", Generation: "generation-1"}
}

func TestPiProviderInfoMatchesMemberCredentialBeforeCachedUsage(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/usage" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("wrong scoped metrics request")
		}
		_, _ = w.Write([]byte(`{"limits":[],"request_count":0}`))
	}))
	defer upstream.Close()
	s, frozen, app, id := piProviderInfoFixture(t, upstream.URL)
	_, global, globalErr := s.providerState()
	if globalErr != nil || global.Active != "openai" {
		t.Fatal("fixture lacks explicit global/member divergence", globalErr)
	}
	settings := piruntime.EffectiveSettings{Provider: "openai", AccountRef: "member-account", Model: "model"}
	result, err := s.piProviderInfo(context.Background(), id, settings, frozen, app)
	if err != nil || result.ProviderID != "pool" || result.AccountRef != "member-account" || result.Usage.State != "available" || calls != 1 {
		t.Fatalf("scoped read: %+v %v calls%d", result, err, calls)
	}
	if result.Usage.Metrics.Requests == nil || *result.Usage.Metrics.Requests != 0 || result.Usage.Metrics.Tokens != nil {
		t.Fatal("absence and zero collapsed")
	}
	if _, err = s.piProviderInfo(context.Background(), id, settings, frozen, app); err != nil || calls != 1 {
		t.Fatal("exact binding did not reuse cache", err)
	}
	auth := filepath.Join(s.config.PiCreation.AuthDirectory, "auth.json")
	if err = os.WriteFile(auth, []byte(`{"openai":{"type":"api_key","key":"other-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = s.piProviderInfo(context.Background(), id, settings, frozen, app)
	if err != nil || result.Usage.State != "unavailable" || result.Usage.Metrics != nil || calls != 1 || result.AccountRef != "member-account" {
		t.Fatal("mismatched credential received cached other-account usage", err)
	}
	// Host token rotation also retires the prior cache entry before any return.
	if err = os.WriteFile(auth, []byte(`{"openai":{"type":"api_key","key":"test-secret"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		t.Fatal(err)
	}
	route, _ := catalog.Find("pool")
	if err = os.WriteFile(route.TokenFile, []byte("rotated-host-token"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = s.piProviderInfo(context.Background(), id, settings, frozen, app)
	if err != nil || result.Usage.State != "unavailable" || calls != 1 {
		t.Fatal("host credential rotation reused stale binding", err)
	}
	data, _ := json.Marshal(result)
	for _, private := range []string{auth, route.TokenFile, upstream.URL, "test-secret", "rotated-host-token", "credentialKey", "baseURL", "tokenFile"} {
		if strings.Contains(string(data), private) {
			t.Fatal("private input escaped public response")
		}
	}
}

func TestPiProviderInfoQuotaFailureAndAmbiguousRouteRetainFrozenIdentity(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer upstream.Close()
	s, frozen, app, id := piProviderInfoFixture(t, upstream.URL)
	settings := piruntime.EffectiveSettings{Provider: "openai", AccountRef: "member-account", Model: "model"}
	result, err := s.piProviderInfo(context.Background(), id, settings, frozen, app)
	if err != nil || result.ProviderID != "pool" || result.AccountRef != "member-account" || result.Usage.State != "unavailable" || result.Usage.Metrics != nil || calls != 1 {
		t.Fatal("quota failure erased identity", err)
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		t.Fatal(err)
	}
	duplicate := catalog.Providers[1]
	duplicate.ID = "duplicate-route"
	catalog.Providers = append(catalog.Providers, duplicate)
	data, _ := json.Marshal(catalog)
	if err = os.WriteFile(codexproviders.CatalogPath(), data, 0644); err != nil {
		t.Fatal(err)
	}
	result, err = s.piProviderInfo(context.Background(), id, settings, frozen, app)
	if err != nil || result.ProviderID != "frozen-route" || result.AccountRef != "member-account" || result.Usage.State != "unavailable" || calls != 1 {
		t.Fatal("ambiguous route borrowed global identity", err)
	}
	// No matching endpoint also keeps declared identity without HTTP.
	frozen.Catalog.Providers[0].NativeID = "other-native"
	settings.Provider = "other-native"
	result, err = s.piProviderInfo(context.Background(), id, settings, frozen, app)
	if err != nil || result.ProviderID != "frozen-route" || result.Usage.State != "unsupported" || calls != 1 {
		t.Fatal("unsupported native adapter borrowed global route", err)
	}
}

func TestPiLiteralProviderCredentialStrictShapeAndSDK(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "auth.json")
	valid := `{"openai":{"type":"api_key","key":"synthetic-key"},"other":{"type":"oauth","access":"never-used"}}`
	for _, bad := range []string{
		`{"openai":{"type":"oauth","key":"synthetic-key"}}`,
		`{"openai":{"type":"api_key","key":"!echo synthetic-key"}}`,
		`{"openai":{"type":"api_key","key":"${SYNTHETIC_KEY}"}}`,
		`{"openai":{"type":"api_key","key":"synthetic-key","env":{}}}`,
		`{"openai":{"type":"api_key"}}`,
		`{"openai":{"type":"api_key","key":"x","key":"y"}}`,
		`{"openai":null}`, `{}`, valid + `{}`, `[]`,
	} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readPiLiteralKey(dir, "openai"); err == nil {
			t.Fatal("unsupported credential accepted")
		}
	}
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := readPiLiteralKey(dir, "openai")
	if err != nil || key != "synthetic-key" {
		t.Fatal("literal credential unavailable", err)
	}
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	t.Run("pinned_sdk", func(t *testing.T) {
		pi := os.Getenv("PI_PROBE_PACKAGE")
		if pi == "" {
			t.Skip("requires pinned PI_PROBE_PACKAGE")
		}
		script := `import {pathToFileURL} from "node:url"; const {readStoredCredential}=await import(pathToFileURL(process.argv[1])); process.stdout.write(JSON.stringify(readStoredCredential("openai",process.argv[2])));`
		module := filepath.Join(pi, "lib/pi/node_modules/@earendil-works/pi-coding-agent/dist/core/auth-storage.js")
		output, err := exec.Command("node", "--input-type=module", "-e", script, module, path).CombinedOutput()
		if err != nil {
			t.Fatalf("public SDK literal reader failed: %v %s", err, output)
		}
		var selected struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		if json.Unmarshal(output, &selected) != nil || selected.Type != "api_key" || selected.Key != key {
			t.Fatal("owning SDK raw shape differs")
		}
	})
	after, _ := os.ReadFile(path)
	current, _ := os.Stat(path)
	if string(before) != string(after) || !current.ModTime().Equal(info.ModTime()) || current.Mode() != info.Mode() {
		t.Fatal("read mutated credential fixture")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPiLiteralKey(dir, "openai"); err == nil {
		t.Fatal("public credential file accepted")
	}
}

func TestPiScopedProviderHTTPUsesObservedFrozenIdentityAndWritesNothing(t *testing.T) {
	s, f, handler, scope, policyPath := piSettingsHTTPFixture(t)
	defer s.Close()
	before := f.state.Settings
	creates, sends, configures := f.creates, f.sends, f.configures
	slug := f.state.Conversation.Session.Slug
	s.operationMu.Lock()
	delete(s.creations, slug)
	s.operationMu.Unlock()
	page := httptest.NewRecorder()
	s.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/"+slug+"/", nil))
	carrier := regexp.MustCompile(`data-thread-id="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || len(carrier) != 2 || carrier[1] != f.state.Conversation.NativeID || !strings.Contains(page.Body.String(), `data-conversation-scope="`+scope+`"`) {
		t.Fatalf("production provider carrier: %d %s", page.Code, page.Body.String())
	}

	request := func(selectedScope string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/codex/conversations/"+f.state.Conversation.Session.Slug+"/provider", nil)
		r.Header.Set("X-Workspace-Conversation-Scope", selectedScope)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	response := request(scope)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var info backend.ProviderInfo
	if json.Unmarshal(response.Body.Bytes(), &info) != nil || info.ProviderID != "host" || info.AccountRef != "account" || info.ThreadID != carrier[1] || info.Generation != f.state.Generation || info.Usage.State != "unsupported" {
		t.Fatalf("wrong observed identity: %s", response.Body.String())
	}
	if request("").Code == 200 || request("pi-workflow-v1:"+strings.Repeat("f", 64)).Code == 200 {
		t.Fatal("stale scope accepted")
	}
	if !reflect.DeepEqual(before, f.state.Settings) || creates != f.creates || sends != f.sends || configures != f.configures {
		t.Fatal("provider GET performed native mutation")
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) {
		t.Fatal("provider GET wrote policy", err)
	}
}
