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

	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
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
