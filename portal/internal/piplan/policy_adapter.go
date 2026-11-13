package piplan

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// ToolPolicyAdapter binds Plan acceptance to the optional proved P3 port. Every
// call runs under ordinary host admission; there is no fallback to settings mode.
type ToolPolicyAdapter struct {
	Controller   piruntime.ToolPolicyController
	VerifyOrigin func(context.Context, Caller, Proposal) error
	Current      func(context.Context, Caller) (Caller, error)
	Admit        func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error
	Eligible     func(context.Context, Caller) (bool, error)
	// ComposeDefault uses current trusted saved access, never browser tool names.
	ComposeDefault func(context.Context, Caller, uint64) (piruntime.ToolPolicyTarget, error)
}
type policyToken struct {
	mode             string
	revision         uint64
	original, target string
}

func (t policyToken) String() string {
	return fmt.Sprintf("pi-plan-policy-v1:%s:%d:%s:%s", t.mode, t.revision, t.original, t.target)
}
func parsePolicyToken(value string) (policyToken, error) {
	var t policyToken
	parts := strings.Split(value, ":")
	if len(value) > 192 || len(parts) != 5 || parts[0] != "pi-plan-policy-v1" || (parts[1] != "plan" && parts[1] != "default") {
		return t, piquestions.ErrConflict
	}
	revision, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || revision == 0 || revision > piruntime.MaxToolPolicyRevision || strconv.FormatUint(revision, 10) != parts[2] {
		return t, piquestions.ErrConflict
	}
	for _, h := range parts[3:] {
		if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
			return t, piquestions.ErrConflict
		}
	}
	t = policyToken{parts[1], revision, parts[3], parts[4]}
	if (t.mode == "plan" && t.revision == piruntime.MaxToolPolicyRevision) || (t.mode == "default" && t.original != t.target) {
		return policyToken{}, piquestions.ErrConflict
	}
	return t, nil
}
func (a *ToolPolicyAdapter) ready() bool {
	return a != nil && a.Controller != nil && a.Current != nil && a.Admit != nil && a.Eligible != nil && a.ComposeDefault != nil
}
func (a *ToolPolicyAdapter) current(ctx context.Context, c Caller) (Caller, piruntime.ToolPolicyProof, error) {
	var proof piruntime.ToolPolicyProof
	if !a.ready() || piquestions.ValidateCaller(c) != nil {
		return c, proof, piquestions.ErrUnavailable
	}
	current, err := a.Current(ctx, c)
	if err != nil {
		return c, proof, err
	}
	if piquestions.ValidateCaller(current) != nil || !sameCaller(current, c) {
		return c, proof, piquestions.ErrConflict
	}
	proof, err = a.Controller.InspectToolPolicy(ctx, current.Handle)
	if err != nil {
		return current, proof, err
	}
	if piruntime.ValidateToolPolicyProof(proof, proof.Policy, current.Handle.Generation) != nil {
		return current, proof, piquestions.ErrUnavailable
	}
	return current, proof, nil
}
func (a *ToolPolicyAdapter) target(ctx context.Context, c Caller, t policyToken) (piruntime.ToolPolicyTarget, error) {
	revision := t.revision
	if t.mode == "plan" {
		revision++
	}
	target, err := a.ComposeDefault(ctx, c, revision)
	if err != nil {
		return target, err
	}
	if piruntime.ValidateToolPolicy(target) != nil || target.Mode != "default" || target.Revision != revision || target.Digest != t.target {
		return target, piquestions.ErrConflict
	}
	return target, nil
}

// Check may inspect native policy and must run outside the short domain lock.
func (a *ToolPolicyAdapter) Check(ctx context.Context, ref agentbackend.ConversationRef, c Caller) (Policy, error) {
	var result Policy
	if c.Handle.Conversation != ref {
		return result, piquestions.ErrConflict
	}
	c, proof, err := a.current(ctx, c)
	if err != nil {
		return result, err
	}
	eligible, err := a.Eligible(ctx, c)
	if err != nil || !eligible {
		return result, piquestions.ErrUnavailable
	}
	revision := proof.Policy.Revision
	if proof.Policy.Mode == "plan" {
		if revision == piruntime.MaxToolPolicyRevision {
			return result, piquestions.ErrUnavailable
		}
		revision++
	}
	target, err := a.ComposeDefault(ctx, c, revision)
	if err != nil {
		return result, err
	}
	if piruntime.ValidateToolPolicy(target) != nil || target.Mode != "default" || target.Revision != revision || !equalNames(target.RegisteredCeiling, proof.Policy.RegisteredCeiling) {
		return result, piquestions.ErrConflict
	}
	token := policyToken{proof.Policy.Mode, proof.Policy.Revision, proof.Policy.Digest, target.Digest}
	if _, err = parsePolicyToken(token.String()); err != nil {
		return result, err
	}
	return Policy{OriginalMode: proof.Policy.Mode, Revision: token.String(), Idle: true, Eligible: true, Enforced: true}, nil
}
func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ApplyDefault uses exactly the original revision and frozen desired digest.
func (a *ToolPolicyAdapter) ApplyDefault(ctx context.Context, c Caller, value string) (bool, error) {
	token, err := parsePolicyToken(value)
	if err != nil {
		return false, err
	}
	c, proof, err := a.current(ctx, c)
	if err != nil {
		return false, err
	}
	if proof.Policy.Mode != token.mode || proof.Policy.Revision != token.revision || proof.Policy.Digest != token.original {
		return false, piquestions.ErrConflict
	}
	eligible, err := a.Eligible(ctx, c)
	if err != nil || !eligible {
		return false, piquestions.ErrUnavailable
	}
	target, err := a.target(ctx, c, token)
	if err != nil {
		return false, err
	}
	if !equalNames(target.RegisteredCeiling, proof.Policy.RegisteredCeiling) {
		return false, piquestions.ErrConflict
	}
	result, err := a.Controller.ApplyToolPolicy(ctx, c.Handle, token.revision, target)
	if err != nil {
		return false, err
	}
	if result.Outcome != "applied" || piruntime.ValidateToolPolicyProof(result.Proof, target, c.Handle.Generation) != nil {
		return false, piquestions.ErrUnavailable
	}
	return true, nil
}

// ObserveDefault never repeats Apply or accepts a later equal-digest revision.
func (a *ToolPolicyAdapter) ObserveDefault(ctx context.Context, c Caller, value string) (bool, error) {
	token, err := parsePolicyToken(value)
	if err != nil {
		return false, err
	}
	c, proof, err := a.current(ctx, c)
	if err != nil {
		return false, err
	}
	target, err := a.target(ctx, c, token)
	if err != nil {
		return false, err
	}
	if piruntime.ValidateToolPolicyProof(proof, target, c.Handle.Generation) != nil {
		return false, piquestions.ErrUnavailable
	}
	return true, nil
}

// PolicyStore is the concrete HTTP store composition. Its reads remain native-
// free and its Accept owns ordinary admission across inspection and capture.
type PolicyStore struct {
	Store        *FileStore
	Adapter      *ToolPolicyAdapter
	Observations PolicyObservations
}

func (s PolicyStore) Accept(ctx context.Context, ref agentbackend.ConversationRef, in AcceptanceInput) (view AcceptanceView, err error) {
	if s.Store == nil {
		return view, piquestions.ErrUnavailable
	}
	_, view, found, err := s.Store.acceptanceSelection(ctx, ref, in)
	if found || err != nil {
		return view, err
	}
	inspect := func(ctx context.Context, ref agentbackend.ConversationRef, p Proposal) (Policy, error) {
		if !s.Adapter.ready() || s.Adapter.VerifyOrigin == nil {
			return Policy{}, piquestions.ErrUnavailable
		}
		c, err := s.Adapter.Current(ctx, p.Caller)
		if err != nil {
			return Policy{}, err
		}
		if piquestions.ValidateCaller(c) != nil || !sameCaller(c, p.Caller) {
			return Policy{}, piquestions.ErrConflict
		}
		if err = s.Adapter.VerifyOrigin(ctx, c, p); err != nil {
			return Policy{}, err
		}
		return s.Adapter.Check(ctx, ref, c)
	}
	if !s.Adapter.ready() {
		return view, piquestions.ErrUnavailable
	}
	err = s.Adapter.Admit(ctx, ref, func(ctx context.Context) error {
		var e error
		view, e = s.Store.accept(ctx, ref, in, inspect)
		return e
	})
	return
}
func (s PolicyStore) Snapshot(ctx context.Context, ref agentbackend.ConversationRef) (Snapshot, error) {
	if s.Store == nil {
		return Snapshot{}, piquestions.ErrUnavailable
	}
	snapshot, err := s.Store.Snapshot(ctx, ref)
	if err != nil || s.Observations == nil || !s.Adapter.ready() {
		return snapshot, err
	}
	observation, err := s.Observations(ctx, ref)
	if err != nil {
		return snapshot, nil
	}
	record, err := s.Store.readRecord(ctx, ref)
	if err != nil {
		return snapshot, nil
	}
	observation.ImplementationReleased = observation.ImplementationReleased && s.Adapter.VerifyOrigin != nil
	return projectPolicy(snapshot, record, ref, observation), nil
}
func (s PolicyStore) LookupAcceptance(ctx context.Context, ref agentbackend.ConversationRef, id string) (AcceptanceView, error) {
	if s.Store == nil {
		return AcceptanceView{}, piquestions.ErrUnavailable
	}
	return s.Store.LookupAcceptance(ctx, ref, id)
}

// BindService composes the existing ordinary receipt ports; missing receipt
// ownership is unavailable rather than a fake successful implementation path.
func (a *ToolPolicyAdapter) BindService(store *FileStore, delivery Ports) (*Service, error) {
	if store == nil || !a.ready() || delivery.Prepare == nil || delivery.Send == nil || delivery.Lookup == nil {
		return nil, piquestions.ErrUnavailable
	}
	if delivery.VerifyOrigin == nil {
		delivery.VerifyOrigin = a.VerifyOrigin
	}
	delivery.Admit = a.Admit
	delivery.Current = a.Current
	delivery.ApplyDefault = a.ApplyDefault
	delivery.ObserveDefault = a.ObserveDefault
	return &Service{Store: store, Ports: delivery, Policy: a.Check}, nil
}
