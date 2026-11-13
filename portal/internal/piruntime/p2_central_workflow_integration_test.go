//go:build pi_native_fixture

package piruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"github.com/aither64/dev-workspace/portal/internal/web"
	"gopkg.in/yaml.v3"
)

var _ interface {
	BindPiWorkflowNativeFixture(context.Context, agentteams.Installed, piruntime.RuntimeRef, bool) error
} = (*web.Server)(nil)

func TestPiCentralWorkflowIntentFixture(t *testing.T) {
	path := os.Getenv("PI_CENTRAL_CATALOG_FIXTURE")
	if path == "" {
		t.Skip("finite batch must export the existing typed catalog fixture")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var catalog agentteams.Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	fixture := piruntime.P6NativeFixture{Runtime: piruntime.RuntimeRef{Session: agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}}}
	intent, _ := centralIntent(t, fixture, catalog)
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.DecodePiCreationIntent(encoded); err != nil {
		t.Fatal(err)
	}
	centralFixturePackage(t, t.TempDir())
	if root := os.Getenv("PI_CENTRAL_APP_ROOT"); root != "" {
		data, err := os.ReadFile(filepath.Join(root, "share/workspace-portal/runtime-contract.json"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.DecodePiRoleInstructionsContract(data); err != nil {
			t.Fatal(err)
		}
		if _, err := agentteams.LoadInstalled(root); err != nil {
			t.Fatal(err)
		}
		// Pure shape preflight only: no Manager, worker or native proof is used.
		// The actual component below supplies real Create/Accepted evidence.
		profile := filepath.Join(t.TempDir(), "profile")
		if err := os.Symlink(root, profile); err != nil {
			t.Fatal(err)
		}
		command, err := exec.LookPath("false")
		if err != nil {
			t.Fatal(err)
		}
		authority := filepath.Join(t.TempDir(), "authority")
		if err := os.Mkdir(authority, 0700); err != nil {
			t.Fatal(err)
		}
		host := t.TempDir()
		config := web.Config{Workspace: intent.Workspace, BaseURL: "https://portal.example", HostProfile: profile, AuthorityDir: authority, UserStateRoot: t.TempDir(), PackageRoot: root, WorkspaceName: "central", DevSession: command, CodexSocket: filepath.Join(host, "app-server.sock"), RegistrationMarker: filepath.Join(host, "registration.json"), Logger: log.New(&bytes.Buffer{}, "", 0)}
		state := piruntime.ConversationState{Conversation: agentbackend.ConversationRef{Session: intent.SessionRef(), Backend: agentbackend.Pi, Member: "lead", Incarnation: intent.Conversations[0].Incarnation, NativeID: "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"}, Generation: strings.Repeat("8", 64), HistoryRef: strings.Repeat("9", 64)}
		centralPublishIncomplete(t, config, intent, state, "publish optional question and exact plan")
		// The tracking phase runs only after the ordinary retry owns a running
		// receipt. This pure preflight supplies that exact mutable status.
		receiptPath := filepath.Join(userstate.WorkspaceDirectory(config.UserStateRoot, "portal", config.Workspace), "creations", intent.Slug+".json")
		receiptData, err := os.ReadFile(receiptPath)
		if err != nil {
			t.Fatal(err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(receiptData, &receipt); err != nil {
			t.Fatal(err)
		}
		receipt["state"] = "running"
		centralJSON(t, receiptPath, receipt)
		config.DevSession = centralTrackingCommand(t, config)
		goalFile := filepath.Join(t.TempDir(), "original-goal")
		centralWrite(t, goalFile, []byte("publish optional question and exact plan"))
		transition, err := os.CreateTemp(t.TempDir(), "transition")
		if err != nil {
			t.Fatal(err)
		}
		defer transition.Close()
		if err := syscall.Flock(int(transition.Fd()), syscall.LOCK_SH); err != nil {
			t.Fatal(err)
		}
		childContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		trackingChild := exec.CommandContext(childContext, config.DevSession, "start", intent.Slug, "--as-is", "--backend", "pi", "--creation-phase", "prepare-tracking", "--creation-receipt-id", intent.ReceiptID, "--creation-evidence", filepath.Join(userstate.WorkspaceDirectory(config.UserStateRoot, "portal", config.Workspace), "creations", intent.Slug+"."+intent.ReceiptID+".complete.json"), "--session-incarnation", intent.SessionIncarnation, "--binding-revision", "1", "--goal-file", goalFile, "--json")
		trackingChild.ExtraFiles = []*os.File{transition}
		trackingChild.Env = append(os.Environ(), "DEV_SESSION_REQUIRE_RUNTIME=1", "DEV_WORKSPACE_TRANSITION_LOCK_FD=3")
		output, err := trackingChild.CombinedOutput()
		if err != nil {
			t.Fatalf("owning historical tracking preflight: %v %s", err, output)
		}
		var prepared struct{ Phase, Slug, ReceiptID string }
		if json.Unmarshal(output, &prepared) != nil || prepared.Phase != "prepare-tracking" || prepared.Slug != intent.Slug || prepared.ReceiptID != intent.ReceiptID {
			t.Fatalf("tracking preflight lost original references: %s", output)
		}
		server, err := web.NewContext(context.Background(), config)
		if err != nil {
			t.Fatal("owning incomplete receipt shape", err)
		}
		defer server.Close()
		response := centralRequest(context.Background(), server.Handler(), http.MethodGet, "/api/sessions/"+intent.Slug+"/creation", "", nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), intent.ReceiptID) {
			t.Fatalf("original paused receipt shape: %d %s", response.Code, response.Body.String())
		}
	}
}

func centralFixturePackage(t *testing.T, root string) {
	t.Helper()
	// The owning unmanaged wire arm contains only managed:false. Go's struct
	// omitempty does not omit zero struct fields, so encode that exact arm.
	centralJSON(t, filepath.Join(root, "share", "dev-workspace", "package.json"), map[string]any{"schema": 1, "activationEnvironmentAliases": []string{}, "routerSocket": "/fixture/router", "userNamespace": "fixture", "agent_teams": map[string]bool{"managed": false}})
	contract := map[string]any{"piTeamRecords": session.PiTeamRecords(), "piQuestionsSchema": 1, "piPlanSchema": 1, "piToolPolicy": map[string]int{"policy": 1, "conversationSchema": 2}, "hostCallbacks": 1, "linearLineage": 1, "workflowObservations": 1, "piRoleInstructions": 1}
	centralJSON(t, filepath.Join(root, "share", "workspace-portal", "runtime-contract.json"), contract)
	if _, err := agentteams.LoadInstalled(root); err != nil {
		t.Fatal("owning startup metadata", err)
	}
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.DecodePiWorkflowContract(data); err != nil {
		t.Fatal("owning workflow contract", err)
	}
	if _, err := session.DecodePiRoleInstructionsContract(data); err != nil {
		t.Fatal("owning role instructions contract", err)
	}
}

func centralWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Sync()
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
}
func centralJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	centralWrite(t, path, data)
}

func centralProviderCatalog() piproviders.Catalog {
	return piproviders.Catalog{Schema: 1, Digest: strings.Repeat("1", 64), Providers: []piproviders.Provider{{ID: "fixture", Label: "Fixture", NativeID: "p3-fake", RuntimeConfigRef: "fixture"}}, Accounts: []piproviders.Account{{Ref: "fake-account", ProviderID: "fixture", CredentialRef: piruntime.CanonicalCredentialRef, CredentialKey: "p3-fake", AuthKind: "api_key"}}, Models: []piproviders.Model{{Kind: "physical", ProviderID: "fixture", ID: "p3-model", Label: "Fixture model", ThinkingLevels: []string{"high"}, Inputs: []string{"text"}, Tools: true, EvidenceRef: "finite-fixture"}}, Routes: []piproviders.RouteGate{{ProviderID: "fixture", AccountRef: "fake-account", ModelID: "p3-model", State: "available", ReasonCode: "verified", EvidenceRef: "finite-fixture"}}}
}

func centralIntent(t *testing.T, f piruntime.P6NativeFixture, catalog agentteams.Catalog) (session.PiCreationIntent, piproviders.Catalog) {
	t.Helper()
	presets, err := teamruntime.PresetsFromCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var preset teamruntime.Preset
	for _, p := range presets {
		if p.ID == catalog.DefaultTeam {
			preset = p
		}
	}
	if preset.ID == "" {
		t.Fatal("typed fixture has no exact default team")
	}
	if preset.ID != "solo" || !slices.Equal(preset.Roles, []string{"lead"}) || len(preset.Members) != 0 {
		t.Fatal("native creation fixture requires explicit Solo")
	}
	reserved := []session.PiReservedConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64)}}
	team := session.PiCreationTeam{ID: preset.ID, Name: preset.Name, Description: preset.Description, Roles: slices.Clone(preset.Roles), CatalogDigest: preset.CatalogDigest, TeamDigest: preset.TeamDigest, LeadModel: preset.LeadModel, LeadEffort: preset.LeadEffort, LeadInstructions: preset.LeadInstructions, Members: []session.PiCreationTeamMember{}}
	for index, m := range preset.Members {
		reserved = append(reserved, session.PiReservedConversation{Address: m.Address, Incarnation: fmt.Sprintf("%064x", index+100)})
		team.Members = append(team.Members, session.PiCreationTeamMember{Role: m.Role, Address: m.Address, Model: m.Model, Effort: m.Effort, Behavior: m.Behavior, Access: m.Access, Purpose: m.Purpose, Instructions: m.Instructions})
	}
	providers := centralProviderCatalog()
	policy := piproviders.Policy{Session: f.Runtime.Session, Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: piproviders.Selection{Chain: []piproviders.Target{{ProviderID: "fixture", AccountRef: "fake-account", ModelID: "p3-model", Thinking: "high"}}, FallbackOn: []piproviders.FailureCategory{}}}, Overrides: []piproviders.Override{}}
	frozen, err := session.FreezePiCreationSelection(providers, policy, reserved)
	if err != nil {
		t.Fatal(err)
	}
	history, err := session.CompletedRemovalHistory(f.Runtime.Session.Workspace, f.Runtime.Session.Slug, filepath.Join(filepath.Dir(f.Runtime.Session.Workspace), "state"))
	if err != nil {
		t.Fatal(err)
	}
	intent := session.PiCreationIntent{Schema: 4, Workspace: f.Runtime.Session.Workspace, Slug: f.Runtime.Session.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: strings.Repeat("c", 64), DeletionHistorySHA256: history, SessionIncarnation: f.Runtime.Session.Incarnation, Conversations: reserved, GoalSHA256: piruntime.MessageDigest("publish optional question and exact plan"), Model: "p3-model", Effort: "high", Provider: "p3-fake", ProviderSelection: frozen, DirectTeam: team, InitialInputID: "44444444-4444-4444-8444-444444444444"}
	if err := intent.Validate(providers); err != nil {
		t.Fatal(err)
	}
	return intent, providers
}

// This is an honest retained-ready seed over the existing finite native fixture,
// not a claim that public portal root allocation was exercised. All native
// initialization/Accepted proof is real; the current P2 root reader validates the
// owning typed request/journal/binding/manifest and exact completion tuple.
func centralPublishReady(t *testing.T, config web.Config, intent session.PiCreationIntent, state piruntime.ConversationState, status piruntime.SubmissionStatus) {
	t.Helper()
	if status.Phase != "Accepted" || status.Witness == nil {
		t.Fatal("ready seed lacks original Accepted witness")
	}
	store := session.BackendBindingStore{Root: config.UserStateRoot, Workspace: config.Workspace}
	binding, err := store.Create(context.Background(), intent.Binding())
	if err != nil {
		t.Fatal(err)
	}
	binding.Revision++
	binding.State = "ready"
	binding.Conversations[0].State = "ready"
	binding.Conversations[0].NativeID = state.Conversation.NativeID
	binding, err = store.CompareAndSwap(context.Background(), binding.Revision-1, binding)
	if err != nil {
		t.Fatal(err)
	}
	authority := session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: intent.Workspace, Slug: intent.Slug, SessionIncarnation: intent.SessionIncarnation, BindingRevision: binding.Revision, Generation: state.Generation, State: "ready"}
	if err := session.PublishPiRuntimeAuthority(config.AuthorityDir, binding, authority); err != nil {
		t.Fatal(err)
	}
	journal := session.PiCreationJournal{PiCreationIntent: intent, State: "ready", NativeCreateStarted: true, NativeGeneration: state.Generation, Native: &session.PiCreationNative{NativeID: state.Conversation.NativeID, HistoryRef: state.HistoryRef}}
	centralJSON(t, filepath.Join(intent.Workspace, "worktrees", ".locks", intent.Slug+".creation.json"), journal)
	manifest := session.Manifest{Schema: 4, Slug: intent.Slug, Backend: &session.ManifestBackend{Kind: agentbackend.Pi, Contract: 1, SessionIncarnation: intent.SessionIncarnation}, Creation: session.Creation{State: "ready", InitialGoalSent: true, GoalSHA256: intent.GoalSHA256}, Repositories: []session.Repository{}}
	data, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	tracking := filepath.Join(intent.Workspace, "work", intent.Slug)
	centralWrite(t, filepath.Join(tracking, session.ManifestName), data)
	centralWrite(t, filepath.Join(tracking, "plan.md"), []byte("# Native workflow fixture\n"))
	centralWrite(t, filepath.Join(tracking, "state.md"), []byte("---\nlifecycle: active\n---\n"))
	var stat syscall.Stat_t
	if err := syscall.Lstat(tracking, &stat); err != nil {
		t.Fatal(err)
	}
	completion := map[string]any{"schema": 4, "workspace": intent.Workspace, "slug": intent.Slug, "backend": "pi", "backendContract": 1, "receiptId": intent.ReceiptID, "sessionIncarnation": intent.SessionIncarnation, "bindingRevision": binding.Revision, "conversations": intent.Conversations, "nativeId": state.Conversation.NativeID, "initialInputId": intent.InitialInputID, "initialSubmissionDigest": status.Ref.Digest, "goalSha256": intent.GoalSHA256, "model": intent.Model, "effort": intent.Effort, "provider": intent.Provider, "providerSelection": intent.ProviderSelection, "catalogDigest": intent.DirectTeam.CatalogDigest, "teamDigest": intent.DirectTeam.TeamDigest, "team": intent.DirectTeam.ID, "trackingDevice": uint64(stat.Dev), "trackingInode": stat.Ino}
	evidence := filepath.Join(userstate.WorkspaceDirectory(config.UserStateRoot, "portal", config.Workspace), "creations", intent.Slug+"."+intent.ReceiptID+".complete.json")
	centralJSON(t, evidence+".request", intent)
	centralJSON(t, evidence, completion)
	if _, err := session.Find(intent.Workspace, intent.Slug); err != nil {
		t.Fatal(err)
	}
}

func centralRequest(ctx context.Context, handler http.Handler, method, path, scope string, value any) *httptest.ResponseRecorder {
	var data []byte
	if value != nil {
		data, _ = json.Marshal(value)
	}
	request := httptest.NewRequest(method, "https://portal.example"+path, bytes.NewReader(data)).WithContext(ctx)
	if scope != "" {
		request.Header.Set("X-Workspace-Conversation-Scope", scope)
	}
	if method == http.MethodPost {
		request.Header.Set("Origin", "https://portal.example")
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func centralWait(t *testing.T, ctx context.Context, description string, check func() bool) {
	t.Helper()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", description, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func centralDecode(t *testing.T, response *httptest.ResponseRecorder, out any) {
	t.Helper()
	if response.Code < 200 || response.Code >= 300 {
		t.Fatalf("route %d: %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
}

type centralStreamWriter struct {
	header http.Header
	ready  chan struct{}
	once   sync.Once
	status int
}

type centralDiagnostics struct {
	mu   sync.Mutex
	tail []byte
}

func (d *centralDiagnostics) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tail = append(d.tail, data...)
	if len(d.tail) > 8192 {
		d.tail = append([]byte{}, d.tail[len(d.tail)-8192:]...)
	}
	return len(data), nil
}
func (d *centralDiagnostics) String() string { d.mu.Lock(); defer d.mu.Unlock(); return string(d.tail) }

func (w *centralStreamWriter) Header() http.Header            { return w.header }
func (w *centralStreamWriter) WriteHeader(status int)         { w.status = status }
func (w *centralStreamWriter) Write(data []byte) (int, error) { return len(data), nil }
func (w *centralStreamWriter) Flush()                         { w.once.Do(func() { close(w.ready) }) }

// The two finite cases share the original seed setup; the restart case does
// not repeat answer, mode-change or implementation actions.
type centralReadyFixture struct {
	fixture  piruntime.P6NativeFixture
	intent   session.PiCreationIntent
	catalog  agentteams.Catalog
	config   web.Config
	server   *web.Server
	handle   agentbackend.ExecutionHandle
	ops      backend.Operations
	scope    string
	original piruntime.SubmissionRef
	state    piruntime.ConversationState
	goal     string
}

func centralNativeReady(t *testing.T) centralReadyFixture {
	t.Helper()
	return centralNativeSeed(t, false, false)
}

func centralNativeSeed(t *testing.T, recovery, legacy bool) centralReadyFixture {
	t.Helper()
	path := os.Getenv("PI_CENTRAL_CATALOG_FIXTURE")
	if path == "" {
		t.Skip("finite batch must export the existing owning typed catalog fixture first")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var catalog agentteams.Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	f := piruntime.NewP6NativeFixture(t)
	ctx := f.Context
	intent, providers := centralIntent(t, f, catalog)
	goal := "publish optional question and exact plan"
	if recovery {
		goal = "one original role recovery request"
		intent.GoalSHA256 = piruntime.MessageDigest(goal)
	}
	root := filepath.Dir(f.Runtime.Session.Workspace)
	packageRoot := filepath.Join(root, "portal-package")
	profile := filepath.Join(root, "portal-profile")
	if recovery {
		start, err := f.Manager.ReadStart(ctx, intent.SessionRef())
		if err != nil {
			t.Fatal(err)
		}
		packageRoot, profile = start.AppStorePath, start.ProfilePath
		data, err := os.ReadFile(filepath.Join(packageRoot, "share/workspace-portal/runtime-contract.json"))
		if err != nil {
			t.Fatal("prepared immutable role package", err)
		}
		if _, err := session.DecodePiRoleInstructionsContract(data); err != nil {
			t.Fatal(err)
		}
		if _, err := agentteams.LoadInstalled(packageRoot); err != nil {
			t.Fatal(err)
		}
	} else {
		centralFixturePackage(t, packageRoot)
		if err := os.Symlink(packageRoot, profile); err != nil {
			t.Fatal(err)
		}
	}
	authority := filepath.Join(root, "portal-authority")
	if err := os.Mkdir(authority, 0700); err != nil {
		t.Fatal(err)
	}
	transition := filepath.Join(root, "portal-transition.lock")
	centralWrite(t, transition, nil)
	devSession, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(root, "absent-provider-catalog"))
	diagnostics := &centralDiagnostics{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("bounded portal diagnostics:\n" + diagnostics.String())
		}
	})
	config := web.Config{Workspace: intent.Workspace, BaseURL: "https://portal.example", DevSession: devSession, HostProfile: profile, AuthorityDir: authority, TransitionLock: transition, UserStateRoot: filepath.Join(root, "state"), PackageRoot: packageRoot, WorkspaceName: "central", CodexSocket: filepath.Join(root, "portal-host", "app-server.sock"), RegistrationMarker: filepath.Join(root, "portal-host", "registration.json"), Logger: log.New(diagnostics, "", 0), PiCreation: &web.PiCreationServices{Catalog: providers, Runtime: f.Manager}}
	if recovery {
		config.DevSession = centralTrackingCommand(t, config)
		config.PiCreation.Selection = func(context.Context, agentbackend.SessionRef, []session.PiReservedConversation, teamruntime.Preset) (piproviders.Policy, error) {
			t.Error("retained recovery selected replacement provider settings")
			return piproviders.Policy{}, piruntime.ErrUnavailable
		}
		config.PiCreation.StartRequest = func(current session.PiCreationIntent, _ uint64) (piruntime.StartRequest, error) {
			return f.Manager.ReadStart(ctx, current.SessionRef())
		}
	}
	server, err := web.NewContext(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	rootLock, err := session.LockRuntimeExclusiveContext(ctx, authority, intent.Slug)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if rootLock != nil {
			rootLock.Close()
		}
	})
	if !recovery {
		if err := server.BindPiWorkflowNativeFixture(ctx, agentteams.Installed{Managed: true, Catalog: &catalog}, f.Runtime, true); err != nil {
			t.Fatal(err)
		}
	}
	initial := piruntime.ToolPolicyTarget{Mode: "default", Revision: 1, Active: []string{"bash", "edit", "find", "grep", "ls", "publish_plan", "read", "request_user_input_async", "write"}, RegisteredCeiling: []string{"bash", "edit", "find", "grep", "ls", "publish_plan", "read", "request_user_input_async", "write"}}
	if recovery {
		// Reuse the owning role fixture's original retained schema-2 policy.
		// Its ordinary denied write settles without aggregate host callbacks.
		// Current first-Create/full-domain cases retain their full-nine proof.
		initial.Active, initial.RegisteredCeiling = []string{}, []string{}
	}
	initial.Digest = piruntime.ToolPolicyDigest(initial)
	role := intent.DirectTeam.LeadInstructions
	if legacy {
		role = ""
	}
	state, err := f.Manager.Create(ctx, f.Runtime, piruntime.CreateRequest{Session: f.Runtime.Session, Member: "lead", Incarnation: intent.Conversations[0].Incarnation, CreationID: intent.ReceiptID, Instructions: role, Settings: intent.ProviderSelection.Members[0].Settings.Effective(), ToolPolicy: &initial})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	if !recovery {
		proof, err := f.Manager.InspectToolPolicy(ctx, handle)
		if err != nil || piruntime.ValidateToolPolicyProof(proof, initial, handle.Generation) != nil || !slices.Equal(proof.Registered, initial.RegisteredCeiling) {
			t.Fatal("initial native Default1/full registry", proof, err)
		}
	}
	ops, err := f.Manager.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	initialized, err := f.Manager.VerifyCreation(ctx, handle, intent.ReceiptID, state.HistoryRef, nil)
	expectedRole := ""
	if role != "" {
		digest := sha256.Sum256([]byte(role))
		expectedRole = hex.EncodeToString(digest[:])
	}
	if err != nil || initialized.InstructionsDigest != expectedRole {
		t.Fatal("actual initialized role proof", initialized, err)
	}
	_, err = ops.Send.Send(ctx, piruntime.BackendIdentity(handle), backend.SendRequest{Message: goal, ClientUserMessageID: intent.InitialInputID, ActionContext: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	original := nativeRef(piquestions.Caller{Handle: handle}, intent.InitialInputID, goal, "initial", state.Settings)
	accepted, err := f.Manager.Lookup(ctx, original)
	if err != nil || accepted.Phase != "Accepted" || accepted.Witness == nil {
		t.Fatal("actual first ACK/receipt", accepted, err)
	}
	creation, err := f.Manager.VerifyCreation(ctx, handle, intent.ReceiptID, state.HistoryRef, &original)
	if err != nil || creation.Bootstrap == nil || creation.Bootstrap.Ref != original || creation.InstructionsDigest != expectedRole {
		t.Fatal("actual original creation proof", creation, err)
	}
	questionPath, _ := (piquestions.StateFile{Root: config.UserStateRoot, Kind: "pi-questions", Name: "questions.json", Ref: state.Conversation}).Path()
	if _, err := os.Stat(questionPath); !os.IsNotExist(err) {
		t.Fatal("callback published before root EX released", err)
	}
	if recovery {
		centralPublishIncomplete(t, config, intent, state, goal)
		rootLock.Close()
		rootLock = nil
		// Original callbacks may wait for SH even though input is Accepted.
		// Release EX before model settlement; the creating binding cannot
		// authorize domain effects or manufacture ready proof.
		f.Idle(handle)
		return centralReadyFixture{fixture: f, intent: intent, catalog: catalog, config: config, server: server, handle: handle, ops: ops, original: original, state: state, goal: goal}
	}
	centralPublishReady(t, config, intent, state, accepted)
	rootLock.Close()
	rootLock = nil
	page := centralRequest(ctx, server.Handler(), "GET", "/"+intent.Slug+"/", "", nil)
	if page.Code != 200 {
		t.Fatalf("page %d %s", page.Code, page.Body.String())
	}
	matches := regexp.MustCompile(`data-conversation-scope="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(matches) != 2 {
		t.Fatal("missing ordinary carrier")
	}
	scope := html.UnescapeString(matches[1])
	if !strings.Contains(page.Body.String(), `data-session-backend="pi"`) {
		t.Fatal("Pi rendered as legacy")
	}
	return centralReadyFixture{fixture: f, intent: intent, catalog: catalog, config: config, server: server, handle: handle, ops: ops, scope: scope, original: original, state: state, goal: goal}
}

// Controlled historical files carry real native creation and Accepted input.
// The public portal loader/retry still validates every retained reference.
func centralPublishIncomplete(t *testing.T, config web.Config, intent session.PiCreationIntent, state piruntime.ConversationState, goal string) {
	t.Helper()
	tracking := filepath.Join(intent.Workspace, "work", intent.Slug)
	centralWrite(t, filepath.Join(tracking, "plan.md"), []byte("# "+intent.Slug+"\n\n## Goal\n\nHistorical native creation fixture.\n\n## Affected repositories\n\n## Approach\n\n## Compatibility and deployment\n\n## Testing plan\n"))
	centralWrite(t, filepath.Join(tracking, "state.md"), []byte("---\nlifecycle: active\n---\n# "+intent.Slug+"\n\n## Repositories\n\n## Status\n\nHistorical native input accepted.\n\n## Commands run\n\n## Results\n\n## Open questions\n\n## Cleanup\n"))
	binding, err := (session.BackendBindingStore{Root: config.UserStateRoot, Workspace: config.Workspace}).Create(context.Background(), intent.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.PublishPiRuntimeAuthority(config.AuthorityDir, binding, session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: intent.Workspace, Slug: intent.Slug, SessionIncarnation: intent.SessionIncarnation, BindingRevision: binding.Revision, Generation: state.Generation, State: "starting"}); err != nil {
		t.Fatal(err)
	}
	journal := session.PiCreationJournal{PiCreationIntent: intent, State: "creating", NativeCreateStarted: true, NativeGeneration: state.Generation, Native: &session.PiCreationNative{NativeID: state.Conversation.NativeID, HistoryRef: state.HistoryRef}}
	centralJSON(t, filepath.Join(intent.Workspace, "worktrees/.locks", intent.Slug+".creation.json"), journal)
	directory := filepath.Join(userstate.WorkspaceDirectory(config.UserStateRoot, "portal", config.Workspace), "creations")
	centralJSON(t, filepath.Join(directory, intent.Slug+"."+intent.ReceiptID+".complete.json.request"), intent)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	receipt := map[string]any{"schema": 4, "workspace": intent.Workspace, "request": map[string]any{"kind": "new", "slug": intent.Slug, "goal": goal, "backend": "pi", "team": intent.DirectTeam.ID, "catalogDigest": intent.DirectTeam.CatalogDigest}, "receiptId": intent.ReceiptID, "deletionHistorySha256": intent.DeletionHistorySHA256, "attempt": 1, "state": "paused", "phase": "Historical native input accepted", "startedAt": now, "updatedAt": now, "validated": true, "goal": goal, "model": intent.Model, "effort": intent.Effort, "directTeam": intent.DirectTeam, "pi": map[string]any{"backendContract": 1, "sessionIncarnation": intent.SessionIncarnation, "conversations": intent.Conversations, "provider": intent.Provider, "initialInputId": intent.InitialInputID, "selection": intent.ProviderSelection}}
	centralJSON(t, filepath.Join(directory, intent.Slug+".json"), receipt)
}

func centralTrackingCommand(t *testing.T, config web.Config) string {
	t.Helper()
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../../libexec/dev-session")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { data, _ := json.Marshal(value); return string(data) }
	path := filepath.Join(t.TempDir(), "tracking-child")
	centralWrite(t, path, []byte("#!"+ruby+"\nENV['DEV_WORKSPACES_STATE']="+quote(config.UserStateRoot)+"\nENV.delete('DEV_WORKSPACE_NAME')\nENV.delete('DEV_SESSION_REQUIRE_RUNTIME')\nload "+quote(source)+"\nARGV.unshift('--workspace',"+quote(config.Workspace)+",'--authority-dir',"+quote(config.AuthorityDir)+")\nexit DevSession::CLI.new(ARGV).run\n"))
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every port delegates to the same actual Manager. Counters distinguish reads
// of original proof from forbidden replacement Create/Load/Bind-to-Send paths.
type centralCreationRuntime struct {
	*piruntime.Manager
	creates, loads, binds, verifies int
	lastDigest                      string
	lastVerifyError                 error
	lastProof                       piruntime.CreationProof
}

func (r *centralCreationRuntime) Create(ctx context.Context, runtime piruntime.RuntimeRef, request piruntime.CreateRequest) (piruntime.ConversationState, error) {
	r.creates++
	return r.Manager.Create(ctx, runtime, request)
}
func (r *centralCreationRuntime) Load(ctx context.Context, handle agentbackend.ExecutionHandle) (piruntime.ConversationState, error) {
	r.loads++
	return r.Manager.Load(ctx, handle)
}
func (r *centralCreationRuntime) Bind(ctx context.Context, handle agentbackend.ExecutionHandle) (backend.Operations, error) {
	r.binds++
	return r.Manager.Bind(ctx, handle)
}
func (r *centralCreationRuntime) VerifyCreation(ctx context.Context, handle agentbackend.ExecutionHandle, creation, history string, bootstrap *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	r.verifies++
	proof, err := r.Manager.VerifyCreation(ctx, handle, creation, history, bootstrap)
	r.lastDigest = proof.InstructionsDigest
	r.lastVerifyError, r.lastProof = err, proof
	return proof, err
}

func TestPiCentralNativeRoleRecovery(t *testing.T) {
	if os.Getenv("PI_CENTRAL_APP_ROOT") == "" {
		t.Skip("finite role recovery requires the prepared immutable app fixture")
	}
	t.Setenv("DEV_SESSION_REQUIRE_RUNTIME", "1")
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			seed := centralNativeSeed(t, true, legacy)
			f, config := seed.fixture, seed.config
			original, err := f.Manager.Lookup(f.Context, seed.original)
			if err != nil || original.Phase != "Accepted" {
				t.Fatal(err)
			}
			journalPath := filepath.Join(config.Workspace, "worktrees/.locks", seed.intent.Slug+".creation.json")
			originalJournal, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			before := map[string]string{}
			if err := filepath.WalkDir(config.UserStateRoot, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() && (entry.Name() == "conversation.json" || entry.Name() == seed.original.OperationID+".json" || strings.HasSuffix(path, ".complete.json.request")) {
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					before[path] = string(data)
				}
				return nil
			}); err != nil || len(before) < 3 {
				t.Fatal("native mapping/receipt/companion snapshot", err, len(before))
			}
			runtime := &centralCreationRuntime{Manager: f.Manager}
			services := *config.PiCreation
			services.Runtime = runtime
			config.PiCreation = &services
			server, err := web.NewContext(f.Context, config)
			if err != nil {
				t.Fatal("owning historical receipt loader", err)
			}
			defer server.Close()
			// The new server already loaded the original receipt. No catalog
			// binder or preattachment is allowed after this point.
			api := "/api/sessions/" + seed.intent.Slug + "/creation"
			response := centralRequest(f.Context, server.Handler(), http.MethodPost, api+"/retry", "", map[string]any{"receiptId": seed.intent.ReceiptID, "attempt": 1})
			if response.Code != http.StatusAccepted {
				t.Fatalf("public original retry %d %s", response.Code, response.Body.String())
			}
			var status struct {
				State, Error, ReceiptID string
				Attempt                 int
			}
			centralWait(t, f.Context, "public original creation retry reached terminal proof", func() bool {
				response := centralRequest(f.Context, server.Handler(), http.MethodGet, api, "", nil)
				return response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &status) == nil && status.State != "running"
			})
			if status.ReceiptID != seed.intent.ReceiptID || status.Attempt != 2 || runtime.creates != 0 || runtime.loads != 0 || runtime.binds != 0 {
				t.Fatalf("retry replaced original input/identity: %#v ports=%+v", status, runtime)
			}
			if legacy {
				if status.State != "failed" || !strings.Contains(status.Error, "The original Pi lead instructions could not be verified") || runtime.verifies != 1 || runtime.lastDigest != "" || runtime.lastVerifyError != nil || runtime.lastProof.Bootstrap == nil || runtime.lastProof.Bootstrap.Ref != seed.original || runtime.lastProof.Bootstrap.Phase != "Accepted" {
					t.Fatalf("legacy omission failed before expected role proof: %#v verifies=%d digest=%q", status, runtime.verifies, runtime.lastDigest)
				}
				binding, err := (session.BackendBindingStore{Root: config.UserStateRoot, Workspace: config.Workspace}).Load(seed.intent.Slug)
				if err != nil || binding.State != "creating" || binding.Revision != 1 {
					t.Fatal("legacy omission published ready", binding, err)
				}
				journal, err := os.ReadFile(journalPath)
				if err != nil || !bytes.Equal(journal, originalJournal) {
					t.Fatal("legacy recovery changed original native journal", err)
				}
			} else {
				digest := sha256.Sum256([]byte(seed.intent.DirectTeam.LeadInstructions))
				if status.State != "ready" || runtime.verifies < 2 || runtime.lastDigest != hex.EncodeToString(digest[:]) {
					t.Fatalf("exact current role proof failed recovery: %#v verifies=%d", status, runtime.verifies)
				}
			}
			for path, saved := range before {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != saved {
					t.Fatalf("original mapping/receipt/intent changed: %s %v", path, err)
				}
			}
			after, err := f.Manager.Lookup(f.Context, seed.original)
			if err != nil || !reflect.DeepEqual(after, original) {
				t.Fatal("original Accepted receipt changed", after, err)
			}
			transcript, err := seed.ops.Read.Read(f.Context, piruntime.BackendIdentity(seed.handle))
			if err != nil {
				t.Fatal(err)
			}
			inputs := 0
			for _, entry := range transcript.Entries {
				if entry.Kind == "user" {
					inputs++
				}
			}
			if inputs != 1 || os.Getenv("DEV_SESSION_REQUIRE_RUNTIME") != "1" {
				t.Fatal("recovery replayed user input or changed parent runtime policy", inputs)
			}
		})
	}
}

func TestPiCentralWorkflowNativeComposition(t *testing.T) {
	seed := centralNativeReady(t)
	f, intent, catalog, config := seed.fixture, seed.intent, seed.catalog, seed.config
	ctx, server, handle, ops, scope := f.Context, seed.server, seed.handle, seed.ops, seed.scope
	defer func() { server.Close() }()
	handler := server.Handler()
	var questions piquestions.Snapshot
	var plans piplan.Snapshot
	lastViews := ""
	defer func() {
		if t.Failed() {
			t.Log("last workflow views: " + lastViews)
		}
	}()
	api := "/api/sessions/" + intent.Slug
	centralWait(t, ctx, "actual callbacks and coherent question/Plan view", func() bool {
		q := centralRequest(ctx, handler, "GET", api+"/pi-questions", scope, nil)
		p := centralRequest(ctx, handler, "GET", api+"/pi-plan", scope, nil)
		lastViews = fmt.Sprintf("questions HTTP%d %s; Plan HTTP%d %s", q.Code, q.Body.String(), p.Code, p.Body.String())
		if q.Code != 200 || p.Code != 200 {
			return false
		}
		if json.Unmarshal(q.Body.Bytes(), &questions) != nil || json.Unmarshal(p.Body.Bytes(), &plans) != nil {
			return false
		}
		return len(questions.Questions) == 1 && questions.Questions[0].CanRespond && len(plans.Proposals) == 1 && plans.CanSelectMode
	})
	if plans.Proposals[0].Text != nativePlanText || plans.Proposals[0].TextSHA256 != piplan.Digest(nativePlanText) {
		t.Fatal("exact CRLF/Unicode Plan lost")
	}
	question := questions.Questions[0]
	responseInput := piquestions.ResponseInput{ResponseID: "99999999-9999-4999-8999-999999999999", QuestionID: question.ID, QuestionRevision: question.Revision, Answers: []piquestions.Answer{{QuestionID: question.Questions[0].ID, ChoiceID: question.Questions[0].Options[0].ID}}}
	var response piquestions.ResponseView
	centralDecode(t, centralRequest(ctx, handler, "POST", api+"/pi-questions/responses", scope, responseInput), &response)
	centralWait(t, ctx, "original ordinary answer receipt", func() bool {
		r := centralRequest(ctx, handler, "GET", api+"/pi-questions/responses/"+responseInput.ResponseID, scope, nil)
		return r.Code == 200 && json.Unmarshal(r.Body.Bytes(), &response) == nil && response.DeliveryPhase == "Accepted"
	})
	centralWait(t, ctx, "idle policy after answer", func() bool {
		r := centralRequest(ctx, handler, "GET", api+"/pi-plan", scope, nil)
		return r.Code == 200 && json.Unmarshal(r.Body.Bytes(), &plans) == nil && plans.CanSelectMode
	})
	modeInput := piplan.ModeInput{Mode: "plan", ExpectedRevision: plans.ToolPolicyRevision, ExpectedDigest: plans.ToolPolicyDigest}
	var mode piplan.ModeResult
	centralDecode(t, centralRequest(ctx, handler, "POST", api+"/pi-plan/mode", scope, modeInput), &mode)
	if mode.Outcome != "applied" || mode.Policy == nil || mode.Policy.Mode != "plan" {
		t.Fatal("manual mode proof", mode)
	}
	if r := centralRequest(ctx, handler, "POST", api+"/pi-plan/mode", scope, modeInput); r.Code != 409 {
		t.Fatalf("stale CAS %d", r.Code)
	}
	centralWait(t, ctx, "fresh Plan acceptance proof", func() bool {
		r := centralRequest(ctx, handler, "GET", api+"/pi-plan", scope, nil)
		return r.Code == 200 && json.Unmarshal(r.Body.Bytes(), &plans) == nil && len(plans.Proposals) == 1 && plans.Proposals[0].CanAccept
	})
	proposal := plans.Proposals[0]
	consent := piplan.AcceptanceInput{AcceptanceID: "88888888-8888-4888-8888-888888888888", PlanID: proposal.PlanID, Revision: proposal.Revision, TextSHA256: proposal.TextSHA256}
	var implementation piplan.AcceptanceView
	centralDecode(t, centralRequest(ctx, handler, "POST", api+"/pi-plan/acceptances", scope, consent), &implementation)
	centralWait(t, ctx, "original implementation receipt", func() bool {
		r := centralRequest(ctx, handler, "GET", api+"/pi-plan/acceptances/"+consent.AcceptanceID, scope, nil)
		return r.Code == 200 && json.Unmarshal(r.Body.Bytes(), &implementation) == nil && implementation.Phase == "accepted"
	})
	centralWait(t, ctx, "Default-only fake write", func() bool {
		data, err := os.ReadFile(filepath.Join(intent.Workspace, "work", intent.Slug, "owned-p6-implementation"))
		return err == nil && string(data) == "accepted plan implemented"
	})
	before, err := f.Manager.ObserveWorkflow(ctx, handle)
	if err != nil || before.Retained == nil || before.Retained.Mode != "default" || before.Retained.Revision != 3 {
		t.Fatal("exact Default target", before, err)
	}
	centralDecode(t, centralRequest(ctx, handler, "POST", api+"/pi-questions/responses", scope, responseInput), &response)
	centralDecode(t, centralRequest(ctx, handler, "POST", api+"/pi-plan/acceptances", scope, consent), &implementation)
	wrongScope := "pi-workflow-v1:" + strings.Repeat("f", 64)
	if r := centralRequest(ctx, handler, "POST", "/codex/conversations/"+intent.Slug+"/message", wrongScope, map[string]any{"message": "must not send", "clientUserMessageId": "77777777-7777-4777-8777-777777777777"}); r.Code == 200 {
		t.Fatal("wrong page scope sent input")
	}
	transcript, err := ops.Read.Read(ctx, piruntime.BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "user" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("duplicate/reconciliation created input: %d", count)
	}
	// The real EventSource route consumes its sole scope query before C and
	// keeps no authority lock over the established stream lifetime.
	streamContext, stopStream := context.WithCancel(ctx)
	stream := &centralStreamWriter{header: make(http.Header), ready: make(chan struct{})}
	streamDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest("GET", "https://portal.example/codex/conversations/"+intent.Slug+"/events?conversationScope="+scope, nil).WithContext(streamContext)
		handler.ServeHTTP(stream, request)
		close(streamDone)
	}()
	select {
	case <-stream.ready:
	case <-ctx.Done():
		t.Fatal("scoped event stream did not establish", ctx.Err())
	}
	streamLockContext, stopLock := context.WithTimeout(ctx, time.Second)
	streamLock, err := session.LockRuntimeExclusiveContext(streamLockContext, config.AuthorityDir, intent.Slug)
	stopLock()
	if err != nil {
		stopStream()
		t.Fatal("stream pinned authority lock", err)
	}
	streamLock.Close()
	stopStream()
	select {
	case <-streamDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	literal, err := f.Manager.ReadCurrentRuntime(ctx, handle.Conversation.Session)
	if err != nil || literal != f.Runtime {
		t.Fatal("retained identity changed", literal, err)
	}
	server.Close()
	replacement, err := web.NewContext(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	server = replacement
	if err := server.BindPiWorkflowNativeFixture(ctx, agentteams.Installed{Managed: true, Catalog: &catalog}, literal, true); err != nil {
		t.Fatal(err)
	}
	handler = server.Handler()
	centralWait(t, ctx, "new Server retained attachment/current proof", func() bool {
		r := centralRequest(ctx, handler, "GET", api+"/pi-plan", scope, nil)
		return r.Code == 200 && json.Unmarshal(r.Body.Bytes(), &plans) == nil && plans.CanSelectMode
	})
	after, err := f.Manager.ObserveWorkflow(ctx, handle)
	if err != nil || after.AttachmentID == before.AttachmentID {
		t.Fatal("restart reused attachment proof", after, err)
	}
	if r := centralRequest(ctx, handler, "GET", api+"/pi-questions?member=missing0", scope, nil); r.Code == 200 {
		t.Fatal("unknown member retargeted lead")
	}
	t.Log("Actual aggregate callback, durable question/Plan, scoped routes/carrier, fresh action, ordinary receipts, CAS, duplicate no-resend and retained Server attachment proved; typed catalog/raw startup boundary and public root allocation remain separately scoped.")
}

func TestPiCentralWorkflowNativeRetainedRestart(t *testing.T) {
	seed := centralNativeReady(t)
	f, ctx := seed.fixture, seed.fixture.Context
	var before piruntime.WorkflowObservation
	centralWait(t, ctx, "original input and callbacks settled before portal restart", func() bool {
		var err error
		before, err = f.Manager.ObserveWorkflow(ctx, seed.handle)
		return err == nil && before.Idle
	})
	original, err := f.Manager.Lookup(ctx, seed.original)
	if err != nil || original.Phase != "Accepted" || original.Witness == nil {
		t.Fatal("original seed receipt", original, err)
	}
	seed.server.Close()
	reopened, err := f.Reopen()
	if err != nil || reopened == f.Manager {
		t.Fatal("independent retained Manager", err)
	}
	literal, err := reopened.ReadCurrentRuntime(ctx, seed.handle.Conversation.Session)
	if err != nil || literal != f.Runtime {
		t.Fatal("new Manager literal current Runtime", literal, err)
	}
	config := seed.config
	creation := *config.PiCreation
	creation.Runtime = reopened
	config.PiCreation = &creation
	server, err := web.NewContext(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.BindPiWorkflowNativeFixture(ctx, agentteams.Installed{Managed: true, Catalog: &seed.catalog}, literal, false); err != nil {
		t.Fatal(err)
	}
	// No fixture preattachment may certify the new portal. Observe is read-only
	// and refuses the supervisor's disconnected negotiated workflow barrier.
	if observed, err := reopened.ObserveWorkflow(ctx, seed.handle); err == nil || observed.AttachmentID != "" {
		t.Fatal("new portal started with positive attachment proof", observed, err)
	}
	handler := server.Handler()
	api := "/api/sessions/" + seed.intent.Slug
	var details map[string]any
	centralDecode(t, centralRequest(ctx, handler, "GET", api+"/details", seed.scope, nil), &details)
	if details["sessionBackend"] != "pi" || details["conversationScope"] != seed.scope || details["conversationInteractive"] != true || details["conversationHeld"] != false {
		t.Fatal("native-free ordinary details demand", details)
	}
	var questions piquestions.Snapshot
	var plans piplan.Snapshot
	centralWait(t, ctx, "ordinary demand admitted background retained attachment and coherent proof", func() bool {
		q := centralRequest(ctx, handler, "GET", api+"/pi-questions", seed.scope, nil)
		p := centralRequest(ctx, handler, "GET", api+"/pi-plan", seed.scope, nil)
		return q.Code == 200 && p.Code == 200 && json.Unmarshal(q.Body.Bytes(), &questions) == nil && json.Unmarshal(p.Body.Bytes(), &plans) == nil && len(questions.Questions) == 1 && questions.Questions[0].CanRespond && len(plans.Proposals) == 1 && plans.CanSelectMode
	})
	after, err := reopened.ObserveWorkflow(ctx, seed.handle)
	if err != nil || after.AttachmentID == "" || after.AttachmentID == before.AttachmentID || !after.Idle {
		t.Fatal("new background attachment did not replace original proof", after, err)
	}
	current, err := reopened.ReadCurrentRuntime(ctx, seed.handle.Conversation.Session)
	if err != nil || current != literal {
		t.Fatal("background attachment changed full Runtime", current, err)
	}
	receipt, err := reopened.Lookup(ctx, seed.original)
	if err != nil || !reflect.DeepEqual(receipt, original) {
		t.Fatal("restart changed original receipt", receipt, err)
	}
	ops, err := reopened.Bind(ctx, seed.handle)
	if err != nil || ops.Read == nil {
		t.Fatal("retained conversation operations", err)
	}
	transcript, err := ops.Read.Read(ctx, piruntime.BackendIdentity(seed.handle))
	if err != nil {
		t.Fatal(err)
	}
	inputs := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "user" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Fatalf("restart created or replayed user input: %d", inputs)
	}
	t.Log("New Manager over exact retained fixture config/disk plus new Server: no Ensure, Load or fixture preattachment; scoped ordinary GET demand drove admitted background current-read/AttachHost/ACK/full-ref refresh, coherent workflows and unchanged original receipt/one user input.")
}
