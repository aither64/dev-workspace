package turnretry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const PolicySchema = 1
const MaxPolicyBytes = 1024 * 1024
const MaxPolicyThreads = 128
const maxJSONInteger = int64(1<<53 - 1)

var ErrPolicyUnavailable = errors.New("automatic retry policy is unavailable")

// PolicyScope identifies retained tracking, independently of a loaded native
// generation. Callers obtain Incarnation from session.ObservationIdentity.
type PolicyScope struct {
	Workspace   string `json:"workspace"`
	Slug        string `json:"slug"`
	Root        string `json:"root"`
	Incarnation string `json:"incarnation"`
}

type Enrollment struct {
	Turn   string `json:"turn"`
	Active bool   `json:"active"`
}

// Suppression keeps cancellation authority when a preference changes before
// the per-thread ledger can be settled. Never discard an unresolved Pending.
type Suppression struct {
	Chain   string `json:"chain"`
	Pending string `json:"pending"`
	Result  string `json:"result"`
}

// Summary is an optional checkpoint, never submission or cancellation proof.
type Summary struct {
	Chain           string `json:"chain"`
	Class           string `json:"class"`
	Phase           string `json:"phase"`
	Outcome         string `json:"outcome"`
	Reason          string `json:"reason"`
	ConfirmedStarts *int64 `json:"confirmedStarts"`
	CountQuality    string `json:"countQuality"`
	StartedAtMS     *int64 `json:"startedAtMs"`
	FinishedAtMS    *int64 `json:"finishedAtMs"`
	TimingQuality   string `json:"timingQuality"`
	LastConfirmed   string `json:"lastConfirmed"`
	PendingID       string `json:"pendingId"`
}

type ThreadPolicy struct {
	Thread        string        `json:"thread"`
	Capacity      Enrollment    `json:"capacity"`
	Cybersecurity Enrollment    `json:"cybersecurity"`
	Suppressed    []Suppression `json:"suppressed"`
	Summary       *Summary      `json:"summary"`
}

type Policy struct {
	Schema        int            `json:"schema"`
	Scope         PolicyScope    `json:"scope"`
	Revision      string         `json:"revision"`
	Capacity      bool           `json:"capacity"`
	Cybersecurity bool           `json:"cybersecurity"`
	Threads       []ThreadPolicy `json:"threads"`
}

type PolicyStore struct{ Root, Workspace string }

func (s PolicyStore) directory() string {
	return filepath.Join(s.Root, "turn-retry-policy", hash(s.Workspace))
}

func (s PolicyStore) path(scope PolicyScope) string {
	return filepath.Join(s.directory(), scope.Slug+".json")
}

func (s PolicyStore) validScope(scope PolicyScope) bool {
	return filepath.IsAbs(s.Root) && filepath.IsAbs(s.Workspace) && scope.Workspace == s.Workspace &&
		boundedIdentity(scope.Slug) && scope.Slug != "." && scope.Slug != ".." && !strings.ContainsAny(scope.Slug, "/\\") &&
		boundedIdentity(scope.Root) && boundedIdentity(scope.Incarnation)
}

func boundedIdentity(value string) bool { return value != "" && len(value) <= 4096 }

func (p *Policy) Thread(thread string) *ThreadPolicy {
	for i := range p.Threads {
		if p.Threads[i].Thread == thread {
			return &p.Threads[i]
		}
	}
	return nil
}

func (s PolicyStore) Load(scope PolicyScope) (Policy, error) {
	if !s.validScope(scope) {
		return Policy{}, ErrPolicyUnavailable
	}
	file, err := os.Open(s.path(scope))
	if err != nil {
		return Policy{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	var owner unix.Stat_t
	if err != nil || unix.Fstat(int(file.Fd()), &owner) != nil || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0600 || owner.Uid != uint32(os.Geteuid()) || info.Size() > MaxPolicyBytes {
		return Policy{}, ErrPolicyUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxPolicyBytes+1))
	if err != nil || len(data) > MaxPolicyBytes {
		return Policy{}, ErrPolicyUnavailable
	}
	var syntax yaml.Node
	if yaml.Unmarshal(data, &syntax) != nil || uniqueKeys(&syntax) != nil {
		return Policy{}, ErrPolicyUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy Policy
	if decoder.Decode(&policy) != nil || decoder.Decode(new(any)) != io.EOF {
		return Policy{}, ErrPolicyUnavailable
	}
	// Missing booleans are not an explicit saved opt-out.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || string(fields["capacity"]) != "true" && string(fields["capacity"]) != "false" ||
		string(fields["cybersecurity"]) != "true" && string(fields["cybersecurity"]) != "false" {
		return Policy{}, ErrPolicyUnavailable
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(fields["threads"], &entries) != nil {
		return Policy{}, ErrPolicyUnavailable
	}
	for _, entry := range entries {
		for _, class := range []string{"capacity", "cybersecurity"} {
			var baseline map[string]json.RawMessage
			if json.Unmarshal(entry[class], &baseline) != nil || baseline["turn"] == nil || string(baseline["turn"]) == "null" || string(baseline["active"]) != "true" && string(baseline["active"]) != "false" {
				return Policy{}, ErrPolicyUnavailable
			}
		}
	}
	if err := validatePolicy(scope, policy); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func validatePolicy(scope PolicyScope, p Policy) error {
	if p.Schema != PolicySchema || p.Scope != scope || !attemptIDPattern.MatchString(p.Revision) || p.Threads == nil || len(p.Threads) > MaxPolicyThreads {
		return ErrPolicyUnavailable
	}
	seen := map[string]bool{}
	for _, thread := range p.Threads {
		if !boundedIdentity(thread.Thread) || seen[thread.Thread] || thread.Suppressed == nil || len(thread.Suppressed) > 2 ||
			len(thread.Capacity.Turn) > 4096 || len(thread.Cybersecurity.Turn) > 4096 ||
			thread.Capacity.Active && thread.Capacity.Turn == "" || thread.Cybersecurity.Active && thread.Cybersecurity.Turn == "" {
			return ErrPolicyUnavailable
		}
		seen[thread.Thread] = true
		chains := map[string]bool{}
		for _, stop := range thread.Suppressed {
			if !boundedIdentity(stop.Chain) || chains[stop.Chain] || len(stop.Result) > 4096 || stop.Pending != "" && !attemptIDPattern.MatchString(stop.Pending) {
				return ErrPolicyUnavailable
			}
			chains[stop.Chain] = true
		}
		if v := thread.Summary; v != nil {
			if !boundedIdentity(v.Chain) || v.Class != "capacity" && v.Class != "cybersecurity" || len(v.Reason) > 128 ||
				v.LastConfirmed != "" && !attemptIDPattern.MatchString(v.LastConfirmed) || v.PendingID != "" && !attemptIDPattern.MatchString(v.PendingID) {
				return ErrPolicyUnavailable
			}
			if !oneOf(v.Phase, "scheduled", "submitting", "reconciling", "running", "terminal") ||
				!oneOf(v.CountQuality, "exact", "partial", "unavailable") || !oneOf(v.TimingQuality, "exact", "partial", "unavailable") ||
				!oneOf(v.Outcome, "", "success", "stopped", "exhausted", "superseded", "blocked") {
				return ErrPolicyUnavailable
			}
			for _, n := range []*int64{v.ConfirmedStarts, v.StartedAtMS, v.FinishedAtMS} {
				if n != nil && (*n < 0 || *n > maxJSONInteger) {
					return ErrPolicyUnavailable
				}
			}
			if (v.CountQuality == "unavailable") != (v.ConfirmedStarts == nil) || (v.TimingQuality == "unavailable") != (v.StartedAtMS == nil) ||
				v.FinishedAtMS != nil && v.StartedAtMS != nil && *v.FinishedAtMS < *v.StartedAtMS {
				return ErrPolicyUnavailable
			}
		}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// Update is a short cross-process RMW checkpoint. The callback must not make
// native requests or acquire admission/outer locks. Create is reserved for a
// complete caller-owned runtime-EX bootstrap, never polling or manual delivery.
func (s PolicyStore) Update(ctx context.Context, scope PolicyScope, create bool, update func(*Policy) error) (Policy, error) {
	if !s.validScope(scope) {
		return Policy{}, ErrPolicyUnavailable
	}
	for _, directory := range []string{filepath.Join(s.Root, "turn-retry-policy"), s.directory()} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return Policy{}, err
		}
		info, err := os.Stat(directory)
		var owner unix.Stat_t
		if err != nil || unix.Stat(directory, &owner) != nil || !info.IsDir() || info.Mode().Perm() != 0700 || owner.Uid != uint32(os.Geteuid()) {
			return Policy{}, ErrPolicyUnavailable
		}
	}
	lock, err := os.OpenFile(s.path(scope)+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Policy{}, err
	}
	defer lock.Close()
	info, statErr := lock.Stat()
	var owner unix.Stat_t
	if statErr != nil || unix.Fstat(int(lock.Fd()), &owner) != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || owner.Uid != uint32(os.Geteuid()) {
		return Policy{}, ErrPolicyUnavailable
	}
	for {
		if err := ctx.Err(); err != nil {
			return Policy{}, err
		}
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return Policy{}, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Policy{}, ctx.Err()
		case <-timer.C:
		}
	}
	p, err := s.Load(scope)
	existed := err == nil
	if errors.Is(err, os.ErrNotExist) && create {
		revision, idErr := NewID()
		if idErr != nil {
			return Policy{}, idErr
		}
		p, err = Policy{Schema: PolicySchema, Scope: scope, Revision: revision, Capacity: true, Cybersecurity: true, Threads: []ThreadPolicy{}}, nil
	}
	if err != nil {
		return Policy{}, err
	}
	before, err := json.Marshal(p)
	if err != nil {
		return Policy{}, err
	}
	if err := update(&p); err != nil {
		return Policy{}, err
	}
	if err := validatePolicy(scope, p); err != nil {
		return Policy{}, err
	}
	data, err := json.Marshal(p)
	if err != nil || len(data)+1 > MaxPolicyBytes {
		return Policy{}, fmt.Errorf("%w: size bound", ErrPolicyUnavailable)
	}
	if existed && bytes.Equal(before, data) {
		return p, nil
	}
	if err := ctx.Err(); err != nil {
		return Policy{}, err
	}
	if err := replacePolicy(s.path(scope), append(data, '\n')); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func replacePolicy(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".policy-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
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

// EqualChoices deliberately excludes checkpoints and enrollment. A lost-reply
// same-valued POST is a no-op even when its expected revision became stale.
func (p Policy) EqualChoices(capacity, cybersecurity bool) bool {
	return p.Capacity == capacity && p.Cybersecurity == cybersecurity
}
