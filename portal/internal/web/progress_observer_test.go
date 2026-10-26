package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type progressSend struct {
	thread, id, message string
	options             codex.TurnOptions
}
type progressTestClient struct {
	*workspacecodex.Client
	transcripts  map[string]codex.Transcript
	sends        []progressSend
	interrupts   []string
	cwd, rollout string
	unknown      bool
	deferred     bool
}

func (c *progressTestClient) ReadThreadMetadata(_ context.Context, thread string, _ bool) (codex.ThreadMetadata, error) {
	if c.rollout == "" {
		return codex.ThreadMetadata{}, errors.New("history missing")
	}
	return codex.ThreadMetadata{ID: thread, Cwd: c.cwd, Path: &c.rollout}, nil
}
func (c *progressTestClient) VerifyThread(_ context.Context, thread, _ string) error {
	if _, ok := c.transcripts[thread]; !ok {
		return errors.New("unknown fixture thread")
	}
	return nil
}
func (c *progressTestClient) ReadThread(_ context.Context, thread string) (codex.Transcript, error) {
	v, ok := c.transcripts[thread]
	if !ok {
		return v, errors.New("unknown fixture thread")
	}
	return v, nil
}
func (c *progressTestClient) ReadThreadPage(ctx context.Context, thread, _ string) (codex.TranscriptPage, error) {
	v, err := c.ReadThread(ctx, thread)
	return codex.TranscriptPage{Transcript: v}, err
}
func (c *progressTestClient) CompactAcceptedSendOptions(string) error { return nil }
func (c *progressTestClient) SendWithOptions(_ context.Context, thread, message, id, _ string, opts codex.TurnOptions) (codex.SendReceipt, error) {
	c.sends = append(c.sends, progressSend{thread, id, message, opts})
	if thread == "observer-thread" {
		c.transcripts[thread] = codex.Transcript{ThreadID: thread, LatestTurnID: "scheduled-turn", Status: "active", Entries: []codex.TranscriptEntry{{Kind: "userMessage", Text: message, TurnID: "scheduled-turn", ClientUserMessageID: id}}}
	}
	if c.unknown {
		return codex.SendReceipt{}, errors.New("lost response")
	}
	return codex.SendReceipt{TurnID: "scheduled-turn", ClientUserMessageID: id}, nil
}
func (c *progressTestClient) SendProgressCheck(ctx context.Context, thread, message, id, action string, opts codex.TurnOptions) (codex.SendReceipt, error) {
	if c.deferred {
		return codex.SendReceipt{}, workspacecodex.ErrProgressCheckDeferred
	}
	return c.SendWithOptions(ctx, thread, message, id, action, opts)
}
func (c *progressTestClient) Request(_ context.Context, method string, params, output any) error {
	if method != "turn/interrupt" {
		return errors.New("unexpected fixture request")
	}
	p := params.(map[string]any)
	c.interrupts = append(c.interrupts, p["turnId"].(string))
	return nil
}
func progressFixture(t *testing.T) (*Server, *progressTestClient, *session.Summary, time.Time) {
	t.Helper()
	s := newTestServer(t)
	summary := prepareInteractiveConversation(t, s, "example")
	c := &progressTestClient{Client: workspacecodex.NewWithOptions("/unused.sock", s.config.Workspace, codex.ClientOptions{}), transcripts: map[string]codex.Transcript{
		"thread-1":        {ThreadID: "thread-1", Status: "active", LatestTurnID: "work", Entries: []codex.TranscriptEntry{{Kind: "userMessage", Text: "Deliver the portal fix", TurnID: "work"}}},
		"observer-thread": {ThreadID: "observer-thread", Status: "idle"}}}
	c.cwd = filepath.Join(s.config.Workspace, "work", "example")
	c.rollout = filepath.Join(t.TempDir(), "conversation.jsonl")
	if err := os.WriteFile(c.rollout, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.config.Codex = c
	s.config.VerifyThread = c.VerifyThread
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), "example", "thread-1", true, func(r *teamruntime.Roster) error {
		r.Members = []teamruntime.Member{{Role: "observer", Address: "observer0", Thread: "observer-thread", Behavior: "general", Purpose: "general", Access: "read_only", Instructions: "Observe without tools; return JSON only.", State: "ready", Model: "retained-observer", Effort: "xhigh", AddedAt: time.Now()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	state, _ := s.progressStore().Load("thread-1")
	state.LastCheck = now.Add(-progressobserver.Interval)
	if err := s.progressStore().Save(state); err != nil {
		t.Fatal(err)
	}
	return s, c, summary, now
}
func progressComplete(c *progressTestClient, text string) {
	t := c.transcripts["observer-thread"]
	t.Status = "idle"
	t.Entries = append(t.Entries, codex.TranscriptEntry{Kind: "agentMessage", TurnID: t.LatestTurnID, TurnStatus: "completed", Text: text})
	c.transcripts["observer-thread"] = t
}
func TestProgressIdleSessionsNeverWake(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	root := c.transcripts["thread-1"]
	root.Status = "idle"
	c.transcripts["thread-1"] = root
	for i := 0; i < 3; i++ {
		if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.sends) != 0 {
		t.Fatal("idle session woke", c.sends)
	}
	observer := c.transcripts["observer-thread"]
	observer.Status = "active"
	c.transcripts["observer-thread"] = observer
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 0 {
		t.Fatal("observer activity sustained idle session")
	}
}

func TestProgressColdObserverCanBeScheduled(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "working"}[active], func(t *testing.T) {
			s, c, summary, now := progressFixture(t)
			socket := filepath.Join(t.TempDir(), "codex.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			path := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), s.config.CodexSocket, socket)), 0600); err != nil {
				t.Fatal(err)
			}
			s.config.CodexSocket, s.config.RecoverSessions = socket, true
			writeWebRuntimeAuthority(t, s, "example")
			if err := s.recoveryStore().Set(context.Background(), "example", "thread-1", socket, true, true); err != nil {
				t.Fatal(err)
			}
			if !s.recoveryHeld("example", "observer-thread") {
				t.Fatal("observer fixture is already active")
			}
			if !active {
				root := c.transcripts["thread-1"]
				root.Status = "idle"
				c.transcripts["thread-1"] = root
			}
			if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
				t.Fatal(err)
			}
			want := 0
			if active {
				want = 1
			}
			if len(c.sends) != want {
				t.Fatalf("cold observer sends=%d, want %d", len(c.sends), want)
			}
		})
	}
}

func TestProgressCheckNowRunsOnceWhilePausedAndIdle(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	root := c.transcripts["thread-1"]
	root.Status = "idle"
	c.transcripts["thread-1"] = root
	state, _ := s.progressStore().Load("thread-1")
	state.Paused = true
	state.IntervalMinutes = 60
	if err := s.progressStore().Save(state); err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/sessions/example/progress", strings.NewReader(`{"checkNow":true}`))
		r.Header.Set("Origin", s.config.BaseURL)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(c.sends) != 1 {
		t.Fatal("manual check did not dispatch once", c.sends)
	}
	if w := request(); w.Code != 409 {
		t.Fatal("overlapping manual check accepted", w.Code, w.Body.String())
	}
	progressComplete(c, `{"status":"concern","key":"loop","summary":"Repeated checks.","recommendation":"Deliver the result."}`)
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	state, _ = s.progressStore().Load("thread-1")
	if !state.Paused || state.IntervalMinutes != 60 || state.Pending != nil || state.Concern == nil || len(c.sends) != 1 {
		t.Fatal("manual check changed automatic settings or woke idle lead", state, c.sends)
	}
}

func TestProgressUndispatchedManualRetryDeferralCanBeRequestedAgain(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	state, _ := s.progressStore().Load("thread-1")
	state.Paused = true
	if err := s.progressStore().Save(state); err != nil {
		t.Fatal(err)
	}
	c.deferred = true
	if err := s.checkProgress(context.Background(), summary, now, true); !errors.Is(err, workspacecodex.ErrProgressCheckDeferred) {
		t.Fatal("manual retry ownership did not defer progress", err)
	}
	state, _ = s.progressStore().Load("thread-1")
	if state.Pending != nil || len(c.sends) != 0 {
		t.Fatal("undispatched check left pending ownership", state, c.sends)
	}
	c.deferred = false
	if err := s.checkProgress(context.Background(), summary, now.Add(progressobserver.Deadline+time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 1 {
		t.Fatal("deferred check could not be requested again", c.sends)
	}
}

func TestProgressLaterRetryDeferralPreservesUnknownSubmission(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	state, _ := s.progressStore().Load("thread-1")
	state.Pending = &progressobserver.Pending{ID: "retained-unknown", Thread: "observer-thread", Address: "observer0", Model: "retained-observer", Effort: "xhigh", Message: "original", StartedAt: now}
	if err := s.progressStore().Save(state); err != nil {
		t.Fatal(err)
	}
	c.deferred = true
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); !errors.Is(err, workspacecodex.ErrProgressCheckDeferred) {
		t.Fatal(err)
	}
	state, _ = s.progressStore().Load("thread-1")
	if state.Pending == nil || state.Pending.ID != "retained-unknown" {
		t.Fatal("later refusal discarded an earlier uncertain send", state)
	}
}
func TestProgressQuietSuccessAndRestartSettlement(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 1 || c.sends[0].thread != "observer-thread" || c.sends[0].options.Model != "retained-observer" {
		t.Fatal(c.sends)
	}
	for i := 0; i < 2; i++ {
		if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.sends) != 1 {
		t.Fatal("duplicate pending check")
	}
	progressComplete(c, `{"status":"on_track"}`)
	restarted := &Server{config: s.config, operationStore: s.operationStore, hostProfile: s.hostProfile}
	if err := restarted.stepProgressObserver(context.Background(), summary, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, _ := s.progressStore().Load("thread-1")
	if state.Pending != nil || state.Concern != nil || len(c.sends) != 1 {
		t.Fatal("healthy result woke lead", state, c.sends)
	}
}
func TestProgressConcernsEscalateAndCaptureLeadResponse(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	result := `{"status":"concern","key":"verification-loop","summary":"Equivalent index metadata checks repeated.","recommendation":"Use the logical staged diff and run the remaining acceptance check once."}`
	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i) * progressobserver.Interval)
		if err := s.stepProgressObserver(context.Background(), summary, at); err != nil {
			t.Fatal(err)
		}
		progressComplete(c, result)
		if err := s.stepProgressObserver(context.Background(), summary, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			lead := c.transcripts["thread-1"]
			lead.Entries = append(lead.Entries, codex.TranscriptEntry{Kind: "agentMessage", Text: "Progress response verification-loop: Adjust: run the remaining check, then deliver."})
			c.transcripts["thread-1"] = lead
		}
	}
	state, _ := s.progressStore().Load("thread-1")
	if !state.Warning() || state.Concern.Response == "" || len(c.sends) != 4 || c.sends[1].thread != "thread-1" {
		t.Fatal("unresolved concern did not challenge again", state, c.sends)
	}
}
func TestProgressUnknownSendReconcilesWithoutReplacement(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	c.unknown = true
	if err := s.stepProgressObserver(context.Background(), summary, now); err == nil {
		t.Fatal("lost response not reported")
	}
	c.unknown = false
	progressComplete(c, `{"status":"on_track"}`)
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 1 {
		t.Fatal("unknown accepted submission was replaced")
	}
}
func TestProgressDeadlineTargetsOnlyOwnedTurn(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(progressobserver.Deadline)); err != nil {
		t.Fatal(err)
	}
	if len(c.interrupts) != 1 || c.interrupts[0] != "scheduled-turn" {
		t.Fatal(c.interrupts)
	}
	other := c.transcripts["observer-thread"]
	other.LatestTurnID = "manual-turn"
	c.transcripts["observer-thread"] = other
	_ = s.stepProgressObserver(context.Background(), summary, now.Add(4*time.Minute))
	if len(c.interrupts) != 1 {
		t.Fatal("manual observer work interrupted")
	}
}
func TestProgressFinishingCheckDoesNotWakeIdleLead(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	root := c.transcripts["thread-1"]
	root.Status = "idle"
	c.transcripts["thread-1"] = root
	progressComplete(c, `{"status":"concern","key":"loop","summary":"Repeated checks.","recommendation":"Deliver the verified result."}`)
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 1 {
		t.Fatal("late concern woke idle lead")
	}
}
func TestProgressAPIControlsAndIsolation(t *testing.T) {
	s, _, _, _ := progressFixture(t)
	request := func(method, origin, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/sessions/example/progress", strings.NewReader(body))
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "https://foreign.example", `{"enabled":false}`); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", s.config.BaseURL, `{"enabled":false,"intervalMinutes":7}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := request("GET", "", "")
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["enabled"] != false || value["available"] != true || value["intervalMinutes"] != float64(7) {
		t.Fatal(value)
	}
	other, err := s.progressStore().Load("foreign-root")
	if err != nil || other.Paused {
		t.Fatal("cross-session write", other, err)
	}
	if other.CheckInterval() != progressobserver.Interval {
		t.Fatal("cross-session interval changed")
	}
	for _, body := range []string{`{"intervalMinutes":0}`, `{"intervalMinutes":1441}`, `{"intervalMinutes":1.5}`, `{"checkNow":true,"enabled":false}`} {
		if w := request("POST", s.config.BaseURL, body); w.Code != 400 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
}
func TestProgressResultRequiresScheduledCompletedTurn(t *testing.T) {
	p := &progressobserver.Pending{Turn: "owned"}
	other := codex.Transcript{LatestTurnID: "manual", Status: "idle", Entries: []codex.TranscriptEntry{{Kind: "agentMessage", Text: `{"status":"on_track"}`, TurnID: "manual", TurnStatus: "completed"}}}
	if _, done, _ := progressResult(other, p); done {
		t.Fatal("accepted another turn")
	}
}
func TestProgressBrowserPresentation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node required")
	}
	if output, err := exec.Command(node, "progress_observer_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
}

func TestProgressLifecycleAndMissingObserverDoNotWake(t *testing.T) {
	for _, scenario := range []string{"lifecycle", "no-observer"} {
		t.Run(scenario, func(t *testing.T) {
			s, c, summary, now := progressFixture(t)
			if scenario == "lifecycle" {
				directory := filepath.Join(s.config.Workspace, "worktrees", ".locks")
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "example.archive.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				store, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
				if _, err := store.Update(context.Background(), "example", "thread-1", false, func(r *teamruntime.Roster) error { r.Members = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
				t.Fatal(err)
			}
			if len(c.sends) != 0 {
				t.Fatal("ineligible session woke", c.sends)
			}
		})
	}
}

func TestProgressHumanWaitIsIdle(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	for _, reason := range []string{"userInput", "approval", "subagents"} {
		s.activity = &activityMonitor{clock: func() time.Time { return now }, snapshots: map[string]activityCachedSnapshot{"thread-1": {at: now, value: codex.ActivitySnapshot{ThreadID: "thread-1", CurrentTurnID: "work", CurrentState: "waiting", WaitReason: reason}}}}
		active, err := s.progressWorking(context.Background(), codex.Transcript{ThreadID: "thread-1", LatestTurnID: "work", Status: "active"})
		if err != nil || active != (reason == "subagents") {
			t.Fatalf("%s: active=%v error=%v", reason, active, err)
		}
	}
	s.activity = nil
}

func TestProgressPendingTimeoutSurvivesFailedDevelopmentEvidence(t *testing.T) {
	for _, broken := range []string{"thread-1", "implementer-thread"} {
		t.Run(broken, func(t *testing.T) {
			s, c, summary, now := progressFixture(t)
			if broken == "implementer-thread" {
				c.transcripts[broken] = codex.Transcript{ThreadID: broken, Status: "active"}
				store, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
				_, err := store.Update(context.Background(), "example", "thread-1", false, func(r *teamruntime.Roster) error {
					r.Members = append(r.Members, teamruntime.Member{Role: "implementer", Address: "implementer0", Thread: broken, State: "ready", AddedAt: now})
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
				t.Fatal(err)
			}
			delete(c.transcripts, broken)
			if err := s.stepProgressObserver(context.Background(), summary, now.Add(progressobserver.Deadline)); err != nil {
				t.Fatal(err)
			}
			if len(c.interrupts) != 1 || c.interrupts[0] != "scheduled-turn" {
				t.Fatal("unrelated evidence prevented deadline", c.interrupts)
			}
		})
	}
}

func TestProgressRetiredObserverDoesNotBlockReplacement(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	progressComplete(c, `{"status":"on_track"}`)
	store, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	_, err := store.Update(context.Background(), "example", "thread-1", false, func(r *teamruntime.Roster) error {
		retired := now.Add(time.Second)
		r.Members[0].State = "removed"
		r.Members[0].RemovedAt = &retired
		replacement := r.Members[0]
		replacement.Index = 1
		replacement.Address = "observer1"
		replacement.Thread = "replacement"
		replacement.State = "ready"
		replacement.RemovedAt = nil
		r.Members = append(r.Members, replacement)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.transcripts["replacement"] = codex.Transcript{ThreadID: "replacement", Status: "idle"}
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, _ := s.progressStore().Load("thread-1")
	if state.Pending != nil {
		t.Fatal("retired pending check persisted", state)
	}
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute+progressobserver.Interval)); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 2 || c.sends[1].thread != "replacement" {
		t.Fatal("replacement did not receive fresh check", c.sends)
	}
}

func TestProgressUnknownUnsentCheckDoesNotWakeIdleSession(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	root := c.transcripts["thread-1"]
	root.Status = "idle"
	c.transcripts["thread-1"] = root
	state, _ := s.progressStore().Load("thread-1")
	state.Pending = &progressobserver.Pending{ID: progressobserver.ID("unconfirmed"), Address: "observer0", Thread: "observer-thread", Model: "retained-observer", Effort: "xhigh", Message: "unchanged original", StartedAt: now}
	if err := s.progressStore().Save(state); err != nil {
		t.Fatal(err)
	}
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 0 {
		t.Fatal("unknown unsent check started after development stopped")
	}
}
