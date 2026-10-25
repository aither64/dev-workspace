package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type delayedArchiveSendLock struct {
	underlying conversation.MutationLocker
	first      atomic.Bool
	entered    chan struct{}
	release    chan struct{}
}

func (lock *delayedArchiveSendLock) Lock(ctx context.Context) error {
	if !lock.first.Swap(true) {
		close(lock.entered)
		select {
		case <-lock.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lock.underlying.Lock(ctx)
}
func (lock *delayedArchiveSendLock) Unlock() { lock.underlying.Unlock() }

func TestArchivePreparationCancellationWhileWaitingForSendLock(t *testing.T) {
	s, op, c, _ := archivePreparationFixture(t)
	lock := &delayedArchiveSendLock{underlying: s.messageLock(op.Slug), entered: make(chan struct{}), release: make(chan struct{})}
	s.messageMu.Lock()
	s.messageLocks[op.Slug] = lock
	s.messageMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- s.runArchivePreparation(ctx, op) }()
	select {
	case <-lock.entered:
	case <-ctx.Done():
		t.Fatal("worker never reached send boundary")
	}
	response := httptest.NewRecorder()
	s.cancelArchivePreparation(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"receiptId":%q}`, op.ReceiptID))), op.Slug)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	close(lock.release)
	if err := <-ended; err == nil {
		t.Fatal("canceled worker continued")
	}
	if c.sends != 0 {
		t.Fatalf("sent %d repair messages after cancellation succeeded", c.sends)
	}
}

type archiveRepairFixture struct {
	*browserContractCodex
	workspace  string
	sends      int
	accepted   map[string]codex.SendReceipt
	options    codex.TurnOptions
	onSend     func(int)
	unknown    bool
	transient  bool
	needsInput bool
}

func (c *archiveRepairFixture) ReadThreadSettings(context.Context, string) (codex.ThreadSettings, error) {
	return codex.ThreadSettings{Model: "retained-model", ReasoningEffort: "xhigh"}, nil
}
func (c *archiveRepairFixture) ReadThreadMetadata(_ context.Context, id string, _ bool) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{ID: id, Cwd: filepath.Join(c.workspace, "work", "example")}, nil
}
func (c *archiveRepairFixture) ReconcileSendWithOptions(_ context.Context, _ string, _ string, id string, _ string, _ codex.TurnOptions) (codex.SendReceipt, bool, error) {
	r, ok := c.accepted[id]
	return r, ok, nil
}
func (c *archiveRepairFixture) SendWithOptions(_ context.Context, _ string, _ string, id string, _ string, options codex.TurnOptions) (codex.SendReceipt, error) {
	if _, ok := c.accepted[id]; ok {
		return codex.SendReceipt{}, errors.New("duplicate repair delivery")
	}
	c.sends++
	c.options = options
	r := codex.SendReceipt{TurnID: fmt.Sprintf("repair-%d", c.sends), ClientUserMessageID: id}
	c.accepted[id] = r
	if c.onSend != nil {
		c.onSend(c.sends)
	}
	if c.unknown {
		c.unknown = false
		if c.transient {
			return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: errors.New("lost native response")}
		}
		return codex.SendReceipt{}, errors.New("lost native response")
	}
	return r, nil
}
func (c *archiveRepairFixture) Request(_ context.Context, method string, _ any, result any) error {
	if method != "thread/turns/list" {
		return fmt.Errorf("unexpected request %s", method)
	}
	data := fmt.Sprintf(`{"data":[{"id":"repair-%d","status":"completed"}]}`, c.sends)
	return json.Unmarshal([]byte(data), result)
}
func (c *archiveRepairFixture) PromptsWithItems(context.Context, string) ([]codex.Prompt, error) {
	if c.needsInput {
		return []codex.Prompt{{IsBlocking: true}}, nil
	}
	return nil, nil
}

func archivePreparationFixture(t *testing.T) (*Server, lifecycleOperation, *archiveRepairFixture, string) {
	t.Helper()
	s := newTestServer(t)
	summary := browserTargetFixture(t, s, "work", "active", "root-a")
	manifest := fmt.Sprintf("schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root-a\n  socket_path: %s\n  client_version: %s\n", s.config.CodexSocket, s.config.CodexVersion)
	if err := os.WriteFile(filepath.Join(s.config.Workspace, "work", "example", "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}

	op := failedBrowserOperation(t, summary, "archive")
	op.Options.Mode = "archived"
	op.State = "running"
	saveBrowserOperation(t, s, op)
	if err := s.beginArchivePreparation(op); err != nil {
		t.Fatal(err)
	}
	c := &archiveRepairFixture{browserContractCodex: &browserContractCodex{}, workspace: s.config.Workspace, accepted: map[string]codex.SendReceipt{}}
	s.config.Codex = c
	marker := filepath.Join(t.TempDir(), "fixed")
	blocked, _ := json.Marshal(archiveCheckResult{Schema: 1, Workspace: s.config.Workspace, Slug: "example", Total: 6, Passed: 5, Stage: 1, Count: 1, Blockers: []archiveBlocker{{Code: "unmanaged_entry", Message: "Relocate the hook cache."}}})
	ready, _ := json.Marshal(archiveCheckResult{Schema: 1, Workspace: s.config.Workspace, Slug: "example", Ready: true, Total: 6, Passed: 6, Stage: 6, Blockers: []archiveBlocker{}})
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = archive-check ] || exit 23\nif [ -f '%s' ]; then\ncat <<'READY'\n%s\nREADY\nelse\ncat <<'BLOCKED'\n%s\nBLOCKED\nfi\n", marker, ready, blocked)
	helper := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.config.DevSession = helper
	return s, op, c, marker
}

func TestArchivePreparationRepairsBeforeHandoffWithoutLifecycleJournal(t *testing.T) {
	s, op, c, marker := archivePreparationFixture(t)
	c.onSend = func(_ int) {
		if err := os.WriteFile(marker, nil, 0600); err != nil {
			t.Error(err)
		}
	}
	if err := s.runArchivePreparation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	p, _ := s.archivePreparationFor("example")
	if p.State != "archiving" || c.sends != 1 || c.options.Model != "retained-model" || c.options.ReasoningEffort != "xhigh" {
		t.Fatalf("preparation %#v, sends %d, options %#v", p, c.sends, c.options)
	}
	if _, err := os.Stat(filepath.Join(s.config.Workspace, "worktrees", ".locks", "example.archive.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation published a lifecycle journal: %v", err)
	}
}

func TestArchivePreparationStopsAfterThreeUnchangedRepairRounds(t *testing.T) {
	s, op, c, _ := archivePreparationFixture(t)
	err := s.runArchivePreparation(context.Background(), op)
	if err == nil || !strings.Contains(err.Error(), "three repair cycles") || c.sends != 3 {
		t.Fatalf("error %v, sends %d", err, c.sends)
	}
	p, _ := s.archivePreparationFor("example")
	if p.active() {
		t.Fatal("failed preparation remains active")
	}
}

func TestArchivePreparationReconcilesLostResponseOnRetry(t *testing.T) {
	s, op, c, marker := archivePreparationFixture(t)
	c.unknown = true
	c.onSend = func(_ int) { _ = os.WriteFile(marker, nil, 0600) }
	if err := s.runArchivePreparation(context.Background(), op); err == nil {
		t.Fatal("expected lost response")
	}
	op.Attempt++
	saveBrowserOperation(t, s, op)
	if err := s.beginArchivePreparation(op); err != nil {
		t.Fatal(err)
	}
	if err := s.runArchivePreparation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if c.sends != 1 {
		t.Fatalf("repair was sent %d times", c.sends)
	}
}

func TestArchivePreparationCancellationRevokesContinuation(t *testing.T) {
	s, op, c, _ := archivePreparationFixture(t)
	response := httptest.NewRecorder()
	s.cancelArchivePreparation(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"receiptId":%q}`, op.ReceiptID))), op.Slug)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	if err := s.runArchivePreparation(context.Background(), op); err == nil {
		t.Fatal("canceled preparation continued")
	}
	if c.sends != 0 {
		t.Fatal("canceled preparation delivered a repair")
	}
}

func TestArchivePreparationRejectsReplacementBeforeLeadDelivery(t *testing.T) {
	s, op, c, _ := archivePreparationFixture(t)
	path := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.Rename(path, path+"-retained"); err != nil {
		t.Fatal(err)
	}
	_ = browserTargetFixture(t, s, "work", "active", "root-b")
	if err := s.runArchivePreparation(context.Background(), op); err == nil {
		t.Fatal("replaced target accepted")
	}
	if c.sends != 0 {
		t.Fatal("sent to replaced target")
	}
}

func TestArchivePreparationStopsForBlockingLeadInput(t *testing.T) {
	s, op, c, _ := archivePreparationFixture(t)
	c.needsInput = true
	if err := s.runArchivePreparation(context.Background(), op); err == nil || !strings.Contains(err.Error(), "needs your input") {
		t.Fatalf("error %v", err)
	}
	if c.sends != 1 {
		t.Fatal(c.sends)
	}
}

func TestArchivePreparationFailedChecksLeaveUnrelatedSessionAvailable(t *testing.T) {
	s, op, _, _ := archivePreparationFixture(t)
	_ = s.runArchivePreparation(context.Background(), op)
	other := filepath.Join(s.config.Workspace, "work", "other")
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}
	if owner, err := session.PendingLifecycle(s.config.Workspace, "other"); err != nil || owner != "" {
		t.Fatalf("other session blocked: %s %v", owner, err)
	}
}

func TestArchivePreparationAutomaticallyReconcilesUncertainDelivery(t *testing.T) {
	s, op, c, marker := archivePreparationFixture(t)
	c.unknown = true
	c.transient = true
	c.onSend = func(_ int) { _ = os.WriteFile(marker, nil, 0600) }
	if err := s.runArchivePreparation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if c.sends != 1 {
		t.Fatalf("duplicate native repair: %d", c.sends)
	}
}

func TestArchivePreparationContinuesBeyondThreeTurnsWhenProgressing(t *testing.T) {
	s, op, c, _ := archivePreparationFixture(t)
	writeResult := func(count int) {
		result := archiveCheckResult{Schema: 1, Workspace: s.config.Workspace, Slug: "example", Ready: count == 0, Total: 6, Passed: 5, Stage: 1, Count: count, Blockers: []archiveBlocker{{Code: "unmanaged_entry", Message: "Relocate remaining hook caches."}}}
		if count == 0 {
			result.Passed = 6
			result.Stage = 6
			result.Blockers = []archiveBlocker{}
		}
		payload, _ := json.Marshal(result)
		script := "#!/bin/sh\ncat <<'RESULT'\n" + string(payload) + "\nRESULT\n"
		if err := os.WriteFile(s.config.DevSession, []byte(script), 0700); err != nil {
			t.Error(err)
		}
	}
	writeResult(5)
	c.onSend = func(round int) { writeResult(5 - round) }
	if err := s.runArchivePreparation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if c.sends != 5 {
		t.Fatalf("progress stopped after %d turns", c.sends)
	}
}

func TestArchivePreparationRestartReconcilesAcceptedRepairAndParksForOldPackages(t *testing.T) {
	s, op, c, marker := archivePreparationFixture(t)
	c.unknown = true
	c.onSend = func(_ int) { _ = os.WriteFile(marker, nil, 0600) }
	if err := s.runArchivePreparation(context.Background(), op); err == nil {
		t.Fatal("expected uncertain reply")
	}
	s.archivePrepMu.Lock()
	p := s.archivePreparations[op.Slug]
	p.State = "paused"
	err := s.saveArchivePreparationLocked(p)
	s.archivePrepMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	stored, err := s.operationStore.loadReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	if stored[op.Slug].State != "failed" {
		t.Fatal("older package would see a running preparation")
	}
	// The same checker now succeeds; the ordinary executor is a disposable
	// stand-in. Native submission identity must survive the portal restart.
	ready, _ := json.Marshal(archiveCheckResult{Schema: 1, Workspace: s.config.Workspace, Slug: op.Slug, Ready: true, Total: 6, Passed: 6, Stage: 6, Blockers: []archiveBlocker{}})
	script := "#!/bin/sh\nif [ \"$1\" = archive-check ]; then\ncat <<'READY'\n" + string(ready) + "\nREADY\nfi\nexit 0\n"
	if err := os.WriteFile(s.config.DevSession, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		reopened.operationMu.Lock()
		current := reopened.operations[op.Slug]
		reopened.operationMu.Unlock()
		if current.State == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart did not complete: %#v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.sends != 1 {
		t.Fatalf("restart duplicated accepted repair: %d", c.sends)
	}
}

func TestArchivePreparationCorruptRecordDoesNotPreventPortalStartup(t *testing.T) {
	s, op, _, _ := archivePreparationFixture(t)
	s.Close()
	if err := os.WriteFile(s.archivePreparationPath(op.Slug), []byte("broken preparation"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.config)
	if err != nil {
		t.Fatalf("one bad preparation blocked the portal: %v", err)
	}
	defer reopened.Close()
	response := httptest.NewRecorder()
	reopened.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
}
