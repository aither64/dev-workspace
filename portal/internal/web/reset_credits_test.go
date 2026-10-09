package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aither64/codex-web/codex"
)

type resetLimitsCodex struct {
	*limitsCodex
	consume func(context.Context, string, string) (codex.ResetCreditResult, error)
}

func (c *resetLimitsCodex) ConsumeRateLimitResetCredit(ctx context.Context, key, id string) (codex.ResetCreditResult, error) {
	return c.consume(ctx, key, id)
}

func fixtureAccountLimits(count int64) codex.AccountRateLimits {
	account, balance := "fixture-account", "12.50"
	return codex.AccountRateLimits{AccountID: &account,
		RateLimits:   codex.RateLimitSnapshot{Credits: &codex.CreditsSnapshot{HasCredits: true, Balance: &balance}},
		ResetCredits: &codex.RateLimitResetCredits{AvailableCount: count}}
}

func resetRequest(server *Server, origin, scope, key string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"accountScope": scope, "idempotencyKey": key, "creditId": "fixture-reset"})
	r := httptest.NewRequest(http.MethodPost, "/api/codex-limits/reset", strings.NewReader(string(body)))
	r.Header.Set("Origin", origin)
	server.Handler().ServeHTTP(response, r)
	return response
}

func TestResetCreditsOriginAccountAndIdempotency(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	used, calls := 0, 0
	keys := map[string]bool{}
	server.config.Codex = &resetLimitsCodex{limitsCodex: &limitsCodex{browserContractCodex: &browserContractCodex{},
		read: func(context.Context) (codex.AccountRateLimits, error) {
			return fixtureAccountLimits(int64(3 - used)), nil
		}},
		consume: func(_ context.Context, key, id string) (codex.ResetCreditResult, error) {
			calls++
			if id != "fixture-reset" {
				t.Fatal("credit changed")
			}
			outcome := "alreadyRedeemed"
			if !keys[key] {
				keys[key] = true
				used++
				outcome = "reset"
			}
			return codex.ResetCreditResult{Outcome: outcome}, nil
		}}
	snapshot, err := server.loadCodexLimits(context.Background())
	if err != nil || !snapshot.CanReset || snapshot.ResetCredits.AvailableCount != 3 || *snapshot.Credits.Balance != "12.50" {
		t.Fatalf("snapshot: %#v, %v", snapshot, err)
	}
	const key = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if response := resetRequest(server, "https://untrusted.invalid", snapshot.AccountScope, key); response.Code != 403 {
		t.Fatalf("origin: %d", response.Code)
	}
	if response := resetRequest(server, server.config.BaseURL, strings.Repeat("b", 64), key); response.Code != 409 {
		t.Fatalf("account: %d %s", response.Code, response.Body)
	}
	if calls != 0 {
		t.Fatal("rejected request consumed a reset")
	}
	for _, outcome := range []string{"reset", "alreadyRedeemed"} {
		response := resetRequest(server, server.config.BaseURL, snapshot.AccountScope, key)
		if response.Code != 200 || !strings.Contains(response.Body.String(), outcome) {
			t.Fatalf("reset: %d %s", response.Code, response.Body)
		}
	}
	if used != 1 || calls != 2 {
		t.Fatalf("used %d; calls %d", used, calls)
	}
	fresh, err := server.loadCodexLimits(context.Background())
	if err != nil || fresh.ResetCredits.AvailableCount != 2 {
		t.Fatalf("fresh: %#v, %v", fresh, err)
	}
	encoded, _ := json.Marshal(fresh)
	if strings.Contains(string(encoded), "fixture-account") || strings.Contains(string(encoded), "accountId") {
		t.Fatal("raw account ID exposed")
	}
}

func TestResetInvalidationFencesAnOlderRead(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	server.config.Codex = &resetLimitsCodex{limitsCodex: &limitsCodex{browserContractCodex: &browserContractCodex{},
		read: func(context.Context) (codex.AccountRateLimits, error) {
			if reads.Add(1) == 1 {
				close(entered)
				<-release
				return fixtureAccountLimits(3), nil
			}
			return fixtureAccountLimits(2), nil
		}}, consume: func(context.Context, string, string) (codex.ResetCreditResult, error) {
		return codex.ResetCreditResult{Outcome: "reset"}, nil
	}}
	go func() { defer close(finished); _, _ = server.loadCodexLimits(context.Background()) }()
	<-entered
	response := resetRequest(server, server.config.BaseURL, accountLimitsScope(fixtureAccountLimits(3)), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if response.Code != 200 {
		t.Fatalf("reset: %d %s", response.Code, response.Body)
	}
	if _, err := server.loadCodexLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-finished
	fresh, err := server.loadCodexLimits(context.Background())
	if err != nil || fresh.ResetCredits.AvailableCount != 2 {
		t.Fatalf("older read poisoned cache: %#v, %v", fresh, err)
	}
}
