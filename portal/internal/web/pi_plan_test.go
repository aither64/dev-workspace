package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func TestPiPlanSnapshotRendersRetainedProposalsWithoutChangingExactContent(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	questions, caller, resolve := workflowFixture(t)
	policyCalls, modeCalls := 0, 0
	plans := &piplan.FileStore{Root: questions.Root, Validate: questions.Validate, Policy: func(context.Context, agentbackend.ConversationRef, piplan.Caller) (piplan.Policy, error) {
		policyCalls++
		return piplan.Policy{OriginalMode: "plan", Revision: "proof", Idle: true, Eligible: true, Enforced: true}, nil
	}}
	ctx, ref := context.Background(), caller.Handle.Conversation
	text := "# Plan é\r\n\r\n- **Exact** [safe](https://example.test)\r\n\r\n```sh\r\nprintf '<sample>'\r\n```\r\n\r\n<script>alert(1)</script>\r\n<img src=x onerror=alert(1)>\r\n[unsafe](javascript:alert(1))\r\n  "
	first, err := plans.PublishProposal(ctx, caller, text, piplan.NativeOrigin{EntryID: "first-entry", Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	acceptance, err := plans.Accept(ctx, ref, piplan.AcceptanceInput{AcceptanceID: piquestions.UUID(), PlanID: first.PlanID, Revision: first.Revision, TextSHA256: first.TextSHA256})
	if err != nil {
		t.Fatal(err)
	}
	caller.NativeToolCallID = "second-publication"
	second, err := plans.PublishProposal(ctx, caller, "# Amendment\r\n\r\n| Step | Result |\r\n| --- | --- |\r\n| 1 | 保留 |\r\n", piplan.NativeOrigin{EntryID: "second-entry", Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	want, err := plans.Snapshot(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	path, err := (piquestions.StateFile{Root: plans.Root, Kind: "pi-plans", Name: "plans.json", Ref: ref}).Path()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforePolicyCalls := policyCalls
	allowed, renderCalls := true, 0
	h := PiPlanHandler{Origin: "https://portal.example", Store: plans, Resolve: func(ctx context.Context, r *http.Request, op string) (agentbackend.ConversationRef, error) {
		if !allowed || op != "pi-plan.read" {
			return ref, errors.New("scope unavailable")
		}
		return resolve(ctx, r, op)
	}, Modes: modeSetter(func(context.Context, agentbackend.ConversationRef, piplan.ModeInput) (piplan.ModeResult, error) {
		modeCalls++
		return piplan.ModeResult{}, errors.New("GET invoked a mode action")
	}), RenderTextMarkdown: func(text string) template.HTML {
		renderCalls++
		return server.renderTextMarkdown(text)
	}}
	read := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/pi-plan", nil))
		return w
	}
	w := read()
	var projected struct {
		piplan.Snapshot
		Proposals []piPlanProposalView `json:"proposals"`
	}
	var unchanged piplan.Snapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &projected) != nil || json.Unmarshal(w.Body.Bytes(), &unchanged) != nil || !reflect.DeepEqual(unchanged, want) {
		t.Fatalf("snapshot projection changed identity or authority: %d %s", w.Code, w.Body)
	}
	if len(projected.Proposals) != 2 || len(projected.Acceptances) != 1 || projected.Proposals[0].PlanID != first.PlanID || projected.Proposals[1].PlanID != second.PlanID || projected.Acceptances[0] != acceptance {
		t.Fatal("publication/acceptance order changed", projected)
	}
	if projected.Proposals[0].Text != text || projected.Proposals[0].TextSHA256 != piplan.Digest(text) || first.TextSHA256 == piplan.Digest(strings.ReplaceAll(text, "\r\n", "\n")) {
		t.Fatal("canonical text was normalized")
	}
	html := string(projected.Proposals[0].HTML) + string(projected.Proposals[1].HTML)
	for _, marker := range []string{"<h1>", "<ul>", "<strong>", `<a href="https://example.test"`, "<pre><code", "<table>"} {
		if !strings.Contains(html, marker) {
			t.Fatalf("shared Markdown omitted %q: %s", marker, html)
		}
	}
	for _, unsafe := range []string{"<script", "onerror=", `href="javascript:`, "private-"} {
		if strings.Contains(html, unsafe) || strings.Contains(w.Body.String(), "private-") {
			t.Fatalf("unsafe/private projection: %s", w.Body)
		}
	}
	h.RenderTextMarkdown = nil
	w = read()
	if w.Code != 200 || strings.Contains(w.Body.String(), `"html"`) || !strings.Contains(w.Body.String(), first.TextSHA256) {
		t.Fatal("missing renderer did not preserve plain text fallback", w.Code, w.Body)
	}
	allowed = false
	if denied := read(); denied.Code != 403 {
		t.Fatal("projection bypassed admission", denied.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || policyCalls != beforePolicyCalls || modeCalls != 0 || renderCalls != 2 {
		t.Fatal("GET changed stored/native state", err, policyCalls, modeCalls, renderCalls)
	}
}

type modeSetter func(context.Context, agentbackend.ConversationRef, piplan.ModeInput) (piplan.ModeResult, error)

func (f modeSetter) SetMode(ctx context.Context, ref agentbackend.ConversationRef, in piplan.ModeInput) (piplan.ModeResult, error) {
	return f(ctx, ref, in)
}

type modeSnapshotStore struct{ *piplan.FileStore }

func (modeSnapshotStore) Snapshot(context.Context, agentbackend.ConversationRef) (piplan.Snapshot, error) {
	return piplan.Snapshot{CanSelectMode: true}, nil
}

func TestPiWorkflowManualModePermissionsBoundsAndOutcomes(t *testing.T) {
	s, caller, _ := workflowFixture(t)
	ref := caller.Handle.Conversation
	calls := 0
	allow := true
	resolve := func(_ context.Context, _ *http.Request, op string) (agentbackend.ConversationRef, error) {
		if op != "pi-plan.mode" || !allow {
			return ref, errors.New("manual mode permission required")
		}
		return ref, nil
	}
	h := PiPlanHandler{Origin: "https://portal.example", Resolve: resolve, Store: &piplan.FileStore{Root: s.Root}}
	want := piplan.ModeResult{Outcome: "applied", Policy: &piplan.ModeView{Mode: "default", Revision: 2, Digest: strings.Repeat("b", 64)}}
	var effectErr error
	h.Modes = modeSetter(func(_ context.Context, got agentbackend.ConversationRef, in piplan.ModeInput) (piplan.ModeResult, error) {
		calls++
		if got != ref || in.Mode != "default" || in.ExpectedRevision != 1 || in.ExpectedDigest != strings.Repeat("a", 64) {
			t.Fatal(got, in)
		}
		return want, effectErr
	})
	body := `{"mode":"default","expectedRevision":1,"expectedDigest":"` + strings.Repeat("a", 64) + `"}`
	request := func(origin, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/pi-plan/mode", strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("https://evil.example", body); w.Code != 403 || calls != 0 {
		t.Fatal(w.Code, calls)
	}
	allow = false
	if w := request(h.Origin, body); w.Code != 403 || calls != 0 {
		t.Fatal(w.Code, calls)
	}
	allow = true
	for _, bad := range []string{`{"mode":null}`, `{"mode":"default","expectedRevision":1.5}`, strings.Replace(body, `"expectedRevision":1`, `"expectedRevision":9007199254740992`, 1), strings.Replace(body, `"mode":"default"`, `"mode":"default","extra":true`, 1), strings.Replace(body, `"mode":"default"`, `"mode":"plan","mode":"default"`, 1)} {
		if w := request(h.Origin, bad); w.Code != 400 || calls != 0 {
			t.Fatal(w.Code, w.Body, calls)
		}
	}
	if w := request(h.Origin, strings.Repeat("x", 65537)); w.Code != 413 || calls != 0 {
		t.Fatal(w.Code, calls)
	}
	for _, tc := range []struct {
		outcome string
		policy  *piplan.ModeView
		code    int
	}{{"applied", want.Policy, 200}, {"rejected", nil, 409}, {"unknown", nil, 202}, {"applied", nil, 202}, {"nonsense", want.Policy, 202}} {
		want = piplan.ModeResult{Outcome: tc.outcome, Policy: tc.policy}
		w := request(h.Origin, body)
		if w.Code != tc.code {
			t.Fatal(tc, w.Code, w.Body)
		}
		var result piplan.ModeResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || tc.code == 202 && (result.Outcome != "unknown" || result.Policy != nil) {
			t.Fatal(result, err)
		}
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{piquestions.ErrInvalid, 400}, {piquestions.ErrConflict, 409}, {piquestions.ErrUnavailable, 501}, {piruntime.ErrHeld, 503}} {
		effectErr = tc.err
		if w := request(h.Origin, body); w.Code != tc.code {
			t.Fatal(tc, w.Code)
		}
	}
	h.Modes = nil
	if w := request(h.Origin, body); w.Code != 501 {
		t.Fatal(w.Code)
	}
}

func TestPiWorkflowMissingModeActionSuppressesObservationalControl(t *testing.T) {
	_, _, resolve := workflowFixture(t)
	h := PiPlanHandler{Origin: "https://portal.example", Resolve: resolve, Store: modeSnapshotStore{}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/pi-plan", nil))
	var snapshot piplan.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || w.Code != 200 || snapshot.CanSelectMode {
		t.Fatal(w.Code, snapshot, err)
	}
}
