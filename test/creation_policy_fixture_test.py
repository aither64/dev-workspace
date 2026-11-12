"""Actual policy setup/preflight controls; modeled replies are not systemd proof."""

import ast
import contextlib
import io
import json
from pathlib import Path
import re
import shlex
import subprocess
import textwrap
from types import SimpleNamespace
import unittest


ROOT = Path(__file__).resolve().parents[1]
NIX = (ROOT / "nix/tests/session-creation.nix").read_text()
TRANSPORT = (ROOT / "test/creation_transport.py").read_text()


def unique(pattern):
    matches = re.findall(pattern, NIX, re.MULTILINE | re.DOTALL)
    if len(matches) != 1:
        raise AssertionError("owning policy fixture extraction must be unique")
    return textwrap.dedent(matches[0])


USER = unique(r"(^    def user\(.*?)(?=^    def sync_source\()")
POLICY = unique(
    r'^        with subtest\("production native enforces guest system provider policy and recovers the same histories"\):\n'
    r"(.*?)(?=^    ''\)\})"
)
functions = [
    node
    for node in ast.parse(TRANSPORT).body
    if isinstance(node, ast.AsyncFunctionDef) and node.name == "production_policy"
]
if len(functions) != 1:
    raise AssertionError("production_policy must be unique")
body = functions[0].body
starts = [
    i
    for i, node in enumerate(body)
    if isinstance(node, ast.Assign)
    and any(isinstance(t, ast.Name) and t.id == "environment" for t in node.targets)
]
if len(starts) != 1:
    raise AssertionError("policy environment must be unique")
start = starts[0]
if not (
    isinstance(body[start + 1], ast.Assign) and isinstance(body[start + 2], ast.For)
):
    raise AssertionError("policy service preflight boundary changed")
PREFLIGHT = textwrap.dedent(
    "\n".join(
        TRANSPORT.splitlines()[body[start].lineno - 1 : body[start + 2].end_lineno]
    )
)
SERVICES = [
    "workspace-codex@fixture.service",
    "workspace-portal@fixture.service",
    "workspace-codex-provider@fixture.service",
    "workspace-tmux@fixture.service",
    "workspace-router.service",
    "creation-transport.service",
]
SCENARIOS = [
    "strict-mismatch",
    "missing-definition",
    "malformed-current",
    "managed-selection",
    "managed-definition",
]
QUIESCENT = "MainPID=0\nControlPID=0\nActiveState=inactive\n"


class PolicyFixtureTest(unittest.TestCase):
    def setUp(self):
        self.events = []
        self.shows = []
        self.responses = {}
        self.stop_error = None
        self.harness_output = json.dumps({"scenarios": SCENARIOS})

        def check_output(argv, *, text, timeout):
            service = argv[-2]
            self.shows.append(service)
            self.assertEqual(
                argv,
                [
                    "runuser",
                    "-u",
                    "developer",
                    "--",
                    "env",
                    "-i",
                    "XDG_RUNTIME_DIR=/run/user/1000",
                    "PATH=/declared/path",
                    "systemctl",
                    "--user",
                    "show",
                    service,
                    "--property=MainPID,ControlPID,ActiveState",
                ],
            )
            self.assertIs(text, True)
            self.assertEqual(timeout, 5)
            result = self.responses.get(service, QUIESCENT)
            if isinstance(result, Exception):
                raise result
            return result

        def monotonic():
            self.events.append("clock")
            return self.events.count("clock")

        def succeed(command, *, timeout=None):
            if timeout is None:
                self.events.append("stop")
                outer = shlex.split(command)
                self.assertEqual(
                    outer[:8],
                    [
                        "runuser",
                        "-u",
                        "developer",
                        "--",
                        "env",
                        "XDG_RUNTIME_DIR=/run/user/1000",
                        "sh",
                        "-c",
                    ],
                )
                self.assertEqual(len(outer), 9)
                self.assertEqual(
                    shlex.split(outer[8]),
                    ["systemctl", "--user", "stop", "workspace-*"],
                )
                if self.stop_error:
                    raise self.stop_error
                return ""
            self.events.append("harness")
            self.assertEqual(timeout, 600)
            self.assertEqual(
                shlex.split(command),
                [
                    "${python}/bin/python",
                    "${../../test/creation_transport.py}",
                    "production-policy",
                    "/nix/store/selected-package",
                    "${../../test/creation_model.py}",
                    "/tmp/creation-evidence/production-policy",
                ],
            )
            exec(
                PREFLIGHT,
                {
                    "os": SimpleNamespace(environ={"PATH": "/declared/path"}),
                    "subprocess": SimpleNamespace(check_output=check_output),
                },
            )
            # Later directory/policy/native work is outside this extracted preflight.
            self.events.append("policy-work")
            return self.harness_output

        self.env = {
            "json": json,
            "shlex": shlex,
            "package": "/nix/store/selected-package",
            "time": SimpleNamespace(monotonic=monotonic),
            "machine": SimpleNamespace(succeed=succeed),
            "user_prefix": "runuser -u developer -- env XDG_RUNTIME_DIR=/run/user/1000 ",
        }
        exec(USER, self.env)

    def run_policy(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            exec(POLICY, self.env)
        return json.loads(output.getvalue())

    def test_loaded_quiescent_and_unloaded_style_success(self):
        for model, state in [
            ("loaded-inactive", "inactive"),
            ("loaded-failed", "failed"),
            ("unloaded-style-success", "inactive"),
        ]:
            with self.subTest(modeled_response=model):
                self.setUp()
                self.responses = {
                    unit: QUIESCENT.replace("inactive", state) for unit in SERVICES
                }
                result = self.run_policy()
                self.assertEqual(self.shows, SERVICES)
                self.assertEqual(
                    self.events, ["stop", "clock", "harness", "policy-work", "clock"]
                )
                self.assertEqual(
                    result,
                    {"productionPolicy": {"scenarios": SCENARIOS}, "elapsedSeconds": 1},
                )

    def test_strict_stop_failure_prevents_timer_and_harness(self):
        for error in [
            subprocess.CalledProcessError(5, "systemctl stop"),
            subprocess.TimeoutExpired("systemctl stop", 5),
        ]:
            with self.subTest(error=type(error).__name__):
                self.setUp()
                self.stop_error = error
                with self.assertRaises(type(error)):
                    self.run_policy()
                self.assertEqual(self.events, ["stop"])
                self.assertEqual(self.shows, [])

    def test_live_pids_and_nonquiescent_states_refuse_every_unit(self):
        replies = [
            QUIESCENT.replace("MainPID=0", "MainPID=7"),
            QUIESCENT.replace("ControlPID=0", "ControlPID=7"),
            "MainPID=7\nControlPID=0\nActiveState=failed\n",
        ]
        replies += [
            QUIESCENT.replace("inactive", state)
            for state in ["active", "activating", "deactivating", "unknown"]
        ]
        self.assert_preflight_refusals(replies)

    def test_malformed_and_missing_properties_refuse_every_unit(self):
        self.assert_preflight_refusals(
            [
                "",
                "not-a-property",
                "MainPID=0\nControlPID=0\n",
                "ControlPID=0\nActiveState=inactive\n",
                "MainPID=0\nActiveState=inactive\n",
                QUIESCENT.replace("MainPID=0", "MainPID=not-a-pid"),
            ]
        )

    def test_show_errors_and_timeouts_refuse_every_unit(self):
        self.assert_preflight_refusals(
            [
                subprocess.CalledProcessError(1, "systemctl show"),
                subprocess.TimeoutExpired("systemctl show", 5),
            ]
        )

    def test_unloaded_style_nonzero_remains_fatal(self):
        self.responses[SERVICES[-1]] = subprocess.CalledProcessError(
            5, "systemctl show", stderr="Unit creation-transport.service not loaded."
        )
        with self.assertRaises(subprocess.CalledProcessError):
            self.run_policy()
        self.assertEqual(self.shows, SERVICES)
        self.assertNotIn("policy-work", self.events)

    def assert_preflight_refusals(self, replies):
        for index, unit in enumerate(SERVICES):
            for reply in replies:
                with self.subTest(unit=unit, reply=repr(reply)):
                    self.setUp()
                    self.responses[unit] = reply
                    with self.assertRaises(
                        (
                            AssertionError,
                            KeyError,
                            ValueError,
                            subprocess.CalledProcessError,
                            subprocess.TimeoutExpired,
                        )
                    ):
                        self.run_policy()
                    self.assertEqual(self.shows, SERVICES[: index + 1])
                    self.assertEqual(self.events, ["stop", "clock", "harness"])

    def test_malformed_harness_result_and_changed_scenarios_still_fail(self):
        for output in [
            "not json",
            "[]",
            "{}",
            json.dumps({"scenarios": SCENARIOS[:-1]}),
            json.dumps({"scenarios": list(reversed(SCENARIOS))}),
        ]:
            with self.subTest(output=output):
                self.setUp()
                self.harness_output = output
                with self.assertRaises(
                    (ValueError, TypeError, KeyError, AssertionError)
                ):
                    self.run_policy()
                self.assertEqual(self.shows, SERVICES)
                self.assertEqual(self.events.count("harness"), 1)


if __name__ == "__main__":
    unittest.main()
