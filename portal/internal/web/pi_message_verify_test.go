package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// Exercise the production resolver and HTTP handler with a disposable runtime.
// The fake receipt represents native reconciliation, not portal-side dedup.
type piMessageVerifyRuntime struct {
	*piCreationRuntimeFixture
	deadline         time.Time
	snapshotDelay    time.Duration
	staleVerify      bool
	loseFinalBind    bool
	cancelAndLoseAck context.CancelFunc
	binds, effects   int
	verifyAt, sendAt []int
	finalErr         error
	original         backend.SendRequest
	receipt          backend.SendReceipt
}

type piMessageVerificationOperation struct {
	runtime *piMessageVerifyRuntime
	bind    int
}

func (f *piMessageVerifyRuntime) Bind(ctx context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	f.binds++
	if h.Conversation != f.state.Conversation || h.Generation != f.state.Generation {
		return backend.Operations{}, piruntime.ErrScope
	}
	if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(f.deadline) {
		return backend.Operations{}, errors.New("Bind did not use the original request deadline")
	}
	if f.snapshotDelay != 0 {
		timer := time.NewTimer(f.snapshotDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		f.finalErr = err
		return backend.Operations{}, err
	}
	if f.loseFinalBind && len(f.verifyAt) > 0 {
		f.finalErr = piruntime.ErrUnavailable
		return backend.Operations{}, f.finalErr
	}
	op := piMessageVerificationOperation{runtime: f, bind: f.binds}
	return backend.Operations{Verify: op, Send: op}, nil
}

func (o piMessageVerificationOperation) Verify(ctx context.Context, id backend.Identity) error {
	f := o.runtime
	f.verifyAt = append(f.verifyAt, o.bind)
	if id != piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: f.state.Conversation, Generation: f.state.Generation}) || f.staleVerify {
		return agentbackend.ErrStaleExecution
	}
	return ctx.Err()
}

func (o piMessageVerificationOperation) Send(ctx context.Context, _ backend.Identity, req backend.SendRequest) (backend.SendResult, error) {
	f := o.runtime
	f.sendAt = append(f.sendAt, o.bind)
	if f.receipt.ClientUserMessageID == "" && f.cancelAndLoseAck != nil {
		f.cancelAndLoseAck()
	}
	if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(f.deadline) || ctx.Err() != nil {
		return backend.SendResult{Outcome: backend.Unknown}, errors.New("admitted Send lost its detached context or original deadline")
	}
	if f.receipt.ClientUserMessageID != "" {
		if req.ClientUserMessageID != f.original.ClientUserMessageID || req.Message != f.original.Message || !req.Retry {
			return backend.SendResult{Outcome: backend.Unknown}, piruntime.ErrScope
		}
		return backend.SendResult{Outcome: backend.Accepted, Receipt: f.receipt}, nil
	}
	f.effects++
	f.original = req
	f.receipt = backend.SendReceipt{ClientUserMessageID: req.ClientUserMessageID, TurnID: "fixture-entry"}
	if f.cancelAndLoseAck != nil {
		return backend.SendResult{Outcome: backend.Unknown}, errors.New("synthetic lost acknowledgement")
	}
	return backend.SendResult{Outcome: backend.Accepted, Receipt: f.receipt}, nil
}

func TestPiMessageFreshVerification(t *testing.T) {
	const slug = "2026-11-22-fixture-message"
	const client = "00000000-0000-4000-8000-000000000001"
	const budget = 1750 * time.Millisecond
	for _, name := range []string{"budget", "stale_verify", "final_bind_unavailable", "detached_same_id_receipt"} {
		t.Run(name, func(t *testing.T) {
			s, creation, native := newPiCreationFixtureForSlug(t, slug)
			if err := s.initializePiCreation(creation); err != nil {
				t.Fatal(err)
			}
			f := &piMessageVerifyRuntime{piCreationRuntimeFixture: native, staleVerify: name == "stale_verify", loseFinalBind: name == "final_bind_unavailable"}
			if name == "budget" {
				// Two snapshots fit the original deadline; three cannot.
				f.snapshotDelay = 650 * time.Millisecond
			}
			s.config.PiCreation.Runtime, s.piWorkflows.runtime = f, f
			h, err := conversation.NewBackendHandler(conversation.BackendOptions{
				BasePath: "/codex", AllowedOrigins: []string{s.config.BaseURL}, OperationTimeout: budget,
				Resolver: conversation.BackendResolverFunc(func(ctx context.Context, req conversation.ResolveRequest) (conversation.BackendTarget, error) {
					f.deadline, _ = ctx.Deadline()
					return s.resolvePiBackend(ctx, req)
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			post := func(ctx context.Context, retry bool, status int, code string) backend.SendReceipt {
				t.Helper()
				body := `{"message":"fixture","clientUserMessageId":"` + client + `"}`
				if retry {
					body = strings.TrimSuffix(body, "}") + `,"retry":true}`
				}
				r := httptest.NewRequest(http.MethodPost, "/codex/conversations/"+slug+"/message", strings.NewReader(body))
				r.Header.Set("Origin", s.config.BaseURL)
				r.Header.Set("Content-Type", "application/json")
				r = r.WithContext(context.WithValue(ctx, piConversationScopeKey{}, piWorkflowNamespace(native.state.Conversation)))
				w := httptest.NewRecorder()
				start := time.Now()
				h.ServeHTTP(w, r)
				t.Logf("status=%d elapsed=%s binds=%d verify-at=%v send-at=%v effects=%d bind-error=%v", w.Code, time.Since(start).Round(time.Millisecond), f.binds, f.verifyAt, f.sendAt, f.effects, f.finalErr)
				var result struct {
					backend.SendReceipt
					Code    string `json:"code"`
					NotSent *bool  `json:"notSent"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != status || result.Code != code {
					t.Fatalf("HTTP %d, code %q, parse %v; want HTTP %d, code %q", w.Code, result.Code, err, status, code)
				}
				switch status {
				case http.StatusAccepted:
					if result.ClientUserMessageID != client || result.TurnID != "fixture-entry" {
						t.Fatal("missing matching fixture receipt")
					}
				case http.StatusServiceUnavailable:
					if result.NotSent == nil || !*result.NotSent || result.SendReceipt != (backend.SendReceipt{}) {
						t.Fatal("refusal was not proved pre-dispatch")
					}
				case http.StatusConflict:
					if result.NotSent == nil || *result.NotSent || result.SendReceipt != (backend.SendReceipt{}) {
						t.Fatal("ambiguous delivery must not be reported as NotSubmitted")
					}
				case http.StatusNotFound:
					if result.NotSent != nil || result.SendReceipt != (backend.SendReceipt{}) {
						t.Fatal("failed verifier must not assert a receipt class")
					}
				}
				return result.SendReceipt
			}
			ctx := context.Background()
			binds, effects, verifyAt, sendAt := 2, 1, []int{1}, []int{2}
			switch name {
			case "budget":
				post(ctx, false, http.StatusAccepted, "")
			case "stale_verify":
				post(ctx, false, http.StatusNotFound, "")
				binds, effects, sendAt = 1, 0, nil
			case "final_bind_unavailable":
				post(ctx, false, http.StatusServiceUnavailable, "message_not_sent")
				effects, sendAt = 0, nil
			case "detached_same_id_receipt":
				browser, cancel := context.WithCancel(ctx)
				defer cancel()
				f.cancelAndLoseAck = cancel
				post(browser, false, http.StatusConflict, "message_delivery_unknown")
				if !errors.Is(browser.Err(), context.Canceled) || f.effects != 1 {
					t.Fatal("detached Send did not finish its synthetic native dispatch")
				}
				if got := post(ctx, true, http.StatusAccepted, ""); got != f.receipt {
					t.Fatal("same-ID reconciliation did not return the original fixture receipt")
				}
				binds, verifyAt, sendAt = 4, []int{1, 3}, []int{2, 4}
			}
			if f.binds != binds || f.effects != effects || !slices.Equal(f.verifyAt, verifyAt) || !slices.Equal(f.sendAt, sendAt) {
				t.Fatalf("want binds=%d effects=%d verify-at=%v send-at=%v", binds, effects, verifyAt, sendAt)
			}
		})
	}
}
