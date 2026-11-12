{
  description = "Reusable development workspaces backed by Codex App Server";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    llm-agents.url = "github:numtide/llm-agents.nix/91ff550fe42d4dd51dcf8b6bad87a0134fe7f314";
    pi.url = "github:earendil-works/pi/a22b888a58fe55b668cc45b80749ec88d4eb4ddc";
    codex-web = {
      url = "github:aither64/codex-web/a883fcb6610bd41c6bd4e7f8a00c19e9adb5d5e8";
      flake = false;
    };
  };

  outputs =
    inputs@{
      self,
      nixpkgs,
      llm-agents,
      pi,
      codex-web,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      hostPaths = (import ./nix/host-paths.nix { inherit (nixpkgs) lib; }).defaults;
      mkCodexPackage = import ./nix/codex-package.nix;
      piUpstream = builtins.fromJSON (builtins.readFile ./pi-runtime/upstream.json);
      mkPiWorker = pkgs:
        assert pi.rev == piUpstream.revision;
        pkgs.callPackage ./nix/pi-worker.nix {
          src = self;
          piPackage = pi.packages.${pkgs.stdenv.hostPlatform.system}.default;
          # Keep the SDK's tested Node, including its upstream nixpkgs lock.
          nodejs = (import pi.inputs.nixpkgs {
            system = pkgs.stdenv.hostPlatform.system;
          }).nodejs_22;
        };
      piWorker = mkPiWorker pkgs;
      piRuntimeManifest = pkgs.writeText "dev-workspace-pi-runtime.json" (builtins.toJSON {
        schema = 1;
        host_launcher_contract = 1;
        launch_request_schema = 1;
        handshake_schema = 1;
        supervisor_entrypoint = "bin/workspace-portal";
        supervisor_command = [ "pi-runtime" "supervise" ];
      });
      mkPackage =
        {
          activationEnvironmentAliases ? [ ],
          pkgs,
          extensions ? { },
          routerSocket ? hostPaths.routerSocket,
          teamConfig ? null,
          coordinationConfig ? null,
          machineIdentityConfig ? null,
          integrationConfig ? null,
          piProviderConfig ? null,
          userNamespace ? "dev-workspaces",
        }:
        pkgs.callPackage ./nix/workspace-portal.nix {
          src = self;
          codex = mkCodexPackage {
            inherit pkgs;
            codex = llm-agents.packages.${pkgs.system}.codex;
          };
          codexModelCatalog = "${codex-web}/codex/ephemeral_luna_models.json";
          codexWebSrc = codex-web;
          codexWebVersion = "v0.0.0-${codex-web.lastModifiedDate}-${builtins.substring 0 12 codex-web.rev}";
          piWorker = mkPiWorker pkgs;
          inherit piRuntimeManifest;
          inherit
            activationEnvironmentAliases
            extensions
            routerSocket
            teamConfig
            coordinationConfig
            machineIdentityConfig
            integrationConfig
            piProviderConfig
            userNamespace
            ;
        };
      runtimeContract = "${self}/portal/internal/session/runtime-contract.json";
      runtimeAuthorityCorpus = "${self}/test/fixtures/runtime-authority-corpus.json";
      devWorkspace = mkPackage { inherit pkgs; };
    in
    {
      lib = {
        coordinationCommandLibrary = "${self}/libexec/workspace-coordination-command.rb";
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
        pi-worker = piWorker;
      };
      devShells.${system}.default = pkgs.mkShell {
        inputsFrom = [ devWorkspace ];
        packages = [
          pkgs.go
          pkgs.ruby
          pkgs.git
          pkgs.nix
          pkgs.nodejs
          pkgs.jq
        ];
        GOFLAGS = "-mod=readonly";
      };
      apps.${system}.workspace-host = {
        type = "app";
        program = "${devWorkspace}/bin/workspace-host";
      };
      checks.${system} = devWorkspace.rubyChecks // {
        codex-package = import ./nix/tests/codex-package.nix {
          inherit mkCodexPackage pkgs;
          codex = llm-agents.packages.${system}.codex;
        };
        package = devWorkspace;
        pi-worker = import ./nix/tests/pi-worker.nix {
          inherit pkgs piWorker piRuntimeManifest;
          src = self;
        };
        pi-runtime-host-unit = pkgs.runCommand "dev-workspace-pi-runtime-host-unit" {
          nativeBuildInputs = [ pkgs.ruby ];
        } ''
          ruby ${self}/test/pi_runtime_host_test.rb
          touch "$out"
        '';
        pi-runtime-host = import ./nix/tests/pi-runtime-host.nix {
          inherit pkgs self;
        };
        ruby-source-filter = import ./nix/tests/ruby-source-filter.nix { inherit pkgs; };
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
        session-archive-lifecycle = import ./nix/tests/session-archive-lifecycle.nix {
          inherit pkgs self devWorkspace;
        };
        session-creation = import ./nix/tests/session-creation.nix {
          inherit pkgs self mkPackage;
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
      nixosModules.piRuntimeHost = import ./nix/pi-runtime-host.nix;
      nixosConfigurations.example = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          self.nixosModules.host
          ./nix/example-host.nix
        ];
      };
    };
}
