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
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

func awaitArchivePreparation(t *testing.T, s *Server, predicate func(archivePreparation) bool) archivePreparation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, _ := s.archivePreparationFor("example")
		if predicate(p) {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("preparation did not reach expected state: %#v", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestArchivePreparationWaitsForRuntimeWithoutReservingWorkspace(t *testing.T) {
	s, op, c, marker := archivePreparationFixture(t)
	s.config.TransitionLock = filepath.Join(t.TempDir(), "transition.lock")
	if err := os.WriteFile(s.config.TransitionLock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	owner, err := session.LockRuntimeExclusive(s.config.AuthorityDir, op.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- s.runArchivePreparation(ctx, op) }()
	p := awaitArchivePreparation(t, s, func(p archivePreparation) bool { return p.Error == archiveRuntimeWaitMessage })
	if !p.active() || p.Round != 0 || p.NoProgress != 0 || p.MessageID != "" || p.ReceiptID != op.ReceiptID || p.Attempt != op.Attempt {
		t.Fatalf("contention changed request/progress: %#v", p)
	}
	// The durable wait must not pin the package generation while another
	// session or host operation needs the transition lock.
	transition, err := os.OpenFile(s.config.TransitionLock, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Close()
	if err := unix.Flock(int(transition.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("backoff retained transition lock: %v", err)
	}
	_ = unix.Flock(int(transition.Fd()), unix.LOCK_UN)
	data, err := os.ReadFile(s.archivePreparationPath(op.Slug))
	if err != nil {
		t.Fatal(err)
	}
	var saved archivePreparation
	if err := json.Unmarshal(data, &saved); err != nil || saved.Error != archiveRuntimeWaitMessage || !saved.matches(op) {
		t.Fatalf("wait was not durable: %#v %v", saved, err)
	}
	s.publishLifecycleSnapshot(op.Slug, op, time.Now())
	response := httptest.NewRecorder()
	s.lifecycleStatus(response, op.Slug)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), archiveRuntimeWaitMessage) || !strings.Contains(response.Body.String(), `"state":"running"`) {
		t.Fatalf("wait not visible: %d %s", response.Code, response.Body.String())
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	p, _ = s.archivePreparationFor(op.Slug)
	if p.State != "archiving" || p.Error != "" || p.Round != 0 || c.sends != 0 {
		t.Fatalf("released request did not continue cleanly: %#v sends=%d", p, c.sends)
	}
}

func TestArchivePreparationRuntimeWaitPreservesAcceptedRepairAcrossRestart(t *testing.T) {
	s, op, c, marker := archivePreparationFixture(t)
	observed := make(chan struct{})
	var reads atomic.Int32
	c.onSend = func(int) { _ = os.WriteFile(marker, nil, 0600) }
	c.turnState = func(_ context.Context, id string) (codex.TurnState, error) {
		state := codex.TurnState{ThreadID: id, LatestTurnID: "repair-1", LatestTurnStatus: "completed", Status: "idle"}
		if reads.Add(1) == 1 {
			state.LatestTurnStatus = "inProgress"
			close(observed)
		}
		return state, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ended := make(chan error, 1)
	go func() { ended <- s.runArchivePreparation(ctx, op) }()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("repair was not accepted")
	}
	// Wait for the admitted observation to release its shared runtime lock.
	var owner *session.RuntimeLock
	for owner == nil {
		var err error
		owner, err = session.LockRuntimeExclusive(s.config.AuthorityDir, op.Slug)
		if err != nil && !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		if owner == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	defer owner.Close()
	p := awaitArchivePreparation(t, s, func(p archivePreparation) bool { return p.Error == archiveRuntimeWaitMessage })
	if p.Round != 1 || p.NoProgress != 0 || p.Delivery == nil || p.Delivery.TurnID != "repair-1" {
		t.Fatalf("wait lost accepted delivery: %#v", p)
	}
	cancel()
	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait ignored interruption: %v", err)
	}
	s.Close()
	// Both the paused record and its original repair identity must be readable
	// by the ordinary restart path; no new receipt or repair turn is needed.
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
	resumed := awaitArchivePreparation(t, reopened, func(p archivePreparation) bool {
		return p.Error == archiveRuntimeWaitMessage && p.State == "lead_fixing_blockers"
	})
	if !resumed.active() || !resumed.matches(op) || resumed.MessageID != p.MessageID || resumed.Delivery == nil || *resumed.Delivery != *p.Delivery || resumed.Round != p.Round || resumed.NoProgress != p.NoProgress {
		t.Fatalf("restart changed accepted request: before=%#v after=%#v", p, resumed)
	}
	reopened.publishLifecycleSnapshot(op.Slug, op, time.Now())
	response := httptest.NewRecorder()
	reopened.lifecycleStatus(response, op.Slug)
	if !strings.Contains(response.Body.String(), `"phase":"lead_fixing_blockers"`) || !strings.Contains(response.Body.String(), archiveRuntimeWaitMessage) {
		t.Fatalf("restart did not restore cancelable wait presentation: %s", response.Body.String())
	}
	_ = owner.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		reopened.operationMu.Lock()
		current := reopened.operations[op.Slug]
		reopened.operationMu.Unlock()
		if current.State == "complete" {
			if current.ReceiptID != op.ReceiptID || current.Attempt != op.Attempt {
				t.Fatalf("restart replaced receipt: %#v", current)
			}
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

func TestArchivePreparationRuntimeWaitRechecksCancellationAndIdentity(t *testing.T) {
	for _, scenario := range []string{"cancel", "generation", "target"} {
		t.Run(scenario, func(t *testing.T) {
			s, op, c, _ := archivePreparationFixture(t)
			owner, err := session.LockRuntimeExclusive(s.config.AuthorityDir, op.Slug)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ended := make(chan error, 1)
			go func() { ended <- s.runArchivePreparation(ctx, op) }()
			awaitArchivePreparation(t, s, func(p archivePreparation) bool { return p.Error == archiveRuntimeWaitMessage })
			switch scenario {
			case "cancel":
				response := httptest.NewRecorder()
				s.cancelArchivePreparation(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"receiptId":%q}`, op.ReceiptID))), op.Slug)
				if response.Code != http.StatusOK {
					t.Fatal(response.Code, response.Body.String())
				}
			case "generation":
				if err := os.Remove(s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
			case "target":
				path := filepath.Join(s.config.Workspace, "work", op.Slug)
				if err := os.Rename(path, path+"-retained"); err != nil {
					t.Fatal(err)
				}
				_ = browserTargetFixture(t, s, "work", "active", "root-a")
				_ = owner.Close()
			}
			if err := <-ended; err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("changed %s did not stop admission: %v", scenario, err)
			}
			p, _ := s.archivePreparationFor(op.Slug)
			if p.active() || c.sends != 0 || p.Round != 0 || p.NoProgress != 0 {
				t.Fatalf("changed %s continued or consumed repair rounds: %#v sends=%d", scenario, p, c.sends)
			}
		})
	}
}

func TestArchivePreparationDoesNotRetryActionContentionOrPersistenceFailure(t *testing.T) {
	t.Run("action errno", func(t *testing.T) {
		s, op, _, _ := archivePreparationFixture(t)
		p, _ := s.archivePreparationFor(op.Slug)
		calls := 0
		err := s.withArchivePreparationAdmission(context.Background(), op, p, func(context.Context) error { calls++; return unix.EAGAIN })
		if !errors.Is(err, unix.EAGAIN) || calls != 1 {
			t.Fatalf("retried an action error: calls=%d err=%v", calls, err)
		}
	})
	t.Run("wait cannot persist", func(t *testing.T) {
		s, op, c, _ := archivePreparationFixture(t)
		owner, err := session.LockRuntimeExclusive(s.config.AuthorityDir, op.Slug)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		path := s.archivePreparationPath(op.Slug)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.runArchivePreparation(ctx, op); err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost durable wait was silently retried: %v", err)
		}
		if c.sends != 0 {
			t.Fatal("sent repair despite persistence failure")
		}
	})
}
