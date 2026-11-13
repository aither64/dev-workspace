package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
