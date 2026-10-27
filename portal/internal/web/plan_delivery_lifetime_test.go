package web

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type disconnectingPlanClient struct {
	*browserContractCodex
	cancelBrowser context.CancelFunc
}

func (c *disconnectingPlanClient) Send(ctx context.Context, thread, text, id, action string) (codex.SendReceipt, error) {
	c.cancelBrowser()
	if err := ctx.Err(); err != nil {
		return codex.SendReceipt{}, err
	}
	return c.browserContractCodex.Send(ctx, thread, text, id, action)
}

func TestAdmittedPlanSendSurvivesBrowserDisconnection(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := &browserContractCodex{transcript: codex.Transcript{ThreadID: "thread-1", Status: "idle", LatestTurnID: "plan", CollaborationMode: "plan", Entries: []codex.TranscriptEntry{{TurnID: "plan", TurnStatus: "completed", Kind: "plan", Text: "Implement"}}}}
	s.config.Codex = &disconnectingPlanClient{browserContractCodex: base, cancelBrowser: cancel}
	body := fmt.Sprintf(`{"action":"same","planContextVersion":2,"planTurnId":"plan","planSha256":%q,"clientUserMessageId":"00000000-0000-4000-8000-000000000001"}`, planDigest("Implement"))
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(parent)
	response := httptest.NewRecorder()
	s.implementPlan(response, request, &session.Summary{Manifest: session.Manifest{Slug: "example", Codex: session.Codex{ThreadID: "thread-1"}}})
	if response.Code != http.StatusAccepted || base.sendCount != 1 {
		t.Fatalf("browser disconnect lost admitted send: %d %s count=%d", response.Code, response.Body.String(), base.sendCount)
	}
}

func TestAdmittedMessagePreservesDeadlineAndObservesShutdown(t *testing.T) {
	operations, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	s := &Server{operationContext: operations}
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dispatch, stop := s.admittedMessageContext(parent)
	defer stop()
	want, _ := parent.Deadline()
	got, _ := dispatch.Deadline()
	if !got.Equal(want) {
		t.Fatal("admission changed the operation budget")
	}
	cancel()
	if dispatch.Err() != nil {
		t.Fatal("browser canceled admitted operation")
	}
	shutdown()
	select {
	case <-dispatch.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel admitted operation")
	}
}

func TestPlanDeliveryErrorRetainsRetryContract(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	for _, item := range []struct {
		err    error
		status int
		code   string
	}{
		{&codex.MessageNotSentError{Err: context.DeadlineExceeded, Retryable: true}, 503, `"retryable":true`},
		{&codex.UnknownSendOutcomeError{Err: context.DeadlineExceeded}, 409, `"notSent":false`},
	} {
		response := httptest.NewRecorder()
		s.writeMessageDeliveryError(response, item.err)
		if response.Code != item.status || !strings.Contains(response.Body.String(), item.code) {
			t.Fatalf("delivery contract: %d %s", response.Code, response.Body.String())
		}
	}
}

// The receipt boundary is inside resolver admission even if the browser leaves.
type transitionSendClient struct {
	*browserContractCodex
	entered chan struct{}
	receipt chan struct{}
	cancel  context.CancelFunc
}

func (c *transitionSendClient) Send(ctx context.Context, thread, text, id, action string) (codex.SendReceipt, error) {
	c.cancel()
	close(c.entered)
	<-c.receipt
	if ctx.Err() != nil {
		return codex.SendReceipt{}, ctx.Err()
	}
	return c.browserContractCodex.Send(ctx, thread, text, id, action)
}
func TestTransitionExcludesDisconnectedSendUntilReceipt(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	browserTargetFixture(t, s, "work", "active", "thread-1")
	manifest := fmt.Sprintf("schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n  socket_path: %s\n  client_version: %s\ncreation:\n  state: ready\n  initial_goal_sent: true\n", s.config.CodexSocket, s.config.CodexVersion)
	if err := os.WriteFile(filepath.Join(s.config.Workspace, "work", "example", "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	writeWebRuntimeAuthority(t, s, "example")
	path := filepath.Join(t.TempDir(), "transition.lock")
	owner, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	s.config.TransitionLock = path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &transitionSendClient{browserContractCodex: &browserContractCodex{}, entered: make(chan struct{}), receipt: make(chan struct{}), cancel: cancel}
	s.config.Codex = client
	done := make(chan struct{})
	response := httptest.NewRecorder()
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/codex/conversations/example/message", strings.NewReader(`{"message":"hello","clientUserMessageId":"00000000-0000-4000-8000-000000000004"}`)).WithContext(ctx)
		request.Header.Set("Origin", s.config.BaseURL)
		s.Handler().ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("send never admitted: %d %s", response.Code, response.Body.String())
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
		t.Fatalf("EX did not wait for receipt: %v", err)
	}
	close(client.receipt)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("send did not settle")
	}
	if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
		t.Fatalf("send response %d %s", response.Code, response.Body.String())
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("receipt did not release admission", err)
	}
	// Fresh admission cannot pass while the host holds EX.
	blocked, cancelBlocked := context.WithCancel(context.Background())
	acquired := make(chan error, 1)
	go func() {
		_, release, err := s.acquireTransitionContext(blocked, unix.LOCK_SH)
		if err == nil {
			release()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatal("new admission settled while EX held", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancelBlocked()
	if err := <-acquired; err == nil {
		t.Fatal("new sender crossed EX")
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}
