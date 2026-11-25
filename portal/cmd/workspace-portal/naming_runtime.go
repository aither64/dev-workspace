package main

import (
	"context"
	"encoding/json"
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

// Startup export has its own allowance; naming requests keep their independent
// deadline and closing the owner cancels this prerequisite immediately.
const namingProviderStartupTimeout = 30 * time.Second

// namingRuntime owns one pinned native child for this portal lifetime. Its
// endpoint is never the ordinary daemon and is never adopted after restart.
// The application must close its workers before closing this owner.
type namingRuntime struct {
	socket, catalog, directory string
	cancel                     context.CancelFunc
	done                       chan struct{}
	once                       sync.Once
}

func startNamingRuntime(packageRoot, portalSocket, workspaceName, codexVersion string, logger *log.Logger) *namingRuntime {
	unavailable := func() *namingRuntime { logger.Print("session naming runtime unavailable"); return nil }
	// The child shares Codex home with the ordinary daemon. Even a version
	// probe of the newer binary is forbidden during the same-native bootstrap.
	if codexVersion != "0.172.0" {
		return unavailable()
	}
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
		started := time.Now()
		stageStarted := started
		logger.Print("session_naming_startup stage=version event=start elapsed_ms=0")
		probeCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		probe := exec.Command(native, "--version")
		probe.Dir = cwd
		probe.Env = namingRuntimeEnvironment(os.Environ(), home)
		version := &namingCommandOutput{limit: 128}
		probe.Stdout = version
		probe.Stderr = io.Discard
		probeErr := runNamingProcess(probeCtx, probe, 0)
		outcome, exit := namingProcessOutcome(ctx, probeCtx, probe, probeErr)
		if probeErr == nil && (version.text != "codex-cli 0.172.0\n" || version.overflow) {
			outcome = "invalid_output"
		}
		logger.Printf("session_naming_startup stage=version event=result outcome=%s exit=%d elapsed_ms=%d duration_ms=%d", outcome, exit, time.Since(started).Milliseconds(), time.Since(stageStarted).Milliseconds())
		stop()
		if probeErr != nil || version.text != "codex-cli 0.172.0\n" || version.overflow {
			logger.Print("session naming version prerequisite unavailable")
			return
		}
		if ctx.Err() != nil {
			return
		}
		arguments, err := namingProviderArguments(ctx, packageRoot, workspaceName, logger, started)
		if err != nil {
			logger.Print("session naming provider configuration unavailable")
			return
		}
		arguments = append(arguments, "-c", "sqlite_home="+strconv.Quote(home), "-c", "log_dir="+strconv.Quote(filepath.Join(home, "log")), "-c", "model_catalog_json="+strconv.Quote(catalog), "app-server", "--strict-config", "--listen", "unix://"+owner.socket)
		command := exec.Command(native, arguments...)
		command.Dir = cwd
		command.Env = namingRuntimeEnvironment(os.Environ(), home)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		stageStarted = time.Now()
		logger.Printf("session_naming_startup stage=native event=start elapsed_ms=%d", time.Since(started).Milliseconds())
		err = runNamingChild(ctx, command)
		outcome, exit = namingProcessOutcome(ctx, ctx, command, err)
		logger.Printf("session_naming_startup stage=native event=result outcome=%s exit=%d elapsed_ms=%d duration_ms=%d", outcome, exit, time.Since(started).Milliseconds(), time.Since(stageStarted).Milliseconds())
		if err != nil && ctx.Err() == nil {
			logger.Print("session naming runtime exited")
		}
	}()
	return owner
}

// The host owns provider definitions and token-file authentication arguments.
// Import that read-only projection rather than reconstructing it in Go. A
// generic launcher without workspace identity keeps native configuration.
func namingProviderArguments(parent context.Context, packageRoot, workspaceName string, logger *log.Logger, started time.Time) ([]string, error) {
	stageStarted := time.Now()
	logger.Printf("session_naming_startup stage=provider event=start elapsed_ms=%d", time.Since(started).Milliseconds())
	outcome, exit := "success", -1
	defer func() {
		logger.Printf("session_naming_startup stage=provider event=result outcome=%s exit=%d elapsed_ms=%d duration_ms=%d", outcome, exit, time.Since(started).Milliseconds(), time.Since(stageStarted).Milliseconds())
	}()
	if workspaceName == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(parent, namingProviderStartupTimeout)
	defer cancel()
	command := exec.Command(filepath.Join(packageRoot, "bin/workspace-host"), "provider", "native-args", workspaceName)
	command.Env = os.Environ()
	// Avoid optional Ruby diagnostic startup for this internal helper while
	// preserving caller options, RubyGems and the host's normal dispatch.
	if os.Getenv("RUBYOPT") == "" {
		command.Env = append(command.Env, "RUBYOPT=--disable-error_highlight --disable-did_you_mean --disable-syntax_suggest")
	}
	output := &namingCommandOutput{limit: 256 * 1024}
	command.Stdout, command.Stderr = output, io.Discard
	err := runNamingProcess(ctx, command, 0)
	outcome, exit = namingProcessOutcome(parent, ctx, command, err)
	if err != nil {
		return nil, errors.New("naming provider arguments unavailable")
	}
	if output.overflow {
		outcome = "oversized_output"
		return nil, errors.New("naming provider arguments unavailable")
	}
	var result struct {
		Arguments []string `json:"argv"`
	}
	if json.Unmarshal([]byte(output.text), &result) != nil || result.Arguments == nil {
		outcome = "invalid_output"
		return nil, errors.New("invalid naming provider arguments")
	}
	return result.Arguments, nil
}

// Categories contain no command, arguments, output or environment values.
func namingProcessOutcome(parent, deadline context.Context, command *exec.Cmd, err error) (string, int) {
	exit := -1
	if command.ProcessState != nil {
		exit = command.ProcessState.ExitCode()
	}
	if parent.Err() != nil {
		return "owner_canceled", exit
	}
	if errors.Is(deadline.Err(), context.DeadlineExceeded) {
		return "deadline", exit
	}
	if err == nil {
		return "success", exit
	}
	if command.ProcessState == nil {
		return "start_error", exit
	}
	if exit != 0 {
		return "nonzero_exit", exit
	}
	return "exit", exit
}

func namingRuntimeEnvironment(environment []string, home string) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		if strings.HasPrefix(entry, "CODEX_HOME=") || strings.HasPrefix(entry, "CODEX_SQLITE_HOME=") || strings.HasPrefix(entry, "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "CODEX_HOME="+home, "CODEX_SQLITE_HOME="+home, "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=1")
}

// Version output is public, but a broken executable still cannot grow memory
// or relay arbitrary diagnostics through the portal.
type namingCommandOutput struct {
	limit    int
	text     string
	overflow bool
}

func (output *namingCommandOutput) Write(data []byte) (int, error) {
	count := len(data)
	if len(output.text)+count > output.limit {
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
