package piplan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type modeController struct {
	inspect func(context.Context, agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error)
	apply   func(context.Context, agentbackend.ExecutionHandle, uint64, piruntime.ToolPolicyTarget) (piruntime.ToolPolicyResult, error)
}

func (c modeController) InspectToolPolicy(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
	return c.inspect(ctx, h)
}
func (c modeController) ApplyToolPolicy(ctx context.Context, h agentbackend.ExecutionHandle, expected uint64, target piruntime.ToolPolicyTarget) (piruntime.ToolPolicyResult, error) {
	return c.apply(ctx, h, expected, target)
}

func modeFixture(t *testing.T) (*ModeAdapter, agentbackend.ConversationRef, *policyController) {
	t.Helper()
	_, caller, policy, ctl := adapterFixture(t)
	return &ModeAdapter{Controller: ctl,
		Admit: policy.Admit,
		Resolve: func(_ context.Context, ref agentbackend.ConversationRef) (agentbackend.ExecutionHandle, error) {
			return agentbackend.ExecutionHandle{Conversation: ref, Generation: caller.Handle.Generation}, nil
		}, Eligible: func(context.Context, agentbackend.ExecutionHandle) (bool, error) { return true, nil },
		Compose: func(_ context.Context, _ agentbackend.ExecutionHandle, mode string, revision uint64) (piruntime.ToolPolicyTarget, error) {
			active := []string{"read"}
			if mode == "default" {
				active = []string{"bash", "read"}
			}
			return targetPolicy(mode, revision, active), nil
		}}, caller.Handle.Conversation, ctl
}
func modeInput(p piruntime.ToolPolicyTarget, mode string) ModeInput {
	return ModeInput{Mode: mode, ExpectedRevision: p.Revision, ExpectedDigest: p.Digest}
}

func TestModeActionWithoutProposalAndSameModeNoOp(t *testing.T) {
	ctx := context.Background()
	a, ref, ctl := modeFixture(t)
	before := ctl.policy
	result, err := a.SetMode(ctx, ref, modeInput(before, "plan"))
	if err != nil || result.Outcome != "applied" || result.Policy == nil || result.Policy.Revision != 1 || ctl.apply != 0 || ctl.inspect != 1 {
		t.Fatal(result, err, ctl)
	}
	result, err = a.SetMode(ctx, ref, modeInput(before, "default"))
	if err != nil || result.Outcome != "applied" || result.Policy == nil || result.Policy.Mode != "default" || result.Policy.Revision != 2 || ctl.apply != 1 {
		t.Fatal(result, err, ctl)
	}
	// No proposal, acceptance, synthetic tool caller or settings write was needed.
}

func TestModeStaleInvalidBusyAndUnsupportedRefuseBeforeApply(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []ModeInput{{Mode: "other", ExpectedRevision: 1, ExpectedDigest: strings.Repeat("a", 64)}, {Mode: "plan", ExpectedDigest: strings.Repeat("a", 64)}, {Mode: "plan", ExpectedRevision: piruntime.MaxToolPolicyRevision + 1, ExpectedDigest: strings.Repeat("a", 64)}, {Mode: "plan", ExpectedRevision: 1, ExpectedDigest: strings.Repeat("A", 64)}} {
		a, ref, ctl := modeFixture(t)
		if _, err := a.SetMode(ctx, ref, bad); !errors.Is(err, piquestions.ErrInvalid) || ctl.inspect != 0 || ctl.apply != 0 {
			t.Fatal(bad, err, ctl)
		}
	}
	for _, changed := range []string{"revision", "digest"} {
		a, ref, ctl := modeFixture(t)
		in := modeInput(ctl.policy, "plan")
		if changed == "revision" {
			in.ExpectedRevision++
		} else {
			in.ExpectedDigest = strings.Repeat("0", 64)
		}
		a.Compose = func(context.Context, agentbackend.ExecutionHandle, string, uint64) (piruntime.ToolPolicyTarget, error) {
			t.Fatal("stale CAS reached composition")
			return piruntime.ToolPolicyTarget{}, nil
		}
		if _, err := a.SetMode(ctx, ref, in); !errors.Is(err, piquestions.ErrConflict) || ctl.apply != 0 {
			t.Fatal(changed, err)
		}
	}
	a, ref, ctl := modeFixture(t)
	a.Eligible = func(context.Context, agentbackend.ExecutionHandle) (bool, error) { return false, nil }
	if _, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default")); !errors.Is(err, piruntime.ErrHeld) || ctl.apply != 0 {
		t.Fatal(err)
	}
	if _, err := (*ModeAdapter)(nil).SetMode(ctx, ref, modeInput(ctl.policy, "plan")); !errors.Is(err, piquestions.ErrUnavailable) {
		t.Fatal(err)
	}
	for _, pair := range [][2]error{{piruntime.ErrScope, piquestions.ErrConflict}, {piruntime.ErrConflict, piquestions.ErrConflict}, {piruntime.ErrUnavailable, piquestions.ErrUnavailable}, {piruntime.ErrHeld, piruntime.ErrHeld}} {
		a.Resolve = func(context.Context, agentbackend.ConversationRef) (agentbackend.ExecutionHandle, error) {
			return agentbackend.ExecutionHandle{}, pair[0]
		}
		if _, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default")); !errors.Is(err, pair[1]) || ctl.apply != 0 {
			t.Fatal(pair, err)
		}
	}
}

func TestModeFixedCeilingCompositionAndSafeMaximum(t *testing.T) {
	ctx := context.Background()
	for _, names := range [][]string{{"read"}, {"bash", "read", "write"}} {
		a, ref, ctl := modeFixture(t)
		a.Compose = func(_ context.Context, _ agentbackend.ExecutionHandle, mode string, revision uint64) (piruntime.ToolPolicyTarget, error) {
			target := targetPolicy(mode, revision, []string{"read"})
			target.RegisteredCeiling = names
			target.Digest = piruntime.ToolPolicyDigest(target)
			return target, nil
		}
		if _, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default")); !errors.Is(err, piquestions.ErrConflict) || ctl.apply != 0 {
			t.Fatal(names, err)
		}
	}
	a, ref, ctl := modeFixture(t)
	ctl.policy = targetPolicy("default", 1, []string{"bash", "read"})
	a.Compose = func(context.Context, agentbackend.ExecutionHandle, string, uint64) (piruntime.ToolPolicyTarget, error) {
		return targetPolicy("default", 1, []string{"read"}), nil
	}
	if _, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default")); !errors.Is(err, piquestions.ErrConflict) || ctl.apply != 0 {
		t.Fatal("same-mode composition replacement", err)
	}
	a, ref, ctl = modeFixture(t)
	ctl.policy.Revision = piruntime.MaxToolPolicyRevision
	if result, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "plan")); err != nil || result.Outcome != "applied" || ctl.apply != 0 {
		t.Fatal(result, err)
	}
	if _, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default")); !errors.Is(err, piquestions.ErrConflict) || ctl.apply != 0 {
		t.Fatal("overflow", err)
	}
}

func TestModeMissingPolicyAndWrongCurrentProofRefuse(t *testing.T) {
	ctx := context.Background()
	for _, wrong := range []string{"scope", "generation", "policy", "callable"} {
		t.Run(wrong, func(t *testing.T) {
			a, ref, ctl := modeFixture(t)
			want := piquestions.ErrConflict
			if wrong == "scope" || wrong == "generation" {
				resolve := a.Resolve
				a.Resolve = func(ctx context.Context, ref agentbackend.ConversationRef) (agentbackend.ExecutionHandle, error) {
					h, err := resolve(ctx, ref)
					if wrong == "scope" {
						h.Conversation.NativeID = "other"
					} else {
						h.Generation = ""
					}
					return h, err
				}
			} else {
				want = piquestions.ErrUnavailable
				if wrong == "callable" {
					want = piruntime.ErrHeld
				}
				a.Controller = modeController{inspect: func(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
					if wrong == "policy" {
						return piruntime.ToolPolicyProof{}, nil
					}
					p, err := ctl.InspectToolPolicy(ctx, h)
					p.Callable = []string{"bash", "read"}
					return p, err
				}, apply: ctl.ApplyToolPolicy}
			}
			if _, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default")); !errors.Is(err, want) || ctl.apply != 0 {
				t.Fatal(wrong, err, ctl.apply)
			}
		})
	}
}

func TestModeInvokedErrorsAndIncompleteProofAreUnknown(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []string{"error-rejected", "error-scope", "unknown", "malformed", "incomplete", "stale", "rejected", "applied"} {
		t.Run(outcome, func(t *testing.T) {
			a, ref, ctl := modeFixture(t)
			calls := 0
			a.Controller = modeController{inspect: ctl.InspectToolPolicy, apply: func(ctx context.Context, h agentbackend.ExecutionHandle, expected uint64, target piruntime.ToolPolicyTarget) (piruntime.ToolPolicyResult, error) {
				calls++
				if h.Conversation != ref || h.Generation != ctl.generation || expected != 1 || target.Revision != 2 || target.Mode != "default" {
					t.Fatal(h, expected, target)
				}
				switch outcome {
				case "error-rejected":
					return piruntime.ToolPolicyResult{Outcome: "rejected"}, errors.New("lost response")
				case "error-scope":
					return piruntime.ToolPolicyResult{Outcome: "rejected"}, piruntime.ErrScope
				case "unknown", "malformed", "rejected":
					return piruntime.ToolPolicyResult{Outcome: outcome}, nil
				}
				result, err := ctl.ApplyToolPolicy(ctx, h, expected, target)
				if outcome == "incomplete" {
					result.Proof.Applied = false
				} else if outcome == "stale" {
					result.Proof.Generation = "stale"
				}
				return result, err
			}}
			result, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default"))
			want := "unknown"
			if outcome == "applied" || outcome == "rejected" {
				want = outcome
			}
			if err != nil || calls != 1 || result.Outcome != want || (result.Policy != nil) != (want == "applied") {
				t.Fatal(result, err, calls)
			}
		})
	}
	a, ref, ctl := modeFixture(t)
	a.Admit = func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		if err := fn(ctx); err != nil {
			return err
		}
		return piruntime.ErrScope
	}
	result, err := a.SetMode(ctx, ref, modeInput(ctl.policy, "default"))
	if err != nil || result.Outcome != "unknown" || result.Policy != nil || ctl.apply != 1 {
		t.Fatal("post-Apply admission error", result, err)
	}
}
