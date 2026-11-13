package piproviders

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const installedConfigurationPath = "share/dev-workspace/pi-providers.json"
const installedConfigurationLimit = 1 << 20

// PackageConfiguration is explicit immutable-package input. It neither selects
// a runtime nor establishes production adapter support or account entitlement.
type PackageConfiguration struct {
	Catalog Catalog
	Default Selection
}

// Validate checks trusted typed composition input with the same catalog and
// selection rules as LoadInstalled. Catalog.Digest must retain its original
// DecodeCatalog byte identity; validation never reserializes or replaces it.
func (c PackageConfiguration) Validate() error {
	if err := ValidateCatalog(c.Catalog); err != nil {
		return err
	}
	return validateSelection(c.Catalog, VersionedSelection{Revision: 1, Selection: c.Default})
}

// LoadInstalled reads only the fixed nonsecret configuration beneath the
// caller's admitted package root. Missing input wraps os.ErrNotExist; malformed
// input is a refusal and never returns partial configuration or another default.
func LoadInstalled(packageRoot string) (PackageConfiguration, error) {
	if !textOK(packageRoot) || !filepath.IsAbs(packageRoot) || filepath.Clean(packageRoot) != packageRoot {
		return PackageConfiguration{}, ErrInvalidCatalog
	}
	f, err := os.Open(filepath.Join(packageRoot, installedConfigurationPath))
	if err != nil {
		return PackageConfiguration{}, fmt.Errorf("read installed Pi provider configuration: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return PackageConfiguration{}, fmt.Errorf("inspect installed Pi provider configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > installedConfigurationLimit {
		return PackageConfiguration{}, ErrInvalidCatalog
	}
	data, err := io.ReadAll(io.LimitReader(f, installedConfigurationLimit+1))
	if err != nil {
		return PackageConfiguration{}, fmt.Errorf("read installed Pi provider configuration: %w", err)
	}
	if len(data) > installedConfigurationLimit || !utf8.Valid(data) {
		return PackageConfiguration{}, ErrInvalidCatalog
	}
	return decodePackageConfiguration(data)
}

func decodePackageConfiguration(data []byte) (PackageConfiguration, error) {
	// Decode the exact envelope explicitly: strictDecode's reflected shape rules
	// intentionally do not reinterpret RawMessage objects as ordinary slices.
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return PackageConfiguration{}, ErrInvalidCatalog
	}
	var schema int
	var catalogBytes, selectionBytes json.RawMessage
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return PackageConfiguration{}, ErrInvalidCatalog
		}
		seen[key] = true
		switch key {
		case "schema":
			err = d.Decode(&schema)
		case "catalog":
			err = d.Decode(&catalogBytes)
		case "default":
			err = d.Decode(&selectionBytes)
		default:
			return PackageConfiguration{}, ErrInvalidCatalog
		}
		if err != nil {
			return PackageConfiguration{}, ErrInvalidCatalog
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || len(seen) != 3 || schema != 1 {
		return PackageConfiguration{}, ErrInvalidCatalog
	}
	if _, err = d.Token(); err != io.EOF {
		return PackageConfiguration{}, ErrInvalidCatalog
	}
	// RawMessage retains the original nested byte span, including whitespace,
	// key order and escaping. Never hash a reserialized Catalog.
	catalog, err := DecodeCatalog(catalogBytes)
	if err != nil {
		return PackageConfiguration{}, err
	}
	var selection Selection
	if strictDecode(selectionBytes, &selection) != nil {
		return PackageConfiguration{}, ErrInvalidPolicy
	}
	// Every load decodes fresh owned catalog slices; clone the retained default
	// as well. Neither aliases producer input or another load's configuration.
	config := PackageConfiguration{Catalog: catalog, Default: cloneSelection(selection)}
	if err = config.Validate(); err != nil {
		return PackageConfiguration{}, err
	}
	return config, nil
}
