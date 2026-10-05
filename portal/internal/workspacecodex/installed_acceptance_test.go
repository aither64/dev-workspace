package workspacecodex_test

// Opt-in installed acceptance support, never a fake App Server. Main starts the
// selected private server and invokes these cases after source/package review.
// No turn/start, queue/start, assignment, rollout writer or native DB reader.
import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type installedArchiveFixture struct {
	Root        string            `json:"root"`
	Workspace   string            `json:"workspace"`
	Package     string            `json:"package"`
	Name        string            `json:"workspaceName"`
	Slug        string            `json:"slug"`
	Purpose     string            `json:"purpose"`
	Status      string            `json:"status"`
	UID         int               `json:"uid"`
	Environment map[string]string `json:"environment"`
}

type installedArchiveRoot struct {
	ThreadID string `json:"threadId"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
}

func nativeFixtureEnvironment(fixture installedArchiveFixture) map[string]string {
	return map[string]string{
		"DEV_SESSION_SLUG": fixture.Slug, "DEV_SESSION_WORKSPACE": fixture.Workspace,
		"DEV_SESSION_WORK_DIR":        filepath.Join(fixture.Workspace, "work", fixture.Slug),
		"DEV_SESSION_REQUIRE_RUNTIME": "1",
		"DEV_SESSION_CODEX_SOCKET":    filepath.Join(fixture.Root, "run", fixture.Name, "app-server.sock"),
	}
}

func installedFixture(t *testing.T) (installedArchiveFixture, *workspacecodex.Client, context.Context) {
	t.Helper()
	file := os.Getenv("ARCHIVE_ACCEPTANCE_FIXTURE")
	if file == "" {
		t.Skip("requires an explicitly supervised private installed-package fixture")
	}
	var fixture installedArchiveFixture
	data, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &fixture) != nil {
		t.Fatalf("read private fixture: %v", err)
	}
	if !regexp.MustCompile(`^/tmp/archive-acceptance-[a-z0-9]{1,12}$`).MatchString(fixture.Root) ||
		file != filepath.Join(fixture.Root, "fixture.json") || fixture.UID != os.Getuid() ||
		fixture.Purpose != "disposable-archive-acceptance" || fixture.Status != "prepared" ||
		fixture.Workspace != filepath.Join(fixture.Root, "workspace") ||
		fixture.Name != "acceptance-"+filepath.Base(fixture.Root)[len("archive-acceptance-"):] ||
		fixture.Slug != "2026-10-04-fixture-legacy" {
		t.Fatal("unexpected fixture ownership")
	}
	for key, expected := range fixture.Environment {
		if os.Getenv(key) != expected {
			t.Fatalf("private child environment differs: %s", key)
		}
	}
	for _, key := range []string{"DEV_SESSION_SLUG", "DEV_SESSION_WORKSPACE", "CODEX_THREAD_ID", "SSH_AUTH_SOCK", "OPENAI_API_KEY"} {
		if os.Getenv(key) != "" {
			t.Fatalf("parent identity/credential inherited: %s", key)
		}
	}
	socket := filepath.Join(fixture.Root, "run", fixture.Name, "app-server.sock")
	client := workspacecodex.NewWithOptions(socket, fixture.Workspace, codex.ClientOptions{
		ClientInfo:                         codex.ClientInfo{Name: "dev-workspace", Title: "Archive acceptance fixture", Version: "0.1.0"},
		DeveloperInstructions:              "Disposable fixture. Do not submit a user or model turn.",
		PreserveThreadInstructionsOnResume: true,
		SubmissionLedgerPath:               socket + ".submission-attempts-v3.json",
	})
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return fixture, client, ctx
}

func saveInstalledEvidence(t *testing.T, fixture installedArchiveFixture, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(fixture.Root, "logs", name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		t.Fatal(err)
	}
}

func requireNativeEmptyHistory(t *testing.T, ctx context.Context, client *workspacecodex.Client, id, cwd, project string, archived bool) workspacecodex.ThreadObservation {
	t.Helper()
	state, err := workspacecodex.ProveArchivedThread(ctx, client, workspacecodex.ArchivedThreadIdentity{
		ThreadID: id, Cwd: cwd, ProjectID: project, CodexHome: client.CodexHome,
		SourceKind: "vscode", RequireActiveFile: true,
	})
	if err != nil || (archived && state != workspacecodex.ArchiveArchived) || (!archived && state != workspacecodex.ArchiveActive) {
		t.Fatalf("native persistence %s: state=%v err=%v", id, state, err)
	}
	// Native response, without manufacturing a JSONL header or an empty turn.
	var turns struct {
		Data       *[]json.RawMessage `json:"data"`
		NextCursor *string            `json:"nextCursor"`
	}
	if err := client.Request(ctx, "thread/turns/list", map[string]any{
		"threadId": id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
	}, &turns); err != nil || turns.Data == nil || len(*turns.Data) != 0 || turns.NextCursor != nil {
		t.Fatalf("native no-turn history %s: %+v err=%v", id, turns, err)
	}
	observation, err := client.ObserveThreadIdentity(ctx, id, cwd, project, state)
	if err != nil || !observation.ActivityKnown || !observation.Idle || observation.LastActivityAt != nil {
		t.Fatalf("native no-turn idle proof %s: %+v err=%v", id, observation, err)
	}
	// The archived path above reuses the owner; it never calls cold queue/list.
	return observation
}

func TestInstalledArchiveFixtureSeed(t *testing.T) {
	fixture, client, ctx := installedFixture(t)
	if _, err := os.Stat(filepath.Join(fixture.Root, "logs", "native-root.json")); !os.IsNotExist(err) {
		t.Fatal("seed requires a fresh fixture, never a replacement root")
	}
	installed, err := agentteams.LoadInstalled(fixture.Package)
	if err != nil || installed.Catalog == nil {
		t.Fatalf("load selected installed catalog: %v", err)
	}
	team, ok := installed.Catalog.Team(installed.Catalog.DefaultTeam)
	lead, found := team.Roles["team_lead"]
	if !ok || !found {
		t.Fatal("installed default has no lead settings")
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := workspacecodex.ResolveNewThreadSettings(models, codex.ThreadSettings{Model: lead.Model, ReasoningEffort: lead.Effort})
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(fixture.Workspace, "work", fixture.Slug)
	if _, err := os.Lstat(filepath.Join(cwd, "portal.yml")); !os.IsNotExist(err) {
		t.Fatal("native seed requires the fresh manifestless fixture")
	}
	if err := workspacecodex.RequireThreadlessConversations(ctx, client, cwd); err != nil {
		t.Fatalf("seed will not adopt or replace an existing conversation: %v", err)
	}
	id, err := client.StartThreadWithSettings(ctx, cwd, nativeFixtureEnvironment(fixture), settings)
	if err != nil {
		t.Fatal(err)
	}
	var injected json.RawMessage
	if err := client.Request(ctx, "thread/inject_items", map[string]any{
		"threadId": id, "items": []any{map[string]any{"type": "message", "role": "developer", "content": []any{
			map[string]any{"type": "input_text", "text": "Disposable archive acceptance root; no assignment or user turn."},
		}}},
	}, &injected); err != nil {
		t.Fatal(err)
	}
	requireNativeEmptyHistory(t, ctx, client, id, cwd, "", false)
	saveInstalledEvidence(t, fixture, "native-root.json", installedArchiveRoot{id, lead.Model, lead.Effort})
}

// Invoke once per named stage, with fresh logs retained across server restarts.
// resume explicitly changes only settings on the retained native identities.
func TestInstalledArchiveFixtureVerify(t *testing.T) {
	fixture, client, ctx := installedFixture(t)
	stage := os.Getenv("ARCHIVE_ACCEPTANCE_STAGE")
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`).MatchString(stage) {
		t.Fatal("verification needs a unique evidence stage")
	}
	mode := os.Getenv("ARCHIVE_ACCEPTANCE_MODE")
	if mode != "inspect" && mode != "resume" && mode != "archived" {
		t.Fatal("mode must be inspect, resume or archived")
	}
	var root installedArchiveRoot
	data, err := os.ReadFile(filepath.Join(fixture.Root, "logs", "native-root.json"))
	if err != nil || json.Unmarshal(data, &root) != nil || root.ThreadID == "" {
		t.Fatalf("read actual seed identity: %v", err)
	}
	store, err := teamruntime.NewStore(fixture.Environment["DEV_WORKSPACES_STATE"], fixture.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	roster, err := store.Load(fixture.Slug, root.ThreadID)
	if err != nil || roster == nil || len(roster.Members) != 2 {
		t.Fatalf("two ordinary installed team additions required: %v", err)
	}
	cwd := filepath.Join(fixture.Workspace, "work", fixture.Slug)
	ids := map[string]string{"root": root.ThreadID}
	for _, member := range roster.Members {
		expected := "ready"
		if mode == "archived" {
			expected = "archived"
		}
		if member.State != expected {
			t.Fatalf("member %s is %s", member.Address, member.State)
		}
		ids[member.Address] = member.Thread
	}
	if mode == "resume" {
		if id, err := client.ResumeThreadWithSettings(ctx, root.ThreadID, cwd, nativeFixtureEnvironment(fixture), codex.ThreadSettings{Model: root.Model, ReasoningEffort: root.Effort}); err != nil || id != root.ThreadID {
			t.Fatalf("resume exact root settings: %s %v", id, err)
		}
		for _, member := range roster.Members {
			environment := nativeFixtureEnvironment(fixture)
			environment["DEV_SESSION_MEMBER_ADDRESS"] = member.Address
			if id, err := client.ResumeThreadWithSettings(ctx, member.Thread, cwd, environment, codex.ThreadSettings{Model: member.Model, ReasoningEffort: member.Effort}); err != nil || id != member.Thread {
				t.Fatalf("resume exact member settings: %s %v", id, err)
			}
		}
	}
	observations := map[string]workspacecodex.ThreadObservation{
		"root": requireNativeEmptyHistory(t, ctx, client, root.ThreadID, cwd, "", mode == "archived"),
	}
	for _, member := range roster.Members {
		observations[member.Address] = requireNativeEmptyHistory(t, ctx, client, member.Thread, cwd, member.ProjectID, mode == "archived")
	}
	saveInstalledEvidence(t, fixture, "native-"+stage+".json", map[string]any{"identities": ids, "roster": roster, "mode": mode, "observations": observations,
		"evidence": "native persistence, empty actual turn list and owner idle proof; no notification-order claim"})
}
