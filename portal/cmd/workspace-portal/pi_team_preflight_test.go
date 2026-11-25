package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/inputdelivery"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"github.com/aither64/dev-workspace/portal/internal/web"
)

func TestPiTeamPreflightRetainsNegativeOnlyPortalIdentitiesWithoutSourceWrites(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	scope.Slug = "2026-11-23-refusal"
	ctx := context.Background()
	store, err := inputdelivery.Open(ctx, s.root, scope.Workspace, func(ctx context.Context, run func() error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return run()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native-lead"}
	request := backend.DeliveryRequest{Version: 1, ClientUserMessageID: "11111111-1111-4111-8111-111111111111", Message: "private rejected draft", Delivery: "steer", Selection: strings.Repeat("c", 64)}
	_, err = store.Reject(ctx, inputdelivery.ConversationScope(ref), ref, request, "delivery_selection_changed")
	var refused *backend.DeliveryError
	if !errors.As(err, &refused) || refused.NotQueued == nil {
		t.Fatal("missing durable refusal", err)
	}
	before := piTeamFixtureSnapshot(t, s.root)
	result, err := piTeamSessionPreflight(ctx, s.root, s.workspaces)
	if err != nil || !result.Required || len(result.Sessions) != 1 || result.Sessions[0].Source.Kind != "portal_delivery" {
		t.Fatal(result, err)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), request.Message) || strings.Contains(string(data), request.Selection) || strings.Contains(string(data), request.ClientUserMessageID) {
		t.Fatal("private refusal escaped preflight", string(data))
	}
	if after := piTeamFixtureSnapshot(t, s.root); !reflect.DeepEqual(before, after) {
		t.Fatal("preflight mutated refusal source")
	}
	if _, err = inputdelivery.InspectAll(ctx, s.root, nil); err == nil {
		t.Fatal("unregistered refusal database ignored")
	}
}

func piTeamScannerFixture(t *testing.T) (*piTeamScanner, monitoroperation.PiSessionRecord) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	return &piTeamScanner{root: root, workspaces: []string{workspace}, claims: []piTeamClaim{}, monitors: map[string]monitoroperation.PiOperation{}}, monitoroperation.PiSessionRecord{Workspace: workspace, Slug: "example", Incarnation: strings.Repeat("a", 64)}
}

func TestPiTeamPreflightRuntimeProjectionKeepsLegacyWireAndEmptyPreparationArray(t *testing.T) {
	_, scope := piTeamScannerFixture(t)
	for _, schema := range []int{1, 2, 3} {
		result := piTeamPreflight{Schema: schema, Sessions: []piTeamClaim{}}
		if schema == 2 {
			result.PiPreparations = []web.PiPreparationReference{{Workspace: scope.Workspace, RequestID: "11111111-1111-4111-8111-111111111111"}}
		}
		if schema == 3 {
			result.PiRuntimes = []piTeamCurrentRuntime{{Session: scope, Generation: strings.Repeat("e", 64), Phase: "preparing"}}
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["required"]) != "false" || string(fields["sessions"]) != "[]" {
			t.Fatal("runtime presence changed domain-claim meaning", string(data))
		}
		if schema < 3 {
			if _, present := fields["piRuntimes"]; present || schema == 1 && fields["piPreparations"] != nil || schema == 2 && strings.Contains(string(fields["piPreparations"]), `"schema"`) {
				t.Fatal("legacy preflight wire changed", string(data))
			}
		} else if string(fields["piPreparations"]) != "[]" || len(fields["piRuntimes"]) == 0 {
			t.Fatal("zero-member runtime omitted or preparation requirement invented", string(data))
		}
	}
}
func TestPiTeamPreflightRecoveryTransitionProjection(t *testing.T) {
	value := piTeamCurrentRuntime{TeamConflictRecovery: &piruntime.TeamConflictRecoveryTransition{RequestID: "22222222-2222-4222-8222-222222222222", SourceGeneration: strings.Repeat("a", 64), BindingRevision: 2, Phase: "settled"}}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal(string(raw))
	}
	var proof map[string]any
	if json.Unmarshal(fields["teamConflictRecovery"], &proof) != nil || len(proof) != 4 || proof["phase"] != "settled" || proof["bindingRevision"] != float64(2) {
		t.Fatal(string(raw))
	}
	value.TeamConflictRecovery = nil
	raw, _ = json.Marshal(value)
	if strings.Contains(string(raw), "teamConflictRecovery") {
		t.Fatal("absent relation was emitted", string(raw))
	}
}

func piTeamWriteFixture(t *testing.T, path string, record any) []byte {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}
func piTeamRosterFixture(scope monitoroperation.PiSessionRecord) teamruntime.PiRoster {
	now := time.Now().UTC()
	return teamruntime.PiRoster{Schema: 2, Backend: "pi", BackendContract: 1, Workspace: scope.Workspace, Slug: scope.Slug, Revision: 1, CreatedAt: now, UpdatedAt: now, SessionIncarnation: scope.Incarnation, LeadIncarnation: strings.Repeat("b", 64), Members: []teamruntime.PiRosterMember{{Address: "implementer0", Role: "implementer", Index: 0, State: "ready", AddedAt: now, Incarnation: strings.Repeat("c", 64), CreationID: strings.Repeat("d", 64), InitialInputID: "22222222-2222-4222-8222-222222222222", Model: "model", Effort: "high", Behavior: "implementer", Purpose: "implementation", Instructions: "Retained fixture instructions", Access: "workspace_write"}}}
}
func piTeamMonitorFixture(t *testing.T, scope monitoroperation.PiSessionRecord) monitoroperation.PiOperation {
	t.Helper()
	id := "33333333-3333-4333-8333-333333333333"
	lead := monitoroperation.PiConversationRecord{Session: scope, Backend: "pi", Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native-lead"}
	owner := monitoroperation.PiOwnerRecord{Session: scope, Lead: lead, OriginInput: monitoroperation.PiInputRecord{Conversation: lead, OperationID: "11111111-1111-4111-8111-111111111111", Action: "send", Digest: strings.Repeat("4", 64)}, Generation: strings.Repeat("e", 64)}
	policy := monitoroperation.PiUtilityPolicyRecord{Kind: "verification_watcher", PackageRoot: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-fixture", CatalogDigest: strings.Repeat("f", 64), Model: "model", Effort: "high", Instructions: "Observe owned fixture only", SavedAccess: "read_only", ExecutionAccess: "full_access", MaxConcurrent: 1, Settings: monitoroperation.PiSettingsRecord{Provider: "native", AccountRef: "account", Model: "model", Thinking: "high", PolicyRevision: strings.Repeat("1", 64), Revision: 1}}
	req := monitoroperation.StartRequest{ID: id, Brief: "Observe fixture", Source: monitoroperation.Source{Kind: "local", Argv: []string{"fixture-never-executed"}, Cwd: scope.Workspace, InputIdentity: "fixture-input", Revision: "fixture-revision", InvocationID: id}}
	op, err := monitoroperation.NewPiOperation(req, owner, policy, strings.Repeat("9", 64), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func piTeamDirectObserverFixture(scope monitoroperation.PiSessionRecord) monitoroperation.ObserverRecord {
	now := time.Now().UTC()
	source := monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "example/project", WorkflowID: "12", RunID: "34", RunAttempt: 1, Commit: strings.Repeat("a", 40)}
	return monitoroperation.ObserverRecord{Schema: 1, ID: "44444444-4444-4444-8444-444444444444", Workspace: scope.Workspace,
		Owner:  monitoroperation.Owner{Slug: scope.Slug, RootThreadID: "native-owner", Incarnation: "owner-incarnation", OriginTurnID: "original-input", Socket: filepath.Join(scope.Workspace, "native.sock"), SocketEpoch: "epoch", RuntimeSchema: 1},
		Driver: "direct-ci", RequestDigest: strings.Repeat("d", 64), Source: source, SourceDigest: monitoroperation.SourceDigest(source), CreatedAt: now, UpdatedAt: now, Revision: 1, Phase: "observing", Events: []monitoroperation.Event{}}
}

// Capture synthetic bytes and metadata, excluding read-access timestamps.
func piTeamFixtureSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		value := fmt.Sprintf("%v/%d/%d/%d/%d/%v/%v", info.Mode(), info.Size(), stat.Uid, stat.Gid, stat.Ino, info.ModTime(), stat.Ctim)
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += "/" + string(data)
		}
		result[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPiTeamPreflightDirectObserversPreservePiClaimsWithoutWrites(t *testing.T) {
	for _, mode := range []string{"empty", "observer_only", "with_pi"} {
		t.Run(mode, func(t *testing.T) {
			s, scope := piTeamScannerFixture(t)
			directory := filepath.Join(s.root, "monitor-operations", piTeamHash(scope.Workspace), "observers")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if mode != "empty" {
				record := piTeamDirectObserverFixture(scope)
				piTeamWriteFixture(t, filepath.Join(directory, record.ID+".json"), record)
				for _, name := range []string{".admission.lock", record.ID + ".json.lock"} {
					if err := os.WriteFile(filepath.Join(filepath.Dir(directory), name), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "with_pi" {
				path := filepath.Join(userstate.WorkspaceDirectory(s.root, "codex-teams", scope.Workspace), scope.Slug+".json")
				piTeamWriteFixture(t, path, piTeamRosterFixture(scope))
			}
			before := piTeamFixtureSnapshot(t, s.root)
			if err := s.domains(context.Background()); err != nil {
				t.Fatal(err)
			}
			result, err := s.result()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "with_pi" {
				r := piTeamRosterFixture(scope)
				want := piTeamClaim{Source: piTeamSource{"roster", scope.Slug}, Session: scope, Lead: &piTeamLead{Incarnation: r.LeadIncarnation}, Members: []piTeamMember{{Address: "implementer0", Incarnation: strings.Repeat("c", 64), CreationID: strings.Repeat("d", 64), InitialInputID: "22222222-2222-4222-8222-222222222222"}}}
				if !result.Required || !reflect.DeepEqual(result.Sessions, []piTeamClaim{want}) {
					t.Fatal("direct observer changed Pi claims", result)
				}
			} else if result.Required || len(result.Sessions) != 0 {
				t.Fatal("direct observer acquired Pi authority", result)
			}
			if !reflect.DeepEqual(before, piTeamFixtureSnapshot(t, s.root)) {
				t.Fatal("observer preflight changed fixture bytes, metadata or filenames")
			}
		})
	}
}

func TestPiTeamPreflightDirectObserverInventoryRefusesInvalidState(t *testing.T) {
	for _, kind := range []string{"malformed", "schema", "duplicate", "unknown_field", "trailing", "owner", "source", "filename", "workspace", "file_mode", "directory_mode", "store_file", "store_symlink", "file_symlink", "file_directory", "foreign_owner", "orphan", "orphan_empty", "unknown_entry", "substore_lock", "nested", "observer_temp", "operation_temp", "parent_temp", "native_collision", "pi_collision", "record_limit", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			s, scope := piTeamScannerFixture(t)
			record := piTeamDirectObserverFixture(scope)
			parent := filepath.Join(s.root, "monitor-operations", piTeamHash(scope.Workspace))
			if kind == "orphan" || kind == "orphan_empty" {
				parent = filepath.Join(s.root, "monitor-operations", "orphan")
			}
			directory := filepath.Join(parent, "observers")
			path := filepath.Join(directory, record.ID+".json")
			if kind == "filename" {
				path = filepath.Join(directory, strings.Repeat("a", 32)+".json")
			}
			if kind == "workspace" {
				record.Workspace = t.TempDir()
			}
			if kind == "owner" {
				record.Owner.RootThreadID = ""
			}
			if kind == "source" {
				record.Source.RunID = "different"
			}
			data := piTeamWriteFixture(t, path, record)
			switch kind {
			case "malformed":
				data = []byte("{")
			case "schema":
				data = []byte(strings.Replace(string(data), `"schema":1`, `"schema":2`, 1))
			case "duplicate":
				data = []byte(strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1))
			case "unknown_field":
				data = []byte(strings.Replace(string(data), `"schema":1`, `"extra":true,"schema":1`, 1))
			case "trailing":
				data = append(data, []byte(" {}")...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file_mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory_mode":
				if err := os.Chmod(directory, 0755); err != nil {
					t.Fatal(err)
				}
			case "store_file", "store_symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(directory); err != nil {
					t.Fatal(err)
				}
				if kind == "store_file" {
					if err := os.WriteFile(directory, nil, 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(scope.Workspace, directory); err != nil {
					t.Fatal(err)
				}
			case "file_symlink", "file_directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "file_symlink" {
					if err := os.Symlink(scope.Workspace, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "foreign_owner":
				if os.Geteuid() != 0 {
					t.Skip("changing file ownership requires root")
				}
				if err := os.Chown(path, 1, -1); err != nil {
					t.Fatal(err)
				}
			case "orphan_empty":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "unknown_entry", "substore_lock", "observer_temp", "operation_temp", "parent_temp", "nested":
				name := map[string]string{"unknown_entry": "notes", "substore_lock": record.ID + ".json.lock", "observer_temp": ".observer-pending", "operation_temp": ".operation-pending", "parent_temp": ".operation-pending", "nested": "nested"}[kind]
				base := directory
				if kind == "parent_temp" {
					base = parent
				}
				if kind == "nested" {
					if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(base, name), []byte("unfinished"), 0600); err != nil {
					t.Fatal(err)
				}
			case "native_collision", "pi_collision":
				schema := 1
				if kind == "pi_collision" {
					schema = 2
				}
				piTeamWriteFixture(t, filepath.Join(parent, record.ID+".json"), map[string]any{"schema": schema})
			case "record_limit":
				s.count = 65536
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			var err error
			if kind == "native_collision" || kind == "pi_collision" {
				// Any existing parent is a collision without opening its bytes.
				err = s.monitorObservers(ctx, directory, scope.Workspace, 512<<10)
				if err == nil || !strings.Contains(err.Error(), "duplicate native or Pi") {
					t.Fatal("parent collision omitted", err)
				}
			} else {
				err = s.domains(ctx)
			}
			if err == nil {
				t.Fatal("invalid observer store admitted", kind)
			}
		})
	}
}

func TestPiTeamPreflightDirectObserversActualCommandIsReadOnly(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	record := piTeamDirectObserverFixture(scope)
	path := filepath.Join(s.root, "monitor-operations", piTeamHash(scope.Workspace), "observers", record.ID+".json")
	valid := piTeamWriteFixture(t, path, record)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func() ([]byte, error) {
		command := exec.Command(executable, "-test.run=^TestPiTeamPreflightProcess$", "--", "pi-team-preflight", "--state-root", s.root, "--workspace", scope.Workspace)
		command.Env = append(os.Environ(), "P2_TEAM_PREFLIGHT_PROCESS=1")
		return command.CombinedOutput()
	}
	before := piTeamFixtureSnapshot(t, s.root)
	output, err := invoke()
	if err != nil || string(output) != "{\"schema\":1,\"required\":false,\"sessions\":[]}\n" {
		t.Fatalf("actual observer-only command: %s %v", output, err)
	}
	if !reflect.DeepEqual(before, piTeamFixtureSnapshot(t, s.root)) {
		t.Fatal("actual CLI created locks or changed records")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(valid), `"schema":1`, `"schema":2`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	before = piTeamFixtureSnapshot(t, s.root)
	output, err = invoke()
	if err == nil || !strings.Contains(string(output), "invalid monitor observer identity") {
		t.Fatalf("actual CLI bypassed owning observer decoder: %s %v", output, err)
	}
	if !reflect.DeepEqual(before, piTeamFixtureSnapshot(t, s.root)) {
		t.Fatal("failed actual CLI changed records")
	}
}
func TestPiTeamPreflightInventoriesAllDomainStoresWithoutBindingOrEffects(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	roster := piTeamRosterFixture(scope)
	paths := map[string][]byte{}
	path := filepath.Join(userstate.WorkspaceDirectory(s.root, "codex-teams", scope.Workspace), scope.Slug+".json")
	paths[path] = piTeamWriteFixture(t, path, roster)
	op := piTeamMonitorFixture(t, scope)
	path = filepath.Join(s.root, "monitor-operations", piTeamHash(scope.Workspace), op.ID+".json")
	paths[path] = piTeamWriteFixture(t, path, op)
	observer := progressobserver.PiState{Schema: 2, Backend: "pi", Session: scope, LeadIncarnation: roster.LeadIncarnation}
	path = filepath.Join(s.root, "progress-observers", piTeamHash(scope.Workspace), piTeamHash("pi:"+scope.Incarnation+":"+roster.LeadIncarnation)+".json")
	paths[path] = piTeamWriteFixture(t, path, observer)
	lead := communication.EndpointRecord{Pi: &communication.PiEndpoint{Backend: "pi", Session: scope, LeadIncarnation: roster.LeadIncarnation, NativeID: "native-lead"}}
	legacy := communication.EndpointRecord{Codex: &communication.Identity{Slug: "legacy", Thread: "legacy-thread", Incarnation: "legacy-incarnation"}}
	now := time.Now().UTC()
	grant := communication.PiGrantRecord{ID: "44444444-4444-4444-8444-444444444444", A: lead, B: legacy, ApprovedBy: scope.Slug, Approval: "Exact fixture pair", CreatedAt: now, RevokedAt: &now}
	message := communication.PiMessageRecord{ID: "55555555-5555-4555-8555-555555555555", GrantID: grant.ID, From: lead, To: legacy, Text: "This private message must not reach stdout", Status: "canceled", CreatedAt: now}
	state := communication.PiCommunicationState{Schema: 2, Workspace: scope.Workspace, Grants: []communication.PiGrantRecord{grant}, Messages: []communication.PiMessageRecord{message}}
	path = filepath.Join(s.root, "session-communication", piTeamHash(scope.Workspace), "state.json")
	paths[path] = piTeamWriteFixture(t, path, state)
	if err := s.domains(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.utilities(nil); err != nil {
		t.Fatal(err)
	}
	result, err := s.result()
	if err != nil || !result.Required || len(result.Sessions) != 5 {
		t.Fatalf("all-store claims: %#v %v", result, err)
	}
	data, _ := json.Marshal(result)
	for _, private := range []string{message.Text, op.Launch.Envelope, "account", "Observe fixture"} {
		if strings.Contains(string(data), private) {
			t.Fatal("private native/domain content escaped compatibility output")
		}
	}
	for path, before := range paths {
		after, err := os.ReadFile(path)
		if err != nil || string(before) != string(after) {
			t.Fatal("preflight mutated retained record", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.root, "session-backends")); !os.IsNotExist(err) {
		t.Fatal("preflight created a binding", err)
	}
}

func TestPiTeamPreflightIgnoresOnlyPrivateAncillaryRosterFiles(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	directory := userstate.WorkspaceDirectory(s.root, "codex-teams", scope.Workspace)
	roster := piTeamRosterFixture(scope)
	canonical := map[string][]byte{}
	path := filepath.Join(directory, scope.Slug+".json")
	canonical[path] = piTeamWriteFixture(t, path, roster)
	path = filepath.Join(directory, "legacy.json")
	canonical[path] = piTeamWriteFixture(t, path, teamruntime.Roster{Schema: 1, Workspace: scope.Workspace, Slug: "legacy", RootThreadID: "legacy-thread", Revision: 1, CreatedAt: roster.CreatedAt, UpdatedAt: roster.UpdatedAt, Members: []teamruntime.Member{}})
	for _, name := range []string{"example.lock", "example.operation.lock"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ancillary := map[string]os.FileInfo{}
	backup := []byte("not JSON; synthetic ancillary bytes\n")
	for _, name := range []string{"example.json.backup", "example.lock.backup", "notes"} {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, backup, 0600); err != nil {
			t.Fatal(err)
		}
		stat, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		ancillary[path] = stat
	}
	before, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.domains(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := s.result()
	if err != nil || !result.Required || s.count != 2 || len(result.Sessions) != 1 || result.Sessions[0].Source != (piTeamSource{"roster", scope.Slug}) {
		t.Fatalf("canonical roster claims changed: %#v %v", result, err)
	}
	for path, original := range ancillary {
		stat, err := os.Lstat(path)
		if err != nil || !os.SameFile(original, stat) || original.Mode() != stat.Mode() || !reflect.DeepEqual(original.Sys(), stat.Sys()) {
			t.Fatal("scanner changed ancillary metadata", err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != string(backup) {
			t.Fatal("scanner changed ancillary bytes", err)
		}
	}
	for path, original := range canonical {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != string(original) {
			t.Fatal("scanner changed canonical roster", err)
		}
	}
	after, err := os.ReadDir(directory)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("scanner changed roster inventory", err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "session-backends")); !os.IsNotExist(err) {
		t.Fatal("scanner created a binding", err)
	}
}

func TestPiTeamPreflightAncillaryExclusionKeepsStrictInventoryBoundaries(t *testing.T) {
	for _, kind := range []string{"malformed_json", "invalid_slug_json", "orphan_backup", "hidden_backup", "team_writer", "pi_writer", "unknown_lock", "nonempty_lock", "lock_mode", "symlink", "directory", "private_mode", "monitor_backup"} {
		t.Run(kind, func(t *testing.T) {
			s, scope := piTeamScannerFixture(t)
			directory := userstate.WorkspaceDirectory(s.root, "codex-teams", scope.Workspace)
			piTeamWriteFixture(t, filepath.Join(directory, scope.Slug+".json"), piTeamRosterFixture(scope))
			path := filepath.Join(directory, "example.json.backup")
			switch kind {
			case "malformed_json":
				path = filepath.Join(directory, "broken.json")
			case "invalid_slug_json":
				path = filepath.Join(directory, "invalid slug.json")
			case "orphan_backup":
				path = filepath.Join(s.root, "codex-teams", "orphan", "example.json.backup")
			case "hidden_backup":
				path = filepath.Join(directory, ".backup")
			case "team_writer":
				path = filepath.Join(directory, ".team-unfinished.tmp")
			case "pi_writer":
				path = filepath.Join(directory, ".pi-record-unfinished")
			case "unknown_lock":
				path = filepath.Join(directory, "invalid slug.lock")
			case "nonempty_lock", "lock_mode":
				path = filepath.Join(directory, "example.lock")
			case "monitor_backup":
				path = filepath.Join(s.root, "monitor-operations", piTeamHash(scope.Workspace), "example.json.backup")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(scope.Slug+".json", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				data := []byte("invalid JSON; synthetic fixture\n")
				if kind == "invalid_slug_json" {
					data, _ = json.Marshal(piTeamRosterFixture(scope))
				}
				if kind == "lock_mode" {
					data = nil
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "private_mode" || kind == "lock_mode" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.domains(context.Background()); err == nil {
				t.Fatal("unsafe, authoritative or non-roster artifact treated as ancillary")
			}
		})
	}
}

func TestPiTeamPreflightRefusesOrphanMalformedAndConflictingState(t *testing.T) {
	for _, kind := range []string{"orphan_namespace", "wrong_workspace", "wrong_filename", "private_mode", "unknown_artifact", "null", "mixed_schema", "record_limit", "conflicting_incarnation"} {
		t.Run(kind, func(t *testing.T) {
			s, scope := piTeamScannerFixture(t)
			roster := piTeamRosterFixture(scope)
			path := filepath.Join(userstate.WorkspaceDirectory(s.root, "codex-teams", scope.Workspace), scope.Slug+".json")
			switch kind {
			case "orphan_namespace":
				path = filepath.Join(s.root, "codex-teams", "foreign-namespace", scope.Slug+".json")
			case "wrong_workspace":
				roster.Workspace = t.TempDir()
			case "wrong_filename":
				path = filepath.Join(filepath.Dir(path), "other.json")
			case "record_limit":
				s.count = 65536
			}
			data := piTeamWriteFixture(t, path, roster)
			switch kind {
			case "private_mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "unknown_artifact":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".pi-record-partial"), []byte("unfinished"), 0600); err != nil {
					t.Fatal(err)
				}
			case "null":
				data = []byte(strings.Replace(string(data), `"sessionIncarnation":"`+scope.Incarnation+`"`, `"sessionIncarnation":null`, 1))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "mixed_schema":
				data = []byte(strings.Replace(string(data), `"schema":2`, `"schema":1`, 1))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "conflicting_incarnation":
				s.claims = append(s.claims, piTeamClaim{Source: piTeamSource{"observer", "source"}, Session: monitoroperation.PiSessionRecord{Workspace: scope.Workspace, Slug: scope.Slug, Incarnation: strings.Repeat("f", 64)}, Members: []piTeamMember{}})
			}
			err := s.domains(context.Background())
			if err == nil {
				_, err = s.result()
			}
			if err == nil {
				t.Fatal("invalid retained state treated as absence/support")
			}
		})
	}
}

func TestPiTeamPreflightRejectsUnownedSchemaTwoCommunicationAndOutputOverflow(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	state := communication.PiCommunicationState{Schema: 2, Workspace: scope.Workspace, Grants: []communication.PiGrantRecord{}, Messages: []communication.PiMessageRecord{}}
	path := filepath.Join(s.root, "session-communication", piTeamHash(scope.Workspace), "state.json")
	piTeamWriteFixture(t, path, state)
	if err := s.domains(context.Background()); err == nil {
		t.Fatal("ownerless schema2 treated as empty success")
	}
	member := piTeamMember{Address: "implementer0", Incarnation: strings.Repeat("c", 64), NativeID: strings.Repeat("n", 4096)}
	for index := 0; index < 2000; index++ {
		if err := s.claim("observer", fmt.Sprintf("source-%d", index), scope, nil, member); err != nil {
			if s.claimBytes <= piTeamOutputLimit-64 {
				t.Fatal("unexpected claim error", err)
			}
			return
		}
	}
	t.Fatal("oversized output inventory accepted")
}

func TestPiTeamPreflightUtilityJoinUsesOriginalInputSettingsAndGeneration(t *testing.T) {
	for _, bad := range []string{"valid", "missing_operation", "wrong_native", "wrong_creation", "wrong_generation", "wrong_settings", "wrong_input", "wrong_digest", "wrong_owner", "wrong_original_context"} {
		t.Run(bad, func(t *testing.T) {
			s, scope := piTeamScannerFixture(t)
			op := piTeamMonitorFixture(t, scope)
			op.Launch.CreateAttempted, op.Launch.State, op.Launch.Generation, op.Launch.Outcome = true, "held", op.Owner.Generation, "unknown"
			op.Launch.Utility.NativeID = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
			utility := piruntime.UtilityRef{Session: scope.Ref(), OperationID: op.ID, Incarnation: op.Launch.Utility.Incarnation, NativeID: op.Launch.Utility.NativeID}
			input := piruntime.SendInput{Message: op.Launch.Envelope, ActionContext: "monitor-launch:" + op.ID}
			ref := piruntime.UtilitySubmissionRef{Utility: utility, OperationID: op.Launch.MessageID, Action: "send"}
			ref.Digest = piruntime.UtilitySubmissionDigest(ref, input, op.Launch.Settings.Runtime())
			op.Launch.Input = &monitoroperation.PiUtilityInputRecord{Utility: op.Launch.Utility, OperationID: ref.OperationID, Action: ref.Action, Digest: ref.Digest}
			s.monitors[scope.Workspace+"\x00"+op.ID] = op
			receipt := piruntime.UtilitySubmissionStatus{Schema: 2, OwnerKind: "utility", Ref: ref, Phase: "Unknown", Generation: op.Launch.Generation, Input: input, Settings: op.Launch.Settings.Runtime()}
			record := piruntime.UtilityRetention{Utility: utility, CreationID: op.Launch.CreationID, Generation: op.Launch.Generation, Settings: receipt.Settings, InitialSettings: receipt.Settings, Receipts: []piruntime.UtilitySubmissionStatus{receipt}}
			switch bad {
			case "missing_operation":
				s.monitors = map[string]monitoroperation.PiOperation{}
			case "wrong_native":
				record.Utility.NativeID = "different-native"
			case "wrong_creation":
				record.CreationID = "different-creation"
			case "wrong_generation":
				record.Generation = strings.Repeat("f", 64)
			case "wrong_settings":
				record.Settings.Model = "different-model"
			case "wrong_input":
				record.Receipts[0].Input.Message = "different-input"
			case "wrong_digest":
				record.Receipts[0].Ref.Digest = strings.Repeat("f", 64)
			case "wrong_owner":
				record.Receipts[0].OwnerKind = "retained"
			case "wrong_original_context":
				record.Receipts[0].Input.ActionContext = "monitor-launch:other"
			}
			err := s.utilities([]piruntime.UtilityRetention{record})
			if (bad == "valid") != (err == nil) {
				t.Fatalf("unexpected original utility join result: %v", err)
			}
		})
	}
}

func TestPiTeamPreflightCommandMissingStateIsReadOnlyAndArgumentsAreStrict(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	if err := os.Chmod(s.root, 0700); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(s.root)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	err = piTeamPreflightCommand([]string{"--state-root", s.root, "--workspace", scope.Workspace})
	os.Stdout = original
	write.Close()
	data, readErr := io.ReadAll(read)
	read.Close()
	if err != nil || readErr != nil || string(data) != "{\"schema\":1,\"required\":false,\"sessions\":[]}\n" {
		t.Fatalf("missing state: %s %v %v", data, err, readErr)
	}
	after, _ := os.ReadDir(s.root)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read-only command created state")
	}
	for _, args := range [][]string{nil, {"--state-root", s.root, "--state-root", s.root, "--workspace", scope.Workspace}, {"--state-root", s.root, "--workspace", scope.Workspace, "--workspace", scope.Workspace}, {"--state-root", s.root, "--workspace", "relative"}, {"--state-root", s.root, "--workspace", scope.Workspace, "extra"}} {
		if err := piTeamPreflightCommand(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestPiTeamPreflightProcess(t *testing.T) {
	if os.Getenv("P2_TEAM_PREFLIGHT_PROCESS") != "1" {
		return
	}
	// The Ruby fixture explicitly models a legacy package without input
	// delivery. Its synthetic store identity has no files in the host store.
	// Stub only that package observation in this test subprocess; every real
	// package still uses the strict owning reader and all record codecs run.
	readPackage := piTeamInputDeliveryPackage
	piTeamInputDeliveryPackage = func(app string) (bool, error) {
		if app == "/nix/store/00000000000000000000000000000000-retained-fixture" {
			return false, nil
		}
		return readPackage(app)
	}
	index := 0
	for index < len(os.Args) && os.Args[index] != "--" {
		index++
	}
	if index == len(os.Args) {
		os.Exit(2)
	}
	if err := run(os.Args[index+1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestPiTeamPreflightRubyHostAndActualScannerKeepRetiredFence(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "workspace-portal")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nexport P2_TEAM_PREFLIGHT_PROCESS=1\nexec " + quote(executable) + " -test.run=^TestPiTeamPreflightProcess$ -- \"$@\"\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("ruby", filepath.Join(source, "test/support/pi_team_preflight_driver.rb"))
	command.Env = append(os.Environ(), "P2_TEAM_SOURCE_ROOT="+source, "P2_TEAM_PORTAL_HELPER="+helper)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual scanner/host fixture: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

func TestPiTeamPreflightArchivedPiThreadlessEnvelope(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	binding := session.BackendBinding{Schema: 1, Backend: "pi", BackendContract: 1, Workspace: scope.Workspace, Slug: scope.Slug, SessionIncarnation: scope.Incarnation, CreationReceiptID: strings.Repeat("d", 64), Revision: 1, State: "ready", Conversations: []session.BackendConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64), State: "ready", NativeID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}}
	authority := session.PiRuntimeAuthority{Schema: 2, Backend: "pi", BackendContract: 1, Workspace: scope.Workspace, Slug: scope.Slug, SessionIncarnation: scope.Incarnation, BindingRevision: 1, Generation: strings.Repeat("e", 64), State: "ready"}
	runtime := piruntime.RuntimeRef{Session: scope.Ref(), InstanceID: "11111111-1111-4111-8111-111111111111", RequestID: "22222222-2222-4222-8222-222222222222", Generation: authority.Generation, Digest: strings.Repeat("f", 64)}
	conversation, err := binding.Resolve("lead")
	if err != nil {
		t.Fatal(err)
	}
	settings := piruntime.EffectiveSettings{Provider: "native", AccountRef: "account", Model: "model", Thinking: "high", PolicyRevision: strings.Repeat("a", 64), Revision: 1}
	capture := session.PiLifecycleCapture{Kind: "pi", Contract: 1, Binding: binding, Authority: authority, Runtime: runtime, Members: []piruntime.LifecycleMember{{Generation: runtime.Generation, Slot: piruntime.LifecycleSlot{Member: "lead", Incarnation: binding.Conversations[0].Incarnation, CreationID: binding.CreationReceiptID, State: "ready", NativeID: conversation.NativeID, InitialSettings: settings}, Conversation: &conversation, Settings: &settings, HistoryRef: strings.Repeat("b", 64)}}, Selectors: []session.PiLifecycleSelector{}}
	for _, name := range []string{"binding", "creation", "completion", "companion", "authority"} {
		capture.Selectors = append(capture.Selectors, session.PiLifecycleSelector{Name: name, Path: filepath.Join(scope.Workspace, name), SHA256: strings.Repeat("a", 64)})
	}
	capture.Terminal = &piruntime.HostObservation{LaunchRequest: piruntime.LaunchRequest{InstanceID: runtime.InstanceID, RequestID: runtime.RequestID, Generation: runtime.Generation}, Digest: runtime.Digest, Phase: "terminal", Outcome: "stopped", DrainProof: map[string]any{"empty": true}}
	operation := strings.Repeat("c", 64)
	path := filepath.Join(userstate.WorkspaceDirectory(s.root, "archived-worktrees", scope.Workspace), scope.Slug, operation, "lifecycle.json")
	// Ruby's archive producer explicitly records absence of a Codex root.
	journal := map[string]any{"schema": 5, "workspace": scope.Workspace, "slug": scope.Slug, "operation_id": operation, "phase": "archived", "retained_thread_id": nil, "pi": capture}
	original := piTeamWriteFixture(t, path, journal)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"threadless", "null-scope", "null-pi", "duplicate-root", "trailing", "missing-drain", "null-other-envelope"} {
		t.Run(kind, func(t *testing.T) {
			data := append([]byte{}, original...)
			switch kind {
			case "null-other-envelope":
				data = []byte(strings.Replace(string(data), `"phase":"archived"`, `"phase":null`, 1))
			case "null-scope":
				data = []byte(strings.Replace(string(data), `"slug":"example"`, `"slug":null`, 1))
			case "null-pi":
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(data, &fields)
				fields["pi"] = json.RawMessage("null")
				data, _ = json.Marshal(fields)
			case "duplicate-root":
				data = []byte(strings.Replace(string(data), `"retained_thread_id":null`, `"retained_thread_id":null,"retained_thread_id":"foreign"`, 1))
			case "trailing":
				data = append(data, []byte(` {}`)...)
			case "missing-drain":
				data = []byte(strings.Replace(string(data), `"empty":true`, `"empty":false`, 1))
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(executable, "-test.run=^TestPiTeamPreflightProcess$", "--", "pi-team-preflight", "--state-root", s.root, "--workspace", scope.Workspace)
			command.Env = append(os.Environ(), "P2_TEAM_PREFLIGHT_PROCESS=1")
			output, err := command.CombinedOutput()
			if kind == "threadless" {
				if err != nil {
					t.Fatalf("actual archived Pi preflight: %v: %s", err, output)
				}
				var result piTeamPreflight
				if json.Unmarshal(output, &result) != nil || len(result.Sessions) != 1 || result.Sessions[0].Source.Kind != "lifecycle" || result.Sessions[0].Session != scope {
					t.Fatal("lost archived scope", string(output))
				}
			} else if err == nil {
				t.Fatal("ambiguous archived Pi proof admitted", kind)
			}
			after, err := os.ReadFile(path)
			if err != nil || !reflect.DeepEqual(after, data) {
				t.Fatal("preflight mutated retained bytes", err)
			}
		})
	}
}

func TestPiTeamPreflightPreparationArmAndZeroRootOrphan(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	invoke := func(workspaces ...string) ([]byte, error) {
		t.Helper()
		args := []string{"--state-root", s.root}
		for _, workspace := range workspaces {
			args = append(args, "--workspace", workspace)
		}
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = write
		err = piTeamPreflightCommand(args)
		os.Stdout = original
		write.Close()
		data, readErr := io.ReadAll(read)
		read.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return data, err
	}
	if data, err := invoke(); err != nil || string(data) != "{\"schema\":1,\"required\":false,\"sessions\":[]}\n" {
		t.Fatalf("zero-root absence: %s %v", data, err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(userstate.WorkspaceDirectory(s.root, "portal", scope.Workspace), "session-preparation-mappings", id+".json")
	before := piTeamWriteFixture(t, path, map[string]any{"schema": 2, "workspace": scope.Workspace, "requestId": id, "inputVersion": 2, "inputDigest": strings.Repeat("a", 64), "receiptId": strings.Repeat("b", 64), "attempt": 1, "state": "terminal", "phase": "initializing", "startedAt": "2026-11-13T10:00:00Z", "updatedAt": "2026-11-13T10:00:00Z", "slug": "2026-11-13-solo", "epoch": strings.Repeat("c", 64), "terminal": "ready"})
	data, err := invoke(scope.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	var result piTeamPreflight
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Schema != 2 || result.Required || len(result.Sessions) != 0 || len(result.PiPreparations) != 1 || result.PiPreparations[0].RequestID != id {
		t.Fatal("pre-root reference became session authority", string(data))
	}
	if _, err := invoke(); err == nil {
		t.Fatal("zero-root orphan preparation omitted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("scanner changed compact original mapping")
	}
}

func TestPiTeamPreflightPackageCapabilityReaderRefusesMissingPackage(t *testing.T) {
	supported, err := piTeamInputDeliveryPackage("/nix/store/00000000000000000000000000000000-missing-input-delivery-contract-fixture")
	if supported || !os.IsNotExist(err) {
		t.Fatal("missing immutable package became legacy support", supported, err)
	}
}
