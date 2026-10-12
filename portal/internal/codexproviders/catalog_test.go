package codexproviders

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
