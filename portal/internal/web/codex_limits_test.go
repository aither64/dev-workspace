package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

func (client *browserContractCodex) ReadAccountRateLimits(context.Context) (codex.AccountRateLimits, error) {
	return codex.AccountRateLimits{}, nil
}

type limitsCodex struct {
	*browserContractCodex
	read func(context.Context) (codex.AccountRateLimits, error)
}

func (client *limitsCodex) ReadAccountRateLimits(ctx context.Context) (codex.AccountRateLimits, error) {
	return client.read(ctx)
}

func limitWindow(duration int64, used int) *codex.RateLimitWindow {
	reset := int64(1800000000)
	return &codex.RateLimitWindow{UsedPercent: used, WindowDurationMins: &duration, ResetsAt: &reset}
}

func TestMainCodexWindows(t *testing.T) {
	weekly, short := limitWindow(10080, 42), limitWindow(300, 7)
	for _, tc := range []struct {
		name     string
		input    codex.AccountRateLimits
		expected []codex.RateLimitWindow
	}{
		{"weekly in primary", codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{Primary: weekly}}, []codex.RateLimitWindow{*weekly}},
		{"durations instead of positions", codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{LimitID: "codex", Primary: weekly, Secondary: short}}, []codex.RateLimitWindow{*short, *weekly}},
		{"main map preferred", codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{Primary: short}, RateLimitsByLimitID: map[string]codex.RateLimitSnapshot{"codex": {Primary: weekly}, "codex_model": {Primary: short}}}, []codex.RateLimitWindow{*weekly}},
		{"empty main stays empty", codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{Primary: short}, RateLimitsByLimitID: map[string]codex.RateLimitSnapshot{"codex": {}}}, []codex.RateLimitWindow{}},
		{"other buckets excluded", codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{LimitID: "codex_model", Primary: weekly}, RateLimitsByLimitID: map[string]codex.RateLimitSnapshot{"codex_model": {Primary: short}}}, []codex.RateLimitWindow{}},
		{"unreported durations excluded", codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{Primary: &codex.RateLimitWindow{UsedPercent: 5}, Secondary: limitWindow(60, 2)}}, []codex.RateLimitWindow{}},
		{"duplicate duration shown once", codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{Primary: weekly, Secondary: weekly}}, []codex.RateLimitWindow{*weekly}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mainCodexWindows(tc.input); !reflect.DeepEqual(got, tc.expected) {
				t.Fatalf("windows = %#v; expected %#v", got, tc.expected)
			}
		})
	}
}

func TestCodexLimitsEndpointCacheAndRefresh(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	calls := 0
	fail := false
	server.config.Codex = &limitsCodex{browserContractCodex: &browserContractCodex{}, read: func(context.Context) (codex.AccountRateLimits, error) {
		calls++
		if fail {
			return codex.AccountRateLimits{}, errors.New("private upstream detail")
		}
		return codex.AccountRateLimits{RateLimitsByLimitID: map[string]codex.RateLimitSnapshot{"codex": {Primary: limitWindow(10080, 42)}, "codex_model": {Primary: limitWindow(300, 7)}}}, nil
	}}
	request := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		// Account reads need neither a session nor browser-supplied socket/thread.
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/codex-limits", nil))
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("limits response is cacheable")
		}
		return response
	}
	first := request()
	if first.Code != http.StatusOK {
		t.Fatalf("limits = %d: %s", first.Code, first.Body.String())
	}
	var snapshot codexLimitsSnapshot
	if err := json.Unmarshal(first.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Windows) != 1 || snapshot.Windows[0].UsedPercent != 42 || snapshot.UpdatedAt.IsZero() {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if strings.Contains(first.Body.String(), "codex_model") || strings.Contains(first.Body.String(), "limitId") {
		t.Fatal("account metadata leaked")
	}
	second := request()
	if first.Body.String() != second.Body.String() || calls != 1 {
		t.Fatalf("cache missed: %d reads", calls)
	}
	server.codexLimitsCache.UpdatedAt = time.Now().Add(-31 * time.Second)
	fail = true
	failed := request()
	if failed.Code != http.StatusServiceUnavailable || strings.Contains(failed.Body.String(), "private") {
		t.Fatalf("failed refresh = %d: %s", failed.Code, failed.Body.String())
	}
	if server.codexLimitsCache.Windows[0].UsedPercent != 42 {
		t.Fatal("failed refresh discarded cache")
	}
	fail = false
	if recovered := request(); recovered.Code != http.StatusOK || calls != 3 {
		t.Fatalf("refresh did not recover: code %d, calls %d", recovered.Code, calls)
	}
}

func TestCodexLimitsCoalescesReadsAndWaiterCancellation(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	server.config.Codex = &limitsCodex{browserContractCodex: &browserContractCodex{}, read: func(ctx context.Context) (codex.AccountRateLimits, error) {
		calls++
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return codex.AccountRateLimits{}, ctx.Err()
		}
		return codex.AccountRateLimits{RateLimits: codex.RateLimitSnapshot{Primary: limitWindow(10080, 42)}}, nil
	}}
	var group sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, err := server.loadCodexLimits(context.Background()); results <- err }()
	}
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.loadCodexLimits(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter = %v", err)
	}
	close(release)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("concurrent reads = %d", calls)
	}
}

func TestCodexLimitsUnavailable(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/codex-limits", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured limits = %d", response.Code)
	}
}
