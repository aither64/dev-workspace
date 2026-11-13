package piruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// CurrentRuntimeReader returns retained identity only. The returned ref does not
// establish native liveness, attachment readiness or current effect authority.
type CurrentRuntimeReader interface {
	ReadCurrentRuntime(context.Context, agentbackend.SessionRef) (RuntimeRef, error)
}

func (m *Manager) ReadCurrentRuntime(ctx context.Context, ref agentbackend.SessionRef) (RuntimeRef, error) {
	record, err := m.readSavedRuntimeRecord(ctx, ref)
	if err != nil {
		return RuntimeRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return RuntimeRef{}, err
	}
	return record.Runtime, nil
}

// Both read ports use the owning current-record decoder and ReadStart's original
// absence classifier. Neither creates a lock or repairs an ambiguous directory.
func (m *Manager) readSavedRuntimeRecord(ctx context.Context, ref agentbackend.SessionRef) (runtimeRecord, error) {
	if err := ctx.Err(); err != nil {
		return runtimeRecord{}, err
	}
	record, err := m.record(ref)
	if errors.Is(err, os.ErrNotExist) {
		directory, scopeErr := stateDirectory(m.config.UserStateRoot, ref)
		if scopeErr != nil {
			return runtimeRecord{}, scopeErr
		}
		info, statErr := os.Lstat(directory)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return runtimeRecord{}, ErrHeld
		}
		if statErr == nil {
			if !info.IsDir() {
				return runtimeRecord{}, ErrHeld
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil {
				return runtimeRecord{}, ErrHeld
			}
			for _, entry := range entries {
				// An empty ordinary lock may precede first publication. Every
				// other artifact means missing runtime.json is ambiguous.
				if entry.Name() != "runtime.lock" {
					return runtimeRecord{}, ErrHeld
				}
				lock, lockErr := os.Lstat(filepath.Join(directory, entry.Name()))
				if lockErr != nil || !lock.Mode().IsRegular() || lock.Mode().Perm() != 0600 || lock.Size() != 0 {
					return runtimeRecord{}, ErrHeld
				}
				owner, ok := lock.Sys().(*syscall.Stat_t)
				if !ok || owner.Uid != uint32(os.Geteuid()) {
					return runtimeRecord{}, ErrHeld
				}
			}
		}
	}
	if err != nil {
		return runtimeRecord{}, fmt.Errorf("read saved Pi start: %w", err)
	}
	return record, nil
}

var _ CurrentRuntimeReader = (*Manager)(nil)
