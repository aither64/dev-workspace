package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

var errPiAddRuntimeUpgrade = errors.New("Upgrade this session’s Pi runtime before adding this member.")

// Initial creation remains an exact count/order join. Only a ready session can
// append, and every extra binding entry needs its own immutable Add reservation.
func (s *Server) validatePiReadyBinding(intent session.PiCreationIntent, binding session.BackendBinding) error {
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return err
	}
	roster, err := store.LoadPi(binding.Slug)
	if errors.Is(err, os.ErrNotExist) && len(binding.Conversations) == len(intent.Conversations) {
		return intent.ValidateBinding(binding)
	}
	if err != nil {
		return err
	}
	return validatePiExtendedBinding(intent, binding, roster)
}

func validatePiExtendedBinding(intent session.PiCreationIntent, binding session.BackendBinding, roster teamruntime.PiRoster) error {
	n := len(intent.Conversations)
	if n == 0 || len(binding.Conversations) < n || len(binding.Conversations) != len(roster.Members)+1 || roster.Scope().Ref() != intent.SessionRef() || roster.LeadIncarnation != intent.Conversations[0].Incarnation {
		return agentbackend.ErrBindingConflict
	}
	prefix := binding
	prefix.Conversations = binding.Conversations[:n]
	if intent.ValidateBinding(prefix) != nil || roster.Validate() != nil {
		return agentbackend.ErrBindingConflict
	}
	for index, member := range roster.Members {
		bound := binding.Conversations[index+1]
		if bound.Address != member.Address || bound.Incarnation != member.Incarnation {
			return agentbackend.ErrBindingConflict
		}
		if index+1 < n {
			if member.Addition != nil || member.CreationID != intent.ReceiptID {
				return agentbackend.ErrBindingConflict
			}
			continue
		}
		a := member.Addition
		if a != nil && a.OriginalSolo && (intent.Schema != 4 || intent.DirectTeam.ID != "solo" || n != 1 || index != 0) {
			return agentbackend.ErrBindingConflict
		}
		if roster.Schema != 3 || a == nil || a.RootCreationID != intent.ReceiptID || a.PriorBindingRevision >= binding.Revision {
			return agentbackend.ErrBindingConflict
		}
		configuration, err := piproviders.LoadInstalled(a.PackageRoot)
		if err != nil || configuration.Catalog.Digest != a.ProviderCatalogDigest || piproviders.ValidatePolicy(configuration.Catalog, a.Policy) != nil {
			return errors.Join(piruntime.ErrHeld, err)
		}
		resolved, err := piproviders.Resolve(configuration.Catalog, a.Policy, piproviders.MemberKey{Address: member.Address, Incarnation: member.Incarnation}, 1)
		if err != nil || resolved.Settings != a.Selection.Settings.Effective() || resolved.PolicyDigest != a.Selection.PolicyDigest || resolved.Source != a.Selection.Source {
			return errors.Join(piruntime.ErrConflict, err)
		}
	}
	return nil
}

func piAddPayload(request teamRequestBody) string {
	b, _ := json.Marshal([]string{request.Role, request.Model, request.Effort})
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

func (s *Server) piAddMemberAPI(w http.ResponseWriter, r *http.Request, binding session.BackendBinding, request teamRequestBody) {
	if request.Preset != "" || request.Address != "" || request.To != "" || request.Message != "" || (request.From != "" && request.From != "lead") || request.settingsNull || len(request.MessageID) != 36 || !queueClientMessageIDPattern.MatchString(request.MessageID) || (request.Model == "") != (request.Effort == "") {
		workflowError(w, piruntime.ErrScope)
		return
	}
	member, roster, err := s.addPiMember(r.Context(), r, binding, request)
	if err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"member": member, "roster": roster, "backend": "pi"})
}

// This operation alone admits its exact half-published tuple. Ordinary read and
// dispatch admission continue requiring a complete binding/authority/roster join.
func (s *Server) addPiMember(ctx context.Context, requestHTTP *http.Request, supplied session.BackendBinding, request teamRequestBody) (teamruntime.PiRosterMember, teamruntime.PiRoster, error) {
	var selected teamruntime.PiRosterMember
	var roster teamruntime.PiRoster
	file, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return selected, roster, err
	}
	defer release()
	invocation := s.piPackageInvocation(file)
	defer invocation.expire()
	if err := invocation.recheck(); err != nil {
		return selected, roster, err
	}
	contractBytes, err := os.ReadFile(filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json"))
	var contract struct {
		Team json.RawMessage `json:"piTeamRecords"`
	}
	if err != nil || len(contractBytes) > 64<<10 || json.Unmarshal(contractBytes, &contract) != nil {
		return selected, roster, piruntime.ErrUnavailable
	}
	codecs, err := session.DecodePiTeamRecordsContract(contract.Team)
	if err != nil || codecs.Roster != 3 {
		return selected, roster, piruntime.ErrUnavailable
	}
	lock, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, supplied.Slug)
	if err != nil {
		return selected, roster, err
	}
	defer lock.Close()
	if err := invocation.recheck(); err != nil {
		return selected, roster, err
	}
	if err := s.requireProviderAdmission(supplied.Slug); err != nil {
		return selected, roster, err
	}
	if pending, err := session.PendingLifecycle(s.config.Workspace, supplied.Slug); err != nil || pending != "" {
		return selected, roster, errors.Join(piruntime.ErrHeld, err)
	}
	bindings := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, err := bindings.Load(supplied.Slug)
	if err != nil || binding.SessionIncarnation != supplied.SessionIncarnation || binding.State != "ready" {
		return selected, roster, errors.Join(piruntime.ErrConflict, err)
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, binding.Slug, binding.Workspace)
	if err != nil || authority.State != "ready" {
		return selected, roster, errors.Join(piruntime.ErrHeld, err)
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return selected, roster, err
	}
	roster, err = store.LoadPi(binding.Slug)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		return selected, roster, err
	}

	for _, m := range roster.Members {
		if m.Addition != nil && m.Addition.RequestID == request.MessageID {
			if selected.Address != "" || m.Addition.PayloadDigest != piAddPayload(request) {
				return selected, roster, piruntime.ErrConflict
			}
			selected = m
		}
	}
	// Build only this operation's prior projection for retained-root admission.
	priorBinding, priorRoster := binding, roster
	priorRoster.Members = append([]teamruntime.PiRosterMember{}, roster.Members...)
	complete := selected.Address != "" && authority.ValidateBinding(binding) == nil && len(binding.Conversations) == len(roster.Members)+1
	if selected.Address != "" && !complete {
		a := selected.Addition
		if a.Generation != authority.Generation || a.RootCreationID != binding.CreationReceiptID || roster.Members[len(roster.Members)-1].Address != selected.Address {
			return selected, roster, piruntime.ErrConflict
		}
		priorRoster.Members = priorRoster.Members[:len(priorRoster.Members)-1]
		if len(binding.Conversations) == len(roster.Members)+1 {
			last := binding.Conversations[len(binding.Conversations)-1]
			if last.Address != selected.Address || last.Incarnation != selected.Incarnation || last.State != "creating" || last.NativeID != "" || binding.Revision != a.PriorBindingRevision+1 {
				return selected, roster, piruntime.ErrConflict
			}
			priorBinding.Conversations = append([]session.BackendConversation{}, binding.Conversations[:len(binding.Conversations)-1]...)
			priorBinding.Revision = a.PriorBindingRevision
		} else if len(binding.Conversations) != len(roster.Members) || binding.Revision != a.PriorBindingRevision {
			return selected, roster, piruntime.ErrConflict
		}
		if authority.BindingRevision != a.PriorBindingRevision && authority.BindingRevision != binding.Revision {
			return selected, roster, piruntime.ErrConflict
		}
	}
	// The retained root validator reads the immutable original intent and proves
	// package/tracking/evidence. Temporarily projecting records on disk is forbidden.
	receipt := creationReceipt{ReceiptID: binding.CreationReceiptID, Request: creationRequest{Slug: binding.Slug}}
	intent, err := readPiCreationIntent(s.creationEvidencePath(receipt) + ".request")
	if err != nil {
		return selected, roster, err
	}
	originalAbsent := absent || selected.Addition != nil && selected.Addition.OriginalSolo && !complete
	if !originalAbsent && validatePiExtendedBinding(intent, priorBinding, priorRoster) != nil {
		return selected, roster, piruntime.ErrConflict
	}
	if originalAbsent && intent.ValidateBinding(priorBinding) != nil {
		return selected, roster, piruntime.ErrConflict
	}

	projectedAuthority := authority
	projectedAuthority.BindingRevision = priorBinding.Revision
	current := piSessionAdmission{Binding: priorBinding, Authority: projectedAuthority, intent: intent}
	if originalAbsent {
		if _, err := derivedPiSolo(current); err != nil {
			return selected, roster, err
		}
		if _, err := s.piRetainedRootIdentityWithAbsentSoloLocked(current); err != nil {
			return selected, roster, err
		}
	} else if _, err := s.piRetainedRootIdentityWithRosterLocked(current, &priorRoster); err != nil {
		return selected, roster, err
	}

	lead, err := binding.Resolve("lead")
	if err != nil {
		return selected, roster, err
	}
	scope, err := piWorkflowScopeHeader(requestHTTP)
	if err != nil || scope != piWorkflowNamespace(lead) {
		return selected, roster, agentbackend.ErrBindingConflict
	}
	if expected := requestHTTP.Header.Get("X-Workspace-Pi-Generation"); expected != "" && expected != authority.Generation {
		return selected, roster, agentbackend.ErrStaleExecution
	}
	if access, err := s.piSavedAccessLocked(current, lead); err != nil || access != "workspace_write" {
		return selected, roster, errors.Join(piruntime.ErrScope, err)
	}
	runtime, ok := s.config.PiCreation.Runtime.(interface {
		piruntime.CurrentRuntimeReader
		ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
	})
	if !ok {
		return selected, roster, piruntime.ErrUnavailable
	}
	live, err := runtime.ReadCurrentRuntime(ctx, intent.SessionRef())
	if err != nil || live.Generation != authority.Generation {
		return selected, roster, errors.Join(piruntime.ErrHeld, err)
	}
	if complete {
		return selected, roster, nil
	}
	if selected.Address == "" {
		if authority.ValidateBinding(binding) != nil {
			return selected, roster, agentbackend.ErrBindingConflict
		}
		var previous *teamruntime.PiRoster
		if !absent {
			saved := roster
			previous = &saved
		}
		selected, err = s.newPiAddition(ctx, current, previous, request, runtime)
		if err != nil {
			return selected, roster, err
		}
		// Registration is proved by the exact current attachment ACK, separately
		// from the retained root's immutable two/four-tool registered ceiling.
		if err := s.piWorkflows.ensureAttachment(ctx, live); err != nil {
			return selected, roster, err
		}
		admission := piCreationRosterAdmission{ref: intent.SessionRef(), generation: authority.Generation, validate: invocation.recheck}
		next := roster
		if absent {
			next = teamruntime.PiRoster{Schema: 3, Revision: 1, Workspace: intent.Workspace, Slug: intent.Slug, CreatedAt: selected.AddedAt, UpdatedAt: selected.AddedAt, PresetID: intent.DirectTeam.ID, CatalogDigest: intent.DirectTeam.CatalogDigest, TeamDigest: intent.DirectTeam.TeamDigest, LeadModel: intent.DirectTeam.LeadModel, LeadEffort: intent.DirectTeam.LeadEffort, LeadInstructions: intent.DirectTeam.LeadInstructions, Members: []teamruntime.PiRosterMember{selected}, Backend: "pi", BackendContract: 1, SessionIncarnation: intent.SessionIncarnation, LeadIncarnation: intent.Conversations[0].Incarnation}
		} else {
			next.Schema = 3
			next.Revision++
			next.UpdatedAt = selected.AddedAt
			next.Members = append(append([]teamruntime.PiRosterMember{}, roster.Members...), selected)
		}
		roster, err = store.AppendPi(ctx, admission, authority.Generation, previous, next)
		if err != nil {
			return selected, roster, err
		}
		if err := s.piCreationPoint("add_roster"); err != nil {
			return selected, roster, err
		}
	}
	a := selected.Addition
	if binding.Revision == a.PriorBindingRevision {
		next := binding
		next.Revision++
		next.Conversations = append(append([]session.BackendConversation{}, binding.Conversations...), session.BackendConversation{Address: selected.Address, Incarnation: selected.Incarnation, State: "creating"})
		binding, err = bindings.AppendReserved(ctx, binding.Revision, next)
		if err != nil {
			return selected, roster, err
		}
		if err := s.piCreationPoint("add_binding"); err != nil {
			return selected, roster, err
		}
	}
	authority.BindingRevision = binding.Revision
	if err := session.PublishPiRuntimeAuthority(s.config.AuthorityDir, binding, authority); err != nil {
		return selected, roster, err
	}
	if err := s.piCreationPoint("add_authority"); err != nil {
		return selected, roster, err
	}
	s.piWorkflows.invalidateSession(intent.SessionRef())
	return selected, roster, nil
}

func (s *Server) newPiAddition(ctx context.Context, current piSessionAdmission, roster *teamruntime.PiRoster, request teamRequestBody, runtime interface {
	ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
}) (teamruntime.PiRosterMember, error) {
	var m teamruntime.PiRosterMember
	if s.installedTeams == nil || s.installedTeams.Catalog == nil || s.config.PiCreation.providers == nil || s.piWorkflows == nil {
		return m, piruntime.ErrUnavailable
	}
	var members []teamruntime.PiRosterMember
	var priorRevision uint64
	if roster != nil {
		members = roster.Members
		priorRevision = roster.Revision
	}
	active := 0
	for _, m := range members {
		if m.State != "removed" {
			active++
		}
	}
	if active >= s.installedTeams.Catalog.Capacity.RequiredNativeChildThreads {
		return m, teamruntime.ErrPiPolicy
	}
	role, err := teamruntime.ResolveRole(s.installedTeams.Catalog, current.intent.DirectTeam.ID, request.Role, true)
	if err != nil {
		return m, err
	}
	model, effort := request.Model, request.Effort
	if model == "" {
		projection, err := teamruntime.ProjectPiPreset(teamruntime.Preset{CatalogDigest: s.installedTeams.Catalog.CatalogDigest, TeamDigest: current.intent.DirectTeam.TeamDigest, Members: []teamruntime.MemberSpec{{Role: request.Role, Model: role.Model, Effort: role.Effort}}})
		if err != nil {
			return m, err
		}
		model, effort = projection.Members[0].Model, projection.Members[0].Effort
	}
	address, index, err := teamruntime.NextPiAddress(members, request.Role)
	if err != nil {
		return m, err
	}
	incarnation, err := piCreationID(32)
	if err != nil {
		return m, err
	}
	creationID, err := piCreationID(32)
	if err != nil {
		return m, err
	}
	p := s.config.PiCreation.providers
	policy := piproviders.Policy{Session: current.intent.SessionRef(), Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: copyPiSelection(p.configuration.Default)}, Overrides: []piproviders.Override{}}
	selection := copyPiSelection(p.configuration.Default)
	selection.Chain[0].ModelID = model
	selection.Chain[0].Thinking = effort
	key := piproviders.MemberKey{Address: address, Incarnation: incarnation}
	policy.Overrides = append(policy.Overrides, piproviders.Override{Member: key, Value: piproviders.VersionedSelection{Revision: 1, Selection: selection}})
	if err := p.validatePolicy(policy); err != nil {
		return m, err
	}
	start, err := runtime.ReadStart(ctx, current.intent.SessionRef())
	if err != nil {
		return m, err
	}
	workerBytes, err := os.ReadFile(filepath.Join(start.AppStorePath, "share/dev-workspace/pi-worker.json"))
	if err != nil {
		return m, errPiAddRuntimeUpgrade
	}
	worker, err := piruntime.DecodeWorkerMetadata(workerBytes)
	if err != nil || worker.Protocol != 2 {
		return m, errPiAddRuntimeUpgrade
	}
	contract, err := os.ReadFile(filepath.Join(start.AppStorePath, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return m, err
	}
	if _, err := session.DecodePiRoleInstructionsContract(contract); err != nil {
		return m, errPiAddRuntimeUpgrade
	}
	frozen, err := piproviders.LoadInstalled(start.AppStorePath)
	if err != nil {
		return m, err
	}
	accounts, err := piBuiltinAccounts(frozen.Catalog)
	if err != nil {
		return m, err
	}
	if piproviders.ValidatePolicy(frozen.Catalog, policy) != nil || requirePiSupportedTargets(frozen.Catalog, selection.Chain, accounts) != nil {
		return m, errPiAddRuntimeUpgrade
	}
	resolved, err := piproviders.Resolve(p.configuration.Catalog, policy, key, 1)
	if err != nil {
		return m, err
	}
	toolPolicy, err := composePiWorkflowPolicy(role.Access, "default", 1, s.piWorkflows.tools)
	if err != nil {
		return m, err
	}
	m = teamruntime.PiRosterMember{Address: address, Role: request.Role, Index: index, Model: model, Effort: effort, Behavior: role.Behavior, Purpose: role.Purpose, Instructions: role.Instructions, Access: role.Access, PolicyCatalogDigest: s.installedTeams.Catalog.CatalogDigest, State: "creating", AddedAt: time.Now().UTC(), Incarnation: incarnation, CreationID: creationID}
	x := resolved.Settings
	m.Addition = &teamruntime.PiAdditionReservation{RequestID: request.MessageID, PayloadDigest: piAddPayload(request), RootCreationID: current.intent.ReceiptID, OriginalSolo: roster == nil, PriorRosterRevision: priorRevision, PriorBindingRevision: current.Binding.Revision, Generation: current.Authority.Generation, PackageRoot: s.config.PackageRoot, ProviderCatalogDigest: p.configuration.Catalog.Digest, Policy: policy, Selection: session.PiCreationMemberSelection{Address: address, Incarnation: incarnation, Source: resolved.Source, PolicyDigest: resolved.PolicyDigest, Settings: session.PiCreationSettings{Provider: x.Provider, AccountRef: x.AccountRef, Model: x.Model, Thinking: x.Thinking, PolicyRevision: x.PolicyRevision, Revision: x.Revision}}, ToolPolicy: toolPolicy}
	return m, nil
}
