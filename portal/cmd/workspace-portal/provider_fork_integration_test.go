//go:build naming_runtime_integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/creationprogress"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

// This case is executable only in the same frozen disposable VM as naming.
// All commands are from the selected package; only model responses are synthetic.
func TestManagedProviderForkPortalIntegration(t *testing.T) {
	t.Log("phase=provider_setup")
	f := newManagedProviderFork(t)
	const target, control = "2026-11-25-portal-fork", "2026-11-25-portal-control"
	f.cli("start", target, "--as-is", "--no-attach", "--team", "solo", "--model", "gpt-5.5", "--effort", "low", "--provider", "openai", "--goal-file", f.goal("PORTAL_ROOT_SEED"))
	f.cli("team", "add", target, "implementer", "--as-is", "--model", "gpt-5.5", "--effort", "low")
	f.cli("start", control, "--as-is", "--no-attach", "--team", "solo", "--model", "gpt-5.5", "--effort", "low", "--provider", "openai", "--goal-file", f.goal("PORTAL_CONTROL_SEED"))
	root := f.summary(target).Codex.ThreadID
	controlRoot := f.summary(control).Codex.ThreadID
	store, err := teamruntime.NewStore(f.state, f.workspace)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	roster, err := store.Load(target, root)
	if err != nil || len(roster.Members) != 1 || roster.Members[0].State != "ready" {
		t.Fatal("supported retained member creation unavailable")
	}
	member := roster.Members[0]
	f.assign(target, member.Address, "PORTAL_MEMBER_SEED")
	f.history(root, "PORTAL_ROOT_SEED")
	f.history(member.Thread, "PORTAL_MEMBER_SEED")
	f.history(controlRoot, "PORTAL_CONTROL_SEED")
	roster, err = store.Load(target, root)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	controlBefore := f.threadSnapshot(controlRoot)
	ancestors := map[string]any{root: f.threadSnapshot(root), member.Thread: f.threadSnapshot(member.Thread)}

	// Hold the first dependent read only after its exact result is durable.
	// The worker cannot proceed to commit or any later participant while held.
	const operation = "f06a5b6c-892e-4de4-a7a8-321c94b96dd1"
	cutRequest := providerForkCutRequest{operation: operation, target: "pool-b", session: target, inherit: true}
	f.arm(operation)
	f.request("/api/codex-providers/switch", map[string]any{"providerId": cutRequest.target, "idempotencyKey": cutRequest.operation, "session": cutRequest.session, "inherit": cutRequest.inherit}, cutRequest)
	t.Log("phase=durable_cut")
	known := f.waitCut(cutRequest)
	journal := f.journal(cutRequest)
	if journal.OperationID != operation || known.State != "known-child" || known.ChildThreadID == "" || journal.Stage != "verifying" {
		f.operationFailure("cut_validation", cutRequest, "result=precommit_child_mismatch")
	}
	for _, projection := range journal.Projections {
		if projection.Recovery || projection.Terminal {
			f.operationFailure("cut_validation", cutRequest, "result=already_projected")
		}
	}
	snapshot := f.read(cutRequest)
	f.settingsCutAbsent = f.settingsSinkAbsent(cutRequest)
	epoch, err := session.SocketIdentity(f.socket)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	beforeForks := f.forkCount(known.SourceThreadID)
	f.stop("provider")
	f.stop("portal")
	if err := f.stopProxy(); err != nil {
		t.Fatal(managedSmokeFailureText(err))
	}
	f.stop("native")
	// Same home, registration owner and original UUID, with a new endpoint.
	f.startNative("restart")
	f.startPortal("restart")
	newEpoch, err := session.SocketIdentity(f.socket)
	if err != nil || newEpoch == epoch {
		t.Fatal("native restart did not establish a new endpoint epoch")
	}
	t.Log("phase=recovery")
	f.prepareSettingsEvidence(cutRequest, newEpoch, snapshot)
	f.request("/api/codex-providers/recover", map[string]any{"operationId": operation, "session": target}, cutRequest)
	first := f.applied(cutRequest)
	if !bytes.Equal(snapshot, f.read(cutRequest)) || f.forkCount(known.SourceThreadID) != beforeForks {
		f.operationFailure("recovery_validation", cutRequest, "result=snapshot_or_fork_changed")
	}
	for _, p := range f.journal(cutRequest).Participants {
		if p.SourceThreadID == known.SourceThreadID && p.ChildThreadID != known.ChildThreadID {
			f.operationFailure("recovery_validation", cutRequest, "result=saved_child_changed")
		}
	}
	firstRoot, firstMember := f.checkSwitch(target, root, member, roster, first, "pool-b", root, member.Thread)
	if _, explicit := first.Sessions[target]; explicit {
		t.Fatal("public inherit retained an explicit override")
	}
	f.continueTerminal(target, firstRoot, "PORTAL_TERMINAL_B", 1)
	f.assign(target, member.Address, "PORTAL_MEMBER_B")
	f.history(firstMember, "PORTAL_MEMBER_B")
	f.assertControl(control, controlRoot, controlBefore)
	for id, before := range ancestors {
		if !reflect.DeepEqual(before, f.threadSnapshot(id)) {
			t.Fatal("source ancestor history/settings changed")
		}
	}

	ancestors[firstRoot], ancestors[firstMember] = f.threadSnapshot(firstRoot), f.threadSnapshot(firstMember)
	secondRoster, err := store.Load(target, root)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	const back = "d77694f8-f1cb-4ca1-b9ec-c280a58bd570"
	backRequest := providerForkCutRequest{operation: back, target: "openai"}
	t.Log("phase=reverse_switch")
	f.request("/api/codex-providers/switch", map[string]any{"providerId": "openai", "idempotencyKey": back}, backRequest)
	second := f.applied(backRequest)
	lastRoot, lastMember := f.checkSwitch(target, root, member, secondRoster, second, "openai", firstRoot, firstMember)
	if second.Active != "openai" {
		t.Fatal("public workspace default did not change")
	}
	f.continueTerminal(target, lastRoot, "PORTAL_TERMINAL_A", 0)
	f.assign(target, member.Address, "PORTAL_MEMBER_A")
	f.history(lastMember, "PORTAL_MEMBER_A")
	f.assertControl(control, controlRoot, controlBefore)
	for id, before := range ancestors {
		if !reflect.DeepEqual(before, f.threadSnapshot(id)) {
			t.Fatal("source ancestor history/settings changed")
		}
	}
	if f.forkCount(root) != 1 || f.forkCount(member.Thread) != 1 || f.forkCount(firstRoot) != 1 || f.forkCount(firstMember) != 1 || f.forkCount(controlRoot) != 0 {
		t.Fatal("participant fork counts do not match the two original operations")
	}
	if f.smoke.proxyFailure.Load() || !f.smoke.checkEgressCount(f.smoke.counter("final_body"), t.Error) {
		t.Fatal("transport or denied-egress boundary failed")
	}
	t.Log("public session inherit A->B and default B->A; original links, known-child before-commit recovery, exact parents/history, managed terminal and member continuation")
}

type managedProviderForkView struct {
	Active    string
	Inherited bool
	Operation *struct{ ID, Phase, Reason string }
}

type managedProviderFork struct {
	t                                                               *testing.T
	hard                                                            context.Context
	smoke                                                           *managedSmokeHarness
	name, workspace, state, runtime, socket, upstream, portal, tmux string
	env                                                             []string
	units                                                           map[string]*managedSmokeUnit
	unitFiles                                                       []string
	providers                                                       [2]*managedSmokeProvider
	mu                                                              sync.Mutex
	forks                                                           map[string]int
	cutOperation                                                    string
	cut                                                             chan codexproviders.ForkParticipant
	nativeImage                                                     providerForkNativeImage
	nativeEntry                                                     managedSmokeEndpoint
	nativeEntryPath                                                 string
	nativeJoined                                                    bool
	endpointUnaccounted                                             bool
	portalUnaccounted                                               bool
	settingsCutAbsent                                               bool
	settingsSource                                                  *providerForkSnapshot
	settingsDigest, settingsEpoch, settingsPath                     string
}

// This prospective tail gives reporting and teardown an opportunity inside the
// existing Go hard limit. It reduces progress time; it guarantees neither logs
// nor complete cleanup, and never extends a product operation deadline.
func providerForkOperationalContext(hard context.Context) (context.Context, context.CancelFunc, error) {
	deadline, ok := hard.Deadline()
	if !ok {
		return nil, nil, managedSmokeFailure("hard_deadline_unavailable")
	}
	ctx, cancel := context.WithDeadline(hard, deadline.Add(-30*time.Second))
	if err := managedSmokeExpired(ctx); err != nil {
		cancel()
		return nil, nil, err
	}
	return ctx, cancel, nil
}

func newManagedProviderFork(t *testing.T) *managedProviderFork {
	t.Helper()
	f := newManagedProviderForkNative(t)
	f.startPortal("initial")
	return f
}

// The focused case uses the full fixture's exact native setup and cleanup, but
// ends before portal startup, model requests or provider operations.
func TestManagedProviderForkNativeStartupIntegration(t *testing.T) {
	newManagedProviderForkNative(t)
}

func newManagedProviderForkNative(t *testing.T) *managedProviderFork {
	t.Helper()
	hard := managedSmokeScenario(t)
	operational, cancel, err := providerForkOperationalContext(hard)
	if err != nil {
		t.Fatal("phase=provider_setup result=" + managedSmokeContextResult(err))
	}
	t.Cleanup(cancel)
	h := &managedSmokeHarness{t: t, ctx: operational, manifest: managedSmokeManifestFor(t), counterChangeDiagnostic: t.Log}
	if !managedSmokeHex(h.manifest.SourceFiles["portal/cmd/workspace-portal/provider_fork_integration_test.go"], 32) {
		t.Fatal("managed portal fixture source is missing from the immutable manifest")
	}
	root, err := os.MkdirTemp("/tmp", "pf-")
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	h.root = root
	f := &managedProviderFork{t: t, hard: hard, smoke: h, name: fmt.Sprintf("portal-fork-%d", os.Getpid()), forks: map[string]int{}, units: map[string]*managedSmokeUnit{}, cut: make(chan codexproviders.ForkParticipant, 1)}
	f.workspace, f.state = filepath.Join(root, "workspace"), filepath.Join(root, "host-state")
	f.runtime = filepath.Join(root, "runtime", f.name)
	f.socket, f.upstream, f.portal, f.tmux = filepath.Join(f.runtime, "app-server.sock"), filepath.Join(f.runtime, "native.sock"), filepath.Join(f.runtime, "portal.sock"), filepath.Join(f.runtime, "tmux.sock")
	t.Cleanup(f.close)
	for _, dir := range []string{"workspace/work", "workspace/archive", "workspace/worktrees", "registry", "home", "xdg-config", "xdg-state", "xdg-cache", filepath.Join("runtime", f.name, "authority"), filepath.Join("host-state", "codex-homes", f.name)} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal("fixture prerequisite or identity unavailable")
		}
	}
	// A selected host profile includes the generation lock used by run-portal.
	managedSmokeWrite(t, filepath.Join(f.state, "transition.lock"), "", 0600)
	if err := os.Symlink(h.manifest.PackageRoot, filepath.Join(root, "profile")); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	registry, _ := json.Marshal(map[string]any{"schema": 1, "workspaces": []any{map[string]any{"name": f.name, "root": f.workspace, "hostname": "managed-smoke.example.test", "aliases": []string{}}}})
	managedSmokeWrite(t, filepath.Join(root, "registry", "registry.json"), string(registry), 0600)
	f.providers = [2]*managedSmokeProvider{managedSmokeNewProvider(t, true), managedSmokeNewProvider(t, true)}
	home := filepath.Join(f.state, "codex-homes", f.name)
	managedSmokeWrite(t, filepath.Join(home, "auth.json"), "{\"auth_mode\":\"apikey\",\"OPENAI_API_KEY\":\"synthetic-fixture-only\"}\n", 0600)
	// Acknowledge only this offer while retaining the fixture's selected model.
	managedSmokeWrite(t, filepath.Join(home, "config.toml"), "model_provider = \"openai\"\nmodel = \"gpt-5.5\"\nmodel_reasoning_effort = \"low\"\napproval_policy = \"never\"\nopenai_base_url = "+strconv.Quote(f.providers[0].url)+"\n[notice.model_migrations]\n\"gpt-5.5\" = \"gpt-6-sol\"\n", 0600)
	token := filepath.Join(root, "registry", "pool.token")
	managedSmokeWrite(t, token, "synthetic-fixture-only\n", 0600)
	catalog, _ := json.Marshal(map[string]any{"schema": 1, "initialProvider": "pool-b", "providers": []any{map[string]any{"id": "openai", "label": "Fixture A", "kind": "subscription"}, map[string]any{"id": "pool-b", "label": "Fixture B", "kind": "codex-lb", "baseURL": f.providers[1].url, "tokenFile": token}}})
	catalogPath := filepath.Join(root, "registry", "providers.json")
	managedSmokeWrite(t, catalogPath, string(catalog), 0600)
	f.env = managedSmokeEnvironment(root, home)
	for i, value := range f.env {
		switch {
		case strings.HasPrefix(value, "XDG_RUNTIME_DIR="):
			f.env[i] = "XDG_RUNTIME_DIR=/run/user/1000"
		case strings.HasPrefix(value, "DEV_WORKSPACE_CODEX_HOME="):
			f.env[i] = "DEV_WORKSPACE_CODEX_HOME=" + home
		case strings.HasPrefix(value, "DEV_WORKSPACES_CODEX_PROVIDERS="):
			f.env[i] = "DEV_WORKSPACES_CODEX_PROVIDERS=" + catalogPath
		}
	}
	f.env = append(f.env, "DEV_WORKSPACES_RUNTIME_DIR="+filepath.Join(root, "runtime"), "DEV_WORKSPACE_NAME="+f.name, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "TERM=xterm-256color", "SHELL=/run/current-system/sw/bin/bash")
	h.ordinarySocket = f.socket
	h.beforeNativeRequest = f.observe
	h.canaryListener, err = net.Listen("tcp4", "127.0.0.1:"+managedSmokeCanaryPort)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	h.canary = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.canaryHits.Add(1); w.WriteHeader(500) }), ReadHeaderTimeout: time.Second}
	go func() { _ = h.canary.Serve(h.canaryListener) }()
	t.Log("phase=counter_before_deny")
	before := h.counter("before_deny")
	probe, cancel := context.WithTimeout(h.context(), 100*time.Millisecond)
	connection, probeErr := (&net.Dialer{}).DialContext(probe, "tcp4", "127.0.0.1:"+managedSmokeCanaryPort)
	if connection != nil {
		_ = connection.Close()
	}
	cancel()
	h.baseline = h.counter("after_deny")
	if probeErr == nil || h.baseline <= before || h.canaryHits.Load() != 0 {
		t.Fatal("VM denied-egress prerequisite failed")
	}
	f.nativeImage, err = providerForkVerifyNativeImage(filepath.Join(h.manifest.PackageRoot, "libexec/codex/libexec/codex/bin/codex"), h.manifest.NativeSHA256)
	if err != nil {
		t.Fatal("managed native image prerequisite unavailable")
	}
	f.defineUnit("native", "workspace-codex@"+f.name+".service", "run-codex")
	f.defineUnit("tmux", "workspace-tmux@"+f.name+".service", "run-tmux")
	f.defineUnit("portal", "workspace-portal@"+f.name+".service", "run-portal")
	f.defineUnit("provider", "workspace-codex-provider@"+f.name+".service", "run-provider-worker")
	f.systemctl("daemon-reload")
	f.start("tmux")
	f.startNative("initial")
	return f
}

func (f *managedProviderFork) defineUnit(key, name, action string) {
	f.t.Helper()
	dir := "/run/user/1000/systemd/user"
	if err := os.MkdirAll(dir, 0700); err != nil {
		f.t.Fatal("fixture prerequisite or identity unavailable")
	}
	path := filepath.Join(dir, name)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		f.t.Fatal("fixture unit already exists")
	}
	args := append([]string{"/run/current-system/sw/bin/env", "-i"}, f.env...)
	args = append(args, filepath.Join(f.smoke.manifest.PackageRoot, "bin/workspace-host"), action, f.name)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = strconv.Quote(strings.ReplaceAll(arg, "%", "%%"))
	}
	managedSmokeWrite(f.t, path, "[Unit]\nDescription=Disposable provider fork fixture\n[Service]\nType=simple\nRestart=no\nKillMode=control-group\nTimeoutStopSec=3s\nUMask=0077\nExecStart=:"+strings.Join(quoted, " ")+"\n[Install]\nWantedBy=default.target\n", 0600)
	f.unitFiles = append(f.unitFiles, path)
	f.units[key] = &managedSmokeUnit{name: name, root: f.smoke.root}
}
func providerForkSystemctl(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	return managedSmokeCommandResult(ctx, exec.CommandContext(ctx, "/run/current-system/sw/bin/systemctl", append([]string{"--user"}, args...)...))
}
func (f *managedProviderFork) systemctlContext(ctx context.Context, args ...string) []byte {
	f.t.Helper()
	data, err := providerForkSystemctl(ctx, args...)
	if err != nil {
		action := "unknown"
		if len(args) > 0 {
			action = providerForkCutEnum(args[0], "start", "show", "stop", "disable", "daemon-reload")
		}
		f.t.Fatalf("phase=unit_control action=%s parent=%s %s", action, managedSmokeContextResult(ctx.Err()), managedSmokeFailureText(err))
	}
	return data
}
func (f *managedProviderFork) systemctl(args ...string) []byte {
	f.t.Helper()
	return f.systemctlContext(f.smoke.context(), args...)
}
func (f *managedProviderFork) start(key string) {
	f.t.Helper()
	unit := f.units[key]
	f.systemctl("start", unit.name)
	managedSmokeAwait(f.t, f.smoke.context(), 5*time.Second, "unit_main", func(ctx context.Context) bool {
		data := f.systemctlContext(ctx, "show", unit.name, "--property=MainPID", "--property=ControlGroup")
		var pid int
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MainPID=") {
				pid, _ = strconv.Atoi(strings.TrimPrefix(line, "MainPID="))
			}
			if strings.HasPrefix(line, "ControlGroup=") {
				unit.group = strings.TrimPrefix(line, "ControlGroup=")
			}
		}
		process, ok := managedSmokeProcessAt(pid)
		if !ok {
			return false
		}
		if !strings.HasPrefix(unit.group, "/user.slice/user-1000.slice/") || process.group != unit.group {
			f.t.Fatal("owned unit cgroup mismatch")
		}
		unit.main = process
		return true
	})
}
func (f *managedProviderFork) stopOwned(ctx context.Context, key string) (err error) {
	unit := f.units[key]
	if unit == nil {
		return nil
	}
	defer func() {
		if key == "native" && err == nil {
			f.nativeJoined = true
		}
	}()
	if unit.group == "" {
		data, err := providerForkSystemctl(ctx, "show", unit.name, "--property=MainPID", "--property=ControlGroup")
		if err != nil {
			return err
		}
		pid := -1
		groupSeen := false
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MainPID=") {
				parsed, err := strconv.Atoi(strings.TrimPrefix(line, "MainPID="))
				if err == nil && parsed >= 0 {
					pid = parsed
				}
			}
			if strings.HasPrefix(line, "ControlGroup=") {
				unit.group = strings.TrimPrefix(line, "ControlGroup=")
				groupSeen = true
			}
		}
		if pid < 0 || !groupSeen {
			return managedSmokeFailure("result=unit_identity_unobserved")
		}
		if unit.group == "" {
			if pid == 0 {
				return nil
			}
			return managedSmokeFailure("result=unit_identity_unobserved")
		}
		process, ok := managedSmokeProcessAt(pid)
		if ok && process.group != unit.group {
			return managedSmokeFailure("result=worker_cgroup_changed")
		}
		unit.main = process
	}
	if !strings.HasPrefix(unit.group, "/user.slice/user-1000.slice/") {
		return managedSmokeFailure("result=foreign_cgroup")
	}
	if _, err := providerForkSystemctl(ctx, "stop", unit.name); err != nil {
		return err
	}
	if managedSmokeAlive(unit.main) || !managedSmokeCgroupEmpty(unit.group) {
		return managedSmokeFailure("result=owned_service_unaccounted")
	}
	return nil
}
func (f *managedProviderFork) stop(key string) {
	f.t.Helper()
	if err := f.stopOwned(f.smoke.context(), key); err != nil {
		f.t.Fatal("phase=unit_stop " + managedSmokeFailureText(err))
	}
}

// The selected native publishes an alias on Unix. Moving that entry does not
// move its protected socket, and native teardown cannot find the moved alias.
// Keep the exact entry proof until its old owner has independently joined.
func (f *managedProviderFork) cleanupNativeEndpoints() error {
	if f.nativeEntryPath != "" {
		if info, err := os.Lstat(f.nativeEntryPath); !os.IsNotExist(err) && !f.nativeJoined {
			f.endpointUnaccounted = true
			return managedSmokeEndpointFailure("native_holder", managedSmokeEndpointType(info), "owner_not_joined")
		}
		if err := f.nativeEntry.remove(f.nativeEntryPath, "native_holder"); err != nil {
			f.endpointUnaccounted = true
			return err
		}
	}
	for _, endpoint := range []struct{ path, role string }{{f.socket, "public"}, {f.upstream, "native_holder"}} {
		if err := managedSmokeEndpointAbsent(endpoint.path, endpoint.role); err != nil {
			f.endpointUnaccounted = true
			return err
		}
	}
	if f.endpointUnaccounted {
		return managedSmokeEndpointFailure("native_holder", "unobserved", "prior_unaccounted")
	}
	return nil
}

func (f *managedProviderFork) prepareNativeEndpoints(phase string) error {
	if phase == "restart" {
		if !f.nativeJoined {
			return managedSmokeEndpointFailure("native_holder", "unobserved", "owner_not_joined")
		}
		return f.cleanupNativeEndpoints()
	}
	for _, endpoint := range []struct{ path, role string }{{f.socket, "public"}, {f.upstream, "native_holder"}} {
		if err := managedSmokeEndpointAbsent(endpoint.path, endpoint.role); err != nil {
			f.endpointUnaccounted = true
			return err
		}
	}
	return nil
}

func (f *managedProviderFork) holdNativeEndpoint() error {
	if err := managedSmokeEndpointAbsent(f.upstream, "native_holder"); err != nil {
		f.endpointUnaccounted = true
		return err
	}
	entry, err := managedSmokeCaptureEndpoint(f.socket, "native_public")
	if err != nil {
		f.endpointUnaccounted = true
		return err
	}
	f.nativeEntry, f.nativeEntryPath = entry, f.socket
	if err := os.Rename(f.socket, f.upstream); err != nil {
		f.endpointUnaccounted = true
		return managedSmokeEndpointFailure("native_public", managedSmokeEndpointType(entry.info), "rename_failed")
	}
	f.nativeEntryPath = f.upstream
	if err := entry.verify(f.upstream, "native_holder"); err != nil {
		f.endpointUnaccounted = true
		return err
	}
	return nil
}

func (f *managedProviderFork) startNative(phase string) {
	f.t.Helper()
	f.t.Log("phase=native_start")
	if err := f.prepareNativeEndpoints(phase); err != nil {
		f.t.Fatal("phase=native_endpoint " + managedSmokeFailureText(err))
	}
	f.nativeJoined = false
	f.start("native")
	target := providerForkNativeTarget{main: f.units["native"].main, image: f.nativeImage, socket: f.socket}
	readUnit := func() ([]byte, error) {
		return providerForkSystemctl(f.smoke.context(), "show", f.units["native"].name, "--property=MainPID,ControlGroup,ActiveState,SubState,Result,ExecMainCode,ExecMainStatus")
	}
	// Align remaining managed startup with Host#wait_for_codex_sockets. This
	// single fixture allowance is not a native-only timer or a product SLA.
	started := time.Now()
	if failure := target.await(f.smoke.context(), phase, 15*time.Second, started, target.facts, readUnit); failure != "" {
		f.nativeStartupFailure(target, failure, providerForkNativeJournal)
	}
	// The transparent fixture proxy gets its own epoch on every restart.
	// It never supplies native results or keeps the captured generation alive.
	if f.smoke.context().Err() != nil || !time.Now().Before(started.Add(15*time.Second)) {
		f.nativeStartupFailure(target, target.failure(phase, "observation_expired", time.Since(started), readUnit), providerForkNativeJournal)
	}
	if err := f.holdNativeEndpoint(); err != nil {
		f.t.Fatal("phase=native_endpoint " + managedSmokeFailureText(err))
	}
	target.socket = f.upstream
	facts := target.facts()
	if f.smoke.context().Err() != nil || !time.Now().Before(started.Add(15*time.Second)) {
		f.nativeStartupFailure(target, target.failure(phase, "observation_expired", time.Since(started), readUnit), providerForkNativeJournal)
	}
	if facts.process != "same" || facts.state != "running" || facts.executable != "selected" || facts.socket != "present" {
		f.nativeStartupFailure(target, target.failure(phase, "admission_raced", time.Since(started), readUnit), providerForkNativeJournal)
	}
	if err := f.nativeEntry.verify(f.upstream, "native_holder"); err != nil {
		f.endpointUnaccounted = true
		f.t.Fatal("phase=native_endpoint " + managedSmokeFailureText(err))
	}
	if managedSmokeExpired(f.smoke.context()) != nil || !time.Now().Before(started.Add(15*time.Second)) {
		f.nativeStartupFailure(target, target.failure(phase, "observation_expired", time.Since(started), readUnit), providerForkNativeJournal)
	}
	f.smoke.proxyFailure.Store(false)
	f.smoke.startProxy(f.upstream)
	f.smoke.ordinary = codex.New(f.socket)
}

func providerForkNativeJournal(ctx context.Context, unit string) ([]byte, error) {
	return managedSmokeCommandResult(ctx, exec.CommandContext(ctx, "/run/current-system/sw/bin/journalctl", "--user", "--boot=0", "--unit="+unit, "--output=json", "--all", "--no-pager"))
}

type providerForkNativeProcessEvidence struct {
	PID, ProcessGroup int
	Start, Cgroup     string
}

func providerForkNativeProcessRecord(p managedSmokeProcess) providerForkNativeProcessEvidence {
	return providerForkNativeProcessEvidence{p.pid, p.processGroup, p.start, p.group}
}

type providerForkNativeEvidence struct {
	Schema                                   int
	Refusal, Unit                            string
	Captured, Before, After                  providerForkNativeProcessEvidence
	ProcessStatus, Executable, JournalStatus string
	CommandLine                              []byte
	Journal                                  string
}

func (f *managedProviderFork) captureNativeStartup(target providerForkNativeTarget, refusal string, journal func(context.Context, string) ([]byte, error)) string {
	directory := os.Getenv("MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR")
	info, err := os.Lstat(directory)
	if err != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !info.IsDir() || info.Mode().Perm() != 0700 || f.hard == nil {
		return "unavailable"
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	unit := f.units["native"]
	if !ok || int(owner.Uid) != os.Geteuid() || unit == nil || unit.main != target.main || target.main.pid <= 0 {
		return "unavailable"
	}
	ctx, cancel := context.WithTimeout(f.hard, 6*time.Second)
	defer cancel()
	if managedSmokeExpired(ctx) != nil {
		return "unavailable"
	}
	evidence := providerForkNativeEvidence{Schema: 1, Refusal: refusal, Unit: unit.name, Captured: providerForkNativeProcessRecord(target.main), ProcessStatus: "unavailable", JournalStatus: "unavailable"}
	before, beforeOK := managedSmokeProcessAt(target.main.pid)
	evidence.Before = providerForkNativeProcessRecord(before)
	if beforeOK && before == target.main {
		executable, exeErr := os.Readlink(fmt.Sprintf("/proc/%d/exe", target.main.pid))
		cmdline, cmdErr := managedSmokeRead(fmt.Sprintf("/proc/%d/cmdline", target.main.pid), 64*1024)
		after, afterOK := managedSmokeProcessAt(target.main.pid)
		evidence.After = providerForkNativeProcessRecord(after)
		if afterOK && after == target.main && exeErr == nil && len(executable) <= 4096 && cmdErr == nil && managedSmokeExpired(ctx) == nil {
			evidence.ProcessStatus, evidence.Executable, evidence.CommandLine = "available", executable, cmdline
		}
	}
	// This exact-unit journal is separate, non-atomic evidence; neither a late
	// executable nor socket observation can change the original refusal.
	if managedSmokeExpired(ctx) == nil {
		data, err := journal(ctx, unit.name)
		if err == nil && len(data) <= 64*1024 && managedSmokeExpired(ctx) == nil {
			evidence.JournalStatus, evidence.Journal = "available", string(data)
		}
	}
	data, err := json.Marshal(evidence)
	if managedSmokeExpired(ctx) != nil || err != nil || len(data) > providerForkSettingsEvidenceLimit || providerForkWriteFirstPrivate(filepath.Join(directory, "native-startup.json"), data) != nil {
		return "unavailable"
	}
	return "available"
}

func (f *managedProviderFork) nativeStartupFailure(target providerForkNativeTarget, refusal string, journal func(context.Context, string) ([]byte, error)) {
	f.t.Helper()
	original := "managed native startup refused: " + refusal
	f.t.Log(original)
	f.t.Log("phase=native_evidence outcome=" + f.captureNativeStartup(target, refusal, journal))
	f.t.Fatal(original)
}

type providerForkNativeTarget struct {
	main   managedSmokeProcess
	image  providerForkNativeImage
	socket string
}

type providerForkNativeImage struct {
	path string
	info os.FileInfo
}

// Hash the opened selected image once before native launch. Admission/restart
// bind /proc/exe to this immutable file, rather than scanning it in the socket
// observation or refusal sample. This proof never crosses fixture lifetimes.
func providerForkVerifyNativeImage(path, expected string) (providerForkNativeImage, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return providerForkNativeImage{}, fmt.Errorf("native image path unavailable")
	}
	file, err := os.Open(canonical)
	if err != nil {
		return providerForkNativeImage{}, fmt.Errorf("native image unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return providerForkNativeImage{}, fmt.Errorf("native image identity unavailable")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return providerForkNativeImage{}, fmt.Errorf("native image read failed")
	}
	if hex.EncodeToString(digest.Sum(nil)) != expected {
		return providerForkNativeImage{}, fmt.Errorf("native image digest mismatch")
	}
	return providerForkNativeImage{path: canonical, info: info}, nil
}

func (image providerForkNativeImage) matches(executable string) bool {
	path, err := filepath.EvalSymlinks(executable)
	if err != nil || path != image.path || image.info == nil {
		return false
	}
	info, err := os.Stat(executable)
	return err == nil && os.SameFile(info, image.info)
}

type providerForkNativeFacts struct{ process, executable, socket, state string }

func providerForkNativeProcess(main managedSmokeProcess) (string, string) {
	current, live := managedSmokeProcessAt(main.pid)
	if live && current != main {
		return "changed", "unknown"
	}
	// The ordinary reader excludes zombies. Only a readable terminal stat for
	// this exact lifetime establishes exit; absence is still unobserved.
	data, err := managedSmokeRead(fmt.Sprintf("/proc/%d/stat", main.pid), 4096)
	if err == nil {
		end := strings.LastIndexByte(string(data), ')')
		if end >= 0 {
			fields := strings.Fields(string(data[end+1:]))
			if len(fields) >= 20 {
				_, startErr := strconv.ParseUint(fields[19], 10, 64)
				if startErr != nil {
					return "unobserved", "unknown"
				}
				if fields[19] != main.start {
					return "changed", "unknown"
				}
			}
			if len(fields) >= 20 && (fields[0] == "Z" || fields[0] == "X") {
				group, groupErr := managedSmokeRead(fmt.Sprintf("/proc/%d/cgroup", main.pid), 4096)
				processGroup, parseErr := strconv.Atoi(fields[2])
				if groupErr == nil && parseErr == nil {
					if processGroup != main.processGroup || strings.TrimSpace(strings.TrimPrefix(string(group), "0::")) != main.group {
						return "changed", "unknown"
					}
					return "same", "terminal"
				}
			}
			if live && len(fields) >= 20 && len(fields[0]) == 1 && strings.Contains("RSDTtIWKP", fields[0]) {
				return "same", "running"
			}
		}
	}
	return "unobserved", "unknown"
}

func (target providerForkNativeTarget) facts() providerForkNativeFacts {
	process, state := providerForkNativeProcess(target.main)
	facts := providerForkNativeFacts{process: process, state: state, executable: "unobserved", socket: "unobserved"}
	executable := fmt.Sprintf("/proc/%d/exe", target.main.pid)
	if path, err := os.Readlink(executable); err == nil && target.image.path != "" && target.image.info != nil {
		if info, err := os.Stat(executable); err == nil {
			facts.executable = "nonselected"
			if path == target.image.path && os.SameFile(info, target.image.info) {
				facts.executable = "selected"
			}
		}
	}
	if info, err := os.Stat(target.socket); err == nil {
		facts.socket = "non_socket"
		if info.Mode()&os.ModeSocket != 0 {
			facts.socket = "present"
		}
	} else if os.IsNotExist(err) {
		facts.socket = "absent"
	}
	after, afterState := providerForkNativeProcess(target.main)
	if process == "changed" || after == "changed" {
		facts.process, facts.state, facts.executable = "changed", "unknown", "unobserved"
	} else if process != "same" || after != "same" {
		facts.process, facts.state, facts.executable = "unobserved", "unknown", "unobserved"
	} else {
		facts.state = afterState
	}
	return facts
}

func (target providerForkNativeTarget) await(parent context.Context, phase string, budget time.Duration, started time.Time, sample func() providerForkNativeFacts, readUnit func() ([]byte, error)) string {
	deadline := started.Add(budget)
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	reason := "observation_expired"
	// Reads remain synchronous: this is an admission cutoff, not a preemptive
	// return cap. Check after completed work as well as before starting more.
	for managedSmokeExpired(ctx) == nil && time.Now().Before(deadline) {
		facts := sample()
		if managedSmokeExpired(ctx) != nil || !time.Now().Before(deadline) {
			break
		}
		if facts.process == "changed" {
			reason = "ownership_changed"
			break
		}
		if facts.state == "terminal" {
			reason = "terminal_before_readiness"
			break
		}
		if facts.process == "same" && facts.state == "running" && facts.executable == "selected" && facts.socket == "present" {
			after := sample()
			if managedSmokeExpired(ctx) != nil || !time.Now().Before(deadline) {
				reason = "observation_expired"
			} else if after == facts {
				return ""
			} else {
				reason = "admission_raced"
			}
			break
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	if parent.Err() != nil {
		reason = "scenario_" + managedSmokeContextResult(parent.Err())
	}
	return target.failure(phase, reason, time.Since(started), readUnit)
}

type providerForkNativeUnit struct {
	query, active, sub, result, group string
	pid, code, status                 int
}

func providerForkReadNativeUnit(data []byte, err error) providerForkNativeUnit {
	unit := providerForkNativeUnit{query: "unavailable", active: "unobserved", sub: "unobserved", result: "unobserved", pid: -1, code: -1, status: -1}
	if err != nil {
		return unit
	}
	fields := map[string]string{}
	malformed := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if _, duplicate := fields[key]; !ok || duplicate {
			malformed = true
		}
		fields[key] = value
	}
	unit.query = "available"
	for _, key := range []string{"MainPID", "ControlGroup", "ActiveState", "SubState", "Result", "ExecMainCode", "ExecMainStatus"} {
		if _, ok := fields[key]; !ok {
			unit.query = "partial"
		}
	}
	for key, value := range fields {
		switch key {
		case "MainPID", "ExecMainCode", "ExecMainStatus":
			number, parseErr := strconv.Atoi(value)
			if parseErr != nil || number < 0 || key == "ExecMainCode" && number > 6 || key == "ExecMainStatus" && number > 255 {
				malformed = true
				continue
			}
			switch key {
			case "MainPID":
				unit.pid = number
			case "ExecMainCode":
				unit.code = number
			case "ExecMainStatus":
				unit.status = number
			}
		case "ControlGroup":
			unit.group = value
		case "ActiveState":
			switch value {
			case "active", "inactive", "failed", "activating", "deactivating", "reloading", "maintenance":
				unit.active = value
			default:
				malformed = true
			}
		case "SubState":
			switch value {
			case "running", "dead", "failed", "exited", "start-pre", "start", "start-post", "stop", "stop-sigterm", "stop-sigkill", "stop-post", "auto-restart", "reload":
				unit.sub = value
			default:
				malformed = true
			}
		case "Result":
			switch value {
			case "success", "exit-code", "signal", "core-dump", "timeout", "watchdog", "start-limit-hit", "resources", "protocol", "oom-kill", "exec-condition":
				unit.result = value
			default:
				malformed = true
			}
		default:
			malformed = true
		}
	}
	if malformed {
		return providerForkNativeUnit{query: "malformed", active: "unobserved", sub: "unobserved", result: "unobserved", pid: -1, code: -1, status: -1}
	}
	return unit
}

func (target providerForkNativeTarget) failure(phase, reason string, observed time.Duration, readUnit func() ([]byte, error)) string {
	if phase != "initial" && phase != "restart" {
		phase = "unobserved"
	}
	started := time.Now()
	before := target.facts()
	unit := providerForkReadNativeUnit(readUnit())
	facts := target.facts()
	if before.process == "changed" || facts.process == "changed" || unit.pid > 0 && unit.pid != target.main.pid || unit.group != "" && unit.group != target.main.group {
		facts.process, facts.executable, facts.state = "changed", "unobserved", "unknown"
	} else if before.process != facts.process {
		facts.process, facts.executable, facts.state = "unobserved", "unobserved", "unknown"
		unit.query = "raced"
	}
	terminal := unit.query == "available" && unit.pid == 0 && (unit.active == "inactive" || unit.active == "failed") && unit.code > 0 && unit.status >= 0
	if terminal {
		if facts.process == "unobserved" || facts.state == "terminal" {
			facts.state = "terminal"
		} else {
			facts.state, unit.query = "unknown", "raced"
		}
	}
	if facts.state == "terminal" && unit.query == "available" && (unit.pid > 0 || unit.active == "active" || unit.active == "activating") {
		facts.state, unit.query = "unknown", "raced"
	}
	// This is a later diagnostic sample, never a second admission opportunity.
	// Socket mode is not FD ownership/RPC liveness; context and process facts
	// do not replace the ordinary service/group cleanup and joins.
	return fmt.Sprintf("phase=%s reason=%s observation_ms=%d sample_ms=%d process=%s executable=%s socket=%s state=%s unit_query=%s active=%s sub=%s result=%s code_known=%t code=%d status_known=%t status=%d", phase, reason, observed.Milliseconds(), time.Since(started).Milliseconds(), facts.process, facts.executable, facts.socket, facts.state, unit.query, unit.active, unit.sub, unit.result, unit.code >= 0, unit.code, unit.status >= 0, unit.status)
}
func (f *managedProviderFork) stopProxy() error { return f.smoke.stopProxy() }
func (f *managedProviderFork) cli(args ...string) []byte {
	f.t.Helper()
	f.t.Log("phase=cli_setup")
	// Match the public initialization envelope. The original operational
	// deadline still bounds these sequential setup calls together.
	ctx, cancel := context.WithTimeout(f.smoke.context(), 210*time.Second)
	defer cancel()
	output, failure := f.runCLI(ctx, args...)
	if failure != "" {
		f.t.Fatalf("supported CLI setup failed: %s", failure)
	}
	return output
}

func (f *managedProviderFork) runCLI(ctx context.Context, args ...string) ([]byte, string) {
	label := "other"
	if len(args) > 0 && args[0] == "start" {
		label = "start"
	} else if len(args) > 1 && args[0] == "team" {
		switch args[1] {
		case "add":
			label = "team_add"
		case "assign":
			label = "team_assign"
		}
	}
	command := exec.Command(filepath.Join(f.smoke.manifest.PackageRoot, "bin/dev-session"), append([]string{"--workspace", f.name}, args...)...)
	command.Dir, command.Env = f.workspace, f.env
	command.WaitDelay = 2 * time.Second
	var stdout, stderr managedSmokeOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	seen, stage, event := false, "unobserved", "unobserved"
	var decoder *creationprogress.Decoder
	if label == "start" {
		command.Env = append(append([]string(nil), f.env...), creationprogress.Environment+"=1")
		decoder = creationprogress.NewDecoder(&stderr, func(progress creationprogress.Event) {
			seen, stage, event = true, progress.Stage, progress.Event
		})
		command.Stderr = decoder
	}
	started := time.Now()
	err := managedSmokeExpired(ctx)
	if err == nil {
		err = runNamingProcess(ctx, command, 0)
	}
	contextErr, elapsed := ctx.Err(), time.Since(started)
	// Run has joined the stream copies. A last reported finish is not proof
	// of readiness; neither this context sample nor Run identifies a killer
	// or certifies descendant/service cleanup.
	if decoder != nil {
		_ = decoder.Close()
	}
	overflow := stdout.overflow || stderr.overflow || len(stdout.data)+len(stderr.data) > 64*1024
	if err == nil && managedSmokeExpired(ctx) == nil && !overflow {
		return append(stdout.data, stderr.data...), ""
	}
	failure := fmt.Sprintf("command=%s %s cutoff=%s progress_seen=%t progress_stage=%s progress_event=%s", label, managedSmokeCommandFailure(contextErr, elapsed, command, err, overflow), managedSmokeContextResult(managedSmokeExpired(ctx)), seen, stage, event)
	f.t.Log("supported CLI setup failed: " + failure)
	f.t.Log("phase=cli_evidence outcome=" + f.handoffCLIFailure(label, failure, stdout.data, stderr.data, overflow))
	return nil, failure
}

type providerForkCLIFailureEvidence struct {
	Schema       int     `json:"schema"`
	Phase        string  `json:"phase"`
	Command      string  `json:"command"`
	Failure      string  `json:"failure"`
	OutputStatus string  `json:"outputStatus"`
	Stdout       *[]byte `json:"stdout,omitempty"`
	Stderr       *[]byte `json:"stderr,omitempty"`
}

func (f *managedProviderFork) handoffCLIFailure(label, failure string, stdout, stderr []byte, overflow bool) string {
	directory := os.Getenv("MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR")
	info, err := os.Lstat(directory)
	if err != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !info.IsDir() || info.Mode().Perm() != 0700 || f.hard == nil || managedSmokeExpired(f.hard) != nil {
		return "unavailable"
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return "unavailable"
	}
	evidence := providerForkCLIFailureEvidence{Schema: 1, Phase: "cli_failure_joined_output", Command: label, Failure: failure, OutputStatus: "unavailable_overflow"}
	if !overflow {
		// These joined buffers have no inter-stream chronology and prove no
		// assignment admission or cleanup, including after a deadline stop.
		out, errout := append([]byte{}, stdout...), append([]byte{}, stderr...)
		evidence.OutputStatus, evidence.Stdout, evidence.Stderr = "retained", &out, &errout
	}
	data, err := json.Marshal(evidence)
	if err != nil || len(data) > providerForkSettingsEvidenceLimit || managedSmokeExpired(f.hard) != nil || providerForkWriteFirstPrivate(filepath.Join(directory, "cli-failure.json"), data) != nil {
		return "unavailable"
	}
	return "available"
}

func TestManagedProviderForkCLIWiring(t *testing.T) {
	for _, mode := range []string{"start", "team_add", "team_assign", "failure", "failure_unavailable", "unobserved", "canceled", "deadline", "limit", "overflow", "truncated", "missing", "descendant"} {
		t.Run(mode, func(t *testing.T) {
			root, workspace := t.TempDir(), t.TempDir()
			directory := t.TempDir()
			if os.Chmod(directory, 0700) != nil {
				t.Fatal("private CLI evidence directory unavailable")
			}
			fixtureMode := mode
			if mode == "failure_unavailable" {
				directory, fixtureMode = filepath.Join(directory, "absent"), "failure"
			}
			t.Setenv("MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR", directory)
			hard, cancelHard := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancelHard()
			record := filepath.Join(t.TempDir(), "entered")
			args := []string{"start", "private-goal-canary"}
			options := "1"
			if strings.HasPrefix(mode, "team_") {
				args, options = []string{"team", strings.TrimPrefix(mode, "team_"), "private-goal-canary"}, "0"
			}
			t.Setenv(creationprogress.Environment, "parent-private-canary")
			t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
			environment := append(os.Environ(), creationprogress.Environment+"=0", "MANAGED_PROVIDER_CLI_FIXTURE="+fixtureMode, "MANAGED_PROVIDER_CLI_RECORD="+record, "MANAGED_PROVIDER_CLI_CWD="+workspace, "MANAGED_PROVIDER_CLI_OPTIONS="+options, "MANAGED_PROVIDER_CLI_ARGS="+strings.Join(append([]string{"--workspace", "fixture"}, args...), "\n"))
			original := append([]string(nil), environment...)
			f := &managedProviderFork{t: t, hard: hard, smoke: &managedSmokeHarness{manifest: managedSmokeManifest{PackageRoot: root}}, workspace: workspace, name: "fixture", env: environment}
			if mode != "missing" {
				if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
				script := "#!/bin/sh\nexec " + strconv.Quote(os.Args[0]) + " -test.run=^TestManagedProviderForkCLIChildFixture$ -- \"$@\"\n"
				if err := os.WriteFile(filepath.Join(root, "bin/dev-session"), []byte(script), 0700); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var joined chan struct{}
			if mode == "canceled" {
				joined = make(chan struct{})
				go func() {
					defer close(joined)
					for ctx.Err() == nil {
						if _, err := os.Stat(record); err == nil {
							cancel()
							return
						}
						time.Sleep(time.Millisecond)
					}
				}()
			}
			output, failure := f.runCLI(ctx, args...)
			if joined != nil {
				<-joined
			}
			if mode == "descendant" || mode == "canceled" {
				data, err := os.ReadFile(record + ".child")
				if err != nil {
					t.Fatal("owned CLI descendant receipt unavailable")
				}
				pid, _ := strconv.Atoi(string(data))
				// Group KILL is issued before return; allow the synthetic child to
				// reach terminal state before observing the result, without another killer.
				if err := managedSmokeWait(context.Background(), time.Second, func(context.Context) (bool, error) {
					_, live := managedSmokeProcessAt(pid)
					return !live, nil
				}); err != nil {
					t.Fatal("CLI retained a live owned descendant after group cleanup")
				}
			}
			if !reflect.DeepEqual(f.env, original) || os.Getenv(creationprogress.Environment) != "parent-private-canary" {
				t.Fatal("CLI progress opt-in mutated the inherited environment")
			}
			data, evidenceErr := os.ReadFile(filepath.Join(directory, "cli-failure.json"))
			if failure == "" || mode == "failure_unavailable" {
				if !os.IsNotExist(evidenceErr) {
					t.Fatal("CLI success or unavailable write published failure evidence")
				}
			} else {
				var evidence providerForkCLIFailureEvidence
				info, statErr := os.Stat(filepath.Join(directory, "cli-failure.json"))
				if evidenceErr != nil || statErr != nil || info.Mode().Perm() != 0600 || len(data) > providerForkSettingsEvidenceLimit || json.Unmarshal(data, &evidence) != nil || evidence.Schema != 1 || evidence.Phase != "cli_failure_joined_output" || evidence.Command != "start" || evidence.Failure != failure {
					t.Fatal("joined CLI evidence unavailable before cleanup")
				}
				if mode == "overflow" {
					var fields map[string]json.RawMessage
					_ = json.Unmarshal(data, &fields)
					if evidence.OutputStatus != "unavailable_overflow" || fields["stdout"] != nil || fields["stderr"] != nil {
						t.Fatal("overflowing CLI streams presented as complete")
					}
				} else if evidence.OutputStatus != "retained" || evidence.Stdout == nil || evidence.Stderr == nil {
					t.Fatal("bounded CLI streams were not retained separately")
				} else if (mode == "failure" || mode == "unobserved") && (string(*evidence.Stdout) != "private-stdout-canary" || string(*evidence.Stderr) != "private-stderr-canary"+creationprogress.Prefix+"{private-stderr-canary") {
					t.Fatal("CLI joined output or progress filtering changed")
				}
			}
			switch mode {
			case "start", "team_add", "team_assign", "descendant":
				if failure != "" || string(output) != "ordinary stdout\nordinary stderr\n" {
					t.Fatal("CLI wiring changed ordinary output or retained valid progress")
				}
			case "limit":
				if failure != "" || len(output) != 64*1024 {
					t.Fatal("valid progress consumed the ordinary output allowance")
				}
			case "truncated":
				if failure != "" || string(output) != creationprogress.Prefix+"{" {
					t.Fatal("CLI did not close and drain the progress decoder")
				}
			default:
				if output != nil || failure == "" {
					t.Fatal("failed CLI returned ordinary data or lost its diagnostic")
				}
				for _, private := range []string{root, workspace, "private-goal-canary", "private-stdout-canary", "private-stderr-canary", "privatemembercanary", "parent-private-canary"} {
					if strings.Contains(failure, private) {
						t.Fatal("CLI failure diagnostic exposed private command data")
					}
				}
				if mode == "canceled" || mode == "deadline" {
					category := "canceled"
					if mode == "deadline" {
						category = "deadline_exceeded"
					}
					if !strings.Contains(failure, "context="+category+" ") || !strings.Contains(failure, "process=signaled exit=-1 signal=") || !strings.Contains(failure, "progress_seen=true progress_stage=team_member progress_event=finish") {
						t.Fatal("CLI lost the owning context/process/progress snapshot")
					}
				} else if mode == "overflow" {
					if !strings.Contains(failure, "context=active ") || !strings.Contains(failure, "exit=0 signal=0 result=overflow wait_delay=false overflow=true ") {
						t.Fatal("combined ordinary output overflow was accepted")
					}
				} else if mode == "missing" {
					if !strings.Contains(failure, "process=not_started exit=-1 signal=-1 ") {
						t.Fatal("CLI start failure was not retained safely")
					}
				} else if !strings.Contains(failure, "context=active ") || !strings.Contains(failure, "process=exited exit=7 signal=0 result=run_failed wait_delay=false overflow=false ") {
					t.Fatal("CLI nonzero exit did not retain its fixed process result")
				}
				if mode == "unobserved" || mode == "missing" {
					if !strings.Contains(failure, "progress_seen=false progress_stage=unobserved progress_event=unobserved") {
						t.Fatal("unobserved progress became a false stage observation")
					}
				}
			}
		})
	}
}

func TestManagedProviderForkCLIChildFixture(t *testing.T) {
	mode := os.Getenv("MANAGED_PROVIDER_CLI_FIXTURE")
	if mode == "" {
		return
	}
	if mode == "portal_process" {
		record, gate, socket := os.Getenv("MANAGED_PROVIDER_CLI_RECORD"), os.Getenv("MANAGED_PROVIDER_PORTAL_GATE"), os.Getenv("MANAGED_PROVIDER_PORTAL_SOCKET")
		if namingTestPublish(record, []byte("started")) != nil {
			os.Exit(3)
		}
		for {
			if _, err := os.Stat(record + ".stop"); err == nil {
				os.Exit(0)
			}
			if _, err := os.Stat(gate); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			os.Exit(3)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && r.URL.Path == "/healthz" {
				if os.WriteFile(record+".health", nil, 0600) != nil {
					os.Exit(3)
				}
				w.WriteHeader(200)
				return
			}
			var body struct {
				OperationID string `json:"operationId"`
			}
			if r.Method != "POST" || r.URL.Path != "/api/codex-providers/recover" || r.Header.Get("Origin") != managedSmokeOrigin || json.NewDecoder(r.Body).Decode(&body) != nil || body.OperationID != "f06a5b6c-892e-4de4-a7a8-321c94b96dd1" {
				w.WriteHeader(400)
				return
			}
			file, err := os.OpenFile(record+".posts", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if err != nil {
				os.Exit(3)
			}
			_, _ = file.WriteString("post\n")
			_ = file.Close()
			if os.Getenv("MANAGED_PROVIDER_PORTAL_LOST") == "1" {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = connection.Close()
				}
				return
			}
			if os.Getenv("MANAGED_PROVIDER_PORTAL_REFUSE") == "1" {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})}
		done := make(chan struct{})
		go func() { defer close(done); _ = server.Serve(listener) }()
		for {
			if _, err := os.Stat(record + ".stop"); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_ = server.Close()
		<-done
		os.Exit(0)
	}
	if mode == "native_process" {
		if namingTestPublish(os.Getenv("MANAGED_PROVIDER_CLI_RECORD"), []byte("ready")) != nil {
			os.Exit(3)
		}
		for {
			if _, err := os.Stat(os.Getenv("MANAGED_PROVIDER_NATIVE_RELEASE")); err == nil {
				status, _ := strconv.Atoi(os.Getenv("MANAGED_PROVIDER_NATIVE_EXIT"))
				os.Exit(status)
			}
			time.Sleep(time.Millisecond)
		}
	}
	cwd, _ := os.Getwd()
	if cwd != os.Getenv("MANAGED_PROVIDER_CLI_CWD") || os.Getenv(creationprogress.Environment) != os.Getenv("MANAGED_PROVIDER_CLI_OPTIONS") || strings.Join(os.Args[3:], "\n") != os.Getenv("MANAGED_PROVIDER_CLI_ARGS") {
		os.Exit(3)
	}
	if os.Getenv(creationprogress.Environment) == "1" && mode != "unobserved" {
		fmt.Fprint(os.Stderr, creationprogress.Prefix+`{"stage":"team_member","event":"finish","elapsedMs":1,"member":"privatemembercanary"}`+"\n")
	}
	if mode == "descendant" || mode == "canceled" {
		child := exec.Command("/bin/sh", "-c", "trap '' TERM; exec sleep 30")
		if child.Start() != nil {
			os.Exit(3)
		}
		if os.WriteFile(os.Getenv("MANAGED_PROVIDER_CLI_RECORD")+".child", []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
			os.Exit(3)
		}
	}
	switch mode {
	case "canceled", "deadline":
		if err := os.WriteFile(os.Getenv("MANAGED_PROVIDER_CLI_RECORD"), nil, 0600); err != nil {
			os.Exit(3)
		}
		for {
			time.Sleep(time.Second)
		}
	case "failure", "unobserved":
		fmt.Fprint(os.Stdout, "private-stdout-canary")
		fmt.Fprint(os.Stderr, "private-stderr-canary"+creationprogress.Prefix+"{private-stderr-canary")
		os.Exit(7)
	case "limit", "overflow":
		count := 32 * 1024
		if mode == "overflow" {
			count++
		}
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("o"), count))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), count))
	case "truncated":
		fmt.Fprint(os.Stderr, creationprogress.Prefix+"{")
	default:
		fmt.Fprint(os.Stdout, "ordinary stdout\n")
		fmt.Fprint(os.Stderr, "ordinary stderr\n")
	}
	os.Exit(0)
}

func TestManagedProviderForkNativeProbeWiring(t *testing.T) {
	// Static wiring only: real setup/admission is exercised by the separately
	// selected native integration case, not by these source call-order checks.
	_, path, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fixture source unavailable")
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal("fixture source unavailable")
	}
	calls := map[string][]string{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				switch name := call.Fun.(type) {
				case *ast.Ident:
					calls[function.Name.Name] = append(calls[function.Name.Name], name.Name)
				case *ast.SelectorExpr:
					calls[function.Name.Name] = append(calls[function.Name.Name], name.Sel.Name)
				}
			}
			return true
		})
	}
	full, prefix, focused := calls["newManagedProviderFork"], calls["newManagedProviderForkNative"], calls["TestManagedProviderForkNativeStartupIntegration"]
	if slices.Index(full, "newManagedProviderForkNative") < 0 || slices.Index(full, "startPortal") <= slices.Index(full, "newManagedProviderForkNative") || slices.Contains(prefix, "startPortal") || !slices.Contains(prefix, "startNative") || !slices.Contains(prefix, "Cleanup") || !reflect.DeepEqual(focused, []string{"newManagedProviderForkNative"}) {
		t.Fatal("full/focused startup no longer shares the native prefix before portal")
	}
}

func TestManagedProviderForkNativeRefusalEvidence(t *testing.T) {
	if mode := os.Getenv("MANAGED_PROVIDER_NATIVE_EVIDENCE_CONTROL"); mode != "" {
		directory := os.Getenv("MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR")
		main, ok := managedSmokeProcessAt(os.Getpid())
		if !ok {
			t.Fatal("synthetic lifetime unavailable")
		}
		if mode == "changed" {
			main.start += "-different"
		}
		hard, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if mode == "spent" {
			cancel()
		}
		if mode == "timeout" {
			cancel()
			hard, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
		}
		f := &managedProviderFork{t: t, hard: hard, units: map[string]*managedSmokeUnit{"native": {name: "private-native-unit-canary.service", main: main}}}
		t.Cleanup(func() {
			data, err := os.ReadFile(filepath.Join(directory, "native-startup.json"))
			if mode == "available" || mode == "changed" || mode == "journal_error" {
				var evidence providerForkNativeEvidence
				info, statErr := os.Stat(filepath.Join(directory, "native-startup.json"))
				if err != nil || statErr != nil || info.Mode().Perm() != 0600 || json.Unmarshal(data, &evidence) != nil || evidence.Refusal != "phase=initial reason=observation_expired" || evidence.Captured.Start != main.start {
					t.Error("private refusal evidence unavailable before cleanup")
				} else {
					if mode == "changed" && (evidence.ProcessStatus != "unavailable" || evidence.Executable != "" || len(evidence.CommandLine) != 0) || mode != "changed" && evidence.ProcessStatus != "available" {
						t.Error("process sample adopted a changed or unavailable lifetime")
					}
					if mode == "journal_error" && evidence.JournalStatus != "unavailable" || mode != "journal_error" && evidence.JournalStatus != "available" {
						t.Error("journal availability was confused with process ownership")
					}
					t.Log("private_capture_verified")
				}
			} else if mode == "existing" {
				if err != nil || string(data) != "private-existing-canary" {
					t.Error("capture replaced earlier evidence")
				}
			} else if !os.IsNotExist(err) {
				t.Error("failed optional capture unexpectedly published evidence")
			}
			t.Log("native_cleanup_joined")
		})
		journal := func(ctx context.Context, unit string) ([]byte, error) {
			t.Log("capture_entered")
			deadline, hasDeadline := ctx.Deadline()
			hardDeadline, _ := hard.Deadline()
			if unit != f.units["native"].name || !hasDeadline || deadline.After(hardDeadline) || time.Until(deadline) > 6*time.Second {
				t.Error("capture escaped exact unit or original admin allowance")
			}
			if mode == "timeout" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			if mode == "journal_error" {
				return nil, errors.New("private-journal-error-canary")
			}
			return []byte("private-journal-canary"), nil
		}
		f.nativeStartupFailure(providerForkNativeTarget{main: main}, "phase=initial reason=observation_expired", journal)
		return
	}
	for _, mode := range []string{"available", "changed", "journal_error", "missing", "existing", "spent", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			if os.Chmod(directory, 0700) != nil {
				t.Fatal("private control directory unavailable")
			}
			if mode == "missing" {
				directory = filepath.Join(directory, "absent")
			} else if mode == "existing" {
				managedSmokeWrite(t, filepath.Join(directory, "native-startup.json"), "private-existing-canary", 0600)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedProviderForkNativeRefusalEvidence$", "-test.v")
			command.Env = append(os.Environ(), "MANAGED_PROVIDER_NATIVE_EVIDENCE_CONTROL="+mode, "MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR="+directory)
			output, err := command.CombinedOutput()
			text := string(output)
			original := "managed native startup refused: phase=initial reason=observation_expired"
			if err == nil || ctx.Err() != nil || command.ProcessState == nil || command.ProcessState.ExitCode() != 1 || strings.Count(text, original) != 2 || !strings.Contains(text, "native_cleanup_joined") || strings.Contains(text, "--- PASS") || strings.Contains(text, "Error:") {
				t.Fatal("capture replaced the original refusal or skipped joined cleanup")
			}
			if entered := strings.Index(text, "capture_entered"); entered >= 0 && (entered < strings.Index(text, original) || entered > strings.Index(text, "native_cleanup_joined")) {
				t.Fatal("capture did not follow original refusal before cleanup")
			}
			for _, private := range []string{directory, "private-journal", "private-native-unit", "private-existing"} {
				if strings.Contains(text, private) {
					t.Fatal("native capture exposed private process or journal data")
				}
			}
			if mode == "available" || mode == "changed" || mode == "journal_error" {
				if !strings.Contains(text, "private_capture_verified") {
					t.Fatal("private capture was not verified before cleanup")
				}
			}
		})
	}
}

func TestManagedProviderForkNativeImage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private-image-canary")
	content := []byte("synthetic immutable image")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	digest := sha256.Sum256(content)
	expected := hex.EncodeToString(digest[:])
	image, err := providerForkVerifyNativeImage(path, expected)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || image.path != path || image.info == nil || !os.SameFile(info, image.info) {
		t.Fatal("verified image did not retain its actual opened-file identity")
	}
	copyPath := filepath.Join(root, "equal-content")
	if os.WriteFile(copyPath, content, 0600) != nil {
		t.Fatal("copy fixture unavailable")
	}
	if !image.matches(path) || image.matches(copyPath) || image.matches(filepath.Join(root, "missing")) {
		t.Fatal("shared naming image predicate accepted a foreign or missing file")
	}
	for _, candidate := range []struct{ path, digest string }{{path, strings.Repeat("0", 64)}, {root, expected}, {filepath.Join(root, "missing"), expected}} {
		image, err := providerForkVerifyNativeImage(candidate.path, candidate.digest)
		if err == nil || image.info != nil || image.path != "" || strings.Contains(err.Error(), root) {
			t.Fatal("failed image proof retained identity or exposed a private path")
		}
	}
}

func TestManagedProviderForkNativeStartup(t *testing.T) {
	selected, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	digest := managedSmokeHash(t, selected)
	image, err := providerForkVerifyNativeImage(selected, digest)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	// A byte-identical copy must not stand in for the verified executable.
	copyPath := filepath.Join(t.TempDir(), "private-native-canary")
	input, err := os.Open(selected)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	output, err := os.Create(copyPath)
	if err != nil {
		_ = input.Close()
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	_, copyErr := io.Copy(output, input)
	_ = input.Close()
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal("synthetic image copy failed")
	}
	copyImage, err := providerForkVerifyNativeImage(copyPath, digest)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	for _, mode := range []string{"selected", "nonselected", "foreign_identity", "missing_identity", "no_socket", "non_socket", "terminal_zero", "terminal_nonzero", "changed", "unavailable", "malformed", "foreign_unit", "contradictory", "raced", "late_socket", "late_sample", "late_recheck"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			record, release := filepath.Join(root, "record"), filepath.Join(root, "release")
			exit := 0
			if mode == "terminal_nonzero" {
				exit = 7
			}
			command := exec.Command(os.Args[0], "-test.run=^TestManagedProviderForkCLIChildFixture$")
			command.Env = append(os.Environ(), "MANAGED_PROVIDER_CLI_FIXTURE=native_process", "MANAGED_PROVIDER_CLI_RECORD="+record, "MANAGED_PROVIDER_NATIVE_RELEASE="+release, "MANAGED_PROVIDER_NATIVE_EXIT="+strconv.Itoa(exit), "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := command.Start(); err != nil {
				t.Fatal("fixture prerequisite or identity unavailable")
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			})
			namingTestWait(t, record)
			main, ok := managedSmokeProcessAt(command.Process.Pid)
			if !ok {
				t.Fatal("synthetic process lifetime unavailable")
			}
			target := providerForkNativeTarget{main: main, image: image, socket: filepath.Join(root, "private-socket-canary")}
			listen := func() {
				t.Helper()
				listener, err := net.Listen("unix", target.socket)
				if err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
				t.Cleanup(func() { _ = listener.Close() })
			}
			finish := func(reap bool) {
				t.Helper()
				if err := os.WriteFile(release, nil, 0600); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
				if reap {
					_ = command.Wait()
					waited = true
				} else {
					deadline := time.Now().Add(time.Second)
					for {
						_, state := providerForkNativeProcess(main)
						if state == "terminal" {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("synthetic terminal state unavailable")
						}
						time.Sleep(time.Millisecond)
					}
				}
			}
			if mode == "selected" || mode == "nonselected" || mode == "foreign_identity" || mode == "missing_identity" || mode == "late_sample" || mode == "late_recheck" {
				listen()
			}
			if mode == "nonselected" {
				target.image = copyImage
			} else if mode == "foreign_identity" {
				target.image.info = copyImage.info
			} else if mode == "missing_identity" {
				target.image.info = nil
			} else if mode == "non_socket" {
				if err := os.WriteFile(target.socket, nil, 0600); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
			} else if mode == "changed" {
				target.main.start = "0"
			} else if strings.HasPrefix(mode, "terminal_") {
				finish(false)
			} else if mode == "unavailable" {
				finish(true)
			}
			queries := 0
			readUnit := func() ([]byte, error) {
				queries++
				if mode == "unavailable" {
					return []byte("private-unit-canary"), fmt.Errorf("private-unit-canary")
				}
				pid, group, active, sub, result, code := main.pid, main.group, "active", "running", "success", 0
				if mode == "raced" {
					finish(true)
				}
				if strings.HasPrefix(mode, "terminal_") || mode == "raced" || mode == "contradictory" {
					pid, group, active, sub, code = 0, "", "inactive", "dead", 1
					if exit != 0 {
						active, sub, result = "failed", "failed", "exit-code"
					}
				} else if mode == "foreign_unit" {
					group = "/private-unit-canary"
				} else if mode == "late_socket" {
					listen()
				}
				data := fmt.Sprintf("MainPID=%d\nControlGroup=%s\nActiveState=%s\nSubState=%s\nResult=%s\nExecMainCode=%d\nExecMainStatus=%d\n", pid, group, active, sub, result, code, exit)
				if mode == "malformed" {
					data += "Result=private-unit-canary\n"
				}
				return []byte(data), nil
			}
			phase := "initial"
			if mode == "no_socket" {
				phase = "restart"
			}
			budget := 40 * time.Millisecond
			if mode == "selected" {
				budget = time.Second
			}
			samples := 0
			sample := func() providerForkNativeFacts {
				samples++
				facts := target.facts()
				if mode == "late_sample" || mode == "late_recheck" && samples == 2 {
					time.Sleep(60 * time.Millisecond)
				}
				return facts
			}
			failure := target.await(context.Background(), phase, budget, time.Now(), sample, readUnit)
			if mode == "selected" {
				if failure != "" || queries != 0 {
					t.Fatal("same selected process and socket were not quietly admitted")
				}
				return
			}
			if failure == "" || queries != 1 || !strings.Contains(failure, "phase="+phase+" ") {
				t.Fatal("startup refusal lost its single bounded sample")
			}
			for _, private := range []string{root, selected, "private-unit-canary", "private-native-canary", "private-socket-canary", "MainPID=", "ControlGroup="} {
				if strings.Contains(failure, private) {
					t.Fatal("startup sample exposed private process or unit data")
				}
			}
			var expected []string
			switch mode {
			case "nonselected", "foreign_identity":
				expected = []string{"reason=observation_expired", "executable=nonselected", "state=running"}
			case "no_socket", "non_socket":
				expected = []string{"executable=selected", "socket=absent", "state=running"}
				if mode == "non_socket" {
					expected[1] = "socket=non_socket"
				}
			case "terminal_zero", "terminal_nonzero":
				expected = []string{"reason=terminal_before_readiness", "process=same", "state=terminal", "code_known=true code=1", "status_known=true status=" + strconv.Itoa(exit)}
			case "changed", "foreign_unit":
				expected = []string{"process=changed", "state=unknown"}
			case "unavailable":
				expected = []string{"process=unobserved", "state=unknown", "unit_query=unavailable", "code_known=false"}
			case "malformed":
				expected = []string{"process=same", "unit_query=malformed", "result=unobserved", "code_known=false"}
			case "raced":
				expected = []string{"process=unobserved", "state=unknown", "unit_query=raced"}
			case "contradictory":
				expected = []string{"process=same", "state=unknown", "unit_query=raced"}
			case "late_socket":
				expected = []string{"reason=observation_expired", "socket=present", "state=running"}
			case "missing_identity":
				expected = []string{"executable=unobserved", "state=running"}
			case "late_sample", "late_recheck":
				expected = []string{"reason=observation_expired", "executable=selected", "socket=present"}
				want := 1
				if mode == "late_recheck" {
					want = 2
				}
				if samples != want {
					t.Fatal("expired sample started more observation work")
				}
			}
			for _, token := range expected {
				if !strings.Contains(failure, token) {
					t.Fatal("startup decision or evidence category contradicted its owned fixture")
				}
			}
		})
	}
}
func (f *managedProviderFork) goal(marker string) string {
	path := filepath.Join(f.smoke.root, marker+".txt")
	managedSmokeWrite(f.t, path, marker+"\nReturn the fixed synthetic answer without tools.\n", 0600)
	return path
}
func (f *managedProviderFork) summary(slug string) *session.Summary {
	f.t.Helper()
	summary, err := session.Find(f.workspace, slug)
	if err != nil || summary.Codex.ThreadID == "" {
		f.t.Fatal("supported original summary unavailable")
	}
	return summary
}
func (f *managedProviderFork) assign(slug, address, marker string) {
	f.cli("team", "assign", slug, "--as-is", "--from", "lead", "--to", address, "--message", marker)
}
func (f *managedProviderFork) read(expected providerForkCutRequest) []byte {
	f.t.Helper()
	f.t.Helper()
	data, err := managedSmokeRead(f.operationPath(expected.operation), 16<<20)
	if err != nil {
		f.operationFailure("operation_snapshot", expected, "result=read_unavailable")
	}
	return data
}
func (f *managedProviderFork) operationPath(id string) string {
	return filepath.Join(f.state, "providers", f.name, id+".snapshot.json")
}

func (f *managedProviderFork) settingsSinkAbsent(expected providerForkCutRequest) bool {
	_, err := os.Lstat(f.operationPath(expected.operation) + providerForkSettingsEvidenceSuffix)
	return os.IsNotExist(err)
}

func (f *managedProviderFork) prepareSettingsEvidence(expected providerForkCutRequest, epoch string, frozen []byte) {
	f.settingsSource = nil
	if !f.settingsCutAbsent || !f.settingsSinkAbsent(expected) {
		return
	}
	var source providerForkSnapshot
	data, err := readProviderForkJSON(f.operationPath(expected.operation), &source)
	catalog, catalogErr := codexproviders.ReadCatalog(filepath.Join(f.smoke.root, "registry", "providers.json"))
	if err != nil || catalogErr != nil || !bytes.Equal(data, frozen) || validateProviderForkSnapshot(source, f.operationPath(expected.operation), catalog) != nil || source.OperationID != expected.operation || source.SourcePackage != f.smoke.manifest.PackageRoot || epoch == "" || epoch == source.SocketIdentity {
		return
	}
	f.settingsSource, f.settingsDigest, f.settingsEpoch = &source, providerForkDigest(data), epoch
	f.settingsPath = f.operationPath(expected.operation) + providerForkSettingsEvidenceSuffix
}

// This evidence is mandatory failure handoff, not optional state projection.
// The original refusal is already logged; expired operational time does not
// suppress a bounded private copy while the original hard tail is still live.
func (f *managedProviderFork) handoffSettingsEvidence() {
	f.t.Helper()
	outcome := "unavailable"
	if f.hard == nil || managedSmokeExpired(f.hard) != nil {
		f.t.Log("phase=settings_evidence outcome=not_sampled_deadline")
		return
	}
	defer func() { f.t.Log("phase=settings_evidence outcome=" + outcome) }()
	if f.settingsSource == nil {
		return
	}
	directory := os.Getenv("MANAGED_PROVIDER_SETTINGS_EVIDENCE_DIR")
	info, err := os.Lstat(directory)
	if err != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !info.IsDir() || info.Mode().Perm() != 0700 {
		return
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return
	}
	before, err := os.Lstat(f.settingsPath)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || before.Size() > providerForkSettingsEvidenceLimit {
		return
	}
	fileOwner, ok := before.Sys().(*syscall.Stat_t)
	if !ok || int(fileOwner.Uid) != os.Geteuid() {
		return
	}
	var diagnostic providerForkSettingsDiagnostic
	data, err := readProviderForkJSON(f.settingsPath, &diagnostic)
	if err != nil || len(data) > providerForkSettingsEvidenceLimit || !diagnostic.matches(*f.settingsSource, f.settingsDigest, f.settingsEpoch) {
		return
	}
	after, err := os.Lstat(f.settingsPath)
	if err != nil || !os.SameFile(before, after) || managedSmokeExpired(f.hard) != nil {
		return
	}
	if providerForkWriteFirstPrivate(filepath.Join(directory, "settings-mismatch.json"), data) == nil {
		outcome = "available"
	}
}

type providerForkCutRequest struct {
	operation, target, session string
	inherit                    bool
}

type providerForkCutState struct {
	Catalog, State, Receipt, Operation, Phase, Stage string
	Snapshot, SnapshotIdentity                       string
	Journal, JournalIdentity, JournalStage           string
	CountsKnown                                      bool
	NotAttempted, Intent, KnownChild, Verified       int
	Armed                                            bool
}

func providerForkCutReadStatus(err error, opaque string) string {
	if os.IsNotExist(err) {
		return "absent"
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return "read_unavailable"
	}
	return opaque
}

func providerForkCutEnum(value string, allowed ...string) string {
	if slices.Contains(allowed, value) {
		return value
	}
	return "unknown"
}

func (f *managedProviderFork) cutState(expected providerForkCutRequest) string {
	sample := providerForkCutState{Catalog: "not_sampled", State: "not_sampled", Receipt: "unknown", Operation: "unknown", Phase: "unknown", Stage: "unknown", Snapshot: "not_sampled", SnapshotIdentity: "unknown", Journal: "not_sampled", JournalIdentity: "unknown", JournalStage: "unknown", NotAttempted: -1, Intent: -1, KnownChild: -1, Verified: -1}
	finish := func() string {
		f.mu.Lock()
		sample.Armed = f.cutOperation == expected.operation
		f.mu.Unlock()
		data, _ := json.Marshal(sample) // Only fixed categories, booleans/counts.
		return string(data)
	}
	catalog, err := codexproviders.ReadCatalog(filepath.Join(f.smoke.root, "registry", "providers.json"))
	if err != nil {
		sample.Catalog = providerForkCutReadStatus(err, "rejected")
		return finish()
	}
	sample.Catalog = "valid"
	state, err := codexproviders.ReadBindingState(f.state, f.name, f.workspace, f.socket, catalog)
	if err != nil {
		sample.State = providerForkCutReadStatus(err, "rejected")
	} else {
		sample.State, sample.Receipt, sample.Operation = "valid", "absent", "absent"
		if receipt, ok := state.Receipts[expected.operation]; ok {
			sample.Receipt = "mismatch"
			if receipt.Target == expected.target && receipt.Session == expected.session && receipt.Inherit == expected.inherit {
				sample.Receipt = "match"
			}
		}
		if operation := state.Operation; operation != nil {
			sample.Operation = "other"
			if operation.ID == expected.operation {
				sample.Operation = "mismatch"
				if operation.Target == expected.target && operation.Session == expected.session && operation.Inherit == expected.inherit {
					sample.Operation = "exact"
					sample.Phase = providerForkCutEnum(operation.Phase, "waiting", "applying", "applied", "failed", "canceled", "recovery-required")
					sample.Stage = providerForkCutEnum(operation.Stage, "accepted", "waiting-creation", "waiting-package", "draining", "preparing", "forking", "verifying", "committing", "restoring-terminals")
				}
			}
		}
	}
	var source providerForkSnapshot
	path := f.operationPath(expected.operation)
	sourceBytes, err := readProviderForkJSON(path, &source)
	if err != nil {
		sample.Snapshot = providerForkCutReadStatus(err, "rejected")
		return finish()
	}
	if validateProviderForkSnapshot(source, path, catalog) != nil {
		sample.Snapshot = "rejected"
		return finish()
	}
	sample.Snapshot, sample.SnapshotIdentity = "valid", "mismatch"
	operation := source.State.Operation
	if source.OperationID != expected.operation || source.Workspace != f.name || source.Root != f.workspace || source.Socket != f.socket || source.SourcePackage != f.smoke.manifest.PackageRoot || operation == nil || operation.Target != expected.target || operation.Session != expected.session || operation.Inherit != expected.inherit {
		sample.Snapshot = "rejected"
		return finish()
	}
	sample.SnapshotIdentity = "match"
	data, err := managedSmokeRead(path+".journal", 16<<20)
	if err != nil {
		sample.Journal = providerForkCutReadStatus(err, "read_unavailable")
		return finish()
	}
	// The live operation may have advanced. The journal reserves this original
	// source and byte/package proof, not the later live state's revision.
	journal, err := codexproviders.DecodeForkJournal(data, source.State, catalog)
	if err != nil {
		sample.Journal = "rejected"
		return finish()
	}
	sample.JournalIdentity = "mismatch"
	if journal.SourceSnapshotDigest != providerForkDigest(sourceBytes) || journal.SourcePackage != source.SourcePackage {
		sample.Journal = "rejected"
		return finish()
	}
	sample.Journal, sample.JournalIdentity = "valid", "match"
	sample.JournalStage = providerForkCutEnum(journal.Stage, "preparing", "forking", "verifying", "committing", "restoring-terminals")
	sample.CountsKnown = true
	sample.NotAttempted, sample.Intent, sample.KnownChild, sample.Verified = 0, 0, 0, 0
	for _, participant := range journal.Participants {
		switch participant.State {
		case "not-attempted":
			sample.NotAttempted++
		case "intent":
			sample.Intent++
		case "known-child":
			sample.KnownChild++
		case "verified":
			sample.Verified++
		}
	}
	return finish()
}

// Reuse the real transaction fixture's frozen source and durable known child;
// only relocate its artifacts to the managed fixture's existing exact paths.
func managedProviderForkCutFixture(t *testing.T) (*managedProviderFork, providerForkCutRequest, providerForkSnapshot, codexproviders.ForkJournal) {
	t.Helper()
	transaction := newForkProviderFixture(t)
	transaction.childReadFailure = "one-child"
	if err := transaction.run(); err == nil {
		t.Fatal("synthetic dependent read unexpectedly succeeded")
	}
	journal := transaction.journal()
	f := &managedProviderFork{t: t, state: transaction.root, workspace: transaction.root, name: "work", socket: transaction.source.Socket, cut: make(chan codexproviders.ForkParticipant, 1)}
	f.smoke = &managedSmokeHarness{t: t, root: transaction.root, manifest: managedSmokeManifest{PackageRoot: transaction.source.SourcePackage}}
	expected := providerForkCutRequest{operation: transaction.source.OperationID, target: "pool"}
	f.arm(expected.operation)
	catalogPath := filepath.Join(f.smoke.root, "registry", "providers.json")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0700); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	if err := providerAtomicJSON(catalogPath, transaction.catalog); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	catalog, err := codexproviders.ReadCatalog(catalogPath)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	path := f.operationPath(expected.operation)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	source := transaction.source
	source.CatalogDigest, source.State.Operation.CatalogDigest, source.State.Operation.Snapshot = catalog.Digest, catalog.Digest, path
	if err := providerAtomicJSON(path, source); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	sourceBytes, err := readProviderForkJSON(path, new(providerForkSnapshot))
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	journal.CatalogDigest, journal.SourceSnapshotDigest = catalog.Digest, providerForkDigest(sourceBytes)
	if err := writeProviderForkJournal(path+".journal", journal, source, catalog); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	state := transaction.state
	state.Operation.CatalogDigest, state.Operation.Snapshot = catalog.Digest, path
	state.Receipts = map[string]codexproviders.RequestReceipt{expected.operation: {Target: expected.target, CatalogDigest: catalog.Digest}}
	statePath, err := codexproviders.StatePath(f.state, f.name)
	if err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	if err := providerAtomicJSON(statePath, state); err != nil {
		t.Fatal("fixture prerequisite or identity unavailable")
	}
	return f, expected, source, journal
}

func managedProviderForkSettingsFixture(t *testing.T, child ...bool) *managedProviderFork {
	t.Helper()
	f, expected, _, _ := managedProviderForkCutFixture(t)
	frozen, err := os.ReadFile(f.operationPath(expected.operation))
	if err != nil {
		t.Fatal("settings source unavailable")
	}
	f.settingsCutAbsent = f.settingsSinkAbsent(expected)
	if os.Chmod(f.socket, 0600) != nil {
		t.Fatal("replacement endpoint control unavailable")
	}
	epoch, err := session.SocketIdentity(f.socket)
	if err != nil {
		t.Fatal("replacement endpoint identity unavailable")
	}
	f.prepareSettingsEvidence(expected, epoch, frozen)
	if f.settingsSource == nil {
		t.Fatal("validated source evidence binding unavailable")
	}
	p := f.settingsSource.Participants[0]
	key, checkpoint := "approvalPolicy", "before-participants"
	if len(child) > 0 && child[0] {
		key, checkpoint = "collaborationMode", "participant-child-absent"
	}
	value, present := p.Thread.Original[key]
	mismatch := &providerForkSettingsMismatch{Checkpoint: checkpoint, Participant: 0, Role: "lead", Differences: []providerForkSettingsDifference{{Key: key, FrozenPresent: present, CurrentPresent: true, Frozen: value, Current: "private-current-canary"}}}
	retainProviderForkSettingsDiagnostic(f.operationPath(expected.operation), *f.settingsSource, f.settingsDigest, epoch, mismatch)
	if mismatch.evidence != "available" {
		t.Fatal("private evidence writer unavailable")
	}
	return f
}

func TestManagedProviderForkSettingsHandoff(t *testing.T) {
	for _, mode := range []string{"expired_operation", "child_expired_operation", "spent_hard", "stale_epoch", "malformed", "replaced", "existing_destination", "unproved_absence"} {
		t.Run(mode, func(t *testing.T) {
			f := managedProviderForkSettingsFixture(t, mode == "child_expired_operation")
			directory := t.TempDir()
			if os.Chmod(directory, 0700) != nil {
				t.Fatal("private evidence directory unavailable")
			}
			t.Setenv("MANAGED_PROVIDER_SETTINGS_EVIDENCE_DIR", directory)
			operation, stopOperation := context.WithCancel(context.Background())
			stopOperation()
			f.smoke.ctx = operation
			hard, stopHard := context.WithTimeout(context.Background(), time.Second)
			defer stopHard()
			f.hard = hard
			original, err := os.ReadFile(f.settingsPath)
			if err != nil {
				t.Fatal("private evidence unavailable")
			}
			var check providerForkSettingsDiagnostic
			if _, err := readProviderForkJSON(f.settingsPath, &check); err != nil {
				t.Fatal("private diagnostic decoding failed")
			}
			if !check.matches(*f.settingsSource, f.settingsDigest, f.settingsEpoch) {
				t.Fatal("private diagnostic binding failed")
			}
			destination := filepath.Join(directory, "settings-mismatch.json")
			switch mode {
			case "spent_hard":
				stopHard()
			case "stale_epoch":
				f.settingsEpoch += "-different"
			case "unproved_absence":
				f.settingsSource = nil
			case "malformed":
				if os.WriteFile(f.settingsPath, []byte("private-malformed-canary"), 0600) != nil {
					t.Fatal("malformed evidence fixture unavailable")
				}
			case "replaced":
				if os.Remove(f.settingsPath) != nil || os.Symlink(filepath.Join(directory, "other"), f.settingsPath) != nil {
					t.Fatal("replacement evidence fixture unavailable")
				}
			case "existing_destination":
				if os.WriteFile(destination, []byte("private-existing-canary"), 0600) != nil {
					t.Fatal("existing destination fixture unavailable")
				}
			}
			f.handoffSettingsEvidence()
			copied, err := os.ReadFile(destination)
			switch mode {
			case "expired_operation", "child_expired_operation":
				info, statErr := os.Stat(destination)
				if err != nil || statErr != nil || !bytes.Equal(original, copied) || info.Mode().Perm() != 0600 {
					t.Fatal("spent operation suppressed exact private handoff under live hard tail")
				}
			case "existing_destination":
				if err != nil || string(copied) != "private-existing-canary" {
					t.Fatal("handoff overwrote or adopted prior evidence")
				}
			default:
				if !os.IsNotExist(err) {
					t.Fatal("unproved evidence was handed off")
				}
			}
		})
	}
}

func TestManagedProviderForkCutState(t *testing.T) {
	f, expected, source, journal := managedProviderForkCutFixture(t)
	statePath, _ := codexproviders.StatePath(f.state, f.name)
	path := f.operationPath(expected.operation)
	baseline := map[string][]byte{}
	for _, name := range []string{statePath, path, path + ".journal", filepath.Join(f.smoke.root, "registry", "providers.json")} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal("fixture prerequisite or identity unavailable")
		}
		baseline[name] = data
	}
	for _, mode := range []string{"known", "waiting", "failed_later_live", "intent", "state_absent", "catalog_rejected", "snapshot_rejected", "snapshot_mismatch", "journal_absent", "journal_unavailable", "journal_rejected", "journal_proof_mismatch", "receipt_mismatch", "operation_other", "unknown_enum", "disarmed"} {
		t.Run(mode, func(t *testing.T) {
			for name, data := range baseline {
				_ = os.RemoveAll(name)
				managedSmokeWrite(t, name, string(data), 0600)
			}
			f.arm(expected.operation)
			f.smoke.manifest.PackageRoot = source.SourcePackage
			var live codexproviders.State
			if err := json.Unmarshal(baseline[statePath], &live); err != nil {
				t.Fatal("fixture prerequisite or identity unavailable")
			}
			currentJournal := journal
			currentJournal.Participants = append([]codexproviders.ForkParticipant(nil), journal.Participants...)
			switch mode {
			case "waiting":
				live.Operation.Phase, live.Operation.Stage = "waiting", "accepted"
			case "failed_later_live":
				live.Operation.Phase, live.BindingRevision = "failed", live.BindingRevision+5
			case "intent":
				currentJournal.Stage = "forking"
				currentJournal.Participants[0].State, currentJournal.Participants[0].ChildThreadID = "intent", ""
			case "snapshot_mismatch":
				f.smoke.manifest.PackageRoot = "/nix/store/private-package-canary"
			case "journal_proof_mismatch":
				currentJournal.SourceSnapshotDigest = strings.Repeat("c", 64)
			case "receipt_mismatch":
				live.Receipts[expected.operation] = codexproviders.RequestReceipt{Target: "openai", CatalogDigest: live.Operation.CatalogDigest}
			case "operation_other":
				live.Operation.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			case "unknown_enum":
				live.Operation.Stage = "private-enum-canary"
			case "disarmed":
				f.arm("")
			}
			if err := providerAtomicJSON(statePath, live); err != nil {
				t.Fatal("fixture prerequisite or identity unavailable")
			}
			if err := providerAtomicJSON(path+".journal", currentJournal); err != nil {
				t.Fatal("fixture prerequisite or identity unavailable")
			}
			switch mode {
			case "state_absent":
				_ = os.Remove(statePath)
			case "catalog_rejected":
				managedSmokeWrite(t, filepath.Join(f.smoke.root, "registry", "providers.json"), "private-payload-canary", 0600)
			case "snapshot_rejected":
				managedSmokeWrite(t, path, "private-payload-canary", 0600)
			case "journal_absent":
				_ = os.Remove(path + ".journal")
			case "journal_unavailable":
				_ = os.Remove(path + ".journal")
				if err := os.Mkdir(path+".journal", 0700); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
			case "journal_rejected":
				managedSmokeWrite(t, path+".journal", "private-payload-canary", 0600)
			}
			text := f.cutState(expected)
			for _, private := range []string{f.state, f.socket, expected.operation, source.SourcePackage, "private-package-canary", "private-payload-canary", "private-enum-canary"} {
				if strings.Contains(text, private) {
					t.Fatal("cut-state projection exposed private data")
				}
			}
			var sample providerForkCutState
			if json.Unmarshal([]byte(text), &sample) != nil || sample.Armed != (mode != "disarmed") {
				t.Fatal("cut-state projection lost its typed arm sample")
			}
			unknownCounts := strings.HasPrefix(mode, "journal_") || strings.HasPrefix(mode, "snapshot_") || mode == "catalog_rejected"
			if unknownCounts {
				if sample.CountsKnown || sample.NotAttempted != -1 || sample.Intent != -1 || sample.KnownChild != -1 || sample.Verified != -1 {
					t.Fatal("rejected progress became observed zero or partial counts")
				}
			} else if !sample.CountsKnown || sample.NotAttempted != 1 || mode == "intent" && sample.Intent != 1 || mode != "intent" && sample.KnownChild != 1 {
				t.Fatal("projection did not validate the original frozen journal")
			}
			switch mode {
			case "catalog_rejected":
				if sample.Catalog != "rejected" || sample.State != "not_sampled" {
					t.Fatal("rejected catalog acquired dependent facts")
				}
			case "journal_absent":
				if sample.Journal != "absent" {
					t.Fatal("absent journal lost")
				}
			case "journal_unavailable":
				if sample.Journal != "read_unavailable" {
					t.Fatal("unreadable journal became absence or invalidity")
				}
			case "waiting":
				if sample.Phase != "waiting" || sample.Stage != "accepted" {
					t.Fatal("accepted state not retained")
				}
			case "failed_later_live":
				if sample.Phase != "failed" || sample.Journal != "valid" {
					t.Fatal("later live state replaced frozen journal reservation")
				}
			case "state_absent":
				if sample.State != "absent" || sample.Operation != "unknown" {
					t.Fatal("missing state synthesized a legacy result")
				}
			case "snapshot_mismatch":
				if sample.SnapshotIdentity != "mismatch" {
					t.Fatal("typed package identity mismatch lost")
				}
			case "journal_proof_mismatch":
				if sample.JournalIdentity != "mismatch" {
					t.Fatal("typed snapshot byte proof mismatch lost")
				}
			case "journal_rejected", "snapshot_rejected":
				if sample.JournalIdentity != "unknown" || mode == "snapshot_rejected" && sample.SnapshotIdentity != "unknown" {
					t.Fatal("opaque rejection invented identity mismatch")
				}
			case "receipt_mismatch":
				if sample.Receipt != "mismatch" {
					t.Fatal("request tuple mismatch lost")
				}
			case "operation_other":
				if sample.Operation != "other" || sample.Phase != "unknown" {
					t.Fatal("other operation projected as this request")
				}
			}
		})
	}
}

func TestManagedProviderForkOperationFailure(t *testing.T) {
	if mode := os.Getenv("MANAGED_PROVIDER_UNIT_FIXTURE"); mode != "" {
		switch mode {
		case "canceled_query":
			_, _ = io.Copy(io.Discard, os.Stdin)
		case "unavailable":
			os.Exit(1)
		case "malformed":
			fmt.Print("Result=private-unit-result-canary\n")
		case "terminal":
			fmt.Print("MainPID=0\nControlGroup=\nActiveState=inactive\nSubState=dead\nResult=success\nExecMainCode=2\nExecMainStatus=15\n")
		case "portal":
			pid, err := strconv.Atoi(os.Getenv("MANAGED_PROVIDER_UNIT_PID"))
			if err != nil || pid <= 0 {
				os.Exit(1)
			}
			fmt.Printf("MainPID=%d\nControlGroup=%s\nActiveState=active\nSubState=running\nResult=success\nExecMainCode=0\nExecMainStatus=0\n", pid, os.Getenv("MANAGED_PROVIDER_UNIT_GROUP"))
		default:
			fmt.Print("MainPID=0\nControlGroup=/private-group-canary\nActiveState=inactive\nSubState=dead\nResult=success\nExecMainCode=2\nExecMainStatus=15\n")
		}
		os.Exit(0)
	}
	if mode := os.Getenv("MANAGED_PROVIDER_FAILURE_FIXTURE"); mode != "" {
		f, expected, _, _ := managedProviderForkCutFixture(t)
		if mode == "settings_expired" {
			f = managedProviderForkSettingsFixture(t)
			expected = providerForkCutRequest{operation: f.settingsSource.OperationID, target: f.settingsSource.State.Operation.Target}
			directory := t.TempDir()
			if os.Chmod(directory, 0700) != nil {
				t.Fatal("private evidence directory unavailable")
			}
			t.Setenv("MANAGED_PROVIDER_SETTINGS_EVIDENCE_DIR", directory)
			t.Cleanup(func() {
				if _, err := os.Stat(filepath.Join(directory, "settings-mismatch.json")); err != nil {
					t.Error("private handoff missing before cleanup")
				} else {
					t.Log("settings_copy_verified")
				}
			})
		}
		t.Cleanup(func() { t.Log("cleanup_continued") })
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		if mode == "deadline" {
			cancel()
			parent, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
			defer cancel()
		}
		f.smoke.ctx = parent
		hardBudget := 4 * time.Second
		if mode == "active" {
			hardBudget = 10 * time.Second
		}
		if mode == "spent_hard" {
			hardBudget = time.Second
		} else if mode == "canceled_query" {
			hardBudget = 2*time.Second + 60*time.Millisecond
		}
		var stop context.CancelFunc
		f.hard, stop = context.WithTimeout(context.Background(), hardBudget)
		defer stop()
		f.units = map[string]*managedSmokeUnit{"provider": {name: "private-unit-canary"}}
		if mode == "absent_unit" {
			delete(f.units, "provider")
		}
		statePath, _ := codexproviders.StatePath(f.state, f.name)
		data, err := os.ReadFile(statePath)
		var state codexproviders.State
		if err != nil || json.Unmarshal(data, &state) != nil {
			t.Fatal("operation fixture unavailable")
		}
		state.Operation.Phase = "failed"
		if providerAtomicJSON(statePath, state) != nil {
			t.Fatal("operation fixture unavailable")
		}
		if mode == "expired" || mode == "spent_hard" {
			cancel()
		}
		last, waitErr := providerForkOperationWait(parent, func(context.Context) (bool, error) {
			_, ready, err := f.operationState(expected)
			return ready, err
		})
		contextAtReturn := managedSmokeExpired(parent)
		if mode == "settings_expired" {
			cancel()
		}
		if mode == "projection_unavailable" {
			managedSmokeWrite(t, filepath.Join(f.smoke.root, "registry", "providers.json"), "private-failure-canary", 0600)
		}
		query := func(ctx context.Context, args ...string) ([]byte, error) {
			t.Log("query_entered")
			deadline, ok := ctx.Deadline()
			hard, _ := f.hard.Deadline()
			if !ok || deadline.After(hard.Add(-2*time.Second)) || time.Until(deadline) > 6*time.Second || mode == "active" && time.Until(deadline) < 5*time.Second || len(args) != 3 || args[1] != f.units["provider"].name {
				t.Fatal("worker query escaped its exact owner or hard tail")
			}
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedProviderForkOperationFailure$")
			command.Env = append(os.Environ(), "MANAGED_PROVIDER_UNIT_FIXTURE="+mode)
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal("query fixture unavailable")
			}
			defer input.Close()
			defer writer.Close()
			command.Stdin = input
			data, err := managedSmokeCommandResult(ctx, command)
			t.Log("query_joined")
			if mode == "canceled_query" && (err == nil || ctx.Err() == nil || command.ProcessState == nil) {
				t.Fatal("worker query cancellation was not joined")
			}
			if mode == "later_cancel" {
				cancel()
			}
			return data, err
		}
		f.operationFailureAt("operation_apply", "result="+managedSmokeContextResult(waitErr)+" refusal="+providerForkOperationRefusal(waitErr), contextAtReturn, last, query, func() {
			t.Log("sampler_entered")
			f.operationSample(expected)
		})
		return
	}
	for _, mode := range []string{"active", "expired", "deadline", "spent_hard", "absent_unit", "unavailable", "malformed", "canceled_query", "projection_unavailable", "later_cancel", "settings_expired", "portal_selected", "portal_other", "portal_unavailable", "portal_raced", "portal_terminal", "portal_malformed", "portal_spent"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedProviderForkOperationFailure$", "-test.v")
			command.Env = append(os.Environ(), "MANAGED_PROVIDER_FAILURE_FIXTURE="+mode)
			if strings.HasPrefix(mode, "portal_") {
				command.Args = []string{os.Args[0], "-test.run=^TestManagedProviderForkPortalReadiness$", "-test.v"}
				command.Env = append(os.Environ(), "MANAGED_PROVIDER_PORTAL_CONTROL="+mode)
			}
			var output managedSmokeOutput
			command.Stdout, command.Stderr = &output, &output
			command.WaitDelay = 2 * time.Second
			err := command.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil || output.overflow {
				t.Fatal("failure-owner fixture did not return its expected failure")
			}
			text := string(output.data)
			if mode == "settings_expired" {
				first := strings.Index(text, "phase=operation_apply parent=")
				copy := strings.Index(text, "phase=settings_evidence outcome=available")
				if first < 0 || copy <= first || !strings.Contains(text, "settings_copy_verified") || !strings.Contains(text, "cleanup_continued") || strings.Contains(text, "private-current-canary") || strings.Contains(text, "sampler_entered") {
					t.Fatal("original refusal, expired-operation handoff or cleanup ownership changed")
				}
				return
			}
			if strings.HasPrefix(mode, "portal_") {
				first := strings.Index(text, "boundary=return")
				sample := strings.Index(text, "phase=portal_sample startup=")
				if first < 0 || sample <= first || !strings.Contains(text, "cleanup_continued") || !strings.Contains(text, "portal_owner_joined") {
					t.Fatal("portal result did not precede its sample and continued owned cleanup")
				}
				query, joins := strings.Count(text, "portal_query_entered"), strings.Count(text, "portal_query_joined")
				if mode == "portal_spent" {
					if query != 0 || joins != 0 || !strings.Contains(text, "query=not_sampled_deadline") {
						t.Fatal("spent hard tail started portal evidence")
					}
				} else if query != 1 || joins != 1 || strings.Index(text, "portal_query_entered") <= sample {
					t.Fatal("portal sample did not own one joined exact query")
				}
				want := map[string]string{"portal_selected": "image=selected_portal state=running query=available", "portal_other": "image=other_image state=running query=available", "portal_unavailable": "image=unavailable state=running query=unavailable", "portal_raced": "image=raced state=unknown query=raced", "portal_terminal": "image=unavailable state=terminal query=available", "portal_malformed": "image=selected_portal state=running query=malformed"}[mode]
				if want != "" && !strings.Contains(text, want) {
					t.Fatal("portal image/unit sample lost its precise known or unknown category")
				}
				clocks := map[string]map[string]int64{}
				for _, line := range strings.Split(text, "\n") {
					if !strings.Contains(line, "boundary=") || !strings.Contains(line, "startup=restart") {
						continue
					}
					values, boundary := map[string]int64{}, ""
					for _, field := range strings.Fields(line) {
						key, value, _ := strings.Cut(field, "=")
						if key == "boundary" {
							boundary = value
						}
						if number, err := strconv.ParseInt(value, 10, 64); err == nil {
							values[key] = number
						}
					}
					if clocks[boundary] == nil {
						clocks[boundary] = values
					}
				}
				entry, health, returned := clocks["entry"], clocks["health_entry"], clocks["return"]
				if len(entry) == 0 || len(health) == 0 || len(returned) == 0 || entry["remaining_ms"] < health["remaining_ms"] || health["remaining_ms"] < returned["remaining_ms"] || returned["remaining_ms"] < 0 || returned["total_elapsed_ms"] < health["total_elapsed_ms"] || returned["total_elapsed_ms"] < returned["health_elapsed_ms"] || health["health_elapsed_ms"] < 0 {
					t.Fatal("portal boundary clocks renewed or confused their original budget")
				}
				// Both values use the same monotonic clock; allow millisecond rounding.
				arithmetic := entry["remaining_ms"] - returned["remaining_ms"] - (returned["total_elapsed_ms"] - entry["total_elapsed_ms"])
				if arithmetic < -2 || arithmetic > 2 {
					t.Fatal("portal remaining budget and elapsed clock disagree")
				}
				for _, private := range []string{"private-unit-canary", "private-unit-result-canary", "private-group-canary", "pfrdy-"} {
					if strings.Contains(text, private) {
						t.Fatal("portal sample exposed private data")
					}
				}
				return
			}
			reason := strings.Index(text, "phase=operation_apply parent=")
			worker := strings.Index(text, "phase=worker_result sample=entry")
			sample := strings.Index(text, "sampler_entered")
			projection := strings.Index(text, "after_refusal, non_atomic")
			if reason < 0 || worker <= reason || !strings.Contains(text, "cleanup_continued") {
				t.Fatal("original result did not precede evidence and continued cleanup")
			}
			spent := mode == "expired" || mode == "deadline" || mode == "spent_hard"
			if spent {
				category := "canceled"
				if mode == "deadline" {
					category = "deadline_exceeded"
				}
				if !strings.Contains(text, "parent="+category+" result="+category+" refusal=unobserved predicate_sampled=false") {
					t.Fatal("spent-owner result was not retained")
				}
			} else if !strings.Contains(text, "parent=active result=refused refusal=operation_terminal_failed predicate_sampled=true") {
				t.Fatal("original typed result was replaced by later context")
			}
			queries := strings.Count(text, "query_entered")
			joins := strings.Count(text, "query_joined")
			if mode == "spent_hard" || mode == "absent_unit" {
				if queries != 0 || joins != 0 || !strings.Contains(text, "query=not_sampled_") {
					t.Fatal("unavailable hard tail or owner started query work")
				}
			} else if queries != 1 || joins != 1 || strings.Index(text, "query_entered") <= worker {
				t.Fatal("worker sample did not have exactly one joined query")
			}
			if spent || mode == "later_cancel" {
				if sample >= 0 || projection >= 0 || !strings.Contains(text, "operation failure sample: not_sampled_deadline") {
					t.Fatal("expired operational parent entered optional decoding")
				}
			} else if sample <= worker || projection <= sample || joins == 1 && strings.Index(text, "query_joined") >= sample {
				t.Fatal("projection preceded original result or worker query")
			}
			if mode == "active" && (!strings.Contains(text, `"CountsKnown":true`) || !strings.Contains(text, "query=available active=inactive sub=dead result=success code_known=true code=2 status_known=true status=15")) {
				t.Fatal("valid safe sample was lost")
			}
			if mode == "projection_unavailable" && (!strings.Contains(text, `"Catalog":"rejected"`) || !strings.Contains(text, `"CountsKnown":false`)) {
				t.Fatal("unavailable projection became positive evidence")
			}
			wantQuery := map[string]string{"unavailable": "unavailable", "canceled_query": "unavailable", "malformed": "malformed"}[mode]
			if wantQuery != "" && !strings.Contains(text, "query="+wantQuery+" active=unobserved") {
				t.Fatal("worker uncertainty was not retained")
			}
			for _, private := range []string{"private-package-canary", "private-payload-canary", "privatemembercanary", "private-failure-canary", "private-unit-canary", "private-unit-result-canary", "private-group-canary"} {
				if strings.Contains(text, private) {
					t.Fatal("failure owner exposed private data")
				}
			}
		})
	}
}

// Cross the deadline between the completion and admission checks without a
// scheduler race or a real long wait. The actual shared wait still decides.
type providerForkAdmissionBoundaryContext struct {
	context.Context
	checks int
}

func (ctx *providerForkAdmissionBoundaryContext) Deadline() (time.Time, bool) {
	ctx.checks++
	if ctx.checks > 2 {
		return time.Now().Add(-time.Millisecond), true
	}
	return time.Now().Add(time.Second), true
}

func TestManagedProviderForkOperationWait(t *testing.T) {
	t.Run("deadline_ownership", func(t *testing.T) {
		hard, stop := context.WithTimeout(context.Background(), 31*time.Second)
		defer stop()
		operational, cancel, err := providerForkOperationalContext(hard)
		if err != nil {
			t.Fatal("operational deadline unavailable")
		}
		defer cancel()
		hardDeadline, _ := hard.Deadline()
		deadline, _ := operational.Deadline()
		if !deadline.Equal(hardDeadline.Add(-30 * time.Second)) {
			t.Fatal("operational deadline renewed or lost its reporting tail")
		}
		cancel()
		if hard.Err() != nil {
			t.Fatal("operational cancellation spent the hard reporting parent")
		}
		short, stopShort := context.WithTimeout(context.Background(), time.Millisecond)
		defer stopShort()
		if _, _, err := providerForkOperationalContext(short); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("spent operational allocation admitted setup")
		}
		if _, _, err := providerForkOperationalContext(context.Background()); err == nil {
			t.Fatal("missing test deadline became unbounded")
		}
	})
	for _, mode := range []string{"late_ready", "masked_terminal", "admission_boundary"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			var parent context.Context = ctx
			if mode == "admission_boundary" {
				parent = &providerForkAdmissionBoundaryContext{Context: context.Background()}
			}
			last, err := providerForkOperationWait(parent, func(ctx context.Context) (bool, error) {
				if mode == "admission_boundary" {
					return true, nil
				}
				<-ctx.Done()
				if mode == "masked_terminal" {
					return false, providerForkOperationError("operation_terminal_failed")
				}
				return true, nil
			})
			if !errors.Is(err, context.DeadlineExceeded) || !last.sampled {
				t.Fatal("expiry lost precedence over actual completed predicate")
			}
			want := map[string]string{"late_ready": "predicate=late_ready_refused", "masked_terminal": "predicate_refusal=operation_terminal_failed", "admission_boundary": "predicate=admission_boundary_expired"}[mode]
			if !strings.Contains(last.text(), want) || mode == "admission_boundary" && last.context != nil {
				t.Fatal("completed sample was confused with wait admission result")
			}
		})
	}

	f, expected, _, journal := managedProviderForkCutFixture(t)
	statePath, _ := codexproviders.StatePath(f.state, f.name)
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal("operation fixture unavailable")
	}
	for _, mode := range []string{"terminal", "unknown", "spent_parent", "actual_cut", "applied_without_cut", "receipt_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			var state codexproviders.State
			if json.Unmarshal(original, &state) != nil {
				t.Fatal("operation fixture invalid")
			}
			f.cut = make(chan codexproviders.ForkParticipant, 1)
			switch mode {
			case "terminal":
				state.Operation.Phase = "failed"
			case "applied_without_cut":
				state.Operation.Phase, state.Operation.Stage = "applied", "restoring-terminals"
			case "receipt_mismatch":
				receipt := state.Receipts[expected.operation]
				receipt.Target = "openai"
				state.Receipts[expected.operation] = receipt
			}
			if providerAtomicJSON(statePath, state) != nil {
				t.Fatal("operation fixture write failed")
			}
			if mode == "unknown" {
				_ = os.Remove(statePath)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			if mode == "actual_cut" || mode == "spent_parent" {
				f.cut <- journal.Participants[0]
			}
			if mode == "spent_parent" {
				cancel()
			}
			known, last, err := f.waitCutResult(ctx, expected)
			if last.sampled != (mode != "spent_parent") {
				t.Fatal("actual predicate sampling boundary changed")
			}
			switch mode {
			case "actual_cut":
				if err != nil || known.ChildThreadID != journal.Participants[0].ChildThreadID || len(f.cut) != 0 {
					t.Fatal("wait replaced the actual cut channel with sampled progress")
				}
			case "spent_parent":
				if !errors.Is(err, context.Canceled) || len(f.cut) != 1 {
					t.Fatal("spent scenario consumed a cut")
				}
			case "unknown":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("missing evidence became terminal/readiness")
				}
			default:
				want := map[string]string{"terminal": "operation_terminal_failed", "applied_without_cut": "applied_without_cut", "receipt_mismatch": "receipt_identity_mismatch"}[mode]
				if providerForkOperationRefusal(err) != want {
					t.Fatal("validated operation refusal was not prompt and typed")
				}
			}
			if err != nil && strings.Contains(err.Error(), expected.operation) {
				t.Fatal("operation refusal exposed identity")
			}
		})
	}
}

// Use the same start/health and mutation owners as the managed constructor and
// restart. Each synthetic process owns its listener and is joined before rebind.
func TestManagedProviderForkPortalReadiness(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "pfrdy-")
	if err != nil {
		t.Fatal("portal fixture unavailable")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	control := os.Getenv("MANAGED_PROVIDER_PORTAL_CONTROL")
	if control != "" {
		t.Cleanup(func() { t.Log("cleanup_continued") })
	}
	if os.Mkdir(filepath.Join(root, "bin"), 0700) != nil || os.Symlink(os.Args[0], filepath.Join(root, "bin", "workspace-portal")) != nil {
		t.Fatal("selected synthetic process image unavailable")
	}
	socket := filepath.Join(root, "portal.sock")
	f := &managedProviderFork{t: t, portal: socket, smoke: &managedSmokeHarness{t: t}, units: map[string]*managedSmokeUnit{}}
	f.smoke.manifest.PackageRoot = root
	body := map[string]any{"operationId": "f06a5b6c-892e-4de4-a7a8-321c94b96dd1"}
	posts := func(record string) int {
		data, err := os.ReadFile(record + ".posts")
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal("portal POST receipt unavailable")
		}
		return bytes.Count(data, []byte("post\n"))
	}
	// A spent parent must not launch another generation.
	spent, stop := context.WithCancel(context.Background())
	stop()
	f.smoke.ctx = spent
	started, probed := false, false
	if f.startPortalWith("initial", func() { started = true }, func(context.Context) (int, error) { probed = true; return 200, nil }, providerForkSystemctl) == nil || started || probed {
		t.Fatal("spent parent launched a portal")
	}
	if f.smoke.healthWith(spent, 0, "naming_health", func(context.Context) (int, error) { probed = true; return 200, nil }) == nil || probed {
		t.Fatal("spent naming parent entered a health probe")
	}
	var priorEpoch string
	modes := []string{"initial", "restart_lost", "naming", "naming_late", "owner_changed", "owner_unavailable", "canceled_wait", "late_health", "refused"}
	if control != "" {
		modes = []string{control}
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			f.t = t
			// Inspect, never sleep through, a deadline longer than the removed
			// startup4/5s child so a shortened loop fails the actual probe seam.
			parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f.smoke.ctx = parent
			hardBudget := 4 * time.Second
			if mode == "portal_spent" {
				hardBudget = time.Second
			}
			var stopHard context.CancelFunc
			f.hard, stopHard = context.WithTimeout(context.Background(), hardBudget)
			defer stopHard()
			f.portalUnaccounted = false
			record, gate := filepath.Join(root, mode), filepath.Join(root, mode+".gate")
			var command *exec.Cmd
			var done chan error
			joined := false
			join := func() {
				if command == nil || joined {
					return
				}
				_ = os.WriteFile(record+".stop", nil, 0600)
				select {
				case err := <-done:
					if err != nil {
						t.Error("synthetic portal did not exit normally")
					}
				case <-time.After(time.Second):
					_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
					<-done
					t.Error("synthetic portal required forced cleanup")
				}
				joined = true
				t.Log("portal_owner_joined")
			}
			defer join()
			probe := func(ctx context.Context) (int, error) {
				deadline, _ := ctx.Deadline()
				original, _ := parent.Deadline()
				if !deadline.Equal(original) {
					t.Fatal("startup health shortened or renewed its original parent")
				}
				status, _, err := f.smoke.httpWith(ctx, socket, "GET", "/healthz", nil, func(request context.Context) {
					bounded, ok := request.Deadline()
					if !ok || !bounded.Before(original) || time.Until(bounded) > 2*time.Second {
						t.Fatal("actual HTTP probe lost its separate two-second bound")
					}
				})
				if (mode == "late_health" || mode == "naming_late" || control != "") && err == nil && status == http.StatusOK {
					if mode == "portal_terminal" {
						join()
					}
					cancel() // Actual200 cannot authorize admission after expiry.
				}
				return status, err
			}
			var gateDone chan struct{}
			start := func() {
				image := os.Args[0]
				if mode == "portal_other" {
					image = filepath.Join(root, "other-image")
					input, openErr := os.Open(os.Args[0])
					output, createErr := os.OpenFile(image, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0700)
					if openErr != nil || createErr != nil {
						t.Fatal("second synthetic image unavailable")
					}
					_, copyErr := io.Copy(output, input)
					closeInput, closeOutput := input.Close(), output.Close()
					if copyErr != nil || closeInput != nil || closeOutput != nil {
						t.Fatal("second synthetic image incomplete")
					}
				}
				if mode == "portal_unavailable" {
					if os.Remove(filepath.Join(root, "bin", "workspace-portal")) != nil {
						t.Fatal("partial image fixture unavailable")
					}
				}
				command = exec.Command(image, "-test.run=^TestManagedProviderForkCLIChildFixture$")
				lost, refused := "0", "0"
				if mode == "restart_lost" {
					lost = "1"
				}
				if mode == "refused" {
					refused = "1"
				}
				command.Env = append(os.Environ(), "MANAGED_PROVIDER_CLI_FIXTURE=portal_process", "MANAGED_PROVIDER_CLI_RECORD="+record, "MANAGED_PROVIDER_PORTAL_GATE="+gate, "MANAGED_PROVIDER_PORTAL_SOCKET="+socket, "MANAGED_PROVIDER_PORTAL_LOST="+lost, "MANAGED_PROVIDER_PORTAL_REFUSE="+refused)
				command.Stdout, command.Stderr = io.Discard, io.Discard
				command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if command.Start() != nil {
					t.Fatal("synthetic portal process unavailable")
				}
				done = make(chan error, 1)
				go func() { done <- command.Wait() }()
				namingTestWait(t, record)
				main, ok := managedSmokeProcessAt(command.Process.Pid)
				if !ok {
					t.Fatal("synthetic portal lifetime unavailable")
				}
				f.units["portal"] = &managedSmokeUnit{main: main, group: main.group}
				if control != "" {
					f.units["portal"].name = "private-unit-canary"
				}
				switch mode {
				case "owner_changed":
					f.units["portal"].main.start = "0"
				case "owner_unavailable":
					f.units["portal"].main.pid = -1
				case "canceled_wait":
					cancel()
				default:
					gateDone = make(chan struct{})
					go func() {
						defer close(gateDone)
						time.Sleep(25 * time.Millisecond)
						if posts(record) != 0 {
							t.Error("POST reached the portal before health admission")
						}
						if os.WriteFile(gate, nil, 0600) != nil {
							t.Error("portal readiness gate unavailable")
						}
					}()
				}
			}
			query := func(ctx context.Context, args ...string) ([]byte, error) {
				t.Log("portal_query_entered")
				deadline, ok := ctx.Deadline()
				hard, _ := f.hard.Deadline()
				if !ok || deadline.After(hard.Add(-2*time.Second)) || time.Until(deadline) > 6*time.Second || len(args) != 3 || args[1] != f.units["portal"].name {
					t.Fatal("portal query escaped its hard tail or exact unit")
				}
				if mode == "portal_raced" {
					join()
				}
				unitMode := "portal"
				switch mode {
				case "portal_malformed":
					unitMode = "malformed"
				case "portal_unavailable":
					unitMode = "unavailable"
				case "portal_terminal":
					unitMode = "terminal"
				}
				unit := f.units["portal"]
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedProviderForkOperationFailure$")
				command.Env = append(os.Environ(), "MANAGED_PROVIDER_UNIT_FIXTURE="+unitMode, "MANAGED_PROVIDER_UNIT_PID="+strconv.Itoa(unit.main.pid), "MANAGED_PROVIDER_UNIT_GROUP="+unit.main.group)
				data, err := managedSmokeCommandResult(ctx, command)
				t.Log("portal_query_joined")
				return data, err
			}
			var err error
			if mode == "naming" || mode == "naming_late" {
				start()
				err = f.smoke.healthWith(parent, 0, "naming_health", probe)
			} else {
				phase := "restart"
				if mode == "initial" {
					phase = "initial"
				}
				err = f.startPortalWith(phase, start, probe, query)
			}
			if gateDone != nil {
				<-gateDone
			}
			rejected := mode == "owner_changed" || mode == "owner_unavailable" || mode == "canceled_wait" || mode == "late_health" || mode == "naming_late" || control != ""
			if rejected {
				if err == nil || posts(record) != 0 {
					t.Fatal("unproved portal owner admitted mutation")
				}
				if (mode == "owner_changed" || mode == "owner_unavailable") && !f.portalUnaccounted {
					t.Fatal("unknown portal lifetime became accounted")
				}
				if mode == "late_health" && (!strings.Contains(managedSmokeFailureText(err), "result=canceled parent=canceled") || !strings.Contains(managedSmokeFailureText(err), "status_known=true status=200 result=success")) {
					t.Fatal("late actual health was admitted or confused with wait result")
				}
				if mode == "naming_late" && (!strings.Contains(managedSmokeFailureText(err), "result=canceled parent=canceled") || !strings.Contains(managedSmokeFailureText(err), "status=200 result=success")) {
					t.Fatal("late naming health was admitted or confused with wait result")
				}
				if strings.Contains(managedSmokeFailureText(err), root) {
					t.Fatal("portal refusal exposed private location")
				}
				if control != "" {
					t.Fatal(managedSmokeFailureText(err)) // Actual failure owner/unwind.
				}
				return
			}
			if err != nil {
				t.Fatal(managedSmokeFailureText(err))
			}
			if _, err := os.Stat(record + ".health"); err != nil {
				t.Fatal("common start helper skipped actual health admission")
			}
			epoch, err := session.SocketIdentity(socket)
			if err != nil || epoch == priorEpoch {
				t.Fatal("portal rebind did not establish a new endpoint epoch")
			}
			priorEpoch = epoch
			err = f.requestResult(parent, "/api/codex-providers/recover", body)
			switch mode {
			case "restart_lost":
				if err == nil || !strings.Contains(managedSmokeFailureText(err), "http=eof") || !strings.Contains(managedSmokeFailureText(err), "status_known=false") {
					t.Fatal("lost response did not remain uncertain")
				}
			case "refused":
				if err == nil || !strings.Contains(managedSmokeFailureText(err), "status_known=true status=409") {
					t.Fatal("server refusal lost observed status")
				}
			default:
				if err != nil {
					t.Fatal(managedSmokeFailureText(err))
				}
			}
			if posts(record) != 1 {
				t.Fatal("recovery POST was missing or retried")
			}
			if err != nil && strings.Contains(managedSmokeFailureText(err), root) {
				t.Fatal("transport refusal exposed private location")
			}
			join()
			if _, err := os.Lstat(socket); !os.IsNotExist(err) {
				t.Fatal("joined portal retained its socket before rebind")
			}
		})
	}
}

func TestManagedProviderForkHTTPFailure(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "pfhttp-")
	if err != nil {
		t.Fatal("HTTP fixture unavailable")
	}
	defer os.RemoveAll(root)
	f := &managedProviderFork{portal: filepath.Join(root, "missing.sock")}
	body := map[string]any{"operationId": "f06a5b6c-892e-4de4-a7a8-321c94b96dd1"}
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.requestResult(parent, "/api/codex-providers/recover", body); err == nil || !strings.Contains(managedSmokeFailureText(err), "http=dial_absent operation=dial") {
		t.Fatal("actual missing endpoint lost typed dial refusal")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: f.portal, Net: "unix"})
	if err != nil {
		t.Fatal("refused endpoint unavailable")
	}
	listener.SetUnlinkOnClose(false)
	_ = listener.Close()
	if err := f.requestResult(parent, "/api/codex-providers/recover", body); err == nil || !strings.Contains(managedSmokeFailureText(err), "http=dial_refused operation=dial") {
		t.Fatal("actual stopped endpoint lost typed dial refusal")
	}
	wrapped := fmt.Errorf("private-transport-canary: %w", &net.OpError{Op: "write", Net: "unix", Err: syscall.ECONNRESET})
	message := managedSmokeFailureText(managedSmokeHTTPFailure(nil, wrapped, 202))
	if !strings.Contains(message, "http=reset operation=write") || !strings.Contains(message, "status_known=true status=202") || strings.Contains(message, "private-transport-canary") {
		t.Fatal("wrapped transport facts lost status or exposed raw error")
	}
}

// Exercise the fixture's actual alias/rename/proxy/cleanup helpers twice. The
// synthetic native owns a protected socket; it never runs the selected binary.
func TestManagedProviderForkEndpointLifecycle(t *testing.T) {
	f, expected, _, journal := managedProviderForkCutFixture(t)
	root, err := os.MkdirTemp("/tmp", "pfe-")
	if err != nil {
		t.Fatal("endpoint fixture unavailable")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f.socket, f.upstream = filepath.Join(root, "public.sock"), filepath.Join(root, "holder.sock")
	f.smoke.ordinarySocket = f.socket
	f.smoke.beforeNativeRequest = f.observe
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f.smoke.ctx = ctx
	t.Cleanup(func() {
		if err := f.stopProxy(); err != nil {
			t.Error(managedSmokeFailureText(err))
		}
	})
	var previousEpoch string
	for epoch := 0; epoch < 2; epoch++ {
		if err := f.prepareNativeEndpoints("initial"); err != nil {
			t.Fatal(managedSmokeFailureText(err))
		}
		physical := filepath.Join(root, fmt.Sprintf("protected-%d.sock", epoch))
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: physical, Net: "unix"})
		if err != nil {
			t.Fatal("synthetic native listener unavailable")
		}
		nativeCtx, stopNative := context.WithCancel(ctx)
		var handlers sync.WaitGroup
		forwarded := make(chan struct{}, 4)
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			defer handlers.Done()
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			for {
				kind, data, err := connection.Read(nativeCtx)
				if err != nil {
					return
				}
				select {
				case forwarded <- struct{}{}:
				case <-nativeCtx.Done():
					return
				}
				if connection.Write(nativeCtx, kind, data) != nil {
					return
				}
			}
		})}
		serveDone := make(chan struct{})
		go func() { defer close(serveDone); _ = server.Serve(listener) }()
		var once sync.Once
		joinNative := func() { once.Do(func() { stopNative(); _ = server.Close(); <-serveDone; handlers.Wait() }) }
		t.Cleanup(joinNative)
		if os.Symlink(physical, f.socket) != nil {
			t.Fatal("native alias fixture unavailable")
		}
		resolved, statErr := os.Stat(f.socket)
		advertised, lstatErr := os.Lstat(f.socket)
		if statErr != nil || lstatErr != nil || resolved.Mode()&os.ModeSocket == 0 || advertised.Mode()&os.ModeSymlink == 0 {
			t.Fatal("native alias/socket distinction lost")
		}
		f.nativeJoined = false
		if err := f.holdNativeEndpoint(); err != nil {
			t.Fatal(managedSmokeFailureText(err))
		}
		moved, err := os.Lstat(f.upstream)
		if err != nil || !os.SameFile(advertised, moved) || f.nativeEntry.verify(f.upstream, "native_holder") != nil {
			t.Fatal("rename changed the captured directory entry")
		}
		f.smoke.startProxy(f.upstream)
		currentEpoch, err := session.SocketIdentity(f.socket)
		if err != nil || currentEpoch == previousEpoch {
			t.Fatal("new proxy endpoint epoch not established")
		}
		previousEpoch = currentEpoch
		transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.socket)
		}}
		t.Cleanup(transport.CloseIdleConnections)
		client, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			t.Fatal("endpoint proxy dial failed")
		}
		t.Cleanup(func() { _ = client.CloseNow() })
		raw := []byte(`{"id":1,"method":"config/read"}`)
		if client.Write(ctx, websocket.MessageText, raw) != nil {
			t.Fatal("endpoint frame write failed")
		}
		if _, reply, err := client.Read(ctx); err != nil || !bytes.Equal(reply, raw) {
			t.Fatal("renamed alias did not forward actual traffic")
		}
		select {
		case <-forwarded:
		case <-ctx.Done():
			t.Fatal("native forwarding unobserved")
		}
		f.arm(expected.operation)
		raw, _ = json.Marshal(map[string]any{"id": 2, "method": "thread/read", "params": map[string]any{"threadId": journal.Participants[0].ChildThreadID, "excludeTurns": true}})
		if client.Write(ctx, websocket.MessageText, raw) != nil {
			t.Fatal("cut frame write failed")
		}
		select {
		case known := <-f.cut:
			if known.ChildThreadID != journal.Participants[0].ChildThreadID {
				t.Fatal("wrong actual cut")
			}
		case <-ctx.Done():
			t.Fatal("actual cut missing")
		}
		select {
		case <-forwarded:
			t.Fatal("held cut forwarded before cancellation")
		default:
		}
		if err := f.stopProxy(); err != nil {
			t.Fatal(managedSmokeFailureText(err))
		}
		if _, _, err := client.Read(ctx); err == nil {
			t.Fatal("held client did not join proxy cancellation")
		}
		if managedSmokeEndpointAbsent(f.socket, "public") != nil {
			t.Fatal("captured proxy entry survived closure")
		}
		target, err := os.Stat(physical)
		if err != nil || target.Mode()&os.ModeSocket == 0 {
			t.Fatal("proxy cleanup removed protected native socket")
		}
		if err := f.prepareNativeEndpoints("restart"); err == nil {
			t.Fatal("restart removed an entry before native join")
		}
		if f.nativeEntry.verify(f.upstream, "native_holder") != nil {
			t.Fatal("premature restart altered the native alias")
		}
		joinNative()
		f.nativeJoined = true // Independent synthetic owner/handler join, not absence.
		if _, err := os.Stat(f.upstream); !os.IsNotExist(err) {
			t.Fatal("native closure did not leave a dangling moved alias")
		}
		if f.nativeEntry.verify(f.upstream, "native_holder") != nil {
			t.Fatal("dangling alias lost its captured identity")
		}
		if err := f.prepareNativeEndpoints("restart"); err != nil {
			t.Fatal(managedSmokeFailureText(err))
		}
		if managedSmokeEndpointAbsent(f.upstream, "native_holder") != nil {
			t.Fatal("captured holder entry not removed")
		}
	}
}

func TestManagedProviderForkEndpointRefusal(t *testing.T) {
	for _, mode := range []string{"initial_file", "initial_alias", "replaced_socket", "replaced_alias", "unproved", "unreadable", "absent", "direct_socket", "proxy_partial", "proxy_replaced"} {
		t.Run(mode, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "pfr-")
			if err != nil {
				t.Fatal("endpoint refusal fixture unavailable")
			}
			defer os.RemoveAll(root)
			public, holder, target := filepath.Join(root, "public"), filepath.Join(root, "holder"), filepath.Join(root, "target")
			f := &managedProviderFork{t: t, socket: public, upstream: holder, smoke: &managedSmokeHarness{t: t, ordinarySocket: public}}
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: target, Net: "unix"})
			if err != nil {
				t.Fatal("target fixture unavailable")
			}
			defer listener.Close()
			if strings.HasPrefix(mode, "proxy_") {
				proxy, err := net.ListenUnix("unix", &net.UnixAddr{Name: public, Net: "unix"})
				if err != nil {
					t.Fatal("proxy fixture unavailable")
				}
				proxy.SetUnlinkOnClose(false)
				defer proxy.Close()
				if mode == "proxy_replaced" {
					if err := f.smoke.ownProxy(proxy); err != nil {
						t.Fatal(managedSmokeFailureText(err))
					}
				}
				if os.Remove(public) != nil || os.WriteFile(public, []byte("private-replacement"), 0600) != nil {
					t.Fatal("proxy replacement unavailable")
				}
				if mode == "proxy_partial" {
					if err := f.smoke.ownProxy(proxy); err == nil {
						t.Fatal("partial proxy adopted non-socket")
					}
				}
				if err := f.stopProxy(); err == nil {
					t.Fatal("unproved proxy cleanup became accounted")
				}
				if data, err := os.ReadFile(public); err != nil || string(data) != "private-replacement" {
					t.Fatal("proxy close deleted an unproved/replaced entry")
				}
				if err := f.stopProxy(); err == nil {
					t.Fatal("endpoint uncertainty was lost by repeated cleanup")
				}
				return
			}
			if mode == "initial_file" {
				if os.WriteFile(public, []byte("private-replacement"), 0600) != nil {
					t.Fatal("file fixture unavailable")
				}
			} else {
				if os.Symlink(target, public) != nil {
					t.Fatal("alias fixture unavailable")
				}
			}
			if strings.HasPrefix(mode, "initial_") {
				if err := f.prepareNativeEndpoints("initial"); err == nil {
					t.Fatal("initial preflight adopted a preexisting entry")
				}
				if _, err := os.Lstat(public); err != nil {
					t.Fatal("initial refusal deleted existing entry")
				}
				return
			}
			if mode == "direct_socket" {
				if os.Remove(public) != nil {
					t.Fatal("alias removal failed")
				}
				direct, err := net.ListenUnix("unix", &net.UnixAddr{Name: public, Net: "unix"})
				if err != nil {
					t.Fatal("direct socket fixture unavailable")
				}
				direct.SetUnlinkOnClose(false)
				defer direct.Close()
			}
			if err := f.holdNativeEndpoint(); err != nil {
				t.Fatal(managedSmokeFailureText(err))
			}
			f.nativeJoined = true
			switch mode {
			case "replaced_socket":
				if os.Remove(holder) != nil {
					t.Fatal("holder fixture unavailable")
				}
				replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: holder, Net: "unix"})
				if err != nil {
					t.Fatal("replacement socket unavailable")
				}
				replacement.SetUnlinkOnClose(false)
				defer replacement.Close()
			case "replaced_alias":
				if os.Remove(holder) != nil || os.Symlink(public, holder) != nil {
					t.Fatal("replacement alias unavailable")
				}
			case "unproved":
				f.nativeEntry.info = nil
			case "unreadable":
				if os.WriteFile(public, []byte("private-replacement"), 0600) != nil {
					t.Fatal("unreadable fixture unavailable")
				}
				f.nativeEntryPath = filepath.Join(public, "not-directory")
			case "absent":
				if os.Remove(holder) != nil {
					t.Fatal("absent holder fixture unavailable")
				}
			}
			err = f.prepareNativeEndpoints("restart")
			if mode == "absent" || mode == "direct_socket" {
				if err != nil {
					t.Fatal(managedSmokeFailureText(err))
				}
			} else {
				if err == nil {
					t.Fatal("restart adopted a replaced or unproved entry")
				}
				if _, err := os.Lstat(holder); err != nil {
					t.Fatal("refusal deleted retained holder")
				}
				message := managedSmokeFailureText(err)
				if !strings.Contains(message, "endpoint=") || !strings.Contains(message, "type=") || !strings.Contains(message, "proof=") || strings.Contains(message, root) || strings.Contains(message, "private-replacement") {
					t.Fatal("endpoint refusal lost fixed private categories")
				}
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatal("endpoint cleanup removed protected target")
			}
		})
	}
}

func TestManagedProviderForkObserveProxy(t *testing.T) {
	f, expected, _, journal := managedProviderForkCutFixture(t)
	for _, mode := range []string{"missing", "unavailable", "intent", "unmatched", "known", "malformed_request", "malformed_journal"} {
		t.Run(mode, func(t *testing.T) {
			path := f.operationPath(expected.operation) + ".journal"
			_ = os.RemoveAll(path)
			current := journal
			current.Participants = append([]codexproviders.ForkParticipant(nil), journal.Participants...)
			if mode == "intent" {
				current.Stage = "forking"
				current.Participants[0].State, current.Participants[0].ChildThreadID = "intent", ""
			}
			if mode != "missing" {
				if err := providerAtomicJSON(path, current); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
			}
			if mode == "unavailable" {
				_ = os.Remove(path)
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
			} else if mode == "malformed_journal" {
				managedSmokeWrite(t, path, "{", 0600)
			}
			f.arm(expected.operation)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			forwarded, connected, joined := make(chan struct{}, 4), make(chan struct{}, 4), make(chan struct{}, 4)
			upstream := threadCommandAppServer(t, func(connection *websocket.Conn) error {
				connected <- struct{}{}
				defer func() { _ = connection.CloseNow(); joined <- struct{}{} }()
				for {
					kind, raw, err := connection.Read(ctx)
					if err != nil {
						return nil
					}
					forwarded <- struct{}{}
					if err := connection.Write(ctx, kind, raw); err != nil {
						return nil
					}
				}
			})
			directory, err := os.MkdirTemp("/tmp", "pfc-")
			if err != nil {
				t.Fatal("fixture prerequisite or identity unavailable")
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			f.smoke.ordinarySocket = filepath.Join(directory, "proxy.sock")
			f.smoke.beforeNativeRequest = f.observe
			f.smoke.startProxy(upstream)
			var clients []*websocket.Conn
			t.Cleanup(func() {
				for _, client := range clients {
					_ = client.CloseNow()
				}
				if err := f.stopProxy(); err != nil {
					t.Error(managedSmokeFailureText(err))
				}
				for range clients {
					select {
					case <-connected:
					case <-time.After(time.Second):
						t.Error("synthetic upstream did not start")
					}
					select {
					case <-joined:
					case <-time.After(time.Second):
						t.Error("synthetic upstream did not join")
					}
				}
			})
			dial := func() *websocket.Conn {
				t.Helper()
				transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", f.smoke.ordinarySocket)
				}}
				t.Cleanup(transport.CloseIdleConnections)
				client, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal("synthetic proxy connection failed")
				}
				clients = append(clients, client)
				return client
			}
			child := journal.Participants[0].ChildThreadID
			if mode == "unmatched" {
				child = "unmatched-child"
			}
			raw, _ := json.Marshal(map[string]any{"id": 1, "method": "thread/read", "params": map[string]any{"threadId": child, "excludeTurns": true}})
			if mode == "malformed_request" {
				raw = []byte("{")
				if f.observe(ctx, raw) == nil {
					t.Fatal("malformed request changed observer behavior")
				}
			}
			client := dial()
			if err := client.Write(ctx, websocket.MessageText, raw); err != nil {
				t.Fatal("synthetic frame write failed")
			}
			if mode == "known" {
				select {
				case participant := <-f.cut:
					if participant.State != "known-child" || participant.ChildThreadID != child {
						t.Fatal("cut did not retain exact durable participant")
					}
				case <-ctx.Done():
					t.Fatal("actual observer never delivered the cut")
				}
				select {
				case <-forwarded:
					t.Fatal("known-child request forwarded before cut cancellation")
				default:
				}
				// The held connection is blocked; another connection proves the
				// now-disarmed hook forwards without sending a duplicate cut.
				second := dial()
				if err := second.Write(ctx, websocket.MessageText, raw); err != nil {
					t.Fatal("fixture prerequisite or identity unavailable")
				}
				if _, reply, err := second.Read(ctx); err != nil || !bytes.Equal(reply, raw) {
					t.Fatal("disarmed request did not forward")
				}
				<-forwarded
				select {
				case <-f.cut:
					t.Fatal("disarmed hook sent another cut")
				default:
				}
				f.smoke.proxyStop()
				if _, _, err := client.Read(ctx); err == nil {
					t.Fatal("held request survived proxy cancellation")
				}
			} else if strings.HasPrefix(mode, "malformed_") {
				if _, _, err := client.Read(ctx); err == nil {
					t.Fatal("malformed input was forwarded")
				}
				select {
				case <-forwarded:
					t.Fatal("malformed input reached upstream")
				default:
				}
			} else {
				if _, reply, err := client.Read(ctx); err != nil || !bytes.Equal(reply, raw) {
					t.Fatal("non-cut request did not forward")
				}
				<-forwarded
			}
			select {
			case <-f.cut:
				t.Fatal("unexpected or duplicate cut")
			default:
			}
		})
	}
}

func (f *managedProviderFork) journal(expected providerForkCutRequest) codexproviders.ForkJournal {
	f.t.Helper()
	var journal codexproviders.ForkJournal
	data, err := managedSmokeRead(f.operationPath(expected.operation)+".journal", 16<<20)
	if err != nil || json.Unmarshal(data, &journal) != nil {
		f.operationFailure("operation_journal", expected, "result=journal_unavailable_or_rejected")
	}
	return journal
}
func (f *managedProviderFork) arm(id string) { f.mu.Lock(); defer f.mu.Unlock(); f.cutOperation = id }
func (f *managedProviderFork) observe(ctx context.Context, raw []byte) error {
	var request struct {
		Method string
		Params struct{ ThreadID string }
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	f.mu.Lock()
	if request.Method == "thread/fork" {
		f.forks[request.Params.ThreadID]++
	}
	id := f.cutOperation
	f.mu.Unlock()
	if id == "" || request.Method != "thread/read" {
		return nil
	}
	data, err := managedSmokeRead(f.operationPath(id)+".journal", 16<<20)
	if err != nil {
		return nil
	} // No journal yet, so this cannot be the cut.
	var journal codexproviders.ForkJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return err
	}
	for _, p := range journal.Participants {
		if p.State == "known-child" && p.ChildThreadID == request.Params.ThreadID {
			f.mu.Lock()
			armed := f.cutOperation == id
			if armed {
				f.cutOperation = ""
			}
			f.mu.Unlock()
			if armed {
				f.cut <- p
				<-ctx.Done()
				return ctx.Err()
			}
		}
	}
	return nil
}
func (f *managedProviderFork) forkCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forks[id]
}
func (f *managedProviderFork) request(path string, body map[string]any, expected providerForkCutRequest) {
	f.t.Helper()
	if err := f.requestResult(f.smoke.context(), path, body); err != nil {
		f.operationFailure("operation_accept", expected, managedSmokeFailureText(err))
	}
}

// This mutation is issued once. A failed transport can follow peer acceptance;
// neither readiness nor the idempotency key permits an automatic retry here.
func (f *managedProviderFork) requestResult(parent context.Context, path string, body map[string]any) error {
	data, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(parent, 18*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", f.portal)
	}}
	defer transport.CloseIdleConnections()
	if err := managedSmokeExpired(ctx); err != nil {
		return managedSmokeFailure("http=not_started context=" + managedSmokeContextResult(err) + " status_known=false status=0")
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://managed-smoke.example.test"+path, bytes.NewReader(data))
	if err != nil {
		return managedSmokeHTTPFailure(ctx.Err(), err, 0)
	}
	request.Header.Set("Origin", managedSmokeOrigin)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		return managedSmokeHTTPFailure(ctx.Err(), err, status)
	}
	defer response.Body.Close()
	// Only discard bounded response bytes. Acceptance is202 plus the subsequent
	// strict durable proof, not a new response-body or response-size contract.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if managedSmokeExpired(ctx) != nil {
		return managedSmokeFailure(fmt.Sprintf("http=late_result context=%s status_known=true status=%d", managedSmokeContextResult(ctx.Err()), response.StatusCode))
	}
	if response.StatusCode != http.StatusAccepted {
		return managedSmokeFailure(fmt.Sprintf("http=refused context=%s status_known=true status=%d", managedSmokeContextResult(ctx.Err()), response.StatusCode))
	}
	return nil
}

// Type=simple MainPID admission can precede Ruby/Go initialization and HTTP
// binding. Initial and restarted generations share this one readiness owner.
func (f *managedProviderFork) startPortal(phase string) {
	f.t.Helper()
	if err := f.startPortalWith(phase, func() { f.start("portal") }, f.portalHealthProbe, providerForkSystemctl); err != nil {
		f.t.Fatal(managedSmokeFailureText(err))
	}
}

func (f *managedProviderFork) portalHealthProbe(ctx context.Context) (int, error) {
	status, _, err := f.smoke.http(ctx, f.portal, "GET", "/healthz", nil)
	return status, err
}

// Boundary clocks share the original monotonic deadline. They describe budget
// position, not a startup substage or a promise that the remaining work fits.
func providerForkPortalClock(parent context.Context, started, healthStarted time.Time) string {
	now := time.Now()
	deadline, known := parent.Deadline()
	remaining, health := int64(-1), int64(-1)
	if known {
		remaining = max(0, deadline.Sub(now).Milliseconds())
	}
	if !healthStarted.IsZero() {
		health = now.Sub(healthStarted).Milliseconds()
	}
	return fmt.Sprintf("total_elapsed_ms=%d health_elapsed_known=%t health_elapsed_ms=%d remaining_known=%t remaining_ms=%d", now.Sub(started).Milliseconds(), !healthStarted.IsZero(), health, known, remaining)
}

func (f *managedProviderFork) startPortalWith(phase string, start func(), probe func(context.Context) (int, error), query func(context.Context, ...string) ([]byte, error)) error {
	parent := f.smoke.context()
	phase = providerForkCutEnum(phase, "initial", "restart")
	started, healthStarted := time.Now(), time.Time{}
	owner, last, status := "unobserved", "http=unobserved", 0
	var unit *managedSmokeUnit
	var captured managedSmokeProcess
	f.t.Logf("phase=portal_start startup=%s boundary=entry context=%s %s", phase, managedSmokeContextResult(managedSmokeExpired(parent)), providerForkPortalClock(parent, started, healthStarted))
	finish := func(err, parentAtReturn error) error {
		original := fmt.Sprintf("phase=portal_health startup=%s boundary=return result=%s parent=%s owner=%s %s status_known=%t status=%d %s", phase, managedSmokeContextResult(err), managedSmokeContextResult(parentAtReturn), owner, providerForkPortalClock(parent, started, healthStarted), status != 0, status, last)
		f.t.Log(original) // Held result FIRST; optional evidence cannot hide it.
		if err != nil {
			f.portalStartupSample(phase, unit, captured, query)
			return managedSmokeFailure(original)
		}
		return nil
	}
	if err := managedSmokeExpired(parent); err != nil {
		return finish(err, err)
	}
	start()
	healthStarted = time.Now()
	f.t.Logf("phase=portal_start startup=%s boundary=health_entry context=%s %s", phase, managedSmokeContextResult(managedSmokeExpired(parent)), providerForkPortalClock(parent, started, healthStarted))
	unit = f.units["portal"]
	if unit == nil {
		f.portalUnaccounted = true
		return finish(managedSmokeFailure("phase=portal_health owner=unobserved"), managedSmokeExpired(parent))
	}
	captured = unit.main
	checkOwner := func() error {
		current, ok := managedSmokeProcessAt(captured.pid)
		owner = "unobserved"
		if ok && captured.pid > 0 && captured.start != "" && captured.group != "" {
			owner = "changed"
			if current == captured {
				owner = "same"
			}
		}
		if owner != "same" {
			f.portalUnaccounted = true
			return managedSmokeFailure("phase=portal_health owner=" + owner)
		}
		return nil
	}
	// Host configuration and strict application construction precede listening;
	// they have no total five-second startup contract. Observe this generation
	// only within the original operational deadline. Each HTTP probe stays2s.
	err := managedSmokeWait(parent, 0, func(ctx context.Context) (bool, error) {
		if err := checkOwner(); err != nil {
			return false, err
		}
		var err error
		status, err = probe(ctx)
		last = managedSmokeFailureText(err)
		if proof := checkOwner(); proof != nil {
			return false, proof
		}
		return err == nil && status == http.StatusOK, nil
	})
	parentAtReturn := managedSmokeExpired(parent)
	if err != nil {
		return finish(err, parentAtReturn)
	}
	if err := checkOwner(); err != nil {
		return finish(err, parentAtReturn)
	}
	if err := managedSmokeExpired(parent); err != nil {
		return finish(err, parentAtReturn)
	}
	return finish(nil, parentAtReturn)
}

// This later, non-atomic sample is never another readiness opportunity. Missing
// process evidence alone cannot establish exit, and other_image names no stage.
func (f *managedProviderFork) portalStartupSample(phase string, unit *managedSmokeUnit, captured managedSmokeProcess, query func(context.Context, ...string) ([]byte, error)) {
	f.t.Helper()
	f.t.Logf("phase=portal_sample startup=%s sample=entry observation=after_refusal non_atomic=true", phase)
	ctx, cancel, skipped := f.unitSampleContext(unit)
	if skipped != "" {
		f.t.Logf("phase=portal_sample query=%s image=unavailable process=unobserved state=unknown", skipped)
		return
	}
	defer cancel()
	before, _ := providerForkNativeProcess(captured)
	image := "unavailable"
	if before == "same" {
		selected, selectedErr := os.Stat(filepath.Join(f.smoke.manifest.PackageRoot, "bin/workspace-portal"))
		current, currentErr := os.Stat(fmt.Sprintf("/proc/%d/exe", captured.pid))
		if selectedErr == nil && currentErr == nil {
			image = "other_image"
			if os.SameFile(selected, current) {
				image = "selected_portal"
			}
		}
	}
	sample := providerForkReadNativeUnit(query(ctx, "show", unit.name, "--property=MainPID,ControlGroup,ActiveState,SubState,Result,ExecMainCode,ExecMainStatus"))
	process, state := providerForkNativeProcess(captured)
	if before == "changed" || process == "changed" || sample.pid > 0 && sample.pid != captured.pid || sample.group != "" && sample.group != captured.group {
		process, image, state, sample.query = "changed", "raced", "unknown", "raced"
	} else if before != process {
		process, image, state, sample.query = "unobserved", "raced", "unknown", "raced"
	}
	terminal := sample.query == "available" && sample.pid == 0 && (sample.active == "inactive" || sample.active == "failed") && sample.code > 0 && sample.status >= 0
	if terminal {
		if process == "unobserved" || state == "terminal" {
			state = "terminal"
		} else {
			image, state, sample.query = "raced", "unknown", "raced"
		}
	}
	if state == "terminal" && sample.query == "available" && (sample.pid > 0 || sample.active == "active" || sample.active == "activating") {
		image, state, sample.query = "raced", "unknown", "raced"
	}
	f.t.Logf("phase=portal_sample startup=%s observation=after_refusal non_atomic=true process=%s image=%s state=%s query=%s active=%s sub=%s result=%s code_known=%t code=%d status_known=%t status=%d", phase, process, image, state, sample.query, sample.active, sample.sub, sample.result, sample.code >= 0, sample.code, sample.status >= 0, sample.status)
}

// Read only strict local state while waiting. Unavailable/rejected evidence is
// unknown; validated terminal or wrong-request state refuses promptly. No GET
// helper starts a new worker or extends the original fork-run context.
type providerForkOperationError string

func (err providerForkOperationError) Error() string { return string(err) }

func (f *managedProviderFork) operationState(expected providerForkCutRequest) (codexproviders.State, bool, error) {
	catalog, err := codexproviders.ReadCatalog(filepath.Join(f.smoke.root, "registry", "providers.json"))
	if err != nil {
		return codexproviders.State{}, false, nil
	}
	state, err := codexproviders.ReadBindingState(f.state, f.name, f.workspace, f.socket, catalog)
	if err != nil {
		return codexproviders.State{}, false, nil
	}
	if receipt, ok := state.Receipts[expected.operation]; ok && (receipt.Target != expected.target || receipt.Session != expected.session || receipt.Inherit != expected.inherit) {
		return state, false, providerForkOperationError("receipt_identity_mismatch")
	}
	op := state.Operation
	if op == nil {
		return state, false, nil
	}
	if op.ID != expected.operation || op.Target != expected.target || op.Session != expected.session || op.Inherit != expected.inherit {
		return state, false, providerForkOperationError("operation_identity_mismatch")
	}
	switch op.Phase {
	case "failed", "canceled", "recovery-required":
		return state, false, providerForkOperationError("operation_terminal_" + op.Phase)
	case "applied":
		return state, true, nil
	default:
		return state, false, nil
	}
}

// Only the last completed actual predicate is retained. This adds no reads or
// per-poll output and cannot change the shared wait's expiry precedence.
type providerForkPredicateSample struct {
	sampled, ready   bool
	admissionExpired bool
	refusal          string
	context          error
}

func providerForkOperationWait(ctx context.Context, predicate func(context.Context) (bool, error)) (providerForkPredicateSample, error) {
	var last providerForkPredicateSample
	err := managedSmokeWait(ctx, 0, func(ctx context.Context) (bool, error) {
		ready, err := predicate(ctx)
		last = providerForkPredicateSample{sampled: true, ready: ready, refusal: providerForkOperationRefusal(err), context: managedSmokeExpired(ctx)}
		return ready, err
	})
	last.admissionExpired = errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	return last, err
}

func (last providerForkPredicateSample) text() string {
	readiness, refusal, owner := "unobserved", "unobserved", "unobserved"
	if last.sampled {
		readiness, refusal, owner = "not_ready_or_unknown", last.refusal, managedSmokeContextResult(last.context)
		if last.ready {
			readiness = "ready"
			if last.context != nil {
				readiness = "late_ready_refused"
			} else if last.admissionExpired {
				readiness = "admission_boundary_expired"
			}
		}
	}
	return fmt.Sprintf("predicate_sampled=%t predicate=%s predicate_refusal=%s predicate_context=%s", last.sampled, readiness, refusal, owner)
}

func (f *managedProviderFork) operationFailure(phase string, expected providerForkCutRequest, result string) {
	f.t.Helper()
	f.operationFailureAt(phase, result, managedSmokeExpired(f.smoke.context()), providerForkPredicateSample{}, providerForkSystemctl, func() { f.operationSample(expected) })
}

func (f *managedProviderFork) operationSample(expected providerForkCutRequest) {
	f.t.Helper()
	if managedSmokeExpired(f.smoke.context()) != nil {
		f.t.Log("operation failure sample: not_sampled_deadline")
		return
	}
	f.t.Log("operation failure sample (after_refusal, non_atomic):", f.cutState(expected))
}

func (f *managedProviderFork) operationWorkerSample(query func(context.Context, ...string) ([]byte, error)) {
	f.t.Helper()
	f.t.Log("phase=worker_result sample=entry observation=after_refusal non_atomic=true")
	unit := f.units["provider"]
	ctx, cancel, skipped := f.unitSampleContext(unit)
	if skipped != "" {
		f.t.Log("phase=worker_result query=" + skipped)
		return
	}
	defer cancel()
	sample := providerForkReadNativeUnit(query(ctx, "show", unit.name, "--property=MainPID,ControlGroup,ActiveState,SubState,Result,ExecMainCode,ExecMainStatus"))
	f.t.Logf("phase=worker_result observation=after_refusal non_atomic=true query=%s active=%s sub=%s result=%s code_known=%t code=%d status_known=%t status=%d", sample.query, sample.active, sample.sub, sample.result, sample.code >= 0, sample.code, sample.status >= 0, sample.status)
}

// The already-owned exact unit shares the original hard tail and sole command
// Wait owner. Neither an expired observation nor another caller renews it.
func (f *managedProviderFork) unitSampleContext(unit *managedSmokeUnit) (context.Context, context.CancelFunc, string) {
	if unit == nil || unit.name == "" {
		return nil, nil, "not_sampled_owner"
	}
	if f.hard == nil {
		return nil, nil, "not_sampled_deadline"
	}
	deadline, ok := f.hard.Deadline()
	deadline = deadline.Add(-2 * time.Second) // Preserve the command's existing WaitDelay.
	if !ok || managedSmokeExpired(f.hard) != nil || !time.Now().Before(deadline) {
		return nil, nil, "not_sampled_deadline"
	}
	if cap := time.Now().Add(6 * time.Second); cap.Before(deadline) {
		deadline = cap
	}
	ctx, cancel := context.WithDeadline(f.hard, deadline)
	return ctx, cancel, ""
}

// Optional evidence must never precede the refusal it explains. Local reads
// remain synchronous, and the Go hard timer can still preempt even this log.
func (f *managedProviderFork) operationFailureAt(phase, result string, contextAtReturn error, last providerForkPredicateSample, query func(context.Context, ...string) ([]byte, error), sample func()) {
	f.t.Helper()
	original := fmt.Sprintf("phase=%s parent=%s %s %s", phase, managedSmokeContextResult(contextAtReturn), result, last.text())
	f.t.Log(original)
	f.handoffSettingsEvidence()
	f.operationWorkerSample(query)
	if managedSmokeExpired(f.smoke.context()) != nil {
		f.t.Log("operation failure sample: not_sampled_deadline")
	} else {
		sample()
	}
	f.t.Fatal(original)
}

func (f *managedProviderFork) waitCutResult(ctx context.Context, expected providerForkCutRequest) (codexproviders.ForkParticipant, providerForkPredicateSample, error) {
	var known codexproviders.ForkParticipant
	last, err := providerForkOperationWait(ctx, func(ctx context.Context) (bool, error) {
		select {
		case known = <-f.cut:
			return true, nil
		default:
		}
		_, applied, err := f.operationState(expected)
		if err != nil {
			return false, err
		}
		if applied {
			return false, providerForkOperationError("applied_without_cut")
		}
		return false, nil
	})
	return known, last, err
}

func (f *managedProviderFork) waitCut(expected providerForkCutRequest) codexproviders.ForkParticipant {
	f.t.Helper()
	known, last, err := f.waitCutResult(f.smoke.context(), expected)
	contextAtReturn := managedSmokeExpired(f.smoke.context())
	if err != nil {
		f.operationFailureAt("durable_cut", "result="+managedSmokeContextResult(err)+" refusal="+providerForkOperationRefusal(err), contextAtReturn, last, providerForkSystemctl, func() { f.operationSample(expected) })
	}
	return known
}

func providerForkOperationRefusal(err error) string {
	if refusal, ok := err.(providerForkOperationError); ok {
		return string(refusal)
	}
	return "unobserved"
}

func (f *managedProviderFork) applied(expected providerForkCutRequest) codexproviders.State {
	f.t.Helper()
	f.t.Log("phase=operation_apply")
	var state codexproviders.State
	last, err := providerForkOperationWait(f.smoke.context(), func(ctx context.Context) (bool, error) {
		var ready bool
		var err error
		state, ready, err = f.operationState(expected)
		return ready, err
	})
	contextAtReturn := managedSmokeExpired(f.smoke.context())
	if err != nil {
		f.operationFailureAt("operation_apply", "result="+managedSmokeContextResult(err)+" refusal="+providerForkOperationRefusal(err), contextAtReturn, last, providerForkSystemctl, func() { f.operationSample(expected) })
	}
	for _, projection := range f.journal(expected).Projections {
		if !projection.Manifest || !projection.Authority || !projection.Recovery || !projection.Terminal {
			f.operationFailure("operation_projection", expected, "result=incomplete_projection")
		}
	}
	return state
}
func (f *managedProviderFork) readThread(id string) map[string]any {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.smoke.context(), 3*time.Second)
	defer cancel()
	var result struct{ Thread map[string]any }
	if err := f.smoke.ordinary.Request(ctx, "thread/read", map[string]any{"threadId": id, "excludeTurns": true}, &result); err != nil {
		f.t.Fatalf("phase=thread_read %s", managedSmokeRPCFailure(ctx, err))
	}
	return result.Thread
}
func (f *managedProviderFork) threadSnapshot(id string) any {
	f.t.Helper()
	metadata := f.readThread(id)
	selected := map[string]any{}
	for _, key := range []string{"id", "cwd", "modelProvider", "name", "forkedFromId"} {
		selected[key] = metadata[key]
	}
	ctx, cancel := context.WithTimeout(f.smoke.context(), 3*time.Second)
	defer cancel()
	history, err := readProviderForkHistory(ctx, &workspacecodex.Client{Client: f.smoke.ordinary}, id)
	if err != nil {
		f.t.Fatalf("phase=history_read %s", managedSmokeRPCFailure(ctx, err))
	}
	return struct {
		Metadata map[string]any
		History  providerForkHistory
	}{selected, history}
}
func (f *managedProviderFork) history(id, marker string) {
	f.t.Helper()
	f.t.Log("phase=history")
	managedSmokeAwait(f.t, f.smoke.context(), 8*time.Second, "history", func(parent context.Context) bool {
		ctxRead, stopRead := context.WithTimeout(parent, 3*time.Second)
		defer stopRead()
		history, err := readProviderForkHistory(ctxRead, &workspacecodex.Client{Client: f.smoke.ordinary}, id)
		if err != nil {
			f.t.Fatalf("phase=history %s", managedSmokeRPCFailure(ctxRead, err))
		}
		data, _ := json.Marshal(history)
		ctx, cancel := context.WithTimeout(parent, time.Second)
		defer cancel()
		return bytes.Contains(data, []byte(marker)) && bytes.Contains(data, []byte("ordinary-smoke-completed")) && f.smoke.ordinary.RequireThreadTurnsIdle(ctx, id) == nil
	})
}
func (f *managedProviderFork) checkSwitch(slug, root string, member teamruntime.Member, original *teamruntime.Roster, state codexproviders.State, provider, previousRoot, previousMember string) (string, string) {
	f.t.Helper()
	if f.summary(slug).Codex.ThreadID != root {
		f.t.Fatal("logical root changed")
	}
	store, err := teamruntime.NewStore(f.state, f.workspace)
	if err != nil {
		f.t.Fatal("fixture prerequisite or identity unavailable")
	}
	roster, err := store.Load(slug, root)
	if err != nil || !reflect.DeepEqual(original, roster) {
		f.t.Fatal("original retained roster changed")
	}
	binding := state.Bindings[slug]
	if binding.RootThreadID != root || len(binding.Conversations) != 2 {
		f.t.Fatal("binding lost original root/member links")
	}
	rootRow, memberRow := binding.Conversations[root], binding.Conversations[member.Thread]
	for _, pair := range []struct {
		row            codexproviders.ConversationBinding
		source, marker string
	}{{rootRow, previousRoot, "PORTAL_ROOT_SEED"}, {memberRow, previousMember, "PORTAL_MEMBER_SEED"}} {
		thread := f.readThread(pair.row.ThreadID)
		if pair.row.Provider != provider || pair.row.ThreadID == pair.source || thread["forkedFromId"] != pair.source || thread["modelProvider"] != provider {
			f.t.Fatal("exact native child/parent/provider changed")
		}
		f.history(pair.row.ThreadID, pair.marker)
	}
	if rootRow.Member != "lead" || memberRow.Member != member.Address {
		f.t.Fatal("retained member address changed")
	}
	return rootRow.ThreadID, memberRow.ThreadID
}
func (f *managedProviderFork) continueTerminal(slug, child, marker string, provider int) {
	f.t.Helper()
	tmux := func(args ...string) string {
		return strings.TrimSpace(string(managedSmokeCommand(f.t, f.smoke.context(), "terminal_control", 6*time.Second, "/run/current-system/sw/bin/tmux", append([]string{"-S", f.tmux}, args...)...)))
	}
	if tmux("show-options", "-v", "-t", slug, "@dev_session_codex_thread") != child {
		f.t.Fatal("managed terminal did not rebind")
	}
	pane := tmux("show-options", "-v", "-t", slug, "@dev_session_codex_pane")
	if !strings.HasPrefix(pane, "%") || !strings.Contains(tmux("display-message", "-p", "-t", pane, "#{pane_start_command}"), "resume "+child) {
		f.t.Fatal("real terminal is not resuming the exact current child")
	}
	f.awaitTerminalReady(child, pane, func(ctx context.Context) []byte {
		return managedSmokeCommand(f.t, ctx, "terminal_read", 6*time.Second, "/run/current-system/sw/bin/tmux", "-S", f.tmux, "capture-pane", "-p", "-t", pane)
	})
	count := func(p *managedSmokeProvider) int { p.mu.Lock(); defer p.mu.Unlock(); return p.posts }
	before, other := count(f.providers[provider]), count(f.providers[1-provider])
	tmux("send-keys", "-t", pane, "-l", marker)
	tmux("send-keys", "-t", pane, "Enter")
	f.history(child, marker)
	if count(f.providers[provider]) != before+1 || count(f.providers[1-provider]) != other {
		f.t.Fatal("terminal continuation used the wrong provider")
	}
}

type providerForkTerminalReadyEvidence struct {
	Schema           int    `json:"schema"`
	Phase            string `json:"phase"`
	Child            string `json:"child"`
	Pane             string `json:"pane"`
	Observed         bool   `json:"observed"`
	SampleElapsedMS  *int64 `json:"sampleElapsedMs,omitempty"`
	FailureElapsedMS int64  `json:"failureElapsedMs"`
	Stdout           []byte `json:"stdout,omitempty"`
}

func providerForkTerminalScreenReady(output []byte) bool {
	// The selected TUI renders the catalog display name, not the wire slug.
	// Match its ordinary prompt/footer/help layout without interpreting history.
	prompt, footer := false, false
	for _, row := range strings.Split(string(output), "\n") {
		row = strings.Trim(row, " ") // Visual indentation and fixed-width padding.
		if row == "›" || strings.HasPrefix(row, "› ") {
			prompt, footer = true, false
		} else if prompt && (row == "GPT-5.5 low" || strings.HasPrefix(row, "GPT-5.5 low · ")) {
			footer = true
		} else if footer && (row == "? for shortcuts" || row == "← for agents · ? for shortcuts") {
			return true
		}
	}
	return false
}

func (f *managedProviderFork) awaitTerminalReady(child, pane string, capture func(context.Context) []byte) {
	f.t.Helper()
	started, ready := time.Now(), false
	evidence := providerForkTerminalReadyEvidence{Schema: 1, Phase: "terminal_ready_last_successful", Child: child, Pane: pane}
	defer func() {
		if !ready && f.t.Failed() {
			// Fatalf has already logged the original refusal. This historical
			// sample precedes cleanup and never claims the pane's cutoff state.
			evidence.FailureElapsedMS = time.Since(started).Milliseconds()
			f.t.Log("phase=terminal_evidence outcome=" + f.handoffTerminalReadyEvidence(evidence))
		}
	}()
	managedSmokeAwait(f.t, f.smoke.context(), 5*time.Second, "terminal_ready", func(ctx context.Context) bool {
		output := capture(ctx)
		if len(output) <= 64*1024 && managedSmokeExpired(ctx) == nil {
			elapsed := time.Since(started).Milliseconds()
			evidence.Observed, evidence.SampleElapsedMS = true, &elapsed
			evidence.Stdout = append([]byte(nil), output...)
		}
		return providerForkTerminalScreenReady(output)
	})
	ready = true
}

func (f *managedProviderFork) handoffTerminalReadyEvidence(evidence providerForkTerminalReadyEvidence) string {
	directory := os.Getenv("MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR")
	info, err := os.Lstat(directory)
	if err != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !info.IsDir() || info.Mode().Perm() != 0700 || f.hard == nil || managedSmokeExpired(f.hard) != nil {
		return "unavailable"
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() {
		return "unavailable"
	}
	data, err := json.Marshal(evidence)
	if err != nil || len(data) > providerForkSettingsEvidenceLimit || managedSmokeExpired(f.hard) != nil || providerForkWriteFirstPrivate(filepath.Join(directory, "terminal-ready.json"), data) != nil {
		return "unavailable"
	}
	return "available"
}

func TestManagedProviderForkTerminalReadyEvidence(t *testing.T) {
	const screen, child, pane = "public-terminal-screen-canary", "public-child-canary", "%123456"
	// Public selected-native status_line_model_with_reasoning_context_remaining_footer
	// and agents_navigation_status layouts, with the retained model and effort.
	normalScreens := []string{
		"› Ask Codex to do anything\n\n  GPT-5.5 low · Context 100% left · /tmp/project\n  ? for shortcuts\n",
		"  › Explain this codebase                         \n\n    GPT-5.5 low          \n    ? for shortcuts     \n",
		"› Ask Codex to do anything\n\n  GPT-5.5 low · /tmp/project\n  ← for agents · ? for shortcuts\n",
	}
	if mode := os.Getenv("MANAGED_PROVIDER_TERMINAL_EVIDENCE_CONTROL"); mode != "" {
		directory := os.Getenv("MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR")
		parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		hard, cancelHard := context.WithTimeout(context.Background(), time.Second)
		defer cancelHard()
		f := &managedProviderFork{t: t, hard: hard, smoke: &managedSmokeHarness{ctx: parent}}
		input, reads := false, 0
		t.Cleanup(func() {
			data, err := os.ReadFile(filepath.Join(directory, "terminal-ready.json"))
			if mode == "ready" || mode == "missing" {
				if !os.IsNotExist(err) {
					t.Error("unexpected readiness evidence")
				}
			} else {
				var evidence providerForkTerminalReadyEvidence
				info, statErr := os.Stat(filepath.Join(directory, "terminal-ready.json"))
				if err != nil || statErr != nil || info.Mode().Perm() != 0600 || len(data) > providerForkSettingsEvidenceLimit || json.Unmarshal(data, &evidence) != nil || evidence.Schema != 1 || evidence.Phase != "terminal_ready_last_successful" || evidence.Child != child || evidence.Pane != pane || !evidence.Observed || evidence.SampleElapsedMS == nil || *evidence.SampleElapsedMS > evidence.FailureElapsedMS || string(evidence.Stdout) != screen {
					t.Error("last successful private screen missing before cleanup")
				} else {
					t.Log("historical_screen_verified")
				}
			}
			if mode != "ready" && input {
				t.Error("refused readiness permitted input")
			}
			t.Log("terminal_cleanup_joined")
		})
		capture := func(ctx context.Context) []byte {
			reads++
			if mode == "wait" {
				return []byte(screen)
			}
			if reads == 1 {
				return managedSmokeCommand(t, ctx, "terminal_read", 6*time.Second, "/bin/sh", "-c", "printf %s "+screen)
			}
			return managedSmokeCommand(t, ctx, "terminal_read", 6*time.Second, "/bin/sh", "-c", "exec sleep 1")
		}
		if mode == "ready" {
			for _, normal := range normalScreens {
				f.awaitTerminalReady(child, pane, func(context.Context) []byte { return []byte(normal) })
			}
		} else {
			f.awaitTerminalReady(child, pane, capture)
		}
		input = true
		return
	}
	t.Run("display_negatives", func(t *testing.T) {
		for _, invalid := range []string{
			strings.Replace(normalScreens[0], "GPT-5.5", "gpt-5.5", 1),
			strings.Replace(normalScreens[0], "GPT-5.5", "GPT-6", 1),
			strings.Replace(normalScreens[0], " low", " high", 1),
			strings.Replace(normalScreens[0], "GPT-5.5 low", "GPT-5.5 low-extra", 1),
			strings.Replace(normalScreens[2], "← for agents", "← for history", 1),
			strings.Replace(normalScreens[2], "? for shortcuts", "? for shortcuts · extra", 1),
			"Try new model\nUse existing model\nGPT-5.5 low\n? for shortcuts\n",
			"Try new model\nUse existing model\nGPT-5.5 low\n← for agents · ? for shortcuts\n",
			"Error: GPT-5.5 low is unavailable\n? for shortcuts\n",
			"› Explain GPT-5.5 low\nAssistant: GPT-5.5 low\n? for shortcuts\n",
			"  GPT-5.5 low\n› Ask Codex to do anything\n? for shortcuts\n",
		} {
			if providerForkTerminalScreenReady([]byte(invalid)) {
				t.Fatal("non-ready public screen admitted")
			}
		}
	})
	for _, mode := range []string{"command", "wait", "ready", "missing"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			if os.Chmod(directory, 0700) != nil {
				t.Fatal("private control directory unavailable")
			}
			if mode == "missing" {
				directory = filepath.Join(directory, "absent")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedProviderForkTerminalReadyEvidence$", "-test.v")
			command.Env = append(os.Environ(), "MANAGED_PROVIDER_TERMINAL_EVIDENCE_CONTROL="+mode, "MANAGED_PROVIDER_NATIVE_EVIDENCE_DIR="+directory)
			output, err := command.CombinedOutput()
			text := string(output)
			if ctx.Err() != nil || command.ProcessState == nil || !strings.Contains(text, "terminal_cleanup_joined") || strings.Contains(text, "Error:") {
				t.Fatal("readiness control or cleanup failed")
			}
			if mode == "ready" {
				if err != nil || strings.Contains(text, "phase=terminal_evidence") {
					t.Fatal("ready path emitted refusal evidence")
				}
			} else {
				original := "phase=terminal_read"
				if mode == "wait" {
					original = "phase=terminal_ready result=deadline_exceeded"
				}
				refusal, handoff, cleanup := strings.Index(text, original), strings.Index(text, "phase=terminal_evidence"), strings.Index(text, "terminal_cleanup_joined")
				if err == nil || command.ProcessState.ExitCode() != 1 || refusal < 0 || handoff <= refusal || cleanup <= handoff || strings.Contains(text, "--- PASS") || mode != "missing" && !strings.Contains(text, "historical_screen_verified") || mode == "missing" && !strings.Contains(text, "outcome=unavailable") {
					t.Fatal("readiness evidence lost original refusal, sample or cleanup")
				}
			}
			for _, private := range []string{screen, child, pane, directory} {
				if strings.Contains(text, private) {
					t.Fatal("readiness evidence leaked private values")
				}
			}
		})
	}
}

func (f *managedProviderFork) assertControl(slug, root string, before any) {
	f.t.Helper()
	if f.summary(slug).Codex.ThreadID != root || !reflect.DeepEqual(f.threadSnapshot(root), before) {
		f.t.Fatal("explicit control changed")
	}
	status, data, err := f.smoke.http(f.smoke.context(), f.portal, "GET", "/api/codex-providers?session="+slug, nil)
	var view managedProviderForkView
	if err != nil || status != 200 || json.Unmarshal(data, &view) != nil || view.Active != "openai" || view.Inherited {
		f.t.Fatal("explicit control provider changed")
	}
}
func (f *managedProviderFork) close() {
	f.t.Log("phase=provider_cleanup")
	ctx := context.Background()
	accounted := !f.portalUnaccounted
	if f.portalUnaccounted {
		f.t.Error("phase=provider_cleanup portal_owner=unaccounted")
	}
	// Failure reporting never prevents the remaining exact owned joins.
	for _, key := range []string{"provider", "portal"} {
		if err := f.stopOwned(ctx, key); err != nil {
			f.t.Error("phase=provider_cleanup unit=" + key + " " + managedSmokeFailureText(err))
			accounted = false
		}
	}
	if err := f.stopProxy(); err != nil {
		f.t.Error("phase=provider_cleanup " + managedSmokeFailureText(err))
		accounted = false
	}
	for _, key := range []string{"native", "tmux"} {
		if err := f.stopOwned(ctx, key); err != nil {
			f.t.Error("phase=provider_cleanup unit=" + key + " " + managedSmokeFailureText(err))
			accounted = false
		}
	}
	if err := f.cleanupNativeEndpoints(); err != nil {
		f.t.Error("phase=provider_cleanup " + managedSmokeFailureText(err))
		accounted = false
	}
	for _, p := range f.providers {
		if p != nil {
			p.release()
			_ = p.server.Close()
			p.mu.Lock()
			failure := p.failure
			p.mu.Unlock()
			if failure != "" {
				f.t.Error(failure)
				accounted = false
			}
		}
	}
	if f.smoke.canary != nil {
		_ = f.smoke.canary.Close()
	}
	accounted = f.smoke.finalEgress(f.smoke.readCounter, f.t.Error) && accounted
	if !accounted {
		return
	} // Retain private state/unit definitions for uncertain effects.
	if unit := f.units["provider"]; unit != nil {
		if _, err := providerForkSystemctl(ctx, "disable", unit.name); err != nil {
			f.t.Error("phase=provider_cleanup result=unaccounted")
			return
		}
	}
	for _, path := range f.unitFiles {
		if err := os.Remove(path); err != nil {
			f.t.Error("phase=provider_cleanup result=unaccounted")
			return
		}
	}
	if len(f.unitFiles) > 0 {
		if _, err := providerForkSystemctl(ctx, "daemon-reload"); err != nil {
			f.t.Error("phase=provider_cleanup result=unaccounted")
			return
		}
	}
	_ = os.RemoveAll(f.smoke.root)
}
