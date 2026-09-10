package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	lifecycleOperationStoreSchema   = 1
	maxLifecycleOperationStoreBytes = 256 * 1024
	maxLifecycleOperationRecords    = 512
)

type lifecycleOperationStore struct {
	workspace string
	directory string
	path      string
}

type lifecycleOperationStorePayload struct {
	Schema     int                  `json:"schema"`
	Workspace  string               `json:"workspace"`
	Operations []lifecycleOperation `json:"operations"`
}

func newLifecycleOperationStore(workspace, configuredStateRoot string) (*lifecycleOperationStore, error) {
	stateRoot := configuredStateRoot
	if stateRoot == "" {
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("find lifecycle operation state home: %w", err)
			}
			stateHome = filepath.Join(home, ".local", "state")
		}
		absoluteStateHome, err := filepath.Abs(stateHome)
		if err != nil {
			return nil, fmt.Errorf("resolve lifecycle operation state home: %w", err)
		}
		stateRoot = filepath.Join(absoluteStateHome, "dev-workspaces")
	} else {
		absoluteStateRoot, err := filepath.Abs(stateRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve lifecycle operation state root: %w", err)
		}
		stateRoot = absoluteStateRoot
	}
	workspaceDigest := sha256.Sum256([]byte(workspace))
	workspaceID := filepath.Base(workspace) + "-" + hex.EncodeToString(workspaceDigest[:8])
	directory := filepath.Join(stateRoot, "portal", workspaceID)
	return &lifecycleOperationStore{
		workspace: workspace,
		directory: directory,
		path:      filepath.Join(directory, "lifecycle-operations.json"),
	}, nil
}

func (store *lifecycleOperationStore) load() (map[string]lifecycleOperation, error) {
	if err := store.inspectDirectory(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]lifecycleOperation), nil
		}
		return nil, err
	}
	file, err := store.open()
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]lifecycleOperation), nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLifecycleOperationStoreBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read lifecycle operation state: %w", err)
	}
	if len(data) > maxLifecycleOperationStoreBytes {
		return nil, errors.New("lifecycle operation state exceeds 256 KiB")
	}
	var payload lifecycleOperationStorePayload
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode lifecycle operation state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("lifecycle operation state contains trailing data")
	}
	if payload.Schema != lifecycleOperationStoreSchema || payload.Workspace != store.workspace {
		return nil, errors.New("lifecycle operation state has the wrong workspace identity")
	}
	if len(payload.Operations) > maxLifecycleOperationRecords {
		return nil, errors.New("lifecycle operation state has too many records")
	}
	operations := make(map[string]lifecycleOperation, len(payload.Operations))
	for _, operation := range payload.Operations {
		if err := validateLifecycleOperation(operation); err != nil {
			return nil, fmt.Errorf("validate lifecycle operation state: %w", err)
		}
		if _, duplicate := operations[operation.Slug]; duplicate {
			return nil, fmt.Errorf("lifecycle operation state repeats session %s", operation.Slug)
		}
		// A process-local goroutine cannot survive a portal restart. Journal
		// reconciliation will either keep this paused or prove it completed.
		if operation.State == "running" {
			operation.State = "paused"
		}
		operations[operation.Slug] = operation
	}
	return operations, nil
}

func (store *lifecycleOperationStore) save(operations map[string]lifecycleOperation) error {
	if len(operations) > maxLifecycleOperationRecords {
		return errors.New("too many lifecycle operation records")
	}
	if err := store.ensureDirectory(); err != nil {
		return err
	}
	data, err := store.encode(operations)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(store.directory, ".lifecycle-operations-*.tmp")
	if err != nil {
		return fmt.Errorf("create lifecycle operation state: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		_ = os.Remove(temporary.Name())
		return fmt.Errorf("make lifecycle operation state private: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write lifecycle operation state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync lifecycle operation state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close lifecycle operation state: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("replace lifecycle operation state: %w", err)
	}
	removeTemporary = false
	directory, err := os.Open(store.directory)
	if err != nil {
		return fmt.Errorf("open lifecycle operation state directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync lifecycle operation state directory: %w", err)
	}
	return nil
}

func (store *lifecycleOperationStore) encode(operations map[string]lifecycleOperation) ([]byte, error) {
	if len(operations) > maxLifecycleOperationRecords {
		return nil, errors.New("too many lifecycle operation records")
	}
	payload := lifecycleOperationStorePayload{
		Schema: lifecycleOperationStoreSchema, Workspace: store.workspace,
		Operations: make([]lifecycleOperation, 0, len(operations)),
	}
	for _, operation := range operations {
		if err := validateLifecycleOperation(operation); err != nil {
			return nil, fmt.Errorf("validate lifecycle operation state: %w", err)
		}
		payload.Operations = append(payload.Operations, operation)
	}
	sort.Slice(payload.Operations, func(i, j int) bool {
		return payload.Operations[i].Slug < payload.Operations[j].Slug
	})
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode lifecycle operation state: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxLifecycleOperationStoreBytes {
		return nil, errors.New("lifecycle operation state exceeds 256 KiB")
	}
	return data, nil
}

func (store *lifecycleOperationStore) canPersistTerminalOutcomes(
	operations map[string]lifecycleOperation, slug string, started lifecycleOperation,
) error {
	candidates := make(map[string]lifecycleOperation, len(operations)+1)
	for key, operation := range operations {
		if operation.State == "running" {
			operation.State = "failed"
			operation.Error = strings.Repeat("x", maxLifecycleOperationErrorBytes)
		}
		candidates[key] = operation
	}
	started.State = "failed"
	started.Error = strings.Repeat("x", maxLifecycleOperationErrorBytes)
	candidates[slug] = started
	_, err := store.encode(candidates)
	return err
}

func (store *lifecycleOperationStore) open() (*os.File, error) {
	fd, err := syscall.Open(store.path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), store.path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect lifecycle operation state: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) ||
		info.Mode().Perm() != 0o600 || info.Size() > maxLifecycleOperationStoreBytes {
		file.Close()
		return nil, errors.New("lifecycle operation state is not a private owner-only file")
	}
	return file, nil
}

func (store *lifecycleOperationStore) ensureDirectory() error {
	if err := os.MkdirAll(store.directory, 0o700); err != nil {
		return fmt.Errorf("create lifecycle operation state directory: %w", err)
	}
	return store.inspectDirectory()
}

func (store *lifecycleOperationStore) inspectDirectory() error {
	info, err := os.Lstat(store.directory)
	if err != nil {
		return fmt.Errorf("inspect lifecycle operation state directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("lifecycle operation state directory is unsafe")
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(store.directory, 0o700); err != nil {
			return fmt.Errorf("make lifecycle operation state directory private: %w", err)
		}
	}
	return nil
}
