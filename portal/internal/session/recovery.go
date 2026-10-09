package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// Recovery is private execution permission, separate from runtime authority.
// ActiveThreads applies only to the exact current App Server socket generation.
type Recovery struct {
	Schema         int                   `json:"schema"`
	Workspace      string                `json:"workspace"`
	Slug           string                `json:"slug"`
	ThreadID       string                `json:"thread_id"`
	SocketPath     string                `json:"socket_path"`
	SocketIdentity string                `json:"socket_identity"`
	Automatic      bool                  `json:"automatic"`
	ActiveThreads  []string              `json:"active_threads"`
	Continuation   *RecoveryContinuation `json:"continuation,omitempty"`
}

type RecoveryContinuation struct {
	RequestID string `json:"request_id"`
	Prompt    bool   `json:"prompt"`
}

type RecoveryStore struct{ Root, Workspace string }

var retireRecovery = errors.New("retire session recovery record")

func (s RecoveryStore) directory() string {
	id := sha256.Sum256([]byte(s.Workspace))
	return filepath.Join(s.Root, "session-recovery", hex.EncodeToString(id[:]))
}

func (s RecoveryStore) path(slug string) (string, error) {
	if !ValidSlug(slug) || !filepath.IsAbs(s.Root) || !filepath.IsAbs(s.Workspace) {
		return "", errors.New("invalid session recovery scope")
	}
	return filepath.Join(s.directory(), slug+".json"), nil
}

func (s RecoveryStore) Load(slug string) (Recovery, error) {
	path, err := s.path(slug)
	if err != nil {
		return Recovery{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Recovery{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Recovery{}, err
	}
	var owner unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &owner); err != nil {
		return Recovery{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || owner.Uid != uint32(os.Geteuid()) || info.Size() > 65536 {
		return Recovery{}, errors.New("session recovery must be a private file of at most 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return Recovery{}, err
	}
	var syntax yaml.Node
	if err := yaml.Unmarshal(data, &syntax); err != nil {
		return Recovery{}, err
	}
	if err := rejectAliasesAndDuplicateKeys(&syntax); err != nil {
		return Recovery{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record Recovery
	if err := decoder.Decode(&record); err != nil {
		return Recovery{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Recovery{}, errors.New("session recovery has trailing data")
	}
	if err := s.validate(slug, record); err != nil {
		return Recovery{}, err
	}
	return record, nil
}

func (s RecoveryStore) validate(slug string, record Recovery) error {
	if record.Schema != 1 || record.Workspace != s.Workspace || record.Slug != slug ||
		record.ThreadID == "" || record.SocketIdentity == "" || !validSocketPath(record.SocketPath) || record.ActiveThreads == nil {
		return errors.New("session recovery identity is invalid")
	}
	seen := make(map[string]bool)
	for _, thread := range record.ActiveThreads {
		if thread == "" || len(thread) > 4096 || seen[thread] {
			return errors.New("session recovery contains invalid active threads")
		}
		seen[thread] = true
	}
	if record.Continuation != nil && record.Continuation.RequestID == "" {
		return errors.New("session recovery continuation has no request identity")
	}
	return nil
}

// Update serializes portal and CLI writers independently of their runtime locks.
func (s RecoveryStore) Update(ctx context.Context, slug string, change func(*Recovery) error) error {
	path, err := s.path(slug)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.directory(), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	record, err := s.Load(slug)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := change(&record); err != nil {
		if errors.Is(err, retireRecovery) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			dir, err := os.Open(s.directory())
			if err != nil {
				return err
			}
			defer dir.Close()
			return dir.Sync()
		}
		return err
	}
	if err := s.validate(slug, record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > 65536 {
		return errors.New("session recovery exceeds 64 KiB")
	}
	temporary, err := os.CreateTemp(s.directory(), ".recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(s.directory())
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s RecoveryStore) Retire(ctx context.Context, slug, root, socket string, remove bool) error {
	return s.Update(ctx, slug, func(record *Recovery) error {
		if record.Schema == 0 {
			return retireRecovery
		}
		if record.ThreadID != root || record.SocketPath != socket {
			return errors.New("retained recovery identity changed")
		}
		if remove {
			return retireRecovery
		}
		record.Automatic = false
		record.ActiveThreads = []string{}
		record.Continuation = nil
		return nil
	})
}

func SocketIdentity(path string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return "", errors.New("Codex endpoint is not a socket")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d:%d:%d", string(boot[:len(boot)-1]), stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec), nil
}

func (r Recovery) Active(threadID, socketIdentity string) bool {
	return socketIdentity != "" && r.SocketIdentity == socketIdentity && slices.Contains(r.ActiveThreads, threadID)
}

func (s RecoveryStore) Set(ctx context.Context, slug, root, socket string, automatic, activate bool) error {
	epoch, err := SocketIdentity(socket)
	if err != nil {
		return err
	}
	return s.Update(ctx, slug, func(record *Recovery) error {
		if record.Schema != 0 && (record.ThreadID != root || record.SocketPath != socket) {
			return errors.New("retained recovery identity changed")
		}
		if record.SocketIdentity != epoch {
			record.ActiveThreads = []string{}
			record.Continuation = nil
		}
		record.Schema, record.Workspace, record.Slug = 1, s.Workspace, slug
		record.ThreadID, record.SocketPath, record.SocketIdentity = root, socket, epoch
		record.Automatic = automatic
		if record.ActiveThreads == nil {
			record.ActiveThreads = []string{}
		}
		if activate && !slices.Contains(record.ActiveThreads, root) {
			record.ActiveThreads = append(record.ActiveThreads, root)
		}
		if !automatic {
			record.ActiveThreads = []string{}
			record.Continuation = nil
		}
		return nil
	})
}

// AllowsImplicitResume checks retained permission without making App Server calls.
func (s RecoveryStore) AllowsImplicitResume(socket, thread string) bool {
	epoch, err := SocketIdentity(socket)
	if err != nil {
		return false
	}
	summaries, err := List(s.Workspace)
	if err != nil {
		return false
	}
	for _, summary := range summaries {
		if summary.Archived || RequireRetainedReady(&summary, socket) != nil {
			continue
		}
		if owner, err := PendingLifecycle(s.Workspace, summary.Slug); err != nil || owner != "" {
			continue
		}
		record, err := s.Load(summary.Slug)
		if err == nil && record.ThreadID == summary.Codex.ThreadID && record.SocketPath == socket && record.Active(thread, epoch) {
			return true
		}
	}
	return false
}
