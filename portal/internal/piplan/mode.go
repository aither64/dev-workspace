package piplan

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type ModeInput struct {
	Mode             string `json:"mode"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	ExpectedDigest   string `json:"expectedDigest"`
}
type ModeView struct {
	Mode     string `json:"mode"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}
type ModeResult struct {
	Outcome string    `json:"outcome"`
	Policy  *ModeView `json:"policy,omitempty"`
}
type ModeSetter interface {
	SetMode(context.Context, agentbackend.ConversationRef, ModeInput) (ModeResult, error)
}
type ModeAdapter struct {
	Controller piruntime.ToolPolicyController
	Admit      func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error
	Resolve    func(context.Context, agentbackend.ConversationRef) (agentbackend.ExecutionHandle, error)
	Eligible   func(context.Context, agentbackend.ExecutionHandle) (bool, error)
	Compose    func(context.Context, agentbackend.ExecutionHandle, string, uint64) (piruntime.ToolPolicyTarget, error)
}

func (v ModeView) Validate() error {
	if (v.Mode != "plan" && v.Mode != "default") || v.Revision == 0 || v.Revision > piruntime.MaxToolPolicyRevision || len(v.Digest) != 64 || strings.Trim(v.Digest, "0123456789abcdef") != "" {
		return piquestions.ErrInvalid
	}
	return nil
}
func (in ModeInput) Validate() error {
	return (ModeView{Mode: in.Mode, Revision: in.ExpectedRevision, Digest: in.ExpectedDigest}).Validate()
}
func modeError(err error) error {
	switch {
	case errors.Is(err, piruntime.ErrScope), errors.Is(err, piruntime.ErrConflict):
		return piquestions.ErrConflict
	case errors.Is(err, piruntime.ErrUnavailable):
		return piquestions.ErrUnavailable
	default:
		return err
	}
}

// SetMode holds ordinary host admission, never a domain file lock. An invoked
// Apply cannot be repeated or turned into a no-effect rejection after an error.
func (a *ModeAdapter) SetMode(ctx context.Context, ref agentbackend.ConversationRef, in ModeInput) (result ModeResult, err error) {
	result.Outcome = "rejected"
	if piquestions.ValidateRef(ref) != nil || in.Validate() != nil {
		return result, piquestions.ErrInvalid
	}
	if a == nil || a.Controller == nil || a.Admit == nil || a.Resolve == nil || a.Eligible == nil || a.Compose == nil {
		return result, piquestions.ErrUnavailable
	}
	invoked := false
	err = a.Admit(ctx, ref, func(ctx context.Context) error {
		handle, err := a.Resolve(ctx, ref)
		if err != nil {
			return err
		}
		if handle.Conversation != ref || !piquestions.Text(handle.Generation, 4096) {
			return piquestions.ErrConflict
		}
		proof, err := a.Controller.InspectToolPolicy(ctx, handle)
		if err != nil {
			return err
		}
		if piruntime.ValidateToolPolicy(proof.Policy) != nil {
			return piquestions.ErrUnavailable
		}
		if piruntime.ValidateToolPolicyProof(proof, proof.Policy, handle.Generation) != nil {
			return piruntime.ErrHeld
		}
		if proof.Policy.Revision != in.ExpectedRevision || proof.Policy.Digest != in.ExpectedDigest {
			return piquestions.ErrConflict
		}
		eligible, err := a.Eligible(ctx, handle)
		if err != nil {
			return err
		}
		if !eligible {
			return piruntime.ErrHeld
		}
		revision := proof.Policy.Revision
		if in.Mode != proof.Policy.Mode {
			if revision == piruntime.MaxToolPolicyRevision {
				return piquestions.ErrConflict
			}
			revision++
		}
		target, err := a.Compose(ctx, handle, in.Mode, revision)
		if err != nil {
			return err
		}
		if piruntime.ValidateToolPolicy(target) != nil || target.Mode != in.Mode || target.Revision != revision || !slices.Equal(target.RegisteredCeiling, proof.Policy.RegisteredCeiling) {
			return piquestions.ErrConflict
		}
		if in.Mode == proof.Policy.Mode {
			if target.Digest != proof.Policy.Digest || !slices.Equal(target.Active, proof.Policy.Active) {
				return piquestions.ErrConflict
			}
		} else {
			invoked = true
			native, applyErr := a.Controller.ApplyToolPolicy(ctx, handle, in.ExpectedRevision, target)
			result = ModeResult{Outcome: "unknown"}
			if applyErr != nil {
				return nil
			}
			if native.Outcome == "rejected" {
				result.Outcome = "rejected"
				return nil
			}
			if native.Outcome != "applied" || piruntime.ValidateToolPolicyProof(native.Proof, target, handle.Generation) != nil {
				return nil
			}
		}
		result = ModeResult{Outcome: "applied", Policy: &ModeView{Mode: target.Mode, Revision: target.Revision, Digest: target.Digest}}
		return nil
	})
	if err != nil && invoked {
		return ModeResult{Outcome: "unknown"}, nil
	}
	return result, modeError(err)
}
