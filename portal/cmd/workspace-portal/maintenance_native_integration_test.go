//go:build live_switch_integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This opt-in fixture exercises the production bounded pause and stable resume
// clients against the pinned native. Activation is an owned local stop/restart,
// not Ruby/systemd/profile acceptance. Run only after committed review and a
// separate execution allocation. Compile with -tags=live_switch_integration.
func TestNativeWorkspaceBoundedPause(t *testing.T) {
	native := os.Getenv("LIVE_SWITCH_CODEX")
	if !filepath.IsAbs(native) {
		t.Fatal("LIVE_SWITCH_CODEX requires an exact native executable")
	}
	native, err := filepath.EvalSymlinks(native)
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(native, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.165.0" {
		t.Fatal(string(version), err)
	}
	data, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native=%s sha256=%x", native, sha256.Sum256(data))
	for _, test := range []struct {
		mode, strict string
		interrupt    bool
	}{
		{"code_mode", "absent", false}, {"code_mode_only", "absent", false},
		{"code_mode_only", "false", false}, {"code_mode_only", "true", false}, {"code_mode", "absent", true},
	} {
		name := test.mode + "/strict=" + test.strict
		if test.interrupt {
			name = "timed-interruption/" + name
		}
		t.Run(name, func(t *testing.T) { maintenanceNativePauseCase(t, native, test.mode, test.strict, test.interrupt) })
	}
}

func maintenanceNativePauseCase(t *testing.T, native, mode, strict string, interrupt bool) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "wmn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { err := os.RemoveAll(root); t.Logf("fixture-owned root=%s removed=%t", root, err == nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	home, cwd := filepath.Join(root, "home"), filepath.Join(root, "work/actor")
	packageRoot, profile := filepath.Join(root, "package"), filepath.Join(root, "profile")
	for _, path := range []string{home, cwd, filepath.Join(packageRoot, "bin")} {
		if err = os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Symlink(packageRoot, profile); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var resumed atomic.Int64
	sourceRelease := make(chan struct{})
	release := sync.OnceFunc(func() { close(sourceRelease) })
	defer release()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		var request struct {
			Input []map[string]any `json:"input"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		input, _ := json.Marshal(request.Input)
		if bytes.Contains(input, []byte(maintenanceResumeText)) {
			resumed.Add(1)
		} else {
			select {
			case <-sourceRelease:
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			}
		}
		n := calls.Add(1)
		id := fmt.Sprintf("bounded-response-%d", n)
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(value any) { data, _ := json.Marshal(value); fmt.Fprintf(w, "data: %s\n\n", data) }
		event(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
		event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "id": id + "-answer", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "Saved checkpoint; inspect effects before retrying."}}}})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer func() { cancel(); release(); provider.Close() }()
	catalog, _ := json.Marshal(map[string]any{"models": []any{map[string]any{
		"slug": "maintenance-fixture", "display_name": "Maintenance fixture", "description": "Synthetic provider",
		"model_messages":          map[string]any{"instructions_template": "Synthetic maintenance acceptance actor."},
		"default_reasoning_level": "low", "supported_reasoning_levels": []any{map[string]any{"effort": "low", "description": "Fixture"}},
		"shell_type": "shell_command", "visibility": "list", "supported_in_api": true, "priority": 1,
		"availability_nux": nil, "upgrade": nil, "support_verbosity": false, "default_verbosity": nil,
		"apply_patch_tool_type": "freeform", "truncation_policy": map[string]any{"mode": "tokens", "limit": 10000},
		"experimental_supported_tools": []string{"clock"}, "tool_mode": mode, "supports_parallel_tool_calls": true,
		"context_window": 100000, "supports_search_tool": false,
	}}})
	catalogPath := filepath.Join(home, "fixture-models.json")
	if err := os.WriteFile(catalogPath, catalog, 0444); err != nil {
		t.Fatal(err)
	}
	config := "model_provider=\"fixture\"\nmodel=\"maintenance-fixture\"\nmodel_catalog_json=" + strconv.Quote(catalogPath) + "\napproval_policy=\"never\"\n"
	config += "[features]\ngoals=true\n"
	if strict != "absent" {
		config += "code_mode_only_strict_3p_tools=" + strict + "\n"
	}
	config += "[features.code_mode]\nenabled=true\ndirect_only_tool_namespaces=[\"mcp__operator_retained\"]\n"
	config += "[model_providers.fixture]\nname=\"Fixture\"\nwire_api=\"responses\"\nsupports_websockets=false\nrequires_openai_auth=false\nrequest_max_retries=0\nstream_max_retries=0\nbase_url=" + strconv.Quote(provider.URL+"/v1") + "\n"

	if err = os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "app.sock")
	log, err := os.Create(filepath.Join(root, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var command *exec.Cmd
	var done chan error
	running := false
	wait := func(label string, condition func() bool) {
		t.Helper()
		for !condition() {
			select {
			case <-ctx.Done():
				t.Fatal(label, ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	start := func() {
		t.Helper()
		_ = os.Remove(socket)
		command = exec.Command(native, "app-server", "--strict-config", "--listen", "unix://"+socket)
		command.Env = []string{"HOME=" + root, "CODEX_HOME=" + home, "PATH=" + os.Getenv("PATH")}
		command.Dir = root
		command.Stdout = log
		command.Stderr = log
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
		running = true
		done = make(chan error, 1)
		go func(cmd *exec.Cmd, ch chan<- error) {
			err := cmd.Wait()
			t.Logf("fixture-owned native pid=%d wait=returned exit=%d", cmd.Process.Pid, cmd.ProcessState.ExitCode())
			ch <- err
		}(command, done)
		wait("socket", func() bool { info, err := os.Stat(socket); return err == nil && info.Mode()&os.ModeSocket != 0 })
	}
	stop := func() {
		t.Helper()
		if !running {
			return
		}
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			running = false
			if err != nil {
				t.Fatal("ordinary owned source stop", err)
			}
		case <-ctx.Done():
			t.Fatal("owned source stop", ctx.Err())
		}
	}
	defer func() {
		release()
		if running {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
			<-done
		}
	}()
	start()
	client := codex.NewWithOptions(socket, codex.ClientOptions{AllowImplicitResume: func(string) bool { return false }})
	actor, err := client.StartThreadWithSettings(ctx, cwd, map[string]string{}, codex.ThreadSettings{Model: "maintenance-fixture", ReasoningEffort: "low", Policy: codex.ThreadPolicy{Sandbox: "read-only", ApprovalPolicy: "never"}})
	if err != nil {
		t.Fatal(err)
	}
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err = client.Request(ctx, "turn/start", map[string]any{"threadId": actor, "input": []any{map[string]any{"type": "text", "text": "bounded-pause-work"}}, "model": "maintenance-fixture", "effort": "low"}, &turn); err != nil || turn.Turn.ID == "" {
		t.Fatal("turn/start response", turn, err)
	}
	for name, text := range map[string]string{"plan.md": "Native bounded pause fixture\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": fmt.Sprintf("schema: 1\nslug: actor\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.165.0\n", actor, socket)} {
		if err = os.WriteFile(filepath.Join(cwd, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Seed real durable queue and goal rows while the source is active. Maintenance
	// does not require queue emptiness and never deletes these rows to make progress.
	const queueClientID = "55555555-5555-4555-8555-555555555555"
	const queueText = "retained-maintenance-queue"
	var queued struct {
		Submission struct {
			ID string `json:"id"`
		} `json:"queuedSubmission"`
	}
	if err = client.Request(ctx, "thread/queue/add", map[string]any{"threadId": actor, "clientUserMessageId": queueClientID, "input": []any{map[string]any{"type": "text", "text": queueText}}}, &queued); err != nil || queued.Submission.ID == "" {
		t.Fatal("thread/queue/add response", queued, err)
	}
	if err = client.Request(ctx, "thread/goal/set", map[string]any{"threadId": actor, "objective": "Retain this maintenance fixture goal.", "status": "active", "origin": "user"}, nil); err != nil {
		t.Fatal(err)
	}
	m := maintenanceControl{store: maintenance.Store{Root: filepath.Join(root, "state"), Profile: profile}, workspace: root}
	epoch, err := session.SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := processIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := trackingIncarnation(root, "actor")
	if err != nil {
		t.Fatal(err)
	}
	preparation, _ := json.Marshal(map[string]any{"pauseTimeout": 1})
	p := maintenance.Participant{Workspace: root, Slug: "actor", Incarnation: inc, Root: actor, Thread: actor, Cwd: cwd, SourceTurn: turn.Turn.ID, SocketEpoch: epoch, NoticeID: maintenanceUUID("native-notice"), NoticeAction: "native-pause", NoticeText: "Save a checkpoint and finish without starting more work.", NoticeDisposition: "prepared", ContinuationID: maintenanceUUID("native-resume"), ContinuationDisposition: "pending"}
	o := maintenance.Operation{Schema: maintenance.Schema, OperationID: controlOperationID, Revision: 1, Phase: "prepared", Profile: profile, SourceProfileToken: "fixture", SourcePackage: packageRoot, CandidatePackage: packageRoot, PlansDigest: "fixture", Preparation: preparation, Services: []maintenance.Service{{Workspace: root, Unit: "fixture-owned", Native: native, PID: command.Process.Pid, ProcessIdentity: identity, Socket: socket, SocketEpoch: epoch}}, Participants: []maintenance.Participant{p}, Blockers: []string{}}
	if err = m.store.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	if !interrupt {
		go func() {
			for {
				saved, err := m.store.Load(o.OperationID)
				if err == nil && saved.Participants[0].NoticeDisposition == "accepted" {
					release()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()
	}
	response, err := m.pause(ctx, o.OperationID)
	if err != nil || response.Status != "paused" {
		t.Fatal(response, err)
	}
	if interrupt {
		wait("interrupted source", func() bool { active, err := client.ActiveTurnID(ctx, actor); return err == nil && active == "" })
	}
	client.Close()
	stop()
	// Inject only the ordinary activation boundary: same selected native, same
	// retained home, a fresh process/socket. This is not host-profile acceptance.
	start()
	epoch, err = session.SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	identity, err = processIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "registration.json"), []byte(fmt.Sprintf(`{"launch":{"codex_path":%q}}`, native)), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = m.store.Update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
		current.CutoverIntent = true
		current.SourcesStopped = true
		current.ActivationIntent = true
		current.ActivationPackage = packageRoot
		current.ActivationNative = native
		current.ActivationPlansDigest = "fixture"
		current.Phase = "activating"
		s := &current.Services[0]
		s.DestinationEpoch = epoch
		s.DestinationPID = command.Process.Pid
		s.DestinationProcessIdentity = identity
		s.DestinationNative = native
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	passive := m.client(o, p)
	queue, err := passive.ListQueue(ctx, actor)
	if err != nil {
		passive.Close()
		t.Fatal("destination queue unavailable", err)
	}
	var transcript codex.Transcript
	if len(queue) == 0 {
		transcript, err = passive.ReadThread(ctx, actor)
		if err != nil {
			passive.Close()
			t.Fatal("destination queued-input history unavailable", err)
		}
	}
	disposition, err := maintenanceNativeQueueDisposition(queue, transcript.Entries, queued.Submission.ID, queueClientID, queueText)
	if err != nil {
		passive.Close()
		t.Fatal("queued input not preserved at destination", err)
	}
	t.Logf("destination queued input: %s", disposition)
	goal, err := passive.HasActiveGoal(ctx, actor)
	passive.Close()
	if err != nil || !goal {
		t.Fatal("goal changed before destination activation", err)
	}
	response, err = m.continueParticipants(ctx, o.OperationID)
	if err != nil || response.Status != "continued" {
		t.Fatal(response, err)
	}
	release()
	wait("actual resume command", func() bool { return resumed.Load() > 0 })
	saved, err := m.store.Load(o.OperationID)
	if err != nil || saved.Participants[0].ContinuationDisposition != "accepted" {
		t.Fatal(saved, err)
	}
	before := resumed.Load()
	response, err = m.continueParticipants(ctx, o.OperationID)
	if err != nil || response.Status != "continued" || resumed.Load() != before {
		t.Fatal("stable resume replay duplicated", response, err)
	}
	stop()
}

// A queue row may be consumed by ordinary source work before shutdown. Require
// its exact row identity or positive persisted input identity at the destination.
func maintenanceNativeQueueDisposition(queue []codex.QueueEntry, entries []codex.TranscriptEntry, queueID, clientID, text string) (string, error) {
	if len(queue) != 0 {
		if len(queue) != 1 || queue[0].ID != queueID || queue[0].ClientUserMessageID != clientID || queue[0].Text != text {
			return "", fmt.Errorf("queued input identity changed: %v", queue)
		}
		return fmt.Sprintf("queued row=%s client=%s", queueID, clientID), nil
	}
	for _, entry := range entries {
		if entry.Kind != "userMessage" || entry.ClientUserMessageID != clientID {
			continue
		}
		if entry.Text != text || entry.ItemID == "" || entry.TurnID == "" {
			return "", fmt.Errorf("started queued input identity changed: item=%s turn=%s", entry.ItemID, entry.TurnID)
		}
		return fmt.Sprintf("started row=%s client=%s item=%s turn=%s", queueID, clientID, entry.ItemID, entry.TurnID), nil
	}
	return "", fmt.Errorf("queue row=%s absent without a matching started input client=%s", queueID, clientID)
}

func TestMaintenanceNativeQueueDisposition(t *testing.T) {
	const queueID, clientID, text = "queue-id", "client-id", "queued text"
	queued := codex.QueueEntry{ID: queueID, ClientUserMessageID: clientID, Text: text}
	started := codex.TranscriptEntry{Kind: "userMessage", ClientUserMessageID: clientID, Text: text, ItemID: "item-id", TurnID: "turn-id"}
	for _, test := range []struct {
		name        string
		queue       []codex.QueueEntry
		entries     []codex.TranscriptEntry
		disposition string
	}{
		{"queued", []codex.QueueEntry{queued}, nil, "queued row=queue-id client=client-id"},
		{"started", nil, []codex.TranscriptEntry{started}, "started row=queue-id client=client-id item=item-id turn=turn-id"},
		{"missing", nil, nil, ""},
		{"wrong-row", []codex.QueueEntry{{ID: "wrong", ClientUserMessageID: clientID, Text: text}}, []codex.TranscriptEntry{started}, ""},
		{"wrong-queued-client", []codex.QueueEntry{{ID: queueID, ClientUserMessageID: "wrong", Text: text}}, nil, ""},
		{"wrong-queued-text", []codex.QueueEntry{{ID: queueID, ClientUserMessageID: clientID, Text: "wrong"}}, nil, ""},
		{"extra-row", []codex.QueueEntry{queued, queued}, nil, ""},
		{"wrong-started-client", nil, []codex.TranscriptEntry{{Kind: "userMessage", ClientUserMessageID: "wrong", Text: text, ItemID: "item-id", TurnID: "turn-id"}}, ""},
		{"wrong-started-text", nil, []codex.TranscriptEntry{{Kind: "userMessage", ClientUserMessageID: clientID, Text: "wrong", ItemID: "item-id", TurnID: "turn-id"}}, ""},
		{"wrong-item-kind", nil, []codex.TranscriptEntry{{Kind: "agentMessage", ClientUserMessageID: clientID, Text: text, ItemID: "item-id", TurnID: "turn-id"}}, ""},
		{"missing-item", nil, []codex.TranscriptEntry{{Kind: "userMessage", ClientUserMessageID: clientID, Text: text, TurnID: "turn-id"}}, ""},
		{"missing-turn", nil, []codex.TranscriptEntry{{Kind: "userMessage", ClientUserMessageID: clientID, Text: text, ItemID: "item-id"}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			disposition, err := maintenanceNativeQueueDisposition(test.queue, test.entries, queueID, clientID, text)
			if test.disposition == "" {
				if err == nil || disposition != "" {
					t.Fatal("missing or wrong identity accepted", disposition, err)
				}
			} else if err != nil || disposition != test.disposition {
				t.Fatal(disposition, err)
			}
		})
	}
}
