package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
)

func TestCommunicationForwardKeepsOwningScopeAndExactApproval(t *testing.T) {
	for _, backend := range []string{"codex", "pi"} {
		for _, outcome := range []string{"ready", "expired-parent", "redirect", "foreign-receipt", "oversized"} {
			t.Run(backend+"/"+outcome, func(t *testing.T) {
				ctx := context.Background()
				options, _ := communicationAdmissionFixture(t)
				admission, err := newCommunicationAdmission(options, filepath.Join(options.packageRoot, "bin/workspace-portal"))
				if err != nil {
					t.Fatal(err)
				}
				defer admission.expire()
				root := t.TempDir()
				workspace := t.TempDir()
				authority := filepath.Join(root, "authority")
				if err := os.Mkdir(authority, 0700); err != nil {
					t.Fatal(err)
				}
				id := communication.Identity{Slug: "source", Thread: "source-root", Incarnation: "source-observation"}
				caller := communication.EndpointRecord{Codex: &id}
				scope, generation := id.Incarnation, ""
				if backend == "pi" {
					b := session.BackendBinding{Schema: 1, Backend: agentbackend.Pi, BackendContract: 1, Workspace: workspace, Slug: "source", SessionIncarnation: strings.Repeat("a", 64), CreationReceiptID: strings.Repeat("b", 64), Revision: 1, State: "creating", Conversations: []session.BackendConversation{{Address: "lead", Incarnation: strings.Repeat("c", 64), State: "creating"}}}
					store := session.BackendBindingStore{Root: root, Workspace: workspace}
					b, err = store.Create(ctx, b)
					if err != nil {
						t.Fatal(err)
					}
					b.Revision++
					b.State = "ready"
					b.Conversations[0].State = "ready"
					b.Conversations[0].NativeID = "pi-source"
					b, err = store.CompareAndSwap(ctx, 1, b)
					if err != nil {
						t.Fatal(err)
					}
					a := session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: workspace, Slug: b.Slug, SessionIncarnation: b.SessionIncarnation, BindingRevision: b.Revision, Generation: strings.Repeat("d", 64), State: "ready"}
					if err := session.PublishPiRuntimeAuthority(authority, b, a); err != nil {
						t.Fatal(err)
					}
					ref, err := b.Resolve("lead")
					if err != nil {
						t.Fatal(err)
					}
					caller = communication.EndpointRecord{Pi: &communication.PiEndpoint{Backend: "pi", Session: monitoroperation.PiSessionRecord{Workspace: workspace, Slug: b.Slug, Incarnation: b.SessionIncarnation}, LeadIncarnation: ref.Incarnation, NativeID: ref.NativeID}}
					scope, generation = portalweb.PiConversationScope(ref), a.Generation
				}
				directory, err := os.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer directory.Close()
				socket := filepath.Join("/proc/self/fd", strconv.Itoa(int(directory.Fd())), "portal.sock")
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(socket, 0600); err != nil {
					listener.Close()
					t.Fatal(err)
				}
				approval := "  exact user approval; chat reference 17  "
				request := communication.Request{Action: "allow", Peer: "peer", Approval: approval}
				var count atomic.Int32
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					count.Add(1)
					if r.Method != "POST" || r.URL.Path != "/api/sessions/source/communication" || r.Header.Get("Origin") != "https://portal.fixture" || r.Header.Get("X-Workspace-Conversation-Scope") != scope || r.Header.Get("X-Workspace-Pi-Generation") != generation {
						t.Error("forwarded source/origin changed")
					}
					var got communication.Request
					if json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
						t.Error("approval/request changed", got)
					}
					if outcome == "redirect" {
						w.Header().Set("Location", "/must-not-follow")
						w.WriteHeader(307)
						return
					}
					if outcome == "oversized" {
						w.Write([]byte(strings.Repeat(" ", (1<<20)+1)))
						return
					}
					source := caller
					if outcome == "foreign-receipt" {
						source = communication.EndpointRecord{Codex: &communication.Identity{Slug: "foreign", Thread: "foreign", Incarnation: "foreign"}}
					}
					peer := communication.EndpointRecord{Codex: &communication.Identity{Slug: "peer", Thread: "peer-root", Incarnation: "peer-observation"}}
					json.NewEncoder(w).Encode(communication.EndpointView{Grants: []communication.PiGrantRecord{{ID: strings.Repeat("e", 32), A: source, B: peer, Approval: approval, ApprovedBy: source.Slug(), CreatedAt: time.Now().UTC()}}, Messages: []communication.PiMessageRecord{}})
				})}
				done := make(chan error, 1)
				go func() { done <- server.Serve(listener) }()
				defer func() { server.Close(); <-done }()
				if outcome == "expired-parent" {
					admission.expire()
				}
				var output bytes.Buffer
				err = forwardCommunication(ctx, workspace, root, authority, socket, "https://portal.fixture", caller, request, admission, &output)
				if (outcome == "ready") != (err == nil) {
					t.Fatal(outcome, err)
				}
				expected := int32(1)
				if outcome == "expired-parent" {
					expected = 0
				}
				if count.Load() != expected {
					t.Fatal("replayed request/invalid parent reached HTTP", count.Load())
				}
				if outcome == "ready" && !strings.Contains(output.String(), approval) {
					t.Fatal("exact CLI approval reference lost")
				}
			})
		}
	}
}
