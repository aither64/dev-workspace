//go:build pi_native_fixture

package piruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/inputdelivery"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"github.com/aither64/dev-workspace/portal/internal/web"
	"golang.org/x/sys/unix"
)

// Run only after final committed-deliverable review. Example entrypoints:
// go test -tags=pi_native_fixture -run '^TestPiMessageDeliveryNativeBootstrap$'
// go test -tags=pi_native_fixture -run '^TestPiMessageDeliveryNativeRecovery$'
// go test -tags=pi_native_fixture -run 'TestPiMessageDeliveryNativePerformance/100000/team_activity$'
// Pass -count=1 and an explicit -timeout covering setup plus >=220 operations.
// The fake Host has real Unix/Node/SDK effects but synthetic systemd/drain proof;
// the faux provider omits model/network latency. In-process HTTP includes handler
// overhead, not production TLS/proxy latency. No old deployed pair is simulated.

type deliveryNativeRuntime struct {
	*piruntime.Manager
	t                          *testing.T
	diagnostics                *centralDiagnostics
	mu                         sync.Mutex
	before                     func(context.Context, piruntime.QueuedInputRequest) error
	after                      func(context.Context, piruntime.QueuedInputRequest, piruntime.QueuedInputResult, error) error
	entries                    map[string]int
	lookups                    map[string]int
	active                     map[string]int
	maxMember, maxTotal, total int
	selectionBlocked           atomic.Bool
}

func (r *deliveryNativeRuntime) VerifyCreation(ctx context.Context, handle agentbackend.ExecutionHandle, creation, history string, original *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	proof, err := r.Manager.VerifyCreation(ctx, handle, creation, history, original)
	id := "empty"
	if original != nil {
		id = original.OperationID
	}
	fmt.Fprintf(r.diagnostics, "native operation=verify_creation member=%s original=%s persisted=%t bootstrap=%t error=%T:%v\n", handle.Conversation.Member, id, proof.Persisted, proof.Bootstrap != nil, err, err)
	return proof, err
}

func (r *deliveryNativeRuntime) ReadMemberAdmission(ctx context.Context, ref agentbackend.ConversationRef, creation, id string) (piruntime.ConversationState, piruntime.SubmissionStatus, error) {
	state, status, err := r.Manager.ReadMemberAdmission(ctx, ref, creation, id)
	fmt.Fprintf(r.diagnostics, "native operation=read_member_admission member=%s original=%s phase=%s witness=%t settings=%+v error=%T:%v\n", ref.Member, id, status.Phase, status.Witness != nil, state.Settings, err, err)
	return state, status, err
}

func (r *deliveryNativeRuntime) Lookup(ctx context.Context, ref piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	status, err := r.Manager.Lookup(ctx, ref)
	fmt.Fprintf(r.diagnostics, "native operation=lookup member=%s original=%s phase=%s witness=%t error=%T:%v\n", ref.Conversation.Member, ref.OperationID, status.Phase, status.Witness != nil, err, err)
	return status, err
}

func (r *deliveryNativeRuntime) Create(ctx context.Context, runtime piruntime.RuntimeRef, request piruntime.CreateRequest) (piruntime.ConversationState, error) {
	state, err := r.Manager.Create(ctx, runtime, request)
	if err == nil {
		expected := agentbackend.ConversationRef{Session: request.Session, Backend: agentbackend.Pi, Member: request.Member, Incarnation: request.Incarnation, NativeID: state.Conversation.NativeID}
		if state.Conversation != expected || state.Generation != runtime.Generation || state.Settings != request.Settings {
			r.t.Errorf("delegated native Create tuple differs: requested runtime=%+v conversation=%+v settings=%+v; actual=%+v", runtime, expected, request.Settings, state)
		}
	}
	return state, err
}

func (r *deliveryNativeRuntime) LookupQueuedInput(ctx context.Context, ref piruntime.SubmissionRef) (piruntime.QueuedInputLookupResult, error) {
	ctx, task := trace.NewTask(ctx, "pi.fixture.native.LookupQueuedInput")
	defer task.End()
	trace.Logf(ctx, "originalID", "%s member=%s", ref.OperationID, ref.Conversation.Member)
	region := trace.StartRegion(ctx, "Manager.LookupQueuedInput")
	result, err := r.Manager.LookupQueuedInput(ctx, ref)
	region.End()
	trace.Logf(ctx, "outcome", "phase=%s retired=%t witness=%t error=%v", result.Status.Phase, result.RetiredUnknown, result.Status.Witness != nil, err)
	r.mu.Lock()
	r.lookups[ref.OperationID]++
	r.mu.Unlock()
	return result, err
}
func (r *deliveryNativeRuntime) lookupCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups[id]
}
func (r *deliveryNativeRuntime) ReadInputSelection(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.InputSelection, error) {
	selected, err := r.Manager.ReadInputSelection(ctx, h)
	if r.selectionBlocked.Load() {
		return selected, errors.Join(piruntime.ErrUnavailable, err)
	}
	return selected, err
}
func (r *deliveryNativeRuntime) DispatchQueuedInput(ctx context.Context, q piruntime.QueuedInputRequest) (piruntime.QueuedInputResult, error) {
	ctx, task := trace.NewTask(ctx, "pi.fixture.native.DispatchQueuedInput")
	defer task.End()
	trace.Logf(ctx, "originalID", "%s member=%s", q.Ref.OperationID, q.Ref.Conversation.Member)
	r.mu.Lock()
	r.entries[q.Ref.OperationID]++
	r.active[q.Ref.Conversation.Member]++
	r.total++
	r.maxMember = max(r.maxMember, r.active[q.Ref.Conversation.Member])
	r.maxTotal = max(r.maxTotal, r.total)
	before, after := r.before, r.after
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active[q.Ref.Conversation.Member]--; r.total--; r.mu.Unlock() }()
	// B has already committed queued -> dispatching at this exact entry.
	if before != nil {
		if err := before(ctx, q); err != nil {
			return piruntime.QueuedInputResult{}, err
		}
	}
	region := trace.StartRegion(ctx, "Manager.DispatchQueuedInput")
	result, err := r.Manager.DispatchQueuedInput(ctx, q)
	region.End()
	trace.Logf(ctx, "outcome", "phase=%s notSubmitted=%t witness=%t error=%v", result.Status.Phase, result.NotSubmitted, result.Status.Witness != nil, err)
	if after != nil {
		err = errors.Join(err, after(ctx, q, result, err))
	}
	return result, err
}
func (r *deliveryNativeRuntime) ObserveWorkflow(ctx context.Context, h agentbackend.ExecutionHandle) (piruntime.WorkflowObservation, error) {
	ctx, task := trace.NewTask(ctx, "pi.fixture.native.ObserveWorkflow")
	defer task.End()
	trace.Logf(ctx, "scope", "handle=%+v", h)
	region := trace.StartRegion(ctx, "Manager.ObserveWorkflow")
	result, err := r.Manager.ObserveWorkflow(ctx, h)
	region.End()
	trace.Logf(ctx, "outcome", "error=%v handleMatch=%t pending=%t held=%t idle=%t sequence=%d attachment=%s", err, result.Handle == h, result.Pending, result.Held, result.Idle, result.Sequence, result.AttachmentID)
	return result, err
}
func (r *deliveryNativeRuntime) InspectNativeOrigin(ctx context.Context, h agentbackend.ExecutionHandle, origin piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
	ctx, task := trace.NewTask(ctx, "pi.fixture.native.InspectNativeOrigin")
	defer task.End()
	trace.Logf(ctx, "scope", "handle=%+v origin=%+v", h, origin)
	region := trace.StartRegion(ctx, "Manager.InspectNativeOrigin")
	result, err := r.Manager.InspectNativeOrigin(ctx, h, origin)
	region.End()
	trace.Logf(ctx, "outcome", "error=%v handleMatch=%t originMatch=%t argumentBytes=%d", err, result.Handle == h, result.Origin == origin, len(result.Arguments))
	return result, err
}
func (r *deliveryNativeRuntime) hooks(before func(context.Context, piruntime.QueuedInputRequest) error, after func(context.Context, piruntime.QueuedInputRequest, piruntime.QueuedInputResult, error) error) {
	r.mu.Lock()
	r.before = before
	r.after = after
	r.mu.Unlock()
}
func (r *deliveryNativeRuntime) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[id]
}

type deliveryNativeSuite struct {
	t                          *testing.T
	f                          *piruntime.PiMessageDeliveryFixture
	intent                     session.PiCreationIntent
	catalog                    agentteams.Catalog
	config                     web.Config
	server                     *web.Server
	runtime                    *deliveryNativeRuntime
	handles                    []agentbackend.ExecutionHandle
	sequence                   atomic.Uint64
	artifact                   *os.File
	artifactMu                 sync.Mutex
	started                    time.Time
	setupMember, setupOriginal string
	setupComplete              bool
	diagnosticInterval         atomic.Value // string, set only by the bounded diagnostic
}

func deliveryNativeContext(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("native campaign requires explicit go test -timeout")
	}
	if time.Until(deadline) < time.Minute {
		t.Fatal("campaign budget must cover native setup and requested samples")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-5*time.Second))
	t.Cleanup(cancel)
	return ctx
}

func deliveryNativePreset(t *testing.T) (agentteams.Catalog, teamruntime.Preset) {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("PI_CENTRAL_CATALOG_FIXTURE"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog agentteams.Catalog
	if json.Unmarshal(data, &catalog) != nil || catalog.Validate() != nil {
		t.Fatal("valid typed four-member catalog required")
	}
	preset, err := teamruntime.FindCatalogPreset(catalog, catalog.DefaultTeam)
	if err != nil || len(preset.Members) != 4 {
		t.Fatal("use an authentic default preset with exactly four members, never truncate a catalog", preset, err)
	}
	// Apply the owning new-Pi projection once. Retained intent/roster reads and
	// reopen/successor paths reuse the saved result, never project it again.
	preset, err = teamruntime.ProjectPiPreset(preset)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, preset
}

func deliveryNativeModels(preset teamruntime.Preset) []string {
	models := []string{preset.LeadModel}
	for _, member := range preset.Members {
		if !slices.Contains(models, member.Model) {
			models = append(models, member.Model)
		}
	}
	return models
}

// The central retained-ready publisher is reused below, but its p3-model/high
// intent helper cannot represent this catalog's different member selections.
// Freeze the exact projected preset using the real policy/intent codecs. Only
// provider execution is synthetic; models, efforts and team provenance are kept.
func deliveryNativeIntent(t *testing.T, ref agentbackend.SessionRef, preset teamruntime.Preset, history string) (session.PiCreationIntent, piproviders.Catalog) {
	t.Helper()
	providers := centralProviderCatalog()
	providers.Models, providers.Routes = []piproviders.Model{}, []piproviders.RouteGate{}
	for _, model := range deliveryNativeModels(preset) {
		providers.Models = append(providers.Models, piproviders.Model{Kind: "physical", ProviderID: "fixture", ID: model, Label: "Fixture " + model, ThinkingLevels: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "faux-native-fixture"})
		providers.Routes = append(providers.Routes, piproviders.RouteGate{ProviderID: "fixture", AccountRef: "fake-account", ModelID: model, State: "available", ReasonCode: "verified", EvidenceRef: "faux-native-fixture"})
	}
	data, err := json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	providers, err = piproviders.DecodeCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	selection := func(model, effort string) piproviders.VersionedSelection {
		return piproviders.VersionedSelection{Revision: 1, Selection: piproviders.Selection{Chain: []piproviders.Target{{ProviderID: "fixture", AccountRef: "fake-account", ModelID: model, Thinking: effort}}, FallbackOn: []piproviders.FailureCategory{}}}
	}
	reserved := []session.PiReservedConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64)}}
	team := session.PiCreationTeam{ID: preset.ID, Name: preset.Name, Description: preset.Description, Roles: slices.Clone(preset.Roles), CatalogDigest: preset.CatalogDigest, TeamDigest: preset.TeamDigest, LeadModel: preset.LeadModel, LeadEffort: preset.LeadEffort, LeadInstructions: preset.LeadInstructions, Members: []session.PiCreationTeamMember{}}
	policy := piproviders.Policy{Session: ref, Revision: 1, Default: selection(preset.LeadModel, preset.LeadEffort), Overrides: []piproviders.Override{{Member: piproviders.MemberKey{Address: "lead", Incarnation: reserved[0].Incarnation}, Value: selection(preset.LeadModel, preset.LeadEffort)}}}
	for index, member := range preset.Members {
		conversation := session.PiReservedConversation{Address: member.Address, Incarnation: fmt.Sprintf("%064x", index+100)}
		reserved = append(reserved, conversation)
		team.Members = append(team.Members, session.PiCreationTeamMember{Role: member.Role, Address: member.Address, Model: member.Model, Effort: member.Effort, Behavior: member.Behavior, Access: member.Access, Purpose: member.Purpose, Instructions: member.Instructions})
		policy.Overrides = append(policy.Overrides, piproviders.Override{Member: piproviders.MemberKey{Address: conversation.Address, Incarnation: conversation.Incarnation}, Value: selection(member.Model, member.Effort)})
	}
	frozen, err := session.FreezePiCreationSelection(providers, policy, reserved)
	if err != nil {
		t.Fatal(err)
	}
	intent := session.PiCreationIntent{Schema: 4, Workspace: ref.Workspace, Slug: ref.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: strings.Repeat("c", 64), DeletionHistorySHA256: history, SessionIncarnation: ref.Incarnation, Conversations: reserved, GoalSHA256: piruntime.MessageDigest("publish optional question and exact plan"), Model: preset.LeadModel, Effort: preset.LeadEffort, Provider: "p3-fake", ProviderSelection: frozen, DirectTeam: team, InitialInputID: "44444444-4444-4444-8444-444444444444"}
	if err := intent.Validate(providers); err != nil {
		t.Fatal("catalog/reserved/native settings join", err)
	}
	return intent, providers
}

// This uses the actual supplied canonical catalog and the same setup builder as
// Bootstrap, with no Manager, Host, SDK, Unix listener or native worker startup.
func TestPiMessageDeliverySeedIntent(t *testing.T) {
	catalog, preset := deliveryNativePreset(t)
	before, _ := json.Marshal(catalog)
	ref := agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}
	intent, providers := deliveryNativeIntent(t, ref, preset, strings.Repeat("e", 64))
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := session.DecodePiCreationIntent(data)
	if err != nil || saved.Validate(providers) != nil || saved.ValidateBinding(saved.Binding()) != nil || len(saved.Conversations) != 5 {
		t.Fatal("owning strict intent/binding round trip", err)
	}
	source, err := teamruntime.FindCatalogPreset(catalog, catalog.DefaultTeam)
	if err != nil || source.TeamDigest == saved.DirectTeam.TeamDigest || source.CatalogDigest != saved.DirectTeam.CatalogDigest {
		t.Fatal("source catalog and once-projected team provenance", err)
	}
	for i, member := range preset.Members {
		actual := saved.DirectTeam.Members[i]
		settings := saved.ProviderSelection.Members[i+1]
		if actual.Model != member.Model || actual.Effort != member.Effort || actual.Instructions != member.Instructions || actual.Access != member.Access || actual.Purpose != member.Purpose || settings.Settings.Model != member.Model || settings.Settings.Thinking != member.Effort || settings.Address != member.Address {
			t.Fatal("catalog member selection changed", member.Address)
		}
	}
	// Pin both failures hidden behind the old hardcoded lead selection.
	badLead := saved
	badLead.Model = "p3-model"
	badMember := saved
	badMember.ProviderSelection.Members = slices.Clone(saved.ProviderSelection.Members)
	badMember.ProviderSelection.Members[1].Settings.Model = "p3-model"
	if badLead.ValidateShape() == nil || badMember.ValidateShape() == nil {
		t.Fatal("mismatched lead/member settings passed the owning guard")
	}
	after, _ := json.Marshal(catalog)
	if !bytes.Equal(before, after) {
		t.Fatal("seed composition mutated canonical catalog")
	}
}

func TestPiMessageDeliveryFauxSelections(t *testing.T) {
	_, preset := deliveryNativePreset(t)
	ref := agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}
	intent, _ := deliveryNativeIntent(t, ref, preset, strings.Repeat("e", 64))
	piruntime.CheckPiMessageDeliveryModels(t, deliveryNativeSelections(intent))
}

func deliveryNativeSelections(intent session.PiCreationIntent) []piruntime.EffectiveSettings {
	selections := make([]piruntime.EffectiveSettings, 0, len(intent.ProviderSelection.Members))
	for _, member := range intent.ProviderSelection.Members {
		selections = append(selections, member.Settings.Effective())
	}
	return selections
}

// Retained-ready composition still needs complete creation ports for the owning
// root-proof guard. These ports map only the exact captured original tuple;
// they do not select a replacement, Ensure a runtime or claim public creation.
func deliveryNativeServices(t *testing.T, intent session.PiCreationIntent, preset teamruntime.Preset, providers piproviders.Catalog, start piruntime.StartRequest, runtime web.PiCreationRuntime) *web.PiCreationServices {
	t.Helper()
	encode := func(value any) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	intentJSON, presetJSON := encode(intent), encode(preset)
	reservedJSON := encode(intent.Conversations)
	policyJSON := encode(intent.ProviderSelection.Policy.Public())
	var projected session.PiCreationTeam
	if runtime == nil || intent.Validate(providers) != nil || json.Unmarshal(presetJSON, &projected) != nil || !bytes.Equal(encode(projected), encode(intent.DirectTeam)) || start.Session != intent.SessionRef() || start.BindingRevision != 1 {
		t.Fatal("incoherent frozen creation services")
	}
	ref := intent.SessionRef()
	return &web.PiCreationServices{Catalog: providers, Runtime: runtime,
		Selection: func(ctx context.Context, selected agentbackend.SessionRef, reserved []session.PiReservedConversation, selectedPreset teamruntime.Preset) (piproviders.Policy, error) {
			if err := ctx.Err(); err != nil {
				return piproviders.Policy{}, err
			}
			if selected != ref || !bytes.Equal(encode(reserved), reservedJSON) || !bytes.Equal(encode(selectedPreset), presetJSON) {
				return piproviders.Policy{}, agentbackend.ErrBindingConflict
			}
			var policy piproviders.Policy
			if err := json.Unmarshal(policyJSON, &policy); err != nil {
				return policy, err
			}
			return policy, piproviders.ValidatePolicy(providers, policy)
		},
		StartRequest: func(selected session.PiCreationIntent, revision uint64) (piruntime.StartRequest, error) {
			if revision != start.BindingRevision || !bytes.Equal(encode(selected), intentJSON) {
				return piruntime.StartRequest{}, agentbackend.ErrBindingConflict
			}
			return start, nil
		}}
}

func TestPiMessageDeliveryCreationServices(t *testing.T) {
	catalog, preset := deliveryNativePreset(t)
	f := piruntime.NewPiMessageDeliveryFixture(t, context.Background(), deliveryNativeModels(preset))
	intent, providers := deliveryNativeIntent(t, f.Start.Session, preset, strings.Repeat("e", 64))
	services := deliveryNativeServices(t, intent, preset, providers, f.Start, f.Manager)
	ctx := context.Background()
	policy, err := services.Selection(ctx, intent.SessionRef(), intent.Conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := session.FreezePiCreationSelection(providers, policy, intent.Conversations)
	if err != nil || !bytes.Equal(mustDeliveryJSON(t, frozen), mustDeliveryJSON(t, intent.ProviderSelection)) {
		t.Fatal("original frozen selection changed", err)
	}
	start, err := services.StartRequest(intent, f.Start.BindingRevision)
	if err != nil || start != f.Start {
		t.Fatal("original full Start changed", start, err)
	}
	wrongRef := intent.SessionRef()
	wrongRef.Incarnation = strings.Repeat("f", 64)
	wrongReserved := slices.Clone(intent.Conversations)
	wrongReserved[1].Incarnation = strings.Repeat("f", 64)
	wrongPreset := preset
	wrongPreset.LeadEffort = "off"
	wrongModel := preset
	wrongModel.LeadModel = "p3-model"
	for _, selected := range []struct {
		ref      agentbackend.SessionRef
		reserved []session.PiReservedConversation
		preset   teamruntime.Preset
	}{{wrongRef, intent.Conversations, preset}, {intent.SessionRef(), wrongReserved, preset}, {intent.SessionRef(), intent.Conversations, wrongPreset}, {intent.SessionRef(), intent.Conversations, wrongModel}} {
		if _, err := services.Selection(ctx, selected.ref, selected.reserved, selected.preset); err == nil {
			t.Fatal("changed scope/reservation/preset selected original policy")
		}
	}
	wrongIntent := intent
	wrongIntent.GoalSHA256 = strings.Repeat("f", 64)
	if _, err := services.StartRequest(wrongIntent, f.Start.BindingRevision); err == nil {
		t.Fatal("changed intent selected original Start")
	}
	if _, err := services.StartRequest(intent, f.Start.BindingRevision+1); err == nil {
		t.Fatal("changed revision selected original Start")
	}
	root := filepath.Dir(f.Start.Session.Workspace)
	falseCommand, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(root, "absent-provider-catalog"))
	server, err := web.NewContext(ctx, web.Config{Workspace: intent.Workspace, BaseURL: "https://portal.example", DevSession: falseCommand, HostProfile: f.Start.ProfilePath, AuthorityDir: filepath.Join(root, "authority"), UserStateRoot: filepath.Join(root, "state"), PackageRoot: f.Start.AppStorePath, WorkspaceName: "delivery-prerequisite", CodexSocket: filepath.Join(root, "unused/app-server.sock"), RegistrationMarker: filepath.Join(root, "unused/registration.json"), Logger: log.New(io.Discard, "", 0), PiCreation: services})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	// The actual root-proof entry must pass service availability and stop at
	// missing storage ownership. No binding, witness or Accepted is fabricated.
	if err := server.ProvePiRetainedCreationNativeFixture(ctx, intent); !errors.Is(err, agentbackend.ErrNotBound) {
		t.Fatal("coherent services did not reach missing-binding guard", err, catalog.DefaultTeam)
	}
	if counts := f.Counts(); counts.HostStarts != 0 || counts.HostInspects != 0 || len(counts.Calls) != 0 {
		t.Fatal("pure prerequisite check entered native runtime", counts)
	}
}

func mustDeliveryJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newDeliveryNativeSuite(t *testing.T) *deliveryNativeSuite {
	t.Helper()
	ctx := deliveryNativeContext(t)
	catalog, preset := deliveryNativePreset(t)
	f := piruntime.NewPiMessageDeliveryFixture(t, ctx, deliveryNativeModels(preset))
	history, err := session.CompletedRemovalHistory(f.Start.Session.Workspace, f.Start.Session.Slug, filepath.Join(filepath.Dir(f.Start.Session.Workspace), "state"))
	if err != nil {
		t.Fatal(err)
	}
	intent, providers := deliveryNativeIntent(t, f.Start.Session, preset, history)
	// Validate all static catalog/reservation/settings joins before Ensure or
	// native Create. The same composition has a cheap SDK-free regression below.
	f.Settings = intent.ProviderSelection.Members[0].Settings.Effective()
	piruntime.CheckPiMessageDeliveryModels(t, deliveryNativeSelections(intent))
	f.Ensure(t)
	root := filepath.Dir(f.Runtime.Session.Workspace)
	authority := filepath.Join(root, "delivery-authority")
	if err := os.Mkdir(authority, 0700); err != nil {
		t.Fatal(err)
	}
	transition := filepath.Join(root, "delivery-transition.lock")
	centralWrite(t, transition, nil)
	falseCommand, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(root, "absent-provider-catalog"))
	diagnostics := &centralDiagnostics{}
	spy := &deliveryNativeRuntime{Manager: f.Manager, t: t, diagnostics: diagnostics, entries: map[string]int{}, lookups: map[string]int{}, active: map[string]int{}}
	config := web.Config{Workspace: intent.Workspace, BaseURL: "https://portal.example", DevSession: falseCommand, HostProfile: f.Start.ProfilePath, AuthorityDir: authority, TransitionLock: transition, UserStateRoot: filepath.Join(root, "state"), PackageRoot: f.Start.AppStorePath, WorkspaceName: "delivery-fixture", CodexSocket: filepath.Join(root, "unused/app-server.sock"), RegistrationMarker: filepath.Join(root, "unused/registration.json"), Logger: log.New(diagnostics, "", 0), PiCreation: deliveryNativeServices(t, intent, preset, providers, f.Start, spy)}
	s := &deliveryNativeSuite{t: t, f: f, intent: intent, catalog: catalog, config: config, runtime: spy, started: time.Now()}
	artifactRoot := os.Getenv("PI_MESSAGE_DELIVERY_ARTIFACT_DIR")
	if artifactRoot == "" {
		artifactRoot, err = os.MkdirTemp("", "pi-message-delivery-")
		if err != nil {
			t.Fatal(err)
		}
	}
	if !filepath.IsAbs(artifactRoot) {
		t.Fatal("artifact directory must be an absolute disposable untracked directory")
	}
	if !strings.HasPrefix(filepath.Clean(artifactRoot), filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatal("raw campaign artifacts must remain outside repositories under the temporary directory")
	}
	if err := os.MkdirAll(artifactRoot, 0700); err != nil {
		t.Fatal(err)
	}
	s.artifact, err = os.OpenFile(filepath.Join(artifactRoot, strings.ReplaceAll(t.Name(), "/", "-")+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("raw per-ID artifact:", s.artifact.Name())
	t.Cleanup(func() { _ = s.artifact.Close() })
	t.Cleanup(func() {
		if s.server != nil {
			s.server.Close()
		}
	})
	// Register before NewContext or any native setup can Fatal. LIFO cleanup
	// captures failure evidence before portal Close, artifact Close or TempDir.
	t.Cleanup(func() {
		if t.Failed() && !s.setupComplete {
			s.setupFailure(s.setupMember, s.setupOriginal, diagnostics.String())
		}
	})
	s.setupMember, s.setupOriginal = "lead", intent.InitialInputID
	s.open()
	lock, err := session.LockRuntimeExclusiveContext(ctx, authority, intent.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.server.BindPiWorkflowNativeFixture(ctx, agentteams.Installed{Managed: true, Catalog: &catalog}, f.Runtime, true); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	policy, err := s.server.PiInitialWorkflowPolicyNativeFixture(intent, "lead")
	if err != nil || policy == nil {
		lock.Close()
		t.Fatal(err)
	}
	state, err := f.Manager.Create(ctx, f.Runtime, piruntime.CreateRequest{Session: f.Runtime.Session, Member: "lead", Incarnation: intent.Conversations[0].Incarnation, CreationID: intent.ReceiptID, Instructions: intent.DirectTeam.LeadInstructions, Settings: intent.ProviderSelection.Members[0].Settings.Effective(), ToolPolicy: policy})
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	h := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	ops, err := f.Manager.Bind(ctx, h)
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	goal := "publish optional question and exact plan"
	if _, err := ops.Send.Send(ctx, piruntime.BackendIdentity(h), backend.SendRequest{Message: goal, ClientUserMessageID: intent.InitialInputID, ActionContext: "initial"}); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	original := nativeRef(piquestions.Caller{Handle: h}, intent.InitialInputID, goal, "initial", state.Settings)
	status, err := f.Manager.Lookup(ctx, original)
	if err != nil || status.Phase != "Accepted" || status.Witness == nil {
		lock.Close()
		t.Fatal("real bootstrap witness", status, err)
	}
	centralPublishReady(t, config, intent, state, status)
	if err := s.server.ProvePiRetainedCreationNativeFixture(ctx, intent); err != nil {
		lock.Close()
		t.Fatal("owning retained-ready root proof", err)
	}
	lock.Close()
	f.Idle(h)
	s.handles = append(s.handles, h)
	store, err := teamruntime.NewStore(config.UserStateRoot, config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	roster := teamruntime.PiRoster{Schema: 2, Workspace: intent.Workspace, Slug: intent.Slug, Revision: 1, CreatedAt: now, UpdatedAt: now, PresetID: intent.DirectTeam.ID, CatalogDigest: intent.DirectTeam.CatalogDigest, TeamDigest: intent.DirectTeam.TeamDigest, LeadModel: intent.DirectTeam.LeadModel, LeadEffort: intent.DirectTeam.LeadEffort, LeadInstructions: intent.DirectTeam.LeadInstructions, Backend: "pi", BackendContract: 1, SessionIncarnation: intent.SessionIncarnation, LeadIncarnation: intent.Conversations[0].Incarnation, Members: []teamruntime.PiRosterMember{}}
	for i, m := range intent.DirectTeam.Members {
		roster.Members = append(roster.Members, teamruntime.PiRosterMember{Address: m.Address, Role: m.Role, Model: m.Model, Effort: m.Effort, Behavior: m.Behavior, Purpose: m.Purpose, Instructions: m.Instructions, Access: m.Access, PolicyCatalogDigest: intent.DirectTeam.CatalogDigest, State: "creating", AddedAt: now, Incarnation: intent.Conversations[i+1].Incarnation, CreationID: intent.ReceiptID})
	}
	if err := store.SavePi(ctx, s.server, h.Generation, 0, roster); err != nil {
		t.Fatal(err)
	}
	for _, member := range roster.Members {
		s.setupMember, s.setupOriginal = member.Address, ""
		// This real read route repeats the owning saved binding/catalog/roster/
		// selection/current-authority joins before entering member materialization.
		view := s.request(ctx, "GET", "/api/sessions/"+intent.Slug+"/team", h, nil)
		if view.Code != http.StatusOK {
			t.Fatalf("setup joins before %s assignment: %d %s\n%s", member.Address, view.Code, view.Body.String(), diagnostics.String())
		}
		id := s.id()
		s.setupOriginal = id
		response := s.request(ctx, "POST", "/api/sessions/"+intent.Slug+"/team", h, map[string]string{"action": "assign", "from": "lead", "to": member.Address, "messageId": id, "message": "disposable native setup assignment " + id})
		if response.Code != 200 {
			t.Fatalf("real member assignment %s: %d %s\n%s", member.Address, response.Code, response.Body.String(), diagnostics.String())
		}
		ref := agentbackend.ConversationRef{Session: intent.SessionRef(), Backend: agentbackend.Pi, Member: member.Address, Incarnation: member.Incarnation}
		var memberState piruntime.ConversationState
		centralWait(t, ctx, "original member Accepted", func() bool {
			var receipt piruntime.SubmissionStatus
			memberState, receipt, err = f.Manager.ReadMemberAdmission(ctx, ref, member.CreationID, id)
			return err == nil && receipt.Phase == "Accepted" && receipt.Witness != nil
		})
		memberHandle := agentbackend.ExecutionHandle{Conversation: memberState.Conversation, Generation: memberState.Generation}
		f.Idle(memberHandle)
		binding, err := (session.BackendBindingStore{Root: config.UserStateRoot, Workspace: config.Workspace}).Load(intent.Slug)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := binding.Resolve(member.Address)
		if err != nil || resolved != memberHandle.Conversation {
			t.Fatal("actual member binding", resolved, err)
		}
		saved, err := store.LoadPi(intent.Slug)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range saved.Members {
			if m.Address == member.Address && m.State == "ready" && m.InitialInputID == id {
				found = true
			}
		}
		if !found {
			t.Fatal("actual ready roster missing member", member.Address)
		}
		s.handles = append(s.handles, memberHandle)
	}
	ids := map[string]bool{}
	pid := 0
	for _, h := range s.handles {
		if ids[h.Conversation.NativeID] || h.Conversation.Session != f.Runtime.Session || h.Generation != f.Runtime.Generation {
			t.Fatal("five distinct native conversations required", s.handles)
		}
		ids[h.Conversation.NativeID] = true
		selected, err := f.Manager.ReadInputSelection(ctx, h)
		if err != nil || !selected.CheckedInput || selected.InstructionsDigest == "" {
			t.Fatal("real immutable checked selection", selected, err)
		}
		root, err := piruntime.SessionStateDirectory(config.UserStateRoot, h.Conversation.Session)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "members", h.Conversation.Member, h.Conversation.Incarnation, "conversation.json"))
		if err != nil {
			t.Fatal(err)
		}
		var topology struct {
			WorkerPID      int
			WorkerTopology string
		}
		if json.Unmarshal(data, &topology) != nil || topology.WorkerPID <= 0 || topology.WorkerTopology != "shared-session-v1" || pid != 0 && pid != topology.WorkerPID {
			t.Fatal("candidate shared topology", topology)
		}
		pid = topology.WorkerPID
	}
	s.write(map[string]any{"kind": "setup", "elapsedNs": time.Since(s.started).Nanoseconds(), "conversations": s.handles, "setupAssignments": 4, "counts": f.Counts(), "provenance": s.provenance(), "limits": []string{"retained-ready central seed; public naming/root allocation not exercised", "synthetic Host/systemd/drain", "faux offline provider", "in-process HTTP, no TLS/proxy", "no old deployed-pair baseline"}})
	s.setupComplete = true
	return s
}

func (s *deliveryNativeSuite) setupFailure(member, original, diagnostics string) {
	// Read-only evidence at the failed boundary, before TempDir cleanup. These
	// observations are not a quiescence/acceptance proof and never retry a call.
	binding, bindingErr := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(s.intent.Slug)
	authority, authorityErr := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, s.intent.Slug, s.config.Workspace)
	current, currentErr := s.f.Manager.ReadCurrentRuntime(s.f.Context, s.intent.SessionRef())
	s.write(map[string]any{"kind": "setup_failure", "member": member, "originalID": original, "binding": binding, "bindingError": fmt.Sprint(bindingErr), "authority": authority, "authorityError": fmt.Sprint(authorityErr), "current": current, "currentError": fmt.Sprint(currentErr), "native": s.f.SetupEvidence(), "counts": s.f.Counts(), "diagnostics": diagnostics})
	if err := s.artifact.Sync(); err != nil {
		s.t.Error(err)
	}
}

func (s *deliveryNativeSuite) open() {
	s.t.Helper()
	var err error
	s.server, err = web.NewContext(s.f.Context, s.config)
	if err != nil {
		s.t.Fatal(err)
	}
}
func (s *deliveryNativeSuite) restart() {
	s.t.Helper()
	s.server.Close()
	lock, err := session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
	if err != nil {
		s.t.Fatal(err)
	}
	defer lock.Close()
	s.reopenLocked()
}
func (s *deliveryNativeSuite) reopenLocked() {
	// Hold ordinary runtime EX while installing the existing test-only catalog
	// boundary, before the newly started dispatcher can observe native authority.
	s.server.Close()
	s.open()
	if err := s.server.BindPiWorkflowNativeFixture(s.f.Context, agentteams.Installed{Managed: true, Catalog: &s.catalog}, s.f.Runtime, true); err != nil {
		s.t.Fatal(err)
	}
}
func (s *deliveryNativeSuite) id() string { return fmt.Sprintf("%032x", s.sequence.Add(1)+4096) }
func (s *deliveryNativeSuite) path(h agentbackend.ExecutionHandle) string {
	id := h.Conversation.Session.Slug
	if h.Conversation.Member != "lead" {
		id += "~" + h.Conversation.Member
	}
	return "/codex/conversations/" + id + "/deliveries"
}
func (s *deliveryNativeSuite) request(ctx context.Context, method, path string, h agentbackend.ExecutionHandle, input any) *httptest.ResponseRecorder {
	var body []byte
	if input != nil {
		body, _ = json.Marshal(input)
	}
	return s.encodedRequest(ctx, method, path, h, body)
}
func (s *deliveryNativeSuite) encodedRequest(ctx context.Context, method, path string, h agentbackend.ExecutionHandle, body []byte) *httptest.ResponseRecorder {
	ctx, task := trace.NewTask(ctx, "pi.fixture.http")
	defer task.End()
	started := time.Since(s.started).Nanoseconds()
	interval, _ := s.diagnosticInterval.Load().(string)
	var identity struct {
		ClientID   string `json:"clientUserMessageId"`
		ResponseID string `json:"responseId"`
	}
	_ = json.Unmarshal(body, &identity)
	trace.Logf(ctx, "request", "method=%s endpoint=%s member=%s originalID=%s responseID=%s", method, path, h.Conversation.Member, identity.ClientID, identity.ResponseID)
	if method == "POST" && strings.Contains(path, "/deliveries") {
		ctx = piruntime.PiMessageDeliveryAdmissionContext(ctx)
	}
	r := httptest.NewRequest(method, s.config.BaseURL+path, bytes.NewReader(body)).WithContext(ctx)
	r.Header.Set("Origin", s.config.BaseURL)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Workspace-Conversation-Scope", inputdelivery.ConversationScope(h.Conversation))
	r.Header.Set("X-Workspace-Pi-Generation", h.Generation)
	w := httptest.NewRecorder()
	s.server.Handler().ServeHTTP(w, r)
	trace.Logf(ctx, "response", "status=%d", w.Code)
	if interval != "" {
		outcome := deliveryHTTPOutcomeFor(method, path, h.Conversation.Member, w)
		if outcome.OriginalID == "" {
			outcome.OriginalID = identity.ClientID
			if outcome.OriginalID == "" {
				outcome.OriginalID = identity.ResponseID
			}
		}
		outcome.StartedNS = started
		outcome.CompletedNS = time.Since(s.started).Nanoseconds()
		s.write(map[string]any{"kind": "diagnostic_http", "interval": interval, "completedInterval": s.diagnosticInterval.Load(), "http": outcome})
	}
	return w
}
func (s *deliveryNativeSuite) metadata(h agentbackend.ExecutionHandle) backend.DeliveryCollection {
	s.t.Helper()
	var result backend.DeliveryCollection
	response := s.request(s.f.Context, "GET", s.path(h), h, nil)
	centralDecode(s.t, response, &result)
	if !result.CanEnqueue || result.Selection == "" {
		s.t.Fatal("new admission unavailable", result)
	}
	return result
}
func (s *deliveryNativeSuite) input(h agentbackend.ExecutionHandle, message, mode string) backend.DeliveryRequest {
	return backend.DeliveryRequest{Version: 1, ClientUserMessageID: s.id(), Message: message, Delivery: mode, Selection: s.metadata(h).Selection}
}
func (s *deliveryNativeSuite) post(h agentbackend.ExecutionHandle, r backend.DeliveryRequest) backend.DeliveryStatus {
	s.t.Helper()
	response := s.request(s.f.Context, "POST", s.path(h), h, r)
	if response.Code != 202 {
		s.t.Fatalf("durable admission %d %s", response.Code, response.Body.String())
	}
	var status backend.DeliveryStatus
	centralDecode(s.t, response, &status)
	return status
}
func (s *deliveryNativeSuite) status(h agentbackend.ExecutionHandle, id string) backend.DeliveryStatus {
	s.t.Helper()
	var status backend.DeliveryStatus
	centralDecode(s.t, s.request(s.f.Context, "GET", s.path(h)+"/"+id, h, nil), &status)
	return status
}
func (s *deliveryNativeSuite) wait(h agentbackend.ExecutionHandle, id string, states ...string) backend.DeliveryStatus {
	s.t.Helper()
	var status backend.DeliveryStatus
	centralWait(s.t, s.f.Context, "original delivery "+id+" -> "+strings.Join(states, "/"), func() bool { status = s.status(h, id); return slices.Contains(states, status.State) })
	return status
}
func (s *deliveryNativeSuite) ref(h agentbackend.ExecutionHandle, r backend.DeliveryRequest) piruntime.SubmissionRef {
	s.t.Helper()
	return s.row(h, r.ClientUserMessageID).Admission.Ref
}
func (s *deliveryNativeSuite) assertAccepted(h agentbackend.ExecutionHandle, r backend.DeliveryRequest) {
	s.t.Helper()
	status := s.wait(h, r.ClientUserMessageID, "sent")
	ref := s.ref(h, r)
	lookup, err := s.f.Manager.LookupQueuedInput(s.f.Context, ref)
	if err != nil || lookup.Status.Phase != "Accepted" || lookup.Status.Ref != ref || lookup.Status.Witness == nil || status.Receipt == nil || status.Receipt.TurnID != lookup.Status.Witness.EntryID {
		s.t.Fatal("exact independent native witness", status, lookup, err)
	}
	if s.f.Counts().Enqueues[r.ClientUserMessageID] != 1 || s.runtime.count(r.ClientUserMessageID) != 1 {
		s.t.Fatal("original native input was skipped/replayed", r.ClientUserMessageID, s.f.Counts())
	}
}
func (s *deliveryNativeSuite) write(value any) {
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()
	if err := json.NewEncoder(s.artifact).Encode(value); err != nil {
		s.t.Error(err)
	}
}

func (s *deliveryNativeSuite) provenance() map[string]any {
	digest := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			s.t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(data))
	}
	head := func(dir string) string {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			s.t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	binary, err := os.Executable()
	if err != nil {
		s.t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		s.t.Fatal(err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(cwd, &fs); err != nil {
		s.t.Fatal(err)
	}
	return map[string]any{"app": s.config.PackageRoot, "sdk": s.f.WorkerMetadata.PiPackage, "worker": s.f.WorkerMetadata.Entrypoint, "workerSHA256": digest(s.f.WorkerMetadata.Entrypoint), "fauxEntrypointSHA256": digest(s.f.Entrypoint), "protocolSHA256": digest(filepath.Join(filepath.Dir(s.f.WorkerMetadata.Entrypoint), "protocol.mjs")), "catalogSHA256": digest(os.Getenv("PI_CENTRAL_CATALOG_FIXTURE")), "catalogDigest": s.catalog.CatalogDigest, "devWorkspaceHead": head("../../.."), "codexWebHead": head("../../../../codex-web"), "testBinarySHA256": digest(binary), "go": runtime.Version(), "machine": runtime.GOOS + "/" + runtime.GOARCH, "filesystemType": fs.Type}
}

// This read uses the public durable store with the same real transition gate.
// It allows assertions on an old identity after access/scope itself is revoked.
func (s *deliveryNativeSuite) row(h agentbackend.ExecutionHandle, id string) inputdelivery.Record {
	s.t.Helper()
	gate := func(ctx context.Context, run func() error) error {
		file, err := os.OpenFile(s.config.TransitionLock, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		for {
			if err = unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB); err == nil {
				defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
				return run()
			}
			if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}
	store, err := inputdelivery.Open(s.f.Context, s.config.UserStateRoot, s.config.Workspace, gate)
	if err != nil {
		s.t.Fatal(err)
	}
	defer store.Close(s.f.Context)
	row, err := store.Get(s.f.Context, inputdelivery.ConversationScope(h.Conversation), id)
	if err != nil {
		s.t.Fatal(err)
	}
	return row
}

// Run this single setup/detach check on the revised immutable candidate before
// Recovery or a performance cell. It exercises production concrete endpoints,
// their persisted topology and shared-host ownership, not a wrapper substitute.
func TestPiMessageDeliveryNativeBootstrap(t *testing.T) {
	s := newDeliveryNativeSuite(t)
	sibling, member := s.handles[0], s.handles[1]
	for _, handle := range s.handles {
		s.f.Idle(handle)
	}
	topology := func(handle agentbackend.ExecutionHandle) (int, string, bool) {
		root, err := piruntime.SessionStateDirectory(s.config.UserStateRoot, handle.Conversation.Session)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "members", handle.Conversation.Member, handle.Conversation.Incarnation, "conversation.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			WorkerPID      int
			WorkerTopology string
			Loaded         bool
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		return record.WorkerPID, record.WorkerTopology, record.Loaded
	}
	before, _ := s.f.History(t, member)
	siblingPID, kind, loaded := topology(sibling)
	if siblingPID <= 0 || kind != "shared-session-v1" || !loaded {
		t.Fatal("actual shared sibling topology", siblingPID, kind, loaded)
	}
	result, err := s.f.Manager.Unload(s.f.Context, member)
	if err != nil || !result.VerifiedDetach || result.Outcome != "detached" {
		t.Fatal("ordinary concrete shared detach", result, err)
	}
	// Detach clears the member endpoint/Loaded flag while retaining historical
	// shared-host ownership. The surviving physical process has not exited.
	if pid, kind, loaded := topology(member); pid != siblingPID || kind != "shared-session-v1" || loaded {
		t.Fatal("member detach changed retained shared ownership or remained loaded", pid, kind, loaded)
	}
	if pid, kind, loaded := topology(sibling); pid != siblingPID || kind != "shared-session-v1" || !loaded {
		t.Fatal("member detach replaced shared sibling", pid, kind, loaded)
	}
	state, err := s.f.Manager.Load(s.f.Context, member)
	if err != nil || state.Conversation != member.Conversation || state.Generation != member.Generation {
		t.Fatal("ordinary same native member reload", state, err)
	}
	if pid, kind, loaded := topology(member); pid != siblingPID || kind != "shared-session-v1" || !loaded {
		t.Fatal("member reload changed shared ownership", pid, kind, loaded)
	}
	after, _ := s.f.History(t, member)
	if after != before {
		t.Fatal("ordinary member reload changed native history witness inventory", before, after)
	}
	bootstrap := nativeRef(piquestions.Caller{Handle: sibling}, s.intent.InitialInputID, "publish optional question and exact plan", "initial", s.intent.ProviderSelection.Members[0].Settings.Effective())
	proof, err := s.f.Manager.LookupQueuedInput(s.f.Context, bootstrap)
	if err != nil || proof.Status.Phase != "Accepted" || proof.Status.Ref != bootstrap || proof.Status.Witness == nil {
		t.Fatal("sibling original acceptance witness after member reload", proof, err)
	}
	s.write(map[string]any{"kind": "bootstrap_shared_reload", "member": member, "sibling": sibling, "sharedPID": siblingPID, "originalWitness": proof.Status.Witness, "history": after, "counts": s.f.Counts()})
}

func TestPiMessageDeliveryNativeRecovery(t *testing.T) {
	t.Run("idempotence_and_closed_response", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		r := s.input(h, "closed response original "+s.id(), "steer")
		lock, err := session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() { defer wg.Done(); s.post(h, r) }()
		}
		wg.Wait()
		changed := r
		changed.Message += " changed"
		response := s.request(s.f.Context, "POST", s.path(h), h, changed)
		if response.Code != 409 {
			t.Fatal(response.Code, response.Body.String())
		}
		// Complete the real handler with a writer whose client has gone away.
		body, _ := json.Marshal(r)
		request := httptest.NewRequest("POST", s.config.BaseURL+s.path(h), bytes.NewReader(body)).WithContext(s.f.Context)
		request.Header.Set("Origin", s.config.BaseURL)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Workspace-Conversation-Scope", inputdelivery.ConversationScope(h.Conversation))
		request.Header.Set("X-Workspace-Pi-Generation", h.Generation)
		s.server.Handler().ServeHTTP(&deliveryClosedWriter{header: http.Header{}}, request)
		if row := s.row(h, r.ClientUserMessageID); row.Status.State != "queued" || s.runtime.count(r.ClientUserMessageID) != 0 {
			t.Fatal("pre-claim row escaped real EX", row)
		}
		s.reopenLocked()
		lock.Close()
		s.assertAccepted(h, r)
		s.post(h, r)
		s.assertAccepted(h, r)
		_, users := s.f.History(t, h)
		if count := countExact(users, r.Message); count != 1 {
			t.Fatal("native user append count", users)
		}
	})
	for _, boundary := range []string{"claim_before_wire", "native_enqueue_reply_loss", "native_persistence_before_portal_result", "native_pending_process_loss"} {
		t.Run(boundary, func(t *testing.T) {
			s := newDeliveryNativeSuite(t)
			h := s.handles[0]
			id := s.id()
			message := "fault original " + id
			mode := "steer"
			if boundary == "native_pending_process_loss" {
				active := s.input(h, "fixture-delayed-turn:"+id, "steer")
				s.post(h, active)
				s.awaitGate(id)
				message = "unconsumed followUp " + id
				mode = "followUp"
			}
			r := s.input(h, message, mode)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			s.runtime.hooks(func(ctx context.Context, q piruntime.QueuedInputRequest) error {
				if q.Ref.OperationID != r.ClientUserMessageID {
					return nil
				}
				row := s.row(h, q.Ref.OperationID)
				if row.Status.State != "dispatching" {
					return errors.New("checked port entered before durable claim")
				}
				if boundary == "claim_before_wire" {
					once.Do(func() { close(entered) })
					select {
					case <-release:
						return piruntime.ErrUnavailable
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}, func(ctx context.Context, q piruntime.QueuedInputRequest, result piruntime.QueuedInputResult, err error) error {
				if q.Ref.OperationID != r.ClientUserMessageID {
					return nil
				}
				if boundary == "native_persistence_before_portal_result" {
					for {
						lookup, e := s.f.Manager.LookupQueuedInput(ctx, q.Ref)
						if e == nil && lookup.Status.Phase == "Accepted" {
							break
						}
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(time.Millisecond):
						}
					}
				}
				once.Do(func() { close(entered) })
				select {
				case <-release:
					return piruntime.ErrUnavailable
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			s.post(h, r)
			select {
			case <-entered:
			case <-s.f.Context.Done():
				t.Fatal(s.f.Context.Err())
			}
			if boundary == "native_pending_process_loss" {
				s.f.KillNative()
			}
			close(release)
			s.wait(h, r.ClientUserMessageID, "unknown")
			s.runtime.hooks(nil, nil)
			s.restart()
			if boundary == "claim_before_wire" || boundary == "native_pending_process_loss" {
				s.wait(h, r.ClientUserMessageID, "unknown")
				if s.runtime.count(r.ClientUserMessageID) != 1 {
					t.Fatal("ambiguous claim replayed")
				}
				want := 0
				if boundary == "native_pending_process_loss" {
					want = 1
				}
				if s.f.Counts().Enqueues[r.ClientUserMessageID] != want {
					t.Fatal("wrong native boundary", s.f.Counts())
				}
			} else {
				s.assertAccepted(h, r)
			}
			s.write(map[string]any{"kind": "recovery", "boundary": boundary, "portalRestart": "graceful Close/new Server, not process-loss", "nativeCounts": s.f.Counts(), "status": s.status(h, r.ClientUserMessageID)})
		})
	}
	t.Run("ready_successor_before_claim", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		r := s.input(h, "ready successor original "+s.id(), "steer")
		lock, err := session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
		if err != nil {
			t.Fatal(err)
		}
		s.post(h, r)
		s.runtime.selectionBlocked.Store(true)
		s.server.Close()
		s.reopenLocked()
		lock.Close()
		// The ordinary owner endpoint supplies publication/roster projection.
		s.upgrade(nil, false)
		s.runtime.selectionBlocked.Store(false)
		h = s.handles[0]
		s.assertAccepted(h, r)
		row := s.row(h, r.ClientUserMessageID)
		if row.Admission.Ref.OperationID != r.ClientUserMessageID || row.Admission.Selection.Runtime == row.DispatchRuntime || row.DispatchRuntime != s.f.Runtime {
			t.Fatal("successor rewrote admission or missed execution", row)
		}
	})
	for _, change := range []string{"settings", "policy", "instructions", "access", "member_incarnation", "lifecycle"} {
		t.Run("preclaim_"+change, func(t *testing.T) {
			s := newDeliveryNativeSuite(t)
			h := s.handles[0]
			if change == "access" || change == "member_incarnation" {
				h = s.handles[1]
			}
			r := s.input(h, "frozen selection "+s.id(), "steer")
			lock, err := session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
			if err != nil {
				t.Fatal(err)
			}
			s.post(h, r)
			var verifyIncarnationFault func()
			switch change {
			case "settings":
				selected, err := s.f.Manager.ReadInputSelection(s.f.Context, h)
				if err != nil {
					t.Fatal(err)
				}
				settings := selected.Settings
				settings.Thinking = "off"
				settings.Revision++
				if _, err := s.f.Manager.Configure(s.f.Context, h, settings); err != nil {
					t.Fatal(err)
				}
			case "policy":
				proof, err := s.f.Manager.InspectToolPolicy(s.f.Context, h)
				if err != nil {
					t.Fatal(err)
				}
				target := proof.Policy
				target.Mode = "plan"
				target.Revision++
				target.Active = []string{}
				target.Digest = piruntime.ToolPolicyDigest(target)
				if _, err := s.f.Manager.ApplyToolPolicy(s.f.Context, h, proof.Policy.Revision, target); err != nil {
					t.Fatal(err)
				}
			case "instructions":
				s.f.ChangeInstructions(t, h)
			case "access":
				store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				roster, err := store.LoadPi(s.intent.Slug)
				if err != nil {
					t.Fatal(err)
				}
				for i := range roster.Members {
					if roster.Members[i].Address == h.Conversation.Member {
						if roster.Members[i].Access == "read_only" {
							roster.Members[i].Access = "workspace_write"
						} else {
							roster.Members[i].Access = "read_only"
						}
					}
				}
				roster.Revision++
				roster.UpdatedAt = time.Now().UTC()
				if roster.Validate() != nil {
					t.Fatal("invalid access fault")
				}
				path := filepath.Join(userstate.WorkspaceDirectory(s.config.UserStateRoot, "codex-teams", s.config.Workspace), s.intent.Slug+".json")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				s.runtime.selectionBlocked.Store(true)
				lock.Close()
				lock = nil
				// This ordinary writer has no ready-publication grant for an access
				// replacement. Its refusal must leave the actual owner bytes intact.
				if err := store.SavePi(s.f.Context, s.server, h.Generation, roster.Revision-1, roster); err == nil {
					t.Fatal("ordinary writer accepted unsupported access replacement")
				}
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(original, after) {
					t.Fatal("refused writer changed owner bytes", err)
				}
				lock, err = session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
				if err != nil {
					t.Fatal(err)
				}
				// Deliberate typed disposable state fault, not an access-edit API.
				centralJSON(t, path, roster)
			case "member_incarnation":
				store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
				beforeRow := s.row(h, r.ClientUserMessageID)
				if beforeRow.Status.State != "queued" || beforeRow.RetryDelayMS != 0 {
					t.Fatal("incarnation fault requires a fresh unclaimed row", beforeRow)
				}
				originalAdmission := mustDeliveryJSON(t, beforeRow.Admission)
				beforeHistory, _ := s.f.History(t, h)
				verifyIncarnationFault = func() {
					after := s.row(h, r.ClientUserMessageID)
					if !bytes.Equal(originalAdmission, mustDeliveryJSON(t, after.Admission)) {
						t.Fatal("owner fault rewrote original admission or ID", after)
					}
					history, users := s.f.History(t, h)
					if history != beforeHistory || countExact(users, r.Message) != 0 {
						t.Fatal("owner-incarnation fault appended native input", history, beforeHistory, users)
					}
				}
				path, original, fault := deliveryRefusedIncarnation(t, s.f.Context, store, s.intent.Slug, h.Conversation.Member)
				s.write(map[string]any{"kind": "owner_incarnation_fault_storage", "originalID": r.ClientUserMessageID, "originalBindingBytes": string(original), "fault": fault, "note": "unsupported typed owner-incarnation fault; shape validation grants no ownership"})
				// All setup publications are complete and runtime EX excludes the
				// ordinary owner writers. Keep authority/roster/native mapping old.
				centralJSON(t, path, fault)
			case "lifecycle":
				centralJSON(t, filepath.Join(s.config.Workspace, "worktrees", ".locks", s.intent.Slug+".archive.json"), map[string]any{"schema": 2, "phase": "fixture-reserved"})
			}
			if lock != nil {
				lock.Close()
			}
			s.runtime.selectionBlocked.Store(false)
			if change == "access" || change == "member_incarnation" {
				centralWait(t, s.f.Context, "actual refused owner dispatch attempt", func() bool {
					row := s.row(h, r.ClientUserMessageID)
					return row.RetryDelayMS > 0 || row.Status.State == "not_sent"
				})
				row := s.row(h, r.ClientUserMessageID)
				if row.Status.State != "queued" && row.Status.State != "not_sent" {
					t.Fatal("wrong selected-owner refusal boundary", row)
				}
				note := "disposable typed owner replacement; not a supported access-edit API; earlier strict joins may keep work unavailable"
				if change == "member_incarnation" {
					note = "unsupported owner-incarnation fault; not a supported member replacement; failed strict joins may leave queued/backoff"
				}
				s.write(map[string]any{"kind": "selected_owner_fault", "change": change, "status": row.Status, "note": note})
			} else {
				centralWait(t, s.f.Context, "positive pre-claim refusal", func() bool { return s.row(h, r.ClientUserMessageID).Status.State == "not_sent" })
			}
			if s.runtime.count(r.ClientUserMessageID) != 0 || s.f.Counts().Enqueues[r.ClientUserMessageID] != 0 {
				t.Fatal("changed selected tuple crossed native claim")
			}
			if verifyIncarnationFault != nil {
				verifyIncarnationFault()
			}
		})
	}
	t.Run("negative_proof_and_late_duplicate", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		r := s.input(h, "refused draft original "+s.id(), "steer")
		s.changeSettings(h)
		response := s.request(s.f.Context, "POST", s.path(h), h, r)
		if response.Code != 409 || !bytes.Contains(response.Body.Bytes(), []byte(`"notQueued":true`)) {
			t.Fatal(response.Code, response.Body.String())
		}
		s.restart()
		response = s.request(s.f.Context, "POST", s.path(h), h, r)
		if response.Code != 409 || !bytes.Contains(response.Body.Bytes(), []byte(`"notQueued":true`)) {
			t.Fatal(response.Code, response.Body.String())
		}
		if s.runtime.count(r.ClientUserMessageID) != 0 || s.f.Counts().Enqueues[r.ClientUserMessageID] != 0 {
			t.Fatal("negative identity acquired native work")
		}
		fresh := s.input(h, "fresh explicit input after refusal "+s.id(), "steer")
		s.post(h, fresh)
		s.assertAccepted(h, fresh)
		response = s.request(s.f.Context, "POST", s.path(h), h, r)
		if response.Code != 409 || !bytes.Contains(response.Body.Bytes(), []byte(`"notQueued":true`)) {
			t.Fatal("late negative identity was replaced", response.Code, response.Body.String())
		}
	})
	t.Run("retirement_requires_completed_original_owner_proof", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		r := s.input(h, "retired unknown original "+s.id(), "steer")
		restore := s.f.FailReceipt(t, h, "native")
		s.post(h, r)
		s.wait(h, r.ClientUserMessageID, "unknown")
		restore()
		ref := s.row(h, r.ClientUserMessageID).Admission.Ref
		original := s.f.ReceiptBytes(t, ref)
		wrong := ref
		wrong.Digest = strings.Repeat("f", 64)
		s.upgrade(&wrong, true)
		s.f.RefuseRetirementStop(true)
		s.upgrade(&ref, true)
		lookup, err := s.f.Manager.LookupQueuedInput(s.f.Context, ref)
		if err == nil && lookup.RetiredUnknown {
			t.Fatal("prepared retirement released Unknown")
		}
		s.f.RefuseRetirementStop(false)
		s.upgrade(&ref, false)
		h = s.handles[0]
		centralWait(t, s.f.Context, "owner-proved retired disposition", func() bool { return s.status(h, r.ClientUserMessageID).Retired })
		s.restart()
		if status := s.status(h, r.ClientUserMessageID); status.State != "unknown" || !status.Retired {
			t.Fatal(status)
		}
		if !bytes.Equal(original, s.f.ReceiptBytes(t, ref)) || s.runtime.count(r.ClientUserMessageID) != 1 {
			t.Fatal("retirement rewrote or replayed original")
		}
		next := s.input(h, "distinct after retirement "+s.id(), "steer")
		s.post(h, next)
		s.assertAccepted(h, next)
	})
	t.Run("native_followup_then_steer", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		gate := s.id()
		active := s.input(h, "fixture-delayed-turn:"+gate, "steer")
		s.post(h, active)
		s.awaitGate(gate)
		follow := s.input(h, "followUp original "+s.id(), "followUp")
		s.post(h, follow)
		s.wait(h, follow.ClientUserMessageID, "pending")
		steer := s.input(h, "steer original "+s.id(), "steer")
		s.post(h, steer)
		s.wait(h, steer.ClientUserMessageID, "pending")
		s.releaseGate(gate)
		s.assertAccepted(h, follow)
		s.assertAccepted(h, steer)
		s.f.Idle(h)
		_, users := s.f.History(t, h)
		if slices.Index(users, steer.Message) < 0 || slices.Index(users, steer.Message) > slices.Index(users, follow.Message) {
			t.Fatal("native consumption order", users)
		}
	})
	t.Run("missing_and_malformed_original_storage", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		r := s.input(h, "storage fault original "+s.id(), "steer")
		persisted := make(chan struct{})
		var once sync.Once
		s.runtime.hooks(nil, func(ctx context.Context, q piruntime.QueuedInputRequest, _ piruntime.QueuedInputResult, _ error) error {
			if q.Ref.OperationID != r.ClientUserMessageID {
				return nil
			}
			for {
				lookup, err := s.f.Manager.LookupQueuedInput(ctx, q.Ref)
				if err == nil && lookup.Status.Phase == "Accepted" {
					once.Do(func() { close(persisted) })
					<-ctx.Done()
					return ctx.Err()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
		})
		s.post(h, r)
		select {
		case <-persisted:
		case <-s.f.Context.Done():
			t.Fatal(s.f.Context.Err())
		}
		// Graceful Close cancels the pending portal result after native persistence,
		// joining all portal writers while retaining its durable dispatch claim.
		s.server.Close()
		s.runtime.hooks(nil, nil)
		ref := s.row(h, r.ClientUserMessageID).Admission.Ref
		// Quarantine joins native receipt continuations before reading/moving
		// the original. A new Load/idle probe has no post-Close workflow grant.
		original, malformed, restore := s.f.QuarantineReceipt(t, ref)
		t.Cleanup(func() { s.server.Close(); restore() })
		s.restart()
		s.wait(h, r.ClientUserMessageID, "unknown")
		_, users := s.f.History(t, h)
		if countExact(users, r.Message) != 1 || s.f.Counts().Enqueues[r.ClientUserMessageID] != 1 {
			t.Fatal("missing original caused native replay")
		}
		s.server.Close()
		malformed()
		before := s.runtime.lookupCount(r.ClientUserMessageID)
		s.restart()
		centralWait(t, s.f.Context, "malformed receipt actually looked up", func() bool { return s.runtime.lookupCount(r.ClientUserMessageID) > before })
		if s.status(h, r.ClientUserMessageID).State != "unknown" || s.f.Counts().Enqueues[r.ClientUserMessageID] != 1 {
			t.Fatal("malformed original accepted/replayed")
		}
		s.server.Close()
		restore()
		if !bytes.Equal(original, s.f.ReceiptBytes(t, ref)) {
			t.Fatal("quarantine changed original bytes")
		}
		s.restart()
		s.assertAccepted(h, r)
		_, users = s.f.History(t, h)
		if countExact(users, r.Message) != 1 {
			t.Fatal("storage restoration replayed input")
		}
	})
	t.Run("unloaded_preclaim_waits_without_implicit_load", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		r := s.input(h, "unloaded original "+s.id(), "steer")
		lock, err := session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
		if err != nil {
			t.Fatal(err)
		}
		s.post(h, r)
		if _, err := s.f.Manager.Unload(s.f.Context, h); err != nil {
			t.Fatal(err)
		}
		before := s.f.Counts()
		lock.Close()
		centralWait(t, s.f.Context, "actual unavailable selection attempt", func() bool { return s.row(h, r.ClientUserMessageID).RetryDelayMS > 0 })
		if s.runtime.count(r.ClientUserMessageID) != 0 || s.f.Counts().HostStarts != before.HostStarts {
			t.Fatal("queue implicitly loaded or started unavailable owner")
		}
		if _, err := s.f.Manager.ReadInputSelection(s.f.Context, h); !errors.Is(err, piruntime.ErrUnavailable) {
			t.Fatal("unloaded mapping gained readiness", err)
		}
		lock, err = session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.f.Manager.Load(s.f.Context, h); err != nil {
			t.Fatal(err)
		}
		lock.Close()
		s.assertAccepted(h, r)
	})
	t.Run("postclaim_settings_change_is_lookup_only", func(t *testing.T) {
		s := newDeliveryNativeSuite(t)
		h := s.handles[0]
		r := s.input(h, "postclaim immutable original "+s.id(), "steer")
		persisted := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		s.runtime.hooks(nil, func(ctx context.Context, q piruntime.QueuedInputRequest, _ piruntime.QueuedInputResult, _ error) error {
			if q.Ref.OperationID != r.ClientUserMessageID {
				return nil
			}
			for {
				status, err := s.f.Manager.LookupQueuedInput(ctx, q.Ref)
				if err == nil && status.Status.Phase == "Accepted" {
					once.Do(func() { close(persisted) })
					select {
					case <-release:
						return piruntime.ErrUnavailable
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
		})
		s.post(h, r)
		select {
		case <-persisted:
		case <-s.f.Context.Done():
			t.Fatal(s.f.Context.Err())
		}
		close(release)
		s.wait(h, r.ClientUserMessageID, "unknown")
		s.runtime.hooks(nil, nil)
		s.changeSettings(h)
		s.restart()
		s.assertAccepted(h, r)
		if s.row(h, r.ClientUserMessageID).Admission.Request.Selection != r.Selection {
			t.Fatal("settings change rewrote immutable admission")
		}
	})
}

// CAS publishes readiness within the original reservation; it cannot replace
// an incarnation. Return a shape-valid fault only after proving refusal leaves
// the exact owner bytes intact. Raw injection remains a separate fixture step.
func deliveryRefusedIncarnation(t *testing.T, ctx context.Context, store session.BackendBindingStore, slug, member string) (string, []byte, session.BackendBinding) {
	t.Helper()
	binding, err := store.Load(slug)
	if err != nil {
		t.Fatal(err)
	}
	previous := binding.Revision
	binding.Revision++
	found := false
	for i := range binding.Conversations {
		if binding.Conversations[i].Address == member {
			binding.Conversations[i].Incarnation = strings.Repeat("e", 64)
			found = true
		}
	}
	if !found || binding.Validate(store.Workspace, slug) != nil {
		t.Fatal("invalid typed incarnation fault")
	}
	workspaceSHA := sha256.Sum256([]byte(store.Workspace))
	path := filepath.Join(store.Root, "session-backends", fmt.Sprintf("%x", workspaceSHA), slug+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwap(ctx, previous, binding); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatal("ordinary CAS did not refuse reserved incarnation replacement", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused incarnation CAS changed owner bytes", err)
	}
	return path, before, binding
}

func TestPiMessageDeliveryIncarnationFaultPrerequisite(t *testing.T) {
	ctx := context.Background()
	store := session.BackendBindingStore{Root: t.TempDir(), Workspace: t.TempDir()}
	// A real creating reservation is sufficient to exercise immutable identity;
	// this pure codec check invents no ready native mapping or Accepted receipt.
	binding := session.BackendBinding{Schema: 1, Backend: agentbackend.Pi, BackendContract: 1, Workspace: store.Workspace, Slug: "2026-11-11-fixture", SessionIncarnation: strings.Repeat("a", 64), CreationReceiptID: strings.Repeat("c", 64), Revision: 1, State: "creating", Conversations: []session.BackendConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64), State: "creating"}, {Address: "implementer0", Incarnation: strings.Repeat("d", 64), State: "creating"}}}
	if _, err := store.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	path, _, fault := deliveryRefusedIncarnation(t, ctx, store, binding.Slug, "implementer0")
	if fault.Conversations[1].Incarnation != strings.Repeat("e", 64) || fault.Conversations[1].NativeID != "" {
		t.Fatal("fault invented native readiness", fault)
	}
	centralJSON(t, path, fault)
	loaded, err := store.Load(binding.Slug)
	if err != nil || !bytes.Equal(mustDeliveryJSON(t, loaded), mustDeliveryJSON(t, fault)) {
		t.Fatal("typed fault did not preserve readable shape", err)
	}
}

type deliveryClosedWriter struct{ header http.Header }

func (w *deliveryClosedWriter) Header() http.Header       { return w.header }
func (w *deliveryClosedWriter) WriteHeader(int)           {}
func (w *deliveryClosedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (s *deliveryNativeSuite) awaitGate(id string) {
	centralWait(s.t, s.f.Context, "actual faux response entered", func() bool { _, err := os.Stat(filepath.Join(s.f.Gates, id+".ready")); return err == nil })
}
func (s *deliveryNativeSuite) releaseGate(id string) {
	centralWrite(s.t, filepath.Join(s.f.Gates, id+".release"), []byte("release"))
}
func countExact(values []string, value string) int {
	count := 0
	for _, v := range values {
		if v == value {
			count++
		}
	}
	return count
}

func (s *deliveryNativeSuite) changeSettings(h agentbackend.ExecutionHandle) {
	s.t.Helper()
	selected, err := s.f.Manager.ReadInputSelection(s.f.Context, h)
	if err != nil {
		s.t.Fatal(err)
	}
	settings := selected.Settings
	settings.Thinking = "off"
	if settings.Thinking == selected.Settings.Thinking {
		settings.Thinking = "high"
	}
	settings.Revision++
	if _, err := s.f.Manager.Configure(s.f.Context, h, settings); err != nil {
		s.t.Fatal(err)
	}
	if fresh, err := s.f.Manager.ReadInputSelection(s.f.Context, h); err != nil || fresh.Settings != settings {
		s.t.Fatal("settings did not settle before selection check", fresh, err)
	}
}

func (s *deliveryNativeSuite) upgrade(retire *piruntime.SubmissionRef, wantFailure bool) {
	s.t.Helper()
	id := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	body := map[string]string{"workspace": s.config.Workspace, "requestId": id, "expectedGeneration": s.f.Runtime.Generation}
	if retire != nil {
		body["retireUnknownInput"] = retire.OperationID
		body["inputDigest"] = retire.Digest
		body["member"] = retire.Conversation.Member
	}
	response := s.request(s.f.Context, "POST", "/api/sessions/"+s.intent.Slug+"/pi-runtime-upgrade", s.handles[0], body)
	if wantFailure {
		if response.Code >= 200 && response.Code < 300 {
			s.t.Fatal("unproved retirement succeeded", response.Body.String())
		}
		return
	}
	var result piruntime.UpgradeResult
	centralDecode(s.t, response, &result)
	if result.Runtime.Generation == s.f.Runtime.Generation {
		s.t.Fatal("no actual ready successor", result)
	}
	s.f.Runtime = result.Runtime
	for i := range s.handles {
		s.handles[i].Generation = result.Runtime.Generation
		if _, err := s.f.Manager.Load(s.f.Context, s.handles[i]); err != nil {
			s.t.Fatal("explicit successor load", err)
		}
	}
}

type deliveryNativeSample struct {
	started                                time.Time
	Kind, ID, Member, Mode, State, Failure string
	Warm                                   bool
	HTTPCode                               int
	HTTPUpperNS, AcceptanceUpperNS         int64
	PublicSentUpperNS                      int64
	EnqueueUpperNS                         int64
	Phases                                 []deliveryNativePhase
	Ref                                    piruntime.SubmissionRef
	Settings                               piruntime.EffectiveSettings
	Witness                                *piruntime.NativeWitness
	NativeExecution                        agentbackend.ExecutionHandle
	PublicReceipt                          *backend.SendReceipt
	HTTP                                   deliveryEndpointStats
	ProofPhase, ProofError                 string
	ProofElapsedNS                         int64
}
type deliveryNativePhase struct {
	State     string
	ElapsedNS int64
}

// Endpoint counts retain every issued request; representative evidence is bounded
// by method/endpoint/member/status/outcome, rather than by polling iterations.
type deliveryHTTPOutcome struct {
	Method, Endpoint, Member, OriginalID, Outcome string
	Code                                          int
	StartedNS, CompletedNS                        int64
	Evidence                                      any
}
type deliveryEndpointStats struct {
	Requests, Failures int
	Outcomes           map[string]int
	Evidence           map[string]deliveryHTTPOutcome
}

func (s *deliveryEndpointStats) record(o deliveryHTTPOutcome) {
	if s.Outcomes == nil {
		s.Outcomes = map[string]int{}
		s.Evidence = map[string]deliveryHTTPOutcome{}
	}
	s.Requests++
	if o.Code < 200 || o.Code >= 300 || strings.HasPrefix(o.Outcome, "invalid_json") {
		s.Failures++
	}
	key := fmt.Sprintf("%s %s %s HTTP%d %s", o.Method, o.Endpoint, o.Member, o.Code, o.Outcome)
	s.Outcomes[key]++
	s.Evidence[key] = o
}
func deliveryResponseIdentity(view *piquestions.ResponseView) any {
	if view == nil {
		return nil
	}
	return map[string]any{"responseId": view.ResponseID, "questionId": view.QuestionID, "questionRevision": view.QuestionRevision, "sequence": view.Sequence, "status": view.Status, "deliveryPhase": view.DeliveryPhase}
}
func deliveryHTTPOutcomeFor(method, path, member string, response *httptest.ResponseRecorder) deliveryHTTPOutcome {
	o := deliveryHTTPOutcome{Method: method, Endpoint: path, Member: member, Code: response.Code, Outcome: "ok"}
	var raw map[string]json.RawMessage
	if json.Unmarshal(response.Body.Bytes(), &raw) != nil {
		o.Outcome = "invalid_json"
		return o
	}
	if at := strings.Index(path, "/deliveries/"); at >= 0 {
		o.OriginalID = path[at+len("/deliveries/"):]
		o.Endpoint = path[:at] + "/deliveries/{id}"
	}
	if at := strings.Index(path, "/pi-questions/responses/"); at >= 0 {
		o.OriginalID = path[at+len("/pi-questions/responses/"):]
		o.Endpoint = path[:at] + "/pi-questions/responses/{id}"
	}
	evidence := map[string]any{}
	for _, key := range []string{"error", "code", "state", "failure", "disposition", "canEnqueue", "retired", "responseId", "questionId", "questionRevision", "sequence", "status", "deliveryPhase", "receipt"} {
		if value, ok := raw[key]; ok {
			evidence[key] = value
		}
	}
	o.Evidence = evidence
	if response.Code < 200 || response.Code >= 300 {
		code, _ := json.Marshal(evidence)
		o.Outcome = "http_error:" + string(code)
		return o
	}
	switch {
	case strings.Contains(path, "pi-questions") && !strings.Contains(path, "/responses"):
		var snapshot piquestions.Snapshot
		if json.Unmarshal(response.Body.Bytes(), &snapshot) != nil {
			o.Outcome = "invalid_json_questions"
			break
		}
		questions := []map[string]any{}
		states := []string{}
		for _, q := range snapshot.Questions {
			options := map[string][]string{}
			for _, item := range q.Questions {
				for _, option := range item.Options {
					options[item.ID] = append(options[item.ID], option.ID)
				}
			}
			questions = append(questions, map[string]any{"id": q.ID, "revision": q.Revision, "status": q.Status, "canRespond": q.CanRespond, "options": options, "latestResponse": deliveryResponseIdentity(q.LatestResponse)})
			states = append(states, fmt.Sprintf("%s/respondable=%t", q.Status, q.CanRespond))
		}
		evidence["revision"] = snapshot.Revision
		evidence["questions"] = questions
		o.Outcome = "questions:" + strings.Join(states, ",")
	case strings.Contains(path, "/pi-questions/responses"):
		var view piquestions.ResponseView
		_ = json.Unmarshal(response.Body.Bytes(), &view)
		o.Outcome = "response:" + view.Status + "/" + view.DeliveryPhase
	case strings.Contains(path, "/deliveries/") || method == "POST" && strings.HasSuffix(path, "/deliveries"):
		var status backend.DeliveryStatus
		_ = json.Unmarshal(response.Body.Bytes(), &status)
		o.Outcome = "delivery:" + status.State
		o.OriginalID = status.ClientUserMessageID
	case strings.HasSuffix(path, "/deliveries"):
		var collection backend.DeliveryCollection
		_ = json.Unmarshal(response.Body.Bytes(), &collection)
		o.Outcome = fmt.Sprintf("collection:canEnqueue=%t", collection.CanEnqueue)
	}
	return o
}

type deliveryNativeActivity struct {
	mu                 sync.Mutex
	HTTP               deliveryEndpointStats
	MaxQueueDepth      int
	OldestWaitingMS    int64
	QuestionID         string // attempted response ID; admission must be proved separately
	QuestionPost       *deliveryHTTPOutcome
	QuestionPersisted  *piquestions.ResponseView
	QuestionAcceptedNS int64
}

type deliveryNativeActivitySnapshot struct {
	ObservedAt         time.Time
	HTTP               deliveryEndpointStats
	MaxQueueDepth      int
	OldestWaitingMS    int64
	QuestionID         string
	QuestionPost       *deliveryHTTPOutcome
	QuestionPersisted  *piquestions.ResponseView
	QuestionAcceptedNS int64
}

func (a *deliveryNativeActivity) snapshot() deliveryNativeActivitySnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, _ := json.Marshal(deliveryNativeActivitySnapshot{ObservedAt: time.Now(), HTTP: a.HTTP, MaxQueueDepth: a.MaxQueueDepth, OldestWaitingMS: a.OldestWaitingMS, QuestionID: a.QuestionID, QuestionPost: a.QuestionPost, QuestionPersisted: a.QuestionPersisted, QuestionAcceptedNS: a.QuestionAcceptedNS})
	var copy deliveryNativeActivitySnapshot
	_ = json.Unmarshal(data, &copy)
	// Preserve the local monotonic clock used with these exact cloned counts.
	copy.ObservedAt = time.Now()
	return copy
}
func deliveryActivityRate(before, after deliveryNativeActivitySnapshot) float64 {
	elapsed := after.ObservedAt.Sub(before.ObservedAt).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(after.HTTP.Requests-before.HTTP.Requests) / elapsed
}
func deliveryPersistedResponse(response *httptest.ResponseRecorder, input piquestions.ResponseInput) *piquestions.ResponseView {
	var view piquestions.ResponseView
	if response.Code >= 200 && response.Code < 300 && json.Unmarshal(response.Body.Bytes(), &view) == nil && view.ResponseID == input.ResponseID && view.QuestionID == input.QuestionID && view.QuestionRevision == input.QuestionRevision && view.Sequence > 0 {
		return &view
	}
	return nil
}

// These public payload cases distinguish a real admission from a generated ID,
// and preserve unavailable versus respondable question evidence under polling.
func TestPiMessageDeliveryObservationEvidence(t *testing.T) {
	input := piquestions.ResponseInput{ResponseID: strings.Repeat("1", 32), QuestionID: "question", QuestionRevision: 7}
	for _, kind := range []string{"exact", "rejected", "wrong_id", "wrong_question", "wrong_revision", "unpersisted", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			response := httptest.NewRecorder()
			view := piquestions.ResponseView{ResponseInput: input, Sequence: 3, Status: "open", DeliveryPhase: "Unknown"}
			switch kind {
			case "rejected":
				response.Code = 409
			case "wrong_id":
				view.ResponseID = "other"
			case "wrong_question":
				view.QuestionID = "other"
			case "wrong_revision":
				view.QuestionRevision++
			case "unpersisted":
				view.Sequence = 0
			}
			if kind == "malformed" {
				response.Body.WriteString("not-json")
			} else {
				_ = json.NewEncoder(response.Body).Encode(view)
			}
			if got := deliveryPersistedResponse(response, input); (got != nil) != (kind == "exact") {
				t.Fatalf("unproved response admission %s %+v", kind, got)
			}
		})
	}
	stats := deliveryEndpointStats{}
	for _, respondable := range []bool{false, false, true} {
		response := httptest.NewRecorder()
		_ = json.NewEncoder(response.Body).Encode(piquestions.Snapshot{Revision: 9, Questions: []piquestions.QuestionView{{ID: "question", Revision: 7, Status: "open", CanRespond: respondable, Questions: []piquestions.Question{{ID: "choice", Options: []piquestions.Option{{ID: "option", Label: "label excluded from evidence"}}}}}}})
		stats.record(deliveryHTTPOutcomeFor("GET", "/api/sessions/test/pi-questions?member=lead", "lead", response))
	}
	collection := httptest.NewRecorder()
	collection.Body.WriteString(`{"canEnqueue":true,"entries":[]}`)
	stats.record(deliveryHTTPOutcomeFor("GET", "/codex/conversations/test/deliveries", "lead", collection))
	unavailable := httptest.NewRecorder()
	unavailable.Code = 503
	unavailable.Body.WriteString(`{"error":"operation unavailable","code":"unavailable"}`)
	stats.record(deliveryHTTPOutcomeFor("GET", "/codex/conversations/test/deliveries/original", "lead", unavailable))
	invalid := httptest.NewRecorder()
	invalid.Body.WriteString("invalid")
	stats.record(deliveryHTTPOutcomeFor("GET", "/api/sessions/test/pi-questions", "lead", invalid))
	if stats.Requests != 6 || stats.Failures != 2 || len(stats.Outcomes) != 5 {
		t.Fatal("lost polling/collection/failure evidence", stats)
	}
	data, _ := json.Marshal(stats)
	if !bytes.Contains(data, []byte(`respondable=false`)) || !bytes.Contains(data, []byte(`respondable=true`)) || !bytes.Contains(data, []byte(`operation unavailable`)) || bytes.Contains(data, []byte(`label excluded`)) {
		t.Fatal("question/refusal evidence changed", string(data))
	}
	// Warm and post-measurement requests must not leak into the measured rate.
	before := deliveryNativeActivitySnapshot{ObservedAt: time.Unix(100, 0), HTTP: deliveryEndpointStats{Requests: 20}}
	after := deliveryNativeActivitySnapshot{ObservedAt: time.Unix(102, 0), HTTP: deliveryEndpointStats{Requests: 26}}
	if deliveryActivityRate(before, after) != 3 {
		t.Fatal("rate used an unaligned count interval")
	}
}

func (s *deliveryNativeSuite) background(measuring <-chan struct{}, churn bool) (func(), *deliveryNativeActivity) {
	ctx, cancel := context.WithCancel(s.f.Context)
	done := make(chan struct{})
	stats := &deliveryNativeActivity{}
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var answerID string
		canAnswer := false
		request := func(method, path string, h agentbackend.ExecutionHandle, input any) (*httptest.ResponseRecorder, deliveryHTTPOutcome) {
			start := time.Since(s.started).Nanoseconds()
			response := s.request(ctx, method, path, h, input)
			outcome := deliveryHTTPOutcomeFor(method, path, h.Conversation.Member, response)
			outcome.StartedNS = start
			outcome.CompletedNS = time.Since(s.started).Nanoseconds()
			stats.mu.Lock()
			stats.HTTP.record(outcome)
			stats.mu.Unlock()
			return response, outcome
		}
		pass := func() {
			for _, h := range s.handles {
				paths := []string{strings.TrimSuffix(s.path(h), "deliveries") + "activity", "/api/sessions/" + s.intent.Slug + "/pi-questions?member=" + h.Conversation.Member}
				if churn && h == s.handles[len(s.handles)-1] {
					for range 20 {
						paths = append(paths, paths[1])
					}
				}
				for _, path := range paths {
					response, _ := request("GET", path, h, nil)
					if !canAnswer || h.Conversation.Member != "lead" || !strings.Contains(path, "pi-questions") {
						continue
					}
					var snapshot piquestions.Snapshot
					if response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &snapshot) == nil && len(snapshot.Questions) > 0 && answerID == "" {
						q := snapshot.Questions[0]
						if q.CanRespond && len(q.Questions) > 0 && len(q.Questions[0].Options) > 0 {
							answerID = s.id()
							input := piquestions.ResponseInput{ResponseID: answerID, QuestionID: q.ID, QuestionRevision: q.Revision, Answers: []piquestions.Answer{{QuestionID: q.Questions[0].ID, ChoiceID: q.Questions[0].Options[0].ID}}}
							posted, outcome := request("POST", "/api/sessions/"+s.intent.Slug+"/pi-questions/responses", h, input)
							persisted := deliveryPersistedResponse(posted, input)
							stats.mu.Lock()
							stats.QuestionID = answerID
							stats.QuestionPost = &outcome
							if persisted != nil {
								stats.QuestionPersisted = persisted
							}
							stats.mu.Unlock()
						}
					}
				}
				if h.Conversation.Member == "lead" && answerID != "" {
					response, _ := request("GET", "/api/sessions/"+s.intent.Slug+"/pi-questions/responses/"+answerID, h, nil)
					var view piquestions.ResponseView
					if response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &view) == nil && view.DeliveryPhase == "Accepted" {
						stats.mu.Lock()
						if stats.QuestionAcceptedNS == 0 && stats.QuestionPersisted != nil && view.ResponseID == answerID && view.Sequence == stats.QuestionPersisted.Sequence && view.QuestionID == stats.QuestionPersisted.QuestionID && view.QuestionRevision == stats.QuestionPersisted.QuestionRevision {
							stats.QuestionAcceptedNS = time.Since(s.started).Nanoseconds()
						}
						stats.mu.Unlock()
					}
				}
				response, _ := request("GET", s.path(h), h, nil)
				var collection backend.DeliveryCollection
				if response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &collection) == nil {
					depth := 0
					oldest := int64(0)
					for _, entry := range collection.Entries {
						if (entry.State == "queued" || entry.State == "dispatching" || entry.State == "pending" || entry.State == "unknown") && !entry.Retired {
							depth++
							wait := time.Now().UnixMilli() - entry.CreatedAtMS
							if wait > oldest {
								oldest = wait
							}
						}
					}
					stats.mu.Lock()
					stats.MaxQueueDepth = max(stats.MaxQueueDepth, depth)
					stats.OldestWaitingMS = max(stats.OldestWaitingMS, oldest)
					stats.mu.Unlock()
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-measuring:
				canAnswer = true
				measuring = nil
				pass()
			case <-ticker.C:
				pass()
			}
		}
	}()
	return func() { cancel(); <-done }, stats
}

func (s *deliveryNativeSuite) sample(h agentbackend.ExecutionHandle, r backend.DeliveryRequest, warm bool, postOnly bool) deliveryNativeSample {
	sample := deliveryNativeSample{Kind: "sample", ID: r.ClientUserMessageID, Member: h.Conversation.Member, Mode: r.Delivery, Warm: warm}
	// The complete encoded body is already available. Handler duration is a
	// conservative durable-commit upper bound, never a CreatedAt timestamp.
	body, _ := json.Marshal(r)
	start := time.Now()
	sample.started = start
	response := s.encodedRequest(s.f.Context, "POST", s.path(h), h, body)
	sample.HTTPUpperNS = time.Since(start).Nanoseconds()
	sample.HTTPCode = response.Code
	sample.HTTP.record(deliveryHTTPOutcomeFor("POST", s.path(h), h.Conversation.Member, response))
	var status backend.DeliveryStatus
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 202 {
		sample.State = "http_failure"
		sample.Failure = response.Body.String()
		return sample
	}
	sample.Phases = append(sample.Phases, deliveryNativePhase{status.State, time.Since(start).Nanoseconds()})
	if postOnly {
		sample.State = status.State
		return sample
	}
	return s.finishSample(h, r, start, sample)
}
func (s *deliveryNativeSuite) finishSample(h agentbackend.ExecutionHandle, r backend.DeliveryRequest, start time.Time, sample deliveryNativeSample) deliveryNativeSample {
	if !sample.started.IsZero() {
		start = sample.started
	}
	ctx, cancel := context.WithTimeout(s.f.Context, 75*time.Second)
	defer cancel()
	last := ""
	for {
		response := s.request(ctx, "GET", s.path(h)+"/"+r.ClientUserMessageID, h, nil)
		sample.HTTP.record(deliveryHTTPOutcomeFor("GET", s.path(h)+"/"+r.ClientUserMessageID, h.Conversation.Member, response))
		var status backend.DeliveryStatus
		if response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &status) == nil {
			if status.State != last {
				sample.Phases = append(sample.Phases, deliveryNativePhase{status.State, time.Since(start).Nanoseconds()})
				last = status.State
			}
			sample.State = status.State
			if status.State == "pending" && sample.EnqueueUpperNS == 0 {
				sample.EnqueueUpperNS = time.Since(start).Nanoseconds()
			}
			if status.State == "sent" {
				sample.PublicSentUpperNS = time.Since(start).Nanoseconds()
				accepted, observed := s.f.AcceptedUpper(h, r.ClientUserMessageID)
				if observed && accepted.Handle == h && !accepted.ObservedAt.Before(start) {
					sample.AcceptanceUpperNS = accepted.ObservedAt.Sub(start).Nanoseconds()
					sample.NativeExecution = accepted.Handle
				} else {
					sample.State = "endpoint_unavailable"
					sample.Failure = "missing exact owned post-persistence input_commit_v1 observation"
				}
				sample.PublicReceipt = status.Receipt
				break
			}
			if status.State == "not_sent" || status.State == "unknown" {
				sample.Failure = status.Failure
				break
			}
		} else {
			sample.Failure = fmt.Sprintf("status HTTP%d: %s", response.Code, response.Body.String())
		}
		select {
		case <-ctx.Done():
			sample.State = "timeout"
			sample.Failure = ctx.Err().Error()
			return sample
		case <-time.After(10 * time.Millisecond):
		}
	}
	return sample
}

// This extra fixture proof runs only after every timed endpoint in the batch.
// Production reconciliation remains in the timed path. Even failed endpoints
// receive an independent original-ref lookup; they are never filtered away.
func (s *deliveryNativeSuite) proveSample(h agentbackend.ExecutionHandle, r backend.DeliveryRequest, sample deliveryNativeSample) deliveryNativeSample {
	if sample.HTTPCode != 202 {
		return sample
	}
	row := s.row(h, r.ClientUserMessageID)
	ref := row.Admission.Ref
	ctx, task := trace.NewTask(s.f.Context, "pi.fixture.independentProof")
	defer task.End()
	trace.Log(ctx, "originalID", r.ClientUserMessageID)
	proofStart := time.Now()
	lookup, err := s.f.Manager.LookupQueuedInput(ctx, ref)
	sample.ProofElapsedNS = time.Since(proofStart).Nanoseconds()
	sample.ProofPhase = lookup.Status.Phase
	if err != nil {
		sample.ProofError = err.Error()
	}
	sample.Ref = lookup.Status.Ref
	sample.Settings = lookup.Status.Settings
	sample.Witness = lookup.Status.Witness
	if sample.PublicSentUpperNS > 0 && (err != nil || lookup.Status.Phase != "Accepted" || lookup.Status.Ref != ref || lookup.Status.Settings != row.Admission.Selection.Settings || lookup.Status.Witness == nil || sample.PublicReceipt == nil || lookup.Status.Witness.EntryID != sample.PublicReceipt.TurnID || !reflect.DeepEqual(lookup.Status.Input, row.Admission.Input)) {
		sample.State = "witness_failure"
		sample.Failure = fmt.Sprint(lookup, err)
	}
	return sample
}

// Only cheap original-ID counters join the deferred receipt proof here. Native
// Idle and the independent disk-history audit remain at the end of the cell.
func (s *deliveryNativeSuite) proveBatchCounts(handles []agentbackend.ExecutionHandle, requests []backend.DeliveryRequest, samples []deliveryNativeSample) {
	counts := s.f.Counts()
	for j, h := range handles {
		enqueues := counts.Enqueues[requests[j].ClientUserMessageID]
		checked := s.runtime.count(requests[j].ClientUserMessageID)
		s.write(map[string]any{"kind": "batch_original_counts", "originalID": requests[j].ClientUserMessageID, "member": h.Conversation.Member, "state": samples[j].State, "enqueues": enqueues, "checkedEntries": checked})
		if enqueues > 1 || checked > 1 || samples[j].State == "sent" && (enqueues != 1 || checked != 1) {
			s.t.Errorf("original input skipped/replayed: %s enqueue%d checked%d", requests[j].ClientUserMessageID, enqueues, checked)
		}
	}
}

// This diagnostic deliberately has no percentile or sustained-fairness claim.
// Run once with -count=1 -timeout=3m and -trace=<owned temporary artifact>.
func TestPiMessageDeliveryNativeStageDiagnostic(t *testing.T) {
	s := newDeliveryNativeSuite(t)
	setup := time.Now()
	for _, h := range s.handles {
		s.f.SeedHistory(t, h, 10000)
	}
	s.write(map[string]any{"kind": "diagnostic_history_setup", "entriesPerConversation": 10000, "seedAndLoadNS": time.Since(setup).Nanoseconds()})
	question := s.input(s.handles[0], "fixture-real-question "+s.id(), "steer")
	s.post(s.handles[0], question)
	s.assertAccepted(s.handles[0], question)
	centralWait(t, s.f.Context, "real diagnostic optional question exists", func() bool {
		response := s.request(s.f.Context, "GET", "/api/sessions/"+s.intent.Slug+"/pi-questions", s.handles[0], nil)
		var snapshot piquestions.Snapshot
		return response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &snapshot) == nil && len(snapshot.Questions) > 0
	})
	measuring := make(chan struct{})
	s.diagnosticInterval.Store("initial")
	stop, activity := s.background(measuring, false)
	defer stop()
	select {
	case <-time.After(6 * time.Second):
	case <-s.f.Context.Done():
		t.Fatal(s.f.Context.Err())
	}
	handles := s.handles[:4]
	requests := make([]backend.DeliveryRequest, 4)
	for j, h := range handles {
		id := s.id()
		requests[j] = backend.DeliveryRequest{Version: 1, ClientUserMessageID: id, Message: "diagnostic native literal " + id, Delivery: "steer", Selection: s.metadata(h).Selection}
	}
	before := s.f.CounterSnapshot()
	s.diagnosticInterval.Store("batch")
	close(measuring)
	samples := make([]deliveryNativeSample, 4)
	var wg sync.WaitGroup
	for j, h := range handles {
		wg.Add(1)
		go func(j int, h agentbackend.ExecutionHandle) {
			defer wg.Done()
			samples[j] = s.sample(h, requests[j], false, false)
		}(j, h)
	}
	wg.Wait()
	// All public endpoints/native-hook observations finish before fixture proofs.
	s.diagnosticInterval.Store("quiet")
	quiet := time.NewTimer(10 * time.Second)
	defer quiet.Stop()
	for j, h := range handles {
		samples[j] = s.proveSample(h, requests[j], samples[j])
		s.write(samples[j])
	}
	s.proveBatchCounts(handles, requests, samples)
	select {
	case <-quiet.C:
	case <-s.f.Context.Done():
		t.Error(s.f.Context.Err())
	}
	stop()
	after := s.f.CounterSnapshot()
	s.diagnosticInterval.Store("")
	for j, h := range handles {
		s.f.Idle(h)
		inventory, users := s.f.History(t, h)
		count := countExact(users, requests[j].Message)
		s.write(map[string]any{"kind": "diagnostic_final_history", "member": h.Conversation.Member, "originalID": requests[j].ClientUserMessageID, "nativeUserAppends": count, "history": inventory})
		if count > 1 || samples[j].State == "sent" && count != 1 {
			t.Errorf("original native append count %s=%d", requests[j].ClientUserMessageID, count)
		}
	}
	s.write(map[string]any{"kind": "stage_diagnostic_summary", "historyEntries": 10000, "inputs": 4, "counterInterval": map[string]any{"start": before, "end": after}, "activity": activity.snapshot(), "diagnostics": s.runtime.diagnostics.String(), "postDiagnosticAuditEnd": s.f.CounterSnapshot(), "limits": "inclusive Manager intervals and serialized request-write boundaries; native reply completion and private publication predicates unavailable; no percentile or sustained-fairness conclusion; revised fixture proof workload differs from79dd"})
	for _, sample := range samples {
		if sample.State != "sent" {
			t.Errorf("diagnostic input retained incomplete: %s %s %s", sample.ID, sample.State, sample.Failure)
		}
	}
}

func TestPiMessageDeliveryNativePerformance(t *testing.T) {
	for _, history := range []struct {
		name    string
		entries int
	}{{"short", 0}, {"10000", 10000}, {"100000", 100000}} {
		t.Run(history.name, func(t *testing.T) {
			for _, workload := range []string{"idle_healthy", "team_activity", "failed_churn", "delayed_followUp"} {
				t.Run(workload, func(t *testing.T) {
					s := newDeliveryNativeSuite(t)
					setup := time.Now()
					before := map[string]any{}
					for _, h := range s.handles {
						s.f.SeedHistory(t, h, history.entries)
						inventory, _ := s.f.History(t, h)
						before[h.Conversation.Member] = inventory
					}
					s.write(map[string]any{"kind": "history_setup", "requestedEntries": history.entries, "perConversation": before, "seedAndLoadNS": time.Since(setup).Nanoseconds(), "memory": s.memory(), "counts": s.f.Counts()})
					// A real SDK host callback creates the pending optional question. Its
					// response competes with foreground work during the measured interval.
					question := s.input(s.handles[0], "fixture-real-question "+s.id(), "steer")
					s.post(s.handles[0], question)
					s.assertAccepted(s.handles[0], question)
					centralWait(t, s.f.Context, "real optional question published", func() bool {
						r := s.request(s.f.Context, "GET", "/api/sessions/"+s.intent.Slug+"/pi-questions", s.handles[0], nil)
						var snapshot piquestions.Snapshot
						return r.Code == 200 && json.Unmarshal(r.Body.Bytes(), &snapshot) == nil && len(snapshot.Questions) > 0
					})
					measuring := make(chan struct{})
					stopActivity, activity := s.background(measuring, workload == "failed_churn")
					defer stopActivity()
					activityWarmStart := activity.snapshot()
					var activityMeasuredStart deliveryNativeActivitySnapshot
					failedPeer := ""
					healthy := s.handles
					if workload == "failed_churn" {
						peer := s.handles[4]
						s.f.FailObservation(peer.Conversation.Member)
						failed := s.input(peer, "failed peer backlog "+s.id(), "steer")
						s.post(peer, failed)
						failedPeer = failed.ClientUserMessageID
						healthy = s.handles[:4]
					}
					selections := map[string]string{}
					for _, h := range healthy {
						selections[h.Conversation.Member] = s.metadata(h).Selection
					}
					warmStart := s.f.CounterSnapshot()
					var measured piruntime.PiMessageDeliveryCounterSnapshot
					var proofNS int64
					samples := []deliveryNativeSample{}
					measuredStart := time.Time{}
					foregroundFinished := time.Time{}
					// Bounded batches never approach the queue/native receipt capacities.
					for offset := 0; offset < 220; {
						warm := offset < 20
						if offset == 20 {
							activityMeasuredStart = activity.snapshot()
							measured = s.f.CounterSnapshot()
							measuredStart = measured.ObservedAt
							close(measuring)
						}
						batchSize := 1
						if workload == "team_activity" || workload == "failed_churn" {
							batchSize = 4
						}
						batchSize = min(batchSize, 220-offset)
						if warm {
							batchSize = min(batchSize, 20-offset)
						}
						var admissionLock *session.RuntimeLock
						var heldBefore piruntime.PiMessageDeliveryCounts
						if workload == "failed_churn" {
							var err error
							admissionLock, err = session.LockRuntimeExclusiveContext(s.f.Context, s.config.AuthorityDir, s.intent.Slug)
							if err != nil {
								t.Fatal(err)
							}
							heldBefore = s.f.Counts()
						}
						type pending struct {
							h      agentbackend.ExecutionHandle
							r      backend.DeliveryRequest
							start  time.Time
							sample deliveryNativeSample
							gate   string
						}
						batch := make([]pending, batchSize)
						var wg sync.WaitGroup
						for j := range batch {
							h := healthy[(offset+j)%len(healthy)]
							if workload == "idle_healthy" || workload == "delayed_followUp" {
								h = healthy[0]
							}
							id := s.id()
							r := backend.DeliveryRequest{Version: 1, ClientUserMessageID: id, Message: "measured native literal " + id, Delivery: "steer", Selection: selections[h.Conversation.Member]}
							batch[j] = pending{h: h, r: r}
							if workload == "delayed_followUp" {
								gate := s.id()
								active := s.input(h, "fixture-delayed-turn:"+gate, "steer")
								s.post(h, active)
								s.awaitGate(gate)
								r.Delivery = "followUp"
								batch[j].r = r
								batch[j].gate = gate
							}
							wg.Add(1)
							go func(j int) {
								defer wg.Done()
								batch[j].start = time.Now()
								batch[j].sample = s.sample(batch[j].h, batch[j].r, warm, admissionLock != nil || batch[j].gate != "")
							}(j)
						}
						wg.Wait()
						if admissionLock != nil {
							counts := s.f.Counts()
							hostCalls := counts.AdmissionHostInspects - heldBefore.AdmissionHostInspects
							proofCalls := deliveryAdmissionCalls(counts) - deliveryAdmissionCalls(heldBefore)
							s.write(map[string]any{"kind": "held_admission_batch", "operations": batchSize, "hostInspects": hostCalls, "runtimeDials": proofCalls})
							admissionLock.Close()
							if hostCalls != 0 || proofCalls != 0 {
								t.Errorf("local durable admission called native authority while EX held: host%d identity%d", hostCalls, proofCalls)
							}
						}
						for j := range batch {
							p := &batch[j]
							if p.gate != "" {
								s.wait(p.h, p.r.ClientUserMessageID, "pending")
								p.sample.EnqueueUpperNS = time.Since(p.sample.started).Nanoseconds()
								p.sample.Phases = append(p.sample.Phases, deliveryNativePhase{"pending", p.sample.EnqueueUpperNS})
								s.releaseGate(p.gate)
							}
							if (admissionLock != nil || p.gate != "") && p.sample.HTTPCode == 202 {
								p.sample = s.finishSample(p.h, p.r, p.start, p.sample)
							}
						}
						foregroundFinished = time.Now()
						proofStart := time.Now()
						for j := range batch {
							p := &batch[j]
							p.sample = s.proveSample(p.h, p.r, p.sample)
							s.write(p.sample)
							samples = append(samples, p.sample)
						}
						proofHandles := make([]agentbackend.ExecutionHandle, len(batch))
						proofRequests := make([]backend.DeliveryRequest, len(batch))
						proofSamples := make([]deliveryNativeSample, len(batch))
						for j, p := range batch {
							proofHandles[j] = p.h
							proofRequests[j] = p.r
							proofSamples[j] = p.sample
						}
						s.proveBatchCounts(proofHandles, proofRequests, proofSamples)
						elapsedProof := time.Since(proofStart).Nanoseconds()
						s.write(map[string]any{"kind": "batch_independent_proof", "warm": warm, "operations": len(batch), "elapsedNS": elapsedProof})
						if !warm {
							proofNS += elapsedProof
						}
						offset += batchSize
						if s.f.Context.Err() != nil {
							break
						}
					}
					stopActivity()
					activityResult := activity.snapshot()
					measuredEnd := s.f.CounterSnapshot()
					finished := measuredEnd.ObservedAt
					countsBefore, after := measured.Counts, measuredEnd.Counts
					s.f.FailObservation("")
					durable, accepted, publicSent, enqueue := []int64{}, []int64{}, []int64{}, []int64{}
					states := map[string]int{}
					progress := map[string]int{}
					warmCount := 0
					for _, sample := range samples {
						if sample.Warm {
							warmCount++
							if sample.State != "sent" {
								t.Errorf("warm endpoint retained incomplete: %s %s %s", sample.ID, sample.State, sample.Failure)
							}
							continue
						}
						states[sample.State]++
						durable = append(durable, sample.HTTPUpperNS)
						if sample.EnqueueUpperNS > 0 {
							enqueue = append(enqueue, sample.EnqueueUpperNS)
						}
						if sample.State == "sent" {
							accepted = append(accepted, sample.AcceptanceUpperNS)
							publicSent = append(publicSent, sample.PublicSentUpperNS)
							progress[sample.Member]++
						}
					}
					if after.AdmissionHostInspects != countsBefore.AdmissionHostInspects || deliveryAdmissionCalls(after) != deliveryAdmissionCalls(countsBefore) {
						t.Error("durable POST admission performed a native/Host call")
					}
					inventories := map[string]any{}
					receiptCounts := map[string]int{}
					appends := map[string]int{}
					for _, h := range healthy {
						s.f.Idle(h)
						inventory, users := s.f.History(t, h)
						inventories[h.Conversation.Member] = inventory
						for _, sample := range samples {
							if sample.Member == h.Conversation.Member {
								literal := "measured native literal " + sample.ID
								count := countExact(users, literal)
								appends[sample.ID] = count
								if sample.State == "sent" && (count != 1 || after.Enqueues[sample.ID] != 1 || s.runtime.count(sample.ID) != 1) {
									t.Errorf("original native at-most-once: %s append%d enqueue%d checked%d", sample.ID, count, after.Enqueues[sample.ID], s.runtime.count(sample.ID))
								}
							}
						}
						root, err := piruntime.SessionStateDirectory(s.config.UserStateRoot, h.Conversation.Session)
						if err != nil {
							t.Fatal(err)
						}
						files, err := os.ReadDir(filepath.Join(root, "members", h.Conversation.Member, h.Conversation.Incarnation, "submissions"))
						if err != nil {
							t.Fatal(err)
						}
						receiptCounts[h.Conversation.Member] = len(files)
					}
					activity.mu.Lock()
					questionAccepted := activity.QuestionAcceptedNS
					activity.mu.Unlock()
					s.runtime.mu.Lock()
					maxMember, maxTotal := s.runtime.maxMember, s.runtime.maxTotal
					s.runtime.mu.Unlock()
					elapsed := finished.Sub(measuredStart)
					summary := map[string]any{"kind": "performance_summary", "history": history.name, "workload": workload, "warmSamples": warmCount, "measuredSamples": len(durable), "states": states, "durableHTTPUpper": deliveryDistribution(durable), "httpStartToNativeAcceptedUpper": deliveryDistribution(accepted), "httpStartToPublicSentUpper": deliveryDistribution(publicSent), "httpStartToObservedPendingUpper": deliveryDistribution(enqueue), "pollSleepMS": 10, "elapsedNS": elapsed.Nanoseconds(), "perMemberSent": progress, "actualHistoriesAfter": inventories, "actualReceiptCounts": receiptCounts, "postMeasurementAuditEnd": s.f.CounterSnapshot(), "nativeUserAppends": appends, "countsBefore": countsBefore, "countsAfter": after, "warmInterval": map[string]any{"start": warmStart, "end": measured}, "measuredInterval": map[string]any{"start": measured, "end": measuredEnd, "independentProofNS": proofNS, "lastTimedEndpoint": foregroundFinished}, "workloadRevision": "P0: fixture independent Lookup follows all batch timed endpoints; proof still completes before next batch; differs from79dd", "hostInspectsPerSecond": float64(after.HostInspects-countsBefore.HostInspects) / elapsed.Seconds(), "activity": activityResult, "activityWarmStart": activityWarmStart, "activityMeasuredStart": activityMeasuredStart, "activityRequestsPerSecond": deliveryActivityRate(activityMeasuredStart, activityResult), "failedPeerOriginalID": failedPeer, "maxCheckedDispatchConcurrency": maxTotal, "maxPerConversationDispatchConcurrency": maxMember, "memoryAfter": s.memory(), "acceptanceTiming": "conservative complete-body HTTP-start to serialized write entry of a complete successfully written input_commit_v1 frame, after witness and Accepted persistence; not worker commit completion; single-process monotonic clock; no overlapping HTTP-response subtraction; public Sent separate", "backgroundEvidence": "delegated workflow_observe/event calls prove opportunities; deterministic A tests own exact eight-grant reservation proof", "counterScope": "attempted/full/failed native request writes; injected read uncertainty separate; Call reply failures are observed through original outcomes/witness/holds, not counted as write failures", "referenceOnly": "reported original96 failed refreshes/580 inspections in240s; not remeasured baseline"}
					s.write(summary)
					encoded, _ := json.Marshal(summary)
					t.Log(string(encoded))
					if workload == "delayed_followUp" && len(enqueue) != 200 {
						t.Errorf("unmet native Pending enqueue cell: %d", len(enqueue))
					}
					if warmCount != 20 || len(durable) != 200 {
						t.Errorf("unmet requested cell: warm%d measured%d", warmCount, len(durable))
					}
					if states["sent"] != 200 {
						t.Errorf("unmet acceptance cell: all samples retained: %v", states)
					}
					if deliveryPercentile(durable, .95) >= int64(250*time.Millisecond) {
						t.Errorf("durable HTTP upper p95 >=250ms; exact commit latency unresolved")
					}
					if workload == "idle_healthy" || workload == "team_activity" {
						if after.FailedWrites["input_commit_v1"] != countsBefore.FailedWrites["input_commit_v1"] || after.Calls["input_hold_v1"] != countsBefore.Calls["input_hold_v1"] {
							t.Error("post-persistence commit write failure or native hold in healthy cell; retained in counters")
						}
						if len(accepted) != 200 || deliveryPercentile(accepted, .95) >= int64(time.Second) {
							t.Errorf("healthy HTTP-to-Accepted upper p95 not below1s; over-target upper bound does not attribute a component")
						}
					}
					if maxMember > 1 || maxTotal > 4 {
						t.Errorf("dispatch concurrency member%d pool%d", maxMember, maxTotal)
					}
					if workload == "team_activity" || workload == "failed_churn" {
						for _, h := range healthy {
							if progress[h.Conversation.Member] == 0 {
								t.Errorf("peer starved: %s", h.Conversation.Member)
							}
						}
					}
					if questionAccepted <= measuredStart.Sub(s.started).Nanoseconds() || questionAccepted > foregroundFinished.Sub(s.started).Nanoseconds() {
						t.Error("real question did not progress inside sustained foreground interval")
					}
					if after.Calls["workflow_observe"] <= countsBefore.Calls["workflow_observe"] {
						t.Error("no observed background proof opportunity")
					}
					if workload == "failed_churn" {
						member := s.handles[4].Conversation.Member
						attempts := after.ByMember[member]["workflow_observe"] - countsBefore.ByMember[member]["workflow_observe"]
						if attempts > int(math.Ceil(elapsed.Seconds()))+4 {
							t.Errorf("failed proof attempts not bounded by retry floor: %d", attempts)
						}
					}
				})
			}
		})
	}
}

func deliveryPercentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	copy := slices.Clone(values)
	slices.Sort(copy)
	return copy[max(0, int(math.Ceil(float64(len(copy))*p))-1)]
}
func deliveryAdmissionCalls(counts piruntime.PiMessageDeliveryCounts) int {
	return counts.AdmissionRuntimeDials
}
func deliveryDistribution(values []int64) map[string]any {
	return map[string]any{"samples": len(values), "p50NS": deliveryPercentile(values, .5), "p95NS": deliveryPercentile(values, .95), "p99NS": deliveryPercentile(values, .99), "maxNS": deliveryPercentile(values, 1)}
}
func (s *deliveryNativeSuite) memory() map[string]string {
	result := map[string]string{}
	root, err := piruntime.SessionStateDirectory(s.config.UserStateRoot, s.f.Runtime.Session)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, h := range s.handles {
		data, err := os.ReadFile(filepath.Join(root, "members", h.Conversation.Member, h.Conversation.Incarnation, "conversation.json"))
		if err != nil {
			s.t.Fatal(err)
		}
		var record struct{ WorkerPID int }
		if json.Unmarshal(data, &record) != nil {
			s.t.Fatal("worker memory owner")
		}
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", record.WorkerPID))
		if err != nil {
			result[h.Conversation.Member] = err.Error()
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				result[h.Conversation.Member] = fmt.Sprintf("pid%d %s", record.WorkerPID, line)
			}
		}
	}
	return result
}
