package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

type piLifecycleRuntime interface {
	TerminalCallbacks(context.Context, piruntime.RuntimeRef, []piruntime.LifecycleMember, bool) ([]piruntime.LifecycleCallback, error)
	RetireTerminalCallbacks(context.Context, piruntime.RuntimeRef, []piruntime.LifecycleMember, []piruntime.LifecycleCallback) (piruntime.RuntimeStatus, error)
	CheckLifecycle(context.Context, piruntime.RuntimeRef, []agentbackend.ConversationRef) error
	InterruptLifecycle(context.Context, piruntime.RuntimeRef, []agentbackend.ConversationRef, func(context.Context, piruntime.RuntimeRef, piruntime.HostCallbackConnector) error) error
	piruntime.CurrentRuntimeReader
	CaptureLifecycleMembers(context.Context, piruntime.RuntimeRef, []piruntime.LifecycleSlot) ([]piruntime.LifecycleMember, error)
	Retire(context.Context, piruntime.RuntimeRef, []agentbackend.ConversationRef, func(context.Context, piruntime.RuntimeRef, piruntime.HostCallbackConnector) error) (piruntime.RuntimeStatus, error)
	ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
	Upgrade(context.Context, piruntime.UpgradeRequest, piruntime.UpgradeHooks) (piruntime.UpgradeResult, error)
}

// Borrow only the admitted Ruby lifecycle owner's exact OFD. Closing this
// duplicate must never unlock or recursively acquire its session exclusion.
func piLifecycleParent(config Config, slug string) (*os.File, error) {
	operation := os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION")
	if operation != "archive" && operation != "delete" && operation != "revive" {
		return nil, piruntime.ErrScope
	}
	fd, err := strconv.Atoi(os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_FD"))
	if err != nil || fd < 3 {
		return nil, piruntime.ErrScope
	}
	path := filepath.Join(config.AuthorityDir, slug+".lock")
	if os.Getenv("DEV_SESSION_LIFECYCLE_LOCK_PATH") != path {
		return nil, piruntime.ErrScope
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "pi-lifecycle-parent")
	fail := func(err error) (*os.File, error) { file.Close(); return nil, err }
	probe, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	defer unix.Close(probe)
	var held, current unix.Stat_t
	if unix.Fstat(duplicate, &held) != nil || unix.Fstat(probe, &current) != nil || held.Mode&unix.S_IFMT != unix.S_IFREG || current.Mode&unix.S_IFMT != unix.S_IFREG || held.Mode&0777 != 0600 || current.Mode&0777 != 0600 || held.Uid != uint32(os.Geteuid()) || current.Uid != held.Uid || held.Dev != current.Dev || held.Ino != current.Ino {
		return fail(piruntime.ErrScope)
	}
	if err := unix.Flock(probe, unix.LOCK_SH|unix.LOCK_NB); err != unix.EAGAIN && err != unix.EWOULDBLOCK {
		return fail(piruntime.ErrScope)
	}
	if err := unix.Flock(duplicate, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(piruntime.ErrScope)
	}
	return file, nil
}

func newPiLifecycleServer(config Config, fixture *PiCreationServices) (*Server, error) {
	profile, err := readHostProfileIdentity(config.HostProfile)
	if err != nil {
		return nil, err
	}
	selected, err := filepath.EvalSymlinks(config.HostProfile)
	if err != nil {
		return nil, err
	}
	config.PackageRoot = selected
	installed, err := agentteams.LoadInstalled(selected)
	if err != nil {
		return nil, err
	}
	providers, err := piproviders.LoadInstalled(selected)
	if err != nil {
		return nil, err
	}
	config.PiProviderConfig = &providers
	if err := validatePiPackage(config, profile); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(selected, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return nil, err
	}
	var contract map[string]json.RawMessage
	var version int
	if session.DecodePiCreationJSON(data, &contract, 64<<10) != nil || json.Unmarshal(contract["piLifecycle"], &version) != nil || version != 1 {
		return nil, piruntime.ErrUnavailable
	}
	var services *PiCreationServices
	if fixture == nil {
		services, err = NewPiCreationServices(config)
	} else {
		services, err = newPiSelectionServices(config)
		if err == nil && services != nil {
			services.Runtime = fixture.Runtime
		}
	}
	if err != nil || services == nil {
		return nil, errors.Join(piruntime.ErrUnavailable, err)
	}
	config.PiCreation = services
	if config.Logger == nil {
		config.Logger = log.New(os.Stderr, "", 0)
	}
	store, err := newLifecycleOperationStore(config.Workspace, config.UserStateRoot)
	if err != nil {
		return nil, err
	}
	s := &Server{config: config, hostProfile: profile, installedTeams: &installed, operationStore: store}
	s.piWorkflows = newPiWorkflowComposition(s)
	if s.piWorkflows == nil {
		return nil, piruntime.ErrUnavailable
	}
	return s, nil
}

func (s *Server) capturePiLifecycle(ctx context.Context, slug string) (session.PiLifecycleCapture, error) {
	return s.capturePiLifecycleFor(ctx, slug, false)
}

func (s *Server) capturePiLifecycleFor(ctx context.Context, slug string, forceDelete bool) (session.PiLifecycleCapture, error) {
	var capture session.PiLifecycleCapture
	b, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(slug)
	if err != nil {
		return capture, err
	}
	a, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, slug, s.config.Workspace)
	if err != nil || a.ValidateBinding(b) != nil {
		return capture, errors.Join(piruntime.ErrScope, err)
	}
	runtime, ok := s.config.PiCreation.Runtime.(piLifecycleRuntime)
	if !ok {
		return capture, piruntime.ErrUnavailable
	}
	ref := agentbackend.SessionRef{Workspace: b.Workspace, Slug: b.Slug, Incarnation: b.SessionIncarnation}
	current, err := runtime.ReadCurrentRuntime(ctx, ref)
	if err != nil || current.Generation != a.Generation {
		return capture, errors.Join(piruntime.ErrScope, err)
	}
	creating := b.State == "creating" && len(b.Conversations) > 0 && b.Conversations[0].State == "creating"
	var intent session.PiCreationIntent
	var journal session.PiCreationJournal
	if creating && forceDelete && os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") == "delete" {
		intent, journal, err = s.piCreatingLifecycleIdentity(ctx, b, current)
	} else {
		intent, err = s.piRetainedRootIdentityLocked(piSessionAdmission{Binding: b, Authority: a})
	}
	if err != nil {
		return capture, err
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return capture, err
	}
	roster, rosterErr := store.LoadPi(slug)
	if rosterErr != nil && !errors.Is(rosterErr, os.ErrNotExist) {
		return capture, rosterErr
	}
	slots := []piruntime.LifecycleSlot{}
	for i, bound := range b.Conversations {
		slot := piruntime.LifecycleSlot{Member: bound.Address, Incarnation: bound.Incarnation, CreationID: intent.ReceiptID, NativeID: bound.NativeID, State: bound.State}
		if i < len(intent.ProviderSelection.Members) {
			selection := intent.ProviderSelection.Members[i]
			if selection.Address != bound.Address || selection.Incarnation != bound.Incarnation {
				return capture, piruntime.ErrScope
			}
			slot.InitialSettings = selection.Settings.Effective()
		}
		if bound.Address == "lead" {
			slot.InitialInputID = intent.InitialInputID
		}
		if fork, ok := intent.ForkMember(bound.Address); ok {
			slot.CreationID = fork.CreationID
		}
		if i > 0 {
			if rosterErr != nil || i-1 >= len(roster.Members) {
				return capture, piruntime.ErrScope
			}
			member := roster.Members[i-1]
			if member.Address != bound.Address || member.Incarnation != bound.Incarnation {
				return capture, piruntime.ErrScope
			}
			slot.CreationID = member.CreationID
			slot.InitialInputID = member.InitialInputID
			if member.Addition != nil {
				slot.InitialSettings = member.Addition.Selection.Settings.Effective()
			}
		}
		slots = append(slots, slot)
	}
	members, err := runtime.CaptureLifecycleMembers(ctx, current, slots)
	if err != nil {
		return capture, err
	}
	capture = session.PiLifecycleCapture{Kind: "pi", Contract: 1, Binding: b, Authority: a, Runtime: current, Members: members}
	if rosterErr == nil {
		capture.RosterRevision = roster.Revision
	}
	if creating {
		if err := s.piCreatingLifecycleMember(ctx, intent, journal, current, members); err != nil {
			return capture, err
		}
	}
	capture.Selectors, err = s.piLifecycleSelectors(b)
	if err != nil {
		return capture, err
	}
	return capture, capture.Validate(s.config.Workspace, slug)
}

// Only the admitted force-delete owner can read an incomplete original root.
// This is retained provenance, never creation completion or execution admission.
func (s *Server) piCreatingLifecycleIdentity(ctx context.Context, b session.BackendBinding, current piruntime.RuntimeRef) (session.PiCreationIntent, session.PiCreationJournal, error) {
	r := creationReceipt{ReceiptID: b.CreationReceiptID, Request: creationRequest{Slug: b.Slug}}
	i, err := readPiCreationIntent(s.creationEvidencePath(r) + ".request")
	var j session.PiCreationJournal
	if err != nil {
		return i, j, err
	}
	if i.ValidateBinding(b) != nil || s.validatePiReadyBinding(i, b) != nil {
		return i, j, agentbackend.ErrBindingConflict
	}
	j, err = readPiCreationJournal(piJournalPath(b.Workspace, b.Slug))
	if err != nil || j.State != "creating" || !j.NativeCreateStarted || !reflect.DeepEqual(j.PiCreationIntent, i) || j.NativeGeneration != current.Generation {
		return i, j, errors.Join(piruntime.ErrHeld, err)
	}
	history, err := session.CompletedRemovalHistory(i.Workspace, i.Slug, s.config.UserStateRoot)
	if err != nil || history != i.DeletionHistorySHA256 {
		return i, j, errors.Join(agentbackend.ErrBindingConflict, err)
	}
	var raw map[string]json.RawMessage
	var cached creationReceipt
	if err := readCreationJSON(s.creationPath(i.Slug), &raw); err != nil {
		return i, j, err
	}
	if validateCreationReceiptShape(raw) != nil || readCreationJSON(s.creationPath(i.Slug), &cached) != nil || !cached.Validated || !piReceiptMatches(cached, i) {
		return i, j, agentbackend.ErrBindingConflict
	}
	reader, ok := s.config.PiCreation.Runtime.(piInitialStartReader)
	if !ok {
		return i, j, piruntime.ErrUnavailable
	}
	start, original, err := reader.ReadInitialStart(ctx, i.SessionRef(), i.RuntimeRequestID())
	if err != nil || original != current || current.RequestID != i.RuntimeRequestID() || start.Session != i.SessionRef() || start.RequestID != i.RuntimeRequestID() || start.BindingRevision != 1 || start.ReplacesRequestID != "" {
		return i, j, errors.Join(agentbackend.ErrBindingConflict, err)
	}
	if i.ProviderSelection.CatalogDigest == s.config.PiCreation.Catalog.Digest {
		err = i.Validate(s.config.PiCreation.Catalog)
	} else {
		err = s.validatePiOriginalCatalog(i, start)
	}
	if err != nil {
		return i, j, err
	}
	// No historical completion inode exists here. The admitted lifecycle owner
	// captures the current directory and validates the original creating manifest.
	var stat unix.Stat_t
	if err := unix.Lstat(filepath.Join(i.Workspace, "work", i.Slug), &stat); err != nil {
		return i, j, err
	}
	if err := s.validatePiTracking(i, piTrackingResult{TrackingDevice: stat.Dev, TrackingInode: stat.Ino}, false); err != nil {
		return i, j, err
	}
	// Completion is written only after binding/tracking readiness. A present
	// completion cannot consistently authorize this genuinely creating root.
	if _, err := s.readPiCreationEvidence(r); !errors.Is(err, os.ErrNotExist) {
		return i, j, errors.Join(agentbackend.ErrBindingConflict, err)
	}
	return i, j, nil
}

func (s *Server) piCreatingLifecycleMember(ctx context.Context, i session.PiCreationIntent, j session.PiCreationJournal, current piruntime.RuntimeRef, members []piruntime.LifecycleMember) error {
	if len(members) == 0 || members[0].Conversation == nil || members[0].Generation != current.Generation || members[0].HistoryRef == "" || members[0].Settings == nil {
		return piruntime.ErrHeld
	}
	root := members[0]
	if j.Native != nil && (j.Native.NativeID != root.Conversation.NativeID || j.Native.HistoryRef != root.HistoryRef) {
		return agentbackend.ErrBindingConflict
	}
	reader, ok := s.config.PiCreation.Runtime.(interface {
		ReadMemberAdmission(context.Context, agentbackend.ConversationRef, string, string) (piruntime.ConversationState, piruntime.SubmissionStatus, error)
	})
	if !ok {
		return piruntime.ErrUnavailable
	}
	state, receipt, err := reader.ReadMemberAdmission(ctx, *root.Conversation, i.ReceiptID, i.InitialInputID)
	settings := i.ProviderSelection.Members[0].Settings.Effective()
	if err != nil || state.Conversation != *root.Conversation || state.Generation != current.Generation || state.HistoryRef != root.HistoryRef || !state.Persisted || state.Settings != *root.Settings || *root.Settings != settings || receipt.Ref.Conversation != state.Conversation || receipt.Ref.OperationID != i.InitialInputID || receipt.Ref.Action != "send" || receipt.Generation != current.Generation || receipt.Phase != "Accepted" || receipt.Refusal != "" || receipt.Witness == nil || receipt.Witness.SessionID != state.Conversation.NativeID || receipt.Witness.EntryID == "" || receipt.Witness.EffectiveDigest != i.GoalSHA256 || receipt.Settings != settings || receipt.Input.ActionContext != "initial" || receipt.Input.Retry || piruntime.MessageDigest(receipt.Input.Message) != i.GoalSHA256 || piruntime.SubmissionDigest(receipt.Ref, receipt.Input, receipt.Settings) != receipt.Ref.Digest {
		return errors.Join(piruntime.ErrHeld, err)
	}
	return nil
}

func (s *Server) piLifecycleOperation(ctx context.Context, action, slug string, capture session.PiLifecycleCapture) (session.PiLifecycleCapture, error) {
	if action == "capture" || action == "preflight" || action == "capture-force-delete" {
		capture, err := s.capturePiLifecycleFor(ctx, slug, action == "capture-force-delete")
		if err != nil {
			return capture, err
		}
		if action == "capture-force-delete" {
			if os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "delete" {
				return capture, piruntime.ErrScope
			}
			runtime := s.config.PiCreation.Runtime.(piLifecycleRuntime)
			if err := runtime.CheckLifecycle(ctx, capture.Runtime, capture.ExecutionConversations()); err != nil {
				capture.Callbacks, err = runtime.TerminalCallbacks(ctx, capture.Runtime, capture.Members, true)
				if err != nil || len(capture.Callbacks) == 0 {
					return capture, errors.Join(piruntime.ErrHeld, err)
				}
			}
		}
		if action == "preflight" {
			runtime := s.config.PiCreation.Runtime.(piLifecycleRuntime)
			return capture, runtime.CheckLifecycle(ctx, capture.Runtime, capture.ExecutionConversations())
		}
		return capture, nil
	}
	if err := capture.Validate(s.config.Workspace, slug); err != nil {
		return capture, err
	}
	if action == "finalize-authority" || (action == "check" && ctx.Value(piDeleteRecoveryKey{}) != nil) {
		operation := os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION")
		if (operation != "archive" && operation != "delete") || capture.Terminal == nil {
			return capture, piruntime.ErrScope
		}
		if err := s.requireCurrentHostProfile(); err != nil {
			return capture, err
		}
		if recheck, ok := ctx.Value(piDeleteRecoveryKey{}).(func() error); ok {
			if err := recheck(); err != nil {
				return capture, err
			}
			verifier, ok := s.config.PiCreation.Runtime.(interface {
				VerifyLifecycleTerminal(context.Context, piruntime.RuntimeRef, piruntime.HostObservation) error
			})
			if !ok {
				return capture, piruntime.ErrUnavailable
			}
			if err := verifier.VerifyLifecycleTerminal(ctx, capture.Runtime, *capture.Terminal); err != nil {
				return capture, err
			}
		}
		if action == "check" {
			return capture, nil
		}
		return capture, session.RetirePiLifecycleAuthority(s.config.AuthorityDir, capture)
	}
	runtime, ok := s.config.PiCreation.Runtime.(piLifecycleRuntime)
	if !ok {
		return capture, piruntime.ErrUnavailable
	}
	b, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(slug)
	if err != nil || !reflect.DeepEqual(b, capture.Binding) {
		return capture, errors.Join(piruntime.ErrScope, err)
	}
	if err := s.requireCurrentHostProfile(); err != nil {
		return capture, err
	}
	if b.State == "creating" {
		if os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "delete" {
			return capture, piruntime.ErrScope
		}
		selectors, err := s.piLifecycleSelectors(b)
		if err != nil || !reflect.DeepEqual(selectors, capture.Selectors) {
			return capture, errors.Join(piruntime.ErrConflict, err)
		}
	}
	switch action {
	case "prepare-restore":
		if os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "revive" || capture.Terminal == nil || capture.Successor != nil {
			return capture, piruntime.ErrScope
		}
		current, err := runtime.ReadCurrentRuntime(ctx, capture.Ref())
		if err != nil || current != capture.Runtime {
			return capture, errors.Join(piruntime.ErrConflict, err)
		}
		id, err := newPiInitialInputID()
		if err != nil {
			return capture, err
		}
		start := piruntime.StartRequest{Session: capture.Ref(), BindingRevision: capture.Binding.Revision, RequestID: id, ReplacesRequestID: capture.Runtime.RequestID, AppStorePath: s.config.PackageRoot, ProfilePath: s.config.HostProfile, ProfileGeneration: piProfileToken(s.hostProfile)}
		capture.Successor = &start
		return capture, capture.Validate(s.config.Workspace, slug)
	case "restore":
		if os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "revive" || capture.Terminal == nil || capture.Successor == nil {
			return capture, piruntime.ErrScope
		}
		start := *capture.Successor
		if start.AppStorePath != s.config.PackageRoot || start.ProfilePath != s.config.HostProfile || start.ProfileGeneration != piProfileToken(s.hostProfile) {
			return capture, piruntime.ErrConflict
		}
		authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, slug, s.config.Workspace)
		if errors.Is(err, agentbackend.ErrNotBound) {
			if err := session.PublishPiRuntimeAuthority(s.config.AuthorityDir, b, capture.Authority); err != nil {
				return capture, err
			}
		} else if err != nil {
			return capture, err
		} else if authority != capture.Authority {
			current, err := runtime.ReadCurrentRuntime(ctx, capture.Ref())
			if err != nil || current.RequestID != start.RequestID || authority.Generation != current.Generation || authority.ValidateBinding(b) != nil {
				return capture, piruntime.ErrConflict
			}
		}
		refs := []agentbackend.ConversationRef{}
		for _, slot := range b.Conversations {
			if slot.State == "ready" {
				ref, err := b.Resolve(slot.Address)
				if err != nil {
					return capture, err
				}
				refs = append(refs, ref)
			}
		}
		store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if err != nil {
			return capture, err
		}
		roster, rosterErr := store.LoadPi(slug)
		if rosterErr != nil && !errors.Is(rosterErr, os.ErrNotExist) {
			return capture, rosterErr
		}
		var projection *session.PiRuntimeUpgradeProjection
		if rosterErr == nil {
			if roster.Revision != capture.RosterRevision {
				return capture, piruntime.ErrConflict
			}
			rows := []session.PiRuntimeUpgradeMember{}
			for _, m := range roster.Members {
				rows = append(rows, session.PiRuntimeUpgradeMember{Address: m.Address, Incarnation: m.Incarnation, State: m.State, InitialInputID: m.InitialInputID, Fork: m.Fork != nil})
			}
			p, err := session.ProjectPiRuntimeUpgrade(b, roster.Schema, roster.Revision, rows)
			if err != nil {
				return capture, err
			}
			projection = &p
		} else if capture.RosterRevision != 0 {
			return capture, piruntime.ErrConflict
		}
		_, err = runtime.Upgrade(ctx, piruntime.UpgradeRequest{Start: start, ExpectedGeneration: capture.Runtime.Generation, Members: refs}, piruntime.UpgradeHooks{EnsureAttachment: s.piWorkflows.ensureAttachmentWithConnector, Publish: func(ctx context.Context, proof piruntime.UpgradeProof) error {
			if err := s.requireCurrentHostProfile(); err != nil {
				return err
			}
			current, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(slug)
			if err != nil || !reflect.DeepEqual(current, b) {
				return piruntime.ErrConflict
			}
			if rosterErr == nil {
				fresh, err := store.LoadPi(slug)
				if err != nil || !reflect.DeepEqual(fresh, roster) {
					return piruntime.ErrConflict
				}
			}
			if projection != nil {
				_, err = session.PublishPiRuntimeUpgradeAuthority(s.config.AuthorityDir, b, capture.Authority, proof, *projection)
			} else {
				_, err = session.PublishPiRuntimeUpgradeAuthority(s.config.AuthorityDir, b, capture.Authority, proof)
			}
			return err
		}})
		return capture, err
	case "check":
		slots := make([]piruntime.LifecycleSlot, 0, len(capture.Members))
		for _, member := range capture.Members {
			slots = append(slots, member.Slot)
		}
		actual, err := runtime.CaptureLifecycleMembers(ctx, capture.Runtime, slots)
		if err != nil || !reflect.DeepEqual(actual, capture.Members) {
			return capture, errors.Join(piruntime.ErrConflict, err)
		}
		return capture, nil
	case "terminal-delete":
		if os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "delete" || len(capture.Callbacks) == 0 {
			return capture, piruntime.ErrScope
		}
		status, err := runtime.RetireTerminalCallbacks(ctx, capture.Runtime, capture.Members, capture.Callbacks)
		if err != nil || status.Phase != "stopped" {
			return capture, errors.Join(piruntime.ErrHeld, err)
		}
		capture.Terminal = &status.Observation
		return capture, capture.Validate(s.config.Workspace, slug)
	case "retire", "archive-retire", "delete-retire":
		if action == "archive-retire" || action == "delete-retire" {
			if (action == "archive-retire" && os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "archive") || (action == "delete-retire" && os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "delete") {
				return capture, piruntime.ErrScope
			}
			if err := runtime.InterruptLifecycle(ctx, capture.Runtime, capture.ExecutionConversations(), s.piWorkflows.ensureAttachmentWithConnector); err != nil {
				return capture, err
			}
		}
		status, err := runtime.Retire(ctx, capture.Runtime, capture.ExecutionConversations(), s.piWorkflows.ensureAttachmentWithConnector)
		if err != nil || status.Phase != "stopped" {
			return capture, errors.Join(piruntime.ErrHeld, err)
		}
		capture.Terminal = &status.Observation
		return capture, capture.Validate(s.config.Workspace, slug)
	default:
		return capture, piruntime.ErrScope
	}
}

type piDeleteRecoveryKey struct{}

// This invocation is handed only by the public source-built recovery entry.
// The selected package still owns configuration; no server package is replaced.
func (s *Server) piDeleteRecoveryInvocation(action, slug, executable string, data []byte, capture session.PiLifecycleCapture) (func() error, error) {
	var owner struct {
		Candidate       string            `json:"candidate"`
		Predecessor     string            `json:"predecessor"`
		Workspace       string            `json:"workspace"`
		Slug            string            `json:"slug"`
		OperationID     string            `json:"operation_id"`
		Force           bool              `json:"force"`
		Recovery        string            `json:"recovery"`
		CaptureSHA256   string            `json:"capture_sha256"`
		TransitionFD    string            `json:"transition_fd"`
		ProfileIdentity []json.RawMessage `json:"profile_identity"`
	}
	encoded := os.Getenv("DEV_SESSION_PI_DELETE_RECOVERY")
	if session.DecodePiCreationJSON([]byte(encoded), &owner, 16<<10) != nil ||
		os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") != "delete" || (action != "check" && action != "finalize-authority") ||
		owner.Workspace != s.config.Workspace || owner.Slug != slug || owner.Predecessor != s.config.PackageRoot ||
		owner.Candidate == owner.Predecessor || executable != filepath.Join(owner.Candidate, "bin/workspace-portal") ||
		!explicitPiPath(owner.Candidate) || !messageDigestPattern.MatchString(owner.OperationID) || capture.Terminal == nil || len(owner.ProfileIdentity) != 7 {
		return nil, piruntime.ErrScope
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != owner.CaptureSHA256 {
		return nil, piruntime.ErrConflict
	}
	var expected hostProfileIdentity
	for i, target := range []any{&expected.device, &expected.inode, &expected.ctimeSec, &expected.ctimeNSec, &expected.mtimeSec, &expected.mtimeNSec, &expected.target} {
		if json.Unmarshal(owner.ProfileIdentity[i], target) != nil {
			return nil, piruntime.ErrScope
		}
	}
	fd, err := strconv.Atoi(owner.TransitionFD)
	if err != nil || fd < 3 {
		return nil, piruntime.ErrScope
	}
	recheck := func() error {
		if profile, err := readHostProfileIdentity(s.config.HostProfile); err != nil || profile != expected {
			return piruntime.ErrConflict
		}
		var path, held unix.Stat_t
		if unix.Lstat(s.config.TransitionLock, &path) != nil || unix.Fstat(fd, &held) != nil ||
			path.Mode&unix.S_IFMT != unix.S_IFREG || held.Mode&unix.S_IFMT != unix.S_IFREG ||
			path.Dev != held.Dev || path.Ino != held.Ino || held.Uid != uint32(os.Geteuid()) || held.Mode&0777 != 0600 {
			return piruntime.ErrScope
		}
		probe, err := unix.Open(s.config.TransitionLock, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		err = unix.Flock(probe, unix.LOCK_SH|unix.LOCK_NB)
		unix.Close(probe)
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			return piruntime.ErrHeld
		}
		selectedContract, err := os.ReadFile(filepath.Join(owner.Predecessor, "share/workspace-portal/runtime-contract.json"))
		if err != nil {
			return err
		}
		candidateContract, err := os.ReadFile(filepath.Join(owner.Candidate, "share/workspace-portal/runtime-contract.json"))
		if err != nil || !reflect.DeepEqual(selectedContract, candidateContract) {
			return piruntime.ErrConflict
		}
		journalPath := filepath.Join(s.config.Workspace, "worktrees/.locks", slug+".removal.json")
		file, err := os.OpenFile(journalPath, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			return piruntime.ErrScope
		}
		encoded, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
		var fields map[string]json.RawMessage
		if err != nil || session.DecodePiCreationJSON(encoded, &fields, 64<<10) != nil {
			return piruntime.ErrScope
		}
		var journal struct {
			Schema                           int
			Workspace, Slug, Phase, Recovery string
			OperationID                      string `json:"operation_id"`
			Force                            bool
			Sealed                           bool `json:"worktrees_sealed"`
			Worktrees                        []struct {
				Path    string
				Removed bool
			}
		}
		if json.Unmarshal(encoded, &journal) != nil || journal.Schema != 2 || journal.Workspace != owner.Workspace ||
			journal.Slug != slug || journal.OperationID != owner.OperationID || journal.Force != owner.Force ||
			journal.Recovery != owner.Recovery || !journal.Sealed || fields["worktrees"] == nil ||
			(journal.Phase != "worktrees_removed" && journal.Phase != "runtime_retired" && journal.Phase != "tracking_preserved" && journal.Phase != "tracking_committed") {
			return piruntime.ErrScope
		}
		root := userstate.WorkspaceDirectory(s.config.UserStateRoot, "removed", owner.Workspace)
		if filepath.Dir(owner.Recovery) != root {
			return piruntime.ErrScope
		}
		for _, worktree := range journal.Worktrees {
			if _, err := os.Lstat(worktree.Path); !worktree.Removed || !errors.Is(err, os.ErrNotExist) {
				return piruntime.ErrHeld
			}
		}
		original, err := session.DecodePiLifecycleCapture(fields["pi"], owner.Workspace, slug)
		if err != nil || !reflect.DeepEqual(original, capture) {
			return piruntime.ErrConflict
		}
		return nil
	}
	if err := recheck(); err != nil {
		return nil, err
	}
	return recheck, nil
}

// RunPiLifecycle is only a synchronous child of the admitted Ruby executor.
// Its stdin/output carry the typed original journal capture, never a new owner.
func RunPiLifecycle(ctx context.Context, args []string, input io.Reader, output io.Writer, fixture Config) error {
	config := Config{}
	flags := flag.NewFlagSet("pi-lifecycle", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	for _, value := range []struct {
		name   string
		target *string
	}{
		{"workspace", &config.Workspace}, {"workspace-name", &config.WorkspaceName}, {"user-state-root", &config.UserStateRoot}, {"authority-dir", &config.AuthorityDir}, {"host-profile", &config.HostProfile}, {"transition-lock", &config.TransitionLock}, {"registration-marker", &config.RegistrationMarker}, {"codex-socket", &config.CodexSocket}, {"pi-auth-dir", &config.PiAuthDirectory}, {"pi-host-state-root", &config.PiHostStateRoot},
	} {
		flags.StringVar(value.target, value.name, "", "admitted caller configuration")
	}
	slug := flags.String("slug", "", "session")
	action := flags.String("action", "", "bounded lifecycle step")
	if len(args) != 24 {
		return piruntime.ErrScope
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i += 2 {
		if seen[args[i]] || args[i+1] == "" {
			return piruntime.ErrScope
		}
		seen[args[i]] = true
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !session.ValidSlug(*slug) {
		return piruntime.ErrScope
	}
	for _, path := range []string{config.Workspace, config.UserStateRoot, config.AuthorityDir, config.HostProfile, config.TransitionLock, config.RegistrationMarker, config.CodexSocket, config.PiAuthDirectory, config.PiHostStateRoot} {
		if !explicitPiPath(path) {
			return piruntime.ErrScope
		}
	}
	if *action == "preflight" && os.Getenv("DEV_SESSION_LIFECYCLE_OPERATION") == "" {
		lock, err := session.LockRuntimeSharedContext(ctx, config.AuthorityDir, *slug)
		if err != nil {
			return err
		}
		defer lock.Close()
	} else {
		parent, err := piLifecycleParent(config, *slug)
		if err != nil {
			return err
		}
		defer parent.Close()
	}
	s, err := newPiLifecycleServer(config, fixture.PiCreation)
	if err != nil {
		return err
	}
	operationCtx, finishOperations := context.WithCancel(ctx)
	s.operationContext = operationCtx
	defer func() { finishOperations(); s.operationWG.Wait() }()
	var capture session.PiLifecycleCapture
	var captureData []byte
	if *action != "capture" && *action != "preflight" && *action != "capture-force-delete" {
		data, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
		if err != nil {
			return err
		}
		capture, err = session.DecodePiLifecycleCapture(data, config.Workspace, *slug)
		if err != nil {
			return err
		}
		captureData = data
	}
	if fixture.PiCreation == nil || os.Getenv("DEV_SESSION_PI_DELETE_RECOVERY") != "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			return err
		}
		if os.Getenv("DEV_SESSION_PI_DELETE_RECOVERY") != "" {
			recheck, err := s.piDeleteRecoveryInvocation(*action, *slug, executable, captureData, capture)
			if err != nil {
				return err
			}
			operationCtx = context.WithValue(operationCtx, piDeleteRecoveryKey{}, recheck)
		} else if filepath.Dir(filepath.Dir(executable)) != s.config.PackageRoot {
			return piruntime.ErrConflict
		}
	}
	result, err := s.piLifecycleOperation(operationCtx, *action, *slug, capture)
	if err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(data)+1 > 64<<10 {
		return piruntime.ErrScope
	}
	_, err = output.Write(append(data, '\n'))
	return err
}

func (s *Server) piLifecycleSelectors(b session.BackendBinding) ([]session.PiLifecycleSelector, error) {
	evidence := s.creationEvidencePath(creationReceipt{ReceiptID: b.CreationReceiptID, Request: creationRequest{Slug: b.Slug}})
	paths := []struct{ name, path string }{
		{"binding", filepath.Join(s.config.UserStateRoot, "session-backends", fmt.Sprintf("%x", sha256.Sum256([]byte(b.Workspace))), b.Slug+".json")},
		{"creation", piJournalPath(b.Workspace, b.Slug)},
		{"receipt", s.creationPath(b.Slug)},
		{"completion", evidence}, {"companion", evidence + ".request"},
		{"team", filepath.Join(userstate.WorkspaceDirectory(s.config.UserStateRoot, "codex-teams", b.Workspace), b.Slug+".json")},
		{"authority", filepath.Join(s.config.AuthorityDir, b.Slug+".json")},
	}
	result := []session.PiLifecycleSelector{}
	for _, entry := range paths {
		info, err := os.Lstat(entry.path)
		if errors.Is(err, os.ErrNotExist) && (entry.name == "receipt" || entry.name == "team" || entry.name == "completion" && b.State == "creating" && b.Conversations[0].State == "creating") {
			continue
		}
		if err != nil {
			return nil, err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxCreationReceiptBytes {
			return nil, piruntime.ErrScope
		}
		data, err := os.ReadFile(entry.path)
		if err != nil {
			return nil, err
		}
		result = append(result, session.PiLifecycleSelector{Name: entry.name, Path: entry.path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	return result, nil
}
