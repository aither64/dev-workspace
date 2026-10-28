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
      codexProviders = [{
        id = "primary";
        label = "Archive fixture";
        baseURL = "http://127.0.0.1:2455/primary/v1";
        tokenFile = "/home/developer/.codex/fixture.token";
      }];
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

    machine.start()
    machine.wait_for_unit("user@1000.service")
    package = "${devWorkspace}"
    root = "/home/developer/workspace"
    state = "/home/developer/.local/state/dev-workspaces"
    user_prefix = "runuser -u developer -- env XDG_RUNTIME_DIR=/run/user/1000 "
    cli = package + "/bin/dev-session"

    def user(command):
        return machine.succeed(user_prefix + "sh -c " + shlex.quote(command))

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

    def diagnostics():
        print(user("journalctl --user --no-pager -n 100 -u 'workspace-archive-*' -u workspace-portal@fixture -u workspace-codex@fixture -u archive-model"))
        print(user("find " + root + "/worktrees/.locks -maxdepth 1 -type f -name '*.archive*.json' -exec cat {} \\;"))
        print(user("find ~/.codex/sessions -type f -name '*.jsonl' -exec tail -n 10 {} + | tail -c 16000"))

    def native_status_diagnostics():
        # TEMPORARY diagnostic: retain identity/status/completion only, never
        # prompts, command arguments or output content from the native rollout.
        code = "import glob,json; "
        code += "rows=[json.loads(line) for path in glob.glob('/home/developer/.codex/sessions/**/*.jsonl',recursive=True) for line in open(path)]; "
        code += "events=[r for r in rows if r.get('type')=='event_msg' and isinstance(r.get('payload'),dict)]; "
        code += "print(json.dumps([dict(timestamp=r.get('timestamp'),event=r['payload'].get('type'),threadId=r['payload'].get('thread_id'),turnId=r['payload'].get('turn_id'),rootTurnId=r['payload'].get('root_turn_id'),itemId=r['payload'].get('item',{}).get('id'),status=r['payload'].get('item',{}).get('status'),exitCode=r['payload'].get('item',{}).get('exit_code')) for r in events if r['payload'].get('type') in ('task_started','task_complete','turn_aborted','item_completed')][-20:]))"
        print(user("${pkgs.python3}/bin/python3 -c " + shlex.quote(code)))

    with subtest("install real package and user services with private Git origins"):
        user("mkdir -p " + state + " " + root + "/repos " + root + "/work " + root + "/worktrees")
        user("mkdir -p ~/.codex /tmp/creation-model")
        user("printf fixture-private-token > ~/.codex/fixture.token && chmod 0600 ~/.codex/fixture.token")
        config = 'model_provider="primary"\nmodel="gpt-5.5"\nmodel_reasoning_effort="high"\napproval_policy="never"\nsandbox_mode="danger-full-access"\n[model_providers.primary]\nname="Archive fixture"\nbase_url="http://127.0.0.1:2455/primary/v1"\nwire_api="responses"\nrequires_openai_auth=false\nsupports_websockets=false\n'
        user("printf %s " + shlex.quote(config) + " > ~/.codex/config.toml")
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
            # Initial retry-policy bootstrap takes EX; finish that checkpoint
            # before worktree registration, which must still refuse writers.
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(
                "curl -fsS --unix-socket /run/user/1000/dev-workspaces/fixture/portal.sock "
                + "http://localhost/api/sessions/" + slug + "/retry-policy"))
            user("${pkgs.util-linux}/bin/flock -s -w 30 /run/user/1000/dev-workspaces/fixture/authority/"
                 + slug + ".lock ${pkgs.coreutils}/bin/true")
            user("cd " + root + " && " + cli + " worktree add " + slug + " project --as-is")
            cache = root + "/worktrees/" + slug + "/.hook-cache"
            user("mkdir -p " + cache + "/bin && printf generated > " + cache + "/bin/overcommit")
            repair = "mv " + shlex.quote(cache) + " " + shlex.quote(checkout + "/.gems-archive-cache")
            user("printf %s " + shlex.quote(repair) + " > /tmp/creation-model/archive-repair-command && touch /tmp/creation-model/hold-turn")
            user("cd " + root + " && git add work/" + slug + " && git commit -m 'fixture: archive preparation' && git push origin master")
            accepted = browser(slug, "archive")
            machine.wait_until_succeeds(user_prefix + "test -f /tmp/creation-model/archive-repair-entered")
            user("test -d " + checkout + " && test ! -f " + root + "/worktrees/.locks/" + slug + ".archive.json")
            held = operation(slug)
            if held["state"] != "running" or held["phase"] != "lead_fixing_blockers":
                # TEMPORARY diagnostic: allow the same accepted turn to finish
                # before exposing the original held-stage failure. Do not retry
                # the archive or alter its classification.
                user("rm /tmp/creation-model/hold-turn")
                machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(
                    package + "/bin/workspace-portal thread require-idle --thread-id " + thread + " --socket " + socket
                    + " --cwd " + root + "/work/" + slug + " --user-state-root " + state), timeout=60)
                native_status_diagnostics()
                diagnostics()
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
