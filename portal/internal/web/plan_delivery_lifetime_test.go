package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
