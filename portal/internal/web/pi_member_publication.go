package web

import (
	"context"
	"errors"
	"math"
	"regexp"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

var piPublicationOperationID = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type MemberPublication struct {
	Binding   session.BackendBinding
	Authority session.PiRuntimeAuthority
	Complete  bool
}

// PiMemberAdmission is the immutable original assignment read from the trusted
// retained roster/mapping. Publication callers cannot supply or replace it.
type PiMemberAdmission struct {
	HistoryRef string
	Submission piruntime.SubmissionRef
	Input      piruntime.SendInput
	Settings   piruntime.EffectiveSettings
}

type PiMemberPublicationAdapter interface {
	piruntime.CreationVerifier
	// LockRoster acquires team operation ownership and rechecks retained settings,
	// access and incarnation under the short roster lock. It releases that short
	// store lock before returning. The caller owns transition SH and runtime EX.
	// Neither admission nor verification creates history or dispatches input.
	LockRoster(context.Context, agentbackend.ExecutionHandle) (PiMemberAdmission, func(), error)
}

func validatePiMemberProof(handle agentbackend.ExecutionHandle, creationID, operationID string, expected PiMemberAdmission, proof piruntime.CreationProof) error {
	if !messageDigestPattern.MatchString(expected.HistoryRef) || expected.Submission.Conversation != handle.Conversation ||
		expected.Submission.OperationID != operationID || expected.Submission.Action != "send" ||
		expected.Input.Message == "" || expected.Input.ActionContext == "" || expected.Input.Retry ||
		expected.Settings.Provider == "" || expected.Settings.AccountRef == "" || expected.Settings.Model == "" ||
		expected.Settings.Thinking == "" || expected.Settings.Revision == 0 || !messageDigestPattern.MatchString(expected.Settings.PolicyRevision) ||
		expected.Submission.Digest != piruntime.SubmissionDigest(expected.Submission, expected.Input, expected.Settings) ||
		proof.Conversation != handle.Conversation || proof.Generation != handle.Generation || proof.CreationID != creationID ||
		proof.HistoryRef != expected.HistoryRef || !proof.Persisted || proof.Bootstrap == nil {
		return agentbackend.ErrConversationNotReady
	}
	accepted := proof.Bootstrap
	if accepted.Schema != 1 || accepted.Phase != "Accepted" || accepted.Ref != expected.Submission ||
		accepted.Input != expected.Input || accepted.Settings != expected.Settings || accepted.Refusal != "" ||
		accepted.Witness == nil || accepted.Witness.SessionID != handle.Conversation.NativeID || accepted.Witness.EntryID == "" ||
		accepted.Witness.EffectiveDigest != piruntime.MessageDigest(expected.Input.Message) {
		return agentbackend.ErrConversationNotReady
	}
	return nil
}

// PublishMemberReady owns admission. Callers pass references and hold no locks.
// Native creation/dispatch never occurs here. Partial binding/live publication
// is returned explicitly and remains held by the normal revision checks.
func (s *Server) PublishMemberReady(ctx context.Context, expectedRevision uint64, handle agentbackend.ExecutionHandle, creationID, bootstrapOperationID string) (MemberPublication, error) {
	if s.config.PiMembers == nil {
		return MemberPublication{}, errors.New("Pi member publication is unavailable")
	}
	ref := handle.Conversation
	if ref.Backend != agentbackend.Pi || ref.Session.Workspace != s.config.Workspace || !session.ValidSlug(ref.Session.Slug) ||
		ref.Member == "lead" || ref.Member == "" || ref.NativeID == "" || expectedRevision == 0 || expectedRevision == math.MaxUint64 ||
		!messageDigestPattern.MatchString(creationID) || !piPublicationOperationID.MatchString(bootstrapOperationID) {
		return MemberPublication{}, agentbackend.ErrBindingConflict
	}
	_, releaseTransition, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return MemberPublication{}, err
	}
	defer releaseTransition()
	if err := s.requireCurrentHostProfile(); err != nil {
		return MemberPublication{}, err
	}
	runtimeLock, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, ref.Session.Slug)
	if err != nil {
		return MemberPublication{}, err
	}
	defer runtimeLock.Close()
	if err := s.requireCurrentHostProfile(); err != nil {
		return MemberPublication{}, err
	}
	if owner, err := session.PendingLifecycle(s.config.Workspace, ref.Session.Slug); err != nil || owner != "" {
		return MemberPublication{}, errors.New("session has unfinished lifecycle work")
	}
	admission, releaseRoster, err := s.config.PiMembers.LockRoster(ctx, handle)
	if err != nil {
		return MemberPublication{}, err
	}
	if releaseRoster == nil {
		return MemberPublication{}, errors.New("Pi member admission has no owner")
	}
	defer releaseRoster()
	if err := s.requireCurrentHostProfile(); err != nil {
		return MemberPublication{}, err
	}
	store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, err := store.Load(ref.Session.Slug)
	if err != nil {
		return MemberPublication{}, err
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, ref.Session.Slug, s.config.Workspace)
	if err != nil {
		return MemberPublication{Binding: binding}, err
	}
	result := MemberPublication{Binding: binding, Authority: authority}
	if binding.SessionIncarnation != ref.Session.Incarnation || binding.CreationReceiptID != creationID || binding.State != "ready" ||
		authority.SessionIncarnation != ref.Session.Incarnation || authority.State != "ready" {
		return result, agentbackend.ErrBindingConflict
	}
	if authority.Generation != handle.Generation {
		return result, agentbackend.ErrStaleExecution
	}
	index := -1
	for i, member := range binding.Conversations {
		if member.Address == ref.Member {
			index = i
			break
		}
	}
	if index < 0 {
		return result, agentbackend.ErrBindingConflict
	}
	member := binding.Conversations[index]
	if member.Incarnation != ref.Incarnation || (member.NativeID != "" && member.NativeID != ref.NativeID) {
		return result, agentbackend.ErrBindingConflict
	}
	if member.State == "ready" {
		if (binding.Revision != expectedRevision && binding.Revision != expectedRevision+1) ||
			(authority.BindingRevision != binding.Revision && authority.BindingRevision != expectedRevision) {
			return result, agentbackend.ErrBindingConflict
		}
	} else if binding.Revision != expectedRevision || authority.ValidateBinding(binding) != nil {
		return result, agentbackend.ErrBindingConflict
	}
	partialRevision := uint64(0)
	if member.State == "ready" && binding.Revision == expectedRevision+1 && authority.BindingRevision == expectedRevision {
		partialRevision = expectedRevision
	}
	if _, _, _, err := s.proveRetainedPiRootAtRevisionLocked(ctx, ref.Session.Slug, creationID, partialRevision); err != nil {
		return result, err
	}
	// External authoritative observation precedes the short binding-store lock.
	proof, err := s.config.PiMembers.VerifyCreation(ctx, handle, creationID, admission.HistoryRef, &admission.Submission)
	if err != nil {
		return result, err
	}
	if err := validatePiMemberProof(handle, creationID, bootstrapOperationID, admission, proof); err != nil {
		return result, err
	}
	if member.State != "ready" {
		next := binding
		next.Conversations = append([]session.BackendConversation(nil), binding.Conversations...)
		next.Revision++
		next.Conversations[index].State, next.Conversations[index].NativeID = "ready", ref.NativeID
		result.Binding, err = store.CompareAndSwap(ctx, expectedRevision, next)
		if err != nil {
			return result, err
		}
	}
	nextAuthority := authority
	nextAuthority.BindingRevision = result.Binding.Revision
	if err := session.PublishPiRuntimeAuthority(s.config.AuthorityDir, result.Binding, nextAuthority); err != nil {
		return result, err
	}
	result.Authority, result.Complete = nextAuthority, true
	return result, nil
}
