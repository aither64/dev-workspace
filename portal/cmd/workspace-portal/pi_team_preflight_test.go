package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

func piTeamScannerFixture(t *testing.T) (*piTeamScanner, monitoroperation.PiSessionRecord) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	return &piTeamScanner{root: root, workspaces: []string{workspace}, claims: []piTeamClaim{}, monitors: map[string]monitoroperation.PiOperation{}}, monitoroperation.PiSessionRecord{Workspace: workspace, Slug: "example", Incarnation: strings.Repeat("a", 64)}
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
	for _, args := range [][]string{nil, {"--state-root", s.root}, {"--state-root", s.root, "--state-root", s.root, "--workspace", scope.Workspace}, {"--state-root", s.root, "--workspace", scope.Workspace, "--workspace", scope.Workspace}, {"--state-root", s.root, "--workspace", "relative"}, {"--state-root", s.root, "--workspace", scope.Workspace, "extra"}} {
		if err := piTeamPreflightCommand(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestPiTeamPreflightProcess(t *testing.T) {
	if os.Getenv("P2_TEAM_PREFLIGHT_PROCESS") != "1" {
		return
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
