package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"github.com/coder/websocket"
)

func maintenanceIdleHostFixture(t *testing.T, scenario string) (maintenanceControl, maintenance.Operation) {
	t.Helper()
	m, o := maintenanceWorkerFixture(t, false)
	cwd := filepath.Join(m.workspace, "work", "actor")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	socket := threadCommandAppServer(t, func(conn *websocket.Conn) error {
		if err := threadCommandHandshake(conn); err != nil {
			return err
		}
		for {
			r, err := threadCommandReadObject(conn)
			if err != nil {
				return nil
			}
			var result any
			if scenario == "native-unavailable" && r["method"] == "thread/list" || scenario == "loaded-unavailable" && r["method"] == "thread/loaded/list" || scenario == "turn-unavailable" && r["method"] == "thread/turns/list" {
				if err := threadCommandWriteObject(conn, map[string]any{"id": r["id"], "error": map[string]any{"code": -32000, "message": "ambiguous native child ownership PRIVATE-SENTINEL /peer native payload"}}); err != nil {
					return err
				}
				continue
			}
			switch r["method"] {
			case "thread/read":
				metadata := threadCommandFreshMetadata("thread", cwd, filepath.Join(cwd, "rollout.jsonl"))
				if scenario == "cold-empty" || scenario == "final-missing" || scenario == "final-missing-under-successor" {
					metadata["status"] = map[string]any{"type": "notLoaded"}
				}
				result = map[string]any{"thread": metadata}
			case "thread/turns/list":
				turns := []any{map[string]any{"id": "captured", "status": "completed", "completedAt": 101}}
				if scenario == "cold-empty" || scenario == "loaded-empty" || scenario == "final-missing" {
					turns = []any{}
				}
				if scenario == "missing-under-successor" || scenario == "final-missing-under-successor" {
					turns = []any{map[string]any{"id": "successor", "status": "completed", "completedAt": 102}}
				}
				if scenario == "normalized-interrupted" {
					turns = []any{map[string]any{"id": "captured", "status": "interrupted", "completedAt": nil}}
				}
				result = map[string]any{"data": turns}
			case "thread/loaded/list":
				ids := []string{"thread"}
				if scenario == "owned-child" {
					ids = append(ids, "child")
				}
				if scenario == "unknown-loaded" || scenario == "mixed-blockers" {
					ids = []string{"unowned"}
				}
				if scenario == "cold-empty" || scenario == "final-missing" || scenario == "final-missing-under-successor" {
					ids = []string{}
				}
				result = map[string]any{"data": ids}
			case "thread/list":
				rows := []any{}
				if scenario == "owned-child" || scenario == "child-ownership" {
					rows = append(rows, map[string]any{"id": "child"})
				}
				result = map[string]any{"data": rows}
			case "thread/items/list", "thread/queue/list":
				result = map[string]any{"data": []any{}}
			default:
				return fmt.Errorf("idle host consumer sent nonpassive request: %s", r["method"])
			}
			if err := threadCommandWriteObject(conn, map[string]any{"id": r["id"], "result": result}); err != nil {
				return err
			}
		}
	})
	for name, content := range map[string]string{
		"plan.md":    "Synthetic idle proof fixture\n",
		"state.md":   "---\nlifecycle: active\n---\n",
		"portal.yml": fmt.Sprintf("schema: 1\nslug: actor\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: thread\n  socket_path: %s\n  client_version: 0.165.0\n", socket),
	} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inc, err := trackingIncarnation(m.workspace, "actor")
	if err != nil {
		t.Fatal(err)
	}
	process, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := session.SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	o.CutoverIntent, o.Phase = false, "pausing"
	o.Services = []maintenance.Service{{Workspace: m.workspace, Unit: "source", PID: os.Getpid(), ProcessIdentity: process, Native: exe, Socket: socket, SocketEpoch: epoch}}
	o.Participants = []maintenance.Participant{{Workspace: m.workspace, Slug: "actor", Incarnation: inc, Root: "thread", Thread: "thread", Cwd: cwd, SocketEpoch: epoch, SourceTurn: "captured", NoticeID: controlOperationID, NoticeText: "notice", NoticeAction: "maintenance", NoticeDisposition: "skipped", ContinuationID: controlOperationID, ContinuationDisposition: "suppressed"}}
	if scenario == "cold-empty" {
		o.Participants = []maintenance.Participant{}
	}
	return m, o
}

func TestMaintenanceInventoryRefusesUnknownAndKeepsOwnedChildrenSeparate(t *testing.T) {
	for _, scenario := range []string{"unknown-loaded", "owned-child"} {
		t.Run(scenario, func(t *testing.T) {
			m, o := maintenanceIdleHostFixture(t, scenario)
			err := m.inventory(context.Background(), &o)
			if scenario == "unknown-loaded" {
				if err == nil {
					t.Fatal("unknown loaded writer adopted")
				}
				return
			}
			if err != nil || len(o.Participants) != 0 || len(o.Children) != 1 || o.Children[0].Thread != "child" {
				t.Fatal(o, err)
			}
		})
	}
}

func TestMaintenancePrepareInventoryDiagnosticsAndNoWrites(t *testing.T) {
	for _, scenario := range []struct{ name, reason string }{
		{"unknown-loaded", "prepare-inventory-blocked:unowned-loaded-writer"},
		{"native-unavailable", "prepare-inventory-native-proof-unavailable"},
		{"loaded-unavailable", "prepare-inventory-native-proof-unavailable"},
		{"turn-unavailable", "prepare-inventory-native-proof-unavailable"},
		{"metadata", "prepare-inventory-metadata-unavailable"},
		{"ownership", "prepare-inventory-ownership-ambiguous"},
		{"child-ownership", "prepare-inventory-ownership-ambiguous"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, o := maintenanceIdleHostFixture(t, scenario.name)
			m.store.Root = t.TempDir()
			o.Phase, o.SourcesStopped = "prepared", false
			o.CandidatePackage = t.TempDir()
			writeHeldTarget(t, o.CandidatePackage, "1")
			o.Preparation, _ = json.Marshal(map[string]any{"candidatePackage": o.CandidatePackage})
			if scenario.name == "metadata" {
				path := filepath.Join(userstate.WorkspaceDirectory(m.store.Root, "codex-teams", m.workspace), "actor.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("PRIVATE-SENTINEL"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.name == "ownership" || scenario.name == "child-ownership" {
				path := filepath.Join(m.workspace, "work", "other", "portal.yml")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(m.workspace, "work", "actor", "portal.yml"))
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.Replace(string(data), "slug: actor", "slug: other", 1))
				if scenario.name == "child-ownership" {
					data = []byte(strings.Replace(string(data), "thread_id: thread", "thread_id: other-thread", 1))
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"plan.md", "state.md"} {
					data, err := os.ReadFile(filepath.Join(m.workspace, "work", "actor", name))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "prepare", OperationID: o.OperationID, Operation: &o})
			if err == nil {
				t.Fatal("invalid inventory prepared")
			}
			if got := maintenanceControlFailureReason(err); got != scenario.reason {
				t.Fatalf("reason %q, want %q: %v", got, scenario.reason, err)
			}
			requireNoMaintenancePreparation(t, m, o.OperationID)
		})
	}
}

func TestMaintenancePrepareMixedInventoryBlockersAreBoundedAndDeterministic(t *testing.T) {
	m, o, _, f, _, _ := maintenanceMonitorFixture(t, "mixed-blockers")
	store, err := teamruntime.NewStore(m.store.Root, m.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), "actor", "thread", true, func(r *teamruntime.Roster) error {
		r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Index: 0, State: "creating", Model: "gpt-6.1-sol", Effort: "xhigh", Access: "workspace_write", Behavior: "implementer", Purpose: "implementation", Instructions: "Implement assigned work", AddedAt: time.Now().UTC()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := m.store.Load(o.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	mutations := f.mutations
	err = m.inventory(context.Background(), &o)
	want := "prepare-inventory-blocked:monitor-operation-unresolved,retained-member-transition,unowned-loaded-writer"
	if err == nil || maintenanceControlFailureReason(err) != want {
		t.Fatalf("lost mixed blocker: %v", err)
	}
	after, err := m.store.Load(o.OperationID)
	if err != nil || !reflect.DeepEqual(before, after) || f.mutations != mutations {
		t.Fatal("inventory changed evidence/effects", err)
	}
	// Every subset has fixed order, no duplicates, and no unrecognized values.
	categories := []string{"monitor-operation-unresolved", "retained-member-transition", "unowned-loaded-writer"}
	for mask := 0; mask < 8; mask++ {
		input, expected := []string{"PRIVATE-SENTINEL"}, []string{}
		for i := 2; i >= 0; i-- {
			if mask&(1<<i) != 0 {
				input = append(input, categories[i], categories[i])
			}
		}
		for i, category := range categories {
			if mask&(1<<i) != 0 {
				expected = append(expected, category)
			}
		}
		want := "prepare-inventory-failed"
		if len(expected) != 0 {
			want = "prepare-inventory-blocked:" + strings.Join(expected, ",")
		}
		if got := maintenanceControlFailureReason(maintenanceInventoryBlockers(input)); got != want {
			t.Fatalf("subset %d: %q, want %q", mask, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(m.store.Root, "maintenance", "startup.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inventory wrote startup", err)
	}
}
