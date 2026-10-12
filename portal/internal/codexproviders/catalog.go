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
	ID             string   `json:"id"`
	Target         string   `json:"target"`
	CatalogDigest  string   `json:"catalogDigest"`
	Phase          string   `json:"phase"`
	Blockers       []string `json:"blockers"`
	CreatedAt      string   `json:"createdAt"`
	UpdatedAt      string   `json:"updatedAt"`
	Snapshot       string   `json:"snapshot,omitempty"`
	Message        string   `json:"message,omitempty"`
	Stage          string   `json:"stage,omitempty"`
	PreviousActive string   `json:"previousActive,omitempty"`
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
}

type RequestReceipt struct {
	Target        string `json:"target"`
	CatalogDigest string `json:"catalogDigest"`
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
	if decode(data, &state) != nil || state.Schema != 1 || state.Workspace != name || state.Root != root || state.Socket != socket {
		return State{}, errors.New("Codex provider state does not match the registered workspace")
	}
	if _, err := catalog.Find(state.Active); err != nil {
		return State{}, err
	}
	if len(state.Receipts) > 2048 {
		return State{}, errors.New("provider receipt retention exceeds bounds")
	}
	for id, receipt := range state.Receipts {
		if len(id) != 36 || !identifier.MatchString(receipt.Target) || len(receipt.CatalogDigest) != 64 {
			return State{}, errors.New("invalid provider request receipt")
		}
	}
	if o := state.Operation; o != nil {
		if !identifier.MatchString(o.Target) || len(o.ID) != 36 || len(o.CatalogDigest) != 64 {
			return State{}, errors.New("invalid provider operation identity")
		}
		switch o.Phase {
		case "waiting", "applying", "applied", "canceled", "failed", "recovery-required":
		default:
			return State{}, errors.New("invalid provider operation phase")
		}
	}
	return state, nil
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
