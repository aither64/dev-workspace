//go:build pi_native_fixture

package piruntime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type teamConflictDropRuntime struct {
	*piruntime.Manager
	source  piruntime.RuntimeRef
	dropped atomic.Int32
}
type teamConflictDropHandler struct {
	piruntime.HostCallbackHandler
	owner *teamConflictDropRuntime
}

func (h *teamConflictDropHandler) HostTools() []string {
	return h.HostCallbackHandler.(piruntime.HostToolProvider).HostTools()
}
func (h *teamConflictDropHandler) InvalidateWorkflow(e piruntime.WorkflowInvalidation) {
	h.HostCallbackHandler.(piruntime.HostInvalidationHandler).InvalidateWorkflow(e)
}
func (h *teamConflictDropHandler) HandleHostCall(ctx context.Context, c piruntime.HostCall) (piruntime.HostReply, error) {
	reply, err := h.HostCallbackHandler.HandleHostCall(ctx, c)
	if err == nil && c.Runtime == h.owner.source && c.Tool == "team_message" && reply.Outcome == "result" && h.owner.dropped.CompareAndSwap(0, 1) {
		// The actual handler has completed the immutable pre-send refusal. Lose only
		// its result at the existing callback port, retaining real native history.
		return piruntime.HostReply{Outcome: "unknown", Code: "unavailable"}, nil
	}
	return reply, err
}
func (r *teamConflictDropRuntime) AttachHost(ctx context.Context, ref piruntime.RuntimeRef, h piruntime.HostCallbackHandler) error {
	return r.Manager.AttachHost(ctx, ref, &teamConflictDropHandler{h, r})
}

// Lead runs this ONE affected fixture after final review. The old production
// worker graph is real; predecessor Go RPC source equivalence and fake host
// terminal/drain evidence are explicitly separate from an old binary/systemd run.
func teamConflictSeed(t *testing.T, prepareOnly bool) centralReadyFixture {
	t.Helper()
	return centralPrepareNativeSeedWithUpgrade(t, false, false, true, prepareOnly, true, true, true)
}

func TestPiTeamConflictRetainedWorkerRecovery(t *testing.T) {
	if os.Getenv("PI_CENTRAL_APP_ROOT") == "" || os.Getenv("PI_USABILITY_PACKAGE") == "" {
		t.Skip("explicit retained and candidate packages required")
	}
	seed := teamConflictSeed(t, false)
	f, ctx := seed.fixture, seed.fixture.Context
	f.Idle(seed.handle)
	store, err := teamruntime.NewStore(seed.config.UserStateRoot, seed.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	roster, err := store.LoadPi(seed.intent.Slug)
	if err != nil || len(roster.Members) != 1 || roster.Members[0].Address != "reviewer0" {
		t.Fatal("exact lead/reviewer fixture", roster, err)
	}
	member := roster.Members[0]
	caller := teamruntime.PiCaller{Runtime: f.Runtime, Handle: seed.handle}
	if _, err := store.ReservePiInitialAssignment(ctx, seed.server, caller, roster.Revision, teamruntime.PiMessage{ID: "44444444-4444-4444-8444-444444444444", To: member.Address, Text: "original reserved assignment"}, seed.intent.ProviderSelection.Members[1].Settings.Effective()); err != nil {
		t.Fatal(err)
	}
	policy, err := seed.server.PiInitialWorkflowPolicyNativeFixture(seed.intent, member.Address)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := f.Manager.Create(ctx, f.Runtime, piruntime.CreateRequest{Session: f.Runtime.Session, Member: member.Address, Incarnation: member.Incarnation, CreationID: member.CreationID, Instructions: member.Instructions, Settings: seed.intent.ProviderSelection.Members[1].Settings.Effective(), ToolPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if proof, err := f.Manager.VerifyCreation(ctx, agentbackend.ExecutionHandle{Conversation: empty.Conversation, Generation: empty.Generation}, member.CreationID, empty.HistoryRef, nil); err != nil || proof.Persisted || proof.Bootstrap != nil {
		t.Fatal("native empty initialization", proof, err)
	}
	triggerID := "66666666-6666-4666-8666-666666666666"
	triggerInput := piruntime.SendInput{Message: "one input that produces the original conflict", ActionContext: "pi-delivery-v1:steer"}
	triggerRef := piruntime.SubmissionRef{Conversation: seed.handle.Conversation, OperationID: triggerID, Action: "send"}
	triggerRef.Digest = piruntime.SubmissionDigest(triggerRef, triggerInput, seed.state.Settings)
	sent, err := seed.ops.Send.Send(ctx, piruntime.BackendIdentity(seed.handle), backend.SendRequest{Message: triggerInput.Message, ClientUserMessageID: triggerID, ActionContext: triggerInput.ActionContext, Delivery: "steer"})
	if err != nil || (sent.Outcome != backend.Pending && sent.Outcome != backend.Accepted) {
		t.Fatal(sent, err)
	}
	centralWait(t, ctx, "original trigger has exact durable Accepted receipt", func() bool {
		status, err := f.Manager.Lookup(ctx, triggerRef)
		if err != nil || status.Ref != triggerRef || status.Generation != seed.handle.Generation || status.Settings != seed.state.Settings || status.Input != triggerInput {
			t.Fatal("original trigger receipt mismatch", status, err)
		}
		switch status.Phase {
		case "Pending", "Unknown":
			return false
		case "Accepted":
			if status.Witness == nil || status.Witness.SessionID != seed.handle.Conversation.NativeID || status.Witness.EntryID == "" || status.Witness.EffectiveDigest != piruntime.MessageDigest(triggerInput.Message) {
				t.Fatal("original trigger lacks exact native acceptance witness", status)
			}
			return true
		default:
			t.Fatal("original trigger was not accepted", status)
			return false
		}
	})
	dropped := seed.config.PiCreation.Runtime.(*teamConflictDropRuntime)
	centralWait(t, ctx, "actual conflicting callback lost result and held sender", func() bool {
		var snapshot struct{ Busy, Held bool }
		return json.Unmarshal(f.LegacySnapshot(seed.handle, nil), &snapshot) == nil && dropped.dropped.Load() == 1 && snapshot.Held && !snapshot.Busy
	})
	beforeRoster, err := store.LoadPi(seed.intent.Slug)
	if err != nil {
		t.Fatal(err)
	}
	bindings := session.BackendBindingStore{Root: seed.config.UserStateRoot, Workspace: seed.config.Workspace}
	beforeBinding, err := bindings.Load(seed.intent.Slug)
	if err != nil {
		t.Fatal(err)
	}
	root, err := piruntime.SessionStateDirectory(seed.config.UserStateRoot, f.Runtime.Session)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string][]byte{}
	for _, ref := range []agentbackend.ConversationRef{seed.handle.Conversation, empty.Conversation} {
		for _, directory := range []string{"history", "submissions"} {
			path := filepath.Join(root, "members", ref.Member, ref.Incarnation, directory)
			entries, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.IsDir() {
					t.Fatal("unexpected fixture artifact")
				}
				file := filepath.Join(path, entry.Name())
				saved[file], err = os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	dispatches := f.ObserveUpgradeDispatches()
	// Same existing ordinary CLI/Unix HTTP boundary as the ready-upgrade fixture.
	binary := filepath.Join(t.TempDir(), "workspace-portal")
	build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, "./cmd/workspace-portal")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("ordinary CLI build: %v\n%s", err, output)
	}
	socketDirectory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer socketDirectory.Close()
	socket := fmt.Sprintf("/proc/%d/fd/%d/portal.sock", os.Getpid(), socketDirectory.Fd())
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: seed.server.Handler()}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	args := []string{"pi-runtime", "upgrade", "--workspace", seed.config.Workspace, "--slug", seed.intent.Slug, "--request", "77777777-7777-4777-8777-777777777777", "--expected-generation", seed.handle.Generation, "--socket", socket, "--origin", "https://portal.example", "--recover-team-conflict", "55555555-5555-4555-8555-555555555555", "--member", "lead"}
	recover := func() piruntime.UpgradeResult {
		t.Helper()
		output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("public recovery: %v\n%s", err, output)
		}
		var r piruntime.UpgradeResult
		if json.Unmarshal(output, &r) != nil {
			t.Fatal("strict result", string(output))
		}
		return r
	}
	result := recover()
	if result.Disposition != "recovered-team-conflict" || result.TeamMessageID != "55555555-5555-4555-8555-555555555555" || result.Phase != "ready" || recover() != result {
		t.Fatal(result)
	}
	if starts, stops := f.UpgradeEffects(); starts != 1 || stops != 1 {
		t.Fatal("recovery repeated host effects", starts, stops)
	}
	if creates, sends, configures := dispatches(); creates != 0 || sends != 0 || configures != 0 {
		t.Fatal("recovery dispatched input/settings", creates, sends, configures)
	}
	for path, before := range saved {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("recovery changed original history/receipt", path, err)
		}
	}
	afterRoster, err := store.LoadPi(seed.intent.Slug)
	if err != nil || !reflect.DeepEqual(afterRoster, beforeRoster) {
		t.Fatal("reservation/roster changed", afterRoster, err)
	}
	afterBinding, err := bindings.Load(seed.intent.Slug)
	if err != nil || !reflect.DeepEqual(afterBinding, beforeBinding) {
		t.Fatal("binding changed", afterBinding, err)
	}
	current := agentbackend.ExecutionHandle{Conversation: seed.handle.Conversation, Generation: result.Runtime.Generation}
	selected, err := f.Manager.ReadInputSelection(ctx, current)
	if err != nil || selected.Held || selected.PendingPolicy || selected.PendingSettings || selected.Handle != current {
		t.Fatal("fresh selection held", selected, err)
	}
	read := func(route string) *httptest.ResponseRecorder {
		return centralRequest(ctx, seed.server.Handler(), "GET", "/codex/conversations/"+seed.intent.Slug+"/"+route, seed.scope, nil)
	}
	page := read("thread/page")
	var transcript backend.Transcript
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &transcript) != nil || len(transcript.Entries) == 0 {
		t.Fatal("fresh public page", page.Code, page.Body.String())
	}
	activity := read("activity")
	var observed backend.ActivitySnapshot
	if activity.Code != 200 || json.Unmarshal(activity.Body.Bytes(), &observed) != nil {
		t.Fatal("fresh public activity", activity.Code, activity.Body.String())
	}
	statuses := read("deliveries")
	var collection backend.DeliveryCollection
	if statuses.Code != 200 || json.Unmarshal(statuses.Body.Bytes(), &collection) != nil || !collection.CanEnqueue {
		t.Fatal("fresh public admission unavailable", statuses.Code, statuses.Body.String())
	}
	laterID := "99999999-9999-4999-8999-999999999999"
	request := map[string]any{"message": "one later synthetic input", "clientUserMessageId": laterID}
	for i := 0; i < 2; i++ {
		response := centralRequest(ctx, seed.server.Handler(), "POST", "/api/sessions/"+seed.intent.Slug+"/message", seed.scope, request)
		if response.Code != http.StatusAccepted {
			t.Fatal("later ordinary input", response.Code, response.Body.String())
		}
	}
	f.Idle(current)
	later := read("thread/page")
	var final backend.Transcript
	if later.Code != 200 || json.Unmarshal(later.Body.Bytes(), &final) != nil {
		t.Fatal("later page", later.Code)
	}
	count := 0
	for _, entry := range final.Entries {
		if entry.Kind == "userMessage" && entry.ClientUserMessageID == laterID {
			count++
		}
	}
	if count != 1 {
		t.Fatal("later original input did not start once", count)
	}
	laterBytes, err := os.ReadFile(filepath.Join(root, "members", current.Conversation.Member, current.Conversation.Incarnation, "submissions", laterID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeCreates, beforeSends, beforeConfigures := dispatches()
	if replay := recover(); replay != result {
		t.Fatal("completed replay after new input", replay, result)
	}
	if creates, sends, configures := dispatches(); creates != beforeCreates || sends != beforeSends || configures != beforeConfigures {
		t.Fatal("completed replay dispatched input", creates, sends, configures)
	}
	if starts, stops := f.UpgradeEffects(); starts != 1 || stops != 1 {
		t.Fatal("post-input replay repeated host effects", starts, stops)
	}
	afterLater, err := os.ReadFile(filepath.Join(root, "members", current.Conversation.Member, current.Conversation.Incarnation, "submissions", laterID+".json"))
	if err != nil || !bytes.Equal(laterBytes, afterLater) {
		t.Fatal("completed replay changed later receipt", err)
	}

}

func teamConflictOriginalSetup(t *testing.T) (piproviders.PackageConfiguration, piruntime.P6NativeOptions) {
	t.Helper()
	original, err := piproviders.LoadInstalled(os.Getenv("PI_CENTRAL_APP_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := piproviders.ProjectRuntimeAccounts(original.Catalog, original.Default.Chain, piruntime.CanonicalCredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	options := piruntime.P6NativeOptions{AccountBindings: bindings}
	for _, target := range original.Default.Chain {
		found := false
		for _, provider := range original.Catalog.Providers {
			if provider.ID == target.ProviderID {
				options.Models = append(options.Models, piruntime.P6NativeModel{Provider: provider.NativeID, Model: target.ModelID, Thinking: target.Thinking})
				found = true
			}
		}
		if !found {
			t.Fatal("original native provider missing")
		}
	}
	return original, options
}
func TestPiTeamConflictRetainedSetup(t *testing.T) {
	if os.Getenv("PI_CENTRAL_APP_ROOT") == "" || os.Getenv("PI_CENTRAL_CATALOG_FIXTURE") == "" || os.Getenv("PI_USABILITY_PACKAGE") == "" {
		t.Skip("explicit retained package/catalog and candidate required")
	}
	original, options := teamConflictOriginalSetup(t)
	seed := teamConflictSeed(t, true)
	intent := seed.intent
	if seed.fixture.Manager == nil || seed.providers.Digest == "" || intent.ProviderSelection.CatalogDigest != original.Catalog.Digest {
		t.Fatal("preparation did not reach both immutable packages and original selection")
	}
	starts, stops := seed.fixture.UpgradeEffects()
	root, err := piruntime.SessionStateDirectory(filepath.Join(filepath.Dir(seed.fixture.Runtime.Session.Workspace), "state"), seed.fixture.Runtime.Session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "members")); !os.IsNotExist(err) || starts != 0 || stops != 0 {
		t.Fatal("preparation created native owners or stopped/started a successor", err, starts, stops)
	}
	if len(intent.Conversations) != 2 || intent.DirectTeam.Members[0].Address != "reviewer0" || len(options.Models) != len(original.Default.Chain) {
		t.Fatal("incomplete original team/chain")
	}
	for _, member := range intent.ProviderSelection.Members {
		effective := member.Settings.Effective()
		selected := original.Default.Chain[0]
		if effective.AccountRef != selected.AccountRef || effective.Model != selected.ModelID || effective.Thinking != selected.Thinking {
			t.Fatal("original selection changed", effective)
		}
		found := false
		for _, binding := range options.AccountBindings {
			found = found || binding.Provider == effective.Provider && binding.AccountRef == effective.AccountRef
		}
		if !found {
			t.Fatal("unbound native account")
		}
	}
}
