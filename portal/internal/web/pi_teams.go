package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type piTeamRuntime interface {
	PiCreationRuntime
	piruntime.CurrentRuntimeReader
	InspectToolPolicy(context.Context, agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error)
	ReadMemberAdmission(context.Context, agentbackend.ConversationRef, string, string) (piruntime.ConversationState, piruntime.SubmissionStatus, error)
}

type piTeamService struct{ server *Server }

func (p piTeamService) HostTools() []string { return []string{"report_to_lead", "team_message"} }
func (p piTeamService) Report(ctx context.Context, caller teamruntime.PiCaller, id, text string) (teamruntime.PiDeliveryResult, error) {
	return p.Peer(ctx, caller, teamruntime.PiMessage{ID: id, To: "lead", Text: text})
}

type piTeamInvocation struct {
	piTeamService
	initial         *teamruntime.PiInitialAssignment
	handle          agentbackend.ExecutionHandle
	bindingRevision uint64
}

func (p piTeamService) Peer(ctx context.Context, caller teamruntime.PiCaller, message teamruntime.PiMessage) (teamruntime.PiDeliveryResult, error) {
	invocation := &piTeamInvocation{piTeamService: p}
	result, err := (teamruntime.PiDelivery{Admission: invocation, Native: invocation}).Deliver(ctx, caller, message)
	if err != nil || invocation.initial == nil || (result.Outcome != backend.Accepted && result.Outcome != backend.Pending) {
		return result, err
	}
	// Native enqueue returns before its consumed-user witness. Observe that
	// ORIGINAL receipt only, with no session/team/native dispatch owner held.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		state, receipt, readErr := p.runtime().ReadMemberAdmission(ctx, invocation.handle.Conversation, invocation.initial.CreationID, message.ID)
		if readErr != nil {
			return result, readErr
		}
		if receipt.Phase == "Accepted" {
			if err := p.publishInitial(ctx, invocation, state, receipt); err != nil {
				return result, err
			}
			result.Outcome = backend.Accepted
			result.Native = &backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: message.ID, TurnID: receipt.Witness.EntryID}}
			return result, nil
		}
		if receipt.Phase != "Unknown" {
			return result, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-deadline.C:
			timer.Stop()
			return result, nil
		case <-timer.C:
		}
	}
}

func (p piTeamService) runtime() piTeamRuntime {
	runtime, _ := p.server.config.PiCreation.Runtime.(piTeamRuntime)
	return runtime
}

func (p *piTeamInvocation) Deliver(ctx context.Context, target agentbackend.ExecutionHandle, request backend.SendRequest) (backend.SendResult, error) {
	operations, err := p.runtime().Bind(ctx, target)
	if err != nil || operations.Send == nil {
		return backend.SendResult{Outcome: backend.NotSubmitted}, errors.Join(piruntime.ErrUnavailable, err)
	}
	return operations.Send.Send(ctx, piruntime.BackendIdentity(target), request)
}
func (p *piTeamInvocation) Lookup(ctx context.Context, ref piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	return p.runtime().Lookup(ctx, ref)
}

func (p *piTeamInvocation) WithDelivery(ctx context.Context, caller teamruntime.PiCaller, message teamruntime.PiMessage, deliver func(teamruntime.PiPreparedDelivery) error) error {
	if p.runtime() == nil {
		return piruntime.ErrUnavailable
	}
	s := p.server
	return s.withPiSessionRead(ctx, caller.Runtime.Session, func(current piSessionAdmission) error {
		runtime, err := p.runtime().ReadCurrentRuntime(ctx, caller.Runtime.Session)
		if err != nil || runtime != caller.Runtime || current.Authority.ValidateHandle(current.Binding, caller.Handle) != nil {
			return errors.Join(agentbackend.ErrStaleExecution, err)
		}
		if _, err := s.piSavedAccessLocked(current, caller.Handle.Conversation); err != nil {
			return err
		}
		store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if err != nil {
			return err
		}
		roster, err := store.LoadPi(caller.Runtime.Session.Slug)
		if err != nil || roster.Scope().Ref() != caller.Runtime.Session {
			return errors.Join(piruntime.ErrHeld, err)
		}
		request := backend.SendRequest{ClientUserMessageID: message.ID, Message: teamruntime.PiEnvelope(caller, message), Delivery: "steer", ActionContext: teamruntime.PiActionContext(caller, message)}
		target, resolveErr := current.Binding.Resolve(message.To)
		creating := false
		emptyProven := false
		for _, member := range roster.Members {
			if member.Address == message.To && member.State == "creating" {
				creating = true
			}
		}
		if resolveErr != nil || creating {
			var selected *teamruntime.PiRosterMember
			for i := range roster.Members {
				if roster.Members[i].Address == message.To {
					selected = &roster.Members[i]
					break
				}
			}
			if selected == nil || selected.State != "creating" {
				return piruntime.ErrHeld
			}
			var settings piruntime.EffectiveSettings
			for _, saved := range current.intent.ProviderSelection.Members {
				if saved.Address == selected.Address && saved.Incarnation == selected.Incarnation {
					settings = saved.Settings.Effective()
				}
			}
			if selected.Addition != nil {
				settings = selected.Addition.Selection.Settings.Effective()
			}
			proofs, err := p.readyProofs(ctx, current, roster)
			if err != nil {
				return err
			}
			var assignment teamruntime.PiInitialAssignment
			if len(proofs) == 0 {
				assignment, err = store.ReservePiInitialAssignment(ctx, s, caller, roster.Revision, message, settings)
			} else {
				admission, admissionErr := newPiRosterWriteAdmission(s, current, proofs)
				if admissionErr != nil {
					return admissionErr
				}
				assignment, err = store.ReservePiInitialAssignment(ctx, admission, caller, roster.Revision, message, settings, admission)
			}
			if err != nil {
				return err
			}
			target = agentbackend.ConversationRef{Session: caller.Runtime.Session, Backend: agentbackend.Pi, Member: selected.Address, Incarnation: selected.Incarnation}
			state, _, readErr := p.runtime().ReadMemberAdmission(ctx, target, selected.CreationID, message.ID)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
			if state.Conversation.NativeID == "" {
				policy, err := s.initialPiWorkflowPolicy(current.intent, selected.Address)
				if selected.Addition != nil {
					frozen := selected.Addition.ToolPolicy
					policy, err = &frozen, nil
				}
				if err != nil {
					return err
				}
				if err := s.piWorkflows.ensureAttachment(ctx, runtime); err != nil {
					return err
				}
				state, err = p.runtime().Create(ctx, runtime, piruntime.CreateRequest{Session: caller.Runtime.Session, Member: selected.Address, Incarnation: selected.Incarnation, CreationID: selected.CreationID, Settings: settings, Instructions: selected.Instructions, ToolPolicy: policy})
				if err != nil {
					return err
				}
			}
			target.NativeID = state.Conversation.NativeID
			if state.Conversation != target || state.Generation != runtime.Generation || state.Settings != settings {
				return agentbackend.ErrBindingConflict
			}
			p.initial, p.handle, p.bindingRevision = &assignment, agentbackend.ExecutionHandle{Conversation: target, Generation: runtime.Generation}, current.Binding.Revision
			// Empty proof authorizes the first real send. Existing original receipt
			// permits only the same-ID native enquiry, never a replacement send.
			_, original, readErr := p.runtime().ReadMemberAdmission(ctx, target, selected.CreationID, message.ID)
			if errors.Is(readErr, os.ErrNotExist) {
				proof, err := p.runtime().VerifyCreation(ctx, p.handle, selected.CreationID, state.HistoryRef, nil)
				if err != nil || requirePiCreationInstructions(proof, selected.Instructions) != nil {
					return errors.Join(piruntime.ErrHeld, err)
				}
				emptyProven = true
			} else if readErr != nil {
				return readErr
			} else if original.Input.Message != request.Message || original.Input.ActionContext != request.ActionContext || original.Settings != settings {
				return piruntime.ErrConflict
			}
		} else {
			if current.Authority.ValidateHandle(current.Binding, agentbackend.ExecutionHandle{Conversation: target, Generation: runtime.Generation}) != nil {
				return agentbackend.ErrStaleExecution
			}
		}
		handle := agentbackend.ExecutionHandle{Conversation: target, Generation: runtime.Generation}
		from, err := p.participant(ctx, current, roster, caller.Handle, "", false, false)
		if err != nil {
			return err
		}
		firstID := ""
		if p.initial != nil {
			firstID = message.ID
		}
		recipient, err := p.participant(ctx, current, roster, handle, firstID, emptyProven, true)
		if err != nil {
			return err
		}
		if err := teamruntime.CheckPiDelivery(caller, from, recipient); err != nil {
			return err
		}
		return deliver(teamruntime.PiPreparedDelivery{Target: handle, Settings: recipient.Settings, Request: request})
	})
}

// Native workflow observation excludes outstanding host callbacks. Team delivery
// instead joins the existing member record, native read and policy proof under
// the same session admission. Activity describes state; it grants no readiness.
func (p piTeamService) participant(ctx context.Context, current piSessionAdmission, roster teamruntime.PiRoster, handle agentbackend.ExecutionHandle, firstID string, emptyProven, target bool) (teamruntime.PiParticipant, error) {
	result := teamruntime.PiParticipant{Handle: handle}
	ref := handle.Conversation
	creationID, inputID := current.intent.ReceiptID, current.intent.InitialInputID
	instructions := current.intent.DirectTeam.LeadInstructions
	if ref.Member == "lead" {
		if !p.server.piSourceTeamMatches(current.intent) {
			return result, piruntime.ErrHeld
		}
		team, ok := p.server.installedTeams.Catalog.Team(current.intent.DirectTeam.ID)
		role, found := team.Roles["team_lead"]
		if !ok || !found {
			return result, piruntime.ErrHeld
		}
		role.Model, role.Effort, role.Instructions = current.intent.DirectTeam.LeadModel, current.intent.DirectTeam.LeadEffort, instructions
		var err error
		result.Policy, err = agentteams.ProjectPiPolicy(role, "")
		if err != nil {
			return result, err
		}
		result.State = "ready"
	} else {
		for _, member := range roster.Members {
			if member.Address != ref.Member || member.Incarnation != ref.Incarnation {
				continue
			}
			creationID, inputID, instructions = member.CreationID, member.InitialInputID, member.Instructions
			result.State = member.State
			result.Policy = agentteams.PiPolicy{Model: member.Model, Effort: member.Effort, Behavior: member.Behavior, Purpose: member.Purpose, Instructions: member.Instructions, SavedAccess: member.Access}
		}
		if firstID != "" {
			if inputID != "" && inputID != firstID {
				return result, piruntime.ErrConflict
			}
			inputID = firstID
		}
	}
	state, original, err := p.runtime().ReadMemberAdmission(ctx, ref, creationID, inputID)
	if err != nil && !(emptyProven && errors.Is(err, os.ErrNotExist)) {
		return result, err
	}
	if state.Conversation != ref || state.Generation != handle.Generation {
		return result, agentbackend.ErrStaleExecution
	}
	var originalRef *piruntime.SubmissionRef
	if !emptyProven {
		if original.Phase != "Accepted" || original.Witness == nil || !state.Persisted {
			return result, piruntime.ErrHeld
		}
		originalRef = &original.Ref
	}
	proof, err := p.runtime().VerifyCreation(ctx, handle, creationID, state.HistoryRef, originalRef)
	if err != nil || requirePiCreationInstructions(proof, instructions) != nil {
		return result, errors.Join(piruntime.ErrHeld, err)
	}
	if firstID != "" {
		// This local projection authorizes only this exact first assignment.
		// Binding and roster readiness still require its Accepted witness.
		result.State = "ready"
	}
	result.Persisted = state.Persisted || emptyProven
	policy, err := p.runtime().InspectToolPolicy(ctx, handle)
	if err != nil || piruntime.ValidateToolPolicyProof(policy, policy.Policy, handle.Generation) != nil {
		return result, errors.Join(piruntime.ErrHeld, err)
	}
	expected, err := composePiWorkflowPolicy(result.Policy.SavedAccess, policy.Policy.Mode, policy.Policy.Revision, func() []string {
		names := []string{}
		for _, name := range policy.Policy.RegisteredCeiling {
			if name == "publish_plan" || name == "request_user_input_async" || name == "report_to_lead" || name == "team_message" {
				names = append(names, name)
			}
		}
		return names
	}())
	if err != nil || !reflect.DeepEqual(expected, policy.Policy) {
		return result, piruntime.ErrHeld
	}
	operations, err := p.runtime().Bind(ctx, handle)
	if err != nil || operations.Read == nil {
		return result, errors.Join(piruntime.ErrUnavailable, err)
	}
	transcript, err := operations.Read.Read(ctx, piruntime.BackendIdentity(handle))
	if err != nil || transcript.Status == "held" || transcript.Model != state.Settings.Model || transcript.ReasoningEffort != state.Settings.Thinking {
		return result, errors.Join(piruntime.ErrHeld, err)
	}
	result.Loaded, result.Settings, result.SettingsApplied = true, state.Settings, true
	if target {
		if operations.Activity != nil {
			activity, err := operations.Activity.ReadActivity(ctx, piruntime.BackendIdentity(handle))
			if err != nil {
				return result, err
			}
			result.Activity = activity.CurrentState
		} else if transcript.Status == "idle" {
			result.Activity = "idle"
		}
	}
	return result, nil
}

func (p piTeamService) memberAdmission(ctx context.Context, handle agentbackend.ExecutionHandle, member teamruntime.PiRosterMember) (PiMemberAdmission, error) {
	state, original, err := p.runtime().ReadMemberAdmission(ctx, handle.Conversation, member.CreationID, member.InitialInputID)
	if err != nil {
		return PiMemberAdmission{}, err
	}
	if state.Conversation != handle.Conversation || state.Generation != handle.Generation || original.Ref.Conversation != handle.Conversation || original.Ref.OperationID != member.InitialInputID {
		return PiMemberAdmission{}, agentbackend.ErrBindingConflict
	}
	journal, err := readPiCreationJournal(piJournalPath(p.server.config.Workspace, handle.Conversation.Session.Slug))
	if err != nil || journal.SessionRef() != handle.Conversation.Session || (member.Addition == nil && journal.ReceiptID != member.CreationID) || (member.Addition != nil && journal.ReceiptID != member.Addition.RootCreationID) {
		return PiMemberAdmission{}, piruntime.ErrConflict
	}
	frozen := false
	if member.Addition != nil && member.Addition.Selection.Settings.Effective() == original.Settings {
		frozen = true
	}
	for _, saved := range journal.ProviderSelection.Members {
		if saved.Address == member.Address && saved.Incarnation == member.Incarnation && saved.Settings.Effective() == original.Settings {
			frozen = true
		}
	}
	if !frozen {
		return PiMemberAdmission{}, piruntime.ErrConflict
	}
	return PiMemberAdmission{HistoryRef: state.HistoryRef, Submission: original.Ref, Input: original.Input, Settings: original.Settings}, nil
}

func (p piTeamService) readyProofs(ctx context.Context, current piSessionAdmission, roster teamruntime.PiRoster) ([]piRosterReadyProof, error) {
	proofs := []piRosterReadyProof{}
	for _, member := range roster.Members {
		if member.State != "ready" {
			continue
		}
		ref, err := current.Binding.Resolve(member.Address)
		if err != nil {
			return nil, err
		}
		handle := agentbackend.ExecutionHandle{Conversation: ref, Generation: current.Authority.Generation}
		expected, err := p.memberAdmission(ctx, handle, member)
		if err != nil {
			return nil, err
		}
		proof, err := p.runtime().VerifyCreation(ctx, handle, member.CreationID, expected.HistoryRef, &expected.Submission)
		if err != nil || requirePiCreationInstructions(proof, member.Instructions) != nil {
			return nil, errors.Join(piruntime.ErrHeld, err)
		}
		proofs = append(proofs, piRosterReadyProof{Handle: handle, CreationID: member.CreationID, OperationID: member.InitialInputID, Expected: expected, Proof: proof})
	}
	return proofs, nil
}

type piTeamPublication struct{ piTeamService }

func (p piTeamPublication) VerifyCreation(ctx context.Context, handle agentbackend.ExecutionHandle, creation, history string, original *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	proof, err := p.runtime().VerifyCreation(ctx, handle, creation, history, original)
	if err != nil {
		return proof, err
	}
	store, err := teamruntime.NewStore(p.server.config.UserStateRoot, p.server.config.Workspace)
	if err != nil {
		return proof, err
	}
	roster, err := store.LoadPi(handle.Conversation.Session.Slug)
	if err != nil {
		return proof, err
	}
	for _, member := range roster.Members {
		if member.Address == handle.Conversation.Member && member.Incarnation == handle.Conversation.Incarnation {
			return proof, requirePiCreationInstructions(proof, member.Instructions)
		}
	}
	return proof, piruntime.ErrConflict
}
func (p piTeamPublication) LockRoster(ctx context.Context, handle agentbackend.ExecutionHandle) (PiMemberAdmission, func(), error) {
	store, err := teamruntime.NewStore(p.server.config.UserStateRoot, p.server.config.Workspace)
	if err != nil {
		return PiMemberAdmission{}, nil, err
	}
	release, err := store.LockOperation(ctx, handle.Conversation.Session.Slug)
	if err != nil {
		return PiMemberAdmission{}, nil, err
	}
	roster, err := store.LoadPi(handle.Conversation.Session.Slug)
	if err == nil && roster.Scope().Ref() == handle.Conversation.Session {
		for _, member := range roster.Members {
			if member.Address != handle.Conversation.Member || member.Incarnation != handle.Conversation.Incarnation {
				continue
			}
			admission, readErr := p.memberAdmission(ctx, handle, member)
			if readErr == nil {
				return admission, release, nil
			}
			err = readErr
		}
	}
	release()
	return PiMemberAdmission{}, nil, errors.Join(piruntime.ErrHeld, err)
}

func (p piTeamService) publishInitial(ctx context.Context, invocation *piTeamInvocation, state piruntime.ConversationState, original piruntime.SubmissionStatus) error {
	if original.Witness == nil || original.Phase != "Accepted" || original.Ref.OperationID != invocation.initial.Request.ClientUserMessageID || original.Input.Message != invocation.initial.Request.Message || original.Settings != invocation.initial.Settings {
		return piruntime.ErrHeld
	}
	publication, err := p.server.publishPiMemberReady(ctx, invocation.bindingRevision, invocation.handle, invocation.initial.CreationID, original.Ref.OperationID, piTeamPublication{p})
	if err != nil || !publication.Complete {
		p.server.config.Logger.Printf("Pi first assignment binding publication unavailable: %v", err)
		return errors.Join(piruntime.ErrHeld, err)
	}
	var current piSessionAdmission
	if err := p.server.withPiSessionRead(ctx, invocation.handle.Conversation.Session, func(value piSessionAdmission) error { current = value; return nil }); err != nil {
		return err
	}
	store, err := teamruntime.NewStore(p.server.config.UserStateRoot, p.server.config.Workspace)
	if err != nil {
		return err
	}
	roster, err := store.LoadPi(invocation.handle.Conversation.Session.Slug)
	if err != nil {
		return err
	}
	for i := range roster.Members {
		if roster.Members[i].Address == invocation.handle.Conversation.Member {
			if roster.Members[i].State == "ready" {
				return nil
			}
			roster.Members[i].State = "ready"
		}
	}
	proofs, err := p.readyProofs(ctx, current, roster)
	if err != nil {
		return err
	}
	admission, err := newPiRosterWriteAdmission(p.server, current, proofs)
	if err != nil {
		return err
	}
	previous := roster.Revision
	roster.Revision++
	roster.UpdatedAt = time.Now().UTC()
	err = store.SavePi(ctx, admission, invocation.handle.Generation, previous, roster, admission)
	if err != nil {
		// The original HTTP publisher and its authenticated first callback can
		// finish the same publication concurrently. Reconcile only that exact
		// already-written roster; never retry a write or native operation.
		latest, readErr := store.LoadPi(roster.Slug)
		if readErr == nil && latest.Revision == roster.Revision {
			comparison := roster
			comparison.UpdatedAt = latest.UpdatedAt
			if reflect.DeepEqual(comparison, latest) {
				return nil
			}
		}
		p.server.config.Logger.Printf("Pi first assignment roster publication unavailable: %v", err)
	}
	return err
}

// The authenticated native callback can race its first HTTP assignment's
// publication. Its original Accepted input authorizes reconciliation of that
// SAME reservation before any team tool, without another Create or Send.
func (p piTeamService) ensureCallerReady(ctx context.Context, caller teamruntime.PiCaller, inputID string) error {
	var invocation *piTeamInvocation
	var state piruntime.ConversationState
	var original piruntime.SubmissionStatus
	err := p.server.withPiSessionRead(ctx, caller.Runtime.Session, func(current piSessionAdmission) error {
		if current.Authority.Generation != caller.Handle.Generation {
			return agentbackend.ErrStaleExecution
		}
		if caller.Handle.Conversation.Member == "lead" && current.Authority.ValidateHandle(current.Binding, caller.Handle) == nil {
			return nil
		}
		store, err := teamruntime.NewStore(p.server.config.UserStateRoot, p.server.config.Workspace)
		if err != nil {
			return err
		}
		roster, err := store.LoadPi(caller.Runtime.Session.Slug)
		if err != nil || roster.Scope().Ref() != caller.Runtime.Session {
			return piruntime.ErrHeld
		}
		for _, member := range roster.Members {
			if member.Address == caller.Handle.Conversation.Member && member.Incarnation == caller.Handle.Conversation.Incarnation && member.State == "ready" && current.Authority.ValidateHandle(current.Binding, caller.Handle) == nil {
				return nil
			}
			if member.Address != caller.Handle.Conversation.Member || member.Incarnation != caller.Handle.Conversation.Incarnation || member.State != "creating" || member.InitialInputID != inputID {
				continue
			}
			state, original, err = p.runtime().ReadMemberAdmission(ctx, caller.Handle.Conversation, member.CreationID, inputID)
			if err != nil || original.Phase != "Accepted" || original.Witness == nil {
				return piruntime.ErrHeld
			}
			if _, err := p.memberAdmission(ctx, caller.Handle, member); err != nil {
				return err
			}
			assignment := teamruntime.PiInitialAssignment{Session: caller.Runtime.Session, Member: member.Address, Incarnation: member.Incarnation, CreationID: member.CreationID, Settings: original.Settings, Request: backend.SendRequest{ClientUserMessageID: inputID, Message: original.Input.Message, ActionContext: original.Input.ActionContext, Delivery: "steer"}}
			invocation = &piTeamInvocation{piTeamService: p, initial: &assignment, handle: caller.Handle, bindingRevision: current.Binding.Revision}
			return nil
		}
		return piruntime.ErrHeld
	})
	if err != nil || invocation == nil {
		return err
	}
	return p.publishInitial(ctx, invocation, state, original)
}

func (s *Server) piTeamAPI(w http.ResponseWriter, r *http.Request, summary *session.Summary, binding session.BackendBinding) {
	ref := agentbackend.SessionRef{Workspace: binding.Workspace, Slug: binding.Slug, Incarnation: binding.SessionIncarnation}
	if r.Method == http.MethodGet {
		var roster *teamruntime.PiRoster
		var derived *PiDerivedSolo
		generation := ""
		err := s.withPiSessionRead(r.Context(), ref, func(current piSessionAdmission) error {
			generation = current.Authority.Generation
			store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
			if err == nil {
				var stored teamruntime.PiRoster
				stored, err = store.LoadPi(summary.Slug)
				if err == nil {
					roster = &stored
				} else if errors.Is(err, os.ErrNotExist) {
					derived, err = derivedPiSolo(current)
				}
			}
			return err
		})
		if err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Team roster is unavailable."})
			return
		}
		response := map[string]any{"roster": roster, "backend": "pi", "session": ref, "generation": generation}
		if derived != nil {
			response["derivedSolo"] = derived
		}
		s.writeJSON(w, http.StatusOK, response)
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "unsupported team operation"})
		return
	}
	var request teamRequestBody
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.Action == "add" {
		s.piAddMemberAPI(w, r, binding, request)
		return
	}
	if request.Action != "assign" || request.Model != "" || request.Effort != "" || request.Preset != "" || request.Role != "" || request.Address != "" || request.settingsNull {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "unsupported team operation"})
		return
	}
	if request.From == "" {
		request.From = "lead"
	}
	var caller teamruntime.PiCaller
	err := s.withPiSessionRead(r.Context(), ref, func(current piSessionAdmission) error {
		lead, err := current.Binding.Resolve(request.From)
		if err != nil {
			return err
		}
		scope, scopeErr := piWorkflowScopeHeader(r)
		if scopeErr != nil || scope != piWorkflowNamespace(lead) {
			return agentbackend.ErrBindingConflict
		}
		if expected := r.Header.Get("X-Workspace-Pi-Generation"); expected != "" && expected != current.Authority.Generation {
			return agentbackend.ErrStaleExecution
		}
		runtime, ok := s.config.PiCreation.Runtime.(piruntime.CurrentRuntimeReader)
		if !ok {
			return piruntime.ErrUnavailable
		}
		live, err := runtime.ReadCurrentRuntime(r.Context(), ref)
		caller = teamruntime.PiCaller{Handle: agentbackend.ExecutionHandle{Conversation: lead, Generation: current.Authority.Generation}, Runtime: live}
		return err
	})
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	result, err := (piTeamService{s}).Peer(r.Context(), caller, teamruntime.PiMessage{ID: request.MessageID, To: request.To, Text: request.Message})
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "delivery": result})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) piSourceTeamMatches(intent session.PiCreationIntent) bool {
	if s.installedTeams == nil || s.installedTeams.Catalog == nil || intent.DirectTeam.CatalogDigest != s.installedTeams.Catalog.CatalogDigest {
		return false
	}
	base, err := teamruntime.FindCatalogPreset(*s.installedTeams.Catalog, intent.DirectTeam.ID)
	if err != nil {
		return false
	}
	if intent.DirectTeam.TeamDigest == base.TeamDigest {
		return true
	}
	projected, err := teamruntime.ProjectPiPreset(base)
	return err == nil && projected.TeamDigest == intent.DirectTeam.TeamDigest
}

func (s *Server) requirePiNewPreset(selected teamruntime.Preset) error {
	if s.installedTeams == nil || s.installedTeams.Catalog == nil || selected.CatalogDigest != s.installedTeams.Catalog.CatalogDigest {
		return piruntime.ErrUnavailable
	}
	base, err := teamruntime.FindCatalogPreset(*s.installedTeams.Catalog, selected.ID)
	if err != nil {
		return piruntime.ErrUnavailable
	}
	projected, err := teamruntime.ProjectPiPreset(base)
	if err != nil {
		return err
	}
	// An explicit lead pair is separately validated/frozen by provider policy.
	projected.LeadModel, projected.LeadEffort = selected.LeadModel, selected.LeadEffort
	if !reflect.DeepEqual(projected, selected) {
		return piruntime.ErrUnavailable
	}
	return nil
}

type piCreationRosterAdmission struct {
	ref        agentbackend.SessionRef
	generation string
	validate   func() error
}

func (a piCreationRosterAdmission) WithPiTeamWrite(ctx context.Context, ref agentbackend.SessionRef, generation string, run func() error) error {
	if ref != a.ref || generation != a.generation || a.validate == nil || ctx.Err() != nil {
		return piruntime.ErrScope
	}
	if err := a.validate(); err != nil {
		return err
	}
	return run()
}

// The existing creation owner already holds transition SH/runtime EX. This
// borrowed, one-call admission writes only the reserved roster before lead Send.
func (s *Server) reservePiCreationRoster(ctx context.Context, intent session.PiCreationIntent, binding session.BackendBinding, generation string) error {
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return err
	}
	if existing, err := store.LoadPi(intent.Slug); err == nil {
		if existing.Scope().Ref() != intent.SessionRef() || existing.LeadIncarnation != intent.Conversations[0].Incarnation || existing.TeamDigest != intent.DirectTeam.TeamDigest || len(existing.Members) != len(intent.DirectTeam.Members) {
			return piruntime.ErrConflict
		}
		for i, member := range existing.Members {
			frozen := intent.DirectTeam.Members[i]
			if member.Address != frozen.Address || member.Incarnation != intent.Conversations[i+1].Incarnation || member.CreationID != intent.ReceiptID || member.Model != frozen.Model || member.Effort != frozen.Effort || member.Instructions != frozen.Instructions || member.Access != frozen.Access || member.Purpose != frozen.Purpose || member.Behavior != frozen.Behavior {
				return piruntime.ErrConflict
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	now := time.Now().UTC()
	roster := teamruntime.PiRoster{Schema: 2, Workspace: intent.Workspace, Slug: intent.Slug, Revision: 1, CreatedAt: now, UpdatedAt: now, PresetID: intent.DirectTeam.ID, CatalogDigest: intent.DirectTeam.CatalogDigest, TeamDigest: intent.DirectTeam.TeamDigest, LeadModel: intent.DirectTeam.LeadModel, LeadEffort: intent.DirectTeam.LeadEffort, LeadInstructions: intent.DirectTeam.LeadInstructions, Backend: "pi", BackendContract: 1, SessionIncarnation: intent.SessionIncarnation, LeadIncarnation: intent.Conversations[0].Incarnation, Members: []teamruntime.PiRosterMember{}}
	indices := map[string]uint64{}
	for i, member := range intent.DirectTeam.Members {
		roster.Members = append(roster.Members, teamruntime.PiRosterMember{Address: member.Address, Role: member.Role, Index: indices[member.Role], Model: member.Model, Effort: member.Effort, Behavior: member.Behavior, Purpose: member.Purpose, Instructions: member.Instructions, Access: member.Access, PolicyCatalogDigest: intent.DirectTeam.CatalogDigest, State: "creating", AddedAt: now, Incarnation: intent.Conversations[i+1].Incarnation, CreationID: intent.ReceiptID})
		indices[member.Role]++
	}
	admission := piCreationRosterAdmission{ref: intent.SessionRef(), generation: generation, validate: func() error {
		if err := s.requireCurrentHostProfile(); err != nil {
			return err
		}
		actual, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(intent.Slug)
		if err != nil || !reflect.DeepEqual(actual, binding) {
			return piruntime.ErrConflict
		}
		return nil
	}}
	return store.SavePi(ctx, admission, generation, 0, roster)
}

var _ monitoroperation.PiTeamWriteAdmission = piCreationRosterAdmission{}
