package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type readTraceKey struct{}

type readTrace struct {
	mu                           sync.Mutex
	started                      time.Time
	correlation                  string
	stages                       map[string]time.Duration
	category                     string
	serviceStage, serviceOutcome string
}

// All labels come from this fixed vocabulary. Never accept paths, identifiers,
// query parameters or error text as a diagnostic label.
func readStage(ctx context.Context, name string) func() {
	trace, _ := ctx.Value(readTraceKey{}).(*readTrace)
	if trace == nil {
		return func() {}
	}
	switch name {
	case "lookup", "lifecycle", "interactivity", "native_verify", "runtime_verify",
		"repo_validate", "repo_status", "team", "cluster", "services", "limits_dependency", "limits_cache_wait", "resolve":
	default:
		return func() {}
	}
	start := time.Now()
	return func() {
		trace.mu.Lock()
		trace.stages[name] += time.Since(start)
		trace.mu.Unlock()
	}
}

func readDependencyError(ctx context.Context, err error) {
	trace, _ := ctx.Value(readTraceKey{}).(*readTrace)
	if trace == nil || err == nil {
		return
	}
	category := "dependency_error"
	if errors.Is(err, context.DeadlineExceeded) {
		category = "deadline"
	} else if errors.Is(err, context.Canceled) {
		category = "canceled"
	}
	trace.mu.Lock()
	trace.category = category
	trace.mu.Unlock()
}

func (trace *readTrace) timing() string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	labels := make([]string, 0, len(trace.stages))
	for name := range trace.stages {
		labels = append(labels, name)
	}
	sort.Strings(labels)
	values := make([]string, 0, len(labels)+1)
	for _, name := range labels {
		values = append(values, fmt.Sprintf("%s;dur=%.2f", name, float64(trace.stages[name])/float64(time.Millisecond)))
	}
	return strings.Join(append(values, fmt.Sprintf("total;dur=%.2f", float64(time.Since(trace.started))/float64(time.Millisecond))), ", ")
}

func readDiagnosticLane(r *http.Request) string {
	if r.Method != http.MethodGet {
		return ""
	}
	switch r.URL.Path {
	case "/api/models":
		return "models"
	case "/api/codex-limits":
		return "limits"
	case "/api/codex-providers":
		return "providers"
	}
	operation := ""
	if parts, ok := sessionAPIPath(r.URL); ok {
		operation = parts[1]
	} else if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) >= 4 && len(parts) <= 5 && parts[0] == "codex" && parts[1] == "conversations" {
		operation = parts[3]
	}
	switch operation {
	case "details", "pending", "queue", "activity", "team-stats", "thread", "files":
		return operation
	}
	return ""
}

type readDiagnosticWriter struct {
	http.ResponseWriter
	trace  *readTrace
	status int
}

func (w *readDiagnosticWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *readDiagnosticWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.Header().Set("Server-Timing", w.trace.timing())
	w.ResponseWriter.WriteHeader(status)
}

func (w *readDiagnosticWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

type readDiagnosticState struct {
	sequence       atomic.Uint64
	lastLog        atomic.Int64
	lastServiceLog atomic.Int64
}

// Service producers can outlive their first HTTP waiter. Record their actual
// failure before the shared cache replaces it, without retaining error text.
func (s *Server) serviceReadFailure(ctx context.Context, stage, outcome string, elapsed time.Duration, exitCode, stdoutBytes, stderrBytes int) {
	switch stage {
	case "identity_before", "helper", "projection", "cache_wait", "identity_after":
	default:
		return
	}
	switch outcome {
	case "unavailable", "start", "deadline", "canceled", "exit", "output_limit", "invalid":
	default:
		return
	}
	correlation := "unmeasured"
	if trace, _ := ctx.Value(readTraceKey{}).(*readTrace); trace != nil {
		correlation = trace.correlation
		category := "dependency_error"
		if outcome == "deadline" || outcome == "canceled" {
			category = outcome
		}
		trace.mu.Lock()
		// A later coarse cache failure must not erase the producer context fact.
		if trace.category == "" || category != "dependency_error" {
			trace.category = category
		}
		if trace.serviceStage == "" || stage != "cache_wait" {
			trace.serviceStage, trace.serviceOutcome = stage, outcome
		}
		trace.mu.Unlock()
	}
	// Cache/wait outcomes belong to the HTTP trace. A separate bounded producer
	// record can still arrive after that HTTP waiter has already returned.
	if stage == "cache_wait" || !takeDiagnosticLog(&s.readDiagnostics.lastServiceLog) {
		return
	}
	s.config.Logger.Printf("portal_service_read correlation=%s stage=%s outcome=%s elapsed_ms=%.2f exit_code=%d stdout_bytes=%d stderr_bytes=%d",
		correlation, stage, outcome, float64(elapsed)/float64(time.Millisecond), exitCode, stdoutBytes, stderrBytes)
}

func takeDiagnosticLog(last *atomic.Int64) bool {
	now := time.Now().UnixNano()
	previous := last.Load()
	return now-previous >= int64(time.Second) && last.CompareAndSwap(previous, now)
}

func (s *Server) observeReads(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lane := readDiagnosticLane(r)
		if lane == "" {
			next.ServeHTTP(w, r)
			return
		}
		correlation := fmt.Sprintf("r%016x", s.readDiagnostics.sequence.Add(1))
		trace := &readTrace{started: time.Now(), correlation: correlation, stages: make(map[string]time.Duration)}
		w.Header().Set("X-Portal-Read", correlation)
		writer := &readDiagnosticWriter{ResponseWriter: w, trace: trace}
		next.ServeHTTP(writer, r.WithContext(context.WithValue(r.Context(), readTraceKey{}, trace)))
		elapsed := time.Since(trace.started)
		if writer.status == 0 {
			writer.status = http.StatusOK
		}
		trace.mu.Lock()
		category := trace.category
		serviceStage, serviceOutcome := trace.serviceStage, trace.serviceOutcome
		trace.mu.Unlock()
		if err := r.Context().Err(); err != nil {
			category = "canceled"
			if errors.Is(err, context.DeadlineExceeded) {
				category = "deadline"
			}
		}
		if category == "" {
			category = "ok"
			if writer.status >= 400 {
				category = "http_error"
			}
		}
		if elapsed < time.Second && category == "ok" {
			return
		}
		// At most one bounded record per second for this portal, including storms
		// of canceled tabs. Per-response timing remains available without sampling.
		if !takeDiagnosticLog(&s.readDiagnostics.lastLog) {
			return
		}
		service := ""
		if serviceStage != "" {
			service = fmt.Sprintf(" service_stage=%s service_outcome=%s", serviceStage, serviceOutcome)
		}
		s.config.Logger.Printf("portal_read correlation=%s lane=%s status=%d category=%s timing=%q admission=unmeasured native_wait=unmeasured%s",
			correlation, lane, writer.status, category, trace.timing(), service)
	})
}
