{
  description = "Reusable development workspaces backed by Codex App Server";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  };

  outputs =
    inputs@{
      self,
      nixpkgs,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      devWorkspace = pkgs.callPackage ./nix/workspace-portal.nix {
        src = self;
      };
    in
    {
      packages.${system} = {
        default = devWorkspace;
        dev-workspace = devWorkspace;
        workspace-host = devWorkspace;
        workspace-portal = devWorkspace;
      };
      apps.${system}.workspace-host = {
        type = "app";
        program = "${devWorkspace}/bin/workspace-host";
      };
      checks.${system} = {
        package = devWorkspace;
      };
    };
}
