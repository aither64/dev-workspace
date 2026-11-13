package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"golang.org/x/sys/unix"
)

func (s BackendBindingStore) publicationDirectory(slug string) (string, error) {
	if !backendCanonicalPath(s.Root) || !backendCanonicalPath(s.Workspace) || !backendSlug(slug) {
		return "", errors.New("invalid backend publication scope")
	}
	digest := sha256.Sum256([]byte(s.Workspace))
	return filepath.Join(s.Root, "session-backends", hex.EncodeToString(digest[:])), nil
}

// Create reserves a new explicit Pi binding under the caller's creation/runtime
// ownership. It never adopts legacy evidence or supplies native identity. Exact
// replay returns the original record; different creation identities conflict.
func (s BackendBindingStore) Create(ctx context.Context, binding BackendBinding) (BackendBinding, error) {
	if err := binding.Validate(s.Workspace, binding.Slug); err != nil {
		return BackendBinding{}, err
	}
	if binding.Revision != 1 || binding.State != "creating" {
		return BackendBinding{}, agentbackend.ErrBindingConflict
	}
	for _, conversation := range binding.Conversations {
		if conversation.State != "creating" || conversation.NativeID != "" {
			return BackendBinding{}, agentbackend.ErrBindingConflict
		}
	}
	return s.publish(ctx, binding.Slug, func(current *BackendBinding) (BackendBinding, error) {
		if current != nil && !reflect.DeepEqual(*current, binding) {
			return BackendBinding{}, agentbackend.ErrBindingConflict
		}
		return binding, nil
	})
}

// CompareAndSwap only advances the reserved conversations toward ready. The
// host owns native-history/admission proof and runtime/team locks; this short
// store lock protects file publication and never spans an external operation.
func (s BackendBindingStore) CompareAndSwap(ctx context.Context, expected uint64, next BackendBinding) (BackendBinding, error) {
	if err := next.Validate(s.Workspace, next.Slug); err != nil {
		return BackendBinding{}, err
	}
	return s.publish(ctx, next.Slug, func(current *BackendBinding) (BackendBinding, error) {
		if current == nil || expected == 0 || expected == math.MaxUint64 || current.Revision != expected || next.Revision != expected+1 ||
			current.SessionIncarnation != next.SessionIncarnation || current.CreationReceiptID != next.CreationReceiptID ||
			len(current.Conversations) != len(next.Conversations) || (current.State == "ready" && next.State != "ready") {
			return BackendBinding{}, agentbackend.ErrBindingConflict
		}
		for index, previous := range current.Conversations {
			updated := next.Conversations[index]
			if previous.Address != updated.Address || previous.Incarnation != updated.Incarnation ||
				(previous.NativeID != "" && previous.NativeID != updated.NativeID) ||
				(previous.State == "ready" && updated != previous) {
				return BackendBinding{}, agentbackend.ErrBindingConflict
			}
		}
		return next, nil
	})
}

func (s BackendBindingStore) publish(ctx context.Context, slug string, change func(*BackendBinding) (BackendBinding, error)) (BackendBinding, error) {
	if err := ctx.Err(); err != nil {
		return BackendBinding{}, err
	}
	directory, err := s.publicationDirectory(slug)
	if err != nil {
		return BackendBinding{}, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return BackendBinding{}, err
	}
	lock, err := lockRuntimeContext(ctx, directory, slug, ".binding.lock", unix.LOCK_EX)
	if err != nil {
		return BackendBinding{}, err
	}
	defer lock.Close()
	var current *BackendBinding
	loaded, err := s.Load(slug)
	if err == nil {
		current = &loaded
	} else if !errors.Is(err, agentbackend.ErrNotBound) {
		return BackendBinding{}, err
	}
	next, err := change(current)
	if err != nil {
		return BackendBinding{}, err
	}
	if current != nil && reflect.DeepEqual(*current, next) {
		return *current, nil
	}
	if err := ctx.Err(); err != nil {
		return BackendBinding{}, err
	}
	if err := writeBackendRecord(directory, slug+".json", next); err != nil {
		return BackendBinding{}, err
	}
	return next, nil
}

// PublishPiRuntimeAuthority writes only a same-generation projection. Runtime
// replacement is P3-owned and needs separate proof. Call with runtime EX held,
// after the binding CAS; a failed write leaves the binding for reconciliation.
func PublishPiRuntimeAuthority(directory string, binding BackendBinding, next PiRuntimeAuthority) error {
	if err := next.ValidateBinding(binding); err != nil {
		return err
	}
	if !backendCanonicalPath(directory) {
		return errors.New("invalid Pi authority directory")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	current, err := LoadPiRuntimeAuthority(directory, next.Slug, next.Workspace)
	if err != nil && !errors.Is(err, agentbackend.ErrNotBound) {
		return fmt.Errorf("%w: %w", agentbackend.ErrBindingConflict, err)
	}
	if err == nil {
		if current.SessionIncarnation != next.SessionIncarnation || current.Generation != next.Generation ||
			current.BindingRevision > next.BindingRevision || (current.State == "ready" && next.State != "ready") {
			return agentbackend.ErrBindingConflict
		}
		if current == next {
			return nil
		}
	}
	return writeBackendRecord(directory, next.Slug+".json", next)
}

func writeBackendRecord(directory, name string, record any) error {
	fd, err := openAuthorityDirectory(directory)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data)+1 > authorityMaxSize {
		return errors.New("backend publication exceeds 64 KiB")
	}
	file, err := os.CreateTemp(directory, ".backend-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(directory, name)); err != nil {
		return err
	}
	return unix.Fsync(fd)
}
