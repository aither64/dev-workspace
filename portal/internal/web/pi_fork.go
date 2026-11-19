package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type piForkRequestWire struct {
	Kind                        string `json:"kind"`
	Slug                        string `json:"slug"`
	Backend                     string `json:"backend"`
	Source                      string `json:"source"`
	SelectedMember              string `json:"selectedMember"`
	Sensitive                   bool   `json:"sensitive,omitempty"`
	BrowserNotificationsEnabled *bool  `json:"browserNotificationsEnabled,omitempty"`
}
type piForkReceiptWire struct {
	Schema                int                    `json:"schema"`
	Workspace             string                 `json:"workspace"`
	Request               piForkRequestWire      `json:"request"`
	ReceiptID             string                 `json:"receiptId"`
	DeletionHistorySHA256 string                 `json:"deletionHistorySha256"`
	Attempt               int                    `json:"attempt"`
	State                 string                 `json:"state"`
	Phase                 string                 `json:"phase"`
	Error                 string                 `json:"error,omitempty"`
	StartedAt             string                 `json:"startedAt"`
	UpdatedAt             string                 `json:"updatedAt"`
	Validated             bool                   `json:"validated"`
	Model                 string                 `json:"model"`
	Effort                string                 `json:"effort"`
	DirectTeam            session.PiCreationTeam `json:"directTeam"`
	Pi                    piCreationIdentity     `json:"pi"`
}

func validatePiForkReceiptShape(raw map[string]json.RawMessage) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var wire piForkReceiptWire
	if err = session.DecodePiCreationJSON(data, &wire, maxCreationReceiptBytes); err != nil {
		return err
	}
	if wire.Schema != 5 || wire.Request.Kind != "fork" || wire.Request.Backend != "pi" || !wire.Validated || wire.Attempt < 1 || wire.Phase == "" || len(wire.Phase) > 4096 || len(wire.Error) > 4096 {
		return agentbackend.ErrBindingConflict
	}
	if wire.State != "running" && wire.State != "paused" && wire.State != "failed" && wire.State != "ready" && wire.State != "conflict" {
		return agentbackend.ErrBindingConflict
	}
	if _, err = time.Parse(time.RFC3339Nano, wire.StartedAt); err != nil {
		return err
	}
	if _, err = time.Parse(time.RFC3339Nano, wire.UpdatedAt); err != nil {
		return err
	}
	var receipt creationReceipt
	if err = json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	if receipt.DirectTeam == nil || validDirectTeamPreset(*receipt.DirectTeam) != nil {
		return agentbackend.ErrBindingConflict
	}
	intent, err := receipt.piIntent()
	if err != nil {
		return err
	}
	if intent.Fork.Source.Slug != wire.Request.Source || intent.Fork.SelectedMember != wire.Request.SelectedMember {
		return agentbackend.ErrBindingConflict
	}
	var pi map[string]json.RawMessage
	_ = json.Unmarshal(raw["pi"], &pi)
	if _, present := pi["initialInputId"]; present {
		return errors.New("Pi fork cannot contain an initial input")
	}
	return nil
}

func (s *Server) piForkAPI(w http.ResponseWriter, r *http.Request, source *session.Summary) {
	var body struct {
		Name                        string `json:"name"`
		CreationDate                string `json:"creationDate"`
		Sensitive                   bool   `json:"sensitive,omitempty"`
		BrowserNotificationsEnabled *bool  `json:"browserNotificationsEnabled,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if !session.ValidSlug(body.Name) || len(body.Name) > 48 {
		workflowError(w, piruntime.ErrScope)
		return
	}
	date, err := time.Parse(time.DateOnly, body.CreationDate)
	if err != nil || date.Format(time.DateOnly) != body.CreationDate {
		workflowError(w, piruntime.ErrScope)
		return
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(source.Slug)
	if err != nil {
		workflowError(w, err)
		return
	}
	member := r.URL.Query().Get("member")
	if member == "" {
		member = "lead"
	}
	ref, err := binding.Resolve(member)
	if err != nil {
		workflowError(w, err)
		return
	}
	scoped, err := scopedPiConversationRequest(r, false)
	if err != nil {
		workflowError(w, err)
		return
	}
	if err = checkExpectedPiScope(scoped.Context(), ref); err != nil {
		workflowError(w, err)
		return
	}
	receipt, err := s.acceptCreation(creationRequest{Backend: "pi", Kind: "fork", Slug: body.CreationDate + "-" + body.Name, Source: source.Slug, SelectedMember: member, Sensitive: body.Sensitive, BrowserNotificationsEnabled: body.BrowserNotificationsEnabled, piForkSource: &ref})
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusAccepted, receipt.status())
}

// Capture is observational and checks the complete team before any destination
// reservation. In particular an absent roster is accepted only for original Solo.
func (s *Server) capturePiFork(ctx context.Context, request creationRequest, receiptID, incarnation string) (session.PiCreationIntent, *teamruntime.Preset, error) {
	var out session.PiCreationIntent
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(request.Source)
	if err != nil {
		return out, nil, err
	}
	ref := agentbackend.SessionRef{Workspace: s.config.Workspace, Slug: binding.Slug, Incarnation: binding.SessionIncarnation}
	if request.piForkSource != nil {
		ref = request.piForkSource.Session
	}
	current, release, err := s.acquirePiSessionAdmission(ctx, ref, true)
	if err != nil {
		return out, nil, err
	}
	defer release()
	if request.piForkSource != nil {
		selected, e := current.Binding.Resolve(request.SelectedMember)
		if e != nil || selected != *request.piForkSource {
			return out, nil, agentbackend.ErrBindingConflict
		}
	}
	fork, ok := s.config.PiCreation.Runtime.(piruntime.ForkController)
	if !ok {
		return out, nil, piruntime.ErrUnavailable
	}
	if err = s.requirePiForkContract(); err != nil {
		return out, nil, err
	}
	rosterStore, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return out, nil, err
	}
	roster, err := rosterStore.LoadPi(ref.Slug)
	if errors.Is(err, os.ErrNotExist) {
		if _, err = derivedPiSolo(current); err != nil {
			return out, nil, err
		}
	} else if err != nil {
		return out, nil, err
	}
	if current.Binding.Revision != binding.Revision {
		return out, nil, agentbackend.ErrBindingConflict
	}
	leadRef, err := current.Binding.Resolve("lead")
	if err != nil {
		return out, nil, err
	}
	access, err := s.piSavedAccessLocked(current, leadRef)
	if err != nil {
		return out, nil, err
	}
	preset := teamruntime.Preset{ID: current.intent.DirectTeam.ID, Name: current.intent.DirectTeam.Name, Description: current.intent.DirectTeam.Description, CatalogDigest: current.intent.DirectTeam.CatalogDigest, TeamDigest: current.intent.DirectTeam.TeamDigest, LeadModel: current.intent.Model, LeadEffort: current.intent.Effort, LeadInstructions: current.intent.DirectTeam.LeadInstructions, Roles: []string{"lead"}, Members: []teamruntime.MemberSpec{}}
	data, _ := json.Marshal(current.intent.DirectTeam)
	_ = json.Unmarshal(data, &preset)
	// The current physical roster owns saved changes and additions.
	if roster.Schema != 0 {
		preset.Roles = []string{"lead"}
		preset.Members = []teamruntime.MemberSpec{}
		preset.LeadModel, preset.LeadEffort, preset.LeadInstructions = roster.LeadModel, roster.LeadEffort, roster.LeadInstructions
		for _, m := range roster.Members {
			preset.Roles = append(preset.Roles, m.Address)
			preset.Members = append(preset.Members, teamruntime.MemberSpec{Role: m.Role, Address: m.Address, Model: m.Model, Effort: m.Effort, Behavior: m.Behavior, Purpose: m.Purpose, Instructions: m.Instructions, Access: m.Access})
		}
	}
	out = session.PiCreationIntent{Schema: 5, Workspace: s.config.Workspace, Slug: request.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: receiptID, SessionIncarnation: incarnation, Model: preset.LeadModel, Effort: preset.LeadEffort, Sensitive: request.Sensitive, Fork: &session.PiForkIntent{Source: ref, SourceCreationID: current.intent.ReceiptID, SourceBindingRevision: current.Binding.Revision, SourceRosterRevision: roster.Revision, SelectedMember: request.SelectedMember, LeadAccess: access, Members: []session.PiForkMember{}}, Conversations: []session.PiReservedConversation{}}
	if _, e := piruntime.SessionStateDirectory(s.config.UserStateRoot, out.SessionRef()); e != nil {
		return out, nil, errors.New("Pi session names use lowercase letters, numbers and hyphens.")
	}
	out.Fork.RuntimeRequestID, err = newPiInitialInputID()
	if err != nil {
		return out, nil, err
	}
	root, err := piruntime.SessionStateDirectory(s.config.UserStateRoot, ref)
	if err != nil {
		return out, nil, err
	}
	policy, policyErr := (piproviders.Store{Root: root, Catalog: s.config.PiCreation.Catalog, Session: ref}).Load()
	if policyErr != nil && !errors.Is(policyErr, os.ErrNotExist) {
		return out, nil, policyErr
	}
	basePolicy := policy
	if policyErr != nil {
		basePolicy = current.intent.ProviderSelection.Policy.Public()
	}
	desired := piproviders.Policy{Session: out.SessionRef(), Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: copyPiSelection(basePolicy.Default.Selection)}, Overrides: []piproviders.Override{}}
	for n, bound := range current.Binding.Conversations {
		state, added, removed := "ready", time.Now().UTC().Format(time.RFC3339Nano), ""
		instructions := preset.LeadInstructions
		var member *teamruntime.PiRosterMember
		if n > 0 {
			m := roster.Members[n-1]
			member = &m
			state = m.State
			added = m.AddedAt.Format(time.RFC3339Nano)
			if m.RemovedAt != nil {
				removed = m.RemovedAt.Format(time.RFC3339Nano)
			}
			instructions = m.Instructions
		}
		if state == "archived" {
			return out, nil, fmt.Errorf("Pi member %s is archived; every materialized member must already be loaded and idle before forking", bound.Address)
		}
		id, e := piCreationID(32)
		if e != nil {
			return out, nil, e
		}
		creation := receiptID
		if n > 0 {
			creation, e = piCreationID(32)
			if e != nil {
				return out, nil, e
			}
		}
		dest := session.PiReservedConversation{Address: bound.Address, Incarnation: id}
		out.Conversations = append(out.Conversations, dest)
		m := session.PiForkMember{Address: bound.Address, SourceIncarnation: bound.Incarnation, CreationID: creation, State: state, AddedAt: added, RemovedAt: removed}
		if state == "removed" {
			out.Fork.Members = append(out.Fork.Members, m)
			continue
		}
		selection, source, catalog, revision := s.piForkSourceSelection(current.intent, bound, member, policy, policyErr == nil)
		m.PolicyStored = policyErr == nil
		m.Selection, m.SelectionSource, m.CatalogDigest, m.PolicyRevision = &selection, source, catalog, revision
		if source == "member_override" {
			desired.Overrides = append(desired.Overrides, piproviders.Override{Member: piproviders.MemberKey{Address: dest.Address, Incarnation: dest.Incarnation}, Value: piproviders.VersionedSelection{Revision: 1, Selection: selection.Selection}})
		} else if !reflect.DeepEqual(selection.Selection, desired.Default.Selection) {
			return out, nil, errors.New("Pi source members disagree about their saved session default")
		}

		if state == "creating" {
			if member != nil && member.Addition != nil {
				p := member.Addition.ToolPolicy
				m.ReservedToolPolicy = &p
			} else {
				if len(out.Fork.Members) == 0 || out.Fork.Members[0].Origin == nil || out.Fork.Members[0].Origin.Snapshot.ToolPolicy == nil {
					return out, nil, piruntime.ErrHeld
				}
				hostTools := []string{}
				for _, name := range out.Fork.Members[0].Origin.Snapshot.ToolPolicy.RegisteredCeiling {
					if name == "publish_plan" || name == "report_to_lead" || name == "team_message" || name == "request_user_input_async" {
						hostTools = append(hostTools, name)
					}
				}
				p, e := composePiWorkflowPolicy(member.Access, "default", 1, hostTools)
				if e != nil {
					return out, nil, e
				}
				m.ReservedToolPolicy = &p
			}
			if bound.State != "creating" || bound.NativeID != "" || member == nil || member.InitialInputID != "" || member.Fork != nil {
				return out, nil, fmt.Errorf("Pi member %s has unresolved work", bound.Address)
			}
			// First assignment saves its input ID before any native Create. Together
			// with exact mapping absence this proves the reservation never started.
			_, e := os.Lstat(filepath.Join(root, "members", bound.Address, bound.Incarnation, "conversation.json"))
			if !errors.Is(e, os.ErrNotExist) {
				return out, nil, fmt.Errorf("Pi member %s is not positively unstarted", bound.Address)
			}

		} else {
			conversation, e := current.Binding.Resolve(bound.Address)
			if e != nil {
				return out, nil, e
			}
			handle := agentbackend.ExecutionHandle{Conversation: conversation, Generation: current.Authority.Generation}
			snapshot, e := fork.CaptureFork(ctx, handle)
			if e != nil {
				if errors.Is(e, piruntime.ErrUnavailable) {
					return out, nil, fmt.Errorf("Independent forks are unavailable for member %s in its current Pi runtime: %w", bound.Address, e)
				}
				return out, nil, fmt.Errorf("Member %s must be loaded and idle, with its history saved, before you can fork the session: %w", bound.Address, e)
			}
			if snapshot.Instructions != instructions || snapshot.Native.SourceNativeID != bound.NativeID || len(selection.Selection.Chain) == 0 {
				return out, nil, piruntime.ErrHeld
			}
			target := selection.Selection.Chain[0]
			nativeProvider := ""
			for _, provider := range s.config.PiCreation.Catalog.Providers {
				if provider.ID == target.ProviderID {
					nativeProvider = provider.NativeID
				}
			}
			if snapshot.Settings.Provider != nativeProvider || snapshot.Settings.AccountRef != target.AccountRef || snapshot.Settings.Model != target.ModelID || snapshot.Settings.Thinking != target.Thinking {
				return out, nil, fmt.Errorf("Pi member %s desired settings differ from its applied settings", bound.Address)
			}
			m.NativeID, e = newPiInitialInputID()
			if e != nil {
				return out, nil, e
			}
			m.Origin = &piruntime.ForkOrigin{Source: handle, Snapshot: snapshot}
			if n == 0 {
				preset.LeadModel, preset.LeadEffort = snapshot.Settings.Model, snapshot.Settings.Thinking
				out.Model, out.Effort, out.Provider = snapshot.Settings.Model, snapshot.Settings.Thinking, snapshot.Settings.Provider
			} else {
				preset.Members[n-1].Model, preset.Members[n-1].Effort = snapshot.Settings.Model, snapshot.Settings.Thinking
			}
		}
		out.Fork.Members = append(out.Fork.Members, m)
	}
	if s.config.PiCreation.providers != nil {
		if err = s.config.PiCreation.providers.validatePolicy(desired); err != nil {
			return out, nil, err
		}
	}
	out.ProviderSelection, err = session.FreezePiCreationSelection(s.config.PiCreation.Catalog, desired, out.ActiveConversations())
	if err != nil {
		return out, nil, err
	}
	data, _ = json.Marshal(preset)
	if err = json.Unmarshal(data, &out.DirectTeam); err != nil {
		return out, nil, err
	}
	return out, &preset, nil
}

func (s *Server) piForkSourceSelection(intent session.PiCreationIntent, bound session.BackendConversation, member *teamruntime.PiRosterMember, policy piproviders.Policy, stored bool) (piproviders.VersionedSelection, string, string, uint64) {
	catalog := s.config.PiCreation.Catalog.Digest
	if !stored {
		policy, catalog = intent.ProviderSelection.Policy.Public(), intent.ProviderSelection.CatalogDigest
	}
	key := piproviders.MemberKey{Address: bound.Address, Incarnation: bound.Incarnation}
	policy, addition := piMemberDesiredPolicy(policy, key, member)
	if addition {
		catalog = member.Addition.ProviderCatalogDigest
	}
	value, source := policy.Default, "session_default"
	for _, override := range policy.Overrides {
		if override.Member == key {
			value, source = override.Value, "member_override"
		}
	}
	value.Selection = copyPiSelection(value.Selection)
	return value, source, catalog, policy.Revision
}

func (s *Server) requirePiForkContract() error {
	if session.PiForkVersion() != 1 {
		return piruntime.ErrUnavailable
	}
	data, err := os.ReadFile(filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	var c struct {
		Fork     int                           `json:"piFork"`
		Creation int                           `json:"piCreationSchema"`
		Team     session.PiTeamRecordsContract `json:"piTeamRecords"`
	}
	if json.Unmarshal(data, &c) != nil || c.Fork != 1 || c.Creation != 5 || c.Team.Roster < 4 {
		return errors.New("Upgrade the workspace package before forking this Pi session.")
	}
	return nil
}

func (s *Server) installPiForkLocked(request creationRequest) (creationReceipt, error) {
	if request.Kind != "fork" || request.Backend != "pi" || !session.ValidSlug(request.Slug) || !session.ValidSlug(request.Source) || request.Source == request.Slug || request.SelectedMember == "" || request.Goal != "" || request.Model != "" || request.Effort != "" || request.Provider != "" || request.SourceThreadID != "" || request.SourceIdentity != "" || request.DestinationSettings || request.PlanText != "" || request.PlanTurnID != "" || request.PlanSHA256 != "" || request.Team != "" || request.CatalogDigest != "" {
		return creationReceipt{}, piruntime.ErrScope
	}
	if previous, ok := s.creations[request.Slug]; ok {
		if previous.Schema == 5 && sameCreationRequest(previous.Request, request) {
			return previous, nil
		}
		return creationReceipt{}, agentbackend.ErrBindingConflict
	}
	if s.closing || !s.config.PiCreation.available() {
		return creationReceipt{}, piruntime.ErrUnavailable
	}
	receiptID, err := piCreationID(32)
	if err != nil {
		return creationReceipt{}, err
	}
	incarnation, err := piCreationID(32)
	if err != nil {
		return creationReceipt{}, err
	}
	// This is the mandatory all-member preflight. It precedes every destination
	// lock, intent, runtime reservation and tracking write.
	intent, preset, err := s.capturePiFork(s.operationContext, request, receiptID, incarnation)
	if err != nil {
		return creationReceipt{}, err
	}
	creation, err := session.LockCreation(s.config.AuthorityDir, request.Slug)
	if err != nil {
		return creationReceipt{}, err
	}
	defer creation.Close()
	runtime, err := session.LockRuntimeExclusive(s.config.AuthorityDir, request.Slug)
	if err != nil {
		return creationReceipt{}, err
	}
	defer runtime.Close()
	if err = s.requireCurrentHostProfile(); err != nil {
		return creationReceipt{}, err
	}
	if err = s.requireCodexSessionBackend(request.Slug); err != nil {
		return creationReceipt{}, err
	}
	for _, p := range s.preparations {
		if p.State != "terminal" && p.Slug == request.Slug {
			return creationReceipt{}, errors.New("session has another accepted owner")
		}
	}
	for _, directory := range []string{"work", "archive", "worktrees"} {
		if _, e := os.Lstat(filepath.Join(s.config.Workspace, directory, request.Slug)); !errors.Is(e, os.ErrNotExist) {
			return creationReceipt{}, errors.New("session name exists")
		}
	}
	if owner, e := session.PendingLifecycle(s.config.Workspace, request.Slug); e != nil || owner != "" {
		return creationReceipt{}, piruntime.ErrHeld
	}
	if _, e := os.Lstat(filepath.Join(s.config.AuthorityDir, request.Slug+".json")); !errors.Is(e, os.ErrNotExist) {
		return creationReceipt{}, piruntime.ErrHeld
	}
	intent.DeletionHistorySHA256, err = session.CompletedRemovalHistory(s.config.Workspace, request.Slug, s.config.UserStateRoot)
	if err != nil {
		return creationReceipt{}, err
	}
	if err = intent.Validate(s.config.PiCreation.Catalog); err != nil {
		return creationReceipt{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := creationReceipt{Schema: 5, Workspace: s.config.Workspace, Request: request, ReceiptID: receiptID, DeletionHistorySHA256: intent.DeletionHistorySHA256, Attempt: 1, State: "running", Phase: "Preparing independent Pi fork", StartedAt: now, UpdatedAt: now, Validated: true, Model: intent.Model, Effort: intent.Effort, DirectTeam: preset, Pi: &piCreationIdentity{BackendContract: 1, SessionIncarnation: incarnation, Conversations: intent.Conversations, Provider: intent.Provider, Selection: intent.ProviderSelection, Fork: intent.Fork}}
	if err = s.retireReadyCreations(1); err != nil {
		return creationReceipt{}, err
	}
	if err = s.saveCreation(receipt); err != nil {
		return creationReceipt{}, err
	}
	s.creations[request.Slug] = receipt
	s.operationWG.Add(1)
	go func() { s.operationMu.Lock(); s.operationMu.Unlock(); s.runCreation(receipt) }()
	return receipt, nil
}

func validatePiForkProof(h agentbackend.ExecutionHandle, creationID, nativeID string, origin piruntime.ForkOrigin, p piruntime.ForkProof) error {
	expected := ""
	if origin.Snapshot.Instructions != "" {
		expected = piruntime.MessageDigest(origin.Snapshot.Instructions)
	}
	if h.Conversation.NativeID != nativeID || p.Conversation != h.Conversation || p.Generation != h.Generation || p.CreationID != creationID || p.HistoryRef == "" || !p.Persisted || !reflect.DeepEqual(p.Origin, origin) || (p.Settings.Revision < origin.Snapshot.Settings.Revision || p.Settings.Revision == origin.Snapshot.Settings.Revision && p.Settings != origin.Snapshot.Settings) || p.InstructionsDigest != expected {
		return agentbackend.ErrConversationNotReady
	}
	if origin.Snapshot.ToolPolicy != nil {
		if p.ToolPolicy == nil || piruntime.ValidateToolPolicyProof(*p.ToolPolicy, p.ToolPolicy.Policy, h.Generation) != nil || p.ToolPolicy.Policy.Revision < origin.Snapshot.ToolPolicy.Revision || !reflect.DeepEqual(p.ToolPolicy.Policy.RegisteredCeiling, origin.Snapshot.ToolPolicy.RegisteredCeiling) {
			return agentbackend.ErrConversationNotReady
		}
	} else if p.ToolPolicy != nil {
		return agentbackend.ErrConversationNotReady
	}
	return nil
}
