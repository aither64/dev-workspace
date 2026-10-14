package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func recoveryNoticeTestServer(t *testing.T) (*Server, *recoveryTestCodex, *session.Summary) {
	t.Helper()
	s, c := recoveryTestServer(t)
	manifest := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("creation:\n  state: ready\n  initial_goal_sent: true\n")...)
	if err := os.WriteFile(manifest, data, 0644); err != nil {
		t.Fatal(err)
	}
	writeWebRuntimeAuthority(t, s, "example")
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	s.normalizeInteractivity(context.Background(), summary)
	if !summary.Interactive {
		t.Fatal("fixture is not interactive")
	}
	return s, c, summary
}

func dismissNoticeForTest(s *Server, id, origin string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"noticeId": id})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/example/dismiss-recovery", bytes.NewReader(body))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestRecoveryNoticeDismissalNeverActivatesSavedWork(t *testing.T) {
	for _, name := range []string{"empty", "queued", "goal"} {
		t.Run(name, func(t *testing.T) {
			s, c, summary := recoveryNoticeTestServer(t)
			if name == "queued" {
				c.queue = []codex.QueueEntry{{ID: "saved", Text: "saved work"}}
			}
			c.goal = name == "goal"
			before, err := s.recoveryStore().Load("example")
			if err != nil {
				t.Fatal(err)
			}
			queue := append([]codex.QueueEntry(nil), c.queue...)
			status := s.recoveryStatus(summary)
			if status.State != "waiting" || status.NoticeID == "" || status.NoticeDismissed {
				t.Fatal(status)
			}
			for range 2 {
				w := dismissNoticeForTest(s, status.NoticeID, s.config.BaseURL)
				if w.Code != http.StatusOK {
					t.Fatal(w.Code, w.Body.String())
				}
				var result recoveryStatus
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.NoticeDismissed || result.State != "waiting" {
					t.Fatal(result, err)
				}
			}
			after, err := s.recoveryStore().Load("example")
			if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(queue, c.queue) ||
				len(c.calls) != 0 || c.sendCount != 0 || c.goal != (name == "goal") || !s.recoveryHeld("example", "thread-1") {
				t.Fatal("dismissal touched saved work", before, after, c.calls, c.queue, err)
			}
			// A new portal process reads the same private preference and keeps the hold.
			reopened, err := New(s.config)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if got := reopened.recoveryStatus(summary); !got.NoticeDismissed || got.NoticeID != status.NoticeID || got.State != "waiting" {
				t.Fatal(got)
			}
			page := httptest.NewRecorder()
			reopened.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/example/", nil))
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `class="notice full recovery-notice" role="status" aria-live="polite" hidden`) {
				t.Fatal("reload flashed dismissed notice", page.Code)
			}
		})
	}
}

func TestRecoveryNoticeResetsForCanonicalRestorationAndNewSocket(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "cli-restoration", true: "portal-interrupted-after-restoration"}[interrupted], func(t *testing.T) {
			s, c, summary := recoveryNoticeTestServer(t)
			first := s.recoveryStatus(summary)
			if w := dismissNoticeForTest(s, first.NoticeID, s.config.BaseURL); w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			// Canonical resume publishes fresh terminal authority before its recovery
			// bridge. No portal wrapper or notice-reset callback completes this step.
			authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, "example", s.config.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			previousIdentity := authority.TmuxIdentity
			authority.TmuxIdentity = strings.Repeat("b", 64)
			tmux, err := os.ReadFile(s.config.Tmux)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.config.Tmux, []byte(strings.ReplaceAll(string(tmux), previousIdentity, authority.TmuxIdentity)), 0755); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(authority)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.config.AuthorityDir, "example.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.recoveryStore().Set(context.Background(), "example", "thread-1", s.config.CodexSocket, true, false); err != nil {
				t.Fatal(err)
			}
			if interrupted {
				s.setRecoveryStatus("example", recoveryStatus{State: "recovering"})
			}
			reopened, err := New(s.config)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			second := reopened.recoveryStatus(summary)
			if second.NoticeDismissed || second.NoticeID == first.NoticeID || second.State != "waiting" {
				t.Fatal(second)
			}
			if w := dismissNoticeForTest(reopened, first.NoticeID, s.config.BaseURL); w.Code != http.StatusConflict {
				t.Fatal("stale notice accepted", w.Code)
			}
			if w := dismissNoticeForTest(reopened, second.NoticeID, s.config.BaseURL); w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			if err := os.Remove(s.config.CodexSocket); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", s.config.CodexSocket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			third := reopened.recoveryStatus(summary)
			if third.NoticeDismissed || third.NoticeID == second.NoticeID || !reopened.recoveryHeld("example", "thread-1") || len(c.calls) != 0 {
				t.Fatal(third, c.calls)
			}
		})
	}
}

func TestRecoveryNoticeRefusalsAndWriteFailureKeepBanner(t *testing.T) {
	s, c, summary := recoveryNoticeTestServer(t)
	status := s.recoveryStatus(summary)
	for _, tc := range []struct {
		id, origin string
		code       int
	}{
		{status.NoticeID, "https://foreign.example", http.StatusForbidden},
		{"", s.config.BaseURL, http.StatusConflict},
		{"old-notice", s.config.BaseURL, http.StatusConflict},
	} {
		if w := dismissNoticeForTest(s, tc.id, tc.origin); w.Code != tc.code {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, state := range []string{"recovering", "failed"} {
		s.setRecoveryStatus("example", recoveryStatus{State: state, Error: "recovery error"})
		if w := dismissNoticeForTest(s, status.NoticeID, s.config.BaseURL); w.Code != http.StatusConflict {
			t.Fatal(state, w.Code)
		}
		if got := s.recoveryStatus(summary); got.NoticeDismissed || got.State != state {
			t.Fatal(got)
		}
	}
	s.setRecoveryStatus("example", recoveryStatus{State: "waiting"})
	if err := os.MkdirAll(s.threadRecordPath("recovery-notice", "thread-1"), 0700); err != nil {
		t.Fatal(err)
	}
	if w := dismissNoticeForTest(s, status.NoticeID, s.config.BaseURL); w.Code != http.StatusInternalServerError {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := s.recoveryStatus(summary); got.NoticeDismissed || got.State != "waiting" || len(c.calls) != 0 {
		t.Fatal(got, c.calls)
	}
}

func TestRecoveryNoticeCannotMoveToAnotherSessionIncarnation(t *testing.T) {
	s, _, summary := recoveryNoticeTestServer(t)
	status := s.recoveryStatus(summary)
	if w := dismissNoticeForTest(s, status.NoticeID, s.config.BaseURL); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	dir := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.Rename(dir, dir+"-previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"plan.md", "state.md", "portal.yml"} {
		data, err := os.ReadFile(filepath.Join(dir+"-previous", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.recoveryStatus(summary); got.NoticeDismissed || got.NoticeID == status.NoticeID {
		t.Fatal(got)
	}
}

func TestRecoveryNoticeDismissalLeavesExplicitSendAvailable(t *testing.T) {
	s, c, summary := recoveryNoticeTestServer(t)
	if w := dismissNoticeForTest(s, s.recoveryStatus(summary).NoticeID, s.config.BaseURL); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	if _, err := client.Send(context.Background(), "thread-1", "An explicit new request", "new-request", ""); err != nil {
		t.Fatal(err)
	}
	if s.recoveryHeld("example", "thread-1") || len(c.queue) != 1 || c.queue[0].Text != "An explicit new request" {
		t.Fatal("dismissal prevented explicit work", c.calls, c.queue)
	}
}

func TestRecoveryNoticeDismissalFromHeldMemberPreservesActiveRoot(t *testing.T) {
	s, c, summary := recoveryNoticeTestServer(t)
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), "example", "thread-1", true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Thread: "member-thread", State: "ready", Model: "model-1", Effort: "medium", AddedAt: time.Now().UTC()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.recoveryStore().Set(context.Background(), "example", "thread-1", s.config.CodexSocket, true, true); err != nil {
		t.Fatal(err)
	}
	status, err := s.selectedRecoveryStatus(summary, "implementer0")
	if err != nil || status.State != "waiting" {
		t.Fatal(status, err)
	}
	body, _ := json.Marshal(map[string]string{"noticeId": status.NoticeID, "member": "implementer0"})
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/example/dismiss-recovery", bytes.NewReader(body))
	r.Header.Set("Origin", s.config.BaseURL)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.recoveryHeld("example", "thread-1") || !s.recoveryHeld("example", "member-thread") || len(c.calls) != 0 {
		t.Fatal("member dismissal changed execution permissions", c.calls)
	}
	if got, err := s.selectedRecoveryStatus(summary, "implementer0"); err != nil || !got.NoticeDismissed {
		t.Fatal(got, err)
	}
}
