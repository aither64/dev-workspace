package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

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
