package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

func TestPiSoloAddProductionRouteAndExactPublicationRecovery(t *testing.T) { testPiSoloAdd(t, false) }
func TestPiAbsentSoloAddProductionRouteAndExactPublicationRecovery(t *testing.T) {
	testPiSoloAdd(t, true)
}
func testPiSoloAdd(t *testing.T, absent bool) {
	for _, point := range []string{"", "add_roster", "add_binding", "add_authority"} {
		t.Run(point, func(t *testing.T) {
			s, f, scope := newPiAddFixture(t)
			slug := f.state.Conversation.Session.Slug
			rosterPath := filepath.Join(userstate.WorkspaceDirectory(s.config.UserStateRoot, "codex-teams", s.config.Workspace), slug+".json")
			if absent {
				if err := os.Remove(rosterPath); err != nil {
					t.Fatal(err)
				}
				get := httptest.NewRequest(http.MethodGet, "/api/sessions/"+slug+"/team", nil)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, get)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"roster":null`) || !strings.Contains(w.Body.String(), `"derivedSolo":`) {
					t.Fatal("absent Solo not explicitly projected", w.Body.String())
				}
				page := httptest.NewRecorder()
				s.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/"+slug+"/", nil))
				if page.Code != 200 || !strings.Contains(page.Body.String(), "Saved model/reasoning") || !strings.Contains(page.Body.String(), `data-action="add"`) {
					t.Fatal("actual absent Solo form/lead view unavailable")
				}
				if _, err := os.Stat(rosterPath); !os.IsNotExist(err) {
					t.Fatal("GET wrote missing roster", err)
				}
			}
			bindings := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
			beforeBinding, _ := bindings.Load(slug)
			receipt := creationReceipt{ReceiptID: beforeBinding.CreationReceiptID, Request: creationRequest{Slug: slug}}
			originalIntent, _ := os.ReadFile(s.creationEvidencePath(receipt) + ".request")
			originalReceipt, _ := os.ReadFile(s.creationEvidencePath(receipt))
			originalNative, originalInput := f.state, f.submission
			creates, sends := f.creates, f.sends
			request := teamRequestBody{Action: "add", Role: "implementer", Model: "model", Effort: "high", MessageID: "11111111-1111-4111-8111-111111111111"}
			post := func(req teamRequestBody, selectedScope string) *httptest.ResponseRecorder {
				data, _ := json.Marshal(req)
				r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+slug+"/team", strings.NewReader(string(data)))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", s.config.BaseURL)
				r.Header.Set("X-Workspace-Conversation-Scope", selectedScope)
				r.Header.Set("X-Workspace-Pi-Generation", f.state.Generation)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				return w
			}
			if w := post(request, ""); w.Code == 200 {
				t.Fatal("missing scope appended")
			}
			wrong := request
			wrong.Model = "unsupported"
			if w := post(wrong, scope); w.Code == 200 {
				t.Fatal("unsupported model appended")
			}
			fired := false
			s.piCreationCheckpoint = func(current string) error {
				if point != "" && current == point && !fired {
					fired = true
					return errors.New("fixture interrupted publication")
				}
				return nil
			}
			first := post(request, scope)
			if point != "" {
				if first.Code == 200 || !fired {
					t.Fatal("checkpoint not reached", first.Code, first.Body.String())
				}
				if point != "add_authority" {
					if err := s.withPiSessionRead(context.Background(), f.state.Conversation.Session, func(piSessionAdmission) error { return nil }); err == nil {
						t.Fatal("partial Add admitted ordinary reads")
					}
				}
				first = post(request, scope)
			}
			if first.Code != 200 {
				t.Fatal(first.Code, first.Body.String())
			}
			var result struct {
				Member teamruntime.PiRosterMember `json:"member"`
				Roster teamruntime.PiRoster       `json:"roster"`
			}
			if json.Unmarshal(first.Body.Bytes(), &result) != nil || result.Roster.Schema != 3 || len(result.Roster.Members) != 1 || result.Member.State != "creating" || result.Member.CreationID == beforeBinding.CreationReceiptID || result.Member.InitialInputID != "" {
				t.Fatal("Add did not reserve an independent member", first.Body.String())
			}
			if absent && (result.Roster.Revision != 1 || !result.Member.Addition.OriginalSolo || result.Member.Addition.PriorRosterRevision != 0) {
				t.Fatal("absent Solo provenance/revision changed")
			}
			if w := post(request, scope); w.Code != 200 {
				t.Fatal("same request did not reconcile", w.Body.String())
			}
			changed := request
			changed.Effort = "low"
			if w := post(changed, scope); w.Code == 200 {
				t.Fatal("changed same-ID payload accepted")
			}
			after, _ := bindings.Load(slug)
			if len(after.Conversations) != 2 || after.Revision != beforeBinding.Revision+1 || after.Conversations[1].State != "creating" || after.Conversations[1].NativeID != "" || !reflect.DeepEqual(after.Conversations[0], beforeBinding.Conversations[0]) {
				t.Fatal("existing binding changed", after)
			}
			if err := s.withPiSessionRead(context.Background(), f.state.Conversation.Session, func(piSessionAdmission) error { return nil }); err != nil {
				t.Fatal("completed Add not admitted", err)
			}
			for path, want := range map[string][]byte{s.creationEvidencePath(receipt) + ".request": originalIntent, s.creationEvidencePath(receipt): originalReceipt} {
				got, _ := os.ReadFile(path)
				if string(got) != string(want) {
					t.Fatal("original creation bytes changed")
				}
			}
			if f.creates != creates || f.sends != sends || f.state != originalNative || f.submission != originalInput {
				t.Fatal("Add performed native effects")
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+slug+"/", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `data-action="add"`) || !strings.Contains(w.Body.String(), "implementer0") || !strings.Contains(w.Body.String(), "model · high") {
				t.Fatal("production Add form/reservation missing", w.Body.String())
			}
			if point == "" {
				f.state.Persisted = true
				rootPolicy, err := composePiWorkflowPolicy("workspace_write", "default", 1, []string{"publish_plan", "request_user_input_async"})
				if err != nil {
					t.Fatal(err)
				}
				f.initialPolicy = &rootPolicy
				ports := &piAddedTeamFixture{piSettingsFixture: f, slots: map[string]*piSettingsFixture{}, creation: map[string]string{"lead": beforeBinding.CreationReceiptID}, receipts: map[string]piruntime.SubmissionStatus{}}
				s.config.PiCreation.Runtime = ports
				assignment := teamRequestBody{Action: "assign", From: "lead", To: result.Member.Address, Message: "first real assignment", MessageID: "33333333-3333-4333-8333-333333333333"}
				if w := post(assignment, scope); w.Code != 200 {
					t.Fatal("first assignment", w.Code, w.Body.String())
				}
				ready, err := bindings.Load(slug)
				if err != nil {
					t.Fatal(err)
				}
				memberRef, err := ready.Resolve(result.Member.Address)
				if err != nil {
					t.Fatal("member not ready", err)
				}
				caller := teamruntime.PiCaller{Runtime: f.runtime, Handle: agentbackend.ExecutionHandle{Conversation: memberRef, Generation: f.state.Generation}}
				if report, err := (piTeamService{s}).Report(context.Background(), caller, "44444444-4444-4444-8444-444444444444", "assignment result"); err != nil || report.Outcome != backend.Accepted {
					t.Fatal("report", report, err)
				}
				if w := post(assignment, scope); w.Code != 200 {
					t.Fatal("same assignment", w.Body.String())
				}
				if ports.slots[result.Member.Address].creates != 1 || ports.slots[result.Member.Address].sends != 1 || len(f.initialPolicy.RegisteredCeiling) != len(rootPolicy.RegisteredCeiling) {
					t.Fatal("assignment replayed or root ceiling grew")
				}
				if f.submission != originalInput {
					t.Fatal("report replaced original root receipt")
				}
			}
		})
	}
}

func newPiAddFixture(t *testing.T) (*Server, *piSettingsFixture, string) {
	t.Helper()
	s, f, _, scope, _ := piSettingsHTTPFixture(t)
	// Add freezes installed byte authority. The older settings fixture's
	// in-memory catalog uses a synthetic digest; load its actual file here.
	installed, err := piproviders.LoadInstalled(s.config.PackageRoot)
	if err != nil {
		t.Fatal(err)
	}
	s.config.PiCreation.providers, err = newPiProviderComposition(installed, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	// The installed default development team supplies roles absent in Solo.
	additional := piCreationTeamCatalogFixture(t, historicalPiTeamPreset()).Catalog
	c := s.installedTeams.Catalog
	for name, team := range additional.Teams {
		c.Teams[name] = team
	}
	c.NativeAgentConfigs.Roles = append(c.NativeAgentConfigs.Roles, additional.NativeAgentConfigs.Roles...)
	name := "delegated"
	c.DefaultDevelopmentTeam = &name
	slug := f.state.Conversation.Session.Slug
	// The fixture calls initialization synchronously; complete the same
	// cache publication normally owned by runCreation before a page GET.
	accepted := s.creations[slug]
	accepted.State = "ready"
	s.creations[slug] = accepted
	if err := s.saveCreation(accepted); err != nil {
		t.Fatal(err)
	}
	piAddWorkerDescriptor(t, s, 2)
	return s, f, scope
}
func piAddWorkerDescriptor(t *testing.T, s *Server, protocol int) {
	t.Helper()
	// Synthetic declaration for the existing fake ports, not an SDK launch/proof.
	root := "/nix/store/" + strings.Repeat("0", 32) + "-synthetic-add-worker"
	value := map[string]any{"schema": 1, "worker_protocol": protocol, "node_executable": root + "/bin/node", "worker_entrypoint": root + "/worker.mjs", "pi_package_root": root}
	if err := writePiCreationRecord(filepath.Join(s.config.PackageRoot, "share/dev-workspace/pi-worker.json"), value, 4096); err != nil {
		t.Fatal(err)
	}
}
func TestPiAddRequiresFrozenConstructionSupportBeforeReservation(t *testing.T) {
	for _, scenario := range []string{"protocol1", "missing_role_contract", "attachment_unavailable", "retained_two_tool_root"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, scope := newPiAddFixture(t)
			slug := f.state.Conversation.Session.Slug
			path := filepath.Join(userstate.WorkspaceDirectory(s.config.UserStateRoot, "codex-teams", s.config.Workspace), slug+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			creates, sends := f.creates, f.sends
			rootPolicy, err := composePiWorkflowPolicy("workspace_write", "default", 1, []string{"publish_plan", "request_user_input_async"})
			if err != nil {
				t.Fatal(err)
			}
			f.initialPolicy = &rootPolicy
			switch scenario {
			case "protocol1":
				piAddWorkerDescriptor(t, s, 1)
			case "missing_role_contract":
				contractPath := filepath.Join(s.config.PackageRoot, "share/workspace-portal/runtime-contract.json")
				bytes, err := os.ReadFile(contractPath)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if json.Unmarshal(bytes, &fields) != nil {
					t.Fatal("contract fixture")
				}
				delete(fields, "piRoleInstructions")
				if err := writePiCreationRecord(contractPath, fields, 64<<10); err != nil {
					t.Fatal(err)
				}
			case "attachment_unavailable":
				ports := piAddUnavailableAttachment{f}
				s.config.PiCreation.Runtime = ports
				s.piWorkflows.runtime = ports
				s.piWorkflows.observations.attachments = map[agentbackend.SessionRef]*piWorkflowAttachment{}
			case "retained_two_tool_root":
				// The base settings fake implements only question/plan ports. Use
				// the existing address-scoped team fake and real composition so
				// current four-tool registration is independent of root's two.
				ports := &piAddedTeamFixture{piSettingsFixture: f, slots: map[string]*piSettingsFixture{}, creation: map[string]string{}, receipts: map[string]piruntime.SubmissionStatus{}}
				s.config.PiCreation.Runtime = ports
				s.piWorkflows = newPiWorkflowComposition(s)
				if s.piWorkflows == nil {
					t.Fatal("team workflow composition unavailable")
				}
				lifetime, cancel := context.WithCancel(context.Background())
				s.operationContext = lifetime
				t.Cleanup(func() { cancel(); s.operationWG.Wait() })
				f.idle = false
			}
			body := `{"action":"add","role":"implementer","model":"model","reasoningEffort":"high","messageId":"11111111-1111-4111-8111-111111111111"}`
			r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+slug+"/team", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", s.config.BaseURL)
			r.Header.Set("X-Workspace-Conversation-Scope", scope)
			r.Header.Set("X-Workspace-Pi-Generation", f.state.Generation)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if scenario == "retained_two_tool_root" {
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "report_to_lead") || !strings.Contains(w.Body.String(), "team_message") {
					t.Fatal("new member lost current declared tools")
				}
			} else {
				if w.Code == 200 {
					t.Fatal("unsupported construction reserved member")
				}
				after, _ := os.ReadFile(path)
				if string(after) != string(before) {
					t.Fatal("refused preflight changed roster")
				}
				if (scenario != "attachment_unavailable") != strings.Contains(w.Body.String(), "Upgrade this session’s Pi runtime before adding this member.") {
					t.Fatal("package vs temporary connection refusal conflated", w.Body.String())
				}
			}
			if f.creates != creates || f.sends != sends || !reflect.DeepEqual(f.initialPolicy, &rootPolicy) {
				t.Fatal("preflight performed native input or enlarged root ceiling")
			}
		})
	}
}
