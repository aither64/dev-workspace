package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"github.com/coder/websocket"
)

func TestRootThreadSettingsKeepLeadPolicySeparateFromModel(t *testing.T) {
	for _, selected := range []struct{ model, effort string }{
		{}, {"gpt-6-sol", "xhigh"},
	} {
		settings := rootThreadSettings(selected.model, selected.effort)
		if settings.Model != selected.model || settings.ReasoningEffort != selected.effort ||
			settings.Policy != workspacecodex.LeadThreadPolicy() {
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

func TestTeamMemberSettingsRequireAnExplicitLivePair(t *testing.T) {
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
