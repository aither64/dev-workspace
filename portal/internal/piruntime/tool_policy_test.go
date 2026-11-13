package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func policyTarget(mode string, revision uint64, active, ceiling []string) ToolPolicyTarget {
	p := ToolPolicyTarget{Mode: mode, Revision: revision, Active: active, RegisteredCeiling: ceiling}
	p.Digest = ToolPolicyDigest(p)
	return p
}
func TestToolPolicyDigestBytesAndIndependentRevision(t *testing.T) {
	p := policyTarget("plan", 1, []string{"find", "grep", "ls", "publish_plan", "read", "request_user_input_async"}, []string{"bash", "edit", "find", "grep", "ls", "publish_plan", "read", "request_user_input_async", "write"})
	want := `["pi-tool-policy",1,"plan",["find","grep","ls","publish_plan","read","request_user_input_async"],["bash","edit","find","grep","ls","publish_plan","read","request_user_input_async","write"]]`
	data, err := json.Marshal([]any{"pi-tool-policy", 1, p.Mode, p.Active, p.RegisteredCeiling})
	if err != nil || string(data) != want {
		t.Fatal(string(data), err)
	}
	h := sha256.Sum256([]byte(want))
	if p.Digest != hex.EncodeToString(h[:]) {
		t.Fatal(p.Digest)
	}
	p.Revision = 99
	if ToolPolicyDigest(p) != p.Digest || ValidateToolPolicy(p) != nil {
		t.Fatal("revision changed mapping digest")
	}
	t.Log(p.Digest)
}
func TestToolPolicyRejectsUnboundedOrEscapingMapping(t *testing.T) {
	good := policyTarget("plan", 1, []string{"read"}, []string{"bash", "read"})
	for _, p := range []ToolPolicyTarget{policyTarget("plan", 1, []string{"bash"}, []string{"bash", "read"}), policyTarget("plan", 1, []string{"read", "read"}, []string{"read"}), policyTarget("default", 1, []string{"read"}, []string{}), policyTarget("plan", 1, nil, []string{"read"}), policyTarget("other", 1, []string{}, []string{}), policyTarget("plan", 0, []string{}, []string{}), policyTarget("plan", 1, []string{"read*"}, []string{"read*"})} {
		if ValidateToolPolicy(p) == nil {
			t.Fatal(p)
		}
	}
	proof := ToolPolicyProof{Policy: good, Generation: "current", Active: []string{"read"}, Callable: []string{"read"}, Registered: []string{"bash", "read"}, Applied: true}
	if ValidateToolPolicyProof(proof, good, "current") != nil {
		t.Fatal("valid proof rejected")
	}
	proof.Callable = []string{"bash", "read"}
	if ValidateToolPolicyProof(proof, good, "current") == nil {
		t.Fatal("inactive callable escape accepted")
	}
	proof.Callable = []string{"read"}
	if ValidateToolPolicyProof(proof, good, "stale") == nil {
		t.Fatal("stale generation accepted")
	}
}

func TestToolPolicyRevisionBoundAndFixedCeiling(t *testing.T) {
	p := policyTarget("plan", MaxToolPolicyRevision, []string{"read"}, []string{"bash", "read"})
	if ValidateToolPolicy(p) != nil {
		t.Fatal("max exact revision refused")
	}
	p.Revision++
	if ValidateToolPolicy(p) == nil {
		t.Fatal("unsafe JSON revision accepted")
	}
}

func TestToolPolicyBadCASDoesNotReachRuntime(t *testing.T) {
	m := &Manager{}
	p := policyTarget("plan", 1, []string{"read"}, []string{"read"})
	for _, revision := range []uint64{0, MaxToolPolicyRevision + 1} {
		result, err := m.ApplyToolPolicy(context.Background(), agentbackend.ExecutionHandle{}, revision, p)
		if err == nil || result.Outcome != "rejected" {
			t.Fatal(result, err)
		}
	}
}

func TestInitialToolPolicyReplayNeverRestartsPendingOrInfersOriginal(t *testing.T) {
	p := policyTarget("plan", 1, []string{"read"}, []string{"bash", "read"})
	later := p
	later.Revision = 3
	changed := policyTarget("default", 1, []string{"bash", "read"}, []string{"bash", "read"})
	for _, tc := range []struct {
		schema                      int
		current, pending, requested *ToolPolicyTarget
		want                        error
	}{{1, nil, nil, nil, nil}, {1, nil, nil, &p, ErrConflict}, {2, &p, nil, nil, ErrConflict}, {2, &p, &p, &p, ErrHeld}, {2, nil, &p, &p, ErrHeld}, {2, &p, nil, &p, nil}, {2, &p, nil, &changed, ErrConflict}, {2, &later, nil, &p, ErrUnavailable}} {
		err := validateInitialToolPolicyReplay(tc.schema, tc.current, tc.pending, tc.requested)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

type policyWorker struct {
	policy     ToolPolicyTarget
	generation string
	snapshot   workerSnapshot
	applies    int
	holds      int
	onApply    func(ToolPolicyTarget) error
	outcome    string
}

func (w *policyWorker) proof() ToolPolicyProof {
	return ToolPolicyProof{Policy: w.policy, Generation: w.generation, Active: w.policy.Active, Callable: w.policy.Active, Registered: w.policy.RegisteredCeiling, Applied: w.holds == 0}
}
func (w *policyWorker) Call(_ context.Context, op string, input, out any) error {
	var value any
	switch op {
	case "snapshot":
		value = w.snapshot
	case "tool_policy_inspect":
		value = w.proof()
	case "tool_policy_apply":
		w.applies++
		target := input.(ToolPolicyTarget)
		if w.onApply != nil {
			if err := w.onApply(target); err != nil {
				return err
			}
		}
		outcome := w.outcome
		if outcome == "" {
			outcome = "applied"
		}
		if outcome == "applied" {
			w.policy = target
		}
		value = ToolPolicyResult{Outcome: outcome, Proof: w.proof()}
	case "hold":
		w.holds++
		return nil
	default:
		return ErrUnavailable
	}
	data, err := json.Marshal(value)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}
func (*policyWorker) Wait(context.Context) error { return nil }
func (*policyWorker) PID() (int, string)         { return 0, "" }

func policySupervisorFixture(t *testing.T) (*supervisor, *conversation, *policyWorker, agentbackend.ExecutionHandle) {
	t.Helper()
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-policy", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "33333333-3333-4333-8333-333333333333"}
	handle := agentbackend.ExecutionHandle{Conversation: ref, Generation: strings.Repeat("c", 64)}
	root := t.TempDir()
	s := &supervisor{stateRoot: root, ref: RuntimeRef{Session: ref.Session, Generation: handle.Generation}, members: map[string]*conversation{}, childrenCheck: func() (bool, error) { return false, nil }}
	p := policyTarget("plan", 1, []string{"read"}, []string{"bash", "read"})
	c := &conversation{root: filepath.Join(root, "members", ref.Member, ref.Incarnation), record: conversationRecord{Schema: 2, CreationID: "11111111-1111-4111-8111-111111111111", Conversation: ref, HistoryRef: historyReference(ref), Generation: handle.Generation, Settings: settingsFixture(), InitialSettings: settingsFixture(), Loaded: true, ToolPolicy: &p}}
	c.receipts = &receiptEngine{root: filepath.Join(c.root, "submissions"), ownerKind: "retained"}
	w := &policyWorker{policy: p, generation: handle.Generation, snapshot: workerSnapshot{NativeID: ref.NativeID, Model: c.record.Settings.Model, Provider: c.record.Settings.Provider, Thinking: c.record.Settings.Thinking}}
	c.worker = w
	s.members[c.root] = c
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	return s, c, w, handle
}

func TestSupervisorToolPolicyPendingPrecedesMutationAndDurableAck(t *testing.T) {
	ctx := context.Background()
	s, c, w, handle := policySupervisorFixture(t)
	target := policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"})
	w.onApply = func(got ToolPolicyTarget) error {
		var disk conversationRecord
		if err := readPrivate(filepath.Join(c.root, "conversation.json"), &disk); err != nil || disk.PendingToolPolicy == nil || !sameToolPolicy(*disk.PendingToolPolicy, got) || disk.ToolPolicy.Revision != 1 {
			t.Fatal("native mutation preceded synced pending", disk, err)
		}
		return nil
	}
	result, err := s.applyToolPolicy(ctx, handle, 1, target)
	if err != nil || result.Outcome != "applied" || w.applies != 1 {
		t.Fatal(result, err, w.applies)
	}
	var disk conversationRecord
	if err := readPrivate(filepath.Join(c.root, "conversation.json"), &disk); err != nil || disk.PendingToolPolicy != nil || !sameToolPolicy(*disk.ToolPolicy, target) || disk.Settings != settingsFixture() {
		t.Fatal("ACK preceded durable completion or changed settings", disk, err)
	}
	// Losing only this ACK needs fresh Inspect, never another native Apply.
	if proof, err := s.inspectToolPolicy(ctx, handle); err != nil || ValidateToolPolicyProof(proof, target, handle.Generation) != nil {
		t.Fatal(proof, err)
	}
	if duplicate, err := s.applyToolPolicy(ctx, handle, 2, target); err != nil || duplicate.Outcome != "applied" || w.applies != 1 {
		t.Fatal(duplicate, err, w.applies)
	}
}

func TestSupervisorToolPolicyInspectDoesNotWriteLoadOrClearHolds(t *testing.T) {
	ctx := context.Background()
	s, c, _, handle := policySupervisorFixture(t)
	path := filepath.Join(c.root, "conversation.json")
	before, _ := os.ReadFile(path)
	if _, err := s.inspectToolPolicy(ctx, handle); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("Inspect wrote mapping")
	}
	c.worker = nil
	if _, err := s.inspectToolPolicy(ctx, handle); !errors.Is(err, ErrUnavailable) {
		t.Fatal("cold Inspect loaded a worker", err)
	}
	pending := policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"})
	c.record.PendingToolPolicy = &pending
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	s.members = map[string]*conversation{}
	if _, err := s.inspectToolPolicy(ctx, handle); !errors.Is(err, ErrHeld) {
		t.Fatal("cold pending state bypassed hold", err)
	}
	var disk conversationRecord
	if err := readPrivate(path, &disk); err != nil || disk.PendingToolPolicy == nil {
		t.Fatal(disk, err)
	}
}

func TestSupervisorToolPolicyFailureRetainsPendingAndNeverRetries(t *testing.T) {
	for _, failure := range []string{"lost-native", "bad-result", "completion-write"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			s, c, w, handle := policySupervisorFixture(t)
			target := policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"})
			defer os.Chmod(c.root, 0700)
			w.onApply = func(ToolPolicyTarget) error {
				if failure == "lost-native" {
					w.policy = target // mutation may have completed before response loss
					return errors.New("lost native ACK")
				}
				if failure == "completion-write" {
					return os.Chmod(c.root, 0500)
				}
				w.outcome = "malformed"
				return nil
			}
			result, err := s.applyToolPolicy(ctx, handle, 1, target)
			if err == nil || result.Outcome != "unknown" || w.applies != 1 || w.holds != 1 || c.record.PendingToolPolicy == nil || c.record.ToolPolicy.Revision != 1 {
				t.Fatal(result, err, w.applies, w.holds, c.record)
			}
			if err := os.Chmod(c.root, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := s.applyToolPolicy(ctx, handle, 1, target); !errors.Is(err, ErrHeld) || w.applies != 1 {
				t.Fatal("retried uncertain mutation", err, w.applies)
			}
			s.members = map[string]*conversation{}
			if _, err := s.inspectToolPolicy(ctx, handle); !errors.Is(err, ErrHeld) {
				t.Fatal("restart bypassed pending", err)
			}
		})
	}
}

func TestSupervisorToolPolicyLiveCallbackCanInspectButCannotApply(t *testing.T) {
	ctx := context.Background()
	s, c, w, handle := policySupervisorFixture(t)
	c.callbackCount = 1
	before, _ := os.ReadFile(filepath.Join(c.root, "conversation.json"))
	proof, err := s.inspectToolPolicy(ctx, handle)
	if err != nil || !proof.Applied {
		t.Fatal("normal live publication lost policy proof", proof, err)
	}
	target := policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"})
	if result, err := s.applyToolPolicy(ctx, handle, 1, target); !errors.Is(err, ErrHeld) || result.Outcome != "rejected" || w.applies != 0 {
		t.Fatal(result, err, w.applies)
	}
	c.callbackUnknown = true
	if _, err := s.inspectToolPolicy(ctx, handle); !errors.Is(err, ErrHeld) {
		t.Fatal("unknown callback granted proof", err)
	}
	if after, _ := os.ReadFile(filepath.Join(c.root, "conversation.json")); string(after) != string(before) || w.holds != 0 {
		t.Fatal("observational checks mutated state")
	}
}

func TestSupervisorToolPolicyRefusalsAndMaximumDoNotMutate(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"schema1", "nil-policy", "busy", "held", "children", "callback", "handler", "settings", "stale-cas", "skipped", "ceiling"} {
		t.Run(reason, func(t *testing.T) {
			s, c, w, handle := policySupervisorFixture(t)
			target := policyTarget("default", 2, []string{"bash", "read"}, []string{"bash", "read"})
			expected := uint64(1)
			switch reason {
			case "schema1":
				c.record.Schema, c.record.ToolPolicy = 1, nil
			case "nil-policy":
				c.record.ToolPolicy = nil
			case "busy":
				w.snapshot.Busy = true
			case "held":
				c.record.HeldReason = "existing uncertainty"
			case "children":
				s.childrenCheck = func() (bool, error) { return true, nil }
			case "callback":
				c.callbackCount = 1
			case "handler":
				c.hostTools = []string{"request_user_input_async"}
			case "settings":
				settings := c.record.Settings
				c.record.PendingSettings = &settings
			case "stale-cas":
				expected = 2
			case "skipped":
				target.Revision = 3
			case "ceiling":
				target.RegisteredCeiling = []string{"bash", "read", "write"}
				target.Digest = ToolPolicyDigest(target)
			}
			before, _ := os.ReadFile(filepath.Join(c.root, "conversation.json"))
			result, err := s.applyToolPolicy(ctx, handle, expected, target)
			if err == nil || result.Outcome != "rejected" || w.applies != 0 || w.holds != 0 {
				t.Fatal(reason, result, err, w.applies, w.holds)
			}
			if after, _ := os.ReadFile(filepath.Join(c.root, "conversation.json")); string(after) != string(before) {
				t.Fatal("pre-effect refusal wrote state")
			}
		})
	}
	s, c, w, handle := policySupervisorFixture(t)
	max := cloneToolPolicy(*c.record.ToolPolicy)
	max.Revision = MaxToolPolicyRevision
	c.record.ToolPolicy, w.policy = &max, max
	if result, err := s.applyToolPolicy(ctx, handle, MaxToolPolicyRevision, max); err != nil || result.Outcome != "applied" || w.applies != 0 {
		t.Fatal("max duplicate not freshly proved", result, err)
	}
	max.Revision++
	if _, err := s.applyToolPolicy(ctx, handle, MaxToolPolicyRevision, max); !errors.Is(err, ErrScope) || w.applies != 0 {
		t.Fatal("unsafe revision mutated", err, w.applies)
	}
}

// P6NativeFixture exposes the existing finite runtime fixture only to the
// external test package. Production imports gain no fixture API or domain edge.
type P6NativeFixture struct {
	Manager  *Manager
	Runtime  RuntimeRef
	Settings EffectiveSettings
	Context  context.Context
	Attach   func(HostCallbackHandler)
	Idle     func(agentbackend.ExecutionHandle)
}

func NewP6NativeFixture(t *testing.T) P6NativeFixture {
	t.Helper()
	m, host, start, ctx := supervisorFixture(t)
	status, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	return P6NativeFixture{Manager: m, Runtime: status.Runtime, Settings: settingsFixture(), Context: ctx,
		Attach: func(handler HostCallbackHandler) { attachFixture(t, m, host.s, ctx, status.Runtime, handler) },
		Idle:   func(handle agentbackend.ExecutionHandle) { waitIdle(t, m, ctx, handle) },
	}
}
