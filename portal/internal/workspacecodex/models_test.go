package workspacecodex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
)

func TestSelectedProviderModelsDoNotUseTheSharedDaemonCatalog(t *testing.T) {
	workspace, stateRoot, profile := t.TempDir(), t.TempDir(), t.TempDir()
	catalog := codexproviders.Catalog{Schema: 1, InitialProvider: "pool", Providers: []codexproviders.Provider{{ID: "openai", Kind: "subscription", Label: "Subscription"}, {ID: "pool", Kind: "codex-lb", Label: "Pool", BaseURL: "http://127.0.0.1:2455/v1", TokenFile: "/fixture/token"}}}
	data, _ := json.Marshal(catalog)
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalogPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalogPath)
	t.Setenv("DEV_WORKSPACE_NAME", "fixture")
	cwd := filepath.Join(workspace, "work", "selected")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"plan.md": "Fixture\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": "schema: 1\nslug: selected\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: root\n  socket_path: /unused.sock\n  client_version: 0.163.0\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := codexproviders.State{Schema: 2, Workspace: "fixture", Root: workspace, Socket: "/unused.sock", Active: "pool", Sessions: map[string]codexproviders.Selection{"selected": {Provider: "openai", RootThreadID: "root"}}}
	data, _ = json.Marshal(state)
	path, _ := codexproviders.StatePath(stateRoot, "fixture")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(profile, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1 $2 $3 $4\" = 'provider models fixture openai' ] || exit 1\nprintf '%s\\n' '[{\"model\":\"target-only-daybreak\",\"defaultReasoningEffort\":\"high\",\"supportedReasoningEfforts\":[{\"reasoningEffort\":\"high\"}]}]'\n"
	if err := os.WriteFile(filepath.Join(profile, "bin", "workspace-host"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := NewWithOptions("/unused.sock", workspace, codex.ClientOptions{})
	client.RecoveryRoot = stateRoot
	defer client.Close()
	models, err := client.ModelsForDirectory(context.Background(), cwd, profile, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveNewThreadSettings(models, codex.ThreadSettings{Model: "target-only-daybreak", ReasoningEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	// A fork selects its destination provider before the new root exists.
	state.Sessions["selected"] = codexproviders.Selection{Provider: "openai", CreationID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	data, _ = json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	locks := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(locks, 0700); err != nil {
		t.Fatal(err)
	}
	forkJournal := filepath.Join(locks, "selected.fork.json")
	journal := []byte(`{"schema":2,"slug":"selected","state":"creating","provider":"openai","tmux_identity":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
	if err := os.WriteFile(forkJournal, journal, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ModelsForDirectory(context.Background(), cwd, profile, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locks, "selected.creation.json"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ModelsForDirectory(context.Background(), cwd, profile, ""); err == nil {
		t.Fatal("ambiguous creation and fork journals accepted")
	}
	if err := os.Remove(filepath.Join(locks, "selected.creation.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkJournal, []byte(`{"schema":2,"slug":"selected","state":"creating","provider":"pool","tmux_identity":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ModelsForDirectory(context.Background(), cwd, profile, ""); err == nil {
		t.Fatal("changed fork provider accepted")
	}

}
