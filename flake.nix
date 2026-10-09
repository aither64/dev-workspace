{
  description = "Reusable development workspaces backed by Codex App Server";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    llm-agents.url = "github:numtide/llm-agents.nix";
    codex-web = {
      url = "github:aither64/codex-web/c9878454f07da244c103b8a79340b92b6d5bf052";
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
      mkCodexPackage = import ./nix/codex-package.nix;
      mkPackage =
        {
          activationEnvironmentAliases ? [ ],
          pkgs,
          extensions ? { },
          routerSocket ? hostPaths.routerSocket,
          teamConfig ? null,
          userNamespace ? "dev-workspaces",
        }:
        pkgs.callPackage ./nix/workspace-portal.nix {
          src = self;
          codex = mkCodexPackage {
            inherit pkgs;
            codex = llm-agents.packages.${pkgs.system}.codex;
          };
          codexModelCatalog = "${llm-agents.packages.${pkgs.system}.codex.src}/codex-rs/models-manager/models.json";
          codexWebSrc = codex-web;
          codexWebVersion = "v0.0.0-${codex-web.lastModifiedDate}-${builtins.substring 0 12 codex-web.rev}";
          inherit
            activationEnvironmentAliases
            extensions
            routerSocket
            teamConfig
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
          mkCodexPackage
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
        codex-package = import ./nix/tests/codex-package.nix {
          inherit mkCodexPackage pkgs;
          codex = llm-agents.packages.${system}.codex;
        };
        package = devWorkspace;
        host-module = import ./nix/tests/host-module.nix {
          inherit pkgs nixpkgs self;
        };
        host-auth-benchmark = pkgs.runCommand "dev-workspace-host-auth-benchmark" { } ''
          ${pkgs.python3}/bin/python3 ${./test/host_auth_benchmark.py} \
            --htpasswd ${pkgs.apacheHttpd}/bin/htpasswd \
            --apache-version ${nixpkgs.lib.escapeShellArg pkgs.apacheHttpd.version}
          touch "$out"
        '';
        host-module-idempotency = import ./nix/tests/host-module-idempotency.nix {
          inherit pkgs self;
        };
        session-recovery-boot = import ./nix/tests/session-recovery-boot.nix {
          inherit pkgs self devWorkspace;
        };
        host-package-contract = import ./nix/tests/host-package-contract.nix {
          inherit devWorkspace hostPaths pkgs;
        };
        extension-catalog = import ./nix/tests/extension-catalog.nix {
          inherit pkgs mkPackage;
        };
        agent-team-catalog = import ./nix/tests/agent-team-catalog.nix {
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
