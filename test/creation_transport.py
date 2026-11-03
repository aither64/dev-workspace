"""Transparent native transport fault and read-only proof, never RPC stubs.

The proxy holds the actual successful turn/start response after Codex accepted
the turn. Killing the portal at this barrier exercises an ambiguous initial send.
"""
import asyncio
import hashlib
import itertools
import json
import pathlib
import sys
import tempfile
import time

import websockets


async def proxy(listen, target):
    async def connection(client):
        async with websockets.unix_connect(target, max_size=16 * 1024 * 1024) as native:
            starts = {}

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
                    await native.send(data)

            async def downstream():
                async for data in native:
                    message = json.loads(data)
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


def diagnostic_state(state_path):
    result = {"observedAt": time.time()}
    try:
        with pathlib.Path(state_path).open("rb") as source:
            data = source.read(1024 * 1024 + 1)
        if len(data) > 1024 * 1024:
            return {**result, "error": {"stage": "state-read", "type": "SizeLimit"}}
        state = json.loads(data)
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
        state = json.loads(pathlib.Path(state_path).read_text())
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
    elif sys.argv[1] == "items-proof":
        asyncio.run(items_proof(*sys.argv[2:]))
    elif sys.argv[1] == "prefix-diagnostic":
        asyncio.run(prefix_diagnostic(*sys.argv[2:]))
    else:
        asyncio.run(read(*sys.argv[2:]))
