package processgroup

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const waitDelay = 2 * time.Second

// Run starts command in a new process group and kills the whole group when the
// context ends. WaitDelay bounds pipe cleanup if a descendant escapes the
// group while retaining stdout or stderr.
func Run(ctx context.Context, command *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	if command.WaitDelay == 0 {
		command.WaitDelay = waitDelay
	}
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

// CombinedOutput runs command with the same process-group cancellation as Run.
func CombinedOutput(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	if command.Stdout != nil || command.Stderr != nil {
		return nil, errors.New("processgroup: stdout or stderr already configured")
	}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := Run(ctx, command)
	return output.Bytes(), err
}
