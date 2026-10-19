"""Transparent native transport fault and read-only proof, never RPC stubs.

The proxy holds the actual successful turn/start response after Codex accepted
the turn. Killing the portal at this barrier exercises an ambiguous initial send.
"""
import asyncio
import json
import pathlib
import sys
import tempfile

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


async def read(socket, identity):
    async with websockets.unix_connect(socket, max_size=16 * 1024 * 1024) as native:
        async def rpc(number, method, params):
            await native.send(json.dumps({"id": number, "method": method, "params": params}))
            while True:
                result = json.loads(await native.recv())
                if result.get("id") == number:
                    assert "error" not in result, result
                    return result["result"]

        await rpc(1, "initialize", {"clientInfo": {"name": "creation-proof", "version": "1"},
                                    "capabilities": {"experimentalApi": True}})
        await native.send(json.dumps({"method": "initialized"}))
        if identity == "inventory":
            metadata = await rpc(2, "thread/list", {"limit": 100,
                "modelProviders": ["primary", "alternate"], "sourceKinds": ["vscode"]})
        else:
            metadata = await rpc(2, "thread/read", {"threadId": identity, "includeTurns": True})
        print(json.dumps(metadata))


if sys.argv[1] == "proxy":
    asyncio.run(proxy(*sys.argv[2:]))
else:
    asyncio.run(read(*sys.argv[2:]))
