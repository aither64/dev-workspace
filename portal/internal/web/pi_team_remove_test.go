package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

type piRemovalFixture struct {
	*piForkRuntimeFixture
	unloads  []agentbackend.ExecutionHandle
	failure  error
	positive bool
}

func (f *piRemovalFixture) Unload(_ context.Context, h agentbackend.ExecutionHandle) (piruntime.StopResult, error) {
	f.unloads = append(f.unloads, h)
	if f.failure != nil {
		return piruntime.StopResult{Outcome: "unknown"}, f.failure
	}
	return piruntime.StopResult{Outcome: "detached", VerifiedDetach: f.positive}, nil
}
func TestPiRemovePreservesIdentityAndRetriesExactTombstone(t *testing.T) {
	for _, scenario := range []struct{ address, timestamp string }{
		{"implementer0", "recorded"}, {"reviewer0", "recorded"},
		{"implementer0", "nil"}, {"reviewer0", "nil"},
		{"implementer0", "zero"}, {"reviewer0", "zero"},
	} {
		address := scenario.address
		t.Run(address+"/"+scenario.timestamp, func(t *testing.T) {
			s, source, native := newPiForkFixture(t, true)
			f := &piRemovalFixture{piForkRuntimeFixture: native, positive: true}
			s.config.PiCreation.Runtime = f
			store, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
			before, e := store.LoadPi(source.Request.Slug)
			if e != nil {
				t.Fatal(e)
			}
			if scenario.timestamp != "recorded" {
				before.Members[2].RemovedAt = nil
				if scenario.timestamp == "zero" {
					before.Members[2].RemovedAt = &time.Time{}
				}
				// Seed an already supported historical record; ordinary reads and
				// removal must preserve its absent/zero timestamp exactly.
				path := filepath.Join(userstate.WorkspaceDirectory(s.config.UserStateRoot, "codex-teams", s.config.Workspace), source.Request.Slug+".json")
				if e = monitoroperation.WritePiRecord(path, before, 256<<10); e != nil {
					t.Fatal(e)
				}
			}
			bindings := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
			binding, _ := bindings.Load(source.Request.Slug)
			// The original fork fixture captures source snapshots, but removal
			// must also reprove the remaining ready sibling's first assignment.
			ready := before.Members[1]
			readyRef, _ := binding.Resolve(ready.Address)
			native.assignment = &piCreationRuntimeFixture{outcome: "Accepted", instructions: ready.Instructions, state: piruntime.ConversationState{
				Conversation: readyRef, Generation: native.source.Generation, HistoryRef: strings.Repeat("a", 64), Persisted: true, Settings: native.sources[ready.Address].Settings,
			}}
			if _, e = native.assignment.Send(context.Background(), piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: readyRef, Generation: native.source.Generation}), backend.SendRequest{ClientUserMessageID: ready.InitialInputID, Message: "Review the fixture.", ActionContext: "fixture original assignment"}); e != nil {
				t.Fatal(e)
			}
			lead, _ := binding.Resolve("lead")
			request := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+binding.Slug+"/team", nil)
			request.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(lead))
			request.Header.Set("X-Workspace-Pi-Generation", native.source.Generation)
			body := teamRequestBody{Action: "remove", Address: address}
			if address == "reviewer0" {
				f.failure = piruntime.ErrHeld
				if _, e = s.removePiMember(context.Background(), request, binding, body); !errors.Is(e, piruntime.ErrHeld) {
					t.Fatal(e)
				}
				f.failure = nil
				f.positive = false
				if _, e = s.removePiMember(context.Background(), request, binding, body); !errors.Is(e, piruntime.ErrHeld) {
					t.Fatal(e)
				}
				f.positive = true
			}
			s.piCreationCheckpoint = func(point string) error {
				if point == "remove_detached" {
					return errors.New("lost before CAS")
				}
				return nil
			}
			if _, e = s.removePiMember(context.Background(), request, binding, body); e == nil {
				t.Fatal("interruption ignored")
			}
			unchanged, _ := store.LoadPi(binding.Slug)
			if !reflect.DeepEqual(unchanged, before) {
				t.Fatal("failed CAS changed roster")
			}
			s.piCreationCheckpoint = nil
			after, e := s.removePiMember(context.Background(), request, binding, body)
			if e != nil {
				t.Fatal(e)
			}
			if after.Schema != 5 || after.Revision != before.Revision+1 {
				t.Fatal(after)
			}
			for n, m := range after.Members {
				want := before.Members[n]
				if m.Address == address {
					if m.State != "removed" || m.RemovedAt == nil || m.RemovedAt.IsZero() {
						t.Fatal(m)
					}
					m.State, m.RemovedAt = want.State, want.RemovedAt
				}
				if !reflect.DeepEqual(m, want) {
					t.Fatal("member provenance changed", m, want)
				}
			}
			unloads := len(f.unloads)
			if address == "reviewer0" && (unloads != 4 || f.unloads[2] != f.unloads[3]) {
				t.Fatal("positive detach retry changed target", f.unloads)
			}
			repeated, e := s.removePiMember(context.Background(), request, binding, body)
			if e != nil || !reflect.DeepEqual(repeated, after) || len(f.unloads) != unloads {
				t.Fatal("lost reply retry changed tombstone", e)
			}
			now, _ := bindings.Load(binding.Slug)
			if !reflect.DeepEqual(now, binding) {
				t.Fatal("binding changed")
			}
			if address == "implementer0" && len(f.unloads) != 0 {
				t.Fatal("unstarted removal created native effect")
			}
			capture, _, e := s.capturePiFork(context.Background(), creationRequest{Kind: "fork", Backend: "pi", Source: binding.Slug, Slug: "2026-11-19-removed-fork", SelectedMember: "lead"}, strings.Repeat("1", 64), strings.Repeat("2", 64))
			if e != nil {
				t.Fatal("Fork reads removal roster", e)
			}
			copied, _ := capture.ForkMember(address)
			if copied.State != "removed" || copied.Origin != nil || copied.NativeID != "" {
				t.Fatal("removed history copied", copied)
			}
			for _, bad := range []string{"lead", "missing"} {
				if _, e = s.removePiMember(context.Background(), request, binding, teamRequestBody{Action: "remove", Address: bad}); e == nil {
					t.Fatal("invalid target removed")
				}
			}
			request.Header.Set("X-Workspace-Conversation-Scope", "stale")
			if _, e = s.removePiMember(context.Background(), request, binding, body); e == nil {
				t.Fatal("wrong scope admitted")
			}
			data, _ := json.Marshal(after)
			decoded, e := teamruntime.DecodePiRoster(data)
			if e != nil || !reflect.DeepEqual(decoded, after) {
				t.Fatal("schema5 roundtrip", e)
			}
		})
	}
}
func TestPiForkRetainedRemovalDoesNotRelaxInitialCopyProof(t *testing.T) {
	s, source, native := newPiForkFixture(t, true)
	r := frozenPiForkReceipt(t, s, source, "reviewer0")
	if e := s.initializePiFork(r); e != nil {
		t.Fatal(e)
	}
	i, _ := r.piIntent()
	bindings := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
	binding, _ := bindings.Load(i.Slug)
	lead, _ := binding.Resolve("lead")
	f := &piRemovalFixture{piForkRuntimeFixture: native, positive: true}
	s.config.PiCreation.Runtime = f
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	req.Header.Set("X-Workspace-Conversation-Scope", piWorkflowNamespace(lead))
	req.Header.Set("X-Workspace-Pi-Generation", native.runtime.Generation)
	if _, e := s.removePiMember(context.Background(), req, binding, teamRequestBody{Action: "remove", Address: "reviewer0"}); e != nil {
		t.Fatal(e)
	}
	for ref := range native.copies {
		if ref.Member == "reviewer0" {
			delete(native.copies, ref)
		}
	}
	if e := s.verifyPiForkMembers(context.Background(), i, binding, native.runtime.Generation); e == nil {
		t.Fatal("initial full copy proof relaxed")
	}
	if e := s.verifyPiForkMembers(context.Background(), i, binding, native.runtime.Generation, true); e != nil {
		t.Fatal("retained removed member reproved", e)
	}
	if e := os.Remove(s.creationEvidencePath(r)); e != nil {
		t.Fatal(e)
	}
	if e := s.verifyPiForkMembers(context.Background(), i, binding, native.runtime.Generation, true); e == nil {
		t.Fatal("missing completion permitted omission")
	}
}
func TestTeamManagementSharedTableHasNoAssignmentComposer(t *testing.T) {
	for _, team := range []bool{false, true} {
		s, source, _ := newPiForkFixture(t, team)
		summary, e := session.Find(s.config.Workspace, source.Request.Slug)
		if e != nil {
			t.Fatal(e)
		}
		d := pageData{Session: summary}
		if e = s.configurePiSessionPage(context.Background(), &d, nil); e != nil {
			t.Fatal(e)
		}
		var out strings.Builder
		if e = s.templates.ExecuteTemplate(&out, "member-status", d); e != nil {
			t.Fatal(e)
		}
		html := out.String()
		for _, wanted := range []string{"team-table", "data-action=\"add\"", "data-team-settings=\"lead\"", "Actions", "Saved"} {
			if !strings.Contains(html, wanted) {
				t.Fatal("missing shared control", wanted)
			}
		}
		if strings.Contains(html, "data-action=\"assign\"") {
			t.Fatal("assignment composer retained")
		}
	}
}

func TestTeamManagementHydratedAdapters(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("set PORTAL_BROWSER_TEST=1 for Team hydration")
	}
	for _, kind := range []string{"codex", "pi-solo", "pi-fork"} {
		t.Run(kind, func(t *testing.T) {
			s, source, _ := newPiForkFixture(t, kind != "pi-solo")
			slug := source.Request.Slug
			if kind == "pi-solo" {
				var native *piSettingsFixture
				s, native, _ = newPiAddFixture(t)
				slug = native.state.Conversation.Session.Slug
				path := filepath.Join(userstate.WorkspaceDirectory(s.config.UserStateRoot, "codex-teams", s.config.Workspace), slug+".json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "pi-fork" {
				r := frozenPiForkReceipt(t, s, source, "reviewer0")
				if e := s.initializePiFork(r); e != nil {
					t.Fatal(e)
				}
				slug = r.Request.Slug
			}
			summary, e := session.Find(s.config.Workspace, slug)
			if e != nil {
				t.Fatal(e)
			}
			d := pageData{Session: summary, MaxMessageBytes: session.MaxMessageBytes, AgentTeams: s.agentTeamsPage()}
			scopes := map[string]string{}
			gen := ""
			if kind != "codex" {
				if e = s.configurePiSessionPage(context.Background(), &d, nil); e != nil {
					t.Fatal(e)
				}
				for _, row := range d.TeamManagement().Rows {
					scopes[row.Address] = row.Scope
					gen = row.Generation
				}
			} else {
				s = newTestServer(t)
				d.AgentTeams = s.agentTeamsPage()
				d.Session = &session.Summary{Manifest: session.Manifest{Schema: 1, Slug: "example", Codex: session.Codex{ThreadID: "root"}}, Interactive: true, RecoveryReady: true}
				d.SessionBackend = "codex"
				d.ConversationInteractive = true
				d.HasConversation = true
				d.ConversationReady = true
				d.DirectTeam = &teamruntime.Roster{Members: []teamruntime.Member{{Address: "reviewer0", Role: "reviewer", State: "ready", Thread: "member", Model: "model", Effort: "high", Access: "read_only"}}}
				d.TeamRoles = []teamRoleOption{{ID: "implementer", Label: "Implementer"}}
			}
			handler := s.Handler()
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fixture/team-page" {
					s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { s.render(w, "session", d) })).ServeHTTP(w, r)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer host.Close()
			input, _ := json.Marshal(map[string]any{"origin": host.URL, "kind": kind, "scopes": scopes, "generation": gen, "nativeId": d.SelectedThreadID, "hasMember": kind != "pi-solo"})
			node, e := exec.LookPath("node")
			if e != nil {
				t.Fatal(e)
			}
			out, e := exec.Command(node, "team_management_hydrated_test.cjs", string(input)).CombinedOutput()
			if e != nil {
				t.Fatalf("Team hydration %v\n%s", e, out)
			}
			t.Log(string(out))
		})
	}
}
