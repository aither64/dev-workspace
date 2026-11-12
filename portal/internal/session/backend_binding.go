package session

import (
	"bytes"
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
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"golang.org/x/sys/unix"
)

const backendScalarMaxSize = 4096

var backendMemberPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}[0-9]+$`)

// BackendBinding is the durable Pi selector, not an execution permission. These
// Pi-only records leave schema-1 Codex authorities and receipts unchanged.
type BackendBinding struct {
	Schema             int                   `json:"schema"`
	Backend            agentbackend.Kind     `json:"backend"`
	BackendContract    int                   `json:"backend_contract"`
	Workspace          string                `json:"workspace"`
	Slug               string                `json:"slug"`
	SessionIncarnation string                `json:"session_incarnation"`
	CreationReceiptID  string                `json:"creation_receipt_id"`
	Revision           uint64                `json:"revision"`
	State              string                `json:"state"`
	Conversations      []BackendConversation `json:"conversations"`
}

type BackendConversation struct {
	Address     string `json:"address"`
	Incarnation string `json:"incarnation"`
	State       string `json:"state"`
	NativeID    string `json:"native_id,omitempty"`
}

type BackendBindingStore struct{ Root, Workspace string }

// Load reads session-backends/<full workspace SHA256>/<slug>.json without
// creating directories, repairing records or adopting a legacy backend. Only
// genuine absence wraps ErrNotBound; malformed or inaccessible state refuses.
func (s BackendBindingStore) Load(slug string) (BackendBinding, error) {
	if !backendCanonicalPath(s.Root) || !backendCanonicalPath(s.Workspace) || !backendSlug(slug) {
		return BackendBinding{}, errors.New("invalid backend binding scope")
	}
	digest := sha256.Sum256([]byte(s.Workspace))
	directory := filepath.Join(s.Root, "session-backends", hex.EncodeToString(digest[:]))
	data, err := readBackendRecord(directory, slug+".json")
	if err != nil {
		return BackendBinding{}, err
	}
	var binding BackendBinding
	if err := decodeBackendRecord(data, &binding, []string{
		"schema", "backend", "backend_contract", "workspace", "slug", "session_incarnation",
		"creation_receipt_id", "revision", "state", "conversations",
	}); err != nil {
		return BackendBinding{}, fmt.Errorf("decode backend binding: %w", err)
	}
	// Go's zero string cannot distinguish an omitted optional native_id from an
	// explicitly empty one. Check that distinction before returning typed data.
	var raw struct {
		Conversations []map[string]json.RawMessage `json:"conversations"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return BackendBinding{}, err
	}
	for _, conversation := range raw.Conversations {
		if err := checkBackendKeys(conversation, []string{"address", "incarnation", "state"}, "native_id"); err != nil {
			return BackendBinding{}, err
		}
		if value, present := conversation["native_id"]; present && bytes.Equal(value, []byte(`""`)) {
			return BackendBinding{}, errors.New("backend native_id must be omitted instead of empty")
		}
	}
	if err := binding.Validate(s.Workspace, slug); err != nil {
		return BackendBinding{}, err
	}
	return binding, nil
}

func (b BackendBinding) Validate(workspace, slug string) error {
	if b.Schema != 1 || b.Backend != agentbackend.Pi || b.BackendContract != 1 {
		return agentbackend.ErrUnsupportedBinding
	}
	if !backendCanonicalPath(workspace) || !backendSlug(slug) || b.Workspace != workspace || b.Slug != slug ||
		!isLowerHex(b.SessionIncarnation, 64) || !backendScalar(b.CreationReceiptID) || b.Revision == 0 ||
		(b.State != "creating" && b.State != "ready") || len(b.Conversations) == 0 {
		return errors.New("invalid backend binding identity")
	}
	addresses, nativeIDs := make(map[string]bool), make(map[string]bool)
	lead, readyLead := false, false
	for _, conversation := range b.Conversations {
		if !backendMember(conversation.Address) || addresses[conversation.Address] ||
			!isLowerHex(conversation.Incarnation, 64) ||
			(conversation.State != "creating" && conversation.State != "ready") ||
			(conversation.NativeID != "" && !backendScalar(conversation.NativeID)) {
			return errors.New("invalid backend conversation identity")
		}
		addresses[conversation.Address] = true
		if conversation.State == "ready" {
			if !backendScalar(conversation.NativeID) || nativeIDs[conversation.NativeID] {
				return errors.New("invalid or duplicate ready backend native identity")
			}
			nativeIDs[conversation.NativeID] = true
		}
		if conversation.Address == "lead" {
			lead, readyLead = true, conversation.State == "ready"
		}
	}
	if !lead || (b.State == "ready" && !readyLead) {
		return errors.New("backend binding requires its owning lead")
	}
	return nil
}

// Resolve maps a retained address, without granting membership/settings access
// or proving native history ownership. Creating records are never dispatchable.
func (b BackendBinding) Resolve(member string) (agentbackend.ConversationRef, error) {
	if err := b.Validate(b.Workspace, b.Slug); err != nil {
		return agentbackend.ConversationRef{}, err
	}
	for _, conversation := range b.Conversations {
		if conversation.Address != member {
			continue
		}
		if b.State != "ready" || conversation.State != "ready" {
			return agentbackend.ConversationRef{}, agentbackend.ErrConversationNotReady
		}
		return agentbackend.ConversationRef{
			Session: agentbackend.SessionRef{Workspace: b.Workspace, Slug: b.Slug, Incarnation: b.SessionIncarnation},
			Backend: b.Backend, Member: member, Incarnation: conversation.Incarnation, NativeID: conversation.NativeID,
		}, nil
	}
	return agentbackend.ConversationRef{}, fmt.Errorf("%w: member is not bound", agentbackend.ErrBindingConflict)
}

func backendScalar(value string) bool {
	return value != "" && len(value) <= backendScalarMaxSize && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func backendCanonicalPath(value string) bool {
	return backendScalar(value) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func backendSlug(value string) bool { return backendScalar(value) && ValidSlug(value) }

// The retained roster remains authoritative; this only validates address syntax.
func backendMember(value string) bool {
	return backendScalar(value) && (value == "lead" || backendMemberPattern.MatchString(value))
}

func readBackendRecord(directory, name string) ([]byte, error) {
	directoryFD, err := openAuthorityDirectory(directory)
	if err != nil {
		return nil, backendReadError(err)
	}
	defer unix.Close(directoryFD)
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, backendReadError(err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 {
		return nil, errors.New("backend record must be a portal-uid mode-0600 regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, authorityMaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > authorityMaxSize {
		return nil, errors.New("backend record exceeds 64 KiB")
	}
	return data, nil
}

func backendReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", agentbackend.ErrNotBound, err)
	}
	return err
}

func decodeBackendRecord(data []byte, target any, required []string) error {
	if !utf8.Valid(data) {
		return errors.New("backend JSON must be UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkBackendJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("backend record contains trailing JSON data")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if err := checkBackendKeys(object, required); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func checkBackendKeys(object map[string]json.RawMessage, keys []string, optional ...string) error {
	allowed := make(map[string]bool, len(keys)+len(optional))
	for _, key := range keys {
		allowed[key] = true
		if _, present := object[key]; !present {
			return fmt.Errorf("backend record lacks %q", key)
		}
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range object {
		if !allowed[key] {
			return fmt.Errorf("backend record has unknown field %q", key)
		}
	}
	return nil
}

// Check every object before typed decoding: encoding/json normally accepts
// duplicate keys and null scalar fields. Neither is valid authority evidence.
func checkBackendJSON(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("backend JSON is too deeply nested")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("backend JSON contains null")
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			keys := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("backend JSON contains a duplicate or invalid key")
				}
				keys[name] = true
				if err := checkBackendJSON(decoder, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := checkBackendJSON(decoder, depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid backend JSON delimiter")
		}
		_, err := decoder.Token()
		return err
	}
	return nil
}
