{ ... }:
{
  boot.isContainer = true;
  fileSystems."/" = {
    device = "none";
    fsType = "tmpfs";
  };

  users.users.developer.isNormalUser = true;

  services.dev-workspaces = {
    enable = true;
    owner = "developer";
    hostName = "workspace.example.test";
    wildcardHost = "*.workspace.example.test";
    aliases = [ "legacy-workspace.example.test" ];
  };

  system.stateVersion = "26.05";
}
