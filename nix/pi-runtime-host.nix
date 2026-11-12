{ config, lib, pkgs, ... }:
let
  workspace = config.services.dev-workspaces;
  cfg = workspace.piRuntime;
  owner = config.users.users.${workspace.owner};
  validStateRoot =
    lib.hasPrefix "/" cfg.stateRoot
    && cfg.stateRoot != "/"
    && lib.all (part: part != "" && part != "." && part != "..")
      (builtins.tail (lib.splitString "/" cfg.stateRoot))
    && builtins.match ".*[[:space:]%:].*" cfg.stateRoot == null;
  tools = pkgs.callPackage ./pi-runtime-host-package.nix {
    ownerUid = owner.uid;
    ownerHome = owner.home;
    ownerName = workspace.owner;
    stateRoot = cfg.stateRoot;
  };
in
{
  options.services.dev-workspaces.piRuntime = {
    enable = lib.mkEnableOption "the host-created Pi session execution boundary";
    stateRoot = lib.mkOption {
      type = lib.types.str;
      default = "${owner.home}/.local/state/dev-workspaces/pi-runtime-host";
      description = "Persistent owner-managed instance storage, outside credentials and private scratch.";
    };
  };

  config = lib.mkIf (workspace.enable && cfg.enable) {
    assertions = [
      {
        assertion = validStateRoot;
        message = "Pi runtime stateRoot must be a normalized absolute path without systemd specifiers, colon or whitespace.";
      }
      {
        assertion = owner.uid != null && owner.uid != 0;
        message = "Pi runtime requires a configured non-root owner with a stable UID.";
      }
    ];

    environment.systemPackages = [ tools ];
    # This grants only the immutable fixed-verb parser, never systemctl itself.
    security.sudo.extraRules = [{
      users = [ workspace.owner ];
      runAs = "root";
      commands = [{ command = "${tools}/bin/workspace-pi-host"; options = [ "NOPASSWD" ]; }];
    }];

    systemd.services."workspace-pi-session@" = {
      description = "Pi development-session boundary %i";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      # No wantedBy: only an admitted explicit instance request starts it.
      environment = {
        HOME = owner.home;
        USER = workspace.owner;
        LOGNAME = workspace.owner;
        TMPDIR = "/tmp";
      };
      serviceConfig = {
        User = workspace.owner;
        Group = owner.group;
        WorkingDirectory = owner.home;
        Type = "exec";
        ExecStart = "${tools}/bin/workspace-pi-session-entry %i";
        PrivateUsers = false;
        PrivateMounts = true;
        PrivateTmp = false;
        BindPaths = [
          "${cfg.stateRoot}/instances/%i/tmp:/tmp"
          "${cfg.stateRoot}/instances/%i/var-tmp:/var/tmp"
        ];
        Restart = "no";
        KillMode = "control-group";
        SendSIGKILL = true;
        TimeoutStopSec = "15s";
        Delegate = false;
        UMask = "0077";
      };
    };
  };
}
