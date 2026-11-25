package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func TestPiWorkflowPreflightGenericRoot(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0700, 0755} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%o/present=%t", mode, present), func(t *testing.T) {
				s, scope := piTeamScannerFixture(t)
				scope.Slug = "2026-11-13-example"
				if err := os.Chmod(s.root, mode); err != nil {
					t.Fatal(err)
				}
				ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "11111111-1111-4111-8111-111111111111"}
				path := filepath.Join(s.root, "pi-runtime", piTeamHash(scope.Workspace), scope.Slug, scope.Incarnation, "members", ref.Member, ref.Incarnation, "conversation.json")
				if present {
					// Codec fixture; the owning reader test separately exercises the
					// actual Manager and directory/record writer without a worker.
					locator, _ := json.Marshal([]any{"pi-history", 1, ref.Backend, ref.Session.Workspace, ref.Session.Slug, ref.Session.Incarnation, ref.Member, ref.Incarnation, ref.NativeID})
					policy := piruntime.ToolPolicyTarget{Mode: "plan", Revision: 1, Active: []string{"read"}, RegisteredCeiling: []string{"read"}}
					policy.Digest = piruntime.ToolPolicyDigest(policy)
					settings := piruntime.EffectiveSettings{Provider: "fixture", AccountRef: "fixture", Model: "model", Thinking: "off", PolicyRevision: "fixture", Revision: 1}
					piTeamWriteFixture(t, path, map[string]any{"Schema": 2, "Conversation": ref, "CreationID": strings.Repeat("d", 64), "HistoryRef": piTeamHash(string(locator)), "Generation": strings.Repeat("c", 64), "Settings": settings, "InitialSettings": settings, "ToolPolicy": policy, "Instructions": "Frozen rôle\nexact bytes"})
				}
				snapshot := func() map[string]string {
					result := map[string]string{}
					if err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, err error) error {
						if err != nil {
							return err
						}
						info, err := entry.Info()
						if err != nil {
							return err
						}
						var data []byte
						if !entry.IsDir() {
							if data, err = os.ReadFile(path); err != nil {
								return err
							}
						}
						result[path] = fmt.Sprintf("%o:%s", info.Mode(), data)
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					return result
				}
				before := snapshot()
				command := exec.Command(executable, "-test.run=^TestPiTeamPreflightProcess$", "--", "pi-team-preflight", "--state-root", s.root, "--workspace", scope.Workspace)
				command.Env = append(os.Environ(), "P2_TEAM_PREFLIGHT_PROCESS=1")
				data, err := command.Output()
				if present {
					// A mapping without runtime.json is held by the aggregate reader.
					// Keep this partial fixture's component codec/claim coverage separate.
					failure, ok := err.(*exec.ExitError)
					if !ok || strings.TrimSpace(string(failure.Stderr)) != piruntime.ErrHeld.Error() || len(data) != 0 {
						t.Fatalf("partial runtime did not stay held: %s %v", data, err)
					}
					if strings.Contains(string(failure.Stderr), "Frozen rôle") || strings.Contains(string(failure.Stderr), "RegisteredCeiling") {
						t.Fatal("refusal leaked retained content")
					}
					rows, readErr := piruntime.InspectRetainedConversations(context.Background(), s.root, []string{scope.Workspace})
					if readErr != nil {
						t.Fatal(readErr)
					}
					if err := s.workflowConversations(rows); err != nil {
						t.Fatal(err)
					}
					projected, projectErr := s.result()
					if projectErr != nil {
						t.Fatal(projectErr)
					}
					data, err = json.Marshal(projected)
				}
				var result piTeamPreflight
				if err != nil || json.Unmarshal(data, &result) != nil || result.Schema != 1 || result.Required != present {
					t.Fatalf("actual preflight: %s %v", data, err)
				}
				want := 0
				if present {
					want = 2
				}
				if len(result.Sessions) != want {
					t.Fatal(result)
				}
				kinds := map[string]bool{}
				for _, claim := range result.Sessions {
					if claim.Session != scope || claim.Lead != nil || len(claim.Members) != 1 || claim.Members[0] != (piTeamMember{Address: ref.Member, Incarnation: ref.Incarnation, NativeID: ref.NativeID, CreationID: strings.Repeat("d", 64)}) {
						t.Fatal("retained claim lost identity", claim)
					}
					kinds[claim.Source.Kind] = true
				}
				if present && (!kinds["tool_policy"] || !kinds["role_instructions"]) {
					t.Fatal("retained obligations omitted", result)
				}
				if strings.Contains(string(data), "Frozen rôle") || strings.Contains(string(data), "RegisteredCeiling") || !reflect.DeepEqual(before, snapshot()) {
					t.Fatal("preflight leaked content or changed paths, bytes or modes")
				}
			})
		}
	}
}

func workflowRecordFixture(t *testing.T, kind string) (*piTeamScanner, string, agentbackend.ConversationRef) {
	t.Helper()
	s, scope := piTeamScannerFixture(t)
	scope.Slug = "2026-11-12-example"
	ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native-lead"}
	name := "questions.json"
	var record any = piquestions.Record{Schema: 1, Conversation: ref, NextSequence: 1, Questions: []piquestions.QuestionRecord{}}
	if kind == "pi-plans" {
		name = "plans.json"
		record = piplan.Record{Schema: 1, Conversation: ref, Proposals: []piplan.Proposal{}, Acceptances: []piplan.Acceptance{}}
	}
	path, err := (piquestions.StateFile{Root: s.root, Kind: kind, Name: name, Ref: ref}).Path()
	if err != nil {
		t.Fatal(err)
	}
	piTeamWriteFixture(t, path, record)
	return s, path, ref
}

func TestPiWorkflowPreflightKeepsPresentEmptyIdentityWithoutLocks(t *testing.T) {
	for _, kind := range []string{"pi-questions", "pi-plans"} {
		t.Run(kind, func(t *testing.T) {
			s, path, ref := workflowRecordFixture(t, kind)
			before, _ := os.ReadFile(path)
			if err := s.workflowRecords(context.Background(), kind, filepath.Base(path)); err != nil {
				t.Fatal(err)
			}
			r, err := s.result()
			if err != nil || !r.Required || len(r.Sessions) != 1 || r.Sessions[0].Lead == nil || r.Sessions[0].Lead.NativeID != ref.NativeID || !piTeamHex.MatchString(r.Sessions[0].Source.ID) {
				t.Fatal(r, err)
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("scan rewrote record")
			}
			if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
				t.Fatal("scan created lock")
			}
			if err := os.WriteFile(path+".lock", nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.workflowRecords(context.Background(), kind, filepath.Base(path)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPiWorkflowPreflightRefusesMalformedPathPrivateArtifactAndOverflow(t *testing.T) {
	for _, kind := range []string{"pi-questions", "pi-plans"} {
		for _, bad := range []string{"foreign-workspace", "foreign-session", "foreign-member", "null", "duplicate", "version", "file-mode", "parent-mode", "temp", "symlink", "lock", "count", "size"} {
			t.Run(kind+"/"+bad, func(t *testing.T) {
				s, path, ref := workflowRecordFixture(t, kind)
				data, _ := os.ReadFile(path)
				switch bad {
				case "foreign-workspace":
					data = []byte(strings.ReplaceAll(string(data), ref.Session.Workspace, "/another-workspace"))
				case "foreign-session":
					data = []byte(strings.ReplaceAll(string(data), ref.Session.Slug, "2026-11-12-foreign"))
				case "foreign-member":
					data = []byte(strings.ReplaceAll(string(data), `"Member":"lead"`, `"Member":"implementer0"`))
				case "null":
					data = []byte(strings.Replace(string(data), `"schema":1`, `"schema":null`, 1))
				case "duplicate":
					data = []byte(strings.Replace(string(data), `"schema":1`, `"schema":1,"schema":1`, 1))
				case "version":
					data = []byte(strings.Replace(string(data), `"schema":1`, `"schema":3`, 1))
				case "file-mode":
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				case "parent-mode":
					if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
				case "temp":
					if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".write-interrupted"), []byte("partial"), 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Rename(path, path+"-original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path+"-original", path); err != nil {
						t.Fatal(err)
					}
				case "lock":
					if err := os.WriteFile(path+".lock", []byte("unexpected"), 0600); err != nil {
						t.Fatal(err)
					}
				case "count":
					s.count = 65536
				case "size":
					data = []byte(strings.Repeat(" ", piquestions.StoreLimit+1))
				}
				if bad != "symlink" {
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.workflowRecords(context.Background(), kind, filepath.Base(path)); err == nil {
					t.Fatal("unsafe retained workflow accepted")
				}
			})
		}
	}
}

func TestPiWorkflowMappingClaimsIncludeLeadCreationAndAllRecordBudget(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native-lead"}
	rows := []piruntime.ConversationRetention{{Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 1}, {Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 2, HasInstructions: true}}
	if err := s.workflowConversations(rows); err != nil {
		t.Fatal(err)
	}
	result, err := s.result()
	if err != nil || s.count != 2 || len(result.Sessions) != 2 {
		t.Fatal(result, err)
	}
	claim := result.Sessions[1]
	if claim.Source.Kind != "tool_policy" || claim.Lead != nil || len(claim.Members) != 1 || claim.Members[0].Address != "lead" || claim.Members[0].CreationID != rows[1].CreationID {
		t.Fatal(claim)
	}
	role := result.Sessions[0]
	if role.Source.Kind != "role_instructions" || role.Source.ID != claim.Source.ID || role.Lead != nil || len(role.Members) != 1 || role.Members[0] != claim.Members[0] {
		t.Fatal("instruction claim changed mapping identity", role)
	}
	if s.claimBytes == 0 {
		t.Fatal("claims omitted output budget")
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "ToolPolicy") || strings.Contains(string(data), "Instructions") {
		t.Fatal("tool arrays escaped")
	}
	s.count = 65536
	if err := s.workflowConversations(rows[:1]); err == nil {
		t.Fatal("legacy mapping skipped combined budget")
	}
	s.count = 0
	s.claimBytes = piTeamOutputLimit
	if err := s.workflowConversations(rows[1:]); err == nil {
		t.Fatal("instruction mapping skipped output budget")
	}
	s.claimBytes = 0
	rows[0].HasInstructions = true
	if err := s.workflowConversations(rows[:1]); err == nil {
		t.Fatal("schema1 instructions accepted")
	}
	// Schema 3 is the owning native fork mapping; roster 4 and creation 5
	// are separate record formats and remain unsupported conversation versions.
	fork := &piTeamScanner{root: s.root, workspaces: s.workspaces, claims: []piTeamClaim{}}
	forkRows := []piruntime.ConversationRetention{{Conversation: ref, CreationID: rows[0].CreationID, Schema: 3, HasFork: true, HasInstructions: true}}
	if err := fork.workflowConversations(forkRows); err != nil {
		t.Fatal(err)
	}
	forkResult, err := fork.result()
	if err != nil || fork.count != 1 || fork.claimBytes == 0 || len(forkResult.Sessions) != 3 {
		t.Fatal("fork mapping lost claims or record budget", forkResult, err)
	}
	for index, kind := range []string{"fork", "role_instructions", "tool_policy"} {
		claim := forkResult.Sessions[index]
		if claim.Source.Kind != kind || claim.Session != scope || claim.Lead != nil || len(claim.Members) != 1 || claim.Members[0] != role.Members[0] {
			t.Fatal("fork claim lost original lead creation proof", claim)
		}
	}
	fork.count = 65536
	if err := fork.workflowConversations(forkRows); err == nil {
		t.Fatal("fork mapping skipped combined budget")
	}
	fork.count = 0
	fork.claimBytes = piTeamOutputLimit
	if err := fork.workflowConversations(forkRows); err == nil {
		t.Fatal("fork mapping skipped output budget")
	}
	for _, version := range []int{4, 5} {
		rows[0].Schema = version
		if err := s.workflowConversations(rows[:1]); err == nil {
			t.Fatalf("unsupported conversation schema %d skipped", version)
		}
	}
}

func TestPiWorkflowMappingKeepsIndependentToolPolicyAndMemberInstructions(t *testing.T) {
	for _, member := range []string{"lead", "implementer0"} {
		for _, instructions := range []bool{false, true} {
			s, scope := piTeamScannerFixture(t)
			ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: member, Incarnation: strings.Repeat("b", 64), NativeID: "native-member"}
			if err := s.workflowConversations([]piruntime.ConversationRetention{{Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 2, HasInstructions: instructions}}); err != nil {
				t.Fatal(err)
			}
			want := 1
			if instructions {
				want++
			}
			if s.count != 1 || len(s.claims) != want {
				t.Fatal(s.count, s.claims)
			}
			for _, claim := range s.claims {
				if claim.Lead != nil || len(claim.Members) != 1 || claim.Members[0].Address != member || claim.Members[0].CreationID != strings.Repeat("d", 64) {
					t.Fatal(claim)
				}
			}
		}
	}
}

func TestPiNativeInputDeliveryReceiptRetainsCapabilityClaim(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "native"}
	if err := s.workflowConversations([]piruntime.ConversationRetention{{Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 1, HasInputDelivery: true}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.result()
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0].Source.Kind != "input_delivery" || len(result.Sessions[0].Members) != 1 {
		t.Fatal(result, err)
	}
}

func TestPiWorkflowPreflightClaimsRetainedTeamConflictReader(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "11111111-1111-4111-8111-111111111111"}
	rows := []piruntime.ConversationRetention{{Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 1, HasTeamConflictRecovery: true}}
	if err := s.workflowConversations(rows); err != nil {
		t.Fatal(err)
	}
	result, err := s.result()
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0].Source.Kind != "team_conflict_recovery" || result.Sessions[0].Members[0].NativeID != ref.NativeID {
		t.Fatal(result, err)
	}
}

func TestPiWorkflowPreflightClaimsTerminalRefusalReader(t *testing.T) {
	s, scope := piTeamScannerFixture(t)
	ref := agentbackend.ConversationRef{Session: scope.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "11111111-1111-4111-8111-111111111111"}
	rows := []piruntime.ConversationRetention{{Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 1, HasTeamConflictRecovery: true, HasTeamConflictTerminalRefusal: true}}
	if err := s.workflowConversations(rows); err != nil {
		t.Fatal(err)
	}
	result, err := s.result()
	if err != nil || len(result.Sessions) != 2 {
		t.Fatal(result, err)
	}
	kinds := map[string]bool{}
	for _, claim := range result.Sessions {
		kinds[claim.Source.Kind] = true
	}
	if !kinds["team_conflict_recovery"] || !kinds["team_conflict_terminal_refusal"] {
		t.Fatal("missing supplemental reader", kinds)
	}
}
