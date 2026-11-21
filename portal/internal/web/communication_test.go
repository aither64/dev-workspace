package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
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
		identity, err := communication.IdentityFor(s.config.Workspace, slug, false)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-Workspace-Conversation-Scope", identity.Incarnation)
		w := httptest.NewRecorder()
		s.communicationAPI(w, r, summary)
		return w
	}
	if w := post("a", `{"action":"send","peer":"b","message":"hello","messageId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`); w.Code != http.StatusConflict {
		t.Fatalf("unapproved: %d %s", w.Code, w.Body)
	}
	if w := post("a", `{"action":"allow","peer":"b","approval":"  exact user approval; chat reference 17  "}`); w.Code != 200 || !strings.Contains(w.Body.String(), "  exact user approval; chat reference 17  ") {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	if w := post("b", `{"action":"send","peer":"a","message":"**hello** <script>alert(1)</script>","messageId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`); w.Code != 200 {
		t.Fatalf("reverse: %d %s", w.Code, w.Body)
	}
	summary, err := session.Find(s.config.Workspace, "a")
	if err != nil {
		t.Fatal(err)
	}
	history := httptest.NewRecorder()
	s.communicationAPI(history, httptest.NewRequest(http.MethodGet, "/api/sessions/a/communication", nil), summary)
	var presented struct {
		Messages []struct {
			Text string `json:"text"`
			HTML string `json:"html"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &presented); err != nil || len(presented.Messages) != 1 {
		t.Fatal(history.Body.String(), err)
	}
	message := presented.Messages[0]
	if message.Text != "**hello** <script>alert(1)</script>" || !strings.Contains(message.HTML, "<strong>hello</strong>") || strings.Contains(message.HTML, "<script") {
		t.Fatal("communication Markdown was lost or unsafe", message)
	}
	if w := post("b", `{"action":"revoke","peer":"a"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"canceled"`) {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := post("a", `{"action":"send","peer":"b","message":"hello","messageId":"cccccccccccccccccccccccccccccccc"}`); w.Code != http.StatusConflict {
		t.Fatalf("revoked: %d %s", w.Code, w.Body)
	}
	if w := post("a", `{"action":"allow","peer":"b"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "User approved this pair in the portal from a") {
		t.Fatalf("browser approval: %d %s", w.Code, w.Body)
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

func TestCommunicationPresentationComposition(t *testing.T) {
	s, native, pi, codexEndpoint := piCommunicationHTTPFixture(t)
	otherPiValue := *pi.Pi
	otherPiValue.Session.Slug = "pi-other"
	otherPiValue.Session.Incarnation = strings.Repeat("b", 64)
	otherPiValue.NativeID = "pi-other-native"
	otherPi := communication.EndpointRecord{Pi: &otherPiValue}
	otherCodex := communication.EndpointRecord{Codex: &communication.Identity{Slug: "codex-other", Thread: "codex-other-root", Incarnation: strings.Repeat("c", 64)}}
	created := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	state := communication.PiCommunicationState{Schema: 2, Workspace: s.config.Workspace, Grants: []communication.PiGrantRecord{}, Messages: []communication.PiMessageRecord{}}
	for index, pair := range [][2]communication.EndpointRecord{{pi, codexEndpoint}, {pi, otherPi}, {codexEndpoint, otherCodex}} {
		grant := communication.PiGrantRecord{ID: fmt.Sprintf("%032x", index+1), A: pair[0], B: pair[1], ApprovedBy: pair[0].Slug(), Approval: "Fixture approval", CreatedAt: created}
		state.Grants = append(state.Grants, grant)
		for direction := 0; direction < 2; direction++ {
			state.Messages = append(state.Messages, communication.PiMessageRecord{ID: fmt.Sprintf("%032x", 10+2*index+direction), GrantID: grant.ID,
				From: pair[direction], To: pair[1-direction], Text: "**Exact é** <script>unsafe()</script>", Status: "held", CreatedAt: created.Add(time.Duration(2*index+direction) * time.Minute)})
		}
	}
	if err := state.Validate(); err != nil {
		t.Fatal(err)
	}
	// Seed retained domain records without submitting or dispatching any input.
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "state.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(data)
	creates, sends, configures, binds, reads := native.creates, native.sends, native.configures, native.binds, native.reads
	asset := httptest.NewRecorder()
	s.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/codex/assets/conversation.js", nil))
	if asset.Code != http.StatusOK {
		t.Fatal("shared conversation asset unavailable")
	}
	pages := []map[string]any{}
	for _, endpoint := range []communication.EndpointRecord{pi, codexEndpoint} {
		kind, thread := "codex", codexEndpoint.Codex.Thread
		if endpoint.Pi != nil {
			kind, thread = "pi", endpoint.Pi.NativeID
		}
		summary, err := session.Find(s.config.Workspace, endpoint.Slug())
		if err != nil {
			t.Fatal(err)
		}
		summary.ObservationIdentity, err = session.ObservationIdentity(summary.Workspace, summary.Slug, filepath.Join(summary.Workspace, summary.Root, summary.Slug), summary.Codex.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		view := pageData{Session: summary, SelectedMember: "lead", ConversationID: endpoint.Slug(), MaxMessageBytes: session.MaxMessageBytes}
		if endpoint.Pi != nil {
			if err := s.configurePiSessionPage(context.Background(), &view, nil); err != nil {
				t.Fatal(err)
			}
		}
		page := httptest.NewRecorder()
		s.render(page, "session", view)
		if page.Code != http.StatusOK {
			t.Fatal(page.Code, page.Body.String())
		}
		// Use the same server-rendered carrier and scope with a nonlead selection.
		view.SelectedMember = "reviewer0"
		memberPage := httptest.NewRecorder()
		s.render(memberPage, "session", view)
		request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+endpoint.Slug()+"/communication", nil)
		request.Header.Set("X-Workspace-Conversation-Scope", communicationFixtureScope(endpoint))
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
		var payload struct {
			Messages []communication.PiMessageRecord `json:"messages"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Messages) != 4 {
			t.Fatal(err, response.Body.String())
		}
		entries := []backend.TranscriptEntry{
			{Kind: "userMessage", ItemID: "ordinary", Text: "Discuss Cross-session message from sender to target without an envelope.", Timestamp: created.Add(-time.Minute).Format(time.RFC3339)},
			{Kind: "userMessage", ItemID: "team", Text: "Team assignment from implementer0 to lead:\n\nExact team report", Timestamp: created.Format(time.RFC3339)},
			{Kind: "agentMessage", ItemID: "between", Text: "Ordinary answer", Timestamp: created.Add(30 * time.Second).Format(time.RFC3339)},
			{Kind: "toolCall", ItemID: "team-tool", Communication: &backend.TranscriptCommunication{Kind: "team", To: "lead"}},
			{Kind: "error", ItemID: "error", Text: "Fixture error"},
		}
		for index, message := range payload.Messages {
			punctuation, newline := ":", "\n"
			if index%2 == 1 {
				punctuation, newline = ".", "\r\n"
			}
			text := fmt.Sprintf("Cross-session message from %s to %s (message %s)%s%s%sQuoted native body", message.From.Slug(), message.To.Slug(), message.ID, punctuation, newline, newline)
			entry := backend.TranscriptEntry{Kind: "userMessage", ItemID: "native-" + message.ID, Text: text, Timestamp: message.CreatedAt.Format(time.RFC3339), ClientUserMessageDigest: piruntime.MessageDigest(text)}
			if index%2 == 0 {
				entry.ClientUserMessageID = message.ID
			}
			entries = append(entries, entry)
		}
		if endpoint.Pi != nil {
			transcript := backend.Transcript{Entries: entries}
			s.presentPiTranscript(&transcript)
			entries = transcript.Entries
		} else {
			encoded, _ := json.Marshal(entries)
			transcript := codex.Transcript{}
			if err := json.Unmarshal(encoded, &transcript.Entries); err != nil {
				t.Fatal(err)
			}
			s.presentTranscript(&transcript)
			encoded, _ = json.Marshal(transcript.Entries)
			if err := json.Unmarshal(encoded, &entries); err != nil {
				t.Fatal(err)
			}
		}
		if entries[0].Communication != nil {
			t.Fatal("ordinary mention reclassified")
		}
		for _, entry := range entries[5:] {
			if entry.Communication == nil || entry.Communication.Kind != "session" || entry.Communication.From == "" || entry.Communication.To == "" || entry.ClientUserMessageDigest != piruntime.MessageDigest(entry.Text) {
				t.Fatal("fallback lost names or canonical digest", entry)
			}
		}
		pages = append(pages, map[string]any{"backend": kind, "slug": endpoint.Slug(), "thread": thread, "scope": communicationFixtureScope(endpoint),
			"html": page.Body.String(), "memberHTML": memberPage.Body.String(), "response": json.RawMessage(response.Body.Bytes()), "entries": entries})
	}
	input, err := json.Marshal(map[string]any{"asset": asset.Body.String(), "pages": pages})
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("declared Node required", err)
	}
	command := exec.Command(node, "composer_handoff_contract_test.cjs", "--communication")
	command.Stdin = strings.NewReader(string(input))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("communication display: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(after) != before {
		t.Fatal("read changed communication records", err)
	}
	if native.creates != creates || native.sends != sends || native.configures != configures || native.binds != binds || native.reads != reads {
		t.Fatal("communication display crossed native effect/read boundary")
	}
}

func TestCommunicationPresenterPreservesExistingMetadata(t *testing.T) {
	s := newTestServer(t)
	text := "Cross-session message from peer to current (message aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa).\n\nUnchanged bytes"
	codexHint := &codex.TranscriptCommunication{Kind: "team", From: "architect0", To: "reviewer0"}
	codexTranscript := codex.Transcript{Entries: []codex.TranscriptEntry{{Kind: "userMessage", Text: text, Communication: codexHint}, {Kind: "mcpToolCall", Summary: "Tool · team_reporting/session_communication", Communication: codexHint}}}
	s.presentTranscript(&codexTranscript)
	for _, entry := range codexTranscript.Entries {
		if entry.Communication != codexHint {
			t.Fatal("Codex metadata replaced")
		}
	}
	piHint := &backend.TranscriptCommunication{Kind: "team", From: "architect0", To: "reviewer0"}
	piTranscript := backend.Transcript{Entries: []backend.TranscriptEntry{{Kind: "userMessage", Text: text, Communication: piHint}}}
	s.presentPiTranscript(&piTranscript)
	if piTranscript.Entries[0].Communication != piHint || piTranscript.Entries[0].Text != text {
		t.Fatal("Pi metadata or bytes replaced")
	}
}

// A lost first reply must reconcile the captured input on the next worker pass.
type communicationWakeTransport struct {
	entered chan struct{}
	release chan struct{}
	sends   atomic.Int32
	lookups atomic.Int32
}

func (f *communicationWakeTransport) Ready(context.Context, communication.Identity) (string, error) {
	return "", nil
}
func (f *communicationWakeTransport) Prepare(context.Context, communication.Identity) (codex.TurnOptions, error) {
	return codex.TurnOptions{Model: "retained", ReasoningEffort: "xhigh"}, nil
}
func (f *communicationWakeTransport) Send(ctx context.Context, m communication.Message) (codex.SendReceipt, error) {
	if f.sends.Add(1) == 1 {
		close(f.entered)
		select {
		case <-ctx.Done():
			return codex.SendReceipt{}, ctx.Err()
		case <-f.release:
		}
	}
	return codex.SendReceipt{}, errors.New("lost reply")
}
func (f *communicationWakeTransport) Reconcile(_ context.Context, m communication.Message) (codex.SendReceipt, bool, error) {
	f.lookups.Add(1)
	return codex.SendReceipt{ClientUserMessageID: m.ID, TurnID: "turn-" + m.ID}, true, nil
}

func TestCommunicationWorkerWakeAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := communication.NewStore(t.TempDir(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	from := communication.Identity{Slug: "a", Thread: "a", Incarnation: "a"}
	to := communication.Identity{Slug: "b", Thread: "b", Incarnation: "b"}
	transport := &communicationWakeTransport{entered: make(chan struct{}), release: make(chan struct{})}
	service := communication.Service{Store: store, Transport: transport,
		Resolve: func(slug string) (communication.Identity, error) {
			if slug == "a" {
				return from, nil
			}
			return to, nil
		},
		Lock: func(context.Context, string) (func(), error) { return func() {}, nil }}
	if _, err = service.Run(ctx, from, communication.Request{Action: "allow", Peer: "b", Approval: "fixture approval"}); err != nil {
		t.Fatal(err)
	}
	save := func(id string) {
		t.Helper()
		if _, err := service.Run(ctx, from, communication.Request{Action: "send", Peer: "b", MessageID: id, Message: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	first, second := strings.Repeat("a", 32), strings.Repeat("b", 32)
	save(first)
	s := &Server{operationContext: ctx, communicationWake: make(chan struct{}, 1), config: Config{Logger: log.New(io.Discard, "", 0)}}
	ticks := make(chan time.Time)
	passes := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runCommunicationWorker(service, ticks, func(ctx context.Context, id string) error {
			err := service.Dispatch(ctx, id)
			passes <- id
			return err
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("communication worker did not join")
		}
	})
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not reach checkpoint")
		}
	}
	pass := func(want string) {
		t.Helper()
		select {
		case got := <-passes:
			if got != want {
				t.Fatalf("dispatch %s, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not complete pass")
		}
	}
	s.wakeCommunication()
	wait(transport.entered) // No tick was sent: the original wake picked it up.
	for i := 0; i < 10; i++ {
		s.wakeCommunication()
	}
	if len(s.communicationWake) != 1 {
		t.Fatal("busy-worker wakeups did not coalesce")
	}
	close(transport.release)
	pass(first)
	pass(first) // The coalesced pass looks up the first captured input, never resends it.
	if transport.sends.Load() != 1 || transport.lookups.Load() != 1 {
		t.Fatalf("send/lookup=%d/%d", transport.sends.Load(), transport.lookups.Load())
	}
	save(second) // No wake: the retained periodic recovery path must still work.
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not accept tick")
	}
	pass(second)
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not accept recovery tick")
	}
	pass(second)
	if transport.sends.Load() != 2 || transport.lookups.Load() != 2 {
		t.Fatalf("send/lookup=%d/%d", transport.sends.Load(), transport.lookups.Load())
	}
	cancel()
	wait(done)
}

// Real managed client and Unix RPCs: capture/cancel is not native submission.
func TestCommunicationOriginalManagedDelivery(t *testing.T) {
	for _, stage := range []string{"idle", "active", "prepared", "lost-ack", "acknowledged-history", "queued", "malformed-history"} {
		t.Run(stage, func(t *testing.T) {
			s, pi, sender, _ := piCommunicationHTTPFixture(t)
			f := newPlanDeliveryFixture(t, s)
			policy := workspacecodex.LeadThreadPolicy("example", s.config.Workspace, "")
			f.client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return policy, nil }
			activity := "idle"
			if stage == "active" {
				activity = "working"
			}
			s.activity = &activityMonitor{observer: &communicationActivityObserver{snapshot: codex.ActivitySnapshot{ThreadID: "thread-1", CurrentState: activity}}}
			f.wire.mu.Lock()
			f.wire.mode = "default"
			f.wire.latest = "previous"
			f.wire.status = "completed"
			f.wire.expectedInput = "Cross-session message from "
			if stage == "active" {
				f.wire.status = "inProgress"
			}
			f.wire.writeMode()
			f.wire.mu.Unlock()
			ctx := context.Background()
			if response := postCommunicationFixture(s, sender, pi.state.Generation, communication.Request{Action: "allow", Peer: "example", Approval: "user fixture direction"}); response.Code != http.StatusOK {
				t.Fatal(response.Code, response.Body.String())
			}
			id := "00000000-0000-4000-8000-000000000091"
			if response := postCommunicationFixture(s, sender, pi.state.Generation, communication.Request{Action: "send", Peer: "example", MessageID: id, Message: "Exact original é\nbytes"}); response.Code != http.StatusOK {
				t.Fatal(response.Code, response.Body.String())
			}
			canceled, cancel := context.WithCancel(ctx)
			f.observer = func(event codex.DeliveryEvent) {
				if event.ClientUserMessageID == id && event.Stage == "managed:capture" && !event.Failed {
					cancel()
				}
			}
			_ = s.dispatchCommunication(canceled, id)
			cancel()
			f.observer = nil
			f.wire.mu.Lock()
			starts := f.wire.starts
			f.wire.mu.Unlock()
			if starts != 0 {
				t.Fatal("capture cancellation submitted", starts)
			}
			statePath := filepath.Join(s.config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), "state.json")
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			var record communication.PiCommunicationState
			if err := json.Unmarshal(before, &record); err != nil {
				t.Fatal(err)
			}
			original := record.Messages[0]
			if original.Options == nil || original.Envelope == "" || original.Status != "submitting" {
				t.Fatal("original capture missing", original.Status, original.Detail)
			}
			if stage == "prepared" {
				if err := f.client.PrepareSendWithOptions("thread-1", original.Envelope, id, "communication:"+original.GrantID, false, *original.Options); err != nil {
					t.Fatal(err)
				}
			}
			if stage != "idle" { // Also cover the next pass without restarting the client.
				f.restart(t)
				f.client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return policy, nil }
			}
			if stage == "queued" || stage == "malformed-history" {
				if stage == "queued" {
					if _, err := f.client.QueueWithPolicy(ctx, "thread-1", original.Envelope, id, policy); err != nil {
						t.Fatal(err)
					}
				} else {
					f.wire.mu.Lock()
					f.wire.items = []any{map[string]any{"item": map[string]any{"type": "userMessage"}}}
					f.wire.mu.Unlock()
				}
				for i := 0; i < 2; i++ {
					if err := s.dispatchCommunication(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
				f.wire.mu.Lock()
				defer f.wire.mu.Unlock()
				wantQueues := 0
				if stage == "queued" {
					wantQueues = 1
				}
				if f.wire.starts != 0 || f.wire.queues != wantQueues {
					t.Fatal("unproven original replayed", f.wire.starts, f.wire.queues)
				}
				return
			}
			if stage == "acknowledged-history" {
				// Native acceptance can precede the communication receipt. Retire
				// its ledger row through the ordinary API, leaving native history.
				if _, err := f.client.SendWithOptions(ctx, "thread-1", original.Envelope, id, "communication:"+original.GrantID, *original.Options); err != nil {
					t.Fatal(err)
				}
				acked, err := f.client.AcknowledgeSends(ctx, "thread-1", []codex.SendAcknowledgement{{ClientUserMessageID: id, Digest: planDigest(original.Envelope)}})
				if err != nil || len(acked) != 1 || acked[0] != id {
					t.Fatal("original was not acknowledged", acked, err)
				}
				f.restart(t)
				f.client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return policy, nil }
			}
			if stage == "lost-ack" {
				f.wire.mu.Lock()
				f.wire.lost = true
				f.wire.mu.Unlock()
			}
			if err := s.dispatchCommunication(ctx, id); err != nil {
				t.Fatal(err)
			}
			f.wire.mu.Lock()
			starts = f.wire.starts
			messageID, message := f.wire.messageID, f.wire.message
			f.wire.mu.Unlock()
			if starts != 1 || messageID != id || message != original.Envelope {
				data, _ := os.ReadFile(statePath)
				var held communication.PiCommunicationState
				_ = json.Unmarshal(data, &held)
				t.Fatal("original did not continue exactly once", starts, messageID, held.Messages[0].Status, held.Messages[0].Detail, f.wire.calls)
			}
			if stage == "lost-ack" {
				if err := s.dispatchCommunication(ctx, id); err != nil {
					t.Fatal(err)
				}
				f.wire.mu.Lock()
				if f.wire.starts != 1 {
					t.Fatal("unknown replayed")
				}
				f.wire.hide = false
				f.wire.mu.Unlock()
			}
			if err := s.dispatchCommunication(ctx, id); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			var final communication.PiCommunicationState
			if err := json.Unmarshal(after, &final); err != nil {
				t.Fatal(err)
			}
			got := final.Messages[0]
			if got.Status != "delivered" || got.ID != original.ID || got.Envelope != original.Envelope || !reflect.DeepEqual(got.Options, original.Options) {
				t.Fatal("original tuple or receipt changed", got.Status)
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			wantMethod := "turn/start"
			if stage == "active" {
				wantMethod = "turn/steer"
			}
			if f.wire.starts != 1 || f.wire.queues != 0 || f.wire.sendMethod != wantMethod {
				t.Fatal("wrong native effect", f.wire.starts, f.wire.queues, f.wire.sendMethod)
			}
		})
	}
}
