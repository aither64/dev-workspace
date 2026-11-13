package monitoroperation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func (s *Store) observerPath(id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid monitor observer identity")
	}
	return filepath.Join(s.directory, "observers", id+".json"), nil
}

// Native and observer records share the original per-ID lock to reject collisions.
type observerTransaction struct {
	record *ObserverRecord
	native *Operation
	before *ObserverRecord
	store  *Store
	file   *os.File
}

func (tx *observerTransaction) close() {
	if tx.file != nil {
		_ = tx.file.Close()
		tx.file = nil
	}
}

func (s *Store) lockObserver(ctx context.Context, id string, create bool) (*observerTransaction, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	observerPath, _ := s.observerPath(id)
	if create {
		if err := os.MkdirAll(s.directory, 0700); err != nil {
			return nil, err
		}
	} else if _, err := os.Lstat(observerPath); err != nil {
		return nil, err
	}
	f, err := lockFile(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	tx := &observerTransaction{store: s, file: f}
	if op, err := s.read(path); err == nil {
		tx.native = &op
	} else if !errors.Is(err, os.ErrNotExist) {
		tx.close()
		return nil, err
	}
	if record, err := s.readObserver(observerPath); err == nil {
		tx.record = &record
		copy := cloneObserver(record)
		tx.before = &copy
	} else if !errors.Is(err, os.ErrNotExist) {
		tx.close()
		return nil, err
	}
	if tx.record != nil {
		if err := validateObserverPair(tx.native, *tx.record); err != nil {
			tx.close()
			return nil, err
		}
	} else if !create {
		tx.close()
		return nil, os.ErrNotExist
	}
	return tx, nil
}

func validateObserverPair(native *Operation, record ObserverRecord) error {
	if native != nil {
		return errors.New("duplicate native and direct monitor identity")
	}
	return nil
}

func (s *Store) GetObserver(ctx context.Context, id string) (ObserverRecord, error) {
	tx, err := s.lockObserver(ctx, id, false)
	if err != nil {
		return ObserverRecord{}, err
	}
	defer tx.close()
	return cloneObserver(*tx.record), nil
}

func (s *Store) ListObservers(ctx context.Context, slug string) ([]ObserverRecord, error) {
	root := filepath.Join(s.directory, "observers")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return []ObserverRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("monitor observer directory is invalid")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	result := []ObserverRecord{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".observer-") && entry.Type().IsRegular() {
			continue
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, errors.New("invalid monitor observer entry")
		}
		record, err := s.GetObserver(ctx, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if slug == "" || record.Owner.Slug == slug {
			result = append(result, record)
		}
	}
	return result, nil
}

func cloneObserver(record ObserverRecord) ObserverRecord {
	data, _ := json.Marshal(record)
	var copy ObserverRecord
	_ = json.Unmarshal(data, &copy)
	return copy
}

func (s *Store) readObserver(path string) (ObserverRecord, error) {
	var record ObserverRecord
	f, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxOperationBytes {
		return record, errors.New("observer must be a bounded private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxOperationBytes+1))
	if err != nil {
		return record, err
	}
	if len(data) > maxOperationBytes {
		return record, errors.New("monitor observer exceeds bound")
	}
	if err := uniqueJSONKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return record, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&record); err != nil {
		return record, err
	}
	if d.Decode(new(any)) != io.EOF {
		return record, errors.New("observer has trailing data")
	}
	if record.Workspace != s.workspace || filepath.Base(path) != record.ID+".json" {
		return record, errors.New("observer storage identity differs")
	}
	return record, validateObserver(record)
}

func (tx *observerTransaction) save() error {
	if tx.file == nil || tx.record == nil {
		return errors.New("observer transaction is closed")
	}
	if err := validateObserver(*tx.record); err != nil {
		return err
	}
	if err := validateObserverPair(tx.native, *tx.record); err != nil {
		return err
	}
	if tx.before != nil {
		a, b := *tx.before, *tx.record
		if a.ID != b.ID || a.Schema != b.Schema || a.Workspace != b.Workspace || !reflect.DeepEqual(a.Owner, b.Owner) ||
			a.Driver != b.Driver || a.RequestDigest != b.RequestDigest || a.SourceDigest != b.SourceDigest ||
			!reflect.DeepEqual(a.Source, b.Source) || !a.CreatedAt.Equal(b.CreatedAt) || b.Revision != a.Revision+1 || b.UpdatedAt.Before(a.UpdatedAt) {
			return errors.New("observer immutable identity/revision changed")
		}
		if a.Archive != nil && !reflect.DeepEqual(a.Archive, b.Archive) || a.LocalProof != nil && !reflect.DeepEqual(a.LocalProof, b.LocalProof) ||
			a.CanceledAt != nil && !reflect.DeepEqual(a.CanceledAt, b.CanceledAt) {
			return errors.New("observer proof or revocation is immutable")
		}
		allowed := a.Phase == b.Phase || a.Phase == "observing" && b.Phase == "quiescing" || a.Phase == "quiescing" && b.Phase == "detached"
		if !allowed {
			return errors.New("invalid observer phase transition")
		}
		// Share the existing immutable event/report/accepted-receipt contract.
		if err := validateTransition(Operation{Events: a.Events}, Operation{Events: b.Events}); err != nil {
			return err
		}
	} else if tx.record.Revision != 1 {
		return errors.New("new observer revision must be one")
	}
	path, _ := tx.store.observerPath(tx.record.ID)
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	parent, err := os.Open(tx.store.directory)
	if err != nil {
		return err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return err
	}
	data, err := json.Marshal(tx.record)
	if err != nil {
		return err
	}
	if len(data)+1 > maxOperationBytes {
		return errors.New("monitor observer exceeds reserved capacity")
	}
	f, err := os.CreateTemp(directory, ".observer-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err == nil {
		copy := cloneObserver(*tx.record)
		tx.before = &copy
	}
	return err
}
