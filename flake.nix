{
  description = "vpsFree.cz development workspace tools";

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
      workspacePortal = pkgs.callPackage ./nix/workspace-portal.nix {
        src = self;
      };
    in
    {
      packages.${system} = {
        default = workspacePortal;
        workspace-host = workspacePortal;
        workspace-portal = workspacePortal;
      };
      apps.${system}.workspace-host = {
        type = "app";
        program = "${workspacePortal}/bin/workspace-host";
      };
    };
}
