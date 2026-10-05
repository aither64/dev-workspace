#!/usr/bin/env python3
"""Opt-in installed acceptance support. Execute only after review, under a watcher.

prepare creates a fresh private Git/environment fixture; exec runs an explicitly
supplied command in its allowlisted environment; portal is the real service's
private child launcher; archive uses a real PTY and exactly one expected y.
No services, profiles, native threads or worker are started by prepare. No cleanup
command: Main stops exact recorded fixture handles and retains failed evidence.
The optional committed runtime copy prepares the dated workspace source tool;
it does not install that tool or manufacture native/creation evidence.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import subprocess
import time


ROOT = re.compile(r"/tmp/archive-acceptance-[a-z0-9]{1,12}")
SLUG = "2026-10-04-fixture-legacy"
PROSE = b"# Fixture legacy state\r\n\r\nProse says complete; it is not lifecycle authority.\r\n"


def write_json(path, value):
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def load(root):
    assert ROOT.fullmatch(str(root)) and root.resolve(strict=True) == root
    spec = json.loads((root / "fixture.json").read_text())
    assert spec["root"] == str(root) and spec["uid"] == os.getuid()
    assert spec["purpose"] == "disposable-archive-acceptance" and spec["status"] == "prepared"
    assert spec["workspace"] == str(root / "workspace") and spec["slug"] == SLUG
    assert spec["workspaceName"] == "acceptance-" + root.name.removeprefix("archive-acceptance-")
    return spec


def git(spec, *args):
    return subprocess.check_output(["git", *map(str, args)], env=spec["environment"],
                                   cwd=spec["root"], stdin=subprocess.DEVNULL).decode().strip()


def prepare(args):
    root = Path(args.root)
    assert ROOT.fullmatch(str(root)) and not root.exists() and not root.is_symlink()
    package = Path(args.package).resolve(strict=True)
    assert package.parent == Path("/nix/store")
    assert all((package / "bin" / command).is_file() for command in ("dev-session", "workspace-host", "workspace-portal"))
    paths = args.tool_path.split(":")
    assert paths and all(p.startswith("/nix/store/") and Path(p).is_dir() for p in paths)
    assert bool(args.runtime_source) == bool(args.runtime_revision)
    if args.runtime_source:
        runtime_source = Path(args.runtime_source).resolve(strict=True)
        assert re.fullmatch(r"[0-9a-f]{40}", args.runtime_revision)
    os.umask(0o077)
    root.mkdir(mode=0o700)
    for name in ("home", "config", "state", "cache", "run", "tmp", "codex", "origins", "seeds", "workspace", "logs", "browser"):
        (root / name).mkdir(mode=0o700)
    workspace = root / "workspace"
    for name in ("repos", "work", "worktrees", "archive"):
        (workspace / name).mkdir()
    state = root / "state/dev-workspaces"
    state.mkdir()
    (root / "config/dev-workspaces").mkdir()
    instance = "acceptance-" + root.name.removeprefix("archive-acceptance-")
    gitconfig = root / "home/gitconfig"
    gitconfig.touch(mode=0o600)
    environment = {
        "PATH": args.tool_path, "LANG": "C.UTF-8", "SHELL": "/bin/sh",
        "HOME": str(root / "home"), "TMPDIR": str(root / "tmp"),
        "XDG_CONFIG_HOME": str(root / "config"), "XDG_STATE_HOME": str(root / "state"),
        "XDG_CACHE_HOME": str(root / "cache"), "XDG_RUNTIME_DIR": str(root / "run"),
        "CODEX_HOME": str(root / "codex"), "DEV_WORKSPACE_CODEX_HOME": str(root / "codex"),
        "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED": "1",
        "DEV_WORKSPACES_CONFIG": str(root / "config/dev-workspaces/registry.json"),
        "DEV_WORKSPACES_STATE": str(state), "DEV_WORKSPACES_RUNTIME_DIR": str(root / "run"),
        "DEV_WORKSPACES_PROFILE": str(state / "profile"),
        "DEV_WORKSPACES_SKILLS_DIR": str(root / "home/.codex/skills"),
        "DEV_WORKSPACES_ROUTER_SOCKET": str(root / "run/router.sock"),
        "DEV_WORKSPACE_NAME": instance,
        "GIT_CONFIG_GLOBAL": str(gitconfig), "GIT_CONFIG_SYSTEM": "/dev/null",
        "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_ALLOW_PROTOCOL": "file",
    }
    spec = {"schema": 1, "purpose": "disposable-archive-acceptance", "uid": os.getuid(),
            "root": str(root), "workspace": str(workspace), "workspaceName": instance,
            "package": str(package), "slug": SLUG, "environment": environment,
            "status": "prepared", "origins": {}}
    with (root / "logs/preparation.log").open("xb") as log:
        def run(*arguments):
            subprocess.run(["git", *map(str, arguments)], env=environment, cwd=root,
                           stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT, check=True)
        run("config", "--global", "user.name", "Archive acceptance fixture")
        run("config", "--global", "user.email", "archive-acceptance@example.invalid")
        for project in ("workspace", "alpha", "beta"):
            origin = root / "origins" / (project + ".git")
            raw = "git@github.com:fixture-archive-acceptance/" + project + ".git"
            run("init", "--bare", "--initial-branch=master", origin)
            run("config", "--global", "url." + origin.as_uri() + ".insteadOf", raw)
            spec["origins"][project] = {"raw": raw, "local": str(origin)}
            if project == "workspace":
                continue
            seed = root / "seeds" / project
            run("init", "--initial-branch=master", seed)
            (seed / "README.md").write_text("# Disposable fixture\n")
            run("-C", seed, "add", "README.md")
            run("-C", seed, "commit", "-m", "fixture: initial default")
            if project == "alpha":
                run("-C", seed, "switch", "-c", SLUG)
                (seed / "feature.txt").write_text("Retain this exact feature commit.\n")
                run("-C", seed, "add", "feature.txt")
                run("-C", seed, "commit", "-m", "fixture: retained feature")
                run("-C", seed, "switch", "master")
                run("-C", seed, "merge", "--ff-only", SLUG)
            run("-C", seed, "remote", "add", "origin", raw)
            run("-C", seed, "push", "origin", "master")
            if project == "alpha":
                run("-C", seed, "push", "origin", SLUG)
            canonical = workspace / "repos" / (project + ".git")
            run("clone", "--bare", raw, canonical)
            run("--git-dir=" + str(canonical), "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
            run("--git-dir=" + str(canonical), "fetch", "origin")
            run("--git-dir=" + str(canonical), "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
        if args.runtime_source:
            # Synthetic private Git preparation from an explicit reviewed commit.
            # Native root/team/rollouts still come only from their real owners.
            canonical = workspace / "repos/dev-workspace.git"
            run("clone", "--bare", "--shared", runtime_source, canonical)
            raw = "git@github.com:aither64/dev-workspace.git"
            run("--git-dir=" + str(canonical), "remote", "set-url", "origin", raw)
            source = root / "runtime-source"
            run("--git-dir=" + str(canonical), "worktree", "add", "--detach", source, args.runtime_revision)
            spec["runtimeSource"] = str(source)
            spec["runtimeRevision"] = args.runtime_revision
            spec["origins"]["dev-workspace"] = {"raw": raw, "local": str(canonical)}
        run("init", "--initial-branch=master", workspace)
        (workspace / ".gitignore").write_text("/repos/\n/worktrees/\n")
        (workspace / ".dev-workspace.json").write_text(json.dumps({
            "schema": 2, "displayLabel": "Disposable archive acceptance", "hostLabel": "fixture", "sshHost": "",
            "portal": {"hostname": "acceptance.invalid", "aliases": []}, "developmentClusterProviders": [],
        }, indent=2) + "\n")
        tracking = workspace / "work" / SLUG
        tracking.mkdir()
        (tracking / "plan.md").write_text("# Fixture legacy plan\n\nPreserve this text.\n")
        (tracking / "state.md").write_bytes(PROSE)
        (tracking / "evidence.txt").write_text("Unregistered artifact retained as tracking content.\n")
        peer = workspace / "work/2026-10-04-fixture-peer"
        peer.mkdir()
        (peer / "state.md").write_text("---\nlifecycle: active\n---\nPeer sentinel.\n")
        (peer / "plan.md").write_text("# Peer\n")
        (workspace / "staged.txt").write_text("initial staged sentinel\n")
        (workspace / "unstaged.txt").write_text("initial unstaged sentinel\n")
        run("-C", workspace, "add", ".gitignore", ".dev-workspace.json", "work", "staged.txt", "unstaged.txt")
        run("-C", workspace, "commit", "-m", "fixture: legacy tracking without manifest")
        run("-C", workspace, "remote", "add", "origin", spec["origins"]["workspace"]["raw"])
        run("-C", workspace, "push", "-u", "origin", "master")
        run("-C", workspace, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
        (workspace / "staged.txt").write_text("unrelated staged sentinel\n")
        run("-C", workspace, "add", "staged.txt")
        (workspace / "unstaged.txt").write_text("unrelated unstaged sentinel\n")
        hook = workspace / ".git/hooks/pre-commit"
        hook.write_text("#!/bin/sh\nprintf 'commit hook ran\\n' >> '" + str(root / "logs/hooks.log") + "'\n")
        hook.chmod(0o700)
    write_json(root / "fixture.json", spec)
    write_json(root / "logs/before.json", inventory(spec))
    print(root / "fixture.json")


def inventory(spec):
    workspace = Path(spec["workspace"])
    repos = {"workspace": ["-C", str(workspace)]}
    repos.update({name: ["--git-dir=" + str(workspace / "repos" / (name + ".git"))] for name in ("alpha", "beta")})
    if "runtimeSource" in spec:
        repos["dev-workspace"] = ["--git-dir=" + str(workspace / "repos/dev-workspace.git")]
    return {"repositories": {name: {
        "origin": git(spec, *prefix, "config", "--get", "remote.origin.url"),
        "refs": git(spec, *prefix, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/remotes/origin"),
        "worktrees": git(spec, *prefix, "worktree", "list", "--porcelain"),
    } for name, prefix in repos.items()},
        "index": git(spec, "-C", workspace, "ls-files", "--stage", "--", "staged.txt", "unstaged.txt"),
        "sentinels": {name: hashlib.sha256((workspace / name).read_bytes()).hexdigest() for name in ("staged.txt", "unstaged.txt", "work/2026-10-04-fixture-peer/state.md", "work/2026-10-04-fixture-peer/plan.md")},
        "tracking_head": git(spec, "-C", workspace, "rev-parse", "HEAD")}


def preserve_sentinels(spec, before, after):
    assert before["index"] == after["index"] and before["sentinels"] == after["sentinels"]
    for name in ("alpha", "beta"):
        assert before["repositories"][name]["refs"] == after["repositories"][name]["refs"]
    for name in ("workspace", "alpha", "beta"):
        assert after["repositories"][name]["origin"] == spec["origins"][name]["raw"]
    if "runtimeSource" in spec:
        assert before["repositories"]["dev-workspace"] == after["repositories"]["dev-workspace"]


def verify_repaired(spec, projection_file, label):
    root = Path(spec["root"])
    projection_file = Path(projection_file)
    assert projection_file.is_relative_to(root) and projection_file.parent != root / "workspace/work" / SLUG
    plan = json.loads(projection_file.read_text())
    assert plan["schema"] == 1 and plan["workspace"] == spec["workspace"]
    assert len(plan["sessions"]) == 1
    row = plan["sessions"][0]
    assert row["slug"] == SLUG and row["blockers"] == []
    manifest = row["target"]["manifest"]
    assert "creation" not in manifest and "goals" not in manifest
    assert len(manifest["repositories"]) == 1
    obligation = manifest["repositories"][0]
    assert obligation["project"] == "alpha" and obligation["branch"] == SLUG
    assert "initial_base_sha" not in obligation and "final_head_sha" not in obligation
    if row["root_thread_id"] is None:
        assert "codex" not in manifest
    else:
        native = json.loads((root / "logs/native-root.json").read_text())
        assert manifest["codex"]["thread_id"] == row["root_thread_id"] == native["threadId"]
        assert manifest["codex"]["client_version"] == "0.160.0"
    if "runtimeSource" in spec:
        assert plan["runtime_source"] == spec["runtimeSource"] and plan["runtime_commit"] == spec["runtimeRevision"]
    tracking = root / "workspace/work" / SLUG
    assert (tracking / "state.md").read_bytes() == b"---\nlifecycle: active\n---\n" + PROSE
    assert hashlib.sha256((tracking / "portal.yml").read_bytes()).hexdigest() == row["target"]["portal_sha256"]
    assert (tracking / "evidence.txt").read_text() == "Unregistered artifact retained as tracking content.\n"
    before = json.loads((root / "logs/before.json").read_text())
    after = inventory(spec)
    preserve_sentinels(spec, before, after)
    commits = git(spec, "-C", spec["workspace"], "rev-list", before["tracking_head"] + "..HEAD").splitlines()
    assert len(commits) == 1, "one exact active baseline commit, including after reapply"
    changed = git(spec, "-C", spec["workspace"], "diff-tree", "--no-commit-id", "--name-only", "-r", commits[0]).splitlines()
    assert sorted(changed) == [f"work/{SLUG}/portal.yml", f"work/{SLUG}/state.md"]
    assert (root / "logs/hooks.log").read_text().splitlines() == ["commit hook ran"]
    write_json(root / "logs" / (label + ".repaired.json"), {"digest": plan["digest"], "baselineCommit": commits[0], "inventory": after})


def verify_archived(spec, label):
    root = Path(spec["root"])
    workspace = root / "workspace"
    archived = workspace / "archive" / SLUG
    assert not (workspace / "work" / SLUG).exists()
    assert not (workspace / "worktrees" / SLUG).exists()
    assert (archived / "state.md").read_bytes().endswith(PROSE)
    assert (archived / "evidence.txt").read_text() == "Unregistered artifact retained as tracking content.\n"
    assert b"initial_base_sha:" not in (archived / "portal.yml").read_bytes()
    before = json.loads((root / "logs/before.json").read_text())
    after = inventory(spec)
    preserve_sentinels(spec, before, after)
    alpha_head = git(spec, "--git-dir=" + str(workspace / "repos/alpha.git"), "rev-parse", SLUG)
    assert ("final_head_sha: " + alpha_head).encode() in (archived / "portal.yml").read_bytes()
    assert git(spec, "-C", workspace, "log", "--format=%H", "--", f"archive/{SLUG}/state.md").count("\n") == 0
    assert git(spec, "-C", workspace, "log", "--format=%H", "--", f"archive/{SLUG}/state.md")
    locks = workspace / "worktrees/.locks"
    assert not (locks / (SLUG + ".archive.json")).exists()
    # The ordinary owner unlinks its proved completed journal/sidecar. Tests
    # never delete either.
    assert not (locks / (SLUG + ".archive-cleanup.json")).exists()
    write_json(root / "logs" / (label + ".archived.json"), {"inventory": after,
               "evidence": "real Git/tracking final state; native retirement requires separate exact owner verification"})


def archive_pty(spec, label, expected):
    assert re.fullmatch(r"[a-z][a-z0-9-]{0,31}", label)
    command = [spec["package"] + "/bin/dev-session", "--workspace", spec["workspaceName"], "archive", SLUG, "--as-is"]
    pid, descriptor = pty.fork()
    if pid == 0:
        os.chdir(spec["workspace"])
        os.execve(command[0], command, spec["environment"])
    # pty.fork gives this exact child its own session/process group. Never use
    # broad process matching; kill only this owned, unreaped child on cancellation.
    waited = False
    output = bytearray()
    answered = False
    prompt = ("Archive completed session " + SLUG + "? [y/N] ").encode()
    deadline = time.monotonic() + 420
    def interrupted(signum, frame):
        raise InterruptedError("fixture PTY canceled")
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        with (Path(spec["root"]) / "logs" / (label + ".pty.log")).open("xb") as log:
            while time.monotonic() < deadline:
                ready, _, _ = select.select([descriptor], [], [], 0.2)
                if ready:
                    try:
                        chunk = os.read(descriptor, 65536)
                    except OSError as error:
                        if error.errno != 5:  # Linux PTY EIO after slave closes.
                            raise
                        break
                    if not chunk:
                        break
                    output.extend(chunk)
                    log.write(chunk)
                    log.flush()
                    if len(output) > 2 * 1024 * 1024:
                        raise RuntimeError("fixture PTY output limit exceeded")
                    if prompt in output and not answered:
                        os.write(descriptor, b"y\n")
                        answered = True
                ended, status = os.waitpid(pid, os.WNOHANG)
                if ended:
                    waited = True
                    break
            else:
                raise TimeoutError("fixture archive timed out")
        if not waited:
            while time.monotonic() < deadline:
                ended, status = os.waitpid(pid, os.WNOHANG)
                if ended:
                    waited = True
                    break
                time.sleep(0.1)
            if not waited:
                raise TimeoutError("fixture archive did not exit after closing its PTY")
        code = os.waitstatus_to_exitcode(status)
        assert answered and output.count(prompt) == 1, "not the one expected fixture confirmation"
        assert (code == 0) == (expected == "success"), f"unexpected archive exit {code}"
        write_json(Path(spec["root"]) / "logs" / (label + ".result.json"), {"command": command, "pid": pid, "exit": code, "confirmation": "one y on real PTY"})
    finally:
        if not waited:
            os.killpg(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        os.close(descriptor)
        for sig, handler in previous.items():
            signal.signal(sig, handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True)
    sub = parser.add_subparsers(dest="action", required=True)
    prep = sub.add_parser("prepare")
    prep.add_argument("--package", required=True)
    prep.add_argument("--tool-path", required=True)
    prep.add_argument("--runtime-source")
    prep.add_argument("--runtime-revision")
    execute = sub.add_parser("exec")
    execute.add_argument("--env", action="append", default=[])
    execute.add_argument("command", nargs=argparse.REMAINDER)
    portal = sub.add_parser("portal")
    portal.add_argument("--label", required=True)
    mapping = sub.add_parser("mapping")
    choice = mapping.add_mutually_exclusive_group(required=True)
    choice.add_argument("--threadless", action="store_true")
    choice.add_argument("--native", action="store_true")
    repaired = sub.add_parser("verify-repaired")
    repaired.add_argument("--projection", required=True)
    repaired.add_argument("--label", required=True)
    archived = sub.add_parser("verify-archived")
    archived.add_argument("--label", required=True)
    snap = sub.add_parser("inventory")
    snap.add_argument("--label", required=True)
    terminal = sub.add_parser("archive")
    terminal.add_argument("--label", required=True)
    terminal.add_argument("--expect", choices=("success", "refusal"), default="success")
    args = parser.parse_args()
    if args.action == "prepare":
        return prepare(args)
    root = Path(args.root)
    spec = load(root)
    if args.action == "exec":
        environment = dict(spec["environment"])
        environment["ARCHIVE_ACCEPTANCE_FIXTURE"] = str(root / "fixture.json")
        for entry in args.env:
            key, value = entry.split("=", 1)
            assert key in ("ARCHIVE_ACCEPTANCE_STAGE", "ARCHIVE_ACCEPTANCE_MODE", "PLAYWRIGHT_MODULE", "CHROMIUM_EXECUTABLE", "REPAIR_LEGACY_CONTEXT")
            if key == "REPAIR_LEGACY_CONTEXT":
                context = Path(value)
                assert context.is_absolute() and context.resolve(strict=True) == context and context.is_relative_to(root)
                assert context.stat().st_uid == spec["uid"] and context.stat().st_mode & 0o777 == 0o600
            environment[key] = value
        command = args.command
        if command and command[0] == "--":
            command = command[1:]
        assert command
        os.chdir(spec["workspace"])
        os.execvpe(command[0], command, environment)
    elif args.action == "portal":
        assert re.fullmatch(r"[a-z][a-z0-9-]{0,31}", args.label)
        unit = "workspace-portal@" + spec["workspaceName"] + ".service"
        cgroup = Path("/proc/self/cgroup").read_text()
        assert any(line.endswith("/" + unit) for line in cgroup.splitlines()), "requires the real exact fixture service cgroup"
        proxy = json.loads((root / "proxy.json").read_text())
        assert re.fullmatch(r"https://127\.0\.0\.1:[0-9]+", proxy["origin"])
        runtime = root / "run" / spec["workspaceName"]
        state = Path(spec["environment"]["DEV_WORKSPACES_STATE"])
        command = [spec["package"] + "/bin/workspace-portal", "serve", "--workspace", spec["workspace"],
                   "--base-url", proxy["origin"], "--unix-socket", str(runtime / "portal.sock"),
                   "--dev-session", spec["package"] + "/bin/dev-session", "--authority-dir", str(runtime / "authority"),
                   "--user-state-root", str(state), "--package-root", spec["package"], "--workspace-name", spec["workspaceName"],
                   "--registration-marker", str(runtime / "registration.json"), "--host-profile", str(state / "profile"),
                   "--transition-lock", str(state / "transition.lock"), "--codex-socket", str(runtime / "app-server.sock"),
                   "--codex-version", "0.160.0", "--tmux", "tmux"]
        write_json(root / "logs" / ("portal-" + args.label + "-launch.json"), {"pid": os.getpid(), "unit": unit, "cgroup": cgroup, "command": command})
        os.chdir(spec["workspace"])
        os.execve(command[0], command, spec["environment"])
    elif args.action == "mapping":
        root_id = None if args.threadless else json.loads((root / "logs/native-root.json").read_text())["threadId"]
        write_json(root / "mapping.json", {"schema": 1, "workspace": spec["workspace"], "sessions": [{
            "slug": SLUG, "root_thread_id": root_id, "rationale": "Reviewed disposable exact native root or threadless scope; retain exact alpha branch evidence."}]})
    elif args.action == "verify-repaired":
        assert re.fullmatch(r"[a-z][a-z0-9-]{0,31}", args.label)
        verify_repaired(spec, args.projection, args.label)
    elif args.action == "verify-archived":
        assert re.fullmatch(r"[a-z][a-z0-9-]{0,31}", args.label)
        verify_archived(spec, args.label)
    elif args.action == "inventory":
        assert re.fullmatch(r"[a-z][a-z0-9-]{0,31}", args.label)
        write_json(root / "logs" / (args.label + ".inventory.json"), inventory(spec))
    else:
        archive_pty(spec, args.label, args.expect)


if __name__ == "__main__":
    main()
