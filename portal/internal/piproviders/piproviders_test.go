package piproviders

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func fixture() (Catalog, Policy, MemberKey) {
	c := Catalog{Schema: 1, Digest: strings.Repeat("a", 64), Providers: []Provider{{"a", "Provider A", "native-a", "runtime-a"}, {"b", "Provider B", "native-b", "runtime-b"}}, Accounts: []Account{{"account-a", "a", "auth-a", "native-a", "oauth"}, {"account-b", "b", "auth-b", "native-b", "api_key"}}, Models: []Model{{Kind: "physical", ProviderID: "a", ID: "model-a", Label: "Model A", ThinkingLevels: []string{"off", "medium"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture-a"}, {Kind: "physical", ProviderID: "b", ID: "model-b", Label: "Model B", ThinkingLevels: []string{"medium"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture-b"}}, Routes: []RouteGate{{"a", "account-a", "model-a", "available", "verified", "fixture-a"}, {"b", "account-b", "model-b", "available", "verified", "fixture-b"}}}
	p := Policy{Session: agentbackend.SessionRef{Workspace: "/fixture", Slug: "2026-11-11-session", Incarnation: strings.Repeat("1", 64)}, Revision: 1, Default: VersionedSelection{Revision: 1, Selection: Selection{Chain: []Target{{"a", "account-a", "model-a", "medium"}, {"b", "account-b", "model-b", "medium"}}, Budget: Budget{2, 2}, FallbackOn: []FailureCategory{RateLimit, Overload}}}}
	return c, p, MemberKey{"implementer5", strings.Repeat("2", 64)}
}
func resolveFixture(t *testing.T) (Catalog, Policy, Resolved) {
	t.Helper()
	c, p, m := fixture()
	r, err := Resolve(c, p, m, 1)
	if err != nil {
		t.Fatal(err)
	}
	return c, p, r
}
func TestCatalogStrictAndPrivate(t *testing.T) {
	c, _, _ := fixture()
	data, _ := json.Marshal(c)
	decoded, err := DecodeCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Digest == c.Digest {
		t.Fatal("digest must bind actual bytes")
	}
	for name, bad := range map[string][]byte{"unknown": append([]byte(`{"unexpected":true,`), data[1:]...), "duplicate": append([]byte(`{"schema":1,`), data[1:]...), "case_alias": append([]byte(`{"Schema":1,`), data[1:]...), "null": []byte(`null`), "trailing": append(data, []byte(` {}`)...), "missing_capability": []byte(strings.Replace(string(data), `"tools":true,`, "", 1)), "oversized": make([]byte, (1<<20)+1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCatalog(bad); !errors.Is(err, ErrInvalidCatalog) {
				t.Fatalf("got %v", err)
			}
		})
	}
	public, _ := json.Marshal(c.Public())
	for _, secret := range []string{"runtime-a", "auth-a", "native-a", "credential", "fixture-a"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("public catalog leaks %q: %s", secret, public)
		}
	}
}

func TestExecutionIdentitiesAreNotCatalogIdentifiers(t *testing.T) {
	c, p, m := fixture()
	for _, member := range []MemberKey{m, {Address: "lead", Incarnation: strings.Repeat("f", 64)}} {
		p.Session.Slug = "Example_" + strings.Repeat("a", 64)
		if _, err := Resolve(c, p, member, 1); err != nil {
			t.Fatalf("canonical P2 identity rejected: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*Policy, *MemberKey)
	}{
		{"relative_workspace", func(p *Policy, m *MemberKey) { p.Session.Workspace = "fixture" }},
		{"unclean_workspace", func(p *Policy, m *MemberKey) { p.Session.Workspace = "/fixture/../other" }},
		{"session_short", func(p *Policy, m *MemberKey) { p.Session.Incarnation = strings.Repeat("a", 63) }},
		{"session_upper", func(p *Policy, m *MemberKey) { p.Session.Incarnation = strings.Repeat("A", 64) }},
		{"member_short", func(p *Policy, m *MemberKey) { m.Incarnation = strings.Repeat("a", 63) }},
		{"member_upper", func(p *Policy, m *MemberKey) { m.Incarnation = strings.Repeat("A", 64) }},
		{"bad_slug", func(p *Policy, m *MemberKey) { p.Session.Slug = "../session" }},
		{"bad_address", func(p *Policy, m *MemberKey) { m.Address = "implementer_5" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, m := fixture()
			tc.edit(&p, &m)
			if _, err := Resolve(c, p, m, 1); !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("invalid identity accepted: %v", err)
			}
		})
	}
	c, p, m = fixture()
	c.Providers[0].ID = strings.Repeat("a", 64)
	if _, err := Resolve(c, p, m, 1); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatal("catalog identifier bound was relaxed")
	}
}
func TestCatalogAndPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Catalog, *Policy)
		want error
	}{
		{"duplicate_provider", func(c *Catalog, p *Policy) { c.Providers = append(c.Providers, c.Providers[0]) }, ErrInvalidCatalog},
		{"native_alias", func(c *Catalog, p *Policy) { c.Providers[1].NativeID = c.Providers[0].NativeID }, ErrInvalidCatalog},
		{"account_alias", func(c *Catalog, p *Policy) {
			c.Accounts[1].CredentialRef = c.Accounts[0].CredentialRef
			c.Accounts[1].CredentialKey = c.Accounts[0].CredentialKey
		}, ErrInvalidCatalog},
		{"unknown_auth", func(c *Catalog, p *Policy) { c.Accounts[0].AuthKind = "unknown" }, ErrInvalidCatalog},
		{"virtual", func(c *Catalog, p *Policy) { c.Models[0].Kind = "virtual" }, ErrInvalidCatalog},
		{"missing_account", func(c *Catalog, p *Policy) { c.Accounts = nil }, ErrInvalidCatalog},
		{"duplicate_route", func(c *Catalog, p *Policy) { c.Routes = append(c.Routes, c.Routes[0]) }, ErrInvalidCatalog},
		{"daybreak_unverified", func(c *Catalog, p *Policy) { c.Routes[1].State = "unverified" }, ErrUnavailableRoute},
		{"thinking", func(c *Catalog, p *Policy) { p.Default.Selection.Chain[0].Thinking = "xhigh" }, ErrInvalidPolicy},
		{"cycle", func(c *Catalog, p *Policy) {
			p.Default.Selection.Chain = append(p.Default.Selection.Chain, p.Default.Selection.Chain[0])
		}, ErrInvalidPolicy},
		{"thinking_cycle", func(c *Catalog, p *Policy) {
			target := p.Default.Selection.Chain[0]
			target.Thinking = "off"
			p.Default.Selection.Chain = append(p.Default.Selection.Chain, target)
		}, ErrInvalidPolicy},
		{"budget", func(c *Catalog, p *Policy) { p.Default.Selection.Budget = Budget{8, 8} }, ErrInvalidPolicy},
		{"hop_budget", func(c *Catalog, p *Policy) { p.Default.Selection.Budget = Budget{1, 2} }, ErrInvalidPolicy},
		{"duplicate_category", func(c *Catalog, p *Policy) { p.Default.Selection.FallbackOn = []FailureCategory{Overload, Overload} }, ErrInvalidPolicy},
		{"control", func(c *Catalog, p *Policy) { c.Models[0].EvidenceRef = "secret\n" }, ErrInvalidCatalog},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, _ := fixture()
			tc.edit(&c, &p)
			if err := ValidatePolicy(c, p); !errors.Is(err, tc.want) {
				t.Fatalf("want %v got %v", tc.want, err)
			}
		})
	}
}
func TestResolutionOverrideClearAndDigest(t *testing.T) {
	c, p, m := fixture()
	base, err := Resolve(c, p, m, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := `[1,"` + strings.Repeat("a", 64) + `","session_default",1,[["native-a","account-a","model-a","medium"],["native-b","account-b","model-b","medium"]],[2,2],["overload","rate_limit"]]`
	if got := string(policyBytes(c, "session_default", p.Default)); got != want {
		t.Fatalf("digest bytes\n%s\n%s", got, want)
	}
	other := MemberKey{"implementer0", strings.Repeat("3", 64)}
	selection := cloneSelection(p.Default.Selection)
	selection.Chain = selection.Chain[1:]
	updated, err := WithOverride(p, other, &selection)
	if err != nil {
		t.Fatal(err)
	}
	same, err := Resolve(c, updated, m, 99)
	if err != nil || same.PolicyDigest != base.PolicyDigest {
		t.Fatalf("unrelated override changes digest: %v", err)
	}
	updated, err = WithOverride(updated, m, &selection)
	if err != nil {
		t.Fatal(err)
	}
	over, err := Resolve(c, updated, m, 2)
	if err != nil || over.Source != "member_override" || over.Requested.ProviderID != "b" {
		t.Fatalf("override: %+v %v", over, err)
	}
	staleMember := m
	staleMember.Incarnation = strings.Repeat("4", 64)
	inherited, err := Resolve(c, updated, staleMember, 1)
	if err != nil || inherited.Source != "session_default" {
		t.Fatalf("incarnation inherited old override: %v", err)
	}
	updated, err = WithOverride(updated, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := Resolve(c, updated, m, 3)
	if err != nil || cleared.PolicyDigest != base.PolicyDigest {
		t.Fatalf("clear: %v", err)
	}
	reordered := p
	reordered.Default.Selection.FallbackOn = []FailureCategory{Overload, RateLimit}
	rr, _ := Resolve(c, reordered, m, 1)
	if rr.PolicyDigest != base.PolicyDigest {
		t.Fatal("category order changed digest")
	}
	view := base.View(nil, nil)
	if view.Effective != nil || view.Physical != nil {
		t.Fatal("invented observation")
	}
	actual := DispatchObservation{Provider: "native-b", Model: "model-b", API: "responses", ConfiguredAccountRef: "account-b", ObservedAt: time.Now()}
	effective := over.Settings
	view = base.View(&effective, &actual)
	if view.Requested != base.Requested || view.Physical.Model != "model-b" || view.Effective.Model != "model-b" {
		t.Fatal("requested/effective/physical conflated")
	}
}
func TestStoreCASNoWriteReadAndStrictRecovery(t *testing.T) {
	c, p, m := fixture()
	root := filepath.Join(t.TempDir(), "state")
	s := Store{root, c, p.Session}
	if _, err := s.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created state")
	}
	if err := s.CompareAndSwap(0, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load()
	if err != nil || !reflect.DeepEqual(loaded, p) {
		t.Fatalf("load %+v %v", loaded, err)
	}
	info, _ := os.Stat(filepath.Join(root, "provider-policy.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("public policy file")
	}
	info, _ = os.Stat(root)
	if info.Mode().Perm() != 0700 {
		t.Fatal("public parent")
	}
	data, _ := os.ReadFile(filepath.Join(root, "provider-policy.json"))
	if strings.Contains(string(data), `"Workspace"`) {
		t.Fatal("non-snake-case scope")
	}
	next, _ := WithOverride(p, m, &p.Default.Selection)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.CompareAndSwap(1, next) }()
	}
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrStalePolicy) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("CAS winners %d stale %d", success, stale)
	}
	bad := next
	bad.Revision++
	bad.Default.Revision++
	if err := s.CompareAndSwap(2, bad); !errors.Is(err, ErrStalePolicy) {
		t.Fatalf("unchanged source revision: %v", err)
	}
	path := filepath.Join(root, "provider-policy.json")
	for _, bad := range []string{`{}`, `{"schema":2}`, `{"schema":1,"schema":1}`, `null`} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load(); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("malformed seen as absence: %v", err)
		}
		if err := s.CompareAndSwap(0, p); err == nil {
			t.Fatal("repaired malformed state")
		}
		after, _ := os.ReadFile(path)
		if string(after) != bad {
			t.Fatal("read/CAS changed malformed state")
		}
	}
}

func TestStoreScopePermissionsAndSourceRevisions(t *testing.T) {
	c, p, m := fixture()
	root := filepath.Join(t.TempDir(), "state")
	s := Store{root, c, p.Session}
	if err := s.CompareAndSwap(0, p); err != nil {
		t.Fatal(err)
	}
	same, err := WithDefault(p, p.Default.Selection)
	if err != nil || !reflect.DeepEqual(same, p) {
		t.Fatal("identical default changed revision")
	}
	other := s
	other.Session.Incarnation = strings.Repeat("5", 64)
	if _, err := other.Load(); !errors.Is(err, ErrStalePolicy) {
		t.Fatalf("wrong session: %v", err)
	}
	selection := cloneSelection(p.Default.Selection)
	selection.Chain = selection.Chain[:1]
	next, err := WithDefault(p, selection)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompareAndSwap(1, next); err != nil {
		t.Fatal(err)
	}
	withOverride, err := WithOverride(next, m, &selection)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompareAndSwap(2, withOverride); err != nil {
		t.Fatal(err)
	}
	same, err = WithOverride(withOverride, m, &selection)
	if err != nil || !reflect.DeepEqual(same, withOverride) {
		t.Fatal("identical override changed revision")
	}
	changed := cloneSelection(selection)
	changed.Chain[0].Thinking = "off"
	withOverride, err = WithOverride(withOverride, m, &changed)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompareAndSwap(3, withOverride); err != nil {
		t.Fatal(err)
	}
	if withOverride.Default.Revision != 2 || withOverride.Overrides[0].Value.Revision != 2 {
		t.Fatal("source revisions lost")
	}
	cleared, err := WithOverride(withOverride, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompareAndSwap(4, cleared); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("nonprivate parent: %v", err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(root, "provider-policy.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("nonprivate policy: %v", err)
	}
}

type fakeConfigurer struct {
	calls  int
	result piruntime.SettingsResult
	err    error
}

func (f *fakeConfigurer) Configure(_ context.Context, _ agentbackend.ExecutionHandle, _ piruntime.EffectiveSettings) (piruntime.SettingsResult, error) {
	f.calls++
	return f.result, f.err
}
func handle(r Resolved) agentbackend.ExecutionHandle {
	return agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: r.Session, Backend: agentbackend.Pi, Member: r.Member.Address, Incarnation: r.Member.Incarnation, NativeID: "native-session"}, Generation: "generation"}
}
func TestApplyRevisionAndTruthfulOutcomes(t *testing.T) {
	c, p, r := resolveFixture(t)
	current := r.Settings
	next, err := Resolve(c, p, r.Member, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Resolved, *agentbackend.ExecutionHandle)
		want error
	}{
		{"skipped", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.Settings.Revision = 3 }, ErrStalePolicy},
		{"same_conflict", func(r *Resolved, h *agentbackend.ExecutionHandle) {
			r.Settings.Revision = 1
			r.Settings.Thinking = "off"
		}, ErrStalePolicy},
		{"account", func(r *Resolved, h *agentbackend.ExecutionHandle) { r.Settings.AccountRef = "account-b" }, ErrUnsupportedAccountSwitch},
		{"incarnation", func(r *Resolved, h *agentbackend.ExecutionHandle) { h.Conversation.Incarnation = "other" }, ErrStalePolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := next
			h := handle(rr)
			tc.edit(&rr, &h)
			f := &fakeConfigurer{}
			result, err := Apply(context.Background(), f, h, current, rr)
			if !errors.Is(err, tc.want) || f.calls != 0 || result.Outcome != "rejected" {
				t.Fatalf("%+v calls=%d err=%v", result, f.calls, err)
			}
		})
	}
	for _, rev := range []uint64{1, 2} {
		rr := r
		rr.Settings.Revision = rev
		f := &fakeConfigurer{result: piruntime.SettingsResult{Requested: rr.Settings, Effective: rr.Settings, Outcome: "applied"}}
		result, err := Apply(context.Background(), f, handle(rr), current, rr)
		if err != nil || f.calls != 1 || result.Outcome != "applied" {
			t.Fatalf("exact revision %d: %v", rev, err)
		}
	}
	for _, outcome := range []string{"unknown", "", "invalid", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			nativeErr := errors.New("bound failure")
			f := &fakeConfigurer{result: piruntime.SettingsResult{Requested: next.Settings, Effective: current, Outcome: outcome}, err: nativeErr}
			result, err := Apply(context.Background(), f, handle(next), current, next)
			if f.calls != 1 || !errors.Is(err, nativeErr) {
				t.Fatal("native result/error lost")
			}
			if outcome == "rejected" {
				if result.Outcome != "rejected" {
					t.Fatal(result)
				}
			} else if result.Outcome != "unknown" || !errors.Is(err, ErrHeld) {
				t.Fatalf("false success %+v %v", result, err)
			}
		})
	}
	f := &fakeConfigurer{result: piruntime.SettingsResult{Requested: next.Settings, Effective: next.Settings, Outcome: "applied"}, err: errors.New("reply uncertain")}
	result, err := Apply(context.Background(), f, handle(next), current, next)
	if result.Outcome != "unknown" || !errors.Is(err, ErrHeld) {
		t.Fatal("uncertain mutation reported applied")
	}
	clamped := next.Settings
	clamped.Thinking = "off"
	f = &fakeConfigurer{result: piruntime.SettingsResult{Requested: next.Settings, Effective: clamped, Outcome: "applied"}}
	result, err = Apply(context.Background(), f, handle(next), current, next)
	if err != nil || result.Requested.Thinking != "medium" || result.Effective.Thinking != "off" {
		t.Fatal("native effective thinking rewrote request")
	}
	changedAccount := next.Settings
	changedAccount.AccountRef = "account-b"
	f = &fakeConfigurer{result: piruntime.SettingsResult{Requested: next.Settings, Effective: changedAccount, Outcome: "applied"}}
	result, err = Apply(context.Background(), f, handle(next), current, next)
	if !errors.Is(err, ErrHeld) || result.Outcome != "unknown" {
		t.Fatal("unproved account reply accepted")
	}
}
func attempt(r Resolved) AttemptEvidence {
	actual := r.Chain[0]
	return AttemptEvidence{Submission: piruntime.SubmissionRef{Conversation: handle(r).Conversation, OperationID: "operation", Action: "prompt", Digest: strings.Repeat("b", 64)}, PolicyDigest: r.PolicyDigest, Ordinal: 1, Actual: &actual, SubmissionOutcome: "NotSubmitted", Effects: "none", Failure: Failure{Stage: "provider_response", Category: Overload, Code: "overloaded", EvidenceKind: "p3_not_submitted", EvidenceRef: "positive-no-effect-witness", HTTPStatus: 503, ObservedAt: time.Now()}}
}
func TestPlannerNeverSendsOrAuthorizesReplay(t *testing.T) {
	_, _, r := resolveFixture(t)
	e := attempt(r)
	d, err := PlanNext(r, e)
	if err != nil || d.Outcome != "candidate_execution_unavailable" || d.Next == nil || d.Next.ProviderID != "b" || d.ExtraAttempts != 1 || d.Hops != 1 {
		t.Fatalf("candidate %+v %v", d, err)
	}
	for _, tc := range []struct {
		name    string
		edit    func(*AttemptEvidence)
		outcome string
	}{
		{"accepted", func(e *AttemptEvidence) { e.SubmissionOutcome = "Accepted" }, "stop"},
		{"unknown", func(e *AttemptEvidence) { e.SubmissionOutcome = "Unknown" }, "held"},
		{"partial", func(e *AttemptEvidence) { e.PartialOutput = true }, "held"},
		{"native_entry", func(e *AttemptEvidence) { e.NativeEntryID = "native-entry" }, "held"},
		{"observed_effect", func(e *AttemptEvidence) { e.Effects = "observed" }, "held"},
		{"possible_effect", func(e *AttemptEvidence) { e.Effects = "possible" }, "held"},
		{"missing_effect_proof", func(e *AttemptEvidence) { e.Effects = "" }, "held"},
		{"missing_witness", func(e *AttemptEvidence) { e.Failure.EvidenceRef = "" }, "held"},
		{"text_refusal", func(e *AttemptEvidence) { e.Failure.Category = TextRefusal }, "stop"},
		{"structured_policy", func(e *AttemptEvidence) { e.Failure.Category = StructuredPolicy }, "held"},
		{"auth", func(e *AttemptEvidence) { e.Failure.Category = Authentication }, "held"},
		{"refresh", func(e *AttemptEvidence) { e.Failure.Category = RefreshStorage }, "held"},
		{"429_ambiguous", func(e *AttemptEvidence) {
			e.Failure.Category = RateLimit
			e.Failure.Code = ""
			e.Failure.HTTPStatus = 429
		}, "held"},
		{"quota", func(e *AttemptEvidence) { e.Failure.Category = QuotaBilling; e.Failure.Code = "insufficient_quota" }, "held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ee := attempt(r)
			tc.edit(&ee)
			d, err := PlanNext(r, ee)
			if err != nil || d.Outcome != tc.outcome || d.Next != nil {
				t.Fatalf("%+v %v", d, err)
			}
		})
	}
	for _, edit := range []func(*AttemptEvidence){func(e *AttemptEvidence) { e.PolicyDigest = strings.Repeat("c", 64) }, func(e *AttemptEvidence) { e.Submission.Conversation.Incarnation = "other" }, func(e *AttemptEvidence) { e.Actual = &r.Chain[1] }, func(e *AttemptEvidence) { e.ChainIndex = 99 }, func(e *AttemptEvidence) { e.ExtraAttempts = 1 }} {
		ee := attempt(r)
		edit(&ee)
		if d, err := PlanNext(r, ee); err == nil || d.Next != nil {
			t.Fatalf("stale evidence offered %+v %v", d, err)
		}
	}
	r.Budget = Budget{}
	if d, err := PlanNext(r, attempt(r)); err != nil || d.Outcome != "exhausted" || d.Next != nil {
		t.Fatalf("zero budget %+v %v", d, err)
	}
}

func TestPlannerCumulativeBudgetAndNoCycles(t *testing.T) {
	_, _, r := resolveFixture(t)
	e := attempt(r)
	r.Budget = Budget{1, 1}
	e.ExtraAttempts = 1
	e.Ordinal = 2
	if d, err := PlanNext(r, e); err != nil || d.Outcome != "exhausted" || d.Next != nil {
		t.Fatalf("attempt budget reset %+v %v", d, err)
	}
	r.Budget = Budget{2, 2}
	e = attempt(r)
	e.ChainIndex = 1
	e.Hops = 1
	e.ExtraAttempts = 1
	e.Ordinal = 2
	e.Actual = &r.Chain[1]
	if d, err := PlanNext(r, e); err != nil || d.Outcome != "exhausted" || d.Next != nil {
		t.Fatalf("end of finite chain %+v %v", d, err)
	}
	r.Chain = append(r.Chain, r.Chain[0])
	if d, err := PlanNext(r, attempt(r)); !errors.Is(err, ErrInvalidPolicy) || d.Next != nil {
		t.Fatalf("cycle %+v %v", d, err)
	}
}
