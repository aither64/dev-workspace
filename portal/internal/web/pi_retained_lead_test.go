package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/aither64/codex-web/conversation/backend"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// Materialize the existing typed team fixture for the strict installed reader.
// These ASCII-only inputs use the catalog's canonical sorted JSON digest rules.
func piRetainedLeadPackage(t *testing.T, access, description string) (string, *agentteams.Installed) {
	t.Helper()
	catalog := piCreationTeamCatalogFixture(t, piCreationSoloPreset()).Catalog
	team := catalog.Teams["solo"]
	team.Description = description
	role := team.Roles["team_lead"]
	role.Access = access
	role.Model = "catalog-default"
	catalog.ModelInventory = append(catalog.ModelInventory, role.Model)
	slices.Sort(catalog.ModelInventory)
	team.Roles["team_lead"] = role
	catalog.Teams["solo"] = team
	data, _ := json.Marshal(catalog)
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	digest := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var canonical any
		if err := json.Unmarshal(data, &canonical); err != nil {
			t.Fatal(err)
		}
		data, err = json.Marshal(canonical)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	rawTeam := object["teams"].(map[string]any)["solo"].(map[string]any)
	delete(rawTeam, "team_digest")
	team.TeamDigest = digest(map[string]any{"schema_version": object["schema_version"], "capacity": object["capacity"], "work_policy": object["work_policy"], "team_name": "solo", "team": rawTeam, "execution_access": object["execution_access"]})
	catalog.Teams["solo"] = team
	delete(object, "catalog_digest")
	delete(object, "model_inventory")
	delete(object, "native_agent_configs")
	catalog.CatalogDigest = digest(object)
	for i := range catalog.NativeAgentConfigs.Roles {
		v := &catalog.NativeAgentConfigs.Roles[i]
		v.Name = "dw_" + digest(map[string]any{"catalog_digest": catalog.CatalogDigest, "kind": v.Kind, "identity": v.Identity, "team": v.Team, "effort": v.Effort, "role": v.Role})[:48]
	}
	for i := range catalog.NativeAgentConfigs.Utilities {
		v := &catalog.NativeAgentConfigs.Utilities[i]
		v.Name = "dw_" + digest(map[string]any{"catalog_digest": catalog.CatalogDigest, "kind": v.Kind, "identity": v.Identity, "utility": v.Utility, "effort": v.Effort})[:48]
	}
	metadata := agentteams.PackageMetadata{Schema: agentteams.PackageMetadataSchema, ActivationEnvironmentAliases: []string{"DEV_WORKSPACES"}, RouterSocket: "/run/dev-workspaces/router.sock", UserNamespace: "dev-workspaces",
		AgentTeams: agentteams.AgentTeamsMetadata{Managed: true, MetadataSchema: 1, Catalog: agentteams.CatalogReference{Path: "share/dev-workspace/agent-teams.json", Digest: catalog.CatalogDigest, SchemaVersion: catalog.SchemaVersion}, Capacity: catalog.Capacity, NativeCapacity: agentteams.NativeCapacity{ConfigKey: "agents.max_concurrent_threads_per_session", RequiredValue: catalog.Capacity.RequiredNativeChildThreads}, Generator: catalog.NativeAgentConfigs.Roles[0].Identity.Generator, NativeAdapter: catalog.NativeAgentConfigs.Adapter, NativeRoleConfigs: catalog.NativeAgentConfigs.Roles, NativeUtilityConfigs: catalog.NativeAgentConfigs.Utilities}}
	root := t.TempDir()
	write := func(path string, value any) {
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("share/dev-workspace/package.json", metadata)
	write("share/dev-workspace/agent-teams.json", catalog)
	for _, v := range catalog.NativeAgentConfigs.Roles {
		write(v.Path, "fixture role")
	}
	for _, v := range catalog.NativeAgentConfigs.Utilities {
		write(v.Path, "fixture utility")
	}
	installed, err := agentteams.LoadInstalled(root)
	if err != nil || !installed.Managed || installed.Catalog == nil {
		t.Fatal("strict team fixture", err)
	}
	return root, &installed
}

type piRetainedLeadReadFixture struct {
	*piActivityReadFixture
	initial                *piCatalogInitialFixture
	bindError, verifyError error
}

func (f *piRetainedLeadReadFixture) ReadInitialStart(ctx context.Context, ref agentbackend.SessionRef, request string) (piruntime.StartRequest, piruntime.RuntimeRef, error) {
	return f.initial.ReadInitialStart(ctx, ref, request)
}

func newPiRetainedLeadHTTPFixture(t *testing.T, access string) (*Server, *piRetainedLeadReadFixture) {
	t.Helper()
	aRoot, original := piRetainedLeadPackage(t, access, "Original team")
	currentAccess := "workspace_write"
	if access == "workspace_write" {
		currentAccess = "read_only"
	}
	bRoot, current := piRetainedLeadPackage(t, currentAccess, "New reader team")
	preset, err := teamruntime.FindCatalogPreset(*original.Catalog, "solo")
	if err != nil {
		t.Fatal(err)
	}
	// Explicit retained overrides differ from the original role defaults.
	preset.LeadModel, preset.LeadInstructions = "model", "Saved explicit lead instructions."
	s, _, native := newHistoricalPiCreationFixtureForSlug(t, "2026-11-25-retained-lead", preset)
	s.cancelOperations()
	s.displayRefreshWG.Wait()
	s.operationWG.Wait()
	// Keep original creation provenance while current authority/execution advances.
	initial := &piCatalogInitialFixture{initial: piruntime.StartRequest{Session: native.runtime.Session, RequestID: native.runtime.RequestID, BindingRevision: 1, AppStorePath: aRoot}, original: native.runtime}
	native.runtime.RequestID = "22222222-2222-4222-8222-222222222222"
	native.runtime.Generation = strings.Repeat("6", 64)
	native.state.Generation = native.runtime.Generation
	binding, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(native.runtime.Session.Slug)
	if err != nil {
		t.Fatal(err)
	}
	piTeamAuthorityFixture(t, s, session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: s.config.Workspace, Slug: binding.Slug, SessionIncarnation: binding.SessionIncarnation, BindingRevision: binding.Revision, Generation: native.runtime.Generation, State: "ready"})
	contract, err := os.ReadFile(filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bRoot, "share/workspace-portal"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bRoot, "share/workspace-portal/runtime-contract.json"), contract, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bRoot, s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	s.hostProfile, err = readHostProfileIdentity(s.config.HostProfile)
	if err != nil {
		t.Fatal(err)
	}
	s.config.PackageRoot, s.installedTeams = bRoot, current
	f := &piRetainedLeadReadFixture{piActivityReadFixture: &piActivityReadFixture{piAcceptanceReadFixture: &piAcceptanceReadFixture{piCreationRuntimeFixture: native}}, initial: initial}
	s.config.PiCreation.Runtime = f
	s.piWorkflows = newPiWorkflowComposition(s)
	dispatcher, err := s.piConversationHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected Codex fallback") }))
	if err != nil {
		t.Fatal(err)
	}
	s.conversation = dispatcher
	// The activity/admission fixture has no attachment scope to materialize.
	s.uploadStore = nil
	// Equal provider metadata deliberately cannot prove historical team access.
	if s.config.PiCreation.Catalog.Digest != piCreationCatalogFixture().Digest || original.Catalog.CatalogDigest == current.Catalog.CatalogDigest {
		t.Fatal("fixture identities")
	}
	return s, f
}

func TestPiRetainedLeadActivityAcrossReaderCatalog(t *testing.T) {
	s, f := newPiRetainedLeadHTTPFixture(t, "read_only")
	journal, err := os.ReadFile(piJournalPath(s.config.Workspace, f.state.Conversation.Session.Slug))
	if err != nil {
		t.Fatal(err)
	}
	// This exact ready admission proves the fixture reaches lead access, before
	// BindRead. The old reader then returns its catalog refusal at HTTP 404.
	if _, release, err := s.acquirePiSessionAdmission(context.Background(), f.runtime.Session, false); err != nil {
		t.Fatal("ready fixture", err)
	} else {
		release()
	}
	before := privateWebInventory(t, s.config.UserStateRoot)
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+f.runtime.Session.Slug+"/activity", nil)
	request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(f.state.Conversation))
	response := httptest.NewRecorder()
	s.sessionAPI(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("retained lead activity HTTP %d %s; BindRead=%d Verify=%d Activity=%d", response.Code, response.Body.String(), f.readBindings, f.verifies, f.activities)
	}
	if f.readBindings != 1 || f.binds != 1 || f.verifies != 1 || f.activities != 1 || f.reads != 0 || f.initial.reads == 0 {
		t.Fatal("read boundary counts", f.readBindings, f.binds, f.verifies, f.activities, f.initial.reads)
	}
	afterJournal, err := os.ReadFile(piJournalPath(s.config.Workspace, f.runtime.Session.Slug))
	if err != nil || string(journal) != string(afterJournal) || !reflect.DeepEqual(before, privateWebInventory(t, s.config.UserStateRoot)) || f.creates != 0 || f.sends != 0 {
		t.Fatalf("read changed original/current state: journal=%t inventory=%t creates=%d sends=%d err=%v", string(journal) == string(afterJournal), reflect.DeepEqual(before, privateWebInventory(t, s.config.UserStateRoot)), f.creates, f.sends, err)
	}
}

func (f *piRetainedLeadReadFixture) BindRead(ctx context.Context, handle agentbackend.ExecutionHandle) (backend.Operations, bool, error) {
	ops, supported, err := f.piActivityReadFixture.BindRead(ctx, handle)
	ops.Verify = f
	return ops, supported, errors.Join(err, f.bindError)
}
func (f *piRetainedLeadReadFixture) Verify(ctx context.Context, id backend.Identity) error {
	return errors.Join(f.piAcceptanceReadFixture.Verify(ctx, id), f.verifyError)
}
func (f *piRetainedLeadReadFixture) Bind(ctx context.Context, handle agentbackend.ExecutionHandle) (backend.Operations, error) {
	ops, err := f.piAcceptanceReadFixture.Bind(ctx, handle)
	ops.Read, ops.Activity = f, f
	return ops, err
}
func (f *piRetainedLeadReadFixture) Read(_ context.Context, id backend.Identity) (backend.Transcript, error) {
	f.reads++
	if id != piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: f.state.Conversation, Generation: f.state.Generation}) {
		return backend.Transcript{}, piruntime.ErrScope
	}
	return backend.Transcript{ThreadID: f.state.Conversation.NativeID, Status: "idle", Model: f.state.Settings.Model, ReasoningEffort: f.state.Settings.Thinking}, nil
}
func (f *piRetainedLeadReadFixture) ReadMemberAdmission(_ context.Context, ref agentbackend.ConversationRef, creation, input string) (piruntime.ConversationState, piruntime.SubmissionStatus, error) {
	if ref != f.state.Conversation || creation == "" || input != f.submission.Ref.OperationID {
		return piruntime.ConversationState{}, piruntime.SubmissionStatus{}, piruntime.ErrScope
	}
	state := f.state
	state.Persisted = true
	return state, *f.submission, nil
}

func TestPiRetainedLeadPermissionAndParticipant(t *testing.T) {
	for _, access := range []string{"read_only", "workspace_write"} {
		t.Run(access, func(t *testing.T) {
			s, f := newPiRetainedLeadHTTPFixture(t, access)
			current, release, err := s.acquirePiSessionAdmission(context.Background(), f.runtime.Session, false)
			if err != nil {
				t.Fatal(err)
			}
			release()
			before := privateWebInventory(t, s.config.UserStateRoot)
			intentBefore, _ := json.Marshal(current.intent)
			startBefore, runtimeBefore := f.initial.initial, f.initial.original
			admission, err := s.projectPiConversationAdmission(current, f.state.Conversation)
			if err != nil || admission.SavedAccess != access {
				t.Fatal("saved access", admission.SavedAccess, err)
			}
			called := 0
			err = s.piWorkflows.withMutation("pi-plan.accept")(context.Background(), f.state.Conversation, func(context.Context) error { called++; return nil })
			if (err == nil) != (access == "workspace_write") || called != map[string]int{"read_only": 0, "workspace_write": 1}[access] {
				t.Fatal("historical write permission replaced", called, err)
			}
			policy, err := composePiWorkflowPolicy(access, "default", 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			f.initialPolicy = &policy
			f.instructions = current.intent.DirectTeam.LeadInstructions
			participant, err := (piTeamService{s}).participant(context.Background(), current, teamruntime.PiRoster{}, admission.Handle, "", false, true)
			if err != nil {
				t.Fatal("retained participant", err)
			}
			if participant.Policy.SavedAccess != access || participant.Policy.Model != current.intent.DirectTeam.LeadModel || participant.Policy.Effort != current.intent.DirectTeam.LeadEffort || participant.Policy.Instructions != current.intent.DirectTeam.LeadInstructions || participant.Settings != f.state.Settings || !participant.Loaded {
				t.Fatal("saved participant settings replaced", participant)
			}
			intentAfter, _ := json.Marshal(current.intent)
			if string(intentBefore) != string(intentAfter) || !reflect.DeepEqual(before, privateWebInventory(t, s.config.UserStateRoot)) || f.creates != 0 || f.sends != 0 || f.state.Conversation != admission.Handle.Conversation || f.runtime.Generation != admission.Handle.Generation {
				t.Fatal("participant/admission mutated retained state")
			}
			if f.initial.initial != startBefore || f.initial.original != runtimeBefore {
				t.Fatal("original Start/runtime changed")
			}
			if s.piSourceTeamMatches(current.intent) {
				t.Fatal("new-preset source matcher broadened")
			}
			old, _ := agentteams.LoadInstalled(f.initial.initial.AppStorePath)
			preset, err := teamruntime.FindCatalogPreset(*old.Catalog, current.intent.DirectTeam.ID)
			if err != nil || s.requirePiNewPreset(preset) == nil {
				t.Fatal("historical preset admitted for new creation", err)
			}
		})
	}
}

func TestPiRetainedLeadStrictProvenance(t *testing.T) {
	for _, fault := range []string{"reader", "missing-root", "relative-root", "unclean-root", "symlink-root", "metadata", "unmanaged", "catalog", "role", "missing-access", "unknown-access", "catalog-digest", "team-digest", "team", "start-session", "runtime-session", "start-request", "runtime-request", "generation", "successor", "binding", "journal-state", "journal-intent", "journal-native", "current-metadata", "successor-target"} {
		t.Run(fault, func(t *testing.T) {
			s, f := newPiRetainedLeadHTTPFixture(t, "read_only")
			current, release, err := s.acquirePiSessionAdmission(context.Background(), f.runtime.Session, false)
			if err != nil {
				t.Fatal(err)
			}
			release()
			root := f.initial.initial.AppStorePath
			switch fault {
			case "reader":
				s.config.PiCreation.Runtime = f.piActivityReadFixture
			case "missing-root":
				f.initial.initial.AppStorePath = filepath.Join(t.TempDir(), "missing")
			case "relative-root":
				f.initial.initial.AppStorePath = "."
			case "unclean-root":
				f.initial.initial.AppStorePath += "/."
			case "symlink-root":
				link := filepath.Join(t.TempDir(), "original")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				f.initial.initial.AppStorePath = link
			case "metadata":
				if err := os.WriteFile(filepath.Join(root, "share/dev-workspace/package.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unmanaged":
				if err := os.WriteFile(filepath.Join(root, "share/dev-workspace/package.json"), []byte(`{"schema":1,"activationEnvironmentAliases":["DEV_WORKSPACES"],"routerSocket":"/run/dev-workspaces/router.sock","userNamespace":"dev-workspaces","agent_teams":{"managed":false}}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "catalog":
				if err := os.Remove(filepath.Join(root, "share/dev-workspace/agent-teams.json")); err != nil {
					t.Fatal(err)
				}
			case "role", "missing-access", "unknown-access":
				path := filepath.Join(root, "share/dev-workspace/agent-teams.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var catalog map[string]any
				if err := json.Unmarshal(data, &catalog); err != nil {
					t.Fatal(err)
				}
				roles := catalog["teams"].(map[string]any)["solo"].(map[string]any)["roles"].(map[string]any)
				role := roles["team_lead"].(map[string]any)
				if fault == "role" {
					delete(roles, "team_lead")
				} else if fault == "missing-access" {
					delete(role, "access")
				} else {
					role["access"] = "full_access"
				}
				if err := writePiCreationRecord(path, catalog, 4<<20); err != nil {
					t.Fatal(err)
				}
			case "catalog-digest":
				current.intent.DirectTeam.CatalogDigest = strings.Repeat("f", 64)
			case "team-digest":
				current.intent.DirectTeam.TeamDigest = strings.Repeat("f", 64)
			case "team":
				current.intent.DirectTeam.ID = "foreign"
			case "start-session":
				f.initial.initial.Session.Incarnation = strings.Repeat("f", 64)
			case "runtime-session":
				f.initial.original.Session.Incarnation = strings.Repeat("f", 64)
			case "start-request":
				f.initial.initial.RequestID = f.runtime.RequestID
			case "runtime-request":
				f.initial.original.RequestID = f.runtime.RequestID
			case "generation":
				f.initial.original.Generation = f.runtime.Generation
			case "successor":
				f.initial.initial.ReplacesRequestID = f.runtime.RequestID
			case "binding":
				f.initial.initial.BindingRevision = current.Binding.Revision
			case "journal-state", "journal-intent", "journal-native":
				path := piJournalPath(s.config.Workspace, current.intent.Slug)
				journal, err := readPiCreationJournal(path)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "journal-state" {
					journal.State = "creating"
				} else if fault == "journal-intent" {
					journal.DirectTeam.LeadInstructions += "changed"
				} else {
					journal.Native = nil
				}
				if err := writePiCreationRecord(path, journal, maxCreationReceiptBytes); err != nil {
					t.Fatal(err)
				}
			case "current-metadata":
				s.installedTeams = nil
			case "successor-target":
				f.initial.initial.AppStorePath = s.config.PackageRoot
			}
			before := privateWebInventory(t, s.config.UserStateRoot)
			if role, err := s.piRetainedLeadRole(context.Background(), current.intent); err == nil || role.Access != "" {
				t.Fatal("unproved role admitted", role)
			}
			if !reflect.DeepEqual(before, privateWebInventory(t, s.config.UserStateRoot)) || f.readBindings != 0 || f.sends != 0 || f.creates != 0 {
				t.Fatal("refusal had effects")
			}
		})
	}
}

func TestPiRetainedLeadActivityKeepsLaterRefusals(t *testing.T) {
	for _, fault := range []string{"bind", "verify", "scope", "authority"} {
		t.Run(fault, func(t *testing.T) {
			s, f := newPiRetainedLeadHTTPFixture(t, "read_only")
			scope := piWorkflowNamespace(f.state.Conversation)
			switch fault {
			case "bind":
				f.bindError = piruntime.ErrUnavailable
			case "verify":
				f.verifyError = agentbackend.ErrStaleExecution
			case "scope":
				scope = "pi-workflow-v1:" + strings.Repeat("f", 64)
			case "authority":
				authority, err := session.LoadPiRuntimeAuthority(s.config.AuthorityDir, f.runtime.Session.Slug, s.config.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				authority.Generation = strings.Repeat("f", 64)
				piTeamAuthorityFixture(t, s, authority)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+f.runtime.Session.Slug+"/activity", nil)
			request.Header.Set("X-Workspace-Conversation-Scope", scope)
			response := httptest.NewRecorder()
			s.sessionAPI(response, request)
			if response.Code == http.StatusOK || f.activities != 0 || f.sends != 0 || f.creates != 0 {
				t.Fatal("later refusal bypassed", response.Code, f.activities)
			}
			if (fault == "bind" || fault == "verify") && f.readBindings != 1 {
				t.Fatal("wrong refusal boundary", f.readBindings)
			}
			if (fault == "authority" || fault == "scope") && f.readBindings != 0 {
				t.Fatal("stale authority/scope reached native reader")
			}
		})
	}
}

func TestPiRetainedLeadAcceptsProjectedTeamProvenance(t *testing.T) {
	s, f := newPiRetainedLeadHTTPFixture(t, "read_only")
	current, release, err := s.acquirePiSessionAdmission(context.Background(), f.runtime.Session, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	original, err := agentteams.LoadInstalled(f.initial.initial.AppStorePath)
	if err != nil {
		t.Fatal(err)
	}
	base, err := teamruntime.FindCatalogPreset(*original.Catalog, current.intent.DirectTeam.ID)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := teamruntime.ProjectPiPreset(base)
	if err != nil {
		t.Fatal(err)
	}
	current.intent.DirectTeam.TeamDigest = projected.TeamDigest
	journal, err := readPiCreationJournal(piJournalPath(s.config.Workspace, current.intent.Slug))
	if err != nil {
		t.Fatal(err)
	}
	journal.PiCreationIntent = current.intent
	if err := writePiCreationRecord(piJournalPath(s.config.Workspace, current.intent.Slug), journal, maxCreationReceiptBytes); err != nil {
		t.Fatal(err)
	}
	before := privateWebInventory(t, s.config.UserStateRoot)
	role, err := s.piRetainedLeadRole(context.Background(), current.intent)
	if err != nil || role.Access != "read_only" || !reflect.DeepEqual(before, privateWebInventory(t, s.config.UserStateRoot)) {
		t.Fatal("projected provenance", role, err)
	}
}
