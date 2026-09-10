package session

import (
	"crypto/sha256"
	"encoding/hex"
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

const removalRecoveryMaxBytes = 64 * 1024

// CompletedRemoval reports whether dev-session durably completed a deletion
// after notBefore. Merely failing to discover a portal manifest is not proof
// that the session's tracking, runtime, clusters, and worktrees were removed.
func CompletedRemoval(
	workspace, slug, configuredStateHome, operationID string, notBefore time.Time,
) (bool, error) {
	if !ValidSlug(slug) {
		return false, errors.New("invalid session slug")
	}
	if !validLifecycleJournalID(operationID) {
		return false, errors.New("invalid removal operation identity")
	}
	stateHome := configuredStateHome
	if stateHome == "" {
		stateHome = os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return false, fmt.Errorf("find removal recovery state home: %w", err)
			}
			stateHome = filepath.Join(home, ".local", "state")
		}
	}
	absoluteStateHome, err := filepath.Abs(stateHome)
	if err != nil {
		return false, fmt.Errorf("resolve removal recovery state home: %w", err)
	}
	digest := sha256.Sum256([]byte(workspace))
	workspaceID := filepath.Base(workspace) + "-" + hex.EncodeToString(digest[:8])
	root := filepath.Join(absoluteStateHome, "dev-workspaces", "removed", workspaceID)
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect removal recovery root: %w", err)
	}
	if err := validatePrivateRemovalDirectory(rootInfo); err != nil {
		return false, fmt.Errorf("removal recovery root is unsafe: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || canonicalRoot != filepath.Clean(root) {
		return false, errors.New("removal recovery root has unsafe path components")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, fmt.Errorf("read removal recovery root: %w", err)
	}
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), "-"+slug+"-") {
			continue
		}
		completed, err := completedRemovalCandidate(
			filepath.Join(root, entry.Name()), workspace, slug, operationID, notBefore,
		)
		if err != nil {
			return false, err
		}
		if completed {
			return true, nil
		}
	}
	return false, nil
}

func completedRemovalCandidate(
	directory, workspace, slug, operationID string, notBefore time.Time,
) (bool, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return false, fmt.Errorf("inspect removal recovery candidate: %w", err)
	}
	if err := validatePrivateRemovalDirectory(info); err != nil {
		return false, fmt.Errorf("removal recovery candidate is unsafe: %w", err)
	}
	path := filepath.Join(directory, "recovery.json")
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, fmt.Errorf("open removal recovery marker: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect removal recovery marker: %w", err)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !opened.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) ||
		opened.Mode().Perm() != 0o600 || opened.Size() > removalRecoveryMaxBytes {
		return false, errors.New("removal recovery marker is unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(file, removalRecoveryMaxBytes+1))
	if err != nil {
		return false, fmt.Errorf("read removal recovery marker: %w", err)
	}
	if len(data) > removalRecoveryMaxBytes {
		return false, errors.New("removal recovery marker exceeds 64 KiB")
	}
	var marker struct {
		Schema      int    `json:"schema"`
		Slug        string `json:"slug"`
		Workspace   string `json:"workspace"`
		State       string `json:"state"`
		Phase       string `json:"phase"`
		Tracking    string `json:"tracking"`
		Recovery    string `json:"recovery"`
		OperationID string `json:"operation_id"`
		RemovedAt   string `json:"removed_at"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return false, fmt.Errorf("decode removal recovery marker: %w", err)
	}
	if marker.Schema != 1 || marker.Slug != slug || marker.Workspace != workspace ||
		marker.State != "removed" || marker.Phase != "removed" ||
		(marker.Tracking != "work" && marker.Tracking != "archive") ||
		marker.Recovery != directory || marker.OperationID != operationID {
		return false, nil
	}
	removedAt, err := time.Parse(time.RFC3339Nano, marker.RemovedAt)
	if err != nil {
		return false, errors.New("removal recovery marker has an invalid completion time")
	}
	if removedAt.Before(notBefore) {
		return false, nil
	}
	trackingInfo, err := os.Lstat(filepath.Join(directory, marker.Tracking))
	if err != nil {
		return false, fmt.Errorf("inspect preserved removal tracking: %w", err)
	}
	if !trackingInfo.IsDir() || trackingInfo.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("preserved removal tracking is unsafe")
	}
	return true, nil
}

func validatePrivateRemovalDirectory(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok ||
		stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o700 {
		return errors.New("expected an owner-only directory")
	}
	return nil
}
