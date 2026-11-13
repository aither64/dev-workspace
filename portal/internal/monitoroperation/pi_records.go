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
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// These are domain-record reference projections, not native receipts or a
// registry. Lower package placement preserves the existing import direction.
type PiSessionRecord struct {
	Workspace   string `json:"workspace"`
	Slug        string `json:"slug"`
	Incarnation string `json:"incarnation"`
}

func (s PiSessionRecord) Ref() agentbackend.SessionRef {
	return agentbackend.SessionRef{Workspace: s.Workspace, Slug: s.Slug, Incarnation: s.Incarnation}
}
func (s PiSessionRecord) Validate() error {
	if !canonicalPath(s.Workspace) || !session.ValidSlug(s.Slug) || !piUtilityHex.MatchString(s.Incarnation) {
		return piruntime.ErrScope
	}
	return nil
}

type PiConversationRecord struct {
	Session     PiSessionRecord `json:"session"`
	Backend     string          `json:"backend"`
	Member      string          `json:"member"`
	Incarnation string          `json:"incarnation"`
	NativeID    string          `json:"nativeId"`
}

func (c PiConversationRecord) Ref() agentbackend.ConversationRef {
	return agentbackend.ConversationRef{Session: c.Session.Ref(), Backend: agentbackend.Kind(c.Backend), Member: c.Member, Incarnation: c.Incarnation, NativeID: c.NativeID}
}
func (c PiConversationRecord) Validate() error {
	if c.Session.Validate() != nil || c.Backend != "pi" || !piUtilityHex.MatchString(c.Incarnation) || !piRecordText(c.Member, 64) || !piRecordText(c.NativeID, 4096) {
		return piruntime.ErrScope
	}
	return nil
}

type PiInputRecord struct {
	Conversation PiConversationRecord `json:"conversation"`
	OperationID  string               `json:"operationId"`
	Action       string               `json:"action"`
	Digest       string               `json:"digest"`
}

func (r PiInputRecord) Ref() piruntime.SubmissionRef {
	return piruntime.SubmissionRef{Conversation: r.Conversation.Ref(), OperationID: r.OperationID, Action: r.Action, Digest: r.Digest}
}
func (r PiInputRecord) Validate() error {
	if r.Conversation.Validate() != nil || !validID(r.OperationID) || !piRecordText(r.Action, 4096) || !piUtilityHex.MatchString(r.Digest) {
		return piruntime.ErrScope
	}
	return nil
}

type PiSettingsRecord struct {
	Provider       string `json:"provider"`
	AccountRef     string `json:"accountRef"`
	Model          string `json:"model"`
	Thinking       string `json:"thinking"`
	PolicyRevision string `json:"policyRevision"`
	Revision       uint64 `json:"revision"`
}

func (s PiSettingsRecord) Runtime() piruntime.EffectiveSettings {
	return piruntime.EffectiveSettings{Provider: s.Provider, AccountRef: s.AccountRef, Model: s.Model, Thinking: s.Thinking, PolicyRevision: s.PolicyRevision, Revision: s.Revision}
}
func (s PiSettingsRecord) Validate() error {
	if s.Revision == 0 || !piUtilityHex.MatchString(s.PolicyRevision) || !piRecordText(s.Provider, 4096) || !piRecordText(s.AccountRef, 4096) || !piRecordText(s.Model, 4096) || !piRecordText(s.Thinking, 4096) {
		return piruntime.ErrScope
	}
	return nil
}

type PiDeliveryRecord struct {
	Settings   PiSettingsRecord `json:"settings"`
	Input      PiInputRecord    `json:"input"`
	Outcome    string           `json:"outcome,omitempty"`
	Generation string           `json:"generation,omitempty"`
	Detail     string           `json:"detail,omitempty"`
}

func (d PiDeliveryRecord) Validate() error {
	if d.Settings.Validate() != nil || d.Input.Validate() != nil || !bounded(d.Detail, 4096) ||
		(d.Outcome != "" && d.Outcome != "accepted" && d.Outcome != "not_submitted" && d.Outcome != "unknown") ||
		(d.Generation != "" && !piUtilityHex.MatchString(d.Generation)) {
		return piruntime.ErrScope
	}
	return nil
}

func ValidatePiDeliveryTransition(old, next *PiDeliveryRecord) error {
	if old == nil {
		return nil
	}
	if next == nil || old.Settings != next.Settings || old.Input != next.Input || old.Generation != "" && old.Generation != next.Generation {
		return piruntime.ErrConflict
	}
	if (old.Outcome == "accepted" || old.Outcome == "not_submitted") && old.Outcome != next.Outcome {
		return piruntime.ErrConflict
	}
	return nil
}
func piRecordText(s string, n int) bool { return bounded(s, n) && strings.TrimSpace(s) != "" }

// PiTeamWriteAdmission is supplied by central composition only after the full
// capability/preflight/old-reader/retained-creation-fence proof is wired. It
// holds transition/runtime admission and rechecks selection after waits before
// invoking this callback. No domain lock may be held on entry. Nil refuses.
type PiTeamWriteAdmission interface {
	WithPiTeamWrite(context.Context, agentbackend.SessionRef, string, func() error) error
}

func WithPiRecordWrite(ctx context.Context, admission PiTeamWriteAdmission, scope PiSessionRecord, generation string, write func() error) error {
	if admission == nil {
		return piruntime.ErrUnavailable
	}
	if scope.Validate() != nil || !piUtilityHex.MatchString(generation) || write == nil {
		return piruntime.ErrScope
	}
	called := false
	var callbackErr error
	err := admission.WithPiTeamWrite(ctx, scope.Ref(), generation, func() error {
		if called {
			callbackErr = piruntime.ErrConflict
			return callbackErr
		}
		called = true
		if err := ctx.Err(); err != nil {
			return err
		}
		callbackErr = write()
		return callbackErr
	})
	if callbackErr != nil {
		return callbackErr
	}
	if err == nil && !called {
		return piruntime.ErrUnavailable
	}
	return err
}

// DecodePiRecord is shared by the existing domain stores, with no filesystem
// or authority side effects. Nulls, duplicate keys and extra JSON are refused.
func DecodePiRecord(data []byte, out any, limit int) error {
	return decodeDomainRecord(data, out, limit, true)
}

// DecodeMixedPiRecord permits legacy payload nulls. Domain identity arms still
// validate their own exact non-null shapes; existing Codex option values survive.
func DecodeMixedPiRecord(data []byte, out any, limit int) error {
	return decodeDomainRecord(data, out, limit, false)
}
func decodeDomainRecord(data []byte, out any, limit int, rejectNull bool) error {
	if len(data) > limit || !utf8.Valid(data) {
		return errors.New("invalid Pi record size or UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 32 {
			return errors.New("Pi record nesting exceeds bound")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		if t == nil && rejectNull {
			return errors.New("null Pi record field")
		}
		if delim, ok := t.(json.Delim); ok {
			seen := map[string]bool{}
			for d.More() {
				if delim == '{' {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return errors.New("duplicate Pi record key")
					}
					seen[name] = true
				}
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := scan(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing Pi record data")
	}
	if err := piRequiredFields(data, reflect.TypeOf(out), rejectNull); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func piRequiredFields(data []byte, t reflect.Type, strict bool) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if !strict && bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	if t == reflect.TypeOf(time.Time{}) || strings.Contains(t.PkgPath(), "/codex") || reflect.PointerTo(t).Implements(reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()) {
		return nil
	}
	if t.Kind() == reflect.Struct {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return err
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tag := field.Tag.Get("json")
			parts := strings.Split(tag, ",")
			name := parts[0]
			if name == "-" || field.PkgPath != "" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			value, present := object[name]
			optional := false
			for _, option := range parts[1:] {
				optional = optional || option == "omitempty"
			}
			if !present {
				if !optional {
					return errors.New("missing Pi record field: " + name)
				}
				continue
			}
			if name == "generation" || name == "initialInputId" {
				var identity string
				if json.Unmarshal(value, &identity) == nil && identity == "" {
					return errors.New("empty Pi record identity: " + name)
				}
			}
			if err := piRequiredFields(value, field.Type, strict); err != nil {
				return err
			}
		}
	} else if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := piRequiredFields(value, t.Elem(), strict); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadPiRecord and WritePiRecord preserve existing private atomic publication;
// callers own the domain lock and writer admission. Neither creates a registry.
func ReadPiRecord(path string, out any, limit int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() > int64(limit) {
		return errors.New("invalid private Pi record")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return err
	}
	return DecodePiRecord(data, out, limit)
}
func WritePiRecord(path string, value any, limit int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > limit {
		return errors.New("Pi record exceeds reserved capacity")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pi-record-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func LockPiRecord(ctx context.Context, path string) (*os.File, error) { return lockFile(ctx, path) }
