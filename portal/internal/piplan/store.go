package piplan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

type FileStore struct {
	Root     string
	Validate piquestions.Validate
	Policy   PolicyCheck
}

func (s *FileStore) file(ref agentbackend.ConversationRef) piquestions.StateFile {
	return piquestions.StateFile{Root: s.Root, Kind: "pi-plans", Name: "plans.json", Ref: ref}
}
func fresh(ref agentbackend.ConversationRef) Record {
	return Record{Schema: 1, Conversation: ref, Proposals: []Proposal{}, Acceptances: []Acceptance{}}
}
func (s *FileStore) validate(ctx context.Context, ref agentbackend.ConversationRef, c *Caller) error {
	if piquestions.ValidateRef(ref) != nil {
		return piquestions.ErrInvalid
	}
	if s.Validate == nil {
		return piquestions.ErrUnavailable
	}
	return s.Validate(ctx, ref, c)
}
func validateRecord(r Record, ref agentbackend.ConversationRef) error {
	if r.Schema != 1 || r.Conversation != ref || r.Proposals == nil || r.Acceptances == nil {
		return piquestions.ErrConflict
	}
	plans := map[string]Proposal{}
	calls := map[string]bool{}
	for _, p := range r.Proposals {
		if calls[p.Caller.NativeToolCallID] || p.CreatedAt.IsZero() || !piquestions.ValidID(p.PlanID) || p.Conversation != ref || piquestions.ValidateCaller(p.Caller) != nil || p.Caller.Handle.Conversation != ref || p.Revision != 1 || Digest(p.Text) != p.TextSHA256 || !piquestions.Text(p.Text, piquestions.MessageLimit) || !piquestions.Text(p.NativeOrigin.EntryID, 4096) || !p.NativeOrigin.Complete || !p.Complete || plans[p.PlanID].PlanID != "" {
			return piquestions.ErrConflict
		}
		plans[p.PlanID] = p
		calls[p.Caller.NativeToolCallID] = true
	}
	seen := map[string]bool{}
	accepted := map[string]bool{}
	for _, a := range r.Acceptances {
		p, ok := plans[a.PlanID]
		if (a.OriginalMode != "plan" && a.OriginalMode != "default") || a.ActionContext != fmt.Sprintf("plan:%s:%s", p.PlanID, p.TextSHA256) || !ok || seen[a.AcceptanceID] || accepted[a.PlanID] || !piquestions.ValidID(a.AcceptanceID) || !piquestions.ValidID(a.OperationID) || a.Revision != p.Revision || a.TextSHA256 != p.TextSHA256 || a.TargetMode != "default" || !piquestions.Text(a.PolicyRevision, 4096) || len(a.Message) > piquestions.MessageLimit || a.Message != messageFor(p) {
			return piquestions.ErrConflict
		}
		switch a.Phase {
		case "prepared", "mode_pending", "dispatch_pending", "held", "accepted":
		default:
			return piquestions.ErrConflict
		}
		seen[a.AcceptanceID] = true
		accepted[a.PlanID] = true
	}
	return nil
}
func sameCaller(a, b Caller) bool {
	return a.Handle.Conversation == b.Handle.Conversation && a.NativeToolCallID == b.NativeToolCallID && a.Lineage == b.Lineage
}
func (s *FileStore) PublishProposal(ctx context.Context, c Caller, text string, origin NativeOrigin) (view ProposalView, err error) {
	if piquestions.ValidateCaller(c) != nil || !piquestions.Text(text, piquestions.MessageLimit) || !piquestions.Text(origin.EntryID, 4096) || !origin.Complete {
		return view, piquestions.ErrInvalid
	}
	ref := c.Handle.Conversation
	if err = s.validate(ctx, ref, &c); err != nil {
		return
	}
	r := fresh(ref)
	err = s.file(ref).Update(ctx, &r, func() error {
		if err := s.validate(ctx, ref, &c); err != nil {
			return err
		}
		if err := validateRecord(r, ref); err != nil {
			return err
		}
		for _, p := range r.Proposals {
			if p.Caller.NativeToolCallID == c.NativeToolCallID {
				if !sameCaller(c, p.Caller) || p.Text != text || p.NativeOrigin != origin {
					return piquestions.ErrConflict
				}
				view = proposalView(p, false)
				return nil
			}
		}
		p := Proposal{PlanID: piquestions.UUID(), Revision: 1, Conversation: ref, Caller: c, NativeOrigin: origin, Text: text, TextSHA256: Digest(text), Complete: true, CreatedAt: time.Now().UTC()}
		r.Proposals = append(r.Proposals, p)
		r.Revision++
		view = proposalView(p, false)
		return nil
	})
	return
}
func proposalView(p Proposal, allowed bool) ProposalView {
	return ProposalView{p.PlanID, p.Revision, p.Text, p.TextSHA256, p.Complete, allowed}
}
func acceptanceView(a Acceptance) AcceptanceView {
	return AcceptanceView{a.AcceptanceInput, a.OperationID, a.Phase}
}
func (s *FileStore) Snapshot(ctx context.Context, ref agentbackend.ConversationRef) (Snapshot, error) {
	out := Snapshot{Proposals: []ProposalView{}, Acceptances: []AcceptanceView{}}
	if err := s.validate(ctx, ref, nil); err != nil {
		return out, err
	}
	r := fresh(ref)
	err := s.file(ref).Read(ctx, &r)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = validateRecord(r, ref); err != nil {
		return out, err
	}
	out.Revision = r.Revision
	for _, p := range r.Proposals {
		out.Proposals = append(out.Proposals, proposalView(p, false))
	}
	for _, a := range r.Acceptances {
		out.Acceptances = append(out.Acceptances, acceptanceView(a))
	}
	return out, nil
}
func (s *FileStore) Accept(ctx context.Context, ref agentbackend.ConversationRef, in AcceptanceInput) (view AcceptanceView, err error) {
	if !piquestions.ValidID(in.AcceptanceID) {
		return view, piquestions.ErrInvalid
	}
	if err = s.validate(ctx, ref, nil); err != nil {
		return
	}
	r := fresh(ref)
	err = s.file(ref).Update(ctx, &r, func() error {
		if err := validateRecord(r, ref); err != nil {
			return err
		}
		for _, a := range r.Acceptances {
			if a.AcceptanceID == in.AcceptanceID {
				if !reflect.DeepEqual(a.AcceptanceInput, in) {
					return piquestions.ErrConflict
				}
				view = acceptanceView(a)
				return nil
			}
			if a.PlanID == in.PlanID {
				return piquestions.ErrConflict
			}
		}
		if len(r.Proposals) == 0 {
			return piquestions.ErrNotFound
		}
		p := r.Proposals[len(r.Proposals)-1]
		if p.PlanID != in.PlanID || p.Revision != in.Revision || p.TextSHA256 != in.TextSHA256 {
			return piquestions.ErrConflict
		}
		if err := s.validate(ctx, ref, &p.Caller); err != nil {
			return err
		}
		if s.Policy == nil {
			return piquestions.ErrUnavailable
		}
		policy, err := s.Policy(ctx, ref, p.Caller)
		if err != nil {
			return err
		}
		if !policy.Idle || !policy.Eligible || !policy.Enforced || !piquestions.Text(policy.Revision, 4096) || (policy.OriginalMode != "plan" && policy.OriginalMode != "default") {
			return piquestions.ErrUnavailable
		}
		message := messageFor(p)
		if len(message) > piquestions.MessageLimit {
			return piquestions.ErrCapacity
		}
		a := Acceptance{AcceptanceInput: in, OriginalMode: policy.OriginalMode, TargetMode: "default", PolicyRevision: policy.Revision, OperationID: piquestions.UUID(), Phase: "prepared", Message: message, ActionContext: fmt.Sprintf("plan:%s:%s", p.PlanID, p.TextSHA256)}
		r.Acceptances = append(r.Acceptances, a)
		r.Revision++
		view = acceptanceView(a)
		return nil
	})
	return
}
func (s *FileStore) LookupAcceptance(ctx context.Context, ref agentbackend.ConversationRef, id string) (AcceptanceView, error) {
	if err := s.validate(ctx, ref, nil); err != nil {
		return AcceptanceView{}, err
	}
	r := fresh(ref)
	if err := s.file(ref).Read(ctx, &r); err != nil {
		if os.IsNotExist(err) {
			err = piquestions.ErrNotFound
		}
		return AcceptanceView{}, err
	}
	if err := validateRecord(r, ref); err != nil {
		return AcceptanceView{}, err
	}
	for _, a := range r.Acceptances {
		if a.AcceptanceID == id {
			return acceptanceView(a), nil
		}
	}
	return AcceptanceView{}, piquestions.ErrNotFound
}

func messageFor(p Proposal) string {
	data, _ := json.Marshal(struct {
		PlanID string `json:"planId"`
		Digest string `json:"textSha256"`
		Text   string `json:"text"`
	}{p.PlanID, p.TextSHA256, p.Text})
	return "Implement the accepted plan. The following JSON preserves the exact approved plan as data.\n" + string(data)
}
