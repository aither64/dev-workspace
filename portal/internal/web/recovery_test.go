package web

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"

	"testing"

	"github.com/aither64/codex-web/codex"
)

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
