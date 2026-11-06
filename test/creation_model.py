"""External Responses fixture for packaged creation and archive acceptance.

No portal, receipt, initializer or native App Server is replaced. Control files
hold inference at an external boundary; requests remain observable on failure.
"""
import base64
import errno
import gzip
import http.server
import itertools
import json
import os
import pathlib
import sys
import threading
import time
import urllib.parse
import zlib

ROOT = pathlib.Path("/tmp/creation-model")
ROOT.mkdir(exist_ok=True)
sequence = itertools.count(1)
lock = threading.Lock()


def request_evidence(request):
    # Keep the fields read by the fixture, without narrowing input contents.
    return {key: request[key] for key in ("model", "input", "client_metadata") if key in request}


def request_rows(path=ROOT / "requests.jsonl"):
    """Read one finite append-only prefix, rejecting corrupt complete records."""
    def invalid_constant(_value):
        raise ValueError("non-JSON constant in request evidence")

    line, offset = 1, 0
    try:
        with path.open("rb") as source:
            size = os.fstat(source.fileno()).st_size
            while offset < size:
                raw = source.readline(size - offset)
                if not raw:
                    raise OSError("request evidence ended before captured size")
                if not raw.endswith(b"\n"):
                    break
                row = json.loads(raw.decode("utf-8"), parse_constant=invalid_constant)
                if (not isinstance(row, dict) or type(row.get("index")) is not int
                        or not isinstance(row.get("provider"), str)
                        or type(row.get("overload")) is not bool or type(row.get("retryAdvice")) is not bool):
                    raise ValueError("invalid request evidence row shape")
                if "bodyEncoding" in row:
                    if row["bodyEncoding"] != "gzip+base64-v1" or not isinstance(row.get("body"), str):
                        raise ValueError("invalid request evidence body encoding")
                    body = gzip.decompress(base64.b64decode(row["body"], validate=True))
                    row["body"] = json.loads(body.decode("utf-8"), parse_constant=invalid_constant)
                    del row["bodyEncoding"]
                if not isinstance(row.get("body"), dict):
                    raise ValueError("invalid request evidence body shape")
                yield row
                offset += len(raw)
                line += 1
    except (OSError, UnicodeError, ValueError, EOFError, zlib.error) as error:
        raise ValueError(f"model request evidence line {line} offset {offset}: {type(error).__name__}") from error


def maintenance_request_matches(request, thread_id, marker):
    metadata = request.get("client_metadata")
    if not isinstance(metadata, dict) or metadata.get("thread_id") != thread_id:
        return False
    return any(marker in part["text"].splitlines()
               for item in request.get("input", []) if isinstance(item, dict) and item.get("role") == "user"
               for part in item.get("content", []) if isinstance(part, dict)
               and part.get("type") == "input_text" and isinstance(part.get("text"), str))


def maintenance_request_held(request):
    try:
        rules = json.loads((ROOT / "hold-maintenance.json").read_text())
    except FileNotFoundError:
        return False
    if (not isinstance(rules, list) or len(rules) != 2
            or any(not isinstance(rule, dict) or set(rule) != {"threadId", "marker"}
                   or any(not isinstance(rule[key], str) or not rule[key] for key in rule)
                   or rule["marker"].splitlines() != [rule["marker"]] for rule in rules)
            or len({rule["threadId"] for rule in rules}) != 2):
        raise ValueError("invalid maintenance hold rules")
    return any(maintenance_request_matches(request, rule["threadId"], rule["marker"]) for rule in rules)


def wait_for_turn_response(index, request):
    while (ROOT / "hold-turn").exists() or maintenance_request_held(request):
        try:
            (ROOT / f"release-turn-{index}").unlink()
        except FileNotFoundError:
            time.sleep(0.05)
        else:
            return


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def json(self, value):
        data = json.dumps(value).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        if path.endswith("/models"):
            while (ROOT / "hold-models").exists():
                (ROOT / "models-entered").touch()
                time.sleep(0.05)
            self.json({"object": "list", "data": [{"id": name, "object": "model"}
                      for name in ["gpt-5.5", "gpt-6-luna"]]})
        elif path.endswith("/usage"):
            self.json({})
        else:
            self.send_error(404)

    def do_POST(self):
        if not self.path.endswith("/responses"):
            self.send_error(404)
            return
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        index = next(sequence)
        provider = self.path.split("/")[1]
        naming = request.get("model") == "gpt-6-luna"
        overload = False
        retry_advice = naming and (ROOT / "naming-retry-advice").exists()
        if naming:
            # ServerOverloaded is terminal without structured server advice.
            # The labelled native-progress case supplies advice and a ceiling.
            failures = ROOT / "naming-overloads"
            with lock:
                remaining = int(failures.read_text()) if failures.exists() else 0
                if remaining:
                    failures.write_text(str(remaining - 1))
                    overload = True
        with lock:
            # Encode each full body independently, preserving outer newline commits.
            body = json.dumps(request_evidence(request)).encode("utf-8")
            encoded = base64.b64encode(gzip.compress(body, compresslevel=1, mtime=0)).decode("ascii")
            serialized = json.dumps({"index": index, "provider": provider, "body": encoded,
                "overload": overload, "retryAdvice": retry_advice, "bodyEncoding": "gzip+base64-v1"}) + "\n"
            journal = ROOT / "requests.jsonl"
            try:
                with journal.open("a") as log:
                    log.write(serialized)
            except OSError as error:
                if error.errno == errno.ENOSPC:
                    # Sample metadata only; failed evidence never authorizes a response.
                    metadata = request.get("client_metadata")
                    metadata = metadata if isinstance(metadata, dict) else {}
                    evidence = {"event": "model-evidence-enospc", "sampledAtUnix": time.time(),
                        "index": index, "model": request.get("model"),
                        "threadId": metadata.get("thread_id"), "turnId": metadata.get("turn_id"),
                        "serializedAsciiBytes": len(serialized)}
                    try:
                        stat = journal.stat()
                        evidence["journal"] = {"available": True, "st_size": stat.st_size,
                            "st_blocks": stat.st_blocks, "st_dev": stat.st_dev}
                    except Exception as diagnostic_error:
                        evidence["journal"] = {"available": False, "error": type(diagnostic_error).__name__}
                    try:
                        statvfs = os.statvfs(journal.parent)
                        evidence["filesystem"] = {"available": True, **{key: getattr(statvfs, key)
                            for key in ("f_bsize", "f_frsize", "f_blocks", "f_bfree", "f_bavail", "f_files", "f_ffree", "f_favail")}}
                    except Exception as diagnostic_error:
                        evidence["filesystem"] = {"available": False, "error": type(diagnostic_error).__name__}
                    try:
                        print(json.dumps(evidence), file=sys.stderr, flush=True)
                    except Exception:
                        pass  # Even unavailable stderr must not replace the original write error.
                raise
        inputs = request.get("input", [])
        last_user = next((item for item in reversed(inputs)
                          if isinstance(item, dict) and item.get("role") == "user"), {})
        repair = (ROOT / "archive-repair-command").exists() and "The user requested archival of this session." in json.dumps(last_user)
        repair_tool = None
        if repair and not (ROOT / "archive-repair-submitted").exists():
            def find_shell(tools, namespace=""):
                for tool in tools:
                    if tool.get("type") == "namespace":
                        found = find_shell(tool.get("tools", []), tool["name"])
                        if found:
                            return found
                    if tool.get("name") in ("exec_command", "shell_command"):
                        return tool["name"], namespace
                return None
            shell = find_shell(request.get("tools", []))
            if shell:
                name, namespace = shell
                command = (ROOT / "archive-repair-command").read_text()
                repair_tool = {"type": "function_call", "name": name,
                               "call_id": "archive-repair-call",
                               "arguments": json.dumps({"cmd" if name == "exec_command" else "command": command})}
                if namespace:
                    repair_tool["namespace"] = namespace
                (ROOT / "archive-repair-submitted").touch()
                (ROOT / "archive-repair-entered").touch()
        if naming:
            text = "invalid naming output" if (ROOT / "bad-name").exists() else json.dumps({"name": "accepted-session"})
        elif "CREATION_PLAN_REQUEST" in json.dumps(last_user):
            text = "<proposed_plan>\n# Creation acceptance plan\n\nReturn the CREATION_PLAN_DONE sentinel.\n</proposed_plan>"
        else:
            text = "CREATION_REPLY_" + provider
        if not naming:
            (ROOT / "turn-entered").touch()
            wait_for_turn_response(index, request)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        def event(value):
            self.wfile.write(("event: " + value["type"] + "\ndata: " + json.dumps(value) + "\n\n").encode())
            self.wfile.flush()

        try:
            identity = "response-" + str(index)
            if overload:
                failure = {"code": "server_is_overloaded", "message": "fixture overload"}
                if retry_advice:
                    failure["headers"] = {"retry-after": "1"}
                event({"type": "response.failed", "response": {"id": identity,
                    "status": "failed", "error": failure}})
                return
            event({"type": "response.created", "response": {"id": identity}})
            event({"type": "response.output_item.done", "item": repair_tool or {
                "type": "message", "role": "assistant", "id": "answer-" + identity,
                "phase": "final_answer", "content": [{"type": "output_text", "text": text}]}})
            event({"type": "response.completed", "response": {"id": identity,
                "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
        except (BrokenPipeError, ConnectionResetError):
            pass


if __name__ == "__main__":
    http.server.ThreadingHTTPServer(("127.0.0.1", 2455), Handler).serve_forever()
