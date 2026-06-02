{
  description = "Workspace-local vpsAdmin development clusters";

  inputs = {
    vpsadmin.url = "github:vpsfreecz/vpsadmin/master";
    vpsadminos.url = "github:vpsfreecz/vpsadminos/staging";
    vpsfStatus = {
      url = "github:vpsfreecz/vpsf-status/master";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.vpsadmin.follows = "vpsadmin";
      inputs.vpsadminos.follows = "vpsadminos";
    };
    nixpkgs.follows = "vpsadminos/nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      vpsadmin,
      vpsadminos,
      vpsfStatus,
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

      workspace = env "VPSADMIN_DEVCLUSTER_WORKSPACE" (env "PWD" ".");
      slug = env "VPSADMIN_DEVCLUSTER_SLUG" "dev";
      topology = env "VPSADMIN_DEVCLUSTER_TOPOLOGY" "single";
      networkMode = env "VPSADMIN_DEVCLUSTER_NETWORK" "bridge";
      bridgeHelper = env "VPSADMIN_DEVCLUSTER_BRIDGE_HELPER" "/run/wrappers/bin/qemu-bridge-helper";
      certDir = env "VPSADMIN_DEVCLUSTER_CERT_DIR" "${workspace}/.dev-clusters/vpsadmin/certs/default";
      clusterConfigFile = env "VPSADMIN_DEVCLUSTER_CONFIG_FILE" "";
      sshPubKey = env "VPSADMIN_DEVCLUSTER_SSH_PUBKEY" "${workspace}/.dev-clusters/vpsadmin/ssh/id_ed25519.pub";
      vpsadminSourcePath = env "VPSADMIN_DEVCLUSTER_VPSADMIN_SOURCE" vpsadmin.outPath;
      vpsadminosSourcePath = env "VPSADMIN_DEVCLUSTER_VPSADMINOS_SOURCE" vpsadminos.outPath;
      haveapiSourcePath = env "VPSADMIN_DEVCLUSTER_HAVEAPI_SOURCE" "";
      configSourcePath = env "VPSADMIN_DEVCLUSTER_CONFIG_SOURCE" "";
      mailTemplatesSourcePath = env "VPSADMIN_DEVCLUSTER_MAIL_TEMPLATES_SOURCE" "";
      vpsfStatusSourcePath = env "VPSADMIN_DEVCLUSTER_VPSF_STATUS_SOURCE" "";
      vpsadminGoClientSourcePath = env "VPSADMIN_DEVCLUSTER_VPSADMIN_GO_CLIENT_SOURCE" "";
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
          vpsadmin
          vpsadminos
          vpsfStatus
          workspace
          slug
          topology
          networkMode
          bridgeHelper
          certDir
          clusterConfigFile
          sshPubKey
          vpsadminSourcePath
          vpsadminosSourcePath
          haveapiSourcePath
          configSourcePath
          mailTemplatesSourcePath
          vpsfStatusSourcePath
          vpsadminGoClientSourcePath
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
        name = "vpsadmin-devcluster-runner-deps";
        gemfile = vpsadminos.outPath + "/os/packages/test-runner/Gemfile";
        lockfile = vpsadminos.outPath + "/os/packages/test-runner/Gemfile.lock";
        gemset = vpsadminos.outPath + "/os/packages/test-runner/gemset.nix";
        groups = [ "default" ];
        inherit ruby;
      };

      runner = pkgs.writeShellScriptBin "vpsadmin-devcluster-runner" ''
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
          program = "${runner}/bin/vpsadmin-devcluster-runner";
        };
        default = self.apps.${system}.runner;
      };
    };
}
