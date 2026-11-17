package piruntime

import (
	"context"
	"encoding/json"
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

type RetainedRuntimeState struct {
	Runtime RuntimeRef
	Phase   string
}

// InspectRetainedCurrentRuntime is compatibility observation, not execution
// admission. Retained configuration is used only by the existing record codec;
// no host/auth path is opened or granted authority by this metadata reader.
func InspectRetainedCurrentRuntime(ctx context.Context, userStateRoot string, ref agentbackend.SessionRef) (RetainedRuntimeState, error) {
	if err := ctx.Err(); err != nil {
		return RetainedRuntimeState{}, err
	}
	if !filepath.IsAbs(userStateRoot) || filepath.Clean(userStateRoot) != userStateRoot {
		return RetainedRuntimeState{}, ErrScope
	}
	directory, err := stateDirectory(userStateRoot, ref)
	if err != nil {
		return RetainedRuntimeState{}, err
	}
	var raw json.RawMessage
	if err := readPrivate(filepath.Join(directory, "runtime.json"), &raw); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Preserve the owning empty/ordinary-lock versus retained-artifact
			// classifier even before configuration metadata can be decoded.
			_, classified := (&Manager{config: Config{UserStateRoot: userStateRoot}}).readSavedRuntimeRecord(ctx, ref)
			return RetainedRuntimeState{}, classified
		}
		return RetainedRuntimeState{}, err
	}
	var header struct{ Schema int }
	if err := json.Unmarshal(raw, &header); err != nil {
		return RetainedRuntimeState{}, err
	}
	var retained runtimeRecord
	switch header.Schema {
	case 1:
		if err := strictDecode(raw, &retained, recordLimit); err != nil {
			return RetainedRuntimeState{}, err
		}
	case 2:
		var records runtimeRecords
		if err := strictDecode(raw, &records, recordLimit); err != nil {
			return RetainedRuntimeState{}, err
		}
		retained = records.Requests[records.CurrentRequestID]
	default:
		return RetainedRuntimeState{}, ErrScope
	}
	if !filepath.IsAbs(retained.HostStateRoot) || filepath.Clean(retained.HostStateRoot) != retained.HostStateRoot || !filepath.IsAbs(retained.AuthDirectory) || filepath.Clean(retained.AuthDirectory) != retained.AuthDirectory || validateAccountBindings(retained.AccountBindings) != nil {
		return RetainedRuntimeState{}, ErrScope
	}
	observer := Manager{config: Config{UserStateRoot: userStateRoot, HostStateRoot: retained.HostStateRoot, AuthDirectory: retained.AuthDirectory, AccountBindings: retained.AccountBindings}}
	current, err := observer.readSavedRuntimeRecord(ctx, ref)
	if err != nil {
		return RetainedRuntimeState{}, err
	}
	return RetainedRuntimeState{Runtime: current.Runtime, Phase: current.Phase}, nil
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
