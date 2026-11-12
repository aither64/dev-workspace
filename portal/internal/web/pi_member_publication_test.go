package web

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type memberPublicationFixture struct {
	server      *Server
	proof       piruntime.CreationProof
	admission   PiMemberAdmission
	handle      agentbackend.ExecutionHandle
	afterVerify func()
	locked      bool
}

func (f *memberPublicationFixture) LockRoster(_ context.Context, _ agentbackend.ExecutionHandle) (PiMemberAdmission, func(), error) {
	f.locked = true
	return f.admission, func() { f.locked = false }, nil
}
func (f *memberPublicationFixture) VerifyCreation(_ context.Context, _ agentbackend.ExecutionHandle, _, _ string, ref *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	if !f.locked {
		return piruntime.CreationProof{}, errors.New("publication bypassed roster admission")
	}
	if f.afterVerify != nil {
		f.afterVerify()
	}
	return f.proof, nil
}

func newMemberPublicationFixture(t *testing.T) (*memberPublicationFixture, session.BackendBindingStore) {
	t.Helper()
	s, receipt, _ := newPiCreationFixture(t)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, err := store.Load("example")
	if err != nil {
		t.Fatal(err)
	}
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, "example", s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: binding.Workspace, Slug: binding.Slug, Incarnation: binding.SessionIncarnation}, Backend: agentbackend.Pi, Member: binding.Conversations[1].Address, Incarnation: binding.Conversations[1].Incarnation, NativeID: "pi-member"}, Generation: authority.Generation}
	settings := receipt.Pi.Selection.Members[1].Settings.Effective()
	input := piruntime.SendInput{Message: "the first real assignment", ActionContext: "team"}
	ref := piruntime.SubmissionRef{Conversation: handle.Conversation, OperationID: submissionValidationID, Action: "send"}
	ref.Digest = piruntime.SubmissionDigest(ref, input, settings)
	status := &piruntime.SubmissionStatus{Schema: 1, Ref: ref, Phase: "Accepted", Generation: handle.Generation, Settings: settings, Input: input,
		Witness: &piruntime.NativeWitness{SessionID: handle.Conversation.NativeID, EntryID: "entry", EffectiveDigest: piruntime.MessageDigest(input.Message)}}
	f := &memberPublicationFixture{server: s, handle: handle,
		admission: PiMemberAdmission{HistoryRef: strings.Repeat("9", 64), Submission: ref, Input: input, Settings: settings},
		proof:     piruntime.CreationProof{Conversation: handle.Conversation, Generation: handle.Generation, CreationID: binding.CreationReceiptID, HistoryRef: strings.Repeat("9", 64), Persisted: true, Bootstrap: status}}
	s.config.PiMembers = f
	return f, store
}

func TestPiMemberPublicationRequiresMaterializationAndReplaysPartialCAS(t *testing.T) {
	f, store := newMemberPublicationFixture(t)
	ctx := context.Background()
	f.proof.Persisted = false
	result, err := f.server.PublishMemberReady(ctx, 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID)
	if !errors.Is(err, agentbackend.ErrConversationNotReady) || result.Complete {
		t.Fatalf("empty history became ready: %#v %v", result, err)
	}
	binding, err := store.Load("example")
	if err != nil || binding.Revision != 2 || binding.Conversations[1].State != "creating" {
		t.Fatalf("refusal changed binding: %#v %v", binding, err)
	}
	f.proof.Persisted = true
	f.afterVerify = func() {
		if err := os.Chmod(f.server.config.AuthorityDir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	result, err = f.server.PublishMemberReady(ctx, 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID)
	if err == nil || result.Complete || result.Binding.Revision != 3 || result.Binding.Conversations[1].State != "ready" {
		t.Fatalf("partial CAS lost durable result: %#v %v", result, err)
	}
	if err := os.Chmod(f.server.config.AuthorityDir, 0700); err != nil {
		t.Fatal(err)
	}
	f.afterVerify = nil
	result, err = f.server.PublishMemberReady(ctx, 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID)
	if err != nil || !result.Complete || result.Binding.Revision != 3 || result.Authority.BindingRevision != 3 || result.Authority.Generation != f.handle.Generation {
		t.Fatalf("exact replay failed: %#v %v", result, err)
	}
	if f.locked {
		t.Fatal("member operation lock leaked")
	}
	if _, err := f.server.PublishMemberReady(ctx, 1, f.handle, f.proof.CreationID, f.admission.Submission.OperationID); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("unrelated revision accepted: %v", err)
	}
}

func TestPiMemberPublicationRefusesUnregisteredAdapterAndFreshUtility(t *testing.T) {
	f, store := newMemberPublicationFixture(t)
	f.server.config.PiMembers = nil
	if _, err := f.server.PublishMemberReady(context.Background(), 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID); err == nil {
		t.Fatal("missing verifier accepted")
	}
	f.server.config.PiMembers = f
	handle := f.handle
	handle.Conversation.Member = "utility0"
	if _, err := f.server.PublishMemberReady(context.Background(), 2, handle, f.proof.CreationID, f.admission.Submission.OperationID); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("fresh utility entered retained binding: %v", err)
	}
	binding, err := store.Load("example")
	if err != nil || binding.Revision != 2 || len(binding.Conversations) != 2 {
		t.Fatalf("refusal mutated binding: %#v %v", binding, err)
	}
}

func TestPiMemberPublicationRequiresExactAuthoritativeProof(t *testing.T) {
	cases := map[string]func(*memberPublicationFixture){
		"missing-bootstrap":    func(f *memberPublicationFixture) { f.proof.Bootstrap = nil },
		"unknown-input":        func(f *memberPublicationFixture) { f.proof.Bootstrap.Phase = "Unknown" },
		"terminal-negative":    func(f *memberPublicationFixture) { f.proof.Bootstrap.Phase = "NotSubmitted" },
		"wrong-history":        func(f *memberPublicationFixture) { f.proof.HistoryRef = strings.Repeat("8", 64) },
		"wrong-native":         func(f *memberPublicationFixture) { f.proof.Conversation.NativeID = "another" },
		"wrong-generation":     func(f *memberPublicationFixture) { f.proof.Generation = strings.Repeat("7", 64) },
		"wrong-creation":       func(f *memberPublicationFixture) { f.proof.CreationID = strings.Repeat("8", 64) },
		"wrong-original-input": func(f *memberPublicationFixture) { f.proof.Bootstrap.Input.Message = "replacement" },
		"wrong-context":        func(f *memberPublicationFixture) { f.proof.Bootstrap.Input.ActionContext = "initial" },
		"wrong-settings":       func(f *memberPublicationFixture) { f.proof.Bootstrap.Settings.Model = "replacement" },
		"wrong-operation":      func(f *memberPublicationFixture) { f.proof.Bootstrap.Ref.OperationID = "another" },
		"wrong-digest":         func(f *memberPublicationFixture) { f.proof.Bootstrap.Ref.Digest = strings.Repeat("8", 64) },
		"missing-witness":      func(f *memberPublicationFixture) { f.proof.Bootstrap.Witness = nil },
		"wrong-witness-native": func(f *memberPublicationFixture) { f.proof.Bootstrap.Witness.SessionID = "another" },
		"missing-entry":        func(f *memberPublicationFixture) { f.proof.Bootstrap.Witness.EntryID = "" },
		"operation-digest-is-not-message-witness": func(f *memberPublicationFixture) {
			f.proof.Bootstrap.Witness.EffectiveDigest = f.admission.Submission.Digest
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, store := newMemberPublicationFixture(t)
			mutate(f)
			result, err := f.server.PublishMemberReady(context.Background(), 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID)
			if err == nil || result.Complete {
				t.Fatalf("forged proof accepted: %#v %v", result, err)
			}
			binding, err := store.Load("example")
			if err != nil || binding.Revision != 2 || binding.Conversations[1].State != "creating" {
				t.Fatalf("refusal mutated binding: %#v %v", binding, err)
			}
		})
	}
}
