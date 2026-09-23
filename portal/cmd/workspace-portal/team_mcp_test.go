package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func teamMCPFixture(t *testing.T) (teamMCPBinding, teamMCPDependencies, []string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	stateRoot := filepath.Join(root, "state")
	slug := "test-session"
	workDir := filepath.Join(workspace, "work", slug)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	packageBin := filepath.Join(root, "package", "bin")
	if err := os.MkdirAll(packageBin, 0o700); err != nil {
		t.Fatal(err)
	}
	devSession := filepath.Join(packageBin, "dev-session")
	if err := os.WriteFile(devSession, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := teamruntime.NewStore(stateRoot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), slug, "lead-thread", true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{{Address: "reviewer0", Role: "reviewer", Index: 0,
			Thread: "member-thread", State: "ready", AddedAt: time.Now().UTC(),
			Behavior: "reviewer", Access: "read_only"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := teamMCPBinding{stateRoot: stateRoot, workspace: workspace, slug: slug,
		rootThreadID: "lead-thread", address: "reviewer0", memberThreadID: "member-thread",
		devSession: devSession, workDir: workDir, store: store}
	args := []string{"--user-state-root", stateRoot, "--workspace", workspace,
		"--session-slug", slug, "--root-thread-id", "lead-thread",
		"--member-address", "reviewer0", "--member-thread-id", "member-thread"}
	deps := teamMCPDependencies{executable: func() (string, error) { return filepath.Join(packageBin, "workspace-portal"), nil }}
	return binding, deps, args
}

func teamMCPRequestLine(t *testing.T, id int, method string, params any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func teamMCPResponses(t *testing.T, output string) []map[string]any {
	t.Helper()
	var responses []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("non-protocol stdout %q: %v", line, err)
		}
		responses = append(responses, response)
	}
	return responses
}

func teamMCPResult(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing MCP result: %#v", response)
	}
	return result
}

func TestTeamMCPProtocolAndBoundHostInvocation(t *testing.T) {
	binding, deps, args := teamMCPFixture(t)
	t.Setenv("DEV_SESSION_SLUG", "foreign-session")
	t.Setenv("DEV_SESSION_MEMBER_ADDRESS", "implementer0")
	t.Setenv("DEV_WORKSPACES_STATE", "/foreign/state")
	var invocations []teamMCPInvocation
	deps.run = func(_ context.Context, invocation teamMCPInvocation) error {
		invocations = append(invocations, invocation)
		return nil
	}
	const id = "12345678-1234-1234-1234-123456789abc"
	input := teamMCPRequestLine(t, 1, "initialize", map[string]any{"protocolVersion": teamMCPProtocolVersion}) +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		teamMCPRequestLine(t, 2, "tools/list", map[string]any{}) +
		teamMCPRequestLine(t, 3, "tools/call", map[string]any{"name": "report_to_lead", "arguments": map[string]any{
			"message": "Review is complete.", "message_id": id}})
	var output, diagnostics bytes.Buffer
	if err := teamMCPCommand(args, strings.NewReader(input), &output, &diagnostics, deps); err != nil {
		t.Fatal(err)
	}
	responses := teamMCPResponses(t, output.String())
	if len(responses) != 3 || diagnostics.Len() != 0 {
		t.Fatalf("responses = %#v, diagnostics = %q", responses, diagnostics.String())
	}
	if teamMCPResult(t, responses[0])["protocolVersion"] != teamMCPProtocolVersion {
		t.Fatalf("initialize result = %#v", responses[0])
	}
	tools, ok := teamMCPResult(t, responses[1])["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tool list = %#v", responses[1])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "report_to_lead" {
		t.Fatalf("wrong tool = %#v", tool)
	}
	properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if len(properties) != 2 || properties["message"] == nil || properties["message_id"] == nil {
		t.Fatalf("tool exposed unexpected input = %#v", properties)
	}
	if teamMCPResult(t, responses[2])["isError"] != false || len(invocations) != 1 {
		t.Fatalf("report was not invoked: %#v, %#v", responses[2], invocations)
	}
	invocation := invocations[0]
	if invocation.command != binding.devSession || invocation.workDir != binding.workDir ||
		strings.Join(invocation.args, "\x00") != strings.Join([]string{"team", "assign", binding.slug, "--as-is",
			"--from", binding.address, "--to", "lead", "--message-stdin", "--message-id", id}, "\x00") ||
		invocation.message != "Review is complete." {
		t.Fatalf("host invocation = %#v", invocation)
	}
	environment := map[string]string{}
	for _, entry := range invocation.environment {
		key, value, _ := strings.Cut(entry, "=")
		environment[key] = value
	}
	for key, want := range map[string]string{
		"DEV_WORKSPACES_STATE":                binding.stateRoot,
		"DEV_SESSION_SLUG":                    binding.slug,
		"DEV_SESSION_WORKSPACE":               binding.workspace,
		"DEV_SESSION_WORK_DIR":                binding.workDir,
		"DEV_SESSION_MEMBER_ADDRESS":          binding.address,
		"DEV_SESSION_EXPECTED_ROOT_THREAD_ID": binding.rootThreadID,
	} {
		if environment[key] != want {
			t.Fatalf("%s = %q, want %q", key, environment[key], want)
		}
	}
}

func TestTeamMCPRejectsInvalidCallsAndChangedBinding(t *testing.T) {
	binding, deps, args := teamMCPFixture(t)
	called := 0
	deps.run = func(context.Context, teamMCPInvocation) error { called++; return nil }
	requests := []string{
		teamMCPRequestLine(t, 1, "tools/call", map[string]any{"name": "other", "arguments": map[string]any{"message": "hello", "message_id": strings.Repeat("a", 32)}}),
		teamMCPRequestLine(t, 2, "tools/call", map[string]any{"name": "report_to_lead", "arguments": map[string]any{"message": "hello", "message_id": strings.Repeat("a", 32), "to": "lead"}}),
		teamMCPRequestLine(t, 3, "tools/call", map[string]any{"name": "report_to_lead", "arguments": map[string]any{"message": "hello", "message_id": "unstable"}}),
		teamMCPRequestLine(t, 4, "tools/call", map[string]any{"name": "report_to_lead", "arguments": map[string]any{"message": strings.Repeat("x", teamMCPMaxMessage+1), "message_id": strings.Repeat("a", 32)}}),
	}
	var output, diagnostics bytes.Buffer
	if err := teamMCPCommand(args, strings.NewReader(strings.Join(requests, "")), &output, &diagnostics, deps); err != nil {
		t.Fatal(err)
	}
	for _, response := range teamMCPResponses(t, output.String()) {
		if teamMCPResult(t, response)["isError"] != true {
			t.Fatalf("invalid call succeeded: %#v", response)
		}
	}
	if called != 0 {
		t.Fatalf("invalid calls invoked host %d times", called)
	}
	_, err := binding.store.Update(context.Background(), binding.slug, binding.rootThreadID, false, func(roster *teamruntime.Roster) error {
		roster.Members[0].Thread = "another-thread"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	call := teamMCPRequestLine(t, 5, "tools/call", map[string]any{"name": "report_to_lead", "arguments": map[string]any{
		"message": "do not send", "message_id": strings.Repeat("a", 32)}})
	if err := teamMCPCommand(args, strings.NewReader(call), &output, &diagnostics, deps); err != nil {
		t.Fatal(err)
	}
	if teamMCPResult(t, teamMCPResponses(t, output.String())[0])["isError"] != true || called != 0 {
		t.Fatalf("changed thread was accepted: %s", output.String())
	}
}

func TestTeamMCPDeliveryFailureKeepsMessagePrivateAndRetryIDStable(t *testing.T) {
	_, deps, args := teamMCPFixture(t)
	const message = "private report text"
	const id = "abcdefabcdefabcdefabcdefabcdefab"
	var seen []string
	deps.run = func(_ context.Context, invocation teamMCPInvocation) error {
		seen = append(seen, invocation.args[len(invocation.args)-1])
		if strings.Join(invocation.args, " ") == "" || strings.Contains(strings.Join(invocation.args, " "), message) || invocation.message != message {
			t.Fatal("report body was not confined to child stdin")
		}
		return errors.New("failure containing " + message)
	}
	call := teamMCPRequestLine(t, 1, "tools/call", map[string]any{"name": "report_to_lead", "arguments": map[string]any{
		"message": message, "message_id": id}})
	var output, diagnostics bytes.Buffer
	if err := teamMCPCommand(args, strings.NewReader(call+call), &output, &diagnostics, deps); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != id || seen[1] != id {
		t.Fatalf("retry IDs = %#v", seen)
	}
	if strings.Contains(output.String(), message) || strings.Contains(diagnostics.String(), message) {
		t.Fatalf("message leaked to MCP output or diagnostics")
	}
	for _, response := range teamMCPResponses(t, output.String()) {
		if teamMCPResult(t, response)["isError"] != true {
			t.Fatalf("failed delivery reported success: %#v", response)
		}
	}
}

func TestTeamMCPRejectsMissingIdentityAndOversizedInput(t *testing.T) {
	_, deps, args := teamMCPFixture(t)
	var output, diagnostics bytes.Buffer
	if err := teamMCPCommand(args[:len(args)-2], strings.NewReader(""), &output, &diagnostics, deps); err == nil {
		t.Fatal("missing member thread binding was accepted")
	}
	input := strings.Repeat("x", teamMCPMaxLineBytes+1) + "\n"
	if err := teamMCPCommand(args, strings.NewReader(input), &output, &diagnostics, deps); err == nil {
		t.Fatal("oversized MCP frame was accepted")
	}
}

func TestTeamMCPChildOutputNeverEntersProtocol(t *testing.T) {
	binding, deps, args := teamMCPFixture(t)
	if err := os.WriteFile(binding.devSession,
		[]byte("#!/bin/sh\nfor arg in \"$@\"; do [ \"$arg\" = report ] && exit 9; done\n[ \"$(cat)\" = report ] || exit 8\nprintf 'host stdout\\n'\nprintf 'host stderr\\n' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	call := teamMCPRequestLine(t, 1, "tools/call", map[string]any{"name": "report_to_lead", "arguments": map[string]any{
		"message": "report", "message_id": strings.Repeat("a", 32)}})
	var output, diagnostics bytes.Buffer
	if err := teamMCPCommand(args, strings.NewReader(call), &output, &diagnostics, deps); err != nil {
		t.Fatal(err)
	}
	responses := teamMCPResponses(t, output.String())
	if len(responses) != 1 || teamMCPResult(t, responses[0])["isError"] != false ||
		strings.Contains(output.String(), "host stdout") || strings.Contains(diagnostics.String(), "host stderr") {
		t.Fatalf("host output escaped into MCP streams: %q, %q", output.String(), diagnostics.String())
	}
}
