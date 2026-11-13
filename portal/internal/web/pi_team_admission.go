package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

// piPackageInvocation borrows the actual outer transition scope. Its writer
// closure only rechecks that scope; Service.Run already owns session locks.
// Each HTTP/background invocation gets a distinct lifetime, never a server flag.
type piPackageInvocation struct {
	mu     sync.Mutex
	server *Server
	file   *os.File
	active bool
}

func (s *Server) piPackageInvocation(file *os.File) *piPackageInvocation {
	return &piPackageInvocation{server: s, file: file, active: true}
}

func (p *piPackageInvocation) expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = false
}

func (p *piPackageInvocation) recheck() error {
	if p == nil || !p.active || p.file == nil || p.server.config.TransitionLock == "" {
		return piruntime.ErrUnavailable
	}
	s := p.server
	var path, descriptor unix.Stat_t
	if unix.Lstat(s.config.TransitionLock, &path) != nil || unix.Fstat(int(p.file.Fd()), &descriptor) != nil ||
		path.Mode&unix.S_IFMT != unix.S_IFREG || descriptor.Mode&unix.S_IFMT != unix.S_IFREG ||
		path.Dev != descriptor.Dev || path.Ino != descriptor.Ino || descriptor.Uid != uint32(os.Geteuid()) || descriptor.Mode&0777 != 0600 {
		return piruntime.ErrUnavailable
	}
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	root := s.config.PackageRoot
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return piruntime.ErrUnavailable
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return piruntime.ErrUnavailable
	}
	selected, err := filepath.EvalSymlinks(s.config.HostProfile)
	if err != nil || selected != root {
		return piruntime.ErrHeld
	}
	file, err := os.Open(filepath.Join(root, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return err
	}
	var contract map[string]json.RawMessage
	if err := session.DecodePiCreationJSON(data, &contract, 1<<20); err != nil {
		return err
	}
	_, err = session.DecodePiTeamRecordsContract(contract["piTeamRecords"])
	return err
}

func (p *piPackageInvocation) withWrite(ctx context.Context, run func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if run == nil {
		return piruntime.ErrUnavailable
	}
	if err := p.recheck(); err != nil {
		return err
	}
	return run()
}

type piSessionAdmission struct {
	Binding   session.BackendBinding
	Authority session.PiRuntimeAuthority
	intent    session.PiCreationIntent
}

type piConversationAdmission struct {
	piSessionAdmission
	Handle          agentbackend.ExecutionHandle
	BindingRevision uint64
	SavedAccess     string
}

// No domain lock may be held on entry. Native effects need their owning
// verifier as well; these gates grant only current read/record ownership.
func (s *Server) withPiSessionRead(ctx context.Context, ref agentbackend.SessionRef, run func(piSessionAdmission) error) error {
	return s.withPiSessionAdmission(ctx, ref, false, run)
}

func (s *Server) withPiSessionRecordWrite(ctx context.Context, ref agentbackend.SessionRef, generation string, run func(piSessionAdmission) error) error {
	if !messageDigestPattern.MatchString(generation) || run == nil {
		return piruntime.ErrScope
	}
	return s.withPiSessionAdmission(ctx, ref, false, func(current piSessionAdmission) error {
		if generation != current.Authority.Generation {
			return agentbackend.ErrStaleExecution
		}
		return run(current)
	})
}

func (s *Server) withPiSessionAdmission(ctx context.Context, ref agentbackend.SessionRef, exclusive bool, run func(piSessionAdmission) error) error {
	if ref.Workspace != s.config.Workspace || !session.ValidSlug(ref.Slug) || !messageDigestPattern.MatchString(ref.Incarnation) || run == nil {
		return piruntime.ErrScope
	}
	file, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer release()
	invocation := s.piPackageInvocation(file)
	defer invocation.expire()
	if err := invocation.recheck(); err != nil {
		return err
	}
	if err := s.requireProviderAdmission(ref.Slug); err != nil {
		return err
	}
	var lock *session.RuntimeLock
	if exclusive {
		lock, err = session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, ref.Slug)
	} else {
		lock, err = session.LockRuntimeSharedContext(ctx, s.config.AuthorityDir, ref.Slug)
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := invocation.recheck(); err != nil {
		return err
	}
	current, err := s.piSessionAdmissionLocked(ref)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return run(current)
}

func (s *Server) piSessionAdmissionLocked(ref agentbackend.SessionRef) (piSessionAdmission, error) {
	var current piSessionAdmission
	if err := s.requireProviderAdmission(ref.Slug); err != nil {
		return current, err
	}
	if owner, err := session.PendingLifecycle(ref.Workspace, ref.Slug); err != nil || owner != "" {
		return current, errors.Join(piruntime.ErrHeld, err)
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: ref.Workspace}).Load(ref.Slug)
	if err != nil {
		return current, err
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, ref.Slug, ref.Workspace)
	if err != nil {
		return current, err
	}
	if binding.SessionIncarnation != ref.Incarnation || binding.State != "ready" || authority.State != "ready" || authority.ValidateBinding(binding) != nil {
		return current, agentbackend.ErrBindingConflict
	}
	current.Binding, current.Authority = binding, authority
	current.intent, err = s.piRetainedRootIdentityLocked(current)
	if err != nil {
		return piSessionAdmission{}, err
	}
	return current, nil
}

// This is the retained host identity join only. The original receipt's contents
// and native witness remain the owning runtime reader/verifier's responsibility.
// In particular, observational admission never calls the native creation helper.
func (s *Server) piRetainedRootIdentityLocked(current piSessionAdmission) (session.PiCreationIntent, error) {
	b := current.Binding
	r := creationReceipt{ReceiptID: b.CreationReceiptID, Request: creationRequest{Slug: b.Slug}}
	i, err := readPiCreationIntent(s.creationEvidencePath(r) + ".request")
	if err != nil {
		return i, err
	}
	if s.config.PiCreation == nil || i.Validate(s.config.PiCreation.Catalog) != nil || i.ValidateBinding(b) != nil || i.ReceiptID != b.CreationReceiptID || i.SessionRef() != (agentbackend.SessionRef{Workspace: b.Workspace, Slug: b.Slug, Incarnation: b.SessionIncarnation}) {
		return i, agentbackend.ErrBindingConflict
	}
	j, err := readPiCreationJournal(piJournalPath(b.Workspace, b.Slug))
	if err != nil || j.State != "ready" || j.Native == nil || !reflect.DeepEqual(j.PiCreationIntent, i) {
		return i, errors.Join(piruntime.ErrHeld, err)
	}
	lead, err := b.Resolve("lead")
	if err != nil || lead.NativeID != j.Native.NativeID {
		return i, agentbackend.ErrBindingConflict
	}
	e, err := s.readPiCreationEvidence(r)
	if err != nil {
		return i, err
	}
	if e.Schema != 4 || e.Workspace != i.Workspace || e.Slug != i.Slug || e.Backend != agentbackend.Pi || e.BackendContract != 1 || e.ReceiptID != i.ReceiptID || e.SessionIncarnation != i.SessionIncarnation || e.BindingRevision == 0 || e.BindingRevision > b.Revision || !reflect.DeepEqual(e.Conversations, i.Conversations) || e.NativeID != lead.NativeID || e.InitialInputID != i.InitialInputID || !messageDigestPattern.MatchString(e.InitialSubmissionDigest) || e.GoalSHA256 != i.GoalSHA256 || e.Model != i.Model || e.Effort != i.Effort || e.Provider != i.Provider || !reflect.DeepEqual(e.ProviderSelection, i.ProviderSelection) || e.CatalogDigest != i.DirectTeam.CatalogDigest || e.TeamDigest != i.DirectTeam.TeamDigest || e.Team != i.DirectTeam.ID {
		return i, agentbackend.ErrBindingConflict
	}
	history, err := session.CompletedRemovalHistory(i.Workspace, i.Slug, s.config.UserStateRoot)
	if err != nil || history != i.DeletionHistorySHA256 {
		return i, errors.Join(agentbackend.ErrBindingConflict, err)
	}
	var cached creationReceipt
	var raw map[string]json.RawMessage
	if err := readCreationJSON(s.creationPath(i.Slug), &raw); err == nil {
		if validateCreationReceiptShape(raw) != nil || readCreationJSON(s.creationPath(i.Slug), &cached) != nil || !piReceiptMatches(cached, i) {
			return i, agentbackend.ErrBindingConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return i, err
	}
	if err := s.validatePiTracking(i, piTrackingResult{TrackingDevice: e.TrackingDevice, TrackingInode: e.TrackingInode}, true); err != nil {
		return i, err
	}
	return i, nil
}

func (s *Server) withPiConversationMutation(ctx context.Context, ref agentbackend.ConversationRef, run func(piConversationAdmission) error) error {
	if ref.Backend != agentbackend.Pi || run == nil {
		return piruntime.ErrScope
	}
	return s.withPiSessionAdmission(ctx, ref.Session, true, func(current piSessionAdmission) error {
		resolved, err := current.Binding.Resolve(ref.Member)
		if err != nil || resolved != ref {
			return errors.Join(agentbackend.ErrBindingConflict, err)
		}
		access, err := s.piSavedAccessLocked(current, ref)
		if err != nil {
			return err
		}
		return run(piConversationAdmission{piSessionAdmission: current, Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: current.Authority.Generation}, BindingRevision: current.Binding.Revision, SavedAccess: access})
	})
}

func (s *Server) piSavedAccessLocked(current piSessionAdmission, ref agentbackend.ConversationRef) (string, error) {
	if ref.Member == "lead" {
		// The original wire lacks lead Access. An exact historical catalog/team
		// digest proves its explicit value; a newer default cannot fill that gap.
		if s.installedTeams == nil || s.installedTeams.Catalog == nil || s.installedTeams.Catalog.CatalogDigest != current.intent.DirectTeam.CatalogDigest {
			return "", piruntime.ErrUnavailable
		}
		team, ok := s.installedTeams.Catalog.Team(current.intent.DirectTeam.ID)
		role, exists := team.Roles["team_lead"]
		if !ok || !exists || team.TeamDigest != current.intent.DirectTeam.TeamDigest || (role.Access != "read_only" && role.Access != "workspace_write") {
			return "", piruntime.ErrUnavailable
		}
		return role.Access, nil
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return "", err
	}
	roster, err := store.LoadPi(ref.Session.Slug)
	if err != nil {
		return "", err
	}
	if roster.Scope().Ref() != ref.Session || roster.LeadIncarnation != current.Binding.Conversations[0].Incarnation {
		return "", agentbackend.ErrBindingConflict
	}
	for _, member := range roster.Members {
		if member.Address == ref.Member && member.Incarnation == ref.Incarnation && member.State == "ready" && (member.Access == "read_only" || member.Access == "workspace_write") {
			return member.Access, nil
		}
	}
	return "", piruntime.ErrHeld
}

func (s *Server) WithPiTeamWrite(ctx context.Context, ref agentbackend.SessionRef, generation string, run func() error) error {
	if run == nil {
		return piruntime.ErrUnavailable
	}
	return s.withPiSessionRecordWrite(ctx, ref, generation, func(piSessionAdmission) error { return run() })
}

func (s *Server) WithPiInputRead(ctx context.Context, input monitoroperation.PiInputRecord, read func() error) error {
	if input.Validate() != nil || read == nil {
		return piruntime.ErrScope
	}
	return s.withPiSessionRead(ctx, input.Conversation.Session.Ref(), func(current piSessionAdmission) error {
		ref, err := current.Binding.Resolve(input.Conversation.Member)
		if err != nil || ref != input.Conversation.Ref() {
			return errors.Join(agentbackend.ErrBindingConflict, err)
		}
		return read()
	})
}

func (s *Server) WithPiUtilityRead(ctx context.Context, utility monitoroperation.PiUtilityRecord, inputID, originalGeneration string, read func() error) error {
	if utility.Validate(true) != nil || !piPublicationOperationID.MatchString(inputID) || !messageDigestPattern.MatchString(originalGeneration) || read == nil {
		return piruntime.ErrScope
	}
	// The operation's owning reader compares the original input/generation under
	// this gate. They are deliberately distinct from the current live authority.
	return s.withPiSessionRead(ctx, utility.Session.Ref(), func(piSessionAdmission) error { return read() })
}

func (s *Server) WithUtilityRead(ctx context.Context, runtime piruntime.RuntimeRef, original piruntime.UtilitySubmissionRef, read func() error) error {
	u := monitoroperation.PiUtilityRecord{Session: monitoroperation.PiSessionRecord{Workspace: original.Utility.Session.Workspace, Slug: original.Utility.Session.Slug, Incarnation: original.Utility.Session.Incarnation}, OperationID: original.Utility.OperationID, Incarnation: original.Utility.Incarnation, NativeID: original.Utility.NativeID}
	if u.Validate(true) != nil || runtime.Session != original.Utility.Session || !piPublicationOperationID.MatchString(original.OperationID) || original.Action != "send" || !messageDigestPattern.MatchString(original.Digest) || read == nil {
		return piruntime.ErrScope
	}
	return s.withPiSessionRead(ctx, runtime.Session, func(current piSessionAdmission) error {
		if runtime.Generation != current.Authority.Generation {
			return agentbackend.ErrStaleExecution
		}
		return read()
	})
}

var _ monitoroperation.PiTeamWriteAdmission = (*Server)(nil)
var _ communication.PiInputReadAdmission = (*Server)(nil)
var _ monitoroperation.PiUtilityReadAdmission = (*Server)(nil)
var _ piruntime.UtilityReadAuthority = (*Server)(nil)

type piRosterReadyProof struct {
	Handle      agentbackend.ExecutionHandle
	CreationID  string
	OperationID string
	Expected    PiMemberAdmission
	Proof       piruntime.CreationProof
}

// One SavePi invocation uses this object as both its admission and ready
// verifier. Native proofs are captured beforehand. The verifier cannot outlive
// the actual record-write callback or reacquire transition/runtime/roster locks.
type piRosterWriteAdmission struct {
	server   *Server
	snapshot piSessionAdmission
	proofs   []piRosterReadyProof
	used     atomic.Bool
	active   atomic.Bool
}

func newPiRosterWriteAdmission(s *Server, snapshot piSessionAdmission, proofs []piRosterReadyProof) (*piRosterWriteAdmission, error) {
	if s == nil || snapshot.Authority.ValidateBinding(snapshot.Binding) != nil || len(proofs) == 0 {
		return nil, piruntime.ErrUnavailable
	}
	// Detach every pointer/slice, including Bootstrap/Witness, before the
	// capturing caller can change its copy while SavePi waits for admission.
	data, err := json.Marshal(proofs)
	if err != nil {
		return nil, err
	}
	var detached []piRosterReadyProof
	if err := json.Unmarshal(data, &detached); err != nil {
		return nil, err
	}
	snapshot.Binding.Conversations = append([]session.BackendConversation{}, snapshot.Binding.Conversations...)
	for _, proof := range detached {
		if snapshot.Authority.ValidateHandle(snapshot.Binding, proof.Handle) != nil || validatePiMemberProof(proof.Handle, proof.CreationID, proof.OperationID, proof.Expected, proof.Proof) != nil {
			return nil, agentbackend.ErrConversationNotReady
		}
	}
	return &piRosterWriteAdmission{server: s, snapshot: snapshot, proofs: detached}, nil
}

func (a *piRosterWriteAdmission) WithPiTeamWrite(ctx context.Context, ref agentbackend.SessionRef, generation string, run func() error) error {
	if a == nil || run == nil || !a.used.CompareAndSwap(false, true) {
		return piruntime.ErrUnavailable
	}
	return a.server.withPiSessionRecordWrite(ctx, ref, generation, func(current piSessionAdmission) error {
		if !reflect.DeepEqual(current.Binding, a.snapshot.Binding) || current.Authority != a.snapshot.Authority {
			return agentbackend.ErrStaleExecution
		}
		a.active.Store(true)
		defer a.active.Store(false)
		return run()
	})
}

func (a *piRosterWriteAdmission) VerifyPiRosterReady(ctx context.Context, roster teamruntime.PiRoster) error {
	if a == nil || !a.active.Load() || ctx.Err() != nil || roster.Validate() != nil || roster.Scope().Ref() != a.snapshot.intent.SessionRef() {
		return piruntime.ErrUnavailable
	}
	current, err := a.server.piSessionAdmissionLocked(roster.Scope().Ref())
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current.Binding, a.snapshot.Binding) || current.Authority != a.snapshot.Authority || roster.LeadIncarnation != current.Binding.Conversations[0].Incarnation {
		return agentbackend.ErrStaleExecution
	}
	for _, member := range roster.Members {
		if member.State != "ready" {
			continue
		}
		ref, err := current.Binding.Resolve(member.Address)
		if err != nil || ref.Incarnation != member.Incarnation {
			return agentbackend.ErrBindingConflict
		}
		found := false
		for _, proof := range a.proofs {
			if proof.Handle.Conversation != ref {
				continue
			}
			if found || proof.CreationID != member.CreationID || proof.OperationID != member.InitialInputID || current.Authority.ValidateHandle(current.Binding, proof.Handle) != nil || validatePiMemberProof(proof.Handle, member.CreationID, member.InitialInputID, proof.Expected, proof.Proof) != nil {
				return agentbackend.ErrConversationNotReady
			}
			found = true
		}
		if !found {
			return agentbackend.ErrConversationNotReady
		}
	}
	return nil
}

var _ monitoroperation.PiTeamWriteAdmission = (*piRosterWriteAdmission)(nil)
var _ teamruntime.PiRosterReadyVerifier = (*piRosterWriteAdmission)(nil)
