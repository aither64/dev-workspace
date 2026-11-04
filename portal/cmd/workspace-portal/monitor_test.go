package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestMonitorMCPRoutesExactLeadWithoutMemberAuthority(t *testing.T) {
	binding := monitorLeadFixture(t)
	const id = "11111111111111111111111111111111"
	var captured teamMCPInvocation
	called := 0
	run := func(_ context.Context, invocation teamMCPInvocation) error {
		called++
		captured = invocation
		return json.NewEncoder(invocation.receipt).Encode(monitoroperation.Operation{Schema: 1, ID: id, Workspace: binding.workspace,
			Owner: monitoroperation.Owner{Slug: "test-session", RootThreadID: "lead-thread"}})
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
