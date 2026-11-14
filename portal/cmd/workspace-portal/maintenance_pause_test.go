package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/coder/websocket"
)

// Exercise the real client, ledger, controller and persisted deadline. The fake
// native endpoint owns only ordinary turn/steer, reads and turn/interrupt.
func maintenancePauseFixture(t *testing.T, cooperative bool, reply ...func(*websocket.Conn, map[string]any) bool) (maintenanceControl, maintenance.Operation, *[]string) {
	t.Helper()
	m, o := maintenanceWorkerFixture(t, false)
	cwd := filepath.Join(m.workspace, "work/actor")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	turns := map[string]string{"thread": "captured", "child": "child-turn"}
	events := []string{}
	socket := threadCommandAppServer(t, func(c *websocket.Conn) error {
		if err := threadCommandHandshake(c); err != nil {
			return err
		}
		for {
			r, err := threadCommandReadObject(c)
			if err != nil {
				return nil
			}
			if len(reply) > 0 && !reply[0](c, r) {
				continue
			}
			params, _ := r["params"].(map[string]any)
			thread, _ := params["threadId"].(string)
			mu.Lock()
			var result any
			switch r["method"] {
			case "thread/resume":
				events = append(events, "resume:"+thread)
				fallthrough
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": thread, "cwd": cwd, "status": map[string]any{"type": "active"}, "historyMode": "paginated", "turns": []any{}}}
			case "thread/turns/list":
				rows := []any{}
				if turns[thread] != "" {
					rows = append(rows, map[string]any{"id": turns[thread], "status": "inProgress"})
				}
				result = map[string]any{"data": rows, "nextCursor": nil}
			case "thread/items/list":
				result = map[string]any{"data": []any{}, "nextCursor": nil}
			case "thread/queue/list":
				result = map[string]any{"data": []any{map[string]any{"id": "retained-queue", "text": "queued user work"}}, "nextCursor": nil}
			case "thread/goal/get":
				result = map[string]any{"goal": map[string]any{"threadId": thread, "status": "active", "objective": "retained goal"}}
			case "thread/list":
				result = map[string]any{"data": []any{map[string]any{"id": "child"}}, "nextCursor": nil}
			case "turn/start":
				events = append(events, "send:"+thread)
				result = map[string]any{"turn": map[string]any{"id": "resume-" + thread, "status": "inProgress"}}
			case "turn/steer":
				saved, err := m.store.Load(o.OperationID)
				if err != nil || saved.PauseDeadline.IsZero() {
					mu.Unlock()
					return fmt.Errorf("notice before durable deadline: %v", err)
				}
				events = append(events, "steer")
				if cooperative {
					turns["thread"] = ""
					turns["child"] = ""
				}
				result = map[string]any{"turnId": "captured"}
			case "turn/interrupt":
				if params["turnId"] != turns[thread] {
					mu.Unlock()
					return fmt.Errorf("interrupt selected wrong turn")
				}
				events = append(events, "interrupt:"+thread)
				turns[thread] = ""
				result = map[string]any{}
			default:
				mu.Unlock()
				return fmt.Errorf("unexpected bounded pause RPC %s", r["method"])
			}
			mu.Unlock()
			if err := threadCommandWriteObject(c, map[string]any{"id": r["id"], "result": result}); err != nil {
				return err
			}
		}
	})
	for name, text := range map[string]string{"plan.md": "Pause fixture\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": fmt.Sprintf("schema: 1\nslug: actor\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: thread\n  socket_path: %s\n  client_version: 0.165.0\n", socket)} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inc, err := trackingIncarnation(m.workspace, "actor")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := session.SocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	o.Phase = "prepared"
	o.CutoverIntent = false
	o.SourcesStopped = false
	o.Preparation = json.RawMessage(`{"pauseTimeout":90}`)
	o.Services = []maintenance.Service{{Workspace: m.workspace, Unit: "source", PID: os.Getpid(), ProcessIdentity: identity, Native: exe, Socket: socket, SocketEpoch: epoch}}
	o.Participants = []maintenance.Participant{{Workspace: m.workspace, Slug: "actor", Incarnation: inc, Root: "thread", Thread: "thread", Cwd: cwd, SourceTurn: "captured", SocketEpoch: epoch, NoticeID: controlOperationID, NoticeText: "Save checkpoint and finish", NoticeAction: "pause", NoticeDisposition: "prepared", ContinuationID: maintenanceUUID("resume"), ContinuationDisposition: "pending"}}
	if _, err = m.store.Update(context.Background(), o.OperationID, 0, func(saved *maintenance.Operation) error { *saved = o; return nil }); err != nil {
		t.Fatal(err)
	}
	return m, o, &events
}
func TestMaintenanceCooperativePauseUsesOrdinarySteer(t *testing.T) {
	m, o, events := maintenancePauseFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := m.pause(ctx, o.OperationID)
	if err != nil || response.Status != "paused" {
		t.Fatal(response, err)
	}
	if fmt.Sprint(*events) != "[steer]" {
		t.Fatal(*events)
	}
	saved, err := m.store.Load(o.OperationID)
	if err != nil || saved.PauseDeadline.IsZero() || saved.Participants[0].ContinuationDisposition != "pending" {
		t.Fatal(saved, err)
	}
}
func TestMaintenanceDeadlineSurvivesWorkerRestartAndInterruptsOwnedChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var childListReached atomic.Bool
	m, o, events := maintenancePauseFixture(t, false, func(_ *websocket.Conn, request map[string]any) bool {
		method := request["method"].(string)
		// Cancel only after initialization and the pause notice, rather than
		// racing connection initialization with an unrelated wall-clock timer.
		if method == "thread/list" && childListReached.CompareAndSwap(false, true) {
			cancel()
			return false
		}
		return true
	})
	_, err := m.pause(ctx, o.OperationID)
	if !childListReached.Load() || !errors.Is(err, context.Canceled) {
		t.Fatal("pause did not cancel after initialized child inventory", err)
	}
	saved, err := m.store.Load(o.OperationID)
	if err != nil || saved.PauseDeadline.IsZero() || saved.Participants[0].NoticeDisposition != "accepted" {
		t.Fatal(saved, err)
	}
	deadline := time.Now().Add(-time.Second)
	if _, err = m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error {
		current.PauseDeadline = deadline
		current.Phase = "recovery-required"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	recovered := maintenanceControl{store: m.store, workspace: m.workspace}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer recoveryCancel()
	response, err := recovered.pause(recoveryCtx, o.OperationID)
	if err != nil || response.Status != "paused" {
		t.Fatal(response, err)
	}
	saved, _ = m.store.Load(o.OperationID)
	if !saved.PauseDeadline.Equal(deadline) {
		t.Fatal("recovery extended grace period")
	}
	if fmt.Sprint(*events) != "[steer interrupt:thread interrupt:child]" {
		t.Fatal(*events)
	}
	if saved.Participants[0].StopRevision != 0 || saved.Participants[0].ContinuationDisposition != "pending" {
		t.Fatal("maintenance interrupt became user Stop", saved.Participants)
	}
}

func TestMaintenanceResumeUsesStableCommandAndReconcilesSameAttempt(t *testing.T) {
	m, o, events := maintenancePauseFixture(t, true)
	ctx := context.Background()
	if _, err := m.pause(ctx, o.OperationID); err != nil {
		t.Fatal(err)
	}
	// This test isolates delivery from target process activation. Cancellation
	// keeps the actual source and uses the identical locked Send path.
	if _, err := m.store.Update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
		current.CancelRequested = true
		current.Phase = "resuming"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err := m.continueParticipants(ctx, o.OperationID)
	if err != nil || result.Status != "continued" {
		t.Fatal(result, err)
	}
	saved, _ := m.store.Load(o.OperationID)
	if saved.Participants[0].ContinuationDisposition != "accepted" {
		t.Fatal(saved)
	}
	sends := fmt.Sprint(*events)
	if !strings.Contains(sends, "send:thread") {
		t.Fatal("missing explicit resume command", sends)
	}
	// Lose only the worker's receipt after the SDK recorded native acceptance.
	if _, err = m.store.Update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
		current.ContinuationsSettled = false
		current.Participants[0].ContinuationDisposition = "unknown"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err = m.continueParticipants(ctx, o.OperationID)
	if err != nil || result.Status != "continued" || fmt.Sprint(*events) != sends {
		t.Fatal("recovery duplicated delivery", result, err, *events)
	}
}
func TestMaintenanceStopAndFailedActivationDoNotResume(t *testing.T) {
	for _, scenario := range []string{"stop", "shutdown-incomplete", "activation-incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			m, o, events := maintenancePauseFixture(t, true)
			ctx := context.Background()
			if _, err := m.pause(ctx, o.OperationID); err != nil {
				t.Fatal(err)
			}
			if scenario == "stop" {
				if _, err := (maintenance.Controller{Store: m.store}).SuppressResume(ctx, maintenance.Request{OperationID: o.OperationID, ThreadID: "thread"}); err != nil {
					t.Fatal(err)
				}
				_, _ = m.store.Update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error { current.CancelRequested = true; return nil })
			}
			if scenario != "stop" {
				_, err := m.store.Update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
					current.CutoverIntent = true
					if scenario == "shutdown-incomplete" {
						current.ActivationIntent = true
						current.ActivationPackage = "/candidate"
						current.ActivationNative = "/native"
						current.ActivationPlansDigest = "plans"
					} else {
						current.SourcesStopped = true
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := fmt.Sprint(*events)
			result, err := m.continueParticipants(ctx, o.OperationID)
			if scenario == "stop" {
				if err != nil || result.Status != "continued" {
					t.Fatal(result, err)
				}
			} else if err == nil {
				t.Fatal("resume admitted before activation")
			}
			if fmt.Sprint(*events) != before {
				t.Fatal("failed/suppressed activation performed resume", *events)
			}
		})
	}
}

func TestMaintenanceIndependentRosterParticipantsEachReceiveResume(t *testing.T) {
	m, o, events := maintenancePauseFixture(t, true)
	ctx := context.Background()
	if _, err := m.pause(ctx, o.OperationID); err != nil {
		t.Fatal(err)
	}
	roster, err := teamruntime.NewStore(m.store.Root, m.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = roster.Update(ctx, "actor", "thread", true, func(r *teamruntime.Roster) error {
		r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Index: 0, Thread: "member", State: "ready", Model: "gpt-6.1-sol", Effort: "xhigh", Access: "workspace_write", Behavior: "implementer", Purpose: "implementation", Instructions: "Implement assigned work", AddedAt: time.Now().UTC()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.store.Update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
		p := current.Participants[0]
		p.Thread = "member"
		p.Member = "implementer0"
		p.ContinuationID = maintenanceUUID("member-resume")
		p.NoticeID = maintenanceUUID("member-notice")
		current.Participants = append(current.Participants, p)
		current.CancelRequested = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err := m.continueParticipants(ctx, o.OperationID)
	if err != nil || result.Status != "continued" {
		t.Fatal(result, err)
	}
	log := fmt.Sprint(*events)
	if strings.Count(log, "send:thread") != 1 || strings.Count(log, "send:member") != 1 || strings.Contains(log, "resume:child") {
		t.Fatal(log)
	}
}

// Separate an initialized connection's call cap from ConditionalSteer's private
// setup and real submission bookkeeping, which the integration test covers below.
func TestMaintenancePauseSilentNativeOperationsAreBounded(t *testing.T) {
	for _, method := range []string{"thread/turns/list", "turn/steer", "thread/list", "turn/interrupt"} {
		t.Run(method, func(t *testing.T) {
			var armed atomic.Bool
			var reached, steers, starts, resumes atomic.Int32
			var firstEntry atomic.Int64
			requests := make(chan map[string]any, 1)
			m, o, _ := maintenancePauseFixture(t, false, func(_ *websocket.Conn, request map[string]any) bool {
				if !armed.Load() {
					return true
				}
				actual := request["method"].(string)
				switch actual {
				case "turn/steer":
					steers.Add(1)
				case "turn/start":
					starts.Add(1)
				case "thread/resume":
					resumes.Add(1)
				}
				if actual == method {
					reached.Add(1)
					firstEntry.CompareAndSwap(0, time.Now().UnixNano())
					select {
					case requests <- request:
					default:
					}
					return false
				}
				return true
			})
			m.pauseCallTimeout = 500 * time.Millisecond
			m.pauseRecoveryTimeout = 700 * time.Millisecond
			c := m.client(o, o.Participants[0])
			defer c.Close()
			setup, setupCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer setupCancel()
			if active, err := c.ActiveTurnID(setup, "thread"); err != nil || active != "captured" {
				t.Fatal("initialize real pause client", active, err)
			}
			armed.Store(true)
			deadline := time.Now().Add(time.Second)
			allowance := deadline.Add(700 * time.Millisecond)
			if method == "turn/interrupt" {
				deadline = time.Now().Add(-time.Second)
				allowance = time.Now().Add(700 * time.Millisecond)
			}
			_, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.PauseDeadline = deadline; return nil })
			if err != nil {
				t.Fatal(err)
			}
			params := map[string]any{"threadId": "thread"}
			switch method {
			case "thread/turns/list":
				params["limit"], params["sortDirection"], params["itemsView"] = 1, "desc", "notLoaded"
			case "turn/steer":
				params["expectedTurnId"], params["clientUserMessageId"] = o.Participants[0].SourceTurn, o.Participants[0].NoticeID
				params["input"] = []map[string]any{{"type": "text", "text": o.Participants[0].NoticeText}}
			case "thread/list":
				params = map[string]any{"ancestorThreadId": "thread", "archived": false, "modelProviders": []string{}, "limit": 100}
			case "turn/interrupt":
				params["turnId"] = o.Participants[0].SourceTurn
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var bounded context.Context
			start := time.Now()
			err = m.pauseNative(ctx, o.OperationID, deadline, allowance, func(call context.Context) error {
				bounded = call
				return c.Request(call, method, params, nil)
			})
			elapsed, parentErr := time.Since(start), ctx.Err()
			saved, loadErr := m.store.Load(o.OperationID)
			var request map[string]any
			select {
			case request = <-requests:
			default:
			}
			actualParams, _ := json.Marshal(request["params"])
			expectedParams, _ := json.Marshal(params)
			reason := maintenanceControlFailureReason(err)
			expectedReason := "pause-native-timeout"
			if method == "turn/interrupt" {
				expectedReason = "pause-interrupt-timeout"
			}
			expectedSteers := int32(0)
			if method == "turn/steer" {
				expectedSteers = 1
			}
			valid := reached.Load() == 1 && firstEntry.Load() != 0 && string(actualParams) == string(expectedParams) && errors.Is(err, context.DeadlineExceeded) && reason == expectedReason && parentErr == nil && bounded != nil && bounded.Err() != nil && steers.Load() == expectedSteers && starts.Load() == 0 && resumes.Load() == 0
			valid = valid && loadErr == nil && saved.PauseDeadline.Equal(deadline) && saved.Phase == o.Phase && saved.CutoverIntent == o.CutoverIntent && saved.SourcesStopped == o.SourcesStopped && saved.ActivationIntent == o.ActivationIntent && saved.ActivationPackage == o.ActivationPackage && len(saved.Participants) == 1
			if len(saved.Participants) == 1 {
				p, original := saved.Participants[0], o.Participants[0]
				valid = valid && p.StopRevision == original.StopRevision && p.ContinuationID == original.ContinuationID && p.ContinuationDisposition == original.ContinuationDisposition && p.NoticeID == original.NoticeID && p.NoticeDisposition == original.NoticeDisposition
			}
			evidence := fmt.Sprintf("method=%s reached=%d first=%s params=%s steers=%d starts=%d resumes=%d elapsed=%s deadline=%s parent=%v reason=%s err=%v load=%v phase=%s participants=%+v", method, reached.Load(), time.Unix(0, firstEntry.Load()).UTC().Format(time.RFC3339Nano), actualParams, steers.Load(), starts.Load(), resumes.Load(), elapsed, deadline.UTC().Format(time.RFC3339Nano), parentErr, reason, err, loadErr, saved.Phase, saved.Participants)
			if !valid {
				t.Fatal("initialized native call bound unproved:", evidence)
			}
			t.Log(evidence)
		})
	}
}

func TestMaintenancePauseConditionalSteerAmbiguityKeepsLedger(t *testing.T) {
	type observation struct {
		request  map[string]any
		writeErr error
	}
	observed := make(chan observation, 1)
	var steers, starts, resumes atomic.Int32
	m, o, _ := maintenancePauseFixture(t, false, func(c *websocket.Conn, request map[string]any) bool {
		switch request["method"] {
		case "turn/start":
			starts.Add(1)
		case "thread/resume":
			resumes.Add(1)
		case "turn/steer":
			steers.Add(1)
			writeErr := threadCommandWriteObject(c, map[string]any{"id": request["id"], "error": map[string]any{"code": -32000, "message": "fixture ambiguous steer outcome"}})
			select {
			case observed <- observation{request: request, writeErr: writeErr}:
			default:
			}
			return false
		}
		return true
	})
	deadline := time.Now().Add(90 * time.Second)
	if _, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.PauseDeadline = deadline; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := m.pause(ctx, o.OperationID)
	parentErr := ctx.Err()
	saved, loadErr := m.store.Load(o.OperationID)
	c := m.client(o, o.Participants[0])
	ledger, exportErr := c.ExportSubmissionAttempts(o.Participants[0].Root)
	c.Close()
	var response observation
	var arrived bool
	select {
	case response = <-observed:
		arrived = true
	default:
	}
	original := o.Participants[0]
	params, _ := response.request["params"].(map[string]any)
	expectedParams := map[string]any{"threadId": original.Root, "expectedTurnId": original.SourceTurn, "clientUserMessageId": original.NoticeID, "input": []map[string]any{{"type": "text", "text": original.NoticeText}}}
	actualWire, _ := json.Marshal(params)
	expectedWire, _ := json.Marshal(expectedParams)
	var unknown *codex.UnknownSendOutcomeError
	var unsent *codex.MessageNotSentError
	valid := arrived && response.writeErr == nil && steers.Load() == 1 && starts.Load() == 0 && resumes.Load() == 0 && string(actualWire) == string(expectedWire) && parentErr == nil && result.Status != "paused" && errors.As(err, &unknown) && !errors.As(err, &unsent) && maintenanceControlFailureReason(err) == "notice-outcome-unknown"
	valid = valid && loadErr == nil && saved.PauseDeadline.Equal(deadline) && saved.Phase == o.Phase && saved.CutoverIntent == o.CutoverIntent && saved.SourcesStopped == o.SourcesStopped && saved.ActivationIntent == o.ActivationIntent && saved.ActivationPackage == o.ActivationPackage && len(saved.Participants) == 1
	if len(saved.Participants) == 1 {
		p := saved.Participants[0]
		valid = valid && p.StopRevision == original.StopRevision && p.ContinuationID == original.ContinuationID && p.ContinuationDisposition == original.ContinuationDisposition && p.Root == original.Root && p.Thread == original.Thread && p.Cwd == original.Cwd && p.SourceTurn == original.SourceTurn && p.NoticeID == original.NoticeID && p.NoticeText == original.NoticeText && p.NoticeAction == original.NoticeAction && p.NoticeDisposition == "unknown"
	}
	var attempts struct {
		Sends map[string]map[string]struct {
			State, Context string
			Steered        bool
		}
	}
	decodeErr := json.Unmarshal(ledger, &attempts)
	identity, encodeErr := json.Marshal([]string{original.Cwd, original.SourceTurn, original.NoticeAction})
	entry := attempts.Sends[original.Root][original.NoticeID]
	valid = valid && exportErr == nil && decodeErr == nil && encodeErr == nil && len(attempts.Sends[original.Root]) == 1 && entry.State == "submitting" && entry.Context == "conditional-steer:v1:"+string(identity) && entry.Steered
	evidence := fmt.Sprintf("arrived=%t write=%v steers=%d starts=%d resumes=%d params=%s parent=%v status=%s reason=%s err=%v load=%v deadline=%s phase=%s participants=%+v ledger=%s export=%v", arrived, response.writeErr, steers.Load(), starts.Load(), resumes.Load(), actualWire, parentErr, result.Status, maintenanceControlFailureReason(err), err, loadErr, deadline.UTC().Format(time.RFC3339Nano), saved.Phase, saved.Participants, ledger, exportErr)
	if !valid {
		t.Fatal("integrated steer uncertainty unproved:", evidence)
	}
	t.Log(evidence)
}

func TestMaintenanceGraceBoundaryRetriesPassiveReadInInterruptPass(t *testing.T) {
	var reads atomic.Int32
	var firstRead, retryRead atomic.Int64
	m, o, events := maintenancePauseFixture(t, false, func(_ *websocket.Conn, request map[string]any) bool {
		method := request["method"].(string)
		if method == "thread/turns/list" {
			switch reads.Add(1) {
			case 1:
				firstRead.Store(time.Now().UnixNano())
				return false
			case 2:
				retryRead.Store(time.Now().UnixNano())
			}
		}
		return true
	})
	m.pauseCallTimeout = time.Second
	m.pauseRecoveryTimeout = 3 * time.Second
	deadline := time.Now().Add(500 * time.Millisecond)
	_, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.PauseDeadline = deadline; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := m.pause(ctx, o.OperationID)
	first, retry := firstRead.Load(), retryRead.Load()
	phase := "grace-read-retried"
	switch {
	case first == 0:
		phase = "first-read-missing"
	case first >= deadline.UnixNano():
		phase = "first-read-missed-grace"
	case retry == 0:
		phase = "retry-read-missing"
	case retry < deadline.UnixNano():
		phase = "retry-read-before-grace"
	}
	log := fmt.Sprint(*events)
	saved, loadErr := m.store.Load(o.OperationID)
	reason := result.Reason
	if err != nil {
		reason = maintenanceControlFailureReason(err)
	}
	fail := func(detail string) {
		t.Helper()
		t.Fatalf("%s: phase=%s reads=%d first=%s retry=%s deadline=%s events=%s status=%s reason=%s err=%v load=%v saved=%+v", detail, phase, reads.Load(), time.Unix(0, first).UTC().Format(time.RFC3339Nano), time.Unix(0, retry).UTC().Format(time.RFC3339Nano), deadline.UTC().Format(time.RFC3339Nano), log, result.Status, reason, err, loadErr, saved)
	}
	if err != nil || result.Status != "paused" || phase != "grace-read-retried" || loadErr != nil || !saved.PauseDeadline.Equal(deadline) || saved.Phase != "draining-writers" || log != "[interrupt:thread interrupt:child]" || len(saved.Participants) != 1 {
		fail("grace-to-interrupt transition unproved")
	}
	participant := saved.Participants[0]
	if participant.StopRevision != o.Participants[0].StopRevision || participant.ContinuationID != o.Participants[0].ContinuationID || participant.ContinuationDisposition != "pending" {
		fail("maintenance interruption changed Stop or continuation identity")
	}
}
func TestMaintenancePersistedCancelAndCallerCancellationBoundOutstandingRead(t *testing.T) {
	for _, caller := range []bool{false, true} {
		t.Run(fmt.Sprint(caller), func(t *testing.T) {
			entered := make(chan struct{}, 1)
			m, o, events := maintenancePauseFixture(t, false, func(_ *websocket.Conn, request map[string]any) bool {
				method := request["method"].(string)
				if method == "thread/turns/list" {
					select {
					case entered <- struct{}{}:
					default:
					}
					return false
				}
				return true
			})
			m.pauseCallTimeout = 500 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				r, e := m.pause(ctx, o.OperationID)
				if !caller && r.Status != "cancelled" {
					e = fmt.Errorf("Cancel not observed: %v %v", r, e)
				}
				done <- e
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("read never reached connected endpoint", err)
			case <-ctx.Done():
				t.Fatal("read did not reach connected endpoint", ctx.Err())
			}
			if caller {
				cancel()
			} else {
				if _, err := (maintenance.Controller{Store: m.store}).Cancel(context.Background(), o.OperationID); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if caller && !errors.Is(err, context.Canceled) || !caller && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation stuck behind native read")
			}
			if len(*events) != 0 {
				t.Fatal("effects after Cancel", *events)
			}
			saved, _ := m.store.Load(o.OperationID)
			if saved.Phase == "draining-writers" {
				t.Fatal(saved)
			}
		})
	}
}
func TestMaintenanceUnknownSteerIsSavedAfterCallerExpiresAndCleanupIsBounded(t *testing.T) {
	entered := make(chan map[string]any, 1)
	var steers, starts, resumes, cleanupReads atomic.Int32
	m, o, _ := maintenancePauseFixture(t, false, func(_ *websocket.Conn, request map[string]any) bool {
		switch request["method"] {
		case "turn/start":
			starts.Add(1)
		case "thread/resume":
			resumes.Add(1)
		case "turn/steer":
			steers.Add(1)
			select {
			case entered <- request:
			default:
			}
			return false
		case "thread/items/list":
			cleanupReads.Add(1)
			return false
		}
		return true
	})
	// Dispatch uses ordinary limits; the short allowance belongs to cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.pause(ctx, o.OperationID); done <- err }()
	var request map[string]any
	select {
	case request = <-entered:
	case err := <-done:
		t.Fatal("steer never dispatched", err)
	case <-ctx.Done():
		t.Fatal("steer did not reach connected endpoint before caller expired", ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("steer reached connected endpoint after caller expired", err)
	}
	original := o.Participants[0]
	params, _ := request["params"].(map[string]any)
	expectedParams := map[string]any{"threadId": original.Root, "expectedTurnId": original.SourceTurn, "clientUserMessageId": original.NoticeID, "input": []map[string]any{{"type": "text", "text": original.NoticeText}}}
	actualWire, _ := json.Marshal(params)
	expectedWire, _ := json.Marshal(expectedParams)
	cancel()
	pauseErr := <-done
	// Copy only after joining pause, so its controller is never mutated in flight.
	cleanup := m
	cleanup.pauseCallTimeout = 500 * time.Millisecond
	cleanup.pauseRecoveryTimeout = 650 * time.Millisecond
	readLedger := func() (json.RawMessage, error) {
		c := m.client(o, original)
		defer c.Close()
		return c.ExportSubmissionAttempts(original.Root)
	}
	identity, encodeErr := json.Marshal([]string{original.Cwd, original.SourceTurn, original.NoticeAction})
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(original.NoticeText)))
	ledgerValid := func(data json.RawMessage) bool {
		var attempts struct {
			Schema int
			Sends  map[string]map[string]struct {
				State, Context, Digest string
				Steered                bool
			}
		}
		decodeErr := json.Unmarshal(data, &attempts)
		entry := attempts.Sends[original.Root][original.NoticeID]
		return decodeErr == nil && encodeErr == nil && attempts.Schema == 3 && len(attempts.Sends) == 1 && len(attempts.Sends[original.Root]) == 1 && entry.State == "submitting" && entry.Context == "conditional-steer:v1:"+string(identity) && entry.Digest == digest && entry.Steered
	}
	before, beforeLoadErr := m.store.Load(o.OperationID)
	beforeLedger, beforeExportErr := readLedger()
	if _, err := (maintenance.Controller{Store: m.store}).Cancel(context.Background(), o.OperationID); err != nil {
		t.Fatal(err)
	}
	// Native cleanup must exhaust its own allowance while this outer guard lives.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cleanupCancel()
	start := time.Now()
	result, err := cleanup.cancelNotices(cleanupCtx, o.OperationID)
	elapsed, callerErr := time.Since(start), cleanupCtx.Err()
	// Gather both persisted proofs even when the cleanup verdict is invalid.
	saved, loadErr := m.store.Load(o.OperationID)
	afterLedger, afterExportErr := readLedger()
	var unknown *codex.UnknownSendOutcomeError
	var unsent *codex.MessageNotSentError
	valid := errors.Is(pauseErr, context.Canceled) && errors.As(pauseErr, &unknown) && !errors.As(pauseErr, &unsent) && string(actualWire) == string(expectedWire) && steers.Load() == 1 && starts.Load() == 0 && resumes.Load() == 0
	valid = valid && beforeLoadErr == nil && beforeExportErr == nil && ledgerValid(beforeLedger) && !before.PauseDeadline.IsZero() && len(before.Participants) == 1 && loadErr == nil && afterExportErr == nil && ledgerValid(afterLedger) && len(saved.Participants) == 1
	valid = valid && err == nil && result.Status == "recovery-required" && result.Reason == "notice-cleanup-timeout" && callerErr == nil && saved.CancelRequested && !saved.NoticeCleanupComplete && fmt.Sprint(saved.Blockers) == "[notice-cleanup-unresolved]" && saved.PauseDeadline.Equal(before.PauseDeadline)
	valid = valid && saved.OperationID == o.OperationID && saved.Profile == o.Profile && saved.SourceProfileToken == o.SourceProfileToken && saved.SourcePackage == o.SourcePackage && saved.CandidatePackage == o.CandidatePackage && saved.PlansDigest == o.PlansDigest && saved.Phase == before.Phase && saved.CutoverIntent == o.CutoverIntent && saved.SourcesStopped == o.SourcesStopped && saved.ActivationIntent == o.ActivationIntent && saved.ActivationPackage == o.ActivationPackage && saved.ActivationNative == o.ActivationNative && saved.ActivationPlansDigest == o.ActivationPlansDigest
	if len(before.Participants) == 1 && len(saved.Participants) == 1 {
		expected := original
		expected.NoticeDisposition = "unknown"
		valid = valid && before.Participants[0] == expected
		// Intent and an unresolved reason may be saved without cleanup acceptance.
		expected.NoticeRetirementIntent = saved.Participants[0].NoticeRetirementIntent
		expected.Reason = saved.Participants[0].Reason
		valid = valid && saved.Participants[0] == expected
	}
	evidence := fmt.Sprintf("steers=%d starts=%d resumes=%d params=%s pause=%v cleanupReads=%d elapsed=%s caller=%v status=%s reason=%s err=%v beforeLoad=%v load=%v before=%+v saved=%+v beforeLedger=%s beforeExport=%v afterLedger=%s afterExport=%v", steers.Load(), starts.Load(), resumes.Load(), actualWire, pauseErr, cleanupReads.Load(), elapsed, callerErr, result.Status, result.Reason, err, beforeLoadErr, loadErr, before, saved, beforeLedger, beforeExportErr, afterLedger, afterExportErr)
	if callerErr != nil {
		t.Fatal("cleanup caller expired before independent cleanup timeout:", evidence)
	}
	if !valid {
		t.Fatal("cleanup timeout and retained uncertainty unproved:", evidence)
	}
	t.Log(evidence)
}
func TestMaintenanceExpiredRecoveryHasOneSharedAllowance(t *testing.T) {
	var starts, resumes atomic.Int32
	m, o, _ := maintenancePauseFixture(t, false, func(_ *websocket.Conn, request map[string]any) bool {
		switch request["method"] {
		case "turn/start":
			starts.Add(1)
		case "thread/resume":
			resumes.Add(1)
		}
		return request["method"] != "thread/turns/list"
	})
	m.pauseCallTimeout = time.Second
	m.pauseRecoveryTimeout = 40 * time.Millisecond
	deadline := time.Now().Add(-time.Hour)
	_, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.PauseDeadline = deadline; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	result, err := m.pause(ctx, o.OperationID)
	elapsed, callerErr := time.Since(start), ctx.Err()
	saved, loadErr := m.store.Load(o.OperationID)
	valid := err != nil && maintenanceControlFailureReason(err) == "pause-interrupt-timeout" && callerErr == nil && result.Status != "paused" && loadErr == nil && saved.PauseDeadline.Equal(deadline) && starts.Load() == 0 && resumes.Load() == 0 && len(saved.Participants) == 1
	valid = valid && saved.OperationID == o.OperationID && saved.Profile == o.Profile && saved.SourceProfileToken == o.SourceProfileToken && saved.SourcePackage == o.SourcePackage && saved.CandidatePackage == o.CandidatePackage && saved.Phase == o.Phase && saved.CutoverIntent == o.CutoverIntent && saved.SourcesStopped == o.SourcesStopped && saved.ActivationIntent == o.ActivationIntent && saved.ActivationPackage == o.ActivationPackage && saved.ActivationNative == o.ActivationNative && saved.ActivationPlansDigest == o.ActivationPlansDigest
	if len(saved.Participants) == 1 {
		valid = valid && saved.Participants[0] == o.Participants[0]
	}
	evidence := fmt.Sprintf("elapsed=%s caller=%v status=%s reason=%s err=%v load=%v starts=%d resumes=%d deadline=%s saved=%+v", elapsed, callerErr, result.Status, maintenanceControlFailureReason(err), err, loadErr, starts.Load(), resumes.Load(), deadline.UTC().Format(time.RFC3339Nano), saved)
	if !valid {
		t.Fatal("shared recovery timeout or saved identity unproved:", evidence)
	}
	t.Log(evidence)
}

func TestMaintenanceSharedAllowanceDoesNotRenewAcrossCalls(t *testing.T) {
	m, o := maintenanceWorkerFixture(t, false)
	m.pauseCallTimeout = 3 * time.Minute
	allowance := time.Now().Add(time.Minute)
	grace := allowance.Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), allowance.Add(2*time.Minute))
	defer cancel()
	for i := 0; i < 2; i++ {
		entered := false
		err := m.pauseNative(ctx, o.OperationID, grace, allowance, func(call context.Context) error {
			entered = true
			if deadline, ok := call.Deadline(); !ok || !deadline.Equal(allowance) {
				t.Fatalf("call %d renewed allowance: %v, deadline present=%t", i, deadline, ok)
			}
			return nil
		})
		if err != nil || !entered {
			t.Fatal("shared-allowance call not admitted", i, err)
		}
	}
	entered := false
	err := m.pauseNative(ctx, o.OperationID, grace, time.Now().Add(-time.Second), func(context.Context) error {
		entered = true
		return nil
	})
	if entered || maintenanceControlFailureReason(err) != "pause-interrupt-timeout" {
		t.Fatal("expired allowance was renewed", entered, err)
	}
}

func TestMaintenanceNativePauseCapDerivesBoundedDeadline(t *testing.T) {
	m, o := maintenanceWorkerFixture(t, false)
	m.pauseCallTimeout = 500 * time.Millisecond
	grace := time.Now().Add(time.Second)
	allowance := grace.Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), allowance.Add(time.Second))
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	var bounded context.Context
	var callbackEntry, deadline time.Time
	var hasDeadline bool
	before := time.Now()
	err := m.pauseNative(ctx, o.OperationID, grace, allowance, func(call context.Context) error {
		callbackEntry = time.Now()
		bounded = call
		deadline, hasDeadline = call.Deadline()
		return nil
	})
	if err != nil || bounded == nil || ctx.Err() != nil {
		t.Fatal("native-cap callback not entered under live parent", err, bounded, ctx.Err())
	}
	if !hasDeadline || deadline.Before(before.Add(500*time.Millisecond)) || deadline.After(callbackEntry.Add(500*time.Millisecond)) || !deadline.Before(grace) || !deadline.Before(allowance) || !deadline.Before(parentDeadline) {
		t.Fatalf("native-call cap deadline outside 500ms derivation bracket: before=%s entry=%s deadline=%s present=%t grace=%s allowance=%s parent=%s", before, callbackEntry, deadline, hasDeadline, grace, allowance, parentDeadline)
	}
	if !errors.Is(bounded.Err(), context.Canceled) {
		t.Fatal("derived native context not cancelled after callback return", bounded.Err())
	}
	t.Logf("native-cap deadline=%s bracket=[%s,%s] grace=%s allowance=%s parent=%s derived=%v", deadline, before.Add(500*time.Millisecond), callbackEntry.Add(500*time.Millisecond), grace, allowance, parentDeadline, bounded.Err())
}

func TestMaintenanceCallerDeadlineWinsNativePauseCap(t *testing.T) {
	// Deadline selection is metadata: no RPC must arrive inside this caller window.
	m, o := maintenanceWorkerFixture(t, false)
	m.pauseCallTimeout = 500 * time.Millisecond
	grace := time.Now().Add(time.Second)
	allowance := grace.Add(time.Second)
	if _, err := m.store.Update(context.Background(), o.OperationID, 0, func(saved *maintenance.Operation) error {
		saved.Phase, saved.CutoverIntent, saved.SourcesStopped = "prepared", false, false
		saved.PauseDeadline = grace
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := m.store.Load(o.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	callerDeadline := time.Now().Add(250 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()
	var bounded context.Context
	var deadline time.Time
	var hasDeadline, enteredLive bool
	nativeBound := time.Now().Add(500 * time.Millisecond)
	err = m.pauseNative(ctx, o.OperationID, grace, allowance, func(call context.Context) error {
		bounded = call
		deadline, hasDeadline = call.Deadline()
		enteredLive = ctx.Err() == nil && call.Err() == nil
		return nil
	})
	if err != nil || bounded == nil || !enteredLive || ctx.Err() != nil || !hasDeadline || !deadline.Equal(callerDeadline) ||
		!deadline.Before(nativeBound) || !deadline.Before(grace) || !deadline.Before(allowance) {
		t.Fatal("caller deadline selection changed", deadline, callerDeadline, enteredLive, ctx.Err(), err)
	}
	if !errors.Is(bounded.Err(), context.Canceled) {
		t.Fatal("derived context not canceled after callback return", bounded.Err())
	}
	saved, loadErr := m.store.Load(o.OperationID)
	if loadErr != nil || !reflect.DeepEqual(saved, before) {
		t.Fatal("deadline selection changed saved operation", saved, before, loadErr)
	}
	t.Logf("metadata caller=%s inherited=%s parent=%v derived=%v saved=unchanged", callerDeadline, deadline, ctx.Err(), bounded.Err())
}

func TestMaintenanceExpiredCallerDeadlineRefusesNativePause(t *testing.T) {
	// An already-expired real parent proves refusal without timer or I/O scheduling.
	m, o := maintenanceWorkerFixture(t, false)
	m.pauseCallTimeout = 500 * time.Millisecond
	grace := time.Now().Add(time.Second)
	allowance := grace.Add(time.Second)
	if _, err := m.store.Update(context.Background(), o.OperationID, 0, func(saved *maintenance.Operation) error {
		saved.Phase, saved.CutoverIntent, saved.SourcesStopped = "prepared", false, false
		saved.PauseDeadline = grace
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := m.store.Load(o.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	entered := false
	err = m.pauseNative(ctx, o.OperationID, grace, allowance, func(context.Context) error {
		entered = true
		return nil
	})
	var proof *maintenance.ProofError
	if entered || err == nil || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, errMaintenanceGraceExpired) || errors.As(err, &proof) || maintenanceControlFailureReason(err) != "maintenance-timeout" {
		t.Fatal("expired caller refusal changed", entered, err, proof)
	}
	saved, loadErr := m.store.Load(o.OperationID)
	if loadErr != nil || !reflect.DeepEqual(saved, before) {
		t.Fatal("expired caller changed saved operation", saved, before, loadErr)
	}
	t.Logf("expired caller callback=%t reason=%s err=%v saved=unchanged", entered, maintenanceControlFailureReason(err), err)
}

func TestMaintenanceCallerCancellationBoundsInitializedSilentRead(t *testing.T) {
	// Publish exact receipt before cancelling; timed native expiry is a separate neighbor.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var armed atomic.Bool
	var reached, steers, starts, resumes, interrupts atomic.Int32
	requests := make(chan map[string]any, 1)
	m, o, _ := maintenancePauseFixture(t, false, func(_ *websocket.Conn, request map[string]any) bool {
		switch request["method"] {
		case "turn/steer":
			steers.Add(1)
		case "turn/start":
			starts.Add(1)
		case "thread/resume":
			resumes.Add(1)
		case "turn/interrupt":
			interrupts.Add(1)
		case "thread/turns/list":
			if armed.Load() {
				reached.Add(1)
				select {
				case requests <- request:
				default:
				}
				cancel()
				return false
			}
		}
		return true
	})
	m.pauseCallTimeout = 500 * time.Millisecond
	if len(o.Participants) != 1 {
		t.Fatal("fixture participant missing", o.Participants)
	}
	c := m.client(o, o.Participants[0])
	defer c.Close()
	setup, setupCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer setupCancel()
	if active, err := c.ActiveTurnID(setup, "thread"); err != nil || active != "captured" {
		t.Fatal("initialize real pause client", active, err)
	}
	before, err := m.store.Load(o.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	grace := time.Now().Add(time.Second)
	allowance := grace.Add(time.Second)
	var bounded context.Context
	err = m.pauseNative(ctx, o.OperationID, grace, allowance, func(call context.Context) error {
		bounded = call
		_, err := c.ActiveTurnID(call, "thread")
		return err
	})
	var request map[string]any
	select {
	case request = <-requests:
	default:
	}
	actualParams, _ := json.Marshal(request["params"])
	expectedParams, _ := json.Marshal(map[string]any{"threadId": "thread", "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"})
	var proof *maintenance.ProofError
	if reached.Load() != 1 || request["method"] != "thread/turns/list" || request["id"] == nil || string(actualParams) != string(expectedParams) ||
		bounded == nil || !errors.Is(bounded.Err(), context.Canceled) || !errors.Is(ctx.Err(), context.Canceled) ||
		err == nil || !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errMaintenanceGraceExpired) || errors.As(err, &proof) || maintenanceControlFailureReason(err) != "maintenance-cancelled" {
		t.Fatal("receipt-triggered caller cancellation unproved", request, reached.Load(), bounded, ctx.Err(), err, proof)
	}
	if steers.Load() != 0 || starts.Load() != 0 || resumes.Load() != 0 || interrupts.Load() != 0 {
		t.Fatal("effects after caller cancel", steers.Load(), starts.Load(), resumes.Load(), interrupts.Load())
	}
	saved, loadErr := m.store.Load(o.OperationID)
	if loadErr != nil || !reflect.DeepEqual(saved, before) || saved.Phase != "prepared" || saved.CutoverIntent ||
		!saved.PauseDeadline.Equal(o.PauseDeadline) || len(saved.Participants) != 1 || saved.Participants[0].NoticeDisposition != "prepared" {
		t.Fatal("caller cancellation changed pause state", saved, before, loadErr)
	}
	t.Logf("initialized silent request id=%v method=%s params=%s receipts=%d reason=%s parent=%v derived=%v effects=0 saved=unchanged socket=%s store=%s",
		request["id"], request["method"], actualParams, reached.Load(), maintenanceControlFailureReason(err), ctx.Err(), bounded.Err(), o.Services[0].Socket, m.store.Root)
}

func TestMaintenanceGraceExpiryRetainsUnknownSteerForRecovery(t *testing.T) {
	var steers, starts, resumes, interrupts atomic.Int32
	m, o, _ := maintenancePauseFixture(t, false, func(c *websocket.Conn, request map[string]any) bool {
		switch request["method"] {
		case "turn/start":
			starts.Add(1)
		case "thread/resume":
			resumes.Add(1)
		case "turn/interrupt":
			interrupts.Add(1)
		case "turn/steer":
			steers.Add(1)
			// Establish an actual ambiguous submission before exercising expired-grace
			// recovery. A short wall-clock grace can expire during cold client/ledger
			// setup, in which case no steer has been submitted and Unknown is unproved.
			if err := threadCommandWriteObject(c, map[string]any{"id": request["id"], "error": map[string]any{"code": -32000, "message": "fixture ambiguous steer outcome"}}); err != nil {
				t.Errorf("write ambiguous steer response: %v", err)
			}
			return false
		}
		return true
	})
	deadline := time.Now().Add(90 * time.Second)
	if _, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.PauseDeadline = deadline; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := m.pause(ctx, o.OperationID)
	var unknown *codex.UnknownSendOutcomeError
	if steers.Load() != 1 || !errors.As(err, &unknown) || result.Status == "paused" || maintenanceControlFailureReason(err) != "notice-outcome-unknown" || ctx.Err() != nil {
		t.Fatal("actual steer uncertainty unproved", result, err, steers.Load(), ctx.Err())
	}
	saved, loadErr := m.store.Load(o.OperationID)
	if loadErr != nil || !saved.PauseDeadline.Equal(deadline) || saved.Participants[0].NoticeDisposition != "unknown" || saved.Phase == "draining-writers" {
		t.Fatal(saved, loadErr)
	}
	c := m.client(o, o.Participants[0])
	defer c.Close()
	before, exportErr := c.ExportSubmissionAttempts(o.Participants[0].Thread)
	if exportErr != nil {
		t.Fatal(exportErr)
	}
	var ledger struct {
		Sends map[string]map[string]struct {
			State   string
			Steered bool
		}
	}
	if err := json.Unmarshal(before, &ledger); err != nil {
		t.Fatal(err)
	}
	attempt := ledger.Sends[o.Participants[0].Thread][o.Participants[0].NoticeID]
	if attempt.State != "submitting" || !attempt.Steered {
		t.Fatalf("original uncertain steer missing from durable ledger: %s", before)
	}
	// Model the next recovery call after the persisted grace has elapsed. This
	// phase has no scheduler-dependent window before the original native write.
	deadline = time.Now().Add(-time.Second)
	if _, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.PauseDeadline = deadline; return nil }); err != nil {
		t.Fatal(err)
	}
	recovery, stopRecovery := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopRecovery()
	result, err = m.pause(recovery, o.OperationID)
	saved, loadErr = m.store.Load(o.OperationID)
	after, afterErr := c.ExportSubmissionAttempts(o.Participants[0].Thread)
	if err == nil || result.Status == "paused" || recovery.Err() != nil || loadErr != nil || afterErr != nil || !saved.PauseDeadline.Equal(deadline) || saved.Participants[0].NoticeDisposition != "unknown" || saved.Participants[0].Reason != "notice-outcome-unknown" || saved.Phase == "draining-writers" || string(before) != string(after) || steers.Load() != 1 || starts.Load() != 0 || resumes.Load() != 0 || interrupts.Load() != 0 {
		t.Fatalf("expired grace lost unresolved steer: result=%+v err=%v parent=%v saved=%+v load=%v ledger=%s after=%s export=%v steers=%d starts=%d resumes=%d interrupts=%d", result, err, recovery.Err(), saved, loadErr, before, after, afterErr, steers.Load(), starts.Load(), resumes.Load(), interrupts.Load())
	}
}
