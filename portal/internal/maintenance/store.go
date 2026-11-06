package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

var errMissingActivePointer = errors.New("nonterminal maintenance record has no active pointer")

var createDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var phases = []string{"prepared", "pausing", "draining-writers", "stopping-native", "activating", "resuming", "complete", "cancelled", "recovery-required"}

type Store struct{ Root, Profile string }

func (s Store) directory() string     { return filepath.Join(s.Root, "maintenance") }
func (s Store) Path(id string) string { return filepath.Join(s.directory(), id, "operation.json") }

func decode(data []byte, value any) error {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	var unique func(*yaml.Node) error
	unique = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode {
			return errors.New("maintenance state has aliases")
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i].Value
				if seen[key] {
					return errors.New("maintenance state has duplicate keys")
				}
				seen[key] = true
			}
		}
		for _, c := range n.Content {
			if err := unique(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := unique(&node); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("maintenance state has trailing data")
	}
	return nil
}

func validate(o Operation, profile string) error {
	if o.Schema != Schema || !uuid.MatchString(o.OperationID) || o.Revision == 0 || !slices.Contains(phases, o.Phase) ||
		o.Profile != profile || !filepath.IsAbs(profile) || !filepath.IsAbs(o.SourcePackage) || !filepath.IsAbs(o.CandidatePackage) ||
		o.SourceProfileToken == "" || o.PlansDigest == "" || o.Services == nil || o.Participants == nil || len(o.Participants) > 4096 || len(o.Services) > 128 {
		return errors.New("invalid maintenance operation identity or phase")
	}
	if !slices.Contains([]string{"", "updated", "source-recovered"}, o.Outcome) || o.SourceRecovered && !o.CutoverIntent || o.ActivationIntent && (!o.CutoverIntent || !filepath.IsAbs(o.ActivationPackage) || !filepath.IsAbs(o.ActivationNative) || o.ActivationPlansDigest == "") {
		return errors.New("invalid maintenance activation identity")
	}
	if r := o.Restoration; r != nil {
		if !slices.Contains([]string{"updated", "cancelled", "source-recovered"}, r.Outcome) || !filepath.IsAbs(r.Package) || r.ProfileToken == "" || r.Portals == nil || r.Terminals == nil || r.Completed != o.Terminal() || !slices.Equal(r.Portals, o.DrainedPortals) || !slices.Equal(r.Terminals, o.TerminalIntents) {
			return errors.New("invalid maintenance restoration identity")
		}
		if r.Outcome == "cancelled" {
			if o.CutoverIntent || !o.CancelRequested || r.Package != o.SourcePackage || r.ProfileToken != o.SourceProfileToken {
				return errors.New("invalid cancel restoration")
			}
		} else if !o.ContinuationsSettled || !o.ActivationIntent || r.Package != o.ActivationPackage || o.SourceRecovered != (r.Outcome == "source-recovered") {
			return errors.New("invalid final restoration")
		}
	}
	if len(o.Children) > 4096 {
		return errors.New("too many owned children")
	}
	seenChildren := map[string]bool{}
	for _, child := range o.Children {
		key := child.Workspace + ":" + child.Thread
		if child.Parent == "" || child.Thread == "" || child.Parent == child.Thread || !filepath.IsAbs(child.Workspace) || seenChildren[key] {
			return errors.New("invalid child identity")
		}
		seenChildren[key] = true
	}
	seen := map[string]bool{}
	services := map[string]string{}
	for _, service := range o.Services {
		if !filepath.IsAbs(service.Workspace) || !filepath.IsAbs(service.Socket) || !filepath.IsAbs(service.Native) || service.Unit == "" || service.PID <= 0 || service.ProcessIdentity == "" || service.SocketEpoch == "" || services[service.Workspace] != "" {
			return errors.New("invalid maintenance service identity")
		}
		if service.DestinationEpoch != "" && (service.DestinationPID <= 0 || service.DestinationProcessIdentity == "" || !filepath.IsAbs(service.DestinationNative)) {
			return errors.New("invalid destination identity")
		}
		services[service.Workspace] = service.SocketEpoch
	}
	if len(o.TerminalIntents) > 4096 {
		return errors.New("too many maintenance terminal identities")
	}
	for _, intent := range o.TerminalIntents {
		if services[intent.Workspace] == "" || intent.Slug == "" || filepath.Base(intent.Slug) != intent.Slug || intent.Root == "" || intent.Incarnation == "" {
			return errors.New("invalid maintenance terminal identity")
		}
	}
	for _, p := range o.Participants {
		key := p.Workspace + ":" + p.Thread
		if seen[key] || p.Slug == "" || p.Incarnation == "" || p.Root == "" || p.Thread == "" || p.SourceTurn == "" || p.Cwd != filepath.Join(p.Workspace, "work", p.Slug) || services[p.Workspace] != p.SocketEpoch || !uuid.MatchString(p.NoticeID) || !uuid.MatchString(p.ContinuationID) || p.NoticeText == "" || p.NoticeAction == "" || !slices.Contains([]string{"prepared", "accepted", "unknown", "refused", "skipped"}, p.NoticeDisposition) || !slices.Contains([]string{"pending", "submitting", "accepted", "unknown", "suppressed"}, p.ContinuationDisposition) {
			return errors.New("invalid maintenance participant identity")
		}
		seen[key] = true
	}
	return nil
}

func privateRead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, linkErr := os.Lstat(path); linkErr == nil {
				return nil, errors.New("maintenance record exists but is unavailable")
			}
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) || info.Size() > MaxBytes {
		return nil, errors.New("maintenance record is not private and bounded")
	}
	return io.ReadAll(io.LimitReader(f, MaxBytes+1))
}

func (s Store) Load(id string) (Operation, error) {
	if !uuid.MatchString(id) || !filepath.IsAbs(s.Root) {
		return Operation{}, errors.New("invalid maintenance store identity")
	}
	data, err := privateRead(s.Path(id))
	if err != nil {
		return Operation{}, err
	}
	var o Operation
	if err = decode(data, &o); err != nil {
		return o, err
	}
	if o.OperationID != id {
		return o, errors.New("maintenance operation ID changed")
	}
	return o, validate(o, s.Profile)
}

func atomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > MaxBytes {
		return errors.New("maintenance state exceeds bound")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".maintenance-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s Store) locked(ctx context.Context, fn func() error) error {
	if !filepath.IsAbs(s.Root) || !filepath.IsAbs(s.Profile) {
		return errors.New("invalid maintenance store root")
	}
	if err := os.MkdirAll(s.directory(), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.directory(), "operation.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fn()
}

func (s Store) Active() (Operation, error) {
	data, err := privateRead(filepath.Join(s.directory(), "active.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			paths, globErr := filepath.Glob(filepath.Join(s.directory(), "*", "operation.json"))
			if globErr != nil {
				return Operation{}, globErr
			}
			for _, path := range paths {
				o, loadErr := s.loadStatus(filepath.Base(filepath.Dir(path)))
				if loadErr != nil {
					return Operation{}, loadErr
				}
				if !o.Terminal() {
					return Operation{}, errMissingActivePointer
				}
			}
		}
		return Operation{}, err
	}
	var pointer struct {
		Schema      int    `json:"schema"`
		OperationID string `json:"operationId"`
		Profile     string `json:"profile"`
	}
	if err = decode(data, &pointer); err != nil {
		return Operation{}, err
	}
	if (pointer.Schema != Schema && pointer.Schema != 1) || pointer.Profile != s.Profile {
		return Operation{}, errors.New("maintenance pointer identity changed")
	}
	o, e := s.loadStatus(pointer.OperationID)
	if errors.Is(e, os.ErrNotExist) {
		if e == nil && o.Schema != Schema {
			after, err := privateRead(filepath.Join(s.directory(), "active.json"))
			if err != nil || !bytes.Equal(data, after) {
				return Operation{}, errors.New("maintenance active owner changed during status")
			}
		}
		return o, errors.New("maintenance active pointer has no operation")
	}
	if e == nil && o.Schema != Schema {
		after, err := privateRead(filepath.Join(s.directory(), "active.json"))
		if err != nil || !bytes.Equal(data, after) {
			return Operation{}, errors.New("maintenance active owner changed during status")
		}
	}
	return o, e
}

func (s Store) Create(ctx context.Context, o Operation) error {
	return s.locked(ctx, func() error {
		if err := validate(o, s.Profile); err != nil {
			return err
		}
		o.CreateDigest = ""
		encoded, err := json.Marshal(o)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(encoded)
		o.CreateDigest = hex.EncodeToString(sum[:])
		if existing, err := s.Load(o.OperationID); err == nil {
			if existing.CreateDigest != o.CreateDigest {
				return errors.New("maintenance request identity changed")
			}
			if active, err := s.Active(); err == nil {
				if active.OperationID != o.OperationID {
					return errors.New("maintenance active pointer changed")
				}
				return nil
			} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errMissingActivePointer) {
				return err
			}
			return atomic(filepath.Join(s.directory(), "active.json"), map[string]any{"schema": Schema, "operationId": o.OperationID, "profile": s.Profile})
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if active, err := s.Active(); err == nil && !active.Terminal() {
			return errors.New("another maintenance operation owns the profile")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.Path(o.OperationID)), 0700); err != nil {
			return err
		}
		if err := atomic(s.Path(o.OperationID), o); err != nil {
			return err
		}
		return atomic(filepath.Join(s.directory(), "active.json"), map[string]any{"schema": Schema, "operationId": o.OperationID, "profile": s.Profile})
	})
}

func (s Store) Update(ctx context.Context, id string, revision uint64, change func(*Operation) error) (Operation, error) {
	var result Operation
	err := s.locked(ctx, func() error {
		o, err := s.Load(id)
		if err != nil {
			return err
		}
		if revision != 0 && revision != o.Revision {
			return errors.New("maintenance revision changed")
		}
		if err = change(&o); err != nil {
			return err
		}
		o.Revision++
		if err = validate(o, s.Profile); err != nil {
			return err
		}
		if err = atomic(s.Path(id), o); err != nil {
			return err
		}
		result = o
		return nil
	})
	return result, err
}

// RecoverPointer completes only initial publication of the exact retained
// operation. An orphan remains a fence until the shipped recovery path performs
// this transaction; neither a foreign owner nor ambiguous records are replaced.
func (s Store) RecoverPointer(ctx context.Context, id string) error {
	return s.locked(ctx, func() error {
		target, err := s.Load(id)
		if err != nil {
			return err
		}
		if !createDigestPattern.MatchString(target.CreateDigest) {
			return errors.New("maintenance creation identity is missing or malformed")
		}
		paths, err := filepath.Glob(filepath.Join(s.directory(), "*", "operation.json"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			other, loadErr := s.loadStatus(filepath.Base(filepath.Dir(path)))
			if loadErr != nil {
				return loadErr
			}
			if other.OperationID != id && !other.Terminal() {
				return errors.New("another maintenance operation owns the profile")
			}
		}
		active, err := s.Active()
		if err == nil {
			if active.OperationID == id {
				return nil
			}
			if !active.Terminal() {
				return errors.New("another maintenance operation owns the profile")
			}
			if target.Terminal() {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errMissingActivePointer) {
			return err
		}
		return atomic(filepath.Join(s.directory(), "active.json"), map[string]any{"schema": Schema, "operationId": id, "profile": s.Profile})
	})
}

// Identity is a bounded routing envelope, never permission to interpret another
// owner's private protocol. Both historical schema1 and current schema2 route
// before semantic decoding; no record is converted.
type Identity struct {
	Schema        int    `json:"schema"`
	OperationID   string `json:"operationId"`
	Profile       string `json:"profile"`
	SourcePackage string `json:"sourcePackage"`
}

func (s Store) Identity(id string) (Identity, error) {
	if id == "" {
		data, err := privateRead(filepath.Join(s.directory(), "active.json"))
		if err != nil {
			return Identity{}, err
		}
		var p struct {
			Schema      int    `json:"schema"`
			OperationID string `json:"operationId"`
			Profile     string `json:"profile"`
		}
		if err = decode(data, &p); err != nil {
			return Identity{}, err
		}
		if (p.Schema != 1 && p.Schema != Schema) || p.Profile != s.Profile {
			return Identity{}, errors.New("maintenance pointer identity changed")
		}
		id = p.OperationID
	}
	if !uuid.MatchString(id) {
		return Identity{}, errors.New("invalid operation identity")
	}
	data, err := privateRead(s.Path(id))
	if err != nil {
		return Identity{}, err
	}
	var fields map[string]json.RawMessage
	if err = decode(data, &fields); err != nil {
		return Identity{}, err
	}
	var o Identity
	for key, value := range map[string]any{"schema": &o.Schema, "operationId": &o.OperationID, "profile": &o.Profile, "sourcePackage": &o.SourcePackage} {
		if err = json.Unmarshal(fields[key], value); err != nil {
			return Identity{}, err
		}
	}
	if (o.Schema != 1 && o.Schema != Schema) || o.OperationID != id || o.Profile != s.Profile || !filepath.IsAbs(o.SourcePackage) {
		return Identity{}, errors.New("invalid maintenance owner envelope")
	}
	return o, nil
}

// DecodeEnvelope validates duplicate keys and JSON framing while intentionally
// leaving private fields to the retained owner's semantic decoder.
func DecodeEnvelope(data []byte, value any) error {
	var fields map[string]json.RawMessage
	if err := decode(data, &fields); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
