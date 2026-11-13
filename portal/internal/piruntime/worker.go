package piruntime

import (
	"bufio"
	"context"
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
	PiPackage       string            `json:"piPackage"`
	Settings        EffectiveSettings `json:"settings"`
}
type workerSnapshot struct {
	HistoryStart  int               `json:"historyStart"`
	HistoryTotal  int               `json:"historyTotal"`
	HistoryDigest string            `json:"historyDigest"`
	NativeID      string            `json:"nativeId"`
	HistoryFile   string            `json:"historyFile"`
	Persisted     bool              `json:"persisted"`
	LeafID        string            `json:"leafId"`
	Entries       []json.RawMessage `json:"entries"`
	LiveMessage   json.RawMessage   `json:"liveMessage"`
	Busy          bool              `json:"busy"`
	Held          bool              `json:"held"`
	Model         string            `json:"model"`
	Provider      string            `json:"provider"`
	Thinking      string            `json:"thinking"`
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
type workerProcess struct {
	handle      workerIdentity
	command     *exec.Cmd
	input       io.WriteCloser
	mu          sync.Mutex
	writeMu     sync.Mutex
	pending     map[string]chan workerReply
	closed      chan struct{}
	start       string
	exitErr     error
	changed     func()
	hostPending map[string]context.CancelFunc
	hostHandler func(context.Context, nativeHostCall) (HostReply, error)
}

func startWorker(ctx context.Context, metadata workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, changed func()) (worker, error) {
	return startOwnedWorker(ctx, metadata, retainedWorkerIdentity(handle), config, changed)
}
func startOwnedWorker(ctx context.Context, metadata workerMetadata, handle workerIdentity, config workerConfig, changed func()) (worker, error) {
	command := exec.Command(metadata.Node, metadata.Entrypoint)
	// H0's ordinary tool environment is retained, without ambient provider
	// credentials. Model auth is the explicitly configured canonical directory.
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "NIX_PATH": true, "NIX_CONFIG": true, "NIX_SSL_CERT_FILE": true, "SSL_CERT_FILE": true}
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if allowed[key] {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env, "PI_OFFLINE=1")
	command.Dir = config.Cwd
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Never log raw provider diagnostics or credential-bearing child stderr.
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &workerProcess{handle: handle, command: command, input: input, pending: map[string]chan workerReply{}, closed: make(chan struct{}), changed: changed, hostPending: map[string]context.CancelFunc{}, hostHandler: config.onHostCall}
	process.start, err = processStart(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), recordLimit)
		for scanner.Scan() {
			var reply workerReply
			if err := strictDecode(scanner.Bytes(), &reply, recordLimit); err != nil || reply.Schema != 1 || !equalJSON(reply.Identity, handle) {
				_ = command.Process.Kill()
				break
			}
			if reply.Event == "host_call" || reply.Event == "host_cancel" {
				process.hostFrame(reply)
				continue
			}
			if reply.Event == "invalidate" {
				if changed != nil {
					changed()
				}
				continue
			}
			process.mu.Lock()
			response := process.pending[reply.ID]
			if response != nil {
				delete(process.pending, reply.ID)
			}
			process.mu.Unlock()
			if response != nil {
				response <- reply
			}
		}
		if scanner.Err() != nil {
			_ = command.Process.Kill()
		}
		process.exitErr = command.Wait()
		process.mu.Lock()
		for _, cancel := range process.hostPending {
			cancel()
		}
		for id, response := range process.pending {
			response <- workerReply{Error: "worker exited"}
			delete(process.pending, id)
		}
		close(process.closed)
		process.mu.Unlock()
		if changed != nil {
			changed()
		}
	}()
	var initial workerSnapshot
	if err := process.Call(ctx, "init", config, &initial); err != nil {
		_ = input.Close()
		_ = command.Process.Kill()
		<-process.closed
		return nil, err
	}
	if initial.NativeID != handle.nativeID() {
		_ = command.Process.Kill()
		<-process.closed
		return nil, ErrScope
	}
	return process, nil
}
func (p *workerProcess) Call(ctx context.Context, operation string, body, result any) error {
	id := newUUID()
	response := make(chan workerReply, 1)
	p.mu.Lock()
	select {
	case <-p.closed:
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
	p.writeMu.Lock()
	_, err = p.input.Write(append(bytes, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case reply := <-response:
		if reply.Error != "" {
			return ErrHeld
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(reply.Body, result)
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closed:
		return ErrHeld
	}
}
func (p *workerProcess) PID() (int, string) { return p.command.Process.Pid, p.start }
func (p *workerProcess) Wait(ctx context.Context) error {
	select {
	case <-p.closed:
		return p.exitErr
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
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.input.Write(data)
	return err
}
