package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/session"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
)

func TestPiForkCommandUsesSelectedScopeWithoutCodexOverrides(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	authority := filepath.Join(root, "authority")
	if e := os.MkdirAll(authority, 0700); e != nil {
		t.Fatal(e)
	}
	b := session.BackendBinding{Schema: 1, Backend: agentbackend.Pi, BackendContract: 1, Workspace: workspace, Slug: "source", SessionIncarnation: strings.Repeat("a", 64), CreationReceiptID: strings.Repeat("b", 64), Revision: 1, State: "creating", Conversations: []session.BackendConversation{{Address: "lead", Incarnation: strings.Repeat("c", 64), State: "creating"}, {Address: "reviewer0", Incarnation: strings.Repeat("d", 64), State: "creating"}}}
	store := session.BackendBindingStore{Root: root, Workspace: workspace}
	var e error
	b, e = store.Create(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	b.Revision++
	b.State = "ready"
	for n := range b.Conversations {
		b.Conversations[n].State = "ready"
		b.Conversations[n].NativeID = []string{"lead-native", "reviewer-native"}[n]
	}
	b, e = store.CompareAndSwap(context.Background(), 1, b)
	if e != nil {
		t.Fatal(e)
	}
	a := session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: workspace, Slug: b.Slug, SessionIncarnation: b.SessionIncarnation, BindingRevision: b.Revision, Generation: strings.Repeat("e", 64), State: "ready"}
	if e = session.PublishPiRuntimeAuthority(authority, b, a); e != nil {
		t.Fatal(e)
	}
	ref, _ := b.Resolve("reviewer0")
	socket := filepath.Join(t.TempDir(), "portal.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(socket, 0600); e != nil {
		t.Fatal(e)
	}
	calls := 0
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.URL.Path != "/api/sessions/source/fork" || r.URL.Query().Get("member") != "reviewer0" || r.Header.Get("X-Workspace-Conversation-Scope") != portalweb.PiConversationScope(ref) || r.Header.Get("X-Workspace-Pi-Generation") != a.Generation || r.Header.Get("Origin") != "https://portal.example" {
				t.Error("wrong selected scope", r.URL, r.Header)
			}
			var body map[string]any
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
			}
			if len(body) != 3 || body["name"] != "fork" || body["creationDate"] != "2026-11-18" || body["sensitive"] != true {
				t.Error("override or missing field", body)
			}
		} else {
			if r.URL.Path != "/api/sessions/2026-11-18-fork/creation/retry" || r.Header.Get("X-Workspace-Conversation-Scope") != "" {
				t.Error("recovery depended on source", r.URL, r.Header)
			}
			var body map[string]any
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
			}
			if len(body) != 2 || body["receiptId"] != "receipt" || body["attempt"] != float64(2) {
				t.Error("wrong recovery owner", body)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"slug":"2026-11-18-fork","receiptId":"receipt","state":"running","phase":"Preparing fork","url":"/2026-11-18-fork/?member=reviewer0"}`))
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	var output bytes.Buffer
	e = runPiForkCommand(context.Background(), piForkCommandOptions{StateRoot: root, Workspace: workspace, Slug: "source", Destination: "2026-11-18-fork", Member: "reviewer0", Authority: authority, Socket: socket, Origin: "https://portal.example", Sensitive: true}, &output)
	if e != nil || calls != 1 || !strings.Contains(output.String(), "?member=reviewer0") {
		t.Fatal(e, calls, output.String())
	}
	sum := sha256.Sum256([]byte(workspace))
	key := filepath.Base(workspace) + "-" + hex.EncodeToString(sum[:])[:16]
	cache := filepath.Join(root, "portal", key, "creations", "2026-11-18-fork.json")
	if e = os.MkdirAll(filepath.Dir(cache), 0700); e != nil {
		t.Fatal(e)
	}
	saved := map[string]any{"schema": 5, "workspace": workspace, "receiptId": "receipt", "attempt": 2, "request": map[string]string{"backend": "pi", "kind": "fork", "slug": "2026-11-18-fork", "source": "source", "selectedMember": "reviewer0"}}
	data, _ := json.Marshal(saved)
	if e = os.WriteFile(cache, data, 0600); e != nil {
		t.Fatal(e)
	}
	output.Reset()
	e = runPiForkCommand(context.Background(), piForkCommandOptions{StateRoot: root, Workspace: workspace, Slug: "source", Destination: "2026-11-18-fork", Member: "reviewer0", Authority: "/absent-source-authority", Socket: socket, Origin: "https://portal.example"}, &output)
	if e != nil || calls != 2 {
		t.Fatal("destination recovery required source", e, calls)
	}

}
