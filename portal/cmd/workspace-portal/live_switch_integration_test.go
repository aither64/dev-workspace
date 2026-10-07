//go:build live_switch_integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/coder/websocket"
)

// This explicitly selected fixture owns only temporary state, a loopback model
// provider and one native App Server. It never opens the installed workspace.
func TestNativeLiveSwitchReconnect(t *testing.T) {
	native := os.Getenv("LIVE_SWITCH_CODEX")
	if !filepath.IsAbs(native) {
		t.Fatal("LIVE_SWITCH_CODEX must select the exact native executable")
	}
	version, err := exec.Command(native, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native=%s version=%s sha256=%x", native, strings.TrimSpace(string(version)), sha256.Sum256(bytes))
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(hold) })
	entered := make(chan struct{}, 4)
	var mu sync.Mutex
	requests := map[string]int{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The exact native provider probes Responses WebSocket support. Signal
		// the supported HTTP fallback, as the native naming fixture does.
		if r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		kind := "ordinary"
		for _, candidate := range []string{"approval-probe", "question-probe"} {
			if strings.Contains(string(data), candidate) {
				kind = candidate
			}
		}
		mu.Lock()
		requests[kind]++
		index := requests[kind]
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(value any) { encoded, _ := json.Marshal(value); _, _ = fmt.Fprintf(w, "data: %s\n\n", encoded) }
		event(map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("probe-%s-%d", kind, index)}})
		w.(http.Flusher).Flush()
		if kind == "ordinary" {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		item := map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprintf("answer-%s-%d", kind, index), "phase": "final_answer",
			"content": []any{map[string]any{"type": "output_text", "text": "synthetic-completed"}}}
		if kind != "ordinary" && index == 1 {
			name, arguments := "exec_command", `{"cmd":"printf synthetic-approved","sandbox_permissions":"require_escalated","justification":"Synthetic reconnect approval probe"}`
			if kind == "question-probe" {
				name, arguments = "request_user_input", `{"questions":[{"id":"choice","header":"Choice","question":"Choose the synthetic result","options":[{"label":"Continue","description":"Complete the probe"},{"label":"Stop","description":"Stop the probe"}]}]}`
			}
			item = map[string]any{"type": "function_call", "id": "call-item-" + kind, "call_id": "call-" + kind, "name": name, "arguments": arguments}
		}
		event(map[string]any{"type": "response.output_item.done", "item": item})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("probe-%s-%d", kind, index),
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer provider.Close()
	config := "model_provider = \"openai\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\napproval_policy = \"on-request\"\nopenai_base_url = " + strconv.Quote(provider.URL+"/v1") + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	socket := filepath.Join(root, "app.sock")
	command := exec.CommandContext(ctx, native, "app-server", "--listen", "unix://"+socket)
	command.Dir = root
	command.Env = []string{"HOME=" + root, "CODEX_HOME=" + home, "PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=synthetic-only"}
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = command.Wait() }()
	var lastIdleError error
	wait := func(label string, predicate func() bool) {
		t.Helper()
		for !predicate() {
			select {
			case <-ctx.Done():
				data, _ := os.ReadFile(log.Name())
				mu.Lock()
				counts, _ := json.Marshal(requests)
				mu.Unlock()
				t.Fatalf("%s: %v; last idle check: %v; provider requests: %s; native log: %s", label, ctx.Err(), lastIdleError, counts, data)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	wait("native socket", func() bool { _, err := os.Stat(socket); return err == nil })
	client := codex.New(socket)
	defer func() { client.Close() }()
	idle := func(id string) bool {
		err := client.RequireThreadIdle(ctx, id, root)
		if err != nil && ctx.Err() == nil {
			lastIdleError = err
		}
		return err == nil
	}
	start := func(mode string) string {
		t.Helper()
		id, err := client.StartThreadWithSettings(ctx, root, map[string]string{}, codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low", CollaborationMode: mode, Policy: codex.ThreadPolicy{Sandbox: "workspace-write"}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	initial := func(id, text, mode string) codex.SendReceipt {
		t.Helper()
		// A fresh thread has no saved rollout yet. Seed it through turn/start,
		// as the native naming smoke does, before the normal client resume path.
		var result struct {
			Turn struct{ ID string } `json:"turn"`
		}
		params := map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": text}},
			"model": "gpt-5.5", "effort": "low", "clientUserMessageId": text}
		if mode == "plan" {
			params["collaborationMode"] = map[string]any{"mode": "plan", "settings": map[string]any{
				"model": "gpt-5.5", "reasoning_effort": "low", "developer_instructions": nil}}
		}
		if err := client.Request(ctx, "turn/start", params, &result); err != nil || result.Turn.ID == "" {
			t.Fatalf("initial native turn: %v", err)
		}
		return codex.SendReceipt{TurnID: result.Turn.ID}
	}
	lead, member := start("default"), start("default")
	leadReceipt := initial(lead, "running-lead", "default")
	memberReceipt := initial(member, "running-member", "default")
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("both native turns did not reach the provider")
		}
	}
	queued, err := client.Queue(ctx, lead, "queued-after-reconnect", "queued-probe")
	if err != nil {
		t.Fatal(err)
	}
	reconnect := func(ids ...string) {
		t.Helper()
		client.Close()
		client = codex.New(socket)
		for _, id := range ids {
			if _, err := client.ResumeThread(ctx, id, root, map[string]string{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	completed := func(id, turn string) codex.Transcript {
		t.Helper()
		transcript, err := client.ReadThread(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range transcript.Entries {
			if entry.TurnID == turn && entry.TurnStatus == "completed" && entry.Kind == "agentMessage" && entry.Text == "synthetic-completed" {
				return transcript
			}
		}
		t.Fatalf("original turn %s did not complete its synthetic response: %#v", turn, transcript)
		return transcript
	}
	reconnect(lead, member)
	for id, turn := range map[string]string{lead: leadReceipt.TurnID, member: memberReceipt.TurnID} {
		transcript, err := client.ReadThread(ctx, id)
		if err != nil || transcript.Status != "active" || transcript.LatestTurnID != turn {
			t.Fatalf("running turn did not survive reconnect: %#v, %v", transcript, err)
		}
	}
	queuedAgain, err := client.Queue(ctx, lead, "queued-after-reconnect", "queued-probe")
	if err != nil || queuedAgain.ID != queued.ID {
		t.Fatalf("queue retry identity changed: %#v, %v", queuedAgain, err)
	}
	release.Do(func() { close(hold) })
	wait("lead and member completion", func() bool {
		return idle(lead) && idle(member)
	})
	completed(lead, leadReceipt.TurnID)
	completed(member, memberReceipt.TurnID)
	transcript, err := client.ReadThread(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range transcript.Entries {
		if entry.ClientUserMessageID == "queued-probe" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("queued message delivered %d times", count)
	}
	// Accepted reports use the same durable send ledger as team assignments.
	// A retained retry after reconnect must resolve to the existing turn.
	report, err := client.Send(ctx, lead, "synthetic member report", "report-probe", "team:report")
	if err != nil {
		t.Fatal(err)
	}
	wait("report completion", func() bool { return idle(lead) })
	completed(lead, report.TurnID)
	reconnect(lead)
	retried, err := client.Send(ctx, lead, "synthetic member report", "report-probe", "team:report")
	if err != nil || retried.TurnID != report.TurnID {
		t.Fatalf("report retry changed native turn: %#v, %v", retried, err)
	}
	transcript, err = client.ReadThread(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	count = 0
	for _, entry := range transcript.Entries {
		if entry.ClientUserMessageID == "report-probe" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("report delivered %d times", count)
	}
	for _, probe := range []struct{ text, mode, kind string }{{"approval-probe", "default", "command"}, {"question-probe", "plan", "userInput"}} {
		thread := start(probe.mode)
		original := initial(thread, probe.text, probe.mode)
		wait("initial "+probe.kind, func() bool { return len(client.Prompts(thread)) == 1 })
		before, err := client.PromptsWithItems(ctx, thread)
		if err != nil || len(before) != 1 {
			t.Fatalf("initial prompt authority: %#v, %v", before, err)
		}
		reconnect(thread)
		wait("reconnected "+probe.kind, func() bool { return len(client.Prompts(thread)) == 1 })
		after, err := client.PromptsWithItems(ctx, thread)
		if err != nil || len(after) != 1 {
			t.Fatalf("reconnected prompt authority: %#v, %v", after, err)
		}
		prompt := after[0]
		if prompt.Kind != probe.kind || prompt.ThreadID != thread || prompt.TurnID != original.TurnID || prompt.ItemID != before[0].ItemID {
			t.Fatalf("wrong blocking request: %#v", prompt)
		}
		if before[0].AuthorityAvailable && !prompt.AuthorityAvailable {
			t.Fatal("approval authority was lost during reconnect")
		}
		t.Logf("%s authority before/after reconnect: %t/%t", probe.kind, before[0].AuthorityAvailable, prompt.AuthorityAvailable)
		if probe.kind == "userInput" {
			err = client.RespondAnswers(ctx, prompt.ID, thread, map[string]map[string][]string{"choice": {"answers": {"Continue"}}})
		} else {
			err = client.RespondDecision(ctx, prompt.ID, thread, "accept")
			if err != nil && strings.Contains(err.Error(), "matching approval item is unavailable") {
				// Native 0.160 omits this pending command item. Keep
				// the browser's authority guard and exercise its terminal fallback
				// on a fresh connection to the same isolated native process.
				client.Close()
				terminal := liveSwitchTerminalApproval(t, ctx, socket, root, thread, prompt.ItemID)
				defer terminal.CloseNow()
				client = codex.New(socket)
				_, err = client.ResumeThread(ctx, thread, root, map[string]string{})
				t.Log("replayed approval completed through native terminal fallback; browser authority guard retained")
			}
		}
		if err != nil {
			var items map[string]any
			itemsErr := client.Request(ctx, "thread/items/list", map[string]any{"threadId": thread, "limit": 100, "sortDirection": "desc"}, &items)
			encoded, _ := json.Marshal(items)
			t.Fatalf("respond to %s: %v; replayed prompt: %#v; items: %s (%v)", probe.kind, err, prompt, encoded, itemsErr)
		}
		wait("resolved "+probe.kind, func() bool { return idle(thread) })
		transcript := completed(thread, original.TurnID)
		if probe.kind == "command" {
			executed := false
			for _, entry := range transcript.Entries {
				if entry.TurnID == original.TurnID && entry.ItemID == prompt.ItemID && entry.Kind == "commandExecution" && entry.TurnStatus == "completed" && strings.Contains(entry.Details, "synthetic-approved") {
					executed = true
				}
			}
			if !executed {
				t.Fatalf("replayed approval did not execute its original command: %#v", transcript)
			}
		}
	}
	if command.ProcessState != nil || command.Process.Pid == 0 {
		t.Fatal("native process exited across portal reconnects")
	}
	t.Logf("preserved native PID %d; active turns, queue/report retries, approval and question completed", command.Process.Pid)
}

// Emulate the terminal's JSON-RPC response to a replayed native command request.
// This never uses a workspace socket or bypasses the browser's approval guard.
func liveSwitchTerminalApproval(t *testing.T, ctx context.Context, socket, cwd, thread, item string) *websocket.Conn {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	connection, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.CloseNow() })
	write := func(message any) {
		t.Helper()
		data, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"id": "terminal-init", "method": "initialize", "params": map[string]any{
		"clientInfo":   map[string]any{"name": "live-switch-terminal", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true}}})
	for {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Error  json.RawMessage `json:"error"`
			Params struct {
				ThreadID string `json:"threadId"`
				ItemID   string `json:"itemId"`
			} `json:"params"`
		}
		_, data, err := connection.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if len(message.Error) > 0 {
			t.Fatalf("native terminal request: %s", message.Error)
		}
		if string(message.ID) == `"terminal-init"` {
			write(map[string]any{"method": "initialized"})
			write(map[string]any{"id": "terminal-resume", "method": "thread/resume", "params": map[string]any{
				"threadId": thread, "cwd": cwd, "excludeTurns": true}})
		}
		if message.Method == "item/commandExecution/requestApproval" {
			if message.Params.ThreadID != thread || message.Params.ItemID != item || len(message.ID) == 0 {
				t.Fatal("terminal approval replay changed thread or item identity")
			}
			write(map[string]any{"id": message.ID, "result": map[string]any{"decision": "accept"}})
			return connection
		}
	}
}
