package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	workspacecodex "github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type settingsRecoveryCodex struct {
	*recoveryTestCodex
	activated codex.ThreadSettings
}

func (c *settingsRecoveryCodex) ActivateThreadWithSettings(_ context.Context, thread string, settings codex.ThreadSettings) error {
	c.calls = append(c.calls, "activate-settings:"+thread)
	c.activated = settings
	return nil
}

func TestColdSettingsSurviveRestartAndApplyBeforeActivation(t *testing.T) {
	s, base := recoveryTestServer(t)
	c := &settingsRecoveryCodex{recoveryTestCodex: base}
	s.config.Codex = c
	s.config.VerifyThread = c.VerifyThread
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	model, effort := "model-1", "high"
	result, err := client.UpdateThreadSettings(context.Background(), "thread-1", codex.ThreadSettingsUpdate{Model: &model, ReasoningEffort: &effort})
	if err != nil || result.Model != model || result.ReasoningEffort != effort {
		t.Fatal(result, err)
	}
	if len(c.calls) != 0 || !s.recoveryHeld("example", "thread-1") {
		t.Fatal("saving activated work", c.calls)
	}
	restarted := &Server{config: s.config, operationStore: s.operationStore}
	saved, err := restarted.coldSettings("example", "thread-1", "thread-1")
	if err != nil || saved == nil || saved.Effort != effort {
		t.Fatal("lost choice", saved, err)
	}
	if _, err := restarted.coldSettings("foreign", "thread-1", "thread-1"); err == nil {
		t.Fatal("accepted another session's settings")
	}
	if err := s.activateRecovery(context.Background(), "example", "thread-1", "thread-1", ""); err != nil {
		t.Fatal(err)
	}
	if c.activated.Model != model || c.activated.ReasoningEffort != effort || s.recoveryHeld("example", "thread-1") {
		t.Fatal("activation ignored choice", c.activated)
	}
	if saved, err := s.coldSettings("example", "thread-1", "thread-1"); err != nil || saved != nil {
		t.Fatal("applied choice remained pending", saved, err)
	}
}

func TestTeamStatsHeldSnapshotDoesNotLoadOrExtendTime(t *testing.T) {
	s, c := recoveryTestServer(t)
	snapshot := codex.ActivitySnapshot{ThreadID: "thread-1", CurrentState: "waiting", WorkingMS: 1234, WaitingMS: 2000, OpenWaitingMS: 500, SentMessages: 17, ReceivedMessages: 9, TotalToolCalls: 23}
	if err := s.saveThreadRecord("activity-final", "thread-1", snapshot); err != nil {
		t.Fatal(err)
	}
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.teamStatsAPI(response, httptest.NewRequest("GET", "/api/sessions/example/team-stats", nil), summary)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"sentMessages":17`) || !strings.Contains(response.Body.String(), `"waitingMs":2500`) || !strings.Contains(response.Body.String(), `"state":"stopped"`) {
		t.Fatal(response.Body.String())
	}
	if len(c.calls) != 0 {
		t.Fatal("stats activated native work", c.calls)
	}
}

// The wrapper was resolved while held; Continue wins the mutation lock before
// Save. The saved pair must reach native defaults instead of becoming deferred.
func TestColdSettingsRecheckActivationBeforeSave(t *testing.T) {
	s, base := recoveryTestServer(t)
	c := &settingsRecoveryCodex{recoveryTestCodex: base}
	s.config.Codex, s.config.VerifyThread = c, c.VerifyThread
	prepareInteractiveConversation(t, s, "example")
	p := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(data), "/run/dev-workspace-codex/app-server.sock", s.config.CodexSocket)), 0644); err != nil {
		t.Fatal(err)
	}
	writeWebRuntimeAuthority(t, s, "example")
	stale := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "thread-1"}
	if err := s.activateRecovery(context.Background(), "example", "thread-1", "thread-1", ""); err != nil {
		t.Fatal(err)
	}
	model, effort := "model-1", "high"
	if _, err := stale.UpdateThreadSettings(context.Background(), "thread-1", codex.ThreadSettingsUpdate{Model: &model, ReasoningEffort: &effort}); err != nil {
		t.Fatal(err)
	}
	if c.settings.Model != model || c.settings.ReasoningEffort != effort {
		t.Fatal("native defaults ignored save", c.settings)
	}
	if pending, err := s.coldSettings("example", "thread-1", "thread-1"); err != nil || pending != nil {
		t.Fatal("live save left deferred settings", pending, err)
	}
}

type boundMemberCodex struct {
	*workspacecodex.Client
	wrongMember bool
	updates     int
}

func (c *boundMemberCodex) VerifyThread(_ context.Context, thread, _ string) error {
	if c.wrongMember && thread == "member-thread" {
		return errors.New("thread cwd mismatch")
	}
	return nil
}
func (c *boundMemberCodex) ListModels(ctx context.Context) ([]codex.Model, error) {
	return (&browserContractCodex{}).ListModels(ctx)
}
func (c *boundMemberCodex) ListCollaborationModes(ctx context.Context) ([]codex.CollaborationMode, error) {
	return (&browserContractCodex{}).ListCollaborationModes(ctx)
}
func (c *boundMemberCodex) UpdateThreadSettings(context.Context, string, codex.ThreadSettingsUpdate) (codex.ThreadSettings, error) {
	c.updates++
	return codex.ThreadSettings{}, errors.New("stopped settings reached native client")
}
func stoppedMemberFixture(t *testing.T) (*Server, *boundMemberCodex, *teamruntime.Store) {
	t.Helper()
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "example")
	c := &boundMemberCodex{Client: workspacecodex.NewWithOptions(s.config.CodexSocket, s.config.Workspace, codex.ClientOptions{})}
	s.config.Codex, s.config.VerifyThread = c, c.VerifyThread
	// No live authority: the retained thread remains safe for cold settings.
	if err := os.Remove(filepath.Join(s.config.AuthorityDir, "example.json")); err != nil {
		t.Fatal(err)
	}
	store, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), "example", "thread-1", true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Thread: "member-thread", State: "ready", Model: "model-1", Effort: "medium", AddedAt: time.Now().UTC()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, store
}
func TestColdSettingsStoppedMemberPageRefreshAndSave(t *testing.T) {
	s, c, store := stoppedMemberFixture(t)
	for _, path := range []string{"/example/?member=implementer0", "/api/sessions/example/details?member=implementer0"} {
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, httptest.NewRequest("GET", path, nil))
		if out.Code != 200 {
			t.Fatal(out.Body.String())
		}
		if strings.Contains(path, "details") {
			if !strings.Contains(out.Body.String(), `"readyMembers":["implementer0"]`) {
				t.Fatal(out.Body.String())
			}
		} else if !strings.Contains(out.Body.String(), `data-conversation-id="example~implementer0"`) {
			t.Fatal(out.Body.String())
		}
	}
	target, err := s.resolveConversation(context.Background(), conversation.ResolveRequest{ID: "example~implementer0", Operation: "settings", Mutation: true, Method: "POST"})
	if err != nil {
		t.Fatal("resolve stopped member", err)
	}
	target.Release()
	request := httptest.NewRequest("POST", "/codex/conversations/example~implementer0/settings", strings.NewReader(`{"model":"model-1","reasoningEffort":"high"}`))
	request.Header.Set("Origin", s.config.BaseURL)
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, request)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	roster, err := store.Load("example", "thread-1")
	if err != nil || roster.Members[0].Effort != "high" || c.updates != 0 {
		t.Fatal(roster, err, c.updates)
	}
}

func TestColdSettingsBoundMemberUsesCurrentChildAndOriginalRoster(t *testing.T) {
	s, c, store := stoppedMemberFixture(t)
	socket := filepath.Join(t.TempDir(), "registered", "app-server.sock")
	s.config.CodexSocket = socket
	s.config.AuthorityDir = filepath.Join(filepath.Dir(socket), "authority")
	if err := os.MkdirAll(s.config.AuthorityDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("schema: 6\nslug: example\nrepositories: []\ncodex:\n  thread_id: thread-1\n  socket_path: %s\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\ncreation:\n  state: ready\n  initial_goal_sent: true\n", socket)
	if err := os.WriteFile(filepath.Join(s.config.Workspace, "work", "example", "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalog)
	op, inc, digest := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", strings.Repeat("a", 64), strings.Repeat("c", 64)
	state := codexproviders.State{Schema: 3, Workspace: "registered", Root: s.config.Workspace, Socket: socket, Active: "openai", BindingRevision: 2, Bindings: map[string]codexproviders.SessionBinding{"example": {RootThreadID: "thread-1", Incarnation: inc, Revision: 2, Conversations: map[string]codexproviders.ConversationBinding{
		"thread-1": {Member: "lead", ThreadID: "root-child", Provider: "openai", OperationID: op, EvidenceDigest: digest}, "member-thread": {Member: "implementer0", ThreadID: "member-child", Provider: "openai", OperationID: op, EvidenceDigest: digest}}}}}
	authority := session.RuntimeAuthority{Schema: 3, State: "ready", Slug: "example", Workspace: s.config.Workspace, TmuxSocket: "/run/tmux.sock", TmuxSessionID: "$1", TmuxIdentity: inc, CodexThreadID: "thread-1", CodexActiveThreadID: "root-child", CodexBindingRevision: 2, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
	statePath, err := codexproviders.StatePath(s.config.UserStateRoot, "registered")
	if err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]any{statePath: state, filepath.Join(s.config.AuthorityDir, "example.json"): authority} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := recoveryConversationClient{Client: c, server: s, slug: "example", root: "thread-1", thread: "member-child", address: "implementer0"}
	mode := "plan"
	if _, err := client.UpdateThreadSettings(context.Background(), "member-child", codex.ThreadSettingsUpdate{CollaborationMode: &mode}); err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load("example", "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if roster.Members[0].Thread != "member-thread" || roster.Members[0].Effort != "medium" || c.updates != 0 {
		t.Fatal("settings changed origin or activated native execution", roster, c.updates)
	}
	saved, err := s.coldSettings("example", "thread-1", "member-child")
	if err != nil || saved == nil || saved.Mode != "plan" {
		t.Fatal("current child settings were not saved", saved, err)
	}
	for _, fault := range []string{"ancestor", "wrong-member", "pending-original-input"} {
		t.Run(fault, func(t *testing.T) {
			current := client
			target := "member-child"
			if fault == "ancestor" {
				current.thread = "member-thread"
				target = current.thread
			}
			if fault == "wrong-member" {
				current.address = "reviewer0"
			}
			if fault == "pending-original-input" {
				if err := s.saveThreadRecord("next-settings", target, coldSettings{Slug: "example", Root: "thread-1", Thread: target, RequestID: "request", Epoch: "epoch"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := current.UpdateThreadSettings(context.Background(), target, codex.ThreadSettingsUpdate{CollaborationMode: &mode}); err == nil {
				t.Fatal("stale/unknown settings target accepted")
			}
		})
	}
}
func TestTeamStatsVerifyMemberDirectoryBeforeRead(t *testing.T) {
	s, c, _ := stoppedMemberFixture(t)
	c.wrongMember = true
	observer := &testActivityObserver{read: make(chan string, 4)}
	s.activity = &activityMonitor{server: s, observer: observer}
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	s.teamStatsAPI(out, httptest.NewRequest(http.MethodGet, "/api/sessions/example/team-stats", nil), summary)
	var payload struct {
		Members []teamActivity `json:"members"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, row := range payload.Members {
		if row.Address == "implementer0" && row.Snapshot != nil {
			t.Fatal("read unverified member activity", row)
		}
	}
	if len(observer.read) != 1 {
		t.Fatal("wrong member reached activity provider", len(observer.read))
	}
}
