package piplan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type policyController struct {
	policy         piruntime.ToolPolicyTarget
	generation     string
	inspect, apply int
	lost           bool
}

func (c *policyController) InspectToolPolicy(context.Context, agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
	c.inspect++
	return piruntime.ToolPolicyProof{Policy: c.policy, Generation: c.generation, Active: c.policy.Active, Callable: c.policy.Active, Registered: c.policy.RegisteredCeiling, Applied: true}, nil
}
func (c *policyController) ApplyToolPolicy(_ context.Context, _ agentbackend.ExecutionHandle, expected uint64, target piruntime.ToolPolicyTarget) (piruntime.ToolPolicyResult, error) {
	c.apply++
	if expected != c.policy.Revision {
		return piruntime.ToolPolicyResult{Outcome: "rejected"}, nil
	}
	c.policy = target
	if c.lost {
		c.lost = false
		return piruntime.ToolPolicyResult{Outcome: "unknown"}, errors.New("lost after durable application")
	}
	p, _ := c.InspectToolPolicy(context.Background(), agentbackend.ExecutionHandle{})
	return piruntime.ToolPolicyResult{Outcome: "applied", Proof: p}, nil
}
func targetPolicy(mode string, revision uint64, active []string) piruntime.ToolPolicyTarget {
	p := piruntime.ToolPolicyTarget{Mode: mode, Revision: revision, Active: active, RegisteredCeiling: []string{"bash", "read"}}
	p.Digest = piruntime.ToolPolicyDigest(p)
	return p
}
func adapterFixture(t *testing.T) (*FileStore, Caller, *ToolPolicyAdapter, *policyController) {
	s, c := fixture(t)
	ctl := &policyController{policy: targetPolicy("plan", 1, []string{"read"}), generation: c.Handle.Generation}
	a := &ToolPolicyAdapter{Controller: ctl, VerifyOrigin: func(context.Context, Caller, Proposal) error { return nil }, Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Eligible: func(context.Context, Caller) (bool, error) { return true, nil }, ComposeDefault: func(_ context.Context, _ Caller, revision uint64) (piruntime.ToolPolicyTarget, error) {
		return targetPolicy("default", revision, []string{"bash", "read"}), nil
	}}
	return s, c, a, ctl
}
func TestPolicyAdapterDuplicateAndLostModeAck(t *testing.T) {
	ctx := context.Background()
	s, c, a, ctl := adapterFixture(t)
	p, err := s.PublishProposal(ctx, c, "exact plan\r\n", NativeOrigin{"anchor", true})
	if err != nil {
		t.Fatal(err)
	}
	store := PolicyStore{Store: s, Adapter: a}
	input := AcceptanceInput{piquestions.UUID(), p.PlanID, 1, p.TextSHA256}
	accepted, err := store.Accept(ctx, c.Handle.Conversation, input)
	if err != nil {
		t.Fatal(err)
	}
	inspects := ctl.inspect
	duplicate, err := store.Accept(ctx, c.Handle.Conversation, input)
	if err != nil || accepted.OperationID != duplicate.OperationID || ctl.inspect != inspects {
		t.Fatal(duplicate, err, ctl.inspect)
	}
	record := fresh(c.Handle.Conversation)
	if err = s.file(c.Handle.Conversation).Read(ctx, &record); err != nil {
		t.Fatal(err)
	}
	token := record.Acceptances[0].PolicyRevision
	if !strings.HasPrefix(token, "pi-plan-policy-v1:plan:1:") {
		t.Fatal(token)
	}
	ctl.lost = true
	if ok, err := a.ApplyDefault(ctx, c, token); err == nil || ok {
		t.Fatal("lost ACK succeeded")
	}
	if ok, err := a.ObserveDefault(ctx, c, token); err != nil || !ok || ctl.apply != 1 {
		t.Fatal(ok, err, ctl.apply)
	}
	ctl.policy.Revision++
	if ok, _ := a.ObserveDefault(ctx, c, token); ok {
		t.Fatal("later equal digest accepted")
	}
	if _, err = a.ApplyDefault(ctx, c, token); err == nil || ctl.apply != 1 {
		t.Fatal("reapplied stale mode", err)
	}
	inspects = ctl.inspect
	if _, err = store.Snapshot(ctx, c.Handle.Conversation); err != nil {
		t.Fatal(err)
	}
	if _, err = store.LookupAcceptance(ctx, c.Handle.Conversation, input.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if ctl.inspect != inspects {
		t.Fatal("read called native policy")
	}
}
func TestPolicyInspectionOutsideStoreLockAndCaptureRechecks(t *testing.T) {
	ctx := context.Background()
	s, c, a, _ := adapterFixture(t)
	p, err := s.PublishProposal(ctx, c, "plan", NativeOrigin{"anchor", true})
	if err != nil {
		t.Fatal(err)
	}
	original := a.Eligible
	a.Eligible = func(ctx context.Context, c Caller) (bool, error) {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		r := fresh(c.Handle.Conversation)
		if err := s.file(c.Handle.Conversation).Update(bounded, &r, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		return original(ctx, c)
	}
	if _, err = (PolicyStore{Store: s, Adapter: a}).Accept(ctx, c.Handle.Conversation, AcceptanceInput{piquestions.UUID(), p.PlanID, 1, p.TextSHA256}); err != nil {
		t.Fatal(err)
	}
}
func TestPolicyTokenAndCompositionRefusals(t *testing.T) {
	ctx := context.Background()
	_, c, a, ctl := adapterFixture(t)
	proof, err := a.Check(ctx, c.Handle.Conversation, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{strings.Replace(proof.Revision, ":1:", ":01:", 1), strings.Replace(proof.Revision, ":1:", ":1.5:", 1), strings.Replace(proof.Revision, ":1:", ":9007199254740992:", 1), proof.Revision + ":extra"} {
		if _, err := parsePolicyToken(token); err == nil {
			t.Fatal(token)
		}
	}
	a.ComposeDefault = func(_ context.Context, _ Caller, rev uint64) (piruntime.ToolPolicyTarget, error) {
		return targetPolicy("default", rev, []string{"read"}), nil
	}
	if _, err = a.ApplyDefault(ctx, c, proof.Revision); err == nil || ctl.apply != 0 {
		t.Fatal("changed target applied")
	}
	ctl.policy = targetPolicy("plan", piruntime.MaxToolPolicyRevision, []string{"read"})
	if _, err = a.Check(ctx, c.Handle.Conversation, c); err == nil {
		t.Fatal("overflow captured")
	}
}

func TestConcretePolicyAcceptanceRetainsOrdinaryInputAfterLostAck(t *testing.T) {
	ctx := context.Background()
	s, c, a, ctl := adapterFixture(t)
	p, err := s.PublishProposal(ctx, c, "approved exact plan\r\n", NativeOrigin{"anchor", true})
	if err != nil {
		t.Fatal(err)
	}
	in := AcceptanceInput{piquestions.UUID(), p.PlanID, 1, p.TextSHA256}
	accepted, err := (PolicyStore{Store: s, Adapter: a}).Accept(ctx, c.Handle.Conversation, in)
	if err != nil {
		t.Fatal(err)
	}
	sends, lookups := 0, 0
	var operation, message string
	service, err := a.BindService(s, Ports{Prepare: func(_ context.Context, _ Caller, v Acceptance) (string, error) {
		operation = v.OperationID
		message = v.Message
		return "original-receipt", nil
	}, Send: func(_ context.Context, _ Caller, v Acceptance) (string, error) {
		sends++
		if v.OperationID != accepted.OperationID || v.ReceiptRef != "original-receipt" || v.TextSHA256 != p.TextSHA256 {
			t.Fatal(v)
		}
		return "NotSubmitted", nil
	}, Lookup: func(_ context.Context, _ Caller, v Acceptance) (string, error) {
		lookups++
		if v.OperationID != operation || v.Message != message {
			t.Fatal("replaced ordinary input")
		}
		return "NotSubmitted", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctl.lost = true
	if err = service.Advance(ctx, c.Handle.Conversation, in.AcceptanceID); err == nil {
		t.Fatal("lost mode ACK accepted")
	}
	if sends != 0 {
		t.Fatal(sends)
	}
	for range 3 {
		if err = service.Advance(ctx, c.Handle.Conversation, in.AcceptanceID); err != nil {
			t.Fatal(err)
		}
	}
	if sends != 1 || lookups != 2 || ctl.apply != 1 {
		t.Fatal(sends, lookups, ctl.apply)
	}
	if !strings.Contains(message, "approved exact plan") || operation != accepted.OperationID {
		t.Fatal(message, operation)
	}
}
