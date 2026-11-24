package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

func workflowFixture(t *testing.T) (*piquestions.FileStore, piquestions.Caller, PiWorkflowAdmission) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-11-example", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "private-native-id"}
	c := piquestions.Caller{Handle: agentbackend.ExecutionHandle{Conversation: ref, Generation: "generation"}, Lineage: piquestions.LineageRef{AnchorEntryID: "anchor", SelectionToken: "private-branch-token"}, NativeToolCallID: "tool"}
	store := &piquestions.FileStore{Root: root, Validate: func(context.Context, agentbackend.ConversationRef, *piquestions.Caller) error { return nil }}
	resolve := func(context.Context, *http.Request, string) (agentbackend.ConversationRef, error) { return ref, nil }
	return store, c, resolve
}
func TestPiWorkflowQuestionsOriginBoundsAndObservationalReads(t *testing.T) {
	s, c, resolve := workflowFixture(t)
	q, err := s.Publish(context.Background(), c, piquestions.PublishInput{Questions: []piquestions.Question{{Title: "Which?"}}})
	if err != nil {
		t.Fatal(err)
	}
	h := PiQuestionHandler{Origin: "https://portal.example", Resolve: resolve, Store: s}
	in := piquestions.ResponseInput{ResponseID: piquestions.UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []piquestions.Answer{{QuestionID: "0", Text: "answer"}}}
	body, _ := json.Marshal(in)
	request := func(method, path, origin, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/pi-questions/responses", "https://evil.example", string(body)); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	if w := request("POST", "/pi-questions/responses", "https://portal.example", string(body)); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	if w := request("GET", "/pi-questions", "", ""); w.Code != 200 || strings.Contains(w.Body.String(), "private-") {
		t.Fatal(w.Code, w.Body)
	}
	if w := request("POST", "/pi-questions/responses", "https://portal.example", `{"responseId":null}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	if w := request("POST", "/pi-questions/responses", "https://portal.example", strings.Repeat("x", 65537)); w.Code != 413 {
		t.Fatal(w.Code, w.Body)
	}
}
func TestPiWorkflowPlanUnavailableWithoutPolicy(t *testing.T) {
	s, c, resolve := workflowFixture(t)
	plans := &piplan.FileStore{Root: s.Root, Validate: s.Validate}
	p, err := plans.PublishProposal(context.Background(), c, "exact plan", piplan.NativeOrigin{EntryID: "entry", Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	in := piplan.AcceptanceInput{AcceptanceID: piquestions.UUID(), PlanID: p.PlanID, Revision: 1, TextSHA256: p.TextSHA256}
	data, _ := json.Marshal(in)
	r := httptest.NewRequest("POST", "/pi-plan/acceptances", strings.NewReader(string(data)))
	r.Header.Set("Origin", "https://portal.example")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	(PiPlanHandler{Origin: "https://portal.example", Resolve: resolve, Store: plans}).ServeHTTP(w, r)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestPiWorkflowAdmittedOriginCaptureAndDuplicateUnavailable(t *testing.T) {
	s, c, resolve := workflowFixture(t)
	q, err := s.Publish(context.Background(), c, piquestions.PublishInput{Questions: []piquestions.Question{{Title: "Which?"}}})
	if err != nil {
		t.Fatal(err)
	}
	native := `{"questions":[{"title":"Changed"}]}`
	checks := 0
	admitted := piquestions.AdmittedStore{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Current: func(_ context.Context, c piquestions.Caller) (piquestions.Caller, error) { return c, nil }, VerifyOrigin: func(_ context.Context, _ piquestions.Caller, q piquestions.QuestionRecord) error {
		checks++
		return piquestions.VerifyNativeArguments(q, []byte(native))
	}}
	h := PiQuestionHandler{Origin: "https://portal.example", Resolve: resolve, Store: admitted}
	in := piquestions.ResponseInput{ResponseID: piquestions.UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []piquestions.Answer{{QuestionID: "0", Text: "answer"}}}
	body, _ := json.Marshal(in)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/pi-questions/responses", strings.NewReader(string(body)))
		r.Header.Set("Origin", h.Origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	native = `{"questions":[{"title":"Which?"}]}`
	if w := request(); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	admitted.Current = nil
	admitted.VerifyOrigin = nil
	admitted.Admit = nil
	h.Store = admitted
	if w := request(); w.Code != 202 || checks != 2 {
		t.Fatal(w.Code, w.Body, checks)
	}
	originalID := in.ResponseID
	in.ResponseID = piquestions.UUID()
	in.SupersedesResponseID = originalID
	body, _ = json.Marshal(in)
	if w := request(); w.Code != 501 {
		t.Fatal(w.Code, w.Body)
	}
}

// Question response IDs have a different contract from the delivery queue's
// 32-hex client IDs. Exercise the real handler/storage boundary before native
// fixtures attempt to use an ID or interpret a rejected POST as durable intent.
func TestPiWorkflowQuestionResponseIDContract(t *testing.T) {
	store, caller, resolve := workflowFixture(t)
	ctx := context.Background()
	q, err := store.Publish(ctx, caller, piquestions.PublishInput{Questions: []piquestions.Question{{Title: "Which?", Options: []piquestions.Option{{Label: "First"}, {Label: "Second"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := PiQuestionHandler{Origin: "https://portal.example", Resolve: resolve, Store: store}
	path := "/api/sessions/" + caller.Handle.Conversation.Session.Slug + "/pi-questions/responses"
	input := piquestions.ResponseInput{ResponseID: strings.Repeat("1", 32), QuestionID: q.ID, QuestionRevision: q.Revision, Answers: []piquestions.Answer{{QuestionID: q.Questions[0].ID, ChoiceID: q.Questions[0].Options[0].ID}}}
	request := func(method, path string, input any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(input)
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Origin", handler.Origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	file, err := (piquestions.StateFile{Root: store.Root, Kind: "pi-questions", Name: "questions.json", Ref: caller.Handle.Conversation}).Path()
	if err != nil {
		t.Fatal(err)
	}
	read := func() []byte {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	original := read()
	if response := request("POST", path, input); response.Code != 400 || strings.TrimSpace(response.Body.String()) != "Bad Request" {
		t.Fatal("32-hex response admitted", response.Code, response.Body.String())
	}
	// The real outer session router rejects this invalid status URL before the
	// question handler. A 404 here cannot prove a missing admitted response.
	response := httptest.NewRecorder()
	(&Server{}).sessionAPI(response, httptest.NewRequest("GET", path+"/"+input.ResponseID, nil))
	if response.Code != 404 || !bytes.Equal(original, read()) {
		t.Fatal("invalid response changed storage or routed", response.Code)
	}
	input.ResponseID = piquestions.UUID()
	if _, ok := sessionAPIPath(httptest.NewRequest("GET", path+"/"+input.ResponseID, nil).URL); !ok {
		t.Fatal("valid UUID status URL rejected")
	}
	response = request("POST", path, input)
	var accepted piquestions.ResponseView
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &accepted) != nil || !reflect.DeepEqual(accepted.ResponseInput, input) || accepted.Sequence != 1 || accepted.DeliveryPhase != "pending" {
		t.Fatal("valid response was not durably captured", response.Code, response.Body.String())
	}
	persisted := read()
	saved, err := store.Response(ctx, caller.Handle.Conversation, input.ResponseID)
	if err != nil || !reflect.DeepEqual(saved, accepted) {
		t.Fatal("HTTP response differs from saved original", saved, accepted, err)
	}
	response = request("GET", path+"/"+input.ResponseID, nil)
	var observed piquestions.ResponseView
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &observed) != nil || !reflect.DeepEqual(observed, accepted) {
		t.Fatal("original status identity changed", response.Code, response.Body.String())
	}
	response = request("POST", path, input)
	if response.Code != 202 || !bytes.Equal(persisted, read()) {
		t.Fatal("duplicate response was rewritten/resequenced", response.Code)
	}
	changed := input
	changed.QuestionRevision++
	if response := request("POST", path, changed); response.Code != 409 || !bytes.Equal(persisted, read()) {
		t.Fatal("changed original response accepted", response.Code)
	}
}
