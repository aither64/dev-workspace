package web

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadDiagnosticsMeasureStagesAndBoundPrivateOutput(t *testing.T) {
	s := newTestServer(t)
	var logs bytes.Buffer
	s.config.Logger = log.New(&logs, "", 0)
	handler := s.observeReads(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stage := readStage(r.Context(), "native_verify")
		time.Sleep(2 * time.Millisecond)
		stage()
		readStage(r.Context(), "private-stage-canary")()
		readDependencyError(r.Context(), fmt.Errorf("private-error-canary: %w", context.DeadlineExceeded))
		w.WriteHeader(503)
		_, _ = w.Write([]byte("private-response-canary"))
	}))
	for i := 0; i < 20; i++ {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/api/sessions/private-session-canary/details?message=private-query-canary", strings.NewReader("private-body-canary"))
		request.Header.Set("Authorization", "private-auth-canary")
		handler.ServeHTTP(response, request)
		if response.Code != 503 || response.Body.String() != "private-response-canary" {
			t.Fatal("diagnostics changed the response")
		}
		if !regexp.MustCompile(`^r[0-9a-f]{16}$`).MatchString(response.Header().Get("X-Portal-Read")) {
			t.Fatal("missing local correlation token")
		}
		timing := response.Header().Get("Server-Timing")
		match := regexp.MustCompile(`native_verify;dur=([0-9.]+)`).FindStringSubmatch(timing)
		if len(match) != 2 {
			t.Fatalf("missing measured dependency: %s", timing)
		}
		elapsed, err := strconv.ParseFloat(match[1], 64)
		if err != nil || elapsed < 1 {
			t.Fatalf("unmeasured delay: %s", timing)
		}
		if strings.Contains(timing, "canary") {
			t.Fatal("unsafe timing label")
		}
	}
	if strings.Count(logs.String(), "portal_read ") != 1 || logs.Len() > 1024 {
		t.Fatalf("unbounded diagnostics: %s", logs.String())
	}
	for _, expected := range []string{"lane=details", "status=503", "category=deadline", "admission=unmeasured", "native_wait=unmeasured"} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatalf("missing %q: %s", expected, logs.String())
		}
	}
	if strings.Contains(logs.String(), "canary") {
		t.Fatalf("private data leaked: %s", logs.String())
	}
}

func TestReadDiagnosticsDoNotWrapStreamsOrMutations(t *testing.T) {
	for _, item := range []struct{ method, path, lane string }{
		{"GET", "/codex/conversations/private/pending", "pending"},
		{"GET", "/codex/conversations/private/thread/page", "thread"},
		{"GET", "/api/codex-limits?provider=private", "limits"},
		{"GET", "/api/sessions/private/team-stats", "team-stats"},
		{"GET", "/codex/conversations/private/events", ""},
		{"POST", "/api/sessions/private/queue", ""},
		{"GET", "/private", ""},
	} {
		request := httptest.NewRequest(item.method, item.path, nil)
		if got := readDiagnosticLane(request); got != item.lane {
			t.Fatalf("%s %s: %s", item.method, item.path, got)
		}
	}
	s := newTestServer(t)
	s.observeReads(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(readTraceKey{}) != nil {
			t.Fatal("mutation acquired read diagnostics")
		}
		w.WriteHeader(202)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/sessions/private/message", nil))
}

func TestDetailsReadDiagnosticsKeepAuthorityAndMeasureNativeVerification(t *testing.T) {
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "example")
	verify := s.config.VerifyThread
	s.config.VerifyThread = func(ctx context.Context, thread, cwd string) error {
		time.Sleep(time.Millisecond)
		return verify(ctx, thread, cwd)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/example/details", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"interactive":true`) {
		t.Fatalf("authority changed: %d %s", response.Code, response.Body.String())
	}
	for _, stage := range []string{"lookup;dur=", "native_verify;dur=", "runtime_verify;dur=", "repo_status;dur="} {
		if !strings.Contains(response.Header().Get("Server-Timing"), stage) {
			t.Fatalf("missing %s: %s", stage, response.Header().Get("Server-Timing"))
		}
	}
}
