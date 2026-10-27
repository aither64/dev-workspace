package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func installRecoveryFailureHelper(t *testing.T, s *Server, code string) string {
	t.Helper()
	helper, calls := filepath.Join(t.TempDir(), "dev-session"), filepath.Join(t.TempDir(), "calls")
	prior := "'" + strings.ReplaceAll(s.config.DevSession, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\nif [ \"$1\" != resume ]; then exec " + prior + " \"$@\"; fi\nprintf '%s\\n' \"$*\" >> '" + calls + "'\necho 'another dev-session command is changing example' >&2\nexit " + code + "\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.config.DevSession = helper
	return calls
}

func failRecoveryHelper(t *testing.T, s *Server, code string) string {
	t.Helper()
	calls := installRecoveryFailureHelper(t, s, code)
	s.restoreRuntime(context.Background(), "example")
	return calls
}

func TestRecoveryTemporaryFailureRetiresAfterRuntimeBecomesLive(t *testing.T) {
	for _, kind := range []string{"active", "idle", "queue", "goal", "unknown", "original"} {
		t.Run(kind, func(t *testing.T) {
			s, c, summary := recoveryNoticeTestServer(t)
			ctx := context.Background()
			c.status = map[string]any{"type": "notLoaded"}
			want := "waiting"
			switch kind {
			case "active":
				want = "active"
			case "idle", "original":
				want = "idle"
			case "queue":
				c.queue = []codex.QueueEntry{{ID: "old", Text: "saved request"}}
			case "goal":
				c.goal = true
			case "unknown":
				want = "unavailable"
				c.status = map[string]any{"type": "systemError"}
			}
			var original codex.SendReceipt
			if kind == "original" {
				c.sendErr = &codex.UnknownSendOutcomeError{Err: errors.New("lost receipt")}
				original, _ = c.browserContractCodex.Send(ctx, "thread-1", "original", "original-id", "")
			}
			if err := s.recoveryStore().Update(ctx, "example", func(record *session.Recovery) error {
				record.Continuation = &session.RecoveryContinuation{RequestID: "existing-decision", Prompt: true}
				if kind == "active" {
					record.ActiveThreads = []string{"thread-1"}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			calls := failRecoveryHelper(t, s, "75")
			if status := s.recoveryStatus(summary); status.State != "failed" || !status.Retry {
				t.Fatal("fixture did not exercise actual temporary helper failure", status)
			}
			before, err := s.recoveryStore().Load("example")
			if err != nil {
				t.Fatal(err)
			}
			beforeSends, beforeQueue := c.sendCount, append([]codex.QueueEntry(nil), c.queue...)
			if s.recoverRuntimeSession(ctx, *summary, time.Now().Add(time.Hour), make(chan struct{}, 2)) {
				t.Fatal("live runtime launched another restore during backoff")
			}
			status := s.recoveryStatus(summary)
			if status.State != want || status.ExecutionHeld != (kind != "active") {
				t.Fatal("live runtime retained admission failure or lost work blocker", status)
			}
			after, err := s.recoveryStore().Load("example")
			if err != nil || !reflect.DeepEqual(before, after) || len(c.calls) != 0 || c.sendCount != beforeSends || !reflect.DeepEqual(beforeQueue, c.queue) {
				t.Fatal("reconciliation changed permission, continuation or executed work", before, after, c.calls, err)
			}
			if kind == "original" {
				receipt, found, err := c.ReconcileSend(ctx, "thread-1", "original", "original-id", "")
				var unknown *codex.UnknownSendOutcomeError
				if !errors.As(err, &unknown) || receipt != original || found || c.sendCount != beforeSends {
					t.Fatal("unknown original changed", receipt, found, err)
				}
			}
			if data, err := os.ReadFile(calls); err != nil || !bytes.Equal(data, []byte("resume example --as-is\n")) {
				t.Fatal("helper was replayed", string(data), err)
			}
		})
	}
}

func TestRecoveryTemporaryFailureKeepsUnprovenOrHardFailures(t *testing.T) {
	for _, refusal := range []string{"hard", "automatic", "root", "socket", "generation", "provider", "lifecycle", "creation", "creation-job", "runtime", "native"} {
		t.Run(refusal, func(t *testing.T) {
			s, c, summary := recoveryNoticeTestServer(t)
			code := "75"
			if refusal == "hard" {
				code = "1"
			}
			calls := failRecoveryHelper(t, s, code)
			observed := s.recoveryOperations["example"]
			switch refusal {
			case "automatic":
				if err := s.recoveryStore().Update(context.Background(), "example", func(record *session.Recovery) error { record.Automatic = false; return nil }); err != nil {
					t.Fatal(err)
				}
			case "root":
				summary.Codex.ThreadID = "different-root"
			case "socket":
				summary.Codex.SocketPath = "/different.sock"
			case "generation":
				if err := os.Remove(s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
			case "provider":
				stateRoot := s.config.UserStateRoot
				providerTestCatalog(t, s, "http://127.0.0.1:1")
				s.config.UserStateRoot = stateRoot
				path, _ := codexproviders.StatePath(stateRoot, s.config.WorkspaceName)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				state := codexproviders.State{Schema: 2, Workspace: s.config.WorkspaceName, Root: s.config.Workspace, Socket: s.config.CodexSocket, Active: "pool", Operation: &codexproviders.Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "openai", CatalogDigest: strings.Repeat("a", 64), Phase: "applying", Strategy: "scoped-v1", Session: "example", Stage: "preparing", Sessions: []string{"example"}, Roots: map[string]string{"example": "thread-1"}}}
				data, _ := json.Marshal(state)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if s.requireProviderAdmission("example") == nil {
					t.Fatal("fixture did not reserve provider")
				}
			case "lifecycle":
				dir := filepath.Join(s.config.Workspace, "worktrees", ".locks")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "example.archive.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "creation":
				path := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
				data, _ := os.ReadFile(path)
				if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte("state: ready"), []byte("state: creating")), 0600); err != nil {
					t.Fatal(err)
				}
			case "creation-job":
				s.creations["example"] = creationReceipt{State: "running"}
			case "runtime":
				if err := os.Remove(filepath.Join(s.config.AuthorityDir, "example.json")); err != nil {
					t.Fatal(err)
				}
			case "native":
				s.config.VerifyThread = func(context.Context, string, string) error { return errors.New("native identity unknown") }
			}
			s.retireTemporaryRecovery(context.Background(), summary, observed)
			if got := s.recoveryOperations["example"]; got != observed || len(c.calls) != 0 || c.sendCount != 0 {
				t.Fatal("unproven recovery cleared or dispatched", got, observed, c.calls)
			}
			if data, err := os.ReadFile(calls); err != nil || string(data) != "resume example --as-is\n" {
				t.Fatal("repeated helper", string(data), err)
			}
		})
	}
}

func TestRecoveryTemporaryFailurePreservesReplacementAndOwningJob(t *testing.T) {
	for _, replacement := range []string{"hard", "temporary", "job", "closing"} {
		t.Run(replacement, func(t *testing.T) {
			s, _, summary := recoveryNoticeTestServer(t)
			failRecoveryHelper(t, s, "75")
			observed := s.recoveryOperations["example"]
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			verify := s.config.VerifyThread
			first := true
			s.config.VerifyThread = func(ctx context.Context, thread, cwd string) error {
				if first {
					first = false
					close(entered)
					<-release
				}
				return verify(ctx, thread, cwd)
			}
			go func() { defer close(done); s.retireTemporaryRecovery(context.Background(), summary, observed) }()
			<-entered
			newer := recoveryStatus{State: "failed", Error: "new identity refusal"}
			switch replacement {
			case "temporary":
				newer.Retry = true
			case "job":
				jobRelease := make(chan struct{})
				if !s.launchRecovery("example", func() { <-jobRelease }) {
					t.Fatal("job refused")
				}
				defer close(jobRelease)
				// Even an identical snapshot must survive while a job owns it.
				newer = observed
			case "closing":
				s.Close()
				newer = observed
			}
			s.setRecoveryStatus("example", newer)
			close(release)
			<-done
			if got := s.recoveryOperations["example"]; got != newer {
				t.Fatal("newer recovery was retired", got, newer)
			}
		})
	}
}

func TestRecoveryContinueUsesFreshDecisionAfterActivationStopAndSocketLoss(t *testing.T) {
	s, c := recoveryTestServer(t)
	ctx := context.Background()
	first, err := s.acceptContinuation(ctx, "example", "thread-1", "first")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.continueRecovery(ctx, "example", "thread-1", first); err != nil {
		t.Fatal(err)
	}
	record, err := s.recoveryStore().Load("example")
	if err != nil || record.Continuation != nil {
		t.Fatal("completed decision retained", record, err)
	}
	c.queue = nil // The native worker consumed the first prompt.
	if err := s.recoveryStore().Set(ctx, "example", "thread-1", s.config.CodexSocket, false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.recoveryStore().Set(ctx, "example", "thread-1", s.config.CodexSocket, true, false); err != nil {
		t.Fatal(err)
	}
	second, err := s.acceptContinuation(ctx, "example", "thread-1", "second")
	if err != nil || second != "second" {
		t.Fatal("stop reused completed receipt", second, err)
	}
	if err := s.continueRecovery(ctx, "example", "thread-1", second); err != nil {
		t.Fatal(err)
	}
	c.queue = nil
	// Simulate losing only App Server: tmux and portal survive, so Set need not run.
	if err := os.Remove(s.config.CodexSocket); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", s.config.CodexSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	third, err := s.acceptContinuation(ctx, "example", "thread-1", "third")
	if err != nil || third != "third" {
		t.Fatal("socket loss reused receipt", third, err)
	}
	if err := s.continueRecovery(ctx, "example", "thread-1", third); err != nil {
		t.Fatal(err)
	}
	if len(c.queue) != 1 || c.queue[0].ClientUserMessageID != "third" {
		t.Fatal("new recovery did not continue", c.queue)
	}
}

func TestRecoveryHelperTemporaryFailureRemainsEligibleForAutomaticRetry(t *testing.T) {
	s, _ := recoveryTestServer(t)
	marker := filepath.Join(t.TempDir(), "first-attempt")
	helper := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nif [ ! -e '"+marker+"' ]; then touch '"+marker+"'; echo 'native request temporarily unavailable' >&2; exit 75; fi\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.config.DevSession = helper
	s.restoreRuntime(context.Background(), "example")
	failed := s.recoveryOperations["example"]
	if failed.State != "failed" || !failed.Retry {
		t.Fatal("transient helper failure became permanent", failed)
	}
	s.restoreRuntime(context.Background(), "example")
	if status := s.recoveryOperations["example"]; status.State != "waiting" {
		t.Fatal("retry did not restore runtime", status)
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho 'identity refused' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.restoreRuntime(context.Background(), "example")
	if status := s.recoveryOperations["example"]; status.State != "failed" || status.Retry {
		t.Fatal("identity refusal became automatic retry", status)
	}
}

type recoveryDeletionCodex struct {
	*recoveryTestCodex
	deleted, completed, reconciled bool
}

func (c *recoveryDeletionCodex) DeleteQueueEntryWithCompletion(_ context.Context, _, _ string, complete func() error) error {
	c.deleted = true
	if err := complete(); err != nil {
		return err
	}
	c.completed = true
	return nil
}
func (c *recoveryDeletionCodex) ReconcileQueueDeletionsWithCompletion(_ context.Context, _ string, complete func(string) error) error {
	c.reconciled = true
	return complete("interrupted-deletion")
}

func TestRecoveryHeldQueuePreservesAttachmentDeletionCompletion(t *testing.T) {
	s, base := recoveryTestServer(t)
	c := &recoveryDeletionCodex{recoveryTestCodex: base}
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	if err := client.DeleteQueueEntryWithCompletion(context.Background(), "thread-1", "entry", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := (recoveryPagedConversationClient{client}).ReconcileQueueDeletionsWithCompletion(context.Background(), "thread-1", func(id string) error {
		if id != "interrupted-deletion" {
			t.Fatal(id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !c.deleted || !c.completed || !c.reconciled || len(base.calls) != 0 || !s.recoveryHeld("example", "thread-1") {
		t.Fatal("deletion lost completion or loaded thread", c)
	}
}

type recoveryTestCodex struct {
	*browserContractCodex
	queue          []codex.QueueEntry
	goal           bool
	calls          []string
	failActivation bool
	workspace      string
	status         map[string]any
	turnsErr       error
	notPrepared    bool
	preparations   int
	beforeSend     func()
	beforeMode     func()
	modeErr        error
}

func (c *recoveryTestCodex) LoadedThreadIDs(context.Context) ([]string, error) { return nil, nil }
func (c *recoveryTestCodex) HasActiveGoal(context.Context, string) (bool, error) {
	return c.goal, nil
}
func (c *recoveryTestCodex) ListQueue(context.Context, string) ([]codex.QueueEntry, error) {
	return c.queue, nil
}
func (c *recoveryTestCodex) Queue(_ context.Context, thread, text, id string) (codex.QueueEntry, error) {
	c.calls = append(c.calls, "queue/add:"+thread)
	for _, entry := range c.queue {
		if entry.ClientUserMessageID == id {
			return entry, nil
		}
	}
	entry := codex.QueueEntry{ID: "entry-" + id, Text: text, ClientUserMessageID: id}
	c.queue = append(c.queue, entry)
	return entry, nil
}
func (c *recoveryTestCodex) ActivateThread(_ context.Context, thread string, _ codex.ThreadPolicy) error {
	c.calls = append(c.calls, "resume:"+thread)
	if c.failActivation {
		return errors.New("lost activation response")
	}
	return nil
}

func (c *recoveryTestCodex) ReadThreadMetadata(_ context.Context, thread string, _ bool) (codex.ThreadMetadata, error) {
	status := c.status
	if status == nil {
		status = map[string]any{"type": "idle"}
	}
	return codex.ThreadMetadata{ID: thread, Cwd: filepath.Join(c.workspace, "work", "example"), Status: status}, nil
}
func (c *recoveryTestCodex) RequireThreadTurnsIdle(context.Context, string) error { return c.turnsErr }
func (c *recoveryTestCodex) PrepareSendWithOptions(thread, text, id, action string, steered bool, _ codex.TurnOptions) error {
	c.preparations++
	return c.PrepareSend(thread, text, id, action, steered)
}
func (c *recoveryTestCodex) IsPreparedSendWithOptions(_, _, _, _ string, _ codex.TurnOptions) (bool, error) {
	return !c.notPrepared, nil
}
func (c *recoveryTestCodex) ReconcileSendWithOptions(ctx context.Context, thread, text, id, action string, _ codex.TurnOptions) (codex.SendReceipt, bool, error) {
	return c.ReconcileSend(ctx, thread, text, id, action)
}
func (c *recoveryTestCodex) SendRecoveredWithOptions(ctx context.Context, thread, text, id, action string, _ codex.TurnOptions) (codex.SendReceipt, error) {
	c.calls = append(c.calls, "send:"+thread)
	if c.beforeSend != nil {
		c.beforeSend()
	}
	return c.Send(ctx, thread, text, id, action)
}
func (c *recoveryTestCodex) UpdateThreadSettings(ctx context.Context, thread string, update codex.ThreadSettingsUpdate) (codex.ThreadSettings, error) {
	c.calls = append(c.calls, "mode:"+thread)
	if c.beforeMode != nil {
		c.beforeMode()
	}
	if c.modeErr != nil {
		return codex.ThreadSettings{}, c.modeErr
	}
	return c.browserContractCodex.UpdateThreadSettings(ctx, thread, update)
}

func recoveryTestServer(t *testing.T) (*Server, *recoveryTestCodex) {
	t.Helper()
	s := newTestServer(t)
	s.config.RecoverSessions = true
	socketDir, err := os.MkdirTemp("", "recovery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	s.config.CodexSocket = filepath.Join(socketDir, "codex.sock")
	listener, err := net.Listen("unix", s.config.CodexSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	dir := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, dir, "active")
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n  socket_path: " + s.config.CodexSocket + "\n  client_version: 0.152.1\ncreation:\n  state: ready\n  initial_goal_sent: true\n"
	if err := os.WriteFile(filepath.Join(dir, "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	c := &recoveryTestCodex{browserContractCodex: &browserContractCodex{emptyPrompts: true}, workspace: s.config.Workspace}
	s.config.Codex = c
	writeWebRuntimeAuthority(t, s, "example")
	if err := s.recoveryStore().Set(context.Background(), "example", "thread-1", s.config.CodexSocket, true, false); err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestRecoveryColdSendRefusesOlderWorkBeforeLoading(t *testing.T) {
	for _, work := range []string{"queue", "goal", "active", "unknown", "turn", "pending"} {
		t.Run(work, func(t *testing.T) {
			s, c := recoveryTestServer(t)
			switch work {
			case "queue":
				c.queue = []codex.QueueEntry{{ID: "old", Text: "older request", ClientUserMessageID: "old-id"}}
			case "goal":
				c.goal = true
			case "active":
				c.status = map[string]any{"type": "active"}
			case "unknown":
				c.status = map[string]any{"type": "systemError"}
			case "pending":
				c.emptyPrompts = false
			case "turn":
				c.turnsErr = errors.New("latest turn is in progress")
			}
			client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
			_, err := client.Send(context.Background(), "thread-1", "new request", "new-id", "")
			var unsent *codex.MessageNotSentError
			if !errors.As(err, &unsent) || unsent.Retryable || len(c.calls) != 0 || c.sendCount != 0 || !s.recoveryHeld("example", "thread-1") {
				t.Fatal(err, c.calls, c.sendCount)
			}
			if work == "queue" && (len(c.queue) != 1 || c.queue[0].ID != "old") {
				t.Fatal(c.queue)
			}
		})
	}
}

func TestRecoveryColdRetryPreservesHotSendOutcome(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "unknown"}[unknown], func(t *testing.T) {
			s, c := recoveryTestServer(t)
			ctx := context.Background()
			if unknown {
				c.sendErr = &codex.UnknownSendOutcomeError{Err: errors.New("lost native response")}
			}
			// The first browser lost its reply after the hot send. The retained
			// receipt survives while native restart puts the root on hold.
			original, originalErr := c.browserContractCodex.Send(ctx, "thread-1", "original request", "same-id", "")
			client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
			receipt, err := client.Send(ctx, "thread-1", "original request", "same-id", "")
			if unknown {
				var outcome *codex.UnknownSendOutcomeError
				if !errors.As(err, &outcome) || originalErr == nil {
					t.Fatal("unknown attempt was resubmitted", receipt, err)
				}
			} else if err != nil || receipt != original {
				t.Fatal("accepted receipt changed", receipt, original, err)
			}
			if c.sendCount != 1 || len(c.calls) != 0 || len(c.queue) != 0 || !s.recoveryHeld("example", "thread-1") {
				t.Fatal("retry submitted or activated held work", c.calls, c.queue, c.sendCount)
			}
		})
	}
}

func TestRecoveryContinueRetriesOneDecisionAcrossBrowsers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queue  bool
		goal   bool
		prompt bool
	}{
		{"empty", false, false, true}, {"queued", true, false, false}, {"goal", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := recoveryTestServer(t)
			ctx := context.Background()
			c.goal = tc.goal
			if tc.queue {
				c.queue = []codex.QueueEntry{{ID: "older", Text: "saved request"}}
			}
			id, err := s.acceptContinuation(ctx, "example", "thread-1", "request-one")
			if err != nil {
				t.Fatal(err)
			}
			second, err := s.acceptContinuation(ctx, "example", "thread-1", "request-two")
			if err != nil || second != id {
				t.Fatal("retry replaced continuation", second, err)
			}
			c.failActivation = true
			if err := s.continueRecovery(ctx, "example", "thread-1", id); err == nil {
				t.Fatal("missing simulated failure")
			}
			c.failActivation = false
			if err := s.continueRecovery(ctx, "example", "thread-1", second); err != nil {
				t.Fatal(err)
			}
			if err := s.continueRecovery(ctx, "example", "thread-1", second); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.queue || tc.prompt {
				want = 1
			}
			if len(c.queue) != want {
				t.Fatal("duplicated continuation", c.queue)
			}
			if tc.prompt && c.queue[0].ClientUserMessageID != id {
				t.Fatal("prompt lost stable receipt")
			}
			if s.recoveryHeld("example", "thread-1") {
				t.Fatal("explicit continuation remained held")
			}
		})
	}
}

func TestRecoveryRefusesActivationBeforeTouchingNativeThread(t *testing.T) {
	s, c := recoveryTestServer(t)
	if err := s.recoveryStore().Retire(context.Background(), "example", "thread-1", s.config.CodexSocket, true); err != nil {
		t.Fatal(err)
	}
	if err := s.activateRecovery(context.Background(), "example", "thread-1", "thread-1", ""); err == nil {
		t.Fatal("missing record activated thread")
	}
	if len(c.calls) != 0 {
		t.Fatal("native thread touched before identity checks", c.calls)
	}
}

func TestRecoveryAdmissionCoalescesRetriesAndRejectsShutdown(t *testing.T) {
	s, _ := recoveryTestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	if !s.launchRecovery("example", func() { close(entered); <-release }) {
		t.Fatal("first recovery refused")
	}
	<-entered
	if s.launchRecovery("example", func() { t.Error("duplicate recovery ran") }) {
		t.Fatal("duplicate admitted")
	}
	close(release)
	s.Close()
	if s.launchRecovery("example", func() { t.Error("recovery ran after shutdown") }) {
		t.Fatal("shutdown admitted work")
	}
}
