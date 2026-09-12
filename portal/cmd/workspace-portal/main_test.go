package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

type runtimeContract struct {
	TrackingMaxBytes       int                        `json:"trackingMaxBytes"`
	MaxMessageBytes        int                        `json:"maxMessageBytes"`
	FormEncodingExpansion  int                        `json:"formEncodingExpansion"`
	JSONEncodingExpansion  int                        `json:"jsonEncodingExpansion"`
	TransportEnvelopeBytes int                        `json:"transportEnvelopeBytes"`
	LifecycleJournals      []session.LifecycleJournal `json:"lifecycleJournals"`
	ThreadEnvironmentKeys  []string                   `json:"threadEnvironmentKeys"`
	PortalServeFlags       []string                   `json:"portalServeFlags"`
}

func loadRuntimeContract(t *testing.T) runtimeContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "session", "runtime-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var contract runtimeContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	return contract
}

func TestThreadRuntimeEnvironmentMatchesPublishedContract(t *testing.T) {
	contract := loadRuntimeContract(t)
	if contract.MaxMessageBytes != session.MaxMessageBytes {
		t.Fatalf("maximum message size = %d, want %d", session.MaxMessageBytes, contract.MaxMessageBytes)
	}
	if contract.TrackingMaxBytes != session.TrackingMaxSize {
		t.Fatalf("tracking size = %d, want %d", session.TrackingMaxSize, contract.TrackingMaxBytes)
	}
	actualJournals := session.LifecycleJournals()
	if len(actualJournals) != len(contract.LifecycleJournals) {
		t.Fatalf("lifecycle journals = %#v, want %#v", actualJournals, contract.LifecycleJournals)
	}
	for index := range actualJournals {
		if actualJournals[index] != contract.LifecycleJournals[index] {
			t.Fatalf("lifecycle journals = %#v, want %#v", actualJournals, contract.LifecycleJournals)
		}
	}
	if contract.FormEncodingExpansion != session.FormEncodingExpansion ||
		contract.JSONEncodingExpansion != session.JSONEncodingExpansion ||
		contract.TransportEnvelopeBytes != session.TransportEnvelopeBytes {
		t.Fatalf("transport encoding contract does not match the Go projection: %#v", contract)
	}
	if session.MaxFormRequestBodyBytes < 3*session.MaxMessageBytes ||
		session.MaxJSONRequestBodyBytes < 6*session.MaxMessageBytes {
		t.Fatal("transport request ceilings do not admit worst-case encoded messages")
	}
	runtime := threadRuntime{
		Slug: "example", Workspace: "/workspace", WorkDir: "/workspace/work/example",
		WorktreesDir: "/workspace/worktrees/example", PortalBaseURL: "https://workspace.example",
		PortalURL: "https://workspace.example/example/", AuthorityDir: "/run/authority",
		TmuxSocket: "/run/tmux.sock", CodexCommand: "/nix/store/codex/bin/codex",
		CodexSocket: "/run/codex.sock", CodexVersion: "0.152.1",
		PortalCommand: "/run/current-system/sw/bin/workspace-portal",
	}
	if !runtime.complete() {
		t.Fatal("complete runtime was rejected")
	}
	environment := runtime.environment()
	actual := make([]string, 0, len(environment))
	for key, value := range environment {
		if value == "" {
			t.Fatalf("runtime environment %s is empty", key)
		}
		actual = append(actual, key)
	}
	expected := contract.ThreadEnvironmentKeys
	sort.Strings(actual)
	sort.Strings(expected)
	if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("runtime environment keys = %v, want %v", actual, expected)
	}
}

func TestPortalServeFlagsIncludePublishedDeploymentContract(t *testing.T) {
	contract := loadRuntimeContract(t)
	flags, _ := newServeFlagSet()
	available := make(map[string]bool)
	flags.VisitAll(func(item *flag.Flag) { available["--"+item.Name] = true })
	for _, name := range contract.PortalServeFlags {
		if !available[name] {
			t.Errorf("published portal serve flag %s is not implemented", name)
		}
	}
}

func TestValidateCommandRejectsInvalidPersistedManifest(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateCommand([]string{"--workspace", workspace}); err == nil {
		t.Fatal("invalid persisted manifest was accepted")
	}
}

func TestValidateCommandRequiresWorkspace(t *testing.T) {
	if err := validateCommand(nil); err == nil || err.Error() != "validate requires --workspace" {
		t.Fatalf("validateCommand() error = %v", err)
	}
}

func TestPortalUnixSocketIsPrivateToItsOwnerAndGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.sock")
	listener, err := portalListener(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, ok := listener.(*net.UnixListener); !ok {
		t.Fatalf("listener type = %T", listener)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o660 {
		t.Fatalf("socket mode = %#o", mode)
	}
}

func TestPortalUnixSocketRefusesANonSocketPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.sock")
	if err := os.WriteFile(path, []byte("do not replace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := portalListener(path); err == nil {
		t.Fatal("non-socket path was replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "do not replace" {
		t.Fatalf("non-socket path changed: %q, %v", data, err)
	}
}

func TestPortalRequiresAUnixSocket(t *testing.T) {
	if _, err := portalListener(""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing socket result = %v", err)
	}
}

func TestUploadRemovalUsesOwnerCatalogAndRequiresThreadEvidence(t *testing.T) {
	root := t.TempDir()
	workspace, stateRoot := filepath.Join(root, "workspace"), filepath.Join(root, "state")
	args := []string{"remove-session", "--workspace", workspace, "--user-state-root", stateRoot, "--session-slug", "example"}
	if err := uploadCommand(args); err != nil {
		t.Fatal("missing catalog was not a no-op", err)
	}
	if _, err := os.Stat(stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing catalog created state", err)
	}
	ctx := context.Background()
	store, err := uploads.ForWorkspace(workspace, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	store.MinFreeBytes = 0
	epoch, err := session.CompletedRemovalHistory(workspace, "example", stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := store.SessionScope(ctx, "example", "thread", epoch)
	if err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: store, ScopeID: scope.ID}
	file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "input", Size: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Complete(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	if err := uploadCommand(args); err == nil {
		t.Fatal("missing retired thread was accepted")
	}
	content, err := backend.Open(ctx, file.ID)
	if err != nil {
		t.Fatal("failed cleanup changed file", err)
	}
	content.File.Close()
	if err := uploadCommand(append(args, "--thread-id", "thread")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Open(ctx, file.ID); err == nil {
		t.Fatal("owner CLI retained deleted bytes")
	}
}
