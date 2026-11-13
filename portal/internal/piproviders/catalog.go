package piproviders

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var sessionSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
var memberAddress = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}[0-9]+$`)

func textOK(s string) bool {
	return s != "" && len(s) <= 4096 && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}
func memberOK(m MemberKey) bool {
	return textOK(m.Address) && (m.Address == "lead" || memberAddress.MatchString(m.Address)) && digestPattern.MatchString(m.Incarnation)
}
func validCategory(c FailureCategory) bool {
	switch c {
	case Configuration, Authentication, RefreshStorage, ModelUnavailable, RateLimit, Overload, QuotaBilling, Transport, StructuredPolicy, ContextLimit, TextRefusal, Cancelled, Unknown:
		return true
	}
	return false
}
func uniqueStrings(values []string, allowed func(string) bool) bool {
	seen := map[string]bool{}
	for _, s := range values {
		if !allowed(s) || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}
func strictDecode(data []byte, out any) error {
	if !utf8.Valid(data) {
		return ErrInvalidCatalog
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var scan func() error
	scan = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return err
					}
					key, ok := k.(string)
					if !ok || seen[key] {
						return ErrInvalidCatalog
					}
					seen[key] = true
					if err := scan(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := scan(); err != nil {
						return err
					}
				}
			default:
				return ErrInvalidCatalog
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := scan(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalidCatalog
	}
	// encoding/json otherwise accepts case-insensitive field aliases and null
	// primitives. Catalog/state fields are exact and all declared fields required.
	var shape any
	if json.Unmarshal(data, &shape) != nil || !validShape(shape, reflect.TypeOf(out).Elem()) {
		return ErrInvalidCatalog
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func validShape(value any, typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key == "-" {
				continue
			}
			if key == "" {
				key = f.Name
			}
			fields[key] = f.Type
		}
		if len(object) != len(fields) {
			return false
		}
		for k, v := range object {
			t, ok := fields[k]
			if !ok || !validShape(v, t) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if value == nil {
			return true
		}
		array, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range array {
			if !validShape(v, typ.Elem()) {
				return false
			}
		}
		return true
	default:
		return value != nil
	}
}
func DecodeCatalog(data []byte) (Catalog, error) {
	var c Catalog
	if len(data) > 1<<20 || strictDecode(data, &c) != nil {
		return c, ErrInvalidCatalog
	}
	h := sha256.Sum256(data)
	c.Digest = hex.EncodeToString(h[:])
	return c, ValidateCatalog(c)
}
func ValidateCatalog(c Catalog) error {
	data, err := json.Marshal(c)
	if err != nil || len(data) > 1<<20 || c.Schema != 1 || !digestPattern.MatchString(c.Digest) || len(c.Providers) == 0 || len(c.Providers) > 64 || len(c.Accounts) > 64 || len(c.Models) > 4096 || len(c.Routes) > 4096 {
		return ErrInvalidCatalog
	}
	providers := map[string]Provider{}
	native := map[string]bool{}
	for _, p := range c.Providers {
		if !identifier.MatchString(p.ID) || !identifier.MatchString(p.NativeID) || !textOK(p.Label) || !identifier.MatchString(p.RuntimeConfigRef) || providers[p.ID].ID != "" || native[p.NativeID] {
			return ErrInvalidCatalog
		}
		providers[p.ID] = p
		native[p.NativeID] = true
	}
	accounts := map[string]Account{}
	mappings := map[[2]string]bool{}
	for _, a := range c.Accounts {
		k := [2]string{a.CredentialRef, a.CredentialKey}
		if !identifier.MatchString(a.Ref) || providers[a.ProviderID].ID == "" || !identifier.MatchString(a.CredentialRef) || !identifier.MatchString(a.CredentialKey) || accounts[a.Ref].Ref != "" || mappings[k] {
			return ErrInvalidCatalog
		}
		switch a.AuthKind {
		case "oauth", "api_key", "ambient":
		default:
			return ErrInvalidCatalog
		}
		accounts[a.Ref] = a
		mappings[k] = true
	}
	models := map[[2]string]bool{}
	for _, m := range c.Models {
		k := [2]string{m.ProviderID, m.ID}
		if m.Kind != "physical" || providers[m.ProviderID].ID == "" || !textOK(m.ID) || !textOK(m.Label) || !textOK(m.EvidenceRef) || models[k] || len(m.ThinkingLevels) == 0 || len(m.ThinkingLevels) > 7 || len(m.Inputs) == 0 || len(m.Inputs) > 2 || !uniqueStrings(m.ThinkingLevels, func(s string) bool {
			switch s {
			case "off", "minimal", "low", "medium", "high", "xhigh":
				return true
			}
			return false
		}) || !uniqueStrings(m.Inputs, func(s string) bool { return s == "text" || s == "image" }) {
			return ErrInvalidCatalog
		}
		models[k] = true
	}
	routes := map[[3]string]bool{}
	for _, r := range c.Routes {
		k := [3]string{r.ProviderID, r.AccountRef, r.ModelID}
		if !models[[2]string{r.ProviderID, r.ModelID}] || accounts[r.AccountRef].ProviderID != r.ProviderID || routes[k] || !identifier.MatchString(r.ReasonCode) || !textOK(r.EvidenceRef) {
			return ErrInvalidCatalog
		}
		switch r.State {
		case "available", "unavailable", "unverified":
		default:
			return ErrInvalidCatalog
		}
		routes[k] = true
	}
	return nil
}
func (c Catalog) Public() PublicCatalog {
	v := PublicCatalog{}
	for _, p := range c.Providers {
		v.Providers = append(v.Providers, ProviderSummary{p.ID, p.Label})
	}
	for _, m := range c.Models {
		v.Models = append(v.Models, ModelSummary{m.ProviderID, m.ID, m.Label, append([]string(nil), m.ThinkingLevels...), append([]string(nil), m.Inputs...), m.Tools})
	}
	for _, r := range c.Routes {
		v.Routes = append(v.Routes, RouteSummary{r.ProviderID, r.AccountRef, r.ModelID, r.State, r.ReasonCode})
	}
	return v
}
