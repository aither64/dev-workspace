{
  pkgs,
  nixpkgs,
  self,
}:
let
  exampleConfig = self.nixosConfigurations.example.config;
  exampleReconcile =
    nixpkgs.lib.findFirst (package: package.name == "workspace-portal-substrate-reconcile")
      (throw "example workspace reconciliation package is missing")
      exampleConfig.environment.systemPackages;
  hostPathConfigValid =
    overrides:
    let
      paths = nixpkgs.lib.recursiveUpdate {
        routerSocket = "/run/dev-workspaces/router.sock";
        lockFile = "/run/lock/dev-workspace-substrate.lock";
        passwordFile = "/var/lib/dev-workspaces/password/password";
        htpasswdFile = "/var/lib/dev-workspaces/auth/htpasswd";
        caStateDirectory = "/var/lib/dev-workspaces/pki";
        certificateDirectory = "/var/lib/dev-workspaces/tls";
        publicCaFile = "/var/lib/dev-workspaces/public/ca.pem";
      } overrides;
      evaluated = import ../host-paths.nix {
        lib = nixpkgs.lib;
        inherit paths;
      };
    in
    evaluated.valid;
in
pkgs.runCommand "dev-workspace-host-module-check" { } ''
  test ${nixpkgs.lib.escapeShellArg exampleConfig.services.dev-workspaces.hostName} = workspace.example.test
  test ${nixpkgs.lib.escapeShellArg (nixpkgs.lib.boolToString exampleConfig.services.nginx.enable)} = true
  test ${
    nixpkgs.lib.escapeShellArg (
      builtins.toJSON exampleConfig.services.nginx.virtualHosts."*.workspace.example.test".serverAliases
    )
  } = '["workspace.example.test","legacy-workspace.example.test"]'
  grep -Fq "DNS:workspace.example.test" ${exampleReconcile}/bin/workspace-portal-substrate-reconcile
  grep -Fq "DNS:*.workspace.example.test" ${exampleReconcile}/bin/workspace-portal-substrate-reconcile
  grep -Fq "DNS:legacy-workspace.example.test" ${exampleReconcile}/bin/workspace-portal-substrate-reconcile
  test ${nixpkgs.lib.escapeShellArg (nixpkgs.lib.boolToString (hostPathConfigValid { }))} = true
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        passwordFile = "/var/lib/dev-workspaces/shared/password";
        htpasswdFile = "/var/lib/dev-workspaces/shared/htpasswd";
      })
    )
  } = false
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        caStateDirectory = "/var/lib/dev-workspaces/tls/pki";
      })
    )
  } = false
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        passwordFile = "relative/password";
      })
    )
  } = false
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        passwordFile = "/tmp/dev-workspaces/password";
      })
    )
  } = false
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        passwordFile = "/var/lib/password";
      })
    )
  } = false
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        routerSocket = "/var/lib/dev-workspaces/router.sock";
      })
    )
  } = false
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        routerSocket = "/run/router.sock";
      })
    )
  } = false
  test ${
    nixpkgs.lib.escapeShellArg (
      nixpkgs.lib.boolToString (hostPathConfigValid {
        lockFile = "/tmp/dev-workspace.lock";
      })
    )
  } = false
  touch "$out"
''
