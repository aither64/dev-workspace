"""Execute the owning held-router fixture with simulated time, never a guest."""

import contextlib
import copy
import io
import json
from pathlib import Path
import re
import textwrap
from types import SimpleNamespace
import unittest


SOURCE = (
    Path(__file__).resolve().parents[1] / "nix/tests/session-creation.nix"
).read_text()


def unique(pattern):
    matches = re.findall(pattern, SOURCE, re.MULTILINE | re.DOTALL)
    if len(matches) != 1:
        raise AssertionError("owning fixture extraction must be unique")
    return textwrap.dedent(matches[0])


HELPERS = "\n".join(
    unique(r"(^    def " + name + r"\(.*?)(?=^    def )")
    for name in (
        "user_texts",
        "continuation_items",
        "stable_goal_fields",
        "retained_state",
    )
)
HELD = unique(
    r"(^            barrier_clock = None\n.*?)(?=            restored_client =)"
)
AFTER_RELEASE = unique(
    r'(^            for p in completed\["participants"\]:.*?)(?=            # Exhaust the prefix)'
)
CLEANUP = unique(
    r"^        finally:\n(            maintenance_model_hold\(rules\)\n            if fault_installed:\n                release_target\(\))"
)


class MaintenanceFixtureTest(unittest.TestCase):
    def setup_case(self, *, cooperative=False, stop=False, fail=False):
        self.clock = 0.0
        self.events = []
        self.native_calls = []
        self.requests = []
        self.released = False
        self.completed = False
        self.user_cost = 0.4  # Synthetic overhead; only native read costs below are retained measurements.
        self.final_router_cost = 0.5
        self.read_costs = {"root": [10.66, 9.83, 10.83], "member": [4.0, 4.0]}
        self.router_calls = 0
        self.router = {
            "ActiveState": "activating",
            "MainPID": "300",
            "ControlPID": "301",
            "InvocationID": "held-router",
        }
        self.final_router = copy.deepcopy(self.router)
        self.marker = "300 held-router"
        self.queue = {
            "id": "queue-id",
            "clientUserMessageId": "queue-client",
            "input": [{"type": "text", "text": "marker_queue"}],
        }
        self.goal = {
            "threadId": "root",
            "objective": "saved goal",
            "status": "active",
            "tokenBudget": 100,
            "createdAt": 1,
        }
        self.live_queue = {"data": [copy.deepcopy(self.queue)], "nextCursor": None}
        self.live_goal = copy.deepcopy(self.goal)
        status = "completed" if cooperative else "interrupted"
        self.histories = {
            name: {
                "id": name,
                "cwd": "/work/session",
                "modelProvider": "alternate",
                "model": "gpt-5.5",
                "reasoningEffort": "high",
                "turns": [
                    {
                        "id": "prior",
                        "status": "completed",
                        "items": [{"type": "agentMessage", "text": "retained body"}],
                    },
                    {
                        "id": name + "-source",
                        "status": status,
                        "items": [
                            {
                                "type": "userMessage",
                                "id": name + "-item",
                                "content": [{"type": "text", "text": "source input"}],
                            }
                        ],
                    },
                ],
            }
            for name in ("root", "member")
        }
        self.before = copy.deepcopy(self.histories["root"])
        self.before["turns"] = self.before["turns"][:1]
        self.operation = {
            "operationId": "same-operation",
            "activationPackage": "/target",
            "activationNative": "/launcher",
            "sourcesStopped": True,
            "activationIntent": True,
            "continuationsSettled": False,
            "participants": [
                {"thread": name, "continuationId": name + "-continuation"}
                for name in ("root", "member")
            ],
        }

        def native(identity):
            self.native_calls.append(("later" if self.released else "held", identity))
            costs = self.read_costs[identity]
            self.clock += costs.pop(0) if costs else 4.0
            if not self.released:
                return {"thread": self.histories[identity]}
            history = copy.deepcopy(self.histories[identity])
            if identity != "member" or not stop:
                history["turns"].append(
                    {
                        "id": "resume",
                        "status": "completed",
                        "items": [
                            {
                                "type": "userMessage",
                                "clientId": identity + "-continuation",
                                "content": [{"type": "text", "text": "RESUME"}],
                            }
                        ],
                    }
                )
            return {"thread": history}

        def native_request(method, **kwargs):
            self.requests.append((method, kwargs))
            if method == "thread/queue/list":
                self.clock += 3.88
                return copy.deepcopy(self.live_queue)
            self.assertEqual(method, "thread/goal/get")
            self.clock += 3.83
            return {"goal": copy.deepcopy(self.live_goal)}

        def unit_state(unit):
            if unit == "router":
                self.router_calls += 1
                self.clock += 0.5 if self.router_calls == 1 else self.final_router_cost
                return copy.deepcopy(
                    self.router if self.router_calls == 1 else self.final_router
                )
            self.clock += 0.5
            if unit == "native":
                return {
                    "ActiveState": "active",
                    "ControlPID": "0",
                    "MainPID": "200",
                    "InvocationID": "new-native",
                }
            self.assertEqual(unit, "portal")
            return {"ActiveState": "inactive"}

        def user(command):
            self.clock += self.user_cost
            return {
                "cat /entered": self.marker,
                "readlink /proc/200/exe": "/native-target",
                "stat -Lc '%d:%i' /runtime/app-server.sock": "new-socket",
                "readlink -f /profile": "/target",
                "test ! -e /proc/100": "",
            }[command]

        def execute(command, *, timeout):
            self.assertEqual(timeout, 5)
            self.assertEqual(
                command, "test -s /entered && test -S /runtime/app-server.sock"
            )
            self.clock += 0.1
            return 0, ""

        def maintenance_wait(label, predicate, identity):
            self.assertEqual(
                (label, identity), ("owned target startup barrier", "same-operation")
            )
            self.assertTrue(predicate(self.operation))
            return self.operation

        def managed_client_state(*args):
            self.assertEqual(args[-1], "stopped")
            self.clock += 3.22
            return {"state": "stopped"}

        def release_target():
            self.events.append("release")
            self.released = True

        def model_hold(rules):
            self.assertEqual(rules, ["owned-model-hold"])
            self.events.append("model-hold")

        def switch_done(identity, unit):
            self.assertTrue(self.released)
            self.assertEqual(identity, "same-operation")
            self.assertEqual(unit, None if fail else "command-unit")
            self.events.append("complete")
            self.completed = True
            completed = copy.deepcopy(self.operation)
            for participant in completed["participants"]:
                participant["continuationDisposition"] = (
                    "suppressed"
                    if stop and participant["thread"] == "member"
                    else "accepted"
                )
            return completed

        self.namespace = {
            "native": native,
            "native_request": native_request,
            "unit_state": unit_state,
            "user": user,
            "machine": SimpleNamespace(execute=execute),
            "time": SimpleNamespace(monotonic=lambda: self.clock),
            "json": json,
            "maintenance_wait": maintenance_wait,
            "managed_client_state": managed_client_state,
            "release_target": release_target,
            "maintenance_model_hold": model_hold,
            "switch_done": switch_done,
            "identity": "same-operation",
            "target_entered": "/entered",
            "runtime": "/runtime",
            "user_prefix": "",
            "native_unit": "native",
            "router_unit": "router",
            "portal_unit": "portal",
            "profile": "/profile",
            "source_service": {"MainPID": "100", "InvocationID": "old-native"},
            "source_socket": "old-socket",
            "barrier_source": {"InvocationID": "old-router"},
            "target_native": {
                "package": "/target",
                "launcher": "/launcher",
                "executable": "/native-target",
            },
            "record": {"threadId": "root", "slug": "session"},
            "member": {"threadId": "member"},
            "source_client": {"pid": "old-client"},
            "queue": self.queue,
            "goal": self.goal,
            "before": self.before,
            "marker": "marker",
            "rules": ["owned-model-hold"],
            "root_turn": "root-source",
            "member_turn": "member-source",
            "cooperative": cooperative,
            "stop": stop,
            "fail": fail,
            "command_unit": "command-unit",
            "resume_text": "RESUME",
        }
        exec(compile(HELPERS, "actual-maintenance-helpers", "exec"), self.namespace)
        body = (
            "def exercise():\n    fault_installed = True\n    try:\n"
            + textwrap.indent(HELD, "        ")
            + "\n    finally:\n"
            + textwrap.indent(CLEANUP, "        ")
            + "\n    return locals()\n"
        )
        exec(compile(body, "actual-held-router-block", "exec"), self.namespace)

    def run_case(self):
        with contextlib.redirect_stdout(io.StringIO()):
            return self.namespace["exercise"]()

    def refuses(self):
        with self.assertRaises((AssertionError, KeyError, StopIteration)) as error:
            self.run_case()
        self.assertFalse(self.completed)
        self.assertEqual(self.events, ["model-hold", "release"])
        return error.exception

    def test_saved_latency_single_reads_full_snapshots_and_all_case_expectations(self):
        for flags in ({"cooperative": True}, {"stop": True}, {"fail": True}):
            with self.subTest(flags=flags):
                self.setup_case(**flags)
                before = copy.deepcopy(self.histories)
                result = self.run_case()
                self.assertEqual(
                    self.native_calls, [("held", "root"), ("held", "member")]
                )
                self.assertEqual(self.histories, before)
                self.assertIs(result["held_threads"]["root"], self.histories["root"])
                self.assertIs(result["source"], result["held_threads"]["root"])
                self.assertIs(
                    result["member_history"], result["held_threads"]["member"]
                )
                self.assertEqual(
                    [method for method, _ in self.requests],
                    ["thread/queue/list", "thread/goal/get"],
                )
                self.assertEqual(result["held_inspection"]["stage"], "before-release")
                self.assertLess(result["held_inspection"]["elapsedSeconds"], 40)
                self.assertEqual(
                    self.events, ["release", "model-hold", "complete", "model-hold"]
                )
                self.namespace.update(result)
                exec(
                    compile(
                        AFTER_RELEASE, "actual-after-release-continuations", "exec"
                    ),
                    self.namespace,
                )
                self.assertEqual(
                    self.native_calls[-2:], [("later", "root"), ("later", "member")]
                )
                self.assertEqual(self.histories, before)

    def test_duplicate_participant_id_is_read_only_once(self):
        self.setup_case()
        self.operation["participants"].append(
            copy.deepcopy(self.operation["participants"][0])
        )
        self.run_case()
        self.assertEqual(self.native_calls, [("held", "root"), ("held", "member")])

    def test_optional_helpers_are_fresh_by_default_across_boundaries(self):
        self.setup_case()
        helper = self.namespace["continuation_items"]
        for _ in ("source", "failed-start", "recovered"):
            helper("root", "not-present")
        self.assertEqual(self.native_calls, [("held", "root")] * 3)
        self.namespace["retained_state"](
            self.namespace["record"], self.queue, self.goal, self.before, "marker_queue"
        )
        self.assertEqual(len(self.native_calls), 4)
        self.released = True
        for _ in ("completion", "completed-UUID-replay"):
            self.assertEqual(len(helper("root", "root-continuation")), 1)
        self.assertEqual(self.native_calls[-2:], [("later", "root"), ("later", "root")])

    def test_invalid_supplied_snapshots_do_not_fall_back_to_native(self):
        self.setup_case()
        for snapshot in ({}, {"id": "foreign", "turns": []}):
            with self.subTest(snapshot=snapshot):
                with self.assertRaises((AssertionError, KeyError)):
                    self.namespace["continuation_items"](
                        "root", "unused", thread_snapshot=snapshot
                    )
                with self.assertRaises((AssertionError, KeyError)):
                    self.namespace["retained_state"](
                        self.namespace["record"],
                        self.queue,
                        self.goal,
                        self.before,
                        "marker_queue",
                        thread_snapshot=snapshot,
                    )
        self.assertEqual(self.native_calls, [])

    def test_early_continuation_in_complete_history_refuses_and_cleans_up(self):
        self.setup_case()
        self.histories["root"]["turns"] += [
            {"id": str(i), "status": "completed", "items": []} for i in range(30)
        ]
        self.histories["root"]["turns"][-1]["items"] = [
            {"type": "userMessage", "clientId": "root-continuation", "content": []}
        ]
        self.refuses()

    def test_wrong_or_missing_history_identity_and_source_status_refuse(self):
        for defect in (
            "root-id",
            "member-id",
            "missing-prior",
            "cwd",
            "missing-turns",
            "source-turn",
            "source-status",
            "member-status",
        ):
            with self.subTest(defect=defect):
                self.setup_case(cooperative=True)
                if defect.endswith("-id"):
                    self.histories[defect.removesuffix("-id")]["id"] = "foreign"
                elif defect == "missing-prior":
                    self.histories["root"]["turns"].pop(0)
                elif defect == "cwd":
                    self.histories["root"]["cwd"] = "/foreign"
                elif defect == "missing-turns":
                    del self.histories["root"]["turns"]
                elif defect == "source-turn":
                    self.histories["root"]["turns"][1]["id"] = "wrong-turn"
                else:
                    self.histories["root" if defect == "source-status" else "member"][
                        "turns"
                    ][1]["status"] = "inProgress"
                self.refuses()

    def test_live_queue_and_goal_gates_remain(self):
        for defect in ("queue", "goal"):
            with self.subTest(defect=defect):
                self.setup_case()
                if defect == "queue":
                    self.live_queue["data"][0]["clientUserMessageId"] = "foreign"
                else:
                    self.live_goal["status"] = "paused"
                self.refuses()

    def test_consumed_queue_is_checked_in_the_full_held_snapshot(self):
        for text in ("marker_queue", "wrong queue text"):
            with self.subTest(text=text):
                self.setup_case()
                self.live_queue["data"] = []
                self.histories["root"]["turns"][1]["items"].append(
                    {
                        "type": "userMessage",
                        "id": "consumed-item",
                        "clientId": self.queue["clientUserMessageId"],
                        "content": [{"type": "text", "text": text}],
                    }
                )
                before = copy.deepcopy(self.histories)
                if text == "marker_queue":
                    self.run_case()
                    self.assertEqual(
                        self.native_calls, [("held", "root"), ("held", "member")]
                    )
                else:
                    self.refuses()
                self.assertEqual(self.histories, before)

    def test_whole_window_includes_previously_unmeasured_identity_and_reads(self):
        self.setup_case()
        self.user_cost = 6.0  # Synthetic early overhead; no real sleep.
        error = self.refuses()
        label, evidence = error.args[0]
        self.assertEqual(label, "bounded target inspection exceeded fixture budget")
        self.assertGreaterEqual(evidence["elapsedSeconds"], 40)
        self.assertEqual(evidence["stage"], "participant-history-after")

    def test_final_service_read_is_inside_budget(self):
        self.setup_case()
        self.final_router_cost = 20.0  # Synthetic late overhead.
        error = self.refuses()
        self.assertEqual(error.args[0][1]["stage"], "before-release")

    def test_expired_or_changed_activating_router_is_not_the_owned_barrier(self):
        for field, value in (
            ("ActiveState", "failed"),
            ("ActiveState", "active"),
            ("MainPID", "400"),
            ("ControlPID", "401"),
            ("InvocationID", "restarted-router"),
        ):
            with self.subTest(field=field, value=value):
                self.setup_case()
                self.final_router[field] = value
                self.refuses()

    def test_initial_marker_and_positive_process_ids_are_required(self):
        for defect in ("marker", "MainPID", "ControlPID"):
            with self.subTest(defect=defect):
                self.setup_case()
                if defect == "marker":
                    self.marker = "400 restarted-router"
                else:
                    self.router[defect] = "0"
                    self.marker = self.router["MainPID"] + " held-router"
                self.refuses()


if __name__ == "__main__":
    unittest.main()
