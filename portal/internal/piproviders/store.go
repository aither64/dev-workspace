package piproviders

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Store owns only provider-policy.json and provider-policy.lock beneath an
// already authorized P3 session state root. Load never creates either file.
type Store struct {
	Root    string
	Catalog Catalog
	Session agentbackend.SessionRef
}
type storedSession struct {
	Workspace   string `json:"workspace"`
	Slug        string `json:"slug"`
	Incarnation string `json:"incarnation"`
}
type document struct {
	Schema    int                `json:"schema"`
	Session   storedSession      `json:"session"`
	Revision  uint64             `json:"revision"`
	Default   VersionedSelection `json:"default"`
	Overrides []Override         `json:"overrides"`
}

func (s Store) Load() (Policy, error) {
	if !filepath.IsAbs(s.Root) || !sessionOK(s.Session) {
		return Policy{}, ErrInvalidPolicy
	}
	parent, err := os.Stat(s.Root)
	if err != nil {
		return Policy{}, err
	}
	if !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return Policy{}, ErrInvalidPolicy
	}
	f, err := os.Open(filepath.Join(s.Root, "provider-policy.json"))
	if err != nil {
		return Policy{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Policy{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
		return Policy{}, ErrInvalidPolicy
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil {
		return Policy{}, err
	}
	if len(data) > 64<<10 {
		return Policy{}, ErrInvalidPolicy
	}
	var d document
	if strictDecode(data, &d) != nil || d.Schema != 1 {
		return Policy{}, ErrInvalidPolicy
	}
	p := Policy{Session: agentbackend.SessionRef{Workspace: d.Session.Workspace, Slug: d.Session.Slug, Incarnation: d.Session.Incarnation}, Revision: d.Revision, Default: d.Default, Overrides: d.Overrides}
	if p.Session != s.Session {
		return Policy{}, ErrStalePolicy
	}
	if err := ValidatePolicy(s.Catalog, p); err != nil {
		return Policy{}, err
	}
	return p, nil
}
func (s Store) CompareAndSwap(expectedRevision uint64, next Policy) error {
	if !filepath.IsAbs(s.Root) || next.Session != s.Session {
		return ErrInvalidPolicy
	}
	if err := ValidatePolicy(s.Catalog, next); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	info, err := os.Stat(s.Root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return ErrInvalidPolicy
	}
	lock, err := os.OpenFile(filepath.Join(s.Root, "provider-policy.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil {
		return err
	}
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0600 {
		return ErrInvalidPolicy
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	old, err := s.Load()
	if errors.Is(err, os.ErrNotExist) {
		if expectedRevision != 0 || next.Revision != 1 || next.Default.Revision != 1 {
			return ErrStalePolicy
		}
		for _, o := range next.Overrides {
			if o.Value.Revision != 1 {
				return ErrStalePolicy
			}
		}
	} else if err != nil {
		return err
	} else if old.Revision != expectedRevision || !validTransition(old, next) {
		return ErrStalePolicy
	}
	d := document{Schema: 1, Session: storedSession{next.Session.Workspace, next.Session.Slug, next.Session.Incarnation}, Revision: next.Revision, Default: next.Default, Overrides: next.Overrides}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(data) > 64<<10 {
		return ErrInvalidPolicy
	}
	tmp, err := os.CreateTemp(s.Root, ".provider-policy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Close()
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(s.Root, "provider-policy.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.Root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
