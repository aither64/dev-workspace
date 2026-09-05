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
