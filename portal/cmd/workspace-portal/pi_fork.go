package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
)

type piForkCommandOptions struct {
	StateRoot, Workspace, Slug, Destination, Member, Authority, Socket, Origin string
	Sensitive                                                                  bool
}

func piForkCommand(args []string) error {
	f := flag.NewFlagSet("fork", flag.ContinueOnError)
	var o piForkCommandOptions
	f.StringVar(&o.StateRoot, "user-state-root", "", "host state root")
	f.StringVar(&o.Workspace, "workspace", "", "workspace root")
	f.StringVar(&o.Slug, "session-slug", "", "source session")
	f.StringVar(&o.Destination, "destination-slug", "", "new session")
	f.StringVar(&o.Member, "member", "lead", "source selected member")
	f.StringVar(&o.Authority, "authority-dir", "", "runtime authority directory")
	f.StringVar(&o.Socket, "portal-socket", "", "serving portal socket")
	f.StringVar(&o.Origin, "portal-origin", "", "registered portal origin")
	f.BoolVar(&o.Sensitive, "sensitive", false, "keep records local")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return piruntime.ErrScope
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return runPiForkCommand(ctx, o, os.Stdout)
}
func runPiForkCommand(ctx context.Context, o piForkCommandOptions, output io.Writer) error {
	if !session.ValidSlug(o.Slug) || !session.ValidSlug(o.Destination) || len(o.Destination) < 12 || o.Slug == o.Destination {
		return piruntime.ErrScope
	}
	date, name := o.Destination[:10], o.Destination[11:]
	if o.Destination[10] != '-' || len(name) > 48 || !session.ValidSlug(name) {
		return piruntime.ErrScope
	}
	if parsed, e := time.Parse(time.DateOnly, date); e != nil || parsed.Format(time.DateOnly) != date {
		return piruntime.ErrScope
	}
	origin, e := url.Parse(o.Origin)
	if e != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return piruntime.ErrScope
	}
	client, closeClient, e := privatePiPortalClient(o.Socket)
	if e != nil {
		return e
	}
	defer closeClient()
	var scope, generation string
	method := http.MethodPost
	var body []byte
	path := "http://" + origin.Host + "/api/sessions/" + url.PathEscape(o.Slug) + "/fork?member=" + url.QueryEscape(o.Member)
	// Retry only the exact existing accepted destination. The serving portal
	// remains the receipt validator and may recover it without a live source.
	sum := sha256.Sum256([]byte(o.Workspace))
	key := filepath.Base(o.Workspace) + "-" + hex.EncodeToString(sum[:])[:16]
	cache := filepath.Join(o.StateRoot, "portal", key, "creations", o.Destination+".json")
	f, readErr := os.Open(cache)
	if readErr == nil {
		maximum := 12*session.MaxMessageBytes + 16384
		data, err := io.ReadAll(io.LimitReader(f, int64(maximum+1)))
		_ = f.Close()
		if err != nil || len(data) > maximum {
			return piruntime.ErrScope
		}
		var saved struct {
			Schema    int
			Workspace string
			ReceiptID string `json:"receiptId"`
			Attempt   int
			Request   struct {
				Backend, Kind, Slug, Source string
				SelectedMember              string `json:"selectedMember"`
			}
		}
		if json.Unmarshal(data, &saved) != nil || saved.Schema != 5 || saved.Workspace != o.Workspace || saved.Request.Backend != "pi" || saved.Request.Kind != "fork" || saved.Request.Slug != o.Destination || saved.Request.Source != o.Slug || saved.Request.SelectedMember != o.Member || saved.ReceiptID == "" || saved.Attempt < 1 {
			return piruntime.ErrScope
		}
		body, _ = json.Marshal(map[string]any{"receiptId": saved.ReceiptID, "attempt": saved.Attempt})
		path = "http://" + origin.Host + "/api/sessions/" + url.PathEscape(o.Destination) + "/creation/retry"
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	} else {
		b, e := (session.BackendBindingStore{Root: o.StateRoot, Workspace: o.Workspace}).Load(o.Slug)
		if e != nil || b.Backend != agentbackend.Pi || b.State != "ready" {
			return piruntime.ErrScope
		}
		a, e := session.LoadPiRuntimeAuthority(o.Authority, o.Slug, o.Workspace)
		if e != nil || a.ValidateBinding(b) != nil || a.State != "ready" {
			return piruntime.ErrHeld
		}
		ref, e := b.Resolve(o.Member)
		if e != nil {
			return e
		}
		scope, generation = portalweb.PiConversationScope(ref), a.Generation
		body, _ = json.Marshal(map[string]any{"name": name, "creationDate": date, "sensitive": o.Sensitive})
	}
	r, e := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	if e != nil {
		return e
	}
	r.Header.Set("Origin", origin.String())
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	if scope != "" {
		r.Header.Set("X-Workspace-Conversation-Scope", scope)
	}
	if generation != "" {
		r.Header.Set("X-Workspace-Pi-Generation", generation)
	}
	response, e := client.Do(r)
	if e != nil {
		return fmt.Errorf("Pi fork destination %s is unconfirmed; retry the same destination: %w", o.Destination, e)
	}
	defer response.Body.Close()
	data, e := io.ReadAll(io.LimitReader(response.Body, 65537))
	if e != nil || len(data) > 65536 {
		return piruntime.ErrUnavailable
	}
	if response.StatusCode != http.StatusAccepted {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return fmt.Errorf("Pi fork returned HTTP %d; retain destination %s", response.StatusCode, o.Destination)
	}
	var result struct{ Slug, URL, ReceiptID, State, Phase string }
	if json.Unmarshal(data, &result) != nil || result.Slug != o.Destination || result.ReceiptID == "" || result.URL == "" {
		return piruntime.ErrScope
	}
	_, e = output.Write(data)
	return e
}
