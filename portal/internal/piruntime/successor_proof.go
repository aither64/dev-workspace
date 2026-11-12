package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// The public H0 terminal receipt is immutable evidence, not a fresh manager
// observation. H0 Start performs that fresh unit/job check after publication.
func validTerminalReceipt(record runtimeRecord, observed HostObservation) bool {
	if observed.LaunchRequest != launchFor(record.Start, record.Runtime) || observed.Digest != record.Runtime.Digest || observed.Outcome != "stopped" || observed.Phase != "terminal" || (observed.Result != "exited" && observed.Result != "stopped") || observed.BootID == "" || observed.InvocationID == "" || !filepath.IsAbs(observed.ControlGroup) || filepath.Clean(observed.ControlGroup) != observed.ControlGroup || observed.ControlGroup == "/" || observed.LoadState != "loaded" || (observed.ActiveState != "inactive" && observed.ActiveState != "failed") || (observed.Job != "" && observed.Job != "0") {
		return false
	}
	proof := observed.DrainProof
	if proof == nil || proof["empty"] != true || proof["control_group"] != observed.ControlGroup || proof["boot_id"] != observed.BootID || proof["load_state"] != "loaded" || (proof["active_state"] != "inactive" && proof["active_state"] != "failed") || (proof["job"] != "" && proof["job"] != "0") {
		return false
	}
	if invocation, ok := proof["invocation_id"]; ok && invocation != observed.InvocationID {
		return false
	}
	return true
}

func verifyCurrentDrain(observed HostObservation) error {
	return verifyCurrentDrainAt(observed, "/proc/sys/kernel/random/boot_id", "/sys/fs/cgroup")
}
func verifyCurrentDrainAt(observed HostObservation, bootPath, cgroupRoot string) error {
	boot, err := os.ReadFile(bootPath)
	if err != nil || strings.TrimSpace(string(boot)) != observed.BootID {
		return ErrHeld
	}
	if !filepath.IsAbs(observed.ControlGroup) || filepath.Clean(observed.ControlGroup) != observed.ControlGroup || observed.ControlGroup == "/" {
		return ErrScope
	}
	if info, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil || !info.Mode().IsRegular() {
		return ErrHeld
	}
	group := filepath.Join(cgroupRoot, observed.ControlGroup)
	data, err := os.ReadFile(filepath.Join(group, "cgroup.events"))
	if errors.Is(err, os.ErrNotExist) {
		// Missing events alone is not positive absence of the owned group.
		if _, groupErr := os.Stat(group); errors.Is(groupErr, os.ErrNotExist) {
			return nil
		}
		return ErrHeld
	}
	if err != nil || len(data) > 65536 {
		return ErrHeld
	}
	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "populated" {
			if seen || len(fields) != 2 || fields[1] != "0" {
				return ErrHeld
			}
			seen = true
		}
	}
	if !seen {
		return ErrHeld
	}
	return nil
}

func settledDirectory(path string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, ErrHeld
	}
	return entries, nil
}

// This reads only existing completion records. Neither an empty cgroup nor a
// dead worker upgrades Loaded, pending input/settings/policy or Unknown effects.
func (m *Manager) assertRuntimeSettled(ctx context.Context, records runtimeRecords) error {
	directory, err := stateDirectory(m.config.UserStateRoot, records.Session)
	if err != nil {
		return err
	}
	check := func(root, kind string, session agentbackend.SessionRef, owner, incarnation string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var mapping conversationRecord
		if err := readPrivate(filepath.Join(root, "conversation.json"), &mapping); err != nil {
			return err
		}
		if kind == "utility" {
			if mapping.Utility == nil || mapping.Utility.Session != session || mapping.Utility.OperationID != owner || mapping.Utility.Incarnation != incarnation {
				return ErrScope
			}
			if err := validateUtilityRecord(mapping, *mapping.Utility, root); err != nil {
				return err
			}
		} else {
			ref := mapping.Conversation
			if ref.Session != session || ref.Member != owner || ref.Incarnation != incarnation {
				return ErrScope
			}
			if err := validateConversationRecord(mapping, ref, root); err != nil {
				return err
			}
		}
		if mapping.Loaded || mapping.HeldReason != "" || mapping.PendingSettings != nil || mapping.PendingToolPolicy != nil {
			return ErrHeld
		}
		engine := receiptEngine{root: filepath.Join(root, "submissions"), ownerKind: kind}
		held, err := engine.held()
		if err != nil || held {
			return ErrHeld
		}
		entries, err := settledDirectory(engine.root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			status, err := engine.loadOwned(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return err
			}
			if kind == "utility" {
				if status.ref.utility == nil || status.ref.utility.Utility != *mapping.Utility {
					return ErrScope
				}
			} else if status.ref.retained == nil || status.ref.retained.Conversation != mapping.Conversation {
				return ErrScope
			}
		}
		return nil
	}
	for _, kind := range []string{"members", "utilities"} {
		owners, err := settledDirectory(filepath.Join(directory, kind))
		if err != nil {
			return err
		}
		for _, owner := range owners {
			if !owner.IsDir() || owner.Type()&os.ModeSymlink != 0 || (kind == "members" && !memberPattern.MatchString(owner.Name())) || (kind == "utilities" && !operationPattern.MatchString(owner.Name())) {
				return ErrConflict
			}
			incarnations, err := settledDirectory(filepath.Join(directory, kind, owner.Name()))
			if err != nil {
				return err
			}
			if len(incarnations) == 0 {
				return ErrHeld
			}
			for _, incarnation := range incarnations {
				if !incarnation.IsDir() || incarnation.Type()&os.ModeSymlink != 0 || !hexPattern.MatchString(incarnation.Name()) {
					return ErrConflict
				}
				ownerKind := "retained"
				if kind == "utilities" {
					ownerKind = "utility"
				}
				if err := check(filepath.Join(directory, kind, owner.Name(), incarnation.Name()), ownerKind, records.Session, owner.Name(), incarnation.Name()); err != nil {
					return err
				}
			}
		}
	}
	resources, err := settledDirectory(filepath.Join(directory, "resources"))
	if err != nil {
		return err
	}
	for _, entry := range resources {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !operationPattern.MatchString(id) {
			return ErrConflict
		}
		var record resourceRecord
		if err := readPrivate(filepath.Join(directory, "resources", entry.Name()), &record); err != nil {
			return err
		}
		handle := record.Status.Handle
		original, ok := records.Requests[handle.Runtime.RequestID]
		if !ok || original.Runtime != handle.Runtime || record.Schema != 1 || handle.ResourceID != id || record.Request.ResourceID != id || record.Request.OperationID != handle.OperationID || record.Request.Digest != ResourceDigest(handle.Runtime, record.Request) {
			return ErrScope
		}
		if record.Status.Phase != "stopped" || record.Status.ExitCode == nil {
			return ErrHeld
		}
	}
	return nil
}
