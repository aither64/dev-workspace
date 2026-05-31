{
  description = "Workspace-local vpsAdminOS development clusters";

  inputs = {
    vpsadminos.url = "github:vpsfreecz/vpsadminos/staging";
    nixpkgs.follows = "vpsadminos/nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      vpsadminos,
    }:
    let
      system = "x86_64-linux";
      lib = nixpkgs.lib;

      env =
        name: default:
        let
          value = builtins.getEnv name;
        in
        if value == "" then default else value;

      workspace = env "VPSADMINOS_DEVCLUSTER_WORKSPACE" (env "PWD" ".");
      slug = env "VPSADMINOS_DEVCLUSTER_SLUG" "dev";
      topology = env "VPSADMINOS_DEVCLUSTER_TOPOLOGY" "single";
      networkMode = env "VPSADMINOS_DEVCLUSTER_NETWORK" "local";
      bridgeHelper = env "VPSADMINOS_DEVCLUSTER_BRIDGE_HELPER" "/run/wrappers/bin/qemu-bridge-helper";
      clusterConfigFile = env "VPSADMINOS_DEVCLUSTER_CONFIG_FILE" "";
      sshPubKey = env "VPSADMINOS_DEVCLUSTER_SSH_PUBKEY" "${workspace}/.dev-clusters/vpsadminos/ssh/id_ed25519.pub";
      vpsadminosSourcePath = env "VPSADMINOS_DEVCLUSTER_VPSADMINOS_SOURCE" vpsadminos.outPath;
      sharedRunnerLib = builtins.path {
        path = "${workspace}/dev-clusters/lib";
        name = "devcluster-runner-lib";
      };

      pkgs = import nixpkgs {
        inherit system;
        overlays = import (vpsadminos.outPath + "/os/overlays");
      };

      clusterTest = import ./nix/test.nix {
        inherit
          lib
          vpsadminos
          workspace
          slug
          topology
          networkMode
          bridgeHelper
          clusterConfigFile
          sshPubKey
          vpsadminosSourcePath
          ;
      };

      clusterConfig = import (vpsadminos.outPath + "/tests/make-test.nix") clusterTest {
        inherit system;
        pkgs = nixpkgs.outPath;
        extraArgs = {
          inherit vpsadminos;
        };
      };

      ruby = pkgs.ruby_vpsadminos;
      runnerDeps = pkgs.bundlerEnv {
        name = "vpsadminos-devcluster-runner-deps";
        gemfile = vpsadminos.outPath + "/os/packages/test-runner/Gemfile";
        lockfile = vpsadminos.outPath + "/os/packages/test-runner/Gemfile.lock";
        gemset = vpsadminos.outPath + "/os/packages/test-runner/gemset.nix";
        groups = [ "default" ];
        inherit ruby;
      };

      runner = pkgs.writeShellScriptBin "vpsadminos-devcluster-runner" ''
        export GEM_HOME=${runnerDeps}/${ruby.gemPath}
        export GEM_PATH=${runnerDeps}/${ruby.gemPath}
        export RUBYLIB=${./lib}:${sharedRunnerLib}:${vpsadminos.outPath}/test-runner/lib:${vpsadminos.outPath}/osvm/lib:${vpsadminos.outPath}/libosctl/lib

        exec ${ruby}/bin/ruby ${./lib/devcluster-runner.rb} "$@"
      '';
    in
    {
      packages.${system} = {
        cluster-config = clusterConfig.json;
        inherit runner;
        default = clusterConfig.json;
      };

      apps.${system} = {
        runner = {
          type = "app";
          program = "${runner}/bin/vpsadminos-devcluster-runner";
        };
        default = self.apps.${system}.runner;
      };
    };
}
