package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// Extend the owning creation/settings fake ports without replacing their
// creation/policy proof. Keep the original input separate from later messages.
type piCommunicationFixture struct {
	*piSettingsFixture
	original                          piruntime.SubmissionStatus
	receipts                          map[piruntime.SubmissionRef]piruntime.SubmissionStatus
	activity                          backend.ActivitySnapshot
	unloaded                          bool
	mismatchedSettings                bool
	wrongThread                       bool
	beforeSend                        func(backend.SendRequest)
	binds, reads, activities, lookups int
}

func (f *piCommunicationFixture) ReadMemberAdmission(_ context.Context, ref agentbackend.ConversationRef, _ string, input string) (piruntime.ConversationState, piruntime.SubmissionStatus, error) {
	if ref != f.state.Conversation || input != f.original.Ref.OperationID {
		return piruntime.ConversationState{}, piruntime.SubmissionStatus{}, piruntime.ErrScope
	}
	state := f.state
	state.Persisted = true
	return state, f.original, nil
}
func (f *piCommunicationFixture) Bind(ctx context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	f.binds++
	ops, err := f.piSettingsFixture.Bind(ctx, h)
	ops.Read, ops.Send, ops.Activity = f, f, f
	return ops, err
}
func (f *piCommunicationFixture) Read(ctx context.Context, id backend.Identity) (backend.Transcript, error) {
	f.reads++
	if f.unloaded {
		return backend.Transcript{}, piruntime.ErrUnavailable
	}
	transcript, err := f.piSettingsFixture.Read(ctx, id)
	if f.mismatchedSettings {
		transcript.Model = "foreign"
	}
	return transcript, err
}
func (f *piCommunicationFixture) ReadActivity(_ context.Context, id backend.Identity) (backend.ActivitySnapshot, error) {
	f.activities++
	activity := f.activity
	activity.ThreadID = id.ConversationID
	if f.wrongThread {
		activity.ThreadID = "foreign-native"
	}
	return activity, nil
}
func (f *piCommunicationFixture) Send(ctx context.Context, id backend.Identity, request backend.SendRequest) (backend.SendResult, error) {
	if f.beforeSend != nil {
		f.beforeSend(request)
	}
	result, err := f.piCreationRuntimeFixture.Send(ctx, id, request)
	f.receipts[f.submission.Ref] = *f.submission
	f.submission = &f.original
	return result, err
}
func (f *piCommunicationFixture) Lookup(_ context.Context, ref piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	f.lookups++
	if ref == f.original.Ref {
		return f.original, nil
	}
	if value, ok := f.receipts[ref]; ok {
		return value, nil
	}
	return piruntime.SubmissionStatus{}, os.ErrNotExist
}
func piCommunicationHTTPFixture(t *testing.T) (*Server, *piCommunicationFixture, communication.EndpointRecord, communication.EndpointRecord) {
	t.Helper()
	s, settings, _, _, _ := piSettingsHTTPFixture(t)
	f := &piCommunicationFixture{piSettingsFixture: settings, original: *settings.submission, receipts: map[piruntime.SubmissionRef]piruntime.SubmissionStatus{}, activity: backend.ActivitySnapshot{CurrentState: "idle"}}
	s.config.PiCreation.Runtime = f
	s.piWorkflows.runtime = f
	// initializePiCreation completed the owning durable proof; the settings
	// fixture leaves its in-memory creation receipt queued rather than running
	// the ordinary asynchronous creation wrapper.
	s.operationMu.Lock()
	receipt := s.creations[f.state.Conversation.Session.Slug]
	receipt.State = "ready"
	s.creations[f.state.Conversation.Session.Slug] = receipt
	s.operationMu.Unlock()
	s.config.Codex = communicationTestClient{}
	slug := "codex-peer"
	dir := filepath.Join(s.config.Workspace, "work", slug)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"state.md": "---\nlifecycle: active\n---\n", "plan.md": "fixture\n", "portal.yml": fmt.Sprintf("schema: 1\nslug: %s\ncodex:\n  thread_id: codex-peer-root\nrepositories: []\nartifacts: []\n", slug)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pi, err := communication.EndpointFor(s.config.Workspace, s.config.UserStateRoot, f.state.Conversation.Session.Slug, false)
	if err != nil {
		t.Fatal(err)
	}
	codex, err := communication.EndpointFor(s.config.Workspace, s.config.UserStateRoot, slug, false)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, pi, codex
}
func communicationFixtureScope(e communication.EndpointRecord) string {
	if e.Pi != nil {
		return PiConversationScope(e.Conversation())
	}
	return e.Codex.Incarnation
}
func postCommunicationFixture(s *Server, e communication.EndpointRecord, generation string, request communication.Request) *httptest.ResponseRecorder {
	data, _ := json.Marshal(request)
	r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+e.Slug()+"/communication", strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.config.BaseURL)
	r.Header.Set("X-Workspace-Conversation-Scope", communicationFixtureScope(e))
	if generation != "" {
		r.Header.Set("X-Workspace-Pi-Generation", generation)
	}
	w := httptest.NewRecorder()
	s.sessionAPI(w, r)
	return w
}
func TestPiCommunicationHTTPExactScopeApprovalAndDurableSteer(t *testing.T) {
	for _, activity := range []string{"idle", "working", "streaming"} {
		t.Run(activity, func(t *testing.T) {
			s, f, pi, codex := piCommunicationHTTPFixture(t)
			f.activity.CurrentState = activity
			baselineCreates, baselineSends, baselineEnsures := f.creates, f.sends, f.ensures
			approval := "  User allowed this exact pair; chat message 17  "
			allow := communication.Request{Action: "allow", Peer: pi.Slug(), Approval: approval}
			for _, bad := range []string{"", "pi-workflow-v1:" + strings.Repeat("f", 64), strings.Repeat("f", 64)} {
				data, _ := json.Marshal(allow)
				r := httptest.NewRequest("POST", "/api/sessions/"+codex.Slug()+"/communication", strings.NewReader(string(data)))
				if bad != "" {
					r.Header.Set("X-Workspace-Conversation-Scope", bad)
				}
				w := httptest.NewRecorder()
				s.sessionAPI(w, r)
				if w.Code == 200 {
					t.Fatal("foreign/missing Codex scope admitted")
				}
			}
			if w := postCommunicationFixture(s, codex, "", allow); w.Code != 200 || !strings.Contains(w.Body.String(), approval) {
				t.Fatal(w.Code, w.Body.String())
			}
			// Duplicate/missing Pi scope/generation cannot mutate even with a valid pair.
			request := communication.Request{Action: "send", Peer: codex.Slug(), MessageID: strings.Repeat("b", 32), Message: "reverse message"}
			for _, generation := range []string{"", strings.Repeat("f", 64)} {
				if w := postCommunicationFixture(s, pi, generation, request); w.Code == 200 {
					t.Fatal("invalid generation admitted")
				}
			}
			id := strings.Repeat("a", 32)
			send := communication.Request{Action: "send", Peer: pi.Slug(), MessageID: id, Message: "literal <quoted> peer text"}
			if w := postCommunicationFixture(s, codex, "", send); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}

			f.beforeSend = func(r backend.SendRequest) {
				if r.Delivery != "steer" || r.ActionContext != "pi-delivery-v1:steer" || r.Retry || r.ClientUserMessageID != id {
					t.Fatal("wrong native input", r)
				}
				data, err := os.ReadFile(filepath.Join(s.config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				st, err := communication.DecodePiCommunication(data)
				if err != nil || len(st.Messages) != 1 || st.Messages[0].Status != "submitting" || st.Messages[0].PiDelivery == nil || st.Messages[0].Envelope != r.Message {
					t.Fatal("native send before durable tuple", err)
				}
			}
			if err := s.dispatchCommunication(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			service, err := s.communicationService()
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.ReadEndpoint(context.Background(), pi)
			if err != nil || len(view.Messages) != 1 || view.Messages[0].Status != "delivered" || view.ValidatePublic() != nil {
				t.Fatal("typed view", view, err)
			}
			if w := postCommunicationFixture(s, pi, f.state.Generation, communication.Request{Action: "reply", Peer: codex.Slug(), MessageID: strings.Repeat("b", 32), ReplyTo: id, Message: "reply without another approval"}); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if f.creates != baselineCreates || f.ensures != baselineEnsures || f.sends != baselineSends+1 || f.configures != 0 {
				t.Fatal("unexpected native effect", f.creates, f.ensures, f.sends, f.configures)
			}
		})
	}
}
func TestPiCommunicationHTTPRecipientHoldsNeverSendOrLoad(t *testing.T) {
	for _, hold := range []string{"unloaded", "stale", "approval", "unclassified", "policy", "settings", "wrong-thread"} {
		t.Run(hold, func(t *testing.T) {
			s, f, pi, codex := piCommunicationHTTPFixture(t)
			if w := postCommunicationFixture(s, codex, "", communication.Request{Action: "allow", Peer: pi.Slug(), Approval: "user reference"}); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			id := strings.Repeat("a", 32)
			if w := postCommunicationFixture(s, codex, "", communication.Request{Action: "send", Peer: pi.Slug(), MessageID: id, Message: "original"}); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			creates, ensures, sends := f.creates, f.ensures, f.sends
			switch hold {
			case "unloaded":
				f.unloaded = true
			case "stale":
				f.activity.Stale = true
			case "approval":
				f.activity.WaitReason = "approval"
			case "unclassified":
				f.activity.CurrentState = "unclassified"
			case "policy":
				f.initialPolicy = nil
			case "settings":
				f.mismatchedSettings = true
			case "wrong-thread":
				f.wrongThread = true
			}
			if err := s.dispatchCommunication(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if f.creates != creates || f.ensures != ensures || f.sends != sends {
				t.Fatal("held recipient caused native effect")
			}
			service, err := s.communicationService()
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.ReadEndpoint(context.Background(), codex)
			if err != nil || view.Messages[0].Status != "held" {
				t.Fatal(view, err)
			}
		})
	}
}

func TestPiCommunicationHTTPRevokeUnavailableEndpoints(t *testing.T) {
	for _, hold := range []string{"held", "unloaded", "approval"} {
		for _, side := range []string{"source", "recipient"} {
			t.Run(hold+"-"+side, func(t *testing.T) {
				s, f, pi, codex := piCommunicationHTTPFixture(t)
				if w := postCommunicationFixture(s, codex, "", communication.Request{Action: "allow", Peer: pi.Slug(), Approval: "exact user approval; reference 17"}); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				capturedID, unsentID := strings.Repeat("a", 32), strings.Repeat("b", 32)
				f.piCreationRuntimeFixture.outcome = "Unknown"
				if w := postCommunicationFixture(s, codex, "", communication.Request{Action: "send", Peer: pi.Slug(), MessageID: capturedID, Message: "original captured input"}); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				if err := s.dispatchCommunication(context.Background(), capturedID); err != nil {
					t.Fatal(err)
				}
				if w := postCommunicationFixture(s, codex, "", communication.Request{Action: "send", Peer: pi.Slug(), MessageID: unsentID, Message: "known unsent input"}); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				path := filepath.Join(s.config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), "state.json")
				read := func() communication.PiCommunicationState {
					t.Helper()
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					st, err := communication.DecodePiCommunication(data)
					if err != nil {
						t.Fatal(err)
					}
					return st
				}
				before := read()
				captured := before.Messages[0]
				if len(before.Grants) != 1 || captured.PiDelivery == nil || captured.Status != "held" || before.Messages[1].Status != "pending" {
					t.Fatal("fixture lacks captured and unsent records")
				}
				switch hold {
				case "held":
					f.activity.CurrentState = "held"
				case "unloaded":
					f.unloaded = true
				case "approval":
					f.activity.WaitReason = "approval"
				}
				from, to, generation := codex, pi, ""
				if side == "source" {
					from, to, generation = pi, codex, f.state.Generation
				}
				revoke := communication.Request{Action: "revoke", Peer: to.Slug()}
				// Removing a grant still requires this exact current source and pair. Every
				// refusal must leave authorization and original captured bytes unchanged.
				for _, bad := range []string{"missing-scope", "wrong-scope", "duplicate-scope", "wrong-generation", "wrong-pair"} {
					request := revoke
					if bad == "wrong-pair" {
						request.Peer = "missing-peer"
					}
					data, _ := json.Marshal(request)
					r := httptest.NewRequest("POST", "/api/sessions/"+from.Slug()+"/communication", strings.NewReader(string(data)))
					r.Header.Set("Origin", s.config.BaseURL)
					r.Header.Set("Content-Type", "application/json")
					if bad != "missing-scope" {
						r.Header.Set("X-Workspace-Conversation-Scope", communicationFixtureScope(from))
					}
					if bad == "wrong-scope" {
						r.Header.Set("X-Workspace-Conversation-Scope", "foreign-scope")
					}
					if bad == "duplicate-scope" {
						r.Header.Add("X-Workspace-Conversation-Scope", communicationFixtureScope(from))
					}
					if generation != "" {
						r.Header.Set("X-Workspace-Pi-Generation", generation)
					}
					if bad == "wrong-generation" {
						r.Header.Set("X-Workspace-Pi-Generation", strings.Repeat("f", 64))
					}
					w := httptest.NewRecorder()
					s.sessionAPI(w, r)
					if w.Code == 200 {
						t.Fatal("invalid revoke admitted", bad)
					}
					if !reflect.DeepEqual(before, read()) {
						t.Fatal("refused revoke changed grant/input", bad)
					}
				}
				creates, ensures, sends, configures := f.creates, f.ensures, f.sends, f.configures
				binds, reads, activities, verifies, lookups := f.binds, f.reads, f.activities, f.verifications, f.lookups
				if w := postCommunicationFixture(s, from, generation, revoke); w.Code != 200 {
					t.Fatalf("unavailable %s must remain revocable: %d %s", side, w.Code, w.Body.String())
				}
				after := read()
				if len(after.Grants) != 1 || after.Grants[0].ID != before.Grants[0].ID || after.Grants[0].RevokedAt == nil || after.Messages[1].Status != "canceled" || !reflect.DeepEqual(captured, after.Messages[0]) {
					t.Fatal("revoke changed captured tuple or retained unsent authorization")
				}
				if f.binds != binds || f.reads != reads || f.activities != activities || f.verifications != verifies || f.lookups != lookups {
					t.Fatal("revoke requested native readiness/receipt proof")
				}
				// The existing lookup path can still confirm only the captured original,
				// while the known unsent message remains canceled even after a second visit.
				saved := f.receipts[captured.PiDelivery.Input.Ref()]
				saved.Phase = "Accepted"
				saved.Witness = &piruntime.NativeWitness{SessionID: pi.Pi.NativeID, EntryID: "original-consumed-entry", EffectiveDigest: piruntime.MessageDigest(captured.Envelope)}
				f.receipts[saved.Ref] = saved
				if err := s.dispatchCommunication(context.Background(), capturedID); err != nil {
					t.Fatal(err)
				}
				if err := s.dispatchCommunication(context.Background(), unsentID); err != nil {
					t.Fatal(err)
				}
				reconciled := read().Messages[0]
				if reconciled.Status != "delivered" || reconciled.Envelope != captured.Envelope || reconciled.PiDelivery.Input != captured.PiDelivery.Input || reconciled.PiDelivery.Settings != captured.PiDelivery.Settings || reconciled.PiDelivery.Generation != captured.PiDelivery.Generation || f.lookups != lookups+1 {
					t.Fatal("revocation lost original lookup identity")
				}
				if f.creates != creates || f.ensures != ensures || f.sends != sends || f.configures != configures {
					t.Fatal("revocation/recovery loaded,created,configured or resent input")
				}
			})
		}
	}
}
