package processgroup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const waitDelay = 2 * time.Second

// Result retains producer evidence that the ordinary Run error intentionally
// hides on cancellation. GroupAbsent is proof about this invocation's original
// owned group, sampled before returning; it is not a reusable PID identity.
type Result struct {
	Started         bool
	WaitReturned    bool
	WaitErr         error
	CancelErr       error
	CleanupErr      error
	GroupAbsent     bool
	PipesComplete   bool
	PipeErr         error
	Output          []byte
	OutputTruncated bool
	StartedAt       time.Time
	FinishedAt      time.Time
}

// RunDetailed captures a bounded combined response and joins the original
// command/group. Its own pipe reader proves EOF independently of exec.Cmd.Wait:
// Wait can hide ErrWaitDelay behind a nonzero process exit. onStart publishes
// the exact live receipt before waiting; a publication error cancels and joins.
// Existing Run callers retain their original behavior.
func RunDetailed(ctx context.Context, command *exec.Cmd, onStart func() error) (Result, error) {
	var result Result
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if command.Stdout != nil || command.Stderr != nil || command.Stdin != nil {
		return result, errors.New("detailed runner requires owned output and no input")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer reader.Close()
	defer writer.Close()
	command.Stdout, command.Stderr = writer, writer
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	if command.WaitDelay == 0 {
		command.WaitDelay = waitDelay
	}
	if err := command.Start(); err != nil {
		return result, err
	}
	result.Started, result.StartedAt = true, time.Now().UTC()
	_ = writer.Close()
	var output boundedOutput
	copied := make(chan error, 1)
	go func() { _, err := io.Copy(&output, reader); copied <- err }()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var outcome error
	var deadline time.Time
	if onStart != nil {
		outcome = onStart()
	}
	if outcome != nil {
		deadline = time.Now().Add(waitDelay)
		result.CancelErr = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		result.WaitErr = <-done
	} else {
		select {
		case result.WaitErr = <-done:
			outcome = result.WaitErr
		case <-ctx.Done():
			deadline = time.Now().Add(waitDelay)
			result.CancelErr = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			result.WaitErr = <-done
			outcome = ctx.Err()
		}
	}
	result.WaitReturned = true
	if deadline.IsZero() {
		deadline = time.Now().Add(waitDelay)
	}
	cleanupAttempted := false
	for {
		err := syscall.Kill(-command.Process.Pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			result.GroupAbsent = true
			break
		}
		if err != nil {
			result.CleanupErr = err
			break
		}
		if !cleanupAttempted {
			cleanupAttempted = true
			result.CleanupErr = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			if result.CleanupErr != nil && !errors.Is(result.CleanupErr, syscall.ESRCH) {
				break
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	timer := time.NewTimer(max(0, time.Until(deadline)))
	defer timer.Stop()
	select {
	case result.PipeErr = <-copied:
		result.PipesComplete = result.PipeErr == nil
	case <-timer.C:
		// An escaped pipe holder remains uncertain even if Wait returned an
		// ExitError and the original group no longer exists. Close only our reader.
		result.PipeErr = exec.ErrWaitDelay
		_ = reader.Close()
		<-copied
	}
	result.Output, result.OutputTruncated = output.data, output.truncated
	result.FinishedAt = time.Now().UTC()
	return result, outcome
}

type boundedOutput struct {
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	const limit = 512 * 1024
	n := len(data)
	remaining := limit - len(b.data)
	if len(data) > remaining {
		b.truncated = true
		data = data[:remaining]
	}
	b.data = append(b.data, data...)
	return n, nil // Keep draining after the bound instead of blocking the child.
}

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
