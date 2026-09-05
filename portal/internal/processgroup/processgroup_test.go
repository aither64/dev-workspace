package processgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunKillsDescendantsOnTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	command := exec.Command(
		"sh", "-c",
		`sleep 30 & pid=$!; awk '{print $1, $22, $5}' "/proc/$pid/stat" > "$1"; wait`,
		"sh", pidFile,
	)
	err := Run(ctx, command)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout result = %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	identity := strings.Fields(string(data))
	if len(identity) != 3 {
		t.Fatalf("invalid descendant identity %q", data)
	}
	pid, err := strconv.Atoi(identity[0])
	if err != nil {
		t.Fatal(err)
	}
	processGroup, err := strconv.Atoi(identity[2])
	if err != nil {
		t.Fatal(err)
	}
	if processGroup != command.Process.Pid {
		t.Fatalf("descendant process group = %d, want %d", processGroup, command.Process.Pid)
	}
	startTime, err := strconv.ParseUint(identity[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processStillExists(pid, startTime) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processStillExists(pid, startTime) {
		t.Fatalf("descendant process %d survived the command timeout", pid)
	}
}

func TestRunGracefulLetsTheLeaderCleanUpBeforeKillingDescendants(t *testing.T) {
	directory := t.TempDir()
	started := filepath.Join(directory, "started")
	cleaned := filepath.Join(directory, "cleaned")
	identityFile := filepath.Join(directory, "descendant")
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.Command(
		"sh", "-c",
		`trap 'touch "$CLEANED"; exit 0' TERM
sh -c 'trap "" TERM; sleep 30' &
child=$!
awk '{print $1, $22, $5}' "/proc/$child/stat" > "$IDENTITY.tmp"
mv "$IDENTITY.tmp" "$IDENTITY"
touch "$STARTED"
wait`,
	)
	command.Env = append(
		os.Environ(), "STARTED="+started, "CLEANED="+cleaned, "IDENTITY="+identityFile,
	)
	result := make(chan error, 1)
	go func() { result <- RunGraceful(ctx, command, time.Second) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("graceful cancellation result = %v", err)
	}
	if _, err := os.Stat(cleaned); err != nil {
		t.Fatalf("leader did not handle SIGTERM: %v", err)
	}
	data, err := os.ReadFile(identityFile)
	if err != nil {
		t.Fatal(err)
	}
	identity := strings.Fields(string(data))
	if len(identity) != 3 {
		t.Fatalf("invalid descendant identity %q", data)
	}
	pid, err := strconv.Atoi(identity[0])
	if err != nil {
		t.Fatal(err)
	}
	startTime, err := strconv.ParseUint(identity[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	exitDeadline := time.Now().Add(2 * time.Second)
	for processStillExists(pid, startTime) && time.Now().Before(exitDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processStillExists(pid, startTime) {
		t.Fatalf("TERM-ignoring descendant %d survived graceful cancellation", pid)
	}
}

func processStillExists(pid int, startTime uint64) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err == nil {
		fields := strings.Fields(string(data))
		if len(fields) < 22 || fields[2] == "Z" {
			return false
		}
		currentStart, parseErr := strconv.ParseUint(fields[21], 10, 64)
		return parseErr == nil && currentStart == startTime
	}
	return syscall.Kill(pid, 0) == nil
}
