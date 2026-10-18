// Package codexproviders owns the nonsecret host catalog and workspace routing
// reader. The workspace-host worker is the sole state writer.
package codexproviders

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const DefaultCatalog = "/etc/dev-workspaces/codex-providers.json"

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var operationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var digestID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func threadIdentity(value string) bool {
	return value != "" && len(value) <= 512 && !strings.ContainsAny(value, "\x00\t\r\n ")
}

func ValidID(value string) bool { return identifier.MatchString(value) }

type Provider struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	BaseURL   string `json:"baseURL,omitempty"`
	TokenFile string `json:"tokenFile,omitempty"`
}

type PublicProvider struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

func (p Provider) Public() PublicProvider { return PublicProvider{p.ID, p.Label, p.Kind} }

type Catalog struct {
	Schema          int        `json:"schema"`
	InitialProvider string     `json:"initialProvider"`
	Providers       []Provider `json:"providers"`
	Digest          string     `json:"-"`
}

func CatalogPath() string {
	if path := os.Getenv("DEV_WORKSPACES_CODEX_PROVIDERS"); path != "" {
		return path
	}
	return DefaultCatalog
}

func ReadCatalog(path string) (Catalog, error) {
	var catalog Catalog
	data, err := readBounded(path, 64*1024, false)
	if err != nil {
		return catalog, err
	}
	if err := decode(data, &catalog); err != nil || catalog.Schema != 1 || len(catalog.Providers) < 1 || len(catalog.Providers) > 32 {
		return Catalog{}, errors.New("invalid Codex provider catalog")
	}
	seen := map[string]bool{}
	for _, p := range catalog.Providers {
		if !identifier.MatchString(p.ID) || seen[p.ID] || strings.TrimSpace(p.Label) == "" || len(p.Label) > 128 || strings.ContainsAny(p.Label, "\r\n\x00") {
			return Catalog{}, errors.New("invalid Codex provider identity")
		}
		seen[p.ID] = true
		switch p.Kind {
		case "subscription":
			if p.ID != "openai" || p.BaseURL != "" || p.TokenFile != "" {
				return Catalog{}, errors.New("invalid subscription provider")
			}
		case "codex-lb":
			u, e := url.Parse(p.BaseURL)
			if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/v1") || len(p.BaseURL) > 2048 || !filepath.IsAbs(p.TokenFile) || filepath.Clean(p.TokenFile) != p.TokenFile || len(p.TokenFile) > 4096 {
				return Catalog{}, errors.New("invalid load balancer definition")
			}
		default:
			return Catalog{}, errors.New("unknown Codex provider kind")
		}
	}
	if !seen["openai"] || !seen[catalog.InitialProvider] {
		return Catalog{}, errors.New("catalog is missing its subscription or initial provider")
	}
	digest := sha256.Sum256(data)
	catalog.Digest = hex.EncodeToString(digest[:])
	return catalog, nil
}

func (c Catalog) Find(id string) (Provider, error) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, nil
		}
	}
	return Provider{}, errors.New("Codex provider is unavailable")
}

type Operation struct {
	ID              string            `json:"id"`
	Target          string            `json:"target"`
	CatalogDigest   string            `json:"catalogDigest"`
	Phase           string            `json:"phase"`
	Blockers        []string          `json:"blockers"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
	Snapshot        string            `json:"snapshot,omitempty"`
	Message         string            `json:"message,omitempty"`
	Stage           string            `json:"stage,omitempty"`
	PreviousActive  string            `json:"previousActive,omitempty"`
	Strategy        string            `json:"strategy,omitempty"`
	Session         string            `json:"session,omitempty"`
	Sessions        []string          `json:"sessions,omitempty"`
	Inherit         bool              `json:"inherit,omitempty"`
	CancelRequested bool              `json:"-"`
	Dismissed       bool              `json:"-"`
	Roots           map[string]string `json:"roots,omitempty"`
}

func (o *Operation) Reserved() bool {
	return o != nil && (o.Phase == "applying" || o.Phase == "recovery-required")
}

type State struct {
	Schema    int                       `json:"schema"`
	Workspace string                    `json:"workspace"`
	Root      string                    `json:"root"`
	Socket    string                    `json:"socket"`
	Active    string                    `json:"active"`
	Operation *Operation                `json:"operation,omitempty"`
	Receipts  map[string]RequestReceipt `json:"receipts,omitempty"`
	Sessions  map[string]Selection      `json:"sessions,omitempty"`
}

// Selection binds a session override to its exact retained root. During fresh
// creation CreationID instead binds it to the CLI's durable creation incarnation.
type Selection struct {
	Provider     string `json:"provider"`
	RootThreadID string `json:"rootThreadId,omitempty"`
	CreationID   string `json:"creationId,omitempty"`
}

func (s State) Provider(slug, root string) string {
	if selection, ok := s.Sessions[slug]; ok && root != "" && selection.RootThreadID == root {
		return selection.Provider
	}
	return s.Active
}

// BlocksSession excludes only the requested session for scoped transitions.
// Workspace-default changes preserve explicit overrides and fence new inherited
// creations as well as the retained sessions inventoried by the worker.
func (s State) BlocksSession(slug, root string) bool {
	if !s.Operation.Reserved() {
		return false
	}
	if s.Operation.Strategy != "scoped-v1" {
		return true
	}
	if s.Operation.Session != "" {
		return s.Operation.Session == slug
	}
	selection, explicit := s.Sessions[slug]
	if explicit && root == "" && selection.RootThreadID == "" && selection.CreationID != "" {
		return false
	}
	return !explicit || root == "" || selection.RootThreadID != root
}

type RequestReceipt struct {
	Target        string `json:"target"`
	CatalogDigest string `json:"catalogDigest"`
	Session       string `json:"session,omitempty"`
	Inherit       bool   `json:"inherit,omitempty"`
}

func StatePath(stateRoot, name string) (string, error) {
	if !identifier.MatchString(name) || !filepath.IsAbs(stateRoot) {
		return "", errors.New("invalid workspace provider state identity")
	}
	return filepath.Join(stateRoot, "providers", name, "state.json"), nil
}

func ReadState(stateRoot, name, root, socket string, catalog Catalog) (State, error) {
	path, err := StatePath(stateRoot, name)
	if err != nil {
		return State{}, err
	}
	data, err := readBounded(path, 1024*1024, true)
	if errors.Is(err, os.ErrNotExist) {
		return State{Schema: 1, Workspace: name, Root: root, Socket: socket, Active: catalog.InitialProvider}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if decode(data, &state) != nil || (state.Schema != 1 && state.Schema != 2) || state.Workspace != name || state.Root != root || state.Socket != socket {
		return State{}, errors.New("Codex provider state does not match the registered workspace")
	}
	if _, err := catalog.Find(state.Active); err != nil {
		return State{}, err
	}
	if len(state.Receipts) > 2048 {
		return State{}, errors.New("provider receipt retention exceeds bounds")
	}
	for id, receipt := range state.Receipts {
		if !operationID.MatchString(id) || !identifier.MatchString(receipt.Target) || !digestID.MatchString(receipt.CatalogDigest) || (receipt.Session != "" && !identifier.MatchString(receipt.Session)) || receipt.Inherit && receipt.Session == "" {
			return State{}, errors.New("invalid provider request receipt")
		}
	}
	if len(state.Sessions) > 4096 || (state.Schema == 1 && len(state.Sessions) != 0) {
		return State{}, errors.New("invalid session provider selections")
	}
	for slug, selection := range state.Sessions {
		if !identifier.MatchString(slug) || selection.RootThreadID != "" && !threadIdentity(selection.RootThreadID) || selection.CreationID != "" && !digestID.MatchString(selection.CreationID) || selection.RootThreadID == "" && selection.CreationID == "" {
			return State{}, errors.New("invalid session provider selection identity")
		}
		if _, err := catalog.Find(selection.Provider); err != nil {
			return State{}, err
		}
	}
	if o := state.Operation; o != nil {
		if !identifier.MatchString(o.Target) || !operationID.MatchString(o.ID) || !digestID.MatchString(o.CatalogDigest) {
			return State{}, errors.New("invalid provider operation identity")
		}
		switch o.Phase {
		case "waiting", "applying", "applied", "canceled", "failed", "recovery-required":
		default:
			return State{}, errors.New("invalid provider operation phase")
		}
		if o.Strategy != "" && (state.Schema != 2 || o.Strategy != "scoped-v1" || (o.Session != "" && !identifier.MatchString(o.Session))) {
			return State{}, errors.New("unsupported scoped provider operation")
		}

		if o.Strategy == "scoped-v1" {
			switch o.Stage {
			case "accepted", "waiting-creation", "waiting-package":
				if o.Reserved() {
					return State{}, errors.New("invalid reserved provider stage")
				}
			case "quiescing", "preparing", "switching", "resuming", "restoring-terminals":
			default:
				return State{}, errors.New("invalid scoped provider stage")
			}
			if len(o.Sessions) > 4096 || len(o.Roots) != len(o.Sessions) || o.Inherit && o.Session == "" || o.Session != "" && (len(o.Sessions) != 1 || o.Sessions[0] != o.Session) {
				return State{}, errors.New("invalid scoped provider roots")
			}
			seen := map[string]bool{}
			for _, slug := range o.Sessions {
				if !identifier.MatchString(slug) || seen[slug] || !threadIdentity(o.Roots[slug]) {
					return State{}, errors.New("invalid scoped provider scope")
				}
				seen[slug] = true
			}
		}
	}
	if o := state.Operation; o != nil {
		control, err := ReadOperationControl(filepath.Dir(path), o.ID)
		if err != nil {
			return State{}, err
		}
		o.CancelRequested, o.Dismissed = control.CancelRequested, control.Dismissed
	}
	return state, nil
}

type OperationControl struct {
	Operation       string `json:"operation"`
	CancelRequested bool   `json:"cancelRequested,omitempty"`
	Dismissed       bool   `json:"dismissed,omitempty"`
}

// Separate UI/control metadata preserves rollback to strict schema-2 readers.
func ReadOperationControl(directory, id string) (OperationControl, error) {
	var control OperationControl
	if !operationID.MatchString(id) {
		return control, errors.New("invalid provider control operation")
	}
	data, err := readBounded(filepath.Join(directory, id+".control.json"), 4096, true)
	if errors.Is(err, os.ErrNotExist) {
		return control, nil
	}
	if err != nil {
		return control, err
	}
	if decode(data, &control) != nil || control.Operation != id {
		return control, errors.New("invalid provider control identity")
	}
	return control, nil
}

func decode(data []byte, result any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing provider JSON")
	}
	return nil
}

func readBounded(path string, max int64, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max || (private && info.Mode().Perm()&0077 != 0) {
		return nil, fmt.Errorf("invalid provider file permissions or size")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("provider file exceeds bounds")
	}
	return data, nil
}
