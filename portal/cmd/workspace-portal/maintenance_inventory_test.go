package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
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
				if scenario == "unknown-loaded" {
					ids = []string{"unowned"}
				}
				if scenario == "cold-empty" || scenario == "final-missing" || scenario == "final-missing-under-successor" {
					ids = []string{}
				}
				result = map[string]any{"data": ids}
			case "thread/list":
				rows := []any{}
				if scenario == "owned-child" {
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
