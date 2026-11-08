{
  pkgs,
  self,
  devWorkspace,
}:
pkgs.testers.runNixOSTest {
  name = "dev-workspace-session-archive-lifecycle";
  nodes.machine = {
    imports = [ self.nixosModules.host ];
    users.users.developer = {
      isNormalUser = true;
      uid = 1000;
      linger = true;
    };
    services.dev-workspaces = {
      enable = true;
      owner = "developer";
      hostName = "workspace.example.test";
      wildcardHost = "*.workspace.example.test";
      codexInitialProvider = "primary";
      codexProviders = [
        {
          id = "primary";
          label = "Archive fixture";
          baseURL = "http://127.0.0.1:2455/primary/v1";
          tokenFile = "/home/developer/.local/state/dev-workspaces/codex-homes/fixture/fixture.token";
        }
      ];
    };
    networking.extraHosts = "127.0.0.1 fixture.workspace.example.test";
    environment.systemPackages = [
      pkgs.git
      pkgs.curl
      pkgs.nodejs
      pkgs.chromium
    ];
    virtualisation = {
      memorySize = 4096;
      cores = 2;
      additionalPaths = [
        devWorkspace
        pkgs.playwright-driver
      ];
    };
    system.stateVersion = "26.05";
  };
  testScript = ''
    import json
    import shlex
    import hashlib
    import re

    machine.start()
    machine.wait_for_unit("user@1000.service")
    package = "${devWorkspace}"
    root = "/home/developer/workspace"
    state = "/home/developer/.local/state/dev-workspaces"
    user_prefix = "runuser -u developer -- env XDG_RUNTIME_DIR=/run/user/1000 "
    cli = package + "/bin/dev-session"

    def user(command):
        return machine.succeed(user_prefix + "sh -c " + shlex.quote(command))

    def add_repair_worktree(slug):
        command = user_prefix + "sh -c " + shlex.quote(
            "cd " + root + " && " + cli + " worktree add " + slug + " project --as-is") + " 2>&1"
        refusal = "error: another dev-session command is changing " + slug
        attempts, last = 0, None
        def admitted(final):
            nonlocal attempts, last
            if final:
                raise AssertionError(("worktree admission deadline", command, attempts, last))
            attempts += 1
            # Preserve user()/succeed's unqualified command wait; only admission retries get 30s.
            status, output = machine.execute(command, timeout=None)
            last = {"status": status, "output": output[-2048:] if isinstance(output, str) else repr(output)[:2048]}
            assert type(status) is int and isinstance(output, str), ("invalid worktree result", command, attempts, last)
            if status == 0:
                return True
            if status == 1 and output in (refusal, refusal + "\n"):
                return False
            raise AssertionError(("worktree add failed", command, attempts, last))
        retry(admitted, timeout_seconds=30)

    def browser(slug, action):
        output = user("PLAYWRIGHT_MODULE=${pkgs.playwright-driver} "
                      "CHROMIUM_EXECUTABLE=${pkgs.chromium}/bin/chromium "
                      "${pkgs.nodejs}/bin/node ${../../test/archive_lifecycle_browser.cjs} "
                      + shlex.quote(slug) + " " + action)
        return json.loads(output)

    def operation(slug):
        output = user("curl -fsS --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
                      + "http://localhost/api/sessions/" + slug + "/operation")
        return json.loads(output)

    def wait_operation(slug, wanted):
        # Fail on an unexpected terminal result instead of polling it until timeout.
        command = "curl -fsS --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
        command += "http://localhost/api/sessions/" + slug + "/operation"
        command += " | ${pkgs.python3}/bin/python3 -c " + shlex.quote(
            "import sys,json; d=json.load(sys.stdin); "
            "assert d.get('state') in ['complete','failed','paused']; print(json.dumps(d))")
        output = machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(command))
        result = json.loads(output)
        assert result["state"] == wanted, result
        return result

    def unit(receipt):
        return "workspace-archive-" + receipt["receiptId"] + "-" + str(receipt["attempt"]) + ".service"

    def fixture(slug):
        user("cd " + root + " && " + cli + " start " + slug + " --as-is --no-codex --no-attach")
        user("cd " + root + " && " + cli + " worktree add " + slug + " project --as-is")
        checkout = root + "/worktrees/" + slug + "/project"
        user("cd " + checkout + " && printf staged > tracked.txt && git add tracked.txt "
             "&& printf unstaged > tracked.txt && printf untracked > untracked.txt "
             "&& printf ignored > ignored.tmp")
        # Commit substantive tracking so ordinary CLI creation/revival proofs apply.
        user("cd " + root + " && git add work/" + slug + " && git commit -m 'fixture: session' "
             "&& git push origin master")
        return checkout

    def assert_restored(slug, checkout):
        assert user("cat " + checkout + "/tracked.txt") == "base"
        assert user("git -C " + checkout + " show :tracked.txt") == "base"
        user("test ! -e " + checkout + "/untracked.txt && test ! -e " + checkout + "/ignored.tmp")
        assert user("git -C " + checkout + " status --porcelain --ignored=matching").strip() == ""
        assert user("git -C " + checkout + " symbolic-ref --short HEAD").strip() == slug

    def archived(slug, checkout):
        result = wait_operation(slug, "complete")
        # Operation GET is cache-only; target publication follows its own cadence.
        # Keep terminal failures strict, then wait for this receipt's target view.
        command = "curl -fsS --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
        command += "http://localhost/api/sessions/" + slug + "/operation"
        command += " | ${pkgs.python3}/bin/python3 -c " + shlex.quote(
            "import sys,json; d=json.load(sys.stdin); "
            "assert d.get('receiptId') == " + repr(result["receiptId"]) + " and "
            "d.get('state') == 'complete' and d.get('currentTarget', {}).get('archived') is True, d; "
            "print(json.dumps(d))")
        result = json.loads(machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(command), timeout=60))
        user("test ! -e " + checkout + " && test -d " + root + "/archive/" + slug)
        user("git --git-dir=" + root + "/repos/project.git rev-parse refs/heads/" + slug)
        return result

    def revive(slug, checkout):
        browser(slug, "revive")
        wait_operation(slug, "complete")
        assert_restored(slug, checkout)

    def service_status(slug):
        return json.loads(user("cd " + root + " && " + cli + " service list " + slug + " --as-is --json"))

    def service_details_failure(details):
        # Keep this bounded evidence independent of helper/private output.
        snapshot = details.get("serviceCountSnapshot", {})
        snapshot = snapshot if isinstance(snapshot, dict) else {}
        result = {"servicesKnown": details.get("servicesKnown") if type(details.get("servicesKnown")) is bool else None,
                  "serviceCountSnapshot": {key: snapshot.get(key) if type(snapshot.get(key)) is bool else None
                      for key in ["known", "stale"]}}
        value = snapshot.get("value")
        result["serviceCountSnapshot"]["value"] = value if type(value) is int and 0 <= value <= 256 else None
        try:
            headers = user("${pkgs.coreutils}/bin/head -c 4096 /tmp/service-details-headers")
            status = re.search(r"^HTTP/[^ ]+ ([0-9]{3})", headers)
            correlation = re.search(r"(?im)^X-Portal-Read: (r[0-9a-f]{16})\s*$", headers)
            timing = re.search(r"(?im)^Server-Timing: ([^\r\n]{1,2048})", headers)
            result["httpStatus"] = int(status.group(1)) if status else None
            result["correlation"] = correlation.group(1) if correlation else None
            if timing:
                result["timing"] = re.findall(
                    r"(?:^|, )(lookup|lifecycle|interactivity|native_verify|runtime_verify|repo_validate|repo_status|team|cluster|services|limits_dependency|limits_cache_wait|resolve|total);dur=([0-9]{1,12}\.[0-9]{2})(?=, |$)",
                    timing.group(1))
            journal = user("journalctl --user --no-pager -n 80 -o cat -u workspace-portal@fixture | ${pkgs.coreutils}/bin/tail -c 32768")
            pattern = r"portal_service_read correlation=(r[0-9a-f]{16}) stage=(identity_before|helper|projection|cache_wait|identity_after) outcome=(unavailable|start|deadline|canceled|exit|output_limit|invalid) elapsed_ms=([0-9]{1,12}\.[0-9]{2}) exit_code=(-?[0-9]{1,3}) stdout_bytes=([0-9]{1,12}) stderr_bytes=([0-9]{1,12})$"
            result["serviceRead"] = [match.groups() for line in journal.splitlines()
                if (match := re.search(pattern, line)) and match.group(1) == result["correlation"]][-2:]
        except Exception:
            result["diagnostic"] = "unavailable"
        print("service-details-diagnostic " + json.dumps(result, sort_keys=True))
        return result

    def assert_service_details(details, expected_rows):
        evidence = service_details_failure(details)
        assert evidence.get("httpStatus") == 200, "unexpected service details HTTP status"
        snapshot = details.get("serviceCountSnapshot", {})
        assert type(snapshot.get("value")) is int, "invalid service count type"
        markup = details["servicesHTML"]
        rows = re.findall(r'<article\b[^>]*data-service-id="([^"]+)"[^>]*>(.*?)</article>', markup, re.S)
        if details.get("servicesKnown") is True:
            assert snapshot.get("known") is True and snapshot["value"] == 3 and snapshot.get("stale") is False, "invalid known service count"
            assert '<h2>Temporary services</h2><span>3</span>' in markup, "known count presentation changed"
            assert len(rows) == 3 and {identity for identity, _ in rows} == set(expected_rows), "unexpected public service rows"
            for identity, body in rows:
                assert re.findall(r"<h3>(.*?)</h3>", body, re.S) == [expected_rows[identity]], "unexpected service label"
            branch = "positive"
        else:
            assert details.get("servicesKnown") is False, "invalid service membership state"
            assert snapshot.get("known") is False and snapshot.get("stale") is False, "invalid unavailable flags"
            assert snapshot == {"known": False, "value": 0, "stale": False}, "invalid unavailable service count"
            assert not rows and "data-service-id=" not in markup and "<article" not in markup, "unavailable status fabricated rows"
            assert "Service status is unavailable." in markup and '<h2>Temporary services</h2><span>?</span>' in markup, "missing unavailable presentation"
            assert "No registered temporary services" not in markup, "unavailable status became confirmed empty"
            correlation = evidence.get("correlation")
            assert isinstance(correlation, str) and re.fullmatch(r"r[0-9a-f]{16}", correlation), "missing response correlation"
            assert any(row[0] == correlation and row[1] == "helper" and row[2] == "deadline" for row in evidence.get("serviceRead", [])), "missing correlated producer deadline"
            branch = "helper-deadline-fallback"
        assert "workspace-session-" not in markup and "argv" not in markup and "control_group" not in markup, "private service details"
        print("service-details-branch " + branch)

    def service_registry(slug):
        workspace_id = "workspace-" + hashlib.sha256(root.encode()).hexdigest()[:16]
        return state + "/session-services/" + workspace_id + "/" + slug + ".json"

    def launch_preview(slug, label, stubborn=False, fast=False):
        directory = root + "/work/" + slug
        source = directory + "/" + label + ".sh"
        proof = "/tmp/" + slug + "-" + label + "-stopped"
        if stubborn:
            script = "trap \"\" TERM\nsleep infinity &\nwait\n"
        else:
            check = "test -f " + shlex.quote(source) + " && test -f " + shlex.quote(directory + "/runtime.tmp")
            script = "trap " + shlex.quote(check + " && printf stopped > " + proof + "; exit 0") + " TERM\n"
            script += "sleep infinity &\nwait\n"
        script = script.replace("sleep infinity &", "touch " + source + ".ready\nsleep infinity &")
        user("printf %s " + shlex.quote(script) + " > " + source)
        command = "${pkgs.coreutils}/bin/true" if fast else "${pkgs.bash}/bin/bash " + source
        result = json.loads(user("cd " + root + " && " + cli + " service run " + slug
            + " --as-is --json --name " + label + " --cwd " + directory + " -- " + command))
        row = next(row for row in result["services"] if row["label"] == label)
        if not fast:
            machine.wait_until_succeeds(user_prefix + "test -f " + source + ".ready")
        name = "workspace-session-" + row["id"] + ".service"
        group = user("systemctl --user show " + name + " -p ControlGroup --value").strip()
        return name, group, proof

    def assert_services_retired(slug, units):
        result = service_status(slug)
        assert result["countKnown"] and result["count"] == 0 and not result["services"], result
        assert len(result["history"]) == len(units), result
        for name, group, _ in units:
            assert user("systemctl --user show " + name + " -p LoadState --value 2>/dev/null || true").strip() == "not-found"
            if group:
                user("test ! -e " + shlex.quote("/sys/fs/cgroup" + group))

    def set_service_invocation(slug, invocation):
        script = "import json,pathlib; p=pathlib.Path(" + repr(service_registry(slug)) + "); d=json.loads(p.read_text()); "
        script += "d['resources'][0]['invocation_id']=" + repr(invocation) + "; p.write_text(json.dumps(d))"
        user("${pkgs.python3}/bin/python3 -c " + shlex.quote(script))

    def diagnostics():
        print(user("journalctl --user --no-pager -n 100 -u 'workspace-archive-*' -u workspace-portal@fixture -u workspace-codex@fixture -u archive-model"))
        print(user("find " + root + "/worktrees/.locks -maxdepth 1 -type f -name '*.archive*.json' -exec cat {} \\;"))
        print(user("find ~/.local/state/dev-workspaces/codex-homes/fixture/sessions -type f -name '*.jsonl' -exec tail -n 10 {} + | tail -c 16000"))

    with subtest("install real package and user services with private Git origins"):
        user("mkdir -p " + state + " " + root + "/repos " + root + "/work " + root + "/worktrees")
        user("mkdir -p ~/.local/state/dev-workspaces/codex-homes/fixture /tmp/creation-model")
        user("printf fixture-private-token > ~/.local/state/dev-workspaces/codex-homes/fixture/fixture.token && chmod 0600 ~/.local/state/dev-workspaces/codex-homes/fixture/fixture.token")
        config = 'model_provider="primary"\nmodel="gpt-5.5"\nmodel_reasoning_effort="high"\napproval_policy="never"\nsandbox_mode="danger-full-access"\n[model_providers.primary]\nname="Archive fixture"\nbase_url="http://127.0.0.1:2455/primary/v1"\nwire_api="responses"\nrequires_openai_auth=false\nsupports_websockets=false\n'
        user("printf %s " + shlex.quote(config) + " > ~/.local/state/dev-workspaces/codex-homes/fixture/config.toml")
        user("systemd-run --user --unit=archive-model ${pkgs.python3}/bin/python ${../../test/creation_model.py}")
        machine.wait_until_succeeds("curl -fsS http://127.0.0.1:2455/primary/v1/models")
        user("git config --global user.name 'Archive fixture' && git config --global user.email fixture@example.invalid")
        user("git config --global url.file:///home/developer/origins/.insteadOf git@github.com:fixture/")
        user("mkdir ~/origins && git init --bare --initial-branch=master ~/origins/workspace.git "
             "&& git init --bare --initial-branch=master ~/origins/project.git")
        user("git init --initial-branch=master ~/seed && cd ~/seed && printf base > tracked.txt "
             "&& printf '*.tmp\\n' > .gitignore && git add . && git commit -m 'fixture: base' "
             "&& git remote add origin git@github.com:fixture/project.git && git push origin master")
        user("git init --initial-branch=master " + root + " && cd " + root + " "
             "&& printf '/repos/\\n/worktrees/\\n' > .gitignore && git add .gitignore "
             "&& git commit -m 'fixture: workspace' && git remote add origin git@github.com:fixture/workspace.git "
             "&& git push origin master && git clone --bare git@github.com:fixture/project.git repos/project.git")
        user("git --git-dir=" + root + "/repos/project.git config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*' "
             "&& git --git-dir=" + root + "/repos/project.git fetch origin "
             "&& git --git-dir=" + root + "/repos/project.git symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/master")
        user("nix-env --profile " + state + "/profile --set " + package)
        user(package + "/bin/workspace-host register fixture " + root + " --hostname fixture.workspace.example.test")
        for service in ["workspace-codex@fixture", "workspace-portal@fixture", "workspace-tmux@fixture", "workspace-router"]:
            machine.wait_until_succeeds(user_prefix + "systemctl --user is-active " + service)
        # Disable automatic retries only inside this disposable VM; tests drive sync explicitly.
        user("systemctl --user stop workspace-tracking-sync@fixture.timer")

    try:
        with subtest("preflight lead repair survives portal restart before agent shutdown"):
            slug = "2026-10-25-lead-archive-repair"
            user("printf ARCHIVE_PREPARATION_FIXTURE > /tmp/archive-goal")
            user("cd " + root + " && " + cli + " start " + slug + " --as-is --no-attach "
                 "--model gpt-5.5 --effort high --goal-file /tmp/archive-goal")
            checkout = root + "/worktrees/" + slug + "/project"
            manifest = json.loads(user("${pkgs.ruby}/bin/ruby -ryaml -rjson -e " + shlex.quote(
                "puts JSON.generate(YAML.safe_load(File.read(ARGV.fetch(0))))") + " " + root + "/work/" + slug + "/portal.yml"))
            thread = manifest["codex"]["thread_id"]
            socket = manifest["codex"]["socket_path"]
            # Direct helpers share the portal's selected submission database.
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(
                package + "/bin/workspace-portal thread require-idle --thread-id " + thread + " --socket " + socket
                + " --cwd " + root + "/work/" + slug + " --user-state-root " + state))
            # Observe retry policy; this does not reserve later worktree admission.
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(
                "curl -fsS --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
                + "http://localhost/api/sessions/" + slug + "/retry-policy"))
            add_repair_worktree(slug)
            cache = root + "/worktrees/" + slug + "/.hook-cache"
            user("mkdir -p " + cache + "/bin && printf generated > " + cache + "/bin/overcommit")
            repair = "mv " + shlex.quote(cache) + " " + shlex.quote(checkout + "/.gems-archive-cache")
            user("printf %s " + shlex.quote(repair) + " > /tmp/creation-model/archive-repair-command && touch /tmp/creation-model/hold-turn")
            user("cd " + root + " && git add work/" + slug + " && git commit -m 'fixture: archive preparation' && git push origin master")
            accepted = browser(slug, "archive")
            machine.wait_until_succeeds(user_prefix + "test -f /tmp/creation-model/archive-repair-entered")
            user("test -d " + checkout + " && test ! -f " + root + "/worktrees/.locks/" + slug + ".archive.json")
            held = operation(slug)
            assert held["state"] == "running" and held["phase"] == "lead_fixing_blockers", held
            # Neither the lead's model turn nor its preparation owns global locks.
            fixture("2026-10-25-unrelated-during-repair")
            user("systemctl --user restart workspace-portal@fixture")
            machine.wait_until_succeeds(user_prefix + "test -S /run/user/1000/dev-workspaces/fixture/portal.sock")
            user("rm /tmp/creation-model/hold-turn")
            completed = archived(slug, checkout)
            assert completed["receiptId"] == accepted["receiptId"] and completed["attempt"] == accepted["attempt"], completed
            user("test ! -e " + cache)
            user("rm /tmp/creation-model/archive-repair-command")

        with subtest("browser archive removes readonly caches and revival restores committed content"):
            slug = "2026-10-17-roundtrip"
            checkout = fixture(slug)
            for cache in [checkout + "/module-cache", root + "/work/" + slug + "/module-cache"]:
                user("mkdir -p " + cache + "/nested && printf disposable > " + cache + "/nested/raw.json "
                     "&& printf retained > " + cache + "/nested/README.md "
                     "&& chmod 0400 " + cache + "/nested/* && chmod 0500 " + cache + " " + cache + "/nested")
            accepted = browser(slug, "archive")
            completed = archived(slug, checkout)
            assert accepted["receiptId"] == completed["receiptId"]
            assert user("systemctl --user show " + unit(completed) + " -p ExecMainStatus --value").strip() == "0"
            retained = root + "/archive/" + slug + "/module-cache/nested"
            assert user("cat " + retained + "/README.md") == "retained"
            user("test ! -e " + retained + "/raw.json")
            assert user("stat -c %a " + retained).strip() == "500"
            revive(slug, checkout)

        with subtest("accepted archive waits for shared session access across portal restart"):
            slug = "2026-10-19-contended-archive"
            checkout = fixture(slug)
            lock = "/run/user/1000/dev-workspaces/fixture/authority/" + slug + ".lock"
            holder = "archive-lock-holder.service"
            user("systemd-run --user --quiet --unit=" + holder + " ${pkgs.util-linux}/bin/flock -s "
                 + lock + " ${pkgs.bash}/bin/bash -c 'touch /tmp/archive-lock-ready; exec sleep infinity'")
            machine.wait_until_succeeds(user_prefix + "test -f /tmp/archive-lock-ready")
            accepted = browser(slug, "archive")
            command = "curl -fsS --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
            command += "http://localhost/api/sessions/" + slug + "/operation"
            command += " | ${pkgs.python3}/bin/python3 -c " + shlex.quote(
                "import sys,json; d=json.load(sys.stdin); assert d.get('state') == 'running' and "
                "d.get('progress', {}).get('stage') == 'Waiting for session access', d")
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(command), timeout=60)
            user("test -d " + checkout + " && test ! -f " + root + "/worktrees/.locks/" + slug + ".archive.json")
            worker = unit(accepted)
            before = user("systemctl --user show " + worker + " -p InvocationID --value").strip()
            user("systemctl --user restart workspace-portal@fixture")
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(command), timeout=60)
            current = operation(slug)
            assert current["receiptId"] == accepted["receiptId"] and current["attempt"] == accepted["attempt"], current
            assert before == user("systemctl --user show " + worker + " -p InvocationID --value").strip()
            user("systemctl --user stop " + holder)
            archived(slug, checkout)
            revive(slug, checkout)

        with subtest("portal restart and worker interruption preserve admitted identity"):
            slug = "2026-10-17-restart"
            checkout = fixture(slug)
            # Test-only systemd pre-exec barrier; the archive executable is unchanged.
            dropin = "/home/developer/.config/systemd/user/workspace-archive-.service.d"
            user("mkdir -p " + dropin)
            user("printf '[Service]\\nExecStartPre=${pkgs.coreutils}/bin/sleep infinity\\n' > " + dropin + "/barrier.conf")
            user("systemctl --user daemon-reload")
            accepted = browser(slug, "archive")
            worker = unit(accepted)
            machine.wait_until_succeeds(user_prefix + "systemctl --user show " + worker + " -p SubState --value | grep start-pre")
            user("systemctl --user restart workspace-portal@fixture")
            machine.wait_until_succeeds(user_prefix + "test -S /run/user/1000/dev-workspaces/fixture/portal.sock")
            # Restored receipts start paused until the background reconciler
            # proves the still-running supervised worker. Socket readiness
            # alone does not prove that reconciliation has finished.
            command = "curl -fsS --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
            command += "http://localhost/api/sessions/" + slug + "/operation"
            command += " | ${pkgs.python3}/bin/python3 -c " + shlex.quote(
                "import sys,json; d=json.load(sys.stdin); "
                "assert d.get('receiptId') == " + repr(accepted["receiptId"]) +
                " and d.get('state') == 'running', d; print(json.dumps(d))")
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(command), timeout=60)
            user("systemctl --user kill --signal=SIGKILL --kill-whom=all " + worker)
            wait_operation(slug, "failed")
            user("rm " + dropin + "/barrier.conf && systemctl --user daemon-reload")
            retried = browser(slug, "retry")
            assert retried["receiptId"] == accepted["receiptId"]
            assert retried["options"]["journalId"] == accepted["options"]["journalId"]
            assert retried["attempt"] == accepted["attempt"] + 1
            archived(slug, checkout)
            revive(slug, checkout)

        with subtest("unwritable archive parent fails before stopping agents or removing checkouts"):
            slug = "2026-10-25-preflight-permissions"
            checkout = fixture(slug)
            user("chmod 0500 " + root + "/archive")
            browser(slug, "archive")
            wait_operation(slug, "failed")
            user("test -d " + checkout + " && test ! -f " + root + "/worktrees/.locks/" + slug + ".archive.json")
            user("chmod 0755 " + root + "/archive")
            browser(slug, "retry")
            archived(slug, checkout)
            revive(slug, checkout)

        with subtest("retry after checkout removal keeps committed recovery metadata"):
            slug = "2026-10-17-recovery"
            checkout = fixture(slug)
            # Trigger a real filesystem failure after successful preflight,
            # when Git starts removal. The shipped executor is unchanged.
            race = "import ctypes,os,pathlib; lib=ctypes.CDLL(None); fd=lib.inotify_init1(0); "
            race += "assert fd>=0; assert lib.inotify_add_watch(fd," + repr(checkout.encode()) + ",0x200)>=0; "
            race += "pathlib.Path('/tmp/archive-removal-watch-ready').touch(); os.read(fd,4096); "
            race += "os.chmod(" + repr(root + "/archive") + ",0o500)"
            user("printf %s " + shlex.quote(race) + " > /tmp/archive-removal-race.py")
            user("systemd-run --user --quiet --unit=archive-removal-race ${pkgs.python3}/bin/python /tmp/archive-removal-race.py")
            machine.wait_until_succeeds(user_prefix + "test -f /tmp/archive-removal-watch-ready")
            accepted = browser(slug, "archive")
            wait_operation(slug, "failed")
            user("test ! -e " + checkout)
            journal = root + "/worktrees/.locks/" + slug + ".archive.json"
            retained = json.loads(user("cat " + journal))
            assert retained["operation_id"] == accepted["options"]["journalId"]
            recovery = state + "/archived-worktrees/"
            before = user("find " + recovery + " -type f -exec sha256sum {} \\; | sort")
            user("chmod 0755 " + root + "/archive")
            browser(slug, "retry")
            archived(slug, checkout)
            assert user("find " + recovery + " -type f -exec sha256sum {} \\; | sort") == before
            revive(slug, checkout)

        with subtest("registered candidates and descendants stop before prune and interrupted cleanup retries"):
            slug = "2026-11-02-service-archive"
            checkout = fixture(slug)
            user("printf runtime > " + root + "/work/" + slug + "/runtime.tmp")
            units = [launch_preview(slug, "main"), launch_preview(slug, "candidate", stubborn=True),
                     launch_preview(slug, "fast", fast=True)]
            foreign = "archive-unregistered-fixture.service"
            user("systemd-run --user --quiet --unit=" + foreign + " ${pkgs.coreutils}/bin/sleep infinity")
            before = service_status(slug)
            assert set(before) == {"schema", "workspace", "slug", "registryId", "registered", "count", "countKnown", "observedAt", "services", "history", "cleanup", "notice"}, "private CLI projection fields"
            assert before["registered"] is True and before["countKnown"] is True, "CLI membership is not known"
            assert type(before["count"]) is int and before["count"] == 3 and len(before["services"]) == 3, "CLI membership count changed"
            expected_rows = {unit[0].removeprefix("workspace-session-").removesuffix(".service"): label
                for unit, label in zip(units, ["main", "candidate", "fast"])}
            assert len(expected_rows) == 3 and len({row["id"] for row in before["services"]}) == 3, "CLI IDs are not distinct"
            assert {row["id"]: row["label"] for row in before["services"]} == expected_rows, "CLI IDs or labels changed"
            public_fields = {"id", "label", "phase", "state", "observedAt", "retiredAt", "error"}
            public_rows = before["services"] + before["history"]
            assert all(set(row) == public_fields for row in public_rows), "private CLI row fields"
            assert all(row["error"] is None or set(row["error"]) == {"code", "message"} for row in public_rows), "private CLI diagnostic fields"
            assert not any(private in json.dumps(public_rows) for private in ["workspace-session-", "argv", "control_group", "environment"]), "private CLI service data"
            details = json.loads(user("curl -fsS -D /tmp/service-details-headers --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
                + "http://localhost/api/sessions/" + slug + "/details"))
            assert_service_details(details, expected_rows)
            assert "workspace-session-" not in details["servicesHTML"] and "argv" not in details["servicesHTML"]
            invocation = user("systemctl --user show " + units[0][0] + " -p InvocationID --value").strip()
            set_service_invocation(slug, "f" * 32)
            accepted = browser(slug, "archive")
            failed = wait_operation(slug, "failed")
            user("test -f " + root + "/work/" + slug + "/runtime.tmp && test -d " + checkout)
            prelude = json.loads(user("cat " + root + "/worktrees/.locks/" + slug + ".services-cleanup.json"))
            assert prelude["operation_id"] == failed["options"]["journalId"], (prelude, failed)
            user("systemctl --user is-active " + foreign)
            set_service_invocation(slug, invocation)
            retry = browser(slug, "retry")
            assert retry["receiptId"] == accepted["receiptId"] and retry["options"]["journalId"] == prelude["operation_id"]
            archived(slug, checkout)
            assert user("cat " + units[0][2]) == "stopped"
            assert_services_retired(slug, units)
            user("systemctl --user is-active " + foreign)
            revive(slug, checkout)
            assert_services_retired(slug, units)
            user("systemctl --user stop " + foreign)

        with subtest("CLI delete preserves files on uncertain service stop then transfers retired evidence"):
            slug = "2026-11-02-service-delete"
            checkout = fixture(slug)
            user("printf runtime > " + root + "/work/" + slug + "/runtime.tmp")
            owned = launch_preview(slug, "delete-preview")
            invocation = user("systemctl --user show " + owned[0] + " -p InvocationID --value").strip()
            set_service_invocation(slug, "f" * 32)
            # Preserve the CLI's real-terminal confirmation and child exit status.
            delete = "cd " + root + " && " + cli + " delete " + slug + " --as-is --force"
            command = "printf 'yes\n' | ${pkgs.util-linux}/bin/script --quiet --return --command " + shlex.quote(delete) + " /dev/null"
            failed = machine.fail(user_prefix + "sh -c " + shlex.quote(command))
            assert "error: shutdown of registered service " in failed and "could not be verified; retry the original command" in failed, failed
            user("test -d " + checkout + " && test -f " + root + "/work/" + slug + "/runtime.tmp")
            journal = json.loads(user("cat " + root + "/worktrees/.locks/" + slug + ".removal.json"))
            prelude = json.loads(user("cat " + root + "/worktrees/.locks/" + slug + ".services-cleanup.json"))
            assert journal["phase"] == "thread_retired" and journal["force"], journal
            assert prelude["operation"] == "delete" and prelude["phase"] == "services_pending" and prelude["force"], prelude
            assert prelude["operation_id"] == journal["operation_id"], (prelude, journal)
            set_service_invocation(slug, invocation)
            user(command)
            user("test ! -e " + checkout + " && test ! -e " + root + "/work/" + slug)
            assert user("cat " + owned[2]) == "stopped"
            assert user("systemctl --user show " + owned[0] + " -p LoadState --value 2>/dev/null || true").strip() == "not-found"
            retained = json.loads(user("cat " + journal["recovery"] + "/services-registry.json"))
            assert retained["resources"][0]["phase"] == "retired", retained
            user("test ! -e " + service_registry(slug))

        with subtest("automatic archive applies the same native registered-service cleanup"):
            slug = "2026-11-02-service-automatic"
            user("cd " + root + " && " + cli + " start " + slug + " --as-is --no-codex --no-attach")
            user("printf runtime > " + root + "/work/" + slug + "/runtime.tmp")
            owned = launch_preview(slug, "automatic-preview")
            stop = "cd " + root + " && " + cli + " stop " + slug + " --as-is"
            user("printf '%s\n' " + shlex.quote(slug) + " | ${pkgs.util-linux}/bin/script --quiet --return --command " + shlex.quote(stop) + " /dev/null")
            user("sed -i 's/lifecycle: active/lifecycle: complete/' " + root + "/work/" + slug + "/state.md")
            user("cd " + root + " && git add work/" + slug + " && git commit -m 'fixture: complete automatic service session' && git push origin master")
            user("cd " + root + " && " + cli + " auto-archive enable --json && " + cli + " auto-archive scan --json")
            automatic_root = state + "/auto-archive/" + hashlib.sha256(root.encode()).hexdigest()
            observed = automatic_root + "/session-" + slug + ".json"
            row = json.loads(user("cat " + observed))
            assert row["activity_known"] and row["idle"] and row["tier"] == "complete", row
            # Age only this disposable observation; host time and live state are unchanged.
            age = "import json,pathlib,datetime; p=pathlib.Path(" + repr(observed) + "); d=json.loads(p.read_text()); "
            age += "d['idle_since']=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(days=2)).strftime('%Y-%m-%dT%H:%M:%SZ'); p.write_text(json.dumps(d))"
            user("${pkgs.python3}/bin/python3 -c " + shlex.quote(age))
            user("cd " + root + " && " + cli + " auto-archive scan --json")
            user("test -d " + root + "/archive/" + slug)
            assert user("cat " + owned[2]) == "stopped"
            assert_services_retired(slug, [owned])
            user("cd " + root + " && " + cli + " auto-archive disable --json")

        with subtest("tracking Git failure does not fail local archival"):
            slug = "2026-10-17-offline"
            checkout = fixture(slug)
            user("mv ~/origins/workspace.git ~/origins/workspace.offline")
            browser(slug, "archive")
            archived(slug, checkout)
            user("cd " + root + " && " + cli + " tracking-sync --slug " + slug + " --force --json")
            result = json.loads(user("cd " + root + " && " + cli + " tracking-sync --status --slug " + slug + " --json"))
            assert result[0]["state"] == "pending" and result[0]["error"], result
            user("mv ~/origins/workspace.offline ~/origins/workspace.git")
            user("cd " + root + " && " + cli + " tracking-sync --slug " + slug + " --force --json")
            result = json.loads(user("cd " + root + " && " + cli + " tracking-sync --status --slug " + slug + " --json"))
            assert result[0]["state"] == "synced", result
            revive(slug, checkout)
    except Exception:
        diagnostics()
        raise
  '';
}
