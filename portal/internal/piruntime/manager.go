package piruntime

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
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type Host interface {
	Start(context.Context, RuntimeRef) (HostObservation, error)
	Inspect(context.Context, RuntimeRef) (HostObservation, error)
}
type Config struct {
	AccountBindings                                         []AccountBinding
	UserStateRoot, HostStateRoot, AuthDirectory, HostHelper string
	Host                                                    Host
	Dial                                                    func(context.Context, string) (net.Conn, error)
	retainPackage                                           func(context.Context, string, string) error
	verifyProcess                                           func(HostObservation) error
}
type Manager struct{ config Config }
type runtimeConfig struct {
	Schema          int              `json:"schema"`
	Scope           LaunchScope      `json:"scope"`
	InstanceID      string           `json:"instance_id"`
	RequestID       string           `json:"request_id"`
	Generation      string           `json:"generation"`
	LaunchDigest    string           `json:"launch_request_sha256"`
	UserStateRoot   string           `json:"user_state_root"`
	AuthDirectory   string           `json:"auth_directory"`
	AccountBindings []AccountBinding `json:"account_bindings"`
}
type runtimeRecord struct {
	Schema                              int
	Start                               StartRequest
	Runtime                             RuntimeRef
	RuntimeConfigSHA256                 string
	Phase, HostStateRoot, AuthDirectory string
	AccountBindings                     []AccountBinding
}

func ownedDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", ErrUnavailable
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0700 != 0700 {
		return "", ErrUnavailable
	}
	if err := syscall.Access(canonical, 7); err != nil {
		return "", err
	}
	return canonical, nil
}

func NewManager(config Config) (*Manager, error) {
	var err error
	if err := validateAccountBindings(config.AccountBindings); err != nil {
		return nil, err
	}
	config.AccountBindings = append([]AccountBinding{}, config.AccountBindings...)
	if config.AuthDirectory, err = ownedDirectory(config.AuthDirectory); err != nil {
		return nil, fmt.Errorf("Pi auth directory unavailable: %w", err)
	}
	if config.UserStateRoot, err = ownedDirectory(config.UserStateRoot); err != nil {
		return nil, err
	}
	if config.HostStateRoot, err = ownedDirectory(config.HostStateRoot); err != nil {
		return nil, err
	}
	if strings.ContainsAny(config.HostStateRoot, " \t\n\r:%") {
		return nil, ErrScope
	}
	if strings.HasPrefix(config.AuthDirectory, config.UserStateRoot+"/pi-runtime/") || strings.HasPrefix(config.AuthDirectory, config.HostStateRoot+"/") {
		return nil, ErrScope
	}
	if config.Host == nil {
		path := config.HostHelper
		if path == "" {
			path = "/run/current-system/sw/bin/workspace-pi-host"
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil || !storeFile(path) || filepath.Base(path) != "workspace-pi-host" {
			return nil, ErrUnavailable
		}
		config.Host = nativeHost{path: path}
	}
	if config.Dial == nil {
		config.Dial = func(ctx context.Context, path string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
	}
	if config.verifyProcess == nil {
		config.verifyProcess = verifyProcess
	}
	if config.retainPackage == nil {
		config.retainPackage = func(ctx context.Context, root, app string) error {
			return exec.CommandContext(ctx, "nix-store", "--add-root", root, "--indirect", "--realise", app).Run()
		}
	}
	return &Manager{config: config}, nil
}

func stateDirectory(root string, ref agentbackend.SessionRef) (string, error) {
	if err := validateSession(ref); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(ref.Workspace))
	return filepath.Join(root, "pi-runtime", hex.EncodeToString(digest[:]), ref.Slug, ref.Incarnation), nil
}
func (m *Manager) instance(ref RuntimeRef) string {
	return filepath.Join(m.config.HostStateRoot, "instances", ref.InstanceID)
}
func runtimeLock(directory string) (func(), error) {
	if err := makePrivateDirectory(directory); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, ErrHeld
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
func freezeBytes(path string, data []byte) error {
	old, err := readPrivateBytes(path, 65536)
	if err == nil {
		if string(old) != string(data) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writePrivateBytes(path, data)
}

func (m *Manager) record(ref agentbackend.SessionRef) (runtimeRecord, error) {
	var record runtimeRecord
	dir, err := stateDirectory(m.config.UserStateRoot, ref)
	if err != nil {
		return record, err
	}
	err = readPrivate(filepath.Join(dir, "runtime.json"), &record)
	if err == nil && (record.Schema != 1 || record.Runtime.Session != ref || record.Start.Session != ref || !hexPattern.MatchString(record.RuntimeConfigSHA256) || record.HostStateRoot != m.config.HostStateRoot || record.AuthDirectory != m.config.AuthDirectory || !equalJSON(record.AccountBindings, m.config.AccountBindings) || (record.Phase != "preparing" && record.Phase != "starting")) {
		return record, ErrScope
	}
	return record, err
}

func (m *Manager) Ensure(ctx context.Context, start StartRequest) (RuntimeStatus, error) {
	directory, err := stateDirectory(m.config.UserStateRoot, start.Session)
	if err != nil {
		return RuntimeStatus{}, err
	}
	if !uuidPattern.MatchString(start.RequestID) || start.BindingRevision == 0 || !storePattern.MatchString(start.AppStorePath) || !filepath.IsAbs(start.ProfilePath) || start.ProfileGeneration == "" {
		return RuntimeStatus{}, ErrScope
	}
	release, err := runtimeLock(directory)
	if err != nil {
		return RuntimeStatus{}, err
	}
	defer release()

	record, err := m.record(start.Session)
	if err == nil {
		if record.Start != start {
			return RuntimeStatus{}, ErrConflict
		}
		if record.Phase != "preparing" {
			return m.Inspect(ctx, record.Runtime)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return RuntimeStatus{}, err
	} else {
		ref := RuntimeRef{Session: start.Session, InstanceID: newUUID(), RequestID: start.RequestID, Generation: randomHex(32)}
		launch := launchFor(start, ref)
		encoded, _ := json.Marshal(launch)
		encoded = append(encoded, '\n')
		_, ref.Digest, err = DecodeLaunch(encoded)
		if err != nil {
			return RuntimeStatus{}, err
		}
		config := m.runtimeConfiguration(launch, ref)
		configBytes, _ := json.Marshal(config)
		configBytes = append(configBytes, '\n')
		sum := sha256.Sum256(configBytes)
		record = runtimeRecord{Schema: 1, Start: start, Runtime: ref, RuntimeConfigSHA256: hex.EncodeToString(sum[:]), Phase: "preparing", HostStateRoot: m.config.HostStateRoot, AuthDirectory: m.config.AuthDirectory, AccountBindings: append([]AccountBinding{}, m.config.AccountBindings...)}
		// Reserve the only instance/generation before preparation can be interrupted.
		if err := writePrivate(filepath.Join(directory, "runtime.json"), record); err != nil {
			return RuntimeStatus{}, err
		}
	}
	ref := record.Runtime
	instance := m.instance(ref)
	for _, name := range []string{"receipts", "tmp", "var-tmp", "requests/" + ref.RequestID} {
		if err := makePrivateDirectory(filepath.Join(instance, name)); err != nil {
			return RuntimeStatus{}, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(instance, "launch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return RuntimeStatus{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return RuntimeStatus{}, ErrHeld
	}
	launch := launchFor(start, ref)
	encoded, _ := json.Marshal(launch)
	encoded = append(encoded, '\n')
	_, digest, err := DecodeLaunch(encoded)
	if err != nil || digest != ref.Digest {
		return RuntimeStatus{}, ErrConflict
	}
	if err := freezeBytes(filepath.Join(instance, "launch.json"), encoded); err != nil {
		return RuntimeStatus{}, err
	}
	configBytes, _ := json.Marshal(m.runtimeConfiguration(launch, ref))
	configBytes = append(configBytes, '\n')
	sum := sha256.Sum256(configBytes)
	if hex.EncodeToString(sum[:]) != record.RuntimeConfigSHA256 {
		return RuntimeStatus{}, ErrConflict
	}
	if err := freezeBytes(filepath.Join(instance, "requests", ref.RequestID, "runtime-config.json"), configBytes); err != nil {
		return RuntimeStatus{}, err
	}
	root := filepath.Join(instance, "package-root")
	if target, err := os.Readlink(root); err == nil {
		if target != start.AppStorePath {
			return RuntimeStatus{}, ErrConflict
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(start.AppStorePath, root); err != nil {
			return RuntimeStatus{}, err
		}
	} else {
		return RuntimeStatus{}, err
	}
	folder, err := os.Open(instance)
	if err != nil {
		return RuntimeStatus{}, err
	}
	syncErr := folder.Sync()
	_ = folder.Close()
	if syncErr != nil {
		return RuntimeStatus{}, syncErr
	}
	if err := m.config.retainPackage(ctx, root, start.AppStorePath); err != nil {
		return RuntimeStatus{}, fmt.Errorf("retain Pi closure: %w", err)
	}
	record.Phase = "starting"
	if err := writePrivate(filepath.Join(directory, "runtime.json"), record); err != nil {
		return RuntimeStatus{}, err
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	observation, err := m.config.Host.Start(ctx, ref)
	status := RuntimeStatus{Runtime: ref, Phase: "unknown", Observation: observation, ObservedAt: time.Now().UTC()}
	if err != nil {
		return status, err
	}
	return m.inspectObservation(ctx, record, observation)
}
func launchFor(start StartRequest, ref RuntimeRef) LaunchRequest {
	return LaunchRequest{Schema: 1, HostContract: 1, Scope: LaunchScope{Workspace: start.Session.Workspace, Slug: start.Session.Slug, SessionIncarnation: start.Session.Incarnation, Backend: "pi", BackendContract: 1, BindingRevision: start.BindingRevision}, InstanceID: ref.InstanceID, RequestID: ref.RequestID, Generation: ref.Generation, AppStorePath: start.AppStorePath, ProfilePath: start.ProfilePath, ProfileGeneration: start.ProfileGeneration}
}
func (m *Manager) runtimeConfiguration(launch LaunchRequest, ref RuntimeRef) runtimeConfig {
	return runtimeConfig{Schema: 1, Scope: launch.Scope, InstanceID: ref.InstanceID, RequestID: ref.RequestID, Generation: ref.Generation, LaunchDigest: ref.Digest, UserStateRoot: m.config.UserStateRoot, AuthDirectory: m.config.AuthDirectory, AccountBindings: append([]AccountBinding{}, m.config.AccountBindings...)}
}

func (m *Manager) Inspect(ctx context.Context, ref RuntimeRef) (RuntimeStatus, error) {
	record, err := m.record(ref.Session)
	if err != nil {
		return RuntimeStatus{}, err
	}
	if record.Runtime != ref {
		return RuntimeStatus{}, ErrScope
	}
	observation, err := m.config.Host.Inspect(ctx, ref)
	if err != nil {
		return RuntimeStatus{Runtime: ref, Phase: "unknown"}, err
	}
	return m.inspectObservation(ctx, record, observation)
}
func (m *Manager) inspectObservation(ctx context.Context, record runtimeRecord, observed HostObservation) (RuntimeStatus, error) {
	status := RuntimeStatus{Runtime: record.Runtime, Phase: "unknown", Observation: observed, ObservedAt: time.Now().UTC()}
	ref := record.Runtime
	if observed.LaunchRequest != launchFor(record.Start, ref) || observed.InstanceID != ref.InstanceID || observed.RequestID != ref.RequestID || observed.Digest != ref.Digest || observed.Generation != ref.Generation || observed.AppStorePath != record.Start.AppStorePath {
		return status, ErrScope
	}
	if observed.Outcome != "running" {
		status.Phase = observed.Outcome
		return status, nil
	}
	var handshake Handshake
	if err := m.callRaw(ctx, ref, "handshake", struct{}{}, &handshake); err != nil {
		status.Phase = "starting"
		return status, err
	}
	if handshake.RuntimeConfigSHA256 != record.RuntimeConfigSHA256 || !sameExecution(observed, handshake.HostObservation) {
		return status, ErrScope
	}
	if err := m.config.verifyProcess(observed); err != nil {
		return status, err
	}
	status.Phase = "ready"
	status.Handshake = &handshake
	return status, nil
}
func sameExecution(a, b HostObservation) bool {
	return a.Schema == 1 && b.Schema == 1 && a.HostContract == 1 && a.Scope == b.Scope && a.InstanceID == b.InstanceID && a.RequestID == b.RequestID && a.Digest == b.Digest && a.Generation == b.Generation && a.AppStorePath == b.AppStorePath && a.BootID != "" && a.BootID == b.BootID && a.InvocationID != "" && a.InvocationID == b.InvocationID && a.ControlGroup != "" && a.ControlGroup == b.ControlGroup && a.MainPID > 0 && a.MainPID == b.MainPID && a.PIDStart != "" && a.PIDStart == b.PIDStart && a.MountNamespace != "" && a.MountNamespace == b.MountNamespace && a.UserNamespace != "" && a.UserNamespace == b.UserNamespace
}

type nativeHost struct{ path string }

func (h nativeHost) request(ctx context.Context, verb string, ref RuntimeRef) (HostObservation, error) {
	var result HostObservation
	cmd := exec.CommandContext(ctx, "/run/wrappers/bin/sudo", "-n", h.path, verb, "--instance", ref.InstanceID, "--request", ref.RequestID, "--digest", ref.Digest)
	output, err := cmd.Output()
	if err != nil {
		return result, ErrUnavailable
	}
	if err := strictDecode(output, &result, recordLimit); err != nil {
		return result, err
	}
	return result, nil
}
func (h nativeHost) Start(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	return h.request(ctx, "start", ref)
}
func (h nativeHost) Inspect(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	return h.request(ctx, "inspect", ref)
}

func equalJSON(a, b any) bool { return reflect.DeepEqual(a, b) }
