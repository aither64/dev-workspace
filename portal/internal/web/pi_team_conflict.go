package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type piTeamConflictDomain struct {
	server  *Server
	current piSessionAdmission
	roster  teamruntime.PiRoster
}
type piTeamConflictProof struct {
	evidence piruntime.TeamConflictEvidence
}

func (p piTeamConflictProof) TeamConflictEvidence() piruntime.TeamConflictEvidence { return p.evidence }
func conflictDigest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (d *piTeamConflictDomain) currentRecords(ctx context.Context, source piruntime.RuntimeRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if source.Session != (agentbackend.SessionRef{Workspace: d.current.Binding.Workspace, Slug: d.current.Binding.Slug, Incarnation: d.current.Binding.SessionIncarnation}) {
		return piruntime.ErrScope
	}
	current, err := d.server.piReadySessionIdentityLocked(source.Session)
	if err != nil || !reflect.DeepEqual(current.Binding, d.current.Binding) || !reflect.DeepEqual(current.intent, d.current.intent) {
		return errors.Join(piruntime.ErrConflict, err)
	}
	store, err := teamruntime.NewStore(d.server.config.UserStateRoot, d.server.config.Workspace)
	if err != nil {
		return err
	}
	roster, err := store.LoadPi(source.Session.Slug)
	if err != nil || !reflect.DeepEqual(roster, d.roster) {
		return errors.Join(piruntime.ErrConflict, err)
	}
	return nil
}
func (d *piTeamConflictDomain) Published(ctx context.Context, successor piruntime.RuntimeRef) (bool, error) {
	if err := d.currentRecords(ctx, successor); err != nil {
		return false, err
	}
	current, err := d.server.piReadySessionIdentityLocked(successor.Session)
	if err != nil {
		return false, err
	}
	return current.Authority.Generation == successor.Generation, nil
}
func (d *piTeamConflictDomain) Slots(ctx context.Context, source piruntime.RuntimeRef) ([]piruntime.LifecycleSlot, error) {
	if err := d.currentRecords(ctx, source); err != nil {
		return nil, err
	}
	slots := []piruntime.LifecycleSlot{}
	for i, b := range d.current.Binding.Conversations {
		slot := piruntime.LifecycleSlot{Member: b.Address, Incarnation: b.Incarnation, CreationID: d.current.intent.ReceiptID, NativeID: b.NativeID, State: b.State}
		for _, selection := range d.current.intent.ProviderSelection.Members {
			if selection.Address == b.Address && selection.Incarnation == b.Incarnation {
				slot.InitialSettings = selection.Settings.Effective()
			}
		}
		if i == 0 {
			slot.InitialInputID = d.current.intent.InitialInputID
		} else {
			if i-1 >= len(d.roster.Members) {
				return nil, piruntime.ErrScope
			}
			member := d.roster.Members[i-1]
			if member.Address != b.Address || member.Incarnation != b.Incarnation || member.Fork != nil {
				return nil, piruntime.ErrHeld
			}
			slot.State = member.State
			slot.CreationID = member.CreationID
			slot.InitialInputID = member.InitialInputID
			if member.Addition != nil {
				slot.InitialSettings = member.Addition.Selection.Settings.Effective()
			}
		}
		slots = append(slots, slot)
	}
	return slots, nil
}
func sameConflictCaller(saved piquestions.Caller, call piruntime.TeamConflictCall) bool {
	return saved.Handle.Conversation == call.Conversation && saved.NativeToolCallID == call.Origin.NativeToolCallID && saved.Lineage.AnchorEntryID == call.Origin.AnchorEntryID && saved.Lineage.SelectionToken == call.Origin.SelectionToken
}
func (d *piTeamConflictDomain) Settle(ctx context.Context, source piruntime.RuntimeRef, call piruntime.TeamConflictCall, members []piruntime.LifecycleMember) (piruntime.TeamConflictProof, error) {
	if err := d.currentRecords(ctx, source); err != nil {
		return nil, err
	}
	if call.Conversation.Session != source.Session || call.Input.Conversation != call.Conversation || conflictDigestRaw(call.Arguments) != call.ArgumentsSHA256 {
		return nil, piruntime.ErrScope
	}
	evidence := piruntime.TeamConflictEvidence{RecordsSHA256: conflictDigest(struct {
		Binding session.BackendBinding
		Roster  teamruntime.PiRoster
		Intent  session.PiCreationIntent
	}{d.current.Binding, d.roster, d.current.intent})}
	if call.Origin.Tool == "team_message" || call.Origin.Tool == "report_to_lead" {
		var args map[string]string
		if piquestions.DecodeOriginArguments(call.Arguments, &args) != nil {
			return nil, piruntime.ErrScope
		}
		to := args["to"]
		count := 3
		if call.Origin.Tool == "report_to_lead" {
			to = "lead"
			count = 2
		}
		message := teamruntime.PiMessage{ID: args["message_id"], To: to, Text: args["message"]}
		if len(args) != count || teamruntime.ValidatePiMessage(message) != nil || message.ID != call.MessageID || to != call.To {
			return nil, piruntime.ErrScope
		}
		var target *piruntime.LifecycleMember
		for i := range members {
			if members[i].Slot.Member == to {
				if target != nil {
					return nil, piruntime.ErrScope
				}
				target = &members[i]
			}
		}
		if target == nil || target.Conversation == nil {
			return nil, piruntime.ErrHeld
		}
		evidence.Target = *target.Conversation
		evidence.InitialInputID = target.Slot.InitialInputID
		caller := teamruntime.PiCaller{Runtime: source, Handle: agentbackend.ExecutionHandle{Conversation: call.Conversation, Generation: source.Generation}}
		evidence.Input = piruntime.SendInput{Message: teamruntime.PiEnvelope(caller, message), ActionContext: teamruntime.PiActionContext(caller, message)}
		// Existing private submission lookup is observational; never reserve or send.
		reader, ok := d.server.config.PiCreation.Runtime.(interface {
			ReadMemberAdmission(context.Context, agentbackend.ConversationRef, string, string) (piruntime.ConversationState, piruntime.SubmissionStatus, error)
		})
		if !ok {
			return nil, piruntime.ErrUnavailable
		}
		_, status, err := reader.ReadMemberAdmission(ctx, evidence.Target, target.Slot.CreationID, message.ID)
		// This existing reader is metadata-only, including for the fenced predecessor.
		if err == nil {
			if status.Input != evidence.Input || status.Ref.Conversation != evidence.Target || status.Ref.OperationID != message.ID {
				return nil, piruntime.ErrConflict
			}
			switch status.Phase {
			case "Accepted":
				evidence.Kind = "accepted"
			case "NotSubmitted":
				evidence.Kind = "not_submitted"
			default:
				return nil, piruntime.ErrHeld
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if call.HasTerminalPolicyRefusal() {
				evidence.Kind = "terminal_policy_refused"
				evidence.TerminalRefusal = call.TerminalRefusal
				return piTeamConflictProof{evidence}, nil
			}
			if target.Slot.State != "creating" || target.Slot.InitialInputID == "" {
				return nil, piruntime.ErrHeld
			}
			if target.Slot.InitialInputID != message.ID {
				evidence.Kind = "initial_conflict"
			} else {
				evidence.Kind = "created_empty"
			}
		} else {
			return nil, err
		}
		return piTeamConflictProof{evidence}, nil
	}
	validate := func(ctx context.Context, ref agentbackend.ConversationRef, _ *piquestions.Caller) error {
		if ref != call.Conversation {
			return piruntime.ErrScope
		}
		return d.currentRecords(ctx, source)
	}
	caller := piquestions.Caller{Handle: agentbackend.ExecutionHandle{Conversation: call.Conversation, Generation: source.Generation}, Lineage: piquestions.LineageRef{AnchorEntryID: call.Origin.AnchorEntryID, SelectionToken: call.Origin.SelectionToken}, NativeToolCallID: call.Origin.NativeToolCallID}
	switch call.Origin.Tool {
	case "request_user_input_async":
		store := piquestions.FileStore{Root: d.server.config.UserStateRoot, Validate: validate}
		questions, err := store.Questions(ctx, call.Conversation)
		if err != nil {
			return nil, err
		}
		evidence.RecordsSHA256 = conflictDigest(struct {
			Base      string
			Questions []piquestions.QuestionRecord
		}{evidence.RecordsSHA256, questions})
		found := false
		for _, q := range questions {
			for _, a := range q.Responses {
				if a.Delivery.Phase != "Accepted" && a.Delivery.Phase != "NotSubmitted" {
					return nil, piruntime.ErrHeld
				}
				ref, err := parseWorkflowReceipt(a.Delivery.ReceiptRef, caller, a.Delivery.OperationID)
				if err != nil {
					return nil, err
				}
				evidence.Receipts = append(evidence.Receipts, piruntime.TeamConflictSubmission{Ref: ref, Input: piruntime.SendInput{Message: a.Delivery.Message, ActionContext: a.Delivery.ActionContext}})
			}
			if sameConflictCaller(q.Caller, call) {
				if found || piquestions.VerifyNativeArguments(q, call.Arguments) != nil {
					return nil, piruntime.ErrConflict
				}
				found = true
			}
		}
		if !found {
			return nil, piruntime.ErrHeld
		}
		evidence.Kind = "question_publication"
		evidence.Target = call.Conversation
	case "publish_plan":
		store := piplan.FileStore{Root: d.server.config.UserStateRoot, Validate: validate}
		proposals, acceptances, err := store.Publications(ctx, call.Conversation)
		if err != nil {
			return nil, err
		}
		evidence.RecordsSHA256 = conflictDigest(struct {
			Base        string
			Proposals   []piplan.Proposal
			Acceptances []piplan.Acceptance
		}{evidence.RecordsSHA256, proposals, acceptances})
		for _, a := range acceptances {
			if a.Phase != "accepted" {
				return nil, piruntime.ErrHeld
			}
			ref, err := parseWorkflowReceipt(a.ReceiptRef, caller, a.OperationID)
			if err != nil {
				return nil, err
			}
			evidence.Receipts = append(evidence.Receipts, piruntime.TeamConflictSubmission{Ref: ref, Input: piruntime.SendInput{Message: a.Message, ActionContext: a.ActionContext}})
		}
		found := false
		for _, p := range proposals {
			if sameConflictCaller(p.Caller, call) {
				if found || piplan.VerifyNativeArguments(p, call.Arguments) != nil {
					return nil, piruntime.ErrConflict
				}
				found = true
			}
		}
		if !found {
			return nil, piruntime.ErrHeld
		}
		evidence.Kind = "plan_publication"
		evidence.Target = call.Conversation
	default:
		return nil, piruntime.ErrHeld
	}
	return piTeamConflictProof{evidence}, nil
}
func conflictDigestRaw(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
