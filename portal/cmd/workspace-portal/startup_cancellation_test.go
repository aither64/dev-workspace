package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestServeStartupSignalFixture(t *testing.T) {
	encoded := os.Getenv("PORTAL_STARTUP_SIGNAL_ARGS")
	if encoded == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(encoded), &args); err != nil {
		t.Fatal(err)
	}
	if err := serve(args); err != nil {
		t.Fatal(err)
	}
}

func TestServeStopsWhileStartupWaitsForPackageTransition(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, "share", "dev-workspace", "package.json")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"schema":1,"activationEnvironmentAliases":["DEV_WORKSPACES"],"routerSocket":"/run/fixture/router.sock","userNamespace":"fixture","agent_teams":{"managed":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	runtime := t.TempDir()
	profile := filepath.Join(root, "profile")
	if err := os.Symlink(root, profile); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "transition.lock")
	owner, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	identity, err := owner.Stat()
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(runtime, "portal.sock")
	args, _ := json.Marshal([]string{
		"--workspace", workspace, "--unix-socket", socket,
		"--base-url", "https://workspace.example.test", "--dev-session", "/missing/dev-session",
		"--host-profile", profile, "--package-root", root, "--user-state-root", t.TempDir(),
		"--authority-dir", t.TempDir(),
		"--workspace-name", "fixture", "--registration-marker", filepath.Join(runtime, "registration.json"),
		"--codex-socket", filepath.Join(runtime, "app.sock"), "--transition-lock", lockPath,
	})
	command := exec.Command(os.Args[0], "-test.run=^TestServeStartupSignalFixture$")
	command.Env = append(os.Environ(), "PORTAL_STARTUP_SIGNAL_ARGS="+string(args),
		"DEV_WORKSPACES_CODEX_PROVIDERS="+filepath.Join(root, "absent-provider.json"))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finished := false
	defer func() {
		if !finished {
			_ = command.Process.Kill()
			<-done
		}
	}()
	// Observe the real child opening the exact transition inode before sending
	// SIGTERM. The exclusive owner remains held throughout cancellation.
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for !ready && time.Now().Before(deadline) {
		select {
		case err := <-done:
			finished = true
			t.Fatalf("portal exited before startup wait: %v\n%s", err, output.String())
		default:
		}
		entries, _ := os.ReadDir(filepath.Join("/proc", strconv.Itoa(command.Process.Pid), "fd"))
		for _, entry := range entries {
			info, err := os.Stat(filepath.Join("/proc", strconv.Itoa(command.Process.Pid), "fd", entry.Name()))
			if err == nil && os.SameFile(identity, info) {
				ready = true
				break
			}
		}
		if !ready {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatal("portal never opened the transition lock")
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("HTTP listener published during startup wait: %v", err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("portal stop: %v\n%s", err, output.String())
		}
	case <-time.After(2 * time.Second):
		// Retain Go stacks on failure rather than waiting for the service's 150s kill.
		_ = command.Process.Signal(syscall.SIGQUIT)
		<-done
		finished = true
		t.Fatalf("portal ignored SIGTERM while startup waited for transition lock\n%s", output.String())
	}
}
