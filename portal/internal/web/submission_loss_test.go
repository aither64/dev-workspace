package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aither64/codex-web/codex"
)

// The transport fixture retains the same native idempotency contract as Codex:
// repeated identical IDs return a receipt; mismatched input refuses.
type responseLossCodex struct {
	*browserContractCodex
	mu       sync.Mutex
	receipts map[string]codex.SendReceipt
}

func (c *responseLossCodex) Send(ctx context.Context, thread, message, id, action string) (codex.SendReceipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if receipt, ok := c.receipts[id]; ok {
		return receipt, nil
	}
	receipt, err := c.browserContractCodex.Send(ctx, thread, message, id, action)
	if err == nil {
		c.receipts[id] = receipt
	}
	return receipt, err
}

func TestPortalSendResponseLossAndHTTPRestartReconcileOriginalAttempt(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	prepareInteractiveConversation(t, server, "example")
	controller := &browserContractCodex{transcript: codex.Transcript{ThreadID: "thread-1", Status: "idle", CollaborationMode: "default"}}
	server.config.Codex = &responseLossCodex{browserContractCodex: controller, receipts: make(map[string]codex.SendReceipt)}
	handler := server.Handler()
	// Execute the actual handler and then lose its response on the wire. Native
	// acceptance and the retained submission ledger survive the HTTP restart.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusAccepted {
			t.Errorf("initial handler: %d %s", recorder.Code, recorder.Body.String())
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	const body = `{"message":"Response-loss fixture","clientUserMessageId":"12c12a7b-1898-4d29-85b4-012345678901"}`
	send := func(base string, retry bool) (*http.Response, error) {
		content := body
		if retry {
			content = strings.TrimSuffix(content, "}") + `,"retry":true}`
		}
		request, err := http.NewRequest(http.MethodPost, base+"/api/sessions/example/message", strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", server.config.BaseURL)
		request.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(request)
	}
	response, err := send(upstream.URL, false)
	if err == nil {
		response.Body.Close()
		t.Fatal("fixture did not lose the response")
	}
	upstream.Close()
	restarted := httptest.NewServer(handler)
	defer restarted.Close()
	for range 2 {
		response, err = send(restarted.URL, true)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusAccepted || !strings.Contains(string(data), "12c12a7b-1898-4d29-85b4-012345678901") {
			t.Fatalf("retry: %v %d %s", readErr, response.StatusCode, data)
		}
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.sendCount != 1 {
		t.Fatalf("response loss created %d native turns", controller.sendCount)
	}
}
