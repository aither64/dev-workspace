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
		"description": "Observe an exact existing GitHub CI run using bounded runtime reads, or start a native local-command watcher. Verify executionOwner in the acknowledgement; runtime-ci has no native launch. Start/cancel/resolve require the exact lead. Registered utilities may report and inspect their own native operation. Resolve requires external-effects evidence. Cancel suppresses unsent continuation without interrupting commands or remote CI. Retry unchanged requests with the same identities.",
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
	var header struct {
		ExecutionOwner string `json:"executionOwner"`
		Driver         string `json:"driver"`
	}
	if json.Unmarshal(invocation.receipt.Bytes(), &header) != nil {
		return teamMCPToolResult("Monitor acknowledgement could not be verified. Inspect status before retrying.", true)
	}
	var operation monitoroperation.Operation
	valid := false
	if header.ExecutionOwner == "runtime-ci" {
		var direct monitoroperation.ObserverStatus
		if teamMCPDecode(invocation.receipt.Bytes(), &direct) == nil {
			operation = monitoroperation.Operation{Schema: direct.Schema, ID: direct.ID, Workspace: direct.Workspace, Owner: direct.Owner}
			valid = header.ExecutionOwner == "runtime-ci" && direct.Driver == "direct-ci" && (request.Action == "start" || request.Action == "status" && request.ID != "" || request.Action == "cancel")
		}
	} else if header.ExecutionOwner == "native" {
		var native monitoroperation.NativeStatus
		if teamMCPDecode(invocation.receipt.Bytes(), &native) == nil {
			operation = native.Operation
			valid = true
		}
	}
	if !valid || operation.Schema != monitoroperation.Schema ||
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
