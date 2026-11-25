package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func TestProviderForkActivityUsesDisjointImmutableSegments(t *testing.T) {
	original := codex.ActivitySnapshot{ThreadID: "origin", Scope: "thread", SentMessages: 3, ReceivedMessages: 4, TotalToolCalls: 5, WorkingMS: 11, WaitingMS: 13, BetweenTurnsMS: 17, IdleMS: 900, OpenWaitingMS: 800, CurrentTurnID: "old-turn", CountsComplete: true, CoverageComplete: true, CoverageStartMS: 10}
	b := codex.ActivitySnapshot{ThreadID: "b-child", Scope: "sinceFork", Messages: 2, ToolCalls: 3, SentMessages: 2, ReceivedMessages: 1, TotalToolCalls: 7, WorkingMS: 19, WaitingMS: 23, BetweenTurnsMS: 29, IdleMS: 5, OpenWaitingMS: 6, CurrentTurnID: "b-turn", CurrentState: "waiting", ObservedAtMS: 100, StateSinceMS: 90, CountsComplete: true, CoverageComplete: true, CoverageStartMS: 50}
	logicalB, err := composeProviderForkActivity(&original, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if logicalB.SentMessages != 5 || logicalB.WorkingMS != 30 || logicalB.WaitingMS != 36 || logicalB.BetweenTurnsMS != 46 || logicalB.IdleMS != 22 || logicalB.OpenWaitingMS != 6 || logicalB.CurrentTurnID != "b-turn" || logicalB.Messages != 2 || logicalB.ToolCalls != 3 || logicalB.CoverageComplete || logicalB.CoverageStartMS != 10 {
		t.Fatal("ancestor live/latest timing was added", logicalB)
	}
	a := codex.ActivitySnapshot{ThreadID: "a-child", Scope: "sinceFork", SentMessages: 1, ReceivedMessages: 2, TotalToolCalls: 3, WorkingMS: 4, WaitingMS: 5, BetweenTurnsMS: 6, CountsComplete: true, CurrentTurnID: "new-a-turn", OpenWaitingMS: 7, ObservedAtMS: 200}
	for i := 0; i < 5; i++ {
		logicalA, err := composeProviderForkActivity(&logicalB, a, false)
		if err != nil {
			t.Fatal(err)
		}
		if logicalA.SentMessages != 6 || logicalA.WorkingMS != 34 || logicalA.WaitingMS != 41 || logicalA.OpenWaitingMS != 7 || logicalA.CurrentTurnID != "new-a-turn" || logicalA.ObservedAtMS != 200 {
			t.Fatal("refresh/restart accumulated inherited history", logicalA)
		}
	}
	if original.WorkingMS != 11 || b.WorkingMS != 19 || logicalB.WorkingMS != 30 {
		t.Fatal("frozen predecessor or raw segment mutated")
	}
	for _, fault := range []string{"unknown-child", "unknown-parent", "negative", "overflow-count", "overflow-time"} {
		t.Run(fault, func(t *testing.T) {
			parent, child := original, b
			switch fault {
			case "unknown-child":
				child.Scope = "unknown"
			case "unknown-parent":
				parent.Scope = "unknown"
			case "negative":
				parent.WorkingMS = -1
			case "overflow-count":
				parent.SentMessages = int(^uint(0) >> 1)
			case "overflow-time":
				parent.WaitingMS = int64(^uint64(0) >> 1)
			}
			if _, err := composeProviderForkActivity(&parent, child, false); err == nil {
				t.Fatal("unproved/overflowed activity was composed")
			}
		})
	}
	missing, err := composeProviderForkActivity(nil, b, false)
	if err != nil || missing.CountsComplete || missing.CoverageComplete {
		t.Fatal("missing observation became zero history", missing, err)
	}
}

type testActivityObserver struct {
	verified chan string
	read     chan string
	stopped  chan string
	wrong    atomic.Bool
	events   chan struct{}
}

type budgetActivityObserver struct {
	*testActivityObserver
	state string
	fail  bool
}

type fixedActivityObserver struct {
	*testActivityObserver
	snapshot codex.ActivitySnapshot
}

func (o *fixedActivityObserver) ReadActivity(context.Context, string) (codex.ActivitySnapshot, error) {
	return o.snapshot, nil
}

func TestProviderForkActivityFrozenBaselineSurvivesRefreshAndRestart(t *testing.T) {
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "bound")
	socket := filepath.Join(t.TempDir(), "registered", "app-server.sock")
	s.config.CodexSocket = socket
	s.config.AuthorityDir = filepath.Join(filepath.Dir(socket), "authority")
	op, inc := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", strings.Repeat("a", 64)
	key := codexproviders.BindingKey{Workspace: s.config.Workspace, Slug: "bound", RootThreadID: "origin", Incarnation: inc, Member: "lead", OriginalThreadID: "origin"}
	baseline := codex.ActivitySnapshot{ThreadID: "origin", Scope: "thread", SentMessages: 3, WorkingMS: 11, CountsComplete: true, CoverageComplete: true}
	source := map[string]any{"operationId": op, "root": s.config.Workspace, "socket": socket, "participants": []any{map[string]any{"current": codexproviders.CurrentBinding{Key: key}, "thread": map[string]any{"id": "origin"}, "browserRecords": ProviderForkBrowserRecords{ActivityFinal: &baseline}, "history": map[string]any{"turns": []any{}, "items": []any{}}}}}
	sourceBytes, _ := json.Marshal(source)
	hash := func(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
	verifiedBytes, _ := json.Marshal(map[string]any{"operationId": op, "sourceSnapshotDigest": hash(sourceBytes)})
	seal := codexproviders.BindingEvidence{Schema: 1, OperationID: op, Workspace: "registered", Root: s.config.Workspace, Socket: socket, PreviousBindingRevision: 1, CommittedBindingRevision: 2, SnapshotDigest: hash(verifiedBytes), Participants: []codexproviders.BindingParticipant{{Key: key, OldThreadID: "origin", NewThreadID: "child", OldProvider: "openai", NewProvider: "openai"}}}
	sealBytes, _ := json.Marshal(seal)
	state := codexproviders.State{Schema: 3, Workspace: "registered", Root: s.config.Workspace, Socket: socket, Active: "openai", BindingRevision: 2, Bindings: map[string]codexproviders.SessionBinding{"bound": {RootThreadID: "origin", Incarnation: inc, Revision: 2, Conversations: map[string]codexproviders.ConversationBinding{"origin": {Member: "lead", ThreadID: "child", Provider: "openai", OperationID: op, EvidenceDigest: hash(sealBytes)}}}}}
	authority := session.RuntimeAuthority{Schema: 3, State: "ready", Slug: "bound", Workspace: s.config.Workspace, TmuxSocket: "/run/tmux.sock", TmuxSessionID: "$1", TmuxIdentity: inc, CodexThreadID: "origin", CodexActiveThreadID: "child", CodexBindingRevision: 2, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
	statePath, err := codexproviders.StatePath(s.config.UserStateRoot, "registered")
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalogPath)
	manifest := fmt.Sprintf("schema: 6\nslug: bound\ncodex:\n  thread_id: origin\n  socket_path: %s\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\nrepositories: []\n", socket)
	stateBytes, _ := json.Marshal(state)
	authorityBytes, _ := json.Marshal(authority)
	basePath := filepath.Join(filepath.Dir(statePath), op+".snapshot.json")
	for path, data := range map[string][]byte{statePath: stateBytes, filepath.Join(s.config.AuthorityDir, "bound.json"): authorityBytes, basePath: sourceBytes, basePath + ".verified": verifiedBytes, basePath + ".evidence": sealBytes, catalogPath: []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"}]}`), filepath.Join(s.config.Workspace, "work", "bound", "portal.yml"): []byte(manifest)} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	observer := &fixedActivityObserver{testActivityObserver: &testActivityObserver{}, snapshot: codex.ActivitySnapshot{ThreadID: "child", Scope: "sinceFork", SentMessages: 2, WorkingMS: 19, CountsComplete: true}}
	for restart := 0; restart < 2; restart++ {
		m := &activityMonitor{server: s, observer: observer}
		release, err := m.lockActivityObservation(context.Background(), "bound", "child")
		if err != nil {
			t.Fatal(err)
		}
		provider := boundActivityProvider{monitor: m, conversationID: "bound", thread: "child"}
		for refresh := 0; refresh < 3; refresh++ {
			value, err := provider.ReadActivity(context.Background(), "child")
			if err != nil || value.SentMessages != 5 || value.WorkingMS != 30 || value.CoverageComplete {
				t.Fatal("frozen predecessor lost or repeatedly added", value, err)
			}
		}
		if m.snapshots["child"].value.SentMessages != 2 {
			t.Fatal("logical aggregate entered raw activity cache")
		}
		release()
	}
	if err := os.WriteFile(basePath, append(sourceBytes, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	m := &activityMonitor{server: s, observer: observer}
	provider := boundActivityProvider{monitor: m, conversationID: "bound", thread: "child"}
	if _, err := provider.ReadActivity(context.Background(), "child"); err == nil {
		t.Fatal("changed frozen source was accepted")
	}
	var saved codex.ActivitySnapshot
	if err := s.loadThreadRecord("activity-final", "child", &saved); err != nil || saved.SentMessages != 5 {
		t.Fatal("failed refresh overwrote retained aggregate", saved, err)
	}
}

type heldActivityObserver struct {
	*testActivityObserver
	entered, release chan struct{}
}

func (o *heldActivityObserver) ReadActivity(ctx context.Context, id string) (codex.ActivitySnapshot, error) {
	close(o.entered)
	select {
	case <-o.release:
		return codex.ActivitySnapshot{ThreadID: id, Scope: "thread", SentMessages: 99}, nil
	case <-ctx.Done():
		return codex.ActivitySnapshot{}, ctx.Err()
	}
}

func TestActivityObservationRuntimeTeamExclusionAndStaleWrite(t *testing.T) {
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "example")
	observer := &heldActivityObserver{testActivityObserver: &testActivityObserver{}, entered: make(chan struct{}), release: make(chan struct{})}
	m := &activityMonitor{server: s, observer: observer}
	release, err := m.lockActivityObservation(context.Background(), "example", "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	if lock, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, "example"); err == nil {
		lock.Close()
		t.Fatal("fork EX crossed observer snapshot/write ownership")
	}
	cancel()
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
	if unlock, err := store.LockOperation(ctx, "example"); err == nil {
		unlock()
		t.Fatal("team mutation crossed observer snapshot/write ownership")
	}
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.readSnapshotValidated(context.Background(), "thread-1", true, func() error { return m.validateActivityTarget("example", "thread-1") }, nil)
		done <- err
	}()
	<-observer.entered
	manifestPath := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(strings.Replace(string(manifest), "thread-1", "changed-root", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	close(observer.release)
	if err := <-done; err == nil {
		t.Fatal("stale in-flight observation was published")
	}
	if _, err := os.Stat(s.threadRecordPath("activity-final", "thread-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale snapshot reached persistent display record", err)
	}
	release()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := session.LockRuntimeExclusiveContext(ctx, s.config.AuthorityDir, "example")
	if err != nil {
		t.Fatal("observation locks did not release", err)
	}
	raw := &activityMonitor{observer: &budgetActivityObserver{testActivityObserver: &testActivityObserver{read: make(chan string, 1)}, state: "idle"}}
	if _, err := raw.readFreshActivity(ctx, "current-child"); err != nil {
		t.Fatal("raw admission reentered runtime ownership", err)
	}
	lock.Close()
}

func (o *budgetActivityObserver) ReadActivity(_ context.Context, id string) (codex.ActivitySnapshot, error) {
	o.read <- id
	if o.fail {
		return codex.ActivitySnapshot{}, errors.New("activity unavailable")
	}
	return codex.ActivitySnapshot{ThreadID: id, CurrentState: o.state}, nil
}

func TestActivityReadSharesSnapshotWithinStateBudget(t *testing.T) {
	base := time.Now()
	var elapsed atomic.Int64
	observer := &budgetActivityObserver{testActivityObserver: &testActivityObserver{read: make(chan string, 16)}, state: "working"}
	monitor := &activityMonitor{observer: observer, clock: func() time.Time { return base.Add(time.Duration(elapsed.Load())) }}
	for _, second := range []int64{0, 4, 5} {
		elapsed.Store(int64(time.Duration(second) * time.Second))
		if _, err := monitor.ReadActivity(context.Background(), "thread-1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(observer.read); got != 2 {
		t.Fatalf("active reads = %d, want 2", got)
	}
	observer.state = "idle"
	elapsed.Store(int64(10 * time.Second))
	if _, err := monitor.ReadActivity(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	elapsed.Store(int64(39 * time.Second))
	if _, err := monitor.ReadActivity(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	if got := len(observer.read); got != 3 {
		t.Fatalf("idle reads = %d, want 3", got)
	}
	elapsed.Store(int64(40 * time.Second))
	observer.fail = true
	stale, err := monitor.ReadActivity(context.Background(), "thread-1")
	if err != nil || !stale.Stale {
		t.Fatalf("failed read did not retain explicit stale state: %#v %v", stale, err)
	}
	observer.fail = false
	if value, err := monitor.ReadActivity(context.Background(), "thread-1"); err != nil || !value.Stale {
		t.Fatal("retry ignored backoff or lost stale snapshot")
	}
	if got := len(observer.read); got != 4 {
		t.Fatalf("backoff issued another read: %d", got)
	}
	elapsed.Store(int64(44 * time.Second))
	if value, err := monitor.ReadActivity(context.Background(), "thread-1"); err != nil || value.Stale {
		t.Fatalf("recovery did not replace stale snapshot: %#v %v", value, err)
	}
	if got := len(observer.read); got != 5 {
		t.Fatalf("retry reads: %d", got)
	}

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
	prepareInteractiveConversation(t, s, "example")
	observer := &testActivityObserver{verified: make(chan string, 1), read: make(chan string, 1), stopped: make(chan string, 1)}
	observer.wrong.Store(true)
	monitor := &activityMonitor{server: s, observer: observer}
	ctx, cancel := context.WithCancel(context.Background())
	monitor.wg.Add(1)
	go monitor.observe(ctx, "example", "thread-1")
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
	prepareInteractiveConversation(t, s, "example")
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
	// Slot fairness does not depend on snapshot fsync latency.
	s := newTestServer(t)
	const workers = 40
	observer := &limitedActivityObserver{
		testActivityObserver: testActivityObserver{stopped: make(chan string, workers)},
		started:              make(chan string, workers), advance: make(chan struct{}, workers),
	}
	monitor := &activityMonitor{server: s, observer: observer}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); monitor.wg.Wait() }()
	for i := 0; i < workers; i++ {
		slug, thread := fmt.Sprintf("session-%d", i), fmt.Sprintf("thread-%d", i)
		directory := filepath.Join(s.config.Workspace, "work", slug)
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
		writeWebTrackingFiles(t, directory, "active")
		if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte(fmt.Sprintf("schema: 1\nslug: %s\ncodex:\n  thread_id: %s\n", slug, thread)), 0644); err != nil {
			t.Fatal(err)
		}
		monitor.wg.Add(1)
		go monitor.observe(ctx, slug, thread)
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
	directory := filepath.Join(s.config.Workspace, "work", "unrelated-session")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, directory, "active")
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: unrelated-session\ncodex:\n  thread_id: unrelated-thread\n"), 0644); err != nil {
		t.Fatal(err)
	}
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

func testActivityRetainedMembers(t *testing.T, held bool) {
	s := newTestServer(t)
	if held {
		s.config.CodexSocket = filepath.Join(t.TempDir(), "codex.sock")
		listener, err := net.Listen("unix", s.config.CodexSocket)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
	}
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
	s.config.UserStateRoot = t.TempDir()
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), "example", "thread-1", true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{{Address: "reviewer0", Role: "reviewer", State: "ready", Thread: "member-thread", Model: "model-1", Effort: "medium", AddedAt: time.Now().UTC()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer := &testActivityObserver{verified: make(chan string, 8), read: make(chan string, 8), stopped: make(chan string, 8)}
	monitor := &activityMonitor{server: s, observer: observer, workers: make(map[string]activityWorker)}
	ctx, cancel := context.WithCancel(context.Background())
	if held {
		s.config.RecoverSessions = true
		// A held lead must not suppress an explicitly activated member.
		if err := s.recoveryStore().Set(ctx, "example", "thread-1", s.config.CodexSocket, true, false); err != nil {
			t.Fatal(err)
		}
		if err := s.recoveryStore().Update(ctx, "example", func(record *session.Recovery) error {
			record.ActiveThreads = []string{"member-thread"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	monitor.reconcile(ctx)
	seen := map[string]bool{}
	count := 2
	if held {
		count = 1
	}
	for range count {
		select {
		case id := <-observer.read:
			seen[id] = true
		case <-time.After(time.Second):
			cancel()
			t.Fatal("missing member observation")
		}
	}
	cancel()
	monitor.wg.Wait()
	if seen["thread-1"] == held || !seen["member-thread"] {
		t.Fatal(seen)
	}
	for range count {
		if cwd := <-observer.verified; cwd != directory {
			t.Fatal("member escaped session directory", cwd)
		}
	}
}

func TestActivityObservationWatchesRetainedMembersWithoutBrowser(t *testing.T) {
	testActivityRetainedMembers(t, false)
}
func TestActivityObservationWatchesActiveMemberWithHeldLead(t *testing.T) {
	testActivityRetainedMembers(t, true)
}
