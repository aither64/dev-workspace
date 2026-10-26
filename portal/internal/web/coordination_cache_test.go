package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitCoordination(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("coordination read did not reach expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func coordinationWaiters(c *coordinationCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, flight := range c.flights {
		count += flight.waiters
	}
	return count
}

func coordinationClock(c *coordinationCache) *atomic.Int64 {
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	return clock
}

func installCoordinationHelper(t *testing.T, s *Server, script string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "bin", "workspace-host")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.config.PackageRoot = root
	s.config.DevSession = helper
	return helper
}

func readCoordinationResponse(s *Server, slug string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	s.coordinationAPI(response, httptest.NewRequest(http.MethodGet, "/api/coordination", nil), slug)
	return response
}

func TestCoordinationCacheCoalescesHTTPReadsAndExpiresAfterCompletion(t *testing.T) {
	s := newTestServer(t)
	clock := coordinationClock(s.coordination)
	root := t.TempDir()
	calls, release := filepath.Join(root, "calls"), filepath.Join(root, "release")
	installCoordinationHelper(t, s, fmt.Sprintf("echo call >> %q\nwhile [ ! -f %q ]; do sleep 0.01; done\nprintf ' {\"schema\":1}\\n'", calls, release))
	responses := make(chan *httptest.ResponseRecorder, 20)
	for range 20 {
		go func() { responses <- readCoordinationResponse(s, "") }()
	}
	waitCoordination(t, func() bool { return coordinationWaiters(s.coordination) == 20 })
	// Time spent producing the snapshot must not consume its retention time.
	clock.Add(int64(10 * time.Second))
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	check := func(response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != 200 || response.Body.String() != " {\"schema\":1}\n" ||
			response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response changed: %d %v %s", response.Code, response.Header(), response.Body.String())
		}
	}
	for range 20 {
		check(<-responses)
	}
	count := func() int {
		t.Helper()
		data, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "call\n")
	}
	if count() != 1 {
		t.Fatal("concurrent requests did not share one helper")
	}
	clock.Add(int64(coordinationTTL - time.Nanosecond))
	for range 20 {
		check(readCoordinationResponse(s, ""))
	}
	if count() != 1 {
		t.Fatal("cache hits launched a helper")
	}
	clock.Add(1)
	check(readCoordinationResponse(s, ""))
	if count() != 2 {
		t.Fatal("expiration did not launch exactly one refresh")
	}
}

func TestCoordinationCacheSeparatesIdentity(t *testing.T) {
	c := newCoordinationCache(context.Background())
	defer c.close()
	keys := []coordinationKey{
		{workspace: "/a", name: "a"},
		{workspace: "/b", name: "a"},
		{workspace: "/a", name: "b"},
		{workspace: "/a", name: "a", profile: hostProfileIdentity{inode: 2}},
		{workspace: "/a", name: "a", slug: "one", thread: "root", incarnation: "first"},
		{workspace: "/a", name: "a", slug: "two", thread: "root", incarnation: "first"},
		{workspace: "/a", name: "a", slug: "one", thread: "other", incarnation: "first"},
		{workspace: "/a", name: "a", slug: "one", thread: "root", incarnation: "replacement"},
	}
	var calls int
	load := func(context.Context) ([]byte, error) { calls++; return []byte(`{}`), nil }
	for range 2 {
		for _, key := range keys {
			if _, err := c.read(context.Background(), key, load, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls != len(keys) {
		t.Fatalf("keys shared data or missed hits: %d helpers", calls)
	}
}

func writeCoordinationSession(t *testing.T, s *Server, thread string) string {
	t.Helper()
	directory := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\n"
	if thread != "" {
		manifest += "codex:\n  thread_id: " + thread + "\n"
	}
	for path, contents := range map[string]string{"portal.yml": manifest, "plan.md": "# Plan\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(directory, path), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func TestCoordinationCacheRechecksCurrentIdentity(t *testing.T) {
	s := newTestServer(t)
	installCoordinationHelper(t, s, "printf '{}'")
	if response := readCoordinationResponse(s, ""); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	directory := writeCoordinationSession(t, s, "")
	if response := readCoordinationResponse(s, "example"); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	key, err := s.coordinationIdentity("example")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, filepath.Join(t.TempDir(), "original")); err != nil {
		t.Fatal(err)
	}
	writeCoordinationSession(t, s, "")
	replacement, err := s.coordinationIdentity("example")
	if err != nil || replacement == key {
		t.Fatal("replacement session reused cached identity", err)
	}
	if err := os.Remove(s.config.DevSession); err != nil {
		t.Fatal(err)
	}
	if response := readCoordinationResponse(s, "example"); response.Code != 503 {
		t.Fatalf("replacement received old success: %d %s", response.Code, response.Body.String())
	}
	if err := os.Remove(s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if response := readCoordinationResponse(s, ""); response.Code != 503 {
		t.Fatal("superseded profile served a cached response")
	}
}

func TestCoordinationCacheDoesNotPublishChangedIdentity(t *testing.T) {
	s := newTestServer(t)
	root := t.TempDir()
	release := filepath.Join(root, "release")
	installCoordinationHelper(t, s, fmt.Sprintf("while [ ! -f %q ]; do sleep 0.01; done\nprintf '{}'", release))
	directory := writeCoordinationSession(t, s, "")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- readCoordinationResponse(s, "example") }()
	waitCoordination(t, func() bool { return coordinationWaiters(s.coordination) == 1 })
	if err := os.Rename(directory, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	writeCoordinationSession(t, s, "")
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if response := <-done; response.Code != 503 {
		t.Fatal("flight served a replaced session")
	}
	s.coordination.mu.Lock()
	defer s.coordination.mu.Unlock()
	if len(s.coordination.entries) != 0 {
		t.Fatal("identity-changing flight published a snapshot")
	}
}

func TestCoordinationCacheFailureNeverServesExpiredSuccess(t *testing.T) {
	for _, failure := range []struct{ script, message string }{
		{"printf '{'", "Operations returned an invalid response."},
		{"echo unavailable >&2; exit 1", "Operations cannot be read: unavailable"},
	} {
		t.Run(failure.message, func(t *testing.T) {
			s := newTestServer(t)
			clock := coordinationClock(s.coordination)
			helper := installCoordinationHelper(t, s, "printf '{}'")
			if response := readCoordinationResponse(s, ""); response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			clock.Add(int64(coordinationTTL))
			if err := os.WriteFile(helper, []byte("#!/bin/sh\n"+failure.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				response := readCoordinationResponse(s, "")
				if response.Code != 503 || !strings.Contains(response.Body.String(), failure.message) {
					t.Fatalf("bad failure: %d %s", response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestCoordinationCacheInvalidationFencesFlight(t *testing.T) {
	c := newCoordinationCache(context.Background())
	defer c.close()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) ([]byte, error) {
		calls.Add(1)
		close(started)
		<-release // Deliberately completes successfully after cancellation.
		return []byte(`{"old":true}`), nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.read(context.Background(), coordinationKey{}, load, func() error { return nil })
		done <- err
	}()
	<-started
	c.invalidate()
	// An invalidated helper remains owned until it exits; no duplicate starts.
	second := make(chan error, 1)
	go func() {
		_, err := c.read(context.Background(), coordinationKey{}, load, func() error { return nil })
		second <- err
	}()
	waitCoordination(t, func() bool { return coordinationWaiters(c) == 2 })
	close(release)
	if <-done == nil || <-second == nil || calls.Load() != 1 {
		t.Fatal("invalidated flight served success or launched duplicate")
	}
	c.mu.Lock()
	entries := len(c.entries)
	c.mu.Unlock()
	if entries != 0 {
		t.Fatal("invalidated flight repopulated cache")
	}
	if _, err := c.read(context.Background(), coordinationKey{}, func(context.Context) ([]byte, error) { return []byte(`{}`), nil }, func() error { return nil }); err != nil {
		t.Fatal("fresh read could not replace invalidated flight", err)
	}
}

func TestCoordinationCacheWaitersCancelIndependently(t *testing.T) {
	c := newCoordinationCache(context.Background())
	defer c.close()
	first, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	child := make(chan context.Context, 1)
	load := func(ctx context.Context) ([]byte, error) { child <- ctx; <-release; return []byte(`{}`), nil }
	result := make(chan error, 2)
	go func() { _, err := c.read(first, coordinationKey{}, load, func() error { return nil }); result <- err }()
	helperContext := <-child
	if deadline, ok := helperContext.Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > coordinationTimeout {
		t.Fatal("shared helper lost its independent five-second deadline")
	}
	go func() {
		_, err := c.read(context.Background(), coordinationKey{}, load, func() error { return nil })
		result <- err
	}()
	waitCoordination(t, func() bool { return coordinationWaiters(c) == 2 })
	cancel()
	if !errors.Is(<-result, context.Canceled) || helperContext.Err() != nil {
		t.Fatal("first waiter canceled another waiter's helper")
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal("remaining waiter lost shared success", err)
	}
}

func TestCoordinationCacheLastWaiterAndShutdownCancelOwnedHelper(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint("shutdown=", shutdown), func(t *testing.T) {
			s := newTestServer(t)
			pidFile := filepath.Join(t.TempDir(), "child")
			installCoordinationHelper(t, s, fmt.Sprintf("sleep 1000 &\necho $! > %q\nwait", pidFile))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				s.coordinationAPI(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/coordination", nil).WithContext(ctx), "")
				close(done)
			}()
			var pid int
			waitCoordination(t, func() bool {
				data, err := os.ReadFile(pidFile)
				if err == nil {
					_, err = fmt.Sscan(string(data), &pid)
				}
				return err == nil && pid > 0
			})
			if shutdown {
				s.Close()
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("helper cancellation did not finish waiter")
			}
			waitCoordination(t, func() bool {
				data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				return errors.Is(err, os.ErrNotExist) || err == nil && strings.Contains(string(data), ") Z ")
			})
			if shutdown {
				s.coordination.mu.Lock()
				remaining := len(s.coordination.flights)
				s.coordination.mu.Unlock()
				if remaining != 0 {
					t.Fatal("shutdown returned before helper completion")
				}
			}
		})
	}
}

func TestCoordinationCacheCapacityAndLRU(t *testing.T) {
	for _, payloadSize := range []int{2, 1024 * 1024} {
		t.Run(fmt.Sprint(payloadSize), func(t *testing.T) {
			c := newCoordinationCache(context.Background())
			defer c.close()
			clock := coordinationClock(c)
			payload := []byte(`"` + strings.Repeat("x", payloadSize-2) + `"`)
			load := func(context.Context) ([]byte, error) { return payload, nil }
			read := func(slug string) {
				t.Helper()
				if _, err := c.read(context.Background(), coordinationKey{slug: slug}, load, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			limit := min(coordinationMaxEntries, coordinationMaxBytes/payloadSize)
			for i := range limit {
				read(fmt.Sprint(i))
			}
			read("0") // Preserve the most recently used entry across capacity eviction.
			read("overflow")
			c.mu.Lock()
			_, retained := c.entries[coordinationKey{slug: "0"}]
			_, evicted := c.entries[coordinationKey{slug: "1"}]
			valid := retained && !evicted && len(c.entries) == limit && c.bytes <= coordinationMaxBytes
			c.mu.Unlock()
			if !valid {
				t.Fatal("snapshot capacity/LRU violated")
			}
			clock.Add(int64(coordinationTTL))
			read("fresh")
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.entries) != 1 || c.bytes != payloadSize {
				t.Fatal("expired payloads retained capacity")
			}
		})
	}
}

func TestCoordinationCacheLifecycleSuccessInvalidates(t *testing.T) {
	for _, supervised := range []bool{false, true} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("supervised=%t/success=%t", supervised, success), func(t *testing.T) {
				s := newTestServer(t)
				summary := browserTargetFixture(t, s, "work", "active", "")
				target, err := lifecycleTargetIdentity(summary)
				if err != nil {
					t.Fatal(err)
				}
				helper := "exit 1"
				if success {
					helper = "exit 0"
				}
				installCoordinationHelper(t, s, helper)
				if supervised {
					s.config.WorkspaceName = "fixture"
					directory := t.TempDir()
					for name, script := range map[string]string{
						"systemd-run": "exit 0",
						"systemctl":   "printf 'LoadState=loaded\\nActiveState=active\\nSubState=exited\\nExecMainStatus=0\\n'",
						"journalctl":  "exit 0",
					} {
						if name == "systemd-run" && !success {
							script = "exit 1"
						}
						if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
					s.operationMu.Lock()
					s.operations["example"] = lifecycleOperation{Slug: "example", Kind: "archive", State: "running", ReceiptID: strings.Repeat("a", 64), Attempt: 1}
					s.operationMu.Unlock()
				}
				if _, err := s.coordination.read(context.Background(), coordinationKey{}, func(context.Context) ([]byte, error) { return []byte(`{}`), nil }, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				err = s.executeLifecycleOperation(context.Background(), time.Second, "example", "archive", nil, lifecycleOperationOptions{TargetID: target})
				if (err == nil) != success {
					t.Fatal("unexpected lifecycle outcome", err)
				}
				s.coordination.mu.Lock()
				defer s.coordination.mu.Unlock()
				if (len(s.coordination.entries) == 0) != success || (s.coordination.epoch == 1) != success {
					t.Fatal("invalidation did not follow successful lifecycle execution")
				}
			})
		}
	}
}

func TestCoordinationCacheBoundsDistinctFlights(t *testing.T) {
	c := newCoordinationCache(context.Background())
	defer c.close()
	release := make(chan struct{})
	load := func(context.Context) ([]byte, error) { <-release; return []byte(`{}`), nil }
	done := make(chan error, coordinationMaxFlights+1)
	for i := range coordinationMaxFlights {
		go func() {
			_, err := c.read(context.Background(), coordinationKey{slug: fmt.Sprint(i)}, load, func() error { return nil })
			done <- err
		}()
	}
	waitCoordination(t, func() bool { return coordinationWaiters(c) == coordinationMaxFlights })
	if _, err := c.read(context.Background(), coordinationKey{slug: "overflow"}, load, func() error { return nil }); !errors.Is(err, errCoordinationUnavailable) {
		t.Fatal("distinct flight overflow was not refused")
	}
	go func() {
		_, err := c.read(context.Background(), coordinationKey{slug: "0"}, load, func() error { return nil })
		done <- err
	}()
	waitCoordination(t, func() bool { return coordinationWaiters(c) == coordinationMaxFlights+1 })
	close(release)
	for range coordinationMaxFlights + 1 {
		if err := <-done; err != nil {
			t.Fatal("flight bound broke an existing key", err)
		}
	}
}
