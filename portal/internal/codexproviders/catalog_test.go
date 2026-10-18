package codexproviders

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestScopedReservationsPreserveOtherSessionsAndExactRootSelections(t *testing.T) {
	state := State{Schema: 2, Active: "openai", Sessions: map[string]Selection{"explicit": {Provider: "pool", RootThreadID: "explicit-root"}}, Operation: &Operation{Strategy: "scoped-v1", Session: "one", Phase: "applying"}}
	if !state.BlocksSession("one", "root-one") || state.BlocksSession("two", "root-two") {
		t.Fatal("session reservation leaked into unrelated work")
	}
	state.Operation.Session = ""
	if !state.BlocksSession("one", "root-one") || state.BlocksSession("explicit", "explicit-root") || !state.BlocksSession("explicit", "replacement-root") {
		t.Fatal("workspace default did not respect exact-root overrides")
	}
	if state.Provider("explicit", "explicit-root") != "pool" || state.Provider("explicit", "replacement-root") != "openai" {
		t.Fatal("replacement session inherited a stale provider override")
	}
	state.Operation.Strategy = ""
	if !state.BlocksSession("explicit", "explicit-root") {
		t.Fatal("legacy global reservation was weakened")
	}
}

func TestModelPreflightDoesNotDependOnQuotaMetrics(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Errorf("unexpected provider preflight %s", r.URL.Path)
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
	}))
	defer server.Close()
	provider := Provider{ID: "pool", Kind: "codex-lb", BaseURL: server.URL + "/v1", TokenFile: testToken(t)}
	if err := ProbeModels(context.Background(), provider, []string{"fixture-model"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("preflight issued %d requests", calls)
	}
	if err := ProbeModels(context.Background(), provider, []string{"missing-model"}); err == nil {
		t.Fatal("missing model accepted")
	}
}

func TestCatalogRejectsAmbiguousAndBrowserUnsafeDefinitions(t *testing.T) {
	sub := Provider{ID: "openai", Label: "Subscription", Kind: "subscription"}
	pool := Provider{ID: "pool", Label: "Pool", Kind: "codex-lb", BaseURL: "http://127.0.0.1:2455/v1", TokenFile: "/private/token"}
	for _, test := range []struct {
		name   string
		change func(*Catalog)
	}{
		{"duplicate", func(c *Catalog) { c.Providers = append(c.Providers, c.Providers[1]) }},
		{"endpoint credentials", func(c *Catalog) { c.Providers[1].BaseURL = "http://secret:token@example/v1" }},
		{"relative token", func(c *Catalog) { c.Providers[1].TokenFile = "private/token" }},
		{"unknown selection", func(c *Catalog) { c.InitialProvider = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Catalog{Schema: 1, InitialProvider: "pool", Providers: []Provider{sub, pool}}
			test.change(&c)
			data, _ := json.Marshal(c)
			path := filepath.Join(t.TempDir(), "catalog")
			_ = os.WriteFile(path, data, 0644)
			if _, err := ReadCatalog(path); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}

func TestProviderStateRejectsWrongWorkspaceAndPreservesReservation(t *testing.T) {
	root := t.TempDir()
	catalog := Catalog{InitialProvider: "openai", Providers: []Provider{{ID: "openai"}}}
	path, _ := StatePath(root, "workspace")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	state := State{Schema: 1, Workspace: "workspace", Root: "/owned/workspace", Socket: "/owned/socket", Active: "openai", Operation: &Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "openai", CatalogDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Phase: "recovery-required"}, Receipts: map[string]RequestReceipt{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa": {Target: "openai", CatalogDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}
	data, _ := json.Marshal(state)
	_ = os.WriteFile(path, data, 0600)
	loaded, err := ReadState(root, "workspace", "/owned/workspace", "/owned/socket", catalog)
	if err != nil || !loaded.Operation.Reserved() {
		t.Fatalf("reservation=%#v err=%v", loaded, err)
	}
	if loaded.Receipts[state.Operation.ID].Target != "openai" {
		t.Fatal("atomic operation receipt was lost")
	}
	if _, err := ReadState(root, "workspace", "/another/workspace", "/owned/socket", catalog); err == nil {
		t.Fatal("foreign workspace state accepted")
	}
}

func TestScopedStateRejectsInvalidStageAndRootScope(t *testing.T) {
	stateRoot := t.TempDir()
	catalog := Catalog{Providers: []Provider{{ID: "openai"}}}
	path, _ := StatePath(stateRoot, "workspace")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Operation)
	}{
		{"unknown stage", func(o *Operation) { o.Stage = "unknown" }},
		{"accepted but reserved", func(o *Operation) { o.Stage = "accepted" }},
		{"missing root", func(o *Operation) { o.Roots = nil }},
		{"duplicate scope", func(o *Operation) { o.Sessions = []string{"one", "one"} }},
		{"different session", func(o *Operation) { o.Session = "two" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := State{Schema: 2, Workspace: "workspace", Root: "/workspace", Socket: "/native.sock", Active: "openai", Operation: &Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "openai", CatalogDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Phase: "applying", Strategy: "scoped-v1", Stage: "preparing", Session: "one", Sessions: []string{"one"}, Roots: map[string]string{"one": "root-one"}}}
			good, _ := json.Marshal(state)
			if err := os.WriteFile(path, good, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadState(stateRoot, "workspace", "/workspace", "/native.sock", catalog); err != nil {
				t.Fatal(err)
			}
			test.change(state.Operation)
			data, _ := json.Marshal(state)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadState(stateRoot, "workspace", "/workspace", "/native.sock", catalog); err == nil {
				t.Fatal("invalid scoped state accepted")
			}
		})
	}
}
