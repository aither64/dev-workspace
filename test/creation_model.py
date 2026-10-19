"""External Responses fixture for the packaged creation acceptance only.

No portal, receipt, initializer or native App Server is replaced. Control files
hold inference at an external boundary; requests remain observable on failure.
"""
import http.server
import itertools
import json
import pathlib
import threading
import time

ROOT = pathlib.Path("/tmp/creation-model")
ROOT.mkdir(exist_ok=True)
sequence = itertools.count(1)
lock = threading.Lock()


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
        if self.path.endswith("/models"):
            while (ROOT / "hold-models").exists():
                (ROOT / "models-entered").touch()
                time.sleep(0.05)
            self.json({"object": "list", "data": [{"id": name, "object": "model"}
                      for name in ["gpt-5.5", "gpt-6-luna"]]})
        elif self.path.endswith("/usage"):
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
        with lock:
            with (ROOT / "requests.jsonl").open("a") as log:
                log.write(json.dumps({"index": index, "provider": provider, "body": request}) + "\n")
        naming = request.get("model") == "gpt-6-luna"
        inputs = request.get("input", [])
        last_user = next((item for item in reversed(inputs)
                          if isinstance(item, dict) and item.get("role") == "user"), {})
        if naming:
            text = "invalid naming output" if (ROOT / "bad-name").exists() else json.dumps({"name": "accepted-session"})
        elif "CREATION_PLAN_REQUEST" in json.dumps(last_user):
            text = "<proposed_plan>\n# Creation acceptance plan\n\nReturn the CREATION_PLAN_DONE sentinel.\n</proposed_plan>"
        else:
            text = "CREATION_REPLY_" + provider
        if not naming:
            (ROOT / "turn-entered").touch()
            while (ROOT / "hold-turn").exists():
                time.sleep(0.05)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        def event(value):
            self.wfile.write(("data: " + json.dumps(value) + "\n\n").encode())
            self.wfile.flush()

        try:
            identity = "response-" + str(index)
            event({"type": "response.created", "response": {"id": identity}})
            event({"type": "response.output_item.done", "item": {
                "type": "message", "role": "assistant", "id": "answer-" + identity,
                "phase": "final_answer", "content": [{"type": "output_text", "text": text}]}})
            event({"type": "response.completed", "response": {"id": identity,
                "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
        except (BrokenPipeError, ConnectionResetError):
            pass


http.server.ThreadingHTTPServer(("127.0.0.1", 2455), Handler).serve_forever()
