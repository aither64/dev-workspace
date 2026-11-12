package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

type catalogLogBuffer struct {
	sync.Mutex
	data bytes.Buffer
}

func (b *catalogLogBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.data.Write(p)
}
func (b *catalogLogBuffer) String() string { b.Lock(); defer b.Unlock(); return b.data.String() }

type catalogNativeStub struct {
	browserContractCodex
	err error
}

func (c *catalogNativeStub) ListModels(context.Context) ([]codex.Model, error) {
	return []codex.Model{{Model: "safe-model"}}, c.err
}

func catalogDiagnosticServer(t *testing.T) (*Server, *catalogLogBuffer) {
	t.Helper()
	s := newTestServer(t)
	s.config.Codex = &catalogNativeStub{}
	logs := &catalogLogBuffer{}
	s.config.Logger = log.New(logs, "", 0)
	return s, logs
}

func catalogCommand(t *testing.T, s *Server, body string) (started, release, count string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "private-path-canary")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	started, release, count = filepath.Join(root, "started"), filepath.Join(root, "release"), filepath.Join(root, "count")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!%s\nrelease=%q\nprintf x >> %q\nprintf x > %q\nprintf private-stderr-canary >&2\n%s\n", bash, release, count, started, body)
	if err := os.MkdirAll(filepath.Join(s.config.HostProfile, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.config.HostProfile, "bin/workspace-host"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	return
}

func waitCatalogFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("temporary catalog command did not start")
}

func joinCatalogProducer(t *testing.T, s *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.operationWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("temporary catalog producer did not finish")
	}
}

func assertCatalogPrivate(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, "canary") || len(text) > 8000 {
		t.Fatalf("unsafe diagnostics: %s", text)
	}
}

func TestModelsCatalogDiagnosticsBranches(t *testing.T) {
	t.Run("nil_runtime", func(t *testing.T) {
		s, logs := catalogDiagnosticServer(t)
		s.config.Codex = nil
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/models?provider=private-canary", nil))
		if response.Code != 503 || response.Body.String() != "{\"error\":\"Codex model catalog is unavailable\"}\n" {
			t.Fatal(response.Code, response.Body.String())
		}
		if !strings.Contains(logs.String(), "stage=runtime outcome=unavailable") || strings.Contains(logs.String(), "stage=helper") {
			t.Fatal(logs.String())
		}
		assertCatalogPrivate(t, logs.String())
	})
	t.Run("catalog_rejected_without_helper", func(t *testing.T) {
		s, logs := catalogDiagnosticServer(t)
		providerTestCatalog(t, s, "http://private-endpoint-canary")
		_, _, count := catalogCommand(t, s, "printf '[]'")
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/models?provider=private-provider-canary", nil))
		if response.Code != 503 || !strings.Contains(logs.String(), "stage=catalog outcome=resolution") {
			t.Fatal(response.Code, logs.String())
		}
		if _, err := os.Stat(count); !os.IsNotExist(err) || strings.Contains(logs.String(), "stage=helper") {
			t.Fatal("unexpected helper invocation")
		}
		assertCatalogPrivate(t, logs.String())
	})
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared_native_failure_%t", failure), func(t *testing.T) {
			s, logs := catalogDiagnosticServer(t)
			if failure {
				s.config.Codex = &catalogNativeStub{err: errors.New("private-error-canary")}
			}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/models", nil))
			if (response.Code == 503) != failure || !strings.Contains(logs.String(), "stage=shared_native") || !strings.Contains(logs.String(), "exit_code=unknown") {
				t.Fatal(response.Code, logs.String())
			}
			if response.Header().Get("X-Portal-Read") == "" || !strings.Contains(response.Header().Get("Server-Timing"), "resolve;dur=") {
				t.Fatal("missing existing read correlation")
			}
			assertCatalogPrivate(t, logs.String())
		})
	}
}

func TestModelsCatalogDiagnosticsActualHelperFailures(t *testing.T) {
	for _, item := range []struct{ name, body, outcome, message, code string }{
		{"exit", "exit 29", "exit", "unavailable", "29"},
		{"signal", "kill -TERM $$", "exit", "unavailable", "unknown"},
		{"start", "", "start", "unavailable", "unknown"},
		{"invalid", "printf private-stdout-canary", "invalid", "invalid", "0"},
		{"limit", "trap '' PIPE\nprintf '%1048577s' x\nexit 0", "output_limit", "unavailable", "0"},
	} {
		t.Run(item.name, func(t *testing.T) {
			s, logs := catalogDiagnosticServer(t)
			providerTestCatalog(t, s, "http://private-endpoint-canary")
			catalogCommand(t, s, item.body)
			if item.name == "start" {
				if err := os.Remove(filepath.Join(s.config.HostProfile, "bin/workspace-host")); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/models?provider=pool", nil))
			joinCatalogProducer(t, s)
			want := fmt.Sprintf("{\"error\":\"selected provider model catalog is %s\"}\n", item.message)
			if response.Code != 503 || response.Body.String() != want {
				t.Fatal(response.Code, response.Body.String())
			}
			text := logs.String()
			for _, token := range []string{"stage=helper outcome=" + item.outcome, "exit_code=" + item.code, "correlation=r0000000000000001 producer=r0000000000000001", "stage=wait outcome=" + item.outcome} {
				if !strings.Contains(text, token) {
					t.Fatal("missing", token, text)
				}
			}
			if strings.Count(text, "stage=helper") != 1 {
				t.Fatal(text)
			}
			assertCatalogPrivate(t, text)
		})
	}
}

func TestModelsCatalogDiagnosticsLateSharedCompletion(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure_%t", failure), func(t *testing.T) {
			s, logs := catalogDiagnosticServer(t)
			providerTestCatalog(t, s, "http://private-endpoint-canary")
			// Release is a private finite command-double barrier, not a native service.
			body := "while [ ! -e \"$release\" ]; do sleep 0.005; done\n"
			if failure {
				body += "exit 31"
			} else {
				body += "printf '%s' '[{\"model\":\"safe-model\"}]'"
			}
			started, release, count := catalogCommand(t, s, body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := httptest.NewRecorder()
			returned := make(chan struct{})
			go func() {
				s.Handler().ServeHTTP(first, httptest.NewRequest("GET", "/api/models?provider=pool", nil).WithContext(ctx))
				close(returned)
			}()
			waitCatalogFile(t, started)
			cancel()
			select {
			case <-returned:
			case <-time.After(2 * time.Second):
				t.Fatal("canceled waiter stayed blocked")
			}
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel2()
			second := httptest.NewRecorder()
			s.Handler().ServeHTTP(second, httptest.NewRequest("GET", "/api/models?provider=pool", nil).WithContext(ctx2))
			if first.Code != 503 || first.Body.String() != "{\"error\":\"context canceled\"}\n" || second.Code != 503 || second.Body.String() != "{\"error\":\"context deadline exceeded\"}\n" {
				t.Fatal(first.Code, first.Body.String(), second.Code, second.Body.String())
			}
			if strings.Contains(logs.String(), "stage=helper") {
				t.Fatal("waiter was mistaken for completion")
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			joinCatalogProducer(t, s)
			text := logs.String()
			for _, token := range []string{"correlation=r0000000000000001 producer=r0000000000000001 stage=wait outcome=canceled", "correlation=r0000000000000002 producer=r0000000000000001 stage=wait outcome=deadline"} {
				if !strings.Contains(text, token) {
					t.Fatal("missing joined waiter", text)
				}
			}
			outcome := "ok"
			if failure {
				outcome = "exit"
			}
			if strings.Count(text, "stage=helper") != 1 || !strings.Contains(text, "correlation=r0000000000000001 producer=r0000000000000001 stage=helper outcome="+outcome) {
				t.Fatal(text)
			}
			calls, err := os.ReadFile(count)
			if err != nil || string(calls) != "x" {
				t.Fatal("not one shared invocation", string(calls), err)
			}
			if !failure {
				third := httptest.NewRecorder()
				s.Handler().ServeHTTP(third, httptest.NewRequest("GET", "/api/models?provider=pool", nil))
				calls, _ = os.ReadFile(count)
				want, err := json.Marshal([]codex.Model{{Model: "safe-model"}})
				if err != nil {
					t.Fatal(err)
				}
				if third.Code != 200 || third.Body.String() != string(want)+"\n" || string(calls) != "x" {
					t.Fatal("cache behavior changed", third.Code, third.Body.String(), string(calls))
				}
			}
			assertCatalogPrivate(t, logs.String())
		})
	}
}
