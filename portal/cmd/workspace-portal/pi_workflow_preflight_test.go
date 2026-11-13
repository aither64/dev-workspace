package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

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
	rows := []piruntime.ConversationRetention{{Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 1}, {Conversation: ref, CreationID: strings.Repeat("d", 64), Schema: 2}}
	if err := s.workflowConversations(rows); err != nil {
		t.Fatal(err)
	}
	result, err := s.result()
	if err != nil || s.count != 2 || len(result.Sessions) != 1 {
		t.Fatal(result, err)
	}
	claim := result.Sessions[0]
	if claim.Source.Kind != "tool_policy" || claim.Lead != nil || len(claim.Members) != 1 || claim.Members[0].Address != "lead" || claim.Members[0].CreationID != rows[1].CreationID {
		t.Fatal(claim)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "ToolPolicy") {
		t.Fatal("tool arrays escaped")
	}
	s.count = 65536
	if err := s.workflowConversations(rows[:1]); err == nil {
		t.Fatal("legacy mapping skipped combined budget")
	}
	s.count = 0
	rows[0].Schema = 3
	if err := s.workflowConversations(rows[:1]); err == nil {
		t.Fatal("schema3 skipped")
	}
}
