package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	portalweb "github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
	"golang.org/x/sys/unix"
)

func TestRootThreadSettingsKeepLeadPolicySeparateFromModel(t *testing.T) {
	for _, selected := range []struct{ model, effort string }{
		{}, {"gpt-6-sol", "xhigh"},
	} {
		settings := rootThreadSettings(selected.model, selected.effort, "example", "/workspace", "")
		if settings.Model != selected.model || settings.ReasoningEffort != selected.effort ||
			settings.Policy != workspacecodex.LeadThreadPolicy("example", "/workspace", "") {
			t.Fatalf("root settings = %#v", settings)
		}
	}
}

type teamModelCatalogStub struct {
	models []codex.Model
	err    error
	calls  int
}

func (stub *teamModelCatalogStub) ListModels(context.Context) ([]codex.Model, error) {
	stub.calls++
	return stub.models, stub.err
}

func TestTeamMemberSettingsAcceptOptionalAddPairAndValidateExplicitPair(t *testing.T) {
	catalog := func() *teamModelCatalogStub {
		return &teamModelCatalogStub{models: []codex.Model{
			{Model: "member-model", DisplayName: "Member model", SupportedReasoningEfforts: []codex.ReasoningEffortOption{
				{ReasoningEffort: "medium"}, {ReasoningEffort: "high"},
			}},
		}}
	}
	for _, command := range []string{"add", "configure"} {
		for _, requested := range []codex.ThreadSettings{
			{}, {Model: "member-model"}, {ReasoningEffort: "high"},
		} {
			if command == "add" && requested == (codex.ThreadSettings{}) {
				continue
			}
			t.Run(command+"/missing-pair", func(t *testing.T) {
				client := catalog()
				_, err := resolveTeamMemberSettings(context.Background(), client, command, requested.Model, requested.ReasoningEffort)
				if err == nil || !strings.Contains(err.Error(), "requires --model and --effort") {
					t.Fatalf("missing pair error = %v", err)
				}
				if client.calls != 0 {
					t.Fatalf("missing pair loaded live models %d times", client.calls)
				}
			})
		}
	}
	t.Run("add uses role defaults when both omitted", func(t *testing.T) {
		client := catalog()
		settings, err := resolveTeamMemberSettings(context.Background(), client, "add", "", "")
		if err != nil || settings != (codex.ThreadSettings{}) || client.calls != 0 {
			t.Fatalf("optional add settings = %#v, error = %v, calls = %d", settings, err, client.calls)
		}
	})

	t.Run("accepts advertised pair", func(t *testing.T) {
		client := catalog()
		settings, err := resolveTeamMemberSettings(context.Background(), client, "add", "member-model", "high")
		if err != nil {
			t.Fatal(err)
		}
		if settings != (codex.ThreadSettings{Model: "member-model", ReasoningEffort: "high"}) || client.calls != 1 {
			t.Fatalf("settings = %#v, calls = %d", settings, client.calls)
		}
	})

	t.Run("rejects unavailable effort", func(t *testing.T) {
		client := catalog()
		_, err := resolveTeamMemberSettings(context.Background(), client, "configure", "member-model", "xhigh")
		if err == nil || !strings.Contains(err.Error(), "not available") || client.calls != 1 {
			t.Fatalf("unavailable pair error = %v, calls = %d", err, client.calls)
		}
	})

	t.Run("assign retains optional override semantics", func(t *testing.T) {
		client := catalog()
		settings, err := resolveTeamMemberSettings(context.Background(), client, "assign", "", "")
		if err != nil || settings != (codex.ThreadSettings{}) || client.calls != 0 {
			t.Fatalf("assign settings = %#v, error = %v, calls = %d", settings, err, client.calls)
		}
	})
}

func TestAgentTeamHelperRetainsRegistrationOnly(t *testing.T) {
	request := []byte(`{"schema":1}`)
	root, stateRoot, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	registration := []string{"registration", "--package-root", root, "--state-root", stateRoot, "--workspace", workspace}
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"retired static resolver", []string{"resolve-creation"}},
		{"retired live resolver", []string{"resolve-current-creation"}},
		{"retired publication command", []string{"publish-creation"}},
		{"retired lifecycle guard command", []string{"require-unmanaged"}},
		{"registration missing state authority", []string{"registration", "--package-root", root}},
		{"registration rejects socket", append(append([]string{}, registration...), "--socket", "/run/codex.sock")},
		{"registration rejects positional arguments", append(append([]string{}, registration...), "extra")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := agentTeamsCommandIO(testCase.args, bytes.NewReader(request), &bytes.Buffer{}); err == nil {
				t.Fatal("agent team helper accepted a retired command or invalid registration authority")
			}
		})
	}
	for _, invalid := range []string{`{}`, `{"schema":1,"team":"solo"}`, `{"schema":1,"schema":1}`} {
		if err := agentTeamsCommandIO(registration, strings.NewReader(invalid), &bytes.Buffer{}); err == nil {
			t.Fatalf("registration accepted invalid JSON request %s", invalid)
		}
	}
	if err := agentTeamsCommandIO(registration, bytes.NewReader(request), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "canonical package store root") {
		t.Fatalf("registration did not reach package validation: %v", err)
	}
}

func TestThreadEnsureInitialUsesOrdinaryRootTurn(t *testing.T) {
	workspace := t.TempDir()
	cwd := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "goal")
	if err := os.WriteFile(input, []byte("initial goal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func(socket string) []string {
		return []string{"ensure-initial", "--socket", socket, "--workspace", workspace,
			"--thread-id", "thread-1", "--cwd", cwd, "--input-file", input, "--start-unmaterialized"}
	}
	socket := threadCommandInitialAppServer(t, cwd)
	if err := threadCommand(base(socket)); err != nil {
		t.Fatal(err)
	}
}

func threadCommandInitialAppServer(t *testing.T, cwd string) string {
	t.Helper()
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	return threadCommandAppServer(t, func(connection *websocket.Conn) error {
		if err := threadCommandHandshake(connection); err != nil {
			return err
		}
		started := false
		for requestNumber := 0; requestNumber < 5; requestNumber++ {
			request, err := threadCommandReadObject(connection)
			if err != nil {
				return err
			}
			params, ok := request["params"].(map[string]any)
			if !ok {
				return fmt.Errorf("request has no parameters: %#v", request)
			}
			var result map[string]any
			switch request["method"] {
			case "thread/read":
				result = map[string]any{"thread": threadCommandFreshMetadata("thread-1", cwd, rollout)}
			case "turn/start":
				if started {
					return errors.New("initial turn was started twice")
				}
				if err := assertThreadCommandStartOptions(params); err != nil {
					return err
				}
				if err := os.WriteFile(rollout, []byte("materialized\n"), 0o600); err != nil {
					return err
				}
				started = true
				result = map[string]any{}
			case "thread/resume":
				result = map[string]any{}
			case "thread/turns/list":
				if !started {
					return errors.New("initial history was read before turn/start")
				}
				result = map[string]any{"data": []any{map[string]any{"items": []any{map[string]any{
					"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "initial goal"}},
				}}}}}
			default:
				return fmt.Errorf("unexpected App Server request %q", request["method"])
			}
			if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
				return err
			}
		}
		return nil
	})
}

func assertThreadCommandStartOptions(params map[string]any) error {
	if params["threadId"] != "thread-1" {
		return fmt.Errorf("turn/start thread = %#v", params)
	}
	for _, key := range []string{"model", "effort", "additionalContext"} {
		if _, found := params[key]; found {
			return fmt.Errorf("root turn/start unexpectedly included %s: %#v", key, params)
		}
	}
	return nil
}

func threadCommandFreshMetadata(id, cwd, rollout string) map[string]any {
	return map[string]any{
		"id": id, "cwd": cwd, "path": rollout, "preview": "", "source": "vscode",
		"ephemeral": false, "historyMode": "paginated", "status": map[string]any{"type": "idle"}, "turns": []any{},
	}
}

func threadCommandHandshake(connection *websocket.Conn) error {
	request, err := threadCommandReadObject(connection)
	if err != nil {
		return err
	}
	params, _ := request["params"].(map[string]any)
	capabilities, _ := params["capabilities"].(map[string]any)
	if request["method"] != "initialize" || capabilities["experimentalApi"] != true {
		return fmt.Errorf("invalid App Server initialization: %#v", request)
	}
	if err := threadCommandWriteObject(connection, map[string]any{
		"id": request["id"], "result": map[string]any{"userAgent": "codex-cli/99.0.0"},
	}); err != nil {
		return err
	}
	_, err = threadCommandReadObject(connection)
	return err
}

func threadCommandAppServer(t *testing.T, handler func(*websocket.Conn) error) string {
	t.Helper()
	// AF_UNIX leaves little room for the filename, while Go's per-test
	// directories include the full package/test path. Keep this generated path
	// short and remove only this exact private directory at test cleanup.
	directory, err := os.MkdirTemp("/tmp", "wpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "app-server.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	errorsChannel := make(chan error, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, acceptErr := websocket.Accept(w, r, nil)
		if acceptErr != nil {
			errorsChannel <- acceptErr
			return
		}
		go func() {
			if handlerErr := handler(connection); handlerErr != nil {
				errorsChannel <- handlerErr
			}
		}()
	})}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errorsChannel <- serveErr
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-errorsChannel:
			t.Errorf("fake App Server: %v", err)
		default:
		}
	})
	return socket
}

func threadCommandReadObject(connection *websocket.Conn) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := connection.Read(ctx)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func threadCommandWriteObject(connection *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return connection.Write(ctx, websocket.MessageText, data)
}

func TestServeRequiresExplicitManagedCreationAuthorities(t *testing.T) {
	if err := serve(nil); err == nil || !strings.Contains(err.Error(), "requires --user-state-root") {
		t.Fatalf("serve missing authority error = %v", err)
	}
	root := t.TempDir()
	err := serve([]string{
		"--user-state-root", root, "--package-root", root,
		"--workspace-name", "Invalid_Name", "--registration-marker", filepath.Join(root, "registration.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "workspace name is invalid") {
		t.Fatalf("serve workspace name error = %v", err)
	}
}

type runtimeContract struct {
	AgentTeamRegistration  session.AgentTeamRegistrationContract `json:"agentTeamRegistration"`
	TrackingMaxBytes       int                                   `json:"trackingMaxBytes"`
	MaxMessageBytes        int                                   `json:"maxMessageBytes"`
	FormEncodingExpansion  int                                   `json:"formEncodingExpansion"`
	JSONEncodingExpansion  int                                   `json:"jsonEncodingExpansion"`
	TransportEnvelopeBytes int                                   `json:"transportEnvelopeBytes"`
	LifecycleJournals      []session.LifecycleJournal            `json:"lifecycleJournals"`
	ThreadEnvironmentKeys  []string                              `json:"threadEnvironmentKeys"`
	PortalServeFlags       []string                              `json:"portalServeFlags"`
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
	if contract.AgentTeamRegistration.MaxPersistedScalarBytes != session.MaxAgentTeamPersistedScalarBytes {
		t.Fatalf("agent-team persisted scalar size = %d, want %d", contract.AgentTeamRegistration.MaxPersistedScalarBytes, session.MaxAgentTeamPersistedScalarBytes)
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

func TestAgentTeamRegistrationContractMatchesHelperConstants(t *testing.T) {
	contract := loadRuntimeContract(t).AgentTeamRegistration
	if contract != session.AgentTeamRegistration() {
		t.Fatalf("agent team registration contract = %#v, want %#v", session.AgentTeamRegistration(), contract)
	}
	if contract.HelperSchema != agentteams.HelperSchemaVersion || contract.Policy != agentteams.RegistrationPolicyVersion {
		t.Fatalf("agent team helper contract = %#v, helper=%d policy=%d", contract, agentteams.HelperSchemaVersion, agentteams.RegistrationPolicyVersion)
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

func TestArchivePreflightCommandsRequireDeployedCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
	workspace := t.TempDir()
	cwd := filepath.Join(workspace, "work", "example")
	socket := filepath.Join(t.TempDir(), "app-server.sock")
	authority := filepath.Join(t.TempDir(), "authority")
	if err := threadCommand([]string{
		"require-archived", "--thread-id", "11111111-1111-7111-8111-111111111111",
		"--cwd", cwd, "--socket", socket, "--authority-dir", authority,
		"--codex-home", filepath.Join(home, "other"),
	}); err == nil || !strings.Contains(err.Error(), "runtime provenance") {
		t.Fatalf("untrusted root home passed preflight: %v", err)
	}
	if err := teamCommand([]string{
		"require-archived", "--user-state-root", t.TempDir(), "--workspace", workspace,
		"--session-slug", "example", "--root-thread-id", "11111111-1111-7111-8111-111111111111",
		"--cwd", cwd, "--socket", socket, "--authority-dir", authority,
		"--codex-home", filepath.Join(home, "other"),
	}); err == nil || !strings.Contains(err.Error(), "session authority") {
		t.Fatalf("untrusted team home passed preflight: %v", err)
	}
	if err := teamCommand([]string{
		"require-archive-ready", "--user-state-root", t.TempDir(), "--workspace", workspace,
		"--session-slug", "example", "--root-thread-id", "11111111-1111-7111-8111-111111111111",
		"--cwd", cwd, "--socket", socket, "--authority-dir", authority,
		"--codex-home", filepath.Join(home, "other"),
	}); err == nil || !strings.Contains(err.Error(), "session authority") {
		t.Fatalf("untrusted archive-ready home passed preflight: %v", err)
	}
}

func TestTeamRequireArchivedAcceptsRootOnlySession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
	workspace := t.TempDir()
	cwd := filepath.Join(workspace, "work", "example")
	if err := teamCommand([]string{
		"require-archived", "--user-state-root", t.TempDir(), "--workspace", workspace,
		"--session-slug", "example", "--root-thread-id", "11111111-1111-7111-8111-111111111111",
		"--cwd", cwd, "--socket", filepath.Join(t.TempDir(), "app-server.sock"),
		"--authority-dir", t.TempDir(), "--codex-home", home,
	}); err != nil {
		t.Fatalf("root-only archive preflight: %v", err)
	}
}

func TestTeamRetainedArchiveRequiresTrustedAuthorityAndAcceptsRootOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEV_WORKSPACE_CODEX_HOME", home)
	workspace := t.TempDir()
	cwd := filepath.Join(workspace, "work", "example")
	args := []string{
		"archive", "--retained-only", "--user-state-root", t.TempDir(), "--workspace", workspace,
		"--session-slug", "example", "--root-thread-id", "11111111-1111-7111-8111-111111111111",
		"--cwd", cwd, "--socket", filepath.Join(t.TempDir(), "app-server.sock"),
		"--authority-dir", t.TempDir(), "--codex-home", home,
	}
	if err := teamCommand(args); err != nil {
		t.Fatalf("root-only retained archive: %v", err)
	}
	wrong := append([]string(nil), args...)
	wrong[len(wrong)-1] = filepath.Join(home, "other")
	if err := teamCommand(wrong); err == nil || !strings.Contains(err.Error(), "session authority") {
		t.Fatalf("untrusted retained archive passed: %v", err)
	}
	wrong[0] = "require-idle"
	if err := teamCommand(wrong); err == nil || !strings.Contains(err.Error(), "only valid") {
		t.Fatalf("retained-only nonarchive command passed: %v", err)
	}
}

func TestBoundRootForkCommandProcess(t *testing.T) {
	if os.Getenv("R2C_ROOT_FORK_PROCESS") != "1" {
		return
	}
	for index, argument := range os.Args {
		if argument == "--" {
			if err := run(os.Args[index+1:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

// CLI, journal, selected-package writers, locks, SDK and native guard are real.
// Source readiness/settings and terminal effects are doubled.
func TestBoundRootForkCLIJournalAndInheritedOwner(t *testing.T) {
	for _, fault := range []string{"none", "lost", "discarded", "missing-fd", "wrong-op", "browser", "browser-member", "browser-member-badfd", "browser-member-explicit"} {
		t.Run(fault, func(t *testing.T) {
			workspace, stateRoot, pkg := t.TempDir(), t.TempDir(), t.TempDir()
			destination := "destination"
			if strings.HasPrefix(fault, "browser") {
				destination = "2026-11-25-destination"
			}
			destinationProvider := "openai"
			if fault == "browser-member-explicit" {
				destinationProvider = "pool"
			}
			var native atomic.Int32
			var authorityDir string
			socket := threadCommandAppServer(t, func(conn *websocket.Conn) error {
				if err := threadCommandHandshake(conn); err != nil {
					return err
				}
				for {
					request, err := threadCommandReadObject(conn)
					if err != nil {
						return nil
					}
					params, _ := request["params"].(map[string]any)
					result := map[string]any{}
					switch request["method"] {
					case "thread/turns/list":
						result["data"] = []any{}
					case "thread/fork":
						native.Add(1)
						parent, child, model := "current-root", "saved-child", "saved-model"
						if params["threadId"] == "current-member" && strings.HasPrefix(fault, "browser-member") {
							parent, child, model = "current-member", "saved-member", "member-model"
						}
						if params["threadId"] != parent || params["cwd"] != filepath.Join(workspace, "work", destination) || params["modelProvider"] != destinationProvider || params["model"] != model {
							return fmt.Errorf("fork wire changed: %#v", params)
						}
						for _, slug := range []string{"source", destination} {
							lock, err := session.LockRuntimeExclusive(authorityDir, slug)
							if err == nil {
								lock.Close()
								return fmt.Errorf("runtime owner escaped: %s", slug)
							}
						}
						store, err := teamruntime.NewStore(stateRoot, workspace)
						if err != nil {
							return err
						}
						for _, slug := range []string{"source", destination} {
							release, err := store.TryLockOperation(slug)
							if err == nil {
								release()
								return fmt.Errorf("team owner escaped: %s", slug)
							}
						}
						if fault == "lost" {
							conn.CloseNow()
							return nil
						}
						if fault == "browser" {
							u, err := uploads.ForWorkspace(workspace, stateRoot)
							if err != nil {
								return err
							}
							if err := os.WriteFile(filepath.Join(u.Directory, "catalog.json"), []byte("{"), 0600); err != nil {
								return err
							}
						}
						provider := destinationProvider
						if fault == "discarded" {
							provider = "pool"
						}
						result["modelProvider"] = provider
						result["thread"] = map[string]any{"id": child, "cwd": filepath.Join(workspace, "work", destination), "forkedFromId": parent}
					case "thread/read":
						id, _ := params["threadId"].(string)
						if id == "saved-member" && strings.HasPrefix(fault, "browser-member") {
							result["thread"] = map[string]any{"id": id, "cwd": filepath.Join(workspace, "work", destination), "forkedFromId": "current-member", "source": "vscode", "ephemeral": false, "historyMode": "paginated", "path": filepath.Join(stateRoot, "member-rollout.jsonl"), "preview": "", "status": map[string]any{"type": "idle"}, "turns": []any{}}
							break
						}
						if id != "saved-child" {
							return fmt.Errorf("known retry reselected source: %#v", params)
						}
						result["thread"] = map[string]any{"id": "saved-child", "cwd": filepath.Join(workspace, "work", destination), "forkedFromId": "current-root", "source": "vscode", "ephemeral": false}
					case "thread/inject_items":
						if !strings.HasPrefix(fault, "browser-member") || params["threadId"] != "saved-member" {
							return fmt.Errorf("unexpected bootstrap %#v", params)
						}
						items, ok := params["items"].([]any)
						if !ok || len(items) != 1 {
							return errors.New("invalid bootstrap item")
						}
						meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": "saved-member"}})
						item, _ := json.Marshal(map[string]any{"type": "response_item", "payload": items[0]})
						if err := os.WriteFile(filepath.Join(stateRoot, "member-rollout.jsonl"), append(append(meta, '\n'), append(item, '\n')...), 0600); err != nil {
							return err
						}
					case "thread/name/set":
						if !strings.HasPrefix(fault, "browser-member") || params["threadId"] != "saved-member" {
							return errors.New("unexpected native naming")
						}
					default:
						return fmt.Errorf("unexpected fork effect %v", request["method"])
					}
					if err := threadCommandWriteObject(conn, map[string]any{"id": request["id"], "result": result}); err != nil {
						return nil
					}
				}
			})
			write := func(path string, data []byte, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			name := filepath.Base(filepath.Dir(socket))
			authorityDir = filepath.Join(filepath.Dir(socket), "authority")
			repo, err := filepath.Abs("../../..")
			if err != nil {
				t.Fatal(err)
			}
			scripts, err := filepath.Glob(filepath.Join(repo, "libexec", "*.rb"))
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range append(scripts, filepath.Join(repo, "libexec", "dev-session"), filepath.Join(repo, "libexec", "workspace-host")) {
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				for _, dir := range []string{"libexec", "libexec/workspace-portal"} {
					write(filepath.Join(pkg, dir, filepath.Base(source)), data, 0600)
				}
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(pkg, "bin", "workspace-portal"), data, 0700)
			data, err = os.ReadFile(filepath.Join(repo, "portal", "internal", "session", "runtime-contract.json"))
			if err != nil {
				t.Fatal(err)
			}
			var contract map[string]any
			if err := json.Unmarshal(data, &contract); err != nil {
				t.Fatal(err)
			}
			contract["codexProviderBindingStateSchema"], contract["codexProviderForkTransitionPolicy"], contract["codexRuntimeAuthoritySchema"] = 3, 1, 3
			encoded, _ := json.Marshal(contract)
			write(filepath.Join(pkg, "share/workspace-portal/runtime-contract.json"), encoded, 0600)
			write(filepath.Join(pkg, "share/dev-workspace/package.json"), []byte(`{"schema":1,"activationEnvironmentAliases":[],"routerSocket":"/run/test-router.sock","userNamespace":"dev-workspaces","agent_teams":{"managed":false}}`), 0600)
			if err := os.Symlink(pkg, filepath.Join(stateRoot, "profile")); err != nil {
				t.Fatal(err)
			}
			ruby, err := exec.LookPath("ruby")
			if err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(pkg, "bin/workspace-host"), []byte("#!"+ruby+"\nrequire 'json'\nif ARGV[0,2]==['provider','models']\n File.open(ENV.fetch('R2C_MODEL_TRACE'),'a'){|f|f.puts ARGV.join(' ')}\n puts JSON.generate([{'model'=>'saved-model','displayName'=>'Saved','supportedReasoningEfforts'=>[{'reasoningEffort'=>'high','description'=>'High'}]}])\n exit 0\nend\n$PROGRAM_NAME='root-host-fixture'\nload "+fmt.Sprintf("%q", filepath.Join(pkg, "libexec/workspace-host"))+"\nclass RootForkHostControl < DevWorkspaceHost::Host\n private\n def capture!(*argv)\n  return super unless argv.first == File.join(package_root,'bin/workspace-portal')\n  executable=argv.shift\n  super(executable,'-test.run=^TestBoundRootForkCommandProcess$','--',*argv)\n end\nend\nexit RootForkHostControl.new.run('workspace-host',ARGV)\n"), 0700)
			cwd := filepath.Join(workspace, "work", "source")
			for path, body := range map[string]string{filepath.Join(cwd, "portal.yml"): fmt.Sprintf("schema: 6\nslug: source\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: origin-root\n  socket_path: %s\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\n", socket), filepath.Join(cwd, "plan.md"): "Source\n", filepath.Join(cwd, "state.md"): "---\nlifecycle: active\n---\n"} {
				write(path, []byte(body), 0600)
			}
			if err := os.MkdirAll(filepath.Join(workspace, "repos"), 0700); err != nil {
				t.Fatal(err)
			}
			inc, op := strings.Repeat("a", 64), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			key := codexproviders.BindingKey{Workspace: workspace, Slug: "source", RootThreadID: "origin-root", Incarnation: inc, Member: "lead", OriginalThreadID: "origin-root"}
			evidence := codexproviders.BindingEvidence{Schema: 1, OperationID: op, Workspace: name, Root: workspace, Socket: socket, PreviousBindingRevision: 1, CommittedBindingRevision: 2, SnapshotDigest: strings.Repeat("e", 64), Participants: []codexproviders.BindingParticipant{{Key: key, OldThreadID: "origin-root", NewThreadID: "current-root", OldProvider: "openai", NewProvider: "openai"}}}
			if strings.HasPrefix(fault, "browser-member") {
				memberKey := key
				memberKey.Member, memberKey.OriginalThreadID = "implementer0", "origin-member"
				evidence.Participants = append(evidence.Participants, codexproviders.BindingParticipant{Key: memberKey, OldThreadID: "origin-member", NewThreadID: "current-member", OldProvider: "openai", NewProvider: "openai"})
			}
			sealed, _ := json.Marshal(evidence)
			sum := sha256.Sum256(sealed)
			state := codexproviders.State{Schema: 3, Workspace: name, Root: workspace, Socket: socket, Active: "openai", BindingRevision: 2, Bindings: map[string]codexproviders.SessionBinding{"source": {RootThreadID: "origin-root", Incarnation: inc, Revision: 2, Conversations: map[string]codexproviders.ConversationBinding{"origin-root": {Member: "lead", ThreadID: "current-root", Provider: "openai", OperationID: op, EvidenceDigest: hex.EncodeToString(sum[:])}}}}}
			if strings.HasPrefix(fault, "browser-member") {
				b := state.Bindings["source"]
				b.Conversations["origin-member"] = codexproviders.ConversationBinding{Member: "implementer0", ThreadID: "current-member", Provider: "openai", OperationID: op, EvidenceDigest: hex.EncodeToString(sum[:])}
				state.Bindings["source"] = b
				store, err := teamruntime.NewStore(stateRoot, workspace)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(context.Background(), "source", "origin-root", true, func(roster *teamruntime.Roster) error {
					roster.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Index: 0, Thread: "origin-member", Model: "member-model", Effort: "high", Behavior: "implementer", Purpose: "implementation", Instructions: "Implement retained work.", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			statePath, _ := codexproviders.StatePath(stateRoot, name)
			encoded, _ = json.Marshal(state)
			write(statePath, encoded, 0600)
			write(filepath.Join(filepath.Dir(statePath), op+".snapshot.json.evidence"), sealed, 0600)
			authority := session.RuntimeAuthority{Schema: 3, State: "ready", Slug: "source", Workspace: workspace, TmuxSocket: filepath.Join(filepath.Dir(socket), "tmux.sock"), TmuxSessionID: "$1", TmuxIdentity: inc, CodexThreadID: "origin-root", CodexActiveThreadID: "current-root", CodexBindingRevision: 2, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
			encoded, _ = json.Marshal(authority)
			write(filepath.Join(authorityDir, "source.json"), encoded, 0600)
			catalog := filepath.Join(stateRoot, "catalog.json")
			write(catalog, []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"},{"id":"pool","label":"Pool","kind":"codex-lb","baseURL":"https://pool.test/v1","tokenFile":"/token"}]}`), 0600)
			registry := filepath.Join(stateRoot, "registry.json")
			encoded, _ = json.Marshal(map[string]any{"schema": 1, "workspaces": []any{map[string]any{"name": name, "root": workspace, "hostname": "test.example", "aliases": []string{}}}})
			write(registry, encoded, 0600)
			u, err := uploads.ForWorkspace(workspace, stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(fault, "browser") {
				write(filepath.Join(u.Directory, "catalog.json"), []byte("{"), 0600)
			} // ancillary failure after durable known
			driver := filepath.Join(stateRoot, "driver.rb")
			write(driver, []byte(boundRootForkRubyDriver), 0600)
			if strings.HasPrefix(fault, "browser") {
				manifestPath := filepath.Join(cwd, "portal.yml")
				body, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				write(manifestPath, append(body, []byte("creation:\n  state: ready\n  initial_goal_sent: true\n")...), 0600)
				tmux := filepath.Join(stateRoot, "tmux")
				row := strings.Join([]string{"$1", "source", "1", "source", workspace, "source", authority.TmuxSocket, "current-root", socket, "0.172.0", "%1", inc}, "\t")
				write(tmux, []byte("#!/bin/sh\ncat <<'ROW'\n"+row+"\nROW\n"), 0700)
				devSession := filepath.Join(pkg, "bin", "dev-session")
				write(devSession, []byte("#!/bin/sh\nexec "+ruby+" "+driver+" "+pkg+" "+workspace+" "+stateRoot+" "+socket+" "+fault+" "+destination+" \"$@\"\n"), 0700)
				for key, value := range map[string]string{"R2C_ROOT_FORK_PROCESS": "1", "DEV_WORKSPACES_CODEX_PROVIDERS": catalog, "DEV_WORKSPACES_CONFIG": registry, "DEV_WORKSPACES_RUNTIME_DIR": filepath.Dir(filepath.Dir(socket)), "DEV_WORKSPACES_STATE": stateRoot, "DEV_WORKSPACE_NAME": name, "R2C_MODEL_TRACE": filepath.Join(stateRoot, "models.trace")} {
					t.Setenv(key, value)
				}
				client := &boundRootBrowserClient{Client: workspacecodex.NewWithOptions(socket, workspace, codex.ClientOptions{}), t: t}
				client.RecoveryRoot = stateRoot
				defer client.Close()
				application, err := portalweb.New(portalweb.Config{Workspace: workspace, UserStateRoot: stateRoot, BaseURL: "https://workspace.example.test",
					DevSession: devSession, HostProfile: filepath.Join(stateRoot, "profile"), AuthorityDir: authorityDir, CodexSocket: socket, CodexVersion: "0.172.0",
					PackageRoot: pkg, WorkspaceName: name, RegistrationMarker: filepath.Join(filepath.Dir(socket), "registration.json"), Tmux: tmux, Codex: compositionWithoutInstructions{client},
					VerifyThread: func(_ context.Context, id, cwd string) error {
						if id != "current-root" {
							return fmt.Errorf("browser verified ancestor %s", id)
						}
						return nil
					}})
				if err != nil {
					t.Fatal(err)
				}
				defer application.Close()
				payload := map[string]string{"name": "destination", "creationDate": "2026-11-25", "model": "saved-model", "reasoningEffort": "high"}
				if fault == "browser-member-explicit" {
					payload["provider"] = "pool"
				}
				payloadBytes, _ := json.Marshal(payload)
				request := httptest.NewRequest(http.MethodPost, "/api/sessions/source/fork", bytes.NewReader(payloadBytes))
				request.Header.Set("Origin", "https://workspace.example.test")
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				application.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusAccepted {
					t.Fatal("actual browser acceptance", response.Code, response.Body.String())
				}
				var accepted struct {
					ReceiptID string `json:"receiptId"`
					Attempt   int    `json:"attempt"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || accepted.ReceiptID == "" || accepted.Attempt != 1 {
					t.Fatal("invalid accepted receipt", response.Body.String(), err)
				}
				receiptPath := filepath.Join(filepath.Dir(u.Directory), "creations", destination+".json")
				var originalReceipt struct {
					Request json.RawMessage `json:"request"`
				}
				originalBytes, err := os.ReadFile(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(originalBytes, &originalReceipt); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(workspace, "worktrees", ".locks", destination+".fork.json")
				deadline := time.Now().Add(8 * time.Second)
				var journal *boundRootForkJournal
				for time.Now().Before(deadline) {
					journal, _, err = readBoundRootForkJournal(path)
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(fault, "browser-member") {
						status := httptest.NewRecorder()
						application.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/sessions/"+destination+"/creation", nil))
						var result struct {
							State string `json:"state"`
						}
						_ = json.Unmarshal(status.Body.Bytes(), &result)
						if result.State == "ready" || fault == "browser-member-badfd" && result.State == "failed" {
							break
						}
					} else if journal != nil && journal.RootFork.State == "known" {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if strings.HasPrefix(fault, "browser-member") {
					status := httptest.NewRecorder()
					application.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/sessions/"+destination+"/creation", nil))
					var result struct {
						State     string `json:"state"`
						ReceiptID string `json:"receiptId"`
						Attempt   int    `json:"attempt"`
					}
					decodeErr := json.Unmarshal(status.Body.Bytes(), &result)
					expected := "ready"
					if fault == "browser-member-badfd" {
						expected = "failed"
					}
					if status.Code != http.StatusOK || decodeErr != nil || result.State != expected || result.ReceiptID != accepted.ReceiptID || result.Attempt != accepted.Attempt {
						t.Fatal("terminal receipt was not proved", status.Code, status.Body.String(), decodeErr)
					}
					var durable struct {
						Request json.RawMessage `json:"request"`
					}
					data, err := os.ReadFile(receiptPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &durable); err != nil || !bytes.Equal(durable.Request, originalReceipt.Request) {
						t.Fatal("accepted request/capture changed during completion", err)
					}
				}
				if fault == "browser-member-badfd" {
					store, err := teamruntime.NewStore(stateRoot, workspace)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.Load(destination, "saved-child"); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("bad inherited team descriptor wrote roster", err)
					}
					if journal == nil || journal.RootFork.State != "known" {
						t.Fatal("bad team descriptor lost exact root", journal)
					}
				} else if strings.HasPrefix(fault, "browser-member") {
					store, err := teamruntime.NewStore(stateRoot, workspace)
					if err != nil {
						t.Fatal(err)
					}
					roster, err := store.Load(destination, "saved-child")
					if err != nil || len(roster.Members) != 1 || roster.Members[0].Thread != "saved-member" || roster.Members[0].State != "ready" || roster.ForkSource.RootThreadID != "origin-root" || roster.ForkSource.SourceNativeThreadID != "current-root" || roster.ForkSource.Members[0].Thread != "origin-member" || roster.ForkSource.Members[0].SourceNativeThreadID != "current-member" {
						t.Fatal("actual member fork composition", roster, err)
					}
					if journal != nil {
						t.Fatal("completed member fork retained journal")
					}
				} else if journal == nil || journal.RootFork.State != "known" || journal.RootFork.ThreadID != "saved-child" {
					status := httptest.NewRecorder()
					application.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/sessions/"+destination+"/creation", nil))
					t.Fatal("browser worker did not reach exact durable root", journal, status.Body.String())
				}
				want := int32(1)
				if strings.HasPrefix(fault, "browser-member") && fault != "browser-member-badfd" {
					want = 2
				}
				if native.Load() != want {
					t.Fatal("browser fork count", native.Load(), want)
				}
				if strings.HasPrefix(fault, "browser-member") {
					checks, err := os.ReadFile(filepath.Join(stateRoot, "parent-checks"))
					if err != nil || string(checks) != "root\nteam\n" {
						t.Fatal("team child did not preserve parent OFD", string(checks), err)
					}
				}
				trace, err := os.ReadFile(filepath.Join(stateRoot, "models.trace"))
				if err != nil || strings.TrimSpace(string(trace)) != "provider models "+name+" "+destinationProvider {
					t.Fatal("destination models did not use one real cache/launcher", string(trace), err)
				}
				if fault == "browser-member-explicit" {
					var after codexproviders.State
					data, err := os.ReadFile(statePath)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &after); err != nil {
						t.Fatal(err)
					}
					authority, err := session.LoadRuntimeAuthority(authorityDir, destination, workspace)
					if err != nil {
						t.Fatal(err)
					}
					selection := after.Sessions[destination]
					if after.Schema != 3 || after.BindingRevision != state.BindingRevision || after.Active != state.Active || !reflect.DeepEqual(after.Bindings, state.Bindings) || len(after.Sessions) != 1 ||
						selection.Provider != "pool" || selection.CreationID != authority.TmuxIdentity || selection.RootThreadID != "saved-child" {
						t.Fatal("creation/root writer changed schema3 source state", after)
					}
				}
				return
			}
			command := exec.Command(ruby, driver, pkg, workspace, stateRoot, socket, fault, destination)
			command.Env = append(os.Environ(), "R2C_ROOT_FORK_PROCESS=1", "DEV_WORKSPACES_CODEX_PROVIDERS="+catalog, "DEV_WORKSPACES_CONFIG="+registry, "DEV_WORKSPACES_RUNTIME_DIR="+filepath.Dir(filepath.Dir(socket)), "DEV_WORKSPACES_STATE="+stateRoot, "DEV_WORKSPACE_NAME="+name)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("actual Ruby CLI/root composition: %v\n%s", err, output)
			}
			expected := int32(1)
			if fault == "missing-fd" || fault == "wrong-op" {
				expected = 0
			}
			if native.Load() != expected {
				t.Fatalf("dispatch count %d, want %d", native.Load(), expected)
			}
		})
	}
}

type boundRootBrowserClient struct {
	*workspacecodex.Client
	t *testing.T
}

// These worker controls exclude startup instruction reconciliation. Expose the
// existing browser/team interfaces, without the private current-lead capability;
// TestStartupReconcilesCurrentLeadInstructions uses the real capability below.
type compositionController interface {
	conversation.Client
	teamruntime.Client
	teamruntime.ObservationClient
	ReadAccountRateLimits(context.Context) (codex.AccountRateLimits, error)
	ListThreadActivity(context.Context, []workspacecodex.ThreadActivity) ([]workspacecodex.ThreadActivity, error)
	PrepareSend(string, string, string, string, bool) error
	ReconcileSend(context.Context, string, string, string, string) (codex.SendReceipt, bool, error)
	DiscardPreparedSend(string, string, string, string) (bool, error)
	ReconcileThreadInstructionsWithPolicy(context.Context, string, codex.ThreadPolicy) error
}

type compositionWithoutInstructions struct{ compositionController }

func (client *boundRootBrowserClient) ReadThread(_ context.Context, id string) (codex.Transcript, error) {
	if id != "current-root" {
		client.t.Errorf("browser transcript target %s", id)
	}
	return codex.Transcript{Model: "saved-model", ReasoningEffort: "high", Status: "idle"}, nil
}

const boundRootForkRubyDriver = `require 'stringio'
require 'json'
pkg,workspace,state,socket,fault,destination=ARGV.shift(6)
forwarded=ARGV.dup
load File.join(pkg,'libexec/workspace-portal/dev-session')
class RootForkCLIControl < DevSession::Runner
 private
 def require_creationless_retained_ready!(*,**) = nil
 def require_portal_thread_materialized(*) = nil
 def resolve_portal_fork_settings(*,**) = ['saved-model','high']
 def frozen_direct_team_lead_instructions(*) = nil
 def name_portal_thread(*)
  raise DevSession::Error,'terminal boundary' unless @control_mode.start_with?('browser-member')
 end
 def create_tmux_session(slug,**options)
  raise DevSession::Error,'terminal boundary' unless @control_mode.start_with?('browser-member')
  DevSession::Tmux::Session.new(id:'$2',name:slug,mark:'1',slug:,environment_slug:slug,workspace:,
   socket_path:@configured_tmux_socket,codex_thread_id:options.fetch(:thread_id),codex_socket_path:@codex_socket,
   codex_client_version:@codex_version,codex_pane_id:'%2',identity_token:options.fetch(:identity_token))
 end
 def sync_slug(slug,**options)
  return options.fetch(:session) if @control_mode.start_with?('browser-member')
  super
 end
end
tmux=Object.new
tmux.define_singleton_method(:session){|*|nil}
tmux.define_singleton_method(:sessions){[]}
tmux.define_singleton_method(:managed_session_identities){[]}
actual=DevSession::CommandRunner.new(out:StringIO.new,err:StringIO.new)
runner=RootForkCLIControl.new(workspace:,tmux:,out:forwarded.empty? ? StringIO.new : $stdout,err:StringIO.new,env:ENV,
 require_runtime:true,codex_command:'/test/no-native-executable',
 authority_dir:File.join(File.dirname(socket),'authority'),codex_socket:socket,codex_version:'0.172.0',
 portal_command:[File.join(pkg,'bin/workspace-portal'),'-test.run=^TestBoundRootForkCommandProcess$','--'],
 tmux_socket:File.join(File.dirname(socket),'tmux.sock'),command_runner:actual)
runner.instance_variable_set(:@control_mode,fault)
capture=actual.method(:capture)
checks=0
actual.define_singleton_method(:capture) do |argv,**options|
 if argv.include?('fork') && (argv.include?('thread') || argv.include?('team'))
  opts=options.dup
  if fault=='missing-fd' || fault=='browser-member-badfd' && argv.include?('team')
   opts[:env]=opts.fetch(:env).merge('DEV_SESSION_LIFECYCLE_LOCK_FD'=>'9999')
  elsif fault=='wrong-op'
   opts[:env]=opts.fetch(:env).merge('DEV_SESSION_LIFECYCLE_OPERATION'=>'archive')
  end
  begin
   capture.call(argv,**opts)
  ensure
   lock=runner.instance_variable_get(:@held_slug_lock)
   File.open(lock.fetch(:path),File::RDWR){|probe|raise 'child released parent OFD' if probe.flock(File::LOCK_SH|File::LOCK_NB)}
   File.open(File.join(state,'parent-checks'),'a'){|f|f.puts(argv.include?('team') ? 'team' : 'root')}
   checks+=1
  end
 else
  capture.call(argv,**options)
 end
end
state_path=File.join(state,'providers',ENV.fetch('DEV_WORKSPACE_NAME'),'state.json')
before=File.binread(state_path)
errors=StringIO.new
invoke=lambda do
 cli=DevSession::CLI.new(['--workspace',workspace,'--host-profile',File.join(state,'profile'),
  '--expected-host-generation',pkg,'--expected-host-profile-token',DevWorkspaceProfileIdentity.token(File.join(state,'profile')),
  '--transition-lock',File.join(state,'transition.lock'),*(forwarded.empty? ? ['fork','source',destination,'--as-is','--json'] : forwarded)],
  out:StringIO.new,err:errors,env:ENV)
 cli.define_singleton_method(:runner){runner}
 status=cli.run
 raise 'fixture unexpectedly completed terminal effects' if forwarded.empty? && status!=1
 status
end
status=invoke.call
unless forwarded.empty?; STDERR.write(errors.string); exit status; end
path=File.join(workspace,'worktrees','.locks',destination+'.fork.json')
journal=runner.send(:load_fork_journal,destination,'source')
expected=fault=='none' ? 'known' : %w[missing-fd wrong-op].include?(fault) ? 'not-attempted' : 'intent'
raise "wrong durable progress #{journal.inspect}: #{errors.string}" unless journal && journal.dig('root_fork','state')==expected
raise 'wrong saved child' if expected=='known' && journal.dig('root_fork','threadId')!='saved-child'
saved=File.binread(path)
invoke.call
raise 'retry rewrote accepted request/progress' unless File.binread(path)==saved
raise "wrong inherited owner check count: #{checks}" unless checks==2
raise 'inherited default or schema3 history changed' unless File.binread(state_path)==before
puts "root #{fault}: #{expected}, exact replay/no replay and retained parent exclusion"
`

func TestLifecycleCommandInputAndInheritedOwner(t *testing.T) {
	id := strings.Repeat("b", 64)
	capture := `{"incarnation":"` + strings.Repeat("a", 64) + `","revision":18446744073709551615,"threadId":"native-root","members":[]}`
	valid := `{"operation_id":"` + id + `","codex_targets":` + capture + `}`
	for _, data := range []string{valid, `{"operation_id":"` + id + `"}`} {
		value, err := readLifecycleCommandInput(strings.NewReader(data))
		if err != nil || value.OperationID != id {
			t.Fatal(value, err)
		}
		if strings.Contains(data, "codex_targets") && (value.Targets == nil || value.Targets.Revision != ^uint64(0)) {
			t.Fatal("capture lost uint64", value)
		}
	}
	for _, data := range []string{"", `null`, `{}`, `{"operation_id":null}`, `{"operation_id":"` + id + `","codex_targets":null}`, valid + `{}`, strings.Replace(valid, `"operation_id":`, `"operation_id":"`+id+`","operation_id":`, 1), strings.Replace(valid, `"revision":`, `"revision":1,"revision":`, 1), strings.Replace(valid, `"codex_targets":`, `"other":`, 1), valid + strings.Repeat(" ", 64<<10)} {
		if _, err := readLifecycleCommandInput(strings.NewReader(data)); err == nil {
			t.Fatalf("accepted invalid lifecycle input %q", data[:min(len(data), 200)])
		}
	}
	for _, operation := range []string{"archive", "delete", "revive"} {
		t.Run(operation, func(t *testing.T) {
			workspace, authority := t.TempDir(), t.TempDir()
			path := filepath.Join(authority, "example.lock")
			owner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", operation)
			t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_FD", fmt.Sprint(owner.Fd()))
			t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", path)
			child, err := inheritedLifecycleParentLock(workspace, authority, "example", operation)
			if err != nil {
				t.Fatal(err)
			}
			child.Close()
			probe, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); err == nil {
				t.Fatal("child released parent OFD")
			}
			t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", path+".wrong")
			if child, err := inheritedLifecycleParentLock(workspace, authority, "example", operation); err == nil {
				child.Close()
				t.Fatal("wrong path admitted")
			}
			t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", path)
			t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "fork")
			if child, err := inheritedLifecycleParentLock(workspace, authority, "example", operation); err == nil {
				child.Close()
				t.Fatal("wrong operation admitted")
			}
			t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", operation)
			unix.Flock(int(owner.Fd()), unix.LOCK_UN)
			if child, err := inheritedLifecycleParentLock(workspace, authority, "example", operation); err == nil {
				child.Close()
				t.Fatal("unheld descriptor admitted")
			}
			if err := unix.Flock(int(owner.Fd()), unix.LOCK_SH); err != nil {
				t.Fatal(err)
			}
			if child, err := inheritedLifecycleParentLock(workspace, authority, "example", operation); err == nil {
				child.Close()
				t.Fatal("shared descriptor admitted")
			}
			unix.Flock(int(owner.Fd()), unix.LOCK_UN)
		})
	}
}

// HTTP receipt, Ruby journal/sidecar and Go/SDK retirement are real. Terminal,
// source readiness and host authorization are explicit doubles in the driver.
func TestLifecycleBrowserReceiptToSavedNativeRootAndMember(t *testing.T) {
	testLifecycleComposition(t, "receipt")
}
func TestLifecycleFreshCLIHasOneTupleBeforeEffectsAndInventory(t *testing.T) {
	for _, mode := range []string{"fresh", "partial"} {
		t.Run(mode, func(t *testing.T) { testLifecycleComposition(t, mode) })
	}
}
func TestLifecycleCLIResumesExactPreludeAndSidecar(t *testing.T) {
	for _, mode := range []string{"prelude", "sidecar"} {
		t.Run(mode, func(t *testing.T) { testLifecycleComposition(t, mode) })
	}
}
func TestLifecycleDeleteCLIUsesSavedNativeTargetsAndRetainedCompletion(t *testing.T) {
	testLifecycleComposition(t, "delete")
}
func TestLifecycleDeleteMetadataAndLostReplyKeepExactOwner(t *testing.T) {
	for _, mode := range []string{"delete-metadata", "delete-lost"} {
		t.Run(mode, func(t *testing.T) { testLifecycleComposition(t, mode) })
	}
}
func TestLifecycleReviveUsesRetainedStartAndSavedNativeMembers(t *testing.T) {
	testLifecycleComposition(t, "revive")
}
func TestLifecycleBrowserDeleteUsesCapturedTargetsAndRetainedCompletion(t *testing.T) {
	testLifecycleComposition(t, "receipt-delete")
}
func TestLifecycleDeleteUnstartedNullReservationHasNoNativeMember(t *testing.T) {
	testLifecycleComposition(t, "delete-null")
}
func TestStartupReconcilesCurrentLeadInstructions(t *testing.T) {
	testLifecycleComposition(t, "instructions")
}
func testLifecycleComposition(t *testing.T, mode string) {
	workspace, stateRoot, pkg, home := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	const slug = "2026-11-25-lifecycle"
	const originalRoot = "11111111-1111-7111-8111-111111111111"
	const nativeRoot = "22222222-2222-7222-8222-222222222222"
	const originalMember = "33333333-3333-7333-8333-333333333333"
	const nativeMember = "44444444-4444-7444-8444-444444444444"
	cwd := filepath.Join(workspace, "work", slug)
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	archived := map[string]bool{}
	var nativeMu sync.Mutex
	var drift atomic.Bool
	var reviveStarted atomic.Bool
	var rootReplyDropped atomic.Bool
	var calls []string
	var restoreCalls []string
	var instructionCalls []string
	var authorityDir string
	metadata := func(id string) map[string]any {
		where, status := "sessions", "idle"
		if archived[id] {
			where, status = "archived_sessions", "notLoaded"
		}
		directory := cwd
		if drift.Load() && id == nativeRoot {
			directory = filepath.Join(cwd, "changed")
		}
		return map[string]any{"id": id, "cwd": directory, "source": "vscode", "ephemeral": false, "historyMode": "paginated", "preview": "fixture", "model": "fixture-model", "modelProvider": "openai", "reasoningEffort": "high", "status": map[string]any{"type": status}, "path": filepath.Join(home, where, "rollout-2026-11-25T12-00-00-"+id+".jsonl"), "turns": []any{}}
	}
	socket := threadCommandAppServer(t, func(conn *websocket.Conn) error {
		defer conn.CloseNow()
		init, err := threadCommandReadObject(conn)
		if err != nil {
			return err
		}
		initParams, _ := init["params"].(map[string]any)
		info, _ := initParams["clientInfo"].(map[string]any)
		capabilities, _ := initParams["capabilities"].(map[string]any)
		if init["method"] != "initialize" || capabilities["experimentalApi"] != true {
			return errors.New("invalid lifecycle initialization")
		}
		browserReadDouble := info["name"] == "lifecycle-browser-double"
		if err := threadCommandWriteObject(conn, map[string]any{"id": init["id"], "result": map[string]any{"userAgent": "codex-cli/99.0.0"}}); err != nil {
			return err
		}
		if _, err := threadCommandReadObject(conn); err != nil {
			return err
		}
		for {
			request, err := threadCommandReadObject(conn)
			if err != nil {
				return nil
			}
			params, _ := request["params"].(map[string]any)
			method, _ := request["method"].(string)
			nativeMu.Lock()
			result := map[string]any{}
			id, _ := params["threadId"].(string)
			if id != "" && id != nativeRoot && id != nativeMember && !(reviveStarted.Load() && id == originalRoot && (method == "thread/read" || method == "thread/turns/list" || method == "thread/queue/list" || method == "thread/goal/get")) && !(browserReadDouble && id == originalRoot && (method == "thread/read" || method == "thread/turns/list" || method == "thread/queue/list" || method == "thread/goal/get")) {
				nativeMu.Unlock()
				return fmt.Errorf("ancestor/foreign target consulted: %s %s", method, id)
			}
			switch method {
			case "thread/read":
				result["thread"] = metadata(id)
			case "thread/goal/get":
				result["goal"] = nil
			case "thread/settings/update":
				if !reviveStarted.Load() || id != nativeRoot || params["approvalPolicy"] != "never" {
					nativeMu.Unlock()
					return errors.New("unexpected retained settings update")
				}
				if mode == "instructions" {
					lock, lockErr := session.LockRuntimeExclusive(authorityDir, slug)
					if lockErr == nil {
						lock.Close()
						nativeMu.Unlock()
						return errors.New("settings escaped parent runtime owner")
					}
					store, _ := teamruntime.NewStore(stateRoot, workspace)
					if release, lockErr := store.TryLockOperation(slug); lockErr == nil {
						release()
						nativeMu.Unlock()
						return errors.New("settings escaped team owner")
					}
				}
				instructionCalls = append(instructionCalls, method+":"+id)
			case "thread/turns/list", "thread/queue/list", "thread/loaded/list":
				result["data"] = []any{}
			case "thread/list":
				result["data"] = []any{}
				wantArchived, _ := params["archived"].(bool)
				if archived[nativeRoot] == wantArchived {
					result["data"] = []any{metadata(nativeRoot)}
				}
				if reviveStarted.Load() && wantArchived {
					result["data"] = append(result["data"].([]any), metadata(originalRoot))
				}
			case "thread/unarchive", "thread/resume":
				if mode != "revive" && mode != "instructions" || browserReadDouble || id == "" || mode == "instructions" && method == "thread/unarchive" {
					nativeMu.Unlock()
					return errors.New("unexpected lifecycle restore")
				}
				if lock, lockErr := session.LockRuntimeExclusive(authorityDir, slug); lockErr == nil {
					lock.Close()
					nativeMu.Unlock()
					return errors.New("restore escaped parent runtime owner")
				}
				store, storeErr := teamruntime.NewStore(stateRoot, workspace)
				if storeErr != nil {
					nativeMu.Unlock()
					return storeErr
				}
				if release, lockErr := store.TryLockOperation(slug); lockErr == nil {
					release()
					nativeMu.Unlock()
					return errors.New("restore escaped team owner")
				}
				if method == "thread/unarchive" {
					if !archived[id] {
						nativeMu.Unlock()
						return errors.New("restore replayed unarchive")
					}
					archived[id] = false
				} else if id != nativeRoot || mode != "instructions" && params["cwd"] != cwd || params["modelProvider"] != "openai" {
					nativeMu.Unlock()
					return errors.New("retained root resume changed identity/provider")
				}
				restoreCalls = append(restoreCalls, method+":"+id)
				if mode == "instructions" {
					config, _ := params["config"].(map[string]any)
					if params["model"] != "fixture-model" || config["model_reasoning_effort"] != "high" {
						nativeMu.Unlock()
						return fmt.Errorf("instruction reconciliation lost current settings: %#v", params)
					}
					instructionCalls = append(instructionCalls, method+":"+id)
				}
				result["thread"] = metadata(id)
				result["modelProvider"] = "openai"
			case "thread/archive":
				if browserReadDouble {
					nativeMu.Unlock()
					return errors.New("browser read double attempted lifecycle dispatch")
				}
				lock, lockErr := session.LockRuntimeExclusive(authorityDir, slug)
				if lockErr == nil {
					lock.Close()
					nativeMu.Unlock()
					return errors.New("parent runtime exclusion escaped")
				}
				store, storeErr := teamruntime.NewStore(stateRoot, workspace)
				if storeErr != nil {
					nativeMu.Unlock()
					return storeErr
				}
				if release, lockErr := store.TryLockOperation(slug); lockErr == nil {
					release()
					nativeMu.Unlock()
					return errors.New("native call escaped actual team owner")
				}
				if archived[id] {
					nativeMu.Unlock()
					return errors.New("archive replayed an already archived saved target")
				}
				calls = append(calls, id)
				archived[id] = true
				header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": cwd}})
				path := metadata(id)["path"].(string)
				if err := os.WriteFile(path, append(header, '\n'), 0600); err != nil {
					nativeMu.Unlock()
					return err
				}
				if mode == "delete-lost" && id == nativeRoot && !rootReplyDropped.Swap(true) {
					nativeMu.Unlock()
					conn.CloseNow()
					return nil
				}
			default:
				nativeMu.Unlock()
				return fmt.Errorf("unexpected lifecycle RPC %s", method)
			}
			nativeMu.Unlock()
			if err := threadCommandWriteObject(conn, map[string]any{"id": request["id"], "result": result}); err != nil {
				return nil
			}
		}
	})
	authorityDir = filepath.Join(filepath.Dir(socket), "authority")
	if err := os.MkdirAll(authorityDir, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(filepath.Dir(socket))
	for _, id := range []string{nativeRoot, nativeMember} {
		if mode == "instructions" && id == nativeMember {
			continue
		}
		data := []byte("{}\n")
		if mode == "instructions" {
			header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": cwd}})
			data = append(header, '\n')
		}
		write(filepath.Join(home, "sessions", "rollout-2026-11-25T12-00-00-"+id+".jsonl"), data, 0600)
	}
	if err := os.MkdirAll(filepath.Join(home, "archived_sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if mode == "revive" || mode == "instructions" {
		archived[originalRoot] = true
		header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": originalRoot, "cwd": cwd}})
		write(metadata(originalRoot)["path"].(string), append(header, '\n'), 0600)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Glob(filepath.Join(repo, "libexec", "*.rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range append(scripts, filepath.Join(repo, "libexec", "dev-session"), filepath.Join(repo, "libexec", "workspace-host")) {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{"libexec", "libexec/workspace-portal"} {
			write(filepath.Join(pkg, dir, filepath.Base(source)), data, 0600)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(pkg, "bin/workspace-portal"), data, 0700)
	contractBytes, err := os.ReadFile(filepath.Join(repo, "portal/internal/session/runtime-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var contract map[string]any
	if err := json.Unmarshal(contractBytes, &contract); err != nil {
		t.Fatal(err)
	}
	contract["codexProviderBindingStateSchema"], contract["codexProviderForkTransitionPolicy"], contract["codexRuntimeAuthoritySchema"] = 3, 1, 3
	encoded, _ := json.Marshal(contract)
	write(filepath.Join(pkg, "share/workspace-portal/runtime-contract.json"), encoded, 0600)
	write(filepath.Join(pkg, "share/dev-workspace/package.json"), []byte(`{"schema":1,"activationEnvironmentAliases":[],"routerSocket":"/run/test-router.sock","userNamespace":"dev-workspaces","agent_teams":{"managed":false}}`), 0600)
	if err := os.Symlink(pkg, filepath.Join(stateRoot, "profile")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(cwd, "portal.yml"), []byte(fmt.Sprintf("schema: 6\nslug: %s\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: %s\n  socket_path: %s\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\ncreation:\n  state: ready\n  initial_goal_sent: true\n", slug, originalRoot, socket)), 0600)
	if mode == "delete-null" {
		manifest, readErr := os.ReadFile(filepath.Join(cwd, "portal.yml"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		body := strings.ReplaceAll(string(manifest), "binding_origin_schema: 1", "binding_origin_schema: 2")
		body = strings.ReplaceAll(body, "state: ready\n  initial_goal_sent: true", "state: creating\n  initial_goal_attempted: false\n  initial_goal_sent: false")
		write(filepath.Join(cwd, "portal.yml"), []byte(body), 0600)
	}
	write(filepath.Join(cwd, "plan.md"), []byte("Lifecycle fixture\n"), 0600)
	write(filepath.Join(cwd, "state.md"), []byte("---\nlifecycle: active\n---\n"), 0600)
	for _, dir := range []string{"repos", "worktrees"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "prelude" {
		var tracking unix.Stat_t
		if err := unix.Lstat(cwd, &tracking); err != nil {
			t.Fatal(err)
		}
		marker, _ := json.Marshal(map[string]any{"schema": 1, "workspace": workspace, "slug": slug, "registry_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "state": "initializing", "created_at": "2026-11-25T12:00:00Z", "retained_thread_id": originalRoot, "tracking_identity": map[string]any{"dev": tracking.Dev, "ino": tracking.Ino}})
		write(filepath.Join(workspace, "worktrees", ".locks", slug+".services-registration.json"), marker, 0600)
	}
	for _, argv := range [][]string{{"git", "init", "-b", "master", workspace}, {"git", "-C", workspace, "config", "user.name", "Fixture"}, {"git", "-C", workspace, "config", "user.email", "fixture@example.test"}} {
		if output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, output)
		}
	}
	inc, op := strings.Repeat("a", 64), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	evidence := codexproviders.BindingEvidence{Schema: 1, OperationID: op, Workspace: name, Root: workspace, Socket: socket, PreviousBindingRevision: 1, CommittedBindingRevision: 2, SnapshotDigest: strings.Repeat("e", 64)}
	for _, row := range [][3]string{{"lead", originalRoot, nativeRoot}, {"implementer0", originalMember, nativeMember}} {
		if (mode == "delete-null" || mode == "instructions") && row[0] != "lead" {
			continue
		}
		evidence.Participants = append(evidence.Participants, codexproviders.BindingParticipant{Key: codexproviders.BindingKey{Workspace: workspace, Slug: slug, RootThreadID: originalRoot, Incarnation: inc, Member: row[0], OriginalThreadID: row[1]}, OldThreadID: row[1], NewThreadID: row[2], OldProvider: "openai", NewProvider: "openai"})
	}
	sealed, _ := json.Marshal(evidence)
	sum := sha256.Sum256(sealed)
	state := codexproviders.State{Schema: 3, Workspace: name, Root: workspace, Socket: socket, Active: "openai", BindingRevision: 2, Bindings: map[string]codexproviders.SessionBinding{slug: {RootThreadID: originalRoot, Incarnation: inc, Revision: 2, Conversations: map[string]codexproviders.ConversationBinding{originalRoot: {Member: "lead", ThreadID: nativeRoot, Provider: "openai", OperationID: op, EvidenceDigest: hex.EncodeToString(sum[:])}, originalMember: {Member: "implementer0", ThreadID: nativeMember, Provider: "openai", OperationID: op, EvidenceDigest: hex.EncodeToString(sum[:])}}}}}
	if mode == "delete-null" || mode == "instructions" {
		delete(state.Bindings[slug].Conversations, originalMember)
	}
	statePath := filepath.Join(stateRoot, "providers", name, "state.json")
	bindingBytes, _ := json.Marshal(state)
	write(filepath.Join(filepath.Dir(statePath), op+".snapshot.json.evidence"), sealed, 0600)
	catalog := filepath.Join(stateRoot, "catalog.json")
	write(catalog, []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"}]}`), 0600)
	registry := filepath.Join(stateRoot, "registry.json")
	encoded, _ = json.Marshal(map[string]any{"schema": 1, "workspaces": []any{map[string]any{"name": name, "root": workspace, "hostname": "test.example", "aliases": []string{}}}})
	write(registry, encoded, 0600)
	store, err := teamruntime.NewStore(stateRoot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), slug, originalRoot, true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Index: 0, Thread: originalMember, Model: "fixture-model", Effort: "high", Behavior: "implementer", Purpose: "implementation", Instructions: "Implement the fixture", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()}}
		if mode == "delete-null" {
			roster.Members[0].Thread = ""
			roster.Members[0].State = "creating"
			roster.Members[0].ProjectID = "55555555-5555-4555-8555-555555555555"
			roster.Members[0].CreateAttempted = true
		}
		if mode == "instructions" {
			roster.Members = []teamruntime.Member{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(stateRoot, "driver.rb")
	write(driver, []byte(lifecycleCompositionRubyDriver), 0600)
	devSession := filepath.Join(pkg, "bin/dev-session")
	write(devSession, []byte("#!/bin/sh\nexec "+ruby+" "+driver+" "+pkg+" "+workspace+" "+stateRoot+" "+socket+" "+mode+" \"$@\"\n"), 0700)
	tmux := filepath.Join(stateRoot, "tmux")
	write(tmux, []byte("#!/bin/sh\nexit 0\n"), 0700)
	if mode == "instructions" {
		fields := []string{"$1", slug, "1", slug, workspace, slug, filepath.Join(filepath.Dir(socket), "tmux.sock"), nativeRoot, socket, "0.172.0", "%1", inc}
		encoded, _ := json.Marshal(map[string]any{"schema": 3, "state": "ready", "slug": slug, "workspace": workspace, "tmux_socket": fields[6], "tmux_session_id": "$1", "tmux_identity": inc, "codex_thread_id": originalRoot, "codex_active_thread_id": nativeRoot, "codex_binding_revision": 2, "codex_socket_path": socket, "codex_client_version": "0.172.0"})
		write(filepath.Join(authorityDir, slug+".json"), encoded, 0600)
		write(tmux, []byte("#!/bin/sh\ncat <<'FIXTURE'\n"+strings.Join(fields, "\t")+"\nFIXTURE\n"), 0700)
		write(statePath, bindingBytes, 0600)
		reviveStarted.Store(true)
	}
	for key, value := range map[string]string{"R2C_ROOT_FORK_PROCESS": "1", "DEV_WORKSPACES_CODEX_PROVIDERS": catalog, "DEV_WORKSPACES_CONFIG": registry, "DEV_WORKSPACES_RUNTIME_DIR": filepath.Dir(filepath.Dir(socket)), "DEV_WORKSPACES_STATE": stateRoot, "DEV_WORKSPACES_PROFILE": filepath.Join(stateRoot, "profile"), "DEV_WORKSPACE_NAME": name, "DEV_WORKSPACE_CODEX_HOME": home, "DEV_WORKSPACE_DEFER_TRACKING": "1"} {
		t.Setenv(key, value)
	}
	shadow := filepath.Join(stateRoot, "supervisor-double")
	for _, name := range []string{"systemd-run", "systemctl", "journalctl"} {
		write(filepath.Join(shadow, name), []byte("#!"+ruby+"\n"+lifecycleSupervisorDouble), 0700)
	}
	t.Setenv("R2C_SUPERVISOR_ROOT", stateRoot)
	t.Setenv("R2C_SUPERVISOR_PACKAGE", pkg)
	t.Setenv("PATH", shadow+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"systemd-run", "systemctl", "journalctl"} {
		resolved, err := exec.LookPath(name)
		if err != nil || resolved != filepath.Join(shadow, name) {
			t.Fatalf("supervisor escaped fixture: %s %s %v", name, resolved, err)
		}
		output, err := exec.Command(name, "--r2c-probe").CombinedOutput()
		if err != nil || string(output) != "r2c-supervisor-double\n" {
			t.Fatalf("supervisor double probe: %s %v %s", name, err, output)
		}
	}
	if mode != "receipt" && mode != "receipt-delete" && mode != "instructions" {
		write(statePath, bindingBytes, 0600)
		invoke := func() ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			reviveStarted.Store(true)
			if strings.HasPrefix(mode, "delete") {
				return exec.CommandContext(ctx, devSession, "delete", slug, "--as-is", "--force").CombinedOutput()
			}
			return exec.CommandContext(ctx, devSession, "archive", slug, "--as-is").CombinedOutput()
		}
		output, err := invoke()
		if mode == "revive" {
			if err != nil {
				t.Fatalf("revive archive setup: %v\n%s", err, output)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err = exec.CommandContext(ctx, devSession, "revive", slug, "--as-is").CombinedOutput()
			if err != nil {
				t.Fatalf("retained start/revive: %v\n%s", err, output)
			}
			nativeMu.Lock()
			if !reflect.DeepEqual(calls, []string{nativeMember, nativeRoot}) || !reflect.DeepEqual(restoreCalls, []string{"thread/unarchive:" + nativeRoot, "thread/resume:" + nativeRoot, "thread/unarchive:" + nativeMember}) {
				t.Fatal("revive target/order changed", calls, restoreCalls)
			}
			nativeMu.Unlock()
			summary, findErr := session.Find(workspace, slug)
			if findErr != nil || summary.Archived || summary.Lifecycle != "active" || summary.Codex.BindingSchema != 3 || summary.Codex.ThreadID != originalRoot {
				t.Fatal("revive rewrote original tracking", summary, findErr)
			}
			roster, loadErr := store.Load(slug, originalRoot)
			if loadErr != nil || roster.Members[0].Thread != originalMember || roster.Members[0].State != "ready" {
				t.Fatal("revive rewrote original member", roster, loadErr)
			}
			authority, loadErr := session.LoadRuntimeAuthority(authorityDir, slug, workspace)
			if loadErr != nil || authority.Schema != 3 || authority.CodexThreadID != originalRoot || authority.CodexActiveThreadID != nativeRoot || authority.CodexBindingRevision != 2 || authority.TmuxIdentity != inc {
				t.Fatal("revive authority lost original/native split", authority, loadErr)
			}
			progress, readErr := session.PendingLifecycleProgress(workspace, slug)
			if readErr != nil || progress != nil {
				t.Fatal("revive journal not retired", progress, readErr)
			}
			if after, readErr := os.ReadFile(statePath); readErr != nil || !bytes.Equal(after, bindingBytes) {
				t.Fatal("revive changed provider state", readErr)
			}
			for _, owner := range []string{"start", "revive"} {
				if _, readErr := os.Lstat(filepath.Join(workspace, "worktrees", ".locks", slug+"."+owner+".json")); !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal("revive retained unfinished owner", owner, readErr)
				}
			}
			return
		}
		if strings.HasPrefix(mode, "delete") {
			var savedTargets *session.CodexTargets
			savedOperation := ""
			if mode == "delete-metadata" {
				if err == nil || !bytes.Contains(output, []byte("fixture metadata-only crash")) {
					t.Fatalf("metadata interruption missing: %v\n%s", err, output)
				}
				root := filepath.Join(stateRoot, "removed", filepath.Base(workspace)+"-"+fmt.Sprintf("%x", sha256.Sum256([]byte(workspace)))[:16])
				entries, readErr := os.ReadDir(root)
				if readErr != nil || len(entries) != 1 {
					t.Fatal("first durable metadata was discarded", entries, readErr)
				}
				path := filepath.Join(root, entries[0].Name(), "recovery.json")
				saved, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var metadata struct {
					Targets     *session.CodexTargets `json:"codex_targets"`
					OperationID string                `json:"operation_id"`
				}
				if err := json.Unmarshal(saved, &metadata); err != nil || metadata.Targets == nil {
					t.Fatal("metadata capture missing", metadata, err)
				}
				savedTargets, savedOperation = metadata.Targets.Clone(), metadata.OperationID
				if _, err := os.Lstat(filepath.Join(workspace, "worktrees", ".locks", slug+".removal.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("metadata crash wrote journal", err)
				}
				changed := state
				changed.BindingRevision = 3
				changed.Bindings = map[string]codexproviders.SessionBinding{}
				for key, value := range state.Bindings {
					value.Revision = 3
					changed.Bindings[key] = value
				}
				body, _ := json.Marshal(changed)
				write(statePath, body, 0600)
				output, err = invoke()
				if err == nil || !bytes.Contains(output, []byte("saved lifecycle root selection changed")) {
					t.Fatalf("metadata drift not refused precisely: %v\n%s", err, output)
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(saved, after) {
					t.Fatal("metadata owner changed on refused drift")
				}
				nativeMu.Lock()
				if len(calls) != 0 {
					t.Fatal("metadata interruption dispatched native", calls)
				}
				nativeMu.Unlock()
				write(statePath, bindingBytes, 0600)
				output, err = invoke()
			} else if mode == "delete-lost" {
				if err == nil || !rootReplyDropped.Load() {
					t.Fatalf("root archive lost reply did not leave failure: %v\n%s", err, output)
				}
				progress, readErr := session.PendingLifecycleProgress(workspace, slug)
				if readErr != nil || progress == nil || progress.Phase != "thread_retiring" || progress.CodexTargets == nil {
					t.Fatal("lost reply lost original owner", progress, readErr)
				}
				savedTargets, savedOperation = progress.CodexTargets.Clone(), progress.JournalID
				output, err = invoke()
			}
			if err != nil {
				t.Fatalf("owning delete CLI: %v\n%s", err, output)
			}
			nativeMu.Lock()
			wantCalls := []string{nativeMember, nativeRoot}
			if mode == "delete-null" {
				wantCalls = []string{nativeRoot}
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatal("delete changed native target/order", calls)
			}
			nativeMu.Unlock()
			progress, err := session.PendingLifecycleProgress(workspace, slug)
			if err != nil || progress != nil {
				t.Fatal("delete journal not retired", progress, err)
			}
			if _, err := session.Find(workspace, slug); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("deleted live tracking remains", err)
			}
			recoveryRoot := filepath.Join(stateRoot, "removed", filepath.Base(workspace)+"-"+fmt.Sprintf("%x", sha256.Sum256([]byte(workspace)))[:16])
			candidates, err := os.ReadDir(recoveryRoot)
			if err != nil || len(candidates) != 1 {
				t.Fatal("missing retained deletion owner", candidates, err)
			}
			retained, err := os.ReadFile(filepath.Join(recoveryRoot, candidates[0].Name(), "recovery.json"))
			if err != nil {
				t.Fatal(err)
			}
			var owner struct {
				OperationID  string                `json:"operation_id"`
				Targets      *session.CodexTargets `json:"codex_targets"`
				State, Phase string
			}
			if err := json.Unmarshal(retained, &owner); err != nil || owner.State != "removed" || owner.Phase != "removed" || owner.Targets == nil {
				t.Fatal("wrong retained deletion owner", owner, err)
			}
			if savedTargets != nil && (!session.SameCodexTargets(savedTargets, owner.Targets) || savedOperation != owner.OperationID) {
				t.Fatal("deletion retry retargeted accepted owner")
			}
			if mode == "delete-null" {
				if owner.Targets.ThreadID != nativeRoot || len(owner.Targets.Members) != 1 || owner.Targets.Members[0].Member != "implementer0" || owner.Targets.Members[0].OriginalThreadID != nil || owner.Targets.Members[0].ThreadID != nil {
					t.Fatal("unstarted deletion lost explicit null tuple", owner.Targets)
				}
				data, readErr := os.ReadFile(filepath.Join(recoveryRoot, candidates[0].Name(), "team.json"))
				var roster teamruntime.Roster
				if readErr != nil || json.Unmarshal(data, &roster) != nil || roster.RootThreadID != originalRoot || len(roster.Members) != 1 || roster.Members[0].State != "removed" || roster.Members[0].Thread != "" || !roster.Members[0].CreateAttempted || roster.Members[0].BootstrapAttempted || roster.Members[0].ProjectID != "55555555-5555-4555-8555-555555555555" {
					t.Fatal("unstarted deletion dispatched/adopted a member", roster, readErr)
				}
				if _, readErr := store.Load(slug, originalRoot); !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal("unstarted deletion retained live roster", readErr)
				}
			}
			completed, err := session.CompletedRemovalForTarget(workspace, slug, stateRoot, owner.OperationID, time.Time{}, originalRoot, owner.Targets.Clone())
			if err != nil || !completed {
				t.Fatal("exact retained deletion proof failed", completed, err)
			}
			if _, err := os.Lstat(filepath.Join(stateRoot, "supervisor.trace")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("direct delete invoked supervisor", err)
			}
			held, err := os.ReadFile(filepath.Join(stateRoot, "parent-checks"))
			if err != nil || strings.Count(string(held), "held") < 4 {
				t.Fatal("delete parent exclusion missing", string(held), err)
			}
			if after, err := os.ReadFile(statePath); err != nil || !bytes.Equal(after, bindingBytes) {
				t.Fatal("delete changed provider state", err)
			}
			return
		}
		if mode == "fresh" && err != nil {
			t.Fatalf("fresh CLI archive: %v\n%s", err, output)
		}
		if mode == "prelude" || mode == "sidecar" {
			if err == nil || !bytes.Contains(output, []byte("fixture "+mode+"-only crash")) {
				t.Fatalf("interruption missing: %v\n%s", err, output)
			}
			path := filepath.Join(workspace, "worktrees", ".locks", slug+".archive-cleanup.json")
			if mode == "prelude" {
				path = filepath.Join(workspace, "worktrees", ".locks", slug+".services-cleanup.json")
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(workspace, "worktrees", ".locks", slug+".archive.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("interruption created outer journal", err)
			}
			var owner struct {
				OperationID string                `json:"operation_id"`
				Targets     *session.CodexTargets `json:"codex_targets"`
				Root        string                `json:"retained_thread_id"`
			}
			if err := json.Unmarshal(saved, &owner); err != nil || owner.OperationID == "" || owner.Root != originalRoot || owner.Targets == nil {
				t.Fatal("missing exact earliest owner", owner, err)
			}
			nativeMu.Lock()
			if !reflect.DeepEqual(calls, []string{nativeMember}) {
				t.Fatal("wrong pre-owner partial effects", calls)
			}
			nativeMu.Unlock()
			changed := state
			changed.BindingRevision = 3
			changed.Bindings = map[string]codexproviders.SessionBinding{}
			for key, value := range state.Bindings {
				value.Revision = 3
				changed.Bindings[key] = value
			}
			changedBytes, _ := json.Marshal(changed)
			write(statePath, changedBytes, 0600)
			output, err = invoke()
			if err == nil || !bytes.Contains(output, []byte("saved lifecycle root selection changed")) {
				t.Fatalf("saved owner drift did not refuse precisely: %v\n%s", err, output)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(saved, after) {
				t.Fatal("drift rewrote accepted earliest owner")
			}
			nativeMu.Lock()
			if !reflect.DeepEqual(calls, []string{nativeMember}) {
				t.Fatal("drift replayed native effects", calls)
			}
			nativeMu.Unlock()
			write(statePath, bindingBytes, 0600)
			output, err = invoke()
			if err != nil {
				t.Fatalf("exact earliest-owner recovery: %v\n%s", err, output)
			}
			t.Log(mode + " owner survived crash/drift and resumed saved targets without recapture")
		}
		if mode == "partial" {
			if err == nil || !bytes.Contains(output, []byte("fixture pre-inventory failure")) {
				t.Fatalf("partial failure missing: %v\n%s", err, output)
			}
			progress, err := session.PendingLifecycleProgress(workspace, slug)
			if err != nil || progress != nil {
				t.Fatal("partial pre-acceptance failure fabricated progress", progress, err)
			}
			for _, path := range []string{filepath.Join(workspace, "worktrees", ".locks", slug+".archive-cleanup.json"), filepath.Join(stateRoot, "portal", filepath.Base(workspace)+"-"+fmt.Sprintf("%x", sha256.Sum256([]byte(workspace)))[:16], "lifecycle-operations.json")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("partial failure fabricated durable owner", path, err)
				}
			}
			nativeMu.Lock()
			if !reflect.DeepEqual(calls, []string{nativeMember}) || archived[nativeRoot] {
				t.Fatal("partial effects differ", calls)
			}
			nativeMu.Unlock()
			drift.Store(true)
			output, err = invoke()
			if err == nil || !bytes.Contains(output, []byte("thread/read returned the wrong retained identity")) {
				t.Fatalf("later native-root drift did not refuse for retained identity: %v\n%s", err, output)
			}
			t.Log("later explicit invocation refused: thread/read returned the wrong retained identity; no additional archive")
			nativeMu.Lock()
			if !reflect.DeepEqual(calls, []string{nativeMember}) {
				t.Fatal("later invocation replayed effects", calls)
			}
			nativeMu.Unlock()
		}
		roster, err := store.Load(slug, originalRoot)
		if err != nil || roster.Members[0].Thread != originalMember || roster.Members[0].State != "archived" {
			t.Fatal("original partial/ready roster changed", roster, err)
		}
		captures, err := os.ReadFile(filepath.Join(stateRoot, "memory-captures"))
		if err != nil {
			t.Fatal(err)
		}
		count := 1
		if mode == "partial" {
			count = 2
		}
		rows := strings.Split(strings.TrimSpace(string(captures)), "\n")
		if len(rows) != count {
			t.Fatal("fresh invocation did not capture once each", string(captures))
		}
		for _, row := range rows {
			var targets session.CodexTargets
			if err := json.Unmarshal([]byte(row), &targets); err != nil || targets.ThreadID != nativeRoot || len(targets.Members) != 1 || *targets.Members[0].ThreadID != nativeMember {
				t.Fatal("fresh tuple changed", row, err)
			}
		}
		events, err := os.ReadFile(filepath.Join(stateRoot, "inventory-order"))
		if err != nil || !strings.HasPrefix(string(events), "quiesced\ninventory\n") {
			t.Fatal("quiesce did not precede inventory", string(events), err)
		}
		held, err := os.ReadFile(filepath.Join(stateRoot, "parent-checks"))
		if err != nil || strings.Count(string(held), "held") < 2 {
			t.Fatal("parent OFD proof missing", string(held), err)
		}
		if data, err := os.ReadFile(statePath); err != nil || !bytes.Equal(data, bindingBytes) {
			t.Fatal("CLI lifecycle changed binding state", err)
		}
		if mode == "fresh" || mode == "prelude" || mode == "sidecar" {
			nativeMu.Lock()
			defer nativeMu.Unlock()
			if !reflect.DeepEqual(calls, []string{nativeMember, nativeRoot}) {
				t.Fatal("fresh targets/order", calls)
			}
		}
		return
	}
	clientName := "lifecycle-browser-double"
	if mode == "instructions" {
		clientName = "startup-instruction-fixture"
	}
	client := workspacecodex.NewWithOptions(socket, workspace, codex.ClientOptions{ClientInfo: codex.ClientInfo{Name: clientName}})
	client.RecoveryRoot = stateRoot
	client.CodexHome = home
	client.PackageRoot = pkg // The parent test binary stands in for this package.
	if mode == "instructions" {
		client.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) {
			return workspacecodex.LeadThreadPolicy(slug, workspace, ""), nil
		}
	}
	defer client.Close()
	browserClient := &lifecycleCompositionBrowserClient{Client: client, cwd: cwd}
	configuration := portalweb.Config{Workspace: workspace, UserStateRoot: stateRoot, BaseURL: "https://workspace.example.test", DevSession: devSession, HostProfile: filepath.Join(stateRoot, "profile"), AuthorityDir: authorityDir, CodexSocket: socket, CodexVersion: "0.172.0", PackageRoot: pkg, WorkspaceName: name, RegistrationMarker: filepath.Join(filepath.Dir(socket), "registration.json"), Tmux: tmux, Codex: compositionWithoutInstructions{browserClient}, VerifyThread: func(context.Context, string, string) error { return nil }}
	if mode == "instructions" {
		configuration.Codex = browserClient
	}
	application, err := portalweb.New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if mode == "instructions" {
		deadline := time.Now().Add(5 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		for time.Now().Before(deadline) {
			scope, readErr := client.RetryScope(ctx, nativeRoot)
			if readErr == nil {
				state, readErr := client.RetryStore().Load(scope)
				if readErr == nil && state.Binding != nil && state.Binding.Provider == "openai" && reflect.DeepEqual(state.Binding.Policy, workspacecodex.LeadThreadPolicy(slug, workspace, "")) && scope.Root == originalRoot && scope.Thread == nativeRoot {
					nativeMu.Lock()
					got := append([]string(nil), instructionCalls...)
					nativeMu.Unlock()
					if !reflect.DeepEqual(got, []string{"thread/resume:" + nativeRoot}) {
						t.Fatal("startup instruction order/target changed", got)
					}
					if after, readErr := os.ReadFile(statePath); readErr != nil || !bytes.Equal(after, bindingBytes) {
						t.Fatal("startup instruction changed provider state", readErr)
					}
					runtime, lockErr := session.LockRuntimeExclusiveContext(ctx, authorityDir, slug)
					if lockErr != nil {
						t.Fatal(lockErr)
					}
					defer runtime.Close()
					release, lockErr := store.LockOperation(ctx, slug)
					if lockErr != nil {
						t.Fatal(lockErr)
					}
					defer release()
					if actual, resumeErr := client.Client.ResumeThreadWithSettings(ctx, nativeRoot, cwd, nil, codex.ThreadSettings{Model: "fixture-model", ReasoningEffort: "high"}); resumeErr != nil || actual != nativeRoot {
						t.Fatal("ordinary exact-current resume failed", actual, resumeErr)
					}
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("startup instruction reconciliation did not publish its managed binding")
	}
	// Instruction reconciliation has its separate owning startup control.
	write(statePath, bindingBytes, 0600)
	var tracking unix.Stat_t
	if err := unix.Lstat(cwd, &tracking); err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("2\x00%s\x00%s\x00work\x00%d\x00%d\x00present\x00%s", workspace, slug, tracking.Dev, tracking.Ino, originalRoot))))
	action, body := "archive", `{"mode":"complete","targetId":"`+target+`"}`
	if mode == "receipt-delete" {
		action, body = "delete", `{"force":true,"targetId":"`+target+`"}`
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+slug+"/"+action, strings.NewReader(body))
	request.Header.Set("Origin", "https://workspace.example.test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("acceptance %d %s", response.Code, response.Body.String())
	}
	var accepted struct {
		ReceiptID string `json:"receiptId"`
		Attempt   int    `json:"attempt"`
		Options   struct {
			Targets         *session.CodexTargets `json:"codex_targets"`
			DeletedThreadID string                `json:"deletedThreadId"`
			JournalID       string                `json:"journalId"`
			Force           bool                  `json:"force"`
		} `json:"options"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || accepted.ReceiptID == "" || accepted.Options.Targets == nil {
		t.Fatalf("accepted receipt %s %v", response.Body.String(), err)
	}
	var terminal struct {
		State, Error, ReceiptID string
		Attempt                 int
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status := httptest.NewRecorder()
		application.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/sessions/"+slug+"/operation", nil))
		if status.Code != http.StatusOK {
			t.Fatal(status.Code, status.Body.String())
		}
		if err := json.Unmarshal(status.Body.Bytes(), &terminal); err != nil {
			t.Fatal(err)
		}
		if terminal.State == "complete" || terminal.State == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if accepted.Options.Targets.ThreadID != nativeRoot || len(accepted.Options.Targets.Members) != 1 || *accepted.Options.Targets.Members[0].OriginalThreadID != originalMember || *accepted.Options.Targets.Members[0].ThreadID != nativeMember {
		t.Fatal("HTTP receipt changed original/native selection", accepted.Options.Targets)
	}
	if terminal.State != "complete" || terminal.ReceiptID != accepted.ReceiptID || terminal.Attempt != accepted.Attempt {
		body, _ := os.ReadFile(filepath.Join(stateRoot, "supervisor-worker.log"))
		t.Fatalf("terminal receipt %#v\n%s", terminal, body)
	}
	receiptPath := filepath.Join(stateRoot, "portal", filepath.Base(workspace)+"-"+fmt.Sprintf("%x", sha256.Sum256([]byte(workspace)))[:16], "lifecycle-operations.json")
	receiptData, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receiptStore struct {
		Operations []struct {
			ReceiptID string
			State     string
			Attempt   int
			Options   struct {
				Targets         *session.CodexTargets `json:"codex_targets"`
				DeletedThreadID string                `json:"deletedThreadId"`
				JournalID       string                `json:"journalId"`
				Force           bool                  `json:"force"`
			}
		}
	}
	if err := json.Unmarshal(receiptData, &receiptStore); err != nil || len(receiptStore.Operations) != 1 || receiptStore.Operations[0].ReceiptID != accepted.ReceiptID || !session.SameCodexTargets(receiptStore.Operations[0].Options.Targets, accepted.Options.Targets) {
		t.Fatal("durable terminal receipt changed accepted capture", err, string(receiptData))
	}
	ownerData, err := os.ReadFile(filepath.Join(stateRoot, "owner-captures"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(ownerData)), "\n")
	wantRows := 2
	if mode == "receipt-delete" {
		wantRows = 4
	}
	if len(rows) != wantRows {
		t.Fatal("missing journal/sidecar native-call proof", string(ownerData))
	}
	for _, row := range rows {
		var evidence struct {
			Root    string
			Targets *session.CodexTargets
		}
		if err := json.Unmarshal([]byte(row), &evidence); err != nil || evidence.Root != originalRoot || !session.SameCodexTargets(evidence.Targets, accepted.Options.Targets) {
			t.Fatal("owner capture differs from accepted receipt", row, err)
		}
	}
	if mode == "receipt-delete" {
		durable := receiptStore.Operations[0]
		if durable.State != "complete" || durable.Attempt != accepted.Attempt || !durable.Options.Force || durable.Options.DeletedThreadID != originalRoot || durable.Options.DeletedThreadID != accepted.Options.DeletedThreadID || durable.Options.JournalID != accepted.Options.JournalID {
			t.Fatal("terminal deletion receipt lost immutable owner", durable, accepted)
		}
		completed, readErr := session.CompletedRemovalForTarget(workspace, slug, stateRoot, accepted.Options.JournalID, time.Time{}, originalRoot, accepted.Options.Targets.Clone())
		if readErr != nil || !completed {
			t.Fatal("browser deletion lost exact retained completion", completed, readErr)
		}
		if _, readErr := session.Find(workspace, slug); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal("browser deletion left live tracking", readErr)
		}
		if _, readErr := os.Lstat(filepath.Join(stateRoot, "supervisor.trace")); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal("browser deletion used archive supervisor", readErr)
		}
	} else {
		trace, readErr := os.ReadFile(filepath.Join(stateRoot, "supervisor.trace"))
		if readErr != nil || !bytes.Contains(trace, []byte("systemd-run")) || !bytes.Contains(trace, []byte("systemctl")) {
			t.Fatal("supervisor double evidence missing", string(trace), readErr)
		}
	}
	nativeMu.Lock()
	defer nativeMu.Unlock()
	if !reflect.DeepEqual(calls, []string{nativeMember, nativeRoot}) {
		t.Fatalf("native targets/order %v", calls)
	}
	roster, err := store.Load(slug, originalRoot)
	if mode == "receipt-delete" {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal("completed browser deletion retained live roster", roster, err)
		}
	} else if err != nil || roster.Members[0].Thread != originalMember || roster.Members[0].State != "archived" {
		t.Fatal("original roster changed", roster, err)
	}
	if data, err := os.ReadFile(statePath); err != nil || !bytes.Equal(data, bindingBytes) {
		t.Fatal("lifecycle changed provider state", err)
	}
	if data, err := os.ReadFile(filepath.Join(stateRoot, "parent-checks")); err != nil || strings.Count(string(data), "held") < 2 {
		t.Fatal("parent OFD evidence missing", string(data), err)
	}
	journalName := "archive"
	if mode == "receipt-delete" {
		journalName = "removal"
	}
	if _, err := os.Stat(filepath.Join(workspace, "worktrees", ".locks", slug+"."+journalName+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed journal retained", err)
	}
}

type lifecycleCompositionBrowserClient struct {
	*workspacecodex.Client
	cwd string
}

func (client *lifecycleCompositionBrowserClient) ReadThread(context.Context, string) (codex.Transcript, error) {
	return codex.Transcript{Status: "idle"}, nil
}
func (client *lifecycleCompositionBrowserClient) ObserveThread(_ context.Context, id, cwd string) (workspacecodex.ThreadObservation, error) {
	return workspacecodex.ThreadObservation{Schema: 1, ThreadID: id, Cwd: cwd, ActivityKnown: true, Idle: true, ActivityToken: "fixture"}, nil
}

func (client *lifecycleCompositionBrowserClient) ReadThreadMetadata(_ context.Context, id string, _ bool) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{ID: id, Cwd: client.cwd, Status: map[string]any{"type": "idle"}}, nil
}
func (client *lifecycleCompositionBrowserClient) RequireThreadTurnsIdle(context.Context, string) error {
	return nil
}
func (client *lifecycleCompositionBrowserClient) HasActiveGoal(context.Context, string) (bool, error) {
	return false, nil
}
func (client *lifecycleCompositionBrowserClient) ListQueue(context.Context, string) ([]codex.QueueEntry, error) {
	return nil, nil
}

const lifecycleCompositionRubyDriver = `require 'stringio'
pkg,workspace,state,socket,mode=ARGV.shift(5)
forwarded=ARGV.dup
load File.join(pkg,'libexec/workspace-portal/dev-session')
class LifecycleCompositionControl < DevSession::Runner
 private
 def ensure_archive_conversations_ready!(*,**) = nil
 def quiesce_and_require_idle!(*,**) = nil
 def retire_session_runtime!(*,**) = nil
 def require_archive_check!(*) = nil
 def verify_codex_client! = nil
 def create_tmux_session(slug,run_codex:,thread_id:,identity_token:,**)
  raise 'terminal fixture outside revive' unless @composition_mode=='revive' && run_codex
  @fixture_session=DevSession::Tmux::Session.new(id:'$1',name:slug,mark:'1',slug:,workspace:workspace,
   environment_slug:slug,socket_path:@configured_tmux_socket,codex_thread_id:thread_id,codex_socket_path:@codex_socket,
   codex_client_version:@codex_version,codex_pane_id:'%1',identity_token:)
 end
 def sync_slug(slug,require_session:,session:nil)
  return super unless @composition_mode=='revive'
  raise 'terminal fixture identity changed' unless require_session && session.equal?(@fixture_session)
  session
 end
 def lifecycle_codex_targets!(slug,operation,records)
  fresh=!@lifecycle_access.key?(:codex_targets) && saved_lifecycle_codex_targets!(operation,records).first.empty?
  result=super
  if fresh && result
   raise 'fresh capture repeated under another runtime OFD' if @control_capture_file
   @control_capture_file=@held_slug_lock.fetch(:file)
   File.open(File.join(@workspace_state_root,'memory-captures'),'a'){|f|f.puts JSON.generate(result)}
  elsif @control_capture_file && !@control_capture_file.equal?(@held_slug_lock.fetch(:file))
   raise 'runtime OFD changed after fresh capture'
  end
  result
 end
 def write_removal_journal!(slug,removal)
  marker=File.join(@workspace_state_root,'interruption-fired')
  if @composition_mode=='delete-metadata' && !File.file?(marker)
   File.write(marker,'metadata')
   raise DevSession::Error,'fixture metadata-only crash'
  end
  super
 end
 def write_lifecycle_journal(path,journal,command:,create:)
  marker=File.join(@workspace_state_root,'interruption-fired')
  if @composition_mode=='sidecar' && command=='archive' && create && !File.file?(marker)
   File.write(marker,'sidecar')
   raise DevSession::Error,'fixture sidecar-only crash'
  end
  super
 end
 def quiesce_explicit_archive!(slug)
  super
  File.open(File.join(@workspace_state_root,'inventory-order'),'a'){|f|f.puts 'quiesced'}
 end
end
class LifecycleCompositionCLI < DevSession::CLI
 private
 def validate_portal_authorization!(**) = nil
 def authorize_yes_no!(slug,**) = slug
end
tmux=Object.new
tmux.define_singleton_method(:session){|*|nil}
tmux.define_singleton_method(:sessions){[]}
tmux.define_singleton_method(:managed_session_identities){[]}
tmux.define_singleton_method(:argv){|*args|['tmux',*args]}
actual=DevSession::CommandRunner.new(out:StringIO.new,err:StringIO.new)
runner=LifecycleCompositionControl.new(workspace:,tmux:,out:$stdout,err:$stderr,env:ENV,require_runtime:true,
 codex_command:'/test/no-native-executable',authority_dir:File.join(File.dirname(socket),'authority'),
 codex_socket:socket,codex_version:'0.172.0',portal_command:[File.join(pkg,'bin/workspace-portal'),'-test.run=^TestBoundRootForkCommandProcess$','--'],
 tmux_socket:File.join(File.dirname(socket),'tmux.sock'),command_runner:actual)
runner.instance_variable_set(:@composition_mode,mode)
module LifecyclePreludeInterruption
 def prepare_cleanup!(**options)
  result=super
  marker=File.join(@state_root,'interruption-fired')
  if @runner.instance_variable_get(:@composition_mode)=='prelude' && !File.file?(marker)
   File.write(marker,'prelude')
   raise DevSession::Error,'fixture prelude-only crash'
  end
  result
 end
end
WorkspaceSessionServices::Store.prepend(LifecyclePreludeInterruption)
module LifecycleInventoryOrder
 def discover(*args,**options)
  mode=@runner.instance_variable_get(:@composition_mode)
 if %w[fresh partial prelude sidecar revive].include?(mode)
   root=@runner.instance_variable_get(:@workspace_state_root)
   path=File.join(root,'inventory-order')
   raise 'inventory ran before quiesce' unless File.file?(path) && File.read(path).start_with?("quiesced\n")
   File.open(path,'a'){|f|f.puts 'inventory'}
   raise DevSession::Error,'fixture pre-inventory failure' if mode=='partial'
  end
  super
 end
end
DevSession::ArchiveCleanup.prepend(LifecycleInventoryOrder)
capture=actual.method(:capture)
actual.define_singleton_method(:capture) do |argv,**options|
 if argv.include?('thread') && (argv.include?('retire') || argv.include?('quiesce-explicit') || argv.include?('create') && argv.include?('--recover-archived')) || argv.include?('team') && (argv.include?('archive') || argv.include?('archive-explicit') || argv.include?('retire') || argv.include?('prepare-delete') || argv.include?('revive'))
  slug='2026-11-25-lifecycle'
  expected=JSON.parse(options.fetch(:input))
  operation=runner.instance_variable_get(:@lifecycle_access).fetch(:operation)
  journal=case operation;when 'delete';runner.send(:load_removal_journal,slug);when 'revive';runner.send(:load_revive_journal,slug);else;runner.send(:load_archive_journal,slug);end
  sidecar=operation=='archive' ? DevSession::ArchiveCleanup.new(runner,slug).load(journal:) : nil
  metadata=operation=='delete' && journal ? JSON.parse(File.binread(File.join(journal.fetch('recovery'),'recovery.json'))) : nil
  prelude=runner.send(:session_services,slug).prelude
  owners=[journal,sidecar,metadata,prelude].compact
  if !owners.empty?
   raise 'sidecar command preceded journal restore' if sidecar && !journal
   raise 'saved lifecycle owner changed' unless owners.all?{|record|record['operation_id']==expected['operation_id'] && record.key?('codex_targets')==expected.key?('codex_targets') && record['codex_targets']==expected['codex_targets']}
  else
   request=runner.instance_variable_get(:@lifecycle_access)
   raise 'memory capture changed' unless %w[fresh partial prelude sidecar revive].include?(mode) && request[:operation_id]==expected['operation_id'] && request[:codex_targets]==expected['codex_targets']
  end
  original=operation=='archive' && journal ? journal.fetch('retained_thread_id') : runner.send(:lifecycle_origin_manifest,slug,owners).dig('codex','thread_id')
  File.open(File.join(state,'owner-captures'),'a'){|f|f.puts JSON.generate({'root'=>original,'targets'=>expected.fetch('codex_targets'),'memoryOnly'=>journal.nil?})}
  begin
   capture.call(argv,**options)
  ensure
   lock=runner.instance_variable_get(:@held_slug_lock)
   File.open(lock.fetch(:path),File::RDWR){|probe|raise 'child released parent OFD' if probe.flock(File::LOCK_SH|File::LOCK_NB)}
   File.open(File.join(state,'parent-checks'),'a'){|f|f.puts 'held'}
  end
 else
  capture.call(argv,**options)
 end
end
cli=LifecycleCompositionCLI.new(['--workspace',workspace,'--host-profile',File.join(state,'profile'),
 '--expected-host-generation',pkg,'--expected-host-profile-token',DevWorkspaceProfileIdentity.token(File.join(state,'profile')),
 *forwarded],out:$stdout,err:$stderr,env:ENV)
cli.define_singleton_method(:runner){runner}
exit cli.run
`

const lifecycleSupervisorDouble = `require 'json'
root=ENV.fetch('R2C_SUPERVISOR_ROOT')
raise 'supervisor fixture root changed' unless root.start_with?('/') && File.directory?(root)
if ARGV==['--r2c-probe'];puts 'r2c-supervisor-double';exit 0;end
name=File.basename($0)
File.open(File.join(root,'supervisor.trace'),'a'){|f|f.puts JSON.generate([name,ARGV])}
case name
when 'systemd-run'
 unit=ARGV.find{|arg|arg.start_with?('--unit=')}
 raise 'invalid fixture unit' unless unit && unit.match?(/\A--unit=workspace-archive-[0-9a-f]{64}-1\.service\z/)
 separator=ARGV.index('--');raise 'missing fixture command' unless separator
 command=ARGV[(separator+1)..]
 raise 'supervisor target escaped fixture' unless command[0,3]==[File.join(ENV.fetch('R2C_SUPERVISOR_PACKAGE'),'bin/dev-session'),'archive','2026-11-25-lifecycle']
 File.write(File.join(root,'supervisor.status'),"LoadState=loaded\nActiveState=active\nSubState=running\n")
 File.open(File.join(root,'supervisor-worker.log'),'w'){|log|system(*command,out:log,err:log)}
 status=$?.exitstatus || 1
 File.write(File.join(root,'supervisor.status'),"LoadState=loaded\nActiveState=#{status.zero? ? 'active' : 'failed'}\nSubState=#{status.zero? ? 'exited' : 'failed'}\nExecMainCode=1\nExecMainStatus=#{status}\n")
 exit 0
when 'systemctl'
 raise 'supervisor mutation is forbidden in fixture' unless ARGV.include?('show') && ARGV.any?{|arg|arg.match?(/\Aworkspace-archive-[0-9a-f]{64}-1\.service\z/)}
 path=File.join(root,'supervisor.status');puts(File.file?(path) ? File.read(path) : 'LoadState=not-found')
when 'journalctl'
 raise 'journal target changed' unless ARGV.any?{|arg|arg.match?(/\A--unit=workspace-archive-[0-9a-f]{64}-1\.service\z/)}
 path=File.join(root,'supervisor-worker.log');print File.read(path) if File.file?(path)
else
 raise 'unrecognized fixture supervisor command'
end
`

func TestLifecycleCLIArchiveOperandsMatchSavedMode(t *testing.T) {
	workspace, state, authority := t.TempDir(), t.TempDir(), t.TempDir()
	const slug = "example"
	id := strings.Repeat("b", 64)
	cwd := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "app-server.sock")
	manifest := fmt.Sprintf("schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: origin-root\n  socket_path: %s\n  client_version: 0.172.0\n", socket)
	for name, body := range map[string]string{"portal.yml": manifest, "plan.md": "fixture\n", "state.md": "---\nlifecycle: active\n---\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	locks := filepath.Join(workspace, "worktrees", ".locks")
	if err := os.MkdirAll(locks, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(authority, slug+".lock")
	parent, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := unix.Flock(int(parent.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "archive")
	t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_FD", fmt.Sprint(parent.Fd()))
	t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", path)
	for _, test := range []struct {
		mode, command string
		force         *bool
		want          bool
	}{
		{"complete", "thread/retire", boolPointer(false), true}, {"complete", "thread/retire", boolPointer(true), false},
		{"complete", "team/archive-explicit", nil, false}, {"archived", "team/archive-retained", nil, false},
		{"archived", "thread/retire", boolPointer(true), true}, {"archived", "thread/retire", boolPointer(false), false},
	} {
		t.Run(test.mode+"/"+test.command+fmt.Sprint(test.force != nil && *test.force), func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"workspace": workspace, "slug": slug, "operation_id": id, "phase": "tracking_committed", "mode": test.mode, "retained_thread_id": "origin-root"})
			if err := os.WriteFile(filepath.Join(locks, slug+".archive.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if _, err := writer.WriteString(`{"operation_id":"` + id + `"}`); err != nil {
				t.Fatal(err)
			}
			writer.Close()
			previous := os.Stdin
			os.Stdin = reader
			defer func() { os.Stdin = previous }()
			selection, duplicate, err := admitLifecycleCommand(workspace, state, authority, slug, socket, test.command, test.force)
			if duplicate != nil {
				duplicate.Close()
			}
			if test.want {
				if err != nil || selection == nil {
					t.Fatal("saved mode refused", selection, err)
				}
			} else if err == nil || selection != nil {
				t.Fatal("changed lifecycle operand admitted", selection, err)
			}
			probe, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); err == nil {
				t.Fatal("admission released parent OFD")
			}
		})
	}
}
func boolPointer(value bool) *bool { return &value }
