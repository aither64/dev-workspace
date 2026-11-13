package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// PiCreationSelection is an immutable projection of public P4 types. It carries
// no catalog account objects, auth paths or credentials. The exact catalog must
// be available again to validate recovery; current defaults cannot replace it.
type PiCreationSelection struct {
	Schema        int                         `json:"schema"`
	CatalogDigest string                      `json:"catalog_digest"`
	Policy        PiCreationPolicy            `json:"policy"`
	Members       []PiCreationMemberSelection `json:"members"`
}
type PiCreationSession struct {
	Workspace   string `json:"workspace"`
	Slug        string `json:"slug"`
	Incarnation string `json:"incarnation"`
}
type PiCreationPolicy struct {
	Session   PiCreationSession              `json:"session"`
	Revision  uint64                         `json:"revision"`
	Default   piproviders.VersionedSelection `json:"default"`
	Overrides []piproviders.Override         `json:"overrides"`
}
type PiCreationSettings struct {
	Provider       string `json:"provider"`
	AccountRef     string `json:"account_ref"`
	Model          string `json:"model"`
	Thinking       string `json:"thinking"`
	PolicyRevision string `json:"policy_revision"`
	Revision       uint64 `json:"revision"`
}

func (s PiCreationSettings) Effective() piruntime.EffectiveSettings {
	return piruntime.EffectiveSettings{Provider: s.Provider, AccountRef: s.AccountRef, Model: s.Model, Thinking: s.Thinking, PolicyRevision: s.PolicyRevision, Revision: s.Revision}
}
func creationSettings(s piruntime.EffectiveSettings) PiCreationSettings {
	return PiCreationSettings{s.Provider, s.AccountRef, s.Model, s.Thinking, s.PolicyRevision, s.Revision}
}

type PiCreationMemberSelection struct {
	Address      string             `json:"address"`
	Incarnation  string             `json:"incarnation"`
	Source       string             `json:"source"`
	PolicyDigest string             `json:"policy_digest"`
	Settings     PiCreationSettings `json:"settings"`
}
type PiReservedConversation struct {
	Address     string `json:"address"`
	Incarnation string `json:"incarnation"`
}

func (p PiCreationPolicy) Public() piproviders.Policy {
	return piproviders.Policy{Session: agentbackend.SessionRef{Workspace: p.Session.Workspace, Slug: p.Session.Slug, Incarnation: p.Session.Incarnation}, Revision: p.Revision, Default: p.Default, Overrides: p.Overrides}
}
func FreezePiCreationSelection(c piproviders.Catalog, p piproviders.Policy, conversations []PiReservedConversation) (PiCreationSelection, error) {
	out := PiCreationSelection{Schema: 1, CatalogDigest: c.Digest, Policy: PiCreationPolicy{Session: PiCreationSession{p.Session.Workspace, p.Session.Slug, p.Session.Incarnation}, Revision: p.Revision, Default: p.Default, Overrides: p.Overrides}, Members: []PiCreationMemberSelection{}}
	for _, member := range conversations {
		resolved, err := piproviders.Resolve(c, p, piproviders.MemberKey{Address: member.Address, Incarnation: member.Incarnation}, 1)
		if err != nil {
			return PiCreationSelection{}, err
		}
		out.Members = append(out.Members, PiCreationMemberSelection{member.Address, member.Incarnation, resolved.Source, resolved.PolicyDigest, creationSettings(resolved.Settings)})
	}
	if err := out.Validate(c, p.Session, conversations); err != nil {
		return PiCreationSelection{}, err
	}
	// Detach slices from mutable caller policy/catalog objects before publication.
	data, err := json.Marshal(out)
	if err != nil {
		return PiCreationSelection{}, err
	}
	var frozen PiCreationSelection
	if err = DecodePiCreationJSON(data, &frozen, 64<<10); err != nil {
		return PiCreationSelection{}, err
	}
	return frozen, nil
}
func (s PiCreationSelection) Validate(c piproviders.Catalog, ref agentbackend.SessionRef, conversations []PiReservedConversation) error {
	if s.Schema != 1 || !isLowerHex(s.CatalogDigest, 64) || s.CatalogDigest != c.Digest || s.Policy.Public().Session != ref ||
		s.Policy.Revision != 1 || s.Policy.Default.Revision != 1 || s.Policy.Overrides == nil || len(s.Members) != len(conversations) || len(conversations) == 0 {
		return errors.New("Pi creation provider selection is unavailable or conflicting")
	}
	if err := piproviders.ValidatePolicy(c, s.Policy.Public()); err != nil {
		return err
	}
	reserved := map[piproviders.MemberKey]bool{}
	incarnations := map[string]bool{}
	for i, member := range conversations {
		key := piproviders.MemberKey{Address: member.Address, Incarnation: member.Incarnation}
		if !backendMember(member.Address) || !isLowerHex(member.Incarnation, 64) || reserved[key] || incarnations[member.Incarnation] {
			return errors.New("invalid reserved Pi member")
		}
		for previous := range reserved {
			if previous.Address == member.Address {
				return errors.New("duplicate reserved Pi member")
			}
		}
		reserved[key] = true
		incarnations[member.Incarnation] = true
		resolved, err := piproviders.Resolve(c, s.Policy.Public(), key, 1)
		if err != nil {
			return err
		}
		saved := s.Members[i]
		if saved.Address != member.Address || saved.Incarnation != member.Incarnation || saved.Source != resolved.Source || saved.PolicyDigest != resolved.PolicyDigest || saved.Settings.Effective() != resolved.Settings {
			return errors.New("frozen Pi settings disagree with provider policy")
		}
	}
	if conversations[0].Address != "lead" {
		return errors.New("Pi creation requires its reserved lead first")
	}
	for _, override := range s.Policy.Overrides {
		if !reserved[override.Member] || override.Value.Revision != 1 {
			return errors.New("Pi creation override has no reserved incarnation")
		}
	}
	return nil
}

// DecodePiCreationJSON enforces the exact typed wire shape recursively, including
// required false/zero values and empty arrays. encoding/json alone accepts null,
// absent zero fields and case-insensitive names, all ambiguous in durable proof.
func DecodePiCreationJSON(data []byte, target any, maximum int) error {
	if len(data) == 0 || len(data) > maximum || !utf8.Valid(data) {
		return errors.New("invalid Pi creation record size or encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkBackendJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing Pi creation JSON")
	}
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer {
		return errors.New("Pi creation decoder requires a pointer")
	}
	if err := piCreationShape(data, typ.Elem()); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
func piCreationShape(data json.RawMessage, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil || object == nil {
			return errors.New("Pi creation field must be an object")
		}
		fields := map[string]reflect.StructField{}
		var collect func(reflect.Type)
		collect = func(t reflect.Type) {
			for n := 0; n < t.NumField(); n++ {
				field := t.Field(n)
				tag := field.Tag.Get("json")
				if tag == "-" || field.PkgPath != "" {
					continue
				}
				if field.Anonymous && tag == "" {
					collect(field.Type)
					continue
				}
				name := strings.Split(tag, ",")[0]
				if name == "" {
					name = field.Name
				}
				fields[name] = field
			}
		}
		collect(typ)
		for name, field := range fields {
			if _, present := object[name]; !present && !strings.Contains(field.Tag.Get("json"), ",omitempty") {
				return fmt.Errorf("Pi creation record lacks %s", name)
			}
		}
		for key, value := range object {
			field, present := fields[key]
			if !present {
				return fmt.Errorf("unknown Pi creation field %s", key)
			}
			if err := piCreationShape(value, field.Type); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if json.Unmarshal(data, &values) != nil || values == nil {
			return errors.New("Pi creation field must be an explicit array")
		}
		for _, value := range values {
			if err := piCreationShape(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
