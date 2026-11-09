//go:build live_switch_integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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

type scopedColdBaseline struct {
	Thread   providerThread `json:"thread"`
	Path     string         `json:"path"`
	Compiled map[string]any `json:"compiled"`
}

func captureScopedColdBaseline(t *testing.T, ctx context.Context, client *workspacecodex.Client, thread providerThread, provider string) scopedColdBaseline {
	t.Helper()
	var effective map[string]any
	if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": thread.ID, "excludeTurns": true}, &effective); err != nil {
		t.Fatal(err)
	}
	if err := validateScopedRetainedSettings(thread, effective, provider); err != nil {
		t.Fatal(err)
	}
	if thread.ScopedPermissions != nil {
		if err := verifyScopedPermissionAuthority(ctx, client, thread, effective); err != nil {
			t.Fatal(err)
		}
	}
	thread.Original = effective
	var metadata struct {
		Thread struct {
			ID, Cwd string
			Path    *string
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": thread.ID, "excludeTurns": true}, &metadata); err != nil || metadata.Thread.Path == nil || metadata.Thread.ID != thread.ID || metadata.Thread.Cwd != thread.Cwd {
		t.Fatalf("cold baseline history identity: %v", err)
	}
	history, err := readNativePolicyHistory(*metadata.Thread.Path, true)
	if err != nil || history.ThreadID != thread.ID || history.Cwd != thread.Cwd {
		t.Fatalf("cold baseline compiled identity: %v", err)
	}
	compiled, ok := history.Retained["permission_profile"].(map[string]any)
	if !ok {
		t.Fatal("cold baseline compiled permissions absent")
	}
	return scopedColdBaseline{thread, *metadata.Thread.Path, compiled}
}

// This fixture uses the production policy projector without selecting the
// provider/model/effort whose native persistence the cold boundary must prove.
func resumeScopedColdPolicy(t *testing.T, ctx context.Context, client *workspacecodex.Client, before scopedColdBaseline, evidence map[string]any) {
	t.Helper()
	evidence["before"] = before
	thread := before.Thread
	provider, _ := thread.Original["modelProvider"].(string)
	var metadata struct {
		Thread struct {
			ID, Cwd, ModelProvider string
			Path                   *string
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": thread.ID, "excludeTurns": true}, &metadata); err != nil || metadata.Thread.ID != thread.ID || metadata.Thread.Cwd != thread.Cwd || metadata.Thread.ModelProvider != provider || metadata.Thread.Path == nil || *metadata.Thread.Path != before.Path {
		t.Fatalf("cold retained history identity changed: %v", err)
	}
	history, err := readNativePolicyHistory(before.Path, true)
	if err != nil || history.ThreadID != thread.ID || history.Cwd != thread.Cwd || !reflect.DeepEqual(history.Retained["permission_profile"], before.Compiled) {
		t.Fatalf("cold retained compiled policy changed before resume: %v", err)
	}
	params := map[string]any{"threadId": thread.ID, "cwd": thread.Cwd, "excludeTurns": true, "config": map[string]any{}}
	if err := preserveColdNativePolicy(before.Path, params); err != nil {
		t.Fatal(err)
	}
	evidence["request"] = params
	for key := range params {
		switch key {
		case "threadId", "cwd", "excludeTurns", "config", "sandbox", "permissions", "runtimeWorkspaceRoots", "approvalPolicy", "approvalsReviewer", "serviceTier":
		default:
			t.Fatalf("cold policy request selects routing/model data: %s", key)
		}
	}
	config := params["config"].(map[string]any)
	for key := range config {
		if key != "sandbox_workspace_write" {
			t.Fatalf("cold policy config selects routing/model data: %s", key)
		}
	}
	// Compare only permitted policy fields against the retained production
	// payload. Its provider/model fields are never copied into the cold request.
	expected, err := providerResumeParams(thread, provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sandbox", "permissions", "runtimeWorkspaceRoots", "approvalPolicy", "approvalsReviewer", "serviceTier"} {
		if key == "serviceTier" && providerTierMatches(expected[key], params[key]) {
			continue
		}
		if !reflect.DeepEqual(expected[key], params[key]) {
			t.Fatalf("cold policy projection changed retained %s: got=%#v want=%#v", key, params[key], expected[key])
		}
	}
	if !reflect.DeepEqual(config["sandbox_workspace_write"], expected["config"].(map[string]any)["sandbox_workspace_write"]) {
		t.Fatal("cold policy projection changed authored workspace permissions")
	}
	var effective map[string]any
	err = client.Request(ctx, "thread/resume", params, &effective)
	evidence["response"] = effective
	if err != nil {
		t.Fatal(err)
	}
	if err := validateScopedRetainedSettings(thread, effective, provider); err != nil {
		t.Fatal(err)
	}
	if thread.ScopedPermissions != nil {
		if err := verifyScopedPermissionAuthority(ctx, client, thread, effective); err != nil {
			t.Fatal(err)
		}
	}
	after, err := readNativePolicyHistory(before.Path, true)
	if err != nil {
		t.Fatal(err)
	}
	evidence["compiledAfter"] = after.Retained
	if thread.ScopedPermissions == nil && !reflect.DeepEqual(after.Retained["permission_profile"], before.Compiled) {
		t.Fatal("cold builtin compiled permissions changed")
	}
}

// One genuine retained session and seed turn isolate the authored-permission
// boundary before the longer switch/queue/goal acceptance scenario.
func TestNativeScopedSandboxPreservation(t *testing.T) {
	native, serverArgv := providerAcceptanceLaunch(t)
	root, err := os.MkdirTemp("", "scoped-permission-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			t.Logf("retained scoped permission evidence: %s", root)
		} else {
			_ = os.RemoveAll(root)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var mu sync.Mutex
	posts, wrong := 0, 0
	postRoutes := map[string]int{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5.5"}]}`)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		posts++
		postRoutes[r.URL.Path]++
		valid := providerAcceptanceAuth(r.URL.Path, r.Header.Get("Authorization"), map[string]string{"/a": "fixture-a", "/b": "fixture-b"})
		if !valid {
			wrong++
		}
		mu.Unlock()
		if !valid {
			http.Error(w, "wrong synthetic route/auth", 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []any{
			map[string]any{"type": "response.created", "response": map[string]any{"id": "scoped-policy-seed"}},
			map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "scoped-policy-answer", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "scoped-permission-complete"}}}},
			map[string]any{"type": "response.completed", "response": map[string]any{"id": "scoped-policy-seed", "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}},
		} {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer backend.Close()
	home, state, authority := filepath.Join(root, "home"), filepath.Join(root, "state"), filepath.Join(root, "authority")
	cwd := filepath.Join(root, "work", "idle")
	for _, path := range []string{home, state, authority, cwd, filepath.Join(root, "share", "dev-workspace")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := "model_provider=\"pool-a\"\nmodel=\"gpt-5.5\"\nmodel_reasoning_effort=\"low\"\napproval_policy=\"never\"\n"
	for _, key := range []string{"a", "b"} {
		config += "[model_providers.pool-" + key + "]\nname=\"Fixture " + key + "\"\nwire_api=\"responses\"\nsupports_websockets=false\nenv_key=\"FIXTURE_" + strings.ToUpper(key) + "_TOKEN\"\nbase_url=" + strconv.Quote(backend.URL+"/"+key+"/v1") + "\n"
	}
	for path, data := range map[string]string{filepath.Join(home, "config.toml"): config, filepath.Join(cwd, "state.md"): "---\nlifecycle: active\n---\n", filepath.Join(cwd, "plan.md"): "Scoped permission fixture\n", filepath.Join(root, "share/dev-workspace/package.json"): `{"schema":1,"activationEnvironmentAliases":["DEV_WORKSPACES"],"routerSocket":"/run/fixture/router.sock","userNamespace":"fixture","agent_teams":{"managed":false}}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	environment := providerAcceptanceEnvironment(t, root, home)
	socket := filepath.Join(root, "app.sock")
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	args := append([]string{"app-server"}, serverArgv...)
	args = append(args, "--listen", "unix://"+socket)
	var daemon *exec.Cmd
	stop := func() {
		if daemon != nil {
			providerAcceptanceStop(t, daemon)
			daemon = nil
		}
	}
	defer stop()
	start := func() {
		_ = os.Remove(socket)
		daemon = exec.CommandContext(ctx, native, args...)
		daemon.Env, daemon.Dir, daemon.Stdout, daemon.Stderr = environment, root, log, log
		daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		t.Logf("scoped permission native PID/group=%d", daemon.Process.Pid)
	}
	start()
	wait := func(label string, ready func() bool) {
		t.Helper()
		for !ready() {
			if ctx.Err() != nil {
				data, _ := os.ReadFile(log.Name())
				t.Fatalf("%s: %v %s", label, ctx.Err(), data)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("native socket", func() bool { _, err := os.Stat(socket); return err == nil })
	client := &workspacecodex.Client{Client: codex.NewWithOptions(socket, codex.ClientOptions{ClientInfo: codex.ClientInfo{Name: "dev-workspace", Version: "0.1.0"}, PreserveThreadInstructionsOnResume: true})}
	defer func() { client.Close() }()
	id, err := client.StartThreadWithSettings(ctx, cwd, map[string]string{}, codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low", Policy: codex.ThreadPolicy{Sandbox: "read-only"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("schema: 1\nslug: idle\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.167.0\ncreation:\n  state: ready\n", id, socket)
	if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "materialize-scoped-permissions"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("one real seed turn", func() bool {
		thread, err := client.ReadThread(ctx, id)
		return err == nil && strings.Contains(fmt.Sprint(thread.Entries), "scoped-permission-complete") && client.RequireThreadTurnsIdle(ctx, id) == nil
	})
	archive := func() {
		t.Helper()
		if err := client.Request(ctx, "thread/archive", map[string]any{"threadId": id}, &response); err != nil {
			t.Fatal(err)
		}
		if err := client.Request(ctx, "thread/unarchive", map[string]any{"threadId": id}, &response); err != nil {
			t.Fatal(err)
		}
	}
	archive()
	authored := map[string]any{"threadId": id, "excludeTurns": true, "cwd": cwd, "runtimeWorkspaceRoots": []string{root}, "sandbox": "workspace-write", "config": map[string]any{"sandbox_workspace_write": map[string]any{"writable_roots": []string{root, root, "/tmp"}, "network_access": false, "exclude_tmpdir_env_var": true, "exclude_slash_tmp": false}}}
	var before map[string]any
	if err := client.Request(ctx, "thread/resume", authored, &before); err != nil {
		t.Fatal(err)
	}
	if err := client.Request(ctx, "thread/settings/update", map[string]any{"threadId": id, "approvalPolicy": "never"}, &response); err != nil {
		t.Fatal(err)
	}
	archive()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "snapshot.json")
	operation := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	roots, _ := json.Marshal(map[string]string{"idle": id})
	command := exec.CommandContext(ctx, binary, "-test.run=^TestNativeProviderHelper$", "--", "scoped-prepare", "--workspace", root, "--state-root", state, "--socket", socket, "--host-profile", root, "--authority", authority, "--snapshot", path, "--target", "pool-b", "--operation", operation, "--sessions", "idle", "--roots", string(roots))
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real scoped prepare: %v %s", err, output)
	}
	var captured scopedProviderSnapshot
	if err := json.Unmarshal(output, &captured); err != nil {
		t.Fatal(err)
	}
	if captured.Schema != 2 || len(captured.Threads) != 1 || captured.Threads[0].ID != id {
		t.Fatalf("unexpected captured identity: %+v", captured)
	}
	thread := captured.Threads[0]
	// Match the worker's trusted environment binding after actual capture.
	thread.Environment = map[string]string{"DEV_SESSION_SLUG": thread.Slug, "DEV_SESSION_WORKSPACE": root, "DEV_SESSION_WORK_DIR": thread.Cwd}
	captured.Threads[0] = thread
	if err := validateScopedSnapshot(captured, root, "pool-b"); err != nil {
		t.Fatal(err)
	}
	if err := providerAtomicJSON(path, captured); err != nil {
		t.Fatal(err)
	}
	evidence := map[string]any{"authored": authored, "before": before, "captured": captured}
	proof, err := os.CreateTemp("", "scoped-permission-proof-*.json")
	if err != nil {
		t.Fatal(err)
	}
	proofPath := proof.Name()
	proof.Close()
	writeEvidence := func() {
		data, _ := json.MarshalIndent(evidence, "", "  ")
		if err := os.WriteFile(proofPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	defer writeEvidence()
	legacy := thread
	legacy.ScopedPermissions = nil // Real captured response, interpreted by the retained schema-1 payload path.
	archive()
	legacyParams, legacyResponse, err := resumeScopedProviderThread(ctx, client, legacy, "pool-b")
	evidence["legacyRequest"], evidence["legacyResponse"] = legacyParams, legacyResponse
	if err != nil {
		t.Fatal(err)
	}
	if err := validateScopedRetainedSettings(legacy, legacyResponse, "pool-b"); err == nil || !strings.Contains(err.Error(), "retained sandbox") {
		t.Fatalf("legacy lossy guard did not refuse: %v", err)
	}
	var metadata struct {
		Thread struct {
			Path *string `json:"path"`
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": id, "excludeTurns": true}, &metadata); err != nil || metadata.Thread.Path == nil {
		t.Fatalf("legacy history: %v", err)
	}
	legacyHistory, err := readNativePolicyHistory(*metadata.Thread.Path, true)
	if err != nil {
		t.Fatal(err)
	}
	evidence["legacyCompiledAfter"] = legacyHistory.Retained
	archive()
	params, after, err := resumeScopedProviderThread(ctx, client, thread, "pool-b")
	evidence["schema2Request"], evidence["schema2Response"] = params, after
	if err != nil {
		t.Fatal(err)
	}
	if err := validateScopedRetainedSettings(thread, after, "pool-b"); err != nil {
		t.Fatal(err)
	}
	if err := verifyScopedPermissionAuthority(ctx, client, thread, after); err != nil {
		t.Fatal(err)
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": id, "excludeTurns": true}, &metadata); err != nil {
		t.Fatal(err)
	}
	history, err := readNativePolicyHistory(*metadata.Thread.Path, true)
	if err != nil {
		t.Fatal(err)
	}
	evidence["schema2CompiledAfter"] = history.Retained
	// Exercise the actual latest-turn chronology after B is selected, rather
	// than proving only an idle settings checkpoint with no later turn.
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "materialize-B-cold-policy"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("B turn completed before cold baseline", func() bool {
		mu.Lock()
		count := posts
		mu.Unlock()
		return count == 2 && client.RequireThreadTurnsIdle(ctx, id) == nil
	})
	// A genuine retained member is a separate native root, not a v2 child.
	var memberStart struct {
		Thread struct{ ID string } `json:"thread"`
	}
	if err := client.Request(ctx, "thread/start", map[string]any{"cwd": cwd, "model": "gpt-5.5", "modelProvider": "pool-b", "sandbox": "read-only", "approvalPolicy": "never", "developerInstructions": "Retained member fixture.", "config": map[string]any{"model_reasoning_effort": "low", "shell_environment_policy": map[string]any{"set": map[string]string{"DEV_SESSION_MEMBER": "reviewer0"}}}}, &memberStart); err != nil {
		t.Fatal(err)
	}
	member := memberStart.Thread.ID
	store, err := teamruntime.NewStore(state, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, "idle", id, true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{{Address: "reviewer0", Role: "reviewer", Thread: member, Model: "gpt-5.5", Effort: "low", State: "ready", AddedAt: time.Now().UTC(), Behavior: "reviewer", Purpose: "review", Access: "read_only", Instructions: "Retained member fixture."}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": member, "input": []any{map[string]any{"type": "text", "text": "materialize-member-settings"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("retained member metadata", func() bool {
		transcript, err := client.ReadThread(ctx, member)
		return err == nil && strings.Contains(fmt.Sprint(transcript.Entries), "scoped-permission-complete") && client.RequireThreadTurnsIdle(ctx, member) == nil
	})
	memberSettings, err := client.ReadThreadSettings(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	memberThread := providerThread{ID: member, Slug: "idle", Cwd: cwd, MemberAddress: "reviewer0", Settings: memberSettings}
	if err := captureScopedProviderThread(ctx, client, &memberThread, "pool-a"); err != nil {
		t.Fatal(err)
	}
	if memberThread.Original["activePermissionProfile"] != nil || memberThread.Original["sandbox"].(map[string]any)["type"] != "readOnly" {
		t.Fatal("member did not retain the unnamed read-only policy")
	}
	if err := client.Request(ctx, "turn/start", map[string]any{"threadId": member, "input": []any{map[string]any{"type": "text", "text": "materialize-member-B-chronology"}}}, &response); err != nil {
		t.Fatal(err)
	}
	wait("member B turn completed before cold baseline", func() bool {
		mu.Lock()
		count := posts
		mu.Unlock()
		return count == 4 && client.RequireThreadTurnsIdle(ctx, member) == nil
	})
	baseline := captureScopedColdBaseline(t, ctx, client, thread, "pool-b")
	memberBaseline := captureScopedColdBaseline(t, ctx, client, memberThread, "pool-b")
	evidence["cold"] = map[string]any{}
	evidence["coldMember"] = map[string]any{}
	client.Close()
	stop()
	start()
	wait("cold native socket", func() bool { _, err := os.Stat(socket); return err == nil })
	client = &workspacecodex.Client{Client: codex.NewWithOptions(socket, codex.ClientOptions{ClientInfo: codex.ClientInfo{Name: "dev-workspace", Version: "0.1.0"}, PreserveThreadInstructionsOnResume: true})}
	if data, err := os.ReadFile(filepath.Join(home, "config.toml")); err != nil || string(data) != config {
		t.Fatal("cold control changed the default A/provider configuration", err)
	}
	resumeScopedColdPolicy(t, ctx, client, baseline, evidence["cold"].(map[string]any))
	resumeScopedColdPolicy(t, ctx, client, memberBaseline, evidence["coldMember"].(map[string]any))
	mu.Lock()
	defer mu.Unlock()
	if posts != 4 || wrong != 0 || !reflect.DeepEqual(postRoutes, map[string]int{"/a/v1/responses": 1, "/b/v1/responses": 3}) {
		t.Fatalf("expected only root/member seed and chronology turns: posts=%d wrong=%d routes=%v", posts, wrong, postRoutes)
	}
	evidence["postRoutes"] = postRoutes
	writeEvidence()
	t.Logf("root=%s member=%s legacy guard refused; schema2 and cold root/member policy-only resume preserve exact response+compiled permissions with implicit B, four authenticated seed/chronology turns; receipt=%s", id, member, proofPath)
}

// Real selected native protocol, disposable histories and loopback model
// responses. No live workspace socket, subscription or model inference is used.
func TestNativeScopedProviderSwitch(t *testing.T) {
	native, serverArgv := providerAcceptanceLaunch(t)
	root, err := os.MkdirTemp("", "provider-scoped-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			t.Logf("retained private provider receipts: %s", root)
		} else {
			_ = os.RemoveAll(root)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	var mu sync.Mutex
	requests := map[string]int{}
	threadRequests := map[string]map[string]int{}
	spawned := map[string]bool{}
	oldChildCanceled := false
	wrongAuth := 0
	stage := "seed-a"
	stageRequests := map[string]map[string]map[string]int{}
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
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(body, &input)
		childRequest := false
		lastUser := ""
		for _, item := range input.Input {
			childRequest = childRequest || item.Type == "agent_message"
			if item.Role == "user" {
				lastUser = ""
				for _, part := range item.Content {
					lastUser += part.Text
				}
			}
		}
		mu.Lock()
		if !providerAcceptanceAuth(r.URL.Path, r.Header.Get("Authorization"), map[string]string{"/a": "fixture-a", "/b": "fixture-b", "/c": "fixture-c"}) {
			wrongAuth++
			mu.Unlock()
			http.Error(w, "wrong synthetic route/auth", http.StatusUnauthorized)
			return
		}
		requests[r.URL.Path]++
		threadID := r.Header.Get("x-client-request-id")
		if threadRequests[threadID] == nil {
			threadRequests[threadID] = map[string]int{}
		}
		threadRequests[threadID][r.URL.Path]++
		if stageRequests[stage] == nil {
			stageRequests[stage] = map[string]map[string]int{}
		}
		if stageRequests[stage][threadID] == nil {
			stageRequests[stage][threadID] = map[string]int{}
		}
		stageRequests[stage][threadID][r.URL.Path]++
		spawnMarker := ""
		for _, marker := range []string{"spawn-old-child", "spawn-new-child-pool-b", "spawn-new-child-pool-a"} {
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
		if !strings.Contains(lastUser, "cold-route-") && (strings.Contains(string(body), "scoped-block") || strings.Contains(string(body), "unrelated-block") || strings.Contains(lastUser, "Continue the work interrupted by the provider switch.") || strings.Contains(r.URL.Path, "/b/") && !strings.Contains(lastUser, "fork source proof")) {
			<-r.Context().Done()
			return
		}
		event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "answer", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "scoped-fixture-complete"}}}})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": "fixture", "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer func() { cancel(); backend.Close() }()
	home, stateRoot, authority := filepath.Join(root, "home"), filepath.Join(root, "state"), filepath.Join(root, "authority")
	for _, path := range []string{home, stateRoot, authority} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := "model_provider = \"pool-a\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\napproval_policy = \"never\"\n[features]\nmulti_agent_v2 = true\n"
	for _, provider := range []string{"a", "b", "c"} {
		config += "[model_providers.pool-" + provider + "]\nname = \"Fixture " + provider + "\"\nwire_api = \"responses\"\nsupports_websockets = false\nenv_key = \"FIXTURE_" + strings.ToUpper(provider) + "_TOKEN\"\nbase_url = " + strconv.Quote(backend.URL+"/"+provider+"/v1") + "\n"
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
	environment := providerAcceptanceEnvironment(t, root, home)
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var daemon *exec.Cmd
	stop := func() {
		if daemon != nil {
			providerAcceptanceStop(t, daemon)
			daemon = nil
		}
	}
	defer stop()
	start := func() {
		t.Helper()
		_ = os.Remove(socket)
		args := append([]string{"app-server"}, serverArgv...)
		daemon = exec.CommandContext(ctx, native, append(args, "--listen", "unix://"+socket)...)
		daemon.Env, daemon.Dir, daemon.Stdout, daemon.Stderr = environment, root, log, log
		daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
	}
	start()
	wait := func(label string, condition func() bool) {
		t.Helper()
		phase := time.NewTimer(20 * time.Second)
		defer phase.Stop()
		for !condition() {
			select {
			case <-ctx.Done():
				data, _ := os.ReadFile(log.Name())
				if len(data) > 12000 {
					data = data[len(data)-12000:]
				}
				t.Fatalf("%s: %v %s", label, ctx.Err(), data)
			case <-phase.C:
				data, _ := os.ReadFile(log.Name())
				if len(data) > 12000 {
					data = data[len(data)-12000:]
				}
				t.Fatalf("%s: phase exceeded 20s; native tail: %s", label, data)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	wait("native socket", func() bool { _, err := os.Stat(socket); return err == nil })
	submissionIdentity := sha256.Sum256([]byte(socket))
	submissionBase := filepath.Join(stateRoot, "submissions", fmt.Sprintf("%x", submissionIdentity))
	clientOptions := codex.ClientOptions{
		ClientInfo:                         codex.ClientInfo{Name: "dev-workspace", Version: "0.1.0"},
		PreserveThreadInstructionsOnResume: true,
		SubmissionLedgerPath:               submissionBase + ".json",
		SubmissionDatabasePath:             submissionBase + ".sqlite",
		LegacySubmissionLedgerPath:         socket + ".submission-attempts-v3.json",
	}
	client := codex.NewWithOptions(socket, clientOptions)
	defer func() { client.Close() }()
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
	goalIdentity := func(slug string) map[string]any {
		t.Helper()
		var result struct {
			Goal map[string]any `json:"goal"`
		}
		if err := client.Request(ctx, "thread/goal/get", map[string]any{"threadId": threads[slug]}, &result); err != nil || result.Goal == nil {
			t.Fatal("missing durable goal", slug, err)
		}
		identity := map[string]any{}
		for _, key := range []string{"threadId", "createdAt", "objective", "status", "tokenBudget"} {
			identity[key] = result.Goal[key]
		}
		return identity
	}
	goalBefore := map[string]map[string]any{"busy": goalIdentity("busy"), "goal": goalIdentity("goal")}
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
	started := time.Now()
	switchTo := func(target, operation string) {
		t.Helper()
		route := "/" + strings.TrimPrefix(target, "pool-") + "/v1/responses"
		path := filepath.Join(root, target+".snapshot.json")
		phasePID := daemon.Process.Pid
		unrelatedTurn, err := client.ActiveTurnID(ctx, threads["unrelated"])
		if err != nil || unrelatedTurn == "" {
			t.Fatal("missing unrelated active control", err)
		}
		mu.Lock()
		stage = target
		mu.Unlock()
		roots, _ := json.Marshal(map[string]string{"busy": threads["busy"], "idle": threads["idle"], "plain": threads["plain"], "goal": threads["goal"]})
		helper := func(action string) []byte {
			t.Helper()
			phaseCtx, cancelPhase := context.WithTimeout(ctx, 30*time.Second)
			defer cancelPhase()
			command := exec.CommandContext(phaseCtx, binary, "-test.run=^TestNativeProviderHelper$", "--", action, "--workspace", root, "--state-root", stateRoot, "--socket", socket, "--host-profile", root, "--authority", authority, "--snapshot", path, "--target", target, "--operation", operation, "--sessions", "busy,goal,idle,plain", "--roots", string(roots))
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
		wrongRoutes := func(id string) int {
			total := 0
			for path, count := range threadRequests[id] {
				if path != route {
					total += count
				}
			}
			return total
		}
		mu.Lock()
		wrongBefore := map[string]int{}
		for _, thread := range snapshot.Threads {
			wrongBefore[thread.ID] = wrongRoutes(thread.ID)
		}
		mu.Unlock()
		helper("scoped-continue")
		firstContinuation, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		helper("scoped-continue")
		secondContinuation, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(firstContinuation, secondContinuation) {
			t.Fatal("repeated continuation changed its original receipt", err)
		}
		var continued scopedProviderSnapshot
		if err := json.Unmarshal(secondContinuation, &continued); err != nil {
			t.Fatal(err)
		}
		for _, thread := range continued.Threads {
			intent := continued.Intents[thread.ID]
			if intent == nil || intent.Continuation != snapshot.Intents[thread.ID].Continuation || !intent.Continued {
				t.Fatal("continuation identity changed", thread.ID)
			}
			transcript, err := client.ReadThread(ctx, thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, entry := range transcript.Entries {
				if entry.ClientUserMessageID == intent.Continuation {
					count++
				}
			}
			if count > 1 || (intent.GoalOwned || intent.GoalActive) && count != 0 {
				t.Fatalf("duplicate or goal-owned synthetic continuation: %s count=%d", thread.ID, count)
			}
			if intent.Active && !intent.GoalOwned && !intent.GoalActive && count != 1 {
				t.Fatalf("missing continuation receipt: %s count=%d", thread.ID, count)
			}
		}

		// A no-op approval update persists the current native compiled policy,
		// proving that the comparison cannot use the synthetic input snapshot.
		phaseHistory, err := os.Stat(historyPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Request(ctx, "thread/settings/update", map[string]any{"threadId": threads["idle"], "approvalPolicy": "never"}, &response); err != nil {
			t.Fatal(err)
		}
		// Inspect only settings metadata, never transcript content. Native compilation
		// must preserve the effective entries, including metadata protections.
		var latestCompiled map[string]any
		var latestRoots []string
		wait("persisted target permission snapshot", func() bool {
			data, err := os.ReadFile(historyPath)
			if err != nil || int64(len(data)) <= phaseHistory.Size() {
				return false
			}
			latestCompiled, latestRoots = nil, nil
			for _, line := range strings.Split(string(data[phaseHistory.Size():]), "\n") {
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
				if json.Unmarshal([]byte(line), &record) == nil && record.Payload.ThreadID == threads["idle"] && record.Payload.Settings.Provider == target {
					latestRoots = record.Payload.Settings.Roots
					latestCompiled = record.Payload.Settings.Profile
				}
			}
			return latestCompiled != nil
		})
		if !reflect.DeepEqual(latestRoots, []string{root}) {
			t.Fatalf("persisted native runtime roots changed: %#v", latestRoots)
		}
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
			wanted := target
			if slug == "unrelated" {
				wanted = "pool-a"
			}
			if slug == "idle" && !reflect.DeepEqual(result["runtimeWorkspaceRoots"], []any{root}) {
				t.Fatalf("native changed cold runtime roots: %#v", result["runtimeWorkspaceRoots"])
			}
			if result["modelProvider"] != wanted {
				t.Fatalf("%s provider=%v", slug, result["modelProvider"])
			}
			if actual, ok := result["thread"].(map[string]any); !ok || actual["id"] != threads[slug] {
				t.Fatal("provider switch changed native thread identity", slug, result["thread"])
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
		if active, err := client.ActiveTurnID(ctx, threads["unrelated"]); err != nil || active != unrelatedTurn {
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
			if !reflect.DeepEqual(goalBefore[goalSlug], goalIdentity(goalSlug)) {
				t.Fatal("provider switch changed original goal identity/objective/status", goalSlug)
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
				return stageRequests[target][threads[slug]][route] > 0
			})
		}
		// Idle retained members stay idle until given new work; that work must use B.
		if err := client.Request(ctx, "turn/start", map[string]any{"threadId": member, "input": []any{map[string]any{"type": "text", "text": "member target inference " + target}}}, &response); err != nil {
			t.Fatal(err)
		}
		wait("member target inference", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return stageRequests[target][member][route] > 0
		})
		if err := client.Request(ctx, "turn/start", map[string]any{"threadId": threads["idle"], "input": []any{map[string]any{"type": "text", "text": "spawn-new-child-" + target}}}, &response); err != nil {
			t.Fatal(err)
		}
		wait("new child inherits target provider", func() bool {
			var rows struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := client.Request(ctx, "thread/list", map[string]any{"ancestorThreadId": threads["idle"], "archived": false, "modelProviders": []string{}, "sourceKinds": workspacecodex.ArchiveDiscoverySourceKinds()}, &rows); err != nil || len(rows.Data) == 0 {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			for _, row := range rows.Data {
				if threadRequests[row.ID][route] > 0 && threadRequests[threads["idle"]][route] > 0 {
					return true
				}
			}
			return false
		})
		mu.Lock()
		childWasCanceled := oldChildCanceled
		mu.Unlock()
		if !childWasCanceled {
			t.Fatal("temporary child was expected to stop with its parent")
		}
		if daemon.Process.Pid != phasePID || syscall.Kill(phasePID, 0) != nil {
			t.Fatal("scoped switch restarted the daemon")
		}
		mu.Lock()
		badAuth := wrongAuth
		for _, thread := range snapshot.Threads {
			if wrongRoutes(thread.ID) != wrongBefore[thread.ID] {
				mu.Unlock()
				t.Fatal("post-cutover inference used another route", target, thread.ID)
			}
		}
		mu.Unlock()
		if badAuth != 0 {
			t.Fatalf("wrong endpoint/auth requests: %d", badAuth)
		}
		t.Logf("scoped target=%s operation=%s snapshot=%s daemon=%d unrelatedTurn=%s", target, operation, path, phasePID, unrelatedTurn)
	}
	switchTo("pool-b", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
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
	var forkIdleError error
	var forkRouteCount int
	defer func() {
		if t.Failed() && (forkRouteCount == 0 || forkIdleError != nil) {
			t.Logf("fork source idle diagnostic: thread=%s cwd=%s routeCount=%d idleError=%v", sourceFork.Thread.ID, sourceCwd, forkRouteCount, forkIdleError)
		}
	}()
	wait("fork source materialized and idle", func() bool {
		mu.Lock()
		forkRouteCount = threadRequests[sourceFork.Thread.ID]["/b/v1/responses"]
		routed := forkRouteCount > 0
		mu.Unlock()
		if !routed {
			return false
		}
		forkIdleError = client.RequireThreadIdle(ctx, sourceFork.Thread.ID, sourceCwd)
		return forkIdleError == nil
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
	tokens := map[string]string{}
	for _, provider := range []string{"a", "b", "c"} {
		tokens[provider] = filepath.Join(root, provider+".token")
		if err := os.WriteFile(tokens[provider], []byte("fixture-"+provider), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalogPath := filepath.Join(root, "fork-catalog.json")
	catalog := codexproviders.Catalog{Schema: 1, InitialProvider: "pool-b", Providers: []codexproviders.Provider{
		{ID: "openai", Label: "Subscription", Kind: "subscription"},
		{ID: "pool-a", Label: "A", Kind: "codex-lb", BaseURL: backend.URL + "/a/v1", TokenFile: tokens["a"]},
		{ID: "pool-c", Label: "C", Kind: "codex-lb", BaseURL: backend.URL + "/c/v1", TokenFile: tokens["c"]},
		{ID: "pool-b", Label: "B", Kind: "codex-lb", BaseURL: backend.URL + "/b/v1", TokenFile: tokens["b"]},
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
	destinationClient.Close()
	// This deliberate cold boundary is separate from both no-restart scoped
	// switches. Existing application policy projection preserves permissions;
	// native persisted state must select B/model/effort despite disk default A.
	var verifiedB scopedProviderSnapshot
	data, err := os.ReadFile(filepath.Join(root, "pool-b.snapshot.json"))
	if err != nil || json.Unmarshal(data, &verifiedB) != nil {
		t.Fatal("missing verified B snapshot", err)
	}
	coldBaselines := map[string]scopedColdBaseline{}
	for _, thread := range verifiedB.Threads {
		if thread.ID == threads["idle"] || thread.ID == threads["member"] {
			coldBaselines[thread.ID] = captureScopedColdBaseline(t, ctx, &workspacecodex.Client{Client: client}, thread, "pool-b")
		}
	}
	if len(coldBaselines) != 2 {
		t.Fatal("missing retained root/member cold baseline")
	}
	coldEvidence := map[string]any{}
	proof, err := os.CreateTemp("", "scoped-cold-proof-*.json")
	if err != nil {
		t.Fatal(err)
	}
	proof.Close()
	defer func() {
		data, _ := json.MarshalIndent(coldEvidence, "", "  ")
		if err := os.WriteFile(proof.Name(), data, 0600); err != nil {
			t.Error(err)
		}
		t.Logf("cold policy-only request/response proof: %s", proof.Name())
	}()
	client.Close()
	stop()
	mu.Lock()
	stage = "cold-b"
	mu.Unlock()
	start()
	wait("cold native socket", func() bool { _, err := os.Stat(socket); return err == nil })
	client = codex.NewWithOptions(socket, clientOptions)
	if data, err := os.ReadFile(filepath.Join(home, "config.toml")); err != nil || string(data) != config {
		t.Fatal("cold control changed the default A/provider configuration", err)
	}
	for _, slug := range []string{"idle", "member"} {
		id := threads[slug]
		cwd := filepath.Join(root, "work", "idle")
		if slug == "member" {
			cwd = memberCwd
		}
		coldEvidence[slug] = map[string]any{}
		resumeScopedColdPolicy(t, ctx, &workspacecodex.Client{Client: client}, coldBaselines[id], coldEvidence[slug].(map[string]any))
		marker := "cold-route-" + slug
		if err := client.Request(ctx, "turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": marker}}}, &response); err != nil {
			t.Fatal(err)
		}
		wait("cold B traffic and exactly-one marker "+slug, func() bool {
			mu.Lock()
			routed := stageRequests["cold-b"][id]["/b/v1/responses"] > 0
			wrong := stageRequests["cold-b"][id]["/a/v1/responses"] + stageRequests["cold-b"][id]["/c/v1/responses"]
			mu.Unlock()
			if wrong != 0 {
				t.Fatal("cold B thread used default A or C")
			}
			transcript, err := client.ReadThread(ctx, id)
			if err != nil {
				return false
			}
			count := 0
			for _, entry := range transcript.Entries {
				if entry.Text == marker {
					count++
				}
			}
			if count > 1 {
				t.Fatal("duplicate cold marker receipt", slug)
			}
			return routed && count == 1 && client.RequireThreadIdle(ctx, id, cwd) == nil
		})
	}
	// Materialize the post-restart active controls without changing goals or
	// replaying the first switch's accepted continuation identities.
	for _, slug := range []string{"busy", "plain", "goal", "unrelated"} {
		if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": threads[slug], "excludeTurns": true}, &response); err != nil {
			t.Fatal(err)
		}
		if active, err := client.ActiveTurnID(ctx, threads[slug]); err != nil {
			t.Fatal(err)
		} else if active == "" {
			if err := client.Request(ctx, "turn/start", map[string]any{"threadId": threads[slug], "input": []any{map[string]any{"type": "text", "text": "post-restart-scoped-block-" + slug}}}, &response); err != nil {
				t.Fatal(err)
			}
		}
		wait("post-restart active "+slug, func() bool { active, err := client.ActiveTurnID(ctx, threads[slug]); return err == nil && active != "" })
	}
	switchTo("pool-a", "aaaaaaaa-aaaa-4aaa-8aaa-bbbbbbbbbbbb")
	for _, slug := range []string{"busy", "goal"} {
		active, err := client.HasActiveGoal(ctx, threads[slug])
		if err != nil || !active {
			t.Fatal("return switch lost original goal", slug, err)
		}
	}
	mu.Lock()
	badAuth := wrongAuth
	counters, _ := json.Marshal(stageRequests)
	mu.Unlock()
	if badAuth != 0 {
		t.Fatalf("wrong route/auth count=%d", badAuth)
	}
	t.Logf("A→B/cold B unchanged default A/B→A endpoint+auth counters=%s", counters)
	if err := syscall.Kill(daemon.Process.Pid, 0); err != nil {
		t.Fatal("shared daemon exited")
	}
	t.Logf("scoped A→B→A in %s; no restart within either switch, separate cold restart proven; original goals/roster/queue/continuation identities checked", time.Since(started))
}
