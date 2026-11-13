package piproviders

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func canonicalFixture() (Catalog, Policy, MemberKey) {
	c, p, m := fixture()
	for i := range c.Accounts {
		c.Accounts[i].CredentialRef = piruntime.CanonicalCredentialRef
	}
	return c, p, m
}

func TestAccountProjectionExactTargetsAndStartupUnion(t *testing.T) {
	c, p, _ := canonicalFixture()
	before, _ := json.Marshal(c)
	beforeDigest := c.Digest
	primary, err := ProjectRuntimeAccounts(c, p.Default.Selection.Chain[:1], piruntime.CanonicalCredentialRef)
	if err != nil || !reflect.DeepEqual(primary, []piruntime.AccountBinding{{Provider: "native-a", AccountRef: "account-a"}}) {
		t.Fatalf("primary %+v %v", primary, err)
	}
	want := []piruntime.AccountBinding{{Provider: "native-a", AccountRef: "account-a"}, {Provider: "native-b", AccountRef: "account-b"}}
	union, err := SelectableRuntimeAccounts(c, piruntime.CanonicalCredentialRef)
	if err != nil || !reflect.DeepEqual(union, want) {
		t.Fatalf("noninitial provider missing %+v %v", union, err)
	}
	targets := []Target{p.Default.Selection.Chain[1], p.Default.Selection.Chain[0], p.Default.Selection.Chain[0]}
	all, err := ProjectRuntimeAccounts(c, targets, piruntime.CanonicalCredentialRef)
	if err != nil || !reflect.DeepEqual(all, want) {
		t.Fatalf("sorted/dedup %+v %v", all, err)
	}
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("projection mutated catalog")
	}
	if c.Digest != beforeDigest {
		t.Fatal("projection mutated digest")
	}
}

func TestUnsupportedAccountMappingIsNotRuntimeSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Catalog)
	}{
		{"other_store", func(c *Catalog) { c.Accounts[1].CredentialRef = "other-store" }},
		{"wrong_key", func(c *Catalog) { c.Accounts[1].CredentialKey = "other-key" }},
		{"unverified", func(c *Catalog) { c.Routes[1].State = "unverified" }},
		{"unavailable", func(c *Catalog) { c.Routes[1].State = "unavailable" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, _ := canonicalFixture()
			tc.edit(&c)
			before, _ := json.Marshal(c)
			got, err := SelectableRuntimeAccounts(c, piruntime.CanonicalCredentialRef)
			if err != nil || !reflect.DeepEqual(got, []piruntime.AccountBinding{{Provider: "native-a", AccountRef: "account-a"}}) {
				t.Fatalf("unsupported blocks primary %+v %v", got, err)
			}
			if _, err := ProjectRuntimeAccounts(c, p.Default.Selection.Chain[:1], piruntime.CanonicalCredentialRef); err != nil {
				t.Fatalf("valid primary %v", err)
			}
			if _, err := ProjectRuntimeAccounts(c, p.Default.Selection.Chain[1:], piruntime.CanonicalCredentialRef); !errors.Is(err, ErrUnavailableRoute) {
				t.Fatalf("unsupported selected route: %v", err)
			}
			after, _ := json.Marshal(c)
			if string(before) != string(after) {
				t.Fatal("raw route state rewritten")
			}
		})
	}
}

func TestAccountProjectionRefusesMalformedAndAmbiguousInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		edit      func(*Catalog, *[]Target)
		canonical string
		want      error
	}{
		{"empty_targets", func(c *Catalog, targets *[]Target) { *targets = nil }, piruntime.CanonicalCredentialRef, ErrUnavailableRoute},
		{"wrong_canonical_ref", func(c *Catalog, targets *[]Target) {}, "operator-alias", ErrUnavailableRoute},
		{"unknown_account", func(c *Catalog, targets *[]Target) { (*targets)[0].AccountRef = "missing" }, piruntime.CanonicalCredentialRef, ErrUnavailableRoute},
		{"wrong_provider", func(c *Catalog, targets *[]Target) { (*targets)[0].ProviderID = "b" }, piruntime.CanonicalCredentialRef, ErrUnavailableRoute},
		{"wrong_model", func(c *Catalog, targets *[]Target) { (*targets)[0].ModelID = "model-b" }, piruntime.CanonicalCredentialRef, ErrUnavailableRoute},
		{"wrong_thinking", func(c *Catalog, targets *[]Target) { (*targets)[0].Thinking = "xhigh" }, piruntime.CanonicalCredentialRef, ErrUnavailableRoute},
		{"provider_alias", func(c *Catalog, targets *[]Target) { c.Providers[1].NativeID = "native-a" }, piruntime.CanonicalCredentialRef, ErrInvalidCatalog},
		{"account_alias", func(c *Catalog, targets *[]Target) { c.Accounts[1].CredentialKey = "native-a" }, piruntime.CanonicalCredentialRef, ErrInvalidCatalog},
		{"missing_route", func(c *Catalog, targets *[]Target) { c.Routes = c.Routes[1:] }, piruntime.CanonicalCredentialRef, ErrUnavailableRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, _ := canonicalFixture()
			targets := append([]Target(nil), p.Default.Selection.Chain[:1]...)
			tc.edit(&c, &targets)
			if _, err := ProjectRuntimeAccounts(c, targets, tc.canonical); !errors.Is(err, tc.want) {
				t.Fatalf("want %v got %v", tc.want, err)
			}
		})
	}
	c, _, _ := canonicalFixture()
	for i := range c.Routes {
		c.Routes[i].State = "unverified"
	}
	if _, err := SelectableRuntimeAccounts(c, piruntime.CanonicalCredentialRef); !errors.Is(err, ErrUnavailableRoute) {
		t.Fatal("empty startup union accepted")
	}
	c, _, _ = canonicalFixture()
	c.Providers = append(c.Providers, c.Providers[0])
	if _, err := SelectableRuntimeAccounts(c, piruntime.CanonicalCredentialRef); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("ambiguous startup catalog: %v", err)
	}
}

func TestProjectionDoesNotWaiveFullPolicyAvailability(t *testing.T) {
	c, p, m := canonicalFixture()
	c.Routes[1].State = "unavailable"
	p.Default.Selection.Budget = Budget{}
	p.Default.Selection.FallbackOn = nil
	if _, err := ProjectRuntimeAccounts(c, p.Default.Selection.Chain[:1], piruntime.CanonicalCredentialRef); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(c, p, m, 1); !errors.Is(err, ErrUnavailableRoute) {
		t.Fatal("inactive unavailable fallback became selectable")
	}
}

func TestSameModelAlternativeAccountDoesNotSelectAnotherKey(t *testing.T) {
	c, p, _ := canonicalFixture()
	c.Accounts = append(c.Accounts, Account{Ref: "account-alt", ProviderID: "a", CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: "different-key", AuthKind: "oauth"})
	c.Routes = append(c.Routes, RouteGate{ProviderID: "a", AccountRef: "account-alt", ModelID: "model-a", State: "available", ReasonCode: "verified", EvidenceRef: "fixture-alt"})
	union, err := SelectableRuntimeAccounts(c, piruntime.CanonicalCredentialRef)
	if err != nil || len(union) != 2 || union[0].AccountRef != "account-a" {
		t.Fatalf("alternative account replaced native key %+v %v", union, err)
	}
	target := p.Default.Selection.Chain[0]
	target.AccountRef = "account-alt"
	if _, err := ProjectRuntimeAccounts(c, []Target{target}, piruntime.CanonicalCredentialRef); !errors.Is(err, ErrUnavailableRoute) {
		t.Fatalf("same model selected other key: %v", err)
	}
}
