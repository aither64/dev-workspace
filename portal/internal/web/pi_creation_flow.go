package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

// PiCreationServices is supplied only by trusted host composition after its
// launch/provider/admission checks. Construction itself performs no native call.
// Missing dependencies refuse before receipt publication or native effects.
type PiCreationServices struct {
	AccountBindings                             []piruntime.AccountBinding
	UserStateRoot, HostStateRoot, AuthDirectory string
	providers                                   *piProviderComposition
	Catalog                                     piproviders.Catalog
	Selection                                   func(context.Context, agentbackend.SessionRef, []session.PiReservedConversation, teamruntime.Preset) (piproviders.Policy, error)
	StartRequest                                func(session.PiCreationIntent, uint64) (piruntime.StartRequest, error)
	Runtime                                     PiCreationRuntime
}
type PiCreationRuntime interface {
	piruntime.CreationVerifier
	Ensure(context.Context, piruntime.StartRequest) (piruntime.RuntimeStatus, error)
	Create(context.Context, piruntime.RuntimeRef, piruntime.CreateRequest) (piruntime.ConversationState, error)
	Bind(context.Context, agentbackend.ExecutionHandle) (backend.Operations, error)
	Lookup(context.Context, piruntime.SubmissionRef) (piruntime.SubmissionStatus, error)
}

func (p *PiCreationServices) available() bool {
	return p != nil && p.Runtime != nil && p.Selection != nil && p.StartRequest != nil && piproviders.ValidateCatalog(p.Catalog) == nil
}
func piCreationID(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
func newPiInitialInputID() (string, error) {
	raw, err := piCreationID(16)
	if err != nil {
		return "", err
	}
	return raw[:8] + "-" + raw[8:12] + "-4" + raw[13:16] + "-a" + raw[17:20] + "-" + raw[20:], nil
}

// The resolved preset and immutable intent carry the selected roster. A Solo
// label alone cannot authorize silently dropping retained team members.
func requirePiSoloRoster(id string, roles []string, members int) error {
	if id != "solo" || len(roles) != 1 || roles[0] != "lead" || members != 0 {
		return fmt.Errorf("Pi creation requires an explicitly selected Solo starting team: %w", piruntime.ErrUnavailable)
	}
	return nil
}

func requirePiCreationInstructions(proof piruntime.CreationProof, original string) error {
	expected := ""
	if original != "" {
		digest := sha256.Sum256([]byte(original))
		expected = hex.EncodeToString(digest[:])
	}
	if proof.InstructionsDigest != expected {
		return fmt.Errorf("The original Pi lead instructions could not be verified: %w", piruntime.ErrHeld)
	}
	return nil
}

func (s *Server) installPiCreationLocked(request creationRequest, receiptID, expectedEpoch string, destinationLocked bool) (creationReceipt, error) {
	if request.Backend != "pi" || request.Kind != "new" || request.Source != "" || request.SourceThreadID != "" || request.SourceIdentity != "" || request.PlanText != "" || request.PlanTurnID != "" || request.PlanSHA256 != "" || request.DestinationSettings || !session.ValidSlug(request.Slug) {
		return creationReceipt{}, errors.New("Pi creation requires an explicit new session")
	}
	if previous, exists := s.creations[request.Slug]; exists {
		if previous.Schema == 4 && sameCreationRequest(previous.Request, request) && (receiptID == "" || previous.ReceiptID == receiptID && previous.DeletionHistorySHA256 == expectedEpoch) && previous.DirectTeam != nil && request.directTeam != nil && reflect.DeepEqual(*previous.DirectTeam, *request.directTeam) {
			return previous, nil
		}
		return creationReceipt{}, agentbackend.ErrBindingConflict
	}
	if s.closing {
		return creationReceipt{}, piruntime.ErrUnavailable
	}
	if err := s.piCreationAvailability(); err != nil {
		return creationReceipt{}, err
	}
	if request.directTeam == nil || validDirectTeamPreset(*request.directTeam) != nil || request.Team != request.directTeam.ID || request.CatalogDigest != request.directTeam.CatalogDigest {
		return creationReceipt{}, errors.New("Pi creation requires an exact trusted team")
	}
	if err := requirePiSoloRoster(request.directTeam.ID, request.directTeam.Roles, len(request.directTeam.Members)); err != nil {
		return creationReceipt{}, err
	}
	if err := s.requirePiRoleInstructions(request.directTeam.LeadInstructions); err != nil {
		return creationReceipt{}, err
	}
	if !destinationLocked {
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
	}
	if err := s.requireCurrentHostProfile(); err != nil {
		return creationReceipt{}, err
	}
	if err := s.requireCodexSessionBackend(request.Slug); err != nil {
		return creationReceipt{}, err
	}
	for _, preparation := range s.preparations {
		if preparation.State != "terminal" && preparation.Slug == request.Slug && preparation.ReceiptID != receiptID {
			return creationReceipt{}, errors.New("session has another accepted owner")
		}
	}
	for _, root := range []string{"work", "archive", "worktrees"} {
		if _, err := os.Lstat(filepath.Join(s.config.Workspace, root, request.Slug)); err == nil {
			return creationReceipt{}, errors.New("session name exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return creationReceipt{}, err
		}
	}
	if owner, err := session.PendingLifecycle(s.config.Workspace, request.Slug); err != nil || owner != "" {
		return creationReceipt{}, errors.New("session has unfinished lifecycle work")
	}
	if _, err := os.Lstat(filepath.Join(s.config.AuthorityDir, request.Slug+".json")); !errors.Is(err, os.ErrNotExist) {
		return creationReceipt{}, errors.New("session has retained runtime authority")
	}
	history, err := session.CompletedRemovalHistory(s.config.Workspace, request.Slug, s.config.UserStateRoot)
	if err != nil {
		return creationReceipt{}, err
	}
	if receiptID != "" {
		if !messageDigestPattern.MatchString(receiptID) || history != expectedEpoch {
			return creationReceipt{}, agentbackend.ErrBindingConflict
		}
	} else {
		receiptID, err = piCreationID(32)
		if err != nil {
			return creationReceipt{}, err
		}
	}
	incarnation, err := piCreationID(32)
	if err != nil {
		return creationReceipt{}, err
	}
	conversations := []session.PiReservedConversation{}
	for _, address := range request.directTeam.Roles {
		memberID, err := piCreationID(32)
		if err != nil {
			return creationReceipt{}, err
		}
		conversations = append(conversations, session.PiReservedConversation{Address: address, Incarnation: memberID})
	}
	ref := agentbackend.SessionRef{Workspace: s.config.Workspace, Slug: request.Slug, Incarnation: incarnation}
	policy, err := s.config.PiCreation.Selection(s.operationContext, ref, conversations, *request.directTeam)
	if err != nil {
		return creationReceipt{}, err
	}
	selection, err := session.FreezePiCreationSelection(s.config.PiCreation.Catalog, policy, conversations)
	if err != nil {
		return creationReceipt{}, err
	}
	initialID, err := newPiInitialInputID()
	if err != nil {
		return creationReceipt{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	preset := *request.directTeam
	receipt := creationReceipt{Schema: 4, Workspace: s.config.Workspace, Request: request, ReceiptID: receiptID, DeletionHistorySHA256: history, Attempt: 1, State: "running", Phase: "Preparing Pi session", StartedAt: now, UpdatedAt: now, Validated: true, Goal: normalizedCreationGoal(request.Goal), Model: preset.LeadModel, Effort: preset.LeadEffort, DirectTeam: &preset, Pi: &piCreationIdentity{BackendContract: 1, SessionIncarnation: incarnation, Conversations: conversations, Provider: selection.Members[0].Settings.Provider, InitialInputID: initialID, Selection: selection}}
	intent, err := receipt.piIntent()
	if err != nil {
		return creationReceipt{}, err
	}
	if err = intent.Validate(s.config.PiCreation.Catalog); err != nil {
		return creationReceipt{}, err
	}
	if data, err := json.Marshal(intent); err != nil || len(data) > 64<<10 {
		return creationReceipt{}, errors.New("Pi creation intent exceeds existing journal bounds")
	}
	if err := s.retireReadyCreations(1); err != nil {
		return creationReceipt{}, err
	}
	if err := s.saveCreation(receipt); err != nil {
		return creationReceipt{}, err
	}
	s.creations[request.Slug] = receipt
	s.operationWG.Add(1)
	// Both destinationLocked=false local defers and the preparation caller
	// owning destinationLocked=true release destination locks before their
	// enclosing admission releases operationMu. Wait for that admission only.
	go func() {
		s.operationMu.Lock()
		s.operationMu.Unlock()
		s.runCreation(receipt)
	}()
	return receipt, nil
}
func piJournalPath(workspace, slug string) string {
	return filepath.Join(workspace, "worktrees", ".locks", slug+".creation.json")
}
func writePiCreationRecord(path string, value any, maximum int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maximum {
		return errors.New("Pi creation evidence exceeds existing bounds")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".pi-creation-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func readPiCreationIntent(path string) (session.PiCreationIntent, error) {
	var raw json.RawMessage
	if err := readCreationJSON(path, &raw); err != nil {
		return session.PiCreationIntent{}, err
	}
	return session.DecodePiCreationIntent(raw)
}
func readPiCreationJournal(path string) (session.PiCreationJournal, error) {
	var raw json.RawMessage
	if err := readCreationJSON(path, &raw); err != nil {
		return session.PiCreationJournal{}, err
	}
	return session.DecodePiCreationJournal(raw)
}
func (s *Server) piCreationPoint(point string) error {
	if s.piCreationCheckpoint != nil {
		return s.piCreationCheckpoint(point)
	}
	return nil
}

// Called with transition SH held; never across a Ruby tracking child. All
// effects re-read the actual accepted receipt and package after lock waits.
func (s *Server) withPiCreationOwner(ctx context.Context, receipt creationReceipt, fn func(session.PiCreationIntent) error) error {
	creation, err := session.LockCreation(s.config.AuthorityDir, receipt.Request.Slug)
	if err != nil {
		return err
	}
	defer creation.Close()
	runtime, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, receipt.Request.Slug)
	if err != nil {
		return err
	}
	defer runtime.Close()
	if err = s.requireCurrentHostProfile(); err != nil {
		return err
	}
	var current creationReceipt
	var raw map[string]json.RawMessage
	if err = readCreationJSON(s.creationPath(receipt.Request.Slug), &raw); err != nil {
		return err
	}
	if err = validateCreationReceiptShape(raw); err != nil {
		return err
	}
	if err = readCreationJSON(s.creationPath(receipt.Request.Slug), &current); err != nil {
		return err
	}
	intent, err := receipt.piIntent()
	if err != nil {
		return err
	}
	if intent.Workspace != s.config.Workspace || intent.Slug != receipt.Request.Slug {
		return agentbackend.ErrBindingConflict
	}
	if !piReceiptMatches(current, intent) || current.Attempt != receipt.Attempt || current.State != "running" {
		return errors.New("Pi creation owner changed")
	}
	if owner, err := session.PendingLifecycle(s.config.Workspace, intent.Slug); err != nil || owner != "" {
		return errors.New("Pi creation has unfinished lifecycle work")
	}
	history, err := session.CompletedRemovalHistory(s.config.Workspace, intent.Slug, s.config.UserStateRoot)
	if err != nil {
		return err
	}
	if history != intent.DeletionHistorySHA256 {
		return agentbackend.ErrBindingConflict
	}
	if !s.config.PiCreation.available() {
		return piruntime.ErrUnavailable
	}
	if err = intent.Validate(s.config.PiCreation.Catalog); err != nil {
		return err
	}
	if err = requirePiSoloRoster(intent.DirectTeam.ID, intent.DirectTeam.Roles, len(intent.DirectTeam.Members)); err != nil {
		return err
	}
	if err = s.requirePiRoleInstructions(intent.DirectTeam.LeadInstructions); err != nil {
		return err
	}
	s.piWorkflows.invalidateSession(intent.SessionRef())
	return fn(intent)
}
func (s *Server) preparePiCreation(ctx context.Context, receipt creationReceipt) error {
	return s.withPiCreationOwner(ctx, receipt, func(intent session.PiCreationIntent) error {
		companion := s.creationEvidencePath(receipt) + ".request"
		saved, err := readPiCreationIntent(companion)
		if errors.Is(err, os.ErrNotExist) {
			if err = writePiCreationRecord(companion, intent, 64<<10); err != nil {
				return err
			}
			if err = s.piCreationPoint("companion"); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !reflect.DeepEqual(saved, intent) {
			return agentbackend.ErrBindingConflict
		}
		path := piJournalPath(intent.Workspace, intent.Slug)
		journal, err := readPiCreationJournal(path)
		store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
		if errors.Is(err, os.ErrNotExist) {
			if _, bindingErr := store.Load(intent.Slug); !errors.Is(bindingErr, agentbackend.ErrNotBound) {
				return agentbackend.ErrBindingConflict
			}
			journal = session.PiCreationJournal{PiCreationIntent: intent, State: "creating"}
			if err = writePiCreationRecord(path, journal, 64<<10); err != nil {
				return err
			}
			if err = s.piCreationPoint("journal"); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !reflect.DeepEqual(journal.PiCreationIntent, intent) {
			return agentbackend.ErrBindingConflict
		}
		binding, err := store.Load(intent.Slug)
		if errors.Is(err, agentbackend.ErrNotBound) {
			if journal.NativeCreateStarted {
				return errors.New("Pi binding lost after native create intent")
			}
			if _, err = store.Create(ctx, intent.Binding()); err != nil {
				return err
			}
			return s.piCreationPoint("binding")
		}
		if err != nil {
			return err
		}
		return intent.ValidateBinding(binding)
	})
}

type piTrackingResult struct {
	Schema             int    `json:"schema"`
	Phase              string `json:"phase"`
	Workspace          string `json:"workspace"`
	Slug               string `json:"slug"`
	ReceiptID          string `json:"receiptId"`
	SessionIncarnation string `json:"sessionIncarnation"`
	BindingRevision    uint64 `json:"bindingRevision"`
	Generation         string `json:"generation"`
	TrackingDevice     uint64 `json:"trackingDevice"`
	TrackingInode      uint64 `json:"trackingInode"`
}

func (s *Server) piTrackingPhase(ctx context.Context, transition *os.File, receipt creationReceipt, phase string, binding session.BackendBinding, generation string) (piTrackingResult, error) {
	file, err := os.CreateTemp(s.creationDirectory(), ".pi-goal-*")
	if err != nil {
		return piTrackingResult{}, err
	}
	defer os.Remove(file.Name())
	if _, err = file.WriteString(receipt.Goal); err != nil {
		file.Close()
		return piTrackingResult{}, err
	}
	if err = file.Close(); err != nil {
		return piTrackingResult{}, err
	}
	args := []string{"start", receipt.Request.Slug, "--as-is", "--backend", "pi", "--creation-phase", phase, "--creation-receipt-id", receipt.ReceiptID, "--creation-evidence", s.creationEvidencePath(receipt), "--session-incarnation", binding.SessionIncarnation, "--binding-revision", fmt.Sprint(binding.Revision), "--goal-file", file.Name(), "--json"}
	if generation != "" {
		args = append(args, "--execution-generation", generation)
	}
	if receipt.Request.Sensitive {
		args = append(args, "--sensitive")
	}
	stdout, stderr, err := s.runCreationCommand(ctx, 210*time.Second, transition, s.creationProgressObserver(receipt), args...)
	if err != nil {
		return piTrackingResult{}, commandFailure("Pi tracking phase", stdout, stderr, err)
	}
	var result piTrackingResult
	if err = session.DecodePiCreationJSON([]byte(stdout), &result, 16384); err != nil {
		return result, err
	}
	if result.Schema != 4 || result.Phase != phase || result.Workspace != s.config.Workspace || result.Slug != receipt.Request.Slug || result.ReceiptID != receipt.ReceiptID || result.SessionIncarnation != binding.SessionIncarnation || result.BindingRevision != binding.Revision || result.Generation != generation || result.TrackingInode == 0 {
		return result, agentbackend.ErrBindingConflict
	}
	return result, nil
}

func (s *Server) initializePiCreation(receipt creationReceipt) error {
	if err := s.piCreationAvailability(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.operationContext, 4*time.Minute)
	defer cancel()
	transition, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer release()
	if err = s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if s.provePiCreation(receipt) == nil {
		return nil
	}
	intent, err := receipt.piIntent()
	if err != nil {
		return err
	}
	if err := requirePiSoloRoster(intent.DirectTeam.ID, intent.DirectTeam.Roles, len(intent.DirectTeam.Members)); err != nil {
		return err
	}
	if err = s.preparePiCreation(ctx, receipt); err != nil {
		return err
	}
	var binding session.BackendBinding
	if err = s.withPiCreationOwner(ctx, receipt, func(intent session.PiCreationIntent) error {
		var err error
		binding, err = (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(intent.Slug)
		return err
	}); err != nil {
		return err
	}
	prepared, err := s.piTrackingPhase(ctx, transition, receipt, "prepare-tracking", binding, "")
	if err != nil {
		return err
	}
	if err = s.piCreationPoint("tracking"); err != nil {
		return err
	}
	var handle agentbackend.ExecutionHandle
	if err = s.withPiCreationOwner(ctx, receipt, func(intent session.PiCreationIntent) error {
		summary, err := session.Find(intent.Workspace, intent.Slug)
		if err != nil {
			return err
		}
		trackingReady := summary.Creation.State == "ready"
		if trackingReady && binding.State != "ready" {
			return agentbackend.ErrBindingConflict
		}
		if err := s.validatePiTracking(intent, prepared, trackingReady); err != nil {
			return err
		}
		journal, err := readPiCreationJournal(piJournalPath(intent.Workspace, intent.Slug))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(journal.PiCreationIntent, intent) {
			return agentbackend.ErrBindingConflict
		}
		if journal.NativeCreateStarted && journal.Native == nil {
			return errors.New("Pi native create outcome is Unknown; retained intent cannot create again")
		}
		var initialPolicy *piruntime.ToolPolicyTarget
		if !journal.NativeCreateStarted {
			initialPolicy, err = s.initialPiWorkflowPolicy(intent, "lead")
			if err != nil {
				return err
			}
		}
		store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
		binding, err = store.Load(intent.Slug)
		if err != nil {
			return err
		}
		if err = intent.ValidateBinding(binding); err != nil {
			return err
		}
		start, err := s.config.PiCreation.StartRequest(intent, binding.Revision)
		if err != nil {
			return err
		}
		if start.Session != intent.SessionRef() || start.BindingRevision == 0 || start.BindingRevision > binding.Revision {
			return agentbackend.ErrBindingConflict
		}
		if intent.DirectTeam.LeadInstructions != "" && start.AppStorePath != s.config.PackageRoot {
			return piruntime.ErrHeld
		}
		if start.BindingRevision != binding.Revision {
			reader, ok := s.config.PiCreation.Runtime.(interface {
				ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
			})
			if !ok {
				return piruntime.ErrHeld
			}
			saved, err := reader.ReadStart(ctx, intent.SessionRef())
			if err != nil || saved != start {
				return piruntime.ErrConflict
			}
		}
		status, err := s.config.PiCreation.Runtime.Ensure(ctx, start)
		if err != nil {
			return err
		}
		if status.Phase != "ready" || status.Runtime.Session != intent.SessionRef() || !messageDigestPattern.MatchString(status.Runtime.Generation) || (journal.NativeGeneration != "" && journal.NativeGeneration != status.Runtime.Generation) {
			return piruntime.ErrHeld
		}
		if initialPolicy != nil {
			if err := s.piWorkflows.ensureAttachment(ctx, status.Runtime); err != nil {
				return err
			}
		}
		handle = agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: intent.SessionRef(), Backend: agentbackend.Pi, Member: "lead", Incarnation: intent.Conversations[0].Incarnation}, Generation: status.Runtime.Generation}
		authority := session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: intent.Workspace, Slug: intent.Slug, SessionIncarnation: intent.SessionIncarnation, BindingRevision: binding.Revision, Generation: handle.Generation, State: "starting"}
		if binding.State == "ready" {
			authority.State = "ready"
		}
		if err = session.PublishPiRuntimeAuthority(s.config.AuthorityDir, binding, authority); err != nil {
			return err
		}
		createdHere := false
		if !journal.NativeCreateStarted {
			journal.NativeCreateStarted = true
			journal.NativeGeneration = handle.Generation
			if err = writePiCreationRecord(piJournalPath(intent.Workspace, intent.Slug), journal, 64<<10); err != nil {
				return err
			}
			if err = s.piCreationPoint("native_intent"); err != nil {
				return err
			}
			state, err := s.config.PiCreation.Runtime.Create(ctx, status.Runtime, piruntime.CreateRequest{Session: intent.SessionRef(), Member: "lead", Incarnation: handle.Conversation.Incarnation, CreationID: intent.ReceiptID, Instructions: intent.DirectTeam.LeadInstructions, Settings: intent.ProviderSelection.Members[0].Settings.Effective(), ToolPolicy: initialPolicy})
			if err != nil {
				return fmt.Errorf("Pi native create outcome is Unknown: %w", err)
			}
			expected := handle.Conversation
			expected.NativeID = state.Conversation.NativeID
			if state.Conversation != expected || state.Generation != handle.Generation || (state.State != "idle") || !validCreationOpaqueID(state.HistoryRef, 4096) || !validCreationOpaqueID(state.Conversation.NativeID, 4096) || state.Settings != intent.ProviderSelection.Members[0].Settings.Effective() {
				return errors.New("Pi native create result does not prove the reserved identity")
			}
			journal.Native = &session.PiCreationNative{NativeID: state.Conversation.NativeID, HistoryRef: state.HistoryRef}
			if err = writePiCreationRecord(piJournalPath(intent.Workspace, intent.Slug), journal, 64<<10); err != nil {
				return err
			}
			if err = s.piCreationPoint("native_result"); err != nil {
				return err
			}
			createdHere = true
		}
		handle.Conversation.NativeID = journal.Native.NativeID
		expected := piRootAdmission(intent, journal, handle, receipt.Goal)
		if createdHere {
			// The same live initialized worker can accept the real first input without
			// opening a nonexistent history. Recovery never enters this Send branch.
			initialized, err := s.config.PiCreation.Runtime.VerifyCreation(ctx, handle, intent.ReceiptID, journal.Native.HistoryRef, nil)
			if err != nil {
				return err
			}
			if err := requirePiCreationInstructions(initialized, intent.DirectTeam.LeadInstructions); err != nil {
				return err
			}
			if initialized.Conversation != handle.Conversation || initialized.Generation != handle.Generation || initialized.CreationID != intent.ReceiptID || initialized.HistoryRef != journal.Native.HistoryRef {
				return agentbackend.ErrBindingConflict
			}
			policy, err := s.piWorkflows.runtime.InspectToolPolicy(ctx, handle)
			if err != nil || initialPolicy == nil || initialPolicy.Mode != "default" || initialPolicy.Revision != 1 || piruntime.ValidateToolPolicyProof(policy, *initialPolicy, handle.Generation) != nil || !slices.Equal(policy.Registered, initialPolicy.RegisteredCeiling) {
				return errors.Join(piruntime.ErrHeld, err)
			}
			operations, err := s.config.PiCreation.Runtime.Bind(ctx, handle)
			if err != nil {
				return err
			}
			if operations.Send == nil {
				return piruntime.ErrUnavailable
			}
			_, sendErr := operations.Send.Send(ctx, piruntime.BackendIdentity(handle), backend.SendRequest{Message: receipt.Goal, ClientUserMessageID: intent.InitialInputID, ActionContext: "initial", Retry: false})
			if err = s.piCreationPoint("initial_input"); err != nil {
				return err
			}
			if sendErr != nil {
				if _, lookupErr := s.config.PiCreation.Runtime.Lookup(ctx, expected.Submission); lookupErr != nil {
					return fmt.Errorf("Pi initial input is held: %w", sendErr)
				}
			}
		}
		accepted, err := s.config.PiCreation.Runtime.Lookup(ctx, expected.Submission)
		if err != nil {
			return fmt.Errorf("Pi original initial-input receipt is unavailable; outcome held: %w", err)
		}
		proof, err := s.config.PiCreation.Runtime.VerifyCreation(ctx, handle, intent.ReceiptID, journal.Native.HistoryRef, &expected.Submission)
		if err != nil {
			return err
		}
		if err := requirePiCreationInstructions(proof, intent.DirectTeam.LeadInstructions); err != nil {
			return err
		}
		if proof.Bootstrap == nil || !reflect.DeepEqual(*proof.Bootstrap, accepted) {
			return agentbackend.ErrConversationNotReady
		}
		if err = validatePiMemberProof(handle, intent.ReceiptID, intent.InitialInputID, expected, proof); err != nil {
			return err
		}
		if binding.State != "ready" {
			next := binding
			next.Conversations = append([]session.BackendConversation(nil), binding.Conversations...)
			next.Revision++
			next.State = "ready"
			next.Conversations[0].State = "ready"
			next.Conversations[0].NativeID = handle.Conversation.NativeID
			binding, err = store.CompareAndSwap(ctx, binding.Revision, next)
			if err != nil {
				return err
			}
			if err = s.piCreationPoint("ready_binding"); err != nil {
				return err
			}
		} else if binding.Conversations[0].NativeID != handle.Conversation.NativeID {
			return agentbackend.ErrBindingConflict
		}
		authority.BindingRevision = binding.Revision
		authority.State = "ready"
		if err = session.PublishPiRuntimeAuthority(s.config.AuthorityDir, binding, authority); err != nil {
			return err
		}
		return s.piCreationPoint("ready_authority")
	}); err != nil {
		return err
	}
	// No creation/runtime/team/message ownership crosses either child.
	completed, err := s.piTrackingPhase(ctx, transition, receipt, "complete-tracking", binding, handle.Generation)
	if err != nil {
		return err
	}
	if err = s.piCreationPoint("ready_tracking"); err != nil {
		return err
	}
	return s.withPiCreationOwner(ctx, receipt, func(intent session.PiCreationIntent) error {
		current, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(intent.Slug)
		if err != nil {
			return err
		}
		if current.Revision != binding.Revision {
			return agentbackend.ErrBindingConflict
		}
		authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, intent.Slug, intent.Workspace)
		if err != nil {
			return err
		}
		if err = authority.ValidateHandle(current, handle); err != nil {
			return err
		}
		journal, err := readPiCreationJournal(piJournalPath(intent.Workspace, intent.Slug))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(journal.PiCreationIntent, intent) || journal.Native == nil || journal.Native.NativeID != handle.Conversation.NativeID {
			return agentbackend.ErrBindingConflict
		}
		expected := piRootAdmission(intent, journal, handle, receipt.Goal)
		proof, err := s.config.PiCreation.Runtime.VerifyCreation(ctx, handle, intent.ReceiptID, journal.Native.HistoryRef, &expected.Submission)
		if err != nil {
			return err
		}
		if err := requirePiCreationInstructions(proof, intent.DirectTeam.LeadInstructions); err != nil {
			return err
		}
		if err = validatePiMemberProof(handle, intent.ReceiptID, intent.InitialInputID, expected, proof); err != nil {
			return err
		}
		if err = s.validatePiTracking(intent, completed, true); err != nil {
			return err
		}
		evidence := piCreationEvidence{Schema: 4, Workspace: intent.Workspace, Slug: intent.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: intent.ReceiptID, SessionIncarnation: intent.SessionIncarnation, BindingRevision: current.Revision, Conversations: intent.Conversations, NativeID: handle.Conversation.NativeID, InitialInputID: intent.InitialInputID, InitialSubmissionDigest: expected.Submission.Digest, GoalSHA256: intent.GoalSHA256, Model: intent.Model, Effort: intent.Effort, Provider: intent.Provider, ProviderSelection: intent.ProviderSelection, CatalogDigest: intent.DirectTeam.CatalogDigest, TeamDigest: intent.DirectTeam.TeamDigest, Team: intent.DirectTeam.ID, TrackingDevice: completed.TrackingDevice, TrackingInode: completed.TrackingInode}
		if old, err := s.readPiCreationEvidence(receipt); err == nil {
			if !reflect.DeepEqual(old, evidence) {
				return agentbackend.ErrBindingConflict
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = writePiCreationRecord(s.creationEvidencePath(receipt), evidence, maxCreationReceiptBytes); err != nil {
			return err
		}
		if err = s.piCreationPoint("completion"); err != nil {
			return err
		}
		journal.State = "ready"
		if err = writePiCreationRecord(piJournalPath(intent.Workspace, intent.Slug), journal, 64<<10); err != nil {
			return err
		}
		return s.piCreationPoint("ready_journal")
	})
}
func piRootAdmission(intent session.PiCreationIntent, journal session.PiCreationJournal, handle agentbackend.ExecutionHandle, goal string) PiMemberAdmission {
	input := piruntime.SendInput{Message: goal, ActionContext: "initial", Retry: false}
	settings := intent.ProviderSelection.Members[0].Settings.Effective()
	ref := piruntime.SubmissionRef{Conversation: handle.Conversation, OperationID: intent.InitialInputID, Action: "send"}
	ref.Digest = piruntime.SubmissionDigest(ref, input, settings)
	return PiMemberAdmission{HistoryRef: journal.Native.HistoryRef, Submission: ref, Input: input, Settings: settings}
}
func (s *Server) validatePiTracking(intent session.PiCreationIntent, result piTrackingResult, ready bool) error {
	summary, err := session.Find(intent.Workspace, intent.Slug)
	if err != nil {
		return err
	}
	if summary.Archived || summary.Sensitive != intent.Sensitive || summary.Schema != 4 || summary.Backend == nil || summary.Backend.SessionIncarnation != intent.SessionIncarnation || summary.Creation.GoalSHA256 != intent.GoalSHA256 || summary.Creation.State == "ready" != ready || summary.Creation.InitialGoalSent != ready {
		return agentbackend.ErrBindingConflict
	}
	var stat unix.Stat_t
	if err = unix.Lstat(filepath.Join(intent.Workspace, "work", intent.Slug), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Dev != result.TrackingDevice || stat.Ino != result.TrackingInode {
		return agentbackend.ErrBindingConflict
	}
	return nil
}
