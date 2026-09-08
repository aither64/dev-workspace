package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// PendingLifecycle returns the high-level operation that owns a session slug.
// Unsafe or conflicting journal state is reported as an error so browser
// mutations fail closed until the matching CLI operation repairs it.
func PendingLifecycle(workspace, slug string) (string, error) {
	if !ValidSlug(slug) {
		return "", errors.New("invalid session slug")
	}
	root := filepath.Join(workspace, "worktrees", ".locks")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect lifecycle journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("lifecycle journal directory is unsafe")
	}

	operation := ""
	for _, journal := range LifecycleJournals() {
		path := filepath.Join(root, slug+"."+journal.Name+".json")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect lifecycle journal: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ok ||
			stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600 ||
			info.Size() > 64*1024 {
			return "", fmt.Errorf("lifecycle journal is unsafe: %s", path)
		}
		if operation != "" && operation != journal.Command {
			return "", fmt.Errorf("conflicting lifecycle journals reserve session %s", slug)
		}
		operation = journal.Command
	}
	return operation, nil
}
