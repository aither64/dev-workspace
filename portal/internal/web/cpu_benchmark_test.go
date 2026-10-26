package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// This is an opt-in controlled hot-path experiment, not a wall-clock CI gate.
// See docs/cpu-benchmark.md for the measured boundary and comparison procedure.
type cpuFixtureNative struct {
	*workspacecodex.Client // Unused mutation methods must never be reached.
	store                  turnretry.Store
	scopes                 map[string]turnretry.Scope
	identities             map[string]string
	observations, verifies atomic.Uint64
	version, changedAt     atomic.Int64
	mu                     sync.Mutex
	observed               map[string]bool
	reactions              []float64
	seen                   int64
}

func (c *cpuFixtureNative) VerifyThread(_ context.Context, thread, cwd string) error {
	c.verifies.Add(1)
	scope, ok := c.scopes[thread]
	if !ok || cwd != filepath.Join(scope.Workspace, "work", scope.Slug) {
		return fmt.Errorf("unexpected synthetic thread %q", thread)
	}
	return nil
}
func (c *cpuFixtureNative) ProveArchivedRootThread(context.Context, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}
func (c *cpuFixtureNative) ProveArchivedThread(context.Context, string, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}
func (c *cpuFixtureNative) ProveMaterializedActiveThread(context.Context, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}
func (c *cpuFixtureNative) HeadlessThreadMaterialized(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (c *cpuFixtureNative) ObserveThreadIdentity(_ context.Context, id, cwd, _ string, _ workspacecodex.ArchiveState) (workspacecodex.ThreadObservation, error) {
	return workspacecodex.ThreadObservation{Schema: 1, ThreadID: id, Cwd: cwd, ActivityKnown: true,
		Idle: true, ActivityToken: strings.Repeat("a", 64)}, nil
}
func (c *cpuFixtureNative) ListThreads(_ context.Context, options codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error) {
	threads := []codex.ThreadMetadata{}
	for id, scope := range c.scopes {
		cwd := filepath.Join(scope.Workspace, "work", scope.Slug)
		if options.Cwd == "" || options.Cwd == cwd {
			threads = append(threads, codex.ThreadMetadata{ID: id, Cwd: cwd, Source: "vscode"})
		}
	}
	return threads, nil, nil
}
func (c *cpuFixtureNative) LoadedThreadIDs(context.Context) ([]string, error) { return nil, nil }
func (c *cpuFixtureNative) ReconcileThreadInstructionsWithPolicy(context.Context, string, codex.ThreadPolicy) error {
	return nil
}
func (c *cpuFixtureNative) ListThreadActivity(_ context.Context, expected []workspacecodex.ThreadActivity) ([]workspacecodex.ThreadActivity, error) {
	return expected, nil
}
func (c *cpuFixtureNative) RetryAvailable() bool            { return true }
func (c *cpuFixtureNative) RetryStore() turnretry.Store     { return c.store }
func (c *cpuFixtureNative) RetryBackend() turnretry.Backend { return c }
func (c *cpuFixtureNative) RetryScope(_ context.Context, thread string) (turnretry.Scope, error) {
	scope, ok := c.scopes[thread]
	if !ok {
		return scope, fmt.Errorf("unknown synthetic retry thread %q", thread)
	}
	return scope, nil
}
func (c *cpuFixtureNative) Boundary(_ context.Context, scope turnretry.Scope) (string, error) {
	return c.identities[scope.Slug], nil
}
func (c *cpuFixtureNative) Observe(_ context.Context, scope turnretry.Scope) (turnretry.Observation, error) {
	c.observations.Add(1)
	version := int64(0)
	c.mu.Lock()
	c.observed[scope.Thread] = true
	if scope.Slug == "fixture000" && scope.Thread == scope.Root {
		version = c.version.Load()
		if version > c.seen {
			c.reactions = append(c.reactions, float64(time.Now().UnixNano()-c.changedAt.Load())/1e6)
			c.seen = version
		}
	}
	c.mu.Unlock()
	return turnretry.Observation{Turn: fmt.Sprintf("completed-%d", version), Status: "completed"}, nil
}
func (c *cpuFixtureNative) Prove(context.Context, turnretry.Scope, *turnretry.State, string) error {
	return fmt.Errorf("unexpected benchmark retry proof")
}
func (c *cpuFixtureNative) Check(context.Context, turnretry.Scope, turnretry.Snapshot) error {
	return fmt.Errorf("unexpected benchmark retry dispatch")
}
func (c *cpuFixtureNative) Reconcile(context.Context, turnretry.Scope, turnretry.Pending) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{}, false, fmt.Errorf("unexpected benchmark uncertainty")
}
func (c *cpuFixtureNative) Continue(context.Context, turnretry.Scope, turnretry.Pending) (codex.SendReceipt, error) {
	return codex.SendReceipt{}, fmt.Errorf("unexpected benchmark send")
}
func (c *cpuFixtureNative) NeverSubmitted(context.Context, turnretry.Scope, turnretry.Pending) (bool, error) {
	return false, fmt.Errorf("unexpected benchmark receipt")
}

func TestCPUFixture(t *testing.T) {
	mode := os.Getenv("DEV_WORKSPACE_CPU_BENCH")
	if mode == "" {
		t.Skip("opt-in CPU fixture; see docs/cpu-benchmark.md")
	}
	if mode != "validate" && mode != "measure" {
		t.Fatal("expected validate or measure")
	}
	integer := func(name string, fallback int) int {
		value := os.Getenv(name)
		if value == "" {
			return fallback
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			t.Fatalf("invalid %s", name)
		}
		return n
	}
	roots, members := integer("CPU_BENCH_ROOTS", 20), integer("CPU_BENCH_MEMBERS", 5)
	consumers := integer("CPU_BENCH_CONSUMERS", 0)
	if roots < 1 || roots > 100 || members > 10 || consumers > 5 {
		t.Fatal("fixture scale exceeds bounds")
	}
	root, script := t.TempDir(), filepath.Join("..", "..", "..", "test", "support", "cpu_fixture.rb")
	script, _ = filepath.Abs(script)
	write := func(path, contents string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"workspace", "authority", "package/bin", "state"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "cpu-fixture"), "synthetic fixture\n", 0600)
	packageRoot, workspace := filepath.Join(root, "package"), filepath.Join(root, "workspace")
	profile, socket := filepath.Join(root, "profile"), filepath.Join(root, "native.sock")
	if err := os.Symlink(packageRoot, profile); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Fatal(err)
	}
	for name, action := range map[string]string{"workspace-host": "status", "workspace-portal": "notify"} {
		write(filepath.Join(packageRoot, "bin", name), "#!/bin/sh\nexec "+quote(ruby)+" "+quote(script)+" "+action+" "+quote(root)+" \"$@\"\n", 0700)
	}
	if err := os.MkdirAll(filepath.Join(packageRoot, "share/dev-workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(packageRoot, "share/dev-workspace/coordination.json"), `{"schema":1,"adapters":{},"machines":{},"repositories":{},"profileMachine":null}`, 0600)
	write(filepath.Join(packageRoot, "share/dev-workspace/package.json"), `{"schema":1,"activationEnvironmentAliases":[],"routerSocket":"/fixture/router.sock","userNamespace":"fixture","agent_teams":{"managed":false}}`, 0600)
	config := Config{Workspace: workspace, WorkspaceName: "cpu-fixture", BaseURL: "https://fixture.example.test",
		HostProfile: profile, PackageRoot: packageRoot, AuthorityDir: filepath.Join(root, "authority"),
		CodexSocket: socket, CodexVersion: "0.152.1", UserStateRoot: filepath.Join(root, "state"),
		DevSession: filepath.Join(packageRoot, "bin/workspace-host"), RecoverSessions: true}
	config.RegistrationMarker = filepath.Join(root, "registration.json")
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(root, "absent-providers"))
	c := &cpuFixtureNative{store: turnretry.Store{Root: config.UserStateRoot, Workspace: workspace},
		scopes: map[string]turnretry.Scope{}, identities: map[string]string{}, observed: map[string]bool{}}
	tmux := "#!/bin/sh\nprintf 'tmux\\n' >> " + quote(filepath.Join(root, "helpers.log")) + "\ncase \"$6\" in\n"
	for i := 0; i < roots; i++ {
		slug := fmt.Sprintf("fixture%03d", i)
		thread, dir := "root-"+slug, filepath.Join(workspace, "work", slug)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(dir, "portal.yml"), fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.152.1\n", slug, thread, socket), 0600)
		writeWebTrackingFiles(t, dir, "active")
		authority := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: slug, Workspace: workspace,
			TmuxSocket: filepath.Join(root, "tmux.sock"), TmuxSessionID: fmt.Sprintf("$%d", i),
			TmuxIdentity: strings.Repeat("a", 64), CodexThreadID: thread, CodexSocketPath: socket, CodexClientVersion: config.CodexVersion}
		encoded, _ := json.Marshal(authority)
		write(filepath.Join(config.AuthorityDir, slug+".json"), string(encoded), 0600)
		line := strings.Join([]string{authority.TmuxSessionID, slug, "1", slug, workspace, slug,
			authority.TmuxSocket, thread, socket, config.CodexVersion, "%1", authority.TmuxIdentity}, "\t")
		tmux += quote(authority.TmuxSessionID+":") + ") printf '%s\\n' " + quote(line) + ";;\n"
		store, err := teamruntime.NewStore(config.UserStateRoot, workspace)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Update(context.Background(), slug, thread, true, func(r *teamruntime.Roster) error {
			for m := 0; m < members; m++ {
				r.Members = append(r.Members, teamruntime.Member{Address: fmt.Sprintf("implementer%d", m), Role: "implementer", Index: uint64(m), Thread: fmt.Sprintf("%s-member%d", slug, m), Model: "fixture-model", Effort: "xhigh", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		summary, err := session.Find(workspace, slug)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := policyScope(summary)
		if err != nil {
			t.Fatal(err)
		}
		c.identities[slug] = identity.Incarnation
		threads := []string{thread}
		for m := 0; m < members; m++ {
			threads = append(threads, fmt.Sprintf("%s-member%d", slug, m))
		}
		for _, id := range threads {
			scope := turnretry.Scope{Workspace: workspace, Slug: slug, Root: thread, Thread: id, Socket: socket}
			c.scopes[id] = scope
			tx, err := c.store.Lock(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			tx.State.Enroll(turnretry.Observation{Turn: "completed-0", Status: "completed"})
			tx.State.Boundary = identity.Incarnation
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
		}
		_, err = (turnretry.PolicyStore{Root: config.UserStateRoot, Workspace: workspace}).Update(context.Background(), identity, true, func(p *turnretry.Policy) error {
			for _, id := range threads {
				p.Threads = append(p.Threads, turnretry.ThreadPolicy{Thread: id, Suppressed: []turnretry.Suppression{}})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		recovery := session.RecoveryStore{Root: config.UserStateRoot, Workspace: workspace}
		if err := recovery.Set(context.Background(), slug, thread, socket, true, true); err != nil {
			t.Fatal(err)
		}
		if err := recovery.Update(context.Background(), slug, func(r *session.Recovery) error { r.ActiveThreads = threads; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "tmux"), tmux+"*) exit 1;;\nesac\n", 0700)
	config.Tmux, config.Codex, config.VerifyThread = filepath.Join(root, "tmux"), c, c.VerifyThread
	setup, err := exec.Command(ruby, script, "setup", root, strconv.Itoa(integer("CPU_BENCH_LEDGER_BYTES", 3*1024*1024))).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture setup: %s: %v", setup, err)
	}
	t.Logf("fixture: %s", setup)
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Activity observation has its own native connection; this hot-path fixture
	// excludes that daemon and leaves ObserveActivity off on both revisions.
	request := func() float64 {
		started := time.Now()
		w := httptest.NewRecorder()
		s.coordinationAPI(w, httptest.NewRequest("GET", "/api/coordination", nil), "")
		if w.Code != 200 {
			t.Errorf("fixture status: %d %s", w.Code, w.Body.String())
		}
		return float64(time.Since(started)) / 1e6
	}
	request()
	command := exec.Command(ruby, script, "run", root)
	command.Stderr = os.Stderr
	input, _ := command.StdinPipe()
	output, _ := command.StdoutPipe()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		input.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("coordinator fixture process: %v", err)
		}
	}()
	scan := bufio.NewScanner(output)
	reply := func() json.RawMessage {
		if !scan.Scan() {
			t.Fatal("Ruby fixture exited", scan.Err())
		}
		return append(json.RawMessage(nil), scan.Bytes()...)
	}
	reply()
	// The two workers and two-slot queue do not visit 120 identities in ten
	// seconds. Require actual passive coverage and a completed Step transaction
	// for every identity; startup state alone is not evidence of a warm worker.
	sweepLimit := time.Duration(len(c.scopes)+15) * time.Second
	waitUntil := func(label string, deadline time.Time, ready func() bool) {
		t.Helper()
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatalf("%s exceeded %s", label, sweepLimit)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	warmStarted := time.Now()
	warmDeadline := warmStarted.Add(sweepLimit)
	waitUntil("complete retry sweep", warmDeadline, func() bool {
		c.mu.Lock()
		covered := len(c.observed) == len(c.scopes)
		c.mu.Unlock()
		if !covered {
			return false
		}
		ctx, cancel := context.WithDeadline(context.Background(), warmDeadline)
		defer cancel()
		for _, scope := range c.scopes {
			// Inspect waits for the real controller's ledger transaction, including
			// its final policy checkpoint, before reading the persisted result.
			tx, err := c.store.Inspect(ctx, scope)
			if err != nil {
				t.Fatal("warm retry state:", err)
			}
			settled := tx.State.ClockMS > 0 && tx.State.Enrolled && tx.State.Pending == nil && !tx.State.Uncertain()
			tx.Close()
			if !settled {
				return false
			}
		}
		return true
	})
	if mode == "measure" {
		time.Sleep(max(0, 10*time.Second-time.Since(warmStarted)))
	}
	warmSeconds := time.Since(warmStarted).Seconds()
	t.Logf("warm sweep: %d conversations, %.3fs", len(c.scopes), warmSeconds)
	outstanding := func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.seen < c.version.Load() }
	publishUpdate := func() {
		c.mu.Lock()
		c.changedAt.Store(time.Now().UnixNano())
		c.version.Add(1)
		c.mu.Unlock()
		fmt.Fprintln(input, "event")
		reply()
	}
	if mode == "validate" {
		publishUpdate()
		waitUntil("synthetic reaction", time.Now().Add(sweepLimit), func() bool { return !outstanding() })
		fmt.Fprintln(input, "stop")
		result := reply()
		var delivery struct {
			DeliveryMS        []float64
			PendingDeliveries int
		}
		if err := json.Unmarshal(result, &delivery); err != nil || len(delivery.DeliveryMS) != 1 || delivery.PendingDeliveries != 0 {
			t.Fatalf("fixture notification did not deliver: %s: %v", result, err)
		}
		t.Logf("coordinator fixture: %s", result)
		return
	}
	// Refuse accidental accounting of the live Codex service's cgroup.
	group, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	groupPath := strings.TrimSpace(strings.TrimPrefix(string(group), "0::"))
	if !strings.HasPrefix(filepath.Base(groupPath), "workspace-cpu-bench-") {
		t.Fatal("run inside a dedicated workspace-cpu-bench-* transient unit")
	}
	cpuUS := func() uint64 {
		data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", groupPath, "cpu.stat"))
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(data))
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "usage_usec" {
				n, _ := strconv.ParseUint(fields[i+1], 10, 64)
				return n
			}
		}
		t.Fatal("missing cgroup CPU")
		return 0
	}
	selfCPU := func() float64 {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	}
	active := os.Getenv("CPU_BENCH_ACTIVE") == "1"
	phase := func(duration time.Duration) []float64 {
		latencies := []float64{}
		deadline := time.Now().Add(duration)
		drainDeadline := deadline.Add(sweepLimit)
		for time.Now().Before(deadline) || active && outstanding() {
			cycle := time.Now()
			if cycle.After(drainDeadline) {
				t.Fatal("active reaction exceeded sweep bound")
			}
			if active && cycle.Before(deadline) && !outstanding() {
				publishUpdate()
			}
			values := make([]float64, consumers)
			var requests sync.WaitGroup
			for n := range values {
				requests.Add(1)
				go func(n int) { defer requests.Done(); values[n] = request() }(n)
			}
			requests.Wait()
			latencies = append(latencies, values...)
			until := deadline
			if !cycle.Before(deadline) {
				until = drainDeadline
			}
			time.Sleep(max(0, min(5*time.Second-time.Since(cycle), time.Until(until))))
		}
		return latencies
	}
	fmt.Fprintln(input, "start")
	reply()
	beforeCalls := cpuFixtureHelperCounts(t, root)
	c.observations.Store(0)
	c.verifies.Store(0)
	c.mu.Lock()
	c.reactions = nil
	c.mu.Unlock()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started, beforeCPU, beforeSelfCPU := time.Now(), cpuUS(), selfCPU()
	beforeEvents := c.version.Load()
	latencies := phase(30 * time.Second)
	fmt.Fprintln(input, "stop")
	coordinator := reply()
	seconds, cpu := time.Since(started).Seconds(), float64(cpuUS()-beforeCPU)/1e6
	runtime.ReadMemStats(&after)
	helperCounts := cpuFixtureHelperCounts(t, root)
	for name, count := range beforeCalls {
		helperCounts[name] -= count
	}
	c.mu.Lock()
	reactions := slices.Clone(c.reactions)
	c.mu.Unlock()
	if active {
		var delivered struct {
			DeliveryMS        []float64
			PendingDeliveries int
		}
		expected := int(c.version.Load() - beforeEvents)
		if err := json.Unmarshal(coordinator, &delivered); err != nil || len(delivered.DeliveryMS) != expected || delivered.PendingDeliveries != 0 || len(reactions) != expected {
			t.Errorf("active fixture lost updates: expected=%d observations=%d coordinator=%s", expected, len(reactions), coordinator)
		}
	}
	result := map[string]any{"seconds": seconds, "cgroupCPUSeconds": cpu, "cpuPercentOneCore": cpu / seconds * 100,
		"warmSeconds": warmSeconds, "warmedConversations": len(c.scopes), "activeUpdates": c.version.Load() - beforeEvents,
		"portalSelfCPUSeconds": selfCPU() - beforeSelfCPU,
		"roots":                roots, "membersPerRoot": members, "consumers": consumers, "active": active,
		"passiveObservations": c.observations.Load(), "nativeVerifies": c.verifies.Load(), "helpers": helperCounts,
		"goAllocations": after.Mallocs - before.Mallocs, "goAllocatedBytes": after.TotalAlloc - before.TotalAlloc,
		"statusMS": latencies, "nativeReactionMS": reactions, "coordinator": coordinator}
	encoded, _ := json.Marshal(result)
	t.Logf("CPU_BENCH_RESULT %s", encoded)
}

func cpuFixtureHelperCounts(t *testing.T, root string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	data, err := os.ReadFile(filepath.Join(root, "helpers.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(string(data)) {
		counts[name]++
	}
	return counts
}
