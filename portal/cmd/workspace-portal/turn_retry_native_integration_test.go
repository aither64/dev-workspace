//go:build live_switch_integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

// This explicitly selected fixture uses the same private daemon/loopback
// Responses contract as live_switch_integration_test.go. It never reads an
// installed workspace, credentials or registered user state.
func TestNativeTurnRetryController(t *testing.T) {
	native := os.Getenv("LIVE_SWITCH_CODEX")
	if !filepath.IsAbs(native) {
		t.Fatal("LIVE_SWITCH_CODEX must select the exact pinned native executable")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	cwd := filepath.Join(workspace, "work", "example")
	stateRoot := filepath.Join(root, "state")
	for _, path := range []string{home, cwd, stateRoot} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var fail atomic.Bool
	fail.Store(true)
	var requests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		index := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(v any) { data, _ := json.Marshal(v); _, _ = fmt.Fprintf(w, "data: %s\n\n", data) }
		event(map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("retry-%d", index)}})
		if fail.Load() {
			event(map[string]any{"type": "response.failed", "sequence_number": 3, "response": map[string]any{"id": fmt.Sprintf("retry-%d", index), "object": "response", "created_at": 1759771626, "status": "failed", "background": false, "error": map[string]any{"code": "server_is_overloaded", "message": "Synthetic model capacity failure"}, "incomplete_details": nil}})
			return
		}
		event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprintf("answer-%d", index), "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "synthetic continuation completed"}}}})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("retry-%d", index), "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	}))
	defer provider.Close()
	config := "model_provider = \"fixture\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\napproval_policy = \"never\"\n[model_providers.fixture]\nname = \"Isolated retry fixture\"\nbase_url = " + strconv.Quote(provider.URL+"/v1") + "\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\nrequest_max_retries = 0\nstream_max_retries = 0\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	nativeSocket := filepath.Join(root, "native.sock")
	socket := filepath.Join(root, "proxy.sock")
	command := exec.CommandContext(ctx, native, "app-server", "--strict-config", "--listen", "unix://"+nativeSocket)
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
	lastObservation := "no native turn observation"
	wait := func(label string, check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				data, _ := os.ReadFile(log.Name())
				if len(data) > 8192 {
					data = data[len(data)-8192:]
				}
				t.Fatalf("%s: %v last observation: %s native log tail: %s", label, ctx.Err(), lastObservation, data)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	wait("native socket", func() bool { _, err := os.Stat(nativeSocket); return err == nil })
	barrier := retryNativeReceiptBarrier(t, ctx, nativeSocket, socket)
	t.Setenv("DEV_WORKSPACE_NAME", "")
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
	client := newCodexClient(socket, workspace, stateRoot)
	defer func() { client.Close() }()
	observations := map[string]string{}
	observationCount := 0
	readState := func(label, thread string) (codex.TurnState, error) {
		state, err := client.ReadTurnState(ctx, thread)
		summary := fmt.Sprintf("thread=%s status=%s latest=%s turnStatus=%s failure=%v readError=%v", thread, state.Status, state.LatestTurnID, state.LatestTurnStatus, state.TerminalFailure, err)
		if len(summary) > 2048 {
			summary = summary[:2048]
		}
		lastObservation = label + ": " + summary
		if observations[label] != summary && observationCount < 20 {
			observations[label] = summary
			observationCount++
			t.Log(lastObservation)
		}
		return state, err
	}
	policy := codex.ThreadPolicy{DeveloperInstructions: "Synthetic managed retry contract.", Sandbox: "danger-full-access", ApprovalPolicy: "never"}
	client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return policy, nil }
	for name, content := range map[string]string{"plan.md": "# Synthetic retry\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": "schema: 1\nslug: example\nrepositories: []\nartifacts: []\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Exercise managed creation before the manifest is bound to the native ID.
	// Enrollment and initial admission must precede the first execution.
	thread, err := client.StartThreadWithSettings(ctx, cwd, map[string]string{}, codex.ThreadSettings{Model: "gpt-5.5", ReasoningEffort: "low", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"plan.md": "# Synthetic retry\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: " + thread + "\n  socket_path: " + socket + "\n  client_version: 0.163.0\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	recovery := session.RecoveryStore{Root: stateRoot, Workspace: workspace}
	if err := recovery.Set(ctx, "example", thread, socket, true, true); err != nil {
		t.Fatal(err)
	}
	if err := client.ConfirmRetryBinding(ctx, thread, policy); err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureInitialMessageWithOptions(ctx, thread, cwd, "Synthetic managed failure", true, codex.TurnOptions{Model: "gpt-5.5", ReasoningEffort: "low", ThreadPolicy: policy}); err != nil {
		t.Fatal(err)
	}
	wait("terminal capacity failure", func() bool {
		s, err := readState("terminal capacity failure", thread)
		return err == nil && s.TerminalFailure != nil && s.TerminalFailure.Code == "serverOverloaded"
	})
	scope, err := client.RetryScope(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	controller := turnretry.Controller{Policy: turnretry.Decision{Revision: "native-fixture-policy", Capacity: true, Cybersecurity: true}, Store: client.RetryStore(), Backend: client.RetryBackend(), Clock: func() time.Time { return now }, Random: func() float64 { return 0 }}
	wait("managed execution proof", func() bool {
		stepErr := controller.Step(ctx, scope)
		s, err := controller.Store.Load(scope)
		lastObservation = fmt.Sprintf("managed proof step=%v phase=%s reason=%s load=%v", stepErr, s.Phase, s.Reason, err)
		return err == nil && s.Phase == "scheduled"
	})
	scheduled, err := controller.Store.Load(scope)
	if err != nil {
		t.Fatal(err)
	}
	id, deadline := scheduled.Pending.ID, scheduled.DeadlineMS
	// Recreate the controller from durable state while retaining the native
	// subscriber. Closing an idle thread's last subscriber can unload it; fresh
	// automatic dispatch must never repair that cold hold. The actual client
	// restart below occurs after native acceptance, when only reconciliation is
	// required and no loaded-thread permission may be inferred from this test.
	controller = turnretry.Controller{Policy: turnretry.Decision{Revision: "native-fixture-policy", Capacity: true, Cybersecurity: true}, Store: client.RetryStore(), Backend: client.RetryBackend(), Clock: func() time.Time { return now }, Random: func() float64 { return 0 }}
	retained, err := controller.Store.Load(scope)
	if err != nil || retained.Pending.ID != id || retained.DeadlineMS != deadline {
		t.Fatal("controller restart lost the saved retry identity", err)
	}
	loaded, loadedErr := client.LoadedThreadIDs(ctx)
	metadata, metadataErr := client.ReadThreadMetadata(ctx, thread, false)
	rollout := "no rollout path"
	if metadata.Path != nil {
		if relative, err := filepath.Rel(root, *metadata.Path); err == nil && filepath.IsLocal(relative) {
			info, statErr := os.Stat(*metadata.Path)
			rollout = fmt.Sprintf("path=%s statError=%v", relative, statErr)
			if statErr == nil {
				rollout += fmt.Sprintf(" bytes=%d regular=%t", info.Size(), info.Mode().IsRegular())
			}
		} else {
			rollout = "rollout path outside private fixture"
		}
	}
	t.Logf("source setup loaded=%v loadedError=%v metadataError=%v %s", loaded[:min(len(loaded), 20)], loadedErr, metadataErr, rollout)
	wait("loaded failed retry source", func() bool {
		s, err := readState("loaded failed retry source", thread)
		return err == nil && s.IsLoadedFailedTurn(retained.Pending.Failure)
	})
	// Default enrollment in another independent controller consumes this old
	// failed turn and cannot send it. It uses separate disposable private state.
	baseline := turnretry.Controller{Policy: turnretry.Decision{Revision: "native-fixture-policy", Capacity: true, Cybersecurity: true}, Store: turnretry.Store{Root: filepath.Join(root, "baseline-state"), Workspace: workspace}, Backend: client.RetryBackend(), Clock: func() time.Time { return now }}
	if err := baseline.Step(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if s, _ := baseline.Store.Load(scope); s.Phase != "idle" || s.Pending != nil {
		t.Fatal("cold enrollment retried an old failure")
	}
	fail.Store(false)
	barrier.Store(true)
	now = time.UnixMilli(deadline)
	// The proxy removes the actual accepted reply and hides history until restart.
	stepErr := controller.Step(ctx, scope)
	uncertain, err := controller.Store.Load(scope)
	nativeState, nativeErr := client.ReadTurnState(ctx, thread)
	starts, acceptedStarts, hidden, nativeThread, nativeClientID, nativeTurn, nativeFailure := barrier.snapshot()
	t.Logf("uncertain dispatch step=%v phase=%s reason=%s native=%+v nativeErr=%v barrier starts=%d accepted=%d hiddenHistory=%d receiptTurn=%s failure=%s", stepErr, uncertain.Phase, uncertain.Reason, nativeState, nativeErr, starts, acceptedStarts, hidden, nativeTurn, nativeFailure)
	var unknown *codex.UnknownSendOutcomeError
	if !errors.As(stepErr, &unknown) || err != nil || !uncertain.Uncertain() || uncertain.Pending.ID != id {
		t.Fatalf("native uncertainty lost exact intent: step=%v load=%v phase=%s reason=%s", stepErr, err, uncertain.Phase, uncertain.Reason)
	}
	if starts != 1 || acceptedStarts != 1 || hidden == 0 || nativeThread != thread || nativeClientID != id || nativeTurn == "" || nativeFailure != "" {
		t.Fatal("barrier did not hide exactly one genuine native acceptance")
	}
	retryNativeRequireSubmitting(t, stateRoot, thread, *uncertain.Pending)
	if err := client.RequireSubmissionAttemptsKnown(ctx, thread); err == nil {
		t.Fatal("genuine native submitting ledger did not hold ordinary mutation")
	}
	var continued string
	wait("native continuation completion", func() bool {
		s, err := readState("native continuation completion", thread)
		if err == nil && s.LatestTurnID == nativeTurn && s.LatestTurnStatus == "completed" {
			continued = s.LatestTurnID
			return true
		}
		return false
	})
	before, err := client.ReadTurnContext(ctx, thread, uncertain.Pending.Failure)
	if err != nil {
		t.Fatal(err)
	}
	after, err := client.ReadTurnContext(ctx, thread, continued)
	if err != nil {
		t.Fatal(err)
	}
	if before.Model != after.Model || before.ReasoningEffort != after.ReasoningEffort || before.CollaborationMode != after.CollaborationMode {
		t.Fatal("automatic continuation changed actual settings")
	}
	requestCount := requests.Load()
	client.Close()
	client = newCodexClient(socket, workspace, stateRoot)
	client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return policy, nil }
	controller.Backend, controller.Store = client.RetryBackend(), client.RetryStore()
	barrier.Store(false)
	portal := retryNativePortal(t, root, workspace, stateRoot, socket, thread, client)
	defer portal.Close()
	wait("portal resolver receipt recovery", func() bool {
		s, err := controller.Store.Load(scope)
		if err != nil {
			return false
		}
		for _, p := range s.Snapshots {
			if p.ID == id && p.Turn == continued {
				return true
			}
		}
		return false
	})
	portal.Close()
	if requests.Load() != requestCount {
		t.Fatal("resolver recovery repeated native submission")
	}
	if err := client.RequireSubmissionAttemptsKnown(ctx, thread); err != nil {
		t.Fatal("native submitting ledger was not reconciled", err)
	}
	page, err := client.ReadThreadPage(ctx, thread, "")
	if err != nil {
		t.Fatal(err)
	}
	inputs := 0
	for _, entry := range page.Entries {
		if entry.Kind == "userMessage" && entry.ClientUserMessageID == id {
			inputs++
			if entry.TurnID != continued || entry.ClientUserMessageDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(turnretry.Continuation))) {
				t.Fatal("reconciled native input changed exact turn or continuation")
			}
		}
	}
	if inputs != 1 {
		t.Fatalf("native continuation input count=%d, want exactly one", inputs)
	}
	accepted := uncertain
	// A fresh managed failure schedules another chain; disabling before its
	// deadline must prevent an actual model request even after time advances.
	fail.Store(true)
	if _, err := client.SendWithOptions(ctx, thread, "Synthetic optout failure", "optout-input", "", codex.TurnOptions{Model: "gpt-5.5", ReasoningEffort: "low", ThreadPolicy: policy}); err != nil {
		t.Fatal(err)
	}
	wait("second terminal failure", func() bool {
		s, err := readState("second terminal failure", thread)
		return err == nil && s.LatestTurnID != continued && s.TerminalFailure != nil && s.TerminalFailure.Code == "serverOverloaded"
	})
	wait("second managed schedule", func() bool {
		stepErr := controller.Step(ctx, scope)
		s, err := controller.Store.Load(scope)
		lastObservation = fmt.Sprintf("second schedule step=%v phase=%s reason=%s load=%v", stepErr, s.Phase, s.Reason, err)
		return err == nil && s.Phase == "scheduled"
	})
	state, _ := controller.Store.Load(scope)
	secondDeadline := state.DeadlineMS
	tx, err := controller.Store.Lock(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.SetEnabled(false, turnretry.Observation{Turn: state.Handled, Status: "failed"})
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	requestCount = requests.Load()
	now = time.UnixMilli(secondDeadline).Add(time.Minute)
	if err := controller.Step(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if state, _ := controller.Store.Load(scope); state.Enabled || state.Pending != nil || requests.Load() != requestCount {
		t.Fatal("optout before deadline allowed native dispatch")
	}
	t.Logf("private native controller retained UUID %s across restart; %s -> %s, requests=%d", id, accepted.Pending.Failure, continued, requests.Load())
}

// Inspect the real A writer's private receipt ledger; never seed or rewrite it.
func retryNativeRequireSubmitting(t *testing.T, stateRoot, thread string, pending turnretry.Pending) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(stateRoot, "submissions", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("native submission ledger paths=%v error=%v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		Schema int `json:"schema"`
		Sends  map[string]map[string]struct {
			Digest  string `json:"digest"`
			State   string `json:"state"`
			Context string `json:"context"`
			Turn    string `json:"turnId"`
			Steered bool   `json:"steered"`
		} `json:"sends"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	attempt := ledger.Sends[thread][pending.ID]
	if ledger.Schema != 3 || attempt.State != "submitting" || attempt.Context != pending.Action || attempt.Digest != fmt.Sprintf("%x", sha256.Sum256([]byte(turnretry.Continuation))) || attempt.Turn != "" || attempt.Steered {
		t.Fatalf("native ledger did not retain exact submitting attempt: schema=%d attempt=%+v", ledger.Schema, attempt)
	}
	t.Logf("native schema3 submitting thread=%s client=%s", thread, pending.ID)
}
