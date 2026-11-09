"""Transparent native transport fault and read-only proof, never RPC stubs.

The proxy holds the actual successful turn/start response after Codex accepted
the turn. Killing the portal at this barrier exercises an ambiguous initial send.
"""
import asyncio
import hashlib
import itertools
import sqlite3
import json
import pathlib
import os
import pwd
import runpy
import signal
import stat
import subprocess
import sys
import tempfile
import time
import tomllib
import uuid
import shlex


class TuiObservation:
    """Bounded fixture-only journal; observation never changes forwarding."""
    def __init__(self, directory):
        self.directory = pathlib.Path(directory)
        self.sequence = 0
        self.bytes = 0
        self.overflow = False
        self.journal_name, self.label = "tui-rpc.jsonl", "TUI"

    def rule(self):
        path = self.directory / "observe-tui.json"
        if not path.exists():
            return None
        rule = json.loads(path.read_text())
        assert set(rule) == {"threadId", "markers"}
        assert isinstance(rule["threadId"], str) and rule["threadId"]
        assert 1 <= len(rule["markers"]) <= 2 and all(
            isinstance(value, str) and value.startswith("CREATION_TUI_") for value in rule["markers"])
        return rule

    def append(self, connection, direction, message):
        if self.overflow:
            return
        row = {"connection": connection, "sequence": self.sequence,
               "atUnix": time.time(), "direction": direction, **message}
        data = json.dumps(row).encode() + b"\n"
        if self.sequence >= 256 or self.bytes + len(data) > 1024 * 1024:
            self.overflow = True
            data = json.dumps({"observationError": self.label + " trace bound exceeded"}).encode() + b"\n"
        with (self.directory / self.journal_name).open("ab") as journal:
            journal.write(data)
        self.bytes += len(data)
        self.sequence += 1


def materialization_fields(value):
    fields = ("model", "modelProvider", "reasoningEffort", "effort", "cwd", "threadId",
              "id", "projectId", "parentThreadId", "ephemeral")
    assert isinstance(value, dict)
    assert all(value.get(key) is None or type(value[key]) in (str, int, bool) for key in fields)
    assert all(not isinstance(value.get(key), str) or len(value[key]) <= 4096 for key in fields)
    return {key: {"present": key in value, "value": value.get(key),
                  "state": "unknown" if value.get(key) is None else "reported"} for key in fields}


def ready_identity(authorities):
    """Bind status to CLI completion and the actual ready manifest, never aliases."""
    status, receipt, complete, manifest, expected, tracking = (
        authorities[key] for key in ("status", "receipt", "completion", "manifest", "expected", "tracking"))
    assert all(isinstance(value, dict) for value in (status, receipt, complete, manifest, expected, tracking)), "missing ready authority"
    assert status["state"] == receipt["state"] == "ready" and receipt["validated"] is True, "nonready authority"
    assert status["requestId"] == expected["requestId"] and status["receiptId"] == expected["receiptId"], "wrong admission"
    assert status["slug"] == receipt["request"]["slug"] == manifest["slug"] == expected["slug"], "wrong slug"
    assert receipt["schema"] in (1, 3) and complete["schema"] == receipt["schema"], "unsupported completion schema"
    assert receipt["workspace"] == complete["workspace"] == expected["workspace"], "wrong workspace"
    assert receipt["receiptId"] == complete["receiptId"] == expected["receiptId"] and complete["slug"] == expected["slug"], "wrong completion receipt"
    assert receipt["request"]["kind"] == "new" and not complete["sourceThreadId"], "wrong creation kind"
    for key in ("model", "effort"):
        assert receipt[key] == complete[key] == expected[key], "changed selected pair"
    digest = hashlib.sha256(receipt["goal"].encode()).hexdigest()
    assert complete["goalSha256"] == manifest["creation"]["goalSha256"] == digest, "changed initial goal"
    assert manifest["creation"]["state"] == "ready" and manifest["creation"]["initialGoalSent"] is True, "manifest not ready"
    thread = complete["threadId"]
    assert isinstance(thread, str) and thread and manifest["codex"]["threadId"] == thread, "conflicting root identity"
    assert manifest.get("sensitive", False) == receipt["request"].get("sensitive", False), "changed sensitivity"
    assert complete["trackingDevice"] == tracking["device"] and complete["trackingInode"] == tracking["inode"], "changed tracking identity"
    assert tracking["cwd"] == expected["cwd"] and pathlib.Path(expected["cwd"]).is_absolute(), "wrong root cwd"
    if receipt["schema"] == 3:
        team = receipt["directTeam"]
        assert all(complete[field] == team[target] for field, target in
            (("catalogDigest", "catalogDigest"), ("teamDigest", "teamDigest"), ("team", "id"))), "changed team binding"
    return {**{key: expected[key] for key in ("cwd", "requestId", "receiptId", "slug")}, "threadId": thread}


class MaterializationObservation(TuiObservation):
    """One exact fresh root; shared state correlates connection-scoped RPC IDs."""
    def __init__(self, directory, effective_settings=False):
        super().__init__(directory)
        self.journal_name, self.label = "materialization-rpc.jsonl", "Materialization"
        self.cwd, self.thread, self.binding = None, None, None
        self.clients, self.initializing, self.pending, self.unrelated = {}, {}, {}, set()
        self.effective_settings = effective_settings

    def observe(self, connection, direction, message):
        stage, reason = "rule", "invalid-rule"
        method = message.get("method") if isinstance(message, dict) else None
        safe_method = method if method in ("initialize", "thread/start", "thread/resume", "turn/start", "thread/read") else "other"
        number = message.get("id") if isinstance(message, dict) else None
        safe_id = number if type(number) is int or isinstance(number, str) and len(number) <= 128 else None
        try:
            rule_path = self.directory / "observe-materialization.json"
            if not rule_path.exists():
                return
            rule = json.loads(rule_path.read_text())
            assert set(rule) == {"cwd", "active"} and type(rule["active"]) is bool
            assert isinstance(rule["cwd"], str) and pathlib.Path(rule["cwd"]).is_absolute()
            assert self.cwd in (None, rule["cwd"])
            self.cwd = rule["cwd"]
            if not rule["active"]:
                return
            stage, reason = "binding", "invalid-or-changed-binding"
            binding_path = self.directory / "materialization-admission.json"
            if binding_path.exists():
                binding = json.loads(binding_path.read_text())
                assert set(binding) == {"cwd", "requestId", "receiptId", "slug", "threadId"}
                assert binding["cwd"] == self.cwd and all(isinstance(binding[k], str) and binding[k] for k in ("requestId", "receiptId"))
                assert all(binding[k] is None or isinstance(binding[k], str) and binding[k] for k in ("slug", "threadId"))
                if self.binding:
                    assert all(value is None or binding[key] == value for key, value in self.binding.items())
                if binding != self.binding:
                    self.append(connection, "binding", {"binding": binding})
                    self.binding = binding
            stage, reason = "message", "invalid-relevant-message"
            assert isinstance(message, dict)
            if "id" not in message:
                return
            key = (connection, json.dumps(number))
            params = message.get("params", {})
            if direction == "request" and method == "initialize":
                stage, reason = "initialize", "invalid-client-or-rpc-id"
                assert safe_id is not None and isinstance(params, dict)
                info = params.get("clientInfo", {})
                assert isinstance(info, dict) and all(isinstance(info[k], str) and len(info[k]) <= 256 for k in ("name", "version") if k in info)
                self.clients[connection] = {"info": {k: info[k] for k in ("name", "version") if k in info}, "initialized": False}
                self.initializing[key] = connection
                assert len(self.clients) <= 64 and len(self.initializing) <= 64
                return
            if direction == "response" and key in self.initializing:
                self.clients[self.initializing.pop(key)]["initialized"] = "result" in message and "error" not in message
                return
            if direction == "request":
                relevant = method in ("thread/start", "thread/resume", "turn/start", "thread/read")
                if not relevant:
                    self.unrelated.add(key)
                    assert len(self.unrelated) <= 256
                    return
                assert isinstance(params, dict)
                root_start = (method == "thread/start" and params.get("cwd") == self.cwd
                              and params.get("projectId") is None and params.get("parentThreadId") is None
                              and params.get("ephemeral") in (None, False))
                if not root_start and not (method != "thread/start" and self.thread and params.get("threadId") == self.thread):
                    self.unrelated.add(key)
                    assert len(self.unrelated) <= 256
                    return
                stage, reason = "request", "invalid-or-duplicate-tracked-request"
                assert safe_id is not None and key not in self.pending and len(self.pending) < 64
                projected = materialization_fields(params)
                config = params.get("config")
                assert not isinstance(config, dict) or config.get("model_reasoning_effort") is None or isinstance(config["model_reasoning_effort"], str)
                projected["config.model_reasoning_effort"] = {"present": isinstance(config, dict) and "model_reasoning_effort" in config,
                    "value": config.get("model_reasoning_effort") if isinstance(config, dict) else None,
                    "state": "reported" if isinstance(config, dict) and config.get("model_reasoning_effort") is not None else "unknown"}
                row = {"id": number, "method": method, "client": self.clients.get(connection), "params": projected}
                self.pending[key] = row
                self.append(connection, "request", row)
            elif direction == "response":
                if key in self.unrelated:
                    self.unrelated.remove(key)
                    return
                request = self.pending.pop(key, None)
                result = message.get("result", {})
                thread = result.get("thread", {}) if isinstance(result, dict) else {}
                stage, reason = "response", "uncorrelated-root-response"
                if request is None:
                    if isinstance(thread, dict) and (thread.get("id") == self.thread and self.thread or
                            thread.get("cwd") == self.cwd and thread.get("projectId") is None and thread.get("ephemeral") is False):
                        raise AssertionError
                    return
                stage, reason = "response", "invalid-tracked-response"
                row = {"id": number, "method": request["method"], "client": request["client"],
                       "result": materialization_fields(result), "thread": materialization_fields(thread)}
                if "error" in message:
                    row["rpcError"] = {"code": message["error"].get("code"), "messagePresent": "message" in message["error"]}
                elif self.effective_settings and request["method"] in ("thread/start", "thread/resume"):
                    row["effectiveSettings"] = startup_effective_settings(result)
                self.append(connection, "response", row)
                if request["method"] == "thread/start" and "error" not in message:
                    stage, reason = "root", "ambiguous-or-invalid-root"
                    assert self.thread is None
                    assert thread["cwd"] == self.cwd and isinstance(thread["id"], str) and thread["id"]
                    assert thread["projectId"] is None and thread["parentThreadId"] is None and thread["ephemeral"] is False
                    self.thread = thread["id"]
        except Exception:
            try:
                self.append(connection, "observation-error", {"observationError": "invalid-observation", "stage": stage,
                    "reason": reason, "method": safe_method, "rpcId": number if type(number) is int else None,
                    "rpcIdType": type(number).__name__ if type(number) in (str, int, bool, type(None)) else "other",
                    "messageDirection": direction if direction in ("request", "response") else "other"})
            except OSError:
                pass  # Missing/incomplete anchor read also refuses the proof.


def materialization_proof(directory, expected):
    """Check the retained prefix through the ordinary independent native read."""
    directory = pathlib.Path(directory)
    raw = (directory / "materialization-rpc.jsonl").read_bytes()
    assert raw and raw.endswith(b"\n") and len(raw) <= 1024 * 1024 + 1024, "unreadable or incomplete materialization journal"
    rows = [json.loads(line) for line in raw.splitlines()]
    assert len(rows) <= 257 and all("observationError" not in row for row in rows), "materialization observation failed"
    assert [row["sequence"] for row in rows] == list(range(len(rows))), "materialization sequence gap"
    anchors = [i for i, row in enumerate(rows) if row.get("direction") == "response" and row.get("method") == "thread/read"
               and isinstance(row.get("client"), dict) and row["client"].get("info", {}).get("name") == "creation-proof"]
    assert len(anchors) == 1, "missing or ambiguous ordinary native read"
    # A request on another connection may precede the read anchor while its
    # reply follows it. Validate the complete captured window before requiring
    # every exchange to be complete; do not discard those retained replies.
    pending, root, binding, anchor, exchanges = {}, None, None, None, []
    for row in rows:
        if row["direction"] == "binding":
            binding = row["binding"]
            continue
        client = row["client"]
        assert client and client["initialized"] is True and isinstance(client["info"].get("name"), str) and client["info"]["name"], "missing actual client initialization"
        key = (row["connection"], json.dumps(row["id"]))
        if row["direction"] == "request":
            assert key not in pending, "ambiguous materialization request"
            pending[key] = row
            checks = {"model": "model", "modelProvider": "modelProvider", "effort": "effort", "config.model_reasoning_effort": "effort"}
            for field, target in checks.items():
                value = row["params"][field]
                if value["present"] and value["value"] is not None:
                    assert value["value"] == expected[target], ("first materialization request disagreement", row)
            continue
        assert row["direction"] == "response" and key in pending, "missing request correlation"
        request = pending.pop(key)
        assert request["method"] == row["method"] and "rpcError" not in row, ("materialization RPC failed", row)
        method = row["method"]
        if method == "thread/start":
            assert root is None, "ambiguous materialization roots"
            root = row["thread"]["id"]["value"]
            assert root == expected["threadId"] and row["thread"]["cwd"]["value"] == expected["cwd"], "wrong materialization root"
            for field, value in (("projectId", None), ("parentThreadId", None), ("ephemeral", False)):
                assert row["thread"][field]["present"] and row["thread"][field]["value"] == value, "member or naming response substituted"
        else:
            assert request["params"]["threadId"]["value"] == root, "wrong materialization thread"
        if method in ("thread/start", "thread/resume", "thread/read"):
            assert row["thread"]["id"]["present"] and row["thread"]["id"]["value"] == root, "native response changed thread identity"
            authority = row["thread"] if method == "thread/read" else row["result"]
            for field, target in (("model", "model"), ("modelProvider", "modelProvider"), ("reasoningEffort", "effort"), ("cwd", "cwd")):
                value = authority[field]
                assert value["present"] and value["value"] is not None, ("unknown effective materialization field", field, row)
                assert value["value"] == expected[target], ("first native materialization disagreement", field, row)
        for location in ("result", "thread"):
            for field, target in (("model", "model"), ("modelProvider", "modelProvider"), ("reasoningEffort", "effort"), ("cwd", "cwd")):
                value = row[location][field]
                if value["present"] and value["value"] is not None:
                    assert value["value"] == expected[target], ("native reported-field disagreement", location, field, row)
        exchanges.append({"connection": row["connection"], "id": row["id"], "method": method, "client": client["info"]})
        if method == "thread/read" and client["info"]["name"] == "creation-proof":
            assert binding == {k: expected[k] for k in ("cwd", "requestId", "receiptId", "slug", "threadId")}, "missing or wrong admitted identity"
            anchor = row["sequence"]
    assert not pending, "incomplete materialization exchanges"
    assert root == expected["threadId"] and anchor is not None and not pending, "missing complete ordinary native-read anchor"
    return {"root": root, "binding": binding, "anchorSequence": anchor, "exchanges": exchanges,
            "journalSHA256": hashlib.sha256(raw).hexdigest(), "result": "observed materialization settings preserved"}


def tui_message(message):
    # Keep actual method/connection-scoped ID and input/queue admission evidence,
    # without dumping thread history, instructions or authentication metadata.
    value = {key: message[key] for key in ("id", "method") if key in message}
    if "error" in message:
        value["error"] = {key: message["error"].get(key) for key in ("code", "message")}
    for container in ("params", "result"):
        if not isinstance(message.get(container), dict):
            continue
        raw = message[container]
        projected = {key: raw[key] for key in ("threadId", "turnId", "clientUserMessageId", "input", "status") if key in raw}
        for key in ("thread", "turn", "queuedSubmission", "submission"):
            if isinstance(raw.get(key), dict):
                projected[key] = {field: raw[key][field] for field in
                    ("id", "threadId", "turnId", "clientUserMessageId", "status", "input") if field in raw[key]}
        value[container] = projected
    assert len(json.dumps(value).encode()) <= 32768, "TUI observation message exceeds bound"
    return value


async def proxy(listen, target, directory="/tmp/creation-model", settings=""):
    directory = pathlib.Path(directory)
    observations = TuiObservation(directory)
    materialization = MaterializationObservation(directory, effective_settings=settings == "startup")

    async def connection(client):
        async with websockets.unix_connect(target, max_size=16 * 1024 * 1024) as native:
            starts = {}
            archives = {}
            archive_rule = directory / "drop-archive-reply.json"
            connection_id = str(uuid.uuid4())
            client_info, initialize_reply, resumes, pending = {}, {}, {}, {}
            bound_thread, announced = None, None

            def observe(direction, message):
                nonlocal bound_thread, announced, client_info, initialize_reply
                materialization.observe(connection_id, direction, message)
                try:
                    method = message.get("method", "")
                    params = message.get("params", {})
                    if direction == "request":
                        if method == "initialize":
                            client_info = params.get("clientInfo", {})
                        if method == "thread/resume":
                            bound_thread = params.get("threadId")
                            resumes[bound_thread] = {"request": tui_message(message)}
                        if (method == "initialize" or method.startswith(("thread/resume", "turn/", "thread/queue/"))) and "id" in message:
                            pending[message["id"]] = (method, params.get("threadId", bound_thread))
                            if len(pending) > 32:
                                pending.pop(next(iter(pending)))
                    correlated = pending.pop(message.get("id"), None) if direction == "response" and "id" in message else None
                    if correlated and correlated[0] == "initialize":
                        initialize_reply = tui_message(message)
                    if correlated and correlated[0] == "thread/resume":
                        resumes[correlated[1]]["reply"] = tui_message(message)
                    rule = observations.rule()
                    if not rule or client_info.get("name") != "codex-tui":
                        return
                    identity = params.get("threadId", bound_thread)
                    if correlated:
                        identity = correlated[1]
                    if identity != rule["threadId"]:
                        return
                    if announced != rule["threadId"]:
                        observations.append(connection_id, "identity", {"threadId": identity,
                            "clientInfo": client_info, "initializeReply": initialize_reply,
                            "resume": resumes.get(identity)})
                        announced = identity
                    relevant = method == "thread/resume" or method.startswith(("turn/", "thread/queue/", "item/"))
                    if correlated or relevant:
                        observations.append(connection_id, direction, {"requestMethod": correlated[0] if correlated else method,
                            **tui_message(message)})
                except (OSError, ValueError, AssertionError, TypeError) as error:
                    # A trace problem is an acceptance failure, never a reason
                    # to suppress or replace any native request/response.
                    try:
                        observations.append(connection_id, "observation-error", {"observationError": type(error).__name__})
                    except OSError:
                        pass  # The caller also requires a complete readable trace.

            async def upstream():
                async for data in client:
                    message = json.loads(data)
                    observe("request", message)
                    if (message.get("method") == "thread/start" and
                            message["params"].get("cwd", "").endswith("-before")):
                        while (directory / "hold-root-request").exists():
                            (directory / "root-entered").touch()
                            await asyncio.sleep(0.025)
                        if client.state.name != "OPEN":
                            return
                    if (message.get("method") == "turn/start" and
                            "CREATION_INITIAL_after" in json.dumps(message["params"].get("input"))):
                        starts[message["id"]] = message["params"]["threadId"]
                    if message.get("method") == "thread/archive" and archive_rule.exists():
                        rule = json.loads(archive_rule.read_text())
                        assert set(rule) == {"operation", "threadId"} and all(isinstance(value, str) and value for value in rule.values()), rule
                        if message["params"].get("threadId") == rule["threadId"]:
                            # IDs are scoped to this exact connection pair.
                            archives[message["id"]] = rule
                    await native.send(data)

            async def downstream():
                async for data in native:
                    message = json.loads(data)
                    observe("response", message)
                    rule = archives.pop(message.get("id"), None)
                    if rule and archive_rule.exists() and json.loads(archive_rule.read_text()) == rule:
                        receipt = {**rule, "requestId": message["id"], "response": message,
                                   "nativeSuccess": "result" in message and "error" not in message}
                        assert len(json.dumps(receipt).encode()) <= 16384, "archive response receipt exceeds bound"
                        # Claim/disarm before closing anything; all other pairs
                        # and the listening socket continue forwarding unchanged.
                        archive_rule.rename(archive_rule.with_suffix(".consumed.json"))
                        with tempfile.NamedTemporaryFile(mode="w", dir=directory, delete=False) as marker:
                            json.dump(receipt, marker)
                        pathlib.Path(marker.name).replace(directory / "archive-reply-receipt.json")
                        if receipt["nativeSuccess"]:
                            await client.close()
                            await native.close()
                            return
                    if message.get("id") in starts and "result" in message:
                        with tempfile.NamedTemporaryFile(mode="w", dir=directory, delete=False) as marker:
                            json.dump({"threadId": starts[message["id"]],
                                "turnId": message["result"]["turn"]["id"]}, marker)
                        pathlib.Path(marker.name).replace(directory / "send-accepted")
                        while (directory / "hold-send-reply").exists():
                            await asyncio.sleep(0.025)
                    await client.send(data)

            tasks = [asyncio.create_task(upstream()), asyncio.create_task(downstream())]
            try:
                done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
                for task in done:
                    task.result()
            finally:
                for task in tasks:
                    task.cancel()
                await asyncio.gather(*tasks, return_exceptions=True)

    async with websockets.unix_serve(connection, listen, max_size=16 * 1024 * 1024):
        pathlib.Path(listen).chmod(0o600)
        await asyncio.Future()


def pane_observation(socket, pane, timeout=5):
    command = ["tmux", "-S", socket]
    fields = ("pane_id", "pane_width", "pane_height", "pane_in_mode", "alternate_on", "cursor_x", "cursor_y", "cursor_flag")
    values = subprocess.check_output(command + ["display-message", "-p", "-t", pane,
        "\t".join("#{" + key + "}" for key in fields)], text=True, timeout=timeout).rstrip("\n").split("\t")
    assert len(values) == len(fields) and values[0] == pane
    metadata = dict(zip(fields, values))
    assert 1 <= int(metadata["pane_width"]) <= 300 and 1 <= int(metadata["pane_height"]) <= 100
    def capture(start):
        return subprocess.check_output(command + ["capture-pane", "-p", "-t", pane, "-S", str(start)], text=True, timeout=timeout)
    result = {"pane": metadata, "visible": capture(0), "scrollback": capture(-120)}
    assert len(json.dumps(result).encode()) <= 256 * 1024
    return result


def composer_ready(observed):
    pane, lines = observed["pane"], observed["visible"].splitlines()
    if pane["pane_in_mode"] != "0" or pane["cursor_flag"] != "1":
        return False
    y = int(pane["cursor_y"])
    if y >= len(lines):
        return False
    line = lines[y].lstrip()
    arrows = len(line) - len(line.lstrip("›"))
    # The pinned TUI's empty composer placeholders; arbitrary retained draft
    # text must never be erased merely to make a test input possible.
    return (arrows > 0 and line[arrows:].strip() in ("", "Ask Codex to do anything", "Ask a follow-up question")
            and int(pane["cursor_x"]) == len(lines[y]) - len(line) + arrows + 1)


async def rpc(native, number, method, params, observe=None):
    await native.send(json.dumps({"id": number, "method": method, "params": params}))
    while True:
        result = json.loads(await native.recv())
        if observe is not None:
            observe(native, result)
        if result.get("id") == number:
            if observe is not None:
                assert type(result["id"]) is type(number), "seed RPC ID type changed"
            assert "error" not in result, result
            return result["result"]


class SeedCompletion:
    """Pinned turn/completed authority on this already initialized connection."""
    def __init__(self, connection, thread, evidence):
        assert isinstance(thread, str) and 0 < len(thread) <= 128
        self.connection, self.thread, self.turn = connection, thread, None
        self.sequence = 0
        self.receipt = {"connectionId": str(uuid.uuid4()), "initialized": True, "threadId": thread,
                        "notifications": []}
        evidence["seedCompletion"] = self.receipt

    def observe(self, connection, message):
        assert connection is self.connection, "seed completion connection changed"
        self.sequence += 1
        assert self.sequence <= 256, "seed message bound"
        if message.get("method") != "turn/completed":
            return  # Other notifications/RPCs are not completion authority.
        assert "id" not in message, "completion must be a notification"
        params = message.get("params")
        assert isinstance(params, dict) and isinstance(params.get("threadId"), str) and 0 < len(params["threadId"]) <= 128, "malformed completion thread"
        turn = params.get("turn")
        assert isinstance(turn, dict) and isinstance(turn.get("id"), str) and 0 < len(turn["id"]) <= 128, "malformed completion turn"
        assert turn.get("status") in ("completed", "interrupted", "failed", "inProgress"), "malformed completion status"
        row = {"sequence": self.sequence, "connectionId": self.receipt["connectionId"],
               "threadId": params["threadId"], "turnId": turn["id"], "status": turn["status"]}
        assert len(self.receipt["notifications"]) < 64 and len(json.dumps(row).encode()) <= 1024, "seed notification bound"
        self.receipt["notifications"].append(row)
        assert len(json.dumps(self.receipt).encode()) <= 32768, "seed evidence byte bound"
        if self.turn is not None:
            self.completed()

    def bind(self, response):
        turn = response["turn"]
        assert isinstance(turn["id"], str) and 0 < len(turn["id"]) <= 128, "missing accepted seed turn"
        assert turn["status"] in ("completed", "inProgress"), "unsuccessful accepted seed status"
        self.turn = turn["id"]
        self.receipt["acceptedReply"] = {"turnId": self.turn, "status": turn["status"],
                                         "sequence": self.sequence}

    def completed(self):
        # Well-formed other thread/turn completions remain recorded and ignored.
        matches = [row for row in self.receipt["notifications"]
                   if row["threadId"] == self.thread and row["turnId"] == self.turn]
        assert len(matches) <= 1, "duplicate seed completion"
        if not matches:
            return False
        assert matches[0]["status"] == "completed", "seed did not complete successfully"
        self.receipt["completedNotification"] = matches[0]
        return True


async def completed_seed_history(connection, numbers, thread, seed, evidence):
    """One eight-second segment: actual completion, then unchanged history read."""
    collector = SeedCompletion(connection, thread, evidence)  # Armed before turn/start.
    async with asyncio.timeout(8):
        params = {"threadId": thread, "model": "gpt-5.5", "effort": "high",
                  "input": [{"type": "text", "text": seed}]}
        record = {"method": "turn/start", "params": params}
        evidence["rpc"].append(record)
        response = await asyncio.wait_for(rpc(connection, next(numbers), "turn/start", params, collector.observe), 4)
        record["result"] = response
        collector.bind(response)
        while not collector.completed():
            collector.observe(connection, json.loads(await connection.recv()))
        while True:
            params = {"threadId": thread, "includeTurns": True}
            record = {"method": "thread/read", "params": params}
            evidence["rpc"].append(record)
            response = await asyncio.wait_for(rpc(connection, next(numbers), "thread/read", params, collector.observe), 4)
            record["result"] = response
            before = response["thread"]
            if any(turn["id"] == collector.turn and turn["status"] == "completed" for turn in before["turns"]):
                return before
            await asyncio.sleep(0.025)


async def relinquish_seed(connection, numbers, thread, evidence):
    """Relinquish only this completed seed's subscription, never force cold state."""
    seed = evidence["seedCompletion"]
    assert seed["initialized"] is True and seed["threadId"] == thread, "wrong seed subscription identity"
    completed = seed["completedNotification"]
    assert completed["threadId"] == thread and completed["status"] == "completed", "seed completion missing before unsubscribe"
    before = evidence["before"]
    assert before["id"] == thread and any(turn["id"] == completed["turnId"] and turn["status"] == "completed"
        for turn in before["turns"]), "seed history missing before unsubscribe"
    number = next(numbers)
    request = {"id": number, "method": "thread/unsubscribe", "params": {"threadId": thread}}
    record = {"connectionId": seed["connectionId"], "request": request}
    evidence["seedSubscription"] = record

    def observe(actual, message):
        assert actual is connection, "wrong unsubscribe connection"
        if message.get("id") == number:
            assert type(message["id"]) is type(number), "unsubscribe RPC ID type changed"
            record["reply"] = message

    response = await asyncio.wait_for(rpc(connection, number, request["method"], request["params"], observe), 4)
    assert response == {"status": "unsubscribed"}, "seed subscription was not relinquished"
    await asyncio.wait_for(connection.close(), 1)
    record["closed"] = True


async def initialize(native):
    await rpc(native, 1, "initialize", {"clientInfo": {"name": "creation-proof", "version": "1"},
                                      "capabilities": {"experimentalApi": True}})
    await native.send(json.dumps({"method": "initialized"}))


async def read(socket, identity):
    async with websockets.unix_connect(socket, max_size=16 * 1024 * 1024) as native:
        await initialize(native)
        if identity == "inventory":
            metadata = await rpc(native, 2, "thread/list", {"limit": 100,
                "modelProviders": ["primary", "alternate"], "sourceKinds": ["vscode"]})
        else:
            metadata = await rpc(native, 2, "thread/read", {"threadId": identity, "includeTurns": True})
        print(json.dumps(metadata))


async def fixture_request(socket, method, params_json):
    # Ordinary queue/goal RPCs only; the VM owns all switch/control operations.
    assert method in {"thread/queue/add", "thread/queue/list", "thread/goal/set", "thread/goal/get"}, method
    params = json.loads(params_json)
    assert isinstance(params, dict) and isinstance(params.get("threadId"), str) and params["threadId"]
    async with websockets.unix_connect(socket, max_size=16 * 1024 * 1024) as native:
        await initialize(native)
        print(json.dumps(await rpc(native, 2, method, params)))


def process_start(pid):
    try:
        fields = pathlib.Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()
        return fields[19]
    except (FileNotFoundError, ProcessLookupError):
        return None


def client_observation(tmux_socket, slug, old_client=None):
    # Only the exact managed pane's descendants and its recorded old client PID.
    def tmux(target, fields):
        return subprocess.check_output(["tmux", "-S", tmux_socket, "display-message", "-p", "-t", target,
            "\t".join("#{" + field + "}" for field in fields)], text=True, timeout=5).rstrip("\n").split("\t")
    keys = ["id", "name", "managed", "slug", "workspace", "environmentSlug", "socket", "root", "remote", "pane", "token", "shell"]
    fields = ["session_id", "session_name", "@dev_session", "@dev_session_slug", "E:DEV_SESSION_WORKSPACE",
        "E:DEV_SESSION_SLUG", "socket_path", "@dev_session_codex_thread", "@dev_session_codex_socket",
        "@dev_session_codex_pane", "E:DEV_SESSION_TMUX_IDENTITY", "E:SHELL"]
    values = tmux("=" + slug + ":", fields)
    assert len(values) == len(keys), values
    session = dict(zip(keys, values))
    assert session["pane"].startswith("%") and session["shell"].startswith("/"), session
    pane_fields = ["session_id", "pane_id", "pane_pid", "pane_current_command"]
    pane_values = tmux(session["pane"], pane_fields)
    assert len(pane_values) == 4, pane_values
    pane = dict(zip(["session", "id", "pid", "command"], pane_values))
    pane["pid"] = int(pane["pid"])
    processes, pending, seen = [], [pane["pid"]], set()
    while pending:
        pid = pending.pop()
        if pid in seen:
            continue
        seen.add(pid)
        assert len(seen) <= 64, "fixture pane process tree exceeds bound"
        try:
            start = process_start(pid)
            if start is None:
                continue
            base = pathlib.Path(f"/proc/{pid}")
            process = {"pid": pid, "startTime": start, "command": (base / "comm").read_text().strip(),
                "executable": str((base / "exe").readlink()),
                "argv": (base / "cmdline").read_bytes().rstrip(b"\0").decode().split("\0")}
            pending.extend(int(value) for value in (base / f"task/{pid}/children").read_text().split())
            if process_start(pid) == start:
                processes.append(process)
        except (FileNotFoundError, ProcessLookupError):
            continue
    assert tmux("=" + slug + ":", fields) == values and tmux(session["pane"], pane_fields) == pane_values, "pane changed during observation"
    return {"session": session, "pane": pane, "processes": processes,
        "shellExecutable": str(pathlib.Path(session["shell"]).resolve(strict=True)),
        "oldProcessPresent": old_client is not None and process_start(int(old_client["pid"])) == old_client["startTime"]}


def assert_client_observation(observed, expected):
    session, pane = observed["session"], observed["pane"]
    assert session["id"].startswith("$") and session["name"] == session["slug"] == session["environmentSlug"] == expected["slug"], session
    assert session["managed"] == "1" and session["workspace"] == expected["workspace"], session
    assert session["socket"] == expected["tmuxSocket"] and session["remote"] == expected["socket"] and session["root"] == expected["root"], session
    assert len(session["token"]) == 64 and all(c in "0123456789abcdef" for c in session["token"]), session
    assert pane["session"] == session["id"] and pane["id"] == session["pane"] and pane["pid"] > 0, pane
    source = expected.get("source")
    if source:
        assert session == source["session"], session
        assert pane["id"] == source["pane"]["id"] and observed["oldProcessPresent"] is False, observed
    operation = expected.get("operation")
    if operation and not expected.get("providerOperation"):
        participants = [p for p in operation["participants"] if p["workspace"] == expected["workspace"]
            and p["slug"] == expected["slug"] and p["thread"] == p["root"] == expected["root"]]
        assert len(participants) == 1 and participants[0]["incarnation"], participants
        intents = [t for t in operation.get("terminalIntents", []) if t["workspace"] == expected["workspace"]
            and t["slug"] == expected["slug"] and t["root"] == expected["root"]
            and t["incarnation"] == participants[0]["incarnation"]]
        assert len(intents) == 1, operation
    clients = [p for p in observed["processes"] if p["executable"] == expected["native"]]
    if expected["state"] == "stopped":
        assert source and not clients, observed
        assert any(p["pid"] == pane["pid"] and p["executable"] == observed["shellExecutable"]
            and p["command"] == pane["command"] for p in observed["processes"]), observed
        return None
    assert expected["state"] == "running" and len(clients) == 1, observed
    client = clients[0]
    assert client["pid"] > 0 and client["startTime"].isdigit(), client
    assert client["argv"][-4:] == ["--remote", "unix://" + expected["socket"], "resume", expected["root"]], client
    assert pane["command"] == client["command"], pane
    if source:
        assert (client["pid"], client["startTime"]) != (source["client"]["pid"], source["client"]["startTime"]), client
        if expected.get("providerOperation"):
            assert operation["phase"] == "applied" and not operation.get("message"), operation
        else:
            assert operation["phase"] == "complete" and operation["restoration"]["completed"], operation
    return client


def managed_client(socket, slug, expected_json):
    expected = json.loads(expected_json)
    source = expected.get("source")
    observed = client_observation(socket, slug, source["client"] if source else None)
    observed["client"] = assert_client_observation(observed, expected)
    print(json.dumps(observed))


def production_native(package):
    """Bind acceptance to the measured production output, not version text."""
    metadata = json.loads((pathlib.Path(package) / "share/dev-workspace/package.json").read_text())
    codex = metadata["codex"]
    assert metadata["schema"] == 1 and codex["app_server_argv"] == ["--allow-host-provider-switching"], metadata
    assert codex["native"] == "/nix/store/5j9lm7m2avm6qq5l73jxb9hpzpicba0n-codex-0.167.0", codex
    provenance = codex["provenance"]
    assert provenance["revision"] == "5b0cea5e1551ad69666ff2ce52df8f81f533eba1", provenance
    assert provenance["sourcePath"] == "/nix/store/ghyrgph8811q846xnx5i8b5iy6r0bdh7-source", provenance
    assert provenance["recipeRevision"] == "d960982d3b05c294f3cf98cab3e23798548a0ced", provenance
    binary = (pathlib.Path(codex["native"]) / "bin/codex").resolve(strict=True)
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    assert digest == "8880a0384e3020872fe9635098772d10bb436b4fbe4c931ad8625a56dca73e58", digest
    assert hashlib.sha256((pathlib.Path(package) / "libexec/codex/libexec/codex/bin/codex").resolve().read_bytes()).hexdigest() == digest
    return {"package": package, "executable": str(binary), "sha256": digest, **codex}


def candidate_native(receipt_path, expected_sha256):
    """P's approved realized receipt selects raw native, never relabels a donor."""
    def hex_value(value, length):
        return isinstance(value, str) and len(value) == length and all(c in "0123456789abcdef" for c in value)

    def store_path(value, suffix):
        assert isinstance(value, str) and value.startswith("/nix/store/"), "candidate path outside store"
        name = value[len("/nix/store/"):]
        alphabet = "0123456789abcdfghijklmnpqrsvwxyz"
        assert len(name) > 33 and all(c in alphabet for c in name[:32]) and name[32] == "-", "malformed candidate store path"
        assert "/" not in name and name.endswith(suffix), "wrong candidate store path kind"
        return pathlib.Path(value)

    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            assert key not in result, "duplicate candidate receipt key"
            result[key] = value
        return result

    assert hex_value(expected_sha256, 64), "missing approved candidate receipt digest"
    path = pathlib.Path(receipt_path)
    assert path.is_absolute(), "candidate receipt path must be absolute"
    raw = path.read_bytes()
    assert 0 < len(raw) <= 32768 and hashlib.sha256(raw).hexdigest() == expected_sha256, "candidate receipt digest mismatch"
    receipt = json.loads(raw, object_pairs_hook=unique_object)
    assert isinstance(receipt, dict) and set(receipt) == {"derivation", "output", "provenance", "nativeHashes",
        "runtimeRevision", "runtimeSource", "exportedDerivation", "exportedOutput", "app_server_argv"}, "invalid candidate receipt keys"
    expected_provenance = {
        "version": "0.167.0", "upstreamRelease": "rust-v0.167.0",
        "upstreamRevision": "086ccc833bc9cf01024ad2dd6af7dbd421b6bf9a",
        "revision": "5b0cea5e1551ad69666ff2ce52df8f81f533eba1",
        "hash": "sha256-fnOYvL8mG/NiCGNcK6j7rlju1rNEqwhp7fSj5MMVCa8=",
        "cargoHash": "sha256-Dqo9nMfbkTcyT+S79U2JdXpnxwrN0fQjjMlG055jvJE=",
        "patchRevision": "host-provider-switching-v1", "recipeRevision": "d960982d3b05c294f3cf98cab3e23798548a0ced",
        "repository": "github.com/aither64/codex", "sourcePath": "/nix/store/ghyrgph8811q846xnx5i8b5iy6r0bdh7-source"}
    assert receipt["provenance"] == expected_provenance, "wrong candidate native provenance"
    assert hex_value(receipt["runtimeRevision"], 40), "malformed candidate runtime revision"
    store_path(receipt["runtimeSource"], "-source")
    selected = "/nix/store/sl7nvyki5b70mm3warcw2ccaazqs7sak-codex-0.167.0.drv"
    output = "/nix/store/5j9lm7m2avm6qq5l73jxb9hpzpicba0n-codex-0.167.0"
    assert receipt["derivation"] == receipt["exportedDerivation"] == selected, "candidate derivation/export mismatch"
    assert receipt["output"] == receipt["exportedOutput"] == output, "candidate output/export mismatch"
    root = store_path(receipt["output"], "-codex-0.167.0")
    store_path(receipt["derivation"], ".drv")
    assert receipt["app_server_argv"] == ["--allow-host-provider-switching"], "wrong candidate fixed argv"
    names = ("codex", "codex-code-mode-host", "logs_client")
    hashes = receipt["nativeHashes"]
    assert isinstance(hashes, dict) and set(hashes) == set(names) and all(hex_value(hashes[name], 64) for name in names), "missing realized candidate hashes"
    assert root.resolve(strict=True) == root and root.is_dir(), "candidate output escaped"
    layout = root / "libexec/codex"
    manifest_path = layout / "codex-package.json"
    assert manifest_path.resolve(strict=True).is_relative_to(root), "candidate manifest escaped"
    manifest = json.loads(manifest_path.read_bytes(), object_pairs_hook=unique_object)
    assert isinstance(manifest, dict) and type(manifest.get("layoutVersion")) is int, "malformed candidate layout version"
    assert manifest == {"entrypoint": "bin/codex", "layoutVersion": 1, "pathDir": "codex-path",
        "resourcesDir": "codex-resources", "target": "x86_64-unknown-linux-gnu", "variant": "codex", "version": "0.167.0"}, "inconsistent candidate layout"
    executables = {}
    for name in names:
        binary = (layout / "bin" / name).resolve(strict=True)
        assert binary.is_relative_to(root) and binary.is_file() and os.access(binary, os.X_OK), "candidate executable escaped or unavailable"
        assert hashlib.sha256(binary.read_bytes()).hexdigest() == hashes[name], ("candidate executable hash mismatch", name)
        executables[name] = str(binary)
    assert (root / "bin/codex").resolve(strict=True) == pathlib.Path(executables["codex"]), "candidate entrypoint mismatch"
    # Raw exports link normal immutable rg/bwrap dependencies. Materialized
    # mkCodexPackage no-symlink assembly is a separate final-package contract.
    for relative in ("codex-path/rg", "codex-resources/bwrap"):
        helper = (layout / relative).resolve(strict=True)
        assert helper.is_relative_to("/nix/store") and helper.is_file() and os.access(helper, os.X_OK), "candidate helper escaped or unavailable"
    return {"kind": "candidate-raw-native", "native": str(root), "executable": executables["codex"],
        "sha256": hashes["codex"], "executables": executables, "provenance": receipt["provenance"],
        "app_server_argv": receipt["app_server_argv"], "receiptPath": str(path), "receiptSHA256": expected_sha256,
        "derivation": receipt["derivation"], "nativeHashes": hashes, "runtimeRevision": receipt["runtimeRevision"],
        "runtimeSource": receipt["runtimeSource"], "exportedDerivation": receipt["exportedDerivation"], "exportedOutput": receipt["exportedOutput"]}


def startup_native(package, mode, receipt_path=None, receipt_sha256=None):
    assert mode in ("notice", "provider-only", "provider-only-unsubscribed"), "invalid startup mode"
    assert (receipt_path is None) == (receipt_sha256 is None), "candidate receipt and approved digest must be paired"
    assert receipt_path is None or mode != "notice", "candidate receipt requires provider-only mode"
    donor = production_native(package)  # Validate the final package and its actual embedded bytes.
    candidate = candidate_native(receipt_path, receipt_sha256) if receipt_path is not None else None
    return donor, candidate


def stable_goal_fields(goal):
    return {key: goal[key] for key in ("threadId", "objective", "status", "tokenBudget", "createdAt")}


def startup_configuration(port, alternate=False):
    assert type(port) is int and 1 <= port <= 65535
    return ('model_provider="primary"\nmodel="gpt-5.5"\nmodel_reasoning_effort="xhigh"\n'
            'approval_policy="never"\n[features]\ngoals=true\n'
            '[model_providers.primary]\nname="Primary fixture"\n'
            f'base_url="http://127.0.0.1:{port}/primary/v1"\nwire_api="responses"\n'
            'requires_openai_auth=false\nsupports_websockets=false\n'
            'env_key="CREATION_PRIMARY_TOKEN"\nrequest_max_retries=0\nstream_max_retries=0\n'
            + ('[model_providers.alternate]\nname="Alternate fixture"\n'
               f'base_url="http://127.0.0.1:{port}/alternate/v1"\nwire_api="responses"\n'
               'requires_openai_auth=false\nsupports_websockets=false\n'
               'env_key="CREATION_ALTERNATE_TOKEN"\nrequest_max_retries=0\nstream_max_retries=0\n' if alternate else '')
            + '[notice.model_migrations]\n"gpt-5.5" = "gpt-6-sol"\n')


def startup_effective_settings(response):
    # Pinned Thread omits policy fields; start/resume responses own them.
    fields = ("model", "modelProvider", "reasoningEffort", "cwd", "approvalPolicy",
              "sandbox", "runtimeWorkspaceRoots")
    assert isinstance(response, dict) and all(key in response for key in (*fields, "thread")), "missing startup settings authority"
    thread = response["thread"]
    assert isinstance(thread, dict) and isinstance(thread["id"], str) and thread["id"], "missing startup thread identity"
    assert all(isinstance(response[key], str) and response[key] for key in fields[:5]), "missing effective scalar settings"
    assert pathlib.Path(response["cwd"]).is_absolute(), "nonabsolute effective cwd"
    sandbox = response["sandbox"]
    assert isinstance(sandbox, dict) and isinstance(sandbox["type"], str) and sandbox["type"], "missing effective sandbox"
    roots = response["runtimeWorkspaceRoots"]
    assert isinstance(roots, list) and all(isinstance(root, str) and pathlib.Path(root).is_absolute() for root in roots), "missing effective workspace roots"
    return {"threadId": thread["id"], **{key: response[key] for key in fields}}


def startup_tui_boundary(directory, thread):
    """Only valid incomplete protocol evidence is pending; corruption is fatal."""
    raw = (pathlib.Path(directory) / "materialization-rpc.jsonl").read_bytes()
    assert raw.endswith(b"\n") and 0 < len(raw) <= 1024 * 1024 + 1024, "incomplete startup trace"
    rows = [json.loads(line) for line in raw.splitlines()]
    assert 6 <= len(rows) <= 256 and all(isinstance(row, dict) and "observationError" not in row for row in rows), "invalid startup trace"
    assert [row["sequence"] for row in rows] == list(range(len(rows))), "startup trace sequence gap"
    pending, requests, replies = {}, [], []
    root_cwd = rows[0]["params"]["cwd"]["value"]
    assert isinstance(root_cwd, str) and pathlib.Path(root_cwd).is_absolute(), "invalid startup root cwd"
    seed_methods = ("thread/start", "turn/start", "thread/read")
    for index, row in enumerate(rows):
        client, connection, number = row["client"], row["connection"], row["id"]
        assert isinstance(connection, str) and connection, "missing startup connection"
        assert type(number) is int or isinstance(number, str) and 0 < len(number) <= 128, "invalid startup RPC ID"
        assert client["initialized"] is True and isinstance(client["info"]["name"], str) and client["info"]["name"], "uninitialized startup client"
        method, direction = row["method"], row["direction"]
        assert method in (*seed_methods, "thread/resume") and direction in ("request", "response"), "invalid startup exchange"
        if index < 6:
            assert method == seed_methods[index // 2] and direction == ("request" if index % 2 == 0 else "response"), "invalid startup seed prefix"
            assert client["info"]["name"] == "creation-proof" and connection == rows[0]["connection"], "wrong startup seed client"
        key = (connection, type(number), number)
        if direction == "request":
            assert key not in pending, "duplicate startup RPC ID"
            if method == "thread/start":
                assert index == 0 and row["params"]["cwd"]["present"] is True, "ambiguous startup root"
            else:
                assert row["params"]["threadId"]["present"] is True and row["params"]["threadId"]["value"] == thread, "wrong startup thread"
            pending[key] = row
            if method == "thread/resume":
                assert client["info"]["name"] == "codex-tui", "wrong startup resume client"
                requests.append(row)
                assert len(requests) == 1, "ambiguous TUI resume"
        else:
            request = pending.pop(key, None)
            assert request is not None and request["method"] == method and request["client"] == client, "uncorrelated startup reply"
            assert "rpcError" not in row, "native startup RPC error"
            if method in ("thread/start", "thread/resume", "thread/read"):
                assert row["thread"]["id"]["present"] is True and row["thread"]["id"]["value"] == thread, "wrong startup reply thread"
                assert row["thread"]["cwd"]["present"] is True and row["thread"]["cwd"]["value"] == root_cwd, "wrong startup reply cwd"
            if method in ("thread/start", "thread/resume"):
                effective = row["effectiveSettings"]
                # Reuse the actual start/resume authority validator, including
                # policy/roots. The journal is a projection, never a fallback.
                assert startup_effective_settings({**effective, "thread": {"id": effective["threadId"]}}) == effective
                assert effective["threadId"] == thread and effective["cwd"] == root_cwd, "wrong effective startup identity"
            if method == "thread/resume":
                replies.append(row)
    assert all(request["sequence"] >= 6 for request in pending.values()), "incomplete startup seed prefix"
    result = {"stage": "missing-request", "journalSHA256": hashlib.sha256(raw).hexdigest()}
    if requests:
        result.update(stage="missing-reply", request=requests[0])
    if replies:
        assert len(replies) == 1 and not pending, "ambiguous or incomplete startup reply"
        result.update(stage="protocol-ready", reply=replies[0])
    return result


def startup_tui_readiness(directory, thread, expected, observed, evidence, now, deadline, capture_client=None):
    """Joint protocol/pane decision shared with pure controls, on one deadline."""
    boundary = startup_tui_boundary(directory, thread)
    evidence["startupReadiness"] = boundary
    if boundary["stage"] == "protocol-ready":
        # Retain even a disagreeing reply before failing, regardless of loading
        # pane state. Never poll for settings to converge.
        evidence["tuiBoundary"] = boundary
        evidence["effectiveAfter"] = boundary["reply"]["effectiveSettings"]
        if capture_client is not None and "client" not in evidence:
            capture_client()
        assert evidence["effectiveAfter"] == expected, ("startup effective settings changed", expected, evidence["effectiveAfter"])
        boundary["stage"] = "ready" if composer_ready(observed) and "Resuming session…" not in observed["visible"] else "missing-ready-pane"
    assert now < deadline, ("startup readiness deadline", boundary["stage"], observed)
    return boundary["stage"] == "ready"


async def startup_notice(package, model_script, fixture_path, mode="notice", receipt_path=None, receipt_sha256=None):
    """One private native startup, never a managed-worker/restoration proof."""
    started = time.monotonic()
    donor, candidate = startup_native(package, mode, receipt_path, receipt_sha256)
    relinquish = mode == "provider-only-unsubscribed"
    diagnostic = mode != "notice"
    # The explicit provider flag exercises invocation intent. Private config
    # definitions/env-key auth deliberately do not model runtime auth helpers.
    provider = "alternate" if diagnostic else "primary"
    directory = pathlib.Path(fixture_path)
    assert directory.is_absolute() and len(str(directory / "app.sock").encode()) < 100
    directory.mkdir(mode=0o700)  # Fresh only; never replay a retained fixture.
    for name in ("home", "codex", "sqlite", "state", "cache", "run", "config", "cwd", "model"):
        (directory / name).mkdir(mode=0o700)
    identity = candidate if candidate is not None else donor
    native_socket, tmux_socket = str(directory / "app.sock"), str(directory / "tmux.sock")
    env = {"HOME": str(directory / "home"), "CODEX_HOME": str(directory / "codex"),
           "CODEX_SQLITE_HOME": str(directory / "sqlite"), "XDG_STATE_HOME": str(directory / "state"),
           "XDG_CACHE_HOME": str(directory / "cache"), "XDG_RUNTIME_DIR": str(directory / "run"),
           "XDG_CONFIG_HOME": str(directory / "config"),
           "PATH": os.environ["PATH"], "TERM": "xterm-256color", "LANG": "C.UTF-8",
           "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED": "1",
           "CREATION_PRIMARY_TOKEN": "fixture-primary-token", "CREATION_ALTERNATE_TOKEN": "fixture-alternate-token"}
    config = directory / "codex/config.toml"
    processes, handles, connection, tmux_pid, client_pid = [], [], None, None, None
    tmux_start, client_start = None, None
    evidence = {"native": identity, "directory": str(directory), "rpc": [], "panes": [],
                "scope": "native startup only; no keys, host registration or managed restoration",
                "mode": mode, "providerProjection": "top-level provider flag; private definitions and dummy env-key auth"}
    if candidate is not None:
        evidence.update(donorNative=donor, candidateNative=candidate,
            fixtureSource=str(pathlib.Path(__file__).resolve()),
            scope="candidate raw-native private proof; not final workspace package or managed acceptance")

    async def launch(argv, environment, name):
        output = (directory / (name + ".log")).open("wb")
        handles.append(output)
        process = await asyncio.create_subprocess_exec(*argv, cwd=directory / "cwd", env=environment,
            stdout=output, stderr=output, start_new_session=True)
        processes.append(process)
        evidence.setdefault("processes", []).append({"kind": name, "pid": process.pid, "argv": argv})
        return process

    async def tmux(*args):
        process = await asyncio.create_subprocess_exec("tmux", "-f", "/dev/null", "-S", tmux_socket, *args,
            cwd=directory / "cwd", env=env, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE, start_new_session=True)
        processes.append(process)
        output, error = await asyncio.wait_for(process.communicate(), 2)
        assert process.returncode == 0, (args, error.decode(errors="replace"))
        return output.decode().strip()

    async def stop(process):
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        if process.returncode is None:
            try:
                await asyncio.wait_for(process.wait(), 2)
            except asyncio.TimeoutError:
                os.killpg(process.pid, signal.SIGKILL)
                await asyncio.wait_for(process.wait(), 1)
        else:
            await process.wait()
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            return
        os.killpg(process.pid, signal.SIGKILL)
        deadline = time.monotonic() + 1
        while True:
            try:
                os.killpg(process.pid, 0)
            except ProcessLookupError:
                return
            assert time.monotonic() < deadline, ("startup child group not joined", process.pid)
            await asyncio.sleep(0.025)

    async def stop_pid(pid, start):
        if start is None:
            raise AssertionError(("missing startup process incarnation", pid))
        for action in (signal.SIGTERM, signal.SIGKILL):
            if process_start(pid) != start:
                return
            try:
                os.kill(pid, action)
            except ProcessLookupError:
                return
            deadline = time.monotonic() + 1
            while process_start(pid) == start and time.monotonic() < deadline:
                await asyncio.sleep(0.025)
        assert process_start(pid) != start, ("startup process not joined", pid)

    def requests():
        path = directory / "model/requests.jsonl"
        return list(helpers["request_rows"](path)) if path.exists() else []

    client_capture_attempted = False

    def record_client():
        nonlocal client_capture_attempted
        client_capture_attempted = True
        assert client_start is not None and process_start(client_pid) == client_start, "startup client incarnation changed"
        proc = pathlib.Path(f"/proc/{client_pid}")
        actual_argv = (proc / "cmdline").read_bytes().rstrip(b"\0").decode().split("\0")
        assert str((proc / "exe").readlink()) == identity["executable"] and actual_argv == client_argv, actual_argv
        actual_env = dict(value.split("=", 1) for value in (proc / "environ").read_bytes().decode().split("\0") if "=" in value)
        private_keys = ("HOME", "CODEX_HOME", "CODEX_SQLITE_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR")
        assert all(actual_env.get(key) == env[key] for key in private_keys)
        assert process_start(client_pid) == client_start, "startup client incarnation changed"
        evidence["client"] = {"pid": client_pid, "startTime": client_start, "argv": actual_argv,
            "executable": identity["executable"], "privatePaths": {key: env[key] for key in private_keys}}

    try:
        # Body40 + owned teardown15 is a fixed outer bound below one minute.
        async with asyncio.timeout_at(started + 40):
            helpers = runpy.run_path(model_script)  # Model import performs no writes.
            model_env = {**env, "CREATION_MODEL_ROOT": str(directory / "model"),
                         "CREATION_MODEL_PORT": "0", "CREATION_DISTINCT_PROVIDER_AUTH": "1"}
            listener = await launch([sys.executable, model_script], model_env, "model")
            deadline = time.monotonic() + 3
            while not (directory / "model/listen-port").exists():
                assert listener.returncode is None and time.monotonic() < deadline, "synthetic listener unavailable"
                await asyncio.sleep(0.025)
            config.write_text(startup_configuration(int((directory / "model/listen-port").read_text()), diagnostic))
            before_config = tomllib.loads(config.read_text())
            evidence["configBefore"] = before_config
            server_socket = str(directory / "app.native") if diagnostic else native_socket
            server = await launch([identity["executable"], "app-server", *identity["app_server_argv"],
                "--listen", "unix://" + server_socket], env, "native")
            deadline = time.monotonic() + 8
            while not pathlib.Path(server_socket).exists():
                assert server.returncode is None and time.monotonic() < deadline, "native startup unavailable"
                await asyncio.sleep(0.025)
            if diagnostic:
                (directory / "model/observe-materialization.json").write_text(json.dumps(
                    {"cwd": str(directory / "cwd"), "active": True}))
                transport = await launch([sys.executable, str(pathlib.Path(__file__).resolve()), "proxy",
                    native_socket, server_socket, str(directory / "model"), "startup"], env, "transport")
                while not pathlib.Path(native_socket).exists():
                    assert transport.returncode is None and time.monotonic() < deadline, "private transport unavailable"
                    await asyncio.sleep(0.025)
            connection = await websockets.unix_connect(native_socket, max_size=16 * 1024 * 1024)
            await asyncio.wait_for(initialize(connection), 4)
            numbers = itertools.count(2)

            async def call(method, **params):
                record = {"method": method, "params": params}
                evidence["rpc"].append(record)
                result = await asyncio.wait_for(rpc(connection, next(numbers), method, params), 4)
                record["result"] = result
                return result

            started_response = await call("thread/start", cwd=str(directory / "cwd"), model="gpt-5.5",
                modelProvider=provider, config={"model_reasoning_effort": "high"})
            evidence["startResponse"] = started_response
            effective_before = startup_effective_settings(started_response)
            thread = started_response["thread"]["id"]
            seed = "STARTUP_SEED_" + str(uuid.uuid4())
            before = await completed_seed_history(connection, numbers, thread, seed, evidence)
            rows = requests()
            assert len(rows) == 1 and rows[0]["provider"] == provider and rows[0]["authMatch"] is True, rows
            assert rows[0]["path"] == "/" + provider + "/v1/responses" and helpers["maintenance_request_matches"](rows[0]["body"], thread, seed)
            evidence["seedReceipt"] = rows[0]
            assert (before["model"], before["modelProvider"], before["reasoningEffort"]) == ("gpt-5.5", provider, "high"), before
            assert (effective_before["model"], effective_before["modelProvider"], effective_before["reasoningEffort"]) == ("gpt-5.5", provider, "high"), effective_before
            evidence["before"] = before
            if relinquish:
                # Loaded-thread subscribers can preserve mismatched overrides.
                # Control only this fixture's lifetime; this is not a forced
                # cold resume or full managed-client disconnect equivalence.
                await relinquish_seed(connection, numbers, thread, evidence)
                connection = None
            client_argv = [identity["executable"], "-c", 'model_provider="' + provider + '"',
                           "--remote", "unix://" + native_socket, "resume", thread]
            await tmux("new-session", "-d", "-x", "40", "-y", "24", "-s", "startup",
                "exec " + shlex.join(["env", "-i", *[key + "=" + value for key, value in env.items()], *client_argv]))
            deadline = time.monotonic() + 20
            tmux_pid = int(await tmux("display-message", "-p", "#{pid}"))
            tmux_start = process_start(tmux_pid)
            evidence["tmux"] = {"pid": tmux_pid, "startTime": tmux_start}
            pane = await tmux("display-message", "-p", "#{pane_id}")
            client_pid = int(await tmux("display-message", "-p", "#{pane_pid}"))
            client_start = process_start(client_pid)

            while True:
                observed = pane_observation(tmux_socket, pane, timeout=1)
                assert (observed["pane"]["pane_width"], observed["pane"]["pane_height"]) == ("40", "24"), observed
                if not evidence["panes"] or evidence["panes"][-1] != observed:
                    assert len(evidence["panes"]) < 32, "startup pane evidence bound"
                    evidence["panes"].append(observed)
                assert "Try new model" not in observed["visible"] and "Use existing model" not in observed["visible"], observed
                if diagnostic:
                    if startup_tui_readiness(directory / "model", thread, effective_before,
                            observed, evidence, time.monotonic(), deadline, record_client):
                        break
                else:
                    if composer_ready(observed):
                        break
                    assert time.monotonic() < deadline, ("startup empty composer unavailable", observed)
                await asyncio.sleep(0.1)
            if not client_capture_attempted:
                record_client()
            if diagnostic:
                # Observe the TUI's actual reply; a bare helper resume would
                # change the loaded settings before the passive read.
                effective_after = evidence["tuiBoundary"]["reply"]["effectiveSettings"]
            else:
                resumed_response = await asyncio.wait_for(call("thread/resume", threadId=thread),
                    max(0.001, deadline - time.monotonic()))
                evidence["resumeResponse"] = resumed_response
                effective_after = startup_effective_settings(resumed_response)
            evidence.update(effectiveBefore=effective_before, effectiveAfter=effective_after)
            if relinquish:
                # No helper observer may resubscribe between relinquishment
                # and the actual TUI boundary. Only now open the passive reader.
                assert connection is None and evidence["seedSubscription"]["closed"] is True
                connection = await asyncio.wait_for(websockets.unix_connect(native_socket, max_size=16 * 1024 * 1024),
                    min(4, max(0.001, deadline - time.monotonic())))
                await asyncio.wait_for(initialize(connection), min(4, max(0.001, deadline - time.monotonic())))
                numbers = itertools.count(2)
                evidence["postTuiReadConnection"] = {"initialized": True, "afterBoundary": True}
            after = (await asyncio.wait_for(call("thread/read", threadId=thread, includeTurns=True),
                max(0.001, deadline - time.monotonic())))["thread"]
            evidence["after"] = after
            evidence["configAfter"] = tomllib.loads(config.read_text())
            assert effective_after == effective_before, (effective_before, effective_after)
            keys = ("id", "cwd", "model", "modelProvider", "reasoningEffort", "projectId")
            assert {key: before[key] for key in keys} == {key: after[key] for key in keys}, after
            assert after["turns"] == before["turns"] and len(requests()) == 1, "startup inferred or changed history"
            after_config = tomllib.loads(config.read_text())
            assert all(after_config[key] == before_config[key] for key in ("model", "model_provider", "model_reasoning_effort", "features", "model_providers", "notice")), after_config
            assert time.monotonic() < deadline, "startup composer/settings deadline"
            result = "provider-only startup preservation PASS" if diagnostic else "startup-only PASS"
            if candidate is not None:
                result = "candidate raw-native " + mode + " preservation PASS"
            evidence.update(after=after, configAfter=after_config, seedReceipt=rows[0], result=result)
    except BaseException as error:
        evidence["error"] = {"type": type(error).__name__, "message": str(error)[:2000]}
        if diagnostic and client_pid is not None and not client_capture_attempted:
            # Capture the real invocation before owned teardown, including a
            # disagreeing protocol reply. A failed capture remains explicit;
            # never guess argv, retry it or replace the original failure.
            try:
                record_client()
            except Exception as capture_error:
                evidence["clientCaptureError"] = {"type": type(capture_error).__name__}
        raise
    finally:
        teardown_errors = []
        try:
            async with asyncio.timeout_at(started + 55):
                if connection is not None:
                    try:
                        await asyncio.wait_for(connection.close(), 1)
                    except Exception as error:
                        teardown_errors.append(("rpc-close", type(error).__name__))
                if pathlib.Path(tmux_socket).exists():
                    try:
                        await tmux("kill-server")
                    except Exception as error:
                        teardown_errors.append(("tmux-stop", type(error).__name__))
                for name, pid, start in (("client", client_pid, client_start),
                                         ("tmux", tmux_pid, tmux_start)):
                    if pid is not None:
                        try:
                            await stop_pid(pid, start)
                        except Exception as error:
                            teardown_errors.append((name, type(error).__name__))
                for process in reversed(processes):
                    try:
                        await stop(process)
                    except Exception as error:
                        teardown_errors.append((process.pid, type(error).__name__))
                if "helpers" in locals():
                    evidence["finalPostCount"] = len(requests())
                    if evidence.get("seedReceipt") and evidence["finalPostCount"] != 1:
                        teardown_errors.append(("model", "post-seed inference during teardown"))
                evidence["joined"] = not teardown_errors
        finally:
            for handle in handles:
                handle.close()
            evidence["elapsedSeconds"] = time.monotonic() - started
            evidence["teardownErrors"] = teardown_errors
            (directory / "receipt.json").write_text(json.dumps(evidence))
            print(json.dumps({"directory": str(directory), "result": evidence.get("result"), "joined": evidence.get("joined", False)}))
            assert evidence["elapsedSeconds"] < 60, "startup outer deadline exceeded"
            assert evidence.get("joined") is True, ("startup teardown failed", teardown_errors)


async def production_policy(package, model_script, fixture_path):
    """Fixture-only root entry, invoked by the disposable NixOS VM runner.

    The production loader reads guest /etc, even with a private CODEX_HOME.
    Stop/join native before restoring exact original policy files in finally.
    """
    marker = pathlib.Path("/etc/dev-workspaces/creation-policy-fixture")
    assert os.geteuid() == 0 and marker.read_text() == "session-creation-production-policy-v1\n"
    assert marker.stat().st_uid == 0 and pathlib.Path(fixture_path) == pathlib.Path("/tmp/creation-evidence/production-policy")
    identity = production_native(package)
    developer = pwd.getpwnam("developer")
    assert developer.pw_uid == 1000
    environment = {"XDG_RUNTIME_DIR": "/run/user/1000", "PATH": os.environ["PATH"]}
    services = ["workspace-codex@fixture.service", "workspace-portal@fixture.service",
                "workspace-codex-provider@fixture.service", "workspace-tmux@fixture.service",
                "workspace-router.service", "creation-transport.service"]
    for service in services:
        result = subprocess.check_output(["runuser", "-u", "developer", "--", "env", "-i",
            *[key + "=" + value for key, value in environment.items()], "systemctl", "--user", "show", service,
            "--property=MainPID,ControlPID,ActiveState"], text=True, timeout=5)
        values = dict(line.split("=", 1) for line in result.splitlines())
        assert values["MainPID"] == values["ControlPID"] == "0" and values["ActiveState"] in ("inactive", "failed"), (service, values)
    directory = pathlib.Path(fixture_path)
    directory.mkdir(mode=0o700)
    os.chown(directory, developer.pw_uid, developer.pw_gid)
    policy_dir = pathlib.Path("/etc/codex")
    existed = policy_dir.exists()
    originals = {}
    for path in (policy_dir / "requirements.toml", policy_dir / "managed_config.toml"):
        if path.exists() or path.is_symlink():
            info = path.lstat()
            assert stat.S_ISREG(info.st_mode), path
            originals[path] = (path.read_bytes(), info)
        else:
            originals[path] = None
    requirements = policy_dir / "requirements.toml"
    receipts, processes, joined = [], [], set()
    helpers = runpy.run_path(model_script)
    rows = helpers["request_rows"]
    holds = pathlib.Path("/tmp/creation-model/hold-turn")
    assert not holds.exists(), "ordinary fixture response hold still active"

    async def stop(process):
        # The group contains only this fixture's runuser/native children.
        if process in joined:
            return
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            await asyncio.wait_for(process.wait(), 5)
        except asyncio.TimeoutError:
            os.killpg(process.pid, signal.SIGKILL)
            await asyncio.wait_for(process.wait(), 5)
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            joined.add(process)
            return
        os.killpg(process.pid, signal.SIGKILL)
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                os.killpg(process.pid, 0)
            except ProcessLookupError:
                joined.add(process)
                return
            await asyncio.sleep(0.05)
        raise AssertionError("unjoined production fixture process group")

    def configuration(default="primary", missing=False):
        text = ('model_provider=' + json.dumps(default) + '\nmodel="gpt-5.5"\n'
                'approval_policy="never"\nsandbox_mode="danger-full-access"\n[features]\ngoals=true\n')
        for provider in (("alternate",) if missing else ("primary", "alternate")):
            text += definition(provider, provider)
        return text

    def definition(provider, route):
        return ('[model_providers.' + provider + ']\nname="Fixture ' + provider + '"\n'
                'base_url="http://127.0.0.1:2455/' + route + '/v1"\nwire_api="responses"\n'
                'env_key="CREATION_' + route.upper() + '_TOKEN"\nrequires_openai_auth=false\n'
                'supports_websockets=false\nrequest_max_retries=0\nstream_max_retries=0\n')

    async def scenario(label):
        started = time.monotonic()
        home = directory / label
        home.mkdir(mode=0o700)
        os.chown(home, developer.pw_uid, developer.pw_gid)
        config = home / "config.toml"
        config.write_text(configuration())
        os.chown(config, developer.pw_uid, developer.pw_gid)
        # A real managed selection overrides the deliberately conflicting user
        # default; route controls below must still reach primary before refusal.
        valid_policy = 'model_provider="primary"\n' if label.startswith("managed-") else ""
        requirements.write_text(valid_policy)
        (policy_dir / "managed_config.toml").unlink(missing_ok=True)
        if valid_policy:
            config.write_text(configuration(default="alternate"))
        socket = str(home / "app.sock")
        flags = [] if label == "strict-mismatch" else identity["app_server_argv"]
        env = {"HOME": str(home), "CODEX_HOME": str(home), "XDG_STATE_HOME": str(home / "state"),
               "XDG_CACHE_HOME": str(home / "cache"), "PATH": os.environ["PATH"],
               "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED": "1",
               "CREATION_PRIMARY_TOKEN": "fixture-primary-token", "CREATION_ALTERNATE_TOKEN": "fixture-alternate-token"}
        argv = ["runuser", "-u", "developer", "--", "env", "-i", *[key + "=" + value for key, value in env.items()],
                identity["executable"], "app-server", *flags, "--listen", "unix://" + socket]
        native = None
        connection = None
        log = (home / "native.log").open("wb")
        numbers = itertools.count(10)

        async def launch():
            nonlocal native, connection
            pathlib.Path(socket).unlink(missing_ok=True)
            native = await asyncio.create_subprocess_exec(*argv, cwd=home, stdout=log, stderr=log, start_new_session=True)
            processes.append(native)
            deadline = time.monotonic() + 20
            while not pathlib.Path(socket).exists():
                assert native.returncode is None, (label, "native exited", native.returncode)
                assert time.monotonic() < deadline, (label, "native startup deadline")
                await asyncio.sleep(0.05)
            connection = await websockets.unix_connect(socket, max_size=16 * 1024 * 1024)
            await asyncio.wait_for(initialize(connection), 10)

        async def call(method, **params):
            number = next(numbers)
            await connection.send(json.dumps({"id": number, "method": method, "params": params}))
            async with asyncio.timeout(10):
                while True:
                    result = json.loads(await connection.recv())
                    if result.get("id") == number:
                        return result

        async def ok(method, **params):
            result = await call(method, **params)
            assert "error" not in result, (label, method, result)
            return result["result"]

        def traffic():
            result = list(rows())
            assert all(row.get("authMatch") is True and row.get("path") == "/" + row["provider"] + "/v1/responses" for row in result), "wrong endpoint/auth"
            return len(result)

        async def snapshot(thread):
            history = (await ok("thread/read", threadId=thread, includeTurns=True))["thread"]
            goal = (await ok("thread/goal/get", threadId=thread))["goal"]
            return {"users": [item for turn in history["turns"] for item in turn["items"] if item.get("type") == "userMessage"],
                    "queue": await ok("thread/queue/list", threadId=thread),
                    "goal": stable_goal_fields(goal)}

        async def marker_turn(thread, marker, route, held=False):
            before = traffic()
            turn = (await ok("turn/start", threadId=thread, input=[{"type": "text", "text": marker}]))["turn"]["id"]
            deadline = time.monotonic() + 20
            while True:
                matches = [row for row in list(rows())[before:]
                           if helpers["maintenance_request_matches"](row["body"], thread, marker)]
                assert len(matches) <= 1, (label, "duplicate inference", marker)
                if matches:
                    assert matches[0]["provider"] == route and matches[0]["authMatch"] is True, matches
                    if held:
                        return turn
                    history = (await ok("thread/read", threadId=thread, includeTurns=True))["thread"]
                    if any(value["id"] == turn and value["status"] == "completed" for value in history["turns"]):
                        assert sum(item.get("type") == "userMessage" and any(part.get("text") == marker for part in item.get("content", []))
                                   for value in history["turns"] for item in value["items"]) == 1, history
                        return turn
                assert time.monotonic() < deadline, (label, "route control deadline")
                await asyncio.sleep(0.05)

        try:
            async with asyncio.timeout(90):
                await launch()
                thread = (await ok("thread/start", cwd=str(home), model="gpt-5.5"))["thread"]["id"]
                await marker_turn(thread, "POLICY_BEFORE_" + label, "primary")
                holds.touch()
                active = await marker_turn(thread, "POLICY_HELD_" + label, "primary", held=True)
                queued = (await ok("thread/queue/add", threadId=thread, clientUserMessageId="policy-queue-" + label,
                                   input=[{"type": "text", "text": "POLICY_QUEUE_" + label}]))["queuedSubmission"]
                await ok("thread/goal/set", threadId=thread, objective="POLICY_GOAL_" + label, status="active", origin="user")
                before = await snapshot(thread)
                assert before["goal"]["status"] == "active" and before["queue"]["data"] == [queued], before
                count = traffic()
                if label == "strict-mismatch":
                    config.write_text(configuration(default="alternate"))
                    expected = "modelProviderMismatch"
                elif label == "missing-definition":
                    config.write_text(configuration(default="alternate", missing=True))
                    expected = "modelProviderNotConfigured"
                elif label == "malformed-current":
                    config.write_text("invalid current config !!!")
                    expected = None
                elif label == "managed-selection":
                    requirements.write_text('model_provider="alternate"\n')
                    expected = "modelProviderPolicyConflict"
                else:
                    requirements.write_text('model_provider="primary"\n' + definition("primary", "alternate"))
                    expected = "modelProviderPolicyConflict"
                errors = []
                for method, params in [
                    ("thread/resume", {"modelProvider": "alternate", "config": {"allow_host_provider_switching": True}}),
                    ("turn/start", {"input": [{"type": "text", "text": "MUST_NOT_INSERT_" + label}]}),
                    ("thread/queue/start", {"queuedSubmissionId": queued["id"]}),
                    ("thread/goal/set", {"objective": "MUST_NOT_REPLACE", "status": "active"}),
                ]:
                    error = await call(method, threadId=thread, **params)
                    assert error.get("error", {}).get("code") == -32600, (label, method, error)
                    if expected:
                        assert error["error"].get("data", {}).get("type") == expected, (label, method, error)
                    errors.append({"method": method, "error": error["error"]})
                    assert await snapshot(thread) == before and traffic() == count, (label, "refusal mutated business state")
                if label == "missing-definition":
                    await connection.close()
                    await stop(native)
                    await launch()
                    cold = await snapshot(thread)
                    result = await call("thread/resume", threadId=thread, modelProvider="alternate")
                    assert result.get("error", {}).get("data", {}).get("type") == expected, result
                    assert await snapshot(thread) == cold and traffic() == count, "cold refusal mutated state/inference"
                    errors.append({"method": "cold thread/resume explicit override", "error": result["error"]})
                config.write_text(configuration(default="alternate") if valid_policy else configuration())
                requirements.write_text(valid_policy)
                restored = await ok("thread/resume", threadId=thread)
                assert restored["thread"]["id"] == thread and restored["modelProvider"] == "primary", restored
                # Preservation above precedes deliberate teardown of this case's
                # active goal/queue/turn; never replay them as recovery input.
                await ok("thread/goal/set", threadId=thread, status="paused")
                await ok("thread/queue/delete", threadId=thread, queuedSubmissionId=queued["id"])
                history = (await ok("thread/read", threadId=thread, includeTurns=True))["thread"]
                if any(turn["id"] == active and turn["status"] == "inProgress" for turn in history["turns"]):
                    await ok("turn/interrupt", threadId=thread, turnId=active)
                holds.unlink()
                deadline = time.monotonic() + 10
                while any(turn["status"] == "inProgress" for turn in (await ok("thread/read", threadId=thread, includeTurns=True))["thread"]["turns"]):
                    assert time.monotonic() < deadline, (label, "interrupted source turn did not settle")
                    await asyncio.sleep(0.05)
                await marker_turn(thread, "POLICY_AFTER_" + label, "primary")
                receipts.append({"scenario": label, "threadId": thread, "queueId": queued["id"], "before": before,
                                 "refusals": errors, "inferenceBeforeRefusal": count, "sameIDRecovered": True,
                                 "elapsedSeconds": time.monotonic() - started, "argv": [identity["executable"], "app-server", *flags]})
        except BaseException:
            log.flush()
            print(json.dumps({"policyFailure": label, "elapsedSeconds": time.monotonic() - started,
                              "logTail": (home / "native.log").read_bytes()[-4096:].decode(errors="replace")}))
            raise
        finally:
            holds.unlink(missing_ok=True)
            if connection is not None:
                await connection.close()
            if native is not None:
                await stop(native)
            log.close()

    try:
        policy_dir.mkdir(exist_ok=True)
        for label in ("strict-mismatch", "missing-definition", "malformed-current", "managed-selection", "managed-definition"):
            await scenario(label)
    finally:
        # Even an earlier cleanup error must not leave a private native process
        # observing the policy while originals are restored.
        for process in processes:
            await stop(process)
        for path, original in originals.items():
            if original is None:
                path.unlink(missing_ok=True)
                assert not path.exists()
            else:
                content, info = original
                path.write_bytes(content)
                os.chown(path, info.st_uid, info.st_gid)
                os.chmod(path, stat.S_IMODE(info.st_mode))
                os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns))
                restored = path.stat()
                assert path.read_bytes() == content and (restored.st_uid, restored.st_gid, stat.S_IMODE(restored.st_mode), restored.st_mtime_ns) == (info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode), info.st_mtime_ns)
        if not existed:
            policy_dir.rmdir()
        (directory / "receipt.json").write_text(json.dumps({"native": identity, "scenarios": receipts, "policyRestored": True, "processesJoined": True}, indent=2))
    assert len(receipts) == 5, receipts
    print(json.dumps({"scenarios": [receipt["scenario"] for receipt in receipts], "receipt": str(directory / "receipt.json")}))


def diagnostic_scalar(value):
    if value is None or isinstance(value, (bool, int, float)):
        return value
    if isinstance(value, str) and len(value.encode()) <= 256:
        return value
    return {"type": type(value).__name__}


def diagnostic_fields(value, names):
    return {name: diagnostic_scalar(value[name]) for name in names if name in value}


def diagnostic_envelope(value):
    result = {"type": type(value).__name__}
    if isinstance(value, dict):
        result["keys"] = [diagnostic_scalar(key) for key in sorted(value)[:32]]
    return result


def retry_state(state_path):
    with pathlib.Path(state_path).open("rb") as source:
        data = source.read(1024 * 1024 + 1)
    if len(data) > 1024 * 1024:
        raise ValueError("retry state exceeds metadata bound")
    state = json.loads(data)
    if state.get("schema") == 3:
        with sqlite3.connect(pathlib.Path(str(state_path) + ".sqlite").as_uri() + "?mode=ro", uri=True) as db:
            state = json.loads(db.execute("SELECT body FROM retry_state WHERE id=1").fetchone()[0])
    return state


def diagnostic_state(state_path):
    result = {"observedAt": time.time()}
    try:
        state = retry_state(state_path)
        if not isinstance(state, dict):
            return {**result, "error": {"stage": "state-shape", "type": type(state).__name__}}
        result.update(diagnostic_fields(state, ("schema", "epoch", "phase", "reason", "handled", "cancelled")))
        for key, fields in (("binding", ("epoch", "digest")),
                            ("managedTurn", ("turn", "count", "lastUserItemId", "digest", "bindingDigest")),
                            ("pending", ("id", "resultTurn"))):
            value = state.get(key)
            result[key] = {"present": value is not None, "type": type(value).__name__}
            if isinstance(value, dict):
                result[key].update(diagnostic_fields(value, fields))
        snapshots = state.get("snapshots")
        result["snapshots"] = {"type": type(snapshots).__name__}
        if isinstance(snapshots, list):
            result["snapshots"].update({"total": len(snapshots), "entries": [
                diagnostic_fields(row, ("id", "turn", "origin", "epoch", "proven", "textDigest", "bindingDigest"))
                if isinstance(row, dict) else {"type": type(row).__name__} for row in snapshots[:64]]})
    except Exception as error:
        result["error"] = {"stage": "state-read", "type": type(error).__name__}
    return result


def diagnostic_items(value):
    result = diagnostic_envelope(value)
    if not isinstance(value, dict):
        return result
    cursor = value.get("nextCursor")
    result["nextCursor"] = {"present": "nextCursor" in value, "type": type(cursor).__name__}
    if isinstance(cursor, str):
        result["nextCursor"].update({"length": len(cursor.encode()), "sha256": hashlib.sha256(cursor.encode()).hexdigest()})
    entries = value.get("data")
    result["dataType"] = type(entries).__name__
    if isinstance(entries, list):
        result["total"] = len(entries)
        result["entries"] = []
        for entry in entries[:64]:
            row = diagnostic_envelope(entry)
            if isinstance(entry, dict):
                row.update(diagnostic_fields(entry, ("turnId",)))
                item = entry.get("item")
                row["item"] = diagnostic_envelope(item)
                if isinstance(item, dict):
                    row["item"].update(diagnostic_fields(item, ("id", "type", "clientId")))
                    content = item.get("content")
                    row["item"]["contentType"] = type(content).__name__
                    if isinstance(content, list):
                        row["item"]["contentCount"] = len(content)
                        row["item"]["content"] = []
                        for part in content[:16]:
                            projected = {"type": type(part).__name__}
                            if isinstance(part, dict):
                                projected.update(diagnostic_fields(part, ("type",)))
                                text = part.get("text")
                                if isinstance(text, str):
                                    projected.update({"textLength": len(text.encode()), "textSha256": hashlib.sha256(text.encode()).hexdigest()})
                            row["item"]["content"].append(projected)
            result["entries"].append(row)
    return result


async def prefix_diagnostic(socket, thread, turn, state_path, expected_json, evidence_path):
    summary = {"schema": 1, "observedAt": time.time(), "expected": {"threadId": thread, "turnId": turn},
               "state": diagnostic_state(state_path)}
    stage = "expected"
    try:
        expected = json.loads(expected_json)
        summary["expected"].update({"count": len(expected), "inputs": [
            diagnostic_fields(row, ("clientId", "textSha256", "textLength")) for row in expected[:8]]})

        async def inspect():
            nonlocal stage
            stage = "connect"
            async with websockets.unix_connect(socket, max_size=16 * 1024 * 1024) as native:
                async def request(number, method, params):
                    await native.send(json.dumps({"id": number, "method": method, "params": params}))
                    while True:
                        response = json.loads(await native.recv())
                        if not isinstance(response, dict) or response.get("id") != number:
                            continue
                        if "error" in response:
                            error = response["error"]
                            return None, {"type": "RPCError", "code": error.get("code")
                                          if isinstance(error, dict) and isinstance(error.get("code"), int) else None}
                        if "result" not in response:
                            return None, {"type": "MissingResult"}
                        return response["result"], None

                stage = "initialize"
                _, error = await request(1, "initialize", {"clientInfo": {"name": "creation-proof", "version": "1"},
                    "capabilities": {"experimentalApi": True}})
                if error:
                    summary["error"] = {"stage": stage, **error}
                    return
                await native.send(json.dumps({"method": "initialized"}))
                stage = "thread/read"
                value, error = await request(2, stage, {"threadId": thread, "includeTurns": True})
                projected = diagnostic_envelope(value)
                if isinstance(value, dict) and isinstance(value.get("thread"), dict):
                    actual = value["thread"]
                    projected["thread"] = diagnostic_fields(actual, ("id",))
                    turns = actual.get("turns")
                    projected["turnsType"] = type(turns).__name__
                    if isinstance(turns, list):
                        projected["turns"] = [diagnostic_fields(row, ("id", "status"))
                                              if isinstance(row, dict) else {"type": type(row).__name__} for row in turns[:64]]
                summary["nativeThread"] = {"stage": stage, "projection": projected}
                if error:
                    summary["nativeThread"]["error"] = error
                stage = "thread/items/list"
                value, error = await request(3, stage, {"threadId": thread, "turnId": turn, "sortDirection": "asc", "limit": 64})
                summary["nativeItems"] = {"stage": stage, "projection": diagnostic_items(value)}
                if error:
                    summary["nativeItems"]["error"] = error

        # Leave room to encode the projection inside the caller's 10-second cap.
        await asyncio.wait_for(inspect(), timeout=8)
    except Exception as error:
        summary["error"] = {"stage": stage, "type": type(error).__name__}
    encoded = json.dumps(summary)
    if len(encoded.encode()) > 64 * 1024 - 1024:
        encoded = json.dumps({"schema": 1, "error": {"stage": "output", "type": "SizeLimit"}})
    try:
        pathlib.Path(evidence_path).write_text(encoded)
    except Exception as error:
        summary = json.loads(encoded)
        summary["evidenceError"] = {"stage": "evidence-write", "type": type(error).__name__}
        encoded = json.dumps(summary)
    print(encoded)


async def items_proof(socket, thread, turn, state_path, expected_json, evidence_path):
    expected = json.loads(expected_json)
    evidence = pathlib.Path(evidence_path)
    previous = json.loads(evidence.read_text()) if evidence.exists() else None
    async with websockets.unix_connect(socket, max_size=16 * 1024 * 1024) as native:
        await initialize(native)
        sequence = itertools.count(2)

        async def page(limit, cursor=None):
            params = {"threadId": thread, "turnId": turn, "sortDirection": "asc", "limit": limit}
            if cursor is not None:
                params["cursor"] = cursor
            result = await rpc(native, next(sequence), "thread/items/list", params)
            assert isinstance(result.get("data"), list) and len(result["data"]) <= limit, result
            next_cursor = result.get("nextCursor")
            assert next_cursor is None or isinstance(next_cursor, str) and next_cursor and next_cursor != cursor, result
            for entry in result["data"]:
                assert entry.get("turnId") == turn and isinstance(entry.get("item"), dict), entry
                item = entry["item"]
                assert isinstance(item.get("id"), str) and item["id"] and isinstance(item.get("type"), str), item
            return result

        # This small turn fits the exact production reader's limit. Compare its
        # actual native shape and chronology with deliberately tiny pages.
        full = await page(64)
        assert full.get("nextCursor") is None, full
        entries = full["data"]
        ids = [entry["item"]["id"] for entry in entries]
        assert len(ids) == len(set(ids)), ids
        collected, cursor, seen = [], None, set()
        tail_cursor, tail_start = None, 0
        for _ in range(32):
            chunk = await page(1, cursor)
            tail_cursor, tail_start = cursor, len(collected)
            collected.extend(chunk["data"])
            next_cursor = chunk.get("nextCursor")
            if next_cursor is None:
                break
            assert next_cursor not in seen, chunk
            seen.add(next_cursor)
            cursor = next_cursor
        else:
            raise AssertionError("small native turn did not finish paging")
        assert [entry["item"]["id"] for entry in collected] == ids, (collected, entries)
        users = []
        for entry in entries:
            item = entry["item"]
            if item["type"] != "userMessage":
                continue
            assert isinstance(item.get("content"), list), item
            assert all(part.get("type") == "text" and isinstance(part.get("text"), str) for part in item["content"]), item
            users.append({"itemId": item["id"], "clientId": item.get("clientId"),
                          "text": "\n".join(part["text"] for part in item["content"])})
        assert [{key: value[key] for key in ("clientId", "text")} for value in users] == expected, users
        if previous:
            assert previous["threadId"] == thread and previous["turnId"] == turn, previous
            assert ids[:len(previous["entryIds"])] == previous["entryIds"], (previous, ids)
            assert len(users) == previous["count"] + 1 and users[:-1] == previous["users"], users
            replay, cursor, seen = [], previous["tailCursor"], set()
            for _ in range(32):
                chunk = await page(2, cursor)
                replay.extend(chunk["data"])
                next_cursor = chunk.get("nextCursor")
                if next_cursor is None:
                    break
                assert next_cursor not in seen, chunk
                seen.add(next_cursor)
                cursor = next_cursor
            else:
                raise AssertionError("native tail replay did not finish")
            assert [entry["item"]["id"] for entry in replay] == ids[previous["tailStart"]:], replay

        # Read the actual wrapper's atomic record; never manufacture retry state.
        state = retry_state(state_path)
        managed = state["managedTurn"]
        assert state["scope"]["thread"] == thread and state["scope"]["socket"] == socket, state["scope"]
        assert managed["turn"] == turn and managed["count"] == len(users), managed
        assert state["handled"] != turn and state["pending"] is None, (state["handled"], state["pending"])
        scope = {key: state["scope"][key] for key in ("workspace", "slug", "root", "thread", "socket")}

        def digest(value):
            return hashlib.sha256(json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()

        chain = digest(["managed-turn/v1", scope, state["epoch"], state["binding"]["digest"], turn])
        for item in users:
            text_digest = hashlib.sha256(item["text"].encode()).hexdigest()
            chain = digest(["managed-turn/input/v1", chain, item["itemId"], item["clientId"], text_digest])
        assert managed["bindingDigest"] == state["binding"]["digest"] and managed["digest"] == chain, managed
        assert managed["lastUserItemId"] == users[-1]["itemId"], managed
        if previous:
            assert previous["digest"] != chain, previous
        result = {"schema": 1, "threadId": thread, "turnId": turn, "entryIds": ids, "users": users,
                  "count": len(users), "digest": chain, "tailCursor": tail_cursor, "tailStart": tail_start}
        evidence.write_text(json.dumps(result))
        print(json.dumps(result))


if __name__ == "__main__":
    if sys.argv[1] not in ("ready-identity", "materialization-proof"):
        import websockets
    if sys.argv[1] == "ready-identity":
        try:
            print(json.dumps(ready_identity(json.loads(pathlib.Path(sys.argv[2]).read_text()))))
        except (AssertionError, KeyError, TypeError, ValueError):
            print(json.dumps({"refused": True, "reason": "invalid-ready-authority"}))
            sys.exit(1)
    elif sys.argv[1] == "proxy":
        asyncio.run(proxy(*sys.argv[2:]))
    elif sys.argv[1] == "managed-client":
        managed_client(*sys.argv[2:])
    elif sys.argv[1] == "pane-observation":
        observed = pane_observation(*sys.argv[2:])
        print(json.dumps({**observed, "composerReady": composer_ready(observed)}))
    elif sys.argv[1] == "production-native":
        print(json.dumps(production_native(*sys.argv[2:])))
    elif sys.argv[1] == "production-policy":
        asyncio.run(production_policy(*sys.argv[2:]))
    elif sys.argv[1] == "startup-notice":
        asyncio.run(startup_notice(*sys.argv[2:]))
    elif sys.argv[1] == "materialization-proof":
        try:
            print(json.dumps(materialization_proof(sys.argv[2], json.loads(sys.argv[3]))))
        except Exception as error:
            print(json.dumps({"error": type(error).__name__, "detail": str(error)[:4000]}))
            raise
    elif sys.argv[1] == "request":
        asyncio.run(asyncio.wait_for(fixture_request(*sys.argv[2:]), timeout=20))
    elif sys.argv[1] == "items-proof":
        asyncio.run(items_proof(*sys.argv[2:]))
    elif sys.argv[1] == "prefix-diagnostic":
        asyncio.run(prefix_diagnostic(*sys.argv[2:]))
    else:
        asyncio.run(read(*sys.argv[2:]))
