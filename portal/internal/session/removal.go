package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	workspace, slug, configuredStateRoot, operationID string, notBefore time.Time,
) (bool, error) {
	if !validLifecycleJournalID(operationID) {
		return false, errors.New("invalid removal operation identity")
	}
	removals, err := completedRemovals(workspace, slug, configuredStateRoot, operationID, notBefore, nil)
	if err != nil {
		return false, err
	}
	for _, removal := range removals {
		if removal.OperationID == operationID && !removal.RemovedAt.Before(notBefore) {
			return true, nil
		}
	}
	return false, nil
}

// CompletedRemovalForTarget proves the accepted Codex selection and original
// root from the same terminal removal candidate. Nil targets expects absence.
func CompletedRemovalForTarget(workspace, slug, configuredStateRoot, operationID string, notBefore time.Time, originalRoot string, targets *CodexTargets) (bool, error) {
	if !validLifecycleJournalID(operationID) {
		return false, errors.New("invalid removal operation identity")
	}
	if targets != nil {
		if err := targets.Validate(true); err != nil || originalRoot == "" {
			return false, errors.New("invalid captured removal target")
		}
	}
	removals, err := completedRemovals(workspace, slug, configuredStateRoot, operationID, notBefore, &completedRemovalTarget{originalRoot: originalRoot, targets: targets.Clone()})
	return len(removals) != 0 && err == nil, err
}

// CompletedRemovalHistory identifies completed deletion operations without
// ordering them by wall-clock time. Callers hold the session runtime lock.
func CompletedRemovalHistory(workspace, slug, configuredStateRoot string) (string, error) {
	removals, err := completedRemovals(workspace, slug, configuredStateRoot, "", time.Time{}, nil)
	if err != nil {
		return "", err
	}
	unique := make(map[string]bool)
	for _, removal := range removals {
		unique[removal.OperationID] = true
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(digest[:]), nil
}

// RequireNoPendingRemoval is an absence proof for a fresh lifecycle owner.
// Callers retain runtime/team ownership through their acceptance publication.
// It deliberately returns no target or operation to adopt.
func RequireNoPendingRemoval(workspace, slug, configuredStateRoot string) error {
	candidates, err := removalRecoveryCandidates(workspace, slug, configuredStateRoot)
	if err != nil {
		return err
	}
	for _, directory := range candidates {
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		if err := validatePrivateRemovalDirectory(info); err != nil {
			return err
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			continue
		} // No marker was ever accepted here.
		marker, data, err := readRemovalRecoveryMarker(directory)
		if err != nil {
			return err
		}
		_, targets, err := decodeLifecycleObject(data, removalRecoveryMaxBytes)
		if err != nil {
			return err
		}
		if targets != nil {
			if targets.Validate(true) != nil || marker.Schema != 1 || len(marker.Pi) != 0 {
				return errors.New("invalid removal capture reserves lifecycle acceptance")
			}
		}
		if marker.Schema != 1 && marker.Schema != 2 || marker.Slug != slug || marker.Workspace != workspace || marker.Recovery != directory || !validLifecycleJournalID(marker.OperationID) {
			return errors.New("removal recovery candidate has a different or malformed owner")
		}
		if marker.State != "removed" || marker.Phase != "removed" {
			return errors.New("an accepted removal reserves this session; retry its original deletion")
		}
		completed, err := completedRemovalCandidate(directory, workspace, slug, "", time.Time{}, nil)
		if err != nil {
			return err
		}
		if completed == nil {
			return errors.New("removal history has incomplete terminal proof")
		}
	}
	return nil
}

type completedRemoval struct {
	OperationID string
	RemovedAt   time.Time
}

type completedRemovalTarget struct {
	originalRoot string
	targets      *CodexTargets
}

func removalRecoveryCandidates(workspace, slug, configuredStateRoot string) ([]string, error) {
	if !ValidSlug(slug) {
		return nil, errors.New("invalid session slug")
	}
	stateRoot := configuredStateRoot
	if stateRoot == "" {
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("find removal recovery state home: %w", err)
			}
			stateHome = filepath.Join(home, ".local", "state")
		}
		stateRoot = filepath.Join(stateHome, "dev-workspaces")
	}
	absoluteStateRoot, err := filepath.Abs(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve removal recovery state root: %w", err)
	}
	root := userstate.WorkspaceDirectory(absoluteStateRoot, "removed", workspace)
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect removal recovery root: %w", err)
	}
	if err := validatePrivateRemovalDirectory(rootInfo); err != nil {
		return nil, fmt.Errorf("removal recovery root is unsafe: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || canonicalRoot != filepath.Clean(root) {
		return nil, errors.New("removal recovery root has unsafe path components")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read removal recovery root: %w", err)
	}
	var candidates []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "-"+slug+"-") {
			candidates = append(candidates, filepath.Join(root, entry.Name()))
		}
	}
	return candidates, nil
}

func completedRemovals(workspace, slug, configuredStateRoot, operationID string, notBefore time.Time, target *completedRemovalTarget) ([]completedRemoval, error) {
	candidates, err := removalRecoveryCandidates(workspace, slug, configuredStateRoot)
	if err != nil {
		return nil, err
	}
	var removals []completedRemoval
	for _, directory := range candidates {
		completed, err := completedRemovalCandidate(
			directory, workspace, slug, operationID, notBefore, target,
		)
		if err != nil {
			return nil, err
		}
		if completed != nil {
			removals = append(removals, *completed)
			if operationID != "" && target == nil {
				return removals, nil
			}
		}
	}
	return removals, nil
}

type removalRecoveryMarker struct {
	Schema      int             `json:"schema"`
	Slug        string          `json:"slug"`
	Workspace   string          `json:"workspace"`
	State       string          `json:"state"`
	Phase       string          `json:"phase"`
	Tracking    string          `json:"tracking"`
	Recovery    string          `json:"recovery"`
	OperationID string          `json:"operation_id"`
	RemovedAt   string          `json:"removed_at"`
	Pi          json.RawMessage `json:"pi"`
}

func readRemovalRecoveryMarker(directory string) (removalRecoveryMarker, []byte, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return removalRecoveryMarker{}, nil, fmt.Errorf("inspect removal recovery candidate: %w", err)
	}
	if err := validatePrivateRemovalDirectory(info); err != nil {
		return removalRecoveryMarker{}, nil, fmt.Errorf("removal recovery candidate is unsafe: %w", err)
	}
	path := filepath.Join(directory, "recovery.json")
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return removalRecoveryMarker{}, nil, fmt.Errorf("open removal recovery marker: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return removalRecoveryMarker{}, nil, fmt.Errorf("inspect removal recovery marker: %w", err)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !opened.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) ||
		opened.Mode().Perm() != 0o600 || opened.Size() > removalRecoveryMaxBytes {
		return removalRecoveryMarker{}, nil, errors.New("removal recovery marker is unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(file, removalRecoveryMaxBytes+1))
	if err != nil {
		return removalRecoveryMarker{}, nil, fmt.Errorf("read removal recovery marker: %w", err)
	}
	if len(data) > removalRecoveryMaxBytes {
		return removalRecoveryMarker{}, nil, errors.New("removal recovery marker exceeds 64 KiB")
	}
	var marker removalRecoveryMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return removalRecoveryMarker{}, nil, fmt.Errorf("decode removal recovery marker: %w", err)
	}
	return marker, data, nil
}

func completedRemovalCandidate(
	directory, workspace, slug, operationID string, notBefore time.Time,
	target *completedRemovalTarget,
) (*completedRemoval, error) {
	marker, data, err := readRemovalRecoveryMarker(directory)
	if err != nil {
		return nil, err
	}
	var captured *CodexTargets
	if target != nil && marker.OperationID == operationID {
		_, value, err := decodeLifecycleObject(data, removalRecoveryMaxBytes)
		if err != nil {
			return nil, err
		}
		if value != nil {
			if value.Validate(true) != nil || len(marker.Pi) != 0 || marker.Schema == 2 {
				return nil, errors.New("invalid captured removal proof")
			}
			captured = value
		}
		if !SameCodexTargets(target.targets, captured) {
			return nil, errors.New("completed removal captured targets changed")
		}
		if marker.Schema != 1 && marker.Schema != 2 || marker.Slug != slug || marker.Workspace != workspace || marker.State != "removed" || marker.Phase != "removed" ||
			marker.Recovery != directory || marker.Tracking != "work" && marker.Tracking != "archive" {
			return nil, errors.New("captured removal candidate is incomplete or changed")
		}
	}
	if (marker.Schema != 1 && marker.Schema != 2) || marker.Slug != slug || marker.Workspace != workspace ||
		marker.State != "removed" || marker.Phase != "removed" ||
		(marker.Tracking != "work" && marker.Tracking != "archive") ||
		marker.Recovery != directory || !validLifecycleJournalID(marker.OperationID) ||
		(operationID != "" && marker.OperationID != operationID) {
		return nil, nil
	}
	if marker.Schema == 2 {
		capture, err := DecodePiLifecycleCapture(marker.Pi, workspace, slug)
		if err != nil || capture.Terminal == nil {
			return nil, errors.New("invalid Pi removal proof")
		}
	} else if len(marker.Pi) != 0 {
		return nil, errors.New("mixed Pi removal proof")
	}
	removedAt, err := time.Parse(time.RFC3339Nano, marker.RemovedAt)
	if err != nil {
		return nil, errors.New("removal recovery marker has an invalid completion time")
	}
	if !notBefore.IsZero() && removedAt.Before(notBefore) {
		if target != nil {
			return nil, errors.New("captured removal predates the accepted request")
		}
		return nil, nil
	}
	trackingInfo, err := os.Lstat(filepath.Join(directory, marker.Tracking))
	if err != nil {
		return nil, fmt.Errorf("inspect preserved removal tracking: %w", err)
	}
	if !trackingInfo.IsDir() || trackingInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("preserved removal tracking is unsafe")
	}
	if target != nil {
		file, info, err := OpenRegularFile(directory, filepath.Join(marker.Tracking, ManifestName))
		if err != nil {
			return nil, fmt.Errorf("read captured removal original manifest: %w", err)
		}
		defer file.Close()
		if info.Size() > manifestMaxSize {
			return nil, errors.New("captured removal manifest exceeds 1 MiB")
		}
		data, err := io.ReadAll(io.LimitReader(file, manifestMaxSize+1))
		if err != nil || len(data) > manifestMaxSize {
			return nil, errors.New("captured removal manifest cannot be read")
		}
		var manifest Manifest
		if err := decodeManifest(data, &manifest); err != nil {
			return nil, err
		}
		if err := manifest.Validate(slug); err != nil || manifest.Codex.ThreadID != target.originalRoot {
			return nil, errors.New("completed removal original root changed")
		}
		if captured != nil && (manifest.Schema != 6 || manifest.Codex.BindingSchema != 3 || manifest.Backend != nil) {
			return nil, errors.New("captured removal has no Codex binding origin")
		}
	}
	return &completedRemoval{OperationID: marker.OperationID, RemovedAt: removedAt}, nil
}

func validatePrivateRemovalDirectory(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok ||
		stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o700 {
		return errors.New("expected an owner-only directory")
	}
	return nil
}
