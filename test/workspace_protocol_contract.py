#!/usr/bin/env python3
"""Focused generated-contract proof for local semantic latest-turn reads."""

import json
import pathlib
import sys

from jsonschema import Draft7Validator


def validate(schema_directory):
    request = {"id": 1, "method": "thread/turns/list", "params": {
        "threadId": "019f1351-8000-7000-8000-000000000001",
        "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
    }}
    response_schema = json.loads((schema_directory / "v2/ThreadTurnsListResponse.json").read_text())
    request_schema = json.loads((schema_directory / "ClientRequest.json").read_text())
    Draft7Validator(request_schema).validate(request)
    provider_params = {"threadId": "thread-1", "excludeTurns": True,
        "modelProvider": "pool", "model": "retained", "cwd": "/workspace/work/example",
        "runtimeWorkspaceRoots": ["/workspace"], "approvalPolicy": "never",
        "approvalsReviewer": "user", "sandbox": "workspace-write",
        "config": {"model_reasoning_effort": "low",
            "sandbox_workspace_write": {"writable_roots": ["/workspace"], "network_access": True},
            "shell_environment_policy": {"set": {"DEV_SESSION_SLUG": "example"}}}}
    Draft7Validator(request_schema).validate({"id": 2, "method": "thread/resume", "params": provider_params})
    Draft7Validator(request_schema).validate({"id": 3, "method": "thread/settings/update",
        "params": {"threadId": "thread-1", "disabledPluginIds": [],
            "multiAgentMode": "explicitRequestOnly", "collaborationMode": {
                "mode": "default", "settings": {"model": "retained",
                    "reasoning_effort": "low", "developer_instructions": None}}}})
    Draft7Validator(request_schema).validate({"id": 4, "method": "mcpServerStatus/list",
        "params": {"threadId": "thread-1", "serverName": "team_reporting"}})
    Draft7Validator(request_schema).validate({"id": 5, "method": "mcpServer/tool/call",
        "params": {"threadId": "thread-1", "server": "team_reporting", "tool": "report_to_lead",
            "arguments": {"message": "fixture report", "message_id": "a" * 32},
            "_meta": {"threadId": "native-overwrites-this-value"}}})
    resume = json.loads((schema_directory / "v2/ThreadResumeResponse.json").read_text())
    for field in ("modelProvider", "model", "cwd", "approvalPolicy", "approvalsReviewer", "sandbox"):
        if field not in resume.get("required", []):
            raise ValueError(f"provider transition requires effective resume field {field}")
    turn_properties = response_schema.get("definitions", {}).get("Turn", {}).get("properties", {})
    for field in ("id", "status", "startedAt", "completedAt"):
        if field not in turn_properties:
            raise ValueError(f"semantic observation requires declared Turn.{field}")
    validator = Draft7Validator(response_schema)
    validator.validate({"data": [], "nextCursor": None, "backwardsCursor": None})
    for status in ("completed", "failed", "interrupted", "inProgress"):
        for historical in (False, True):
            turn = {"id": "019f1351-8000-7000-8000-000000000002", "items": [], "itemsView": "notLoaded",
                    "status": status, "error": None, "startedAt": None if historical else 1789387200,
                    "completedAt": None if historical or status == "inProgress" else 1789387201,
                    "durationMs": None}
            validator.validate({"data": [turn], "nextCursor": None, "backwardsCursor": "opaque"})


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: workspace_protocol_contract.py SELECTED_SCHEMA_DIRECTORY")
    validate(pathlib.Path(sys.argv[1]))
    print("local semantic observation protocol contract passed")
