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
	NativeID      string            `json:"nativeId"`
	EmptyProven   bool              `json:"emptyProven"`
	Cwd           string            `json:"cwd"`
	AgentDir      string            `json:"agentDir"`
	HistoryDir    string            `json:"historyDir"`
	HistoryFile   string            `json:"historyFile,omitempty"`
	AuthDirectory string            `json:"authDirectory"`
	PiPackage     string            `json:"piPackage"`
	Settings      EffectiveSettings `json:"settings"`
}
type workerSnapshot struct {
	NativeID    string            `json:"nativeId"`
	HistoryFile string            `json:"historyFile"`
	Persisted   bool              `json:"persisted"`
	LeafID      string            `json:"leafId"`
	Entries     []json.RawMessage `json:"entries"`
	Busy        bool              `json:"busy"`
	Held        bool              `json:"held"`
	Model       string            `json:"model"`
	Provider    string            `json:"provider"`
	Thinking    string            `json:"thinking"`
}
type workerReply struct {
	Schema      int                          `json:"schema"`
	ID          string                       `json:"id,omitempty"`
	Identity    agentbackend.ExecutionHandle `json:"identity"`
	Body        json.RawMessage              `json:"body,omitempty"`
	Error       string                       `json:"error,omitempty"`
	Event       string                       `json:"event,omitempty"`
	NativeEvent json.RawMessage              `json:"nativeEvent,omitempty"`
}
type worker interface {
	Call(context.Context, string, any, any) error
	Wait(context.Context) error
	PID() (int, string)
}
type workerProcess struct {
	handle  agentbackend.ExecutionHandle
	command *exec.Cmd
	input   io.WriteCloser
	mu      sync.Mutex
	writeMu sync.Mutex
	pending map[string]chan workerReply
	closed  chan struct{}
	start   string
	exitErr error
	changed func()
}

func startWorker(ctx context.Context, metadata workerMetadata, handle agentbackend.ExecutionHandle, config workerConfig, changed func()) (worker, error) {
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
	process := &workerProcess{handle: handle, command: command, input: input, pending: map[string]chan workerReply{}, closed: make(chan struct{}), changed: changed}
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
			if err := strictDecode(scanner.Bytes(), &reply, recordLimit); err != nil || reply.Schema != 1 || reply.Identity != handle {
				_ = command.Process.Kill()
				break
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
		process.exitErr = command.Wait()
		process.mu.Lock()
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
	if initial.NativeID != handle.Conversation.NativeID {
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
		Schema    int                          `json:"schema"`
		ID        string                       `json:"id"`
		Identity  agentbackend.ExecutionHandle `json:"identity"`
		Operation string                       `json:"op"`
		Body      any                          `json:"body"`
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
