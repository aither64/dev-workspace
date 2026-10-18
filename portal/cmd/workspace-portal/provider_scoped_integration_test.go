//go:build live_switch_integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// Real selected native protocol, disposable histories and loopback model
// responses. No live workspace socket, subscription or model inference is used.
func TestNativeScopedProviderSwitch(t *testing.T) {
	native := os.Getenv("LIVE_SWITCH_CODEX")
	if !filepath.IsAbs(native) {
		t.Fatal("LIVE_SWITCH_CODEX must select the exact native executable")
	}
	root, err := os.MkdirTemp("", "provider-scoped-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var mu sync.Mutex
	requests := map[string]int{}
	threadRequests := map[string]map[string]int{}
	spawned := map[string]bool{}
	oldChildCanceled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5.5"}]}`)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(503)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var input struct {
			Input []struct {
				Type string `json:"type"`
			} `json:"input"`
		}
		_ = json.Unmarshal(body, &input)
		childRequest := false
		for _, item := range input.Input {
			childRequest = childRequest || item.Type == "agent_message"
		}
		mu.Lock()
		requests[r.URL.Path]++
		threadID := r.Header.Get("x-client-request-id")
		if threadRequests[threadID] == nil {
			threadRequests[threadID] = map[string]int{}
		}
		threadRequests[threadID][r.URL.Path]++
		spawnMarker := ""
		for _, marker := range []string{"spawn-old-child", "spawn-new-child"} {
			if !childRequest && strings.Contains(string(body), marker) && !spawned[marker] {
				spawned[marker], spawnMarker = true, marker
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(value any) {
			data, _ := json.Marshal(value)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		event(map[string]any{"type": "response.created", "response": map[string]any{"id": "fixture"}})
		if spawnMarker != "" {
			arguments, _ := json.Marshal(map[string]any{"task_name": strings.ReplaceAll(spawnMarker, "-", "_"), "message": "native temporary child work", "fork_turns": "none"})
			event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "call_id": spawnMarker, "namespace": "collaboration", "name": "spawn_agent", "arguments": string(arguments), "encrypted_function_args": []any{}}})
			event(map[string]any{"type": "response.completed", "response": map[string]any{"id": "fixture"}})
			return
		}
		if childRequest {
			<-r.Context().Done()
			if strings.Contains(r.URL.Path, "/a/") {
				mu.Lock()
				oldChildCanceled = true
				mu.Unlock()
			}
			return
		}
		if strings.Contains(string(body), "scoped-block") || strings.Contains(string(body), "unrelated-block") || strings.Contains(r.URL.Path, "/b/") && !strings.Contains(string(body), "fork source proof") {
			<-r.Context().Done()
			return
		}
		event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "answer", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "scoped-fixture-complete"}}}})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": "fixture", "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer backend.Close()
	home, stateRoot, authority := filepath.Join(root, "home"), filepath.Join(root, "state"), filepath.Join(root, "authority")
	for _, path := range []string{home, stateRoot, authority} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := "model_provider = \"pool-a\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\napproval_policy = \"never\"\n[features]\nmulti_agent_v2 = true\n"
	for _, provider := range []string{"a", "b", "c"} {
		config += "[model_providers.pool-" + provider + "]\nname = \"Fixture " + provider + "\"\nwire_api = \"responses\"\nsupports_websockets = false\nenv_key = \"OPENAI_API_KEY\"\nbase_url = " + strconv.Quote(backend.URL+"/"+provider+"/v1") + "\n"
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// Inventory reads the selected package's execution policy. This fixture
	// uses an unmanaged package and preserves the member's read-only policy.
	metadataDir := filepath.Join(root, "share", "dev-workspace")
	if err := os.MkdirAll(metadataDir, 0700); err != nil {
		t.Fatal(err)
	}
	metadata := `{"schema":1,"activationEnvironmentAliases":["DEV_WORKSPACES"],"routerSocket":"/run/fixture/router.sock","userNamespace":"fixture","agent_teams":{"managed":false}}`
	if err := os.WriteFile(filepath.Join(metadataDir, "package.json"), []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "app.sock")
	environment := []string{"HOME=" + root, "CODEX_HOME=" + home, "PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=synthetic-only"}
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	daemon := exec.CommandContext(ctx, native, "app-server", "--listen", "unix://"+socket)
	daemon.Env, daemon.Dir, daemon.Stdout, daemon.Stderr = environment, root, log, log
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL); _ = daemon.Wait() }()
	wait := func(label string, condition func() bool) {
		t.Helper()
		for !condition() {
			select {
			case <-ctx.Done():
				data, _ := os.ReadFile(log.Name())
				t.Fatalf("%s: %v %s", label, ctx.Err(), data)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	wait("native socket", func() bool { _, err := os.Stat(socket); return err == nil })
	client := codex.NewWithOptions(socket, codex.ClientOptions{ClientInfo: codex.ClientInfo{Name: "dev-workspace", Version: "0.1.0"}, PreserveThreadInstructionsOnResume: true})
	defer client.Close()
	threads := map[string]string{}
	for _, slug := range []string{"busy", "idle", "plain", "goal", "unrelated"} {
		cwd := filepath.Join(root, "work", slug)
		if err := os.MkdirAll(cwd, 0700); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{"state.md": "---\nlifecycle: active\n---\n", "plan.md": "Scoped switch fixture\n"} {
			if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		id, err := client.StartThreadWithSettings(ctx, cwd, map[string]string{}, codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low", Policy: codex.ThreadPolicy{Sandbox: "read-only"}})
		if err != nil {
			t.Fatal(err)
		}
		threads[slug] = id
		manifest := fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.163.0\ncreation:\n  state: ready\n", slug, id, socket)
		if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		text := "seed-idle"
		if slug == "busy" || slug == "plain" || slug == "goal" {
			text = "scoped-block"
		}
		if slug == "busy" {
			text += " spawn-old-child"
		}
		if slug == "unrelated" {
			text = "unrelated-block"
		}
		var response any
		if err := client.Request(ctx, "turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": text}}}, &response); err != nil {
			t.Fatal(err)
		}
		if slug == "idle" {
			wait("idle materialization", func() bool { return client.RequireThreadTurnsIdle(ctx, id) == nil })
		}
	}
	// A retained team member is an independent root with the same session cwd.
	// Its idle state must not turn it into a temporary-subagent exception.
	memberCwd := filepath.Join(root, "work", "busy")
	member, err := client.StartThreadWithSettings(ctx, memberCwd, map[string]string{"DEV_SESSION_MEMBER": "reviewer0"}, codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low", Policy: codex.ThreadPolicy{Sandbox: "read-only", DeveloperInstructions: "Retained member fixture."}})
	if err != nil {
		t.Fatal(err)
	}
	threads["member"] = member
	var seed any
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": member, "input": []any{map[string]any{"type": "text", "text": "seed-idle"}}}, &seed); err != nil {
		t.Fatal(err)
	}
	wait("retained member idle", func() bool { return client.RequireThreadTurnsIdle(ctx, member) == nil })
	store, err := teamruntime.NewStore(stateRoot, root)
	if err != nil {
		t.Fatal(err)
	}
	roster, err := store.Update(ctx, "busy", threads["busy"], true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{{Address: "reviewer0", Role: "reviewer", Thread: member, Model: "gpt-5.5", Effort: "low", State: "ready", AddedAt: time.Now().UTC(), Behavior: "reviewer", Purpose: "review", Access: "read_only", Instructions: "Retained member fixture."}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rosterBefore, _ := json.Marshal(roster)
	child := ""
	wait("native v2 child", func() bool {
		var rows struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := client.Request(ctx, "thread/list", map[string]any{"ancestorThreadId": threads["busy"], "archived": false, "modelProviders": []string{}, "sourceKinds": workspacecodex.ArchiveDiscoverySourceKinds()}, &rows); err != nil {
			return false
		}
		if len(rows.Data) != 1 {
			return false
		}
		child = rows.Data[0].ID
		active, err := client.ActiveTurnID(ctx, child)
		return err == nil && active != ""
	})
	var childMetadata map[string]any
	if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": child, "excludeTurns": true}, &childMetadata); err != nil {
		t.Fatal(err)
	}
	if childMetadata["thread"].(map[string]any)["canAcceptDirectInput"] != false {
		t.Fatal("fixture did not create a parent-owned v2 child")
	}
	var response any
	for _, goalSlug := range []string{"busy", "goal"} {
		if err := client.Request(ctx, "thread/goal/set", map[string]any{"threadId": threads[goalSlug], "objective": "Keep the scoped provider fixture running.", "status": "active", "origin": "user"}, &response); err != nil {
			t.Fatal(err)
		}
	}
	queuedID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if err := client.Request(ctx, "thread/queue/add", map[string]any{"threadId": threads["busy"], "clientUserMessageId": queuedID, "input": []any{map[string]any{"type": "text", "text": "queued fixture input"}}}, &response); err != nil {
		t.Fatal(err)
	}
	queueBefore, err := client.ListQueue(ctx, threads["busy"])
	if err != nil || len(queueBefore) != 1 {
		t.Fatalf("queue before: %v %v", queueBefore, err)
	}
	// Reproduce the failed idle thread using native settings writes. The old
	// turn is read-only; newer snapshots carry workspace-write permissions,
	// duplicate configured roots and separately saved runtime workspace roots.
	var coldRead struct {
		Thread struct {
			Path   *string        `json:"path"`
			Status map[string]any `json:"status"`
		} `json:"thread"`
	}
	unloadIdle := func() {
		t.Helper()
		if err := client.Request(ctx, "thread/archive", map[string]any{"threadId": threads["idle"]}, &response); err != nil {
			t.Fatal(err)
		}
		if err := client.Request(ctx, "thread/unarchive", map[string]any{"threadId": threads["idle"]}, &response); err != nil {
			t.Fatal(err)
		}
		wait("cold idle thread", func() bool {
			if err := client.Request(ctx, "thread/read", map[string]any{"threadId": threads["idle"], "excludeTurns": true}, &coldRead); err != nil {
				return false
			}
			return coldRead.Thread.Path != nil && coldRead.Thread.Status["type"] == "notLoaded"
		})
	}
	unloadIdle()
	if err := client.Request(ctx, "thread/resume", map[string]any{
		"threadId": threads["idle"], "excludeTurns": true, "cwd": filepath.Join(root, "work", "idle"),
		"runtimeWorkspaceRoots": []string{root}, "sandbox": "workspace-write",
		"config": map[string]any{"sandbox_workspace_write": map[string]any{"writable_roots": []string{root, root, "/tmp"}, "network_access": false, "exclude_tmpdir_env_var": true, "exclude_slash_tmp": false}},
	}, &response); err != nil {
		t.Fatal(err)
	}
	if err := client.Request(ctx, "thread/settings/update", map[string]any{"threadId": threads["idle"], "approvalPolicy": "never"}, &response); err != nil {
		t.Fatal(err)
	}
	unloadIdle()
	historyPath := *coldRead.Thread.Path
	expectedPermissions := compiledWorkspacePolicy([]string{root, root, "/tmp"})
	beforeIdle, err := client.ActiveTurnID(ctx, threads["idle"])
	if err != nil || beforeIdle != "" {
		t.Fatalf("idle before: %s %v", beforeIdle, err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "scoped.snapshot.json")
	operation := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	roots, _ := json.Marshal(map[string]string{"busy": threads["busy"], "idle": threads["idle"], "plain": threads["plain"], "goal": threads["goal"]})
	helper := func(action string) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, binary, "-test.run=^TestNativeProviderHelper$", "--", action, "--workspace", root, "--state-root", stateRoot, "--socket", socket, "--host-profile", root, "--authority", authority, "--snapshot", path, "--target", "pool-b", "--operation", operation, "--sessions", "busy,goal,idle,plain", "--roots", string(roots))
		command.Env = environment
		output, err := command.CombinedOutput()
		if err != nil {
			progress, _ := os.ReadFile(path + ".progress")
			nativeLog, _ := os.ReadFile(log.Name())
			if len(nativeLog) > 12000 {
				nativeLog = nativeLog[len(nativeLog)-12000:]
			}
			t.Fatalf("%s: %v %s; progress %s; native log tail:\n%s", action, err, output, progress, nativeLog)
		}
		return output
	}
	started := time.Now()
	var snapshot scopedProviderSnapshot
	if err := json.Unmarshal(helper("scoped-prepare"), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Threads) != 5 {
		t.Fatalf("wrong retained scope: %+v", snapshot.Threads)
	}
	// The Ruby worker binds trusted session environments after preparation.
	for index := range snapshot.Threads {
		thread := &snapshot.Threads[index]
		thread.Environment = map[string]string{"DEV_SESSION_SLUG": thread.Slug, "DEV_SESSION_WORKSPACE": root, "DEV_SESSION_WORK_DIR": thread.Cwd}
	}
	if err := providerAtomicJSON(path, snapshot); err != nil {
		t.Fatal(err)
	}
	helper("scoped-switch")
	helper("scoped-continue")
	helper("scoped-continue")
	// A no-op approval update persists the current native compiled policy,
	// proving that the comparison cannot use the synthetic input snapshot.
	if err := client.Request(ctx, "thread/settings/update", map[string]any{"threadId": threads["idle"], "approvalPolicy": "never"}, &response); err != nil {
		t.Fatal(err)
	}
	// Inspect only settings metadata, never transcript content. Native compilation
	// must preserve the effective entries, including metadata protections.
	var latestCompiled map[string]any
	wait("persisted target permission snapshot", func() bool {
		data, err := os.ReadFile(historyPath)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "thread_settings_applied") {
				continue
			}
			var record struct {
				Payload struct {
					ThreadID string `json:"thread_id"`
					Settings struct {
						Profile  map[string]any `json:"permission_profile"`
						Provider string         `json:"model_provider_id"`
						Roots    []string       `json:"runtime_workspace_roots"`
					} `json:"thread_settings"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(line), &record) == nil && record.Payload.ThreadID == threads["idle"] && record.Payload.Settings.Provider == "pool-b" {
				if !reflect.DeepEqual(record.Payload.Settings.Roots, []string{root}) {
					t.Fatalf("persisted native runtime roots changed: %#v", record.Payload.Settings.Roots)
				}
				latestCompiled = record.Payload.Settings.Profile
			}
		}
		return latestCompiled != nil
	})
	actualPolicy, err := coldWorkspaceSandbox(latestCompiled, filepath.Join(root, "work", "idle"))
	if err != nil {
		t.Fatalf("native compiled policy: %v %#v", err, latestCompiled)
	}
	expectedPolicy, err := coldWorkspaceSandbox(expectedPermissions, filepath.Join(root, "work", "idle"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualPolicy, expectedPolicy) {
		t.Fatalf("native changed permissions: got %#v want %#v", actualPolicy, expectedPolicy)
	}

	for _, slug := range []string{"busy", "idle", "plain", "goal", "member", "unrelated"} {
		var result map[string]any
		if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": threads[slug], "excludeTurns": true}, &result); err != nil {
			t.Fatal(err)
		}
		wanted := "pool-b"
		if slug == "unrelated" {
			wanted = "pool-a"
		}
		if slug == "idle" && !reflect.DeepEqual(result["runtimeWorkspaceRoots"], []any{root}) {
			t.Fatalf("native changed cold runtime roots: %#v", result["runtimeWorkspaceRoots"])
		}
		if result["modelProvider"] != wanted {
			t.Fatalf("%s provider=%v", slug, result["modelProvider"])
		}
		if slug != "unrelated" {
			for _, thread := range snapshot.Threads {
				if thread.ID != threads[slug] {
					continue
				}
				for _, key := range []string{"sandbox", "approvalPolicy", "approvalsReviewer", "runtimeWorkspaceRoots", "activePermissionProfile", "serviceTier", "model", "reasoningEffort"} {
					if key == "serviceTier" && providerTierMatches(thread.Original[key], result[key]) {
						continue
					}
					if !reflect.DeepEqual(thread.Original[key], result[key]) {
						t.Fatalf("%s changed retained %s: got %#v, want %#v", slug, key, result[key], thread.Original[key])
					}
				}
			}
		}

	}
	if active, err := client.ActiveTurnID(ctx, threads["idle"]); err != nil || active != "" {
		t.Fatalf("idle thread restarted: %s %v", active, err)
	}
	if active, err := client.ActiveTurnID(ctx, threads["unrelated"]); err != nil || active == "" {
		t.Fatalf("unrelated work was interrupted: %s %v", active, err)
	}
	if active, err := client.ActiveTurnID(ctx, member); err != nil || active != "" {
		t.Fatalf("idle retained member restarted: %s %v", active, err)
	}
	wait("temporary child stopped", func() bool {
		active, err := client.ActiveTurnID(ctx, child)
		mu.Lock()
		canceled := oldChildCanceled
		mu.Unlock()
		return err == nil && active == "" && canceled
	})
	// Read the history without resuming the parent-owned child.
	childTranscript, err := client.ReadThread(ctx, child)
	if err != nil || childTranscript.ThreadID != child {
		t.Fatalf("stopped temporary child's history unavailable: %v", err)
	}
	rosterAfter, err := store.Load("busy", threads["busy"])
	if err != nil {
		t.Fatal(err)
	}
	rosterBytes, _ := json.Marshal(rosterAfter)
	if string(rosterBefore) != string(rosterBytes) {
		t.Fatal("provider switch changed retained roster")
	}
	for _, goalSlug := range []string{"busy", "goal"} {
		goal, err := client.HasActiveGoal(ctx, threads[goalSlug])
		if err != nil || !goal {
			t.Fatalf("goal not restored: %v %v", goal, err)
		}
	}
	for _, slug := range []string{"busy", "plain", "goal"} {
		wait(slug+" resumed work", func() bool { active, err := client.ActiveTurnID(ctx, threads[slug]); return err == nil && active != "" })
	}

	queueAfter, err := client.ListQueue(ctx, threads["busy"])
	if err != nil {
		t.Fatal(err)
	}
	if len(queueAfter) > 0 {
		if !reflect.DeepEqual(queueAfter, queueBefore) {
			t.Fatal("native queued submission identity or input changed")
		}
	} else {
		wait("queued input receipt", func() bool {
			transcript, err := client.ReadThread(ctx, threads["busy"])
			if err != nil {
				return false
			}
			count := 0
			for _, entry := range transcript.Entries {
				if entry.ClientUserMessageID == queuedID && entry.Text == "queued fixture input" {
					count++
				}
			}
			return count == 1
		})
	}
	for _, slug := range []string{"busy", "plain", "goal"} {
		wait(slug+" target inference", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return threadRequests[threads[slug]]["/b/v1/responses"] > 0
		})
	}
	// Idle retained members stay idle until given new work; that work must use B.
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": member, "input": []any{map[string]any{"type": "text", "text": "member target inference"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("member target inference", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return threadRequests[member]["/b/v1/responses"] > 0
	})
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": threads["idle"], "input": []any{map[string]any{"type": "text", "text": "spawn-new-child"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("new child inherits target provider", func() bool {
		var rows struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := client.Request(ctx, "thread/list", map[string]any{"ancestorThreadId": threads["idle"], "archived": false, "modelProviders": []string{}, "sourceKinds": workspacecodex.ArchiveDiscoverySourceKinds()}, &rows); err != nil || len(rows.Data) != 1 || rows.Data[0].ID == "" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		return threadRequests[rows.Data[0].ID]["/b/v1/responses"] > 0 && threadRequests[threads["idle"]]["/b/v1/responses"] > 0
	})
	mu.Lock()
	childWasCanceled := oldChildCanceled
	mu.Unlock()
	if !childWasCanceled {
		t.Fatal("temporary child was expected to stop with its parent")
	}
	// A fork's provider comes from its destination incarnation, even when the
	// source/default are B and the daemon was launched with A. Destination C
	// must differ from both so daemon fallback cannot satisfy this proof.
	sourceCwd := filepath.Join(root, "work", "fork-source")
	if err := os.MkdirAll(sourceCwd, 0700); err != nil {
		t.Fatal(err)
	}
	var sourceFork struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/start", map[string]any{"cwd": sourceCwd, "model": "gpt-5.5", "modelProvider": "pool-b", "config": map[string]any{"model_reasoning_effort": "low"}}, &sourceFork); err != nil {
		t.Fatal(err)
	}
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": sourceFork.Thread.ID, "input": []any{map[string]any{"type": "text", "text": "fork source proof"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("fork source materialized and idle", func() bool {
		mu.Lock()
		routed := threadRequests[sourceFork.Thread.ID]["/b/v1/responses"] > 0
		mu.Unlock()
		return routed && client.RequireThreadIdle(ctx, sourceFork.Thread.ID, sourceCwd) == nil
	})
	destination := filepath.Join(root, "work", "fork-destination")
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worktrees", ".locks"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"plan.md": "Fork provider fixture\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(destination, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "schema: 1\nslug: fork-destination\nrepositories: []\nartifacts: []\ncreation:\n  state: ready\n"
	if err := os.WriteFile(filepath.Join(destination, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	incarnation := strings.Repeat("c", 64)
	if err := providerAtomicJSON(filepath.Join(root, "worktrees", ".locks", "fork-destination.fork.json"), map[string]any{"schema": 2, "slug": "fork-destination", "state": "creating", "provider": "pool-c", "tmux_identity": incarnation}); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(root, "synthetic.token")
	if err := os.WriteFile(token, []byte("synthetic-only"), 0600); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(root, "fork-catalog.json")
	catalog := codexproviders.Catalog{Schema: 1, InitialProvider: "pool-b", Providers: []codexproviders.Provider{
		{ID: "openai", Label: "Subscription", Kind: "subscription"},
		{ID: "pool-a", Label: "A", Kind: "codex-lb", BaseURL: backend.URL + "/a/v1", TokenFile: token},
		{ID: "pool-c", Label: "C", Kind: "codex-lb", BaseURL: backend.URL + "/c/v1", TokenFile: token},
		{ID: "pool-b", Label: "B", Kind: "codex-lb", BaseURL: backend.URL + "/b/v1", TokenFile: token},
	}}
	if err := providerAtomicJSON(catalogPath, catalog); err != nil {
		t.Fatal(err)
	}
	statePath, _ := codexproviders.StatePath(stateRoot, "fork-fixture")
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		t.Fatal(err)
	}
	state := codexproviders.State{Schema: 2, Workspace: "fork-fixture", Root: root, Socket: socket, Active: "pool-b", Sessions: map[string]codexproviders.Selection{"fork-destination": {Provider: "pool-c", CreationID: incarnation}}}
	if err := providerAtomicJSON(statePath, state); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACE_NAME", "fork-fixture")
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalogPath)
	destinationClient := workspacecodex.NewWithOptions(socket, root, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
	destinationClient.RecoveryRoot = stateRoot
	defer destinationClient.Close()
	forkID, err := destinationClient.ForkThread(ctx, sourceFork.Thread.ID, destination, map[string]string{}, codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": forkID, "input": []any{map[string]any{"type": "text", "text": "fork destination proof"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("fork routes to its explicit destination provider", func() bool { mu.Lock(); defer mu.Unlock(); return threadRequests[forkID]["/c/v1/responses"] > 0 })
	mu.Lock()
	wrongRoute := threadRequests[forkID]["/b/v1/responses"] + threadRequests[forkID]["/a/v1/responses"]
	mu.Unlock()
	if wrongRoute != 0 {
		t.Fatal("fork used the source provider")
	}
	t.Log("native fork destination override reached C while source and workspace default remained B")
	if err := syscall.Kill(daemon.Process.Pid, 0); err != nil {
		t.Fatal("shared daemon exited")
	}
	t.Logf("scoped provider applied in %s without daemon restart; idle and unrelated work retained, goal restored, queued entries before=%d after=%d", time.Since(started), len(queueBefore), len(queueAfter))
}
