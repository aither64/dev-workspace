// Package communication owns user-approved exchanges between session leads.
package communication

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	maxStateBytes   = 16 * 1024 * 1024
	maxOptionsBytes = 64 * 1024
	maxReceiptBytes = 4 * 1024
	transitionBytes = 512
)

type Store struct {
	directory, workspace string
	// Schema2Write is central package/runtime admission, entered before locks.
	// Missing wiring refuses all schema-2 mutations, including legacy delivery.
	Schema2Write func(context.Context, func() error) error
}

type state struct {
	Pi        *PiCommunicationState `json:"-"`
	Schema    int                   `json:"schema"`
	Workspace string                `json:"workspace"`
	Grants    []Grant               `json:"grants"`
	Messages  []Message             `json:"messages"`
}

func NewStore(root, workspace string) (*Store, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, errors.New("communication requires absolute state and workspace paths")
	}
	return &Store{directory: filepath.Join(root, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(workspace)))), workspace: workspace}, nil
}

func (s *Store) locked(ctx context.Context, write bool, run func(*state) error) error {
	return s.lockedAdmitted(ctx, write, false, run)
}
func (s *Store) lockedAdmitted(ctx context.Context, write, admitted bool, run func(*state) error) error {
	if write && !admitted {
		value, err := s.read()
		if err != nil {
			return err
		}
		if value.Schema == 2 {
			if s.Schema2Write == nil {
				return errors.New("Pi communication writer admission unavailable")
			}
			called := false
			var callbackErr error
			err := s.Schema2Write(ctx, func() error {
				if called {
					callbackErr = errors.New("duplicate Pi communication writer callback")
					return callbackErr
				}
				called = true
				callbackErr = s.lockedAdmitted(ctx, true, true, run)
				return callbackErr
			})
			if callbackErr != nil {
				return callbackErr
			}
			if err == nil && !called {
				return errors.New("Pi communication writer admission unavailable")
			}
			return err
		}
	}
	if write {
		if err := os.MkdirAll(s.directory, 0700); err != nil {
			return err
		}
	}
	flags := os.O_RDWR
	if write {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(filepath.Join(s.directory, "state.lock"), flags, 0600)
	if errors.Is(err, os.ErrNotExist) && !write {
		return run(&state{Schema: 1, Workspace: s.workspace})
	}
	if err != nil {
		return err
	}
	defer f.Close()
	mode := unix.LOCK_SH
	if write {
		mode = unix.LOCK_EX
	}
	for {
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := s.read()
	if err != nil {
		return err
	}
	if write && value.Schema == 2 && !admitted {
		return errors.New("Pi communication changed while waiting for writer admission")
	}
	return run(value)
}

func (s *Store) read() (*state, error) {
	value := &state{Schema: 1, Workspace: s.workspace, Grants: []Grant{}, Messages: []Message{}}
	f, err := os.Open(filepath.Join(s.directory, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxStateBytes {
		return nil, errors.New("invalid communication state file")
	}
	d := json.NewDecoder(io.LimitReader(f, maxStateBytes+1))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF || (value.Schema != 1 && value.Schema != 2) || value.Workspace != s.workspace {
		return nil, errors.New("unsupported communication state")
	}
	if err := value.validate(); err != nil {
		return nil, err
	}
	return value, nil
}

// save is called under the store lock, including immediately before an RPC.
func (s *Store) save(value *state) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return errors.New("communication history is full; no message was submitted")
	}
	f, err := os.CreateTemp(s.directory, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.directory, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Admission budgets the maximum remaining growth of every live record. Ordinary
// delivery and lifecycle writes may use that capacity after admission is full.
func (s *Store) admit(value *state) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	budget := len(data)
	for _, grant := range value.Grants {
		if grant.RevokedAt == nil {
			budget += transitionBytes
		}
	}
	for _, message := range value.Messages {
		if message.Status == "delivered" || message.Status == "canceled" {
			continue
		}
		budget += transitionBytes + maxReceiptBytes
		if message.Options == nil {
			budget += maxOptionsBytes
		}
	}
	if value.Schema == 2 {
		for _, g := range value.Pi.Grants {
			if (g.A.Pi != nil || g.B.Pi != nil) && g.RevokedAt == nil {
				budget += transitionBytes
			}
		}
		for _, m := range value.Pi.Messages {
			if m.From.Pi == nil && m.To.Pi == nil || m.Status == "delivered" || m.Status == "canceled" {
				continue
			}
			budget += transitionBytes + maxReceiptBytes
			if m.To.Pi != nil && m.PiDelivery == nil {
				budget += 64 * 1024
			} else if m.To.Codex != nil && m.Options == nil {
				budget += maxOptionsBytes
			}
		}
	}
	if budget > maxStateBytes {
		return errors.New("communication history is full; capacity is reserved for delivery and revocation")
	}
	return s.save(value)
}
