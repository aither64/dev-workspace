package processgroup

import (
	"context"
	"errors"
	"fmt"
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
		t.Fatal("invalid descendant identity field count")
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
	want := processIdentity{pid: pid, startTime: startTime, pgid: processGroup}
	exitResult := waitForProcessExit(want, func() processObservation { return observeProcess(pid) }, time.Now, time.Sleep)
	if !exitResult.gone {
		t.Fatalf("descendant did not exit after timeout: %s", exitResult.failureContext(want, command.Process.Pid))
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
		t.Fatal("invalid descendant identity field count")
	}
	pid, err := strconv.Atoi(identity[0])
	if err != nil {
		t.Fatal(err)
	}
	startTime, err := strconv.ParseUint(identity[1], 10, 64)
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
	want := processIdentity{pid: pid, startTime: startTime, pgid: processGroup}
	exitResult := waitForProcessExit(want, func() processObservation { return observeProcess(pid) }, time.Now, time.Sleep)
	if !exitResult.gone {
		t.Fatalf("descendant did not exit after graceful cancellation: %s", exitResult.failureContext(want, command.Process.Pid))
	}
}

// Test-local observations distinguish validated exit from an unreadable identity.
type processIdentity struct {
	pid, pgid int
	startTime uint64
}

type processObservation struct {
	identity processIdentity
	state    string
	category string
	errno    syscall.Errno
}

func parseProcessStat(data []byte, pid int) processObservation {
	bad := processObservation{category: "malformed"}
	text := string(data)
	open, close := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open < 1 || close < open || close+1 >= len(text) || text[close+1] != ' ' {
		return bad
	}
	currentPID, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	if err != nil || currentPID <= 0 {
		return bad
	}
	fields := strings.Fields(text[close+1:]) // Field 3 (state) through field 22 (starttime).
	if len(fields) < 20 || len(fields[0]) != 1 {
		return bad
	}
	pgid, groupErr := strconv.Atoi(fields[2])
	start, startErr := strconv.ParseUint(fields[19], 10, 64)
	if groupErr != nil || startErr != nil || pgid <= 0 || start == 0 {
		return bad
	}
	sample := processObservation{identity: processIdentity{pid: currentPID, pgid: pgid, startTime: start}, category: "observed"}
	if currentPID != pid {
		sample.category = "pid-mismatch"
	} else if !strings.Contains("RSDTtZXIPW", fields[0]) {
		sample.category = "unknown-state"
	} else {
		sample.state = fields[0]
	}
	return sample
}

func observeProcess(pid int) processObservation {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	return processStatRead(data, err, pid)
}

func processStatRead(data []byte, err error, pid int) processObservation {
	if errors.Is(err, os.ErrNotExist) {
		return processObservation{category: "absent"}
	}
	if err != nil {
		var errno syscall.Errno
		errors.As(err, &errno)
		return processObservation{category: "read-error", errno: errno}
	}
	return parseProcessStat(data, pid)
}

type processExitResult struct {
	gone    bool
	last    processObservation
	elapsed time.Duration
}

func waitForProcessExit(want processIdentity, observe func() processObservation, now func() time.Time, pause func(time.Duration)) processExitResult {
	started := now()
	result := processExitResult{}
	for {
		result.last = observe()
		result.elapsed = now().Sub(started)
		sample := result.last
		result.gone = sample.category == "absent" || (sample.category == "observed" && sample.identity.pid == want.pid &&
			(sample.identity.startTime != want.startTime ||
				(sample.identity.pgid == want.pgid && (sample.state == "Z" || sample.state == "X"))))
		if result.gone || result.elapsed >= 2*time.Second {
			return result // Do not reverse an established exit with another probe.
		}
		pause(10 * time.Millisecond)
	}
}

func (result processExitResult) failureContext(want processIdentity, leader int) string {
	// All strings originate from fixed categories/state labels, never raw proc data.
	state := result.last.state
	if state == "" {
		state = "unavailable"
	}
	text := fmt.Sprintf("pid=%d start=%d pgid=%d leader=%d elapsed_ms=%d last_pid=%d last_start=%d last_pgid=%d state=%s category=%s errno=%d",
		want.pid, want.startTime, want.pgid, leader, result.elapsed.Milliseconds(),
		result.last.identity.pid, result.last.identity.startTime, result.last.identity.pgid, state, result.last.category, result.last.errno)
	if len(text) > 1024 {
		text = text[:1024]
	}
	return text
}

func TestProcessStatParserAndReadCategories(t *testing.T) {
	stat := func(comm, state, pgid, start string) []byte {
		fields := append([]string{state, "1", pgid}, make([]string, 17)...)
		for i := 3; i < len(fields); i++ {
			fields[i] = "0"
		}
		fields[19] = start
		return []byte("42 (" + comm + ") " + strings.Join(fields, " "))
	}
	for _, state := range []string{"R", "S", "D", "T", "t", "I", "P", "W", "Z", "X"} {
		sample := parseProcessStat(stat("comm with ) spaces ( SECRET_CANARY", state, "7", "123"), 42)
		if sample.category != "observed" || sample.state != state || sample.identity != (processIdentity{pid: 42, pgid: 7, startTime: 123}) {
			t.Fatalf("state %s was not parsed with the expected identity", state)
		}
	}
	for _, data := range [][]byte{nil, []byte("42 broken"), []byte("42 (short) S 1 7"), stat("comm", "SS", "7", "123"), stat("comm", "S", "bad", "123"), stat("comm", "S", "7", "bad"), stat("comm", "S", "0", "123"), stat("comm", "S", "7", "0")} {
		if sample := parseProcessStat(data, 42); sample.category != "malformed" {
			t.Fatal("malformed stat was accepted")
		}
	}
	if sample := parseProcessStat(stat("comm", "S", "7", "123"), 43); sample.category != "pid-mismatch" {
		t.Fatal("wrong PID was accepted")
	}
	if sample := parseProcessStat(stat("SECRET_CANARY", "?", "7", "123"), 42); sample.category != "unknown-state" || sample.state != "" {
		t.Fatal("unknown state was accepted or copied into diagnostics")
	}
	for _, test := range []struct {
		err      error
		category string
		errno    syscall.Errno
	}{
		{os.ErrNotExist, "absent", 0},
		{&os.PathError{Op: "read", Path: "SECRET_CANARY", Err: syscall.EACCES}, "read-error", syscall.EACCES},
		{errors.New("SECRET_CANARY"), "read-error", 0},
	} {
		sample := processStatRead(nil, test.err, 42)
		if sample.category != test.category || sample.errno != test.errno {
			t.Fatal("read error category or errno changed")
		}
	}
}

func TestWaitForProcessExitIsBoundedAndDoesNotReprobeTerminal(t *testing.T) {
	want := processIdentity{pid: 42, pgid: 7, startTime: 123}
	sample := func(state string) processObservation {
		return processObservation{identity: want, state: state, category: "observed"}
	}
	reused := sample("S")
	reused.identity.startTime++
	wrongPID, wrongGroup := sample("Z"), sample("X")
	wrongPID.identity.pid++
	wrongGroup.identity.pgid++
	tests := []struct {
		name string
		last processObservation
		gone bool
	}{
		{"absent", processObservation{category: "absent"}, true}, {"reused", reused, true},
		{"zombie", sample("Z"), true}, {"dead", sample("X"), true},
		{"wrong-pid", wrongPID, false}, {"wrong-group", wrongGroup, false},
		{"malformed", processObservation{category: "malformed"}, false},
		{"permission", processObservation{category: "read-error", errno: syscall.EACCES}, false},
		{"read-error", processObservation{category: "read-error"}, false},
		{"unknown", processObservation{identity: want, category: "unknown-state"}, false},
	}
	for _, state := range []string{"S", "R", "D", "T", "t"} {
		tests = append(tests, struct {
			name string
			last processObservation
			gone bool
		}{state, sample(state), false})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock, calls := time.Unix(0, 0), 0
			result := waitForProcessExit(want, func() processObservation { calls++; return test.last }, func() time.Time { return clock }, func(delay time.Duration) {
				if delay != 10*time.Millisecond {
					t.Fatal("poll interval changed")
				}
				clock = clock.Add(delay)
			})
			if result.gone != test.gone || (!result.gone && result.elapsed != 2*time.Second) || (result.gone && calls != 1) {
				t.Fatalf("unexpected wait result: %s", result.failureContext(want, 7))
			}
			context := result.failureContext(want, 7)
			if len(context) > 1024 || strings.Contains(context, "SECRET_CANARY") {
				t.Fatal("unsafe or excessive failure context")
			}
		})
	}
	// Reaping may progress Z -> X. The first terminal observation is conclusive;
	// even a hypothetical later observation cannot reverse that recorded result.
	for _, next := range []string{"X", "S"} {
		calls := 0
		clock := time.Unix(0, 0)
		result := waitForProcessExit(want, func() processObservation {
			calls++
			if calls == 1 {
				return sample("Z")
			}
			return sample(next)
		}, func() time.Time { return clock }, func(time.Duration) { t.Fatal("terminal result was polled again") })
		if !result.gone || calls != 1 {
			t.Fatal("terminal observation was reversed")
		}
	}
}

func TestProcessExitObserverRejectsAnOwnedLiveChild(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() }) // Only this exact spawned child handle.
	sample := observeProcess(command.Process.Pid)
	if sample.category != "observed" || sample.state == "Z" || sample.state == "X" {
		t.Fatal("owned live-child control identity unavailable")
	}
	result := waitForProcessExit(sample.identity, func() processObservation { return observeProcess(command.Process.Pid) }, time.Now, time.Sleep)
	if result.gone || result.last.category != "observed" || result.elapsed < 2*time.Second {
		t.Fatalf("owned live child was not rejected at the bound: %s", result.failureContext(sample.identity, command.Process.Pid))
	}
}
