package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/coder/websocket"
)

func TestProviderInventoryRetainedMonitorProofs(t *testing.T) {
	for _, fault := range []string{"none", "empty-source-list", "missing-home", "source", "origin", "cwd", "ephemeral", "history", "bootstrap"} {
		t.Run(fault, func(t *testing.T) {
			workspace, stateRoot := t.TempDir(), t.TempDir()
			const slug = "provider-fixture"
			const root = "11111111-1111-4111-8111-111111111111"
			const utility = "22222222-2222-4222-8222-222222222222"
			const project = "33333333-3333-4333-8333-333333333333"
			const operation = "44444444-4444-4444-8444-444444444444"
			const marker = "Internal verification utility fixture initialization."
			cwd, home := filepath.Join(workspace, "work", slug), filepath.Join(workspace, "codex-home")
			rollout := filepath.Join(home, "sessions", "2026", "01", "01", "rollout-2026-01-01T00-00-00-"+utility+".jsonl")
			write := func(path, value string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			bootstrap := marker
			if fault == "bootstrap" {
				bootstrap = "different initialization"
			}
			var history strings.Builder
			for _, record := range []any{
				map[string]any{"type": "session_meta", "payload": map[string]any{"id": utility, "cwd": cwd}},
				map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": bootstrap}}}},
			} {
				data, _ := json.Marshal(record)
				history.Write(data)
				history.WriteByte('\n')
			}
			write(rollout, history.String())
			socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
				if err := threadCommandHandshake(connection); err != nil {
					return err
				}
				for {
					request, err := threadCommandReadObject(connection)
					if err != nil {
						return nil
					}
					params, _ := request["params"].(map[string]any)
					var result any
					switch request["method"] {
					case "project/read":
						if params["projectId"] != project {
							return fmt.Errorf("foreign project read")
						}
						result = map[string]any{"project": map[string]any{"id": project, "name": "monitor " + operation}}
					case "thread/read":
						id := params["threadId"]
						if id != root && id != utility || params["includeTurns"] == true {
							return fmt.Errorf("foreign thread or transcript read")
						}
						metadata := threadCommandFreshMetadata(id.(string), cwd, rollout)
						metadata["model"], metadata["reasoningEffort"] = "fixture-model", "low"
						// Scoped inventory must remain eligible while work is active.
						metadata["status"] = map[string]any{"type": "active", "activeFlags": []any{}}
						if id == utility {
							metadata["projectId"] = project
							switch fault {
							case "source":
								metadata["source"] = "cli"
							case "origin":
								metadata["forkedFromId"] = root
							case "cwd":
								metadata["cwd"] = workspace
							case "ephemeral":
								metadata["ephemeral"] = true
							case "history":
								metadata["historyMode"] = "legacy"
							}
						}
						result = map[string]any{"thread": metadata}
					case "thread/loaded/list":
						result = map[string]any{"data": []string{root, utility, "unrelated-thread"}}
					default:
						return fmt.Errorf("inventory issued unexpected RPC %v", request["method"])
					}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			write(filepath.Join(cwd, "state.md"), "---\nlifecycle: active\n---\n")
			write(filepath.Join(cwd, "plan.md"), "Provider inventory fixture\n")
			write(filepath.Join(cwd, "portal.yml"), fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.165.0\ncreation:\n  state: ready\n", slug, root, socket))
			write(filepath.Join(workspace, "share", "dev-workspace", "package.json"), `{"schema":1,"activationEnvironmentAliases":["DEV_WORKSPACES"],"routerSocket":"/run/fixture/router.sock","userNamespace":"fixture","agent_teams":{"managed":false}}`)
			digest := func(value any) string { data, _ := json.Marshal(value); return fmt.Sprintf("%x", sha256.Sum256(data)) }
			source := monitoroperation.Source{Kind: "local", Argv: []string{"fixture-check"}, Cwd: workspace, InputIdentity: "fixture", Revision: "fixture-revision", InvocationID: operation}
			request := monitoroperation.StartRequest{ID: operation, Brief: "Observe fixture", Source: source}
			now := time.Now().UTC()
			op := monitoroperation.Operation{
				Schema: monitoroperation.Schema, ID: operation, Workspace: workspace, Brief: request.Brief, Source: source, RequestDigest: digest(request), CreatedAt: now,
				Owner:         monitoroperation.Owner{Slug: slug, RootThreadID: root, Incarnation: "fixture", OriginTurnID: "origin", Socket: socket, SocketEpoch: "epoch", RuntimeSchema: 1},
				UtilityPolicy: monitoroperation.UtilityPolicy{PackageRoot: workspace, CatalogDigest: strings.Repeat("a", 64), NativeName: "fixture-watcher", Model: "fixture-model", Effort: "low", Instructions: "Observe only.", Sandbox: "read-only", ApprovalPolicy: "never", MaxConcurrent: 1},
				Launch:        monitoroperation.Launch{State: "unsubmitted", ProjectID: project, ThreadID: utility, CreateAttempted: true, BootstrapAttempted: true, BootstrapMarker: marker, MessageID: digest([]string{"monitor", operation, "launch"})[:32], Envelope: "Fixture check", TerminatedAt: &now},
			}
			path := filepath.Join(stateRoot, "monitor-operations", fmt.Sprintf("%x", sha256.Sum256([]byte(workspace))), operation+".json")
			data, _ := json.Marshal(op)
			write(path, string(data))
			t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
			if fault == "missing-home" {
				t.Setenv("DEV_WORKSPACE_CODEX_HOME", "")
			}
			client := providerCodexClient(socket, stateRoot)
			if fault == "empty-source-list" {
				client.Client.Close()
				client.Client = codex.NewWithOptions(socket, codex.ClientOptions{AllowImplicitResume: func(string) bool { return false }})
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			inventory, err := buildProviderInventoryScope(ctx, client, workspace, stateRoot, socket, workspace, map[string]bool{slug: true})
			if fault != "none" {
				want := "monitor bootstrap identity is unproved"
				switch fault {
				case "empty-source-list":
					want = "headless Codex thread has the wrong identity"
				case "missing-home":
					want = "canonical directory and Codex home"
				case "source", "origin", "cwd":
					want = "monitor thread identity is unproved"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("wanted %q, got %v", want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(inventory.Threads) != 2 || inventory.Threads[1].MonitorOperationID != operation || len(inventory.Blockers) != 0 {
				t.Fatalf("unexpected scoped inventory: %#v", inventory)
			}
		})
	}
}
