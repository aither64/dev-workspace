package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

func piTeamPackageFixture(t *testing.T, s *Server) {
	t.Helper()
	root, err := filepath.EvalSymlinks(s.config.HostProfile)
	if err != nil {
		t.Fatal(err)
	}
	s.config.PackageRoot = root
	dir := filepath.Join(root, "share/workspace-portal")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"piTeamRecords": session.PiTeamRecords()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime-contract.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s.config.TransitionLock = filepath.Join(t.TempDir(), "transition.lock")
	if err := os.WriteFile(s.config.TransitionLock, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func piTeamAuthorityFixture(t *testing.T, s *Server, authority session.PiRuntimeAuthority) {
	t.Helper()
	data, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.config.AuthorityDir, authority.Slug+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPiTeamAdmissionInvocationExpiresAndRefusesIncompleteMap(t *testing.T) {
	s := newTestServer(t)
	piTeamPackageFixture(t, s)
	var escaped *piPackageInvocation
	called := 0
	sentinel := errors.New("callback error")
	err := s.withCurrentGenerationInvocation(func(p *piPackageInvocation) error {
		escaped = p
		return p.withWrite(context.Background(), func() error { called++; return sentinel })
	})
	if !errors.Is(err, sentinel) || called != 1 {
		t.Fatalf("callback: %d %v", called, err)
	}
	if err := escaped.withWrite(context.Background(), func() error { called++; return nil }); err == nil || called != 1 {
		t.Fatal("escaped invocation wrote")
	}
	for _, raw := range []string{`{}`, `{"piTeamRecords":null}`, `{"piTeamRecords":{"policy":1}}`, `{"piTeamRecords":{"policy":1,"roster":2,"monitorOperation":2,"communication":2,"progressObserver":2,"utilityReceipt":3}}`} {
		if err := os.WriteFile(filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		err := s.withCurrentGenerationInvocation(func(p *piPackageInvocation) error {
			return p.withWrite(context.Background(), func() error { called++; return nil })
		})
		if err == nil || called != 1 {
			t.Fatalf("incomplete map admitted: %s %v", raw, err)
		}
	}
	s.config.TransitionLock = ""
	if err := s.withCurrentGenerationInvocation(func(p *piPackageInvocation) error {
		return p.withWrite(context.Background(), func() error { called++; return nil })
	}); err == nil || called != 1 {
		t.Fatal("empty test lock certified production write")
	}
}

func TestPiTeamAdmissionCurrentAuthorityAndOriginalReadUseSharedOwnership(t *testing.T) {
	s, receipt, native := newPiCreationFixture(t)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	piTeamPackageFixture(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ref := native.submission.Ref
	input := monitoroperation.PiInputRecord{Conversation: monitoroperation.PiConversationRecord{Session: monitoroperation.PiSessionRecord{Workspace: ref.Conversation.Session.Workspace, Slug: ref.Conversation.Session.Slug, Incarnation: ref.Conversation.Session.Incarnation}, Backend: "pi", Member: ref.Conversation.Member, Incarnation: ref.Conversation.Incarnation, NativeID: ref.Conversation.NativeID}, OperationID: ref.OperationID, Action: ref.Action, Digest: ref.Digest}
	var snapshot piSessionAdmission
	if err := s.withPiSessionRead(ctx, ref.Conversation.Session, func(current piSessionAdmission) error { snapshot = current; return nil }); err != nil {
		t.Fatal(err)
	}
	// A newer current generation must not rewrite the saved receipt generation
	// or route the reader through idle/effect/native-verification admission.
	original := snapshot.Authority.Generation
	snapshot.Authority.Generation = strings.Repeat("9", 64)
	piTeamAuthorityFixture(t, s, snapshot.Authority)
	native.submission = nil // Original Lookup can remain held; host read still admits.
	called := 0
	err := s.WithPiTeamWrite(ctx, ref.Conversation.Session, snapshot.Authority.Generation, func() error {
		callback := make(chan error, 1)
		go func() { callback <- s.WithPiInputRead(ctx, input, func() error { called++; return nil }) }()
		select {
		case err := <-callback:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil || called != 1 {
		t.Fatalf("early callback read deadlocked/refused: %d %v", called, err)
	}
	utility := monitoroperation.PiUtilityRecord{Session: input.Conversation.Session, OperationID: submissionValidationID, Incarnation: strings.Repeat("9", 64), NativeID: "original-utility"}
	if err := s.WithPiUtilityRead(ctx, utility, submissionValidationID, original, func() error { called++; return nil }); err != nil || called != 2 {
		t.Fatalf("original utility read: %d %v", called, err)
	}
	uRef := piruntime.UtilitySubmissionRef{Utility: utility.Ref(), OperationID: submissionValidationID, Action: "send", Digest: strings.Repeat("a", 64)}
	runtime := native.runtime
	runtime.Generation = snapshot.Authority.Generation
	if err := s.WithUtilityRead(ctx, runtime, uRef, func() error { called++; return nil }); err != nil || called != 3 {
		t.Fatalf("current authority/original full ref: %v", err)
	}
	runtime.Generation = original
	if err := s.WithUtilityRead(ctx, runtime, uRef, func() error { called++; return nil }); err == nil || called != 3 {
		t.Fatal("historical generation replaced current authority")
	}
	if err := s.WithPiTeamWrite(ctx, ref.Conversation.Session, original, func() error { called++; return nil }); err == nil || called != 3 {
		t.Fatal("stale record write")
	}
	wrong := input
	wrong.Conversation.NativeID = "other"
	if err := s.WithPiInputRead(ctx, wrong, func() error { called++; return nil }); err == nil || called != 3 {
		t.Fatal("wrong original conversation admitted")
	}
	if native.creates != 1 || native.sends != 1 {
		t.Fatal("read admission performed native work")
	}
	// No historical lead Access is invented when its catalog is unavailable.
	if err := s.withPiConversationMutation(ctx, ref.Conversation, func(piConversationAdmission) error { called++; return nil }); err == nil || called != 3 {
		t.Fatal("missing saved access accepted")
	}
}

func TestPiTeamAdmissionRechecksGenerationAfterRuntimeWait(t *testing.T) {
	s, receipt, native := newPiCreationFixture(t)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	piTeamPackageFixture(t, s)
	ref := native.submission.Ref.Conversation.Session
	authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, ref.Slug, ref.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := session.LockRuntimeExclusive(s.config.AuthorityDir, ref.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	called := false
	go func() {
		done <- s.WithPiTeamWrite(ctx, ref, authority.Generation, func() error { called = true; return nil })
	}()
	probe, err := os.OpenFile(s.config.TransitionLock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	for {
		err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = unix.Flock(int(probe.Fd()), unix.LOCK_UN)
		select {
		case <-ctx.Done():
			t.Fatal("writer never acquired outer transition")
		case <-time.After(time.Millisecond):
		}
	}
	authority.Generation = strings.Repeat("7", 64)
	piTeamAuthorityFixture(t, s, authority)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || called {
		t.Fatalf("stale after wait: %v %v", called, err)
	}
}

func TestPiTeamAdmissionRetiredBindingLegacyHTTPAndDispatchPreservePiArms(t *testing.T) {
	s, client, _, oldService, caller, messageID := communicationActivityFixture(t)
	piTeamPackageFixture(t, s)
	statePath := filepath.Join(s.config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var mixed communication.PiCommunicationState
	if err := json.Unmarshal(data, &mixed); err != nil {
		t.Fatal(err)
	}
	mixed.Schema = 2
	now := time.Now().UTC()
	endpoint := func(slug, digit string) communication.EndpointRecord {
		return communication.EndpointRecord{Pi: &communication.PiEndpoint{Backend: "pi", Session: monitoroperation.PiSessionRecord{Workspace: s.config.Workspace, Slug: slug, Incarnation: strings.Repeat(digit, 64)}, LeadIncarnation: strings.Repeat(digit, 64), NativeID: "native-" + slug}}
	}
	piGrant := communication.PiGrantRecord{ID: strings.Repeat("e", 32), A: endpoint("retired-a", "a"), B: endpoint("retired-b", "b"), ApprovedBy: "retired-a", Approval: "retained approval", CreatedAt: now, RevokedAt: &now, Reason: "retired"}
	mixed.Grants = append(mixed.Grants, piGrant)
	if err := mixed.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	// There is deliberately no current Pi binding/authority. The installed
	// retained-fence scanner owns registration proof, not this Codex invocation.
	if err := oldService.Dispatch(context.Background(), messageID); err == nil || client.sends != 0 {
		t.Fatal("unadmitted startup service wrote schema2")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/sender/communication", strings.NewReader(`{"action":"allow","peer":"receiver"}`))
	s.sessionAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP admitted legacy mutation: %d %s", w.Code, w.Body)
	}
	var escaped communication.Service
	if err := s.withCurrentGenerationInvocation(func(p *piPackageInvocation) error {
		var err error
		escaped, err = s.communicationService(p)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatchCommunication(context.Background(), messageID); err != nil {
		t.Fatal(err)
	}
	if client.sends != 1 {
		t.Fatalf("dispatch effects: %d", client.sends)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := escaped.Expire(context.Background(), caller.Slug); err == nil {
		t.Fatal("expired invocation wrote aggregate")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("refusal changed bytes")
	}
	current, err := communication.DecodePiCommunication(after)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, grant := range current.Grants {
		if grant.ID == piGrant.ID {
			found = true
			if !reflect.DeepEqual(grant, piGrant) {
				t.Fatalf("legacy mutation changed retained Pi arm: %#v != %#v", grant, piGrant)
			}
		}
	}
	if !found {
		t.Fatal("legacy mutation removed retained Pi arm")
	}
}

func TestPiTeamAdmissionSavedAccessAndDetachedReadyProof(t *testing.T) {
	f, _ := newMemberPublicationFixture(t)
	s := f.server
	result, err := s.PublishMemberReady(context.Background(), 2, f.handle, f.proof.CreationID, f.admission.Submission.OperationID)
	if err != nil || !result.Complete {
		t.Fatalf("member fixture: %v", err)
	}
	piTeamPackageFixture(t, s)
	var current piSessionAdmission
	if err := s.withPiSessionRead(context.Background(), f.handle.Conversation.Session, func(value piSessionAdmission) error { current = value; return nil }); err != nil {
		t.Fatal(err)
	}
	lead, err := current.Binding.Resolve("lead")
	if err != nil {
		t.Fatal(err)
	}
	team := agentteams.Team{TeamDigest: current.intent.DirectTeam.TeamDigest, Roles: map[string]agentteams.Role{"team_lead": {Access: "read_only", Model: current.intent.Model, Effort: current.intent.Effort, Instructions: current.intent.DirectTeam.LeadInstructions}}}
	s.installedTeams = &agentteams.Installed{Catalog: &agentteams.Catalog{CatalogDigest: current.intent.DirectTeam.CatalogDigest, Teams: map[string]agentteams.Team{current.intent.DirectTeam.ID: team}}}
	called := 0
	if err := s.withPiConversationMutation(context.Background(), lead, func(value piConversationAdmission) error {
		called++
		if value.SavedAccess != "read_only" || value.Handle.Conversation != lead || value.Handle.Generation != current.Authority.Generation || value.BindingRevision != current.Binding.Revision {
			t.Fatal("saved access/current handle changed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		lock, err := session.LockRuntimeSharedContext(ctx, s.config.AuthorityDir, lead.Session.Slug)
		if lock != nil {
			_ = lock.Close()
			t.Fatal("conversation mutation did not hold EX")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("EX exclusion: %v", err)
		}
		return nil
	}); err != nil || called != 1 {
		t.Fatalf("historical lead access: %v", err)
	}
	s.installedTeams.Catalog.CatalogDigest = strings.Repeat("f", 64)
	if err := s.withPiConversationMutation(context.Background(), lead, func(piConversationAdmission) error { called++; return nil }); err == nil || called != 1 {
		t.Fatal("new catalog filled historical access")
	}
	proof := piRosterReadyProof{Handle: f.handle, CreationID: f.proof.CreationID, OperationID: f.admission.Submission.OperationID, Expected: f.admission, Proof: f.proof}
	admission, err := newPiRosterWriteAdmission(s, current, []piRosterReadyProof{proof})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	roster := teamruntime.PiRoster{Schema: 2, Workspace: s.config.Workspace, Slug: "example", Revision: 1, CreatedAt: now, UpdatedAt: now, Backend: "pi", BackendContract: 1, SessionIncarnation: lead.Session.Incarnation, LeadIncarnation: lead.Incarnation,
		Members: []teamruntime.PiRosterMember{{Address: "implementer0", Role: "implementer", Index: 0, Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: now, Incarnation: f.handle.Conversation.Incarnation, CreationID: f.proof.CreationID, InitialInputID: f.admission.Submission.OperationID}}}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	f.proof.Bootstrap.Witness.EntryID = "" // Must not mutate captured native proof.
	if err := store.SavePi(context.Background(), admission, current.Authority.Generation, 0, roster, admission); err != nil {
		t.Fatal("detached ready publication", err)
	}
	if err := admission.VerifyPiRosterReady(context.Background(), roster); err == nil {
		t.Fatal("ready verifier escaped its write invocation")
	}
	f.proof.Bootstrap.Witness.EntryID = "entry"
	stale, err := newPiRosterWriteAdmission(s, current, []piRosterReadyProof{proof})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.config.UserStateRoot, "codex-teams", filepath.Base(s.config.Workspace)+"-"+fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace)))[:16], "example.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next := current.Authority
	next.Generation = strings.Repeat("6", 64)
	piTeamAuthorityFixture(t, s, next)
	roster.Revision++
	if err := store.SavePi(context.Background(), stale, current.Authority.Generation, 1, roster, stale); err == nil {
		t.Fatal("stale native ready proof published")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("stale ready refusal changed roster")
	}
	if err := s.WithPiTeamWrite(context.Background(), agentbackend.SessionRef{Workspace: s.config.Workspace, Slug: "example", Incarnation: strings.Repeat("0", 64)}, next.Generation, func() error { called++; return nil }); err == nil || called != 1 {
		t.Fatal("wrong reserved session admitted")
	}
}

func TestPiTeamAdmissionSchemaUpgradeWhileLegacyWriterWaitsRefuses(t *testing.T) {
	s, client, _, service, caller, _ := communicationActivityFixture(t)
	piTeamPackageFixture(t, s)
	dir := filepath.Join(s.config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))))
	owner, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	called := false
	service.Store.Schema2Write = func(context.Context, func() error) error {
		called = true
		return errors.New("unexpected schema2 pre-admission")
	}
	done := make(chan error, 1)
	go func() { done <- service.Expire(ctx, caller.Slug) }()
	// Wait for the owning store's lock loop, without a production test hook or
	// a sleep that guesses whether the schema-1 pre-read already completed.
	for {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		waiting := false
		for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(goroutine, "[select]") && strings.Contains(goroutine, "communication.(*Store).lockedAdmitted") {
				waiting = true
			}
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("legacy writer never reached aggregate lock wait")
		case <-time.After(time.Millisecond):
		}
	}
	path := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["schema"] = json.RawMessage("2")
	upgraded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := communication.ValidateCommunicationRecord(upgraded, s.config.Workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, upgraded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || called {
		t.Fatalf("schema race bypassed under-lock check: called=%v err=%v", called, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(upgraded) || client.sends != 0 {
		t.Fatal("schema race changed bytes/native state")
	}
}

type piTeamReadFunc func(context.Context, monitoroperation.PiInputRecord, func() error) error

func (f piTeamReadFunc) WithPiInputRead(ctx context.Context, input monitoroperation.PiInputRecord, read func() error) error {
	return f(ctx, input, read)
}

type piTeamLookupFunc func(context.Context, piruntime.SubmissionRef) (piruntime.SubmissionStatus, error)

func (f piTeamLookupFunc) Lookup(ctx context.Context, ref piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	return f(ctx, ref)
}

func TestPiTeamAdmissionReconciliationReleasesReadThenRechecksRecord(t *testing.T) {
	s, receipt, native := newPiCreationFixture(t)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	piTeamPackageFixture(t, s)
	ref := native.submission.Ref.Conversation
	scope := monitoroperation.PiSessionRecord{Workspace: ref.Session.Workspace, Slug: ref.Session.Slug, Incarnation: ref.Session.Incarnation}
	store, err := communication.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	store.Schema2Write = func(context.Context, func() error) error {
		t.Fatal("Pi UpdatePi nested legacy admission")
		return piruntime.ErrConflict
	}
	settings := native.submission.Settings
	input := monitoroperation.PiInputRecord{Conversation: monitoroperation.PiConversationRecord{Session: scope, Backend: "pi", Member: ref.Member, Incarnation: ref.Incarnation, NativeID: ref.NativeID}, OperationID: submissionValidationID, Action: "send"}
	now := time.Now().UTC()
	grantID := strings.Repeat("f", 32)
	wireInput := piruntime.SendInput{Message: "original frozen envelope", ActionContext: "communication:" + grantID}
	originalRef := input.Ref()
	originalRef.Digest = piruntime.SubmissionDigest(originalRef, wireInput, settings)
	input.Digest = originalRef.Digest
	from := communication.EndpointRecord{Codex: &communication.Identity{Slug: "other", Thread: "other-native", Incarnation: "other-incarnation"}}
	to := communication.EndpointRecord{Pi: &communication.PiEndpoint{Backend: "pi", Session: scope, LeadIncarnation: ref.Incarnation, NativeID: ref.NativeID}}
	message := communication.PiMessageRecord{ID: input.OperationID, GrantID: grantID, From: from, To: to, Text: "original message", CreatedAt: now, Status: "submitting", Envelope: wireInput.Message,
		PiDelivery: &monitoroperation.PiDeliveryRecord{Settings: monitoroperation.PiSettingsRecord{Provider: settings.Provider, AccountRef: settings.AccountRef, Model: settings.Model, Thinking: settings.Thinking, PolicyRevision: settings.PolicyRevision, Revision: settings.Revision}, Input: input, Generation: native.runtime.Generation}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := store.UpdatePi(ctx, s, scope, native.runtime.Generation, func(state *communication.PiCommunicationState) error {
		state.Grants = append(state.Grants, communication.PiGrantRecord{ID: grantID, A: from, B: to, ApprovedBy: "other", Approval: "retained exact pair", CreatedAt: now})
		state.Messages = append(state.Messages, message)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	lookup := piTeamLookupFunc(func(_ context.Context, saved piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
		lookups++
		if saved != originalRef {
			t.Fatal("original ref replaced")
		}
		return piruntime.SubmissionStatus{Schema: 1, Ref: saved, Phase: "Unknown", Settings: settings, Input: wireInput, Generation: native.runtime.Generation}, nil
	})
	reader := piTeamReadFunc(func(ctx context.Context, original monitoroperation.PiInputRecord, read func() error) error {
		if err := s.WithPiInputRead(ctx, original, read); err != nil {
			return err
		}
		// Read ownership has ended. A configuration/publication owner can acquire
		// EX here; this would time out if reconciliation retained its SH gate.
		exclusive, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, scope.Slug)
		if err != nil {
			return err
		}
		_ = exclusive.Close()
		return store.UpdatePi(ctx, s, scope, native.runtime.Generation, func(state *communication.PiCommunicationState) error {
			state.Messages[0].Detail = "changed after read"
			return nil
		})
	})
	if err := store.ReconcilePiMessage(ctx, s, reader, scope, native.runtime.Generation, input.OperationID, lookup); !errors.Is(err, piruntime.ErrConflict) || lookups != 1 {
		t.Fatalf("stale reconciliation: lookups=%d err=%v", lookups, err)
	}
	path := filepath.Join(s.config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := communication.DecodePiCommunication(data)
	if err != nil {
		t.Fatal(err)
	}
	if state.Messages[0].Detail != "changed after read" || state.Messages[0].PiDelivery.Input != input || state.Messages[0].PiDelivery.Outcome != "" {
		t.Fatal("stale reconciliation overwrote current tuple")
	}
	if native.creates != 1 || native.sends != 1 {
		t.Fatal("lookup attempted another effect")
	}
}
