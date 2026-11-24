package piruntime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type supervisor struct {
	hostMu                       sync.Mutex
	sharedHost                   *workerHost
	hostAdmission                hostAdmission
	inputContext                 context.Context
	inputTasks                   sync.WaitGroup
	workflow                     workflowState
	callbackMu                   sync.Mutex
	attachment                   *hostAttachment
	ref                          RuntimeRef
	launch                       LaunchRequest
	config                       runtimeConfig
	handshake                    Handshake
	metadata                     workerMetadata
	conversationIdentityContract func(string) (bool, error)
	modelsPath                   string
	stateRoot                    string
	mu                           sync.Mutex
	members                      map[string]*conversation
	childrenCheck                func() (bool, error)
	start                        func(context.Context, workerMetadata, agentbackend.ExecutionHandle, workerConfig, func()) (worker, error)
	reclaimControl               bool
	verifiedInstanceDirectory    string
}

// Supervise is the fixed unprivileged H0 entry point. No workers are loaded
// until the controller has independently checked this process's handshake.
func Supervise(ctx context.Context, path, instance, request, digest string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "launch.json" || filepath.Base(filepath.Dir(path)) != instance {
		return ErrScope
	}
	data, err := readPrivateBytes(path, 65536)
	if err != nil {
		return err
	}
	launch, actual, err := DecodeLaunch(data)
	if err != nil || actual != digest || launch.InstanceID != instance || launch.RequestID != request {
		return ErrScope
	}
	configuration, hash, err := readRuntimeConfiguration(filepath.Join(filepath.Dir(path), "requests", request, "runtime-config.json"), launch, digest)
	if err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(filepath.Join(launch.AppStorePath, "bin/workspace-portal"))
	if err != nil {
		return ErrScope
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil || executable != expected {
		return ErrScope
	}
	metadata, err := readWorkerMetadata(launch.AppStorePath)
	if err != nil {
		return err
	}
	pid := os.Getpid()
	start, err := processStart(pid)
	if err != nil {
		return err
	}
	mount, err := processNamespace(pid, "mnt")
	if err != nil {
		return err
	}
	user, err := processNamespace(pid, "user")
	if err != nil {
		return err
	}
	group, err := processCgroup(pid)
	if err != nil {
		return err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	invocation := os.Getenv("INVOCATION_ID")
	if len(invocation) != 32 || !operationPattern.MatchString(invocation) || !strings.HasSuffix(group, "/workspace-pi-session@"+instance+".service") {
		return ErrScope
	}
	observation := HostObservation{LaunchRequest: launch, Digest: digest, Phase: "running", Outcome: "running", Unit: "workspace-pi-session@" + instance + ".service", BootID: strings.TrimSpace(string(boot)), InvocationID: invocation, ControlGroup: group, MainPID: pid, PIDStart: start, MountNamespace: mount, UserNamespace: user}
	s, err := newSupervisor(launch, configuration, Handshake{HostObservation: observation, RuntimeConfigSHA256: hash}, metadata)
	if err != nil {
		return err
	}
	s.verifiedInstanceDirectory = filepath.Dir(path)
	if launch.ReplacesRequestID != "" {
		if err := s.validateSuccessorStartup(filepath.Dir(path)); err != nil {
			return err
		}
		s.reclaimControl = true
	}
	return s.serve(ctx, filepath.Dir(path))
}

func readPrivateBytes(path string, limit int) ([]byte, error) {
	var raw json.RawMessage
	if err := readPrivate(path, &raw); err != nil {
		return nil, err
	}
	// Preserve exact frozen bytes, including the final newline, for hashing.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, ErrScope
	}
	return data, nil
}

func readRuntimeConfiguration(path string, launch LaunchRequest, digest string) (runtimeConfig, string, error) {
	var config runtimeConfig
	data, err := readPrivateBytes(path, 65536)
	if err != nil {
		return config, "", err
	}
	if err := strictDecode(data, &config, 65536); err != nil {
		return config, "", err
	}
	if validateAccountBindings(config.AccountBindings) != nil {
		return config, "", ErrUnavailable
	}
	if config.Schema != 1 || config.Scope != launch.Scope || config.InstanceID != launch.InstanceID || config.RequestID != launch.RequestID || config.Generation != launch.Generation || config.LaunchDigest != digest {
		return config, "", ErrScope
	}
	for _, path := range []*string{&config.UserStateRoot, &config.AuthDirectory} {
		canonical, err := ownedDirectory(*path)
		if err != nil || canonical != *path {
			return config, "", ErrScope
		}
	}
	if strings.HasPrefix(config.AuthDirectory, config.UserStateRoot+"/pi-runtime/") {
		return config, "", ErrScope
	}
	sum := sha256.Sum256(data)
	return config, hex.EncodeToString(sum[:]), nil
}

func newSupervisor(launch LaunchRequest, config runtimeConfig, handshake Handshake, metadata workerMetadata) (*supervisor, error) {
	models, err := LoadInstalledModelConfiguration(launch.AppStorePath)
	if err != nil {
		return nil, err
	}
	if models.Path != "" {
		matched := false
		for _, binding := range config.AccountBindings {
			matched = matched || binding.Provider == "openai"
		}
		if !matched {
			return nil, ErrUnavailable
		}
	}
	ref := RuntimeRef{Session: agentbackend.SessionRef{Workspace: launch.Scope.Workspace, Slug: launch.Scope.Slug, Incarnation: launch.Scope.SessionIncarnation}, InstanceID: launch.InstanceID, RequestID: launch.RequestID, Digest: handshake.Digest, Generation: launch.Generation}
	root, err := stateDirectory(config.UserStateRoot, ref.Session)
	if err != nil {
		return nil, err
	}
	s := &supervisor{ref: ref, launch: launch, config: config, handshake: handshake, metadata: metadata, modelsPath: models.Path, stateRoot: root, members: map[string]*conversation{}, start: startWorker, conversationIdentityContract: readConversationIdentityContract}
	if metadata.Protocol == 2 {
		s.start = func(ctx context.Context, metadata workerMetadata, handle agentbackend.ExecutionHandle, cfg workerConfig, changed func()) (worker, error) {
			return s.startShared(ctx, retainedWorkerIdentity(handle), cfg, changed)
		}
	}
	return s, nil
}

func (s *supervisor) serve(ctx context.Context, instance string) error {
	var cancel context.CancelFunc
	s.inputContext, cancel = context.WithCancel(ctx)
	defer func() { cancel(); s.inputTasks.Wait() }()
	directory, err := os.Open(instance)
	if err != nil {
		return err
	}
	defer directory.Close()
	path := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), "control.sock")
	if err := prepareControlSocket(ctx, directory, s.reclaimControl, nil); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(filepath.Join(instance, "control.sock"), 0600); err != nil {
		return err
	}
	return s.acceptControl(ctx, listener)
}

func (s *supervisor) acceptControl(ctx context.Context, listener interface {
	AcceptUnix() (*net.UnixConn, error)
	Close() error
}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	closed := make(chan error, 1)
	stopClose := context.AfterFunc(ctx, func() { closed <- listener.Close() })
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if stopClose() {
				return err
			}
			// Accept can wake before Close returns. Cancellation alone does not
			// attribute an Accept failure (even a closed listener) to shutdown.
			if closeErr := <-closed; closeErr != nil {
				if errors.Is(closeErr, net.ErrClosed) {
					return err // Already closed by something other than this callback.
				}
				return errors.Join(err, closeErr)
			}
			if errors.Is(err, net.ErrClosed) {
				// The serving context is owned by the supervisor's signal handler.
				// Exit status does not replace host receipt or cgroup drain proof.
				switch ctx.Err() {
				case context.Canceled:
					return nil
				case context.DeadlineExceeded:
					return context.DeadlineExceeded
				}
			}
			return err
		}
		go s.connection(ctx, connection)
	}
}

func (s *supervisor) connection(ctx context.Context, connection *net.UnixConn) {
	defer connection.Close()
	uid, err := peerUID(connection)
	if err != nil || uid != uint32(os.Geteuid()) {
		return
	}
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), recordLimit)
	if !scanner.Scan() {
		return
	}
	var request controlRequest
	if strictDecode(scanner.Bytes(), &request, recordLimit) != nil {
		return
	}
	if request.Schema == 1 && uuidPattern.MatchString(request.ID) && request.Runtime == s.ref && request.Operation == "host_attach" {
		s.attachHost(connection, request)
		return
	}
	// Ordinary control calls are one request/reply, with no client write-half-close.
	// Keep the scanner: a second frame already buffered by the first read must
	// revoke admission just like EOF. This context ends only this operation, not
	// the accepted native run or the supervisor lifetime.
	operationCtx, cancelOperation := context.WithCancel(ctx)
	if request.Schema == 1 && uuidPattern.MatchString(request.ID) && request.Runtime == s.ref {
		monitorDone := make(chan struct{})
		go func() { scanner.Scan(); cancelOperation(); close(monitorDone) }()
		defer func() { cancelOperation(); _ = connection.Close(); <-monitorDone }()
	} else {
		defer cancelOperation()
	}
	response := controlResponse{Schema: 1, ID: request.ID, Runtime: s.ref}
	if request.Schema != 1 || !uuidPattern.MatchString(request.ID) || request.Runtime != s.ref {
		response.Error = "scope"
	} else {
		result, err := s.invoke(operationCtx, request.Operation, request.Body)
		response.Body, _ = json.Marshal(result)
		if err != nil {
			response.Error = errorCode(err)
			if request.Operation == "workflow_observe" && err == errWorkflowObservationChanged {
				if operationCtx.Err() == nil {
					response.Error = workflowObservationChangedCode
				} else {
					response.Body = nil
				}
			}
		}
	}
	data, err := json.Marshal(response)
	if err != nil || len(data) > recordLimit {
		return
	}
	_, _ = connection.Write(append(data, '\n'))
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrScope):
		return "scope"
	case errors.Is(err, ErrHeld):
		return "held"
	case errors.Is(err, ErrConflict):
		return "conflict"
	default:
		return "unavailable"
	}
}

func decodeBody(data []byte, out any) error { return strictDecode(data, out, recordLimit) }
func (s *supervisor) invoke(ctx context.Context, operation string, data []byte) (any, error) {
	if s.metadata.Protocol == 2 && operation != "fork" && operation != "unload" && operation != "utility_unload" && operation != "handshake" {
		leave, err := s.hostAdmission.enter(ctx)
		if err != nil {
			return nil, err
		}
		defer leave()
	}

	if workflowMutates(operation) {
		admitted, finish, err := s.beginWorkflowMutation(ctx)
		if err != nil {
			return nil, err
		}
		defer finish()
		ctx = admitted
	}
	switch operation {
	case "input_delivery_checked_v1":
		var request QueuedInputRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.dispatchQueuedInput(ctx, request)
	case "input_lookup_v1":
		var ref SubmissionRef
		if err := decodeBody(data, &ref); err != nil {
			return nil, err
		}
		return s.lookupQueuedInput(ctx, ref)
	case "fork":
		var request forkOperation
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.fork(ctx, request)
	case "fork_init", "fork_recover":
		var request ForkRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		if operation == "fork_recover" {
			return s.recoverFork(ctx, request)
		}
		return s.forkCreate(ctx, request)
	case "fork_verify":
		var request forkVerification
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.verifyFork(ctx, request)
	case "send_checked":
		var request checkedSubmissionRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.sendCheckedSubmission(ctx, request)
	case "workflow_observe", "tool_policy_inspect", "tool_policy_apply":
		var request toolPolicyRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		if operation == "workflow_observe" {
			if request.ExpectedRevision != 0 || request.Target != nil {
				return nil, ErrScope
			}
			return s.sampleWorkflow(ctx, request.Handle, true)
		}
		if operation == "tool_policy_inspect" {
			return s.inspectToolPolicy(ctx, request.Handle)
		}
		if request.Target == nil {
			return nil, ErrScope
		}
		return s.applyToolPolicy(ctx, request.Handle, request.ExpectedRevision, *request.Target)
	case "utility_create", "utility_load", "utility_bind", "utility_send", "utility_unload":
		var request utilityRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.invokeUtility(ctx, operation, request)
	case "utility_snapshot":
		var request utilityReadRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.utilitySnapshot(ctx, request)
	case "handshake":
		return s.handshake, nil
	case "create":
		var request CreateRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.create(ctx, request)
	case "lookup":
		var ref SubmissionRef
		if err := decodeBody(data, &ref); err != nil {
			return nil, err
		}
		return s.lookup(ref)
	case "resources":
		return s.resources()
	case "start_resource":
		var request ResourceRequest
		if err := decodeBody(data, &request); err != nil {
			return nil, err
		}
		return s.startResource(ctx, request)
	case "inspect_resource", "stop_resource":
		var handle ResourceHandle
		if err := decodeBody(data, &handle); err != nil {
			return nil, err
		}
		if operation == "stop_resource" {
			return s.stopResource(ctx, handle)
		}
		return s.inspectResource(handle)
	}
	var request conversationRequest
	if err := decodeBody(data, &request); err != nil {
		return nil, err
	}
	if err := s.verifyHandle(request.Handle); err != nil {
		return nil, err
	}
	switch operation {
	case "fork_capture":
		return s.captureFork(ctx, request.Handle)
	case "load":
		return s.load(ctx, request.Handle)
	case "identity_v1":
		return s.conversationIdentity(ctx, request.Handle)
	case "snapshot":
		return s.snapshot(ctx, request.Handle, request.Before)
	case "transcript_snapshot":
		return s.transcriptSnapshot(ctx, request.Handle, request.Before)
	case "activity_v1":
		return s.activity(ctx, request.Handle)
	case "input_capabilities_v1":
		c, err := s.member(request.Handle.Conversation)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.worker == nil {
			return nil, ErrUnavailable
		}
		var capabilities struct {
			Version int `json:"version"`
		}
		err = c.worker.Call(ctx, "input_capabilities_v1", struct{}{}, &capabilities)
		return capabilities, err
	case "input_delivery_v1":
		return s.inputDelivery(ctx, request.Handle, request.Send)
	case "send":
		return s.send(ctx, request.Handle, request.Send)
	case "configure":
		return s.configure(ctx, request.Handle, request.Settings)
	case "unload":
		return s.unload(ctx, request.Handle)
	case "abort":
		return s.abort(ctx, request.Handle)
	case "verify_creation":
		return s.verifyCreation(ctx, request)
	case "inspect_origin":
		if request.Origin == nil {
			return nil, ErrScope
		}
		return s.inspectOrigin(ctx, request.Handle, *request.Origin)
	default:
		return nil, ErrUnavailable
	}
}

func (s *supervisor) verifyHandle(handle agentbackend.ExecutionHandle) error {
	if err := validateHandle(handle); err != nil {
		return err
	}
	if handle.Conversation.Session != s.ref.Session || handle.Generation != s.ref.Generation {
		return ErrScope
	}
	return nil
}

func (s *supervisor) startShared(ctx context.Context, identity workerIdentity, cfg workerConfig, changed func()) (worker, error) {
	if identity.subject().Kind == "conversation" && identity.Conversation.Session != s.ref.Session {
		return nil, ErrScope
	}
	if identity.OwnerKind == "utility" && (identity.Utility == nil || identity.Utility.Session != s.ref.Session) {
		return nil, ErrScope
	}
	s.hostMu.Lock()
	host := s.sharedHost
	if host != nil {
		select {
		case <-host.closed:
			host.mu.Lock()
			empty := len(host.endpoints) == 0
			host.mu.Unlock()
			if !empty || host.exitErr != nil {
				s.hostMu.Unlock()
				return nil, ErrHeld
			}
			host = nil
		default:
		}
	}
	if host == nil {
		var err error
		host, err = startWorkerHost(s.metadata, cfg.Cwd, &s.hostAdmission)
		if err != nil {
			s.hostMu.Unlock()
			return nil, err
		}
		s.sharedHost = host
	}
	s.hostMu.Unlock()
	return host.attach(ctx, identity, cfg, changed)
}
