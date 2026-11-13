package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type piCreationRuntimeFixture struct {
	runtime        piruntime.RuntimeRef
	state          piruntime.ConversationState
	submission     *piruntime.SubmissionStatus
	creates, sends int
	outcome        string
	proofError     error
	beforeCreate   func() error
}

func (f *piCreationRuntimeFixture) Ensure(_ context.Context, r piruntime.StartRequest) (piruntime.RuntimeStatus, error) {
	f.runtime = piruntime.RuntimeRef{Session: r.Session, InstanceID: "instance", RequestID: "request", Digest: strings.Repeat("7", 64), Generation: strings.Repeat("8", 64)}
	return piruntime.RuntimeStatus{Runtime: f.runtime, Phase: "ready"}, nil
}
func (f *piCreationRuntimeFixture) Create(_ context.Context, r piruntime.RuntimeRef, request piruntime.CreateRequest) (piruntime.ConversationState, error) {
	if f.beforeCreate != nil {
		if err := f.beforeCreate(); err != nil {
			return piruntime.ConversationState{}, err
		}
	}
	f.creates++
	f.state = piruntime.ConversationState{Conversation: agentbackend.ConversationRef{Session: request.Session, Backend: agentbackend.Pi, Member: request.Member, Incarnation: request.Incarnation, NativeID: "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"}, Generation: r.Generation, State: "idle", HistoryRef: strings.Repeat("9", 64), Settings: request.Settings}
	return f.state, nil
}
func (f *piCreationRuntimeFixture) Bind(_ context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	if h.Conversation != f.state.Conversation || h.Generation != f.state.Generation {
		return backend.Operations{}, piruntime.ErrScope
	}
	return backend.Operations{Send: f}, nil
}
func (f *piCreationRuntimeFixture) Send(_ context.Context, id backend.Identity, request backend.SendRequest) (backend.SendResult, error) {
	f.sends++
	input := piruntime.SendInput{Message: request.Message, ActionContext: request.ActionContext, Retry: request.Retry}
	ref := piruntime.SubmissionRef{Conversation: f.state.Conversation, OperationID: request.ClientUserMessageID, Action: "send"}
	ref.Digest = piruntime.SubmissionDigest(ref, input, f.state.Settings)
	status := piruntime.SubmissionStatus{Schema: 1, Ref: ref, Phase: f.outcome, Generation: f.state.Generation, Settings: f.state.Settings, Input: input, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if status.Phase == "Accepted" {
		status.Witness = &piruntime.NativeWitness{SessionID: f.state.Conversation.NativeID, EntryID: "first-real-user-entry", EffectiveDigest: piruntime.MessageDigest(input.Message)}
	} else if status.Phase == "NotSubmitted" {
		status.Refusal = "before-native"
	}
	f.submission = &status
	return backend.SendResult{}, nil
}
func (f *piCreationRuntimeFixture) Lookup(_ context.Context, ref piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	if f.submission == nil {
		return piruntime.SubmissionStatus{}, os.ErrNotExist
	}
	if f.submission.Ref != ref {
		return piruntime.SubmissionStatus{}, piruntime.ErrConflict
	}
	return *f.submission, nil
}
func (f *piCreationRuntimeFixture) VerifyCreation(_ context.Context, h agentbackend.ExecutionHandle, id, history string, bootstrap *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	if f.proofError != nil {
		return piruntime.CreationProof{}, f.proofError
	}
	p := piruntime.CreationProof{Conversation: f.state.Conversation, Generation: f.state.Generation, CreationID: id, HistoryRef: f.state.HistoryRef, Persisted: f.submission != nil && f.submission.Phase == "Accepted"}
	if bootstrap != nil && f.submission != nil && f.submission.Ref == *bootstrap {
		p.Bootstrap = f.submission
	}
	return p, nil
}
func piCreationCatalogFixture() piproviders.Catalog {
	return piproviders.Catalog{Schema: 1, Digest: strings.Repeat("1", 64), Providers: []piproviders.Provider{{ID: "host", Label: "Fixture", NativeID: "native", RuntimeConfigRef: "runtime"}}, Accounts: []piproviders.Account{{Ref: "account", ProviderID: "host", CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: "native", AuthKind: "api_key"}}, Models: []piproviders.Model{{Kind: "physical", ProviderID: "host", ID: "model", Label: "Model", ThinkingLevels: []string{"high"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture"}}, Routes: []piproviders.RouteGate{{ProviderID: "host", AccountRef: "account", ModelID: "model", State: "available", ReasonCode: "verified", EvidenceRef: "fixture"}}}
}
func newPiCreationFixture(t *testing.T) (*Server, creationReceipt, *piCreationRuntimeFixture) {
	t.Helper()
	s := newTestServer(t)
	runWebGit(t, "init", "-b", "master", s.config.Workspace)
	runWebGit(t, "-C", s.config.Workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture baseline")
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../../libexec/dev-session")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "pi-tracking")
	quote := func(value string) string { data, _ := json.Marshal(value); return string(data) }
	script := "#!" + ruby + "\nENV['DEV_WORKSPACES_STATE']=" + quote(s.config.UserStateRoot) + "\nENV.delete('DEV_WORKSPACE_NAME')\nload " + quote(source) + "\nARGV.unshift('--workspace'," + quote(s.config.Workspace) + ",'--authority-dir'," + quote(s.config.AuthorityDir) + ")\nexit DevSession::CLI.new(ARGV).run\n"
	if err = os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.config.DevSession = wrapper
	c := piCreationCatalogFixture()
	f := &piCreationRuntimeFixture{outcome: "Accepted"}
	selection := func(_ context.Context, ref agentbackend.SessionRef, _ []session.PiReservedConversation, _ teamruntime.Preset) (piproviders.Policy, error) {
		return piproviders.Policy{Session: ref, Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: piproviders.Selection{Chain: []piproviders.Target{{ProviderID: "host", AccountRef: "account", ModelID: "model", Thinking: "high"}}, FallbackOn: []piproviders.FailureCategory{}}}, Overrides: []piproviders.Override{}}, nil
	}
	s.config.PiCreation = &PiCreationServices{Catalog: c, Selection: selection, Runtime: f, StartRequest: func(intent session.PiCreationIntent, rev uint64) (piruntime.StartRequest, error) {
		return piruntime.StartRequest{Session: intent.SessionRef(), BindingRevision: rev}, nil
	}}
	preset := teamruntime.Preset{ID: "delegated", Name: "Fixture", Description: "Fixture team", Roles: []string{"lead", "implementer0"}, CatalogDigest: strings.Repeat("2", 64), TeamDigest: strings.Repeat("3", 64), LeadModel: "model", LeadEffort: "high", Members: []teamruntime.MemberSpec{{Role: "implementer", Address: "implementer0", Model: "model", Effort: "high", Behavior: "implementer", Access: "workspace_write"}}}
	ref := agentbackend.SessionRef{Workspace: s.config.Workspace, Slug: "example", Incarnation: strings.Repeat("a", 64)}
	conversations := []session.PiReservedConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64)}, {Address: "implementer0", Incarnation: strings.Repeat("c", 64)}}
	policy, err := selection(context.Background(), ref, conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := session.FreezePiCreationSelection(c, policy, conversations)
	if err != nil {
		t.Fatal(err)
	}
	history, err := session.CompletedRemovalHistory(s.config.Workspace, "example", s.config.UserStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := creationReceipt{Schema: 4, Workspace: s.config.Workspace, Request: creationRequest{Backend: "pi", Kind: "new", Slug: "example", Goal: "The original real request", Team: preset.ID, CatalogDigest: preset.CatalogDigest, directTeam: &preset}, ReceiptID: strings.Repeat("d", 64), DeletionHistorySHA256: history, Attempt: 1, State: "running", Phase: "Preparing Pi", StartedAt: now, UpdatedAt: now, Validated: true, Goal: "The original real request", Model: "model", Effort: "high", DirectTeam: &preset, Pi: &piCreationIdentity{BackendContract: 1, SessionIncarnation: ref.Incarnation, Conversations: conversations, Provider: "native", InitialInputID: "11111111-1111-4111-8111-111111111111", Selection: frozen}}
	if err = s.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.creations["example"] = receipt
	return s, receipt, f
}
func TestPiCreationPublishesRootAndRetainsCreatingMembers(t *testing.T) {
	s, receipt, f := newPiCreationFixture(t)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || f.sends != 1 {
		t.Fatalf("native effects %d/%d", f.creates, f.sends)
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load("example")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Revision != 2 || binding.State != "ready" || binding.Conversations[1].State != "creating" {
		t.Fatalf("lost held member: %#v", binding)
	}
	if err = s.provePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if err = s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || f.sends != 1 {
		t.Fatal("completion replay repeated native effects")
	}
}
func TestPiCreationNativeUncertaintyNeverRedispatches(t *testing.T) {
	for _, phase := range []string{"Unknown", "NotSubmitted"} {
		t.Run(phase, func(t *testing.T) {
			s, r, f := newPiCreationFixture(t)
			f.outcome = phase
			if err := s.initializePiCreation(r); err == nil {
				t.Fatal("unproved initial input became ready")
			}
			if err := s.initializePiCreation(r); err == nil {
				t.Fatal("terminal/Unknown input resumed")
			}
			if f.creates != 1 || f.sends != 1 {
				t.Fatalf("redispatch %d/%d", f.creates, f.sends)
			}
		})
	}
}
func TestPiCreationCrashPublicationBoundaries(t *testing.T) {
	for _, point := range []string{"companion", "journal", "binding", "tracking", "native_intent", "native_result", "initial_input", "ready_binding", "ready_authority", "ready_tracking", "completion", "ready_journal"} {
		t.Run(point, func(t *testing.T) {
			s, r, f := newPiCreationFixture(t)
			hit := false
			s.piCreationCheckpoint = func(at string) error {
				if at == point && !hit {
					hit = true
					return errors.New("fixture crash")
				}
				return nil
			}
			if err := s.initializePiCreation(r); err == nil || !hit {
				t.Fatalf("boundary not reached: %s %v", point, err)
			}
			s.piCreationCheckpoint = nil
			err := s.initializePiCreation(r)
			held := point == "native_intent" || point == "native_result"
			if held && err == nil {
				t.Fatal("lost native/input proof recovered through another effect")
			}
			if !held && err != nil {
				t.Fatal(err)
			}
			if f.creates > 1 || f.sends > 1 {
				t.Fatalf("crash replay duplicated native effects %d/%d", f.creates, f.sends)
			}
		})
	}
}

func TestPiCreationRetainedRootProofSurvivesCacheRetirementAndMemberCAS(t *testing.T) {
	f, store := newMemberPublicationFixture(t)
	s := f.server
	receipt := s.creations["example"]
	receipt.State = "ready"
	if err := s.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.creations["example"] = receipt
	paths := []string{s.creationEvidencePath(receipt) + ".request", s.creationEvidencePath(receipt), piJournalPath(s.config.Workspace, "example")}
	before := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(data)
	}
	if err := s.retirePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.creationPath("example")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutable cache retained: %v", err)
	}
	if err := s.withPiCreationOwner(context.Background(), receipt, func(session.PiCreationIntent) error { return nil }); err == nil {
		t.Fatal("retired cache resumed creating work")
	}
	result, err := s.PublishMemberReady(context.Background(), 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID)
	if err != nil || !result.Complete {
		t.Fatalf("retained root did not admit member: %#v %v", result, err)
	}
	if err = s.provePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	binding, err := store.Load("example")
	if err != nil || binding.Revision != 3 {
		t.Fatalf("bad member revision: %#v %v", binding, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != before[path] {
			t.Fatalf("immutable root proof changed: %s %v", path, err)
		}
	}
}
func TestPiMemberPublicationRequiresCompletedRootAndExactRetainedInput(t *testing.T) {
	for _, bad := range []string{"missing-completion", "missing-journal", "creating-journal", "wrong-input-digest", "missing-original-input", "wrong-original-context", "wrong-live-revision", "wrong-history-ref", "missing-native-history"} {
		t.Run(bad, func(t *testing.T) {
			f, store := newMemberPublicationFixture(t)
			s := f.server
			r := s.creations["example"]
			switch bad {
			case "missing-completion":
				os.Remove(s.creationEvidencePath(r))
			case "missing-journal":
				os.Remove(piJournalPath(s.config.Workspace, "example"))
			case "creating-journal":
				j, err := readPiCreationJournal(piJournalPath(s.config.Workspace, "example"))
				if err != nil {
					t.Fatal(err)
				}
				j.State = "creating"
				if err = writePiCreationRecord(piJournalPath(s.config.Workspace, "example"), j, 64<<10); err != nil {
					t.Fatal(err)
				}
			case "wrong-input-digest":
				e, err := s.readPiCreationEvidence(r)
				if err != nil {
					t.Fatal(err)
				}
				e.InitialSubmissionDigest = strings.Repeat("0", 64)
				if err = writePiCreationRecord(s.creationEvidencePath(r), e, maxCreationReceiptBytes); err != nil {
					t.Fatal(err)
				}
			case "missing-original-input":
				s.config.PiCreation.Runtime.(*piCreationRuntimeFixture).submission = nil
			case "wrong-original-context":
				s.config.PiCreation.Runtime.(*piCreationRuntimeFixture).submission.Input.ActionContext = "changed"
			case "wrong-history-ref":
				journal, err := readPiCreationJournal(piJournalPath(s.config.Workspace, "example"))
				if err != nil {
					t.Fatal(err)
				}
				journal.Native.HistoryRef = strings.Repeat("7", 64)
				if err = writePiCreationRecord(piJournalPath(s.config.Workspace, "example"), journal, 64<<10); err != nil {
					t.Fatal(err)
				}
			case "missing-native-history":
				s.config.PiCreation.Runtime.(*piCreationRuntimeFixture).proofError = os.ErrNotExist
			case "wrong-live-revision":
				b, err := store.Load("example")
				if err != nil {
					t.Fatal(err)
				}
				b.Revision++
				if _, err = store.CompareAndSwap(context.Background(), 2, b); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.PublishMemberReady(context.Background(), 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID)
			if err == nil || result.Complete {
				t.Fatalf("bad root proof admitted member: %#v %v", result, err)
			}
			b, err := store.Load("example")
			if err != nil || b.Conversations[1].State != "creating" {
				t.Fatalf("refusal published member %#v %v", b, err)
			}
		})
	}
}

func TestPiCreationUnregisteredBrowserBackendNeverFallsBack(t *testing.T) {
	s := newTestServer(t)
	form := url.Values{"backend": {"pi"}, "name": {"example"}, "creation_date": {"2026-11-12"}, "goal": {"The request"}}
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	s.createSession(response, req)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unregistered Pi backend: %d %s", response.Code, response.Body.String())
	}
	if len(s.creations) != 0 {
		t.Fatal("Pi refusal saved a Codex receipt")
	}
	if _, err := s.acceptCreation(creationRequest{Backend: "pi", Kind: "new", Slug: "example", Goal: "original"}); err == nil {
		t.Fatal("nil Pi adapter accepted effects")
	}
}

func TestPiCreationAcceptedIdentityAndSettingsNeverChangeOnReplay(t *testing.T) {
	s, r, f := newPiCreationFixture(t)
	same, err := s.acceptCreation(r.Request)
	if err != nil || same.Pi.InitialInputID != r.Pi.InitialInputID || same.Pi.SessionIncarnation != r.Pi.SessionIncarnation || same.ReceiptID != r.ReceiptID {
		t.Fatalf("accepted replay changed identity: %#v %v", same, err)
	}
	for _, mutate := range []func(*creationRequest){func(q *creationRequest) { q.Backend = "codex" }, func(q *creationRequest) { q.Model = "other" }, func(q *creationRequest) { q.Goal = "other" }, func(q *creationRequest) { q.Kind = "fork" }} {
		q := r.Request
		mutate(&q)
		if _, err := s.acceptCreation(q); err == nil {
			t.Fatal("accepted request options changed")
		}
	}
	if _, err = s.installPiCreationLocked(r.Request, r.ReceiptID, strings.Repeat("0", 64), false); err == nil {
		t.Fatal("changed deletion epoch adopted original identity")
	}
	s.config.PiCreation.Catalog.Digest = strings.Repeat("0", 64)
	if err = s.initializePiCreation(r); err == nil {
		t.Fatal("current catalog replaced frozen catalog")
	}
	if f.creates != 0 || f.sends != 0 {
		t.Fatal("refused selection caused native effects")
	}
}
func TestPiCreationForgedReadyTrackingDoesNotCreateNativeSession(t *testing.T) {
	s, r, f := newPiCreationFixture(t)
	s.piCreationCheckpoint = func(point string) error {
		if point == "tracking" {
			path := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := strings.Replace(string(data), "state: creating", "state: ready", 1)
			text = strings.Replace(text, "initial_goal_sent: false", "initial_goal_sent: true", 1)
			return os.WriteFile(path, []byte(text), 0644)
		}
		return nil
	}
	if err := s.initializePiCreation(r); err == nil {
		t.Fatal("forged ready projection was accepted")
	}
	if f.creates != 0 || f.sends != 0 {
		t.Fatalf("forged readiness caused native effects %d/%d", f.creates, f.sends)
	}
}
func TestPiCreationRechecksPackageAfterRuntimeLockWait(t *testing.T) {
	s, r, f := newPiCreationFixture(t)
	runtime, err := session.LockRuntimeExclusive(s.config.AuthorityDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.withPiCreationOwner(ctx, r, func(session.PiCreationIntent) error { f.creates++; return nil })
	}()
	deadline := time.Now().Add(time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		lock, err := session.LockCreation(s.config.AuthorityDir, "example")
		if err != nil {
			waiting = true
			break
		}
		lock.Close()
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		runtime.Close()
		t.Fatal("fixture did not reach runtime wait")
	}
	replacement := s.config.HostProfile + ".replacement"
	if err = os.Symlink(t.TempDir(), replacement); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	if err = <-done; err == nil {
		t.Fatal("waiter used superseded package")
	}
	if f.creates != 0 || f.sends != 0 {
		t.Fatal("stale waiter reached effects")
	}
}

func TestPiCreationReceiptRejectsIncompleteAndOutOfBoundsRequests(t *testing.T) {
	_, r, _ := newPiCreationFixture(t)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"incomplete-five-field-pi": func(raw map[string]any) { delete(raw["pi"].(map[string]any), "selection") },
		"legacy-source":            func(raw map[string]any) { raw["request"].(map[string]any)["source"] = "another" },
		"null-conversations":       func(raw map[string]any) { raw["pi"].(map[string]any)["conversations"] = nil },
		"fractional-attempt":       func(raw map[string]any) { raw["attempt"] = 1.5 },
		"oversized-original-goal": func(raw map[string]any) {
			raw["request"].(map[string]any)["goal"] = strings.Repeat(" ", session.MaxMessageBytes) + "x"
			raw["goal"] = "x"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			mutate(raw)
			bad, _ := json.Marshal(raw)
			var shape map[string]json.RawMessage
			json.Unmarshal(bad, &shape)
			if validateCreationReceiptShape(shape) == nil {
				t.Fatal("incomplete Pi receipt accepted")
			}
		})
	}
}

func TestPiCreationFreshAcceptanceDurablyFencesBeforeNativeCreate(t *testing.T) {
	s, original, f := newPiCreationFixture(t)
	if err := os.Remove(s.creationPath("example")); err != nil {
		t.Fatal(err)
	}
	s.creations = map[string]creationReceipt{}
	var observedReceipt, observedInput string
	f.beforeCreate = func() error {
		var saved creationReceipt
		if err := readCreationJSON(s.creationPath("example"), &saved); err != nil {
			return err
		}
		intent, err := saved.piIntent()
		if err != nil {
			return err
		}
		companion, err := readPiCreationIntent(s.creationEvidencePath(saved) + ".request")
		if err != nil {
			return err
		}
		journal, err := readPiCreationJournal(piJournalPath(s.config.Workspace, "example"))
		if err != nil {
			return err
		}
		binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load("example")
		if err != nil {
			return err
		}
		if saved.State != "running" || companion.ReceiptID != saved.ReceiptID || journal.ReceiptID != saved.ReceiptID || !journal.NativeCreateStarted || journal.NativeGeneration != strings.Repeat("8", 64) || journal.Native != nil || binding.CreationReceiptID != saved.ReceiptID || binding.Revision != 1 || binding.State != "creating" || intent.InitialInputID != companion.InitialInputID {
			return errors.New("native create preceded durable authority")
		}
		observedReceipt, observedInput = saved.ReceiptID, saved.Pi.InitialInputID
		return nil
	}
	accepted, err := s.acceptCreation(original.Request)
	if err != nil {
		t.Fatal(err)
	}
	s.operationWG.Wait()
	current := s.creations["example"]
	if current.State != "ready" {
		t.Fatalf("fresh creation failed: %s", current.Error)
	}
	if observedReceipt != accepted.ReceiptID || observedInput != accepted.Pi.InitialInputID || f.creates != 1 || f.sends != 1 {
		t.Fatalf("fresh effect fence failed: %s %s %d/%d", observedReceipt, observedInput, f.creates, f.sends)
	}
}
func TestPiCreationReceiptBoundariesSurviveLostPortalMemory(t *testing.T) {
	s, r, f := newPiCreationFixture(t)
	s.creations = nil
	if err := s.loadCreations(); err != nil {
		t.Fatal(err)
	}
	paused := s.creations["example"]
	if paused.State != "paused" || paused.ReceiptID != r.ReceiptID || paused.Pi.InitialInputID != r.Pi.InitialInputID || f.creates != 0 || f.sends != 0 {
		t.Fatal("receipt-only crash changed identity or invoked native code")
	}
	paused.Attempt++
	paused.State = "running"
	if err := s.saveCreation(paused); err != nil {
		t.Fatal(err)
	}
	s.creations["example"] = paused
	s.operationWG.Add(1)
	s.runCreation(paused)
	if s.creations["example"].State != "ready" {
		t.Fatalf("receipt recovery failed: %s", s.creations["example"].Error)
	}
	s.creations = nil
	if err := s.loadCreations(); err != nil {
		t.Fatal(err)
	}
	ready, exists := s.currentCreation("example")
	if !exists || ready.State != "ready" || ready.ReceiptID != r.ReceiptID || ready.Pi.InitialInputID != r.Pi.InitialInputID || f.creates != 1 || f.sends != 1 {
		t.Fatalf("lost ready ACK replayed effects: %#v %d/%d", ready, f.creates, f.sends)
	}
}
