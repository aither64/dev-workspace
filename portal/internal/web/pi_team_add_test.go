package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestPiTeamHostCallbackPreservesKnownPreSendRefusal(t *testing.T) {
	for _, input := range []string{"initial_uuid", "compact_steer"} {
		t.Run(input, func(t *testing.T) {
			testPiTeamHostCallbackPreservesKnownPreSendRefusal(t, input == "compact_steer")
		})
	}
}

func testPiTeamHostCallbackPreservesKnownPreSendRefusal(t *testing.T, compactSteer bool) {
	t.Helper()
	for _, failure := range []string{"initial_conflict", "cancel_initial_conflict", "same_initial", "policy_held", "create_unknown", "proof_unknown", "cancel_after_create", "send_unknown", "accepted"} {
		t.Run(failure, func(t *testing.T) {
			s, root, scope := newPiAddFixture(t)
			bindings := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}
			slug := root.state.Conversation.Session.Slug
			before, err := bindings.Load(slug)
			if err != nil {
				t.Fatal(err)
			}
			root.state.Persisted = true
			policy, err := composePiWorkflowPolicy("workspace_write", "default", 1, []string{"publish_plan", "request_user_input_async"})
			if err != nil {
				t.Fatal(err)
			}
			root.initialPolicy = &policy
			ports := &piCallbackTeamFixture{piAddedTeamFixture: &piAddedTeamFixture{piSettingsFixture: root,
				slots: map[string]*piSettingsFixture{}, creation: map[string]string{"lead": before.CreationReceiptID}, receipts: map[string]piruntime.SubmissionStatus{}}, failure: failure}
			s.config.PiCreation.Runtime = ports
			s.piWorkflows = newPiWorkflowComposition(s)
			lifetime, stop := context.WithCancel(context.Background())
			s.operationContext = lifetime
			t.Cleanup(func() { stop(); s.operationWG.Wait() })
			request := teamRequestBody{Action: "add", Role: "implementer", Model: "model", Effort: "high", MessageID: "11111111-1111-4111-8111-111111111111"}
			data, _ := json.Marshal(request)
			r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+slug+"/team", strings.NewReader(string(data)))
			r.Header.Set("Origin", s.config.BaseURL)
			r.Header.Set("X-Workspace-Conversation-Scope", scope)
			r.Header.Set("X-Workspace-Pi-Generation", root.state.Generation)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatal("reserve member", w.Code, w.Body.String())
			}
			store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			reserved, err := store.LoadPi(slug)
			if err != nil || len(reserved.Members) != 1 {
				t.Fatal(reserved, err)
			}
			member := reserved.Members[0]
			handle := agentbackend.ExecutionHandle{Conversation: root.state.Conversation, Generation: root.state.Generation}
			bootstrap := root.submission
			if bootstrap == nil || bootstrap.Phase != "Accepted" || len(bootstrap.Ref.OperationID) != 36 || bootstrap.Witness == nil {
				t.Fatal("missing original Accepted UUID input", bootstrap)
			}
			originalBootstrap := *bootstrap
			callbackInput := originalBootstrap
			if compactSteer {
				// The existing fake Send port records a separate Accepted steer
				// receipt and restores bootstrap. This proves the adapter consumer,
				// not native enqueue or authenticated origin verification.
				operations, err := ports.Bind(context.Background(), handle)
				if err != nil {
					t.Fatal(err)
				}
				request := backend.SendRequest{ClientUserMessageID: "abcdef0123456789abcdef0123456789", Message: "later compact steer input", Delivery: "steer", ActionContext: "pi-delivery-v1:steer"}
				if result, err := operations.Send.Send(context.Background(), piruntime.BackendIdentity(handle), request); err != nil || result.Outcome != backend.Accepted {
					t.Fatal("compact steer fixture input", result, err)
				}
				callbackInput = ports.receipts[request.ClientUserMessageID]
				if callbackInput.Witness == nil {
					t.Fatal("compact input has no Accepted witness")
				}
				witness := *callbackInput.Witness
				witness.EntryID = "compact-steer-user-entry"
				callbackInput.Witness = &witness
				ports.receipts[request.ClientUserMessageID] = callbackInput
				observed, err := ports.Lookup(context.Background(), callbackInput.Ref)
				input := piruntime.SendInput{Message: request.Message, ActionContext: request.ActionContext}
				if err != nil || !reflect.DeepEqual(observed, callbackInput) || observed.Phase != "Accepted" || observed.Ref.Conversation != handle.Conversation || observed.Ref.OperationID != request.ClientUserMessageID || observed.Ref.Action != "send" || observed.Input != input || observed.Settings != root.state.Settings || observed.Generation != handle.Generation || observed.Ref.Digest != piruntime.SubmissionDigest(observed.Ref, input, observed.Settings) || observed.Witness.SessionID != handle.Conversation.NativeID || observed.Witness.EntryID == bootstrap.Witness.EntryID || observed.Witness.EffectiveDigest != piruntime.MessageDigest(input.Message) {
					t.Fatal("compact original input receipt changed", observed, err)
				}
			}
			if failure == "initial_conflict" || failure == "cancel_initial_conflict" || failure == "same_initial" {
				initialID := "44444444-4444-4444-8444-444444444444"
				if failure == "same_initial" {
					initialID = "33333333-3333-4333-8333-333333333333"
				}
				caller := teamruntime.PiCaller{Runtime: root.runtime, Handle: handle}
				message := teamruntime.PiMessage{ID: initialID, To: member.Address, Text: "one original assignment"}
				if _, err := store.ReservePiInitialAssignment(context.Background(), s, caller, reserved.Revision, message, member.Addition.Selection.Settings.Effective()); err != nil {
					t.Fatal("reserve original initial ID", err)
				}
				reserved, err = store.LoadPi(slug)
				if err != nil {
					t.Fatal(err)
				}
				member = reserved.Members[0]
			}
			rootCreates, rootSends := root.creates, root.sends
			args, _ := json.Marshal(map[string]string{"message_id": "33333333-3333-4333-8333-333333333333", "to": member.Address, "message": "one original assignment"})
			digest := sha256.Sum256(args)
			call := piruntime.HostCall{Runtime: root.runtime, Subject: piruntime.CallbackSubject{Kind: "conversation", Conversation: &handle},
				InputOperationID: callbackInput.Ref.OperationID, AnchorEntryID: "anchor", NativeToolCallID: "tool", SelectionToken: "selection", Tool: "team_message", Arguments: args, ArgumentsSHA256: hex.EncodeToString(digest[:])}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ports.cancel = cancel
			host := &piWorkflowHostHandler{owner: s.piWorkflows, attempt: &piWorkflowAttachment{runtime: root.runtime}}
			if failure == "accepted" {
				bindingBefore, err := bindings.Load(slug)
				if err != nil {
					t.Fatal(err)
				}
				for _, invalid := range []struct {
					name   string
					change func(*piruntime.HostCall)
				}{
					{"malformed_input", func(c *piruntime.HostCall) { c.InputOperationID = "abcdef0123456789abcdef012345678x" }},
					{"uppercase_compact", func(c *piruntime.HostCall) { c.InputOperationID = "ABCDEF0123456789ABCDEF0123456789" }},
					{"uppercase_uuid", func(c *piruntime.HostCall) { c.InputOperationID = "ABCDEF01-2345-6789-ABCD-EF0123456789" }},
					{"padded_input", func(c *piruntime.HostCall) { c.InputOperationID = " " + c.InputOperationID + " " }},
					{"runtime", func(c *piruntime.HostCall) { c.Runtime.Generation += "wrong" }},
					{"subject", func(c *piruntime.HostCall) { c.Subject.Kind = "utility" }},
					{"recipient", func(c *piruntime.HostCall) { c.Subject.Conversation.Conversation.Session.Slug += "-other" }},
					{"generation", func(c *piruntime.HostCall) { c.Subject.Conversation.Generation += "wrong" }},
					{"tool", func(c *piruntime.HostCall) { c.Tool = "unsupported_tool" }},
					{"argument_digest", func(c *piruntime.HostCall) { c.ArgumentsSHA256 = strings.Repeat("0", 64) }},
					{"native_tool", func(c *piruntime.HostCall) { c.NativeToolCallID = "" }},
					{"anchor", func(c *piruntime.HostCall) { c.AnchorEntryID = "" }},
					{"selection", func(c *piruntime.HostCall) { c.SelectionToken = "" }},
				} {
					t.Run(invalid.name, func(t *testing.T) {
						bad, badHandle := call, handle
						bad.Subject.Conversation = &badHandle
						invalid.change(&bad)
						if reply, err := host.HandleHostCall(ctx, bad); err == nil || reply.Outcome != "refused" {
							t.Fatal("invalid original input/scope admitted", reply, err)
						}
						roster, rosterErr := store.LoadPi(slug)
						binding, bindingErr := bindings.Load(slug)
						if rosterErr != nil || bindingErr != nil || !reflect.DeepEqual(roster, reserved) || !reflect.DeepEqual(binding, bindingBefore) || len(ports.slots) != 0 || ports.sendCalls != 0 || root.creates != rootCreates || root.sends != rootSends {
							t.Fatal("refused callback reached domain effects", rosterErr, bindingErr)
						}
					})
				}
			}
			// Snapshot the actual domain records after fixture reservation, including
			// file identity: an atomic same-bytes rewrite is still a record write.
			type savedRecord struct {
				body string
				info os.FileInfo
			}
			snapshot := func() map[string]savedRecord {
				t.Helper()
				records := map[string]savedRecord{}
				if err := filepath.WalkDir(s.config.UserStateRoot, func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !entry.Type().IsRegular() || strings.HasSuffix(path, ".lock") {
						return nil
					}
					body, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					records[path] = savedRecord{string(body), info}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return records
			}
			var saved map[string]savedRecord
			bindingBeforeCall, err := bindings.Load(slug)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "initial_conflict" || failure == "cancel_initial_conflict" {
				saved = snapshot()
			}
			if failure == "cancel_initial_conflict" {
				cancel()
			}
			reply, callErr := host.HandleHostCall(ctx, call)
			var result struct {
				teamruntime.PiDeliveryResult
				Stage, Reason, Materialization string
			}
			if failure == "initial_conflict" || failure == "cancel_initial_conflict" {
				if failure == "initial_conflict" {
					if callErr != nil || reply.Outcome != "result" || json.Unmarshal(reply.Body, &result) != nil || result.Disposition != teamruntime.PiUnavailable || result.Stage != "initial_reservation" || result.Reason != "refused" || result.Materialization != "unattempted" || result.Native != nil {
						t.Fatal("immutable initial ID conflict became transport Unknown", reply, result, callErr)
					}
				} else if callErr == nil {
					t.Fatal("cancellation must retain transport uncertainty", reply)
				}
				after := snapshot()
				if len(saved) != len(after) {
					t.Fatal("conflict created or removed a record")
				}
				for path, before := range saved {
					got, ok := after[path]
					if !ok || before.body != got.body || !os.SameFile(before.info, got.info) || before.info.Mode() != got.info.Mode() || !before.info.ModTime().Equal(got.info.ModTime()) {
						t.Fatal("conflict rewrote record", filepath.Base(path))
					}
				}
				binding, bindingErr := bindings.Load(slug)
				roster, rosterErr := store.LoadPi(slug)
				if bindingErr != nil || rosterErr != nil || !reflect.DeepEqual(binding, bindingBeforeCall) || !reflect.DeepEqual(roster, reserved) || len(ports.slots) != 0 || ports.sendCalls != 0 || root.creates != rootCreates || root.sends != rootSends || root.held || root.pending || !reflect.DeepEqual(root.submission, &originalBootstrap) {
					t.Fatal("initial conflict changed domain or native state", bindingErr, rosterErr, ports.sendCalls)
				}
				if compactSteer {
					if got, err := ports.Lookup(context.Background(), callbackInput.Ref); err != nil || !reflect.DeepEqual(got, callbackInput) {
						t.Fatal("conflict changed caller receipt", got, err)
					}
				}
				return
			}
			if failure == "policy_held" {
				if callErr != nil || reply.Outcome != "result" || json.Unmarshal(reply.Body, &result) != nil || result.Disposition != teamruntime.PiHeld || result.Stage != "recipient_policy" || result.Reason != "held" || result.Materialization != "created_empty" {
					t.Fatal("known refusal became unknown", reply, result, callErr)
				}
			} else if failure == "send_unknown" {
				if callErr != nil || reply.Outcome != "result" || json.Unmarshal(reply.Body, &result) != nil || result.Outcome != backend.Unknown || result.Disposition != teamruntime.PiAttempted || result.Stage != "" {
					t.Fatal("Send uncertainty relabeled", reply, callErr)
				}
			} else if failure == "accepted" || failure == "same_initial" {
				if callErr != nil || reply.Outcome != "result" || json.Unmarshal(reply.Body, &result) != nil || result.Outcome != backend.Accepted {
					t.Fatal("accepted callback", reply, callErr)
				}
			} else if callErr == nil {
				t.Fatal("uncertain effect must retain transport Unknown", reply)
			}
			retained, err := store.LoadPi(slug)
			if err != nil {
				t.Fatal(err)
			}
			got := retained.Members[0]
			if got.Incarnation != member.Incarnation || got.CreationID != member.CreationID || got.InitialInputID != "33333333-3333-4333-8333-333333333333" {
				t.Fatal("original reservation replaced", got)
			}
			target := ports.slots[member.Address]
			if target == nil || target.creates != 1 {
				t.Fatal("lost real empty-member effect")
			}
			if failure != "accepted" && failure != "same_initial" && failure != "send_unknown" && (ports.sendCalls != 0 || target.sends != 0 || got.State != "creating") {
				t.Fatal("preSend refusal dispatched or published", ports.sendCalls, target.sends, got.State)
			}
			if root.held || root.pending || !reflect.DeepEqual(root.initialPolicy, &policy) {
				t.Fatal("caller hold/settings/tool ceiling changed")
			}
			if root.submission != bootstrap || !reflect.DeepEqual(root.submission, &originalBootstrap) || root.creates != rootCreates || root.sends != rootSends || call.InputOperationID != callbackInput.Ref.OperationID {
				t.Fatal("callback replaced or redispatched the caller's original input")
			}
			if compactSteer {
				if observed, err := ports.Lookup(context.Background(), callbackInput.Ref); err != nil || !reflect.DeepEqual(observed, callbackInput) {
					t.Fatal("callback changed compact steer receipt", observed, err)
				}
			}
			if failure == "accepted" || failure == "same_initial" {
				if ports.sendCalls != 1 || target.sends != 1 || result.Native == nil || result.Native.Receipt.ClientUserMessageID != got.InitialInputID {
					t.Fatal("accepted callback changed input identity or repeated Send", ports.sendCalls, target.sends, result)
				}
				// Re-enquire the same original ID; publication never requires a new Send.
				if again, err := host.HandleHostCall(ctx, call); err != nil || again.Outcome != "result" || target.sends != 1 {
					t.Fatal("accepted replay", again, err, target.sends)
				}
			} else if failure == "policy_held" {
				if _, _, err := ports.ReadMemberAdmission(ctx, target.state.Conversation, member.CreationID, got.InitialInputID); err == nil {
					t.Fatal("refusal invented an input receipt")
				}
			}
		})
	}
}

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
