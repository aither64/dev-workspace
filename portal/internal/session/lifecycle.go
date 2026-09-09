package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxLifecycleJournalBytes = 64 * 1024

type LifecycleProgress struct {
	Operation string    `json:"operation"`
	Phase     string    `json:"phase"`
	Mode      string    `json:"mode,omitempty"`
	Force     bool      `json:"force,omitempty"`
	JournalID string    `json:"journalId,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PendingLifecycles discovers lifecycle journals, including deletions whose
// tracked session directory has already moved to private recovery storage.
func PendingLifecycles(workspace string) (map[string]LifecycleProgress, error) {
	root := filepath.Join(workspace, "worktrees", ".locks")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]LifecycleProgress{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect lifecycle journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("lifecycle journal directory is unsafe")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read lifecycle journal directory: %w", err)
	}
	candidates := make(map[string]struct{})
	for _, entry := range entries {
		for _, journal := range LifecycleJournals() {
			suffix := "." + journal.Name + ".json"
			if strings.HasSuffix(entry.Name(), suffix) {
				slug := strings.TrimSuffix(entry.Name(), suffix)
				if ValidSlug(slug) {
					candidates[slug] = struct{}{}
				}
			}
		}
	}
	result := make(map[string]LifecycleProgress, len(candidates))
	for slug := range candidates {
		progress, progressErr := PendingLifecycleProgress(workspace, slug)
		if progressErr != nil {
			return nil, progressErr
		}
		if progress != nil {
			result[slug] = *progress
		}
	}
	return result, nil
}

var lifecyclePhases = map[string]map[string]struct{}{
	"archive": phaseSet(
		"prepared", "quiesced", "clusters_released", "tracking_archived",
		"tracking_committed", "thread_retired", "runtime_retired", "archived",
	),
	"delete": phaseSet(
		"prepared", "validated", "thread_retiring", "thread_retired",
		"clusters_released", "worktrees_removed", "runtime_retired",
		"tracking_preserved", "tracking_committed", "removed",
	),
	"revive": phaseSet(
		"prepared", "tracking_restored", "tracking_committed", "runtime_starting",
		"runtime_started", "revived",
	),
}

func phaseSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

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

// PendingLifecycleProgress reads the journal phase for presentation. The
// public command remains the recovery authority; this function never mutates
// or attempts to infer a completed transition.
func PendingLifecycleProgress(workspace, slug string) (*LifecycleProgress, error) {
	if !ValidSlug(slug) {
		return nil, errors.New("invalid session slug")
	}
	root := filepath.Join(workspace, "worktrees", ".locks")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect lifecycle journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("lifecycle journal directory is unsafe")
	}

	var progress *LifecycleProgress
	for _, journal := range LifecycleJournals() {
		candidate, found, readErr := readLifecycleProgressFile(
			filepath.Join(root, slug+"."+journal.Name+".json"), slug, workspace, journal.Command,
		)
		if readErr != nil {
			return nil, readErr
		}
		if !found {
			continue
		}
		if progress != nil {
			return nil, fmt.Errorf("conflicting lifecycle journals reserve session %s", slug)
		}
		progress = candidate
	}
	return progress, nil
}

func readLifecycleProgressFile(
	path, slug, workspace, operation string,
) (*LifecycleProgress, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open lifecycle journal progress: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect open lifecycle journal progress: %w", err)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !opened.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) ||
		opened.Mode().Perm() != 0o600 || opened.Size() > maxLifecycleJournalBytes {
		return nil, false, errors.New("open lifecycle journal progress is unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLifecycleJournalBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read lifecycle journal progress: %w", err)
	}
	if len(data) > maxLifecycleJournalBytes {
		return nil, false, errors.New("lifecycle journal progress exceeds 64 KiB")
	}
	var payload struct {
		Slug        string `json:"slug"`
		Workspace   string `json:"workspace"`
		Phase       string `json:"phase"`
		Mode        string `json:"mode"`
		Force       *bool  `json:"force"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, false, fmt.Errorf("decode lifecycle journal progress: %w", err)
	}
	if payload.Slug != slug || payload.Workspace != workspace {
		return nil, false, errors.New("lifecycle journal progress has the wrong session identity")
	}
	if _, ok := lifecyclePhases[operation][payload.Phase]; !ok {
		return nil, false, fmt.Errorf("lifecycle journal has an invalid %s phase", operation)
	}
	if operation == "archive" && payload.Mode != "complete" && payload.Mode != "abandoned" {
		return nil, false, errors.New("archive journal progress has an invalid mode")
	}
	if operation == "delete" && payload.Force == nil {
		return nil, false, errors.New("delete journal progress has no force setting")
	}
	if !validLifecycleJournalID(payload.OperationID) {
		return nil, false, errors.New("lifecycle journal has an invalid operation identity")
	}
	force := false
	if payload.Force != nil {
		force = *payload.Force
	}
	return &LifecycleProgress{
		Operation: operation, Phase: payload.Phase, Mode: payload.Mode,
		Force: force, JournalID: payload.OperationID, UpdatedAt: opened.ModTime(),
	}, true, nil
}

func validLifecycleJournalID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
