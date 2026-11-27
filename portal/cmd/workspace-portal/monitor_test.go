package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
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

const monitorFixtureID = "11111111111111111111111111111111"

func monitorLocalStart() json.RawMessage {
	return json.RawMessage(`{"action":"start","id":"` + monitorFixtureID + `","brief":"synthetic check","source":{"kind":"local","argv":["printf","two words","","$HOME; | *"],"cwd":"/synthetic/check","inputIdentity":"fixture-input","revision":"fixture-revision","invocationId":"` + monitorFixtureID + `"}}`)
}

func monitorRPCCall(t *testing.T, binding teamMCPBinding, raw json.RawMessage, meta string, legacy bool, run func(context.Context, teamMCPInvocation) error) map[string]any {
	t.Helper()
	args := []string{"--user-state-root", binding.stateRoot, "--workspace", binding.workspace, "--host-profile", binding.hostProfile}
	if legacy {
		args = append(args, "--session-slug", "test-session", "--root-thread-id", "lead-thread", "--member-address", "reviewer0", "--member-thread-id", "member-thread")
	}
	params := map[string]any{"name": "monitor_operation", "arguments": raw}
	if meta != "" {
		params["_meta"] = json.RawMessage(meta)
	}
	var out, diagnostics bytes.Buffer
	if err := teamMCPCommand(args, strings.NewReader(teamMCPRequestLine(t, 1, "tools/call", params)), &out, &diagnostics, teamMCPDependencies{run: run}); err != nil {
		t.Fatal(err)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected monitor stderr: %q", diagnostics.String())
	}
	return teamMCPResult(t, teamMCPResponses(t, out.String())[0])
}

func monitorRPCError(t *testing.T, result map[string]any, want string) {
	t.Helper()
	expected, err := json.Marshal(teamMCPToolResult(want, true))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(result)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("wanted static monitor error %q, got %s (%v)", want, actual, err)
	}
}

func TestMonitorMCPRoutesExactLeadWithFinalizedPiArchive(t *testing.T) {
	binding := monitorLeadFixture(t)
	for _, key := range []string{"DEV_SESSION_WORKSPACE", "DEV_SESSION_SLUG"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	// This is a finalized schema-4 manifest in archive/, not an in-progress
	// Pi roster. The helper must prove uniqueness across both tracking roots.
	archive := filepath.Join(binding.workspace, "archive", "unrelated-pi")
	if err := os.MkdirAll(archive, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := `schema: 4
slug: unrelated-pi
backend:
  kind: pi
  contract: 1
  session_incarnation: "` + strings.Repeat("a", 64) + `"
creation:
  state: ready
  goal_sha256: "` + strings.Repeat("b", 64) + `"
  initial_goal_sent: true
repositories: []
archive_operation_id: "` + strings.Repeat("c", 64) + `"
finalized_at: "2026-01-01T00:00:00Z"
`
	for name, value := range map[string]string{"portal.yml": manifest, "plan.md": "synthetic archive", "state.md": "---\nlifecycle: archived\n---\n"} {
		if err := os.WriteFile(filepath.Join(archive, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw := monitorLocalStart()
	calls := 0
	run := func(_ context.Context, inv teamMCPInvocation) error {
		calls++
		if inv.command != binding.devSession || inv.workDir != binding.workDir || strings.Join(inv.args, " ") != "monitor start test-session --as-is --request-stdin --json" || inv.message != string(raw) {
			t.Fatal("monitor did not use selected profile and exact request")
		}
		values := map[string]string{}
		for _, item := range inv.environment {
			key, value, _ := strings.Cut(item, "=")
			values[key] = value
		}
		if values["DEV_SESSION_WORKSPACE"] != binding.workspace || values["DEV_SESSION_SLUG"] != "test-session" ||
			values["DEV_SESSION_EXPECTED_MONITOR_THREAD_ID"] != "lead-thread" || values["DEV_SESSION_EXPECTED_ROOT_THREAD_ID"] != "lead-thread" {
			t.Fatal("monitor did not bind the exact lead")
		}
		for _, key := range []string{"DEV_SESSION_MEMBER_ADDRESS", "DEV_SESSION_EXPECTED_MEMBER_THREAD_ID"} {
			if _, ok := values[key]; ok {
				t.Fatal("monitor passed member authority", key)
			}
		}
		request, err := decodeMonitorRequest([]byte(inv.message))
		if err != nil || !reflect.DeepEqual(request.Start.Source.Argv, []string{"printf", "two words", "", "$HOME; | *"}) {
			t.Fatal("monitor changed argv boundaries", err)
		}
		return json.NewEncoder(inv.receipt).Encode(monitoroperation.NativeStatus{ExecutionOwner: "native", Operation: monitoroperation.Operation{
			Schema: 1, ID: monitorFixtureID, Workspace: binding.workspace, Owner: monitoroperation.Owner{Slug: "test-session", RootThreadID: "lead-thread"}}})
	}
	// This simulates a continued conversation at the adapter, not a native
	// compact. Only the native thread metadata is authority across both turns.
	for _, meta := range []string{`{"threadId":"lead-thread","turnId":"initial-turn","context":"initial conversation"}`, `{"threadId":"lead-thread","turnId":"continuation-turn","context":"different summarized conversation mentions member-thread"}`} {
		if result := monitorRPCCall(t, binding, raw, meta, false, run); result["isError"] != false {
			t.Fatal(result)
		}
	}
	if calls != 2 {
		t.Fatalf("expected two exact host invocations, got %d", calls)
	}

	// Simulate a reader that cannot prove the complete inventory. It must
	// never dispatch using the otherwise valid partial inventory.
	if err := os.WriteFile(filepath.Join(archive, "portal.yml"), []byte(strings.Replace(manifest, "schema: 4", "schema: 999", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	monitorRPCError(t, monitorRPCCall(t, binding, raw, `{"threadId":"lead-thread"}`, false, run),
		"Monitor session inventory could not be verified. No monitor action was submitted. Check the reporting helper against the selected package.")
	if calls != 2 {
		t.Fatal("incomplete inventory reached the host")
	}
}

func TestMonitorMCPRefusesBeforeDispatchAndPreservesUncertainty(t *testing.T) {
	binding := monitorLeadFixture(t)
	raw := monitorLocalStart()
	const lead = `{"threadId":"lead-thread"}`
	const metadata = "Valid native caller metadata is required."
	const unowned = "This caller does not own the requested monitor action."
	const invalid = "Invalid monitor start. Use source kind local or ci with the required fields. No monitor action was submitted."
	for _, test := range []struct {
		name, meta, old, replacement, want string
		legacy                             bool
	}{
		{name: "missing metadata", want: metadata},
		{name: "conflicting native threads", meta: `{"threadId":"lead-thread","threadId":"member-thread"}`, want: metadata},
		{name: "invalid native thread", meta: `{"threadId":7}`, want: metadata},
		{name: "member", meta: `{"threadId":"member-thread"}`, want: unowned},
		{name: "retained legacy helper", meta: lead, legacy: true, want: "Legacy member reporting cannot manage monitor operations."},
		{name: "wrong kind", meta: lead, old: `"local"`, replacement: `"fixture-unsupported-kind"`, want: invalid},
		{name: "empty command", meta: lead, old: `"printf"`, replacement: `""`, want: invalid},
		{name: "different invocation ID", meta: lead, old: `"invocationId":"` + monitorFixtureID + `"`, replacement: `"invocationId":"22222222222222222222222222222222"`, want: invalid},
		{name: "wrong argv shape", meta: lead, old: `["printf","two words","","$HOME; | *"]`, replacement: `"fixture-argv"`, want: "Invalid monitor operation request."},
		{name: "extra source field", meta: lead, old: `"kind":"local"`, replacement: `"kind":"local","fixture-extra":"fixture-value"`, want: "Invalid monitor operation request."},
		{name: "invalid source does not authenticate a caller", meta: `{"threadId":"unknown-thread"}`, old: `"local"`, replacement: `"fixture-unsupported-kind"`, want: unowned},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := raw
			if test.old != "" {
				args = json.RawMessage(strings.Replace(string(raw), test.old, test.replacement, 1))
			}
			monitorRPCError(t, monitorRPCCall(t, binding, args, test.meta, test.legacy, func(context.Context, teamMCPInvocation) error {
				t.Fatal("pre-dispatch rejection invoked the host")
				return nil
			}), test.want)
		})
	}
	calls := 0
	monitorRPCError(t, monitorRPCCall(t, binding, raw, lead, false, func(context.Context, teamMCPInvocation) error {
		calls++
		return errors.New("fixture-private-child-error")
	}), "Monitor action was not confirmed. Inspect status and retry the unchanged start or report; do not launch another watcher or rerun the command.")
	if calls != 1 {
		t.Fatal("uncertain reply did not reach host exactly once")
	}
}

func TestMonitorMCPBindsRetainedOperationToOriginalOwner(t *testing.T) {
	binding := monitorLeadFixture(t)
	ctx := context.Background()
	store, err := monitoroperation.NewStore(binding.stateRoot, binding.workspace)
	if err != nil {
		t.Fatal(err)
	}
	// Seed an actual operation through the runtime-CI service. There is no
	// worker, network observer, native utility or external effect in this test.
	resolve := func(slug string) (monitoroperation.Owner, error) {
		owner, err := monitoroperation.IdentityFor(binding.workspace, slug)
		owner.Socket = filepath.Join(binding.workspace, "native.sock")
		owner.OriginTurnID, owner.SocketEpoch, owner.RuntimeSchema = "initial-turn", "fixture-epoch", 1
		return owner, err
	}
	service := monitoroperation.Service{
		Store: store, Resolve: resolve, ResolveLead: func(_ context.Context, thread string) (monitoroperation.Owner, error) {
			owner, err := monitoroperation.ResolveLead(binding.workspace, thread)
			if err != nil {
				return owner, err
			}
			return resolve(owner.Slug)
		},
		LockSession: func(context.Context, string) (func(), error) { return func() {}, nil },
		Admit:       func(context.Context, monitoroperation.Owner) error { return nil },
	}
	const lead = `{"threadId":"lead-thread"}`
	var op monitoroperation.ObserverRecord
	for _, loseReply := range []bool{true, false} {
		result := monitorRPCCall(t, binding, monitorCIStart(), lead, false, func(ctx context.Context, inv teamMCPInvocation) error {
			request, err := decodeMonitorRequest([]byte(inv.message))
			if err != nil {
				return err
			}
			ack, err := service.StartManaged(ctx, "lead-thread", request.Start)
			if err != nil {
				return err
			}
			observer, ok := ack.(monitoroperation.ObserverStatus)
			if !ok || observer.ExecutionOwner != "runtime-ci" {
				t.Fatal("expected a direct runtime acknowledgement")
			}
			if loseReply {
				op = observer.ObserverRecord
				return errors.New("lost fixture acknowledgement after registration")
			}
			if !reflect.DeepEqual(observer.ObserverRecord, op) {
				t.Fatal("unchanged retry did not retain the original observer")
			}
			return json.NewEncoder(inv.receipt).Encode(observer)
		})
		if result["isError"] != loseReply {
			t.Fatal("unexpected runtime-CI start result", result)
		}
	}
	if observers, err := store.ListObservers(ctx, "test-session"); err != nil || len(observers) != 1 || observers[0].ID != op.ID {
		t.Fatal("expected one registered observer across the lost confirmation", err)
	}
	data, err := os.ReadFile(filepath.Join(binding.workDir, "portal.yml"))
	if err != nil {
		t.Fatal(err)
	}
	writeRoot := func(dir, slug, thread string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		manifest := strings.NewReplacer("slug: test-session", "slug: "+slug, "thread_id: lead-thread", "thread_id: "+thread).Replace(string(data))
		for name, value := range map[string]string{"portal.yml": manifest, "plan.md": "synthetic owner", "state.md": "---\nlifecycle: active\n---\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	foreign := filepath.Join(binding.workspace, "work", "foreign-session")
	writeRoot(foreign, "foreign-session", "foreign-thread")
	const denied = "This caller does not own the requested monitor action."
	const other = `{"threadId":"foreign-thread"}`
	status := json.RawMessage(`{"action":"status","id":"` + monitorFixtureID + `"}`)
	refused := func(meta string, raw json.RawMessage) {
		t.Helper()
		monitorRPCError(t, monitorRPCCall(t, binding, raw, meta, false, func(context.Context, teamMCPInvocation) error {
			t.Fatal("unproven owner reached host")
			return nil
		}), denied)
	}
	if result := monitorRPCCall(t, binding, status, lead, false, func(_ context.Context, inv teamMCPInvocation) error {
		observed, err := service.ObserverStatus(ctx, "lead-thread", op.ID)
		if err != nil {
			return err
		}
		return json.NewEncoder(inv.receipt).Encode(monitoroperation.ObserverStatus{ObserverRecord: observed, ExecutionOwner: "runtime-ci"})
	}); result["isError"] != false {
		t.Fatal("original owner could not inspect registered operation", result)
	}
	const unconfirmed = "Monitor action was not confirmed. Inspect status and retry the unchanged start or report; do not launch another watcher or rerun the command."
	rejectedByService := func(meta, wantCaller string, raw json.RawMessage) {
		t.Helper()
		calls := 0
		result := monitorRPCCall(t, binding, raw, meta, false, func(ctx context.Context, inv teamMCPInvocation) error {
			calls++
			var caller string
			for _, entry := range inv.environment {
				if key, value, _ := strings.Cut(entry, "="); key == "DEV_SESSION_EXPECTED_MONITOR_THREAD_ID" {
					caller = value
				}
			}
			if caller != wantCaller {
				t.Fatal("host did not receive the exact native caller")
			}
			request, err := decodeMonitorRequest([]byte(inv.message))
			if err != nil {
				t.Fatal(err)
			}
			switch request.Action {
			case "start":
				_, err = service.StartManaged(ctx, caller, request.Start)
			case "status":
				_, err = service.ObserverStatus(ctx, caller, request.ID)
			case "cancel":
				_, err = service.CancelCIObservation(ctx, caller, request.ID)
			default:
				t.Fatal("unexpected monitor action", request.Action)
			}
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("service did not refuse action on the existing foreign operation: %v", err)
			}
			return err
		})
		monitorRPCError(t, result, unconfirmed)
		if calls != 1 {
			t.Fatalf("expected one dispatch to the real service, got %d", calls)
		}
	}
	for _, raw := range []json.RawMessage{
		monitorCIStart(), status, json.RawMessage(`{"action":"cancel","id":"` + monitorFixtureID + `"}`),
	} {
		rejectedByService(other, "foreign-thread", raw)
	}
	refused(lead, json.RawMessage(`{"action":"report","slot":"terminal","outcome":"passed","exitStatus":0}`))

	// Missing and foreign IDs have the same tool result for the other lead.
	unknown := json.RawMessage(`{"action":"status","id":"33333333333333333333333333333333"}`)
	monitorRPCError(t, monitorRPCCall(t, binding, unknown, other, false, func(context.Context, teamMCPInvocation) error {
		_, err := service.ObserverStatus(ctx, "foreign-thread", "33333333333333333333333333333333")
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal("expected a missing operation", err)
		}
		return err
	}), unconfirmed)

	// An unrelated unreadable manifest blocks fresh lead authority but does
	// not block receipt recovery for the exact retained owner and operation.
	if err := os.WriteFile(filepath.Join(foreign, "portal.yml"), []byte("schema: 999\nslug: foreign-session\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := monitorRPCCall(t, binding, status, lead, false, func(_ context.Context, inv teamMCPInvocation) error {
		recovery, err := monitorRecoveryOwner(binding.workspace, monitoroperation.Operation{Workspace: op.Workspace, Owner: op.Owner}, "lead-thread")
		if err != nil || !monitoroperation.SameOwner(recovery, op.Owner) {
			t.Fatal("exact retained owner could not recover without complete inventory", err)
		}
		return json.NewEncoder(inv.receipt).Encode(monitoroperation.ObserverStatus{ObserverRecord: op, ExecutionOwner: "runtime-ci"})
	}); result["isError"] != false {
		t.Fatal("incomplete inventory masked exact retained receipt recovery", result)
	}
	monitorRPCError(t, monitorRPCCall(t, binding, status, other, false, func(context.Context, teamMCPInvocation) error {
		t.Fatal("unproven recovery caller reached host")
		return nil
	}), "Monitor session inventory could not be verified. No monitor action was submitted. Check the reporting helper against the selected package.")

	writeRoot(foreign, "foreign-session", "lead-thread")
	refused(lead, monitorLocalStart()) // two tracking roots, neither can act
	refused(lead, status)              // no retained receipt recovery fallback
	unreadable := filepath.Join(binding.workspace, "work", "unreadable-session")
	if err := os.Mkdir(unreadable, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unreadable, "portal.yml"), []byte("schema: 999\nslug: unreadable-session\n"), 0600); err != nil {
		t.Fatal(err)
	}
	refused(lead, status) // a known duplicate still blocks exact retained recovery
	if err := os.Remove(filepath.Join(unreadable, "portal.yml")); err != nil {
		t.Fatal(err)
	}
	writeRoot(foreign, "foreign-session", "foreign-thread")
	writeRoot(binding.workDir, "test-session", "replacement-thread")
	refused(lead, monitorLocalStart())
	refused(lead, status)
	rejectedByService(`{"threadId":"replacement-thread"}`, "replacement-thread", status)
	if after, err := store.GetObserver(ctx, op.ID); err != nil || !reflect.DeepEqual(after, op) {
		t.Fatal("rejected calls changed operation or delivery", err)
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

func monitorCIStart() json.RawMessage {
	return json.RawMessage(`{"action":"start","id":"` + monitorFixtureID + `","brief":"exact CI","source":{"kind":"ci","provider":"github","repository":"example/project","workflowId":"12","runId":"34","runAttempt":1,"commit":"` + strings.Repeat("a", 40) + `"}}`)
}

func TestMonitorMCPAcknowledgesDirectOwnerWithoutNativeLaunch(t *testing.T) {
	binding := monitorLeadFixture(t)
	const id = monitorFixtureID
	raw := monitorCIStart()
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
	raw := monitorLocalStart()
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

func TestMonitorMalformedUtilityRequestsExplainSelectorsWithoutDispatch(t *testing.T) {
	binding := monitorLeadFixture(t)
	for _, tc := range []struct{ raw, hint string }{
		{`{"action":"status","operationId":"` + monitorFixtureID + `"}`, "Do not use operationId"},
		{`{"action":"report","id":"` + monitorFixtureID + `","slot":"terminal","outcome":"incomplete","detail":"not launched"}`, "Omit id, operationId"},
		{`{"action":"report","operationId":"` + monitorFixtureID + `","slot":"terminal","outcome":"incomplete","detail":"not launched"}`, "Omit id, operationId"},
	} {
		result := monitorRPCCall(t, binding, json.RawMessage(tc.raw), `{"threadId":"utility-thread"}`, false, func(context.Context, teamMCPInvocation) error { t.Fatal("malformed selector reached host"); return nil })
		encoded, err := json.Marshal(result)
		if err != nil || result["isError"] != true || !strings.Contains(string(encoded), tc.hint) {
			t.Fatal("missing actionable selector rejection", result, err)
		}
	}
	if _, err := decodeMonitorRequest([]byte(`{"action":"report","slot":"terminal","outcome":"incomplete","detail":"Canceled before command launch","evidence":["no source attempt; cells joined"]}`)); err != nil {
		t.Fatal(err)
	}
}
