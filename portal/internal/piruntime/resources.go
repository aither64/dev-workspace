package piruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type resourceRecord struct {
	Schema  int
	Request ResourceRequest
	Status  ResourceStatus
}

// ResourceDigest binds the trusted command to its session runtime and original
// resource/operation IDs. It contains no executable path supplied by a model.
func ResourceDigest(ref RuntimeRef, request ResourceRequest) string {
	request.Digest = ""
	return digestValue(struct {
		Version int
		Runtime RuntimeRef
		Request ResourceRequest
	}{1, ref, request})
}
func (s *supervisor) resourcePath(id string) (string, error) {
	if !operationPattern.MatchString(id) {
		return "", ErrScope
	}
	return filepath.Join(s.stateRoot, "resources", id+".json"), nil
}
func (s *supervisor) resource(handle ResourceHandle) (resourceRecord, error) {
	var record resourceRecord
	if handle.Runtime != s.ref || !operationPattern.MatchString(handle.OperationID) {
		return record, ErrScope
	}
	path, err := s.resourcePath(handle.ResourceID)
	if err != nil {
		return record, err
	}
	if err := readPrivate(path, &record); err != nil {
		return record, err
	}
	if record.Schema != 1 || record.Status.Handle != handle || record.Request.Digest != ResourceDigest(s.ref, record.Request) {
		return record, ErrScope
	}
	return record, nil
}
func (s *supervisor) startResource(ctx context.Context, request ResourceRequest) (ResourceStatus, error) {
	if !operationPattern.MatchString(request.OperationID) || request.Digest != ResourceDigest(s.ref, request) || len(request.Argv) == 0 || len(request.Argv) > 256 || !filepath.IsAbs(request.Cwd) || filepath.Clean(request.Cwd) != request.Cwd {
		return ResourceStatus{}, ErrScope
	}
	for _, value := range append(append([]string{}, request.Argv...), request.Environment...) {
		if len(value) > 65536 || strings.ContainsRune(value, 0) {
			return ResourceStatus{}, ErrScope
		}
	}
	path, err := s.resourcePath(request.ResourceID)
	if err != nil {
		return ResourceStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.workflowCheck(ctx); err != nil {
		return ResourceStatus{}, err
	}
	var old resourceRecord
	if err := readPrivate(path, &old); err == nil {
		if old.Schema != 1 || old.Status.Handle.Runtime != s.ref || old.Request.Digest != ResourceDigest(s.ref, old.Request) {
			return old.Status, ErrScope
		}
		if old.Request.Digest != request.Digest || !equalJSON(old.Request, request) {
			return old.Status, ErrConflict
		}
		if old.Status.Phase == "starting" {
			old.Status.Phase = "unknown"
		}
		return old.Status, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ResourceStatus{}, err
	}
	record := resourceRecord{Schema: 1, Request: request, Status: ResourceStatus{Handle: ResourceHandle{Runtime: s.ref, ResourceID: request.ResourceID, OperationID: request.OperationID}, Phase: "starting"}}
	if err := writePrivate(path, record); err != nil {
		return record.Status, err
	}
	command := exec.Command(request.Argv[0], request.Argv[1:]...)
	command.Dir = request.Cwd
	command.Env = request.Environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := s.workflowCheck(ctx); err != nil {
		return record.Status, err
	}
	if err := command.Start(); err != nil {
		record.Status.Phase = "rejected"
		if saveErr := writePrivate(path, record); saveErr != nil {
			return record.Status, saveErr
		}
		return record.Status, nil
	}
	go func() {
		err := command.Wait()
		check := s.unexpectedChildren
		if s.childrenCheck != nil {
			check = s.childrenCheck
		}
		unknown, checkErr := check()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.workflowChanged() // Natural exit invalidates before terminal publication; no ACK wait.
		record.Status.Phase = "stopped"
		code := command.ProcessState.ExitCode()
		record.Status.ExitCode = &code
		if !processGroupEmpty(command.Process.Pid) || unknown || checkErr != nil {
			record.Status.Phase = "unknown"
		}
		_ = err
		_ = writePrivate(path, record)
	}()
	record.Status.Handle.PID = command.Process.Pid
	record.Status.Handle.ProcessGroup = command.Process.Pid
	start, err := processStart(command.Process.Pid)
	if err != nil {
		record.Status.Phase = "unknown"
		_ = writePrivate(path, record)
		return record.Status, ErrHeld
	}
	record.Status.Handle.PID = command.Process.Pid
	record.Status.Handle.ProcessGroup = command.Process.Pid
	record.Status.Handle.PIDStart = start
	record.Status.Phase = "running"
	if err := writePrivate(path, record); err != nil {
		return record.Status, ErrHeld
	}

	return record.Status, nil
}
func (s *supervisor) inspectResource(handle ResourceHandle) (ResourceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.resource(handle)
	if err != nil {
		return record.Status, err
	}
	if record.Status.Phase == "running" {
		start, err := processStart(handle.PID)
		group, groupErr := processCgroup(handle.PID)
		if err != nil || start != handle.PIDStart || groupErr != nil || group != s.handshake.ControlGroup {
			record.Status.Phase = "unknown"
		}
	}
	return record.Status, nil
}
func (s *supervisor) stopResource(ctx context.Context, handle ResourceHandle) (StopResult, error) {
	s.mu.Lock()
	if err := s.workflowCheck(ctx); err != nil {
		s.mu.Unlock()
		return StopResult{}, err
	}
	record, err := s.resource(handle)
	if err != nil {
		s.mu.Unlock()
		return StopResult{Outcome: "unknown"}, err
	}
	if record.Status.Phase == "stopped" && processGroupEmpty(handle.ProcessGroup) {
		s.mu.Unlock()
		return StopResult{Outcome: "stopped", VerifiedExit: true}, nil
	}
	start, err := processStart(handle.PID)
	group, groupErr := processCgroup(handle.PID)
	if err != nil || start != handle.PIDStart || groupErr != nil || group != s.handshake.ControlGroup || handle.ProcessGroup != handle.PID {
		s.mu.Unlock()
		return StopResult{Outcome: "unknown"}, ErrHeld
	}
	if err := s.workflowCheck(ctx); err != nil {
		s.mu.Unlock()
		return StopResult{}, err
	}
	if err := syscall.Kill(-handle.ProcessGroup, syscall.SIGTERM); err != nil {
		s.mu.Unlock()
		return StopResult{Outcome: "unknown"}, err
	}
	s.mu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return StopResult{Outcome: "unknown"}, ctx.Err()
		case <-timer.C:
			return StopResult{Outcome: "unknown", Reason: "resource survivors"}, ErrHeld
		case <-ticker.C:
			if processGroupEmpty(handle.ProcessGroup) {
				s.mu.Lock()
				settled, readErr := s.resource(handle)
				s.mu.Unlock()
				if readErr != nil {
					return StopResult{Outcome: "unknown"}, readErr
				}
				if settled.Status.Phase == "stopped" {
					return StopResult{Outcome: "stopped", VerifiedExit: true}, nil
				}
				if settled.Status.Phase == "unknown" {
					return StopResult{Outcome: "unknown"}, ErrHeld
				}
			}
		}
	}
}
func (s *supervisor) resources() (ResourceSnapshot, error) {
	result := ResourceSnapshot{ObservedAt: time.Now().UTC(), Cgroup: map[string]string{}, Exclusions: []string{"portal", "shared Nix daemon/builders", "remote tools/VMs", "shared storage outside scratch", "deleted-open scratch files"}}
	group := s.handshake.ControlGroup
	if !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
		return result, ErrScope
	}
	for _, name := range []string{"cpu.stat", "memory.current", "memory.events", "pids.current", "io.stat"} {
		path := filepath.Join("/sys/fs/cgroup", group, name)
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 65536 {
			result.Unavailable = append(result.Unavailable, name)
		} else {
			result.Cgroup[name] = string(data)
		}
	}
	for _, root := range []string{"/tmp", "/var/tmp"} {
		count := 0
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			count++
			if count > 100000 {
				return ErrUnavailable
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				result.ScratchLogicalBytes += info.Size()
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				result.ScratchAllocatedBytes += stat.Blocks * 512
			}
			return nil
		})
		if err != nil {
			result.Unavailable = append(result.Unavailable, root+" usage incomplete")
		}
	}
	return result, nil
}

// unexpectedChildren refuses to attribute a daemonized descendant to another
// member. H0's cgroup is the session boundary; a process group alone is weaker.
func (s *supervisor) unexpectedChildren() (bool, error) { return s.unexpectedChildrenFor(0) }
func (s *supervisor) unexpectedChildrenFor(target int) (bool, error) {
	data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", s.handshake.ControlGroup, "cgroup.procs"))
	if err != nil {
		return true, err
	}
	known := map[int]bool{os.Getpid(): true}
	s.mu.Lock()
	for _, c := range s.members {
		c.activeMu.Lock()
		w := c.worker
		c.activeMu.Unlock()
		if w != nil {
			pid, _ := w.PID()
			known[pid] = true
		}
	}
	defer s.mu.Unlock()
	files, err := os.ReadDir(filepath.Join(s.stateRoot, "resources"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	for _, file := range files {
		var record resourceRecord
		if readPrivate(filepath.Join(s.stateRoot, "resources", file.Name()), &record) != nil {
			return true, ErrHeld
		}
		if record.Status.Phase == "running" {
			known[record.Status.Handle.ProcessGroup] = true
		}
	}
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return true, err
		}
		if known[pid] {
			continue
		}
		group, err := syscall.Getpgid(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return true, err
		}
		if (target != 0 && group == target && pid != target) || !known[group] {
			return true, nil
		}
	}
	return false, nil
}
