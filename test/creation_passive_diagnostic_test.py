"""Exercise the actual failure-only producer and relay without a guest or RPC."""

import contextlib
import copy
import datetime
import io
import json
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
import textwrap
from types import SimpleNamespace
import unittest
from unittest import mock


SOURCE = (
    Path(__file__).resolve().parents[1] / "nix/tests/session-creation.nix"
).read_text()


def unique(pattern):
    matches = re.findall(pattern, SOURCE, re.DOTALL)
    if len(matches) != 1:
        raise AssertionError("passive diagnostic extraction must be unique")
    return textwrap.dedent(matches[0])


HELPER = Path(__file__).with_name("creation_passive_diagnostic.py")
PRODUCER = HELPER.read_text()
PARENT = unique(
    r'(            diagnostic_status, diagnostic_output = machine\.execute\(.*?)(?=            print\(user\("journalctl)'
)
TAIL = unique(
    r"(        try:\n            # These reads occur after the browser failure,.*?)(?=    finally:\n)"
)
ROOT = "01a1cb99-3cc6-7383-8856-7ba3a1938a13"
REQUEST = "31de92a0-336f-47b2-a4f2-4beb021407bf"
RECEIPT = "11de92a0-336f-47b2-a4f2-4beb021407bf"
SLUG = "2026-11-11-alternate"
STAMP = "2026-11-11T17:14:08.533Z"
SECRET = "PRIVATE_BODY_TOKEN_DO_NOT_EMIT"


class Clock(datetime.datetime):
    @classmethod
    def now(cls, tz=None):
        return cls(2026, 11, 11, 17, 14, 10, tzinfo=tz)


class Output(io.StringIO):
    flushed = False

    def flush(self):
        self.flushed = True
        super().flush()


class PassiveDiagnosticTest(unittest.TestCase):
    def setUp(self):
        self.failure = {
            "at": STAMP,
            "capturedAt": STAMP,
            "name": "alternate",
            "record": {
                "slug": SLUG,
                "canonicalUrl": "/" + SLUG + "/",
                "threadId": ROOT,
                "requestId": REQUEST,
                "receiptId": RECEIPT,
                "provider": "alternate",
            },
        }
        self.native = {
            "thread": {
                "id": ROOT,
                "modelProvider": "alternate",
                "turns": [
                    {
                        "id": "initial-turn",
                        "status": "completed",
                        "items": [
                            {
                                "type": "userMessage",
                                "content": [
                                    {
                                        "type": "text",
                                        "text": "CREATION_INITIAL_alternate",
                                    }
                                ],
                            },
                            {
                                "type": "agentMessage",
                                "text": "CREATION_REPLY_alternate",
                            },
                        ],
                    }
                ],
            }
        }
        self.portal = {
            "threadId": ROOT,
            "modelProvider": "alternate",
            "status": "idle",
            "hasOlder": False,
            "entries": [
                {
                    "kind": "userMessage",
                    "text": "CREATION_INITIAL_alternate",
                    "turnId": "initial-turn",
                    "turnStatus": "completed",
                },
                {
                    "kind": "agentMessage",
                    "text": "CREATION_REPLY_alternate",
                    "turnId": "initial-turn",
                    "turnStatus": "completed",
                },
            ],
        }

    def produce(
        self,
        *,
        native_error=None,
        native_exit=0,
        native_raw=None,
        portal_error=None,
        portal_exit=0,
        http="200",
        portal_raw=None,
        artifact_error=False,
        monotonic=(10, 10.25, 20, 20.5),
        failure_raw=None,
    ):
        calls, output = [], Output()

        def run(command, **kwargs):
            calls.append((command, kwargs))
            if len(calls) == 1:
                if native_error:
                    raise native_error
                return SimpleNamespace(
                    returncode=native_exit,
                    stdout=json.dumps(self.native)
                    if native_raw is None
                    else native_raw,
                    stderr=SECRET,
                )
            if portal_error:
                raise portal_error
            body = json.dumps(self.portal) if portal_raw is None else portal_raw
            return SimpleNamespace(
                returncode=portal_exit, stdout=body + "\n" + http, stderr=SECRET
            )

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "browser-failure.json").write_text(
                json.dumps(self.failure) if failure_raw is None else failure_raw
            )
            path_type, write_text = Path, Path.write_text

            def write(path, data, *args, **kwargs):
                if artifact_error and path.name == "post-failure-passive.json":
                    self.assertTrue(
                        output.flushed, "summary must flush before artifact write"
                    )
                    self.assertTrue(output.getvalue().endswith("\n"))
                    raise OSError(SECRET)
                return write_text(path, data, *args, **kwargs)

            error = None
            with (
                mock.patch(
                    "pathlib.Path",
                    side_effect=lambda value: (
                        root if value == "/tmp/creation-evidence" else path_type(value)
                    ),
                ),
                mock.patch.object(path_type, "write_text", write),
                mock.patch("subprocess.run", side_effect=run),
                mock.patch("datetime.datetime", Clock),
                mock.patch("time.monotonic", side_effect=monotonic),
                mock.patch.object(
                    sys,
                    "argv",
                    [
                        str(HELPER),
                        "/runtime",
                        "/driver/python",
                        "/fixture/transport.py",
                    ],
                ),
                contextlib.redirect_stdout(output),
            ):
                try:
                    exec(compile(PRODUCER, str(HELPER), "exec"), {})
                except Exception as caught:
                    error = caught
            artifact = root / "post-failure-passive.json"
            saved = json.loads(artifact.read_text()) if artifact.exists() else None
        return output.getvalue(), calls, error, saved

    def relay(self, output, status=0):
        machine = mock.Mock()
        machine.execute.return_value = (status, output)
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            exec(
                compile(PARENT, "actual-passive-parent", "exec"),
                {
                    "machine": machine,
                    "user_prefix": "",
                    "shlex": shlex,
                    "json": json,
                    "runtime": "/runtime",
                },
            )
        command = shlex.split(machine.execute.call_args.args[0])
        inner = shlex.split(command[2])
        self.assertEqual(command[:2], ["sh", "-c"])
        self.assertEqual(
            inner,
            [
                "${python}/bin/python",
                "${../../test/creation_passive_diagnostic.py}",
                "/runtime",
                "${python}/bin/python",
                "${../../test/creation_transport.py}",
            ],
        )
        return json.loads(printed.getvalue()), printed.getvalue()

    def assess(self, **kwargs):
        output, calls, error, saved = self.produce(**kwargs)
        final, printed = self.relay(output, int(error is not None))
        self.assertNotIn(SECRET, printed)
        return final["postFailurePassiveSummary"], calls, error, saved

    def test_completed_actual_reads_and_complete_parent_output(self):
        output, calls, error, saved = self.produce()
        self.assertIsNone(error)
        self.assertGreater(len(output.encode()), 1000)
        self.assertLessEqual(len(output.rstrip("\n").encode()), 8192)
        self.assertEqual(
            calls,
            [
                (
                    [
                        "/driver/python",
                        "/fixture/transport.py",
                        "read",
                        "/runtime/app-server.sock",
                        ROOT,
                    ],
                    {"capture_output": True, "text": True, "timeout": 8},
                ),
                (
                    [
                        "curl",
                        "--silent",
                        "--show-error",
                        "--max-time",
                        "8",
                        "--unix-socket",
                        "/runtime/portal.sock",
                        "--write-out",
                        "\n%{http_code}",
                        "http://localhost/api/sessions/" + SLUG + "/thread/page",
                    ],
                    {"capture_output": True, "text": True, "timeout": 8},
                ),
            ],
        )
        final, printed = self.relay(output)
        summary = final["postFailurePassiveSummary"]
        self.assertEqual(summary, json.loads(output)["postFailurePassiveSummary"])
        self.assertEqual(final["postFailureDiagnosticStatus"], 0)
        self.assertNotIn(SECRET, printed)
        self.assertEqual(
            saved["reads"][0]["stderr"], SECRET
        )  # Existing artifact semantics.
        for kind in ("native", "portal"):
            self.assertEqual(summary[kind]["outcome"], "ok")
            self.assertEqual(summary[kind]["initialTurnStatus"], "completed")
            self.assertEqual(summary[kind]["replyInInitialTurnCount"], 1)
        self.assertEqual(summary["identity"]["requestId"], REQUEST)
        self.assertEqual(summary["identity"]["receiptId"], RECEIPT)
        self.assertEqual(summary["timing"]["failureAt"], STAMP)
        self.assertEqual(summary["timing"]["capturedAt"], STAMP)

    def test_embedded_marker_token_rejects_extended_tokens_and_metadata(self):
        user = self.native["thread"]["turns"][0]["items"][0]
        for text, count in (
            ("Startup context\nGoal: CREATION_INITIAL_alternate\nEnd", 1),
            ("CREATION_INITIAL_alternate-extra", 0),
            ("CREATION_INITIAL_alternate_extra", 0),
            ("prefixCREATION_INITIAL_alternate", 0),
            ("CREATION_INITIAL_alternate2", 0),
        ):
            with self.subTest(text=text):
                user["content"][0]["text"] = text
                user["metadata"] = "CREATION_INITIAL_alternate"
                self.portal["entries"][0]["text"] = text
                self.portal["entries"][0]["metadata"] = "CREATION_INITIAL_alternate"
                summary, _, _, _ = self.assess()
                for kind in ("native", "portal"):
                    self.assertEqual(summary[kind]["initialCount"], count)

    def test_missing_optional_identity_and_provider_metadata_stays_unknown(self):
        for key in ("requestId", "receiptId"):
            self.failure["record"].pop(key)
        del self.native["thread"]["modelProvider"]
        del self.portal["modelProvider"]
        summary, calls, _, _ = self.assess()
        self.assertEqual(len(calls), 2)
        self.assertIsNone(summary["identity"]["requestId"])
        self.assertIsNone(summary["identity"]["receiptId"])
        for kind in ("native", "portal"):
            self.assertEqual(summary[kind]["outcome"], "ok")
            self.assertIsNone(summary[kind]["providerMatches"])
        self.failure = {}
        summary, calls, _, _ = self.assess()
        self.assertEqual(calls, [])
        self.assertEqual(summary["identity"]["state"], "invalid")
        self.assertEqual(summary["native"]["outcome"], "skipped")

    def test_initial_turn_status_is_not_thread_status_or_retained_tail(self):
        for status in (
            "completed",
            "inProgress",
            "failed",
            "interrupted",
            SECRET,
            None,
        ):
            with self.subTest(status=status):
                self.native["thread"]["turns"][0]["status"] = status
                for entry in self.portal["entries"]:
                    entry["turnStatus"] = status
                summary, _, _, _ = self.assess()
                expected = (
                    status
                    if status in ("completed", "inProgress", "failed", "interrupted")
                    else None
                )
                self.assertEqual(summary["native"]["initialTurnStatus"], expected)
                self.assertEqual(summary["portal"]["initialTurnStatus"], expected)
        self.native["thread"]["turns"] += [
            {
                "id": str(index),
                "status": "completed",
                "items": [{"type": "agentMessage", "text": SECRET}],
            }
            for index in range(30)
        ]
        summary, _, _, _ = self.assess()
        self.assertEqual(summary["native"]["entryCount"], 32)
        self.assertEqual(summary["native"]["replyInInitialTurnCount"], 1)

    def test_different_turn_reply_and_duplicates_do_not_certify_exchange(self):
        initial = self.native["thread"]["turns"][0]
        reply = initial["items"].pop()
        self.native["thread"]["turns"].append(
            {"id": "later", "status": "completed", "items": [reply]}
        )
        self.portal["entries"][1]["turnId"] = "later"
        summary, _, _, _ = self.assess()
        for kind in ("native", "portal"):
            self.assertEqual(summary[kind]["replyInInitialTurnCount"], 0)
        initial["items"].append(copy.deepcopy(initial["items"][0]))
        self.portal["entries"].append(copy.deepcopy(self.portal["entries"][0]))
        summary, _, _, _ = self.assess()
        for kind in ("native", "portal"):
            self.assertEqual(summary[kind]["initialCount"], 2)
            self.assertIsNone(summary[kind]["initialTurnStatus"])
            self.assertIsNone(summary[kind]["replyInInitialTurnCount"])
        self.native["thread"]["turns"].append(copy.deepcopy(initial))
        summary, _, _, _ = self.assess()
        self.assertEqual(summary["native"]["outcome"], "invalid-shape")

    def test_zero_requires_present_valid_arrays_and_page_scope_stays_visible(self):
        self.native["thread"]["turns"] = []
        self.portal["entries"] = []
        self.portal["hasOlder"] = True
        summary, _, _, _ = self.assess()
        for kind in ("native", "portal"):
            self.assertEqual(summary[kind]["initialCount"], 0)
            self.assertEqual(summary[kind]["entryCount"], 0)
            self.assertIsNone(summary[kind]["replyInInitialTurnCount"])
        self.assertTrue(summary["portal"]["hasOlder"])
        del self.portal["hasOlder"]
        del self.native["thread"]["turns"]
        del self.portal["entries"]
        summary, _, _, _ = self.assess()
        for kind in ("native", "portal"):
            self.assertEqual(summary[kind]["outcome"], "invalid-shape")
            self.assertIsNone(summary[kind]["entryCount"])
        self.assertIsNone(summary["portal"]["hasOlder"])

    def test_wrong_root_or_provider_does_not_expose_values(self):
        for field, value in (("id", RECEIPT), ("modelProvider", SECRET)):
            with self.subTest(field=field):
                original = self.native["thread"][field]
                self.native["thread"][field] = value
                portal_field = "threadId" if field == "id" else field
                self.portal[portal_field] = value
                summary, _, _, _ = self.assess()
                for kind in ("native", "portal"):
                    self.assertEqual(summary[kind]["outcome"], "identity-mismatch")
                    self.assertIsNone(summary[kind]["initialCount"])
                self.native["thread"][field] = original
                self.portal[portal_field] = original

    def test_missing_message_fields_and_conflicting_statuses_are_unknown(self):
        del self.native["thread"]["turns"][0]["items"][0]["content"]
        self.portal["entries"][1]["turnStatus"] = "failed"
        summary, _, _, _ = self.assess()
        for kind in ("native", "portal"):
            self.assertEqual(summary[kind]["outcome"], "invalid-shape")
            self.assertIsNone(summary[kind]["initialCount"])
        self.native["thread"]["turns"][0]["items"] = [
            [["type", "agentMessage"], ["text", "CREATION_REPLY_alternate"]]
        ]
        summary, _, _, _ = self.assess()
        self.assertEqual(summary["native"]["outcome"], "invalid-shape")
        self.assertIsNone(summary["native"]["entryCount"])

    def test_portal_failures_keep_native_observation_and_actual_status(self):
        for kwargs, outcome in (
            ({"portal_error": subprocess.TimeoutExpired("curl", 8)}, "timeout"),
            ({"portal_exit": 7}, "command-error"),
            ({"http": "503"}, "command-error"),
            ({"portal_raw": SECRET}, "invalid-json"),
        ):
            with self.subTest(outcome=outcome, kwargs=kwargs):
                summary, calls, error, _ = self.assess(**kwargs)
                self.assertIsNone(error)
                self.assertEqual(len(calls), 2)
                self.assertEqual(summary["native"]["outcome"], "ok")
                self.assertEqual(summary["portal"]["outcome"], outcome)
                self.assertIsNone(summary["portal"]["initialCount"])

    def test_native_failures_do_not_gate_portal_read(self):
        for kwargs, outcome in (
            ({"native_error": subprocess.TimeoutExpired("transport", 8)}, "timeout"),
            ({"native_error": OSError(SECRET)}, "command-error"),
            ({"native_exit": 1}, "command-error"),
            ({"native_raw": SECRET}, "invalid-json"),
        ):
            with self.subTest(outcome=outcome, kwargs=kwargs):
                summary, calls, error, _ = self.assess(**kwargs)
                self.assertIsNone(error)
                self.assertEqual(len(calls), 2)
                self.assertEqual(summary["native"]["outcome"], outcome)
                self.assertEqual(summary["portal"]["outcome"], "ok")
                self.assertIsNone(summary["native"]["entryCount"])

    def test_projection_limits_never_gate_existing_read_admission(self):
        self.failure["record"]["slug"] = "2026-11-11-" + "x" * 300
        self.failure["record"]["canonicalUrl"] = (
            "/" + self.failure["record"]["slug"] + "/"
        )
        self.failure["record"]["requestId"] = SECRET
        self.failure["record"]["receiptId"] = SECRET
        self.failure["name"] = SECRET
        summary, calls, _, _ = self.assess()
        self.assertEqual(len(calls), 2)
        self.assertEqual(summary["identity"]["state"], "valid")
        for key in ("slug", "requestId", "receiptId"):
            self.assertIsNone(summary["identity"][key])
        self.assertIsNone(summary["native"]["initialCount"])
        self.failure["record"]["canonicalUrl"] = "/wrong/"
        summary, calls, _, _ = self.assess()
        self.assertEqual(calls, [])
        self.assertEqual(summary["identity"]["state"], "invalid")
        self.assertEqual(summary["native"]["outcome"], "skipped")

    def test_unknown_provider_and_timing_are_null_without_fabrication(self):
        self.failure["record"]["provider"] = SECRET
        self.failure["at"] = SECRET
        self.failure.pop("capturedAt")
        summary, _, _, _ = self.assess(monotonic=(10, 9, 20, float("nan")))
        self.assertIsNone(summary["identity"]["provider"])
        self.assertIsNone(summary["native"]["providerMatches"])
        self.assertIsNone(summary["native"]["replyInInitialTurnCount"])
        for key in (
            "failureAt",
            "capturedAt",
            "nativeElapsedSeconds",
            "portalElapsedSeconds",
        ):
            self.assertIsNone(summary["timing"][key])
        summary, _, _, _ = self.assess()
        self.assertEqual(
            summary["timing"]["nativeStartedAt"], "2026-11-11T17:14:10+00:00"
        )
        self.assertEqual(summary["timing"]["nativeElapsedSeconds"], 0.25)

    def test_hostile_large_payloads_are_not_relayed(self):
        self.native["thread"]["turns"][0]["items"] += [
            {"type": "agentMessage", "text": SECRET * 10000}
        ]
        self.portal["entries"][0]["cursor"] = SECRET
        self.portal["headers"] = {"Authorization": SECRET}
        summary, _, _, _ = self.assess()
        self.assertEqual(summary["native"]["entryCount"], 3)

    def test_late_artifact_error_preserves_flushed_summary_and_nonzero_status(self):
        output, _, error, saved = self.produce(artifact_error=True)
        self.assertIsInstance(error, OSError)
        self.assertIsNone(saved)
        final, printed = self.relay(
            output + "Traceback (most recent call last):\nOSError: " + SECRET + "\n", 1
        )
        self.assertEqual(final["postFailureDiagnosticStatus"], 1)
        self.assertEqual(final["postFailurePassiveSummary"]["native"]["outcome"], "ok")
        self.assertNotIn(SECRET, printed)

    def test_missing_malformed_multiple_or_oversize_output_is_fixed_unavailable(self):
        valid, _, _, _ = self.produce()
        for output in (
            "",
            SECRET,
            "{}",
            valid + valid,
            valid + '{ "postFailurePassiveSummary":broken}\n',
            json.dumps({"postFailurePassiveSummary": {"identity": SECRET * 1000}}),
            valid.replace('"completed"', json.dumps(SECRET)),
            "[]",
        ):
            with self.subTest(output=output[:40]):
                final, printed = self.relay(output, 1)
                self.assertEqual(
                    final,
                    {
                        "postFailureDiagnosticStatus": 1,
                        "postFailurePassiveSummary": {"outcome": "unavailable"},
                    },
                )
                self.assertNotIn(SECRET, printed)
        for raw in ("{", "null", "[]"):
            output, calls, error, _ = self.produce(failure_raw=raw)
            self.assertIsNotNone(error)
            self.assertEqual(calls, [])
            self.assertEqual(
                self.relay(output, 1)[0]["postFailurePassiveSummary"],
                {"outcome": "unavailable"},
            )

    def test_actual_diagnostic_tail_preserves_primary_exception(self):
        output, _, _, _ = self.produce(artifact_error=True)
        primary = RuntimeError("original browser failure")
        machine = mock.Mock()
        machine.execute.side_effect = [
            (1, output + "OSError: " + SECRET + "\n"),
            (0, ""),
        ]
        namespace = {
            "machine": machine,
            "json": json,
            "shlex": shlex,
            "runtime": "/runtime",
            "user_prefix": "",
            "user": lambda command: "",
            "native_unit": "native",
            "portal_unit": "portal",
            "router_unit": "router",
        }
        printed = io.StringIO()
        with (
            self.assertRaises(RuntimeError) as raised,
            contextlib.redirect_stdout(printed),
        ):
            try:
                raise primary
            except RuntimeError:
                exec(compile(TAIL, "actual-diagnostic-tail", "exec"), namespace)
        self.assertIs(raised.exception, primary)
        final = json.loads(printed.getvalue().splitlines()[0])
        self.assertEqual(final["postFailureDiagnosticStatus"], 1)
        self.assertEqual(final["postFailurePassiveSummary"]["native"]["outcome"], "ok")


if __name__ == "__main__":
    unittest.main()
