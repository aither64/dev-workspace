package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

type retryReadBackend struct {
	turnretry.Backend
	o turnretry.Observation
}

func (b retryReadBackend) Observe(context.Context, turnretry.Scope) (turnretry.Observation, error) {
	return b.o, nil
}

type retryHTTPClient struct {
	*browserContractCodex
	store   turnretry.Store
	scope   turnretry.Scope
	backend turnretry.Backend
}

func (c *retryHTTPClient) RetryAvailable() bool { return true }

func (c *retryHTTPClient) RetryStore() turnretry.Store { return c.store }
func (c *retryHTTPClient) RetryScope(context.Context, string) (turnretry.Scope, error) {
	return c.scope, nil
}
func (c *retryHTTPClient) RetryBackend() turnretry.Backend { return c.backend }

func TestTurnRetryWorkersReserveEachConversationUntilCompletion(t *testing.T) {
	queue := retryJobs{jobs: make(chan string, 2), active: make(map[string]bool)}
	if !queue.enqueue("slow") || <-queue.jobs != "slow" {
		t.Fatal("first conversation not admitted")
	}
	for i := 0; i < 10; i++ {
		queue.enqueue("slow")
	}
	if !queue.enqueue("other") || <-queue.jobs != "other" {
		t.Fatal("slow conversation monopolized workers")
	}
	if len(queue.jobs) != 0 {
		t.Fatal("duplicate conversation queued")
	}
	queue.done("slow")
	if !queue.enqueue("slow") || <-queue.jobs != "slow" {
		t.Fatal("completed conversation cannot be scheduled again")
	}
}

func TestTurnRetryHTTPAuthorityPreferenceAndNoReadEnrollment(t *testing.T) {
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "example")
	scope := turnretry.Scope{Workspace: s.config.Workspace, Slug: "example", Root: "thread-1", Thread: "thread-1", Socket: s.config.CodexSocket}
	client := &retryHTTPClient{browserContractCodex: &browserContractCodex{}, store: turnretry.Store{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}, scope: scope, backend: retryReadBackend{o: turnretry.Observation{Turn: "old-failure", Status: "failed", Code: "serverOverloaded"}}}
	s.config.Codex = client
	request := func(method, path, body, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/sessions/example/retry", "/codex/conversations/example/retry"} {
		w := request("GET", path, "", "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if _, err := client.store.Load(scope); !os.IsNotExist(err) {
		t.Fatal("GET enrolled or wrote retry state", err)
	}
	if w := request("POST", "/api/sessions/example/retry/enabled", `{"enabled":false}`, "https://foreign.example"); w.Code != 403 {
		t.Fatal("foreign origin accepted", w.Code)
	}
	for _, path := range []string{"/api/sessions/example/retry/enabled", "/codex/conversations/example/retry/enabled", "/api/sessions/example/retry/cancel", "/codex/conversations/example/retry/cancel"} {
		if w := request("POST", path, `{}`, s.config.BaseURL); w.Code != 409 || !strings.Contains(w.Body.String(), "retry_reload_required") {
			t.Fatal("legacy mutation accepted", w.Code, w.Body.String())
		}
	}
	if _, err := client.store.Load(scope); !os.IsNotExist(err) {
		t.Fatal("legacy mutation wrote ledger", err)
	}
	if w := request("GET", "/codex/conversations/example~nativechild/retry", "", ""); w.Code == 200 {
		t.Fatal("unretained child exposed retry authority")
	}
}

func TestTurnRetryStatusKeepsPrivateReasonAndSnapshotOutOfResponse(t *testing.T) {
	s := turnretry.New(turnretry.Scope{})
	s.Phase, s.Reason = "blocked", "settings_unproven"
	s.Snapshots = []turnretry.Snapshot{{CommonInstructions: "private-instructions", Options: codex.TurnOptions{Model: "private-model"}}}
	data, err := json.Marshal(retryView(s, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"settings_unproven", "private-instructions", "private-model"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("private retry state leaked")
		}
	}
	if !strings.Contains(string(data), "Continue manually") {
		t.Fatal("missing actionable reason")
	}
}

func TestTurnRetryGenerationFencePreventsOldWorkerAdmission(t *testing.T) {
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "example")
	scope := turnretry.Scope{Workspace: s.config.Workspace, Slug: "example", Root: "thread-1", Thread: "thread-1", Socket: s.config.CodexSocket}
	c := &retryHTTPClient{browserContractCodex: &browserContractCodex{}, store: turnretry.Store{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}, scope: scope}
	s.config.Codex = c
	if err := os.Remove(s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := s.stepTurnRetry(context.Background(), c, "example"); err == nil {
		t.Fatal("superseded worker admitted")
	}
	if _, err := c.store.Load(scope); !os.IsNotExist(err) {
		t.Fatal("old worker changed durable state", err)
	}
}

func TestTurnRetryRootAdmissionSharesRosterOperationFence(t *testing.T) {
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "example")
	s.config.Codex = &ordinaryRetainedCodex{cwd: filepath.Join(s.config.Workspace, "work", "example")}
	service, err := s.teamService("example")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := service.Store.LockOperation(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if target, err := s.resolveConversation(ctx, conversation.ResolveRequest{ID: "example", Operation: "automatic-retry", Mutation: true}); err == nil {
		target.Release()
		unlock()
		t.Fatal("root retry bypassed a CLI team operation")
	}
	unlock()
	target, err := s.resolveConversation(context.Background(), conversation.ResolveRequest{ID: "example", Operation: "automatic-retry", Mutation: true})
	if err != nil {
		t.Fatal(err)
	}
	target.Release()
}
