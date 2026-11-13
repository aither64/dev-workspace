package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piCreationEvidence struct {
	Schema                  int                              `json:"schema"`
	Workspace               string                           `json:"workspace"`
	Slug                    string                           `json:"slug"`
	Backend                 agentbackend.Kind                `json:"backend"`
	BackendContract         int                              `json:"backendContract"`
	ReceiptID               string                           `json:"receiptId"`
	SessionIncarnation      string                           `json:"sessionIncarnation"`
	BindingRevision         uint64                           `json:"bindingRevision"`
	Conversations           []session.PiReservedConversation `json:"conversations"`
	NativeID                string                           `json:"nativeId"`
	InitialInputID          string                           `json:"initialInputId"`
	InitialSubmissionDigest string                           `json:"initialSubmissionDigest"`
	GoalSHA256              string                           `json:"goalSha256"`
	Model                   string                           `json:"model"`
	Effort                  string                           `json:"effort"`
	Provider                string                           `json:"provider"`
	ProviderSelection       session.PiCreationSelection      `json:"providerSelection"`
	CatalogDigest           string                           `json:"catalogDigest"`
	TeamDigest              string                           `json:"teamDigest"`
	Team                    string                           `json:"team"`
	TrackingDevice          uint64                           `json:"trackingDevice"`
	TrackingInode           uint64                           `json:"trackingInode"`
}

func (s *Server) readPiCreationEvidence(receipt creationReceipt) (piCreationEvidence, error) {
	var raw json.RawMessage
	var evidence piCreationEvidence
	if err := readCreationJSON(s.creationEvidencePath(receipt), &raw); err != nil {
		return evidence, err
	}
	if err := session.DecodePiCreationJSON(raw, &evidence, maxCreationReceiptBytes); err != nil {
		return evidence, err
	}
	return evidence, nil
}
func (s *Server) provePiCreation(receipt creationReceipt) error {
	if !s.config.PiCreation.available() {
		return piruntime.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lock, err := session.LockRuntimeSharedContext(ctx, s.config.AuthorityDir, receipt.Request.Slug)
	if err != nil {
		return err
	}
	defer lock.Close()
	return s.provePiCreationLocked(ctx, receipt)
}
func (s *Server) provePiCreationLocked(ctx context.Context, receipt creationReceipt) error {
	intent, err := receipt.piIntent()
	if err != nil {
		return err
	}
	retained, _, _, err := s.proveRetainedPiRootLocked(ctx, intent.Slug, intent.ReceiptID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(retained, intent) {
		return agentbackend.ErrBindingConflict
	}
	return nil
}

// Read the original immutable proof even after its mutable cache receipt was
// retired. This is observational: no Ensure, Load, Create, Bind or Send occurs.
// The caller holds transition/runtime admission and current package identity.
func (s *Server) proveRetainedPiRootLocked(ctx context.Context, slug, creationID string) (session.PiCreationIntent, session.BackendBinding, piCreationEvidence, error) {
	return s.proveRetainedPiRootAtRevisionLocked(ctx, slug, creationID, 0)
}
func (s *Server) proveRetainedPiRootAtRevisionLocked(ctx context.Context, slug, creationID string, partialRevision uint64) (session.PiCreationIntent, session.BackendBinding, piCreationEvidence, error) {
	var intent session.PiCreationIntent
	var binding session.BackendBinding
	var evidence piCreationEvidence
	fail := func(err error) (session.PiCreationIntent, session.BackendBinding, piCreationEvidence, error) {
		return intent, binding, evidence, err
	}
	if !s.config.PiCreation.available() {
		return fail(piruntime.ErrUnavailable)
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(slug)
	if err != nil {
		return fail(err)
	}
	if binding.CreationReceiptID != creationID || binding.State != "ready" {
		return fail(agentbackend.ErrBindingConflict)
	}
	receipt := creationReceipt{ReceiptID: creationID, Request: creationRequest{Slug: slug}}
	intent, err = readPiCreationIntent(s.creationEvidencePath(receipt) + ".request")
	if err != nil {
		return fail(err)
	}
	if intent.Workspace != s.config.Workspace || intent.Slug != slug || intent.ReceiptID != creationID {
		return fail(agentbackend.ErrBindingConflict)
	}
	if err = intent.Validate(s.config.PiCreation.Catalog); err != nil {
		return fail(err)
	}
	if err = intent.ValidateBinding(binding); err != nil {
		return fail(err)
	}
	journal, err := readPiCreationJournal(piJournalPath(intent.Workspace, slug))
	if err != nil {
		return fail(err)
	}
	if journal.State != "ready" || journal.Native == nil || !reflect.DeepEqual(journal.PiCreationIntent, intent) {
		return fail(agentbackend.ErrBindingConflict)
	}
	history, err := session.CompletedRemovalHistory(intent.Workspace, slug, s.config.UserStateRoot)
	if err != nil {
		return fail(err)
	}
	if history != intent.DeletionHistorySHA256 {
		return fail(agentbackend.ErrBindingConflict)
	}
	// A present cache entry must still agree. Absence is accepted only here for
	// completed retained proof, never by withPiCreationOwner/resume admission.
	var current creationReceipt
	var raw map[string]json.RawMessage
	if err = readCreationJSON(s.creationPath(slug), &raw); err == nil {
		if err = validateCreationReceiptShape(raw); err != nil {
			return fail(err)
		}
		if err = readCreationJSON(s.creationPath(slug), &current); err != nil {
			return fail(err)
		}
		if !piReceiptMatches(current, intent) {
			return fail(agentbackend.ErrBindingConflict)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	conversation, err := binding.Resolve("lead")
	if err != nil {
		return fail(err)
	}
	if conversation.NativeID != journal.Native.NativeID {
		return fail(agentbackend.ErrBindingConflict)
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, slug, intent.Workspace)
	if err != nil {
		return fail(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: conversation, Generation: authority.Generation}
	projected := binding
	if partialRevision != 0 {
		// Only the member-publication recovery owner may request this exact prior
		// projection after it validates the same already-ready member CAS. Readers
		// and dispatch always require the current revision with no exception.
		if binding.Revision != partialRevision+1 || authority.BindingRevision != partialRevision {
			return fail(agentbackend.ErrBindingConflict)
		}
		projected.Revision = partialRevision
	}
	if err = authority.ValidateHandle(projected, handle); err != nil {
		return fail(err)
	}
	evidence, err = s.readPiCreationEvidence(receipt)
	if err != nil {
		return fail(err)
	}
	// The current source's CAS preserves the complete reserved list and every
	// established ready/native identity. Historical root proof is immutable;
	// inequality alone cannot replace these complete identity/options checks.
	if evidence.Schema != 4 || evidence.Workspace != intent.Workspace || evidence.Slug != slug || evidence.Backend != agentbackend.Pi || evidence.BackendContract != 1 || evidence.ReceiptID != creationID || evidence.SessionIncarnation != intent.SessionIncarnation || evidence.BindingRevision == 0 || evidence.BindingRevision > binding.Revision || !reflect.DeepEqual(evidence.Conversations, intent.Conversations) || evidence.NativeID != conversation.NativeID || evidence.InitialInputID != intent.InitialInputID || !messageDigestPattern.MatchString(evidence.InitialSubmissionDigest) || evidence.GoalSHA256 != intent.GoalSHA256 || evidence.Model != intent.Model || evidence.Effort != intent.Effort || evidence.Provider != intent.Provider || !reflect.DeepEqual(evidence.ProviderSelection, intent.ProviderSelection) || evidence.CatalogDigest != intent.DirectTeam.CatalogDigest || evidence.TeamDigest != intent.DirectTeam.TeamDigest || evidence.Team != intent.DirectTeam.ID {
		return fail(errors.New("Pi completion evidence disagrees with retained creation"))
	}
	ref := piruntime.SubmissionRef{Conversation: conversation, OperationID: intent.InitialInputID, Action: "send", Digest: evidence.InitialSubmissionDigest}
	accepted, err := s.config.PiCreation.Runtime.Lookup(ctx, ref)
	if err != nil {
		return fail(err)
	}
	if accepted.Ref != ref || accepted.Phase != "Accepted" || accepted.Refusal != "" || accepted.Input.ActionContext != "initial" || accepted.Input.Retry || piruntime.MessageDigest(accepted.Input.Message) != intent.GoalSHA256 || accepted.Settings != intent.ProviderSelection.Members[0].Settings.Effective() || piruntime.SubmissionDigest(ref, accepted.Input, accepted.Settings) != ref.Digest {
		return fail(agentbackend.ErrConversationNotReady)
	}
	proof, err := s.config.PiCreation.Runtime.VerifyCreation(ctx, handle, creationID, journal.Native.HistoryRef, &ref)
	if err != nil {
		return fail(err)
	}
	expected := PiMemberAdmission{HistoryRef: journal.Native.HistoryRef, Submission: ref, Input: accepted.Input, Settings: accepted.Settings}
	if proof.Bootstrap == nil || !reflect.DeepEqual(*proof.Bootstrap, accepted) {
		return fail(agentbackend.ErrConversationNotReady)
	}
	if err = validatePiMemberProof(handle, creationID, intent.InitialInputID, expected, proof); err != nil {
		return fail(err)
	}
	result := piTrackingResult{TrackingDevice: evidence.TrackingDevice, TrackingInode: evidence.TrackingInode}
	if err = s.validatePiTracking(intent, result, true); err != nil {
		return fail(err)
	}
	return intent, binding, evidence, nil
}

// Retiring the bounded portal cache retains its existing immutable proof files
// and predecessor-recognized ready journal for every dependent Pi record.
func (s *Server) retirePiCreation(receipt creationReceipt) error {
	if receipt.State != "ready" {
		return errors.New("Pi creation receipt is still held")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer release()
	if err = s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if err = s.provePiCreation(receipt); err != nil {
		return err
	}
	if err = os.Remove(s.creationPath(receipt.Request.Slug)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("retire Pi cache receipt: %w", err)
	}
	dir, err := os.Open(filepath.Dir(s.creationPath(receipt.Request.Slug)))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return err
	}
	delete(s.creations, receipt.Request.Slug)
	return nil
}
