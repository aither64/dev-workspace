"""Disposable creation-VM diagnostics; never part of the installed application.

strace supplies the failed flock event. A later path/proc snapshot is a separate
evidence tier: it cannot prove the refused descriptor's identity or its holder
at refusal. No FD-lifetime reconstruction or process command-line reads occur.
"""

import argparse
import json
import os
import pathlib
import re
import signal
import stat
import subprocess
import time

SCAN_LIMIT = 1024 * 1024
EVENT_LIMIT = 128
EVENT_BYTES = 8192
BUNDLE_BYTES = 256 * 1024  # Four bounded observer generations fit the VM bundle.
OWNER_LIMIT = 16
SCENARIO_LIMIT = 64
SLUG = r"[A-Za-z0-9][A-Za-z0-9_-]{0,199}"
LOCK = re.compile(rf"^({SLUG})(\.creation)?\.lock$")
TOKEN = re.compile(r"^[A-Za-z0-9_-]{1,100}$")
PHASES = {"create", "admit", "fork", "plan", "failed", "name", "retry",
          "change-effort", "delete-stale", "follow-up exchange",
          "canonical redirect and transcript", "draft restoration after reload",
          "admit creation", "wait for ready", "wait for failed"}
PREFIX = re.compile(r"^(?:\[pid\s+)?(\d+)(?:\])?\s+(\d+\.\d+)\s+"
                    r"\(\+\s*(\d+\.\d+)\)\s+(.*)$")
CALL = re.compile(r"^flock\((\d+)<([^>]+)>, (LOCK_[A-Z_|]+)(?:\))?(.*)$")
RESULT = re.compile(r"^\s*= (-?\d+)(?: ([A-Z0-9_]+))?.* <(\d+\.\d+)>$")
RESUMED = re.compile(r"^<\.\.\. flock resumed>\)?(.*)$")


def bounded_read(path, limit=SCAN_LIMIT):
    with open(path, "rb") as stream:
        data = stream.read(limit + 1)
    return data[:limit].decode("utf-8", errors="replace"), len(data) > limit


def context(path):
    try:
        text, truncated = bounded_read(path, EVENT_BYTES)
        raw = json.loads(text)
        if truncated or not isinstance(raw, dict):
            return {"scenario": "unknown", "phase": "unknown"}
        result = {key: raw[key] for key in ("scenario", "correlation", "sourceSlug")
                  if isinstance(raw.get(key), str) and TOKEN.fullmatch(raw[key])}
        result.setdefault("scenario", "unknown")
        result["phase"] = raw.get("phase") if raw.get("phase") in PHASES else "unknown"
        return result
    except (OSError, ValueError, TypeError):
        return {"scenario": "unknown", "phase": "unavailable"}


def process_info(proc, pid, fixture_uid, portal_pid):
    result = {"pid": pid, "executableClass": "unavailable", "invocationClass": "unknown"}
    try:
        text, truncated = bounded_read(proc / str(pid) / "status", EVENT_BYTES)
        uid = re.search(r"^Uid:\s+(\d+)", text, re.M)
        tgid = re.search(r"^Tgid:\s+(\d+)", text, re.M)
        if truncated or not uid or not tgid or int(uid[1]) != fixture_uid:
            result["unavailable"] = "outside-fixture-uid-or-incomplete-status"
            return result
        result["tgid"] = int(tgid[1])
        executable = pathlib.Path(os.readlink(proc / str(pid) / "exe")).name
        # No arbitrary comm/executable/argv text reaches the diagnostic bundle.
        result["executableClass"] = {"workspace-portal": "portal", "ruby": "ruby",
                                     "git": "git"}.get(executable, "other")
        if result["tgid"] == portal_pid:
            result["invocationClass"] = "fixture-portal-service"
    except OSError:
        result["unavailable"] = "process-exited-or-proc-unreadable"
    return result


def snapshot(path, proc, fixture_uid, portal_pid):
    """One bounded snapshot, with no polling or lock acquisition."""
    result = {"observedMonotonicNs": time.monotonic_ns(), "owners": [],
              "identityBasis": "path-at-observation-only",
              "holderAtRefusal": "unproved", "ownersTruncated": False}
    try:
        observed = os.stat(path, follow_symlinks=False)
        if not stat.S_ISREG(observed.st_mode) or observed.st_uid != fixture_uid:
            result["unavailable"] = "path-not-fixture-owned-regular-file"
            return result
        result["pathIdentity"] = {"deviceMajor": os.major(observed.st_dev),
                                  "deviceMinor": os.minor(observed.st_dev),
                                  "inode": observed.st_ino}
        # Reserve scan budget for the bounded matching-owner status reads too.
        text, truncated = bounded_read(proc / "locks", SCAN_LIMIT - OWNER_LIMIT * (EVENT_BYTES + 1))
        result["scanTruncated"] = truncated
        for line in text.splitlines():
            fields = line.split()
            # Waiting (->) records are not holders. Match dev/inode, not paths.
            if len(fields) < 8 or fields[1:3] != ["FLOCK", "ADVISORY"]:
                continue
            try:
                major, minor, inode = fields[5].split(":")
                identity = (int(major, 16), int(minor, 16), int(inode))
                if identity != (os.major(observed.st_dev), os.minor(observed.st_dev), observed.st_ino):
                    continue
                pid = int(fields[4])
                if pid <= 0 or fields[3] not in ("READ", "WRITE"):
                    continue
            except (ValueError, IndexError):
                continue
            if len(result["owners"]) == OWNER_LIMIT:
                result["ownersTruncated"] = True
                break
            owner = process_info(proc, pid, fixture_uid, portal_pid)
            owner["mode"] = fields[3]
            result["owners"].append(owner)
        if not result["owners"]:
            result["unavailable"] = "no-matching-owner-at-later-snapshot"
    except OSError:
        result["unavailable"] = "path-removed-or-proc-unreadable"
    return result


class Observer:
    def __init__(self, authority, proc, fixture_uid, portal_pid, context_path, output):
        self.authority = pathlib.Path(authority)
        self.proc = pathlib.Path(proc)
        self.fixture_uid, self.portal_pid = fixture_uid, portal_pid
        self.context_path, self.output = context_path, pathlib.Path(output)
        self.events, self.counts, self.pending = [], {}, {}
        self.dropped = self.unresolved = self.collector_errors = 0
        self.attached = False
        self.bytes = 0

    def save(self):
        bundle = {"schema": 1, "observer": "external-strace-flock-only",
                  "attached": self.attached, "events": self.events,
                  "dropped": self.dropped, "unresolved": self.unresolved,
                  "collectorErrors": self.collector_errors,
                  "descriptorIdentity": "unavailable-no-fd-lifetime-proof",
                  "snapshotLimitations": "later owners do not prove holder at refusal"}
        data = json.dumps(bundle, separators=(",", ":"))
        if len(data.encode()) > BUNDLE_BYTES:
            raise ValueError("bounded bundle exceeded")
        temporary = self.output.with_suffix(".pending")
        temporary.write_text(data + "\n")
        os.chmod(temporary, 0o600)
        temporary.replace(self.output)

    def feed(self, line):
        if line.startswith("strace: Process ") and " attached" in line:
            self.attached = True
            self.save()
            return
        match = PREFIX.fullmatch(line.rstrip("\n"))
        if not match:
            # Signals/exit/attach failures are never copied as arbitrary text.
            self.unresolved += 1
            return
        pid, wall, relative, syscall = match.groups()
        pid = int(pid)
        call = CALL.fullmatch(syscall)
        if call:
            self.pending.pop(pid, None)
            fd, raw_path, mode, tail = call.groups()
            path = pathlib.Path(raw_path)
            name = LOCK.fullmatch(path.name)
            if path.parent != self.authority or not name or mode != "LOCK_EX|LOCK_NB":
                return
            entry = {"tid": pid, "fd": int(fd), "lockPath": path.name,
                     "slug": name[1], "lockKind": "creation" if name[2] else "runtime",
                     "mode": mode, "traceAttemptUnixSeconds": float(wall),
                     "traceRelativeSeconds": float(relative)}
            if tail.strip() == "<unfinished ...>":
                if len(self.pending) < 64 or pid in self.pending:
                    self.pending[pid] = entry
                else:
                    self.dropped += 1
                return
        else:
            resumed = RESUMED.fullmatch(syscall)
            entry = self.pending.pop(pid, None)
            if not resumed or entry is None:
                self.unresolved += 1
                return
            tail = resumed[1]
        result = RESULT.fullmatch(tail)
        if not result:
            self.unresolved += 1
            return
        value, errno, duration = result.groups()
        if value != "-1" or errno not in ("EAGAIN", "EWOULDBLOCK"):
            return
        metadata = context(self.context_path)
        scenario = metadata["scenario"]
        if len(self.counts) >= SCENARIO_LIMIT and scenario not in self.counts:
            self.dropped += 1
            return
        if self.counts.get(scenario, 0) >= EVENT_LIMIT:
            self.dropped += 1
            return
        entry.update(metadata)
        entry.update({"result": -1, "errno": errno,
                      "traceDurationSeconds": float(duration),
                      "traceRefusalUnixSeconds": entry["traceAttemptUnixSeconds"] + float(duration),
                      "traceClock": "realtime-entry-and-monotonic-relative-delta",
                      "observedMonotonicNs": time.monotonic_ns(),
                      "descriptorIdentity": "unavailable-no-fd-lifetime-proof"})
        entry["actor"] = process_info(self.proc, pid, self.fixture_uid, self.portal_pid)
        # Actor and source filename are independent facts, not call-stack proof.
        entry["sourceFilenameMatch"] = entry["slug"] == metadata.get("sourceSlug")
        entry["role"] = ("portal-exclusive-candidate-or-other" if
                         entry["actor"]["invocationClass"] == "fixture-portal-service" else
                         "ruby-source-or-other" if entry["actor"]["executableClass"] == "ruby" else "unknown")
        try:
            entry["laterSnapshot"] = snapshot(self.authority / entry["lockPath"], self.proc,
                                               self.fixture_uid, self.portal_pid)
        except Exception:
            # Diagnostics must never change the observed operation's outcome.
            entry["laterSnapshot"] = {"unavailable": "collector-failed"}
            self.collector_errors += 1
        entry["snapshotDelayFromObservationNs"] = (entry["laterSnapshot"].get(
            "observedMonotonicNs", entry["observedMonotonicNs"]) - entry["observedMonotonicNs"])
        entry["snapshotDelayFromRefusalSecondsEstimate"] = max(
            0, time.time() - entry["traceRefusalUnixSeconds"])
        entry["delayEstimateBasis"] = "wall-clock-correlation-not-atomic"
        encoded = len(json.dumps(entry).encode())
        if encoded > EVENT_BYTES or self.bytes + encoded > BUNDLE_BYTES - 8192:
            self.dropped += 1
            return
        self.counts[scenario] = self.counts.get(scenario, 0) + 1
        self.bytes += encoded
        self.events.append(entry)
        self.save()


def monitor(args):
    observer = Observer(args.authority, "/proc", args.uid, args.pid, args.context, args.output)
    observer.save()
    # Attach to the existing service. No wrapper, MainPID replacement, injected
    # syscall, application hook, read/write/exec trace or tracee kill-on-exit.
    try:
        tracer = subprocess.Popen([args.strace, "-f", "--always-show-pid", "-r", "-ttt", "-T",
                                   "-yy", "-e", "trace=flock", "-p", str(args.pid)],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    except OSError:
        observer.collector_errors += 1
        observer.save()
        return
    stopping = False

    def stop(_signal, _frame):
        nonlocal stopping
        stopping = True
        tracer.send_signal(signal.SIGINT)  # strace detaches; it does not kill tracees.

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        while True:
            line = tracer.stderr.readline(EVENT_BYTES + 1)
            if not line:
                break
            if len(line) > EVENT_BYTES:
                observer.dropped += 1
                while line and not line.endswith(b"\n"):
                    line = tracer.stderr.readline(EVENT_BYTES + 1)
                continue
            try:
                observer.feed(line.decode("utf-8", errors="replace"))
            except Exception:
                observer.collector_errors += 1
        tracer.wait()
        if tracer.returncode and not stopping:
            observer.collector_errors += 1
    finally:
        observer.unresolved += len(observer.pending)
        observer.save()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pid", required=True, type=int)
    parser.add_argument("--uid", required=True, type=int)
    parser.add_argument("--authority", required=True)
    parser.add_argument("--context", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--strace", required=True)
    monitor(parser.parse_args())
