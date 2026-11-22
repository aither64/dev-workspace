//go:build pi_native_fixture

package piruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
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

	"github.com/aither64/codex-web/conversation"
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
	return centralIntentWithProviders(t, f, catalog, centralProviderCatalog())
}

func centralIntentWithProviders(t *testing.T, f piruntime.P6NativeFixture, catalog agentteams.Catalog, providers piproviders.Catalog) (session.PiCreationIntent, piproviders.Catalog) {
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
	reserved := []session.PiReservedConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64)}}
	team := session.PiCreationTeam{ID: preset.ID, Name: preset.Name, Description: preset.Description, Roles: slices.Clone(preset.Roles), CatalogDigest: preset.CatalogDigest, TeamDigest: preset.TeamDigest, LeadModel: preset.LeadModel, LeadEffort: preset.LeadEffort, LeadInstructions: preset.LeadInstructions, Members: []session.PiCreationTeamMember{}}
	for index, m := range preset.Members {
		reserved = append(reserved, session.PiReservedConversation{Address: m.Address, Incarnation: fmt.Sprintf("%064x", index+100)})
		team.Members = append(team.Members, session.PiCreationTeamMember{Role: m.Role, Address: m.Address, Model: m.Model, Effort: m.Effort, Behavior: m.Behavior, Access: m.Access, Purpose: m.Purpose, Instructions: m.Instructions})
	}
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
	return centralNativeSeedWithUpgrade(t, recovery, legacy, false)
}

func centralNativeSeedWithUpgrade(t *testing.T, recovery, legacy, upgrade bool, settings ...bool) centralReadyFixture {
	t.Helper()
	settingsCase := len(settings) == 1 && settings[0]
	teamsCase := len(settings) == 2 && settings[1]
	settingsCase = settingsCase || teamsCase
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
	var f piruntime.P6NativeFixture
	if teamsCase && catalog.DefaultTeam != "lead_reviewed" {
		t.Skip("finite team case requires its lead-and-reviewer catalog fixture")
	}
	if upgrade {
		f = piruntime.NewP6NativeUpgradeFixture(t)
	} else if teamsCase {
		f = piruntime.NewP6SharedNativeFixture(t)
	} else {
		f = piruntime.NewP6NativeFixture(t)
	}
	ctx := f.Context
	intent, providers := centralIntent(t, f, catalog)
	var settingsPackage piproviders.PackageConfiguration
	if settingsCase {
		start, err := f.Manager.ReadStart(ctx, f.Runtime.Session)
		if err != nil {
			t.Fatal(err)
		}
		settingsPackage, err = piproviders.LoadInstalled(start.AppStorePath)
		if err != nil {
			t.Fatal("settings immutable provider input", err)
		}
		intent, providers = centralIntentWithProviders(t, f, catalog, settingsPackage.Catalog)
	}
	if upgrade {
		start, err := f.Manager.ReadStart(ctx, f.Runtime.Session)
		if err != nil {
			t.Fatal(err)
		}
		original, err := piproviders.LoadInstalled(start.AppStorePath)
		if err != nil {
			t.Fatal("original fixture catalog", err)
		}
		intent, _ = centralIntentWithProviders(t, f, catalog, original.Catalog)
		intent.InitialInputID = f.Runtime.RequestID
		current, err := piproviders.LoadInstalled(os.Getenv("PI_USABILITY_PACKAGE"))
		if err != nil {
			t.Fatal("current fixture catalog", err)
		}
		providers = current.Catalog
	}
	goal := "publish optional question and exact plan"
	if recovery {
		goal = "one original role recovery request"
		intent.GoalSHA256 = piruntime.MessageDigest(goal)
	}
	root := filepath.Dir(f.Runtime.Session.Workspace)
	packageRoot := filepath.Join(root, "portal-package")
	profile := filepath.Join(root, "portal-profile")
	if recovery || upgrade || settingsCase {
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
		if upgrade {
			packageRoot = os.Getenv("PI_USABILITY_PACKAGE")
			if packageRoot == "" {
				t.Fatal("upgrade requires the explicitly retained candidate metadata fixture")
			}
			if err := os.Remove(profile); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(packageRoot, profile); err != nil {
				t.Fatal(err)
			}
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
	if settingsCase {
		selectionConfig := config
		selectionConfig.PiProviderConfig = &settingsPackage
		selectionConfig.PiAuthDirectory = filepath.Join(root, "auth")
		selectionConfig.PiHostStateRoot = filepath.Join(root, "host")
		services, err := web.NewPiCreationServices(selectionConfig)
		if err != nil {
			t.Fatal("production settings provider composition", err)
		}
		// Retain only the already-existing trusted in-process runtime port.
		services.Runtime = f.Manager
		config.PiCreation = services
	}
	if recovery || upgrade {
		config.DevSession = centralTrackingCommand(t, config)
		config.PiCreation.Selection = func(context.Context, agentbackend.SessionRef, []session.PiReservedConversation, teamruntime.Preset) (piproviders.Policy, error) {
			t.Error("retained recovery selected replacement provider settings")
			return piproviders.Policy{}, piruntime.ErrUnavailable
		}
		config.PiCreation.StartRequest = func(current session.PiCreationIntent, _ uint64) (piruntime.StartRequest, error) {
			if upgrade {
				t.Error("ready upgrade attempted initial creation start")
				return piruntime.StartRequest{}, piruntime.ErrUnavailable
			}
			return f.Manager.ReadStart(ctx, current.SessionRef())
		}
	}
	server, err := web.NewContext(ctx, config)
	if err != nil {
		t.Fatal("native seed portal construction", err)
	}
	if upgrade {
		if err := server.BindPiRetainedCatalogNativeFixture(); err != nil {
			t.Fatal(err)
		}
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
			t.Fatal("native seed host attachment", err)
		}
	}
	initial := piruntime.ToolPolicyTarget{Mode: "default", Revision: 1, Active: []string{"bash", "edit", "find", "grep", "ls", "publish_plan", "read", "request_user_input_async", "write"}, RegisteredCeiling: []string{"bash", "edit", "find", "grep", "ls", "publish_plan", "read", "request_user_input_async", "write"}}
	if teamsCase {
		policy, err := server.PiInitialWorkflowPolicyNativeFixture(intent, "lead")
		if err != nil || policy == nil || !slices.Contains(policy.RegisteredCeiling, "report_to_lead") || !slices.Contains(policy.RegisteredCeiling, "team_message") {
			t.Fatal("current production-composed team root policy", policy, err)
		}
		initial = *policy
	}
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
	if teamsCase {
		// Seed the reserved roster through its actual admitted writer. The owning
		// web creation selector separately exercises allocation before lead Send.
		store, err := teamruntime.NewStore(config.UserStateRoot, config.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		roster := teamruntime.PiRoster{Schema: 2, Workspace: intent.Workspace, Slug: intent.Slug, Revision: 1, CreatedAt: now, UpdatedAt: now, PresetID: intent.DirectTeam.ID, CatalogDigest: intent.DirectTeam.CatalogDigest, TeamDigest: intent.DirectTeam.TeamDigest, LeadModel: intent.DirectTeam.LeadModel, LeadEffort: intent.DirectTeam.LeadEffort, LeadInstructions: intent.DirectTeam.LeadInstructions, Backend: "pi", BackendContract: 1, SessionIncarnation: intent.SessionIncarnation, LeadIncarnation: intent.Conversations[0].Incarnation, Members: []teamruntime.PiRosterMember{}}
		for i, m := range intent.DirectTeam.Members {
			roster.Members = append(roster.Members, teamruntime.PiRosterMember{Address: m.Address, Role: m.Role, Model: m.Model, Effort: m.Effort, Behavior: m.Behavior, Purpose: m.Purpose, Instructions: m.Instructions, Access: m.Access, PolicyCatalogDigest: intent.DirectTeam.CatalogDigest, State: "creating", AddedAt: now, Incarnation: intent.Conversations[i+1].Incarnation, CreationID: intent.ReceiptID})
		}
		if err := store.SavePi(ctx, server, handle.Generation, 0, roster); err != nil {
			t.Fatal("admitted reserved roster", err)
		}
	}
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

func TestPiOrdinaryTeamFirstRealAssignmentAndNativeReport(t *testing.T) {
	path := os.Getenv("PI_CENTRAL_CATALOG_FIXTURE")
	if path == "" {
		t.Skip("finite team case requires its exact catalog fixture")
	}
	t.Run("catalog", func(t *testing.T) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var catalog agentteams.Catalog
		if json.Unmarshal(data, &catalog) != nil {
			t.Fatal("catalog JSON")
		}
		if err := catalog.Validate(); err != nil {
			t.Fatal(err)
		}
		preset, err := teamruntime.FindCatalogPreset(catalog, "lead_reviewed")
		if err != nil || catalog.DefaultTeam != "lead_reviewed" || len(preset.Members) != 1 || preset.Members[0].Role != "reviewer" || preset.Members[0].Access != "read_only" {
			t.Fatal("exact team fixture", preset, err)
		}
		app, pi := os.Getenv("PI_CENTRAL_APP_ROOT"), os.Getenv("PI_PROBE_PACKAGE")
		canonical, err := filepath.EvalSymlinks(app)
		info, statErr := os.Stat(app)
		if err != nil || statErr != nil || canonical != app || !info.IsDir() || info.Mode().Perm()&0222 != 0 {
			t.Fatal("immutable application fixture prerequisite", app, err, statErr)
		}
		if err := piruntime.ValidatePackage(app); err != nil {
			t.Fatal("owning native manifests", err)
		}
		metadata, err := os.ReadFile(filepath.Join(app, "share/dev-workspace/pi-worker.json"))
		if err != nil {
			t.Fatal(err)
		}
		worker, err := piruntime.DecodeWorkerMetadata(metadata)
		if err != nil || worker.Protocol != 2 {
			t.Fatal("current shared-worker protocol prerequisite", worker, err)
		}
		if _, err := piproviders.LoadInstalled(app); err != nil {
			t.Fatal("owning provider/model configuration", err)
		}
		if _, err := agentteams.LoadInstalled(app); err != nil {
			t.Fatal("owning package metadata", err)
		}
		if supported, err := piruntime.InputDeliveryPackage(app); err != nil || !supported {
			t.Fatal("native input delivery capability", supported, err)
		}
		contract, err := os.ReadFile(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.DecodePiWorkflowContract(contract); err != nil {
			t.Fatal(err)
		}
		if _, err := session.DecodePiRoleInstructionsContract(contract); err != nil {
			t.Fatal(err)
		}
		var shape struct {
			PiTeamRecords json.RawMessage `json:"piTeamRecords"`
		}
		if json.Unmarshal(contract, &shape) != nil {
			t.Fatal("runtime contract")
		}
		if _, err := session.DecodePiTeamRecordsContract(shape.PiTeamRecords); err != nil {
			t.Fatal(err)
		}
		wrapper, err := os.ReadFile(filepath.Join(pi, "bin/pi"))
		if err != nil {
			t.Fatal("pinned SDK wrapper", err)
		}
		node := regexp.MustCompile(`/nix/store/[a-z0-9]{32}-nodejs-[^/\s"']+/bin/node`).FindString(string(wrapper))
		entry, err := filepath.Abs("../../../pi-runtime/test/fake-worker.mjs")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{node, entry, filepath.Join(pi, "lib/pi/node_modules/@earendil-works/pi-coding-agent/dist/index.js"), filepath.Join(pi, "lib/pi/node_modules/@earendil-works/pi-ai/dist/index.js")} {
			if info, err := os.Stat(name); err != nil || !info.Mode().IsRegular() {
				t.Fatal("native fixture file prerequisite", name, err)
			}
		}
	})
	t.Run("native", func(t *testing.T) {
		f := centralNativeSeedWithUpgrade(t, false, false, false, true, true)
		ctx := f.fixture.Context
		t.Log("current team lead initialized; awaiting lead idle")
		f.fixture.Idle(f.handle)
		t.Log("lead idle; submitting first real member assignment")
		store, err := teamruntime.NewStore(f.config.UserStateRoot, f.config.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		roster, err := store.LoadPi(f.intent.Slug)
		if err != nil || len(roster.Members) != 1 || roster.Members[0].State != "creating" {
			t.Fatal(roster, err)
		}
		member := roster.Members[0]
		id := "abababab-abab-4bab-8bab-abababababab"
		request := map[string]string{"action": "assign", "from": "lead", "to": member.Address, "messageId": id, "message": "one real member assignment"}
		path := "/api/sessions/" + f.intent.Slug + "/team"
		first := centralRequest(ctx, f.server.Handler(), http.MethodPost, path, f.scope, request)
		if first.Code != http.StatusOK {
			t.Fatalf("first real assignment: %d %s", first.Code, first.Body.String())
		}
		var initial teamruntime.PiDeliveryResult
		centralDecode(t, first, &initial)
		if initial.Outcome != backend.Pending && initial.Outcome != backend.Accepted {
			t.Fatal(initial)
		}
		ref := agentbackend.ConversationRef{Session: f.intent.SessionRef(), Backend: agentbackend.Pi, Member: member.Address, Incarnation: member.Incarnation}
		var state piruntime.ConversationState
		centralWait(t, ctx, "original member consumed input", func() bool {
			var status piruntime.SubmissionStatus
			state, status, err = f.fixture.Manager.ReadMemberAdmission(ctx, ref, member.CreationID, id)
			return err == nil && status.Phase == "Accepted" && status.Witness != nil
		})
		handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
		memberPolicy, err := f.server.PiInitialWorkflowPolicyNativeFixture(f.intent, member.Address)
		if err != nil || memberPolicy == nil {
			t.Fatal(err)
		}
		root, err := piruntime.SessionStateDirectory(f.config.UserStateRoot, f.intent.SessionRef())
		if err != nil {
			t.Fatal(err)
		}
		var topology []struct {
			WorkerPID      int
			WorkerTopology string
		}
		for _, ref := range []agentbackend.ConversationRef{f.handle.Conversation, handle.Conversation} {
			data, err := os.ReadFile(filepath.Join(root, "members", ref.Member, ref.Incarnation, "conversation.json"))
			if err != nil {
				t.Fatal(err)
			}
			var worker struct {
				WorkerPID      int
				WorkerTopology string
			}
			if json.Unmarshal(data, &worker) != nil {
				t.Fatal("worker topology codec")
			}
			topology = append(topology, worker)
		}
		if topology[0].WorkerPID <= 0 || topology[0].WorkerPID != topology[1].WorkerPID || topology[0].WorkerTopology != "shared-session-v1" || topology[1].WorkerTopology != topology[0].WorkerTopology {
			t.Fatal("actual shared host topology", topology)
		}
		t.Log("original member input Accepted; awaiting member idle")
		f.fixture.Idle(handle)
		memberProof, err := f.fixture.Manager.InspectToolPolicy(ctx, handle)
		if err != nil || piruntime.ValidateToolPolicyProof(memberProof, *memberPolicy, handle.Generation) != nil || !slices.Equal(memberProof.Registered, memberPolicy.RegisteredCeiling) {
			t.Fatal("current native member registered policy", memberProof, err)
		}
		again := centralRequest(ctx, f.server.Handler(), http.MethodPost, path, f.scope, request)
		if again.Code != http.StatusOK {
			t.Fatalf("same-ID enquiry: %d %s", again.Code, again.Body.String())
		}
		var accepted teamruntime.PiDeliveryResult
		centralDecode(t, again, &accepted)
		if accepted.Outcome != backend.Accepted {
			t.Fatal(accepted)
		}
		binding, err := (session.BackendBindingStore{Root: f.config.UserStateRoot, Workspace: f.config.Workspace}).Load(f.intent.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if actual, err := binding.Resolve(member.Address); err != nil || actual != state.Conversation {
			t.Fatal("publication", actual, err)
		}
		roster, err = store.LoadPi(f.intent.Slug)
		if err != nil || roster.Members[0].State != "ready" || roster.Members[0].InitialInputID != id {
			t.Fatal("ready roster", roster, err)
		}
		operations, err := f.fixture.Manager.Bind(ctx, handle)
		if err != nil {
			t.Fatal(err)
		}
		transcript, err := operations.Read.Read(ctx, piruntime.BackendIdentity(handle))
		if err != nil {
			t.Fatal(err)
		}
		users := 0
		for _, entry := range transcript.Entries {
			if entry.Kind == "userMessage" {
				users++
			}
		}
		if users != 1 {
			t.Fatalf("first request replayed: %d users", users)
		}
		reportID := "77777777-7777-4777-8777-777777777777"
		centralWait(t, ctx, "native report consumed by lead", func() bool {
			_, status, e := f.fixture.Manager.ReadMemberAdmission(ctx, f.handle.Conversation, f.intent.ReceiptID, reportID)
			return e == nil && status.Phase == "Accepted" && status.Witness != nil
		})
		if original, err := f.fixture.Manager.Lookup(ctx, f.original); err != nil || original.Phase != "Accepted" {
			t.Fatal("original lead receipt changed", original, err)
		}
	})
}

func TestPiSettingsHTTPNativeConfigurePreservesOriginalInput(t *testing.T) {
	if os.Getenv("PI_CENTRAL_APP_ROOT") == "" {
		t.Skip("existing immutable native metadata fixture required")
	}
	seed := centralNativeSeedWithUpgrade(t, false, false, false, true)
	ctx, manager, handle, handler := seed.fixture.Context, seed.fixture.Manager, seed.handle, seed.server.Handler()
	seed.fixture.Idle(handle)
	read := func() backend.Transcript {
		t.Helper()
		var result backend.Transcript
		centralDecode(t, centralRequest(ctx, handler, "GET", "/codex/conversations/"+seed.intent.Slug+"/thread", seed.scope, nil), &result)
		return result
	}
	before, err := manager.ObserveWorkflow(ctx, handle)
	if err != nil || !before.Idle {
		t.Fatal(before, err)
	}
	root, err := piruntime.SessionStateDirectory(seed.config.UserStateRoot, handle.Conversation.Session)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(root, "provider-policy.json")
	member := filepath.Join(root, "members", handle.Conversation.Member, handle.Conversation.Incarnation)
	evidence := filepath.Join(userstate.WorkspaceDirectory(seed.config.UserStateRoot, "portal", seed.config.Workspace), "creations", seed.intent.Slug+"."+seed.intent.ReceiptID+".complete.json")
	files := []string{filepath.Join(member, "submissions", seed.original.OperationID+".json"), filepath.Join(seed.config.Workspace, "worktrees", ".locks", seed.intent.Slug+".creation.json"), evidence, evidence + ".request"}
	// The original receipt and exact native history are the persistent boundary;
	// use the owning snapshot rather than guessing a history filename.
	var snapshot struct{ HistoryFile string }
	data := seed.fixture.LegacySnapshot(handle, nil)
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	historyBefore, err := os.ReadFile(snapshot.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	fingerprints := map[string]string{}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fingerprints[path] = piruntime.MessageDigest(string(b))
	}
	models := centralRequest(ctx, handler, "GET", "/codex/conversations/"+seed.intent.Slug+"/models", seed.scope, nil)
	if models.Code != 200 || !strings.Contains(models.Body.String(), `"low"`) {
		t.Fatal(models.Code, models.Body.String())
	}
	if _, err := os.Stat(policyPath); !os.IsNotExist(err) {
		t.Fatal("GET initialized desired policy", err)
	}
	if got := read(); got.Model != before.Settings.Model || got.ReasoningEffort != before.Settings.Thinking {
		t.Fatal(got)
	}
	for _, body := range []map[string]any{{"model": "foreign", "reasoningEffort": "low"}, {"model": "p3-model", "reasoningEffort": "max"}, {"model": "p3-model", "reasoningEffort": "low", "collaborationMode": "plan"}} {
		if response := centralRequest(ctx, handler, "POST", "/codex/conversations/"+seed.intent.Slug+"/settings", seed.scope, body); response.Code == 200 {
			t.Fatal("invalid settings accepted", body)
		}
	}
	var saved backend.Settings
	centralDecode(t, centralRequest(ctx, handler, "POST", "/codex/conversations/"+seed.intent.Slug+"/settings", seed.scope, map[string]string{"model": "p3-model", "reasoningEffort": "low"}), &saved)
	if saved.Model != "p3-model" || saved.ReasoningEffort != "low" {
		t.Fatal("Configure did not change supported pair", saved)
	}
	after, err := manager.ObserveWorkflow(ctx, handle)
	if err != nil || after.Settings.Revision != before.Settings.Revision+1 || after.Settings.PolicyRevision == before.Settings.PolicyRevision || after.Settings.Provider != before.Settings.Provider || after.Settings.AccountRef != before.Settings.AccountRef || after.Settings.Thinking != "low" {
		t.Fatal(after, err)
	}
	for i := 0; i < 2; i++ {
		if got := read(); got.Model != saved.Model || got.ReasoningEffort != saved.ReasoningEffort {
			t.Fatal("read/reload lost actual pair", got)
		}
	}
	store := piproviders.Store{Root: root, Session: handle.Conversation.Session, Catalog: seed.config.PiCreation.Catalog}
	policy, err := store.Load()
	if err != nil || policy.Revision != 2 {
		t.Fatal(policy, err)
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil || piruntime.MessageDigest(string(b)) != fingerprints[path] {
			t.Fatal("settings changed original history/receipt", path, err)
		}
	}
	historyAfter, err := os.ReadFile(snapshot.HistoryFile)
	if err != nil || !bytes.HasPrefix(historyAfter, historyBefore) {
		t.Fatal("Configure rewrote original history", err)
	}
	for _, line := range bytes.Split(historyAfter[len(historyBefore):], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string
			Message struct{ Role string }
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type == "message" && entry.Message.Role == "user" {
			t.Fatal("settings added native user input")
		}
	}
	entries, err := os.ReadDir(filepath.Join(member, "submissions"))
	if err != nil || len(entries) != 1 {
		t.Fatal("settings created receipt", entries, err)
	}
	retained, err := manager.Lookup(ctx, seed.original)
	if err != nil || retained.Phase != "Accepted" {
		t.Fatal(retained, err)
	}
	if response := centralRequest(ctx, handler, "POST", "/codex/conversations/"+seed.intent.Slug+"/settings", "", map[string]string{"model": "p3-model", "reasoningEffort": "high"}); response.Code == 200 {
		t.Fatal("missing scope admitted settings")
	}
	final, err := manager.ObserveWorkflow(ctx, handle)
	if err != nil || final.Settings != after.Settings {
		t.Fatal("reads/refusals configured again", final, err)
	}
}

func TestPiHTTPInputAfterReadyUpgradeUsesExistingNativeReceiptPath(t *testing.T) {
	if os.Getenv("PI_USABILITY_PACKAGE") == "" || os.Getenv("PI_CENTRAL_APP_ROOT") == "" {
		t.Skip("explicit retained original/candidate package metadata required")
	}
	seed := centralNativeSeedWithUpgrade(t, false, false, true)
	seed.fixture.Idle(seed.handle)
	upgrade := centralRequest(seed.fixture.Context, seed.server.Handler(), "POST", "/api/sessions/"+seed.intent.Slug+"/pi-runtime-upgrade", "",
		map[string]any{"workspace": seed.config.Workspace, "requestId": "77777777-7777-4777-8777-777777777777", "expectedGeneration": seed.handle.Generation})
	var result piruntime.UpgradeResult
	if upgrade.Code != 200 || json.Unmarshal(upgrade.Body.Bytes(), &result) != nil || result.Phase != "ready" {
		t.Fatal("ordinary ready HTTP upgrade", upgrade.Code, upgrade.Body.String())
	}
	seed.server.Close()
	reopened, err := web.NewContext(seed.fixture.Context, seed.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err := reopened.BindPiWorkflowNativeFixture(seed.fixture.Context, agentteams.Installed{Managed: true, Catalog: &seed.catalog}, result.Runtime, false); err != nil {
		t.Fatal(err)
	}
	seed.server = reopened
	response := centralRequest(seed.fixture.Context, seed.server.Handler(), "POST", "/api/sessions/"+seed.intent.Slug+"/message", seed.scope,
		map[string]any{"message": "one ordinary followup", "clientUserMessageId": "99999999-9999-4999-8999-999999999999"})
	if response.Code != http.StatusAccepted {
		t.Fatal("ordinary ready HTTP input", response.Code, response.Body.String())
	}
	seed.fixture.Idle(agentbackend.ExecutionHandle{Conversation: seed.handle.Conversation, Generation: result.Runtime.Generation})
}

// One scenario crosses the production parser/cold admission, reconstructed
// Manager, real Ruby Controller/Entry and ordinary proof-bound HTTP completion.
func TestPiResumeSuccessorColdCLIRealHostThenHTTP(t *testing.T) {
	if os.Getenv("PI_USABILITY_PACKAGE") == "" || os.Getenv("PI_CENTRAL_APP_ROOT") == "" {
		t.Skip("explicit immutable predecessor/selected package metadata required")
	}
	seed := centralNativeSeedWithUpgrade(t, false, false, true)
	f, ctx := seed.fixture, seed.fixture.Context
	f.Idle(seed.handle)
	dispatches := f.ObserveUpgradeDispatches()
	restoreHost := f.UseRubySuccessorHost()
	original, err := f.Manager.Lookup(ctx, seed.original)
	if err != nil || original.Phase != "Accepted" {
		t.Fatal(original, err)
	}
	var snapshot struct{ HistoryFile, HistoryDigest, Model, Provider, Thinking string }
	if err := json.Unmarshal(f.LegacySnapshot(seed.handle, nil), &snapshot); err != nil {
		t.Fatal(err)
	}
	history, err := os.ReadFile(snapshot.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(seed.config.Workspace)
	hostRoot := filepath.Join(root, "host")
	instance := filepath.Join(hostRoot, "instances", f.Runtime.InstanceID)
	legacy, err := os.Readlink(filepath.Join(instance, "package-root"))
	if err != nil {
		t.Fatal(err)
	}
	if legacy == seed.config.PackageRoot {
		t.Fatal("fixture must use different predecessor and successor packages")
	}
	requestID := "77777777-7777-4777-8777-777777777777"
	body := map[string]any{"workspace": seed.config.Workspace, "requestId": requestID, "expectedGeneration": seed.handle.Generation}
	route := "/api/sessions/" + seed.intent.Slug + "/pi-runtime-upgrade"
	failed := centralRequest(ctx, seed.server.Handler(), "POST", route, "", body)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatal("missed host intent must remain unavailable", failed.Code, failed.Body.String())
	}
	reserved, err := piruntime.InspectRetainedCurrentRuntime(ctx, seed.config.UserStateRoot, f.Runtime.Session)
	if err != nil || reserved.Phase != "starting" || reserved.Runtime.RequestID != requestID {
		t.Fatal("missing retained starting successor", reserved, err)
	}
	if starts, stops := f.UpgradeEffects(); starts != 0 || stops != 1 {
		t.Fatal("missed start effects", starts, stops)
	}
	// Reconstruct the candidate controller against the same saved selected target.
	reopened, err := f.Reopen()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.ReadStart(ctx, f.Runtime.Session)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AppStorePath != seed.config.PackageRoot || saved.ProfilePath != seed.config.HostProfile {
		t.Fatal("saved target changed", saved)
	}
	runtimeDirectory := filepath.Join(seed.config.UserStateRoot, "pi-runtime", fmt.Sprintf("%x", sha256.Sum256([]byte(seed.config.Workspace))), seed.intent.Slug, seed.intent.SessionIncarnation)
	memberPath := filepath.Join(runtimeDirectory, "members", seed.handle.Conversation.Member, seed.handle.Conversation.Incarnation, "conversation.json")
	authorityBefore, err := session.LoadPiRuntimeAuthority(seed.config.AuthorityDir, seed.intent.Slug, seed.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	frozen := map[string][]byte{}
	for _, path := range []string{filepath.Join(runtimeDirectory, "runtime.json"), filepath.Join(instance, "launch.json"), filepath.Join(instance, "requests", requestID, "launch.json"), filepath.Join(instance, "requests", requestID, "runtime-config.json"), memberPath, filepath.Join(runtimeDirectory, "members", seed.handle.Conversation.Member, seed.handle.Conversation.Incarnation, "submissions", seed.original.OperationID+".json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		frozen[path] = data
	}
	restoreHost()
	config := seed.config
	args := []string{"--workspace", config.Workspace, "--workspace-name", config.WorkspaceName, "--user-state-root", config.UserStateRoot, "--authority-dir", config.AuthorityDir, "--host-profile", config.HostProfile, "--transition-lock", config.TransitionLock, "--registration-marker", config.RegistrationMarker, "--codex-socket", config.CodexSocket, "--pi-auth-dir", filepath.Join(root, "auth"), "--pi-host-state-root", hostRoot, "--slug", seed.intent.Slug, "--request", requestID, "--expected-generation", seed.handle.Generation}
	// Bad parser inputs and predecessor identity cannot cross the launch boundary.
	for _, invalid := range [][]string{args[:len(args)-1], append(append([]string{}, args[:len(args)-1]...), strings.Repeat("f", 64))} {
		if err := web.RunPiRuntimeResumeSuccessor(ctx, invalid, &bytes.Buffer{}, web.Config{PiCreation: &web.PiCreationServices{Runtime: reopened}}); err == nil {
			t.Fatal("invalid cold command admitted")
		}
	}
	var output bytes.Buffer
	if err := web.RunPiRuntimeResumeSuccessor(ctx, args, &output, web.Config{PiCreation: &web.PiCreationServices{Runtime: reopened}}); err != nil {
		t.Fatal("actual parser→cold→Manager→Ruby", err)
	}
	var result struct {
		ControllerExecutable, SelectedPackage string
		Status                                piruntime.RuntimeStatus
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.SelectedPackage != saved.AppStorePath || result.Status.Runtime != reserved.Runtime || result.Status.Phase != "ready" || strings.HasPrefix(result.ControllerExecutable, saved.AppStorePath+"/") {
		t.Fatal("candidate/selected identity", result, err)
	}
	entry, err := os.ReadFile(filepath.Join(instance, "fixture-entry.json"))
	if err != nil {
		t.Fatal("real Entry not reached", err)
	}
	var argv []string
	if json.Unmarshal(entry, &argv) != nil || len(argv) < 3 || argv[0] != filepath.Join(saved.AppStorePath, "bin/workspace-portal") || argv[1] != "pi-runtime" || argv[2] != "supervise" {
		t.Fatal("Entry selected wrong package", string(entry))
	}
	for path, before := range frozen {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("cold start changed frozen runtime/member/input", path, err)
		}
	}
	authorityAfter, err := session.LoadPiRuntimeAuthority(config.AuthorityDir, seed.intent.Slug, config.Workspace)
	if err != nil || !reflect.DeepEqual(authorityBefore, authorityAfter) {
		t.Fatal("cold start published authority", err)
	}
	if starts, stops := f.UpgradeEffects(); starts != 1 || stops != 1 {
		t.Fatal("cold start effects", starts, stops)
	}
	// A present intent is observation-only, even after losing the command reply.
	if err := web.RunPiRuntimeResumeSuccessor(ctx, args, &bytes.Buffer{}, web.Config{PiCreation: &web.PiCreationServices{Runtime: reopened}}); err != nil {
		t.Fatal("present-intent observation", err)
	}
	completed := centralRequest(ctx, seed.server.Handler(), "POST", route, "", body)
	var upgraded piruntime.UpgradeResult
	if completed.Code != http.StatusOK || json.Unmarshal(completed.Body.Bytes(), &upgraded) != nil || upgraded.Runtime != reserved.Runtime || upgraded.Phase != "ready" {
		t.Fatal("ordinary HTTP completion", completed.Code, completed.Body.String())
	}
	replay := centralRequest(ctx, seed.server.Handler(), "POST", route, "", body)
	if replay.Code != http.StatusOK || replay.Body.String() != completed.Body.String() {
		t.Fatal("completed replay changed", replay.Code)
	}
	if starts, stops := f.UpgradeEffects(); starts != 1 || stops != 1 {
		t.Fatal("replay repeated host effects", starts, stops)
	}
	if creates, sends, configures := dispatches(); creates != 0 || sends != 0 || configures != 0 {
		t.Fatal("recovery replayed native input/settings", creates, sends, configures)
	}
	after, err := f.Manager.Lookup(ctx, seed.original)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("original receipt changed", err)
	}
	current := agentbackend.ExecutionHandle{Conversation: seed.handle.Conversation, Generation: upgraded.Runtime.Generation}
	var afterSnapshot struct{ HistoryFile, HistoryDigest, Model, Provider, Thinking string }
	if err := json.Unmarshal(f.LegacySnapshot(current, nil), &afterSnapshot); err != nil || afterSnapshot != snapshot {
		t.Fatal("native identity/settings changed", afterSnapshot, err)
	}
	afterHistory, err := os.ReadFile(snapshot.HistoryFile)
	if err != nil || !bytes.Equal(history, afterHistory) {
		t.Fatal("history changed", err)
	}
	if target, err := os.Readlink(filepath.Join(instance, "package-root")); err != nil || target != legacy {
		t.Fatal("legacy root changed", target, err)
	}
	transcript := centralRequest(ctx, seed.server.Handler(), "GET", "/api/sessions/"+seed.intent.Slug+"/thread", seed.scope, nil)
	var projected backend.Transcript
	if transcript.Code != 200 || json.Unmarshal(transcript.Body.Bytes(), &projected) != nil {
		t.Fatal("ordinary retained transcript", transcript.Code)
	}
	users := 0
	for _, entry := range projected.Entries {
		if entry.Kind == "userMessage" {
			users++
			if entry.ItemID != original.Witness.EntryID || entry.ClientUserMessageID != seed.original.OperationID || entry.ClientUserMessageDigest != original.Witness.EffectiveDigest {
				t.Fatal("retained message identity changed")
			}
		}
	}
	if users != 1 {
		t.Fatal("initial input replay", users)
	}
}

func TestPiReadyUpgradeCLIHTTPKeepsOriginalReceiptAndGenericFileContinuation(t *testing.T) {
	if os.Getenv("PI_USABILITY_PACKAGE") == "" || os.Getenv("PI_CENTRAL_APP_ROOT") == "" {
		t.Skip("explicit retained original/candidate package metadata required")
	}
	// Compile the existing ordinary CLI before starting the bounded native fixture.
	binary := filepath.Join(t.TempDir(), "workspace-portal")
	build := exec.Command("go", "build", "-mod=readonly", "-o", binary, "./cmd/workspace-portal")
	build.Dir = filepath.Clean(filepath.Join("..", ".."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("ordinary CLI build: %v\n%s", err, output)
	}
	seed := centralNativeSeedWithUpgrade(t, false, false, true)
	f, ctx := seed.fixture, seed.fixture.Context
	f.Idle(seed.handle)
	original, err := f.Manager.Lookup(ctx, seed.original)
	if err != nil || original.Phase != "Accepted" {
		t.Fatal(original, err)
	}
	dispatches := f.ObserveUpgradeDispatches()
	preparationPath, err := seed.server.RetainPiPreparationV2NativeFixture(seed.intent, seed.goal)
	if err != nil {
		t.Fatal("retained old preparation2", err)
	}
	preparationBefore, err := os.ReadFile(preparationPath)
	if err != nil {
		t.Fatal(err)
	}
	preparations, err := web.InspectRetainedPiPreparations(ctx, seed.config.UserStateRoot, []string{seed.config.Workspace})
	if err != nil || len(preparations) != 1 || preparations[0].Schema != 2 {
		t.Fatal("old preparation2/no preparation3 prerequisite", preparations, err)
	}
	var legacyBefore struct {
		HistoryFile   string `json:"historyFile"`
		HistoryDigest string `json:"historyDigest"`
		Model         string `json:"model"`
		Provider      string `json:"provider"`
		Thinking      string `json:"thinking"`
	}
	if err := json.Unmarshal(f.LegacySnapshot(seed.handle, nil), &legacyBefore); err != nil {
		t.Fatal(err)
	}
	historyBefore, err := os.ReadFile(legacyBefore.HistoryFile)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(seed.config.UserStateRoot, "pi-runtime", fmt.Sprintf("%x", sha256.Sum256([]byte(seed.config.Workspace))), seed.intent.Slug, seed.intent.SessionIncarnation,
		"members", seed.handle.Conversation.Member, seed.handle.Conversation.Incarnation, "submissions", seed.original.OperationID+".json")
	receiptBefore, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	requestID := "77777777-7777-4777-8777-777777777777"
	creationRoot := userstate.WorkspaceDirectory(seed.config.UserStateRoot, "portal", seed.config.Workspace)
	completion := filepath.Join(creationRoot, "creations", seed.intent.Slug+"."+seed.intent.ReceiptID+".complete.json")
	journalPath := filepath.Join(seed.config.Workspace, "worktrees", ".locks", seed.intent.Slug+".creation.json")
	frozenFiles := map[string][]byte{}
	for _, path := range []string{completion, completion + ".request", journalPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		frozenFiles[path] = data
	}
	// The actual HTTP gate must refuse bad immutable ready provenance before
	// reserving any successor or touching either host process.
	for _, kind := range []string{"generation", "incomplete", "intent"} {
		var journal map[string]any
		if err := json.Unmarshal(frozenFiles[journalPath], &journal); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "generation":
			journal["native_generation"] = strings.Repeat("f", 64)
		case "incomplete":
			journal["state"] = "creating"
		case "intent":
			journal["initial_input_id"] = "22222222-2222-4222-8222-222222222222"
		}
		centralJSON(t, journalPath, journal)
		response := centralRequest(ctx, seed.server.Handler(), "POST", "/api/sessions/"+seed.intent.Slug+"/pi-runtime-upgrade", "", map[string]any{"workspace": seed.config.Workspace, "requestId": requestID, "expectedGeneration": seed.handle.Generation})
		if response.Code < 400 {
			t.Fatal("bad ready provenance admitted", kind, response.Code)
		}
		centralWrite(t, journalPath, frozenFiles[journalPath])
		current, err := f.Manager.ReadCurrentRuntime(ctx, seed.intent.SessionRef())
		if err != nil || current != f.Runtime {
			t.Fatal("refusal reserved successor", kind, current, err)
		}
		if starts, stops := f.UpgradeEffects(); starts != 0 || stops != 0 {
			t.Fatal("refusal caused host effects", kind, starts, stops)
		}
	}
	// Use the existing directory-FD socket convention so Nix's long TMPDIR
	// plus the test name cannot exceed the AF_UNIX pathname limit.
	socketDirectory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socketDirectory.Close() })
	socket := fmt.Sprintf("/proc/%d/fd/%d/portal.sock", os.Getpid(), socketDirectory.Fd())
	var stopHTTP func()
	serve := func(server *web.Server) {
		t.Helper()
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(socket, 0600); err != nil {
			t.Fatal(err)
		}
		httpServer := &http.Server{Handler: server.Handler()}
		done := make(chan error, 1)
		go func() { done <- httpServer.Serve(listener) }()
		stopHTTP = func() { _ = httpServer.Close(); <-done }
	}
	serve(seed.server)
	t.Cleanup(func() {
		if stopHTTP != nil {
			stopHTTP()
		}
	})
	args := []string{"pi-runtime", "upgrade", "--workspace", seed.config.Workspace, "--slug", seed.intent.Slug,
		"--request", requestID, "--expected-generation", seed.handle.Generation, "--socket", socket, "--origin", "https://portal.example"}
	restoreRetention := f.InterruptUpgrade(requestID)
	interrupted := exec.CommandContext(ctx, binary, args...)
	if output, err := interrupted.CombinedOutput(); err == nil {
		t.Fatal("reserved interruption unexpectedly completed", string(output))
	}
	reserved, err := piruntime.InspectRetainedCurrentRuntime(ctx, seed.config.UserStateRoot, f.Runtime.Session)
	if err != nil || reserved.Phase != "preparing" || reserved.Runtime.RequestID != requestID || reserved.Runtime.Generation == f.Runtime.Generation {
		t.Fatal("interruption did not retain its preparing intent", reserved, err)
	}
	if starts, stops := f.UpgradeEffects(); starts != 0 || stops != 0 {
		t.Fatal("reservation interruption crossed drain/start boundary", starts, stops)
	}
	stopHTTP()
	stopHTTP = nil
	seed.server.Close() // Join the original portal attachment before replacement.
	restoreRetention()
	restarted, err := web.NewContext(ctx, seed.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	if err := restarted.BindPiRetainedCatalogNativeFixture(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.BindPiWorkflowNativeFixture(ctx, agentteams.Installed{Managed: true, Catalog: &seed.catalog}, reserved.Runtime, false); err != nil {
		t.Fatal(err)
	}
	seed.server = restarted
	serve(restarted)
	upgrade := func() piruntime.UpgradeResult {
		t.Helper()
		command := exec.CommandContext(ctx, binary, args...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("ordinary CLI→HTTP upgrade: %v\n%s", err, output)
		}
		var result piruntime.UpgradeResult
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatal(err, string(output))
		}
		return result
	}
	result := upgrade()
	if result.Phase != "ready" || result.Previous != f.Runtime || result.Runtime.Session != f.Runtime.Session || result.Runtime.InstanceID != f.Runtime.InstanceID || result.Runtime.Generation == f.Runtime.Generation || result.AppStorePath != seed.config.PackageRoot {
		t.Fatal("upgrade identity", result)
	}
	if starts, stops := f.UpgradeEffects(); starts != 1 || stops != 1 {
		t.Fatal("upgrade host effects", starts, stops)
	}
	if replay := upgrade(); replay != result {
		t.Fatal("completed replay changed result", replay, result)
	}
	if creates, sends, configures := dispatches(); creates != 0 || sends != 0 || configures != 0 {
		t.Fatal("upgrade dispatched fresh input/settings", creates, sends, configures)
	}
	for path, before := range frozenFiles {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("upgrade rewrote frozen creation proof", path, err)
		}
	}
	if starts, stops := f.UpgradeEffects(); starts != 1 || stops != 1 {
		t.Fatal("replay repeated host effects", starts, stops)
	}
	currentHandle := agentbackend.ExecutionHandle{Conversation: seed.handle.Conversation, Generation: result.Runtime.Generation}
	for _, before := range []*int{nil, new(int)} {
		if before != nil {
			*before = 1
		}
		raw := f.LegacySnapshot(currentHandle, before) // Actual Unix wire + frozen a43 strict decoder.
		if bytes.Contains(raw, []byte(`"acceptedInputs"`)) {
			t.Fatal("legacy response gained receipt fields")
		}
		var legacyAfter struct {
			HistoryFile   string `json:"historyFile"`
			HistoryDigest string `json:"historyDigest"`
			Model         string `json:"model"`
			Provider      string `json:"provider"`
			Thinking      string `json:"thinking"`
		}
		if err := json.Unmarshal(raw, &legacyAfter); err != nil || legacyAfter != legacyBefore {
			t.Fatal("upgrade changed native history/settings identity", legacyAfter, legacyBefore, err)
		}
	}
	historyAfter, err := os.ReadFile(legacyBefore.HistoryFile)
	if err != nil || !bytes.Equal(historyAfter, historyBefore) {
		t.Fatal("upgrade changed native history bytes", err)
	}
	preparationAfter, err := os.ReadFile(preparationPath)
	if err != nil || !bytes.Equal(preparationAfter, preparationBefore) {
		t.Fatal("upgrade changed original preparation2", err)
	}
	receiptAfter, err := os.ReadFile(receiptPath)
	if err != nil || !bytes.Equal(receiptAfter, receiptBefore) {
		t.Fatal("upgrade changed original receipt bytes", err)
	}
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seed.config.Workspace, "repos"), 0755); err != nil {
		t.Fatal(err)
	}
	projectionInput := map[string]string{"workspace": seed.config.Workspace, "state": seed.config.UserStateRoot,
		"profile": seed.config.HostProfile, "package": seed.config.PackageRoot, "authority": seed.config.AuthorityDir,
		"slug": seed.intent.Slug, "originalGeneration": seed.handle.Generation,
		"runtimePath": filepath.Join(seed.config.UserStateRoot, "pi-runtime", fmt.Sprintf("%x", sha256.Sum256([]byte(seed.config.Workspace))), seed.intent.Slug, seed.intent.SessionIncarnation, "runtime.json")}
	projectionPath := filepath.Join(t.TempDir(), "runtime-projection.json")
	centralJSON(t, projectionPath, projectionInput)
	projection := exec.CommandContext(ctx, "ruby", filepath.Join(source, "test/support/pi_team_preflight_driver.rb"))
	projection.Env = append(os.Environ(), "P2_TEAM_SOURCE_ROOT="+source, "P2_TEAM_PORTAL_HELPER="+binary, "P2_PI_RUNTIME_INPUT="+projectionPath)
	if output, err := projection.CombinedOutput(); err != nil {
		t.Fatalf("same upgraded runtime actual scanner/Ruby fence: %v\n%s", err, output)
	}
	if err := seed.ops.Verify.Verify(ctx, piruntime.BackendIdentity(seed.handle)); err == nil {
		t.Fatal("old runtime handle survived reservation")
	}
	read := func(server *web.Server, route string) backend.Transcript {
		t.Helper()
		response := centralRequest(ctx, server.Handler(), "GET", "/api/sessions/"+seed.intent.Slug+"/"+route, seed.scope, nil)
		if response.Code != 200 {
			t.Fatalf("upgraded %s: %d %s", route, response.Code, response.Body.String())
		}
		var transcript backend.Transcript
		if err := json.Unmarshal(response.Body.Bytes(), &transcript); err != nil {
			t.Fatal(err)
		}
		return transcript
	}
	assertOriginal := func(transcript backend.Transcript) {
		t.Helper()
		users := 0
		for _, entry := range transcript.Entries {
			if entry.Kind == "userMessage" {
				users++
				if entry.ItemID != original.Witness.EntryID || entry.ClientUserMessageID != seed.original.OperationID || entry.ClientUserMessageDigest != original.Witness.EffectiveDigest {
					t.Fatal("old Accepted input lost its identity", entry)
				}
			}
		}
		if users != 1 {
			t.Fatal("upgrade created or replayed an input", users)
		}
	}
	assertOriginal(read(seed.server, "thread"))
	assertOriginal(read(seed.server, "thread/page"))
	after, err := f.Manager.Lookup(ctx, seed.original)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("upgrade rewrote the original receipt", after, err)
	}
	seed.server.Close()
	reopened, err := web.NewContext(ctx, seed.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err := reopened.BindPiRetainedCatalogNativeFixture(); err != nil {
		t.Fatal(err)
	}
	if err := reopened.BindPiWorkflowNativeFixture(ctx, agentteams.Installed{Managed: true, Catalog: &seed.catalog}, result.Runtime, false); err != nil {
		t.Fatal(err)
	}
	assertOriginal(read(reopened, "thread"))
	if err := reopened.ProvePiRetainedCreationNativeFixture(ctx, seed.intent); err != nil {
		t.Fatal("changed-catalog retained proof after schema2", err)
	}
	initialStart, initialRuntime, err := f.Manager.ReadInitialStart(ctx, seed.intent.SessionRef(), seed.intent.InitialInputID)
	if err != nil || initialRuntime != f.Runtime || initialStart.AppStorePath == result.AppStorePath {
		t.Fatal("schema2 lost original catalog ancestor", initialStart, initialRuntime, err)
	}
	// Continue through the ordinary generic upload and Send owners only after
	// the completed upgrade, using text and binary attachments in saved order.
	ids := []string{}
	for i, data := range [][]byte{[]byte("ordinary source input\n"), {0, 255, 1, 42}} {
		name := []string{"input.txt", "archive.bin"}[i]
		response := centralRequest(ctx, reopened.Handler(), "POST", "/uploads/s-"+seed.intent.Slug, seed.scope,
			conversation.UploadRequest{ClientID: fmt.Sprintf("88888888-8888-4888-8888-%012d", i+1), Name: name, Size: int64(len(data))})
		var file conversation.Upload
		if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &file) != nil {
			t.Fatal("upload create", response.Code, response.Body.String())
		}
		path := "/uploads/s-" + seed.intent.Slug + "/" + file.ID
		request := httptest.NewRequest("PATCH", "https://portal.example"+path, bytes.NewReader(data)).WithContext(ctx)
		request.Header.Set("Origin", "https://portal.example")
		request.Header.Set("X-Workspace-Conversation-Scope", seed.scope)
		request.Header.Set("Upload-Offset", "0")
		digest := sha256.Sum256(data)
		request.Header.Set("Upload-Checksum", hex.EncodeToString(digest[:]))
		chunk := httptest.NewRecorder()
		reopened.Handler().ServeHTTP(chunk, request)
		if chunk.Code != 200 {
			t.Fatal("upload append", chunk.Code, chunk.Body.String())
		}
		if response := centralRequest(ctx, reopened.Handler(), "POST", path, seed.scope, nil); response.Code != 200 {
			t.Fatal("upload complete", response.Code, response.Body.String())
		}
		ids = append(ids, file.ID)
	}
	followupID := "99999999-9999-4999-8999-999999999999"
	message := map[string]any{"message": "inspect generic inputs", "clientUserMessageId": followupID, "attachmentIds": ids}
	path := "/api/sessions/" + seed.intent.Slug + "/message"
	if response := centralRequest(ctx, reopened.Handler(), "POST", path, seed.scope, message); response.Code != http.StatusAccepted {
		memberRoot := filepath.Join(filepath.Dir(projectionInput["runtimePath"]), "members", seed.handle.Conversation.Member, seed.handle.Conversation.Incarnation)
		data, receiptErr := os.ReadFile(filepath.Join(memberRoot, "submissions", followupID+".json"))
		var receipt piruntime.SubmissionStatus
		decodeErr := json.Unmarshal(data, &receipt)
		t.Logf("followup diagnostic receiptRead=%v decode=%v phase=%s operation=%s generation=%s witness=%v", receiptErr, decodeErr, receipt.Phase, receipt.Ref.OperationID, receipt.Generation, receipt.Witness != nil)
		currentHandle := agentbackend.ExecutionHandle{Conversation: seed.handle.Conversation, Generation: result.Runtime.Generation}
		if currentOps, bindErr := f.Manager.Bind(ctx, currentHandle); bindErr == nil {
			if snapshot, readErr := currentOps.Read.Read(ctx, piruntime.BackendIdentity(currentHandle)); readErr != nil {
				t.Log("followup diagnostic native read", readErr)
			} else {
				users := 0
				for _, entry := range snapshot.Entries {
					if entry.Kind == "userMessage" {
						users++
					}
				}
				t.Logf("followup diagnostic native status=%s model=%s thinking=%s users=%d", snapshot.Status, snapshot.Model, snapshot.ReasoningEffort, users)
			}
		} else {
			t.Log("followup diagnostic bind", bindErr)
		}
		t.Fatal("new generic-file input", response.Code, response.Body.String())
	}
	f.Idle(agentbackend.ExecutionHandle{Conversation: seed.handle.Conversation, Generation: result.Runtime.Generation})
	for _, route := range []string{"thread", "thread/page", "thread"} {
		transcript := read(reopened, route)
		users, found := 0, false
		for _, entry := range transcript.Entries {
			if entry.Kind != "userMessage" {
				continue
			}
			users++
			if entry.ClientUserMessageID == followupID {
				found = entry.DisplayText != nil && *entry.DisplayText == "inspect generic inputs" && len(entry.Attachments) == 2 && entry.Attachments[0].ID == ids[0] && entry.Attachments[1].ID == ids[1] && entry.ClientUserMessageDigest == piruntime.MessageDigest(entry.Text)
			}
		}
		if users != 2 || !found {
			t.Fatal("distinct post-upgrade attachment receipt", transcript.Entries)
		}
	}
	t.Log("ordinary CLI→HTTP/Manager upgrade; real host attachment ACK/Load; one original Accepted user survives full/page/reload; one later text+binary attachment input; no upgrade Create/Send/Configure")
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
		if entry.Kind == "userMessage" {
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
		if entry.Kind == "userMessage" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Fatalf("restart created or replayed user input: %d", inputs)
	}
	t.Log("New Manager over exact retained fixture config/disk plus new Server: no Ensure, Load or fixture preattachment; scoped ordinary GET demand drove admitted background current-read/AttachHost/ACK/full-ref refresh, coherent workflows and unchanged original receipt/one user input.")
}

// The real protocol-2 worker retains its starting intent and initialized creating
// member. Each invocation is a fresh caller with no fixture preattachment; Close
// cancels and joins it before the next capture/retirement/replay child.
func TestPiLifecycleColdCallbackCallerNative(t *testing.T) {
	catalogPath := os.Getenv("PI_CENTRAL_CATALOG_FIXTURE")
	if catalogPath == "" {
		t.Fatal("focused batch must export the existing typed catalog fixture")
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var catalog agentteams.Catalog
	if json.Unmarshal(data, &catalog) != nil || catalog.Validate() != nil {
		t.Fatal("invalid fixture catalog")
	}
	root := t.TempDir()
	packageRoot, profile := filepath.Join(root, "package"), filepath.Join(root, "profile")
	centralFixturePackage(t, packageRoot)
	if err := os.Symlink(packageRoot, profile); err != nil {
		t.Fatal(err)
	}
	command, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	piruntime.RunPiLifecycleTerminalCallbackNativeFixture(t, func(ctx context.Context, manager *piruntime.Manager, runtime piruntime.RuntimeRef, members []piruntime.LifecycleMember, saved []piruntime.LifecycleCallback, retire bool) ([]piruntime.LifecycleCallback, piruntime.RuntimeStatus, error) {
		calls++
		config := web.Config{Workspace: runtime.Session.Workspace, BaseURL: "https://portal.example", DevSession: command, HostProfile: profile, AuthorityDir: filepath.Join(root, "authority"), UserStateRoot: filepath.Join(root, "state"), PackageRoot: packageRoot, WorkspaceName: "central", CodexSocket: filepath.Join(root, "host", "app-server.sock"), RegistrationMarker: filepath.Join(root, "host", "registration.json"), Logger: log.New(&bytes.Buffer{}, "", 0), PiCreation: &web.PiCreationServices{Catalog: centralProviderCatalog(), Runtime: manager}}
		server, err := web.NewContext(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		if err := server.BindPiWorkflowNativeFixture(ctx, agentteams.Installed{Managed: true, Catalog: &catalog}, runtime, false); err != nil {
			t.Fatal(err)
		}
		binding := session.BackendBinding{Schema: 1, Backend: agentbackend.Pi, BackendContract: 1, Workspace: runtime.Session.Workspace, Slug: runtime.Session.Slug, SessionIncarnation: runtime.Session.Incarnation, CreationReceiptID: members[0].Slot.CreationID, Revision: 1, State: "ready"}
		for _, member := range members {
			binding.Conversations = append(binding.Conversations, session.BackendConversation{Address: member.Slot.Member, Incarnation: member.Slot.Incarnation, NativeID: member.Slot.NativeID, State: member.Slot.State})
		}
		capture := session.PiLifecycleCapture{Kind: "pi", Contract: 1, Binding: binding, Authority: session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: binding.Workspace, Slug: binding.Slug, SessionIncarnation: binding.SessionIncarnation, BindingRevision: binding.Revision, Generation: runtime.Generation, State: "ready"}, Runtime: runtime, Members: members, Callbacks: saved}
		for _, name := range []string{"binding", "creation", "companion", "completion", "authority"} {
			capture.Selectors = append(capture.Selectors, session.PiLifecycleSelector{Name: name, Path: filepath.Join(root, name), SHA256: strings.Repeat("a", 64)})
		}
		if err := capture.Validate(binding.Workspace, binding.Slug); err != nil {
			t.Fatal(err)
		}
		result, err := server.PiLifecycleCallbacksNativeFixture(ctx, capture, retire)
		status := piruntime.RuntimeStatus{Runtime: runtime}
		if result.Terminal != nil {
			status.Phase, status.Observation = "stopped", *result.Terminal
		}
		if calls == 2 && !errors.Is(err, piruntime.ErrHeld) {
			t.Fatal("competing owner refusal lost hold identity", err)
		}
		return result.Callbacks, status, err
	})
	if calls != 5 {
		t.Fatal("cold capture/fresh retirement/terminal retry shape", calls)
	}
}
