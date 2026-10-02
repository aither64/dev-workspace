package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

func TestFreshThreadCreateAndForkAvoidDiscovery(t *testing.T) {
	for _, command := range []string{"create", "fork"} {
		t.Run(command, func(t *testing.T) {
			workspace := t.TempDir()
			cwd := filepath.Join(workspace, "work", "example")
			socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
				defer connection.CloseNow()
				if err := threadCommandHandshake(connection); err != nil {
					return err
				}
				if command == "fork" {
					request, err := threadCommandReadObject(connection)
					params, _ := request["params"].(map[string]any)
					if err != nil || request["method"] != "thread/turns/list" || params["threadId"] != "source" ||
						params["limit"] != float64(1) || params["sortDirection"] != "desc" || params["itemsView"] != "notLoaded" {
						return fmt.Errorf("fresh fork source-idleness check = %#v, %v", request, err)
					}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": map[string]any{
						"data": []any{map[string]any{"id": "source-turn", "status": "completed"}},
					}}); err != nil {
						return err
					}
				}
				request, err := threadCommandReadObject(connection)
				want := "thread/start"
				if command == "fork" {
					want = "thread/fork"
				}
				if err != nil || request["method"] != want {
					return fmt.Errorf("fresh path invoked discovery: %#v, %v", request, err)
				}
				params, _ := request["params"].(map[string]any)
				if params["cwd"] != cwd || (command == "fork" && params["threadId"] != "source") {
					return fmt.Errorf("fresh request changed identity: %#v", params)
				}
				metadata := map[string]any{"id": "new-root", "cwd": cwd}
				if command == "fork" {
					metadata["forkedFromId"] = "source"
				}
				return threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": map[string]any{"thread": metadata}})
			})
			args := []string{command, "--socket", socket, "--workspace", workspace, "--session-slug", "example", "--cwd", cwd,
				"--worktrees-dir", filepath.Join(workspace, "worktrees", "example"), "--portal-base-url", "https://workspace.example",
				"--portal-url", "https://workspace.example/example/", "--authority-dir", filepath.Join(workspace, "authority"),
				"--tmux-socket", filepath.Join(workspace, "tmux.sock"), "--codex-command", "/bin/codex", "--codex-version", "0.160.0",
				"--portal-command", "/bin/workspace-portal", "--require-runtime"}
			if command == "fork" {
				args = append(args, "--thread-id", "source", "--fresh")
			}
			if err := threadCommand(args); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreationRosterUsesRecordedRootAndReleasesBeforeReplacement(t *testing.T) {
	workspace, stateRoot := t.TempDir(), t.TempDir()
	store, err := teamruntime.NewStore(stateRoot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = withCreationRoster(ctx, stateRoot, workspace, "missing", "root", func(options workspacecodex.RecoveryOptions) (string, error) {
		if options.RetainedRoster || len(options.ExcludedThreads) != 0 {
			t.Fatal("missing roster supplied exclusions")
		}
		options.BeforeCreate()
		release, err := store.LockOperation(ctx, "missing")
		if err != nil {
			t.Fatalf("replacement retained roster operation lock: %v", err)
		}
		release()
		return "new-root", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(ctx, "retained", "root", true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{
			{Address: "reviewer0", Role: "reviewer", Thread: "member", State: "ready", AddedAt: time.Now()},
			{Address: "implementer0", Role: "implementer", State: "creating", AddedAt: time.Now()},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = withCreationRoster(ctx, stateRoot, workspace, "retained", "root", func(options workspacecodex.RecoveryOptions) (string, error) {
		if !options.RetainedRoster || len(options.ExcludedThreads) != 1 {
			t.Fatalf("exclusions = %#v", options)
		}
		if _, ok := options.ExcludedThreads["member"]; !ok {
			t.Fatal("lost exact legacy member identity")
		}
		return "root", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, wrongRoot := range []string{"", "different-root"} {
		_, err = withCreationRoster(ctx, stateRoot, workspace, "retained", wrongRoot, func(workspacecodex.RecoveryOptions) (string, error) {
			t.Error("used roster without trusted root")
			return "", nil
		})
		if err == nil {
			t.Fatalf("accepted root %q", wrongRoot)
		}
	}
}
