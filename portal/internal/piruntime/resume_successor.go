package piruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ResumeSuccessorStart belongs to the explicit upgrade owner under selected-
// package admission and session EX. It never allocates or rewrites an intent.
func (m *Manager) ResumeSuccessorStart(ctx context.Context, request UpgradeRequest) (RuntimeStatus, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeStatus{}, err
	}
	start := request.Start
	if _, err := m.readSavedRuntimeRecord(ctx, start.Session); err != nil {
		return RuntimeStatus{}, err
	}
	directory, err := stateDirectory(m.config.UserStateRoot, start.Session)
	if err != nil {
		return RuntimeStatus{}, err
	}
	lockPath := filepath.Join(directory, "runtime.lock")
	canonical, err := filepath.EvalSymlinks(lockPath)
	if err != nil || canonical != lockPath {
		return RuntimeStatus{}, ErrHeld
	}
	info, err := os.Lstat(lockPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 {
		return RuntimeStatus{}, ErrHeld
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return RuntimeStatus{}, ErrHeld
	}
	unlock, err := runtimeLock(directory)
	if err != nil {
		return RuntimeStatus{}, err
	}
	defer unlock()
	records, err := m.records(start.Session)
	if err != nil {
		return RuntimeStatus{}, err
	}
	current, exists := records.Requests[start.RequestID]
	previous, previousExists := records.Requests[start.ReplacesRequestID]
	if records.Schema != 2 || !exists || !previousExists || records.CurrentRequestID != start.RequestID || current.Start != start || current.Phase != "starting" || start.ReplacesRequestID == "" || start.ReplacesRequestID == start.RequestID || previous.Phase != "starting" || previous.Runtime.Generation != request.ExpectedGeneration || previous.Runtime.InstanceID != current.Runtime.InstanceID || previous.Runtime.Session != current.Runtime.Session {
		return RuntimeStatus{}, ErrScope
	}
	_, members, err := m.upgradeMembers(ctx, request, records)
	if err != nil {
		return RuntimeStatus{}, err
	}
	for _, member := range members {
		if member.record.Loaded {
			return RuntimeStatus{}, ErrHeld
		}
	}
	if err := m.assertRuntimeSettled(ctx, records); err != nil {
		return RuntimeStatus{}, err
	}
	if err := m.config.validatePackage(start.AppStorePath); err != nil {
		return RuntimeStatus{}, err
	}
	launch, config, err := m.frozenRequestBytes(current)
	if err != nil {
		return RuntimeStatus{}, err
	}
	instance := m.instance(current.Runtime)
	for name, expected := range map[string][]byte{
		filepath.Join(instance, "launch.json"):                                      launch,
		filepath.Join(instance, "requests", start.RequestID, "launch.json"):         launch,
		filepath.Join(instance, "requests", start.RequestID, "runtime-config.json"): config,
	} {
		actual, err := readPrivateBytes(name, 65536)
		if err != nil || !bytes.Equal(actual, expected) {
			return RuntimeStatus{}, ErrConflict
		}
	}
	target, err := os.Readlink(filepath.Join(instance, "requests", start.RequestID, "package-root"))
	if err != nil || target != start.AppStorePath {
		return RuntimeStatus{}, ErrConflict
	}
	oldLaunch, _, err := m.frozenRequestBytes(previous)
	if err != nil {
		return RuntimeStatus{}, err
	}
	if err := m.checkPredecessorFiles(previous, oldLaunch); err != nil {
		return RuntimeStatus{}, err
	}
	oldRetained, err := readPrivateBytes(filepath.Join(instance, "requests", previous.Runtime.RequestID, "launch.json"), 65536)
	if err != nil || !bytes.Equal(oldRetained, oldLaunch) {
		return RuntimeStatus{}, ErrConflict
	}
	observed, err := m.config.Host.Inspect(ctx, current.Runtime)
	if errors.Is(err, ErrHostIntentAbsent) && ctx.Err() == nil {
		stopped, inspectErr := m.config.Host.Inspect(ctx, previous.Runtime)
		if inspectErr != nil || !validTerminalReceipt(previous, stopped) {
			return RuntimeStatus{}, ErrHeld
		}
		if err := m.config.verifyDrain(stopped); err != nil {
			return RuntimeStatus{}, err
		}
		if err := m.assertRuntimeSettled(ctx, records); err != nil {
			return RuntimeStatus{}, err
		}
		if err := ctx.Err(); err != nil {
			return RuntimeStatus{}, err
		}
		// Host owns launch.lock and its atomic intent-before-systemctl decision.
		observed, err = m.config.Host.Start(ctx, current.Runtime)
	}
	if err != nil {
		return RuntimeStatus{Runtime: current.Runtime, Phase: "unknown"}, err
	}
	return m.inspectObservation(ctx, current, observed)
}
