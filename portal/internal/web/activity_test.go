package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type testActivityObserver struct {
	verified chan string
	read     chan string
	stopped  chan string
	wrong    atomic.Bool
	events   chan struct{}
}

func (o *testActivityObserver) VerifyThread(_ context.Context, id, cwd string) error {
	select {
	case o.verified <- cwd:
	default:
	}
	if o.wrong.Load() {
		return errors.New("wrong thread directory")
	}
	return nil
}
func (o *testActivityObserver) ReadActivity(_ context.Context, id string) (codex.ActivitySnapshot, error) {
	select {
	case o.read <- id:
	default:
	}
	return codex.ActivitySnapshot{ThreadID: id}, nil
}
func (o *testActivityObserver) Subscribe(_ context.Context, id string) (<-chan struct{}, func(), error) {
	events := make(chan struct{})
	if o.events != nil {
		events = o.events
	}
	var once sync.Once
	return events, func() { once.Do(func() { close(events); o.stopped <- id }) }, nil
}
func (o *testActivityObserver) Close() {}

func TestActivityObservationContinuesWithoutBrowserAndStopsOnRetirement(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	directory := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\n  socket_path: " + s.config.CodexSocket + "\n  client_version: 0.152.1\ncreation:\n  state: ready\n"
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	writeWebRuntimeAuthority(t, s, "example")
	observer := &testActivityObserver{verified: make(chan string, 8), read: make(chan string, 8), stopped: make(chan string, 8)}
	monitor := &activityMonitor{server: s, observer: observer, workers: make(map[string]activityWorker)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitor.reconcile(ctx)
	select {
	case id := <-observer.read:
		if id != "thread-1" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not observe without a browser")
	}
	if cwd := <-observer.verified; cwd != directory {
		t.Fatalf("verified %s", cwd)
	}
	if err := os.Remove(filepath.Join(s.config.AuthorityDir, "example.json")); err != nil {
		t.Fatal(err)
	}
	monitor.reconcile(ctx)
	select {
	case <-observer.stopped:
	case <-time.After(time.Second):
		t.Fatal("retired session kept its observation")
	}
	monitor.wg.Wait()
	if len(monitor.workers) != 0 {
		t.Fatal("retired worker retained")
	}
}

func TestActivityObservationRequiresMatchingReadyAuthority(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	writeWebRuntimeAuthority(t, s, "example")
	monitor := &activityMonitor{server: s}
	summary := session.Summary{Manifest: session.Manifest{Slug: "example", Codex: session.Codex{ThreadID: "thread-1", SocketPath: s.config.CodexSocket}}}
	summary.Creation.State = "ready"
	if !monitor.owns(summary) {
		t.Fatal("ready matching authority not observed")
	}
	cases := []session.Summary{summary, summary, summary, summary}
	cases[0].Archived = true
	cases[1].Codex.SocketPath = "/run/another-workspace/app-server.sock"
	cases[2].Codex.ThreadID = "another-thread"
	cases[3].Creation.State = "creating"
	for _, candidate := range cases {
		if monitor.owns(candidate) {
			t.Fatalf("unowned session observed: %#v", candidate)
		}
	}
}

func TestActivityObservationRejectsThreadDirectoryMismatch(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	observer := &testActivityObserver{verified: make(chan string, 1), read: make(chan string, 1), stopped: make(chan string, 1)}
	observer.wrong.Store(true)
	monitor := &activityMonitor{server: s, observer: observer}
	ctx, cancel := context.WithCancel(context.Background())
	monitor.wg.Add(1)
	go monitor.observe(ctx, "example", "wrong-thread")
	select {
	case <-observer.verified:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("thread identity was not checked")
	}
	cancel()
	monitor.wg.Wait()
	select {
	case <-observer.read:
		t.Fatal("read activity with the wrong working directory")
	default:
	}
	select {
	case <-observer.stopped:
		t.Fatal("subscribed to the wrong thread")
	default:
	}
}

func TestActivityObservationReleasesWatchAfterIdentityCheckFails(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	observer := &testActivityObserver{read: make(chan string, 2), stopped: make(chan string, 2), events: make(chan struct{}, 1)}
	monitor := &activityMonitor{server: s, observer: observer}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); monitor.wg.Wait() }()
	monitor.wg.Add(1)
	go monitor.observe(ctx, "example", "thread-1")
	select {
	case <-observer.read:
	case <-time.After(time.Second):
		t.Fatal("initial observation did not start")
	}
	observer.wrong.Store(true)
	observer.events <- struct{}{}
	select {
	case <-observer.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("failed identity check retained the previous subscription")
	}
	select {
	case <-observer.read:
		t.Fatal("read activity after identity validation failed")
	default:
	}
}

type limitedActivityObserver struct {
	testActivityObserver
	started chan string
	advance chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
	checks  atomic.Int32
}

func (o *limitedActivityObserver) VerifyThread(context.Context, string, string) error {
	o.checks.Add(1)
	return nil
}

func (o *limitedActivityObserver) ReadActivity(ctx context.Context, thread string) (codex.ActivitySnapshot, error) {
	active := o.active.Add(1)
	defer o.active.Add(-1)
	for peak := o.peak.Load(); active > peak; peak = o.peak.Load() {
		if o.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	o.started <- thread
	select {
	case <-o.advance:
		return codex.ActivitySnapshot{ThreadID: thread, CurrentState: "idle"}, nil
	case <-ctx.Done():
		return codex.ActivitySnapshot{}, ctx.Err()
	}
}

func TestActivityMonitorBoundsStartupAndBrowserReads(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	const workers = 40
	observer := &limitedActivityObserver{
		testActivityObserver: testActivityObserver{stopped: make(chan string, workers)},
		started:              make(chan string, workers), advance: make(chan struct{}, workers),
	}
	monitor := &activityMonitor{server: s, observer: observer}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); monitor.wg.Wait() }()
	for i := 0; i < workers; i++ {
		monitor.wg.Add(1)
		go monitor.observe(ctx, fmt.Sprintf("session-%d", i), fmt.Sprintf("thread-%d", i))
	}
	seen := make(map[string]bool)
	for i := 0; i < activityReadConcurrency; i++ {
		select {
		case id := <-observer.started:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("startup read did not acquire a slot")
		}
	}
	// A queued browser read can cancel promptly without sending another RPC.
	readContext, stopRead := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopRead()
	if _, err := monitor.ReadActivity(readContext, "browser-thread"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued read: %v", err)
	}
	if count := observer.checks.Load(); count != activityReadConcurrency {
		t.Fatalf("unbounded startup verification: %d", count)
	}
	for i := 0; i < workers; i++ {
		observer.advance <- struct{}{}
	}
	for len(seen) < workers {
		select {
		case id := <-observer.started:
			if seen[id] {
				t.Fatalf("one old history monopolized the read slots: %s", id)
			}
			seen[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("queued sessions did not all make progress")
		}
	}
	if peak := observer.peak.Load(); peak > activityReadConcurrency {
		t.Fatalf("concurrent history reads: %d", peak)
	}
}

func TestActivityMonitorDuplicateThreadReadsLeaveOtherWorkersAvailable(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	observer := &limitedActivityObserver{
		testActivityObserver: testActivityObserver{stopped: make(chan string, 1)},
		started:              make(chan string, 64), advance: make(chan struct{}),
	}
	monitor := &activityMonitor{server: s, observer: observer}
	ctx, cancel := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	defer func() { cancel(); readers.Wait(); monitor.wg.Wait() }()
	const duplicates = 40
	for i := 0; i < duplicates; i++ {
		readers.Add(1)
		go func() { defer readers.Done(); _, _ = monitor.ReadActivity(ctx, "old-thread") }()
	}
	deadline := time.Now().Add(time.Second)
	for {
		monitor.readMu.Lock()
		queued := monitor.readGates["old-thread"] != nil && monitor.readGates["old-thread"].users == duplicates
		monitor.readMu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("duplicate reads did not enter the per-thread queue")
		}
		time.Sleep(time.Millisecond)
	}
	if id := <-observer.started; id != "old-thread" {
		t.Fatal(id)
	}
	monitor.wg.Add(1)
	go monitor.observe(ctx, "unrelated-session", "unrelated-thread")
	select {
	case id := <-observer.started:
		if id != "unrelated-thread" {
			t.Fatalf("duplicate consumed another authority slot: %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate browser reads blocked an unrelated worker")
	}
	queuedContext, stopRead := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopRead()
	if _, err := monitor.ReadActivity(queuedContext, "old-thread"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-thread queue cancellation: %v", err)
	}
	cancel()
	readers.Wait()
	monitor.wg.Wait()
	monitor.readMu.Lock()
	defer monitor.readMu.Unlock()
	if len(monitor.readGates) != 0 || len(monitor.readSlots) != 0 {
		t.Fatalf("completed reads retained admission state: gates=%d slots=%d", len(monitor.readGates), len(monitor.readSlots))
	}
}
