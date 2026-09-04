{
  description = "vpsFree.cz development workspace tools";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    llm-agents.url = "github:numtide/llm-agents.nix";
  };

  outputs =
    inputs@{
      self,
      nixpkgs,
      llm-agents,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      workspacePortal = pkgs.callPackage ./nix/workspace-portal.nix {
        codex = llm-agents.packages.${system}.codex;
        src = self;
      };
    in
    {
      lib.workspacePortalRuntimeContract =
        builtins.fromJSON (builtins.readFile ./portal/runtime-contract.json);
      packages.${system} = {
        default = workspacePortal;
        workspace-portal = workspacePortal;
      };
    };
}
