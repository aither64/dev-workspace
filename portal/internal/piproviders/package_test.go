package piproviders

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func packageFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	c, p, _ := fixture()
	catalog, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	selection, err := json.Marshal(p.Default.Selection)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, selection
}

func writePackageConfiguration(t *testing.T, data []byte) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, installedConfigurationPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0444); err != nil {
		t.Fatal(err)
	}
	return root
}

func envelope(catalog, selection []byte) []byte {
	return []byte(`{"schema":1,"catalog":` + string(catalog) + `,"default":` + string(selection) + `}`)
}

func TestLoadInstalledExactCatalogBytesAndExplicitDefault(t *testing.T) {
	catalog, selection := packageFixture(t)
	// Preserve meaningful lexical differences that Catalog reserialization loses.
	catalog = []byte(strings.Replace(string(catalog), `Provider A`, `Provider \u0041`, 1))
	root := writePackageConfiguration(t, envelope(catalog, selection))
	config, err := LoadInstalled(root)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(catalog)
	if config.Catalog.Digest != hex.EncodeToString(digest[:]) {
		t.Fatal("nested catalog digest does not bind exact raw bytes")
	}
	if err := config.Validate(); err != nil || config.Catalog.Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("typed validation changed original digest: %v", err)
	}
	if config.Catalog.Providers[0].Label != "Provider A" {
		t.Fatal("fixture did not preserve equivalent escaped label")
	}
	var expected Selection
	if err := json.Unmarshal(selection, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Default, expected) {
		t.Fatalf("explicit default changed: %+v", config.Default)
	}
	config.Catalog.Models[0].ThinkingLevels[0] = "mutated"
	config.Catalog.Accounts[0].Ref = "mutated"
	config.Default.Chain[0].ModelID = "mutated"
	config.Default.FallbackOn[0] = Unknown
	again, err := LoadInstalled(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Default, expected) || again.Catalog.Models[0].ThinkingLevels[0] != "off" || again.Catalog.Accounts[0].Ref != "account-a" {
		t.Fatal("consumer mutation changed loaded source or another load")
	}
	// Opaque generic references remain valid; production builtin support belongs
	// to composition rather than a hidden loader restriction.
	if again.Catalog.Providers[0].RuntimeConfigRef != "runtime-a" {
		t.Fatal("loader reinterpreted runtime configuration")
	}
}

func TestPackageConfigurationValidateTypedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*PackageConfiguration)
		want error
	}{
		{"valid", func(c *PackageConfiguration) {}, nil},
		{"invalid_catalog", func(c *PackageConfiguration) { c.Catalog.Schema = 2 }, ErrInvalidCatalog},
		{"missing_digest", func(c *PackageConfiguration) { c.Catalog.Digest = "" }, ErrInvalidCatalog},
		{"invalid_digest", func(c *PackageConfiguration) { c.Catalog.Digest = "not-a-byte-digest" }, ErrInvalidCatalog},
		{"missing_default", func(c *PackageConfiguration) { c.Default = Selection{} }, ErrInvalidPolicy},
		{"unknown_target", func(c *PackageConfiguration) { c.Default.Chain[0].ModelID = "missing" }, ErrInvalidPolicy},
		{"unsupported_thinking", func(c *PackageConfiguration) { c.Default.Chain[0].Thinking = "xhigh" }, ErrInvalidPolicy},
		{"invalid_budget", func(c *PackageConfiguration) { c.Default.Budget.MaxHops = 8 }, ErrInvalidPolicy},
		{"unavailable_route", func(c *PackageConfiguration) { c.Catalog.Routes[1].State = "unverified" }, ErrUnavailableRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, p, _ := fixture()
			config := PackageConfiguration{Catalog: catalog, Default: cloneSelection(p.Default.Selection)}
			tc.edit(&config)
			before, _ := json.Marshal(config)
			err := config.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v got %v", tc.want, err)
			}
			after, _ := json.Marshal(config)
			if string(before) != string(after) {
				t.Fatal("validation mutated typed input")
			}
		})
	}
}

func TestLoadInstalledMissingIsNotMalformed(t *testing.T) {
	root := t.TempDir()
	got, err := LoadInstalled(root)
	if !errors.Is(err, os.ErrNotExist) || !reflect.DeepEqual(got, PackageConfiguration{}) {
		t.Fatalf("missing got %+v %v", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("read created package files")
	}
	for _, data := range [][]byte{nil, []byte(`null`), []byte(`{}`)} {
		got, err := LoadInstalled(writePackageConfiguration(t, data))
		if err == nil || errors.Is(err, os.ErrNotExist) || !reflect.DeepEqual(got, PackageConfiguration{}) {
			t.Fatalf("malformed treated as absent/partial: %+v %v", got, err)
		}
	}
}

func TestLoadInstalledStrictEnvelope(t *testing.T) {
	catalog, selection := packageFixture(t)
	valid := envelope(catalog, selection)
	for name, data := range map[string][]byte{
		"wrong_schema":      []byte(strings.Replace(string(valid), `"schema":1`, `"schema":2`, 1)),
		"schema_string":     []byte(strings.Replace(string(valid), `"schema":1`, `"schema":"1"`, 1)),
		"schema_null":       []byte(strings.Replace(string(valid), `"schema":1`, `"schema":null`, 1)),
		"schema_float":      []byte(strings.Replace(string(valid), `"schema":1`, `"schema":1.0`, 1)),
		"duplicate_outer":   append([]byte(`{"schema":1,`), valid[1:]...),
		"duplicate_escaped": append([]byte(`{"schem\u0061":1,`), valid[1:]...),
		"unknown_outer":     append([]byte(`{"extra":true,`), valid[1:]...),
		"wrong_case":        []byte(strings.Replace(string(valid), `"catalog":`, `"Catalog":`, 1)),
		"missing_default":   []byte(`{"schema":1,"catalog":` + string(catalog) + `}`),
		"null_catalog":      envelope([]byte(`null`), selection),
		"array_catalog":     envelope([]byte(`[]`), selection),
		"null_default":      envelope(catalog, []byte(`null`)),
		"array_default":     envelope(catalog, []byte(`[]`)),
		"string_default":    envelope(catalog, []byte(`"default"`)),
		"trailing":          append(append([]byte{}, valid...), []byte(` {}`)...),
		"truncated":         valid[:len(valid)-1],
		"array_outer":       []byte(`[]`),
		"invalid_utf8":      append(append([]byte{}, valid...), 255),
		"oversized":         append(append([]byte{}, valid...), []byte(strings.Repeat(" ", installedConfigurationLimit))...),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LoadInstalled(writePackageConfiguration(t, data))
			if err == nil || errors.Is(err, os.ErrNotExist) || !reflect.DeepEqual(got, PackageConfiguration{}) {
				t.Fatalf("invalid envelope returned %+v %v", got, err)
			}
		})
	}
}

func TestLoadInstalledNestedCatalogAndSelectionValidation(t *testing.T) {
	catalog, selection := packageFixture(t)
	for name, data := range map[string][]byte{
		"catalog_duplicate":         envelope(append([]byte(`{"schema":1,`), catalog[1:]...), selection),
		"catalog_unknown":           envelope(append([]byte(`{"digest":"invented",`), catalog[1:]...), selection),
		"catalog_invalid_reference": envelope([]byte(strings.Replace(string(catalog), `"account-a"`, `"missing"`, 1)), selection),
		"selection_duplicate":       envelope(catalog, append([]byte(`{"chain":[],`), selection[1:]...)),
		"selection_unknown":         envelope(catalog, append([]byte(`{"revision":1,`), selection[1:]...)),
		"selection_missing_field":   envelope(catalog, []byte(`{"chain":[],"budget":{"max_extra_attempts":0,"max_hops":0}}`)),
		"selection_null_budget":     envelope(catalog, []byte(strings.Replace(string(selection), `{"max_extra_attempts":2,"max_hops":2}`, `null`, 1))),
		"selection_wrong_type":      envelope(catalog, []byte(strings.Replace(string(selection), `"max_hops":2`, `"max_hops":"2"`, 1))),
		"selection_budget":          envelope(catalog, []byte(strings.Replace(string(selection), `"max_hops":2`, `"max_hops":8`, 1))),
		"selection_thinking":        envelope(catalog, []byte(strings.Replace(string(selection), `"medium"`, `"xhigh"`, 1))),
		"selection_unavailable":     envelope([]byte(strings.Replace(string(catalog), `"state": "available"`, `"state": "unverified"`, 1)), selection),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LoadInstalled(writePackageConfiguration(t, data))
			if err == nil || errors.Is(err, os.ErrNotExist) || !reflect.DeepEqual(got, PackageConfiguration{}) {
				t.Fatalf("invalid nested input returned %+v %v", got, err)
			}
		})
	}
}

func TestLoadInstalledBoundedFixedFileOnly(t *testing.T) {
	catalog, selection := packageFixture(t)
	valid := envelope(catalog, selection)
	root := writePackageConfiguration(t, valid)
	if err := os.Mkdir(filepath.Join(root, "other"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pi-providers.json"), []byte(`malformed decoy`), 0444); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalled(root); err != nil {
		t.Fatal("read alternate path", err)
	}
	for _, badRoot := range []string{"", "relative", root + "/other/..", root + "\n"} {
		if got, err := LoadInstalled(badRoot); err == nil || !reflect.DeepEqual(got, PackageConfiguration{}) {
			t.Fatalf("unadmitted package root accepted %q", badRoot)
		}
	}
	directoryRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directoryRoot, installedConfigurationPath), 0755); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadInstalled(directoryRoot); err == nil || !reflect.DeepEqual(got, PackageConfiguration{}) {
		t.Fatal("nonregular config accepted")
	}
}
