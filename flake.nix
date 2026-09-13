{
  description = "Reusable development workspaces backed by Codex App Server";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    llm-agents.url = "github:numtide/llm-agents.nix/d706bc302d3a7854659e3b5b5ad35270f7dcf8da";
    codex-web = {
      url = "github:aither64/codex-web/4a4c77b4acc2bbaef44e2327c37d9c984e091867";
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
      hostPaths = (import ./nix/host-paths.nix { inherit (nixpkgs) lib; }).defaults;
      mkPackage =
        {
          activationEnvironmentAliases ? [ ],
          pkgs,
          extensions ? { },
          routerSocket ? hostPaths.routerSocket,
          userNamespace ? "dev-workspaces",
        }:
        pkgs.callPackage ./nix/workspace-portal.nix {
          src = self;
          codex = llm-agents.packages.${pkgs.system}.codex;
          codexWebSrc = codex-web;
          codexWebRev = codex-web.rev;
          inherit
            activationEnvironmentAliases
            extensions
            routerSocket
            userNamespace
            ;
        };
      runtimeContract = "${self}/portal/internal/session/runtime-contract.json";
      runtimeAuthorityCorpus = "${self}/test/fixtures/runtime-authority-corpus.json";
      devWorkspace = mkPackage { inherit pkgs; };
    in
    {
      lib = {
        inherit
          hostPaths
          mkPackage
          runtimeAuthorityCorpus
          runtimeContract
          ;
      };
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
        host-module = import ./nix/tests/host-module.nix {
          inherit pkgs nixpkgs self;
        };
        host-module-idempotency = import ./nix/tests/host-module-idempotency.nix {
          inherit pkgs self;
        };
        host-package-contract = import ./nix/tests/host-package-contract.nix {
          inherit devWorkspace hostPaths pkgs;
        };
        extension-catalog = import ./nix/tests/extension-catalog.nix {
          inherit pkgs mkPackage;
        };
        user-namespace = import ./nix/tests/user-namespace.nix {
          inherit pkgs mkPackage;
        };
        generic-source = pkgs.runCommand "dev-workspace-generic-source" { } ''
          first=vps
          second=aither
          forbidden="$first"'free|'"$second"'dev'
          if grep -RilE "$forbidden" ${self} --exclude-dir=.git > matches; then
            cat matches >&2
            exit 1
          fi
          if ${pkgs.findutils}/bin/find ${self} -printf '%P\n' | grep -iE "$forbidden" > matches; then
            cat matches >&2
            exit 1
          fi
          touch "$out"
        '';
      };
      nixosModules.host = import ./nix/host-module.nix;
      nixosConfigurations.example = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          self.nixosModules.host
          ./nix/example-host.nix
        ];
      };
    };
}
