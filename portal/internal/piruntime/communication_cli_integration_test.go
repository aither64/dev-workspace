//go:build pi_native_fixture

package piruntime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/web"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// Only the Codex peer is synthetic. Pi uses the existing actual Manager, Unix
// supervisor and pinned disposable worker; CLI and HTTP owners are production.
type communicationCodexPeer struct {
	*codex.Client
	mu                sync.Mutex
	workspace, thread string
	receipts          map[string]codex.SendReceipt
	sends             int
}

func (p *communicationCodexPeer) ReadThreadMetadata(_ context.Context, id string, _ bool) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{ID: id, Cwd: filepath.Join(p.workspace, "work", "codex-peer")}, nil
}
func (p *communicationCodexPeer) ReadActivity(_ context.Context, id string) (codex.ActivitySnapshot, error) {
	return codex.ActivitySnapshot{ThreadID: id, CurrentState: "idle"}, nil
}
func (p *communicationCodexPeer) ReadThread(_ context.Context, id string) (codex.Transcript, error) {
	return codex.Transcript{ThreadID: id, Status: "idle"}, nil
}
func (p *communicationCodexPeer) VerifyThread(context.Context, string, string) error { return nil }
func (p *communicationCodexPeer) ListThreadActivity(_ context.Context, rows []workspacecodex.ThreadActivity) ([]workspacecodex.ThreadActivity, error) {
	return rows, nil
}
func (p *communicationCodexPeer) ReconcileThreadInstructionsWithPolicy(context.Context, string, codex.ThreadPolicy) error {
	return nil
}
func (p *communicationCodexPeer) ReadAccountRateLimits(context.Context) (codex.AccountRateLimits, error) {
	return codex.AccountRateLimits{}, nil
}
func (p *communicationCodexPeer) SendWithOptions(_ context.Context, thread, text, id, source string, _ codex.TurnOptions) (codex.SendReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if thread != p.thread || !strings.Contains(text, "Pi CLI message") || !strings.HasPrefix(source, "communication:") {
		return codex.SendReceipt{}, fmt.Errorf("foreign disposable Codex input")
	}
	p.sends++
	receipt := codex.SendReceipt{ClientUserMessageID: id, TurnID: "fixture-codex-turn"}
	p.receipts[id] = receipt
	return receipt, nil
}
func (p *communicationCodexPeer) ReconcileSendWithOptions(_ context.Context, _ string, _ string, id, _ string, _ codex.TurnOptions) (codex.SendReceipt, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.receipts[id]
	return r, ok, nil
}

// centralIntent fixes its lead model to the old p3 catalog. This case selects
// the genuine installed Solo preset, retaining its instructions/access/digests,
// and uses the existing explicit synthetic provider/settings override instead.
func communicationNativeIntent(t *testing.T, f piruntime.P6NativeFixture, catalog agentteams.Catalog) (session.PiCreationIntent, piproviders.Catalog) {
	t.Helper()
	preset, err := teamruntime.FindCatalogPreset(catalog, "solo")
	if err != nil {
		t.Fatal(err)
	}
	preset, err = teamruntime.ProjectPiPreset(preset)
	if err != nil {
		t.Fatal(err)
	}
	providers := centralProviderCatalog()
	reserved := []session.PiReservedConversation{{Address: "lead", Incarnation: strings.Repeat("b", 64)}}
	policy := piproviders.Policy{Session: f.Runtime.Session, Revision: 1, Default: piproviders.VersionedSelection{Revision: 1, Selection: piproviders.Selection{Chain: []piproviders.Target{{ProviderID: "fixture", AccountRef: "fake-account", ModelID: "p3-model", Thinking: "high"}}, FallbackOn: []piproviders.FailureCategory{}}}, Overrides: []piproviders.Override{}}
	frozen, err := session.FreezePiCreationSelection(providers, policy, reserved)
	if err != nil {
		t.Fatal(err)
	}
	history, err := session.CompletedRemovalHistory(f.Runtime.Session.Workspace, f.Runtime.Session.Slug, filepath.Join(filepath.Dir(f.Runtime.Session.Workspace), "state"))
	if err != nil {
		t.Fatal(err)
	}
	intent := session.PiCreationIntent{Schema: 4, Workspace: f.Runtime.Session.Workspace, Slug: f.Runtime.Session.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: strings.Repeat("c", 64), DeletionHistorySHA256: history, SessionIncarnation: f.Runtime.Session.Incarnation, Conversations: reserved, GoalSHA256: piruntime.MessageDigest("communication fixture original"), Model: "p3-model", Effort: "high", Provider: "p3-fake", ProviderSelection: frozen, DirectTeam: session.PiCreationTeam{ID: preset.ID, Name: preset.Name, Description: preset.Description, Roles: preset.Roles, CatalogDigest: preset.CatalogDigest, TeamDigest: preset.TeamDigest, LeadModel: "p3-model", LeadEffort: "high", LeadInstructions: preset.LeadInstructions, Members: []session.PiCreationTeamMember{}}, InitialInputID: "44444444-4444-4444-8444-444444444444"}
	if err := intent.Validate(providers); err != nil {
		t.Fatal(err)
	}
	return intent, providers
}

// The owning readers require real package directories, not a share symlink.
// Keep the immutable metadata bytes beside the current CLI without repointing
// the separate P6 fixture's immutable runtime/worker inputs to this private root.
func stageCommunicationNativePackage(t *testing.T, immutable string) (string, agentteams.Installed, []string) {
	t.Helper()
	original, err := agentteams.LoadInstalled(immutable)
	if err != nil || !original.Managed || original.Catalog == nil {
		t.Fatal("owning immutable catalog", err)
	}
	paths := []string{"share/dev-workspace/package.json", original.Metadata.AgentTeams.Catalog.Path, "share/workspace-portal/runtime-contract.json"}
	for _, role := range original.Catalog.NativeAgentConfigs.Roles {
		paths = append(paths, role.Path)
	}
	for _, utility := range original.Catalog.NativeAgentConfigs.Utilities {
		paths = append(paths, utility.Path)
	}
	root := filepath.Join(t.TempDir(), "communication-package")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range paths {
		data, err := os.ReadFile(filepath.Join(immutable, relative))
		if err != nil {
			t.Fatal("read immutable metadata", relative, err)
		}
		target := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal("stage unchanged metadata", relative, err)
		}
	}
	installed, err := agentteams.LoadInstalled(root)
	if err != nil || !installed.Managed || installed.Catalog == nil {
		t.Fatal("owning staged catalog", err)
	}
	if !reflect.DeepEqual(installed.Metadata, original.Metadata) || !reflect.DeepEqual(installed.Catalog, original.Catalog) {
		t.Fatal("staged package metadata/catalog identity changed")
	}
	return root, installed, paths
}

func TestPiCommunicationNativeFixtureShape(t *testing.T) {
	immutable := os.Getenv("PI_CENTRAL_APP_ROOT")
	if immutable == "" {
		t.Skip("explicit existing immutable component metadata required")
	}
	root, installed, paths := stageCommunicationNativePackage(t, immutable)
	for _, relative := range paths {
		original, err := os.ReadFile(filepath.Join(immutable, relative))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, relative)
		staged, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(staged, original) {
			t.Fatal("staged metadata bytes changed", relative, err)
		}
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			t.Fatal("staged metadata is not an ordinary protected file", relative, err)
		}
		for directory := filepath.Dir(target); ; directory = filepath.Dir(directory) {
			info, err := os.Lstat(directory)
			if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
				t.Fatal("staged metadata parent is not an ordinary protected directory", relative, err)
			}
			if directory == root {
				break
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var contract map[string]json.RawMessage
	if err := session.DecodePiCreationJSON(data, &contract, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := session.DecodePiTeamRecordsContract(contract["piTeamRecords"]); err != nil {
		t.Fatal(err)
	}
	if _, err := session.DecodePiRoleInstructionsContract(data); err != nil {
		t.Fatal(err)
	}
	t.Logf("staged metadata files=%d native_configs=%d managed=%t catalog_digest=%s runtime_contract_sha256=%x", len(paths), len(installed.Catalog.NativeAgentConfigs.Roles)+len(installed.Catalog.NativeAgentConfigs.Utilities), installed.Managed, installed.Catalog.CatalogDigest, sha256.Sum256(data))
	f := piruntime.P6NativeFixture{Runtime: piruntime.RuntimeRef{Session: agentbackend.SessionRef{Workspace: t.TempDir(), Slug: "2026-11-20-communication", Incarnation: strings.Repeat("a", 64)}}}
	communicationNativeIntent(t, f, *installed.Catalog)
}

func TestPiCommunicationCLIHTTPOriginalNativeReply(t *testing.T) {
	immutable := os.Getenv("PI_CENTRAL_APP_ROOT")
	if immutable == "" || os.Getenv("PI_PROBE_PACKAGE") == "" {
		t.Skip("explicit existing immutable component inputs required")
	}
	// Stage/validate before compiling the current CLI and before the native
	// fixture's bounded context begins. Both tests use the same owning reader.
	packageRoot, installed, _ := stageCommunicationNativePackage(t, immutable)
	binary := filepath.Join(packageRoot, "bin/workspace-portal")
	build := exec.Command("go", "build", "-mod=readonly", "-o", binary, "./cmd/workspace-portal")
	build.Dir = filepath.Clean(filepath.Join("..", ".."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("genuine communication CLI build: %v\n%s", err, output)
	}
	f := piruntime.NewP6NativeFixture(t)
	ctx := f.Context

	intent, providers := communicationNativeIntent(t, f, *installed.Catalog)
	goal := "communication fixture original"
	root := filepath.Dir(intent.Workspace)
	authority := filepath.Join(root, "communication-authority")
	if err := os.MkdirAll(authority, 0700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "communication-profile")
	if err := os.Symlink(packageRoot, profile); err != nil {
		t.Fatal(err)
	}
	transition := filepath.Join(root, "communication-transition.lock")
	centralWrite(t, transition, nil)
	peer := &communicationCodexPeer{workspace: intent.Workspace, thread: "codex-peer-root", receipts: map[string]codex.SendReceipt{}}
	host := filepath.Join(root, "communication-host")
	if err := os.MkdirAll(host, 0700); err != nil {
		t.Fatal(err)
	}
	hostDir, err := os.Open(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hostDir.Close() })
	hostSocketDir := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), hostDir.Fd())
	devSession, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", filepath.Join(root, "absent-provider-catalog"))
	config := web.Config{Workspace: intent.Workspace, BaseURL: "https://portal.example", DevSession: devSession, HostProfile: profile, AuthorityDir: authority, TransitionLock: transition, UserStateRoot: filepath.Join(root, "state"), PackageRoot: packageRoot, WorkspaceName: "communication", CodexSocket: filepath.Join(hostSocketDir, "app-server.sock"), RegistrationMarker: filepath.Join(hostSocketDir, "registration.json"), Logger: log.New(io.Discard, "", 0), PiCreation: &web.PiCreationServices{Catalog: providers, Runtime: f.Manager}, Codex: peer}
	server, err := web.NewContext(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	initial, err := server.PiInitialWorkflowPolicyNativeFixture(intent, "lead")
	if err != nil || initial == nil {
		t.Fatal(err)
	}
	state, err := f.Manager.Create(ctx, f.Runtime, piruntime.CreateRequest{Session: intent.SessionRef(), Member: "lead", Incarnation: intent.Conversations[0].Incarnation, CreationID: intent.ReceiptID, Instructions: intent.DirectTeam.LeadInstructions, Settings: intent.ProviderSelection.Members[0].Settings.Effective(), ToolPolicy: initial})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	ops, err := f.Manager.Bind(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Send.Send(ctx, piruntime.BackendIdentity(handle), backend.SendRequest{Message: goal, ClientUserMessageID: intent.InitialInputID, ActionContext: "initial"}); err != nil {
		t.Fatal(err)
	}
	originalRef := piruntime.SubmissionRef{Conversation: state.Conversation, OperationID: intent.InitialInputID, Action: "send"}
	originalRef.Digest = piruntime.SubmissionDigest(originalRef, piruntime.SendInput{Message: goal, ActionContext: "initial"}, state.Settings)
	original, err := f.Manager.Lookup(ctx, originalRef)
	if err != nil || original.Phase != "Accepted" {
		t.Fatal(original, err)
	}
	f.Idle(handle)
	centralPublishReady(t, config, intent, state, original)
	tracking := filepath.Join(intent.Workspace, "work", "codex-peer")
	for name, text := range map[string]string{"plan.md": "Disposable Codex peer\n", "state.md": "---\nlifecycle: active\n---\n", "portal.yml": "schema: 1\nslug: codex-peer\ncodex:\n  thread_id: codex-peer-root\nrepositories: []\nartifacts: []\n"} {
		centralWrite(t, filepath.Join(tracking, name), []byte(text))
	}
	codexSocket, err := net.Listen("unix", config.CodexSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { codexSocket.Close() })
	centralJSON(t, filepath.Join(authority, "codex-peer.json"), session.RuntimeAuthority{Schema: 1, Workspace: intent.Workspace, Slug: "codex-peer", TmuxSocket: filepath.Join(root, "tmux"), TmuxSessionID: "$1", CodexThreadID: peer.thread, CodexSocketPath: config.CodexSocket, CodexClientVersion: "fixture", State: "ready"})
	if err := (session.RecoveryStore{Root: config.UserStateRoot, Workspace: intent.Workspace}).Set(ctx, "codex-peer", peer.thread, config.CodexSocket, true, true); err != nil {
		t.Fatal(err)
	}
	// Production current-context workflow GET owns the ordinary host attachment.
	response := centralRequest(ctx, server.Handler(), "GET", "/codex/conversations/"+intent.Slug+"/pi-workflows/plans", web.PiConversationScope(state.Conversation), nil)
	if response.Code != 200 {
		t.Fatal("ordinary workflow attachment", response.Code, response.Body.String())
	}
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	socket := fmt.Sprintf("/proc/%d/fd/%d/portal.sock", os.Getpid(), directory.Fd())
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	t.Cleanup(func() { httpServer.Close(); <-done })
	parent, err := os.OpenFile(transition, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := syscall.Flock(int(parent.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	var profileStat syscall.Stat_t
	if err := syscall.Lstat(profile, &profileStat); err != nil {
		t.Fatal(err)
	}
	identity, _ := json.Marshal([]any{profileStat.Dev, profileStat.Ino, profileStat.Ctim.Sec, profileStat.Ctim.Nsec, profileStat.Mtim.Sec, profileStat.Mtim.Nsec, packageRoot})
	call := func(slug string, request communication.Request) communication.EndpointView {
		t.Helper()
		input, _ := json.Marshal(request)
		command := exec.CommandContext(ctx, binary, "communication", "--workspace", intent.Workspace, "--user-state-root", config.UserStateRoot, "--authority-dir", authority, "--socket", config.CodexSocket, "--session-slug", slug, "--portal-socket", socket, "--portal-origin", config.BaseURL, "--package-root", packageRoot, "--host-profile", profile, "--transition-lock", transition, "--transition-lock-fd", "3", "--expected-profile-identity", string(identity))
		if slug == "codex-peer" {
			command.Args = append(command.Args, "--expected-root-thread-id", peer.thread)
		}
		command.ExtraFiles = []*os.File{parent}
		command.Stdin = bytes.NewReader(input)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("actual communication child: %v\n%s", err, output)
		}
		var view communication.EndpointView
		if json.Unmarshal(output, &view) != nil || view.ValidatePublic() != nil {
			t.Fatal("typed child receipt", string(output))
		}
		return view
	}
	approval := "  User allowed this exact pair; user-message-17  "
	allowed := call(intent.Slug, communication.Request{Action: "allow", Peer: "codex-peer", Approval: approval})
	if len(allowed.Grants) != 1 || allowed.Grants[0].Approval != approval {
		t.Fatal("CLI approval reference changed", allowed)
	}
	id, replyID := strings.Repeat("e", 32), strings.Repeat("f", 32)
	call(intent.Slug, communication.Request{Action: "send", Peer: "codex-peer", MessageID: id, Message: "Pi CLI message"})
	// The production worker dispatches the existing pending aggregate. History is
	// observation only; no copied dispatch handler or extra source operation.
	wait := func(slug, id string) communication.PiMessageRecord {
		t.Helper()
		for {
			view := call(slug, communication.Request{Action: "history", MessageID: id})
			if len(view.Messages) == 1 && view.Messages[0].Status == "delivered" {
				return view.Messages[0]
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait(intent.Slug, id)
	reply := communication.Request{Action: "reply", Peer: intent.Slug, MessageID: replyID, ReplyTo: id, Message: "Original native Pi reply"}
	call("codex-peer", reply)
	wait("codex-peer", replyID)
	call("codex-peer", reply)
	path := filepath.Join(config.UserStateRoot, "session-communication", fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Workspace))), "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := communication.DecodePiCommunication(data)
	if err != nil || len(aggregate.Messages) != 2 {
		t.Fatal(err)
	}
	saved := aggregate.Messages[1]
	if saved.ID != replyID || saved.PiDelivery == nil || saved.PiDelivery.Settings.Runtime() != state.Settings {
		t.Fatal("original Pi tuple changed", saved)
	}
	status, err := f.Manager.Lookup(ctx, saved.PiDelivery.Input.Ref())
	if err != nil || status.Phase != "Accepted" || status.Witness == nil || status.Witness.SessionID != state.Conversation.NativeID || status.Witness.EntryID == "" || status.Input.Message != saved.Envelope || status.Input.ActionContext != "pi-delivery-v1:steer" || status.Input.Retry || status.Settings != state.Settings || status.Generation != state.Generation {
		t.Fatal("real consumed native witness", status, err)
	}
	if got, err := f.Manager.Lookup(ctx, originalRef); err != nil || !reflect.DeepEqual(got, original) {
		t.Fatal("original creation receipt changed", err)
	}
	f.Idle(handle)
	transcript, err := ops.Read.Read(ctx, piruntime.BackendIdentity(handle))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "userMessage" && entry.ClientUserMessageID == replyID {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate native Pi input", count)
	}
	peer.mu.Lock()
	sends := peer.sends
	peer.mu.Unlock()
	if sends != 1 {
		t.Fatal("Codex input repeated", sends)
	}

}
