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

// RunGraceful gives a process group time to handle SIGTERM before falling back
// to SIGKILL. It is intended for commands such as Git that hold filesystem
// locks and can remove them during ordinary signal handling.
func RunGraceful(ctx context.Context, command *exec.Cmd, grace time.Duration) error {
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
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
	if grace <= 0 {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		// Wait can return after its pipe delay even when a descendant that
		// ignored SIGTERM still owns the original process group.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return ctx.Err()
	case <-timer.C:
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

// CombinedOutputGraceful captures combined output while using RunGraceful.
func CombinedOutputGraceful(ctx context.Context, command *exec.Cmd, grace time.Duration) ([]byte, error) {
	if command.Stdout != nil || command.Stderr != nil {
		return nil, errors.New("processgroup: stdout or stderr already configured")
	}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := RunGraceful(ctx, command, grace)
	return output.Bytes(), err
}
