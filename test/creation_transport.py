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

import websockets


async def proxy(listen, target):
    async def connection(client):
        async with websockets.unix_connect(target, max_size=16 * 1024 * 1024) as native:
            starts = {}
            archives = {}
            archive_rule = pathlib.Path("/tmp/creation-model/drop-archive-reply.json")

            async def upstream():
                async for data in client:
                    message = json.loads(data)
                    if (message.get("method") == "thread/start" and
                            message["params"].get("cwd", "").endswith("-before")):
                        while pathlib.Path("/tmp/creation-model/hold-root-request").exists():
                            pathlib.Path("/tmp/creation-model/root-entered").touch()
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
                    rule = archives.pop(message.get("id"), None)
                    if rule and archive_rule.exists() and json.loads(archive_rule.read_text()) == rule:
                        receipt = {**rule, "requestId": message["id"], "response": message,
                                   "nativeSuccess": "result" in message and "error" not in message}
                        assert len(json.dumps(receipt).encode()) <= 16384, "archive response receipt exceeds bound"
                        # Claim/disarm before closing anything; all other pairs
                        # and the listening socket continue forwarding unchanged.
                        archive_rule.rename(archive_rule.with_suffix(".consumed.json"))
                        with tempfile.NamedTemporaryFile(mode="w", dir="/tmp/creation-model", delete=False) as marker:
                            json.dump(receipt, marker)
                        pathlib.Path(marker.name).replace("/tmp/creation-model/archive-reply-receipt.json")
                        if receipt["nativeSuccess"]:
                            await client.close()
                            await native.close()
                            return
                    if message.get("id") in starts and "result" in message:
                        with tempfile.NamedTemporaryFile(mode="w", dir="/tmp/creation-model", delete=False) as marker:
                            json.dump({"threadId": starts[message["id"]],
                                "turnId": message["result"]["turn"]["id"]}, marker)
                        pathlib.Path(marker.name).replace("/tmp/creation-model/send-accepted")
                        while pathlib.Path("/tmp/creation-model/hold-send-reply").exists():
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


async def rpc(native, number, method, params):
    await native.send(json.dumps({"id": number, "method": method, "params": params}))
    while True:
        result = json.loads(await native.recv())
        if result.get("id") == number:
            assert "error" not in result, result
            return result["result"]


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
    assert codex["native"] == "/nix/store/z8wnwbwag81gyrc64pmyva3p3xh7g3w2-codex-0.167.0", codex
    provenance = codex["provenance"]
    assert provenance["revision"] == "902b743459a7c9ea00cb0b679129a28c8a888563", provenance
    assert provenance["sourcePath"] == "/nix/store/pmzj4n8c01yb674pb576p8km62920bdb-source", provenance
    assert provenance["recipeRevision"] == "d960982d3b05c294f3cf98cab3e23798548a0ced", provenance
    binary = (pathlib.Path(codex["native"]) / "bin/codex").resolve(strict=True)
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    assert digest == "5830f7b53034ecce5b92618a453db3b87d704c8ea8a4b956ce371b0dbdd5933a", digest
    assert hashlib.sha256((pathlib.Path(package) / "libexec/codex/libexec/codex/bin/codex").resolve().read_bytes()).hexdigest() == digest
    return {"package": package, "executable": str(binary), "sha256": digest, **codex}


def stable_goal_fields(goal):
    return {key: goal[key] for key in ("threadId", "objective", "status", "tokenBudget", "createdAt")}


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
    if sys.argv[1] == "proxy":
        asyncio.run(proxy(*sys.argv[2:]))
    elif sys.argv[1] == "managed-client":
        managed_client(*sys.argv[2:])
    elif sys.argv[1] == "production-native":
        print(json.dumps(production_native(*sys.argv[2:])))
    elif sys.argv[1] == "production-policy":
        asyncio.run(production_policy(*sys.argv[2:]))
    elif sys.argv[1] == "request":
        asyncio.run(asyncio.wait_for(fixture_request(*sys.argv[2:]), timeout=20))
    elif sys.argv[1] == "items-proof":
        asyncio.run(items_proof(*sys.argv[2:]))
    elif sys.argv[1] == "prefix-diagnostic":
        asyncio.run(prefix_diagnostic(*sys.argv[2:]))
    else:
        asyncio.run(read(*sys.argv[2:]))
