package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type startupUnitState struct {
	Invocation, Active, Sub, Job, Cgroup, Result string
	PID, ControlPID                              int
}

func (m maintenanceControl) clientForSocket(socket string) *codex.Client {
	identity := sha256.Sum256([]byte(socket))
	base := filepath.Join(m.store.Root, "submissions", hex.EncodeToString(identity[:]))
	return codex.NewWithOptions(socket, codex.ClientOptions{AllowImplicitResume: func(string) bool { return false }, PreserveThreadInstructionsOnResume: true, ClientInfo: codex.ClientInfo{Name: "workspace-maintenance", Version: version}, SubmissionLedgerPath: base + ".json", SubmissionDatabasePath: base + ".sqlite", LegacySubmissionLedgerPath: socket + ".submission-attempts-v3.json"})
}

// A target grant is for a registered unit, including units that had no running
// source. Keep that target authority separate from the captured source set.
func startupTarget(o maintenance.Operation, grant maintenance.StartupGrant) (workspaceBinding, string, *maintenance.Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return workspaceBinding{}, "", nil, err
	}
	namespace := os.Getenv("DEV_WORKSPACES_NAMESPACE")
	if namespace == "" {
		namespace = "dev-workspaces"
	}
	if !workspaceNamePattern.MatchString(namespace) {
		return workspaceBinding{}, "", nil, errors.New("invalid startup workspace namespace")
	}
	config := os.Getenv("DEV_WORKSPACES_CONFIG")
	if config == "" {
		config = filepath.Join(home, ".config", namespace, "registry.json")
	}
	registry, err := loadWorkspaceRegistry(config)
	if err != nil {
		return workspaceBinding{}, "", nil, err
	}
	runtime := os.Getenv("DEV_WORKSPACES_RUNTIME_DIR")
	if runtime == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = fmt.Sprintf("/run/user/%d", os.Geteuid())
		}
		runtime = filepath.Join(base, namespace)
	}
	if !canonicalAbsolutePath(runtime) {
		return workspaceBinding{}, "", nil, errors.New("invalid startup runtime directory")
	}
	for _, binding := range registry.Workspaces {
		if binding.Root != grant.Workspace {
			continue
		}
		if grant.Unit != "workspace-codex@"+binding.Name+".service" {
			return workspaceBinding{}, "", nil, errors.New("startup registered unit tuple changed")
		}
		socket := filepath.Join(runtime, binding.Name, "app-server.sock")
		for i := range o.Services {
			source := &o.Services[i]
			if source.Workspace == grant.Workspace {
				if source.Unit != grant.Unit || source.Socket != socket {
					return workspaceBinding{}, "", nil, errors.New("startup source service tuple changed")
				}
				return binding, socket, source, nil
			}
		}
		return binding, socket, nil, nil
	}
	return workspaceBinding{}, "", nil, errors.New("startup workspace is no longer registered")
}

func readStartupUnit(ctx context.Context, unit string) (startupUnitState, error) {
	b, e := exec.CommandContext(ctx, "systemctl", "--user", "show", unit, "--property=InvocationID,ActiveState,SubState,MainPID,ControlPID,Job,ControlGroup,Result").Output()
	if e != nil {
		return startupUnitState{}, e
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		pair := strings.SplitN(line, "=", 2)
		if len(pair) == 2 {
			values[pair[0]] = pair[1]
		}
	}
	for _, key := range []string{"InvocationID", "ActiveState", "SubState", "MainPID", "ControlPID", "Job", "ControlGroup", "Result"} {
		if _, ok := values[key]; !ok {
			return startupUnitState{}, errors.New("incomplete startup unit observation")
		}
	}
	pid, e := strconv.Atoi(values["MainPID"])
	if e != nil {
		return startupUnitState{}, e
	}
	control, e := strconv.Atoi(values["ControlPID"])
	if e != nil {
		return startupUnitState{}, e
	}
	return startupUnitState{Invocation: values["InvocationID"], Active: values["ActiveState"], Sub: values["SubState"], Job: values["Job"], Cgroup: values["ControlGroup"], Result: values["Result"], PID: pid, ControlPID: control}, nil
}
func processCgroup(pid int) (string, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if e != nil {
		return "", e
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", errors.New("unified startup cgroup unavailable")
}
func (m maintenanceControl) consumeStartup(ctx context.Context, r maintenance.Request) (bool, error) {
	if r.Startup == nil || r.Grant == nil || r.PID <= 0 {
		return false, errors.New("startup consumption identity required")
	}
	o, e := m.store.Load(r.OperationID)
	if e != nil {
		return false, e
	}
	if !o.ActivationIntent || o.ActivationPackage != r.Startup.CandidatePackage {
		return false, errors.New("startup activation not admitted")
	}
	unit, e := readStartupUnit(ctx, r.Grant.Unit)
	if e != nil {
		return false, e
	}
	group, e := processCgroup(r.PID)
	if e != nil {
		return false, e
	}
	if unit.Invocation != r.Invocation || unit.PID != r.PID || group != unit.Cgroup || group == "" || unit.ControlPID != 0 {
		return false, errors.New("startup process is not the exact native unit invocation")
	}
	identity, e := processIdentity(r.PID)
	if e != nil {
		return false, e
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return false, e
	}
	return m.store.ConsumeStartupGrant(ctx, r.OperationID, r.Startup.CandidatePackage, r.Startup.AuthorizedProfileToken, r.Invocation, *r.Grant, maintenance.StartupAttempt{PID: r.PID, ProcessIdentity: identity, Boot: strings.TrimSpace(string(boot)), Cgroup: group})
}
func emptyStartupCgroup(group string) error {
	if !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
		return errors.New("invalid owned startup cgroup")
	}
	root := filepath.Join("/sys/fs/cgroup", group)
	return emptyStartupCgroupTree(root)
}

func emptyStartupCgroupTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() != "cgroup.procs" {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if len(strings.Fields(string(b))) != 0 {
			return errors.New("startup cgroup still owns processes")
		}
		return nil
	})
}
func noStartupListener(ctx context.Context, socket string) error {
	connection, e := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	if e == nil {
		connection.Close()
		return errors.New("startup socket still has a listener")
	}
	var operation *net.OpError
	if !errors.As(e, &operation) {
		return e
	}
	if !errors.Is(e, os.ErrNotExist) && !errors.Is(e, syscall.ECONNREFUSED) {
		return errors.New("startup listener absence is unproved")
	}
	return nil
}

// Reconciliation observes outside the short journal lock. The store compares
// the exact consumed attempt before retaining failure/authorizing one successor.
func (m maintenanceControl) reconcileStartup(ctx context.Context, r maintenance.Request) (maintenance.Response, error) {
	state, e := m.store.Startup()
	if e != nil {
		return maintenance.Response{}, e
	}
	o, e := m.store.Load(r.OperationID)
	if e != nil {
		return maintenance.Response{}, e
	}
	if !state.Pending || state.OperationID != o.OperationID || !o.ActivationIntent || state.CandidatePackage != o.ActivationPackage || r.Startup == nil || state.AuthorizedProfileToken != r.Startup.AuthorizedProfileToken {
		return maintenance.Response{}, errors.New("startup recovery authority changed")
	}
	selected, e := filepath.EvalSymlinks(m.store.Profile)
	if e != nil || selected != state.CandidatePackage {
		return maintenance.Response{}, errors.New("startup selected package changed")
	}
	status := "startup-reconciled"
	for _, grant := range state.LaunchGrants {
		if grant.Invocation == "" {
			continue
		}
		if len(grant.Attempts) == 0 {
			return maintenance.Response{}, errors.New("consumed startup lacks process proof")
		}
		attempt := grant.Attempts[len(grant.Attempts)-1]
		binding, socket, source, err := startupTarget(o, grant)
		if err != nil {
			return maintenance.Response{}, err
		}
		if grant.Native != o.ActivationNative || grant.ArgvDigest == "" {
			return maintenance.Response{}, errors.New("startup native tuple changed")
		}
		unit, err := readStartupUnit(ctx, grant.Unit)
		if err != nil {
			return maintenance.Response{}, err
		}
		identity, processErr := processIdentity(attempt.PID)
		if processErr == nil {
			if identity != attempt.ProcessIdentity || unit.Invocation != attempt.Invocation || unit.PID != attempt.PID {
				return maintenance.Response{}, errors.New("startup process or invocation changed")
			}
			group, groupErr := processCgroup(attempt.PID)
			if groupErr != nil || group != attempt.Cgroup || unit.Cgroup != attempt.Cgroup {
				return maintenance.Response{}, errors.New("startup cgroup identity changed")
			}
			exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", attempt.PID))
			if err != nil {
				return maintenance.Response{}, err
			}
			epoch, epochErr := session.SocketIdentity(socket)
			if !filepath.IsAbs(exe) || epochErr != nil || source != nil && epoch == source.SocketEpoch || unit.Active != "active" || unit.Job != "" {
				status = "retry-wait"
				continue
			}
			// Host verified the exact registration policy and argv before this action;
			// repeat its immutable digest/package/native projection on this socket.
			b, err := os.ReadFile(filepath.Join(filepath.Dir(socket), "registration.json"))
			if err != nil || len(b) > maintenance.MaxBytes {
				return maintenance.Response{}, errors.New("startup launch marker unavailable")
			}
			var raw map[string]json.RawMessage
			if json.Unmarshal(b, &raw) != nil {
				return maintenance.Response{}, errors.New("startup launch marker invalid")
			}
			var launch map[string]json.RawMessage
			var workspace map[string]string
			if json.Unmarshal(raw["launch"], &launch) != nil || json.Unmarshal(raw["workspace"], &workspace) != nil {
				return maintenance.Response{}, errors.New("startup launch marker invalid")
			}
			read := func(key string) string { var value string; _ = json.Unmarshal(launch[key], &value); return value }
			if workspace["name"] != binding.Name || workspace["root"] != grant.Workspace || read("package_root") != state.CandidatePackage || read("codex_path") != grant.Native || read("digest") != grant.ArgvDigest {
				return maintenance.Response{}, errors.New("startup launch marker tuple changed")
			}
			if err = processOwnsListener(attempt.PID, socket); err != nil {
				return maintenance.Response{}, err
			}
			passive := m.clientForSocket(socket)
			_, rpcErr := passive.LoadedThreadIDs(ctx)
			passive.Close()
			if rpcErr != nil {
				status = "retry-wait"
				continue
			}
			currentIdentity, identityErr := processIdentity(attempt.PID)
			currentEpoch, epochErr := session.SocketIdentity(socket)
			if identityErr != nil || epochErr != nil || currentIdentity != attempt.ProcessIdentity || currentEpoch != epoch {
				return maintenance.Response{}, errors.New("startup changed during readiness proof")
			}

			if err = m.store.ReconcileStartupAttempt(ctx, o.OperationID, state.CandidatePackage, state.AuthorizedProfileToken, grant, attempt, "ready", exe, epoch); err != nil {
				return maintenance.Response{}, err
			}
			continue
		}
		if err = proveFailedStartup(ctx, attempt, unit, socket, processErr); err != nil {
			return maintenance.Response{}, err
		}
		markerPath := filepath.Join(filepath.Dir(socket), "registration.json")
		if b, readErr := os.ReadFile(markerPath); readErr == nil {
			if len(b) > maintenance.MaxBytes {
				return maintenance.Response{}, errors.New("startup marker exceeds bound")
			}
			var marker map[string]json.RawMessage
			var launch map[string]json.RawMessage
			if json.Unmarshal(b, &marker) != nil || json.Unmarshal(marker["launch"], &launch) != nil {
				return maintenance.Response{}, errors.New("startup marker malformed")
			}
			read := func(key string) string { var v string; _ = json.Unmarshal(launch[key], &v); return v }
			if read("package_root") != state.CandidatePackage || read("codex_path") != grant.Native || read("digest") != grant.ArgvDigest {
				return maintenance.Response{}, errors.New("failed startup marker tuple changed")
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return maintenance.Response{}, readErr
		}
		if source != nil && source.DestinationEpoch != "" || o.ContinuationsSettled || o.Restoration != nil || attempt.Disposition == "ready" {
			return maintenance.Response{}, errors.New("successful startup effects remain unresolved")
		}
		for _, p := range o.Participants {
			if p.Workspace == grant.Workspace && (p.ContinuationDisposition == "submitting" || p.ContinuationDisposition == "unknown" || p.ContinuationDisposition == "accepted" || p.ContinuationDisposition == "activated") {
				return maintenance.Response{}, errors.New("startup continuation effects remain unresolved")
			}
		}
		freshUnit, observeErr := readStartupUnit(ctx, grant.Unit)
		if observeErr != nil {
			return maintenance.Response{}, observeErr
		}
		_, freshProcessErr := processIdentity(attempt.PID)
		if err = proveFailedStartup(ctx, attempt, freshUnit, socket, freshProcessErr); err != nil {
			return maintenance.Response{}, err
		}
		if err = m.store.ReconcileStartupAttempt(ctx, o.OperationID, state.CandidatePackage, state.AuthorizedProfileToken, grant, attempt, "failed", "", ""); err != nil {
			return maintenance.Response{}, err
		}
	}
	return maintenance.Response{Schema: 1, Status: status}, nil
}

func processOwnsListener(pid int, path string) error {
	// Native advertises a symlink to its protected socket; /proc records the
	// physical bind pathname. Keep resolution local to this ownership lookup.
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil {
		return e
	}
	b, e := os.ReadFile("/proc/net/unix")
	if e != nil {
		return e
	}
	inode := ""
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 8 && fields[7] == resolved && fields[3] == "00010000" {
			if inode != "" {
				return errors.New("ambiguous startup listeners")
			}
			inode = fields[6]
		}
	}
	if inode == "" {
		return errors.New("startup listener identity unavailable")
	}
	entries, e := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if e != nil {
		return e
	}
	for _, entry := range entries {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name()))
		if err == nil && target == "socket:["+inode+"]" {
			return nil
		}
	}
	return errors.New("consumed process does not own native listener")
}

func proveFailedStartup(ctx context.Context, attempt maintenance.StartupAttempt, unit startupUnitState, socket string, processErr error) error {
	if !errors.Is(processErr, os.ErrNotExist) {
		return errors.New("consumed process absence unproved")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) != attempt.Boot {
		return errors.New("startup boot identity changed")
	}
	if unit.Active != "inactive" && unit.Active != "failed" || unit.PID != 0 || unit.ControlPID != 0 || unit.Job != "" {
		return errors.New("startup unit still owns unfinished work")
	}
	if err = emptyStartupCgroup(attempt.Cgroup); err != nil {
		return err
	}
	return noStartupListener(ctx, socket)
}
