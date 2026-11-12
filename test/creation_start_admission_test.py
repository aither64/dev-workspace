"""Exercise the actual source-epoch start boundary without a guest or real waits."""

import json
from pathlib import Path
import re
import shlex
import textwrap
from types import SimpleNamespace
import unittest


SOURCE = (
    Path(__file__).resolve().parents[1] / "nix/tests/session-creation.nix"
).read_text()


def unique(pattern):
    matches = re.findall(pattern, SOURCE, re.MULTILINE | re.DOTALL)
    if len(matches) != 1:
        raise AssertionError("owning start boundary must be unique")
    return textwrap.dedent(matches[0])


HELPER = unique(r"(^    def start_source\(.*?)(?=^    def browser\()")
CALLER = unique(
    r"(^                started = json.loads\(start_source\(.*?"
    r'                sync_source\(package, record\["slug"\]\)\n)'
)


class StartAdmissionTest(unittest.TestCase):
    def setUp(self):
        self.clock = 0.0
        self.calls = []
        self.sleeps = []
        self.syncs = []
        self.frozen_clock = False
        self.record = {"slug": "2026-11-12-before", "threadId": "retained-thread"}
        self.busy = (
            "error: another dev-session command is changing " + self.record["slug"]
        )
        self.success = json.dumps(self.record)
        self.results = []

        def execute(command, *, timeout):
            self.calls.append((command, timeout, self.clock))
            self.assertGreaterEqual(timeout, 1)
            result, elapsed = self.results.pop(0)
            self.clock += elapsed
            if isinstance(result, Exception):
                raise result
            return result

        def sleep(seconds):
            self.sleeps.append(seconds)
            if not self.frozen_clock:
                self.clock += seconds

        self.env = {
            "json": json,
            "shlex": shlex,
            "time": SimpleNamespace(monotonic=lambda: self.clock, sleep=sleep),
            "machine": SimpleNamespace(execute=execute),
            "user_prefix": "runuser -u developer -- env XDG_RUNTIME_DIR=/run/user/1000 ",
            "root": "/home/developer/workspace",
            "package": "/nix/store/selected-package",
            "record": self.record,
            "sync_source": lambda *args: self.syncs.append(args),
        }
        exec(HELPER, self.env)

    def run_caller(self, results):
        self.results = results
        exec(CALLER, self.env)

    def test_exact_refusal_forms_then_success_keep_command_and_envelope(self):
        self.run_caller(
            [((1, self.busy), 1), ((1, self.busy + "\n"), 2), ((0, self.success), 0.5)]
        )
        expected = [
            "runuser",
            "-u",
            "developer",
            "--",
            "env",
            "XDG_RUNTIME_DIR=/run/user/1000",
            "sh",
            "-c",
        ]
        for command, _, _ in self.calls:
            outer = shlex.split(command)
            self.assertEqual(outer[:8], expected)
            self.assertEqual(outer[9:], ["2>&1"])
            self.assertEqual(
                shlex.split(outer[8]),
                [
                    "cd",
                    self.env["root"],
                    "&&",
                    self.env["package"] + "/bin/dev-session",
                    "start",
                    self.record["slug"],
                    "--as-is",
                    "--no-attach",
                    "--json",
                ],
            )
        self.assertEqual([call[1] for call in self.calls], [20, 18, 16])
        self.assertEqual(self.sleeps, [0.25, 0.25])
        self.assertEqual(self.syncs, [(self.env["package"], self.record["slug"])])
        self.assertEqual(self.env["started"], self.record)

    def test_permanent_or_malformed_results_stop_without_sync(self):
        for result in [
            (1, self.busy + "\n\n"),
            (1, self.busy + " extra"),
            (1, " " + self.busy),
            (1, self.busy.replace("before", "other")),
            (1, "warning\n" + self.busy),
            (
                1,
                "error: another dev-session command is creating or closing "
                + self.record["slug"],
            ),
            (1, "error: recorded Codex endpoint belongs to another runtime"),
            (1, "error: permission denied"),
            (2, self.busy),
            (75, self.busy),
            (True, self.busy),
            (1.0, self.busy),
            ("1", self.busy),
            (1, None),
            (0, None),
            (False, self.success),
        ]:
            with self.subTest(result=result):
                self.setUp()
                with self.assertRaises(AssertionError):
                    self.run_caller([(result, 0)])
                self.assertEqual(len(self.calls), 1)
                self.assertEqual(self.syncs, [])
                self.assertEqual(self.sleeps, [])

    def test_execution_errors_are_terminal(self):
        for error in [
            TimeoutError("uncertain command"),
            RuntimeError("driver exception"),
        ]:
            with self.subTest(error=type(error).__name__):
                self.setUp()
                with self.assertRaisesRegex(
                    AssertionError, "start execution failed"
                ) as caught:
                    self.run_caller([(error, 1)])
                self.assertIs(caught.exception.__cause__, error)
                self.assertEqual(len(self.calls), 1)
                self.assertEqual(self.syncs, [])

    def test_successful_process_with_bad_json_or_identity_never_repeats(self):
        for output in [
            "not json",
            "null",
            "{}",
            json.dumps({**self.record, "slug": "other"}),
            json.dumps({**self.record, "threadId": "other"}),
        ]:
            with self.subTest(output=output):
                self.setUp()
                with self.assertRaises(
                    (AssertionError, ValueError, TypeError, KeyError)
                ):
                    self.run_caller([((0, output), 0)])
                self.assertEqual(len(self.calls), 1)
                self.assertEqual(self.syncs, [])
                self.assertEqual(self.sleeps, [])

    def test_deadline_rejects_refusal_and_late_success(self):
        for status, output in [(1, self.busy), (0, self.success)]:
            with self.subTest(status=status):
                self.setUp()
                with self.assertRaisesRegex(AssertionError, "start admission deadline"):
                    self.run_caller([((status, output), 20)])
                self.assertEqual(len(self.calls), 1)
                self.assertEqual(self.syncs, [])

    def test_subsecond_remainder_never_disables_driver_timeout(self):
        with self.assertRaisesRegex(AssertionError, "start admission bound"):
            self.run_caller([((1, self.busy), 19.2)])
        self.assertEqual([call[1] for call in self.calls], [20])
        self.assertEqual(self.sleeps, [0.25])
        self.assertEqual(self.syncs, [])

    def test_explicit_attempt_cap_even_when_clock_does_not_advance(self):
        self.frozen_clock = True
        with self.assertRaisesRegex(AssertionError, "start admission bound"):
            self.run_caller([((1, self.busy), 0)] * 81)
        self.assertEqual(len(self.calls), 80)
        self.assertEqual(len(self.sleeps), 79)
        self.assertEqual(len(self.results), 1)
        self.assertEqual(self.syncs, [])

    def test_last_output_is_bounded(self):
        with self.assertRaises(AssertionError) as caught:
            self.run_caller([((1, "x" * 5000), 0)])
        self.assertEqual(len(caught.exception.args[0][-1]["output"]), 2048)
        self.assertEqual(self.syncs, [])


if __name__ == "__main__":
    unittest.main()
