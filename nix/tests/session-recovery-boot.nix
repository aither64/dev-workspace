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
    };
    environment.systemPackages = [ pkgs.git ];
    # Register the user-profile closure in the VM's own Nix database.
    virtualisation.additionalPaths = [ devWorkspace ];
    system.stateVersion = "26.05";
  };
  testScript = ''
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
        user("nix-env --profile ~/.local/state/dev-workspaces/profile --set " + package)
        user("git -C ~/workspace init --initial-branch=master")
        user(package + "/bin/workspace-host register fixture /home/developer/workspace "
             "--hostname fixture.workspace.example.test")
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
