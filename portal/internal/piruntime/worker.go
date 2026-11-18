package piruntime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type workerMetadata struct {
	Schema     int    `json:"schema"`
	Protocol   int    `json:"worker_protocol"`
	Node       string `json:"node_executable"`
	Entrypoint string `json:"worker_entrypoint"`
	PiPackage  string `json:"pi_package_root"`
}
type workerConfig struct {
	Instructions    string            `json:"instructions,omitempty"`
	ToolPolicy      *ToolPolicyTarget `json:"toolPolicy,omitempty"`
	Generation      string            `json:"generation"`
	HostTools       []string          `json:"hostTools,omitempty"`
	CallbackSubject *CallbackSubject  `json:"callbackSubject,omitempty"`
	onHostCall      func(context.Context, nativeHostCall) (HostReply, error)
	AccountBindings []AccountBinding  `json:"accountBindings"`
	NativeID        string            `json:"nativeId"`
	EmptyProven     bool              `json:"emptyProven"`
	Cwd             string            `json:"cwd"`
	AgentDir        string            `json:"agentDir"`
	HistoryDir      string            `json:"historyDir"`
	HistoryFile     string            `json:"historyFile,omitempty"`
	AuthDirectory   string            `json:"authDirectory"`
	ModelsPath      string            `json:"modelsPath,omitempty"`
	PiPackage       string            `json:"piPackage"`
	Settings        EffectiveSettings `json:"settings"`
}

type workerSnapshot struct {
	InstructionsDigest string            `json:"instructionsDigest,omitempty"`
	HistoryStart       int               `json:"historyStart"`
	HistoryTotal       int               `json:"historyTotal"`
	HistoryDigest      string            `json:"historyDigest"`
	NativeID           string            `json:"nativeId"`
	HistoryFile        string            `json:"historyFile"`
	Persisted          bool              `json:"persisted"`
	LeafID             string            `json:"leafId"`
	Entries            []json.RawMessage `json:"entries"`
	LiveMessage        json.RawMessage   `json:"liveMessage"`
	Busy               bool              `json:"busy"`
	Held               bool              `json:"held"`
	Model              string            `json:"model"`
	Provider           string            `json:"provider"`
	Thinking           string            `json:"thinking"`
}

func (snapshot *workerSnapshot) UnmarshalJSON(data []byte) error {
	type plain workerSnapshot
	var value plain
	if err := strictDecode(data, &value, recordLimit); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return ErrScope
	}
	if _, present := fields["instructionsDigest"]; present && !hexPattern.MatchString(value.InstructionsDigest) {
		return ErrScope
	}
	*snapshot = workerSnapshot(value)
	return nil
}

func instructionsDigest(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type workerReply struct {
	Schema      int             `json:"schema"`
	ID          string          `json:"id,omitempty"`
	Identity    workerIdentity  `json:"identity"`
	Body        json.RawMessage `json:"body,omitempty"`
	Error       string          `json:"error,omitempty"`
	Event       string          `json:"event,omitempty"`
	NativeEvent json.RawMessage `json:"nativeEvent,omitempty"`
}
type worker interface {
	Call(context.Context, string, any, any) error
	Wait(context.Context) error
	PID() (int, string)
}

// workerProcess is a member endpoint. Wait/PID always describe its physical host.
type workerProcess struct {
	instructionsDigest string
	handle             workerIdentity
	host               *workerHost
	mu                 sync.Mutex
	pending            map[string]chan workerReply
	changed            func()
	hostPending        map[string]context.CancelFunc
	hostHandler        func(context.Context, nativeHostCall) (HostReply, error)
}
type hostAdmission struct {
	mu        sync.Mutex
	active    int
	detaching bool
}
type hostFenceKey struct{}

func (a *hostAdmission) enter(ctx context.Context) (func(), error) {
	if ctx.Value(hostFenceKey{}) == a {
		return func() {}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.detaching {
		return nil, ErrHeld
	}
	a.active++
	return func() { a.mu.Lock(); a.active--; a.mu.Unlock() }, nil
}
func (a *hostAdmission) detach(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.detaching || a.active != 0 {
		return ctx, nil, ErrHeld
	}
	a.detaching = true
	return context.WithValue(ctx, hostFenceKey{}, a), func() { a.mu.Lock(); a.detaching = false; a.mu.Unlock() }, nil
}

type workerHost struct {
	command   *exec.Cmd
	input     io.WriteCloser
	writeMu   sync.Mutex
	mu        sync.Mutex
	endpoints map[string]*workerProcess
	closed    chan struct{}
	start     string
	exitErr   error
	shared    bool
	admission *hostAdmission
}

func startWorker(ctx context.Context, metadata workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, changed func()) (worker, error) {
	return startOwnedWorker(ctx, metadata, retainedWorkerIdentity(handle), config, changed)
}
func startOwnedWorker(ctx context.Context, metadata workerMetadata, handle workerIdentity, config workerConfig, changed func()) (worker, error) {
	h, err := startWorkerHost(metadata, config.Cwd, &hostAdmission{})
	if err != nil {
		return nil, err
	}
	p, err := h.attach(ctx, handle, config, changed)
	if err != nil {
		_ = h.command.Process.Kill()
		<-h.closed
		return nil, err
	}
	return p, nil
}
func startWorkerHost(metadata workerMetadata, cwd string, admission *hostAdmission) (*workerHost, error) {
	command := exec.Command(metadata.Node, metadata.Entrypoint)
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "NIX_PATH": true, "NIX_CONFIG": true, "NIX_SSL_CERT_FILE": true, "SSL_CERT_FILE": true}
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if allowed[key] {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env, "PI_OFFLINE=1")
	command.Dir = cwd
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = io.Discard
	if err = command.Start(); err != nil {
		return nil, err
	}
	h := &workerHost{command: command, input: input, endpoints: map[string]*workerProcess{}, closed: make(chan struct{}), shared: metadata.Protocol == 2, admission: admission}
	h.start, err = processStart(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	go h.read(output)
	return h, nil
}
func (h *workerHost) attach(ctx context.Context, handle workerIdentity, config workerConfig, changed func()) (*workerProcess, error) {
	if validateInstructions(config.Instructions) != nil {
		return nil, ErrScope
	}
	leave, err := h.admission.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	p := &workerProcess{handle: handle, host: h, pending: map[string]chan workerReply{}, changed: changed, hostPending: map[string]context.CancelFunc{}, hostHandler: config.onHostCall, instructionsDigest: instructionsDigest(config.Instructions)}
	h.mu.Lock()
	select {
	case <-h.closed:
		h.mu.Unlock()
		return nil, ErrHeld
	default:
	}
	key := digestValue(handle)
	if h.endpoints[key] != nil {
		h.mu.Unlock()
		return nil, ErrConflict
	}
	h.endpoints[key] = p
	h.mu.Unlock()
	var initial workerSnapshot
	if err = p.Call(ctx, "init", config, &initial); err != nil {
		return nil, err
	}
	if initial.NativeID != handle.nativeID() {
		return nil, ErrScope
	}
	return p, nil
}
func (h *workerHost) read(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), recordLimit)
	for scanner.Scan() {
		var reply workerReply
		if err := strictDecode(scanner.Bytes(), &reply, recordLimit); err != nil || reply.Schema != 1 {
			_ = h.command.Process.Kill()
			break
		}
		h.mu.Lock()
		p := h.endpoints[digestValue(reply.Identity)]
		h.mu.Unlock()
		if p == nil || !equalJSON(reply.Identity, p.handle) {
			_ = h.command.Process.Kill()
			break
		}
		if reply.Event == "host_call" || reply.Event == "host_cancel" {
			p.hostFrame(reply)
			continue
		}
		if reply.Event == "invalidate" {
			if p.changed != nil {
				p.changed()
			}
			continue
		}
		p.mu.Lock()
		response := p.pending[reply.ID]
		if response != nil {
			delete(p.pending, reply.ID)
		}
		p.mu.Unlock()
		if response != nil {
			response <- reply
		}
	}
	if scanner.Err() != nil {
		_ = h.command.Process.Kill()
	}
	h.exitErr = h.command.Wait()
	h.mu.Lock()
	endpoints := make([]*workerProcess, 0, len(h.endpoints))
	for _, p := range h.endpoints {
		endpoints = append(endpoints, p)
	}
	h.mu.Unlock()
	for _, p := range endpoints {
		p.mu.Lock()
		for _, cancel := range p.hostPending {
			cancel()
		}
		for id, response := range p.pending {
			response <- workerReply{Error: "worker exited"}
			delete(p.pending, id)
		}
		p.mu.Unlock()
	}
	close(h.closed)
	for _, p := range endpoints {
		if p.changed != nil {
			p.changed()
		}
	}
}
func (h *workerHost) write(data []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err := h.input.Write(data)
	return err
}
func (p *workerProcess) Call(ctx context.Context, operation string, body, result any) error {
	leave, err := p.host.admission.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	id := newUUID()
	response := make(chan workerReply, 1)
	p.mu.Lock()
	select {
	case <-p.host.closed:
		p.mu.Unlock()
		return ErrHeld
	default:
	}
	p.pending[id] = response
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	request := struct {
		Schema    int            `json:"schema"`
		ID        string         `json:"id"`
		Identity  workerIdentity `json:"identity"`
		Operation string         `json:"op"`
		Body      any            `json:"body"`
	}{1, id, p.handle, operation, body}
	bytes, err := json.Marshal(request)
	if err != nil || len(bytes) > recordLimit {
		return ErrScope
	}
	err = p.host.write(append(bytes, '\n'))
	if err != nil {
		return err
	}
	select {
	case reply := <-response:
		if reply.Error != "" {
			return ErrHeld
		}
		if operation == "unload" {
			if p.host.shared {
				var ack struct {
					Detached bool `json:"detached"`
				}
				if strictDecode(reply.Body, &ack, recordLimit) != nil || !ack.Detached {
					return ErrHeld
				}
				p.host.mu.Lock()
				delete(p.host.endpoints, digestValue(p.handle))
				last := len(p.host.endpoints) == 0
				p.host.mu.Unlock()
				if last {
					_ = p.host.input.Close()
				}
			} else {
				_ = p.host.input.Close()
			}
		}
		if result == nil {
			return nil
		}
		if operation == "workflow_observe" {
			return strictDecode(reply.Body, result, recordLimit)
		}
		if err := json.Unmarshal(reply.Body, result); err != nil {
			return err
		}
		if snapshot, ok := result.(*workerSnapshot); ok && snapshot.InstructionsDigest != p.instructionsDigest {
			return ErrHeld
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.host.closed:
		return ErrHeld
	}
}
func (p *workerProcess) PID() (int, string) { return p.host.command.Process.Pid, p.host.start }
func (p *workerProcess) Wait(ctx context.Context) error {
	select {
	case <-p.host.closed:
		return p.host.exitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func processGroupEmpty(pid int) bool { return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) }

func (p *workerProcess) hostFrame(frame workerReply) {
	if !uuidPattern.MatchString(frame.ID) {
		return
	}
	p.mu.Lock()
	if frame.Event == "host_cancel" {
		cancel := p.hostPending[frame.ID]
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	if existing := p.hostPending[frame.ID]; existing != nil {
		p.mu.Unlock()
		existing()
		_ = p.hostResult(frame.ID, HostReply{Outcome: "unknown", Code: "conflict"})
		return
	}
	if len(p.hostPending) >= 16 || p.hostHandler == nil {
		p.mu.Unlock()
		_ = p.hostResult(frame.ID, HostReply{Outcome: "refused", Code: "unavailable"})
		return
	}
	var call nativeHostCall
	if strictDecode(frame.Body, &call, callbackLimit) != nil {
		p.mu.Unlock()
		_ = p.hostResult(frame.ID, HostReply{Outcome: "refused", Code: "invalid"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	p.hostPending[frame.ID] = cancel
	p.mu.Unlock()
	go func() {
		defer cancel()
		defer func() { p.mu.Lock(); delete(p.hostPending, frame.ID); p.mu.Unlock() }()
		reply, err := p.hostHandler(ctx, call)
		if err != nil && reply.Outcome == "" {
			reply = HostReply{Outcome: "unknown", Code: "unknown"}
		}
		if !validHostReply(reply) {
			reply = HostReply{Outcome: "unknown", Code: "unknown"}
		}
		_ = p.hostResult(frame.ID, reply)
	}()
}
func (p *workerProcess) hostResult(id string, reply HostReply) error {
	data, err := marshalWire(struct {
		Schema    int            `json:"schema"`
		ID        string         `json:"id"`
		Identity  workerIdentity `json:"identity"`
		Operation string         `json:"op"`
		Body      HostReply      `json:"body"`
	}{1, id, p.handle, "host_result", reply})
	if err != nil {
		return err
	}
	return p.host.write(data)
}
