package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

func TestScopedProviderSwitchKeepsOtherSessionsAndRecoversLostCloseReply(t *testing.T) {
	for _, closeFault := range []string{"none", "lost-reply", "after-shutdown"} {
		for _, goalEnd := range []string{"active", "during-close", "before-close"} {
			t.Run(closeFault+"-goal-"+goalEnd, func(t *testing.T) {
				goalFinishes := goalEnd != "active"
				workspace := t.TempDir()
				slug := "2026-10-18-scope"
				cwd := filepath.Join(workspace, "work", slug)
				authority := filepath.Join(workspace, "authority")
				if err := os.MkdirAll(authority, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(workspace, "switch.json")
				operation := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
				snapshot := scopedProviderSnapshot{providerInventory: providerInventory{Schema: 1, Workspace: workspace, Threads: []providerThread{}, Loaded: []string{"busy", "idle"}, Blockers: []string{}}, Operation: operation, Target: "openai", Sessions: []string{slug}, Intents: map[string]*scopedThreadIntent{}}
				original := func(id, provider string) map[string]any {
					return map[string]any{"thread": map[string]any{"id": id, "canAcceptDirectInput": true}, "cwd": cwd, "model": "fixture-model", "modelProvider": provider, "reasoningEffort": "low", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "dangerFullAccess"}, "runtimeWorkspaceRoots": []any{workspace}, "activePermissionProfile": nil, "serviceTier": nil}
				}
				for _, id := range []string{"busy", "idle"} {
					snapshot.Threads = append(snapshot.Threads, providerThread{ID: id, Slug: slug, Cwd: cwd, Settings: codex.ThreadSettings{Model: "fixture-model", ReasoningEffort: "low"}, Original: original(id, "pool")})
					snapshot.Intents[id] = &scopedThreadIntent{Active: id == "busy", GoalActive: id == "busy", GoalOwned: id == "busy", PauseTurnID: id + "-turn", Continuation: operation}
				}
				if err := providerAtomicJSON(path, snapshot); err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				archived := map[string]bool{}
				stopped := map[string]bool{}
				continuations := 0
				providers := map[string]string{"busy": "pool", "idle": "pool", "unrelated": "pool", "child": "pool"}
				closes := map[string]int{}
				goalActive := true
				lost := false
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
						id, _ := params["threadId"].(string)
						if id == "unrelated" || id == "child" {
							return fmt.Errorf("unrelated session was touched")
						}
						mu.Lock()
						result := any(map[string]any{})
						injected := false
						switch request["method"] {
						case "thread/read":
							if params["excludeTurns"] != true {
								mu.Unlock()
								return fmt.Errorf("full transcript requested for a scoped check")
							}
							rollout := filepath.Join(workspace, "sessions", id+".jsonl")
							if archived[id] {
								rollout = filepath.Join(workspace, "archived_sessions", id+".jsonl")
							}
							result = map[string]any{"thread": map[string]any{"id": id, "path": rollout, "cwd": cwd, "source": "vscode", "model": "fixture-model", "reasoningEffort": "low", "modelProvider": providers[id], "status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true}}
						case "thread/archive":
							closes[id]++
							if goalFinishes && id == "busy" {
								goalActive = false
							}
							stopped[id] = true
							if closeFault != "after-shutdown" || lost {
								archived[id] = true
							}
							if closeFault != "none" && !lost {
								lost = true
								injected = true
							}
						case "thread/unarchive":
							archived[id] = false
						case "thread/resume":
							if archived[id] {
								mu.Unlock()
								return fmt.Errorf("archived thread resumed")
							}
							if provider, _ := params["modelProvider"].(string); provider != "" {
								providers[id] = provider
							}
							result = original(id, providers[id])
						case "thread/turns/list":
							status := "completed"
							if id == "busy" {
								status = "inProgress"
								if stopped[id] {
									status = "interrupted"
								}
							}
							turnID := id + "-turn"
							if id == "busy" && goalEnd == "before-close" {
								goalActive, turnID = false, "new-ordinary-turn"
							}
							result = map[string]any{"data": []any{map[string]any{"id": turnID, "status": status}}}
						case "turn/start":
							continuations++
							result = map[string]any{"turn": map[string]any{"id": "continued-turn"}}
						case "thread/goal/get":
							if archived[id] {
								mu.Unlock()
								return fmt.Errorf("goal lookup cannot read unloaded archived history %s", id)
							}
							status := "complete"
							if goalActive {
								status = "active"
							}
							result = map[string]any{"goal": nil}
							if id == "busy" {
								result = map[string]any{"goal": map[string]any{"threadId": id, "status": status}}
							}
						default:
							mu.Unlock()
							return fmt.Errorf("unexpected scoped request %v", request["method"])
						}
						mu.Unlock()
						reply := map[string]any{"id": request["id"], "result": result}
						if injected {
							delete(reply, "result")
							reply["error"] = map[string]any{"code": -32000, "message": "synthetic lost close reply"}
						}
						if err := threadCommandWriteObject(connection, reply); err != nil {
							return err
						}
					}
				})
				client := &workspacecodex.Client{Client: codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})}
				defer client.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := os.MkdirAll(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{"state.md": "---\nlifecycle: active\n---\n", "plan.md": "Scoped switch fixture\n"} {
					if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
				manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: busy\n  socket_path: %s\n  client_version: 0.163.0\n", slug, socket)
				if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
					t.Fatal(err)
				}
				roots, _ := json.Marshal(map[string]string{slug: "busy"})
				if _, err := session.Find(workspace, slug); err != nil {
					t.Fatal(err)
				}
				run := func(action string) error {
					return scopedProviderCommand(ctx, client, action, workspace, workspace, socket, workspace, authority, path, "openai", operation, slug, string(roots))
				}
				err := run("scoped-switch")
				if closeFault != "none" {
					if err == nil {
						t.Fatal("injected lost reply succeeded")
					}
					err = run("scoped-switch")
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := run("scoped-continue"); err != nil {
					t.Fatal(err)
				}
				if err := run("scoped-continue"); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				wantedCloses := 1
				if closeFault == "after-shutdown" {
					wantedCloses = 2
				}
				wantedContinuations := 0
				if goalEnd == "before-close" {
					wantedContinuations = 1
				}
				if continuations != wantedContinuations {
					t.Fatalf("continuations=%d, want %d", continuations, wantedContinuations)
				}
				if providers["busy"] != "openai" || providers["idle"] != "openai" || providers["unrelated"] != "pool" || providers["child"] != "pool" || closes["child"] != 0 || goalActive == goalFinishes || closes["busy"] != wantedCloses || closes["idle"] != 1 || archived["busy"] || archived["idle"] {
					t.Fatalf("providers=%v closes=%v archived=%v goal=%v", providers, closes, archived, goalActive)
				}
				var saved scopedProviderSnapshot
				data, _ := os.ReadFile(path)
				if json.Unmarshal(data, &saved) != nil || !saved.Intents["busy"].Continued || !saved.Intents["idle"].Continued {
					t.Fatal("continuation receipt missing")
				}
			})
		}
	}
}

// These doubles use the actual Client/Unix WebSocket request path, real runtime
// and team locks, real strict journal codec and real R1 collective constructor.
// The host callbacks themselves are tested separately by the Ruby writer suite.
type forkProviderFixture struct {
	t                             *testing.T
	root, authority, path         string
	source                        providerForkSnapshot
	state                         codexproviders.State
	catalog                       codexproviders.Catalog
	client                        *workspacecodex.Client
	callbacks                     providerForkCallbacks
	mu                            sync.Mutex
	forks                         map[string]int
	forkRequests                  int
	fault, busy, childReadFailure string
	reserved, firstAcknowledged   bool
	names                         map[string]string
}

func newForkProviderFixture(t *testing.T) *forkProviderFixture {
	t.Helper()
	f := &forkProviderFixture{t: t, root: t.TempDir(), forks: map[string]int{}, names: map[string]string{}}
	f.authority = filepath.Join(f.root, "authority")
	if err := os.MkdirAll(f.authority, 0700); err != nil {
		t.Fatal(err)
	}
	operation := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	f.path = filepath.Join(f.root, operation+".snapshot.json")
	f.catalog = codexproviders.Catalog{Schema: 1, InitialProvider: "openai", Digest: strings.Repeat("a", 64), Providers: []codexproviders.Provider{{ID: "openai", Label: "A", Kind: "subscription"}, {ID: "pool", Label: "B", Kind: "codex-lb", BaseURL: "http://fixture.invalid/v1", TokenFile: "/private/token"}}}
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
			id, _ := params["threadId"].(string)
			method, _ := request["method"].(string)
			f.mu.Lock()
			result := any(map[string]any{})
			var rpcError string
			base := strings.TrimSuffix(id, "-child")
			if id == "leaf" {
				base = "one"
			}
			cwd := filepath.Join(f.root, "work", base)
			provider := "openai"
			if id != base {
				provider = "pool"
			}
			if f.fault == "wrong-known-provider" && id == "one-child" {
				provider = "openai"
			}

			effective := func(thread string) map[string]any {
				approval := "never"
				if f.fault == "changed-policy" {
					approval = "on-request"
				}
				return map[string]any{"thread": map[string]any{"id": thread, "forkedFromId": base}, "cwd": cwd, "model": "fixture-model", "modelProvider": provider, "reasoningEffort": "low", "approvalPolicy": approval, "approvalsReviewer": "user", "sandbox": map[string]any{"type": "dangerFullAccess"}, "runtimeWorkspaceRoots": []any{f.root}, "activePermissionProfile": nil, "serviceTier": nil}
			}
			switch method {
			case "thread/read":
				if id == f.childReadFailure {
					rpcError = "synthetic known-child read failure"
					break
				}
				status := "idle"
				if id == f.busy {
					status = "active"
				}
				if id == "leaf" && f.fault == "descendant-busy" {
					status = "active"
				}
				source := any("vscode")
				if id == "leaf" {
					source = map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "one"}}}
					if f.fault == "descendant-unknown" {
						source = map[string]any{"subAgent": "unknown"}
					}
				}

				result = map[string]any{"thread": map[string]any{"id": id, "cwd": cwd, "source": source, "path": filepath.Join(f.root, id+".jsonl"), "status": map[string]any{"type": status}, "modelProvider": provider, "historyMode": "paginated", "name": "retained title", "forkedFromId": base}}
				if strings.HasPrefix(f.fault, "title") && strings.HasSuffix(id, "-child") {
					name := f.names[id]
					if name == "" {
						name = "different inherited title"
					}
					result.(map[string]any)["thread"].(map[string]any)["name"] = name
					if f.fault == "title-identity" && f.names[id] != "" {
						result.(map[string]any)["thread"].(map[string]any)["id"] = "foreign-child"
					}
					if f.names[id] != "" && f.fault == "title-name-only" {
						result = map[string]any{"thread": map[string]any{"name": name}}
					}
					if f.names[id] != "" && strings.HasPrefix(f.fault, "title-omit-") {
						delete(result.(map[string]any)["thread"].(map[string]any), strings.TrimPrefix(f.fault, "title-omit-"))
					}

				}

			case "thread/name/set":
				if params["name"] != "retained title" {
					f.mu.Unlock()
					return errors.New("wrong retained title")
				}
				f.names[id] = "retained title"
			case "thread/resume":
				if len(params) != 2 || params["excludeTurns"] != true {
					f.mu.Unlock()
					return fmt.Errorf("source observation repaired settings: %v", params)
				}
				result = effective(id)
				if id == "one" && strings.HasPrefix(f.fault, "omit-policy-") {
					delete(result.(map[string]any), strings.TrimPrefix(f.fault, "omit-policy-"))
				}

			case "thread/fork":
				f.forkRequests++
				if !f.reserved || !f.firstAcknowledged {
					f.mu.Unlock()
					return errors.New("native request preceded durable live acknowledgement")
				}
				approval := "never"
				if f.fault == "changed-policy" {
					approval = "on-request"
				}
				if params["modelProvider"] != "pool" || params["model"] != "fixture-model" || params["sandbox"] != "danger-full-access" || params["approvalPolicy"] != approval || params["excludeTurns"] != true {
					f.mu.Unlock()
					return errors.New("fork lost explicit retained fields")
				}
				config, _ := params["config"].(map[string]any)
				env := config["shell_environment_policy"].(map[string]any)["set"].(map[string]any)
				if env["DEV_SESSION_SLUG"] != id || config["model_reasoning_effort"] != "low" {
					f.mu.Unlock()
					return errors.New("fork lost frozen environment/effort")
				}
				f.forks[id]++
				if f.forks[id] != 1 {
					f.mu.Unlock()
					return errors.New("duplicate native fork")
				}
				provider = "pool"
				if f.fault == "guard-"+id {
					provider = "openai"
				}
				result = effective(id + "-child")
				if f.fault == "duplicate-child" && id == "two" {
					result.(map[string]any)["thread"].(map[string]any)["id"] = "one-child"
				}
				if f.fault == "alias-child" {
					result.(map[string]any)["thread"].(map[string]any)["id"] = "two"
				}
			case "thread/turns/list":
				result = map[string]any{"data": []any{map[string]any{"id": "retained-turn", "status": "completed"}}}
			case "thread/items/list":
				result = map[string]any{"data": []any{map[string]any{"id": "retained-item", "type": "userMessage"}}}
			case "thread/queue/list":
				result = map[string]any{"data": []any{}}
			case "thread/goal/get":
				result = map[string]any{"goal": nil}
			case "thread/loaded/list":
				ids := []any{"one", "two"}
				if strings.HasPrefix(f.fault, "descendant-") {
					ids = append(ids, "leaf")
				}
				result = map[string]any{"data": ids}
			default:
				f.mu.Unlock()
				return fmt.Errorf("unexpected fork transaction request %s", method)
			}
			f.mu.Unlock()
			reply := map[string]any{"id": request["id"], "result": result}
			if rpcError != "" {
				delete(reply, "result")
				reply["error"] = map[string]any{"code": -32000, "message": rpcError}
			}
			if err := threadCommandWriteObject(connection, reply); err != nil {
				return err
			}
		}
	})
	f.client = &workspacecodex.Client{Client: codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})}
	t.Cleanup(f.client.Close)
	identity, err := session.SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	o := &codexproviders.Operation{ID: operation, Target: "pool", CatalogDigest: f.catalog.Digest, Phase: "applying", Stage: "preparing", Strategy: "fork-v1", Sessions: []string{"one", "two"}, Roots: map[string]string{"one": "one", "two": "two"}, Snapshot: f.path, Blockers: []string{}, CreatedAt: "2026-11-25T00:00:00Z", UpdatedAt: "2026-11-25T00:00:00Z"}
	f.state = codexproviders.State{Schema: 3, Workspace: "work", Root: f.root, Socket: socket, Active: "openai", Sessions: map[string]codexproviders.Selection{}, BindingRevision: 1, Bindings: map[string]codexproviders.SessionBinding{}, Operation: o}
	f.source = providerForkSnapshot{Schema: 1, Strategy: "fork-v1", OperationID: operation, Workspace: "work", Root: f.root, Socket: socket, CatalogDigest: f.catalog.Digest, SourcePackage: "/nix/store/frozen-package", PreviousStateDigest: strings.Repeat("d", 64), Selection: codexproviders.SelectionChange{Scope: "workspace", Provider: "pool"}, Sessions: o.Sessions, SocketIdentity: identity, Rosters: map[string]providerForkRoster{}, Participants: []providerForkSource{}}
	for _, slug := range o.Sessions {
		cwd := filepath.Join(f.root, "work", slug)
		if err := os.MkdirAll(cwd, 0700); err != nil {
			t.Fatal(err)
		}
		manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.172.0\n", slug, slug, socket)
		for name, content := range map[string]string{"portal.yml": manifest, "state.md": "---\nlifecycle: active\n---\n", "plan.md": "Synthetic transaction fixture\n"} {
			if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		a := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: slug, Workspace: f.root, TmuxSocket: "/private/tmux", TmuxSessionID: "$1", TmuxIdentity: strings.Repeat("c", 64), CodexThreadID: slug, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
		if err := providerAtomicJSON(filepath.Join(f.authority, slug+".json"), a); err != nil {
			t.Fatal(err)
		}
		key := codexproviders.BindingKey{Workspace: f.root, Slug: slug, Incarnation: a.TmuxIdentity, RootThreadID: slug, Member: "lead", OriginalThreadID: slug}
		f.state.Bindings[slug] = codexproviders.SessionBinding{RootThreadID: slug, Incarnation: a.TmuxIdentity, Revision: 1, Conversations: map[string]codexproviders.ConversationBinding{slug: {Member: "lead", ThreadID: slug, Provider: "openai"}}}
		current := codexproviders.CurrentBinding{Key: key, BindingRequired: true, ExpectedRevision: 1, Provider: "openai", Socket: socket, Eligible: true}
		env := map[string]string{"DEV_SESSION_SLUG": slug, "DEV_SESSION_WORKSPACE": f.root, "DEV_SESSION_WORK_DIR": cwd, "DEV_SESSION_WORKTREES_DIR": filepath.Join(f.root, "worktrees", slug), "DEV_SESSION_CODEX_SOCKET": socket, "DEV_SESSION_REQUIRE_RUNTIME": "1"}
		for _, key := range []string{"DEV_SESSION_PORTAL_BASE_URL", "DEV_SESSION_URL", "DEV_SESSION_AUTHORITY_DIR", "DEV_SESSION_TMUX_SOCKET", "DEV_SESSION_CODEX", "DEV_SESSION_CODEX_VERSION", "DEV_SESSION_PORTAL_COMMAND", "DEV_WORKSPACE_CODEX_HOME"} {
			env[key] = "retained-" + key
		}
		participant := providerForkSource{Current: current, Thread: providerThread{ID: slug, Slug: slug, Cwd: cwd, Settings: codex.ThreadSettings{Model: "fixture-model", ReasoningEffort: "low"}, Environment: env}}
		if err := captureProviderForkThread(context.Background(), f.client, &participant, "openai"); err != nil {
			t.Fatal(err)
		}
		f.source.Participants = append(f.source.Participants, participant)
		f.source.Rosters[slug] = providerForkRoster{slug, a.TmuxIdentity, 0}
	}
	// JSON round trip avoids test-only pointer aliasing across source/current state.
	f.source.State = f.state
	data, _ := json.Marshal(f.source)
	if err := json.Unmarshal(data, &f.source); err != nil {
		t.Fatal(err)
	}
	if err := providerAtomicJSON(f.path, f.source); err != nil {
		t.Fatal(err)
	}
	f.callbacks = providerForkCallbacks{
		VerifyAuthority: func(context.Context, session.RuntimeAuthority) error { return nil },
		ReadState:       func() (codexproviders.State, error) { return f.state, nil },
		Reserve: func(ctx context.Context, path, digest string) error {
			f.assertLocks(ctx)
			journal := codexproviders.ForkJournal{Schema: 1, Strategy: "fork-v1", OperationID: operation, Workspace: "work", Root: f.root, Socket: socket, CatalogDigest: f.catalog.Digest, SourcePackage: f.source.SourcePackage, Selection: f.source.Selection, Sessions: o.Sessions, SourceSnapshotDigest: digest, ReservationRevision: 1, Stage: "preparing", Projections: map[string]codexproviders.ForkProjection{}}
			for _, p := range f.source.Participants {
				journal.Participants = append(journal.Participants, codexproviders.ForkParticipant{Current: p.Current, SourceThreadID: p.Thread.ID, State: "not-attempted"})
				journal.Projections[p.Current.Key.Slug] = codexproviders.ForkProjection{Manifest: true, Authority: true}
			}
			if err := writeProviderForkJournal(path+".journal", journal, f.source, f.catalog); err != nil {
				return err
			}
			f.reserved = true
			if f.fault == "reserve-lost" {
				return errors.New("lost reserve ack")
			}
			if f.fault == "drain-race" {
				f.busy = "one"
			}
			return nil
		},
		FirstIntent: func(ctx context.Context, path, digest string) error {
			f.assertLocks(ctx)
			j := f.journal()
			for _, p := range j.Participants {
				if p.State != "not-attempted" {
					return errors.New("existing first intent cannot authorize dispatch")
				}
			}
			j.Participants[0].State = "intent"
			j.Stage = "forking"
			if err := writeProviderForkJournal(path+".journal", j, f.source, f.catalog); err != nil {
				return err
			}
			f.state.Operation.Stage = "forking"
			if f.fault == "intent-lost" {
				return errors.New("lost first intent ack")
			}
			f.firstAcknowledged = true
			return nil
		},
		Commit: func(ctx context.Context, path, digest string) error {
			f.assertLocks(ctx)
			if f.state.Operation.Stage == "restoring-terminals" {
				j := f.journal()
				j.Stage = "restoring-terminals"
				if err := verifyProviderForkCommit(f.state, j, f.source, f.path, f.catalog); err != nil {
					return err
				}
				return writeProviderForkJournal(f.path+".journal", j, f.source, f.catalog)
			}
			if f.fault == "verified-crash" {
				return errors.New("crash after verified journal")
			}
			j := f.journal()
			f.state.Operation.Stage = "committing"
			j.Stage = "committing"
			if err := writeProviderForkJournal(f.path+".journal", j, f.source, f.catalog); err != nil {
				return err
			}
			evidence := codexproviders.BindingEvidence{Schema: 1, OperationID: operation, Workspace: "work", Root: f.root, Socket: socket, PreviousBindingRevision: f.state.BindingRevision, CommittedBindingRevision: f.state.BindingRevision + 1, SnapshotDigest: digest}
			var currents []codexproviders.CurrentBinding
			var updates []codexproviders.BindingUpdate
			for _, p := range j.Participants {
				currents = append(currents, p.Current)
				updates = append(updates, codexproviders.BindingUpdate{Key: p.Current.Key, ThreadID: p.ChildThreadID, Provider: "pool"})
				evidence.Participants = append(evidence.Participants, codexproviders.BindingParticipant{Key: p.Current.Key, OldThreadID: p.SourceThreadID, NewThreadID: p.ChildThreadID, OldProvider: p.Current.Provider, NewProvider: "pool"})
			}
			seal, _ := json.Marshal(evidence)
			if err := providerAtomicJSON(f.path+".evidence", evidence); err != nil {
				return err
			}
			next, err := f.state.PrepareBindingCommit(codexproviders.BindingCommit{ExpectedRevision: f.state.BindingRevision, Participants: currents, Updates: updates, Selection: j.Selection, Evidence: seal}, f.catalog)
			if err != nil {
				return err
			}
			next.Operation.Stage = "restoring-terminals"
			f.state = next
			if f.fault == "commit-state-crash" {
				return errors.New("state committed before journal acknowledgement")
			}
			j.Stage = "restoring-terminals"
			if err := writeProviderForkJournal(f.path+".journal", j, f.source, f.catalog); err != nil {
				return err
			}
			if f.fault == "commit-lost" {
				return errors.New("lost collective commit ack")
			}
			return nil
		},
	}
	return f
}

func (f *forkProviderFixture) assertLocks(ctx context.Context) {
	f.t.Helper()
	lock, err := session.LockRuntimeShared(f.authority, "one")
	if err == nil {
		lock.Close()
		f.t.Fatal("runtime lock escaped callback ownership")
	}
	store, err := teamruntime.NewStore(f.root, f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	release, err := store.LockOperation(blocked, "one")
	if err == nil {
		release()
		f.t.Fatal("team operation escaped callback ownership")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		f.t.Fatalf("competing team lock: %v", err)
	}
}
func (f *forkProviderFixture) journal() codexproviders.ForkJournal {
	f.t.Helper()
	data, err := os.ReadFile(f.path + ".journal")
	if err != nil {
		f.t.Fatal(err)
	}
	j, err := codexproviders.DecodeForkJournal(data, f.source.State, f.catalog)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}
func (f *forkProviderFixture) run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return runProviderFork(ctx, f.client, f.path, f.root, f.authority, f.catalog, f.callbacks)
}

func TestProviderForkTransaction(t *testing.T) {
	for _, fault := range []string{"none", "title", "title-identity", "title-name-only", "title-omit-id", "title-omit-cwd", "title-omit-modelProvider", "title-omit-forkedFromId", "title-omit-historyMode", "reserve-lost", "intent-lost", "drain-race", "guard-one", "guard-two", "read-one", "read-two", "verified-crash", "commit-lost", "commit-state-crash"} {
		t.Run(fault, func(t *testing.T) {
			f := newForkProviderFixture(t)
			f.fault = fault
			if strings.HasPrefix(fault, "read-") {
				f.childReadFailure = strings.TrimPrefix(fault, "read-") + "-child"
			}
			err := f.run()
			if fault == "none" || fault == "title" {
				if err != nil {
					t.Fatal(err)
				}
				if f.state.Active != "pool" || f.forks["one"] != 1 || f.forks["two"] != 1 {
					t.Fatalf("incomplete publication: %v", f.forks)
				}
				return
			}
			if err == nil {
				t.Fatal("fault succeeded")
			}
			before := map[string]int{}
			for k, v := range f.forks {
				before[k] = v
			}
			j := f.journal()
			switch {
			case strings.HasPrefix(fault, "title-"):
				if err.Error() != "fork title or metadata was not preserved" {
					t.Fatalf("wrong post-name refusal: %v", err)
				}
				if j.Participants[0].State != "known-child" || j.Participants[1].State != "not-attempted" || f.forks["one"] != 1 || f.forks["two"] != 0 || f.state.Operation.Stage == "restoring-terminals" {
					t.Fatal("incomplete post-name proof advanced verification/publication")
				}
			case fault == "reserve-lost" || fault == "drain-race":
				if len(before) != 0 {
					t.Fatal("native work before final admission")
				}
			case fault == "intent-lost" || strings.HasPrefix(fault, "guard-"):
				n := 0
				if fault == "guard-two" {
					n = 1
				}
				if j.Participants[n].State != "intent" {
					t.Fatal("unknown child outcome was not fenced")
				}
				f.fault = "none"
				if err := f.run(); err == nil {
					t.Fatal("unknown intent replayed")
				}
				if !reflect.DeepEqual(before, f.forks) {
					t.Fatal("unknown intent issued a second fork")
				}
			case strings.HasPrefix(fault, "read-") || fault == "verified-crash" || fault == "commit-lost" || fault == "commit-state-crash":
				if fault == "read-one" && j.Participants[0].State != "known-child" || fault == "read-two" && j.Participants[1].State != "known-child" {
					t.Fatal("returned child was not durable before reads")
				}
				f.fault = "none"
				f.childReadFailure = ""
				if err := f.run(); err != nil {
					t.Fatal(err)
				}
				for id, n := range f.forks {
					if n != 1 {
						t.Fatalf("fork replay %s=%d", id, n)
					}
				}
			}
			if fault != "commit-lost" && fault != "commit-state-crash" && fault != "read-one" && fault != "read-two" && fault != "verified-crash" && f.state.Active != "openai" {
				t.Fatal("failed transaction published selection")
			}
		})
	}
}

func TestProviderForkStrictPreconditions(t *testing.T) {
	for _, invalid := range []string{"missing-environment", "wrong-environment", "missing-runtime-proof", "incomplete-roster", "wrong-source", "wrong-catalog", "changed-history"} {
		t.Run(invalid, func(t *testing.T) {
			f := newForkProviderFixture(t)
			switch invalid {
			case "missing-environment":
				f.source.Participants[0].Thread.Environment = nil
			case "wrong-environment":
				f.source.Participants[0].Thread.Environment["DEV_SESSION_SLUG"] = "foreign"
			case "missing-runtime-proof":
				f.callbacks.VerifyAuthority = nil
			case "incomplete-roster":
				f.source.Participants = f.source.Participants[:1]
			case "wrong-source":
				f.source.Root = "/foreign"
			case "wrong-catalog":
				f.source.CatalogDigest = strings.Repeat("f", 64)
			case "changed-history":
				f.source.Participants[0].History.Items = []json.RawMessage{json.RawMessage(`{"id":"changed"}`)}
			}
			if err := providerAtomicJSON(f.path, f.source); err != nil {
				t.Fatal(err)
			}
			if err := f.run(); err == nil {
				t.Fatal("invalid precondition admitted")
			}
			if len(f.forks) != 0 {
				t.Fatal("native fork occurred after invalid precondition")
			}
		})
	}
}

func TestProviderForkRejectsKnownWrongChildAndDescendantWork(t *testing.T) {
	for _, fault := range []string{"alias-child", "wrong-known-provider", "descendant-busy", "descendant-unknown"} {
		t.Run(fault, func(t *testing.T) {
			f := newForkProviderFixture(t)
			if fault == "wrong-known-provider" {
				f.childReadFailure = "one-child"
				if err := f.run(); err == nil {
					t.Fatal("lost known-child read succeeded")
				}
				f.childReadFailure = ""
			}
			f.fault = fault
			if err := f.run(); err == nil {
				t.Fatal("invalid child/descendant admitted")
			}
			if f.state.Active != "openai" {
				t.Fatal("invalid child/descendant changed routing")
			}
			if strings.HasPrefix(fault, "descendant-") {
				if len(f.forks) != 0 {
					t.Fatal("fork issued before descendant drain")
				}
			} else {
				if f.journal().Participants[0].State != "known-child" {
					t.Fatal("exact rejected child was discarded")
				}
				if f.forks["one"] != 1 || f.forks["two"] != 0 {
					t.Fatal("invalid child was retried or later participants forked")
				}
			}
		})
	}
}

func TestProviderForkRetainsDuplicateKnownReplyBeforeRefusal(t *testing.T) {
	f := newForkProviderFixture(t)
	f.fault = "duplicate-child"
	if err := f.run(); err == nil {
		t.Fatal("duplicate child committed")
	}
	j := f.journal()
	if j.Participants[0].State != "verified" || j.Participants[1].State != "known-child" || j.Participants[1].ChildThreadID != "one-child" {
		t.Fatal("exact duplicate reply was not durably retained")
	}
	if f.state.Active != "openai" || f.forks["one"] != 1 || f.forks["two"] != 1 {
		t.Fatal("duplicate reply changed routing or fork counts")
	}
}

func TestProviderForkRejectsPolicyObservationDrift(t *testing.T) {
	for _, fault := range []string{"changed-policy", "omit-policy-approvalPolicy", "omit-policy-approvalsReviewer", "omit-policy-sandbox", "omit-policy-runtimeWorkspaceRoots"} {
		t.Run(fault, func(t *testing.T) {
			f := newForkProviderFixture(t)
			f.fault = fault
			frozen, err := json.Marshal(f.source.Participants)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			err = withProviderForkLocks(ctx, f.authority, f.root, f.root, f.source.Sessions, func() error {
				return recheckProviderForkScope(ctx, f.client, f.source, f.root, f.authority, f.callbacks.VerifyAuthority)
			})
			expected := "fork effective policy is incomplete"
			if fault == "changed-policy" {
				expected = "fork source effective settings changed"
			}
			if err == nil || err.Error() != expected {
				t.Errorf("policy observation error=%v; want %q", err, expected)
			}
			after, err := json.Marshal(f.source.Participants)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(frozen) {
				t.Error("recapture mutated frozen participant settings")
			}
			if err := f.run(); err == nil || err.Error() != expected {
				t.Errorf("transaction admitted changed/incomplete policy: %v", err)
			}
			if f.forkRequests != 0 || len(f.forks) != 0 {
				t.Errorf("policy drift issued %d fork requests", f.forkRequests)
			}
			if f.state.Active != "openai" {
				t.Error("policy drift published routing")
			}
		})
	}
}
