package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

func TestMonitorRequestKeepsActionAndReportAuthoritySeparate(t *testing.T) {
	for _, raw := range []string{
		`{"action":"report","slot":"terminal","outcome":"passed","exitStatus":0,"elapsedMillis":1}`,
		`{"action":"status"}`,
		`{"action":"status","id":"11111111111111111111111111111111"}`,
		`{"action":"resolve","operationId":"11111111111111111111111111111111","resolutionId":"22222222222222222222222222222222","evidence":["exact receipt"],"detail":"proved exit"}`,
	} {
		if _, err := decodeMonitorRequest([]byte(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{
		`{"action":"report","id":"11111111111111111111111111111111","outcome":"passed"}`,
		`{"action":"report","threadId":"another-lead"}`,
		`{"action":"cancel","id":"11111111111111111111111111111111","slot":"terminal"}`,
		`{"action":"resolve","operationId":"11111111111111111111111111111111","source":{}}`,
		`{"action":"start","source":{"kind":"ci","foreign":"value"}}`,
		`{"action":"attest-local"}`, `{"action":"status","nativeBindingDigest":"obsolete"}`,
		`{"action":"cancel"}`, `{"action":"archive"}`, `null`, `{"action":"status"} {}`,
	} {
		if _, err := decodeMonitorRequest([]byte(raw)); err == nil {
			t.Fatal("accepted mixed or unsupported request", raw)
		}
	}
}

func monitorLeadFixture(t *testing.T) teamMCPBinding {
	t.Helper()
	binding, _, _ := teamMCPFixture(t)
	manifest := "schema: 1\nslug: test-session\ncodex:\n  thread_id: lead-thread\n  socket_path: " + filepath.Join(binding.workspace, "native.sock") + "\n  client_version: 0.165.0\nrepositories: []\nartifacts: []\n"
	for name, body := range map[string]string{"portal.yml": manifest, "state.md": "---\nlifecycle: active\n---\n", "plan.md": "Monitor fixture\n"} {
		if err := os.WriteFile(filepath.Join(binding.workDir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	binding.slug, binding.rootThreadID, binding.address, binding.memberThreadID = "", "", "", ""
	return binding
}

func TestBoundMonitorMCPAndCommandLockUseCurrentLeadWithOriginalRoot(t *testing.T) {
	binding := monitorLeadFixture(t)
	runtime, err := os.MkdirTemp("/tmp", "monitor-current-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(runtime)
	socket := filepath.Join(runtime, "registered", "app-server.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	authorityDir := filepath.Join(filepath.Dir(socket), "authority")
	if err := os.MkdirAll(authorityDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 6\nslug: test-session\nrepositories: []\ncodex:\n  thread_id: lead-thread\n  socket_path: " + socket + "\n  client_version: 0.172.0\n  binding_schema: 3\n  binding_origin_schema: 1\n"
	if err := os.WriteFile(filepath.Join(binding.workspace, "work", "test-session", "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"schema":1,"initialProvider":"openai","providers":[{"id":"openai","label":"OpenAI","kind":"subscription"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_WORKSPACES_CODEX_PROVIDERS", catalog)
	inc, op, digest := strings.Repeat("a", 64), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", strings.Repeat("c", 64)
	state := codexproviders.State{Schema: 3, Workspace: "registered", Root: binding.workspace, Socket: socket, Active: "openai", BindingRevision: 2, Bindings: map[string]codexproviders.SessionBinding{"test-session": {RootThreadID: "lead-thread", Incarnation: inc, Revision: 2, Conversations: map[string]codexproviders.ConversationBinding{"lead-thread": {Member: "lead", ThreadID: "lead-child", Provider: "openai", OperationID: op, EvidenceDigest: digest}, "member-thread": {Member: "implementer0", ThreadID: "member-child", Provider: "openai", OperationID: op, EvidenceDigest: digest}}}}}
	authority := session.RuntimeAuthority{Schema: 3, State: "ready", Slug: "test-session", Workspace: binding.workspace, TmuxSocket: "/run/tmux.sock", TmuxSessionID: "$1", TmuxIdentity: inc, CodexThreadID: "lead-thread", CodexActiveThreadID: "lead-child", CodexBindingRevision: 2, CodexSocketPath: socket, CodexClientVersion: "0.172.0"}
	statePath, err := codexproviders.StatePath(binding.stateRoot, "registered")
	if err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]any{statePath: state, filepath.Join(authorityDir, "test-session.json"): authority} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(value)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	called := 0
	const id = "11111111111111111111111111111111"
	run := func(_ context.Context, invocation teamMCPInvocation) error {
		called++
		values := map[string]string{}
		for _, entry := range invocation.environment {
			k, v, _ := strings.Cut(entry, "=")
			values[k] = v
		}
		if values["DEV_SESSION_EXPECTED_MONITOR_THREAD_ID"] != "lead-child" || values["DEV_SESSION_EXPECTED_ROOT_THREAD_ID"] != "lead-thread" {
			t.Fatal("monitor dispatch changed origin/caller", values)
		}
		return json.NewEncoder(invocation.receipt).Encode(monitoroperation.NativeStatus{ExecutionOwner: "native", Operation: monitoroperation.Operation{Schema: 1, ID: id, Workspace: binding.workspace, Owner: monitoroperation.Owner{Slug: "test-session", RootThreadID: "lead-thread", LeadThreadID: "lead-child", RuntimeSchema: 3}}})
	}
	raw := json.RawMessage(`{"action":"start","id":"` + id + `","brief":"exact check","source":{"kind":"local"}}`)
	for _, caller := range []string{"lead-thread", "member-child", "foreign"} {
		meta, _ := json.Marshal(map[string]string{"threadId": caller})
		if result := binding.monitor(context.Background(), raw, meta, run).(map[string]any); result["isError"] != true || called != 0 {
			t.Fatal("noncurrent caller reached host", caller, result)
		}
	}
	result := binding.monitor(context.Background(), raw, json.RawMessage(`{"threadId":"lead-child"}`), run).(map[string]any)
	if result["isError"] != false || called != 1 {
		t.Fatal("current lead was not dispatched", result)
	}
	client := workspacecodex.NewWithOptions(socket, binding.workspace, codex.ClientOptions{})
	defer client.Close()
	service, err := newMonitorService(binding.workspace, binding.stateRoot, authorityDir, socket, binding.workspace, client)
	if err != nil {
		t.Fatal(err)
	}
	release, err := service.LockSession(context.Background(), "test-session")
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := session.LockRuntimeExclusive(authorityDir, "test-session"); err == nil {
		lock.Close()
		release()
		t.Fatal("runtime lock escaped monitor")
	}
	teams, err := teamruntime.NewStore(binding.stateRoot, binding.workspace)
	if err != nil {
		release()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if unlock, err := teams.LockOperation(ctx, "test-session"); err == nil {
		unlock()
		release()
		t.Fatal("team lock escaped monitor")
	}
	release()
	lock, err := session.LockRuntimeExclusive(authorityDir, "test-session")
	if err != nil {
		t.Fatal("runtime lock retained after release", err)
	}
	lock.Close()
	unlock, err := teams.LockOperation(context.Background(), "test-session")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestMonitorMCPRoutesExactLeadWithoutMemberAuthority(t *testing.T) {
	binding := monitorLeadFixture(t)
	mixedPiMCPFixture(t, binding)
	const id = "11111111111111111111111111111111"
	var captured teamMCPInvocation
	called := 0
	run := func(_ context.Context, invocation teamMCPInvocation) error {
		called++
		captured = invocation
		return json.NewEncoder(invocation.receipt).Encode(monitoroperation.NativeStatus{ExecutionOwner: "native", Operation: monitoroperation.Operation{Schema: 1, ID: id, Workspace: binding.workspace,
			Owner: monitoroperation.Owner{Slug: "test-session", RootThreadID: "lead-thread"}}})
	}
	raw := json.RawMessage(`{"action":"start","id":"` + id + `","brief":"exact check","source":{"kind":"local"}}`)
	result := binding.monitor(context.Background(), raw, json.RawMessage(`{"threadId":"lead-thread"}`), run).(map[string]any)
	if result["isError"] != false || called != 1 {
		t.Fatal(result, called)
	}
	if captured.command != binding.devSession || strings.Join(captured.args, " ") != "monitor start test-session --as-is --request-stdin --json" || captured.message != string(raw) {
		t.Fatal("monitor did not use selected-profile exact request", captured)
	}
	values := map[string]string{}
	for _, item := range captured.environment {
		key, value, _ := strings.Cut(item, "=")
		values[key] = value
	}
	if values["DEV_SESSION_EXPECTED_MONITOR_THREAD_ID"] != "lead-thread" || values["DEV_SESSION_EXPECTED_ROOT_THREAD_ID"] != "lead-thread" ||
		values["DEV_SESSION_MEMBER_ADDRESS"] != "" || values["DEV_SESSION_EXPECTED_MEMBER_THREAD_ID"] != "" {
		t.Fatal("monitor inherited or invented member authority", values)
	}
	for _, meta := range []string{`{}`, `{"threadId":"member-thread"}`, `{"threadId":"foreign"}`} {
		result := binding.monitor(context.Background(), raw, json.RawMessage(meta), run).(map[string]any)
		if result["isError"] != true || called != 1 {
			t.Fatal("unowned caller invoked monitor host", meta, result)
		}
	}
	result = binding.monitor(context.Background(), json.RawMessage(`{"action":"report","slot":"terminal","outcome":"incomplete"}`),
		json.RawMessage(`{"threadId":"lead-thread"}`), run).(map[string]any)
	if result["isError"] != true || called != 1 {
		t.Fatal("lead impersonated registered utility", result)
	}
}

func TestMonitorRecoveryOwnerRequiresOriginalIncarnationAndJournalRoot(t *testing.T) {
	binding := monitorLeadFixture(t)
	directory := filepath.Join(binding.workspace, "work", "test-session")
	incarnation, err := session.ObservationIdentity(binding.workspace, "test-session", directory, "lead-thread")
	if err != nil {
		t.Fatal(err)
	}
	op := monitoroperation.Operation{Workspace: binding.workspace, Owner: monitoroperation.Owner{Slug: "test-session", RootThreadID: "lead-thread",
		Incarnation: incarnation, Socket: filepath.Join(binding.workspace, "native.sock")}}
	locks := filepath.Join(binding.workspace, "worktrees", ".locks")
	if err := os.MkdirAll(locks, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(locks, "test-session.archive.json")
	writeJournal := func(root string) {
		data, _ := json.Marshal(map[string]any{"workspace": binding.workspace, "slug": "test-session", "phase": "prepared", "mode": "archived",
			"operation_id": strings.Repeat("a", 64), "retained_thread_id": root})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeJournal("lead-thread")
	if _, err := monitorRecoveryOwner(binding.workspace, op, "lead-thread"); err != nil {
		t.Fatal("original owner could not inspect pending lifecycle", err)
	}
	writeJournal("replacement")
	if _, err := monitorRecoveryOwner(binding.workspace, op, "lead-thread"); err == nil {
		t.Fatal("adopted foreign lifecycle root")
	}
	writeJournal("lead-thread")
	op.Owner.Incarnation = "foreign"
	if _, err := monitorRecoveryOwner(binding.workspace, op, "lead-thread"); err == nil {
		t.Fatal("adopted another session incarnation")
	}
}

func TestMonitorMCPAcknowledgesDirectOwnerWithoutNativeLaunch(t *testing.T) {
	binding := monitorLeadFixture(t)
	const id = "11111111111111111111111111111111"
	raw := json.RawMessage(`{"action":"start","id":"` + id + `","brief":"exact CI","source":{"kind":"ci","provider":"github","repository":"example/project","workflowId":"12","runId":"34","runAttempt":1,"commit":"` + strings.Repeat("a", 40) + `"}}`)
	for _, kind := range []string{"direct", "missing-owner", "wrong-owner", "fake-launch", "wrong-id"} {
		called := 0
		run := func(_ context.Context, inv teamMCPInvocation) error {
			called++
			value := map[string]any{"schema": 1, "id": id, "workspace": binding.workspace, "owner": map[string]any{"slug": "test-session", "rootThreadId": "lead-thread"}, "driver": "direct-ci", "executionOwner": "runtime-ci"}
			switch kind {
			case "missing-owner":
				delete(value, "executionOwner")
			case "wrong-owner":
				value["executionOwner"] = "native"
			case "fake-launch":
				value["launch"] = map[string]any{"threadId": "invented"}
			case "wrong-id":
				value["id"] = strings.Repeat("2", 32)
			}
			return json.NewEncoder(inv.receipt).Encode(value)
		}
		result := binding.monitor(context.Background(), raw, json.RawMessage(`{"threadId":"lead-thread"}`), run).(map[string]any)
		if called != 1 || result["isError"] != (kind != "direct") {
			t.Fatal(kind, result, called)
		}
	}
}

func TestMonitorMCPRejectsUnboundNativeAcknowledgement(t *testing.T) {
	binding := monitorLeadFixture(t)
	const id = "11111111111111111111111111111111"
	status := monitoroperation.NativeStatus{ExecutionOwner: "native", Operation: monitoroperation.Operation{
		Schema: 1, ID: id, Workspace: binding.workspace,
		Owner: monitoroperation.Owner{Slug: "test-session", RootThreadID: "lead-thread"}}}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"action":"start","id":"` + id + `","brief":"exact check","source":{"kind":"local"}}`)
	for _, name := range []string{"workspace", "slug", "root", "id", "schema", "missing schema", "discriminator", "missing discriminator", "unknown", "malformed", "trailing"} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "workspace":
				value["workspace"] = binding.workspace + "/foreign"
			case "slug":
				value["owner"].(map[string]any)["slug"] = "foreign"
			case "root":
				value["owner"].(map[string]any)["rootThreadId"] = "foreign"
			case "id":
				value["id"] = strings.Repeat("2", 32)
			case "schema":
				value["schema"] = 2
			case "missing schema":
				delete(value, "schema")
			case "discriminator":
				value["executionOwner"] = "runtime-ci"
			case "missing discriminator":
				delete(value, "executionOwner")
			case "unknown":
				value["unsupported"] = true
			}
			ack, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "malformed" {
				ack = []byte(`{`)
			} else if name == "trailing" {
				ack = append(ack, []byte(`{}`)...)
			}
			called := 0
			run := func(_ context.Context, invocation teamMCPInvocation) error {
				called++
				_, err := invocation.receipt.Write(ack)
				return err
			}
			result := binding.monitor(context.Background(), raw, json.RawMessage(`{"threadId":"lead-thread"}`), run).(map[string]any)
			if called != 1 || result["isError"] != true {
				t.Fatal("unbound native acknowledgement adopted", result, called)
			}
		})
	}
}
