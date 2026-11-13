package piproviders

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// policyBytes is the sole digest encoding: ordered JSON arrays, Go encoding/json
// escaping, UTF-8 strings, decimal unsigned integers, and sorted categories.
func policyBytes(c Catalog, source string, v VersionedSelection) []byte {
	chain := make([][4]string, 0, len(v.Selection.Chain))
	for _, t := range v.Selection.Chain {
		p, a, _ := findTarget(c, t)
		chain = append(chain, [4]string{p.NativeID, a.Ref, t.ModelID, t.Thinking})
	}
	categories := append([]FailureCategory{}, v.Selection.FallbackOn...)
	sort.Slice(categories, func(i, j int) bool { return categories[i] < categories[j] })
	b, _ := json.Marshal([]any{1, c.Digest, source, v.Revision, chain, []uint32{v.Selection.Budget.MaxExtraAttempts, v.Selection.Budget.MaxHops}, categories})
	return b
}
func Resolve(c Catalog, p Policy, member MemberKey, revision uint64) (Resolved, error) {
	if err := ValidatePolicy(c, p); err != nil {
		return Resolved{}, err
	}
	if !memberOK(member) || revision == 0 {
		return Resolved{}, ErrInvalidPolicy
	}
	v := p.Default
	source := "session_default"
	for _, o := range p.Overrides {
		if o.Member == member {
			v = o.Value
			source = "member_override"
			break
		}
	}
	h := sha256.Sum256(policyBytes(c, source, v))
	digest := hex.EncodeToString(h[:])
	s := cloneSelection(v.Selection)
	t := s.Chain[0]
	provider, _, _ := findTarget(c, t)
	if _, err := ProjectRuntimeAccounts(c, []Target{t}, piruntime.CanonicalCredentialRef); err != nil {
		return Resolved{}, err
	}
	r := Resolved{Session: p.Session, Member: member, Requested: t, Settings: piruntime.EffectiveSettings{Provider: provider.NativeID, AccountRef: t.AccountRef, Model: t.ModelID, Thinking: t.Thinking, PolicyRevision: digest, Revision: revision}, Chain: s.Chain, Budget: s.Budget, FallbackOn: s.FallbackOn, Source: source, CatalogDigest: c.Digest, PolicyDigest: digest}
	snapshot := r
	snapshot.Chain = append([]Target(nil), r.Chain...)
	snapshot.FallbackOn = append([]FailureCategory(nil), r.FallbackOn...)
	r.provenance = &resolvedProvenance{value: snapshot}
	return r, nil
}

func (r Resolved) hasProvenance() bool {
	if r.provenance == nil {
		return false
	}
	snapshot := r.provenance.value
	r.provenance = nil
	return reflect.DeepEqual(r, snapshot)
}

// View accepts only independently verified P3 effective settings and observed
// native physical evidence. Missing evidence remains nil, never desired state.
func (r Resolved) View(effective *piruntime.EffectiveSettings, physical *DispatchObservation) SettingsView {
	v := SettingsView{Source: r.Source, Requested: r.Requested, Chain: append([]Target(nil), r.Chain...), PolicyDigest: r.PolicyDigest}
	if effective != nil {
		copy := *effective
		v.Effective = &copy
	}
	if physical != nil {
		copy := *physical
		v.Physical = &copy
	}
	return v
}
