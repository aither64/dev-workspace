package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piplan"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

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
