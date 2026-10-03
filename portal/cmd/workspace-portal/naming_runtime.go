package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// namingRuntime owns one pinned native child for this portal lifetime. Its
// endpoint is never the ordinary daemon and is never adopted after restart.
// The application must close its workers before closing this owner.
type namingRuntime struct {
	socket, catalog, directory string
	cancel                     context.CancelFunc
	done                       chan struct{}
	once                       sync.Once
}

func startNamingRuntime(packageRoot, portalSocket string, logger *log.Logger) *namingRuntime {
	unavailable := func() *namingRuntime { logger.Print("session naming runtime unavailable"); return nil }
	if packageRoot == "" || !filepath.IsAbs(portalSocket) {
		return unavailable()
	}
	native, err := filepath.EvalSymlinks(filepath.Join(packageRoot, "libexec/codex/libexec/codex/bin/codex"))
	if err != nil {
		return unavailable()
	}
	catalog, err := filepath.EvalSymlinks(filepath.Join(packageRoot, "share/workspace-portal/codex-models.json"))
	if err != nil {
		return unavailable()
	}
	home := os.Getenv("DEV_WORKSPACE_CODEX_HOME")
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return unavailable()
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return unavailable()
	}
	directory, err := os.MkdirTemp(filepath.Dir(portalSocket), "naming-")
	if err != nil {
		return unavailable()
	}
	cwd := filepath.Join(directory, "cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		os.RemoveAll(directory)
		return unavailable()
	}
	ctx, cancel := context.WithCancel(context.Background())
	owner := &namingRuntime{directory: directory, socket: filepath.Join(directory, "app.sock"), catalog: catalog, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(owner.done)
		defer os.RemoveAll(directory)
		probeCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		probe := exec.Command(native, "--version")
		probe.Dir = cwd
		probe.Env = namingRuntimeEnvironment(os.Environ(), home)
		version := &namingVersionOutput{}
		probe.Stdout = version
		probe.Stderr = io.Discard
		probeErr := runNamingProcess(probeCtx, probe, 0)
		stop()
		if probeErr != nil || version.text != "codex-cli 0.160.0\n" || version.overflow {
			logger.Print("session naming version prerequisite unavailable")
			return
		}
		if ctx.Err() != nil {
			return
		}
		command := exec.Command(native, "-c", "model_catalog_json="+strconv.Quote(catalog), "app-server", "--strict-config", "--listen", "unix://"+owner.socket)
		command.Dir = cwd
		command.Env = namingRuntimeEnvironment(os.Environ(), home)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := runNamingChild(ctx, command); err != nil && ctx.Err() == nil {
			logger.Print("session naming runtime exited")
		}
	}()
	return owner
}

func namingRuntimeEnvironment(environment []string, home string) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		if strings.HasPrefix(entry, "CODEX_HOME=") || strings.HasPrefix(entry, "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "CODEX_HOME="+home, "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=1")
}

// Version output is public, but a broken executable still cannot grow memory
// or relay arbitrary diagnostics through the portal.
type namingVersionOutput struct {
	text     string
	overflow bool
}

func (output *namingVersionOutput) Write(data []byte) (int, error) {
	count := len(data)
	if len(output.text)+count > 128 {
		output.overflow = true
		return count, nil
	}
	output.text += string(data)
	return count, nil
}

func (owner *namingRuntime) Close() {
	if owner == nil {
		return
	}
	owner.once.Do(owner.cancel)
	<-owner.done
}

// One waiter owns reaping. Even a successful leader exit kills the residual
// owned group; subscriber removal or leader exit cannot leave native workers.
func runNamingChild(ctx context.Context, command *exec.Cmd) error {
	err := runNamingProcess(ctx, command, 2*time.Second)
	if err == nil {
		return errors.New("naming child exited")
	}
	return err
}

func runNamingProcess(ctx context.Context, command *exec.Cmd, grace time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
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
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	case <-timer.C:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return ctx.Err()
}
