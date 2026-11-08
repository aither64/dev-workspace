package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/coder/websocket"
)

func TestStartupReplacementRequiresPositiveProcessUnitCgroupAndListenerAbsence(t *testing.T) {
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		t.Fatal(e)
	}
	directory, e := os.MkdirTemp("/tmp", "ms-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	socket := filepath.Join(directory, "absent.sock")
	attempt := maintenance.StartupAttempt{PID: 123, ProcessIdentity: "boot:123:start", Boot: strings.TrimSpace(string(boot)), Cgroup: "/workspace-maintenance-absent-test-unit"}
	for _, scenario := range []string{"terminated", "live", "permission", "wrong-boot", "unit-starting", "unit-job", "unit-control", "unit-pid", "live-cgroup", "listener"} {
		t.Run(scenario, func(t *testing.T) {
			observed := attempt
			unit := startupUnitState{Active: "failed"}
			processErr := error(os.ErrNotExist)
			switch scenario {
			case "live":
				processErr = nil
			case "permission":
				processErr = os.ErrPermission
			case "wrong-boot":
				observed.Boot = "other"
			case "unit-starting":
				unit.Active = "activating"
			case "unit-job":
				unit.Job = "job"
			case "unit-control":
				unit.ControlPID = 42
			case "unit-pid":
				unit.PID = 42
			case "live-cgroup":
				group, err := processCgroup(os.Getpid())
				if err != nil {
					t.Skipf("live cgroup prerequisite unavailable: %v", err)
				}
				if !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
					t.Skipf("live cgroup prerequisite unavailable: self group %q is not an owned non-root path", group)
				}
				members, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", group, "cgroup.procs"))
				if err != nil {
					t.Skipf("live cgroup prerequisite unavailable: self membership: %v", err)
				}
				if !slices.Contains(strings.Fields(string(members)), strconv.Itoa(os.Getpid())) {
					t.Skip("live cgroup prerequisite unavailable: mapped membership does not contain self PID")
				}
				observed.Cgroup = group
			case "listener":
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			}
			e := proveFailedStartup(context.Background(), observed, unit, socket, processErr)
			if (e == nil) != (scenario == "terminated") {
				t.Fatal(e)
			}
		})
	}
}

func TestEmptyStartupCgroup(t *testing.T) {
	t.Run("invalid-group", func(t *testing.T) {
		for _, group := range []string{"", "relative", "/", "/unit/../other", "/unit/"} {
			if err := emptyStartupCgroup(group); err == nil || err.Error() != "invalid owned startup cgroup" {
				t.Fatalf("group %q: %v", group, err)
			}
		}
	})
	for _, scenario := range []string{"populated-root", "populated-descendant", "empty-tree", "missing-root", "membership-read-error"} {
		t.Run(scenario, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "cgroup")
			if scenario != "missing-root" {
				if err := os.MkdirAll(filepath.Join(root, "child"), 0700); err != nil {
					t.Fatal(err)
				}
				for _, directory := range []string{root, filepath.Join(root, "child")} {
					if err := os.WriteFile(filepath.Join(directory, "cgroup.procs"), []byte(" \n\t"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch scenario {
			case "populated-root", "populated-descendant":
				directory := root
				if scenario == "populated-descendant" {
					directory = filepath.Join(root, "child")
				}
				if err := os.WriteFile(filepath.Join(directory, "cgroup.procs"), []byte("123\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "membership-read-error":
				path := filepath.Join(root, "child", "cgroup.procs")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "missing-membership"), path); err != nil {
					t.Fatal(err)
				}
			}
			err := emptyStartupCgroupTree(root)
			switch scenario {
			case "populated-root", "populated-descendant":
				if err == nil || err.Error() != "startup cgroup still owns processes" {
					t.Fatal("populated cgroup was not refused", err)
				}
			case "membership-read-error":
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("membership read error was not propagated", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestProcessOwnsExactNativeListener(t *testing.T) {
	directory, e := os.MkdirTemp("/tmp", "ms-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	path := filepath.Join(directory, "native.sock")
	listener, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	if e = processOwnsListener(os.Getpid(), path); e != nil {
		t.Fatal(e)
	}
	if e = processOwnsListener(os.Getpid()+10000000, path); e == nil {
		t.Fatal("foreign process listener adopted")
	}
	if e = processOwnsListener(os.Getpid(), path+"-other"); e == nil {
		t.Fatal("wrong listener adopted")
	}
}

func TestStartupListenerAliasPreservesExactOwnership(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "msa-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	physical := filepath.Join(directory, "physical.sock")
	advertised := filepath.Join(directory, "advertised.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: physical, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	if err := os.Symlink(physical, advertised); err != nil {
		t.Fatal(err)
	}
	if err := processOwnsListener(os.Getpid(), physical); err != nil {
		t.Fatal(err)
	}
	t.Run("advertised-alias", func(t *testing.T) {
		if err := processOwnsListener(os.Getpid(), advertised); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("different-live-pid", func(t *testing.T) {
		child := exec.Command("cat")
		input, err := child.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Start(); err != nil {
			_ = input.Close()
			t.Fatal(err)
		}
		defer child.Wait()
		defer input.Close()
		if err := processOwnsListener(child.Process.Pid, advertised); err == nil || err.Error() != "consumed process does not own native listener" {
			t.Fatalf("live foreign PID must not own the listener: %v", err)
		}
	})
	t.Run("dangling-alias", func(t *testing.T) {
		dangling := filepath.Join(directory, "dangling.sock")
		if err := os.Symlink(physical+"-missing", dangling); err != nil {
			t.Fatal(err)
		}
		if err := processOwnsListener(os.Getpid(), dangling); err == nil {
			t.Fatal("dangling alias admitted")
		}
	})
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(physical); err != nil {
		t.Fatal("closed listener node was not retained", err)
	}
	if err := processOwnsListener(os.Getpid(), advertised); err == nil {
		t.Fatal("closed listener admitted")
	}
}

func TestStartupReconcilesRegisteredInactiveUnitsWithoutInventingSources(t *testing.T) {
	for _, active := range []int{1, 0} {
		for _, aliases := range []bool{false, true} {
			t.Run(fmt.Sprintf("active-sources-%d/aliases-%t", active, aliases), func(t *testing.T) {
				f := startupRegistrationFixture(t, active)
				f.socketAliases = aliases
				f.startTargets()
				f.consumeTargets()
				for retry := 0; retry < 2; retry++ {
					response, err := f.control.reconcileStartup(context.Background(), f.request)
					if err != nil || response.Status != "startup-reconciled" {
						t.Fatalf("recovery %d: %+v %v", retry, response, err)
					}
				}
				state, err := f.control.store.Startup()
				if err != nil {
					t.Fatal(err)
				}
				for _, grant := range state.LaunchGrants {
					if grant.Attempts[len(grant.Attempts)-1].Disposition != "ready" {
						t.Fatal("startup did not retain ready proof", grant)
					}
				}
				if err := f.control.store.FinishStartup(context.Background(), controlOperationID, f.candidate, "selected"); err != nil {
					t.Fatal(err)
				}
				state, err = f.control.store.Startup()
				if err != nil || state.Pending || !state.Enabled || len(state.LaunchGrants) != 2 {
					t.Fatalf("%+v %v", state, err)
				}
				// A completed same-operation retry must not reissue consumed startup grants.
				if _, err := f.control.reconcileStartup(context.Background(), f.request); err == nil {
					t.Fatal("completed full operation changed its recovery boundary")
				}
				if err := f.control.store.StageStartup(context.Background(), controlOperationID, f.candidate, "source", true); err != nil {
					t.Fatal(err)
				}
				current, err := f.control.store.Startup()
				if err != nil || !reflect.DeepEqual(state, current) {
					t.Fatal("completed startup retry changed its evidence", err)
				}
				f.requireOriginalSources()
			})
		}
	}
}

func TestStartupSocketAliasReplacementRefusesCompletion(t *testing.T) {
	f := startupRegistrationFixture(t, 1)
	f.socketAliases = true
	replacement := filepath.Join(f.root, "replacement.sock")
	listener, err := net.Listen("unix", replacement)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	retargeted := make(chan struct{}, 1)
	f.beforeLoadedList = func(advertised string) error {
		if err := os.Remove(advertised); err != nil {
			return err
		}
		if err := os.Symlink(replacement, advertised); err != nil {
			return err
		}
		retargeted <- struct{}{}
		return nil
	}
	f.startTargets()
	f.consumeTargets()
	before, err := f.control.store.Startup()
	if err != nil {
		t.Fatal(err)
	}
	operation, err := f.control.store.Load(controlOperationID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.control.reconcileStartup(context.Background(), f.request)
	if err == nil || err.Error() != "startup changed during readiness proof" {
		t.Fatalf("alias replacement was not refused: %+v %v", response, err)
	}
	select {
	case <-retargeted:
	default:
		t.Fatal("passive readiness boundary was not reached")
	}
	after, err := f.control.store.Startup()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("replacement changed consumed startup evidence", err)
	}
	current, err := f.control.store.Load(controlOperationID)
	if err != nil || !reflect.DeepEqual(operation, current) {
		t.Fatal("refusal certified operation or destination effects", err)
	}
	if err := f.control.store.FinishStartup(context.Background(), controlOperationID, f.candidate, "selected"); err == nil {
		t.Fatal("replaced endpoint completed startup")
	}
}

func TestStartupInactiveFailureRecoveryRetainsConsumedAttemptAndCompletes(t *testing.T) {
	f := startupRegistrationFixture(t, 0)
	failed := f.attempt
	failed.PID += 10000000
	failed.ProcessIdentity = "terminated-startup"
	failed.Cgroup = "/maintenance-startup-" + filepath.Base(f.root)
	grant := f.grants[0]
	f.writeUnit(grant.Unit, "failed", "old-invocation", 0, failed.Cgroup)
	if _, err := f.control.store.ConsumeStartupGrant(context.Background(), controlOperationID, f.candidate, "selected", "old-invocation", grant, failed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.reconcileStartup(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	state, err := f.control.store.Startup()
	if err != nil || state.LaunchGrants[0].Sequence != 2 || state.LaunchGrants[0].Attempts[0].Disposition != "failed" {
		t.Fatalf("failed attempt was lost: %+v %v", state, err)
	}
	if err := f.control.store.FinishStartup(context.Background(), controlOperationID, f.candidate, "selected"); err == nil {
		t.Fatal("unproved grants were discarded")
	}
	f.startTargets()
	f.consumeTargets()
	if _, err := f.control.reconcileStartup(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	if err := f.control.store.FinishStartup(context.Background(), controlOperationID, f.candidate, "selected"); err != nil {
		t.Fatal(err)
	}
	state, err = f.control.store.Startup()
	if err != nil || len(state.LaunchGrants[0].Attempts) != 2 || state.LaunchGrants[0].Attempts[0].Disposition != "failed" || state.LaunchGrants[0].Attempts[1].Disposition != "ready" {
		t.Fatalf("replacement history changed: %+v %v", state, err)
	}
	f.requireOriginalSources()
}

func TestStartupInactiveGrantStillRequiresRegisteredAndNativeIdentity(t *testing.T) {
	for _, fault := range []string{"registration", "invocation", "native", "argv", "profile"} {
		t.Run(fault, func(t *testing.T) {
			f := startupRegistrationFixture(t, 0)
			f.startTargets()
			f.consumeTargets()
			switch fault {
			case "registration":
				f.registry.Workspaces[0].Name = "foreign"
				f.writeRegistry()
			case "invocation":
				f.writeUnit(f.grants[0].Unit, "active", "foreign", os.Getpid(), f.attempt.Cgroup)
			case "native", "argv":
				grant := f.grants[0]
				if fault == "native" {
					grant.Native = "/foreign-native"
				} else {
					grant.ArgvDigest = "foreign"
				}
				f.writeMarker(0, grant)
			case "profile":
				if err := os.Remove(f.control.store.Profile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.root, f.control.store.Profile); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.control.reconcileStartup(context.Background(), f.request); err == nil {
				t.Fatal("changed startup identity was admitted")
			}
			state, err := f.control.store.Startup()
			if err != nil || state.LaunchGrants[0].Attempts[0].Disposition != "consumed" {
				t.Fatalf("uncertain attempt was lost: %+v %v", state, err)
			}
			if err := f.control.store.FinishStartup(context.Background(), controlOperationID, f.candidate, "selected"); err == nil {
				t.Fatal("unproved startup completed")
			}
		})
	}
}

type startupRegistrationTest struct {
	t                *testing.T
	root, candidate  string
	control          maintenanceControl
	operation        maintenance.Operation
	request          maintenance.Request
	registry         workspaceRegistry
	grants           []maintenance.StartupGrant
	attempt          maintenance.StartupAttempt
	socketAliases    bool
	beforeLoadedList func(string) error
}

func startupRegistrationFixture(t *testing.T, active int) *startupRegistrationTest {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "mst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := &startupRegistrationTest{t: t, root: root, candidate: filepath.Join(root, "candidate"), registry: workspaceRegistry{Schema: 1}}
	f.control = maintenanceControl{store: maintenance.Store{Root: filepath.Join(root, "state"), Profile: filepath.Join(root, "profile")}}
	f.request = maintenance.Request{Schema: 1, OperationID: controlOperationID, Startup: &maintenance.StartupSelection{CandidatePackage: f.candidate, AuthorizedProfileToken: "selected"}}
	if err := os.MkdirAll(f.candidate, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.candidate, f.control.store.Profile); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	group, err := processCgroup(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	f.attempt = maintenance.StartupAttempt{PID: os.Getpid(), ProcessIdentity: identity, Boot: strings.TrimSpace(string(boot)), Cgroup: group}
	f.operation = maintenance.Operation{Schema: maintenance.Schema, OperationID: controlOperationID, Revision: 1, Phase: "activating", Profile: f.control.store.Profile, SourceProfileToken: "source", SourcePackage: f.candidate, CandidatePackage: f.candidate, PlansDigest: "plans", CutoverIntent: true, ActivationIntent: true, ActivationPackage: f.candidate, ActivationNative: executable, ActivationPlansDigest: "plans", Services: []maintenance.Service{}, Participants: []maintenance.Participant{}}
	for i, name := range []string{"first", "second"} {
		workspace := filepath.Join(root, name)
		binding := workspaceBinding{Name: name, Root: workspace, Hostname: name + ".example.test"}
		f.registry.Workspaces = append(f.registry.Workspaces, binding)
		grant := maintenance.StartupGrant{Unit: "workspace-codex@" + name + ".service", Workspace: workspace, Native: executable, ArgvDigest: "argv-" + name}
		f.grants = append(f.grants, grant)
		socket := filepath.Join(root, "runtime", name, "app-server.sock")
		if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
			t.Fatal(err)
		}
		if i < active {
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			epoch, err := session.SocketIdentity(socket)
			if err != nil {
				t.Fatal(err)
			}
			listener.SetUnlinkOnClose(false)
			if err := os.Rename(socket, socket+".source"); err != nil {
				t.Fatal(err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			f.operation.Services = append(f.operation.Services, maintenance.Service{Workspace: workspace, Unit: grant.Unit, Native: executable, PID: os.Getpid(), ProcessIdentity: identity, Socket: socket, SocketEpoch: epoch})
		}
	}
	f.control.workspace = f.registry.Workspaces[0].Root
	t.Setenv("DEV_WORKSPACES_NAMESPACE", "dev-workspaces")
	t.Setenv("DEV_WORKSPACES_CONFIG", filepath.Join(root, "registry.json"))
	t.Setenv("DEV_WORKSPACES_RUNTIME_DIR", filepath.Join(root, "runtime"))
	f.writeRegistry()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "units"), 0700); err != nil {
		t.Fatal(err)
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!" + shell + "\n[ \"$1\" = --user ] && [ \"$2\" = show ] || exit 64\nexec cat -- \"$MAINTENANCE_STARTUP_TEST_UNITS/$3\"\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAINTENANCE_STARTUP_TEST_UNITS", filepath.Join(root, "units"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	if err := f.control.store.Create(ctx, f.operation); err != nil {
		t.Fatal(err)
	}
	if err := f.control.store.StageStartup(ctx, controlOperationID, f.candidate, "source", true); err != nil {
		t.Fatal(err)
	}
	if err := f.control.store.AuthorizeStartupGrants(ctx, controlOperationID, f.candidate, "selected", f.grants); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *startupRegistrationTest) writeRegistry() {
	f.t.Helper()
	data, err := json.Marshal(f.registry)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "registry.json"), data, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *startupRegistrationTest) writeUnit(unit, active, invocation string, pid int, group string) {
	f.t.Helper()
	data := fmt.Sprintf("InvocationID=%s\nActiveState=%s\nSubState=running\nMainPID=%d\nControlPID=0\nJob=\nControlGroup=%s\nResult=success\n", invocation, active, pid, group)
	if err := os.WriteFile(filepath.Join(f.root, "units", unit), []byte(data), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *startupRegistrationTest) writeMarker(index int, grant maintenance.StartupGrant) {
	f.t.Helper()
	binding := f.registry.Workspaces[index]
	data, err := json.Marshal(map[string]any{"workspace": map[string]string{"name": binding.Name, "root": binding.Root}, "launch": map[string]string{"package_root": f.candidate, "codex_path": grant.Native, "digest": grant.ArgvDigest}})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "runtime", binding.Name, "registration.json"), data, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *startupRegistrationTest) startTargets() {
	f.t.Helper()
	for i, binding := range f.registry.Workspaces {
		socket := filepath.Join(f.root, "runtime", binding.Name, "app-server.sock")
		physical := socket
		if f.socketAliases {
			physical = filepath.Join(f.root, "target-"+binding.Name+".sock")
		}
		listener, err := net.Listen("unix", physical)
		if err != nil {
			f.t.Fatal(err)
		}
		if f.socketAliases {
			if err := os.Symlink(physical, socket); err != nil {
				_ = listener.Close()
				f.t.Fatal(err)
			}
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				f.t.Error(err)
				return
			}
			defer conn.CloseNow()
			if err := threadCommandHandshake(conn); err != nil {
				f.t.Error(err)
				return
			}
			for {
				request, err := threadCommandReadObject(conn)
				if err != nil {
					return
				}
				if request["method"] != "thread/loaded/list" {
					f.t.Errorf("nonpassive startup RPC: %v", request["method"])
					return
				}
				if f.beforeLoadedList != nil {
					if err := f.beforeLoadedList(socket); err != nil {
						f.t.Error(err)
						return
					}
				}
				if err := threadCommandWriteObject(conn, map[string]any{"id": request["id"], "result": map[string]any{"data": []string{}}}); err != nil {
					f.t.Error(err)
					return
				}
			}
		})}
		go server.Serve(listener)
		f.t.Cleanup(func() { _ = server.Close() })
		f.writeMarker(i, f.grants[i])
		f.writeUnit(f.grants[i].Unit, "active", "target-"+binding.Name, os.Getpid(), f.attempt.Cgroup)
	}
}

func (f *startupRegistrationTest) consumeTargets() {
	f.t.Helper()
	for i, grant := range f.grants {
		if _, err := f.control.store.ConsumeStartupGrant(context.Background(), controlOperationID, f.candidate, "selected", "target-"+f.registry.Workspaces[i].Name, grant, f.attempt); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *startupRegistrationTest) requireOriginalSources() {
	f.t.Helper()
	o, err := f.control.store.Load(controlOperationID)
	if err != nil || !reflect.DeepEqual(o.Services, f.operation.Services) {
		f.t.Fatalf("source inventory was fabricated or changed: %+v %v", o.Services, err)
	}
}

func startupOnlyFixture(t *testing.T) *startupRegistrationTest {
	t.Helper()
	f := startupRegistrationFixture(t, 0)
	// Delete only the disposable full operation used to seed the shared helpers.
	if err := os.RemoveAll(filepath.Dir(f.control.store.Path(controlOperationID))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.control.store.Root, "maintenance", "active.json")); err != nil {
		t.Fatal(err)
	}
	f.startTargets()
	return f
}

func TestStartupOnlyConsumeAndReconcileUseRealSavedAuthority(t *testing.T) {
	f := startupOnlyFixture(t)
	for i, g := range f.grants {
		r := f.request
		r.Grant = &g
		r.PID = os.Getpid()
		r.Invocation = "target-" + f.registry.Workspaces[i].Name
		if enabled, err := f.control.consumeStartup(context.Background(), r); err != nil || !enabled {
			t.Fatalf("consume %t %v", enabled, err)
		}
	}
	if _, err := f.control.reconcileStartup(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	if err := f.control.store.FinishStartup(context.Background(), controlOperationID, f.candidate, "selected"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(f.control.store.Path(controlOperationID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fabricated operation directory", err)
	}
}

func TestStartupOnlyObserveExistingKeepsRealInvocationAndRefusesChangedEvidence(t *testing.T) {
	for _, fault := range []string{"none", "old-marker-package", "partial-owner", "full-owner", "marker", "unit", "socket", "registry", "token", "stale-selection", "already-consumed", "failed-attempt"} {
		t.Run(fault, func(t *testing.T) {
			f := startupOnlyFixture(t)
			g := f.grants[0]
			r := f.request
			r.Grant = &g
			if fault == "old-marker-package" {
				path := filepath.Join(f.root, "runtime", "first", "registration.json")
				b, _ := os.ReadFile(path)
				b = []byte(strings.ReplaceAll(string(b), f.candidate, f.root))
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			markerPath := filepath.Join(f.root, "runtime", "first", "registration.json")
			b, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			r.MarkerSHA256 = hex.EncodeToString(sum[:])
			switch fault {
			case "partial-owner":
				if err := os.Mkdir(filepath.Dir(f.control.store.Path(controlOperationID)), 0700); err != nil {
					t.Fatal(err)
				}
			case "full-owner":
				if err := f.control.store.Create(context.Background(), f.operation); err != nil {
					t.Fatal(err)
				}
			case "marker":
				r.MarkerSHA256 = strings.Repeat("0", 64)
			case "unit":
				f.writeUnit(g.Unit, "active", "foreign", os.Getpid()+10000000, f.attempt.Cgroup)
			case "registry":
				f.registry.Workspaces = f.registry.Workspaces[:1]
				f.writeRegistry()
			case "token":
				r.Startup = &maintenance.StartupSelection{CandidatePackage: f.candidate, AuthorizedProfileToken: "foreign"}
			case "already-consumed", "failed-attempt":
				if _, err := f.control.store.ConsumeStartupGrant(context.Background(), controlOperationID, f.candidate, "selected", "target-first", g, f.attempt); err != nil {
					t.Fatal(err)
				}
				if fault == "failed-attempt" {
					state, _ := f.control.store.Startup()
					cg := state.LaunchGrants[0]
					if err := f.control.store.ReconcileStartupAttempt(context.Background(), controlOperationID, f.candidate, "selected", cg, cg.Attempts[0], "failed", "", ""); err != nil {
						t.Fatal(err)
					}
				}
			case "socket":
				f.beforeLoadedList = func(socket string) error { return os.Rename(socket, socket+".replaced") }
			case "stale-selection":
				f.beforeLoadedList = func(string) error {
					state, _ := f.control.store.Startup()
					state.TargetEnabled = false
					data, _ := json.Marshal(state)
					return os.WriteFile(filepath.Join(f.control.store.Root, "maintenance", "startup.json"), data, 0600)
				}
			}
			before, _ := f.control.store.Startup()
			_, err = f.control.observeExistingStartup(context.Background(), r)
			success := fault == "none" || fault == "old-marker-package"
			if (err == nil) != success {
				t.Fatalf("observe %s: %v", fault, err)
			}
			after, _ := f.control.store.Startup()
			if !success && fault != "stale-selection" && !reflect.DeepEqual(before, after) {
				t.Fatal("refusal replaced retained evidence")
			}
			if success {
				a := after.LaunchGrants[0].Attempts
				if len(a) != 1 || a[0].Disposition != "ready" || a[0].PID != os.Getpid() || a[0].Invocation != "target-first" || after.LaunchGrants[0].Sequence != 1 {
					t.Fatalf("not actual observed ready: %+v", after)
				}
				if _, err := f.control.observeExistingStartup(context.Background(), r); err != nil {
					t.Fatal("exact replay", err)
				}
				again, _ := f.control.store.Startup()
				if !reflect.DeepEqual(after, again) {
					t.Fatal("replay rewrote proof")
				}
			}
		})
	}
}

// Re-exec only for Ruby's actual JSON controller transport. This never replaces
// startup success, native readiness, or store writes with a scripted response.
func TestStartupControlTransportHelper(t *testing.T) {
	if os.Getenv("STARTUP_CONTROL_TRANSPORT_HELPER") != "1" {
		return
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) > 0 && args[0] == "maintenance-control" {
		args = args[1:]
	}
	if err := maintenanceControlCommand(args, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStartupOnlyRubyTransportNoRestartEnableAndPendingRecovery(t *testing.T) {
	for _, mode := range []string{"enable", "consumed-launch", "recovery", "repair-executor", "absent-pending", "timer-failure-replay", "wrong-pending", "malformed-pending", "consumed-uncertain", "wrong-token", "wrong-claim", "wrong-registry", "wrong-source", "full-owner"} {
		t.Run(mode, func(t *testing.T) {
			f := startupOnlyFixture(t)
			digest := sha256.Sum256([]byte("[]"))
			for i := range f.grants {
				f.grants[i].ArgvDigest = hex.EncodeToString(digest[:])
				f.writeMarker(i, f.grants[i])
			}
			if err := os.Remove(filepath.Join(f.control.store.Root, "maintenance", "startup.json")); err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			shell, err := exec.LookPath("sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(f.candidate, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.candidate, "bin", "workspace-host"), []byte("#!"+shell+"\nexit 64\n"), 0700); err != nil {
				t.Fatal(err)
			}
			wrapper := "#!" + shell + "\nexec " + strconv.Quote(binary) + " -test.run=^TestStartupControlTransportHelper$ -- \"$@\"\n"
			if err := os.WriteFile(filepath.Join(f.candidate, "bin", "workspace-portal"), []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			source, err := filepath.Abs("../../../libexec/workspace-host")
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range []string{"auto-archive", "tracking-sync"} {
				path := filepath.Join(f.candidate, "share/systemd/user/workspace-"+family+"@.timer")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// The normal marker semantic comparison runs in Ruby; only registration
			// planning/decoding are fixture-local, as in the existing host unit helpers.
			config := map[string]any{"root": f.root, "package": f.candidate, "state": f.control.store.Root, "profile": f.control.store.Profile, "grants": f.grants, "entries": f.registry.Workspaces, "id": controlOperationID, "mode": mode}
			configPath := filepath.Join(f.root, "transport.json")
			data, _ := json.Marshal(config)
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			script := `require 'json'; require 'stringio'; load ARGV.shift
c=JSON.parse(File.read(ARGV.shift)); ENV['STARTUP_CONTROL_TRANSPORT_HELPER']='1'
env=ENV.to_h.merge('HOME'=>c.fetch('root'),'DEV_WORKSPACES_STATE'=>c.fetch('state'),'DEV_WORKSPACES_PROFILE'=>c.fetch('profile'),'DEV_WORKSPACES_SYSTEM_CODEX'=>c.fetch('grants').first.fetch('native'))
h=DevWorkspaceHost::Host.new(env:env,out:StringIO.new,err:StringIO.new)
executor = c['mode']=='repair-executor' ? File.join(c.fetch('root'),'repair-executor') : c.fetch('package')
if executor!=c.fetch('package'); FileUtils.mkdir_p(File.join(executor,'bin')); FileUtils.cp(File.join(c.fetch('package'),'bin/workspace-portal'),File.join(executor,'bin/workspace-portal'));end
h.define_singleton_method(:package_root){ executor }
entries=c.fetch('entries').map { |e| {'name'=>e.fetch('Name',e['name']), 'root'=>e.fetch('Root',e['root'])} }
plans=entries.zip(c.fetch('grants')).map { |entry,g| [entry,DevWorkspaceHost::RegistrationPlan.new(package_root:c.fetch('package'),argv:[],digest:g.fetch('argvDigest'),policy:1,required_native_child_threads:0,states:[])] }
h.define_singleton_method(:registration_plans) { |**| plans }
h.define_singleton_method(:load_registration_marker) { |e| JSON.parse(File.read(registration_marker_path(e))) }
contract_path=File.join(c.fetch('package'),'share/workspace-portal/runtime-contract.json');FileUtils.mkdir_p(File.dirname(contract_path));File.write(contract_path,JSON.generate(DevWorkspaceHost::RUNTIME_CONTRACT))
plans.each do |entry,plan|
 path=h.send(:registration_marker_path,entry); marker=JSON.parse(File.read(path)); marker['launch'].merge!('policy'=>1,'argv'=>[],'required_native_child_threads'=>0,'storage'=>h.send(:codex_storage,entry));File.write(path,JSON.generate(marker))
end
h.define_singleton_method(:portal_service_state) { |unit| File.readlines(File.join(c.fetch('root'),'units',unit)).to_h { |l| l.strip.split('=',2) } }
h.define_singleton_method(:wait_for_codex_sockets) { plans.each { |e,_| raise 'missing socket' unless File.socket?(instance_runtime(e).fetch(:codex)) } }
effects=[]
h.define_singleton_method(:system!) { |*a| raise 'native/service mutation' unless a[0,2]==['systemctl','--user'] && %w[daemon-reload enable].include?(a[2]); effects << a; if c['mode']=='timer-failure-replay' && !@failed_timer && a.include?('enable'); @failed_timer=true; raise DevWorkspaceHost::Error,'fixture timer failure';end }
h.define_singleton_method(:capture!) { |*a| a.include?('is-enabled') ? "enabled\n" : "active\n" }
h.define_singleton_method(:require_coordination_profile_claim!) { |_| raise DevWorkspaceHost::Error,'fixture claim refusal' if c['mode']=='wrong-claim' }
h.define_singleton_method(:coordination_source_identity) { |source| {'source'=>source} }
h.define_singleton_method(:coordination_build) { |source| (c['mode']=='wrong-source' ? c.fetch('root') : executor)+"\n" }
h.define_singleton_method(:require_no_unfinished_lifecycle_operations!) { |**| }
token=h.send(:profile_link_token)
prepared={'candidatePackage'=>c.fetch('package'),'candidateCodex'=>c.fetch('grants').first.fetch('native'),'maintenanceTarget'=>true,'profileToken'=>token,'switchOperationId'=>c.fetch('id')}
h.send(:maintenance_startup_request,'startup-stage',prepared)
h.send(:authorize_maintenance_startup,prepared)
if c['mode']=='consumed-launch'
 h.instance_variable_get(:@env)['INVOCATION_ID']='target-first';g=c.fetch('grants').first
 h.send(:maintenance_control,{'action'=>'startup-launch','operationId'=>c.fetch('id'),'pid'=>Integer(File.read(File.join(c.fetch('root'),'units',g.fetch('unit')))[/MainPID=(\d+)/,1]),'grant'=>g,'startup'=>{'candidatePackage'=>c.fetch('package'),'authorizedProfileToken'=>token}})
end
if %w[enable consumed-launch].include?(c['mode'])
 h.send(:complete_candidate_activation,{'candidate'=>c.fetch('package'),'plans'=>plans,'codex'=>prepared.fetch('candidateCodex'),'profileToken'=>token,'pending'=>nil,'restartRequired'=>false,'quiesced'=>[]})
else
 h.send(:write_pending_codex_update,c.fetch('package'),prepared.fetch('candidateCodex'),plans) unless c['mode']=='absent-pending'
 if c['mode']=='malformed-pending';File.write(h.send(:pending_codex_file),'{not-json');end
 if c['mode']=='consumed-uncertain'
  h.instance_variable_get(:@env)['INVOCATION_ID']='target-first'
  g=c.fetch('grants').first;h.send(:maintenance_control,{'action'=>'startup-launch','operationId'=>c.fetch('id'),'pid'=>Integer(File.read(File.join(c.fetch('root'),'units',g.fetch('unit')))[/MainPID=(\d+)/,1]),'invocation'=>'target-first','grant'=>g,'startup'=>{'candidatePackage'=>c.fetch('package'),'authorizedProfileToken'=>token}})
 end
 if c['mode']=='wrong-pending';path=h.send(:pending_codex_file);v=JSON.parse(File.read(path));v['package_root']=c.fetch('root');File.write(path,JSON.generate(v));end
 if c['mode']=='wrong-token';path=File.join(c.fetch('state'),'maintenance/startup.json');v=JSON.parse(File.read(path));v['sourceProfileToken']='foreign';File.write(path,JSON.generate(v));end
 if c['mode']=='wrong-registry';plans.pop;end
 if c['mode']=='full-owner';Dir.mkdir(File.join(c.fetch('state'),'maintenance',c.fetch('id')));end
 command=['maintenance','recover-startup',c.fetch('id'),'--source',c.fetch('root'),'--from-candidate']
 if c['mode']=='timer-failure-replay'
  raise 'timer failure accepted' unless h.run('workspace-host',command.dup)==1
  raise 'intent lost' unless h.send(:maintenance_startup_selection).fetch('pending')
  raise 'pending lost' unless h.send(:pending_codex_update)
 end
 code=h.run('workspace-host',command.dup)
 negative=%w[wrong-pending malformed-pending consumed-uncertain wrong-token wrong-claim wrong-registry wrong-source full-owner].include?(c['mode'])
 if negative
  raise 'invalid recovery accepted' unless code==1 && h.send(:maintenance_startup_selection).fetch('pending')
  raise 'mutation before refusal' unless effects.empty?
 else
  raise 'recovery failed: '+h.instance_variable_get(:@err).string unless code==0
  before=File.binread(File.join(c.fetch('state'),'maintenance/startup.json'));prior=effects.length
  raise 'completed replay refused' unless h.run('workspace-host',command.dup)==0
  raise 'completed replay rewrote or relaunched effects' unless before==File.binread(File.join(c.fetch('state'),'maintenance/startup.json')) && effects.length==prior
 end
end
state=h.send(:maintenance_startup_selection);negative=%w[wrong-pending malformed-pending consumed-uncertain wrong-token wrong-claim wrong-registry wrong-source full-owner].include?(c['mode'])
raise 'not completed' unless negative || state.fetch('enabled') && !state.fetch('pending')
raise 'fabricated operation' if !negative && File.exist?(File.join(c.fetch('state'),'maintenance',c.fetch('id')))
puts JSON.generate({'mode'=>c['mode'],'startup'=>state,'effects'=>effects})
`
			command := exec.Command("ruby", "-e", script, source, configPath)
			command.Env = append(os.Environ(), "STARTUP_CONTROL_TRANSPORT_HELPER=1")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("real Ruby/controller %s: %v\n%s", mode, err, output)
			}
			t.Logf("real Ruby/controller %s: %s", mode, output)
		})
	}
}

func TestStartupOnlyRoutingRequiresWholeDirectoryAbsence(t *testing.T) {
	for _, kind := range []string{"absent", "partial", "malformed", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			store := maintenance.Store{Root: root, Profile: filepath.Join(root, "profile")}
			dir := filepath.Dir(store.Path(controlOperationID))
			if kind != "absent" {
				if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "partial", "malformed":
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "malformed" {
					if err := os.WriteFile(store.Path(controlOperationID), []byte("{broken"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			}
			owner, err := maintenanceProtocolOwner(store, maintenance.Request{Action: "startup-stage", OperationID: controlOperationID})
			if kind == "absent" {
				if err != nil || owner != "" {
					t.Fatalf("first startup stage: %q %v", owner, err)
				}
			} else if err == nil {
				t.Fatal("partial/malformed owner was treated as startup-only")
			}
		})
	}
}
