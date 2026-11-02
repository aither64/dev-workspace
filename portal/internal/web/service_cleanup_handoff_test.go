package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func serviceHandoffFixture(t *testing.T) (*Server, lifecycleOperation, string, map[string]any) {
	t.Helper()
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	summary, err := session.Find(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(filepath.Join(summary.Workspace, summary.Root, summary.Slug))
	if err != nil {
		t.Fatal(err)
	}
	_ = stat
	root := filepath.Join(server.config.Workspace, "worktrees", ".locks")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "example.services-cleanup.json")
	value := map[string]any{"schema": 1, "workspace": server.config.Workspace, "slug": "example", "registry_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "retained_thread_id": "thread-1", "tracking_identity": map[string]any{"dev": 1, "ino": 2}, "operation": "archive", "operation_id": strings.Repeat("a", 64), "mode": "archived", "force": nil, "automatic": false, "resource_ids": []string{"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}, "phase": "services_pending", "created_at": "2026-11-02T12:00:00Z", "updated_at": "2026-11-02T12:00:00Z"}
	writeServiceHandoffJSON(t, path, value)
	progress, err := session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	operation := operationFromProgress("example", *progress)
	target, err := lifecycleTargetIdentity(summary)
	if err != nil {
		t.Fatal(err)
	}
	operation.TargetIdentityVersion = lifecycleTargetIdentityVersion
	operation.Options.TargetID = target
	operation.Options.TargetLocation = "work"
	return server, operation, path, value
}
func writeServiceHandoffJSON(t *testing.T, path string, value map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func publishServiceOrdinaryJournal(t *testing.T, server *Server, value map[string]any) string {
	t.Helper()
	path := filepath.Join(server.config.Workspace, "worktrees", ".locks", "example.archive.json")
	writeServiceHandoffJSON(t, path, map[string]any{"schema": 2, "workspace": server.config.Workspace, "slug": "example", "phase": "prepared", "mode": value["mode"], "operation_id": value["operation_id"], "retained_thread_id": value["retained_thread_id"], "target_tracking_sha256": strings.Repeat("a", 64)})
	return path
}
func TestInitialLifecycleReceiptCanAdoptItsFirstServicePrelude(t *testing.T) {
	server, operation, _, _ := serviceHandoffFixture(t)
	progress, err := session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	operation.Phase = "starting"
	operation.Options.JournalExpected = false
	operation.Options.JournalEvidence = ""
	if !lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *progress) {
		t.Fatal("initial receipt could not adopt first intent")
	}
	operation.Options.JournalExpected = true
	operation.Phase = "prepared"
	if lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *progress) {
		t.Fatal("ordinary receipt downgraded to prelude")
	}
}
func TestServicePreludeHandoffAcceptsMissedOverlapOnceAndPinsOrdinaryEvidence(t *testing.T) {
	server, operation, path, value := serviceHandoffFixture(t)
	journal := publishServiceOrdinaryJournal(t, server, value)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	progress, err := session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *progress) {
		t.Fatal("missed overlap refused")
	}
	if err := server.prepareLifecycleRetry(&operation, progress); err != nil {
		t.Fatal(err)
	}
	if operation.Phase != "prepared" || operation.Options.JournalEvidence != progress.Evidence {
		t.Fatal("promotion did not pin both phase and evidence")
	}
	var fields map[string]any
	raw, _ := os.ReadFile(journal)
	_ = json.Unmarshal(raw, &fields)
	fields["target_tracking_sha256"] = strings.Repeat("c", 64)
	writeServiceHandoffJSON(t, journal, fields)
	changed, err := session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *changed) {
		t.Fatal("ordinary evidence changed twice")
	}
}
func TestServicePreludeHandoffRequiresFullOverlapEvidenceAndExactPolicy(t *testing.T) {
	server, operation, path, value := serviceHandoffFixture(t)
	publishServiceOrdinaryJournal(t, server, value)
	progress, err := session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *progress) {
		t.Fatal("matching overlap refused")
	}
	value["resource_ids"] = []string{"cccccccc-cccc-cccc-cccc-cccccccccccc"}
	writeServiceHandoffJSON(t, path, value)
	progress, err = session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *progress) {
		t.Fatal("changed full prelude evidence accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	progress, err = session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*lifecycleOperation, *session.LifecycleProgress){
		func(op *lifecycleOperation, p *session.LifecycleProgress) {
			op.Options.JournalID = strings.Repeat("b", 64)
		},
		func(op *lifecycleOperation, p *session.LifecycleProgress) { op.Options.Mode = "abandoned" },
		func(op *lifecycleOperation, p *session.LifecycleProgress) {
			root := "other-root"
			p.RetainedThreadID = &root
		},
		func(op *lifecycleOperation, p *session.LifecycleProgress) {
			op.Options.TargetID = strings.Repeat("b", 64)
		},
		func(op *lifecycleOperation, p *session.LifecycleProgress) {
			op.TargetIdentityVersion = 0
			op.Options.TargetID = ""
		},
		func(op *lifecycleOperation, p *session.LifecycleProgress) { op.Kind = "delete" },
		func(op *lifecycleOperation, p *session.LifecycleProgress) { op.Phase = "prepared" },
	} {
		op, p := operation, *progress
		change(&op, &p)
		if lifecycleJournalEvidenceMatches(server.config.Workspace, op, p) {
			t.Fatal("mismatched handoff accepted")
		}
	}
}
func TestServicePreludeHandoffFollowsRenamedExactTargetAndRefusesReplacement(t *testing.T) {
	server, operation, path, value := serviceHandoffFixture(t)
	publishServiceOrdinaryJournal(t, server, value)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(server.config.Workspace, "work", "example")
	archived := filepath.Join(server.config.Workspace, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(archived), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(archived, "portal.yml"))
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("finalized_at: \"2026-11-02T12:00:00Z\"\n")...)
	if err := os.WriteFile(filepath.Join(archived, "portal.yml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, archived, "archived")
	progress, err := session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *progress) {
		t.Fatal("same moved directory refused")
	}
	if err := os.Rename(archived, archived+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archived, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"portal.yml", "state.md", "plan.md"} {
		data, err := os.ReadFile(filepath.Join(archived+"-retained", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(archived, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if lifecycleJournalEvidenceMatches(server.config.Workspace, operation, *progress) {
		t.Fatal("replacement target accepted")
	}
}
func TestServicePreludeExecutorRequiresCapturedPhaseForForwardHandoff(t *testing.T) {
	server, operation, path, value := serviceHandoffFixture(t)
	publishServiceOrdinaryJournal(t, server, value)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "dev-session")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = command
	args := []string{"archive", "example", "--as-is"}
	if err := server.runLifecycleOperation(context.Background(), "example", "archive", args, operation.Options); err == nil {
		t.Fatal("options alone inferred a handoff")
	}
	if err := server.runLifecycleOperation(context.Background(), "example", "archive", args, operation.Options, operation); err != nil {
		t.Fatal(err)
	}
}

func TestServicePreludeLateExecutorCannotReplaceAlreadyPromotedOrdinaryEvidence(t *testing.T) {
	server, operation, path, value := serviceHandoffFixture(t)
	journal := publishServiceOrdinaryJournal(t, server, value)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ordinary, err := session.PendingLifecycleProgress(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	entered := filepath.Join(root, "entered")
	release := filepath.Join(root, "release")
	command := filepath.Join(root, "dev-session")
	script := "#!/bin/sh\ntouch '" + entered + "'\nwhile [ ! -e '" + release + "' ]; do sleep .01; done\nexit 1\n"
	if err := os.WriteFile(command, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = command
	operation.State = "running"
	operation.Attempt = 1
	server.operationMu.Lock()
	server.operations["example"] = operation
	server.operationMu.Unlock()
	server.operationWG.Add(1)
	done := make(chan struct{})
	go func() { server.runLifecycleJob(operation, []string{"archive", "example", "--as-is"}); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("executor did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	promoted := operation
	promoted.applyProgressOptions(*ordinary)
	promoted.Phase = ordinary.Phase
	server.operationMu.Lock()
	server.operations["example"] = promoted
	server.operationMu.Unlock()
	var fields map[string]any
	raw, _ := os.ReadFile(journal)
	_ = json.Unmarshal(raw, &fields)
	fields["target_tracking_sha256"] = strings.Repeat("d", 64)
	writeServiceHandoffJSON(t, journal, fields)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish")
	}
	server.operationMu.Lock()
	current := server.operations["example"]
	server.operationMu.Unlock()
	if current.Options.JournalEvidence != ordinary.Evidence {
		t.Fatal("late executor replaced already-pinned ordinary evidence")
	}
}
