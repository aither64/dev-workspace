//go:build live_switch_integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

// Child roles use only disposable fixture state. The dispatcher stands in for
// the stable profile CLI; ordinary CLI locks/deduplication are covered separately.
func TestNativeReportingHelper(t *testing.T) {
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		t.Skip("fixture child only")
	}
	var err error
	if args[0] == "team-mcp" {
		err = teamMCPCommand(args[1:], os.Stdin, os.Stdout, os.Stderr, teamMCPDependencies{})
	} else {
		err = func() error {
			trace := args[0]
			args = args[1:]
			if len(args) != 11 || args[0] != "team" || args[1] != "assign" || args[4] != "--from" || args[6] != "--to" || args[7] != "lead" || args[8] != "--message-stdin" {
				return fmt.Errorf("invalid host CLI arguments")
			}
			text, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			thread := os.Getenv("DEV_SESSION_EXPECTED_MEMBER_THREAD_ID")
			if thread == "" || os.Getenv("DEV_SESSION_MEMBER_ADDRESS") != args[5] || os.Getenv("DEV_SESSION_EXPECTED_ROOT_THREAD_ID") == "" {
				return fmt.Errorf("host identity missing")
			}
			f, err := os.OpenFile(trace, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
				return err
			}
			defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
			data, err := io.ReadAll(f)
			if err != nil {
				return err
			}
			rows := map[string]map[string]string{}
			if len(data) > 0 {
				if err := json.Unmarshal(data, &rows); err != nil {
					return err
				}
			}
			id := args[10]
			row := map[string]string{"thread": thread, "slug": args[2], "from": args[5], "lead": os.Getenv("DEV_SESSION_EXPECTED_ROOT_THREAD_ID"), "text": string(text)}
			if old, ok := rows[id]; ok {
				if fmt.Sprint(old) != fmt.Sprint(row) {
					return fmt.Errorf("retry changed identity or text")
				}
			} else {
				rows[id] = row
			}
			encoded, _ := json.Marshal(rows)
			if _, err := f.Seek(0, 0); err != nil {
				return err
			}
			if err := f.Truncate(0); err != nil {
				return err
			}
			if _, err := f.Write(encoded); err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(codex.SendReceipt{TurnID: "fixture-lead-turn", ClientUserMessageID: id})
		}()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestNativeWorkspaceReportingWarmUpgrade(t *testing.T) {
	native := os.Getenv("LIVE_SWITCH_CODEX")
	if !filepath.IsAbs(native) {
		t.Fatal("LIVE_SWITCH_CODEX must select pinned native")
	}
	version, err := exec.Command(native, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native %s", bytes.TrimSpace(version))
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	profile := filepath.Join(root, "custom-profile")
	trace := filepath.Join(root, "host-reports.json")
	for _, dir := range []string{home, filepath.Join(profile, "bin")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Use the production helper's exact argv for real /proc ownership checks.
	packageRoot := filepath.Join(root, "reporting-package")
	portalCommand := filepath.Join(packageRoot, "bin/workspace-portal")
	contractDir := filepath.Join(packageRoot, "share/workspace-portal")
	for _, dir := range []string{filepath.Dir(portalCommand), contractDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-mod=readonly", "-o", portalCommand, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production reporting helper: %v %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(contractDir, "runtime-contract.json"), []byte(`{"workspaceReportingPolicy":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Exact test-only path quoting; no credentials or live host dispatch.
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestNativeReportingHelper$ -- '" + trace + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(profile, "bin/dev-session"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	sequence := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []map[string]any `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "bad fixture request", 400)
			return
		}
		text := ""
		toolOutput := false
		for _, item := range request.Input {
			if item["role"] == "user" {
				data, _ := json.Marshal(item["content"])
				text = string(data)
				toolOutput = false
			}
			if item["type"] == "function_call_output" {
				toolOutput = true
			}
		}
		mu.Lock()
		sequence++
		n := sequence
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(v any) { data, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", data) }
		id := fmt.Sprintf("report-fixture-%d", n)
		event(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
		if strings.Contains(text, "send-report") && !toolOutput {
			// Prompt embeds a stable UUID after send-report. The pinned native expects
			// namespace and unflattened name, matching real model-generated calls.
			start := strings.Index(text, "send-report ") + len("send-report ")
			messageID := text[start : start+32]
			arguments, _ := json.Marshal(map[string]string{"message": "native-model-report", "message_id": messageID})
			item := map[string]any{"type": "function_call", "id": id + "-tool", "call_id": id + "-call", "namespace": "mcp__team_reporting", "name": "report_to_lead", "arguments": string(arguments)}
			event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
			event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		} else {
			event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": id + "-answer", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "report-fixture-complete"}}}})
		}
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer backend.Close()
	config := "model_provider=\"fixture\"\nmodel=\"gpt-5.5\"\napproval_policy=\"never\"\n[model_providers.fixture]\nname=\"Fixture\"\nwire_api=\"responses\"\nsupports_websockets=false\nenv_key=\"OPENAI_API_KEY\"\nbase_url=" + strconv.Quote(backend.URL+"/v1") + "\n"
	unrelatedArgs, _ := json.Marshal([]string{"-test.run=^TestNativeReportingHelper$", "--", "team-mcp", "--user-state-root", state, "--workspace", root, "--host-profile", profile})
	config += "[mcp_servers.unrelated]\ncommand=" + strconv.Quote(binary) + "\nargs=" + string(unrelatedArgs) + "\nenabled_tools=[\"report_to_lead\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	socket := filepath.Join(root, "app.sock")
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var daemon *exec.Cmd
	var client *codex.Client
	var reportingArgv []string
	stop := func() {
		if client != nil {
			client.Close()
			client = nil
		}
		if daemon != nil {
			syscall.Kill(-daemon.Process.Pid, syscall.SIGTERM)
			daemon.Wait()
			daemon = nil
		}
	}
	defer stop()
	start := func(reporting bool) {
		t.Helper()
		os.Remove(socket)
		argv := []string{}
		if reporting {
			launch := map[string]any{"command": portalCommand, "args": []string{"team-mcp", "--user-state-root", state, "--workspace", root, "--host-profile", profile}, "required": true, "enabled_tools": []string{"report_to_lead"}, "tools.report_to_lead.approval_mode": "approve"}
			// Only this owned subtree changes; the unowned fixture server survives.
			for _, key := range []string{"command", "args", "required", "enabled_tools", "tools.report_to_lead.approval_mode"} {
				data, _ := json.Marshal(launch[key])
				argv = append(argv, "-c", "mcp_servers.team_reporting."+key+"="+string(data))
			}
			reportingArgv = append([]string(nil), argv...)
		}
		argv = append(argv, "app-server", "--listen", "unix://"+socket)
		daemon = exec.CommandContext(ctx, native, argv...)
		daemon.Env = []string{"HOME=" + root, "CODEX_HOME=" + home, "PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=synthetic-only"}
		daemon.Dir = root
		daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		daemon.Stdout = log
		daemon.Stderr = log
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		for {
			if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
				break
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			time.Sleep(20 * time.Millisecond)
		}
		client = codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
	}
	start(false)
	settings := codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low", Policy: codex.ThreadPolicy{Sandbox: "read-only"}}
	store, err := teamruntime.NewStore(state, root)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	seed := func(id, text string) {
		t.Helper()
		var result map[string]any
		if err := client.Request(ctx, "turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": text}}, "model": "gpt-5.5", "effort": "low"}, &result); err != nil {
			t.Fatal(err)
		}
		for {
			if client.RequireThreadIdle(ctx, id, root) == nil {
				return
			}
			if ctx.Err() != nil {
				body, _ := os.ReadFile(log.Name())
				t.Fatalf("native turn: %v %s", ctx.Err(), body)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for i := 0; i < 2; i++ {
		id, err := client.StartThreadWithSettings(ctx, root, map[string]string{}, settings)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		seed(id, "materialize-predecessor")
	}
	// Warm members were materialized before the reporting server existed.
	for i, id := range ids {
		slug := fmt.Sprintf("session-%d", i)
		dir := filepath.Join(root, "work", slug)
		os.MkdirAll(dir, 0700)
		os.WriteFile(filepath.Join(dir, "portal.yml"), []byte(fmt.Sprintf("schema: 1\nslug: %s\ncodex:\n  thread_id: lead-%d\n", slug, i)), 0600)
		os.WriteFile(filepath.Join(dir, "state.md"), []byte("---\nlifecycle: active\n---\n"), 0600)
		os.WriteFile(filepath.Join(dir, "plan.md"), []byte("fixture\n"), 0600)
		_, err := store.Update(ctx, slug, fmt.Sprintf("lead-%d", i), true, func(r *teamruntime.Roster) error {
			r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", State: "ready", Thread: id, AddedAt: time.Now().UTC(), Behavior: "implementer", Access: "workspace_write"}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	stop()
	start(true)
	for _, id := range ids {
		if _, err := client.ResumeThreadWithSettings(ctx, id, root, map[string]string{}, settings); err != nil {
			t.Fatal(err)
		}
	}
	var inventory map[string]any
	if err := client.Request(ctx, "mcpServerStatus/list", map[string]any{"threadId": ids[0]}, &inventory); err != nil {
		t.Fatal(err)
	}
	inventoryJSON, _ := json.Marshal(inventory)
	if !bytes.Contains(inventoryJSON, []byte(`"name":"team_reporting"`)) || !bytes.Contains(inventoryJSON, []byte(`"name":"unrelated"`)) {
		t.Fatalf("MCP startup replaced unrelated servers: %s", inventoryJSON)
	}

	for i, id := range ids {
		seed(id, "send-report "+strings.Repeat(strconv.Itoa(i+1), 32))
	}
	check := func(count int) {
		t.Helper()
		data, err := os.ReadFile(trace)
		if err != nil {
			body, _ := os.ReadFile(log.Name())
			t.Fatalf("no actual host report: %v %s", err, body)
		}
		var rows map[string]map[string]string
		if json.Unmarshal(data, &rows) != nil || len(rows) != count {
			t.Fatalf("host reports %s", data)
		}
		for i, id := range ids {
			key := strings.Repeat(strconv.Itoa(i+1), 32)
			if rows[key]["thread"] != id || rows[key]["slug"] != fmt.Sprintf("session-%d", i) {
				t.Fatalf("wrong native routing %s", data)
			}
		}
	}
	check(2)
	assertRestartOwnership := func() {
		t.Helper()
		argv, _ := json.Marshal(reportingArgv)
		guard := exec.CommandContext(ctx, "ruby", "../../../test/fixtures/native_reporting_restart_guard.rb",
			"../../../libexec/workspace-host", root, state, profile, packageRoot, native,
			strconv.Itoa(daemon.Process.Pid), string(argv))
		if output, err := guard.CombinedOutput(); err != nil {
			t.Fatalf("real provider restart ownership: %v %s", err, output)
		} else {
			t.Logf("%s", bytes.TrimSpace(output))
		}
	}
	assertRestartOwnership()
	// A forged _meta is overwritten by native; duplicate ID gets the same receipt
	// and does not produce another host message. Reconnect resumes a warm member.
	client.Close()
	client = codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
	for i, id := range ids {
		if _, err := client.ResumeThreadWithSettings(ctx, id, root, map[string]string{}, settings); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := client.Request(ctx, "mcpServer/tool/call", map[string]any{"threadId": id, "server": "team_reporting", "tool": "report_to_lead", "arguments": map[string]string{"message": "native-model-report", "message_id": strings.Repeat(strconv.Itoa(i+1), 32)}, "_meta": map[string]string{"threadId": "forged-thread"}}, &result); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		if !bytes.Contains(encoded, []byte("fixture-lead-turn")) {
			t.Fatalf("retry receipt %s", encoded)
		}
	}
	check(2)
	stop()
	start(true)
	for i, id := range ids {
		if _, err := client.ResumeThreadWithSettings(ctx, id, root, map[string]string{}, settings); err != nil {
			t.Fatal(err)
		}
		seed(id, "send-report "+strings.Repeat(strconv.Itoa(i+1), 32))
	}
	check(2)
	assertRestartOwnership()
	// A new thread and a native fork obtain the same daemon-start tool without
	// adding anything through resume. Their independent saved roster is unchanged.
	fresh, err := client.StartThreadWithSettings(ctx, root, map[string]string{}, settings)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := client.ForkThread(ctx, ids[0], root, map[string]string{}, settings)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{fresh, fork} {
		slug := fmt.Sprintf("new-%d", i)
		dir := filepath.Join(root, "work", slug)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"portal.yml": fmt.Sprintf("schema: 1\nslug: %s\ncodex:\n  thread_id: new-lead-%d\n", slug, i), "state.md": "---\nlifecycle: active\n---\n", "plan.md": "new fixture\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		roster, err := store.Update(ctx, slug, fmt.Sprintf("new-lead-%d", i), true, func(r *teamruntime.Roster) error {
			r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", State: "ready", Thread: id, AddedAt: time.Now().UTC(), Behavior: "implementer", Access: "workspace_write"}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(roster)
		seed(id, "send-report "+strings.Repeat(strconv.Itoa(i+3), 32))
		after, err := store.Load(slug, roster.RootThreadID)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(after)
		if !bytes.Equal(before, encoded) {
			t.Fatal("report changed saved roster/settings")
		}
	}
	check(4)
}
