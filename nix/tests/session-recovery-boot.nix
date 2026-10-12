{
  pkgs,
  self,
  devWorkspace,
}:
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
    virtualisation.additionalPaths = [ devWorkspace ];
    system.stateVersion = "26.05";
  };
  testScript = ''
    import json
    import shlex

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

    def wait_phase(phase):
        command = "${pkgs.python3}/bin/python3 -c " + shlex.quote(
            "import json; d=json.load(open(" + repr(state_path) + ")); o=d['operation']; "
            "assert o['phase'] == " + repr(phase) + ", json.dumps({k:o.get(k) for k in ['phase','stage','blockers','message']})")
        machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(command))
        return json.loads(user(host + " provider status fixture"))

    with subtest("foreign cgroup processes block the real worker until removed"):
        foreign_pid = machine.succeed("${pkgs.python3}/bin/python3 -c " + shlex.quote(
            "import subprocess; p=subprocess.Popen(['sleep','300'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True); print(p.pid)")).strip()
        group = user("systemctl --user show " + codex_unit + " -p ControlGroup --value").strip()
        machine.succeed("echo " + foreign_pid + " > " + shlex.quote("/sys/fs/cgroup" + group + "/cgroup.procs"))
        user(host + " provider request fixture pool aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
        blocked = "${pkgs.python3}/bin/python3 -c " + shlex.quote(
            "import json; d=json.load(open(" + repr(state_path) + ")); assert d['operation']['phase']=='waiting' and d['operation']['blockers']")
        machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(blocked))
        machine.succeed("kill " + foreign_pid)
        assert wait_phase("applied")["active"] == "pool"
        assert_services()

    with subtest("shared startup failure reserves state until verified compensation"):
        dropin = "/home/developer/.config/systemd/user/" + codex_unit + ".d/failure.conf"
        user("mkdir -p " + shlex.quote(dropin.rsplit("/", 1)[0]))
        user("printf '[Service]\\nExecStart=\\nExecStart=${pkgs.coreutils}/bin/false\\n' > " + shlex.quote(dropin))
        user("systemctl --user daemon-reload")
        user(host + " provider request fixture pooltwo bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
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
  '';
}
