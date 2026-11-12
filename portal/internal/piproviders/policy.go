package piproviders

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func sessionOK(s agentbackend.SessionRef) bool {
	return textOK(s.Workspace) && filepath.IsAbs(s.Workspace) && filepath.Clean(s.Workspace) == s.Workspace && textOK(s.Slug) && sessionSlug.MatchString(s.Slug) && digestPattern.MatchString(s.Incarnation)
}
func findTarget(c Catalog, t Target) (Provider, Account, error) {
	var p Provider
	var a Account
	for _, x := range c.Providers {
		if x.ID == t.ProviderID {
			p = x
		}
	}
	for _, x := range c.Accounts {
		if x.Ref == t.AccountRef && x.ProviderID == t.ProviderID {
			a = x
		}
	}
	if p.ID == "" || a.Ref == "" {
		return p, a, ErrInvalidPolicy
	}
	found := false
	for _, m := range c.Models {
		if m.ProviderID == t.ProviderID && m.ID == t.ModelID {
			for _, level := range m.ThinkingLevels {
				if level == t.Thinking {
					found = true
				}
			}
		}
	}
	if !found {
		return p, a, ErrInvalidPolicy
	}
	for _, r := range c.Routes {
		if r.ProviderID == t.ProviderID && r.AccountRef == t.AccountRef && r.ModelID == t.ModelID {
			if r.State == "available" {
				return p, a, nil
			}
			return p, a, ErrUnavailableRoute
		}
	}
	return p, a, ErrUnavailableRoute
}
func validateSelection(c Catalog, v VersionedSelection) error {
	s := v.Selection
	if v.Revision == 0 || len(s.Chain) == 0 || len(s.Chain) > 8 || s.Budget.MaxExtraAttempts > 7 || s.Budget.MaxHops > 7 || s.Budget.MaxHops > s.Budget.MaxExtraAttempts || len(s.FallbackOn) > 13 {
		return ErrInvalidPolicy
	}
	seen := map[[3]string]bool{}
	for _, t := range s.Chain {
		p, a, err := findTarget(c, t)
		if err != nil {
			return err
		}
		key := [3]string{p.NativeID, a.CredentialRef + ":" + a.CredentialKey, t.ModelID}
		if seen[key] {
			return ErrInvalidPolicy
		}
		seen[key] = true
	}
	categories := map[FailureCategory]bool{}
	for _, k := range s.FallbackOn {
		if !validCategory(k) || categories[k] {
			return ErrInvalidPolicy
		}
		categories[k] = true
	}
	return nil
}
func ValidatePolicy(c Catalog, p Policy) error {
	if err := ValidateCatalog(c); err != nil {
		return err
	}
	if !sessionOK(p.Session) || p.Revision == 0 || p.Default.Revision > p.Revision || len(p.Overrides) > 256 {
		return ErrInvalidPolicy
	}
	if err := validateSelection(c, p.Default); err != nil {
		return err
	}
	seen := map[MemberKey]bool{}
	for _, o := range p.Overrides {
		if !memberOK(o.Member) || seen[o.Member] || o.Value.Revision > p.Revision {
			return ErrInvalidPolicy
		}
		seen[o.Member] = true
		if err := validateSelection(c, o.Value); err != nil {
			return err
		}
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > 64<<10 {
		return ErrInvalidPolicy
	}
	return nil
}

// WithOverride edits only the exact member incarnation and advances the document
// revision. Clearing is explicit; a missing override is already inherited.
func WithOverride(p Policy, member MemberKey, selection *Selection) (Policy, error) {
	if !memberOK(member) || p.Revision == math.MaxUint64 {
		return Policy{}, ErrInvalidPolicy
	}
	next := p
	next.Overrides = append([]Override(nil), p.Overrides...)
	for i, o := range next.Overrides {
		if o.Member == member {
			if selection == nil {
				next.Overrides = append(next.Overrides[:i], next.Overrides[i+1:]...)
			} else {
				if reflect.DeepEqual(o.Value.Selection, *selection) {
					return p, nil
				}
				if o.Value.Revision == math.MaxUint64 {
					return Policy{}, ErrInvalidPolicy
				}
				next.Overrides[i].Value = VersionedSelection{o.Value.Revision + 1, cloneSelection(*selection)}
			}
			next.Revision++
			return next, nil
		}
	}
	if selection == nil {
		return p, nil
	}
	next.Revision++
	next.Overrides = append(next.Overrides, Override{member, VersionedSelection{1, cloneSelection(*selection)}})
	return next, nil
}
func WithDefault(p Policy, selection Selection) (Policy, error) {
	if reflect.DeepEqual(p.Default.Selection, selection) {
		return p, nil
	}
	if p.Revision == math.MaxUint64 || p.Default.Revision == math.MaxUint64 {
		return Policy{}, ErrInvalidPolicy
	}
	p.Revision++
	p.Default = VersionedSelection{p.Default.Revision + 1, cloneSelection(selection)}
	return p, nil
}
func cloneSelection(s Selection) Selection {
	s.Chain = append([]Target(nil), s.Chain...)
	s.FallbackOn = append([]FailureCategory(nil), s.FallbackOn...)
	return s
}
func validTransition(old, next Policy) bool {
	if old.Session != next.Session || old.Revision == math.MaxUint64 || next.Revision != old.Revision+1 {
		return false
	}
	check := func(a, b VersionedSelection) bool {
		if reflect.DeepEqual(a.Selection, b.Selection) {
			return b.Revision == a.Revision
		}
		return a.Revision < math.MaxUint64 && b.Revision == a.Revision+1
	}
	if !check(old.Default, next.Default) {
		return false
	}
	previous := map[MemberKey]VersionedSelection{}
	for _, o := range old.Overrides {
		previous[o.Member] = o.Value
	}
	for _, o := range next.Overrides {
		if a, ok := previous[o.Member]; ok {
			if !check(a, o.Value) {
				return false
			}
		} else if o.Value.Revision != 1 {
			return false
		}
	}
	return true
}
