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
	turnState  func(context.Context, string) (codex.TurnState, error)
	metadata   func(context.Context, string, bool) (codex.ThreadMetadata, error)
}

func (c *archiveRepairFixture) ReadThreadSettings(context.Context, string) (codex.ThreadSettings, error) {
	return codex.ThreadSettings{Model: "retained-model", ReasoningEffort: "xhigh"}, nil
}
func (c *archiveRepairFixture) ReadThreadMetadata(ctx context.Context, id string, exclude bool) (codex.ThreadMetadata, error) {
	if c.metadata != nil {
		return c.metadata(ctx, id, exclude)
	}
	return codex.ThreadMetadata{ID: id, Cwd: filepath.Join(c.workspace, "work", "example"), Status: map[string]any{"type": "idle"}}, nil
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
func (c *archiveRepairFixture) ReadTurnState(ctx context.Context, id string) (codex.TurnState, error) {
	if c.turnState != nil {
		return c.turnState(ctx, id)
	}
	return codex.TurnState{ThreadID: id, LatestTurnID: fmt.Sprintf("repair-%d", c.sends), LatestTurnStatus: "completed", Status: "idle"}, nil
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
	for _, activity := range []string{"idle", "active"} {
		t.Run(activity, func(t *testing.T) {
			s, op, c, _ := archivePreparationFixture(t)
			c.needsInput = true
			c.turnState = func(_ context.Context, id string) (codex.TurnState, error) {
				return codex.TurnState{ThreadID: id, LatestTurnID: "repair-1", LatestTurnStatus: "completed", Status: activity}, nil
			}
			if err := s.runArchivePreparation(context.Background(), op); err == nil || !strings.Contains(err.Error(), "needs your input") {
				t.Fatalf("error %v", err)
			}
			if c.sends != 1 {
				t.Fatal(c.sends)
			}
		})
	}
}

func TestArchivePreparationCancellationWhileActivityRemainsActive(t *testing.T) {
	s, op, c, _ := archivePreparationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.turnState = func(_ context.Context, id string) (codex.TurnState, error) {
		cancel()
		return codex.TurnState{ThreadID: id, LatestTurnID: "repair-1", LatestTurnStatus: "interrupted", Status: "active"}, nil
	}
	if err := s.runArchivePreparation(ctx, op); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	p, _ := s.archivePreparationFor(op.Slug)
	if c.sends != 1 || p.State == "failed" || p.State == "archiving" {
		t.Fatalf("cancellation changed result/delivery: %d %#v", c.sends, p)
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

// These scripted tuples exercise the passive client contract. They do not
// reproduce the native scheduler or establish the original CI raw status.
func TestArchivePreparationRepairStatusMatrix(t *testing.T) {
	for _, raw := range []string{"inProgress", "completed", "failed", "interrupted"} {
		for _, before := range []string{"idle", "active", "systemError"} {
			for _, after := range []string{"idle", "active", "systemError"} {
				t.Run(raw+"/"+before+"/"+after, func(t *testing.T) {
					s, _, c, _ := archivePreparationFixture(t)
					c.turnState = func(context.Context, string) (codex.TurnState, error) {
						return codex.TurnState{ThreadID: "root-a", LatestTurnID: "accepted", LatestTurnStatus: raw, Status: before}, nil
					}
					reads := 0
					c.metadata = func(_ context.Context, id string, exclude bool) (codex.ThreadMetadata, error) {
						reads++
						if !exclude {
							t.Error("observation must be passive")
						}
						return codex.ThreadMetadata{ID: id, Cwd: filepath.Join(c.workspace, "work", "example"), Status: map[string]any{"type": after}}, nil
					}
					complete, err := s.archiveRepairComplete(context.Background(), c, "root-a", "accepted", filepath.Join(c.workspace, "work", "example"))
					pending := raw == "inProgress" || before == "active" || after == "active"
					wantComplete := !pending && raw == "completed" && before == "idle" && after == "idle"
					wantError := !pending && !wantComplete
					if complete != wantComplete || (err != nil) != wantError {
						t.Fatalf("complete=%v err=%v", complete, err)
					}
					wantReads := 1
					if raw == "inProgress" {
						wantReads = 0
					}
					if reads != wantReads {
						t.Fatalf("fresh metadata reads=%d, want%d", reads, wantReads)
					}
				})
			}
		}
	}
}

func TestArchivePreparationRepairObservationRefusals(t *testing.T) {
	for _, scenario := range []string{"empty-accepted", "wrong-thread", "empty-latest", "newer-turn", "cold", "unknown-activity", "empty-activity", "unknown-turn", "fresh-wrong-thread", "fresh-wrong-cwd", "fresh-cold", "fresh-unknown", "fresh-empty", "read-error", "fresh-read-error", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, c, _ := archivePreparationFixture(t)
			state := codex.TurnState{ThreadID: "root-a", LatestTurnID: "accepted", LatestTurnStatus: "interrupted", Status: "active"}
			switch scenario {
			case "wrong-thread":
				state.ThreadID = "foreign"
			case "empty-latest":
				state.LatestTurnID = ""
			case "newer-turn":
				state.LatestTurnID = "newer"
			case "cold":
				state.Status = "notLoaded"
				state.LatestTurnStatus = "inProgress"
			case "unknown-activity":
				state.Status = "unknown"
			case "empty-activity":
				state.Status = ""
			case "unknown-turn":
				state.LatestTurnStatus = "unknown"
			}
			sentinel := errors.New("passive observation failed")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "canceled" {
				cancel()
			}
			c.turnState = func(ctx context.Context, _ string) (codex.TurnState, error) {
				if ctx.Err() != nil {
					return codex.TurnState{}, ctx.Err()
				}
				if scenario == "read-error" {
					return codex.TurnState{}, sentinel
				}
				return state, nil
			}
			c.metadata = func(_ context.Context, id string, exclude bool) (codex.ThreadMetadata, error) {
				if !exclude {
					t.Error("observation must exclude turns")
				}
				if scenario == "fresh-read-error" {
					return codex.ThreadMetadata{}, sentinel
				}
				fresh := codex.ThreadMetadata{ID: id, Cwd: filepath.Join(c.workspace, "work", "example"), Status: map[string]any{"type": "idle"}}
				switch scenario {
				case "fresh-wrong-thread":
					fresh.ID = "foreign"
				case "fresh-wrong-cwd":
					fresh.Cwd = filepath.Join(c.workspace, "work", "foreign")
				case "fresh-cold":
					fresh.Status["type"] = "notLoaded"
				case "fresh-unknown":
					fresh.Status["type"] = "unknown"
				case "fresh-empty":
					fresh.Status = nil
				}
				return fresh, nil
			}
			accepted := "accepted"
			if scenario == "empty-accepted" {
				accepted = ""
			}
			complete, err := s.archiveRepairComplete(ctx, c, "root-a", accepted, filepath.Join(c.workspace, "work", "example"))
			if err == nil || complete {
				t.Fatalf("unverified observation accepted: %v %v", complete, err)
			}
			if strings.Contains(scenario, "read-error") && !errors.Is(err, sentinel) {
				t.Fatalf("read error lost: %v", err)
			}
			if scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestArchivePreparationActiveTerminalRemainsPending(t *testing.T) {
	for _, first := range []string{"interrupted-after-active", "completed-before-active", "failed-before-active", "interrupted-before-active"} {
		t.Run(first, func(t *testing.T) {
			s, op, c, marker := archivePreparationFixture(t)
			reads, fresh := 0, false
			c.turnState = func(_ context.Context, id string) (codex.TurnState, error) {
				reads++
				p, _ := s.archivePreparationFor(op.Slug)
				if p.State == "failed" || p.State == "archiving" {
					t.Errorf("early handoff/failure: %s", p.State)
				}
				raw, before := "completed", "idle"
				if reads == 1 {
					raw = strings.Split(first, "-")[0]
					if first != "interrupted-after-active" {
						before = "active"
					}
				}
				if first == "interrupted-after-active" && reads == 2 {
					raw, before = "inProgress", "active"
				}
				if reads > 1 && first == "failed-before-active" {
					raw = "failed"
				}
				if reads > 1 && first == "interrupted-before-active" {
					raw = "interrupted"
				}
				if raw == "completed" && before == "idle" {
					if err := os.WriteFile(marker, nil, 0600); err != nil {
						t.Error(err)
					}
				}
				fresh = raw != "inProgress"
				return codex.TurnState{ThreadID: id, LatestTurnID: "repair-1", LatestTurnStatus: raw, Status: before}, nil
			}
			c.metadata = func(_ context.Context, id string, _ bool) (codex.ThreadMetadata, error) {
				activity := "idle"
				if fresh && reads == 1 && first == "interrupted-after-active" {
					activity = "active"
				}
				fresh = false
				return codex.ThreadMetadata{ID: id, Cwd: filepath.Join(c.workspace, "work", "example"), Status: map[string]any{"type": activity}}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			err := s.runArchivePreparation(ctx, op)
			wantError := first == "failed-before-active" || first == "interrupted-before-active"
			if (err != nil) != wantError {
				t.Fatalf("error=%v", err)
			}
			if wantError && !strings.Contains(err.Error(), "did not complete successfully") {
				t.Fatal(err)
			}
			wantReads := 2
			if first == "interrupted-after-active" {
				wantReads = 3
			}
			if reads != wantReads || c.sends != 1 {
				t.Fatalf("reads=%d sends=%d", reads, c.sends)
			}
			p, _ := s.archivePreparationFor(op.Slug)
			if !wantError && p.State != "archiving" {
				t.Fatalf("completion did not hand off: %#v", p)
			}
		})
	}
}
