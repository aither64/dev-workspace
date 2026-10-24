package turnretry

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
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

type Store struct{ Root, Workspace string }

var attemptIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s Store) directory() string { return filepath.Join(s.Root, "turn-retries", hash(s.Workspace)) }
func (s Store) path(scope Scope) string {
	return filepath.Join(s.directory(), scope.Slug, hash(scope.Thread)+".json")
}

func (s Store) validScope(scope Scope) bool {
	return filepath.IsAbs(s.Root) && filepath.IsAbs(s.Workspace) && scope.Workspace == s.Workspace &&
		scope.Slug != "" && scope.Slug != "." && scope.Slug != ".." && !strings.ContainsAny(scope.Slug, "/\\") &&
		scope.Root != "" && scope.Thread != "" && len(scope.Thread) <= 4096 && filepath.IsAbs(scope.Socket)
}

// Lock is the application-wide admission fence, shared by independent portal,
// team and CLI processes. Hold it through native submission and durable result
// writes. Acquire it after package/runtime/roster/conversation locks, never
// while waiting for a timer. Native ledger locks are strictly inside it.
func (s Store) Lock(ctx context.Context, scope Scope) (*Transaction, error) {
	return s.lock(ctx, scope, true)
}

// Inspect takes the existing admission fence without creating a ledger. A
// preference inventory must never enroll or rewrite ordinary delivery state.
func (s Store) Inspect(ctx context.Context, scope Scope) (*Transaction, error) {
	return s.lock(ctx, scope, false)
}

func (s Store) lock(ctx context.Context, scope Scope, create bool) (*Transaction, error) {
	if !s.validScope(scope) {
		return nil, errors.New("invalid retry scope")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory := filepath.Dir(s.path(scope))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("retry directory must be private")
	}
	lock, err := os.OpenFile(s.path(scope)+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			lock.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	t := &Transaction{store: s, scope: scope, lock: lock}
	t.State, err = s.Load(scope)
	if errors.Is(err, os.ErrNotExist) && !create {
		t.State = New(scope)
		return t, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		// Bound retained per-session records; never evict unresolved work or
		// discard an opt-out to admit a new thread.
		guard, guardErr := os.OpenFile(filepath.Join(directory, ".records.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if guardErr != nil {
			t.Close()
			return nil, guardErr
		}
		defer guard.Close()
		if guardErr = unix.Flock(int(guard.Fd()), unix.LOCK_EX|unix.LOCK_NB); guardErr != nil {
			t.Close()
			return nil, guardErr
		}
		defer unix.Flock(int(guard.Fd()), unix.LOCK_UN)
		entries, listErr := filepath.Glob(filepath.Join(directory, "*.json"))
		if listErr != nil || len(entries) >= 128 {
			t.Close()
			return nil, errors.New("retry session record limit reached")
		}
		t.State, err = New(scope), nil
		if err = t.Save(); err != nil {
			t.Close()
			return nil, err
		}
	}
	if err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

type Transaction struct {
	store Store
	scope Scope
	lock  *os.File
	State State
}

func (t *Transaction) Close() error {
	if t.lock == nil {
		return nil
	}
	err := t.lock.Close()
	t.lock = nil
	return err
}

func (s Store) Load(scope Scope) (State, error) {
	if !s.validScope(scope) {
		return State{}, errors.New("invalid retry scope")
	}
	file, err := os.Open(s.path(scope))
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	var owner unix.Stat_t
	info, err := file.Stat()
	if err != nil {
		return State{}, err
	}
	if err := unix.Fstat(int(file.Fd()), &owner); err != nil {
		return State{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || owner.Uid != uint32(os.Geteuid()) || info.Size() > MaxBytes {
		return State{}, errors.New("invalid private retry record")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return State{}, err
	}
	var syntax yaml.Node
	if err := yaml.Unmarshal(data, &syntax); err != nil {
		return State{}, err
	}
	if err := uniqueKeys(&syntax); err != nil {
		return State{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return State{}, errors.New("retry record has trailing data")
	}
	return state, validate(scope, state)
}

func uniqueKeys(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return errors.New("retry record has aliases")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if seen[key] {
				return errors.New("retry record has duplicate keys")
			}
			seen[key] = true
		}
	}
	for _, child := range node.Content {
		if err := uniqueKeys(child); err != nil {
			return err
		}
	}
	return nil
}

func validate(scope Scope, state State) error {
	if state.Schema != Schema || state.Epoch == 0 || state.Scope != scope || len(state.Reason) > 128 || len(state.Cyber) > CyberLimit+1 || len(state.Snapshots) > MaxSnapshots || state.Cyber == nil || state.Snapshots == nil {
		return errors.New("retry state identity or bounds changed")
	}
	switch state.Phase {
	case "idle", "scheduled", "submitting", "reconciling", "running", "cancelled", "exhausted", "blocked":
	default:
		return errors.New("unsupported retry phase")
	}
	if state.Class != "" && state.Class != "capacity" && state.Class != "cybersecurity" {
		return errors.New("unsupported retry class")
	}
	charges := map[string]Charge{}
	for _, charge := range state.Cyber {
		if !attemptIDPattern.MatchString(charge.ID) || charge.AtMS <= 0 || charges[charge.ID].ID != "" {
			return errors.New("invalid cyber dispatch time")
		}
		charges[charge.ID] = charge
	}
	check := func(snapshot Snapshot) error {
		digest, err := snapshot.Hash()
		if err != nil || snapshot.Epoch == 0 || snapshot.Epoch > state.Epoch || snapshot.Version != 1 || snapshot.ID == "" || snapshot.Digest != digest {
			return errors.New("retry settings snapshot is invalid")
		}
		if snapshot.Proven && (snapshot.Turn == "" || snapshot.Options.Model == "" || snapshot.Options.ReasoningEffort == "" || snapshot.Provider == "" || (snapshot.Mode != "default" && snapshot.Mode != "plan")) {
			return errors.New("retry settings proof is incomplete")
		}
		return nil
	}
	for _, snapshot := range state.Snapshots {
		if err := check(snapshot); err != nil {
			return err
		}
	}
	if state.Binding != nil {
		digest, err := state.Binding.Hash()
		if err != nil || state.Binding.Epoch != state.Epoch || state.Binding.Digest != digest || state.Binding.SocketIdentity == "" || state.Binding.Provider == "" {
			return errors.New("retry loaded binding is invalid")
		}
	}
	if state.Pending != nil {
		p := state.Pending
		if !attemptIDPattern.MatchString(p.ID) || p.Action == "" || p.Failure == "" {
			return errors.New("retry attempt identity is incomplete")
		}
		if err := check(p.Snapshot); err != nil {
			return err
		}
		if p.ChargedAtMS != 0 && (charges[p.ID].ID == "" || charges[p.ID].AtMS != p.ChargedAtMS) {
			return errors.New("retry quota reservation changed")
		}
		if state.Class == "cybersecurity" && (state.Phase == "submitting" || state.Phase == "reconciling" || state.Phase == "running") && p.ChargedAtMS == 0 {
			return errors.New("retry quota reservation is missing")
		}
	}
	if state.Phase == "scheduled" && (state.Pending == nil || state.DeadlineMS <= 0) || state.Uncertain() && state.Pending == nil {
		return errors.New("retry phase is incomplete")
	}
	return nil
}

func (t *Transaction) Save() error {
	if t.lock == nil {
		return errors.New("retry admission lock is closed")
	}
	if err := validate(t.scope, t.State); err != nil {
		return err
	}
	data, err := json.Marshal(t.State)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxBytes {
		return errors.New("retry state exceeds 1 MiB")
	}
	path := t.store.path(t.scope)
	file, err := os.CreateTemp(filepath.Dir(path), ".retry-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Session records are retained on stop/revive. Cancellation never deletes a
// possibly submitted identity. Lifecycle callers already own the runtime lock.
func (s Store) CancelSession(ctx context.Context, slug, root, socket string, backends ...Backend) error {
	paths, err := filepath.Glob(filepath.Join(s.directory(), slug, "*.json"))
	if err != nil {
		return err
	}
	var unresolved error
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var header struct {
			Scope Scope `json:"scope"`
		}
		if json.Unmarshal(data, &header) != nil || header.Scope.Slug != slug || header.Scope.Root != root || header.Scope.Socket != socket {
			return errors.New("retry session identity changed")
		}
		t, err := s.Lock(ctx, header.Scope)
		if err != nil {
			return err
		}
		t.State.Cancel("lifecycle")
		t.State.Binding = nil
		if t.State.Uncertain() && len(backends) == 1 {
			p := *t.State.Pending
			if receipt, found, err := backends[0].Reconcile(ctx, header.Scope, p); err == nil && found {
				if err := t.State.Accepted(receipt.TurnID, time.Now()); err != nil {
					t.Close()
					return err
				}
			} else if err == nil {
				never, err := backends[0].NeverSubmitted(ctx, header.Scope, p)
				if err == nil && never {
					t.State.Phase, t.State.Pending = "cancelled", nil
				}
			}
		}
		err = t.Save()
		uncertain := t.State.Uncertain()
		t.Close()
		if err != nil {
			return err
		}
		if uncertain {
			unresolved = fmt.Errorf("thread %s has an unresolved automatic submission", header.Scope.Thread)
		}
	}
	return unresolved
}
