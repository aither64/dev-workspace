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
)

const providerAcceptanceNativeSHA = "8880a0384e3020872fe9635098772d10bb436b4fbe4c931ad8625a56dca73e58"

type providerAcceptancePackage struct {
	Schema int `json:"schema"`
	Codex  struct {
		Native     string            `json:"native"`
		Argv       []string          `json:"app_server_argv"`
		Provenance map[string]string `json:"provenance"`
	} `json:"codex"`
}

func providerAcceptanceMetadata(data []byte, native string, expected map[string]string) (providerAcceptancePackage, error) {
	var metadata providerAcceptancePackage
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, err
	}
	if metadata.Schema != 1 || !filepath.IsAbs(metadata.Codex.Native) ||
		filepath.Join(metadata.Codex.Native, "libexec/codex/bin/codex") != native ||
		!reflect.DeepEqual(metadata.Codex.Argv, []string{"--allow-host-provider-switching"}) {
		return metadata, fmt.Errorf("candidate native/launch metadata does not match the selected provider fixture")
	}
	for key, value := range expected {
		if metadata.Codex.Provenance[key] != value {
			return metadata, fmt.Errorf("candidate provenance mismatch: %s", key)
		}
	}
	return metadata, nil
}

func providerAcceptanceLaunch(t *testing.T) (string, []string) {
	t.Helper()
	native, err := filepath.EvalSymlinks(os.Getenv("LIVE_SWITCH_CODEX"))
	if err != nil {
		t.Fatal("LIVE_SWITCH_CODEX must select the exact native", err)
	}
	packageRoot, err := filepath.EvalSymlinks(os.Getenv("LIVE_PROVIDER_PACKAGE"))
	if err != nil || !strings.HasPrefix(packageRoot, "/nix/store/") || !strings.HasPrefix(native, "/nix/store/") {
		t.Fatal("LIVE_PROVIDER_PACKAGE and LIVE_SWITCH_CODEX require real store outputs", err)
	}
	expected := map[string]string{}
	data, err := os.ReadFile("../../../nix/codex-native-source.json")
	if err != nil || json.Unmarshal(data, &expected) != nil {
		t.Fatal("cannot read selected source receipt", err)
	}
	expected["recipeRevision"] = "d960982d3b05c294f3cf98cab3e23798548a0ced"
	expected["repository"] = "github.com/aither64/codex"
	expected["sourcePath"] = "/nix/store/ghyrgph8811q846xnx5i8b5iy6r0bdh7-source"
	data, err = os.ReadFile(filepath.Join(packageRoot, "share/dev-workspace/package.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := providerAcceptanceMetadata(data, native, expected)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{native, filepath.Join(packageRoot, "libexec/codex/libexec/codex/bin/codex")} {
		data, err := os.ReadFile(path)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != providerAcceptanceNativeSHA {
			t.Fatalf("wrong native bytes at %s: %v", path, err)
		}
	}
	t.Logf("provider acceptance package=%s native=%s sha256=%s argv=%q provenance=%v", packageRoot, native, providerAcceptanceNativeSHA, metadata.Codex.Argv, metadata.Codex.Provenance)
	return native, metadata.Codex.Argv
}

func providerAcceptanceEnvironment(t *testing.T, root, home string) []string {
	t.Helper()
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CODEX_HOME=" + home,
		"CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=1", "LANG=C.UTF-8",
		"OPENAI_API_KEY=fixture-openai", "FIXTURE_POOL_TOKEN=fixture-pool",
		"FIXTURE_A_TOKEN=fixture-a", "FIXTURE_B_TOKEN=fixture-b", "FIXTURE_C_TOKEN=fixture-c"}
	for key, dir := range map[string]string{"TMPDIR": "tmp", "CODEX_SQLITE_HOME": "sqlite", "XDG_CONFIG_HOME": "xdg-config", "XDG_STATE_HOME": "xdg-state", "XDG_CACHE_HOME": "xdg-cache"} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		environment = append(environment, key+"="+path)
	}
	return environment
}

// Only synthetic endpoint/token pairs are accepted; count rejected POSTs too.
func providerAcceptanceAuth(path, authorization string, tokens map[string]string) bool {
	for prefix, token := range tokens {
		if strings.HasPrefix(path, prefix+"/") {
			return authorization == "Bearer "+token
		}
	}
	return false
}

func providerAcceptanceStop(t *testing.T, daemon *exec.Cmd) {
	t.Helper()
	if daemon == nil || daemon.Process == nil {
		return
	}
	pid := daemon.Process.Pid
	done := make(chan error, 1)
	go func() { done <- daemon.Wait() }()
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("owned provider daemon did not join")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(-pid, 0) == nil && time.Now().Before(deadline) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(-pid, 0) == nil {
		t.Fatal("owned provider process group survived teardown")
	}
}

func TestProviderAcceptanceMetadataControls(t *testing.T) {
	native := "/nix/store/fixture-native/libexec/codex/bin/codex"
	valid := []byte(`{"schema":1,"codex":{"native":"/nix/store/fixture-native","app_server_argv":["--allow-host-provider-switching"],"provenance":{"revision":"exact"}}}`)
	if _, err := providerAcceptanceMetadata(valid, native, map[string]string{"revision": "exact"}); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte(`{"schema":1}`), []byte(strings.ReplaceAll(string(valid), "--allow-host-provider-switching", "--unknown")), []byte(strings.ReplaceAll(string(valid), "fixture-native", "wrong-native"))} {
		if _, err := providerAcceptanceMetadata(data, native, map[string]string{"revision": "exact"}); err == nil {
			t.Fatal("accepted malformed/missing/wrong candidate metadata")
		}
	}
	if _, err := providerAcceptanceMetadata(valid, native, map[string]string{"revision": "wrong"}); err == nil {
		t.Fatal("accepted stale provenance")
	}
	if !providerAcceptanceAuth("/a/v1/responses", "Bearer a", map[string]string{"/a": "a"}) || providerAcceptanceAuth("/a/v1/responses", "Bearer b", map[string]string{"/a": "a"}) {
		t.Fatal("endpoint/auth admission mismatch")
	}
}

// Run the real private helper and team MCP handler in child test processes.
// All state, credentials, sockets and model responses belong to this fixture.
func TestNativeProviderHelper(t *testing.T) {
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	// Native MCP launches filter their environment. The explicit test-only
	// argument delimiter identifies those children without inherited flags.
	if len(args) == 0 {
		t.Skip("fixture child only")
	}
	var err error
	if len(args) > 0 && args[0] == "team-mcp" {
		err = teamMCPCommand(args[1:], os.Stdin, os.Stdout, os.Stderr, teamMCPDependencies{})
	} else {
		err = codexProviderCommand(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestNativeProviderRouting(t *testing.T) {
	native, serverArgv := providerAcceptanceLaunch(t)
	root, err := os.MkdirTemp("", "provider-routing-native-")
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
	home := filepath.Join(root, "home")
	stateRoot := filepath.Join(root, "state")
	profile := filepath.Join(root, "profile")
	for _, path := range []string{home, stateRoot, filepath.Join(profile, "bin")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(profile, "bin/dev-session"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	routes := map[string]int{}
	wrongAuth := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5.5"}]}`)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		if !providerAcceptanceAuth(r.URL.Path, r.Header.Get("Authorization"), map[string]string{"/subscription": "fixture-openai", "/pool": "fixture-pool"}) {
			wrongAuth++
			mu.Unlock()
			http.Error(w, "wrong synthetic route/auth", http.StatusUnauthorized)
			return
		}
		routes[r.URL.Path]++
		count := routes[r.URL.Path]
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(value any) { data, _ := json.Marshal(value); _, _ = fmt.Fprintf(w, "data: %s\n\n", data) }
		id := fmt.Sprintf("fixture-%d", count)
		event(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
		event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": id + "-answer", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "provider-fixture-complete"}}}})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer backend.Close()
	config := "model_provider = \"openai\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\napproval_policy = \"never\"\n" +
		"openai_base_url = " + strconv.Quote(backend.URL+"/subscription/v1") + "\n" +
		"[model_providers.pool]\nname = \"Fixture pool\"\nwire_api = \"responses\"\nsupports_websockets = false\nenv_key = \"FIXTURE_POOL_TOKEN\"\nbase_url = " + strconv.Quote(backend.URL+"/pool/v1") + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// The built-in provider reads saved auth; App Server does not load API-key env vars.
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{\"auth_mode\":\"apikey\",\"OPENAI_API_KEY\":\"fixture-openai\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(root, "fixture.token")
	if err := os.WriteFile(token, []byte("fixture-pool"), 0600); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(root, "catalog.json")
	catalog := map[string]any{"schema": 1, "initialProvider": "openai", "providers": []any{
		map[string]any{"id": "openai", "label": "Fixture subscription", "kind": "subscription"},
		map[string]any{"id": "pool", "label": "Fixture pool", "kind": "codex-lb", "baseURL": backend.URL + "/pool/v1", "tokenFile": token},
	}}
	data, _ := json.Marshal(catalog)
	if err := os.WriteFile(catalogPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	socket := filepath.Join(root, "app.sock")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := append(providerAcceptanceEnvironment(t, root, home), "PROVIDER_NATIVE_HELPER=1")
	var daemon *exec.Cmd
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	logText := func() string {
		data, _ := os.ReadFile(log.Name())
		if len(data) > 12000 {
			data = data[len(data)-12000:]
		}
		return string(data)
	}
	stop := func() {
		if daemon != nil {
			providerAcceptanceStop(t, daemon)
			daemon = nil
		}
	}
	defer stop()
	currentProvider := ""
	startWithArgs := func(provider string, argv []string) *codex.Client {
		t.Helper()
		currentProvider = provider
		_ = os.Remove(socket)
		args := append([]string{"-c", "model_provider=" + strconv.Quote(provider), "app-server"}, argv...)
		daemon = exec.CommandContext(ctx, native, append(args, "--listen", "unix://"+socket)...)
		daemon.Env, daemon.Dir = environment, root
		daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		daemon.Stdout, daemon.Stderr = log, log
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(socket); err == nil {
				break
			}
			if ctx.Err() != nil || time.Now().After(deadline) {
				t.Fatalf("native startup: %v %s", ctx.Err(), logText())
			}
			time.Sleep(20 * time.Millisecond)
		}
		return codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
	}
	start := func(provider string) *codex.Client { return startWithArgs(provider, serverArgv) }
	client := start("openai")
	defer func() { client.Close() }()
	seed := func(thread providerThread, text string) {
		t.Helper()
		var result map[string]any
		if thread.Original != nil {
			// Bind the retained policy before opening a new native turn, as an
			// application activation does. Native unloads subscriptions owned
			// only by the short-lived helper when its connection closes.
			params, err := providerResumeParams(thread, currentProvider)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Request(ctx, "thread/resume", params, &result); err != nil {
				t.Fatal(err)
			}
		}
		if err := client.Request(ctx, "turn/start", map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": text}}, "model": thread.Settings.Model, "effort": thread.Settings.ReasoningEffort}, &result); err != nil {
			t.Fatal(err)
		}
		for {
			transcript, err := client.ReadThread(ctx, thread.ID)
			if err == nil && strings.Contains(fmt.Sprint(transcript.Entries), "provider-fixture-complete") && client.RequireThreadIdle(ctx, thread.ID, root) == nil {
				return
			}
			if ctx.Err() != nil {
				t.Fatalf("turn did not complete: %v %s", err, logText())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	threads := []providerThread{}
	for index, effort := range []string{"low", "high", "medium"} {
		settings := codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: effort, Policy: codex.ThreadPolicy{Sandbox: "workspace-write"}}
		var id string
		var err error
		if index == 0 {
			var response struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			err = client.Request(ctx, "thread/start", map[string]any{"cwd": root, "model": settings.Model, "permissions": ":workspace", "approvalPolicy": "never", "config": map[string]any{"model_reasoning_effort": effort}}, &response)
			id = response.Thread.ID
		} else {
			id, err = client.StartThreadWithSettings(ctx, root, map[string]string{}, settings)
		}
		if err != nil {
			t.Fatal(err)
		}
		thread := providerThread{ID: id, Slug: "fixture", Cwd: root, Settings: settings, Environment: map[string]string{"DEV_SESSION_SLUG": "fixture"}}
		if index == 1 {
			thread.MemberAddress = "implementer1"
			thread.Environment["DEV_SESSION_MEMBER_ADDRESS"] = "implementer1"
		}
		seed(thread, fmt.Sprintf("materialize-%d", index))
		threads = append(threads, thread)
	}
	// An idle update is newer than the last turn context. Cold capture must
	// retain it, including after a target failure and compensation.
	var updated map[string]any
	if err := client.Request(ctx, "thread/settings/update", map[string]any{"threadId": threads[0].ID, "permissions": ":read-only", "approvalPolicy": "on-request", "serviceTier": "flex"}, &updated); err != nil {
		t.Fatal(err)
	}
	threads[0].Settings.Policy.Sandbox = ""
	mcpArgs := []string{"-test.run=^TestNativeProviderHelper$", "--", "team-mcp", "--user-state-root", stateRoot, "--host-profile", profile, "--workspace", root, "--session-slug", "fixture", "--root-thread-id", threads[0].ID, "--member-address", "implementer1", "--member-thread-id", threads[1].ID}
	threads[1].Settings.Policy.MCPServer = &codex.ThreadMCPServer{Name: "team_fixture", Command: testBinary, Args: mcpArgs, Tool: "report_to_lead"}
	client.Close()
	stop()
	client = start("openai")
	// Bind the real team MCP handler before capture; native keeps it idle.
	if _, err := client.ResumeThreadWithSettings(ctx, threads[1].ID, root, threads[1].Environment, threads[1].Settings); err != nil {
		t.Fatal(err)
	}
	inventory := providerInventory{Schema: 1, Workspace: root, Threads: threads, Loaded: []string{threads[1].ID}, Blockers: []string{}}
	snapshot := filepath.Join(root, "snapshot.json")
	writeSnapshot := func() {
		data, _ := json.Marshal(inventory)
		if err := os.WriteFile(snapshot, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	helper := func(action, target string) []byte {
		t.Helper()
		phaseCtx, cancelPhase := context.WithTimeout(ctx, 20*time.Second)
		defer cancelPhase()
		args := []string{"-test.run=^TestNativeProviderHelper$", "--", action, "--workspace", root, "--state-root", stateRoot, "--socket", socket, "--host-profile", profile, "--snapshot", snapshot, "--catalog", catalogPath}
		if target != "" {
			args = append(args, "--target", target)
		}
		command := exec.CommandContext(phaseCtx, testBinary, args...)
		command.Env = environment
		output, err := command.CombinedOutput()
		if err != nil {
			// Fixture-only diagnostics contain no real prompts or credentials.
			for _, thread := range inventory.Threads {
				var response map[string]any
				if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": thread.ID, "excludeTurns": true}, &response); err == nil {
					t.Logf("fixture thread %s retained sandbox=%s effective=%v profile=%v", thread.ID, thread.Settings.Policy.Sandbox, response["sandbox"], response["activePermissionProfile"])
				}
			}
			t.Fatalf("%s: %v %s; native: %s", action, err, output, logText())
		}
		return output
	}
	prefixes := map[string][]byte{}
	paths := map[string]string{}
	for _, thread := range threads {
		metadata, err := client.ReadThreadMetadata(ctx, thread.ID, false)
		if err != nil || metadata.Path == nil {
			t.Fatalf("materialized history: %v", err)
		}
		paths[thread.ID] = *metadata.Path
		prefixes[thread.ID], err = os.ReadFile(*metadata.Path)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"pool", "openai"} {
		writeSnapshot()
		captured := helper("capture", target)
		if err := json.Unmarshal(captured, &inventory); err != nil {
			t.Fatal(err)
		}
		rootSettings := inventory.Threads[0].Original
		if rootSettings["approvalPolicy"] != "on-request" || rootSettings["serviceTier"] != "flex" || rootSettings["sandbox"].(map[string]any)["type"] != "readOnly" {
			t.Fatalf("cold capture lost the latest idle update: %#v", rootSettings)
		}
		writeSnapshot()
		// Prove required idle MCP children actually remain in the native tree.
		// Linux records children per spawning task. Rust's worker threads can
		// own MCP children even when the main task's children file is empty.
		tasks, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", daemon.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		var childIDs []string
		mcpPresent := false
		for _, path := range tasks {
			children, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range strings.Fields(string(children)) {
				childIDs = append(childIDs, id)
				executable, _ := os.Readlink("/proc/" + id + "/exe")
				if executable == testBinary {
					mcpPresent = true
				}
			}
		}
		if !mcpPresent {
			t.Fatal("native capture did not retain the required idle MCP child")
		}
		t.Logf("%s: idle native children=%v", target, childIDs)
		client.Close()
		stop()
		if target == "pool" {
			// Deliberate upstream default-off control, separate from the
			// candidate's fixed flagged positive invocation.
			client = startWithArgs(target, []string{})
			mu.Lock()
			before := routes["/subscription/v1/responses"] + routes["/pool/v1/responses"]
			mu.Unlock()
			var refused any
			if err := client.Request(ctx, "thread/resume", map[string]any{"threadId": threads[0].ID, "excludeTurns": true, "modelProvider": "openai", "config": map[string]any{"model_provider": "openai"}}, &refused); err == nil || !strings.Contains(err.Error(), "current provider is pool") {
				t.Fatal("strict [] launch did not enforce the saved-provider mismatch", err)
			}
			mu.Lock()
			after := routes["/subscription/v1/responses"] + routes["/pool/v1/responses"]
			mu.Unlock()
			if before != after {
				t.Fatal("strict refusal made an inference request")
			}
			t.Log("strict app_server_argv=[] refused saved A/current B despite request override")
			client.Close()
			stop()
		}
		client = start(target)
		helper("resume", target)
		helper("verify", target)
		helper("health", "")
		if target == "pool" {
			// Simulate failure after target mutation, then recover each original
			// provider through the same native helper used by compensation.
			client.Close()
			stop()
			client = start("openai")
			helper("resume", "")
			helper("verify", "")
			client.Close()
			stop()
			client = start("pool")
			helper("resume", "pool")
			helper("verify", "pool")
		}
		for _, thread := range inventory.Threads {
			data, err := os.ReadFile(paths[thread.ID])
			if err != nil || !bytes.HasPrefix(data, prefixes[thread.ID]) {
				t.Fatalf("history prefix changed for %s: %v", thread.ID, err)
			}
			seed(thread, "after-"+target)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if wrongAuth != 0 || routes["/subscription/v1/responses"] < 6 || routes["/pool/v1/responses"] != 3 {
		t.Fatalf("wrong effective routes: %#v", routes)
	}
	t.Logf("both directions retained IDs/model/effort/sandbox/history; inference routes=%v", routes)
}
