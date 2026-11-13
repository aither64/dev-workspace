package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
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
