package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func piProviderCommandFixture(t *testing.T) (string, string) {
	t.Helper()
	// Deliberate raw formatting and an escaped label prove that the command
	// retains the loader's exact catalog bytes instead of re-encoding them.
	catalog := `{
  "schema":1,
  "providers":[{"id":"a","label":"Provider \u0041","native_id":"native-a","runtime_config_ref":"builtin"}],
  "accounts":[{"ref":"account-a","provider_id":"a","credential_ref":"canonical","credential_key":"native-a","auth_kind":"api_key"}],
  "models":[{"kind":"physical","provider_id":"a","id":"model-a","label":"Model A","thinking_levels":["off"],"inputs":["text"],"tools":true,"evidence_ref":"fixture-model"}],
  "routes":[{"provider_id":"a","account_ref":"account-a","model_id":"model-a","state":"available","reason_code":"fixture","evidence_ref":"fixture-route"}]
}`
	selection := `{"chain":[{"provider_id":"a","account_ref":"account-a","model_id":"model-a","thinking":"off"}],"budget":{"max_extra_attempts":0,"max_hops":0},"fallback_on":[]}`
	root := t.TempDir()
	path := filepath.Join(root, "share/dev-workspace/pi-providers.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":1,"catalog":`+catalog+`,"default":`+selection+`}`), 0444); err != nil {
		t.Fatal(err)
	}
	return root, catalog
}

func TestPiProvidersValidateReadOnlyAndExactDefault(t *testing.T) {
	root, catalog := piProviderCommandFixture(t)
	configuration, err := loadServePiProviderConfiguration(root)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(catalog))
	if configuration == nil || configuration.Catalog.Digest != hex.EncodeToString(digest[:]) || configuration.Default.Chain[0].ModelID != "model-a" || configuration.Catalog.Providers[0].Label != "Provider A" {
		t.Fatal("command loading lost raw digest or explicit default")
	}
	// No auth, host, workspace or worker configuration exists in this package.
	// Validation must succeed without constructing or launching a runtime.
	if err := piProvidersCommand([]string{"validate", "--package-root", root}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "share" {
		t.Fatal("read-only validation created state")
	}
}

func TestPiProvidersValidateMissingMalformedAndUnsupported(t *testing.T) {
	missing := t.TempDir()
	if configuration, err := loadServePiProviderConfiguration(missing); err != nil || configuration != nil {
		t.Fatalf("ordinary missing configuration must disable only Pi: %v", err)
	}
	if err := piProvidersCommand([]string{"validate", "--package-root", missing}); err == nil {
		t.Fatal("configured install check accepted missing configuration")
	}
	for _, replacement := range []struct{ old, new string }{
		{`"schema":1`, `"schema":2`},
		{`"runtime_config_ref":"builtin"`, `"runtime_config_ref":"unsupported"`},
		{`"state":"available"`, `"state":"unverified"`},
		{`"thinking":"off"`, `"thinking":"high"`},
	} {
		root, _ := piProviderCommandFixture(t)
		path := filepath.Join(root, "share/dev-workspace/pi-providers.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(data), replacement.old, replacement.new, 1)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := piProvidersCommand([]string{"validate", "--package-root", root}); err == nil {
			t.Fatalf("invalid configured input accepted: %s", replacement.new)
		}
	}
}

func TestPiProvidersValidateArguments(t *testing.T) {
	root, _ := piProviderCommandFixture(t)
	for _, args := range [][]string{nil, {"unknown"}, {"validate"}, {"validate", "--package-root", "relative"}, {"validate", "--package-root", root, "extra"}, {"validate", "--auth-dir", root}} {
		if err := piProvidersCommand(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}
