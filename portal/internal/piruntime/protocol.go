package piruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const protocolVersion = 1
const recordLimit = 1024 * 1024

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var creationPattern = regexp.MustCompile(`^([0-9a-f]{64}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var operationPattern = regexp.MustCompile(`^([0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var hexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var memberPattern = regexp.MustCompile(`^(lead|[a-z][a-z0-9]{0,31}[0-9]+)$`)
var slugPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9][a-z0-9-]*$`)
var storePattern = regexp.MustCompile(`^/nix/store/[0-9a-z]{32}-[^/[:space:]]+$`)

type LaunchScope struct {
	Workspace          string `json:"workspace"`
	Slug               string `json:"slug"`
	SessionIncarnation string `json:"session_incarnation"`
	Backend            string `json:"backend"`
	BackendContract    int    `json:"backend_contract"`
	BindingRevision    uint64 `json:"binding_revision"`
}
type LaunchRequest struct {
	Schema            int         `json:"schema"`
	HostContract      int         `json:"host_launcher_contract"`
	Scope             LaunchScope `json:"scope"`
	InstanceID        string      `json:"instance_id"`
	RequestID         string      `json:"request_id"`
	Generation        string      `json:"generation"`
	AppStorePath      string      `json:"app_store_path"`
	ProfilePath       string      `json:"profile_path"`
	ProfileGeneration string      `json:"profile_generation"`
	ReplacesRequestID string      `json:"replaces_request_id,omitempty"`
}
type HostObservation struct {
	LaunchRequest
	Digest         string         `json:"request_sha256"`
	Phase          string         `json:"phase"`
	Outcome        string         `json:"outcome"`
	Unit           string         `json:"unit"`
	BootID         string         `json:"boot_id,omitempty"`
	InvocationID   string         `json:"invocation_id,omitempty"`
	ControlGroup   string         `json:"control_group,omitempty"`
	MainPID        int            `json:"main_pid,omitempty"`
	PIDStart       string         `json:"main_pid_start,omitempty"`
	MountNamespace string         `json:"mnt_namespace,omitempty"`
	UserNamespace  string         `json:"user_namespace,omitempty"`
	LoadState      string         `json:"load_state,omitempty"`
	ActiveState    string         `json:"active_state,omitempty"`
	SubState       string         `json:"sub_state,omitempty"`
	Job            string         `json:"job,omitempty"`
	ErrorCode      string         `json:"error_code,omitempty"`
	CreatedAt      string         `json:"created_at,omitempty"`
	ObservedAt     string         `json:"observed_at,omitempty"`
	StopRequested  bool           `json:"stop_requested,omitempty"`
	Result         string         `json:"result,omitempty"`
	DrainProof     map[string]any `json:"drain_proof,omitempty"`
}
type Handshake struct {
	HostObservation
	RuntimeConfigSHA256 string `json:"runtime_config_sha256"`
}

// strictDecode rejects duplicate keys as well as unknown fields and trailing
// values. It is used for private records and the frozen H0 byte descriptor.
func strictDecode(data []byte, out any, limit int) error {
	if !utf8.Valid(data) {
		return ErrScope
	}
	if len(data) > limit {
		return fmt.Errorf("Pi record exceeds %d bytes", limit)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := t.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return fmt.Errorf("duplicate/invalid Pi field")
					}
					seen[name] = true
					if err := value(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := value(); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("invalid Pi JSON delimiter")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing Pi JSON")
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	target := reflect.TypeOf(out)
	if target == nil || target.Kind() != reflect.Pointer {
		return ErrScope
	}
	if err := exactJSONFields(parsed, target.Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func DecodeLaunch(data []byte) (LaunchRequest, string, error) {
	var launch LaunchRequest
	if !utf8.Valid(data) {
		return launch, "", ErrScope
	}
	if err := strictDecode(data, &launch, 65536); err != nil {
		return launch, "", err
	}
	// All v1 fields are non-null; a literal null must not become a zero value.
	var rejectNull func(any) bool
	rejectNull = func(v any) bool {
		switch x := v.(type) {
		case nil:
			return true
		case string:
			if len(x) > 4096 {
				return true
			}
			for _, r := range x {
				if unicode.IsControl(r) {
					return true
				}
			}
		case map[string]any:
			for _, item := range x {
				if rejectNull(item) {
					return true
				}
			}
		case []any:
			for _, item := range x {
				if rejectNull(item) {
					return true
				}
			}
		}
		return false
	}
	var raw any
	_ = json.Unmarshal(data, &raw)
	s := launch.Scope
	if rejectNull(raw) || launch.Schema != 1 || launch.HostContract != 1 || s.Backend != "pi" || s.BackendContract != 1 || s.BindingRevision == 0 || !filepath.IsAbs(s.Workspace) || filepath.Clean(s.Workspace) != s.Workspace || !slugPattern.MatchString(s.Slug) || !hexPattern.MatchString(s.SessionIncarnation) || !uuidPattern.MatchString(launch.InstanceID) || !uuidPattern.MatchString(launch.RequestID) || !hexPattern.MatchString(launch.Generation) || !storePattern.MatchString(launch.AppStorePath) || !filepath.IsAbs(launch.ProfilePath) || filepath.Clean(launch.ProfilePath) != launch.ProfilePath || launch.ProfileGeneration == "" || (launch.ReplacesRequestID != "" && !uuidPattern.MatchString(launch.ReplacesRequestID)) {
		return launch, "", ErrScope
	}
	digest := sha256.Sum256(data)
	return launch, hex.EncodeToString(digest[:]), nil
}

func validString(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// encoding/json matches keys without case sensitivity. Private protocol field
// names are exact, so reject aliases before using its scalar/type decoder.
func exactJSONFields(value any, typ reflect.Type) error {
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if typ.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		return exactJSONFields(value, typ.Elem())
	}
	if reflect.PointerTo(typ).Implements(reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return ErrScope
		}
		fields := map[string]reflect.Type{}
		var collect func(reflect.Type)
		collect = func(t reflect.Type) {
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if !field.IsExported() {
					continue
				}
				tag := strings.Split(field.Tag.Get("json"), ",")[0]
				if tag == "-" {
					continue
				}
				if field.Anonymous && tag == "" {
					collect(field.Type)
					continue
				}
				if tag == "" {
					tag = field.Name
				}
				fields[tag] = field.Type
			}
		}
		collect(typ)
		for key, item := range object {
			field, ok := fields[key]
			if !ok {
				return ErrScope
			}
			if err := exactJSONFields(item, field); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if value == nil && typ.Kind() == reflect.Slice {
			return nil
		}
		items, ok := value.([]any)
		if !ok {
			return ErrScope
		}
		for _, item := range items {
			if err := exactJSONFields(item, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value == nil {
			return nil
		}
		items, ok := value.(map[string]any)
		if !ok {
			return ErrScope
		}
		for _, item := range items {
			if err := exactJSONFields(item, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Interface:
		return nil
	default:
		if value == nil {
			return ErrScope
		}
	}
	return nil
}
