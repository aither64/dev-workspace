package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type communicationTestClient struct{ codexController }

func (communicationTestClient) ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{}, errors.New("unexpected native read")
}
func (communicationTestClient) ReadThreadSettings(context.Context, string) (codex.ThreadSettings, error) {
	return codex.ThreadSettings{}, errors.New("unexpected native settings read")
}
func (communicationTestClient) ReadActivity(context.Context, string) (codex.ActivitySnapshot, error) {
	return codex.ActivitySnapshot{}, errors.New("unexpected activity read")
}
func (communicationTestClient) SendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, error) {
	return codex.SendReceipt{}, errors.New("unexpected native send")
}
func (communicationTestClient) ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{}, false, errors.New("unexpected reconciliation")
}

func TestCommunicationPortalApprovalAndRevocationFromEitherSide(t *testing.T) {
	s := newTestServer(t)
	s.config.Codex = communicationTestClient{}
	for _, slug := range []string{"a", "b"} {
		dir := filepath.Join(s.config.Workspace, "work", slug)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"plan.md": "fixture\n", "state.md": "---\nlifecycle: active\n---\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "portal.yml"), []byte(fmt.Sprintf("schema: 1\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: lead-%s\n", slug, slug)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	post := func(slug, body string) *httptest.ResponseRecorder {
		t.Helper()
		summary, err := session.Find(s.config.Workspace, slug)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+slug+"/communication", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.communicationAPI(w, r, summary)
		return w
	}
	if w := post("a", `{"action":"send","peer":"b","message":"hello","messageId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`); w.Code != http.StatusConflict {
		t.Fatalf("unapproved: %d %s", w.Code, w.Body)
	}
	if w := post("a", `{"action":"allow","peer":"b","approval":"forged browser evidence"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "User approved this pair in the portal from a") || strings.Contains(w.Body.String(), "forged browser evidence") {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	if w := post("b", `{"action":"send","peer":"a","message":"hello","messageId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`); w.Code != 200 {
		t.Fatalf("reverse: %d %s", w.Code, w.Body)
	}
	if w := post("b", `{"action":"revoke","peer":"a"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"canceled"`) {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := post("a", `{"action":"send","peer":"b","message":"hello","messageId":"cccccccccccccccccccccccccccccccc"}`); w.Code != http.StatusConflict {
		t.Fatalf("revoked: %d %s", w.Code, w.Body)
	}
	request := httptest.NewRequest(http.MethodPost, "https://workspace.example.test/api/sessions/a/communication", strings.NewReader(`{"action":"allow","peer":"b"}`))
	request.Header.Set("Origin", "https://foreign.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign origin accepted: %d", w.Code)
	}
}

func TestCommunicationTranscriptProjection(t *testing.T) {
	s := newTestServer(t)
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{
		{Kind: "userMessage", Text: "Cross-session message from 2026-10-28-peer to current (message aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa):\n\nShared result"},
		{Kind: "mcpToolCall", Summary: "Tool · team_reporting/session_communication"},
		{Kind: "userMessage", Text: "Ordinary user request"},
	}}
	s.presentTranscript(&transcript)
	for _, i := range []int{0, 1} {
		if transcript.Entries[i].Communication == nil || transcript.Entries[i].Communication.Kind != "session" {
			t.Fatalf("entry %d has no session presentation", i)
		}
	}
	if transcript.Entries[0].Communication.From != "2026-10-28-peer" || transcript.Entries[0].Communication.To != "current" {
		t.Fatal("peer labels lost")
	}
	if transcript.Entries[2].Communication != nil {
		t.Fatal("ordinary request reclassified")
	}
}
