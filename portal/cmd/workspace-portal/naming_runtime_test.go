package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"

	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
)

func TestNamingRuntimeChildFixture(t *testing.T) {
	mode := os.Getenv("NAMING_RUNTIME_TEST_CHILD")
	if mode == "" {
		return
	}
	switch mode {
	case "hold":
		data, _ := json.Marshal(map[string]any{"args": os.Args, "cwd": mustNamingCwd(), "home": os.Getenv("CODEX_HOME"), "remote": os.Getenv("CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED"), "provider": os.Getenv("NAMING_TEST_PROVIDER"), "pid": os.Getpid()})
		if namingTestPublish(os.Getenv("NAMING_TEST_RECORD"), data) != nil {
			os.Exit(3)
		}
		for {
			time.Sleep(time.Second)
		}
	case "exit":
		os.Exit(0)
	case "descendant":
		child := exec.Command(os.Args[0], "-test.run=^TestNamingRuntimeChildFixture$")
		child.Env = append(os.Environ(), "NAMING_RUNTIME_TEST_CHILD=hold")
		if child.Start() != nil {
			os.Exit(3)
		}
		if namingTestPublish(os.Getenv("NAMING_TEST_PID"), []byte(strconv.Itoa(child.Process.Pid))) != nil {
			os.Exit(3)
		}
		// Let the parent test observe the live group before the leader exits.
		for {
			if _, err := os.Stat(os.Getenv("NAMING_TEST_RELEASE")); err == nil {
				os.Exit(0)
			}
			time.Sleep(time.Millisecond)
		}
	}
	os.Exit(0)
}
func mustNamingCwd() string { cwd, _ := os.Getwd(); return cwd }

// Readers must see complete synthetic evidence, never the create/write window.
// CreateTemp owns a mode-0600 sibling; rename publishes it in the same directory.
func namingTestPublish(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".naming-evidence-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func namingTestWait(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("synthetic child did not publish bounded fixture evidence")
	return nil
}

func TestNamingRuntimeStartupAndLifetime(t *testing.T) {
	for _, mode := range []string{"hold", "bad version", "exit", "missing native"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			runtimeParent := t.TempDir()
			t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
			t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
			t.Setenv("CODEX_HOME", "/unselected/home")
			t.Setenv("CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED", "0")
			t.Setenv("NAMING_TEST_PROVIDER", "synthetic-provider")
			record := filepath.Join(t.TempDir(), "record")
			t.Setenv("NAMING_TEST_RECORD", record)
			count := filepath.Join(t.TempDir(), "count")
			t.Setenv("NAMING_TEST_COUNT", count)
			native := filepath.Join(root, "libexec/codex/libexec/codex/bin/codex")
			catalog := filepath.Join(root, "share/workspace-portal/codex-models.json")
			os.MkdirAll(filepath.Dir(native), 0700)
			os.MkdirAll(filepath.Dir(catalog), 0700)
			if err := os.WriteFile(catalog, []byte("public fixture catalog"), 0444); err != nil {
				t.Fatal(err)
			}
			childMode := mode
			if mode == "bad version" {
				childMode = "hold"
			}
			version := "codex-cli 0.160.0"
			if mode == "bad version" {
				version = "codex-cli 0.160.1"
			}
			// Only a synthetic test executable runs; no Codex/provider is contacted.
			script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '%s\\n' " + strconv.Quote(version) + "; exit 0; fi\nprintf 'started\\n' >> \"$NAMING_TEST_COUNT\"\nexec " + strconv.Quote(os.Args[0]) + " -test.run=^TestNamingRuntimeChildFixture$ -- \"$@\"\n"
			if mode != "missing native" {
				if err := os.WriteFile(native, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("NAMING_RUNTIME_TEST_CHILD", childMode)
			owner := startNamingRuntime(root, filepath.Join(runtimeParent, "portal.sock"), log.New(io.Discard, "", 0))
			if mode == "missing native" {
				if owner != nil {
					t.Fatal("adopted nonexistent native")
				}
				return
			}
			if owner == nil {
				t.Fatal("owner unavailable")
			}
			defer owner.Close()
			if mode == "hold" {
				data := namingTestWait(t, record)
				var evidence struct {
					Args                        []string
					Cwd, Home, Remote, Provider string
				}
				if json.Unmarshal(data, &evidence) != nil {
					t.Fatal("bad evidence")
				}
				args := strings.Join(evidence.Args, "\n")
				if !strings.Contains(args, "model_catalog_json="+strconv.Quote(catalog)) || !strings.Contains(args, "app-server\n--strict-config\n--listen\nunix://"+owner.socket) {
					t.Fatal("startup argv differs", args)
				}
				if evidence.Cwd != filepath.Join(owner.directory, "cwd") || evidence.Home != home || evidence.Remote != "1" || evidence.Provider != "synthetic-provider" {
					t.Fatal("child inherited incorrect trusted environment", evidence)
				}
				if os.Getenv("CODEX_HOME") != "/unselected/home" || os.Getenv("CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED") != "0" {
					t.Fatal("parent environment changed")
				}
				for _, path := range []string{owner.directory, evidence.Cwd} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("private startup placement")
					}
				}
				if entries, _ := os.ReadDir(evidence.Cwd); len(entries) != 0 {
					t.Fatal("startup cwd is not empty")
				}
				if _, err := os.Stat(owner.socket); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("synthetic not-listening fixture unexpectedly listened")
				}
			} else {
				select {
				case <-owner.done:
				case <-time.After(time.Second):
					t.Fatal("failed startup did not stop")
				}
			}
			owner.Close()
			owner.Close()
			if _, err := os.Stat(owner.directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("owned lifetime directory retained", err)
			}
			if mode == "bad version" {
				if _, err := os.Stat(count); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("wrong-version child started")
				}
			} else {
				data, err := os.ReadFile(count)
				if err != nil || string(data) != "started\n" {
					t.Fatal("owner respawned or failed to start exact one child")
				}
			}
		})
	}
}

func TestNamingRuntimeLeaderExitKillsResidualGroup(t *testing.T) {
	root := t.TempDir()
	pidPath := filepath.Join(root, "pid")
	release := filepath.Join(root, "release")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.Command(os.Args[0], "-test.run=^TestNamingRuntimeChildFixture$")
	command.Env = append(os.Environ(), "NAMING_RUNTIME_TEST_CHILD=descendant", "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0", "NAMING_TEST_PID="+pidPath, "NAMING_TEST_RELEASE="+release, "NAMING_TEST_RECORD="+filepath.Join(root, "record"))
	done := make(chan error, 1)
	go func() { done <- runNamingChild(ctx, command) }()
	pid, err := strconv.Atoi(string(namingTestWait(t, pidPath)))
	if err != nil {
		t.Fatal(err)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("synthetic descendant never alive")
	}
	os.WriteFile(release, nil, 0600)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("early exit treated as available")
		}
	case <-time.After(time.Second):
		t.Fatal("leader waiter stuck")
	}
	// Linux reports zombies to kill(0), so use the proc state to distinguish a
	// killed, awaiting-reaper child from a live residual process.
	deadline := time.Now().Add(time.Second)
	for {
		state, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) || err == nil && strings.Contains(string(state), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("residual owned descendant still alive")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNamingRuntimeMissingSelection(t *testing.T) {
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", "")
	if owner := startNamingRuntime(t.TempDir(), filepath.Join(t.TempDir(), "portal.sock"), log.New(io.Discard, "", 0)); owner != nil {
		owner.Close()
		t.Fatal("missing package/home acquired child")
	}
}

func TestNamingRuntimeConstructorFailureClosesOwnedLifetime(t *testing.T) {
	root := t.TempDir()
	runtimeParent := t.TempDir()
	home := t.TempDir()
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	native := filepath.Join(root, "libexec/codex/libexec/codex/bin/codex")
	catalog := filepath.Join(root, "share/workspace-portal/codex-models.json")
	os.MkdirAll(filepath.Dir(native), 0700)
	os.MkdirAll(filepath.Dir(catalog), 0700)
	if err := os.WriteFile(native, []byte("#!/bin/sh\nprintf '%s\\n' 'codex-cli 0.160.0'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, []byte("synthetic"), 0444); err != nil {
		t.Fatal(err)
	}
	err := serve([]string{"--unix-socket", filepath.Join(runtimeParent, "portal.sock"), "--package-root", root, "--user-state-root", t.TempDir(), "--workspace-name", "fixture", "--registration-marker", filepath.Join(runtimeParent, "registration"), "--workspace", t.TempDir(), "--base-url", "invalid"})
	if err == nil {
		t.Fatal("invalid constructor unexpectedly served")
	}
	entries, err := os.ReadDir(runtimeParent)
	if err != nil || len(entries) != 0 {
		t.Fatal("constructor failure retained owned lifetime state", err)
	}
}

func TestNamingRuntimeHTTPExitKeepsChildForWorkerCleanup(t *testing.T) {
	for _, mode := range []string{"signal context", "HTTP listener failure"} {
		t.Run(mode, func(t *testing.T) {
			childCtx, stopChild := context.WithCancel(context.Background())
			defer stopChild()
			record := filepath.Join(t.TempDir(), "child")
			child := exec.Command(os.Args[0], "-test.run=^TestNamingRuntimeChildFixture$")
			child.Env = append(os.Environ(), "NAMING_RUNTIME_TEST_CHILD=hold", "NAMING_TEST_RECORD="+record, "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
			childDone := make(chan struct{})
			go func() { _ = runNamingChild(childCtx, child); close(childDone) }()
			defer func() { stopChild(); <-childDone }()
			var evidence struct{ PID int }
			if json.Unmarshal(namingTestWait(t, record), &evidence) != nil || evidence.PID == 0 {
				t.Fatal("missing live child")
			}
			profile := filepath.Join(t.TempDir(), "profile")
			if err := os.Symlink(t.TempDir(), profile); err != nil {
				t.Fatal(err)
			}
			entered, cleanup := make(chan struct{}), make(chan bool, 1)
			application, err := portalweb.New(portalweb.Config{
				Workspace: t.TempDir(), UserStateRoot: t.TempDir(), BaseURL: "https://workspace.example.test", DevSession: "/missing/dev-session", HostProfile: profile, Logger: log.New(io.Discard, "", 0),
				SessionNamer: func(ctx context.Context, _ string) (string, error) {
					close(entered)
					<-ctx.Done()
					cleanup <- syscall.Kill(evidence.PID, 0) == nil
					return "", ctx.Err()
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			values := url.Values{"clientRequestId": {"00000000-0000-4000-8000-000000000001"}, "goal": {"Synthetic naming request"}, "creation_date": {"2026-10-03"}, "name": {""}}
			request := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(values.Encode()))
			request.Header.Set("Origin", "https://workspace.example.test")
			request.Header.Set("Accept", "application/json")
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			application.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusAccepted {
				t.Fatal("synthetic admission failed", response.Code, response.Body.String())
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("worker did not enter injected boundary")
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "signal context" {
				cancel()
			} else {
				listener.Close()
			}
			err = servePortalHTTP(ctx, application, listener, log.New(io.Discard, "", 0), "https://workspace.example.test")
			if mode == "signal context" && err != nil {
				t.Fatal("graceful HTTP stop failed", err)
			}
			if mode == "HTTP listener failure" && err == nil {
				t.Fatal("closed listener error lost")
			}
			select {
			case alive := <-cleanup:
				if !alive {
					t.Fatal("owned child stopped before worker cleanup")
				}
			default:
				t.Fatal("HTTP exit skipped application cleanup")
			}
			// Application cleanup is complete before the owner's final cancel/join.
			stopChild()
			select {
			case <-childDone:
			case <-time.After(time.Second):
				t.Fatal("owned child did not stop")
			}
		})
	}
}
