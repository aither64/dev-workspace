package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestRecoveryUndismissedIdleRecordProjectsIdleWithoutPermission(t *testing.T) {
	s, c, summary := recoveryNoticeTestServer(t)
	c.status = map[string]any{"type": "notLoaded"}
	// This is the predecessor's unchanged held record, with no dismissal record.
	before, err := s.recoveryStore().Load("example")
	if err != nil {
		t.Fatal(err)
	}
	for _, terminal := range []string{"completed", "failed", "interrupted"} {
		c.transcript = codex.Transcript{ThreadID: "thread-1", Status: "idle", Entries: []codex.TranscriptEntry{{TurnID: "old-turn", TurnStatus: terminal}}}
		status := s.recoveryStatus(summary)
		if status.State != "idle" || !status.ExecutionHeld || status.NoticeDismissed || status.NoticeID != "" || status.InputBlockedReason != "" {
			t.Fatal(status)
		}
	}
	page := httptest.NewRecorder()
	s.Handler().ServeHTTP(page, httptest.NewRequest("GET", "/example/", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), `class="notice full recovery-notice" role="status" aria-live="polite" hidden`) {
		t.Fatal(page.Code, page.Body.String())
	}
	details := httptest.NewRecorder()
	s.Handler().ServeHTTP(details, httptest.NewRequest("GET", "/api/sessions/example/details", nil))
	if details.Code != 200 || !strings.Contains(details.Body.String(), `"state":"idle","executionHeld":true`) {
		t.Fatal(details.Code, details.Body.String())
	}
	after, err := s.recoveryStore().Load("example")
	if err != nil || !reflect.DeepEqual(before, after) || !s.recoveryHeld("example", "thread-1") || len(c.calls) != 0 {
		t.Fatal(before, after, c.calls, err)
	}
	if _, err := os.Stat(s.threadRecordPath("recovery-notice", "thread-1")); !os.IsNotExist(err) {
		t.Fatal("classification wrote notice preference", err)
	}
}

func TestRecoveryInputModeConfirmedBeforeExactSendAndPermission(t *testing.T) {
	s, c := recoveryTestServer(t)
	ctx := context.Background()
	mode := "plan"
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	if _, err := client.UpdateThreadSettings(ctx, "thread-1", codex.ThreadSettingsUpdate{CollaborationMode: &mode}); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 0 {
		t.Fatal("mode save loaded native", c.calls)
	}
	c.beforeMode = func() {
		if !s.recoveryHeld("example", "thread-1") {
			t.Error("permission published before mode confirmation")
		}
	}
	c.beforeSend = func() {
		if c.settings.CollaborationMode != mode || !s.recoveryHeld("example", "thread-1") {
			t.Error("send preceded mode confirmation or hold", c.settings)
		}
	}
	receipt, err := client.Send(ctx, "thread-1", "say hi", "original-id", "")
	if err != nil || receipt.ClientUserMessageID != "original-id" || c.messageID != "original-id" || c.message != "say hi" || c.sendCount != 1 {
		t.Fatal(receipt, err, c.messageID)
	}
	if !reflect.DeepEqual(c.calls, []string{"resume:thread-1", "mode:thread-1", "send:thread-1"}) {
		t.Fatal(c.calls)
	}
	if s.recoveryHeld("example", "thread-1") || !s.recoveryHeld("example", "member-thread") {
		t.Fatal("permission changed for wrong thread")
	}
	if saved, err := s.coldSettings("example", "thread-1", "thread-1"); err != nil || saved != nil {
		t.Fatal(saved, err)
	}
	// Lost browser response: a retry only returns the original receipt.
	if got, err := client.Send(ctx, "thread-1", "say hi", "original-id", ""); err != nil || got != receipt || c.sendCount != 1 {
		t.Fatal(got, err, c.sendCount)
	}
}

func TestRecoveryInputFailureKeepsBindingAndNeverReplaysUnknown(t *testing.T) {
	for _, failure := range []string{"mode", "unsent", "unknown"} {
		t.Run(failure, func(t *testing.T) {
			s, c := recoveryTestServer(t)
			client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
			ctx := context.Background()
			mode := "plan"
			if _, err := client.UpdateThreadSettings(ctx, "thread-1", codex.ThreadSettingsUpdate{CollaborationMode: &mode}); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "mode":
				c.modeErr = errors.New("mode confirmation lost")
			case "unsent":
				c.sendErr = &codex.MessageNotSentError{Err: errors.New("native refused"), Retryable: false}
			case "unknown":
				c.sendErr = &codex.UnknownSendOutcomeError{Err: errors.New("native response lost")}
			}
			if _, err := client.Send(ctx, "thread-1", "same text", "same-id", ""); err == nil {
				t.Fatal("missing failure")
			}
			saved, err := s.coldSettings("example", "thread-1", "thread-1")
			if err != nil || saved == nil || saved.RequestID != "same-id" || saved.Mode != mode || !s.recoveryHeld("example", "thread-1") {
				t.Fatal(saved, err)
			}
			otherMode := "default"
			if _, err := client.UpdateThreadSettings(ctx, "thread-1", codex.ThreadSettingsUpdate{CollaborationMode: &otherMode}); err == nil {
				t.Fatal("changed unresolved mode")
			}
			count := c.sendCount
			if failure == "unknown" {
				_, err := client.Send(ctx, "thread-1", "same text", "same-id", "")
				var unknown *codex.UnknownSendOutcomeError
				if !errors.As(err, &unknown) || c.sendCount != count {
					t.Fatal("unknown replayed", err, c.sendCount)
				}
			} else if failure == "mode" {
				if _, err := client.Send(ctx, "thread-1", "different text", "other-id", ""); err == nil || c.sendCount != count {
					t.Fatal("conflicting request dispatched", err)
				}
				if c.messageID != "same-id" {
					t.Fatal("conflicting input overwrote original preparation", c.messageID)
				}
				c.modeErr = nil
				if _, err := client.Send(ctx, "thread-1", "same text", "same-id", ""); err != nil || c.sendCount != 1 {
					t.Fatal(err, c.sendCount)
				}
			}
		})
	}
}

func TestRecoveryUnprovenPreparationCannotLoadOrSend(t *testing.T) {
	s, c := recoveryTestServer(t)
	c.notPrepared = true
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	_, err := client.Send(context.Background(), "thread-1", "original", "original-id", "")
	var unknown *codex.UnknownSendOutcomeError
	if !errors.As(err, &unknown) || len(c.calls) != 0 || !s.recoveryHeld("example", "thread-1") {
		t.Fatal(err, c.calls)
	}
}

func TestRecoveryBoundOriginalCannotRecreateMissingPreparation(t *testing.T) {
	s, c := recoveryTestServer(t)
	c.notPrepared = true
	if err := s.saveThreadRecord("next-settings", "thread-1", coldSettings{Slug: "example", Root: "thread-1", Thread: "thread-1", Mode: "plan", RequestID: "original-id", Epoch: "previous-epoch"}); err != nil {
		t.Fatal(err)
	}
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	_, err := client.Send(context.Background(), "thread-1", "original", "original-id", "")
	var unknown *codex.UnknownSendOutcomeError
	if !errors.As(err, &unknown) || c.preparations != 0 || len(c.calls) != 0 || !s.recoveryHeld("example", "thread-1") {
		t.Fatal("unproven original was prepared or activated", err, c.preparations, c.calls)
	}
	saved, err := s.coldSettings("example", "thread-1", "thread-1")
	if err != nil || saved == nil || saved.Epoch != "previous-epoch" {
		t.Fatal("unproven original rebound", saved, err)
	}
}

func TestRecoveryAcceptedOldEpochReceiptNeverGrantsNewEpoch(t *testing.T) {
	s, c := recoveryTestServer(t)
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	ctx := context.Background()
	epoch, _ := session.SocketIdentity(s.config.CodexSocket)
	if err := s.saveThreadRecord("next-settings", "thread-1", coldSettings{Slug: "example", Root: "thread-1", Thread: "thread-1", Mode: "plan", RequestID: "old-id", Epoch: epoch}); err != nil {
		t.Fatal(err)
	}
	// Native accepted, but the portal lost permission publication before restart.
	if _, err := c.Send(ctx, "thread-1", "old text", "old-id", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.config.CodexSocket); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", s.config.CodexSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := client.Send(ctx, "thread-1", "old text", "old-id", ""); err != nil || c.sendCount != 1 || !s.recoveryHeld("example", "thread-1") {
		t.Fatal(err, c.sendCount)
	}
}

func TestRecoveryColdRuntimeExcludesManagedWritersThroughReceipt(t *testing.T) {
	s, c, _ := recoveryNoticeTestServer(t)
	c.status = map[string]any{"type": "idle"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	prior, err := session.LockRuntimeShared(s.config.AuthorityDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	resolved := make(chan conversation.Target, 1)
	failed := make(chan error, 1)
	go func() {
		target, err := s.resolveConversation(ctx, conversation.ResolveRequest{ID: "example", Operation: "message", Method: "POST", Mutation: true})
		if err != nil {
			failed <- err
			return
		}
		resolved <- target
	}()
	select {
	case <-resolved:
		t.Fatal("cold activation passed admitted managed writer")
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(25 * time.Millisecond):
	}
	prior.Close()
	var target conversation.Target
	select {
	case target = <-resolved:
	case err := <-failed:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer target.Release()
	// Another CLI/team/scheduler admission uses this exact authority lock.
	if lock, err := session.LockRuntimeShared(s.config.AuthorityDir, "example"); err == nil {
		lock.Close()
		t.Fatal("new managed writer entered cold activation")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	c.beforeSend = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := target.Client.Send(ctx, "thread-1", "new input", "exact-id", ""); done <- err }()
	<-entered
	if lock, err := session.LockRuntimeShared(s.config.AuthorityDir, "example"); err == nil {
		lock.Close()
		t.Fatal("managed writer entered before receipt")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	target.Release()
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestRecoveryColdRefusalUsesExistingGuaranteedUnsentResponse(t *testing.T) {
	s, c, _ := recoveryNoticeTestServer(t)
	c.status = map[string]any{"type": "idle"}
	c.goal = true
	r := httptest.NewRequest("POST", "/codex/conversations/example/message", strings.NewReader(`{"message":"say hi","clientUserMessageId":"00000000-0000-4000-8000-000000000123"}`))
	r.Header.Set("Origin", s.config.BaseURL)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var result struct {
		Code               string
		NotSent, Retryable bool
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != http.StatusServiceUnavailable || result.Code != "message_not_sent" || !result.NotSent || result.Retryable || len(c.calls) != 0 {
		t.Fatal(w.Code, w.Body.String(), c.calls)
	}
}
