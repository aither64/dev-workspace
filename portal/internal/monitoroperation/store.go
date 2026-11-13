package monitoroperation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const maxOperationBytes = 512 * 1024

type Store struct{ directory, workspace string }

// Transaction holds only an operation lock. Callers take session admission
// before Lock; Save commits immediately and Close never commits implicitly.
type Transaction struct {
	Operation *Operation
	store     *Store
	file      *os.File
	before    Operation
	piWrite   bool
}

func NewStore(root, workspace string) (*Store, error) {
	if !canonicalPath(root) || !canonicalPath(workspace) {
		return nil, errors.New("monitor operations require canonical absolute state and workspace paths")
	}
	return &Store{filepath.Join(root, "monitor-operations", fmt.Sprintf("%x", sha256.Sum256([]byte(workspace)))), workspace}, nil
}

func (s *Store) path(id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid monitor operation identity")
	}
	return filepath.Join(s.directory, id+".json"), nil
}

func (s *Store) Get(ctx context.Context, id string) (Operation, error) {
	tx, err := s.Lock(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Close()
	return *tx.Operation, nil
}

func lockFile(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *Store) Lock(ctx context.Context, id string) (*Transaction, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	// Observation of absent state must not create a runtime obligation.
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	f, err := lockFile(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	op, err := s.read(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Transaction{Operation: &op, store: s, file: f, before: cloneOperation(op)}, nil
}

func (tx *Transaction) Close() error {
	if tx.file == nil {
		return nil
	}
	f := tx.file
	tx.file = nil
	return f.Close()
}

func (tx *Transaction) Save() error {
	if tx.file == nil || tx.Operation == nil {
		return errors.New("monitor transaction is closed")
	}
	if tx.Operation.Pi != nil && !tx.piWrite {
		return ErrPiUtilityUnavailable
	}
	if err := validateTransition(tx.before, *tx.Operation); err != nil {
		return err
	}
	if err := tx.store.save(*tx.Operation); err != nil {
		return err
	}
	tx.before = cloneOperation(*tx.Operation)
	return nil
}

func cloneOperation(op Operation) Operation {
	data, _ := json.Marshal(op)
	var copy Operation
	_ = json.Unmarshal(data, &copy)
	return copy
}

func (s *Store) read(path string) (Operation, error) {
	var op Operation
	f, err := os.Open(path)
	if err != nil {
		return op, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return op, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxOperationBytes {
		return op, errors.New("monitor operation state must be a bounded private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxOperationBytes+1))
	if err != nil {
		return op, err
	}
	if len(data) > maxOperationBytes {
		return op, errors.New("monitor operation state exceeds bound")
	}
	if err := uniqueJSONKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return op, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&op); err != nil {
		return op, err
	}
	if d.Decode(new(any)) != io.EOF {
		return op, errors.New("monitor operation state has trailing data")
	}
	if filepath.Base(path) != op.ID+".json" || op.Workspace != s.workspace {
		return op, errors.New("monitor operation state identity differs")
	}
	return op, validateOperation(op)
}

// Duplicate keys make persisted ownership ambiguous even when a JSON decoder
// would otherwise choose the final value. Apply the same strictness as recovery.
func uniqueJSONKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid monitor state key")
			}
			seen[name] = true
			if err := uniqueJSONKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSONKeys(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid monitor state delimiter")
	}
	_, err = d.Token()
	return err
}

func (s *Store) save(op Operation) error {
	if op.Workspace != s.workspace {
		return errors.New("monitor operation workspace differs")
	}
	if err := validateOperation(op); err != nil {
		return err
	}
	path, err := s.path(op.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	if len(data)+1 > maxOperationBytes {
		return errors.New("monitor operation state exceeds its reserved capacity")
	}
	f, err := os.CreateTemp(s.directory, ".operation-")
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
	dir, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) List(ctx context.Context, slug string) ([]Operation, error) {
	entries, err := os.ReadDir(s.directory)
	if errors.Is(err, os.ErrNotExist) {
		return []Operation{}, nil
	}
	if err != nil {
		return nil, err
	}
	operations := []Operation{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, errors.New("invalid monitor operation entry")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := s.path(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		// Atomic publication permits a validated snapshot without waiting on
		// another operation's native RPC. Mutators revalidate under Lock.
		op, err := s.read(path)
		if err != nil {
			return nil, err
		}
		if slug == "" || operationSlug(op) == slug {
			operations = append(operations, op)
		}
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].ID < operations[j].ID })
	return operations, nil
}

// reserve holds the workspace admission lock only for local reservation work.
// Native creation happens after releasing it, under the operation transaction.
func (s *Store) reserve(ctx context.Context, op Operation) (Operation, error) {
	if err := os.MkdirAll(s.directory, 0700); err != nil {
		return Operation{}, err
	}
	f, err := lockFile(ctx, filepath.Join(s.directory, ".admission.lock"))
	if err != nil {
		return Operation{}, err
	}
	defer f.Close()
	path, err := s.path(op.ID)
	if err != nil {
		return Operation{}, err
	}
	if prior, err := s.read(path); err == nil {
		if (op.Pi != nil && (prior.Pi == nil || !reflect.DeepEqual(prior.Pi.Owner, op.Pi.Owner) || prior.Pi.RequestDigest != op.Pi.RequestDigest)) ||
			(op.Pi == nil && (prior.Pi != nil || prior.RequestDigest != op.RequestDigest || !SameOwner(prior.Owner, op.Owner))) {
			return Operation{}, errors.New("monitor start identity was reused for another request or owner")
		}
		return prior, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Operation{}, err
	}
	operations, err := s.List(ctx, operationSlug(op))
	if err != nil {
		return Operation{}, err
	}
	active := 0
	for _, prior := range operations {
		if operationActive(prior) {
			active++
		}
	}
	if active >= operationCapacity(op) {
		return Operation{}, errors.New("monitor utility concurrency is full; prior execution must be proved terminated")
	}
	if err := s.save(op); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func (s *Store) ResolveReporter(ctx context.Context, thread string) (Operation, error) {
	if thread == "" {
		return Operation{}, errors.New("native monitor caller is missing")
	}
	operations, err := s.List(ctx, "")
	if err != nil {
		return Operation{}, err
	}
	var found *Operation
	for i := range operations {
		op := &operations[i]
		if op.Launch.ThreadID != thread {
			continue
		}
		if found != nil {
			return Operation{}, errors.New("ambiguous native monitor caller")
		}
		found = op
	}
	if found == nil || found.Disposition != "" || !found.Launch.BootstrapAttempted {
		return Operation{}, errors.New("caller is not a registered monitor utility")
	}
	return *found, nil
}

func validateTransition(old, next Operation) error {
	if old.Pi != nil || next.Pi != nil {
		if old.Pi == nil || next.Pi == nil {
			return errors.New("monitor owner kind changed")
		}
		return validatePiTransition(*old.Pi, *next.Pi)
	}
	if old.ID != next.ID || old.Schema != next.Schema || old.Workspace != next.Workspace ||
		old.RequestDigest != next.RequestDigest || !reflect.DeepEqual(old.Owner, next.Owner) ||
		!reflect.DeepEqual(old.Source, next.Source) || old.Brief != next.Brief || old.CancellationScope != next.CancellationScope ||
		!reflect.DeepEqual(old.UtilityPolicy, next.UtilityPolicy) || !old.CreatedAt.Equal(next.CreatedAt) {
		return errors.New("monitor operation identity is immutable")
	}
	if old.Launch.ProjectID != "" && next.Launch.ProjectID != old.Launch.ProjectID ||
		old.Launch.ThreadID != "" && next.Launch.ThreadID != old.Launch.ThreadID ||
		old.Launch.CreateAttempted && !next.Launch.CreateAttempted || old.Launch.BootstrapAttempted && !next.Launch.BootstrapAttempted ||
		old.Launch.BootstrapMarker != "" && old.Launch.BootstrapMarker != next.Launch.BootstrapMarker ||
		old.Launch.MessageID != next.Launch.MessageID || old.Launch.Envelope != next.Launch.Envelope ||
		!reflect.DeepEqual(old.Launch.Options, next.Launch.Options) ||
		old.Launch.Receipt != nil && !reflect.DeepEqual(old.Launch.Receipt, next.Launch.Receipt) ||
		old.Launch.TerminatedAt != nil && !reflect.DeepEqual(old.Launch.TerminatedAt, next.Launch.TerminatedAt) ||
		old.CanceledAt != nil && !reflect.DeepEqual(old.CanceledAt, next.CanceledAt) || len(next.Events) < len(old.Events) {
		return errors.New("monitor launch or terminal identity is immutable")
	}
	if old.Resolution != nil && !reflect.DeepEqual(old.Resolution, next.Resolution) {
		return errors.New("monitor resolution is immutable")
	}
	for i, prior := range old.Events {
		event := next.Events[i]
		if prior.ID != event.ID || !prior.CreatedAt.Equal(event.CreatedAt) || !reflect.DeepEqual(prior.Report, event.Report) ||
			prior.Delivery.MessageID != event.Delivery.MessageID || prior.Delivery.Envelope != event.Delivery.Envelope ||
			prior.Delivery.Context != event.Delivery.Context ||
			prior.Delivery.Options != nil && !reflect.DeepEqual(prior.Delivery.Options, event.Delivery.Options) ||
			prior.Delivery.Status == "accepted" && !reflect.DeepEqual(prior.Delivery.Receipt, event.Delivery.Receipt) {
			return errors.New("monitor event identity or accepted receipt is immutable")
		}
	}
	return nil
}
