"""Quick disposable observer checks; no portal, native daemon or VM is started."""

import fcntl
import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time
import types
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("diagnostics", pathlib.Path(__file__).parent /
                                            "support/creation_refusal_diagnostics.py")
diagnostics = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(diagnostics)


class ObserverTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="creation-observer-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.authority = self.root / "authority"
        self.authority.mkdir()
        self.proc = self.root / "proc"
        self.proc.mkdir()
        (self.proc / "locks").write_text("")
        self.context = self.root / "context.json"
        self.context.write_text(json.dumps({"scenario": "plan-explicit", "phase": "wait for ready",
                                            "correlation": "fixture-1", "sourceSlug": "explicit"}))
        self.observer = diagnostics.Observer(self.authority, self.proc, os.getuid(), 100,
                                               self.context, self.root / "bundle.json")

    def process(self, pid, executable, tgid=None):
        directory = self.proc / str(pid)
        directory.mkdir()
        (directory / "status").write_text(f"Uid:\t{os.getuid()}\nTgid:\t{tgid or pid}\n")
        (directory / "exe").symlink_to("/fixture/bin/" + executable)
        # These files must never be read by the observer.
        (directory / "cmdline").write_text("ARGV_CREDENTIAL_CANARY")
        (directory / "environ").write_text("ENV_CREDENTIAL_CANARY")

    def line(self, name="explicit.lock", pid=100, result="-1 EAGAIN (CANARY_ERROR_TEXT)", mode="LOCK_EX|LOCK_NB"):
        return (f"{pid} 1234.000001 (+ 0.000020) flock(7<{self.authority / name}>, "
                f"{mode}) = {result} <0.000010>\n")

    def test_exact_event_and_separate_later_path_identity_for_both_roles_and_lock_kinds(self):
        self.process(101, "workspace-portal", tgid=100)
        self.process(200, "ruby")
        for name, pid in [("candidate.creation.lock", 101), ("candidate.lock", 101), ("explicit.lock", 200)]:
            (self.authority / name).touch()
            self.observer.feed(self.line(name, pid))
        events = self.observer.events
        self.assertEqual([e["lockKind"] for e in events], ["creation", "runtime", "runtime"])
        self.assertEqual([e["role"] for e in events], ["portal-exclusive-candidate-or-other"] * 2 + ["ruby-source-or-other"])
        self.assertTrue(events[2]["sourceFilenameMatch"])
        for event in events:
            self.assertEqual(event["fd"], 7)
            self.assertEqual(event["errno"], "EAGAIN")
            self.assertEqual(event["descriptorIdentity"], "unavailable-no-fd-lifetime-proof")
            self.assertEqual(event["laterSnapshot"]["identityBasis"], "path-at-observation-only")
            self.assertNotIn("CANARY", json.dumps(event))

    def test_unknown_process_and_unfinished_event_are_explicit(self):
        line = self.line().replace(") = -1 EAGAIN (CANARY_ERROR_TEXT) <0.000010>", " <unfinished ...>")
        self.observer.feed(line)
        self.assertEqual(self.observer.events, [])
        self.observer.feed("100 1234.000050 (+ 0.000049) <... flock resumed>) = -1 EAGAIN (ignored) <0.000010>\n")
        self.assertEqual(len(self.observer.events), 1)
        self.assertEqual(self.observer.events[0]["actor"]["executableClass"], "unavailable")
        self.assertEqual(self.observer.events[0]["laterSnapshot"]["unavailable"], "path-removed-or-proc-unreadable")

    def test_success_shared_outside_authority_and_noncontention_failures_are_ignored(self):
        for line in [self.line(result="0"), self.line(mode="LOCK_SH|LOCK_NB"),
                     self.line(result="-1 EBADF (ignored)"), self.line(name="../other.lock"),
                     self.line(name="not-a-lock"), "BODY_CREDENTIAL_CANARY"]:
            self.observer.feed(line)
        self.assertEqual(self.observer.events, [])

    def test_real_held_descriptor_and_release_before_observation_preserve_original_exit(self):
        path = self.authority / "candidate.creation.lock"
        with path.open("w") as holder:
            fcntl.flock(holder, fcntl.LOCK_EX)
            expected = os.fstat(holder.fileno())
            code = ("import fcntl,sys; f=open(sys.argv[1],'a'); "
                    "exec('try:\\n fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)\\n'"
                    "+'except BlockingIOError:\\n sys.exit(37)')")
            env = dict(os.environ, OBSERVER_CANARY="ENV_CREDENTIAL_CANARY")
            result = subprocess.run(["strace", "--always-show-pid", "-r", "-ttt", "-T", "-yy",
                                     "-e", "trace=flock", sys.executable, "-c", code, str(path),
                                     "ARGV_CREDENTIAL_CANARY"], env=env, capture_output=True,
                                    text=True, timeout=5)
            self.assertEqual(result.returncode, 37)
            # This is a controlled descriptor witness, not evidence for either VM.
            self.observer.proc = pathlib.Path("/proc")
            for line in result.stderr.splitlines():
                self.observer.feed(line)
            event = self.observer.events[0]
            identity = event["laterSnapshot"]["pathIdentity"]
            self.assertEqual(identity["inode"], expected.st_ino)
            self.assertEqual(identity["deviceMajor"], os.major(expected.st_dev))
            self.assertIn(os.getpid(), [o["pid"] for o in event["laterSnapshot"]["owners"]])
            fcntl.flock(holder, fcntl.LOCK_UN)
            for line in result.stderr.splitlines():
                self.observer.feed(line)
            released = self.observer.events[1]["laterSnapshot"]
            self.assertEqual(released["owners"], [])
            self.assertEqual(released["unavailable"], "no-matching-owner-at-later-snapshot")
            self.assertNotIn("CREDENTIAL_CANARY", json.dumps(self.observer.events))
            # Observation makes no flock calls, retries or target commands.
            with patch.object(diagnostics.os, "stat", side_effect=OSError), patch.object(
                    diagnostics.subprocess, "Popen", side_effect=AssertionError("must not execute")):
                self.observer.feed(self.line())
            self.assertEqual(result.returncode, 37)

    def test_owner_scan_caps_unreadable_proc_and_private_fields(self):
        path = self.authority / "explicit.lock"
        path.touch()
        info = path.stat()
        key = f"{os.major(info.st_dev):02x}:{os.minor(info.st_dev):02x}:{info.st_ino}"
        (self.proc / "locks").write_text("".join(
            f"{index}: FLOCK ADVISORY WRITE {index + 200} {key} 0 EOF\n" for index in range(20)))
        self.process(200, "ARGV_CREDENTIAL_CANARY")
        self.observer.feed(self.line())
        snap = self.observer.events[0]["laterSnapshot"]
        self.assertEqual(len(snap["owners"]), 16)
        self.assertTrue(snap["ownersTruncated"])
        self.assertEqual(snap["owners"][0]["executableClass"], "other")
        self.assertEqual(snap["owners"][1]["executableClass"], "unavailable")
        self.assertNotIn("CREDENTIAL_CANARY", json.dumps(snap))
        (self.proc / "locks").write_bytes(b"x" * (diagnostics.SCAN_LIMIT + 1))
        self.assertTrue(diagnostics.snapshot(path, self.proc, os.getuid(), 100)["scanTruncated"])

    def test_caps_collector_failure_and_error_text_exclusion(self):
        with patch.object(diagnostics, "snapshot", side_effect=RuntimeError("BODY_CREDENTIAL_CANARY")):
            for _ in range(140):
                self.observer.feed(self.line())
        self.assertEqual(len(self.observer.events), 128)
        self.assertEqual(self.observer.dropped, 12)
        self.observer.save()
        data = self.observer.output.read_text()
        self.assertLess(len(data.encode()), diagnostics.BUNDLE_BYTES)
        self.assertNotIn("CREDENTIAL_CANARY", data)
        self.assertTrue(all(len(json.dumps(e).encode()) <= 8192 for e in self.observer.events))
        self.assertEqual(json.loads(data)["collectorErrors"], 128)

    def test_tracer_start_failure_is_diagnostic_only(self):
        args = types.SimpleNamespace(authority=self.authority, uid=os.getuid(), pid=100,
                                     context=self.context, output=self.root / "startup.json",
                                     strace="unavailable")
        with patch.object(diagnostics.subprocess, "Popen", side_effect=OSError("ERROR_CREDENTIAL_CANARY")):
            diagnostics.monitor(args)
        bundle = json.loads(args.output.read_text())
        self.assertEqual(bundle["collectorErrors"], 1)
        self.assertFalse(bundle["attached"])
        self.assertNotIn("CREDENTIAL_CANARY", json.dumps(bundle))

    def test_standard_attachment_observes_failure_without_replacing_target_or_its_exit(self):
        path = self.authority / "explicit.lock"
        output = self.root / "attached.json"
        # Only this disposable test child opts into same-uid ptrace. The VM uses
        # its root observer; the product and its launcher never get a hook.
        code = ("import ctypes,fcntl,sys; "
                "assert ctypes.CDLL(None).prctl(0x59616d61,ctypes.c_ulong(-1),0,0,0)==0; "
                "print('ready',flush=True); sys.stdin.readline(); "
                "f=open(sys.argv[1],'a'); "
                "exec('try:\\n fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)\\n'"
                "+'except BlockingIOError:\\n sys.exit(37)')")
        with path.open("w") as holder:
            fcntl.flock(holder, fcntl.LOCK_EX)
            target = subprocess.Popen([sys.executable, "-c", code, str(path)], stdin=subprocess.PIPE,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            observer = None
            try:
                self.assertEqual(target.stdout.readline(), "ready\n")
                observer = subprocess.Popen([sys.executable, SPEC.origin, "--pid", str(target.pid),
                    "--uid", str(os.getuid()), "--authority", str(self.authority), "--context",
                    str(self.context), "--output", str(output), "--strace", "strace"],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                deadline = time.monotonic() + 3
                while time.monotonic() < deadline:
                    if output.exists() and json.loads(output.read_text())["attached"]:
                        break
                    time.sleep(0.01)  # Test-only observer readiness, never an operation retry.
                self.assertTrue(output.exists() and json.loads(output.read_text())["attached"])
                target.stdin.write("continue\n"); target.stdin.flush()
                self.assertEqual(target.wait(timeout=3), 37)
                self.assertEqual(observer.wait(timeout=3), 0)
                bundle = json.loads(output.read_text())
                self.assertEqual(len(bundle["events"]), 1)
                self.assertEqual(bundle["events"][0]["tid"], target.pid)
                self.assertEqual(bundle["events"][0]["lockPath"], "explicit.lock")
                self.assertEqual(bundle["events"][0]["laterSnapshot"]["pathIdentity"]["inode"],
                                 os.fstat(holder.fileno()).st_ino)
            finally:
                for process in (observer, target):
                    if process is not None:
                        if process.poll() is None:
                            process.terminate()
                        process.communicate(timeout=3)

    def test_browser_chronology_is_bounded_sanitized_and_diagnostic_failures_do_not_throw(self):
        browser = pathlib.Path(__file__).parent / "creation_acceptance_browser.cjs"
        script = r"""
const assert = require('node:assert/strict');
const {creationChronology} = require(process.argv[1]);
const c = creationChronology('plan-explicit', 'fixture-1', () => {throw Error('ERROR_CREDENTIAL_CANARY');});
c.phase('wait for ready', 'explicit');
const request = {method: () => 'POST',
  url: () => 'https://user:URL_CREDENTIAL_CANARY@fixture.workspace.example.test/api/sessions/explicit/implement-plan?TOKEN_CREDENTIAL_CANARY#FRAGMENT_CREDENTIAL_CANARY',
  postData: () => {throw Error('BODY_CREDENTIAL_CANARY');},
  headers: () => {throw Error('HEADER_CREDENTIAL_CANARY');}};
for (let i=0; i<200; i++) c.emit('request-start', request);
c.emit('response', request, 202);
const b = c.bundle();
assert.equal(b.events.length,128); assert.equal(b.dropped,74);
assert.equal(b.events.at(-1).status,202);
assert.equal(b.events.at(-1).path,'/api/sessions/explicit/implement-plan');
assert.equal(b.diagnosticErrors,1);
assert(!JSON.stringify(b).includes('CREDENTIAL_CANARY'));
assert(Buffer.byteLength(JSON.stringify(b))<1048576);
"""
        result = subprocess.run(["node", "-e", script, str(browser.resolve())], capture_output=True,
                                text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
