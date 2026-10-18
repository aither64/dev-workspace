{
  pkgs,
  self,
  devWorkspace,
}:
let
  metadata = pkgs.writeText "recovery-fixture-metadata.py" ''
    """Failure-only, read-only projection of this VM's fixed disposable fixture."""
    import json
    import os
    import stat

    BASE = "/home/developer/.local/state/dev-workspaces/providers/fixture/"
    IDS = {"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
           "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}
    ROOT = "/home/developer/workspace"
    SOCKET = "/run/user/1000/dev-workspaces/fixture/app-server.sock"
    PHASES = {"waiting", "applying", "applied", "canceled", "failed", "recovery-required"}
    STAGES = {"capturing", "quiescing", "daemon-stop", "shared-start"}
    PROVIDERS = {"openai", "pool", "pooltwo"}


    def private_json(name, maximum):
        # Fixed basename only. Never follow a snapshot reference supplied by state.
        fd = os.open(BASE + name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != 1000 or info.st_mode & 0o077:
                raise ValueError("private file")
            if info.st_size > maximum:
                raise ValueError("file bound")
            data = stream.read(maximum + 1)
            if len(data) > maximum:
                raise ValueError("read bound")
            return json.loads(data)


    def enum(value, allowed):
        return value if isinstance(value, str) and value in allowed else "invalid-or-absent"


    def main():
        result = {"kind": "fixture-provider-metadata"}
        try:
            state = private_json("state.json", 1024 * 1024)
            assert isinstance(state, dict) and state.get("schema") in (1, 2)
            assert state.get("workspace") == "fixture" and state.get("root") == ROOT
            assert state.get("socket") == SOCKET
            op = state["operation"]
            assert isinstance(op, dict) and op.get("id") in IDS
            blockers = op.get("blockers")
            result["operation"] = {
                "phase": enum(op.get("phase"), PHASES),
                "stage": enum(op.get("stage"), STAGES),
                "active": enum(state.get("active"), PROVIDERS),
                "target": enum(op.get("target"), PROVIDERS),
                "previousActive": enum(op.get("previousActive"), PROVIDERS),
                "blockerCount": len(blockers) if isinstance(blockers, list) else None,
            }
            if op.get("strategy") == "scoped-v1":
                result["operation"]["strategy"] = "scoped-v1"
                print(json.dumps(result, sort_keys=True, separators=(",", ":")))
                return
            snapshot_name = op["id"] + ".snapshot.json"
            assert op.get("snapshot") == BASE + snapshot_name
            try:
                snapshot = private_json(snapshot_name, 16 * 1024 * 1024)
                assert isinstance(snapshot, dict) and snapshot.get("schema") == 1
                assert snapshot.get("workspace") == ROOT
                threads, loaded = snapshot["threads"], snapshot["loaded"]
                assert isinstance(threads, list) and len(threads) <= 4096
                assert isinstance(loaded, list) and len(loaded) <= 4096
                result["snapshot"] = {"status": "validated", "threadCount": len(threads), "loadedCount": len(loaded)}
            except Exception:
                result["snapshot"] = {"status": "unavailable-or-invalid"}
            try:
                terminals = private_json(snapshot_name + ".terminals", 1024 * 1024)
                assert isinstance(terminals, dict) and terminals.get("schema") == 1
                assert terminals.get("workspace") == "fixture" and terminals.get("root") == ROOT
                assert terminals.get("socket") == SOCKET
                assert isinstance(terminals["terminals"], list) and len(terminals["terminals"]) <= 4096
                result["terminals"] = {"status": "validated", "terminalCount": len(terminals["terminals"])}
            except Exception:
                result["terminals"] = {"status": "unavailable-or-invalid"}
        except Exception:
            result["state"] = "unavailable-or-invalid"
        print(json.dumps(result, sort_keys=True, separators=(",", ":")))


    if __name__ == "__main__":
        main()
  '';
in
pkgs.testers.runNixOSTest {
  name = "dev-workspace-session-recovery-boot";
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
      codexProviders = [
        {
          id = "pool";
          label = "Disposable provider";
          baseURL = "http://127.0.0.1:2455/v1";
          tokenFile = "/home/developer/provider.token";
        }
        {
          id = "pooltwo";
          label = "Second disposable provider";
          baseURL = "http://127.0.0.1:2455/v1";
          tokenFile = "/home/developer/provider.token";
        }
      ];
    };
    systemd.services.provider-fixture = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 ${pkgs.writeText "provider-fixture.py" ''
        import http.server
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                payload = {
                    "/v1/models": b'{"data":[{"id":"gpt-6.1-sol"}]}',
                    "/v1/usage": b'{"limits":[],"upstream_limits":[]}',
                }.get(self.path)
                if payload is None or self.headers.get("Authorization") != "Bearer synthetic-only":
                    self.send_error(403)
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(payload)
        http.server.HTTPServer(("127.0.0.1", 2455), Handler).serve_forever()
      ''}";
    };
    environment.systemPackages = [ pkgs.git ];
    # Register the user-profile closure in the VM's own Nix database.
    virtualisation.additionalPaths = [
      devWorkspace
      metadata
    ];
    system.stateVersion = "26.05";
  };
  testScript = ''
    import json
    import shlex
    import time

    machine.start(allow_reboot=True)
    machine.wait_for_unit("user@1000.service")
    package = "${devWorkspace}"
    user_prefix = "runuser -u developer -- env XDG_RUNTIME_DIR=/run/user/1000 "

    def user(command):
        return machine.succeed(user_prefix + "sh -c " + shlex.quote(command))

    def assert_services():
        machine.wait_for_unit("user@1000.service")
        for unit in ["workspace-tmux@fixture", "workspace-codex@fixture",
                     "workspace-portal@fixture", "workspace-router"]:
            machine.wait_until_succeeds(user_prefix + "systemctl --user is-active " + unit)
            assert user("systemctl --user is-enabled " + unit).strip() == "enabled"
        after = user("systemctl --user show workspace-portal@fixture -p After --value")
        assert "workspace-tmux@fixture.service" in after
        assert "workspace-codex@fixture.service" in after
        for socket in ["app-server.sock", "tmux.sock"]:
            machine.wait_until_succeeds(user_prefix + "test -S "
                                       "/run/user/1000/dev-workspaces/fixture/" + socket)

    with subtest("install private user profile and enable real application units"):
        user("mkdir -p ~/.local/state/dev-workspaces ~/workspace/repos "
             "~/workspace/work ~/workspace/worktrees")
        user("umask 077; printf synthetic-only > ~/provider.token")
        user("nix-env --profile ~/.local/state/dev-workspaces/profile --set " + package)
        user("git -C ~/workspace init --initial-branch=master")
        user(package + "/bin/workspace-host register fixture /home/developer/workspace "
             "--hostname fixture.workspace.example.test")
        assert_services()

    state_path = "/home/developer/.local/state/dev-workspaces/providers/fixture/state.json"
    host = package + "/bin/workspace-host"
    worker = "workspace-codex-provider@fixture.service"
    codex_unit = "workspace-codex@fixture.service"

    diagnostic_start = machine.succeed("date +%s").strip()
    assert diagnostic_start.isdigit()

    def collect_provider_diagnostics():
        commands = [
            "journalctl --user --no-pager --output=short-monotonic --since=@" + diagnostic_start
            + " --unit=" + worker + " --unit=" + codex_unit
            + " --unit=workspace-codex-reconcile.service --unit=workspace-coordination-gc.service | tail -c 65536",
            "systemctl --user show " + worker + " " + codex_unit
            + " workspace-codex-reconcile.service workspace-coordination-gc.service"
            + " --property=ActiveState,SubState,MainPID,Result,ExecMainStatus,NRestarts",
            "${pkgs.python3}/bin/python3 ${metadata}",
        ]
        for command in commands:
            try:
                status, output = machine.execute(user_prefix + "sh -c " + shlex.quote(command), timeout=10)
                print(output)
                print("PROVIDER_DIAGNOSTIC_COLLECTION_STATUS=" + str(status))
            except Exception:
                print("PROVIDER_DIAGNOSTIC_COLLECTION_UNAVAILABLE")

    def wait_provider_result(predicate):
        # A terminal result returns to Python. A nonzero shell exit alone would
        # keep wait_until_succeeds polling an already-failed operation for 900s.
        command = "${pkgs.python3}/bin/python3 -c " + shlex.quote(
            "import json; d=json.load(open(" + repr(state_path) + ")); o=d['operation']; "
            "expected=(" + predicate + "); terminal=o['phase'] in ['applied','failed','canceled']; "
            "assert expected or terminal; "
            "print('PROVIDER_EXPECTED_RESULT' if expected else 'PROVIDER_UNEXPECTED_TERMINAL')")
        try:
            output = machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(command))
            assert "PROVIDER_UNEXPECTED_TERMINAL" not in output, "provider reached an unexpected terminal phase"
            return json.loads(user(host + " provider status fixture"))
        except Exception:
            collect_provider_diagnostics()
            raise

    def wait_phase(phase):
        return wait_provider_result("o['phase'] == " + repr(phase))

    def legacy_provider_request(target, identity):
        # Only retained schema-1 requests use the daemon-restart engine. New
        # public requests intentionally select scoped-v1, even in an empty workspace.
        user("systemctl --user stop " + worker)
        catalog = json.loads(user(package + "/bin/workspace-portal codex-provider catalog "
                                  "--catalog /etc/dev-workspaces/codex-providers.json"))
        seed = (
            "import datetime,json,os; p=" + repr(state_path) + "; "
            "os.makedirs(os.path.dirname(p),mode=0o700,exist_ok=True); "
            "d=json.load(open(p)) if os.path.exists(p) else "
            + repr({"schema": 1, "workspace": "fixture", "root": "/home/developer/workspace",
                    "socket": "/run/user/1000/dev-workspaces/fixture/app-server.sock", "active": "openai"})
            + "; assert d['schema']==1; "
            "now=datetime.datetime.now(datetime.timezone.utc).isoformat(); "
            "d['operation']=" + repr({"id": identity, "target": target,
                                      "catalogDigest": catalog["digest"], "phase": "waiting", "blockers": []})
            + "; d['operation'].update(createdAt=now,updatedAt=now); "
            "os.umask(0o077); open(p,'w').write(json.dumps(d))"
        )
        user("${pkgs.python3}/bin/python3 -c " + shlex.quote(seed))
        user("systemctl --user enable --now " + worker)

    with subtest("retained legacy requests wait for foreign cgroup processes"):
        foreign_pid = machine.succeed("${pkgs.python3}/bin/python3 -c " + shlex.quote(
            "import subprocess; p=subprocess.Popen(['sleep','300'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True); print(p.pid)")).strip()
        group = user("systemctl --user show " + codex_unit + " -p ControlGroup --value").strip()
        machine.succeed("echo " + foreign_pid + " > " + shlex.quote("/sys/fs/cgroup" + group + "/cgroup.procs"))
        legacy_provider_request("pool", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
        wait_provider_result("o['phase']=='waiting' and o['blockers']")
        machine.succeed("kill " + foreign_pid)
        assert wait_phase("applied")["active"] == "pool"
        assert_services()

    with subtest("shared startup failure reserves state until verified compensation"):
        dropin = "/home/developer/.config/systemd/user/" + codex_unit + ".d/failure.conf"
        user("mkdir -p " + shlex.quote(dropin.rsplit("/", 1)[0]))
        user("printf '[Service]\\nExecStart=\\nExecStart=${pkgs.coreutils}/bin/false\\n' > " + shlex.quote(dropin))
        user("systemctl --user daemon-reload")
        legacy_provider_request("pooltwo", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
        wait_phase("recovery-required")
        user("rm " + shlex.quote(dropin))
        user("systemctl --user daemon-reload")
        user(host + " provider recover fixture")
        assert wait_phase("failed")["active"] == "pool"
        assert_services()

    with subtest("boot resumes a journal interrupted after daemon stop"):
        user("systemctl --user stop " + worker + " " + codex_unit)
        seed = "import json; p=" + repr(state_path) + "; d=json.load(open(p)); d['active']='pooltwo'; d['operation'].update(phase='applying',stage='daemon-stop',target='pooltwo',previousActive='pool'); open(p,'w').write(json.dumps(d))"
        user("${pkgs.python3}/bin/python3 -c " + shlex.quote(seed))
        machine.reboot()
        machine.wait_for_unit("user@1000.service")
        assert wait_phase("failed")["active"] == "pool"
        assert_services()

    with subtest("lingering starts the application after a disposable VM reboot"):
        before = machine.succeed("cat /proc/sys/kernel/random/boot_id").strip()
        machine.reboot()
        machine.wait_for_unit("multi-user.target")
        assert machine.succeed("cat /proc/sys/kernel/random/boot_id").strip() != before
        assert machine.succeed("loginctl show-user developer -p Linger --value").strip() == "yes"
        assert_services()

    with subtest("new scoped requests preserve the daemon and foreign processes"):
        try:
            # A ready socket can precede completion of boot package reconciliation.
            # Join that ordinary oneshot before taking the PID baseline or adding activity.
            machine.succeed(user_prefix + "systemctl --user start workspace-codex-reconcile.service", timeout=300)
            assert_services()
            daemon_pid = user("systemctl --user show " + codex_unit + " -p MainPID --value").strip()
            foreign_pid = machine.succeed("${pkgs.python3}/bin/python3 -c " + shlex.quote(
                "import subprocess; p=subprocess.Popen(['sleep','300'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True); print(p.pid)")).strip()
            group = user("systemctl --user show " + codex_unit + " -p ControlGroup --value").strip()
            machine.succeed("echo " + foreign_pid + " > " + shlex.quote("/sys/fs/cgroup" + group + "/cgroup.procs"))
            # Retry only a concurrent package-admission refusal, retaining the
            # exact request UUID. Every other failure ends this assertion.
            request = host + " provider request fixture pooltwo cccccccc-cccc-4ccc-8ccc-cccccccccccc"
            deadline = time.monotonic() + 60
            while True:
                status, output = machine.execute(user_prefix + "sh -c " + shlex.quote(request + " 2>&1"), timeout=30)
                if status == 0:
                    break
                assert output.strip() == "error: workspace package transition is in progress; retry shortly", output
                assert time.monotonic() < deadline, "package admission did not settle"
                time.sleep(1)
            assert wait_phase("applied")["active"] == "pooltwo"
            assert user("systemctl --user show " + codex_unit + " -p MainPID --value").strip() == daemon_pid
            machine.succeed("kill -0 " + foreign_pid)
            machine.succeed("kill " + foreign_pid)
            assert_services()
        except Exception:
            collect_provider_diagnostics()
            raise
  '';
}
