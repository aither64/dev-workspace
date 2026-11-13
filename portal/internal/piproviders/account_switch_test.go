package piproviders

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func switchFixture(t *testing.T) (Resolved, Resolved) {
	t.Helper()
	c, p, m := canonicalFixture()
	old, err := Resolve(c, p, m, 1)
	if err != nil {
		t.Fatal(err)
	}
	selection := cloneSelection(p.Default.Selection)
	selection.Chain = selection.Chain[1:]
	p, err = WithDefault(p, selection)
	if err != nil {
		t.Fatal(err)
	}
	next, err := Resolve(c, p, m, 2)
	if err != nil {
		t.Fatal(err)
	}
	return old, next
}

func TestApplyCrossProviderPairOnceWithoutMutation(t *testing.T) {
	old, next := switchFixture(t)
	before, _ := json.Marshal(next)
	f := &fakeConfigurer{result: piruntime.SettingsResult{Requested: next.Settings, Effective: next.Settings, Outcome: "applied"}}
	result, err := Apply(context.Background(), f, handle(next), old.Settings, next)
	if err != nil || f.calls != 1 || result != f.result {
		t.Fatalf("switch result %+v calls%d err%v", result, f.calls, err)
	}
	after, _ := json.Marshal(next)
	if string(before) != string(after) || !next.hasProvenance() {
		t.Fatal("Apply changed resolved intent/digest")
	}
	for _, outcome := range []string{"rejected", "unknown", "", "applied"} {
		t.Run(outcome, func(t *testing.T) {
			cause := errors.New("configured failure")
			f := &fakeConfigurer{result: piruntime.SettingsResult{Requested: next.Settings, Effective: old.Settings, Outcome: outcome}, err: cause}
			result, err := Apply(context.Background(), f, handle(next), old.Settings, next)
			if f.calls != 1 || !errors.Is(err, cause) {
				t.Fatal("lost Configure cause or replayed call")
			}
			if outcome == "rejected" {
				if result != f.result {
					t.Fatal("positive rejection changed")
				}
			} else if result.Outcome != "unknown" || !errors.Is(err, ErrHeld) || result.Effective != old.Settings {
				t.Fatalf("uncertain switch %+v %v", result, err)
			}
		})
	}
}

func TestApplyRejectsIncompleteCurrentAndUnprovedTarget(t *testing.T) {
	for _, edit := range []func(*piruntime.EffectiveSettings){
		func(s *piruntime.EffectiveSettings) { *s = piruntime.EffectiveSettings{} },
		func(s *piruntime.EffectiveSettings) { s.Revision = 0 },
		func(s *piruntime.EffectiveSettings) { s.Provider = "" },
		func(s *piruntime.EffectiveSettings) { s.AccountRef = "" },
		func(s *piruntime.EffectiveSettings) { s.Model = "" },
		func(s *piruntime.EffectiveSettings) { s.Thinking = "" },
		func(s *piruntime.EffectiveSettings) { s.PolicyRevision = "" },
	} {
		old, next := switchFixture(t)
		current := old.Settings
		edit(&current)
		f := &fakeConfigurer{}
		result, err := Apply(context.Background(), f, handle(next), current, next)
		if !errors.Is(err, ErrInvalidPolicy) || f.calls != 0 || result.Outcome != "rejected" {
			t.Fatalf("invalid current %+v calls%d %v", result, f.calls, err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*Resolved)
		want error
	}{
		{"same_provider_new_account", func(r *Resolved) { r.Settings.Provider = "native-a" }, ErrUnsupportedAccountSwitch},
		{"new_provider_same_account", func(r *Resolved) { r.Settings.AccountRef = "account-a" }, ErrUnsupportedAccountSwitch},
		{"forged_target", func(r *Resolved) { r.Settings.Model = "unverified-model" }, ErrInvalidPolicy},
		{"stale", func(r *Resolved) { r.Settings.Revision = 1 }, ErrStalePolicy},
		{"skipped", func(r *Resolved) { r.Settings.Revision = 3 }, ErrStalePolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, next := switchFixture(t)
			tc.edit(&next)
			f := &fakeConfigurer{}
			result, err := Apply(context.Background(), f, handle(next), old.Settings, next)
			if !errors.Is(err, tc.want) || f.calls != 0 || result.Outcome != "rejected" {
				t.Fatalf("%+v %v calls%d", result, err, f.calls)
			}
		})
	}
}

func TestResolveProvenanceBindsCompleteValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Resolved, *agentbackend.ExecutionHandle)
	}{
		{"session", func(r *Resolved, h *agentbackend.ExecutionHandle) {
			r.Session.Slug = "other"
			h.Conversation.Session = r.Session
		}},
		{"member", func(r *Resolved, h *agentbackend.ExecutionHandle) {
			r.Member.Address = "implementer0"
			h.Conversation.Member = r.Member.Address
		}},
		{"source", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.Source = "member_override" }},
		{"requested", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.Requested.Thinking = "off" }},
		{"settings", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.Settings.Thinking = "off" }},
		{"catalog_digest", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.CatalogDigest = strings.Repeat("d", 64) }},
		{"policy_digest", func(r *Resolved, h *agentbackend.ExecutionHandle) {
			r.PolicyDigest = strings.Repeat("e", 64)
			r.Settings.PolicyRevision = r.PolicyDigest
		}},
		{"chain", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.Chain[0].Thinking = "off" }},
		{"budget", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.Budget.MaxHops = 0 }},
		{"categories", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.FallbackOn[0] = Transport }},
		{"fabricated", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.provenance = nil }},
		{"deserialized", func(r *Resolved, h *agentbackend.ExecutionHandle) {
			data, _ := json.Marshal(r)
			var decoded Resolved
			if err := json.Unmarshal(data, &decoded); err != nil {
				panic(err)
			}
			*r = decoded
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, next := switchFixture(t)
			h := handle(next)
			tc.edit(&next, &h)
			f := &fakeConfigurer{}
			result, err := Apply(context.Background(), f, h, old.Settings, next)
			if err == nil || f.calls != 0 || result.Outcome != "rejected" {
				t.Fatalf("mutated provenance accepted %+v calls%d %v", result, f.calls, err)
			}
		})
	}
	old, next := switchFixture(t)
	copy := next
	f := &fakeConfigurer{result: piruntime.SettingsResult{Requested: copy.Settings, Effective: copy.Settings, Outcome: "applied"}}
	if _, err := Apply(context.Background(), f, handle(copy), old.Settings, copy); err != nil || f.calls != 1 {
		t.Fatalf("ordinary value copy lost proof: %v", err)
	}
	copy.Chain[0].ModelID = "mutated"
	if next.hasProvenance() {
		t.Fatal("exported slice aliases proof")
	}
	if reflect.DeepEqual(next.Chain, next.provenance.value.Chain) {
		t.Fatal("private snapshot aliases mutable chain")
	}
}

func TestResolveRejectsUnsupportedPrimaryMapping(t *testing.T) {
	for _, edit := range []func(*Catalog){func(c *Catalog) { c.Accounts[0].CredentialRef = "other-store" }, func(c *Catalog) { c.Accounts[0].CredentialKey = "other-key" }} {
		c, p, m := canonicalFixture()
		edit(&c)
		if _, err := Resolve(c, p, m, 2); !errors.Is(err, ErrUnavailableRoute) {
			t.Fatalf("unproved primary resolved: %v", err)
		}
	}
}
