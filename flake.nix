{
  description = "Reusable development workspaces backed by Codex App Server";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    llm-agents.url = "github:numtide/llm-agents.nix/c2a308c84bbfa9f30827344219b7284f8104bdd8";
    codex-web = {
      url = "github:aither64/codex-web/e8655b7b2689da9b1aabe10df69858c32725dd61";
      flake = false;
    };
  };

  outputs =
    inputs@{
      self,
      nixpkgs,
      llm-agents,
      codex-web,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      devWorkspace = pkgs.callPackage ./nix/workspace-portal.nix {
        src = self;
        codex = llm-agents.packages.${system}.codex;
        codexWebSrc = codex-web;
        codexWebRev = codex-web.rev;
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
