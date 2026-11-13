package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type piCreationRuntimeFixture struct {
	runtime                 piruntime.RuntimeRef
	start                   *piruntime.StartRequest
	state                   piruntime.ConversationState
	submission              *piruntime.SubmissionStatus
	ensures, creates, sends int
	outcome                 string
	proofError              error
	beforeCreate            func() error
	initialPolicy           *piruntime.ToolPolicyTarget
	instructions            string
	verifications           int
	changeProof             func(int, *piruntime.CreationProof)
}

func (f *piCreationRuntimeFixture) Ensure(_ context.Context, r piruntime.StartRequest) (piruntime.RuntimeStatus, error) {
	f.ensures++
	if f.start != nil && *f.start != r {
		return piruntime.RuntimeStatus{}, piruntime.ErrConflict
	}
	start := r
	f.start = &start
	f.runtime = piruntime.RuntimeRef{Session: r.Session, InstanceID: "instance", RequestID: "request", Digest: strings.Repeat("7", 64), Generation: strings.Repeat("8", 64)}
	return piruntime.RuntimeStatus{Runtime: f.runtime, Phase: "ready"}, nil
}
func (f *piCreationRuntimeFixture) ReadStart(_ context.Context, ref agentbackend.SessionRef) (piruntime.StartRequest, error) {
	if f.start == nil {
		return piruntime.StartRequest{}, os.ErrNotExist
	}
	if f.start.Session != ref {
		return piruntime.StartRequest{}, piruntime.ErrScope
	}
	return *f.start, nil
}
func (f *piCreationRuntimeFixture) Create(_ context.Context, r piruntime.RuntimeRef, request piruntime.CreateRequest) (piruntime.ConversationState, error) {
	f.instructions = request.Instructions
	if request.ToolPolicy == nil || piruntime.ValidateToolPolicy(*request.ToolPolicy) != nil || request.ToolPolicy.Mode != "default" || request.ToolPolicy.Revision != 1 {
		return piruntime.ConversationState{}, piruntime.ErrScope
	}
	copy := *request.ToolPolicy
	copy.Active = append([]string{}, copy.Active...)
	copy.RegisteredCeiling = append([]string{}, copy.RegisteredCeiling...)
	f.initialPolicy = &copy
	if f.beforeCreate != nil {
		if err := f.beforeCreate(); err != nil {
			return piruntime.ConversationState{}, err
		}
	}
	f.creates++
	f.state = piruntime.ConversationState{Conversation: agentbackend.ConversationRef{Session: request.Session, Backend: agentbackend.Pi, Member: request.Member, Incarnation: request.Incarnation, NativeID: "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"}, Generation: r.Generation, State: "idle", HistoryRef: strings.Repeat("9", 64), Settings: request.Settings}
	return f.state, nil
}

// These are explicit fake ports for isolated creation/authority fixtures only.
// Actual Manager/Unix/SDK composition requires the separate combined fixture.
func (f *piCreationRuntimeFixture) AttachHost(ctx context.Context, runtime piruntime.RuntimeRef, handler piruntime.HostCallbackHandler) error {
	invalidator, ok := handler.(piruntime.HostInvalidationHandler)
	if !ok {
		return piruntime.ErrUnavailable
	}
	for _, sequence := range []uint64{1, 2} {
		invalidator.InvalidateWorkflow(piruntime.WorkflowInvalidation{Runtime: runtime, AttachmentID: "00000000-0000-4000-8000-000000000001", Sequence: sequence, Connected: true})
	}
	<-ctx.Done()
	invalidator.InvalidateWorkflow(piruntime.WorkflowInvalidation{Runtime: runtime, AttachmentID: "00000000-0000-4000-8000-000000000001", Sequence: 2, Connected: false})
	return ctx.Err()
}
func (*piCreationRuntimeFixture) InspectNativeOrigin(context.Context, agentbackend.ExecutionHandle, piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
	return piruntime.NativeOriginObservation{}, piruntime.ErrUnavailable
}
func (f *piCreationRuntimeFixture) InspectToolPolicy(_ context.Context, handle agentbackend.ExecutionHandle) (piruntime.ToolPolicyProof, error) {
	// Synthetic unit proof of the exact captured Create target, never native
	// integration evidence. The real SDK fixture must prove this independently.
	if f.initialPolicy == nil || handle.Conversation != f.state.Conversation || handle.Generation != f.state.Generation {
		return piruntime.ToolPolicyProof{}, piruntime.ErrUnavailable
	}
	policy := cloneWorkflowPolicy(*f.initialPolicy)
	return piruntime.ToolPolicyProof{Policy: policy, Generation: handle.Generation, Active: append([]string{}, policy.Active...), Callable: append([]string{}, policy.Active...), Registered: append([]string{}, policy.RegisteredCeiling...), Applied: true}, nil
}
func (*piCreationRuntimeFixture) ApplyToolPolicy(context.Context, agentbackend.ExecutionHandle, uint64, piruntime.ToolPolicyTarget) (piruntime.ToolPolicyResult, error) {
	return piruntime.ToolPolicyResult{}, piruntime.ErrUnavailable
}
func (*piCreationRuntimeFixture) ObserveWorkflow(context.Context, agentbackend.ExecutionHandle) (piruntime.WorkflowObservation, error) {
	return piruntime.WorkflowObservation{}, piruntime.ErrUnavailable
}
func (*piCreationRuntimeFixture) SendCheckedSubmission(context.Context, agentbackend.ExecutionHandle, piruntime.SubmissionRef, piruntime.SendInput) (piruntime.SubmissionStatus, error) {
	return piruntime.SubmissionStatus{}, piruntime.ErrUnavailable
}

func piCreationTeamCatalogFixture(t *testing.T, preset teamruntime.Preset) *agentteams.Installed {
	t.Helper()
	models := []string{"model", preset.LeadModel}
	for _, member := range preset.Members {
		models = append(models, member.Model)
	}
	slices.Sort(models)
	models = slices.Compact(models)
	policy := agentteams.WorkPolicy{Default: "high", Simple: "high", Allowed: []string{"high"}, SimpleRequiresReason: true, Followup: "retain"}
	leadInstructions := preset.LeadInstructions
	if leadInstructions == "" {
		leadInstructions = "Lead the fixture."
	}
	roles := map[string]agentteams.Role{"team_lead": {Model: preset.LeadModel, Effort: preset.LeadEffort, Behavior: "team_lead", Purpose: "lead", Instructions: leadInstructions, Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "workspace_write"}}
	if len(preset.Members) != 0 {
		roles["implementer"] = agentteams.Role{Model: preset.Members[0].Model, Effort: preset.Members[0].Effort, Behavior: "implementer", Purpose: "implementation", Instructions: "Implement the fixture.", Lifetime: "session", AllowedEfforts: []string{"high"}, Access: preset.Members[0].Access}
		roles["reviewer"] = agentteams.Role{Model: preset.LeadModel, Effort: "high", Behavior: "reviewer", Purpose: "review", Instructions: "Review the fixture.", Lifetime: "session", AllowedEfforts: []string{"high"}, Access: "read_only", FreshContext: true}
	}
	simple := "high"
	c := agentteams.Catalog{SchemaVersion: agentteams.CatalogSchemaVersion, ExecutionAccess: "full_access", CatalogDigest: preset.CatalogDigest, DefaultTeam: preset.ID, Capacity: agentteams.Capacity{RequiredNativeChildThreads: 2}, WorkPolicy: agentteams.WorkPolicies{Design: policy, Implementation: policy},
		Teams:     map[string]agentteams.Team{preset.ID: {Description: preset.Description, DesignOwner: "team_lead", MaxOpenAgents: 2, Mode: "development", ServicePolicy: "unmanaged", TeamDigest: preset.TeamDigest, Roles: roles, Routing: agentteams.Routing{DesignSimpleEffort: &simple, ImplementerSimpleEffort: &simple}, Lifecycle: agentteams.Lifecycle{Startup: "on_demand", Communication: "lead_mediated", ReviewerReuse: "same_change"}}},
		Utilities: agentteams.Utilities{VerificationWatcher: agentteams.VerificationWatcher{Model: "model", Effort: "low", Behavior: "verification_watcher", Lifetime: "operation", Access: "workspace_write", Startup: "on_demand", MaxConcurrent: 1, RequiredFor: []string{"long_check"}}}, ModelInventory: models}
	if preset.ID == "solo" && len(preset.Members) == 0 {
		team := c.Teams[preset.ID]
		team.Mode, team.MaxOpenAgents, team.Routing = "solo", 0, agentteams.Routing{}
		c.Teams[preset.ID] = team
	}
	adapter := agentteams.NativeAdapterIdentity{Identity: "codex-custom-agent-toml", Version: 1}
	c.NativeAgentConfigs.Adapter = adapter
	identity := func(instructions string) agentteams.NativeIdentity {
		sum := sha256.Sum256([]byte(instructions))
		return agentteams.NativeIdentity{Generator: agentteams.GeneratorIdentity{Identity: "dev-workspace-nix-agent-teams", Version: 1, Canonicalization: "nix-builtins-toJSON-attrset-v1"}, NativeAdapter: adapter, BehaviorDigest: hex.EncodeToString(sum[:])}
	}
	name := func(value any) string {
		data, _ := json.Marshal(value)
		var ordered any
		_ = json.Unmarshal(data, &ordered)
		data, _ = json.Marshal(ordered)
		sum := sha256.Sum256(data)
		return "dw_" + hex.EncodeToString(sum[:])[:48]
	}
	roleNames := make([]string, 0, len(roles))
	for role := range roles {
		roleNames = append(roleNames, role)
	}
	slices.Sort(roleNames)
	for _, role := range roleNames {
		v := agentteams.NativeRoleConfig{Kind: "role", Team: preset.ID, Role: role, Effort: "high", Identity: identity(roles[role].Instructions), Path: "share/dev-workspace/agent-teams/" + preset.ID + "/" + role + "-high.toml"}
		v.Name = name(map[string]any{"catalog_digest": c.CatalogDigest, "kind": v.Kind, "identity": v.Identity, "team": v.Team, "effort": v.Effort, "role": v.Role})
		c.NativeAgentConfigs.Roles = append(c.NativeAgentConfigs.Roles, v)
	}
	u := agentteams.NativeUtilityConfig{Kind: "utility", Utility: "verification_watcher", Effort: "low", Identity: identity("Watch the fixture."), Path: "share/dev-workspace/agent-teams/utilities/verification_watcher-low.toml"}
	u.Name = name(map[string]any{"catalog_digest": c.CatalogDigest, "kind": u.Kind, "identity": u.Identity, "utility": u.Utility, "effort": u.Effort})
	c.NativeAgentConfigs.Utilities = []agentteams.NativeUtilityConfig{u}
	if err := c.Validate(); err != nil {
		t.Fatal("typed composition catalog", err)
	}
	return &agentteams.Installed{Managed: true, Catalog: &c}
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
	f.verifications++
	if f.proofError != nil {
		return piruntime.CreationProof{}, f.proofError
	}
	p := piruntime.CreationProof{Conversation: f.state.Conversation, Generation: f.state.Generation, CreationID: id, HistoryRef: f.state.HistoryRef, Persisted: f.submission != nil && f.submission.Phase == "Accepted"}
	if f.instructions != "" {
		digest := sha256.Sum256([]byte(f.instructions))
		p.InstructionsDigest = hex.EncodeToString(digest[:])
	}
	if bootstrap != nil && f.submission != nil && f.submission.Ref == *bootstrap {
		p.Bootstrap = f.submission
	}
	if f.changeProof != nil {
		f.changeProof(f.verifications, &p)
	}
	return p, nil
}
func piCreationCatalogFixture() piproviders.Catalog {
	return piproviders.Catalog{Schema: 1, Digest: strings.Repeat("1", 64), Providers: []piproviders.Provider{{ID: "host", Label: "Fixture", NativeID: "native", RuntimeConfigRef: "runtime"}}, Accounts: []piproviders.Account{{Ref: "account", ProviderID: "host", CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: "native", AuthKind: "api_key"}}, Models: []piproviders.Model{{Kind: "physical", ProviderID: "host", ID: "model", Label: "Model", ThinkingLevels: []string{"high"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "fixture"}}, Routes: []piproviders.RouteGate{{ProviderID: "host", AccountRef: "account", ModelID: "model", State: "available", ReasonCode: "verified", EvidenceRef: "fixture"}}}
}
func newPiCreationFixture(t *testing.T) (*Server, creationReceipt, *piCreationRuntimeFixture) {
	return newPiCreationFixtureForSlug(t, "example")
}

func newPiCreationFixtureForSlug(t *testing.T, slug string) (*Server, creationReceipt, *piCreationRuntimeFixture) {
	t.Helper()
	return newPiCreationFixtureWithPreset(t, slug, piCreationSoloPreset())
}

func piCreationSoloPreset() teamruntime.Preset {
	return teamruntime.Preset{ID: "solo", Name: "Solo", Description: "Fixture lead", Roles: []string{"lead"}, CatalogDigest: strings.Repeat("2", 64), TeamDigest: strings.Repeat("3", 64), LeadModel: "model", LeadEffort: "high", LeadInstructions: "Lead the fixture.", Members: []teamruntime.MemberSpec{}}
}

func newPiCreationFixtureWithPreset(t *testing.T, slug string, preset teamruntime.Preset) (*Server, creationReceipt, *piCreationRuntimeFixture) {
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
	// This disposable tracking child has no tmux/runtime registration. Preserve
	// the parent's installed policy while explicitly isolating that child input.
	script := "#!" + ruby + "\nENV['DEV_WORKSPACES_STATE']=" + quote(s.config.UserStateRoot) + "\nENV.delete('DEV_WORKSPACE_NAME')\nENV.delete('DEV_SESSION_REQUIRE_RUNTIME')\nload " + quote(source) + "\nARGV.unshift('--workspace'," + quote(s.config.Workspace) + ",'--authority-dir'," + quote(s.config.AuthorityDir) + ")\nexit DevSession::CLI.new(ARGV).run\n"
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
		if f.start != nil {
			return *f.start, nil
		}
		return piruntime.StartRequest{Session: intent.SessionRef(), BindingRevision: rev, AppStorePath: s.config.PackageRoot, ProfilePath: s.config.HostProfile, ProfileGeneration: piProfileToken(s.hostProfile)}, nil
	}}
	s.installedTeams = piCreationTeamCatalogFixture(t, preset)
	piWorkflowGuardFixture(t, s)
	contractPath := filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract map[string]any
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	contract["piRoleInstructions"] = 1
	if err := writePiCreationRecord(contractPath, contract, 64<<10); err != nil {
		t.Fatal(err)
	}
	s.piWorkflows = newPiWorkflowComposition(s)
	if s.piWorkflows == nil {
		t.Fatal("typed workflow composition is unavailable")
	}
	ref := agentbackend.SessionRef{Workspace: s.config.Workspace, Slug: slug, Incarnation: strings.Repeat("a", 64)}
	conversations := []session.PiReservedConversation{}
	for index, address := range preset.Roles {
		conversations = append(conversations, session.PiReservedConversation{Address: address, Incarnation: strings.Repeat(string(rune('b'+index)), 64)})
	}
	policy, err := selection(context.Background(), ref, conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := session.FreezePiCreationSelection(c, policy, conversations)
	if err != nil {
		t.Fatal(err)
	}
	history, err := session.CompletedRemovalHistory(s.config.Workspace, slug, s.config.UserStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := creationReceipt{Schema: 4, Workspace: s.config.Workspace, Request: creationRequest{Backend: "pi", Kind: "new", Slug: slug, Goal: "The original real request", Team: preset.ID, CatalogDigest: preset.CatalogDigest, directTeam: &preset}, ReceiptID: strings.Repeat("d", 64), DeletionHistorySHA256: history, Attempt: 1, State: "running", Phase: "Preparing Pi", StartedAt: now, UpdatedAt: now, Validated: true, Goal: "The original real request", Model: "model", Effort: "high", DirectTeam: &preset, Pi: &piCreationIdentity{BackendContract: 1, SessionIncarnation: ref.Incarnation, Conversations: conversations, Provider: "native", InitialInputID: "11111111-1111-4111-8111-111111111111", Selection: frozen}}
	if err = s.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.creations[slug] = receipt
	return s, receipt, f
}

func historicalPiTeamPreset() teamruntime.Preset {
	return teamruntime.Preset{ID: "delegated", Name: "Fixture", Description: "Fixture team", Roles: []string{"lead", "implementer0"}, CatalogDigest: strings.Repeat("2", 64), TeamDigest: strings.Repeat("3", 64), LeadModel: "model", LeadEffort: "high", Members: []teamruntime.MemberSpec{{Role: "implementer", Address: "implementer0", Model: "model", Effort: "high", Behavior: "implementer", Access: "workspace_write"}}}
}

// Seed the schema-4 completed root that the previous source could publish for a
// multi-role roster. These synthetic unit records are historical read fixtures,
// not evidence that current creation supports teams or actual native execution.
func newHistoricalPiTeamCreationFixture(t *testing.T) (*Server, creationReceipt, *piCreationRuntimeFixture) {
	t.Helper()
	return newHistoricalPiCreationFixtureWithPreset(t, historicalPiTeamPreset())
}

func newHistoricalPiCreationFixtureWithPreset(t *testing.T, preset teamruntime.Preset) (*Server, creationReceipt, *piCreationRuntimeFixture) {
	t.Helper()
	s, receipt, f := newPiCreationFixtureWithPreset(t, "example", preset)
	intent, err := receipt.piIntent()
	if err != nil {
		t.Fatal(err)
	}
	store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, err := store.Create(context.Background(), intent.Binding())
	if err != nil {
		t.Fatal(err)
	}
	binding.Revision++
	binding.State = "ready"
	binding.Conversations[0].State, binding.Conversations[0].NativeID = "ready", "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	binding, err = store.CompareAndSwap(context.Background(), binding.Revision-1, binding)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := binding.Resolve("lead")
	if err != nil {
		t.Fatal(err)
	}
	f.state = piruntime.ConversationState{Conversation: conversation, Generation: strings.Repeat("8", 64), State: "idle", HistoryRef: strings.Repeat("9", 64), Settings: intent.ProviderSelection.Members[0].Settings.Effective()}
	journal := session.PiCreationJournal{PiCreationIntent: intent, State: "ready", NativeCreateStarted: true, NativeGeneration: f.state.Generation, Native: &session.PiCreationNative{NativeID: conversation.NativeID, HistoryRef: f.state.HistoryRef}}
	handle := agentbackend.ExecutionHandle{Conversation: conversation, Generation: f.state.Generation}
	input := piRootAdmission(intent, journal, handle, receipt.Goal)
	f.submission = &piruntime.SubmissionStatus{Schema: 1, Ref: input.Submission, Phase: "Accepted", Generation: handle.Generation, Settings: input.Settings, Input: input.Input, Witness: &piruntime.NativeWitness{SessionID: conversation.NativeID, EntryID: "historical-user-entry", EffectiveDigest: piruntime.MessageDigest(receipt.Goal)}}
	if err := session.PublishPiRuntimeAuthority(s.config.AuthorityDir, binding, session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: intent.Workspace, Slug: intent.Slug, SessionIncarnation: intent.SessionIncarnation, BindingRevision: binding.Revision, Generation: handle.Generation, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	tracking := filepath.Join(intent.Workspace, "work", intent.Slug)
	if err := os.MkdirAll(tracking, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := session.Manifest{Schema: 4, Slug: intent.Slug, Backend: &session.ManifestBackend{Kind: agentbackend.Pi, Contract: 1, SessionIncarnation: intent.SessionIncarnation}, Creation: session.Creation{State: "ready", InitialGoalSent: true, GoalSHA256: intent.GoalSHA256}, Repositories: []session.Repository{}}
	data, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tracking, session.ManifestName), data, 0600); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"plan.md": "# Historical creation fixture\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(tracking, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stat unix.Stat_t
	if err := unix.Lstat(tracking, &stat); err != nil {
		t.Fatal(err)
	}
	evidence := piCreationEvidence{Schema: 4, Workspace: intent.Workspace, Slug: intent.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: intent.ReceiptID, SessionIncarnation: intent.SessionIncarnation, BindingRevision: binding.Revision, Conversations: intent.Conversations, NativeID: conversation.NativeID, InitialInputID: intent.InitialInputID, InitialSubmissionDigest: input.Submission.Digest, GoalSHA256: intent.GoalSHA256, Model: intent.Model, Effort: intent.Effort, Provider: intent.Provider, ProviderSelection: intent.ProviderSelection, CatalogDigest: intent.DirectTeam.CatalogDigest, TeamDigest: intent.DirectTeam.TeamDigest, Team: intent.DirectTeam.ID, TrackingDevice: stat.Dev, TrackingInode: stat.Ino}
	for path, value := range map[string]any{s.creationEvidencePath(receipt) + ".request": intent, s.creationEvidencePath(receipt): evidence, piJournalPath(intent.Workspace, intent.Slug): journal} {
		if err := writePiCreationRecord(path, value, maxCreationReceiptBytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.provePiCreation(receipt); err != nil {
		t.Fatal("historical fixture proof", err)
	}
	return s, receipt, f
}

func TestPiCreationPublishesExplicitSoloRoot(t *testing.T) {
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
	if binding.Revision != 2 || binding.State != "ready" || len(binding.Conversations) != 1 || binding.Conversations[0].State != "ready" {
		t.Fatalf("Solo publication changed the selected roster: %#v", binding)
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

func TestPiCreationCarriesExactOriginalRoleAndGoal(t *testing.T) {
	t.Setenv("DEV_SESSION_REQUIRE_RUNTIME", "1")
	preset := piCreationSoloPreset()
	preset.LeadInstructions = " \n/path/to/role.md\nKeep the literal café role.\n"
	s, receipt, f := newPiCreationFixtureWithPreset(t, "example", preset)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if f.instructions != preset.LeadInstructions || f.submission == nil || f.submission.Input.Message != receipt.Goal || f.submission.Ref.OperationID != receipt.Pi.InitialInputID || f.creates != 1 || f.sends != 1 {
		t.Fatalf("original role/goal changed: role=%q input=%#v", f.instructions, f.submission)
	}
	if os.Getenv("DEV_SESSION_REQUIRE_RUNTIME") != "1" {
		t.Fatal("tracking fixture changed parent runtime policy")
	}
}

func TestPiCreationRejectsRoleProofAtEveryPublicationBoundary(t *testing.T) {
	for boundary, name := range []string{"before-send", "before-ready-binding", "before-completion"} {
		for _, bad := range []string{"missing", "malformed", "wrong"} {
			t.Run(name+"/"+bad, func(t *testing.T) {
				s, receipt, f := newPiCreationFixture(t)
				f.changeProof = func(call int, proof *piruntime.CreationProof) {
					if call == boundary+1 {
						proof.InstructionsDigest = map[string]string{"missing": "", "malformed": "not-a-digest", "wrong": strings.Repeat("e", 64)}[bad]
					}
				}
				if err := s.initializePiCreation(receipt); !errors.Is(err, piruntime.ErrHeld) {
					t.Fatalf("unproved role crossed %s: %v", name, err)
				}
				if f.creates != 1 || f.sends != min(boundary, 1) {
					t.Fatalf("wrong native boundary %d/%d", f.creates, f.sends)
				}
				binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load("example")
				if err != nil || (binding.State == "ready") != (boundary == 2) {
					t.Fatalf("wrong ready boundary: %#v %v", binding, err)
				}
				if _, err := os.Stat(s.creationEvidencePath(receipt)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unproved role published completion", err)
				}
			})
		}
	}
}

func TestPiCreationRolePackageRefusesBeforeAcceptanceAndEffects(t *testing.T) {
	for _, bad := range []string{"missing", "wrong", "null", "partial-workflow", "unresolved-root", "unselected-root"} {
		t.Run(bad, func(t *testing.T) {
			s, receipt, f := newPiCreationFixture(t)
			path := filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			switch bad {
			case "missing":
				delete(fields, "piRoleInstructions")
			case "wrong":
				fields["piRoleInstructions"] = 2
			case "null":
				fields["piRoleInstructions"] = nil
			case "partial-workflow":
				delete(fields, "linearLineage")
			case "unresolved-root":
				alias := filepath.Join(t.TempDir(), "package")
				if err := os.Symlink(s.config.PackageRoot, alias); err != nil {
					t.Fatal(err)
				}
				s.config.PackageRoot = alias
			case "unselected-root":
				s.config.PackageRoot = t.TempDir()
			}
			if err := writePiCreationRecord(path, fields, 64<<10); err != nil {
				t.Fatal(err)
			}
			selected := 0
			s.config.PiCreation.Selection = func(context.Context, agentbackend.SessionRef, []session.PiReservedConversation, teamruntime.Preset) (piproviders.Policy, error) {
				selected++
				return piproviders.Policy{}, errors.New("unexpected selection")
			}
			request := receipt.Request
			request.Slug = "unaccepted-role"
			if _, err := s.acceptCreation(request); err == nil {
				t.Fatal("unsupported role package accepted")
			}
			if err := s.initializePiCreation(receipt); err == nil {
				t.Fatal("unsupported role package initialized")
			}
			if selected != 0 || len(s.creations) != 1 || f.ensures != 0 || f.creates != 0 || f.sends != 0 {
				t.Fatal("unsupported role package crossed acceptance/effects")
			}
			if _, err := os.Stat(s.creationPath(request.Slug)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("role refusal created receipt", err)
			}
		})
	}
}

func TestPiCreationRoleRefusesWrongRuntimePackageRoot(t *testing.T) {
	s, receipt, f := newPiCreationFixture(t)
	s.config.PiCreation.StartRequest = func(intent session.PiCreationIntent, rev uint64) (piruntime.StartRequest, error) {
		return piruntime.StartRequest{Session: intent.SessionRef(), BindingRevision: rev, AppStorePath: s.config.PackageRoot + "-other"}, nil
	}
	if err := s.initializePiCreation(receipt); !errors.Is(err, piruntime.ErrHeld) || f.ensures != 0 || f.creates != 0 || f.sends != 0 {
		t.Fatalf("wrong runtime root admitted: %v", err)
	}
}

func TestPiCreationRoleInjectionPreservesSuppliedRuntime(t *testing.T) {
	s, receipt, f := newPiCreationFixture(t)
	config := s.config
	supplied := config.PiCreation
	// No package authorities are supplied to this constructor fixture. Keeping
	// the injected object cannot supply the missing admitted role package.
	config.PackageRoot = ""
	application, err := NewContext(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.config.PiCreation != supplied || supplied.Runtime != f || config.PiProviderConfig != nil {
		t.Fatal("nil-provider construction replaced the supplied services/runtime")
	}
	if err := application.initializePiCreation(receipt); err == nil || f.ensures != 0 || f.creates != 0 || f.sends != 0 {
		t.Fatal("injection presence waived actual package support", err)
	}
}

func TestPiCreationEmptyOriginalRoleKeepsLegacyProofSemantics(t *testing.T) {
	for name, unexpectedProof := range map[string]bool{"legacy-absent": false, "unexpected-proof": true} {
		t.Run(name, func(t *testing.T) {
			preset := piCreationSoloPreset()
			preset.LeadInstructions = ""
			s, receipt, f := newPiCreationFixtureWithPreset(t, "example", preset)
			path := filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")
			var fields map[string]any
			if err := readCreationJSON(path, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, "piRoleInstructions")
			if err := writePiCreationRecord(path, fields, 64<<10); err != nil {
				t.Fatal(err)
			}
			if unexpectedProof {
				f.changeProof = func(_ int, proof *piruntime.CreationProof) { proof.InstructionsDigest = strings.Repeat("e", 64) }
			}
			err := s.initializePiCreation(receipt)
			if unexpectedProof && (!errors.Is(err, piruntime.ErrHeld) || f.sends != 0) || !unexpectedProof && (err != nil || f.sends != 1 || f.instructions != "") {
				t.Fatalf("empty original role adopted another proof: %v", err)
			}
		})
	}
}

func TestPiCreationIncompleteLegacyRoleMappingNeverBecomesReady(t *testing.T) {
	s, receipt, f := newPiCreationFixture(t)
	s.piCreationCheckpoint = func(point string) error {
		if point == "initial_input" {
			return errors.New("original accepted bootstrap, incomplete root")
		}
		return nil
	}
	if err := s.initializePiCreation(receipt); err == nil || f.submission == nil || f.submission.Phase != "Accepted" {
		t.Fatal("fixture did not retain original Accepted input", err)
	}
	s.piCreationCheckpoint = nil
	f.instructions = "" // Synthetic legacy-empty mapping, not a worker retrofit.
	for range 2 {
		if err := s.initializePiCreation(receipt); !errors.Is(err, piruntime.ErrHeld) {
			t.Fatalf("legacy role omission became ready: %v", err)
		}
	}
	if f.creates != 1 || f.sends != 1 {
		t.Fatal("legacy incomplete recovery recreated or resent input")
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load("example")
	if err != nil || binding.State != "creating" {
		t.Fatal("legacy role omission published ready", err)
	}
}

func TestPiCreationCompletedLegacyRoleProofAndRetirementStayReadable(t *testing.T) {
	s, receipt, f := newHistoricalPiCreationFixtureWithPreset(t, piCreationSoloPreset())
	if f.instructions != "" || receipt.DirectTeam.LeadInstructions == "" {
		t.Fatal("fixture must retain original role over legacy-empty mapping")
	}
	if err := os.Remove(filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")); err != nil {
		t.Fatal(err)
	}
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal("completed historical read acquired role gate", err)
	}
	receipt.State = "ready"
	if err := s.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if err := s.retirePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	if err := s.provePiCreation(receipt); err != nil || f.ensures != 0 || f.creates != 0 || f.sends != 0 {
		t.Fatal("retired historical proof changed", err)
	}
}

func TestPiCreationRefusesUnsupportedRosterBeforeReservation(t *testing.T) {
	for _, kind := range []string{"delegated", "other-multi-role", "forged-solo", "other-lead-only", "missing-selection"} {
		t.Run(kind, func(t *testing.T) {
			s, original, f := newPiCreationFixture(t)
			request := original.Request
			request.Slug = "refused"
			preset := historicalPiTeamPreset()
			switch kind {
			case "other-multi-role":
				preset.ID = "lead_designed"
			case "forged-solo":
				preset.ID = "solo"
			case "other-lead-only":
				preset = *original.DirectTeam
				preset.ID = "lead_reviewed"
			}
			if err := validDirectTeamPreset(preset); err != nil {
				t.Fatal("fixture must be format-valid", err)
			}
			request.Team, request.CatalogDigest, request.directTeam = preset.ID, preset.CatalogDigest, &preset
			if kind == "missing-selection" {
				request.Team, request.CatalogDigest, request.directTeam = "", "", nil
			}
			selected := 0
			s.config.PiCreation.Selection = func(context.Context, agentbackend.SessionRef, []session.PiReservedConversation, teamruntime.Preset) (piproviders.Policy, error) {
				selected++
				return piproviders.Policy{}, errors.New("unexpected provider selection")
			}
			before, err := os.ReadDir(s.config.AuthorityDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.acceptCreation(request); err == nil || kind != "missing-selection" && !errors.Is(err, piruntime.ErrUnavailable) {
				t.Fatalf("unsupported roster accepted or wrong refusal: %v", err)
			}
			after, err := os.ReadDir(s.config.AuthorityDir)
			if err != nil || len(after) != len(before) {
				t.Fatalf("refusal created authority files: %v", err)
			}
			if selected != 0 || f.ensures != 0 || f.creates != 0 || f.sends != 0 || len(s.creations) != 1 {
				t.Fatalf("refusal crossed acceptance: selection=%d native=%d/%d/%d receipts=%d", selected, f.ensures, f.creates, f.sends, len(s.creations))
			}
			for _, path := range []string{s.creationPath(request.Slug), piJournalPath(s.config.Workspace, request.Slug), filepath.Join(s.config.Workspace, "work", request.Slug), filepath.Join(s.config.Workspace, "worktrees", request.Slug), filepath.Join(s.config.Workspace, "archive", request.Slug)} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refusal wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestPiCreationFrozenPreparationDoesNotAdoptSoloDefault(t *testing.T) {
	s, original, f := newPiCreationFixture(t)
	preset := historicalPiTeamPreset()
	record := sessionPreparation{Slug: "frozen-team", Snapshot: &preparationSnapshot{Goal: original.Goal, Preset: &preset}}
	request := preparationCreationRequest(record)
	if request.Team != preset.ID || request.directTeam != record.Snapshot.Preset || request.directTeam.LeadInstructions != preset.LeadInstructions {
		t.Fatal("preparation lost its frozen preset")
	}
	// Exercise the Pi acceptance boundary with that exact frozen handoff. The
	// preparation helper's existing Codex backend choice is unchanged.
	request.Backend = "pi"
	if _, err := s.acceptCreation(request); !errors.Is(err, piruntime.ErrUnavailable) {
		t.Fatalf("installed Solo default replaced frozen team: %v", err)
	}
	if len(s.creations) != 1 || f.ensures != 0 || f.creates != 0 || f.sends != 0 {
		t.Fatal("unsupported frozen handoff caused replacement effects")
	}
}

func TestPiCreationIncompleteHistoricalTeamRemainsHeld(t *testing.T) {
	for _, stage := range []string{"receipt", "native-intent", "native-result"} {
		t.Run(stage, func(t *testing.T) {
			s, receipt, f := newPiCreationFixtureWithPreset(t, "example", historicalPiTeamPreset())
			intent, err := receipt.piIntent()
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{s.creationPath("example")}
			if stage != "receipt" {
				journal := session.PiCreationJournal{PiCreationIntent: intent, State: "creating", NativeCreateStarted: true, NativeGeneration: strings.Repeat("8", 64)}
				if stage == "native-result" {
					journal.Native = &session.PiCreationNative{NativeID: "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa", HistoryRef: strings.Repeat("9", 64)}
				}
				for path, value := range map[string]any{s.creationEvidencePath(receipt) + ".request": intent, piJournalPath(intent.Workspace, intent.Slug): journal} {
					if err := writePiCreationRecord(path, value, maxCreationReceiptBytes); err != nil {
						t.Fatal(err)
					}
					paths = append(paths, path)
				}
			}
			before := map[string]string{}
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = string(data)
			}
			duplicate, err := s.acceptCreation(receipt.Request)
			if err != nil || duplicate.ReceiptID != receipt.ReceiptID || duplicate.Pi.InitialInputID != receipt.Pi.InitialInputID || duplicate.Pi.SessionIncarnation != receipt.Pi.SessionIncarnation {
				t.Fatalf("original receipt replay changed: %#v %v", duplicate, err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := s.initializePiCreation(duplicate); !errors.Is(err, piruntime.ErrUnavailable) {
					t.Fatalf("unsupported historical intent resumed: %v", err)
				}
			}
			if err := s.preparePiCreation(context.Background(), receipt); !errors.Is(err, piruntime.ErrUnavailable) {
				t.Fatalf("preparation bypassed immutable roster gate: %v", err)
			}
			for path, content := range before {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != content {
					t.Fatalf("held evidence changed: %s %v", path, err)
				}
			}
			if f.ensures != 0 || f.creates != 0 || f.sends != 0 {
				t.Fatal("historical revisit replaced native effects")
			}
			for _, path := range []string{s.creationEvidencePath(receipt), filepath.Join(s.config.Workspace, "work", "example")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsupported creation published ready: %s %v", path, err)
				}
			}
		})
	}
}

func TestPiCreationCompletedHistoricalTeamProofRemainsReadable(t *testing.T) {
	s, receipt, f := newHistoricalPiTeamCreationFixture(t)
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal("completed historical proof was blocked", err)
	}
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load("example")
	if err != nil || len(binding.Conversations) != 2 || binding.Conversations[1].State != "creating" || binding.Conversations[1].NativeID != "" || f.ensures != 0 || f.creates != 0 || f.sends != 0 {
		t.Fatalf("historical read changed member or native state: %#v %v", binding, err)
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

func TestPiCreationNameOnlyBrowserRequestNeverFallsBack(t *testing.T) {
	s := newTestServer(t)
	form := url.Values{"backend": {"pi"}, "name": {"example"}, "creation_date": {"2026-11-12"}, "goal": {"The request"}}
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	s.createSession(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("name-only Pi request: %d %s", response.Code, response.Body.String())
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

type piCreationRuntimeWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *piCreationRuntimeWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestPiCreationRechecksOwnerAfterRuntimeLockWait(t *testing.T) {
	for _, cause := range []string{"package", "unsupported-roster", "role-contract"} {
		t.Run(cause, func(t *testing.T) {
			s, r, f := newPiCreationFixture(t)
			if cause == "unsupported-roster" {
				s, r, f = newPiCreationFixtureWithPreset(t, "example", historicalPiTeamPreset())
			}
			runtime, err := session.LockRuntimeExclusive(s.config.AuthorityDir, "example")
			if err != nil {
				t.Fatal(err)
			}
			baseContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			ctx := &piCreationRuntimeWaitContext{Context: baseContext, observed: make(chan struct{})}
			done := make(chan error, 1)
			joined, runtimeHeld := false, true
			releaseRuntime := func() {
				if runtimeHeld {
					runtime.Close()
					runtimeHeld = false
				}
			}
			t.Cleanup(func() {
				cancel()
				releaseRuntime()
				if joined {
					return
				}
				cleanup := time.NewTimer(time.Second)
				defer cleanup.Stop()
				select {
				case result := <-done:
					joined = true
					t.Logf("%s worker joined during cleanup: error=%v context=%v", cause, result, baseContext.Err())
				case <-cleanup.C:
					t.Errorf("%s worker join unresolved after cancel and runtime release: context=%v", cause, baseContext.Err())
				}
			})
			go func() {
				done <- s.withPiCreationOwner(ctx, r, func(session.PiCreationIntent) error { f.creates++; return nil })
			}()
			observation := time.NewTimer(time.Second)
			defer observation.Stop()
			// Only the real runtime-lock contention select queries this context's
			// Done method. The observer must use the original context instead.
			select {
			case <-ctx.observed:
			case result := <-done:
				joined = true
				t.Fatalf("%s worker returned before runtime wait: error=%v context=%v", cause, result, baseContext.Err())
			case <-observation.C:
				select {
				case result := <-done:
					joined = true
					t.Fatalf("%s worker returned without a runtime witness at observation deadline: error=%v context=%v", cause, result, baseContext.Err())
				default:
				}
				t.Fatalf("%s fixture did not witness runtime contention: context=%v", cause, baseContext.Err())
			case <-baseContext.Done():
				t.Fatalf("%s context ended before runtime wait: %v", cause, baseContext.Err())
			}
			select {
			case result := <-done:
				joined = true
				t.Fatalf("%s worker finished before mutation: error=%v context=%v", cause, result, baseContext.Err())
			default:
			}
			if f.creates != 0 || f.sends != 0 {
				t.Fatal("runtime waiter reached effects before mutation")
			}
			t.Logf("%s runtime contention witnessed before mutation, creates=%d sends=%d", cause, f.creates, f.sends)
			if cause == "package" {
				replacement := s.config.HostProfile + ".replacement"
				if err = os.Symlink(t.TempDir(), replacement); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(replacement, s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
			}
			if cause == "role-contract" {
				// Replace only the capability while the admitted effect is waiting.
				path := filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")
				var fields map[string]any
				if err := readCreationJSON(path, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "piRoleInstructions")
				if err := writePiCreationRecord(path, fields, 64<<10); err != nil {
					t.Fatal(err)
				}
			}
			releaseRuntime()
			select {
			case err = <-done:
				joined = true
			case <-baseContext.Done():
				t.Fatalf("%s worker did not finish after runtime release: context=%v", cause, baseContext.Err())
			}
			if err == nil || cause == "unsupported-roster" && !errors.Is(err, piruntime.ErrUnavailable) {
				t.Fatalf("waiter bypassed current owner gate: %v", err)
			}
			if f.creates != 0 || f.sends != 0 {
				t.Fatal("stale waiter reached effects")
			}
		})
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
	for _, destinationLocked := range []bool{false, true} {
		name := "local-destination-locks"
		if destinationLocked {
			name = "caller-owned-destination-locks"
		}
		t.Run(name, func(t *testing.T) {
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
			var accepted creationReceipt
			var err error
			if destinationLocked {
				accepted, err = func() (creationReceipt, error) {
					s.operationMu.Lock()
					defer s.operationMu.Unlock()
					creation, err := session.LockCreation(s.config.AuthorityDir, "example")
					if err != nil {
						return creationReceipt{}, err
					}
					defer creation.Close()
					runtime, err := session.LockRuntimeExclusive(s.config.AuthorityDir, "example")
					if err != nil {
						return creationReceipt{}, err
					}
					defer runtime.Close()
					return s.installCreationLocked(original.Request, original.ReceiptID, original.DeletionHistorySHA256, true)
				}()
			} else {
				accepted, err = s.acceptCreation(original.Request)
			}
			if err != nil {
				t.Fatal(err)
			}
			if destinationLocked && (accepted.ReceiptID != original.ReceiptID || accepted.DeletionHistorySHA256 != original.DeletionHistorySHA256) {
				t.Fatal("caller-owned admission changed the original receipt or epoch")
			}
			// AttachHost remains live until server shutdown; await this original
			// creation's outcome rather than the server's lifetime operation group.
			current := awaitCreation(t, s, "example")
			if current.State != "ready" {
				t.Fatalf("fresh creation failed: %s", current.Error)
			}
			if observedReceipt != accepted.ReceiptID || observedInput != accepted.Pi.InitialInputID || f.creates != 1 || f.sends != 1 {
				t.Fatalf("fresh effect fence failed: %s %s %d/%d", observedReceipt, observedInput, f.creates, f.sends)
			}
		})
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
