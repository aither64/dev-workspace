{
  lib,
  vpsadminos,
  workspace,
  slug,
  topology,
  networkMode,
  bridgeHelper,
  clusterConfigFile,
  sshPubKey,
  vpsadminosSourcePath,
}:
{ pkgs, ... }:
let
  inherit (lib)
    concatMapStringsSep
    listToAttrs
    mapAttrs
    nameValuePair
    optionalAttrs
    optionalString
    recursiveUpdate
    ;

  defaultConfig = builtins.fromJSON (builtins.readFile ../default-config.json);
  devConfig =
    if clusterConfigFile == "" then
      defaultConfig
    else
      recursiveUpdate defaultConfig (builtins.fromJSON (builtins.readFile clusterConfigFile));

  networkModeChecked =
    if
      builtins.elem networkMode [
        "bridge"
        "local"
      ]
    then
      networkMode
    else
      throw "Unsupported vpsAdminOS devcluster network mode '${networkMode}'";

  topologyNodeNames =
    devConfig.topologies.${topology}
      or (throw "Unsupported vpsAdminOS devcluster topology '${topology}'");

  networkConfig = devConfig.network;
  localPrefixLength = networkConfig.localPrefixLength or 24;
  bridgePrefixLength = networkConfig.prefixLength or 24;
  selectedPrefixLength =
    if networkModeChecked == "bridge" then bridgePrefixLength else localPrefixLength;

  availableNodes = mapAttrs (
    machineName: attrs:
    attrs
    // {
      inherit machineName;
      internalIp = if networkModeChecked == "bridge" then attrs.bridgeIp else attrs.localIp;
      cpus = attrs.cpus or 4;
      memoryMiB = attrs.memoryMiB or 8192;
      diskSize = attrs.diskSize or "20G";
      sshPort = attrs.sshPort or null;
    }
  ) devConfig.nodes;

  selectedNodes = map (
    machineName:
    if builtins.hasAttr machineName availableNodes then
      availableNodes.${machineName}
    else
      throw "Topology '${topology}' references unknown node '${machineName}'"
  ) topologyNodeNames;

  peerHosts = listToAttrs (
    map (
      node:
      nameValuePair node.internalIp [
        node.machineName
        node.name
        "${node.name}.vpsadminos.test"
      ]
    ) selectedNodes
  );

  mkUserNetwork = hostForward: {
    type = "user";
    opts = {
      network = "10.0.2.0/24";
      host = "10.0.2.2";
      dns = "10.0.2.3";
    }
    // optionalAttrs (hostForward != "") {
      inherit hostForward;
    };
  };

  plainUserNetwork = mkUserNetwork "";
  localUserNetwork = node: mkUserNetwork "tcp:127.0.0.1:${toString node.sshPort}-:22";
  socketNetwork = {
    type = "socket";
    mcast = {
      port = networkConfig.socketPort or "vpsadminos-devcluster-${slug}";
    };
  };
  bridgeNetwork = {
    type = "bridge";
    opts = {
      link = networkConfig.bridge;
    }
    // optionalAttrs (bridgeHelper != "") {
      helper = bridgeHelper;
    };
  };

  machineNetworks =
    node:
    if networkModeChecked == "bridge" then
      [
        plainUserNetwork
        bridgeNetwork
      ]
    else
      [
        (localUserNetwork node)
        socketNetwork
      ];

  nameservers =
    if networkModeChecked == "bridge" then
      networkConfig.upstreamNameservers
    else
      networkConfig.localNameservers or [
        "1.1.1.1"
        "9.9.9.9"
      ];

  sshModule = {
    services.openssh = {
      enable = true;
      settings.PermitRootLogin = "yes";
    };
    users.users.root.openssh.authorizedKeys.keyFiles = [ sshPubKey ];
    networking.firewall.allowedTCPPorts = [ 22 ];
  };

  nodeModule =
    node:
    { pkgs, lib, ... }:
    {
      imports = [ sshModule ];

      networking = {
        hostName = lib.mkForce node.name;
        hosts = peerHosts;
        nameservers = lib.mkForce nameservers;
        custom = lib.mkAfter ''
          ip addr replace ${node.internalIp}/${toString selectedPrefixLength} dev eth1
          ip link set eth1 up
          ${optionalString (networkModeChecked == "bridge") ''
            ip route replace default via ${networkConfig.gateway} dev eth1
          ''}
        '';
      };

      environment.etc."devcluster-vpsadminos-source".text = vpsadminosSourcePath;

      environment.systemPackages = with pkgs; [
        curl
        git
        htop
        jq
        tree
      ];
    };

  mkNodeMachine = node: {
    spin = "vpsadminos";
    disks = [
      {
        type = "file";
        device = "${node.machineName}-tank.img";
        size = node.diskSize;
      }
    ];
    networks = machineNetworks node;
    config = {
      imports = [
        (vpsadminos.outPath + "/tests/configs/vpsadminos/pool-tank.nix")
        (nodeModule node)
      ];

      boot.qemu = {
        memory = node.memoryMiB;
        cpus = node.cpus;
        cpu = {
          cores = node.cpus;
          threads = 1;
          sockets = 1;
        };
      };
    };
  };
in
{
  name = "vpsadminos-devcluster-${slug}";

  description = ''
    Branch-selected vpsAdminOS-only development cluster for ${slug}.
  '';

  machines = listToAttrs (
    map (node: nameValuePair node.machineName (mkNodeMachine node)) selectedNodes
  );

  testScript = ''
    # This config is consumed by devcluster-runner, not by the test runner.
  '';
}
