package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
)

func monitorMCPTool() map[string]any {
	text := func() map[string]any { return map[string]any{"type": "string"} }
	properties := map[string]any{
		"action": map[string]any{"type": "string", "enum": []string{"start", "report", "status", "cancel", "resolve"}},
		"id":     text(), "brief": text(), "cancellationScope": text(),
		"operationId": text(), "resolutionId": text(),
		"source": map[string]any{"type": "object", "properties": map[string]any{
			"kind": text(), "argv": map[string]any{"type": "array", "items": text()}, "cwd": text(),
			"inputIdentity": text(), "revision": text(), "invocationId": text(),
			"environmentReferences": map[string]any{"type": "array", "items": text()},
			"provider":              text(), "repository": text(), "workflowId": text(), "runId": text(),
			"runAttempt": map[string]any{"type": "integer", "minimum": 1}, "commit": text(),
		}, "required": []string{"kind"}, "additionalProperties": false},
		"slot":       map[string]any{"type": "string", "enum": []string{"escalation", "terminal"}},
		"outcome":    map[string]any{"type": "string", "enum": []string{"passed", "failed", "incomplete"}},
		"exitStatus": map[string]any{"type": "integer"}, "conclusion": text(),
		"elapsedMillis": map[string]any{"type": "integer", "minimum": 0},
		"evidence":      map[string]any{"type": "array", "items": text()}, "stillRunning": text(), "detail": text(),
	}
	return map[string]any{"name": "monitor_operation",
		"description": "Start one runtime-owned verification watcher, report its exact outcome, inspect status, cancel continuation, or resolve external effects with exact evidence. Start/cancel/resolve only as the current lead; report only as its registered utility. Retry unchanged requests with the same id. Cancel/resolve never interrupt commands or send a wakeup.",
		"inputSchema": map[string]any{"type": "object", "properties": properties,
			"required": []string{"action"}, "additionalProperties": false}}
}

func (binding teamMCPBinding) monitor(ctx context.Context, raw, meta json.RawMessage, run func(context.Context, teamMCPInvocation) error) any {
	if binding.slug != "" {
		return teamMCPToolResult("Legacy member reporting cannot manage monitor operations.", true)
	}
	thread, err := teamMCPNativeThread(meta)
	if err != nil {
		return teamMCPToolResult("Native caller identity is required.", true)
	}
	request, err := decodeMonitorRequest(raw)
	if err != nil {
		return teamMCPToolResult("Invalid monitor operation request.", true)
	}
	store, err := monitoroperation.NewStore(binding.stateRoot, binding.workspace)
	if err != nil {
		return teamMCPToolResult("Monitor operation state is unavailable.", true)
	}
	owner, err := monitorCaller(ctx, store, binding.workspace, thread, request)
	if err != nil {
		return teamMCPToolResult("This caller does not own the requested monitor action.", true)
	}
	binding.slug, binding.rootThreadID = owner.Slug, owner.RootThreadID
	binding.workDir = filepath.Join(binding.workspace, "work", owner.Slug)
	invocation := binding.invocation("", "")
	invocation.args = []string{"monitor", request.Action, owner.Slug, "--as-is", "--request-stdin", "--json"}
	environment := []string{}
	for _, entry := range invocation.environment {
		if !strings.HasPrefix(entry, "DEV_SESSION_MEMBER_ADDRESS=") && !strings.HasPrefix(entry, "DEV_SESSION_EXPECTED_MEMBER_THREAD_ID=") {
			environment = append(environment, entry)
		}
	}
	invocation.environment = append(environment, "DEV_SESSION_EXPECTED_MONITOR_THREAD_ID="+thread)
	invocation.message, invocation.receipt, invocation.outputLimit = string(raw), new(bytes.Buffer), 1024*1024
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := run(bounded, invocation); err != nil {
		return teamMCPToolResult("Monitor action was not confirmed. Inspect status and retry the unchanged start or report; do not launch another watcher or rerun the command.", true)
	}
	var operation monitoroperation.Operation
	if teamMCPDecode(invocation.receipt.Bytes(), &operation) != nil || operation.Schema != monitoroperation.Schema ||
		operation.Workspace != binding.workspace || operation.Owner.Slug != owner.Slug || operation.Owner.RootThreadID != owner.RootThreadID ||
		(request.Action == "start" && operation.ID != request.Start.ID) ||
		((request.Action == "status" || request.Action == "cancel") && request.ID != "" && operation.ID != request.ID) ||
		(request.Action == "status" && request.ID == "" && operation.Launch.ThreadID != thread) ||
		(request.Action == "report" && operation.Launch.ThreadID != thread) ||
		(request.Action == "resolve" && operation.ID != request.Resolution.OperationID) {
		return teamMCPToolResult("Monitor acknowledgement could not be verified. Inspect status before retrying.", true)
	}
	return teamMCPToolResult(strings.TrimSpace(invocation.receipt.String()), false)
}
