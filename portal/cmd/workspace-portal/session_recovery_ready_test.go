package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/coder/websocket"
)

func TestBoundRecoveryCommandSeparatesOriginalRecordAndCurrentGrant(t *testing.T) {
	workspace, stateRoot := t.TempDir(), t.TempDir()
	runtime, err := os.MkdirTemp("/tmp", "current-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(runtime)
	socket := filepath.Join(runtime, "registered", "app-server.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	authorityDir := filepath.Join(filepath.Dir(socket), "authority")
	if err := os.MkdirAll(authorityDir, 0700); err != nil {
		t.Fatal(err)
	}
	slug, origin, child, inc, op := "bound", "origin-root", "current-child", strings.Repeat("a", 64), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	cwd := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"portal.yml": fmt.Sprintf("schema: 6\nslug: bound\nrepositories: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\n", origin, socket), "plan.md": "Current recovery command control\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalog)
	evidence := codexproviders.BindingEvidence{Schema: 1, OperationID: op, Workspace: "registered", Root: workspace, Socket: socket, PreviousBindingRevision: 1, CommittedBindingRevision: 2, SnapshotDigest: strings.Repeat("e", 64), Participants: []codexproviders.BindingParticipant{{Key: codexproviders.BindingKey{Workspace: workspace, Slug: slug, RootThreadID: origin, Incarnation: inc, Member: "lead", OriginalThreadID: origin}, OldThreadID: origin, NewThreadID: child, OldProvider: "openai", NewProvider: "openai"}}}
	sealed, _ := json.Marshal(evidence)
	sum := sha256.Sum256(sealed)
	state := codexproviders.State{Schema: 3, Workspace: "registered", Root: workspace, Socket: socket, Active: "openai", BindingRevision: 2, Bindings: map[string]codexproviders.SessionBinding{slug: {RootThreadID: origin, Incarnation: inc, Revision: 2, Conversations: map[string]codexproviders.ConversationBinding{origin: {Member: "lead", ThreadID: child, Provider: "openai", OperationID: op, EvidenceDigest: hex.EncodeToString(sum[:])}}}}}
	authority := session.RuntimeAuthority{Schema: 3, State: "ready", Slug: slug, Workspace: workspace, TmuxSocket: "/run/tmux.sock", TmuxSessionID: "$1", TmuxIdentity: inc, CodexThreadID: origin, CodexActiveThreadID: child, CodexBindingRevision: 2, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
	statePath, err := codexproviders.StatePath(stateRoot, "registered")
	if err != nil {
		t.Fatal(err)
	}
	authorityPath := filepath.Join(authorityDir, slug+".json")
	for path, value := range map[string]any{statePath: state, authorityPath: authority} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(value)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(statePath), op+".snapshot.json.evidence"), sealed, 0600); err != nil {
		t.Fatal(err)
	}
	args := func(action, target string) []string {
		return []string{"--workspace", workspace, "--user-state-root", stateRoot, "--slug", slug, "--socket", socket, "--thread-id", target, "--operation", action}
	}
	if err := recoveryCommand(args("running", child)); err != nil {
		t.Fatal(err)
	}
	store := session.RecoveryStore{Root: stateRoot, Workspace: workspace}
	record, err := store.Load(slug)
	epoch, e := session.SocketIdentity(socket)
	if err != nil || e != nil || record.ThreadID != origin || !record.Automatic || !record.Active(child, epoch) || record.Active(origin, epoch) {
		t.Fatal("running rewrote original identity or grants", record, err, e)
	}
	stdout := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	statusErr := recoveryCommand(args("status", child))
	write.Close()
	os.Stdout = stdout
	output, _ := io.ReadAll(read)
	read.Close()
	if statusErr != nil || !strings.Contains(string(output), `"held":false`) {
		t.Fatal("current status refused", statusErr, string(output))
	}
	for _, target := range []string{origin, "wrong-member"} {
		if err := recoveryCommand(args("running", target)); err == nil {
			t.Fatal("noncurrent grant accepted", target)
		}
	}
	authority.CodexBindingRevision = 1
	b, _ := json.Marshal(authority)
	if err := os.WriteFile(authorityPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoveryCommand(args("running", child)); err == nil {
		t.Fatal("stale projection granted execution")
	}
	if err := os.Remove(authorityPath); err != nil {
		t.Fatal(err)
	}
	if err := recoveryCommand(args("archive", origin)); err != nil {
		t.Fatal("original retirement required live authority", err)
	}
	record, err = store.Load(slug)
	if err != nil || record.ThreadID != origin || record.Automatic || len(record.ActiveThreads) != 0 {
		t.Fatal("original retirement changed root or retained grants", record, err)
	}
	if err := recoveryCommand(args("restore", child)); err != nil {
		t.Fatal("sealed current restore required live authority", err)
	}
	if err := recoveryCommand(args("restore", origin)); err == nil {
		t.Fatal("restore admitted original ancestor")
	}
	record, err = store.Load(slug)
	if err != nil || record.ThreadID != origin || !record.Automatic || len(record.ActiveThreads) != 0 {
		t.Fatal("restore activated or rewrote origin", record, err)
	}
	if err := store.Update(context.Background(), slug, func(r *session.Recovery) error {
		r.ActiveThreads = []string{child}
		r.Continuation = &session.RecoveryContinuation{RequestID: "saved-request", Prompt: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := recoveryCommand(args("restore", child)); err != nil {
		t.Fatal(err)
	}
	record, err = store.Load(slug)
	if err != nil || len(record.ActiveThreads) != 1 || record.ActiveThreads[0] != child || record.Continuation == nil || record.Continuation.RequestID != "saved-request" {
		t.Fatal("restore changed same-epoch grants or original input", record, err)
	}
	legacyArgs := args("running", "legacy-root")
	for i := range legacyArgs {
		if legacyArgs[i] == slug {
			legacyArgs[i] = "legacy"
		}
	}
	if err := recoveryCommand(legacyArgs); err != nil {
		t.Fatal("legacy command changed", err)
	}
}

func TestBoundRetainedRootCommandKeepsExactCurrentAndOriginalProvenance(t *testing.T) {
	for _, scenario := range []string{"resume", "materialization", "archived", "unknown-root", "duplicate-page", "cursor", "missing-history", "busy-ancestor", "stale-authority", "wrong-start", "ancestor", "wrong-result", "lost-unarchive", "capability-off"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, stateRoot, home, pkg := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			const slug = "bound"
			ids := []string{"00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000002", "00000000-0000-7000-8000-000000000003"}
			origin, current, inc := ids[0], ids[2], strings.Repeat("a", 64)
			cwd := filepath.Join(workspace, "work", slug)
			archived := scenario != "resume" && scenario != "materialization"
			unarchived := false
			resumes, unarchives := 0, 0
			paths := map[string]string{}
			for _, id := range ids {
				area := "sessions"
				if id == current && archived {
					area = "archived_sessions"
				}
				path := filepath.Join(home, area, "rollout-2026-11-25T12-00-00-"+id+".jsonl")
				paths[id] = path
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n", id, cwd)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			metadata := func(id string) map[string]any {
				path := paths[id]
				if id == current && unarchived {
					path = filepath.Join(home, "sessions", filepath.Base(path))
				}
				return map[string]any{"id": id, "cwd": cwd, "source": "vscode", "path": path, "ephemeral": false, "model": "retained-model", "reasoningEffort": "high", "status": map[string]any{"type": "notLoaded"}, "turns": []any{}}
			}
			socket := threadCommandAppServer(t, func(conn *websocket.Conn) error {
				if err := threadCommandHandshake(conn); err != nil {
					return err
				}
				for {
					req, err := threadCommandReadObject(conn)
					if err != nil {
						return nil
					}
					p, _ := req["params"].(map[string]any)
					id, _ := p["threadId"].(string)
					result := map[string]any{}
					switch req["method"] {
					case "thread/read":
						if _, ok := paths[id]; !ok {
							return fmt.Errorf("foreign retained read %s", id)
						}
						result["thread"] = metadata(id)
					case "thread/list":
						providers, ok := p["modelProviders"].([]any)
						if !ok || len(providers) != 0 {
							return fmt.Errorf("missing all-provider discovery: %#v", p)
						}
						rows := []any{}
						wantArchived, _ := p["archived"].(bool)
						if wantArchived {
							if archived && !unarchived {
								rows = append(rows, metadata(current))
							}
						} else {
							if p["cursor"] == "page2" {
								rows = append(rows, metadata(ids[1]))
								if !archived || unarchived {
									rows = append(rows, metadata(current))
								}
							} else {
								rows = append(rows, metadata(origin))
								result["nextCursor"] = "page2"
							}
							if scenario == "unknown-root" {
								rows = append(rows, map[string]any{"id": "foreign-root", "cwd": cwd, "source": "vscode", "ephemeral": false})
							}
							if scenario == "duplicate-page" && p["cursor"] == "page2" {
								rows = append(rows, metadata(origin))
							}
							if scenario == "cursor" {
								result["nextCursor"] = "page2"
							}
							if scenario == "missing-history" && p["cursor"] == "page2" {
								rows = []any{}
							}
						}
						result["data"] = rows
					case "thread/loaded/list":
						result["data"] = []any{origin}
					case "thread/turns/list":
						result["data"] = []any{}
						if scenario == "busy-ancestor" && id == origin {
							result["data"] = []any{map[string]any{"id": "busy-turn", "status": "inProgress"}}
						}
					case "thread/queue/list":
						result["data"] = []any{}
					case "thread/unarchive":
						unarchives++
						if id != current {
							return fmt.Errorf("ancestor unarchive: %q", id)
						}
						if scenario == "lost-unarchive" {
							conn.CloseNow()
							return nil
						}
						unarchived = true
						result["thread"] = metadata(current)
					case "thread/resume":
						resumes++
						if id != current || p["cwd"] != cwd || p["modelProvider"] != "openai" {
							return fmt.Errorf("wrong retained resume: %#v", p)
						}
						returned := current
						if scenario == "wrong-result" {
							returned = origin
						}
						result["thread"] = map[string]any{"id": returned, "cwd": cwd}
						result["modelProvider"] = "openai"
					default:
						return fmt.Errorf("unexpected retained operation %v", req["method"])
					}
					if err := threadCommandWriteObject(conn, map[string]any{"id": req["id"], "result": result}); err != nil {
						return nil
					}
				}
			})
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf("schema: 6\nslug: bound\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\n", origin, socket)
			for name, body := range map[string]string{"portal.yml": manifest, "plan.md": "Retained bound root\n", "state.md": "---\nlifecycle: active\n---\n"} {
				if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			catalog := filepath.Join(stateRoot, "catalog.json")
			if err := os.WriteFile(catalog, []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"},{"id":"pool","label":"Pool","kind":"codex-lb","baseURL":"https://pool.test/v1","tokenFile":"/token"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalog)
			t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
			t.Setenv("DEV_WORKSPACE_NAME", filepath.Base(filepath.Dir(socket)))
			name := filepath.Base(filepath.Dir(socket))
			statePath, _ := codexproviders.StatePath(stateRoot, name)
			if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
				t.Fatal(err)
			}
			key := codexproviders.BindingKey{Workspace: workspace, Slug: slug, RootThreadID: origin, OriginalThreadID: origin, Member: "lead", Incarnation: inc}
			previous, digest := "", ""
			for i, op := range []string{"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "dddddddd-dddd-4ddd-8ddd-dddddddddddd"} {
				oldProvider, newProvider := "openai", "pool"
				if i == 1 {
					oldProvider, newProvider = newProvider, oldProvider
				}
				e := codexproviders.BindingEvidence{Schema: 1, OperationID: op, Workspace: name, Root: workspace, Socket: socket, PreviousBindingRevision: uint64(i + 1), CommittedBindingRevision: uint64(i + 2), SnapshotDigest: strings.Repeat("e", 64), Participants: []codexproviders.BindingParticipant{{Key: key, OldThreadID: ids[i], NewThreadID: ids[i+1], OldProvider: oldProvider, NewProvider: newProvider, PreviousOperationID: previous, PreviousEvidenceDigest: digest}}}
				b, _ := json.Marshal(e)
				sum := sha256.Sum256(b)
				digest = hex.EncodeToString(sum[:])
				previous = op
				if err := os.WriteFile(filepath.Join(filepath.Dir(statePath), op+".snapshot.json.evidence"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			state := codexproviders.State{Schema: 3, Workspace: name, Root: workspace, Socket: socket, Active: "openai", BindingRevision: 3, Bindings: map[string]codexproviders.SessionBinding{slug: {RootThreadID: origin, Incarnation: inc, Revision: 3, Conversations: map[string]codexproviders.ConversationBinding{origin: {Member: "lead", ThreadID: current, Provider: "openai", OperationID: previous, EvidenceDigest: digest}}}}}
			b, _ := json.Marshal(state)
			if err := os.WriteFile(statePath, b, 0600); err != nil {
				t.Fatal(err)
			}
			authorityDir := filepath.Join(filepath.Dir(socket), "authority")
			if err := os.MkdirAll(authorityDir, 0700); err != nil {
				t.Fatal(err)
			}
			if scenario == "stale-authority" {
				a := session.RuntimeAuthority{Schema: 3, State: "ready", Slug: slug, Workspace: workspace, TmuxSocket: "/run/tmux.sock", TmuxSessionID: "$1", TmuxIdentity: inc, CodexThreadID: origin, CodexActiveThreadID: current, CodexBindingRevision: 2, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
				b, _ := json.Marshal(a)
				if err := os.WriteFile(filepath.Join(authorityDir, slug+".json"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			lockDir := filepath.Join(workspace, "worktrees", ".locks")
			if err := os.MkdirAll(lockDir, 0700); err != nil {
				t.Fatal(err)
			}
			token := inc
			if scenario == "wrong-start" {
				token = strings.Repeat("f", 64)
			}
			journal, _ := json.Marshal(map[string]any{"schema": 1, "slug": slug, "state": "creating", "tmux_identity": token})
			startPath := filepath.Join(lockDir, slug+".start.json")
			if err := os.WriteFile(startPath, journal, 0600); err != nil {
				t.Fatal(err)
			}
			portal := filepath.Join(pkg, "bin", "workspace-portal")
			contractPath := filepath.Join(pkg, "share", "workspace-portal", "runtime-contract.json")
			for path, body := range map[string]string{portal: "fixture command owner", contractPath: `{"codexProviderBindingStateSchema":3,"codexProviderForkTransitionPolicy":1,"codexRuntimeAuthoritySchema":3}`} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(pkg, filepath.Join(stateRoot, "profile")); err != nil {
				t.Fatal(err)
			}
			if scenario == "capability-off" {
				if err := os.WriteFile(contractPath, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			runtimeLock, err := session.LockRuntimeExclusiveContext(context.Background(), authorityDir, slug)
			if err != nil {
				t.Fatal(err)
			}
			defer runtimeLock.Close()
			teams, err := teamruntime.NewStore(stateRoot, workspace)
			if err != nil {
				t.Fatal(err)
			}
			release, err := teams.LockOperation(context.Background(), slug)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			var callErr error
			if scenario == "materialization" {
				callErr = sessionCommand([]string{"require-recovery-ready", "--workspace", workspace, "--session-slug", slug, "--socket", socket, "--user-state-root", stateRoot, "--authority-dir", authorityDir, "--codex-home", home, "--expected-start-tmux-identity", inc})
			} else {
				requested := current
				if scenario == "ancestor" {
					requested = origin
				}
				args := []string{"create", "--require-runtime", "--workspace", workspace, "--session-slug", slug, "--cwd", cwd, "--worktrees-dir", filepath.Join(workspace, "worktrees", slug), "--portal-base-url", "https://workspace.test", "--portal-url", "https://workspace.test/bound", "--portal-command", portal, "--authority-dir", authorityDir, "--tmux-socket", "/run/tmux.sock", "--codex-command", "/fixture/codex", "--codex-version", "0.172.0", "--socket", socket, "--user-state-root", stateRoot, "--thread-id", requested}
				if archived {
					args = append(args, "--recover-archived")
				}
				saved := os.Stdout
				r, w, _ := os.Pipe()
				os.Stdout = w
				callErr = threadCommand(args)
				w.Close()
				os.Stdout = saved
				output, _ := io.ReadAll(r)
				r.Close()
				if (scenario == "resume" || scenario == "archived") && (callErr != nil || !strings.Contains(string(output), current)) {
					t.Fatal("wrong command result", string(output), callErr)
				}
			}
			success := scenario == "resume" || scenario == "archived" || scenario == "materialization"
			if success != (callErr == nil) {
				t.Fatalf("scenario %s: %v", scenario, callErr)
			}
			expectedResume, expectedUnarchive := 0, 0
			if scenario == "resume" || scenario == "archived" || scenario == "wrong-result" {
				expectedResume = 1
			}
			if scenario == "archived" || scenario == "wrong-result" || scenario == "lost-unarchive" {
				expectedUnarchive = 1
			}
			if resumes != expectedResume || unarchives != expectedUnarchive {
				t.Fatalf("effects resume=%d unarchive=%d, want %d/%d (err %v)", resumes, unarchives, expectedResume, expectedUnarchive, callErr)
			}
			after, _ := os.ReadFile(filepath.Join(cwd, "portal.yml"))
			if string(after) != manifest {
				t.Fatal("native recovery rewrote original manifest")
			}
			after, _ = os.ReadFile(startPath)
			if string(after) != string(journal) {
				t.Fatal("native recovery rewrote accepted start nonce")
			}
		})
	}
}

func TestTerminalRecoveryKeepsUnresolvedSendsAndDoesNotDiscoverOrActivateNeighbors(t *testing.T) {
	for _, scenario := range []string{"ready", "wrong cwd", "wrong source", "missing rollout", "lifecycle", "authority", "created start", "changed start", "unfinished creation"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, stateRoot, home, authority := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.Chmod(authority, 0700); err != nil {
				t.Fatal(err)
			}
			const slug = "retained"
			const thread = "00000000-0000-7000-8000-000000000001"
			directory := filepath.Join(workspace, "work", slug)
			rollout := filepath.Join(home, "sessions", "rollout-2026-10-30T12-00-00-"+thread+".jsonl")
			for _, p := range []string{directory, filepath.Dir(rollout), filepath.Join(home, "archived_sessions")} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "missing rollout" {
				if err := os.WriteFile(rollout, []byte("retained history\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
				defer connection.CloseNow()
				if err := threadCommandHandshake(connection); err != nil {
					return err
				}
				for {
					request, err := threadCommandReadObject(connection)
					if err != nil {
						return nil
					}
					if request["method"] != "thread/read" {
						return fmt.Errorf("terminal restoration attempted %v", request["method"])
					}
					params := request["params"].(map[string]any)
					if params["threadId"] != thread {
						return fmt.Errorf("restoration read an unretained thread")
					}
					cwd, source := directory, "vscode"
					if scenario == "wrong cwd" {
						cwd += "-foreign"
					}
					if scenario == "wrong source" {
						source = "exec"
					}
					result := map[string]any{"thread": map[string]any{"id": thread, "cwd": cwd, "source": source, "path": rollout, "status": map[string]any{"type": "notLoaded"}}}
					if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
						return err
					}
				}
			})
			manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.165.0\n", slug, thread, socket)
			if scenario == "created start" || scenario == "changed start" {
				manifest += "creation:\n  state: ready\n"
			}
			if scenario == "unfinished creation" {
				manifest += "creation:\n  state: creating\n"
			}
			for name, body := range map[string]string{"plan.md": "# Retained work\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": manifest} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "lifecycle" {
				dir := filepath.Join(workspace, "worktrees", ".locks")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, slug+".archive-cleanup.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "authority" {
				if err := os.WriteFile(filepath.Join(authority, slug+".json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
			client := newCodexClient(socket, workspace, stateRoot)
			defer client.Close()
			const text = "Preserve this original pending input."
			if err := client.PrepareSend(thread, text, "pending-input", "", false); err != nil {
				t.Fatal(err)
			}
			before, found, err := client.OriginalSendOptions(thread, text, "pending-input", "")
			if err != nil || !found {
				t.Fatal("missing pending preparation", err)
			}
			args := []string{"require-recovery-ready", "--workspace", workspace, "--session-slug", slug, "--socket", socket, "--user-state-root", stateRoot, "--authority-dir", authority, "--codex-home", home}
			if scenario == "created start" || scenario == "changed start" {
				dir := filepath.Join(workspace, "worktrees", ".locks")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				token := strings.Repeat("a", 64)
				data, _ := json.Marshal(map[string]any{"schema": 1, "slug": slug, "state": "creating", "tmux_identity": token})
				if err := os.WriteFile(filepath.Join(dir, slug+".start.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "changed start" {
					token = strings.Repeat("b", 64)
				}
				args = append(args, "--expected-start-tmux-identity", token)
			}
			err = sessionCommand(args)
			if (err == nil) != (scenario == "ready" || scenario == "created start") {
				t.Fatalf("recovery result: %v", err)
			}
			after, found, readErr := client.OriginalSendOptions(thread, text, "pending-input", "")
			beforeJSON, _ := json.Marshal(before)
			afterJSON, _ := json.Marshal(after)
			if readErr != nil || !found || string(beforeJSON) != string(afterJSON) {
				t.Fatal("restoration changed pending input", readErr)
			}
			if err := client.RequireSubmissionAttemptsKnown(context.Background(), thread); err == nil {
				t.Fatal("restoration erased unresolved-send refusal")
			}
		})
	}
}
