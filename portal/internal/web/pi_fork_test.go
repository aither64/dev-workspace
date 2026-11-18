package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// Reuse the existing isolated creation fixture. These are typed fake native
// ports, not a second SDK/provider case or evidence of actual native effects.
type piForkRuntimeFixture struct {
	*piCreationRuntimeFixture
	source                       piruntime.RuntimeRef
	sources                      map[string]piruntime.ForkSnapshot
	copies                       map[agentbackend.ConversationRef]piruntime.ForkProof
	starts                       map[agentbackend.SessionRef]piruntime.StartRequest
	root                         string
	forkCalls                    int
	lostReply, sourceUnavailable bool
	captureError                 map[string]error
	assignment                   *piCreationRuntimeFixture
	recoverError                 error
	requireRecovery              bool
}

func (f *piForkRuntimeFixture) ReadCurrentRuntime(ctx context.Context, ref agentbackend.SessionRef) (piruntime.RuntimeRef, error) {
	if ref == f.source.Session {
		if f.sourceUnavailable {
			return piruntime.RuntimeRef{}, piruntime.ErrUnavailable
		}
		return f.source, nil
	}
	return f.piCreationRuntimeFixture.ReadCurrentRuntime(ctx, ref)
}
func (f *piForkRuntimeFixture) ReadStart(ctx context.Context, ref agentbackend.SessionRef) (piruntime.StartRequest, error) {
	if start, ok := f.starts[ref]; ok {
		return start, nil
	}
	return piruntime.StartRequest{}, os.ErrNotExist
}
func (f *piForkRuntimeFixture) Ensure(_ context.Context, start piruntime.StartRequest) (piruntime.RuntimeStatus, error) {
	if old, ok := f.starts[start.Session]; ok && old != start {
		return piruntime.RuntimeStatus{}, piruntime.ErrConflict
	}
	f.starts[start.Session] = start
	f.runtime = piruntime.RuntimeRef{Session: start.Session, RequestID: start.RequestID, InstanceID: "66666666-6666-4666-8666-666666666666", Digest: strings.Repeat("6", 64), Generation: strings.Repeat("7", 64)}
	root, e := piruntime.SessionStateDirectory(f.root, start.Session)
	if e != nil {
		return piruntime.RuntimeStatus{}, e
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		return piruntime.RuntimeStatus{}, e
	}
	return piruntime.RuntimeStatus{Runtime: f.runtime, Phase: "ready", Observation: piruntime.HostObservation{Outcome: "running", Phase: "running"}}, nil
}
func (f *piForkRuntimeFixture) CaptureFork(_ context.Context, h agentbackend.ExecutionHandle) (piruntime.ForkSnapshot, error) {
	if e := f.captureError[h.Conversation.Member]; e != nil {
		return piruntime.ForkSnapshot{}, e
	}
	if f.sourceUnavailable || h.Generation != f.source.Generation {
		return piruntime.ForkSnapshot{}, piruntime.ErrHeld
	}
	x, ok := f.sources[h.Conversation.Member]
	if !ok {
		return x, piruntime.ErrHeld
	}
	return x, nil
}
func (f *piForkRuntimeFixture) Fork(_ context.Context, runtime piruntime.RuntimeRef, r piruntime.ForkRequest) (piruntime.ForkProof, error) {
	ref := agentbackend.ConversationRef{Session: r.Destination.Session, Backend: agentbackend.Pi, Member: r.Destination.Member, Incarnation: r.Destination.Incarnation, NativeID: r.NativeID}
	if p, ok := f.copies[ref]; ok {
		return p, nil
	}
	if f.sourceUnavailable || !reflect.DeepEqual(f.sources[r.Destination.Member], r.Snapshot) {
		return piruntime.ForkProof{}, piruntime.ErrConflict
	}
	f.forkCalls++
	p := piruntime.ForkProof{Conversation: ref, Generation: runtime.Generation, CreationID: r.Destination.CreationID, HistoryRef: strings.Repeat("f", 64), Origin: piruntime.ForkOrigin{Source: r.Source, Snapshot: r.Snapshot}, Persisted: true, Settings: r.Snapshot.Settings}
	if r.Snapshot.Instructions != "" {
		p.InstructionsDigest = piruntime.MessageDigest(r.Snapshot.Instructions)
	}
	if policy := r.Snapshot.ToolPolicy; policy != nil {
		p.ToolPolicy = &piruntime.ToolPolicyProof{Policy: *policy, Generation: runtime.Generation, Active: policy.Active, Callable: policy.Active, Registered: policy.RegisteredCeiling, Applied: true}
	}
	f.copies[ref] = p
	if f.lostReply {
		f.lostReply = false
		return piruntime.ForkProof{}, errors.New("lost copy reply")
	}
	return p, nil
}
func (f *piForkRuntimeFixture) RecoverFork(_ context.Context, runtime piruntime.RuntimeRef, r piruntime.ForkRequest) (piruntime.ForkProof, bool, error) {
	if f.recoverError != nil {
		return piruntime.ForkProof{}, false, f.recoverError
	}
	ref := agentbackend.ConversationRef{Session: r.Destination.Session, Backend: agentbackend.Pi, Member: r.Destination.Member, Incarnation: r.Destination.Incarnation, NativeID: r.NativeID}
	p, ok := f.copies[ref]
	if ok {
		f.requireRecovery = false
	}
	return p, ok, nil
}
func (f *piForkRuntimeFixture) VerifyFork(_ context.Context, h agentbackend.ExecutionHandle, id string, o piruntime.ForkOrigin) (piruntime.ForkProof, error) {
	if f.requireRecovery {
		return piruntime.ForkProof{}, piruntime.ErrHeld
	}
	p, ok := f.copies[h.Conversation]
	if !ok {
		return p, piruntime.ErrUnavailable
	}
	if p.CreationID != id || p.Generation != h.Generation || !reflect.DeepEqual(p.Origin, o) {
		return p, piruntime.ErrConflict
	}
	return p, nil
}
func (f *piForkRuntimeFixture) ReadMemberAdmission(_ context.Context, ref agentbackend.ConversationRef, creationID, inputID string) (piruntime.ConversationState, piruntime.SubmissionStatus, error) {
	if a := f.assignment; a != nil && a.state.Conversation.Session == ref.Session && a.state.Conversation.Member == ref.Member && a.state.Conversation.Incarnation == ref.Incarnation {
		if a.submission == nil || a.submission.Ref.OperationID != inputID {
			return a.state, piruntime.SubmissionStatus{}, os.ErrNotExist
		}
		return a.state, *a.submission, nil
	}
	return piruntime.ConversationState{}, piruntime.SubmissionStatus{}, os.ErrNotExist
}
func (f *piForkRuntimeFixture) VerifyCreation(ctx context.Context, h agentbackend.ExecutionHandle, id, history string, original *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	if a := f.assignment; a != nil && a.state.Conversation == h.Conversation {
		return a.VerifyCreation(ctx, h, id, history, original)
	}
	return f.piCreationRuntimeFixture.VerifyCreation(ctx, h, id, history, original)
}
func newPiForkFixture(t *testing.T, team bool) (*Server, creationReceipt, *piForkRuntimeFixture) {
	t.Helper()
	preset := piCreationSoloPreset()
	if team {
		preset = historicalPiTeamPreset()
		preset.Members = append(preset.Members, teamruntime.MemberSpec{Role: "reviewer", Address: "reviewer0", Model: "model", Effort: "high", Behavior: "reviewer", Access: "read_only"})
		preset.Roles = append(preset.Roles, "reviewer0", "architect0")
		preset.Members = append(preset.Members, teamruntime.MemberSpec{Role: "architect", Address: "architect0", Model: "model", Effort: "high", Behavior: "designer", Access: "read_only"})
	}
	s, source, base := newHistoricalPiCreationFixtureForSlug(t, "2026-11-18-source", preset)
	native := &piForkRuntimeFixture{piCreationRuntimeFixture: base, source: base.runtime, sources: map[string]piruntime.ForkSnapshot{}, copies: map[agentbackend.ConversationRef]piruntime.ForkProof{}, starts: map[agentbackend.SessionRef]piruntime.StartRequest{}, root: s.config.UserStateRoot, captureError: map[string]error{}}
	s.config.PiCreation.Runtime = native
	s.config.PiCreation.StartRequest = func(i session.PiCreationIntent, _ uint64) (piruntime.StartRequest, error) {
		if start, ok := native.starts[i.SessionRef()]; ok {
			return start, nil
		}
		return piruntime.StartRequest{Session: i.SessionRef(), BindingRevision: 1, RequestID: i.RuntimeRequestID(), AppStorePath: s.config.PackageRoot, ProfilePath: s.config.HostProfile, ProfileGeneration: piProfileToken(s.hostProfile)}, nil
	}
	path := filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var contract map[string]any
	_ = json.Unmarshal(data, &contract)
	contract["piFork"] = 1
	contract["piCreationSchema"] = 5
	data, _ = json.Marshal(contract)
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	intent, e := source.piIntent()
	if e != nil {
		t.Fatal(e)
	}
	bindings := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	b, e := bindings.Load(source.Request.Slug)
	if e != nil {
		t.Fatal(e)
	}
	if team {
		if e = s.reservePiCreationRoster(context.Background(), intent, b, base.runtime.Generation); e != nil {
			t.Fatal(e)
		}
		rosters, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		roster, e := rosters.LoadPi(intent.Slug)
		if e != nil {
			t.Fatal(e)
		}
		b.Conversations[2].State = "ready"
		b.Conversations[2].NativeID = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
		b.Revision++
		b, e = bindings.CompareAndSwap(context.Background(), b.Revision-1, b)
		if e != nil {
			t.Fatal(e)
		}
		roster.Members[1].State = "ready"
		roster.Members[1].InitialInputID = "22222222-2222-4222-8222-222222222222"
		removed := time.Now().UTC()
		roster.Members[2].State = "removed"
		roster.Members[2].RemovedAt = &removed
		roster.Revision++
		sum := sha256.Sum256([]byte(s.config.Workspace))
		key := filepath.Base(s.config.Workspace) + "-" + hex.EncodeToString(sum[:])[:16]
		if e = monitoroperation.WritePiRecord(filepath.Join(s.config.UserStateRoot, "codex-teams", key, intent.Slug+".json"), roster, 256<<10); e != nil {
			t.Fatal(e)
		}
		a, e := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, intent.Slug, intent.Workspace)
		if e != nil {
			t.Fatal(e)
		}
		a.BindingRevision = b.Revision
		if e = session.PublishPiRuntimeAuthority(s.config.AuthorityDir, b, a); e != nil {
			t.Fatal(e)
		}
	}
	for _, bound := range b.Conversations {
		if bound.State != "ready" {
			continue
		}
		policy, e := s.initialPiWorkflowPolicy(intent, bound.Address)
		if e != nil {
			t.Fatal(e)
		}
		settings, ok := intent.InitialMemberSettings(bound.Address)
		if !ok {
			t.Fatal("missing source selection")
		}
		instructions := intent.DirectTeam.LeadInstructions
		for _, m := range intent.DirectTeam.Members {
			if m.Address == bound.Address {
				instructions = m.Instructions
			}
		}
		native.sources[bound.Address] = piruntime.ForkSnapshot{Native: piruntime.NativeForkSnapshot{SourceFile: filepath.Join(s.config.UserStateRoot, bound.Address+".jsonl"), SourceCwd: filepath.Join(intent.Workspace, "work", intent.Slug), SourceNativeID: bound.NativeID, EntriesDigest: strings.Repeat("b", 64), EntryCount: 3, LeafID: "frozen-leaf"}, HistoryRef: strings.Repeat("9", 64), Settings: settings, Instructions: instructions, ToolPolicy: policy}
	}
	return s, source, native
}
func frozenPiForkReceipt(t *testing.T, s *Server, source creationReceipt, selected string) creationReceipt {
	t.Helper()
	request := creationRequest{Backend: "pi", Kind: "fork", Source: source.Request.Slug, SelectedMember: selected, Slug: "2026-11-18-fork", Sensitive: source.Request.Sensitive}
	i, p, e := s.capturePiFork(context.Background(), request, strings.Repeat("1", 64), strings.Repeat("2", 64))
	if e != nil {
		t.Fatal(e)
	}
	i.DeletionHistorySHA256, e = session.CompletedRemovalHistory(i.Workspace, i.Slug, s.config.UserStateRoot)
	if e != nil {
		t.Fatal(e)
	}
	if e = i.Validate(s.config.PiCreation.Catalog); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := creationReceipt{Schema: 5, Workspace: i.Workspace, Request: request, ReceiptID: i.ReceiptID, DeletionHistorySHA256: i.DeletionHistorySHA256, Attempt: 1, State: "running", Phase: "Preparing independent Pi fork", StartedAt: now, UpdatedAt: now, Validated: true, Model: i.Model, Effort: i.Effort, DirectTeam: p, Pi: &piCreationIdentity{BackendContract: 1, SessionIncarnation: i.SessionIncarnation, Conversations: i.Conversations, Provider: i.Provider, Selection: i.ProviderSelection, Fork: i.Fork}}
	if e = s.saveCreation(r); e != nil {
		t.Fatal(e)
	}
	s.creations[r.Request.Slug] = r
	return r
}
func TestPiForkSoloNoGoalAndLostReplyDestinationRecovery(t *testing.T) {
	s, source, native := newPiForkFixture(t, false)
	original, _ := os.ReadFile(s.creationEvidencePath(source) + ".request")
	r := frozenPiForkReceipt(t, s, source, "lead")
	native.lostReply = true
	if e := s.initializePiFork(r); e == nil || !strings.Contains(e.Error(), "lost copy reply") {
		t.Fatal("lost reply", e)
	}
	b, e := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(r.Request.Slug)
	if e != nil || b.State != "creating" {
		t.Fatal("partial published ready", b, e)
	}
	native.sourceUnavailable = true
	native.requireRecovery = true
	if e = s.initializePiFork(r); e != nil {
		t.Fatal("destination-only recovery", e)
	}
	if native.forkCalls != 1 || native.creates != 0 || native.sends != 0 {
		t.Fatal("copy replayed or bootstrap sent", native.forkCalls, native.creates, native.sends)
	}
	summary, e := session.Find(s.config.Workspace, r.Request.Slug)
	if e != nil || summary.Schema != 5 || summary.Creation.InitialGoalSent || summary.Creation.GoalSHA256 != "" || summary.Codex.ThreadID != "" {
		t.Fatal(summary, e)
	}
	if e = s.provePiCreation(r); e != nil {
		t.Fatal("fork readiness", e)
	}
	after, _ := os.ReadFile(s.creationEvidencePath(source) + ".request")
	if string(after) != string(original) {
		t.Fatal("source changed")
	}
}
func TestPiForkWholeTeamSelectedReadonlyAndUnstartedReservation(t *testing.T) {
	s, source, native := newPiForkFixture(t, true)
	r := frozenPiForkReceipt(t, s, source, "reviewer0")
	if len(r.Pi.Selection.Policy.Overrides) != 0 {
		t.Fatal("fork turned inherited defaults into overrides")
	}
	if !strings.HasSuffix(r.status().URL, "?member=reviewer0") {
		t.Fatal(r.status())
	}
	if e := s.initializePiFork(r); e != nil {
		t.Fatal(e)
	}
	rosters, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	roster, e := rosters.LoadPi(r.Request.Slug)
	if e != nil {
		t.Fatal(e)
	}
	if roster.Schema != 4 || len(roster.Members) != 3 || roster.Members[2].State != "removed" || roster.Members[2].RemovedAt == nil || roster.Members[2].Fork != nil || roster.Members[0].State != "creating" || roster.Members[0].InitialInputID != "" || roster.Members[0].Fork != nil || roster.Members[1].State != "ready" || roster.Members[1].InitialInputID != "" || roster.Members[1].Fork == nil || roster.Members[1].Access != "read_only" || native.forkCalls != 2 {
		t.Fatalf("roster %+v copies %d", roster, native.forkCalls)
	}
	if e = s.provePiCreation(r); e != nil {
		t.Fatal(e)
	}
	intent, _ := r.piIntent()
	if p, e := s.initialPiWorkflowPolicy(intent, "implementer0"); e != nil || p == nil {
		t.Fatal("first assignment policy", e)
	}
	ref := native.source.Session
	if e = s.withPiSessionRead(context.Background(), intent.SessionRef(), func(current piSessionAdmission) error {
		m, e := current.Binding.Resolve("reviewer0")
		if e != nil {
			return e
		}
		access, e := s.piSavedAccessLocked(current, m)
		if e != nil {
			return e
		}
		if access != "read_only" {
			t.Fatal(access)
		}
		if m.Session == ref || m.NativeID == native.sources["reviewer0"].Native.SourceNativeID {
			t.Fatal("identity copied")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestPiForkBusyWholeTeamRefusesBeforeDestination(t *testing.T) {
	s, source, native := newPiForkFixture(t, true)
	native.captureError["reviewer0"] = piruntime.ErrHeld
	request := creationRequest{Backend: "pi", Kind: "fork", Source: source.Request.Slug, SelectedMember: "lead", Slug: "2026-11-18-refused"}
	if _, e := s.acceptCreation(request); e == nil || !strings.Contains(e.Error(), "reviewer0") {
		t.Fatal(e)
	}
	for _, p := range []string{s.creationPath(request.Slug), piJournalPath(s.config.Workspace, request.Slug), filepath.Join(s.config.Workspace, "work", request.Slug)} {
		if _, e := os.Stat(p); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("destination effect before preflight", p, e)
		}
	}
	if native.forkCalls != 0 {
		t.Fatal("copy before all-member preflight")
	}
}
func TestPiForkHTTPRejectsOverridesAndPreservesSelectedScope(t *testing.T) {
	s, source, _ := newPiForkFixture(t, true)
	r := frozenPiForkReceipt(t, s, source, "reviewer0")
	b, e := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(source.Request.Slug)
	if e != nil {
		t.Fatal(e)
	}
	ref, _ := b.Resolve("reviewer0")
	summary, e := session.Find(s.config.Workspace, source.Request.Slug)
	if e != nil {
		t.Fatal(e)
	}
	for _, overrides := range []string{`,"model":"model"`, `,"provider":"host"`, `,"reasoningEffort":"high"`, ""} {
		request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+source.Request.Slug+"/fork?member=reviewer0", strings.NewReader(`{"name":"fork","creationDate":"2026-11-18"`+overrides+`}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(ref))
		response := httptest.NewRecorder()
		s.piForkAPI(response, request, summary)
		if overrides != "" {
			if response.Code != http.StatusBadRequest {
				t.Fatal(response.Code, response.Body.String())
			}
		} else if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), r.ReceiptID) || !strings.Contains(response.Body.String(), "?member=reviewer0") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func TestPiForkRecoveryErrorNeverFallsThroughToSourceCopy(t *testing.T) {
	s, source, native := newPiForkFixture(t, false)
	r := frozenPiForkReceipt(t, s, source, "lead")
	native.recoverError = errors.New("partial destination held")
	if e := s.initializePiFork(r); e == nil || !strings.Contains(e.Error(), "partial destination held") {
		t.Fatal(e)
	}
	if native.forkCalls != 0 {
		t.Fatal("recovery error started copy")
	}
}

func TestPiForkAbsentDestinationRequiresSourceAndProofMismatchRefuses(t *testing.T) {
	for _, kind := range []string{"absent", "mismatch"} {
		t.Run(kind, func(t *testing.T) {
			s, source, native := newPiForkFixture(t, false)
			r := frozenPiForkReceipt(t, s, source, "lead")
			if kind == "mismatch" {
				native.lostReply = true
				if e := s.initializePiFork(r); e == nil {
					t.Fatal("expected lost reply")
				}
				for ref, p := range native.copies {
					p.Origin.Snapshot.Native.EntriesDigest = strings.Repeat("0", 64)
					native.copies[ref] = p
				}
				native.requireRecovery = true
			}
			native.sourceUnavailable = true
			calls := native.forkCalls
			if e := s.initializePiFork(r); e == nil {
				t.Fatal("unproved destination admitted")
			}
			if native.forkCalls != calls {
				t.Fatal("missing authority or mismatched proof copied")
			}
		})
	}
}

func TestPiForkDialogInheritsSettingsAndSelectedRedirect(t *testing.T) {
	node, e := exec.LookPath("node")
	if e != nil {
		t.Fatal(e)
	}
	if out, e := exec.Command(node, "pi_fork_contract_test.cjs").CombinedOutput(); e != nil {
		t.Fatalf("%v\n%s", e, out)
	}
}

func TestPiForkHydratedSelectedMember(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 for owning Fork hydration")
	}
	s, source, _ := newPiForkFixture(t, true)
	r := frozenPiForkReceipt(t, s, source, "reviewer0")
	summary, e := session.Find(s.config.Workspace, source.Request.Slug)
	if e != nil {
		t.Fatal(e)
	}
	b, e := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(source.Request.Slug)
	if e != nil {
		t.Fatal(e)
	}
	ref, _ := b.Resolve("reviewer0")
	handler := s.Handler()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/fixture/fork":
			s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				s.render(w, "session", pageData{Session: summary, SessionBackend: "pi", ForkAvailable: true, ForkInheritsSettings: true, SelectedMember: "reviewer0", SelectedThreadID: ref.NativeID, ConversationID: teamConversationID(source.Request.Slug, "reviewer0"), ConversationScope: piWorkflowNamespace(ref), HasConversation: true, ConversationReady: true, CreationDate: "2026-11-18", MaxMessageBytes: session.MaxMessageBytes})
			})).ServeHTTP(w, request)
		case "/api/sessions/" + source.Request.Slug + "/fork":
			s.sessionAPI(w, request)
		case "/" + r.Request.Slug + "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body data-fork-destination>Independent destination</body></html>`))
		default:
			if strings.HasPrefix(request.URL.Path, "/api/") || strings.HasPrefix(request.URL.Path, "/codex/conversations/") {
				s.writeJSON(w, http.StatusConflict, map[string]string{"error": "read fixture unavailable"})
				return
			}
			handler.ServeHTTP(w, request)
		}
	}))
	defer host.Close()
	input, _ := json.Marshal(map[string]string{"origin": host.URL, "path": "/fixture/fork", "destination": r.destinationURL(), "scope": piWorkflowNamespace(ref), "nativeId": ref.NativeID})
	node, e := exec.LookPath("node")
	if e != nil {
		t.Fatal(e)
	}
	if out, e := exec.Command(node, "pi_fork_hydrated_test.cjs", string(input)).CombinedOutput(); e != nil {
		t.Fatalf("Fork hydration: %v\n%s", e, out)
	} else {
		t.Log(string(out))
	}
}

func TestPiForkUnstartedRealAssignmentAndLaterAdditionKeepForkProof(t *testing.T) {
	s, source, native := newPiForkFixture(t, true)
	r := frozenPiForkReceipt(t, s, source, "reviewer0")
	if e := s.initializePiFork(r); e != nil {
		t.Fatal(e)
	}
	intent, _ := r.piIntent()
	var current piSessionAdmission
	if e := s.withPiSessionRead(context.Background(), intent.SessionRef(), func(c piSessionAdmission) error { current = c; return nil }); e != nil {
		t.Fatal(e)
	}
	store, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	roster, e := store.LoadPi(intent.Slug)
	if e != nil {
		t.Fatal(e)
	}
	service := piTeamService{s}
	proofs, e := service.readyProofs(context.Background(), current, roster)
	if e != nil {
		t.Fatal(e)
	}
	admission, e := newPiRosterWriteAdmission(s, current, proofs)
	if e != nil {
		t.Fatal(e)
	}
	lead, _ := current.Binding.Resolve("lead")
	caller := teamruntime.PiCaller{Handle: agentbackend.ExecutionHandle{Conversation: lead, Generation: current.Authority.Generation}, Runtime: native.runtime}
	settings, _ := intent.InitialMemberSettings("implementer0")
	message := teamruntime.PiMessage{ID: "33333333-3333-4333-8333-333333333333", To: "implementer0", Text: "The first real assignment"}
	assignment, e := store.ReservePiInitialAssignment(context.Background(), admission, caller, roster.Revision, message, settings, admission)
	if e != nil {
		t.Fatal(e)
	}
	reserved, _ := intent.ForkMember("implementer0")
	f := &piCreationRuntimeFixture{outcome: "Accepted"}
	native.assignment = f
	state, e := f.Create(context.Background(), native.runtime, piruntime.CreateRequest{Session: intent.SessionRef(), Member: assignment.Member, Incarnation: assignment.Incarnation, CreationID: assignment.CreationID, Settings: assignment.Settings, ToolPolicy: reserved.ReservedToolPolicy})
	if e != nil {
		t.Fatal(e)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	if _, e = f.Send(context.Background(), piruntime.BackendIdentity(handle), assignment.Request); e != nil {
		t.Fatal(e)
	}
	f.state.Persisted = true
	invocation := &piTeamInvocation{piTeamService: service, initial: &assignment, handle: handle, bindingRevision: current.Binding.Revision}
	if e = service.publishInitial(context.Background(), invocation, f.state, *f.submission); e != nil {
		t.Fatal("real assignment publication", e)
	}
	roster, e = store.LoadPi(intent.Slug)
	if e != nil {
		t.Fatal(e)
	}
	if roster.Schema != 4 || roster.Members[0].State != "ready" || roster.Members[0].InitialInputID != message.ID || roster.Members[0].Fork != nil || roster.Members[1].Fork == nil || f.creates != 1 || f.sends != 1 || native.forkCalls != 2 {
		t.Fatal("readiness paths mixed", roster, f.creates, f.sends, native.forkCalls)
	}
	// Append uses the same metadata operation after a fork; its older members and
	// fork header must survive, while the new member still awaits real assignment.
	now := time.Now().UTC()
	member := teamruntime.PiRosterMember{Address: "implementer1", Role: "implementer", Index: 1, Model: "model", Effort: "high", Behavior: "implementer", Access: "workspace_write", State: "creating", AddedAt: now, Incarnation: strings.Repeat("4", 64), CreationID: strings.Repeat("5", 64)}
	policy := intent.ProviderSelection.Policy.Public()
	selection := policy.Default.Selection
	policy, e = piproviders.WithOverride(policy, piproviders.MemberKey{Address: member.Address, Incarnation: member.Incarnation}, &selection)
	if e != nil {
		t.Fatal(e)
	}
	resolved, e := piproviders.Resolve(s.config.PiCreation.Catalog, policy, piproviders.MemberKey{Address: member.Address, Incarnation: member.Incarnation}, 1)
	if e != nil {
		t.Fatal(e)
	}
	applied := resolved.Settings
	member.Addition = &teamruntime.PiAdditionReservation{RequestID: "44444444-4444-4444-8444-444444444444", PayloadDigest: strings.Repeat("6", 64), RootCreationID: intent.ReceiptID, PriorRosterRevision: roster.Revision, PriorBindingRevision: current.Binding.Revision + 1, Generation: current.Authority.Generation, PackageRoot: s.config.PackageRoot, ProviderCatalogDigest: s.config.PiCreation.Catalog.Digest, Policy: policy, Selection: session.PiCreationMemberSelection{Address: member.Address, Incarnation: member.Incarnation, Source: resolved.Source, PolicyDigest: resolved.PolicyDigest, Settings: session.PiCreationSettings{Provider: applied.Provider, AccountRef: applied.AccountRef, Model: applied.Model, Thinking: applied.Thinking, PolicyRevision: applied.PolicyRevision, Revision: applied.Revision}}, ToolPolicy: *reserved.ReservedToolPolicy}
	next := roster
	next.Revision++
	next.UpdatedAt = now
	next.Members = append(append([]teamruntime.PiRosterMember{}, roster.Members...), member)
	write := piCreationRosterAdmission{ref: intent.SessionRef(), generation: current.Authority.Generation, validate: s.requireCurrentHostProfile}
	saved, e := store.AppendPi(context.Background(), write, current.Authority.Generation, &roster, next)
	if e != nil {
		t.Fatal("fork roster append", e)
	}
	if saved.Schema != 4 || saved.ForkCreationID != roster.ForkCreationID || !reflect.DeepEqual(saved.Members[:len(roster.Members)], roster.Members) || saved.Members[len(saved.Members)-1].InitialInputID != "" {
		t.Fatal("Add lost fork provenance", saved)
	}
	if f.submission.Input.Message != assignment.Request.Message || f.submission.Ref.OperationID != message.ID || f.submission.Phase != "Accepted" {
		t.Fatal("real assignment changed")
	}
}

func TestPiForkPolicyGraphPreservesDefaultAndExplicitOverride(t *testing.T) {
	s, source, native := newPiForkFixture(t, true)
	intent, _ := source.piIntent()
	root, e := piruntime.SessionStateDirectory(s.config.UserStateRoot, intent.SessionRef())
	if e != nil {
		t.Fatal(e)
	}
	store := piproviders.Store{Root: root, Catalog: s.config.PiCreation.Catalog, Session: intent.SessionRef()}
	policy := intent.ProviderSelection.Policy.Public()
	if e = store.CompareAndSwap(0, policy); e != nil {
		t.Fatal(e)
	}
	reviewer := intent.Conversations[2]
	selection := policy.Default.Selection
	next, e := piproviders.WithOverride(policy, piproviders.MemberKey{Address: reviewer.Address, Incarnation: reviewer.Incarnation}, &selection)
	if e != nil {
		t.Fatal(e)
	}
	if e = store.CompareAndSwap(policy.Revision, next); e != nil {
		t.Fatal(e)
	}
	applied, e := piproviders.Resolve(s.config.PiCreation.Catalog, next, piproviders.MemberKey{Address: reviewer.Address, Incarnation: reviewer.Incarnation}, 2)
	if e != nil {
		t.Fatal(e)
	}
	snapshot := native.sources[reviewer.Address]
	snapshot.Settings = applied.Settings
	native.sources[reviewer.Address] = snapshot
	before, e := os.ReadFile(filepath.Join(root, "provider-policy.json"))
	if e != nil {
		t.Fatal(e)
	}
	fork, _, e := s.capturePiFork(context.Background(), creationRequest{Backend: "pi", Kind: "fork", Source: intent.Slug, Slug: "2026-11-18-policies", SelectedMember: "reviewer0"}, strings.Repeat("3", 64), strings.Repeat("4", 64))
	if e != nil {
		t.Fatal(e)
	}
	if len(fork.ProviderSelection.Policy.Overrides) != 1 || fork.ProviderSelection.Policy.Overrides[0].Member.Address != "reviewer0" || fork.ProviderSelection.Policy.Overrides[0].Value.Revision != 1 || fork.ProviderSelection.Policy.Revision != 1 {
		t.Fatal("policy graph changed", fork.ProviderSelection.Policy)
	}
	copied, _ := fork.ForkMember("reviewer0")
	if copied.SelectionSource != "member_override" || copied.PolicyRevision != next.Revision || copied.Origin.Snapshot.Settings != applied.Settings || copied.Origin.Snapshot.Settings.Revision != 2 {
		t.Fatal("inherited applied provenance changed", copied)
	}
	if e = s.validatePiForkSourcePolicy(fork, copied); e != nil {
		t.Fatal("canonical source policy recheck", e)
	}
	after, _ := os.ReadFile(filepath.Join(root, "provider-policy.json"))
	if string(after) != string(before) || native.forkCalls != 0 {
		t.Fatal("capture changed source or copied history")
	}
}
