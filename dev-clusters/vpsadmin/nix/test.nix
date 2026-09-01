{
  lib,
  vpsadmin,
  vpsadminos,
  vpsfStatus,
  workspace,
  slug,
  topology,
  networkMode,
  bridgeHelper,
  certDir,
  clusterConfigFile,
  sshPubKey,
  vpsadminSourcePath,
  vpsadminosSourcePath,
  vpsadminRevision,
  vpsadminRevisionDirty,
  vpsadminosRevision,
  vpsadminosRevisionDirty,
  haveapiSourcePath,
  configSourcePath,
  mailTemplatesSourcePath,
  webSourcePath,
  vpsfStatusSourcePath,
  vpsadminGoClientSourcePath,
}:
{ pkgs, ... }:
let
  inherit (lib)
    concatMapStringsSep
    escapeShellArg
    filter
    filterAttrs
    genAttrs
    listToAttrs
    mapAttrs
    nameValuePair
    optionalAttrs
    recursiveUpdate
    ;

  seed = import (vpsadmin.outPath + "/api/db/seeds/test.nix");
  locationDomain = seed.location.domain;

  defaultConfig = builtins.fromJSON (builtins.readFile ../default-config.json);
  devConfig =
    if clusterConfigFile == "" then
      defaultConfig
    else
      recursiveUpdate defaultConfig (builtins.fromJSON (builtins.readFile clusterConfigFile));

  domains = devConfig.domains;
  tmpDomains = devConfig.tmpDomains;
  serviceIp = devConfig.services.ip;
  serviceRootDiskMiB = devConfig.services.rootDiskMiB or (12 * 1024);
  zfsTransferStartDelay = devConfig.nodectld.zfsTransferStartDelay or 0;
  devGateway = devConfig.network.gateway;
  resolverConfig = devConfig.resolver or { };
  resolverMode = resolverConfig.mode or "cluster";
  resolverModeChecked =
    if
      builtins.elem resolverMode [
        "cluster"
        "gateway"
        "none"
        "upstream"
      ]
    then
      resolverMode
    else
      throw "Unsupported devcluster resolver mode '${resolverMode}'";
  resolverEnabled = resolverModeChecked != "none";
  clusterResolverEnabled = resolverModeChecked == "cluster";
  resolverUpstreamNameservers = resolverConfig.upstreamNameservers or [ devGateway ];
  upstreamResolverNameservers =
    if resolverModeChecked == "gateway" then [ devGateway ] else resolverUpstreamNameservers;
  serviceMachineNameservers =
    if clusterResolverEnabled then
      [ "127.0.0.1" ]
    else if resolverEnabled then
      upstreamResolverNameservers
    else
      [ ];
  peerMachineNameservers =
    if clusterResolverEnabled then
      [ serviceIp ]
    else if resolverEnabled then
      upstreamResolverNameservers
    else
      [ ];
  bindForwarders = peerMachineNameservers;
  pluginConfig = devConfig.plugins or { enabled = "all"; };
  pluginEnabledConfig = pluginConfig.enabled or pluginConfig;
  availablePlugins = builtins.attrNames (
    filterAttrs (_name: type: type == "directory") (builtins.readDir "${vpsadmin.outPath}/plugins")
  );
  enabledPlugins =
    if builtins.isString pluginEnabledConfig then
      if pluginEnabledConfig == "all" then
        availablePlugins
      else if pluginEnabledConfig == "none" then
        [ ]
      else
        throw "Unsupported devcluster plugin set '${pluginEnabledConfig}'"
    else if builtins.isList pluginEnabledConfig then
      pluginEnabledConfig
    else
      throw "Unsupported devcluster plugins.enabled value";
  mailCapture = devConfig.mail.capture;
  adminerConfig = devConfig.adminer or { };
  adminerAuth =
    adminerConfig.webAuth or {
      enable = false;
    };
  adminerBasicAuth =
    if adminerAuth.enable or false then
      {
        "${adminerAuth.username}" = adminerAuth.password;
      }
    else
      { };
  adminerPort = adminerConfig.port or 18081;
  webConfig = devConfig.web or { };
  webEnabled = (webConfig.enable or true) && webSourcePath != "";
  webEnvironmentId = webConfig.environmentId or 1;
  webRoot = "/run/vpsfree-web-live";
  webPackage =
    if webEnabled then
      import
        (builtins.path {
          path = webSourcePath;
          name = "vpsfree-web-source";
        })
        {
          inherit pkgs;
          noDev = true;
        }
    else
      null;
  dnsEnabled = devConfig.dns.enable or false;
  dnsServersConfig = if dnsEnabled then devConfig.dns.servers or { } else { };
  mailpitAuth =
    mailCapture.webAuth or {
      enable = false;
    };
  mailpitBasicAuth =
    if mailpitAuth.enable or false then
      {
        "${mailpitAuth.username}" = mailpitAuth.password;
      }
    else
      { };
  networkModeChecked =
    if
      builtins.elem networkMode [
        "bridge"
        "local"
      ]
    then
      networkMode
    else
      throw "Unsupported devcluster network mode '${networkMode}'";

  mkNode =
    {
      id,
      name,
      ip,
      role,
      maxVps ? if role == "node" then 30 else 0,
      cpus ? 4,
      memoryMiB ? 8192,
      swapMiB ? 0,
      sshPort ? null,
    }:
    let
      roleId =
        {
          node = 0;
          storage = 1;
          mailer = 2;
          dns_server = 3;
        }
        .${role};
      hypervisorType =
        if
          builtins.elem role [
            "node"
            "storage"
          ]
        then
          1
        else
          null;
    in
    rec {
      inherit
        id
        name
        ip
        role
        maxVps
        cpus
        memoryMiB
        swapMiB
        sshPort
        ;
      domainName = "${name}.${locationDomain}";
      seedRecord = {
        inherit id name;
        location_id = seed.location.id;
        ip_addr = ip;
        active = true;
        max_vps = maxVps;
        cpus = cpus;
        total_memory = memoryMiB;
        total_swap = swapMiB;
        role = roleId;
        hypervisor_type = hypervisorType;
      };
      portReservations = builtins.genList (i: {
        node_id = id;
        port = 10000 + i;
      }) 100;
    };

  availableNodes = mapAttrs (
    machineName: attrs:
    (mkNode attrs)
    // {
      inherit machineName;
    }
  ) devConfig.nodes;

  topologyNodes =
    devConfig.topologies.${topology} or (throw "Unsupported devcluster topology '${topology}'");

  selectedNodes = listToAttrs (map (name: nameValuePair name availableNodes.${name}) topologyNodes);
  nodeList = builtins.attrValues selectedNodes;

  mkDnsServer =
    machineName: attrs:
    (mkNode {
      inherit (attrs)
        id
        name
        ip
        cpus
        memoryMiB
        ;
      role = "dns_server";
      maxVps = 0;
      swapMiB = attrs.swapMiB or 0;
      sshPort = attrs.sshPort or null;
    })
    // {
      inherit machineName;
      serverName = attrs.serverName;
      hidden = attrs.hidden or false;
      enableUserDnsZones = attrs.enableUserDnsZones or true;
      userDnsZoneType = attrs.userDnsZoneType or "secondary_type";
    };

  dnsServers = mapAttrs mkDnsServer dnsServersConfig;
  dnsServerList = builtins.attrValues dnsServers;
  allNodeList = nodeList ++ dnsServerList;
  nodeHostNames =
    node:
    [
      node.name
      node.domainName
    ]
    ++ lib.optional (node ? serverName) node.serverName;

  nodeRecords = map (node: node.seedRecord) allNodeList;
  nodeInventoryRecords = map (node: {
    inherit (node)
      id
      name
      ip
      role
      maxVps
      cpus
      memoryMiB
      swapMiB
      ;
  }) nodeList;
  portReservationRecords = builtins.concatLists (map (node: node.portReservations) nodeList);
  dnsServerRecords = map (node: {
    node_id = node.id;
    name = node.serverName;
    ipv4_addr = node.ip;
    ipv6_addr = null;
    hidden = node.hidden;
    enable_user_dns_zones = node.enableUserDnsZones;
    user_dns_zone_type = node.userDnsZoneType;
  }) dnsServerList;
  rabbitmqNodeUsers = map (node: node.domainName) allNodeList;
  installMailTemplates =
    mailTemplatesSourcePath != "" && ((devConfig.mail.templates.install or false) == true);
  mailTemplateApiPath = vpsadmin.outPath + "/api/lib/vpsadmin/api/mail_templates.rb";
  mailTemplateSourcesCompatible =
    if !installMailTemplates then
      true
    else if
      !(builtins.pathExists mailTemplateApiPath)
      || !(lib.hasInfix "def self.reconcile!(path:, source_id:)" (builtins.readFile mailTemplateApiPath))
    then
      throw "Installing external mail templates requires vpsAdmin commit ea956e5e or a compatible newer revision with the declarative notification-template reconciler. Set mail.templates.install to false in .dev-clusters/vpsadmin/clusters/${slug}/config.json when using an older vpsAdmin and template pair."
    else if !(builtins.pathExists "${mailTemplatesSourcePath}/templates") then
      throw "Installing external mail templates requires the declarative templates/ package layout. Update the matching template worktree or set mail.templates.install to false in .dev-clusters/vpsadmin/clusters/${slug}/config.json when using an older vpsAdmin and template pair."
    else
      true;
  mailTemplatesStorePath =
    if installMailTemplates && mailTemplateSourcesCompatible then
      builtins.path {
        path = mailTemplatesSourcePath;
        name = "vpsfree-mail-templates";
      }
    else
      null;
  mailTemplatePaths = if installMailTemplates then [ "${mailTemplatesStorePath}" ] else [ ];
  vpsfStatusLocalSource =
    if vpsfStatusSourcePath != "" then
      builtins.path {
        path = vpsfStatusSourcePath;
        name = "vpsf-status-source";
      }
    else
      null;
  vpsadminGoClientSource =
    if vpsadminGoClientSourcePath != "" then
      builtins.path {
        path = vpsadminGoClientSourcePath;
        name = "vpsadmin-go-client-source";
      }
    else
      null;
  vpsfStatusPackage =
    if vpsfStatusLocalSource != null then
      pkgs.callPackage (vpsfStatusLocalSource + "/nix/package.nix") {
        src = vpsfStatusLocalSource;
        version = "dev";
        vpsadminGoClientSource = vpsadminGoClientSource;
      }
    else
      vpsfStatus.packages.${pkgs.stdenv.hostPlatform.system}.vpsf-status;
  vpsfStatusModule =
    if vpsfStatusLocalSource != null then
      import (vpsfStatusLocalSource + "/nix/module.nix")
    else
      vpsfStatus.nixosModules.vpsf-status;
  vpsfStatusLocation = {
    id = seed.location.id;
    label = seed.location.label;
    nodes = map (node: {
      id = node.id;
      name = node.domainName;
      ip_address = node.ip;
    }) nodeList;
    dns_resolvers = map (node: {
      name = node.serverName;
      ip_address = node.ip;
    }) dnsServerList;
  };

  devSeed = pkgs.writeText "vpsadmin-devcluster-seed.rb" ''
    require 'ipaddress'
    require 'json'

    def upsert_sys_config(category, name, value, min_user_level: 0, data_type: 'String')
      record = SysConfig.find_or_initialize_by(category: category, name: name)
      record.assign_attributes(
        value: value,
        min_user_level: min_user_level,
        data_type: data_type
      )
      record.save!
    end

    upsert_sys_config('core', 'api_url', 'https://${domains.api}')
    upsert_sys_config('core', 'auth_url', 'https://${domains.auth}')
    upsert_sys_config('core', 'logo_url', 'https://${domains.webui}/logo.png')
    upsert_sys_config('webui', 'base_url', 'https://${domains.webui}')
    upsert_sys_config('core', 'support_mail', 'support@devcluster.test')
    upsert_sys_config('core', 'webauthn_rp_name', 'vpsAdmin dev')

    environment = Environment.find(${toString seed.environment.id})
    location = Location.find(${toString seed.location.id})

    webui_client = Oauth2Client.find_by(client_id: 'vpsadmin-webui-test')
    if webui_client
      attrs = {
        redirect_uri: 'https://${domains.webui}/?page=login&action=callback'
      }
      if webui_client.respond_to?(:authorization_start_uri=)
        attrs[:authorization_start_uri] =
          'https://${domains.webui}/?page=login&action=login'
      end
      if webui_client.respond_to?(:save_with_default!)
        attrs[:is_default] = true
      end
      webui_client.assign_attributes(attrs)

      if webui_client.respond_to?(:save_with_default!)
        webui_client.save_with_default!
      else
        webui_client.save!
      end
    end

    location.update!(remote_console_server: 'https://${domains.console}')

    nodes = JSON.parse(${builtins.toJSON (builtins.toJSON nodeRecords)})
    nodes.each do |attrs|
      id = attrs.delete('id')
      node = Node.find_or_initialize_by(id: id)
      node.assign_attributes(attrs)
      node.save!
    end

    dns_servers = JSON.parse(${builtins.toJSON (builtins.toJSON dnsServerRecords)})
    dns_servers.each do |attrs|
      server = DnsServer.find_or_initialize_by(name: attrs.fetch('name'))
      server.assign_attributes(
        node: Node.find(attrs.fetch('node_id')),
        ipv4_addr: attrs['ipv4_addr'],
        ipv6_addr: attrs['ipv6_addr'],
        hidden: attrs.fetch('hidden'),
        enable_user_dns_zones: attrs.fetch('enable_user_dns_zones'),
        user_dns_zone_type: attrs.fetch('user_dns_zone_type')
      )
      server.save!
    end

    def devcluster_reverse_zone_name(attrs)
      unless attrs.fetch('ipVersion') == 4
        raise "automatic reverse zone seeding supports only IPv4 networks"
      end

      prefix = attrs.fetch('prefix')
      unless [8, 16, 24, 32].include?(prefix)
        raise "automatic reverse zone seeding supports IPv4 prefixes /8, /16, /24 and /32, got /#{prefix}"
      end

      labels = attrs.fetch('address').split('.').first(prefix / 8).reverse
      "#{labels.join('.')}.in-addr.arpa."
    end

    def with_devcluster_admin_session(admin)
      previous_user = User.current
      previous_session = UserSession.current
      User.current = admin
      UserSession.current ||= UserSession.create!(
        user: admin,
        auth_type: 'basic',
        api_ip_addr: '127.0.0.1',
        client_version: 'devcluster-seed'
      )
      yield
    ensure
      User.current = previous_user
      UserSession.current = previous_session
    end

    def ensure_dns_zone_runtime(zone, dns_servers)
      dns_servers
        .sort_by { |server| server.primary_type? ? 0 : 1 }
        .each do |server|
          server_zone = DnsServerZone.find_by(
            dns_zone: zone,
            dns_server: server
          )

          if server_zone
            if server_zone.zone_type != server.user_dns_zone_type
              server_zone.update!(zone_type: server.user_dns_zone_type)
            end

            next
          end

          server_zone = DnsServerZone.new(
            dns_zone: zone,
            dns_server: server,
            zone_type: server.user_dns_zone_type
          )
          TransactionChains::DnsServerZone::Create.fire(server_zone)
        end
    end

    def refresh_ip_reverse_zones
      reverse_zones = DnsZone
        .where(zone_role: :reverse_role)
        .order(reverse_network_prefix: :desc)
        .to_a

      IpAddress.find_each do |ip|
        zone = reverse_zones.find { |candidate| candidate.include?(ip) }
        next if zone.nil? || ip.reverse_dns_zone_id == zone.id

        ip.update!(reverse_dns_zone: zone)
      end
    end

    def upsert_reverse_dns_zones(networks, dns_servers, admin)
      return if dns_servers.empty?

      with_devcluster_admin_session(admin) do
        networks.each do |attrs|
          next unless attrs.fetch('reverseZone', true)

          zone = DnsZone.find_or_initialize_by(name: devcluster_reverse_zone_name(attrs))
          zone.assign_attributes(
            zone_source: :internal_source,
            zone_role: :reverse_role,
            default_ttl: attrs.fetch('reverseTtl', 3600),
            email: attrs.fetch('reverseEmail', 'dns@devcluster.test'),
            enabled: true,
            confirmed: :confirmed,
            label: attrs.fetch('reverseLabel', "#{attrs.fetch('label')} reverse"),
            reverse_network_address: attrs.fetch('address'),
            reverse_network_prefix: attrs.fetch('prefix')
          )
          zone.save!

          ensure_dns_zone_runtime(zone, dns_servers)
        end
      end
    end

    node_inventory = JSON.parse(${builtins.toJSON (builtins.toJSON nodeInventoryRecords)})
    node_inventory.each do |attrs|
      next unless attrs.fetch('role') == 'node'

      node = Node.find(attrs.fetch('id'))
      status = NodeCurrentStatus.find_or_initialize_by(node: node)
      if status.new_record?
        status.assign_attributes(
          created_at: Time.now.utc,
          updated_at: Time.now.utc,
          kernel: 'devcluster',
          vpsadmin_version: 'dev',
          update_count: 1,
          cpus: attrs.fetch('cpus'),
          total_memory: attrs.fetch('memoryMiB'),
          total_swap: attrs.fetch('swapMiB'),
          used_memory: 0,
          used_swap: 0,
          process_count: 0,
          uptime: 0,
          cgroup_version: :cgroup_v2,
          pool_state: :online,
          pool_scan: :none,
          pool_checked_at: Time.now.utc
        )
        status.save!
      end
    end

    pool_config = JSON.parse(${builtins.toJSON (builtins.toJSON devConfig.seed.pools)})
    node_inventory.each do |attrs|
      next unless attrs.fetch('role') == 'node'

      node = Node.find(attrs.fetch('id'))
      pool = Pool.find_by(node: node, filesystem: pool_config.fetch('filesystem')) ||
             Pool.find_by(node: node, label: pool_config.fetch('label')) ||
             Pool.new(node: node)
      pool.assign_attributes(
        label: pool_config.fetch('label'),
        filesystem: pool_config.fetch('filesystem'),
        role: pool_config.fetch('role', 'hypervisor'),
        max_datasets: pool_config.fetch('maxDatasets'),
        total_space: pool_config.fetch('totalSpaceMiB'),
        available_space: pool_config.fetch('availableSpaceMiB'),
        used_space: pool_config.fetch('usedSpaceMiB'),
        checked_at: Time.now.utc,
        state: :online,
        scan: :none,
        is_open: 1,
        maintenance_lock: 0,
        refquota_check: true
      )
      pool.save!

      VpsAdmin::API::DatasetProperties::Registrator.properties.each do |name, property|
        pool_property = DatasetProperty.find_or_initialize_by(
          pool: pool,
          dataset_in_pool_id: nil,
          dataset_id: nil,
          name: name.to_s
        )
        pool_property.assign_attributes(
          value: property.meta[:default],
          inherited: false,
          confirmed: DatasetProperty.confirmed(:confirmed)
        )
        pool_property.save!
      end
    end

    port_reservations = JSON.parse(${builtins.toJSON (builtins.toJSON portReservationRecords)})
    port_reservations.each do |attrs|
      PortReservation.find_or_create_by!(
        node_id: attrs.fetch('node_id'),
        port: attrs.fetch('port')
      )
    end

    def upsert_network(location, attrs)
      network = Network.find_or_initialize_by(
        address: attrs.fetch('address'),
        prefix: attrs.fetch('prefix')
      )
      network.assign_attributes(
        label: attrs.fetch('label'),
        ip_version: attrs.fetch('ipVersion'),
        role: attrs.fetch('role'),
        managed: attrs.fetch('managed'),
        split_access: attrs.fetch('splitAccess'),
        split_prefix: attrs.fetch('splitPrefix'),
        purpose: attrs.fetch('purpose'),
        primary_location: location
      )
      network.save!

      loc_net = LocationNetwork.find_or_initialize_by(
        location: location,
        network: network
      )
      loc_net.assign_attributes(
        primary: true,
        priority: attrs.fetch('priority', 10),
        autopick: attrs.fetch('autopick', true),
        userpick: attrs.fetch('userpick', true)
      )
      loc_net.save!

      attrs.fetch('addresses').each_with_index do |addr, idx|
        ip = IpAddress.find_by(ip_addr: addr)
        if ip.nil?
          ip = IpAddress.register(
            IPAddress.parse("#{addr}/#{network.split_prefix}"),
            network: network,
            user: nil,
            location: location,
            prefix: network.split_prefix,
            size: 1
          )
        else
          ip.assign_attributes(
            network: network,
            user: nil,
            prefix: network.split_prefix,
            size: 1,
            order: idx
          )
          ip.save!
        end

        host_ip = ip.host_ip_addresses.find_or_initialize_by(ip_addr: ip.ip_addr)
        host_ip.order = nil
        host_ip.save!
      end
    end

    admin = User.find_by!(login: 'test-admin')
    seed_networks = JSON.parse(${builtins.toJSON (builtins.toJSON devConfig.seed.networks)})
    seed_dns_servers = dns_servers.map { |attrs| DnsServer.find_by!(name: attrs.fetch('name')) }

    upsert_reverse_dns_zones(seed_networks, seed_dns_servers, admin)

    seed_networks.each do |attrs|
      upsert_network(location, attrs)
    end

    refresh_ip_reverse_zones

    def upsert_default_vps_resources(environment, resources)
      resources.each do |name, value|
        cluster_resource = ClusterResource.find_by!(name: name)
        record = DefaultObjectClusterResource.find_or_initialize_by(
          environment: environment,
          class_name: 'Vps',
          cluster_resource: cluster_resource
        )
        record.value = value
        record.save!
      end
    end

    def upsert_user_namespace(user, attrs)
      namespace_attrs = attrs.fetch('namespace')
      block_start = namespace_attrs.fetch('blockStart')
      block_count = namespace_attrs.fetch('blockCount')
      blocks = UserNamespaceBlock
        .where(index: block_start...(block_start + block_count))
        .order(:index)
        .to_a

      if blocks.size != block_count
        raise "unable to allocate #{block_count} user namespace blocks from #{block_start}"
      end

      namespace = UserNamespace.find_or_initialize_by(user: user)
      namespace.assign_attributes(
        offset: blocks.first.offset,
        block_count: block_count,
        size: blocks.sum(&:size)
      )
      namespace.save!

      UserNamespaceBlock
        .where(user_namespace: namespace)
        .where.not(id: blocks.map(&:id))
        .update_all(user_namespace_id: nil)

      blocks.each do |block|
        block.update!(user_namespace: namespace) unless block.user_namespace_id == namespace.id
      end

      map = UserNamespaceMap.find_or_initialize_by(
        user_namespace: namespace,
        label: 'Default map'
      )
      map.save!

      [0, 1].each do |kind|
        entry = UserNamespaceMapEntry.find_or_initialize_by(
          user_namespace_map: map,
          kind: kind,
          vps_id: 0
        )
        entry.assign_attributes(ns_id: 0, count: namespace.size)
        entry.save!
      end
    end

    def upsert_user_resources(admin, environment, user, values)
      package = ClusterResourcePackage.find_or_initialize_by(
        environment: environment,
        user: user
      )
      package.label = 'Dev personal package'
      package.save!

      link = UserClusterResourcePackage.find_or_initialize_by(
        environment: environment,
        user: user,
        cluster_resource_package: package
      )
      link.added_by = admin
      link.comment = ""
      link.save!

      ClusterResource.all.each do |resource|
        value = values.fetch(resource.name, 0)

        item = ClusterResourcePackageItem.find_or_initialize_by(
          cluster_resource_package: package,
          cluster_resource: resource
        )
        item.value = value
        item.save!

        user_resource = UserClusterResource.find_or_initialize_by(
          user: user,
          environment: environment,
          cluster_resource: resource
        )
        user_resource.value = value
        user_resource.save!
      end
    end

    def upsert_dev_user(admin, environment, attrs, resources)
      language = Language.find_by(code: attrs.fetch('language', 'en')) ||
                 Language.find_by(code: 'en') ||
                 Language.create!(code: 'en', label: 'English')
      user = User.find_or_initialize_by(login: attrs.fetch('login'))
      user.assign_attributes(
        full_name: attrs.fetch('fullName'),
        email: attrs.fetch('email'),
        level: attrs.fetch('level', 1),
        language: language,
        enable_basic_auth: true,
        enable_token_auth: true,
        enable_oauth2_auth: true,
        enable_multi_factor_auth: false,
        password_reset: false,
        lockout: false,
        mailer_enabled: true,
        object_state: :active
      )
      plaintext_password = attrs.fetch('password')
      password_matches = user.persisted? &&
                         VpsAdmin::API::CryptoProviders
                           .provider(user.password_version)
                           .matches?(user.password, user.login, plaintext_password)
      user.set_password(plaintext_password) unless password_matches
      user.save!

      if ActiveRecord::Base.connection.data_source_exists?('user_accounts')
        account = UserAccount.find_or_initialize_by(user_id: user.id)
        account.monthly_payment = 0
        account.paid_until = nil
        account.save!
      end

      config = EnvironmentUserConfig.find_or_initialize_by(
        environment: environment,
        user: user
      )
      config.assign_attributes(
        can_create_vps: attrs.fetch('canCreateVps', true),
        can_destroy_vps: attrs.fetch('canDestroyVps', true),
        vps_lifetime: attrs.fetch('vpsLifetime', environment.vps_lifetime),
        max_vps_count: attrs.fetch('maxVpsCount', 5),
        default: true
      )
      config.save!

      upsert_user_namespace(user, attrs)
      upsert_user_resources(admin, environment, user, resources)
    end

    upsert_default_vps_resources(
      environment,
      JSON.parse(${builtins.toJSON (builtins.toJSON devConfig.seed.defaultVpsResources)})
    )

    user_resources = JSON.parse(${builtins.toJSON (builtins.toJSON devConfig.seed.userResources)})
    JSON.parse(${builtins.toJSON (builtins.toJSON devConfig.seed.users)}).each do |attrs|
      upsert_dev_user(admin, environment, attrs, user_resources)
    end

    def install_mail_templates_from(path)
      return unless Dir.exist?(path)

      VpsAdmin::API::MailTemplates.reconcile!(path: path, source_id: path)
    end

    JSON.parse(${builtins.toJSON (builtins.toJSON mailTemplatePaths)}).each do |path|
      install_mail_templates_from(path)
    end

    JSON.parse(${
      builtins.toJSON (builtins.toJSON (devConfig.seed.mailRecipients or [ ]))
    }).each do |attrs|
      recipient = MailRecipient.find_or_initialize_by(label: attrs.fetch('label'))
      recipient.assign_attributes(
        to: attrs['to'],
        cc: attrs['cc'],
        bcc: attrs['bcc']
      )
      recipient.save!

      attrs.fetch('templates', []).each do |template_name|
        template = MailTemplate.find_by(name: template_name)

        if template.nil?
          warn "Skipping missing mail template #{template_name.inspect} for recipient #{recipient.label.inspect}"
          next
        end

        MailTemplateRecipient.find_or_create_by!(
          mail_template: template,
          mail_recipient: recipient
        )
      end
    end
  '';

  allDomains = builtins.attrValues domains ++ builtins.attrValues tmpDomains;
  certStoreDir = builtins.path {
    path = certDir;
    name = "vpsadmin-devcluster-certs";
  };
  sslVirtualHosts = genAttrs allDomains (_: {
    addSSL = true;
    sslCertificate = "${certStoreDir}/vpsadmin-cert.crt";
    sslCertificateKey = "${certStoreDir}/vpsadmin-cert.key";
  });

  sharedFileSystems = {
    vpsadmin = vpsadminSourcePath;
    vpsadminos = vpsadminosSourcePath;
  }
  // optionalAttrs (haveapiSourcePath != "") {
    haveapi = haveapiSourcePath;
  }
  // optionalAttrs (configSourcePath != "") {
    config = configSourcePath;
  }
  // optionalAttrs (webSourcePath != "") {
    web = webSourcePath;
  };

  sharedMounts = {
    "/mnt/vpsadmin" = {
      device = "vpsadmin";
      fsType = "virtiofs";
      options = [ "nofail" ];
    };
    "/mnt/vpsadminos" = {
      device = "vpsadminos";
      fsType = "virtiofs";
      options = [ "nofail" ];
    };
  }
  // optionalAttrs (haveapiSourcePath != "") {
    "/mnt/haveapi" = {
      device = "haveapi";
      fsType = "virtiofs";
      options = [ "nofail" ];
    };
  }
  // optionalAttrs (configSourcePath != "") {
    "/mnt/configuration" = {
      device = "config";
      fsType = "virtiofs";
      options = [ "nofail" ];
    };
  }
  // optionalAttrs (webSourcePath != "") {
    "/mnt/web" = {
      device = "web";
      fsType = "virtiofs";
      options = [ "nofail" ];
    };
  };

  devHosts = {
    "${serviceIp}" = [
      "vpsadmin-services"
    ]
    ++ allDomains;
  }
  // listToAttrs (map (node: nameValuePair node.ip (nodeHostNames node)) allNodeList);
  dnsmasqHostRecords = builtins.concatLists (
    lib.mapAttrsToList (ip: names: map (name: "${name},${ip}") names) devHosts
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

  userNetwork = mkUserNetwork "";
  bridgeNetwork = {
    type = "bridge";
    opts = {
      link = devConfig.network.bridge;
    }
    // optionalAttrs (bridgeHelper != "") {
      helper = bridgeHelper;
    };
  };
  socketNetwork = {
    type = "socket";
    mcast = {
      port = "vpsadmin-devcluster-${slug}";
    };
  };
  localForwardPorts = {
    services = {
      ssh = 10022;
      https = 10443;
    };
  }
  // listToAttrs (
    map (node: nameValuePair node.machineName { ssh = node.sshPort; }) (
      filter (node: node.sshPort != null) allNodeList
    )
  );
  localUserNetwork =
    machineName:
    let
      ports = localForwardPorts.${machineName} or { };
      forwards =
        (lib.optional (ports ? https) "tcp:127.0.0.1:${toString ports.https}-:443")
        ++ (lib.optional (ports ? ssh) "tcp:127.0.0.1:${toString ports.ssh}-:22");
    in
    mkUserNetwork (concatMapStringsSep ",hostfwd=" (v: v) forwards);
  machineNetworks =
    machineName:
    if networkModeChecked == "bridge" then
      [
        userNetwork
        bridgeNetwork
      ]
    else
      [
        (localUserNetwork machineName)
        socketNetwork
      ];

  sshModule = {
    services.openssh = {
      enable = true;
      settings.PermitRootLogin = "yes";
    };
    users.users.root.openssh.authorizedKeys.keyFiles = [ sshPubKey ];
  };

  servicesModule =
    {
      config,
      pkgs,
      lib,
      ...
    }:
    let
      mkVpsfreeWebHost = language: {
        addSSL = true;
        sslCertificate = "${certStoreDir}/vpsadmin-cert.crt";
        sslCertificateKey = "${certStoreDir}/vpsadmin-cert.key";
        root = "${webRoot}/${language}/";
        locations."~ \\.php$".extraConfig = ''
          ssi on;
          gzip off;
          fastcgi_pass unix:${config.services.phpfpm.pools.vpsfree.socket};
        '';
        locations."/".extraConfig = ''
          gzip off;
          ssi on;
        '';
        locations."/prihlaska/".extraConfig = ''
          gzip off;
          ssi on;
        '';
        locations."/css/".extraConfig = ''
          alias ${webRoot}/css/;
        '';
        locations."/js/".extraConfig = ''
          alias ${webRoot}/js/;
        '';
        locations."/obrazky/".extraConfig = ''
          alias ${webRoot}/obrazky/;
        '';
        locations."/download/".extraConfig = ''
          alias ${webRoot}/download/;
        '';
      };
    in
    {
      imports = [
        sshModule
        vpsfStatusModule
      ];

      boot.initrd.kernelModules = [ "virtiofs" ];
      boot.supportedFilesystems.virtiofs = true;

      networking = {
        hostName = lib.mkForce "vpsadmin-services";
        hosts = devHosts;
        firewall = {
          allowedTCPPorts = [
            22
            80
            443
          ]
          ++ lib.optional clusterResolverEnabled 53;
          allowedUDPPorts = lib.optional clusterResolverEnabled 53;
        };
      }
      // optionalAttrs (serviceMachineNameservers != [ ]) {
        nameservers = serviceMachineNameservers;
      }
      // optionalAttrs (networkModeChecked == "bridge") {
        defaultGateway = {
          address = devGateway;
          interface = "eth1";
        };
      };

      services.dnsmasq = lib.mkIf clusterResolverEnabled {
        enable = true;
        resolveLocalQueries = false;
        settings = {
          no-resolv = true;
          bind-interfaces = true;
          listen-address = [
            "127.0.0.1"
            serviceIp
          ];
          server = resolverUpstreamNameservers;
          host-record = dnsmasqHostRecords;
        };
      };

      services.vpsf-status = {
        enable = true;
        package = vpsfStatusPackage;
        settings = {
          check_interval = 30;
          check_timeout = 10;
          history_days = 7;
          vpsadmin = {
            api_url = "https://${domains.api}";
            webui_url = "https://${domains.webui}";
            console_url = "https://${domains.console}/console.js";
          };
          locations = [ vpsfStatusLocation ];
          web_services = [ ];
          nameservers = [ ];
        };
      };

      systemd.services.adminer = {
        description = "Adminer database browser";
        wantedBy = [ "multi-user.target" ];
        after = [
          "network.target"
          "mysql.service"
        ];
        serviceConfig = {
          ExecStart = "${pkgs.php}/bin/php -S 127.0.0.1:${toString adminerPort} -t ${pkgs.adminer} ${pkgs.adminer}/adminer.php";
          Restart = "always";
          RestartSec = "2s";
        };
      };

      fileSystems = sharedMounts;

      security.pki.certificateFiles = [ "${certStoreDir}/vpsadmin-ca.crt" ];

      environment.systemPackages = lib.mkIf webEnabled (
        with pkgs;
        [
          xz
        ]
      );

      users = lib.mkIf webEnabled {
        users.vpsfree = {
          isSystemUser = true;
          group = "vpsfree";
          description = "vpsfree main web account";
        };

        groups.vpsfree = { };
      };

      services.phpfpm.pools.vpsfree = lib.mkIf webEnabled {
        user = "vpsfree";
        group = "vpsfree";

        settings = {
          "pm" = "dynamic";
          "listen.owner" = config.services.nginx.user;
          "pm.max_children" = 5;
          "pm.start_servers" = 2;
          "pm.min_spare_servers" = 1;
          "pm.max_spare_servers" = 3;
          "pm.max_requests" = 500;
        };
      };

      systemd.services.vpsfree-web-live-root = lib.mkIf webEnabled {
        description = "Create live vpsFree.cz web source tree";
        wantedBy = [ "multi-user.target" ];
        before = [
          "nginx.service"
          "phpfpm-vpsfree.service"
        ];
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
        };
        path = with pkgs; [
          bash
          coreutils
        ];
        script = ''
          set -euo pipefail

          src=/mnt/web
          dst=${webRoot}

          rm -rf "$dst"
          mkdir -p "$dst"

          cat > "$dst/config.php" <<'PHP'
          <?php
          define ('API_URL', 'https://${domains.api}');
          define ('ENVIRONMENT_ID', ${toString webEnvironmentId});
          PHP

          if [ -L "$src/config.php" ] || [ ! -e "$src/config.php" ]; then
            ln -sfn "$dst/config.php" "$src/config.php"
          fi

          mkdir -p "$src/vendor"

          shopt -s dotglob nullglob
          for entry in "$src/vendor"/*; do
            if [ -L "$entry" ]; then
              rm -f "$entry"
            fi
          done

          for entry in ${webPackage}/vendor/*; do
            base="$(basename "$entry")"
            if [ -L "$src/vendor/$base" ] || [ ! -e "$src/vendor/$base" ]; then
              ln -sfn "$entry" "$src/vendor/$base"
            fi
          done

          for entry in "$src"/*; do
            base="$(basename "$entry")"
            case "$base" in
              .git|config.php|result|result-*|vendor)
                continue
                ;;
            esac
            ln -s "$entry" "$dst/$base"
          done

          ln -s "$src/vendor" "$dst/vendor"
        '';
      };

      systemd.services.phpfpm-vpsfree = lib.mkIf webEnabled {
        requires = [ "vpsfree-web-live-root.service" ];
        after = [ "vpsfree-web-live-root.service" ];
      };

      systemd.services.nginx = lib.mkIf webEnabled {
        requires = [ "vpsfree-web-live-root.service" ];
        after = [ "vpsfree-web-live-root.service" ];
      };

      vpsadmin = {
        plugins = lib.mkForce enabledPlugins;

        databaseSetup.seedFiles = lib.mkForce [
          "test.nix"
          "${devSeed}"
        ];

        varnish.api = {
          test.domain = lib.mkForce domains.api;
          maintenance = {
            domain = tmpDomains.api;
            backend.path = "/run/haproxy/vpsadmin-api.sock";
          };
        };

        frontend = {
          enableACME = lib.mkForce false;
          forceSSL = lib.mkForce false;

          api = {
            test.domain = lib.mkForce domains.api;
            maintenance = {
              domain = tmpDomains.api;
              backend.address = "unix:/run/varnish/vpsadmin-varnish.sock";
            };
          };

          auth.test = {
            domain = domains.auth;
            backend.address = "unix:/run/haproxy/vpsadmin-api.sock";
          };
          auth.maintenance = {
            domain = tmpDomains.auth;
            backend.address = "unix:/run/haproxy/vpsadmin-api.sock";
          };

          console-router = {
            test.domain = lib.mkForce domains.console;
            maintenance = {
              domain = tmpDomains.console;
              backend.address = "unix:/run/haproxy/vpsadmin-console-router.sock";
            };
          };

          webui.test = {
            domain = lib.mkForce domains.webui;
          };
          webui.maintenance = {
            domain = tmpDomains.webui;
            backend.address = "unix:/run/haproxy/vpsadmin-webui.sock";
          };
        };
      };

      services.nginx.virtualHosts =
        sslVirtualHosts
        // {
          "${domains.mailpit}" = {
            addSSL = true;
            sslCertificate = "${certStoreDir}/vpsadmin-cert.crt";
            sslCertificateKey = "${certStoreDir}/vpsadmin-cert.key";
            basicAuth = mailpitBasicAuth;
            locations."/" = {
              proxyPass = "http://127.0.0.1:${toString mailCapture.webPort}";
              proxyWebsockets = true;
            };
          };
          "${domains.status}" = {
            addSSL = true;
            sslCertificate = "${certStoreDir}/vpsadmin-cert.crt";
            sslCertificateKey = "${certStoreDir}/vpsadmin-cert.key";
            locations."/" = {
              proxyPass = "http://127.0.0.1:${toString config.services.vpsf-status.port}";
            };
          };
          "${domains.adminer}" = {
            addSSL = true;
            sslCertificate = "${certStoreDir}/vpsadmin-cert.crt";
            sslCertificateKey = "${certStoreDir}/vpsadmin-cert.key";
            basicAuth = adminerBasicAuth;
            locations."/" = {
              proxyPass = "http://127.0.0.1:${toString adminerPort}";
            };
          };
        }
        // optionalAttrs webEnabled {
          "${domains.webCs}" = mkVpsfreeWebHost "cs";
          "${domains.webEn}" = mkVpsfreeWebHost "en";
        };

      systemd.services.vpsadmin-devcluster-seed =
        let
          dbCfg = config.vpsadmin.databaseSetup;
        in
        {
          description = "Apply vpsAdmin devcluster seed overrides";
          wantedBy = [ "multi-user.target" ];
          after = [ "vpsadmin-database-setup.service" ];
          requires = [ "vpsadmin-database-setup.service" ];
          before = [ "vpsadmin-api.service" ];
          environment = {
            RACK_ENV = "production";
            SCHEMA = "${dbCfg.stateDirectory}/cache/schema.rb";
          };
          serviceConfig = {
            Type = "oneshot";
            User = dbCfg.user;
            Group = dbCfg.group;
            WorkingDirectory = "${dbCfg.package}/database";
          };
          script = ''
            set -euo pipefail
            echo "Seeding file ${devSeed}"
            ${dbCfg.package}/ruby-env/bin/bundle exec rake db:seed:file SEED_FILE=${devSeed}
          '';
        };

      systemd.services.vpsadmin-api = {
        requires = [ "vpsadmin-devcluster-seed.service" ];
        after = [ "vpsadmin-devcluster-seed.service" ];
      };

      containers.webui = {
        bindMounts."/mnt/vpsadmin" = {
          hostPath = "/mnt/vpsadmin";
          isReadOnly = false;
        };

        config =
          {
            config,
            pkgs,
            lib,
            ...
          }:
          {
            networking.hosts = devHosts;
            security.pki.certificateFiles = [ "${certStoreDir}/vpsadmin-ca.crt" ];

            vpsadmin = {
              plugins = lib.mkForce enabledPlugins;

              webui = {
                domain = lib.mkForce domains.webui;
                sourceCodeDir = lib.mkForce "/run/vpsadmin-live-webui";
                api.externalUrl = lib.mkForce "https://${domains.api}";
                api.internalUrl = lib.mkForce "http://${domains.api}";
                api.oauth2TrustedOrigins = [
                  "https://${domains.api}"
                  "https://${domains.auth}"
                  "https://${tmpDomains.auth}"
                ];
                extraConfig = ''
                  ini_set('session.save_path', '/run/vpsadmin-webui-sessions');
                  ini_set('session.gc_probability', '1');
                  ini_set('session.gc_divisor', '1');
                  ini_set('session.gc_maxlifetime', '3600');
                '';
              };
            };

            systemd.tmpfiles.rules = [
              "d /run/vpsadmin-webui-sessions 0750 vpsadmin-webui vpsadmin-webui - -"
            ];

            systemd.services.vpsadmin-webui-prune-sessions = {
              description = "Prune expired vpsAdmin web UI sessions";
              after = [ "systemd-tmpfiles-setup.service" ];
              unitConfig.ConditionPathIsDirectory = "/run/vpsadmin-webui-sessions";
              serviceConfig = {
                Type = "oneshot";
                User = "vpsadmin-webui";
                Group = "vpsadmin-webui";
                ExecStart = "${pkgs.findutils}/bin/find /run/vpsadmin-webui-sessions -maxdepth 1 -type f -name 'sess_*' -mmin +60 -delete";
              };
            };

            systemd.timers.vpsadmin-webui-prune-sessions = {
              description = "Prune expired vpsAdmin web UI sessions";
              wantedBy = [ "timers.target" ];
              timerConfig = {
                OnBootSec = "5min";
                OnUnitActiveSec = "5min";
                AccuracySec = "1min";
              };
            };

            systemd.services.vpsadmin-webui-live-root = {
              description = "Create live vpsAdmin web UI source tree";
              wantedBy = [ "multi-user.target" ];
              before = [
                "nginx.service"
                "phpfpm-vpsadmin-webui.service"
              ];
              serviceConfig = {
                Type = "oneshot";
                RemainAfterExit = true;
              };
              path = with pkgs; [
                bash
                coreutils
              ];
              script = ''
                set -euo pipefail

                src=/mnt/vpsadmin/webui
                dst=/run/vpsadmin-live-webui

                rm -rf "$dst"
                mkdir -p "$dst"

                shopt -s dotglob nullglob
                for entry in "$src"/*; do
                  base="$(basename "$entry")"
                  case "$base" in
                    .git|.phpunit.cache|vendor)
                      continue
                      ;;
                  esac
                  ln -s "$entry" "$dst/$base"
                done

                ln -s ${config.vpsadmin.webui.package}/vendor "$dst/vendor"
                ln -s ${config.vpsadmin.webui.package}/.git-revision "$dst/.git-revision"
              '';
            };

            systemd.services.phpfpm-vpsadmin-webui = {
              requires = [ "vpsadmin-webui-live-root.service" ];
              after = [ "vpsadmin-webui-live-root.service" ];
            };
            systemd.services.nginx = {
              requires = [ "vpsadmin-webui-live-root.service" ];
              after = [ "vpsadmin-webui-live-root.service" ];
            };
          };
      };

      containers.mailer.config =
        { pkgs, lib, ... }:
        lib.mkIf mailCapture.enable {
          systemd.services.mailpit = {
            description = "Development mail capture service";
            wantedBy = [ "multi-user.target" ];
            after = [ "network.target" ];
            serviceConfig = {
              ExecStart = "${pkgs.mailpit}/bin/mailpit --smtp 127.0.0.1:${toString mailCapture.smtpPort} --listen 127.0.0.1:${toString mailCapture.webPort}";
              Restart = "always";
              RestartSec = "2s";
            };
          };

          vpsadmin.nodectld.settings.mailer.smtp_port = lib.mkForce mailCapture.smtpPort;
        };
    };

  nodeModule =
    node:
    { pkgs, lib, ... }:
    {
      imports = [ sshModule ];

      boot.initrd.kernelModules = [ "virtiofs" ];
      boot.supportedFilesystems.virtiofs = true;

      fileSystems = sharedMounts;

      networking = {
        hosts = devHosts;
        custom = lib.mkIf (networkModeChecked == "bridge") (
          lib.mkAfter ''
            ip route replace default via ${devGateway} dev eth1
          ''
        );
      }
      // optionalAttrs (peerMachineNameservers != [ ]) {
        nameservers = peerMachineNameservers;
      };

      environment.systemPackages = with pkgs; [
        git
        htop
        tree
      ];
    };

  dnsServerModule =
    dnsServer:
    { pkgs, lib, ... }:
    {
      imports = [
        sshModule
        (vpsadmin.outPath + "/tests/configs/nixos/vpsadmin-dns-server.nix")
      ];

      networking = {
        hosts = devHosts;
      }
      // optionalAttrs (peerMachineNameservers != [ ]) {
        nameservers = peerMachineNameservers;
      }
      // optionalAttrs (networkModeChecked == "bridge") {
        defaultGateway = {
          address = devGateway;
          interface = "eth1";
        };
      };

      virtualisation.memorySize = lib.mkForce dnsServer.memoryMiB;
      virtualisation.cores = lib.mkForce dnsServer.cpus;

      environment.systemPackages = with pkgs; [
        git
        htop
        tree
      ];

      vpsadmin.test.dnsServer = {
        socketAddress = dnsServer.ip;
        servicesAddress = serviceIp;
        nodeId = dnsServer.id;
        nodeName = dnsServer.name;
        forwarders = bindForwarders;
        inherit locationDomain;
        socketPeers = {
          vpsadmin-services = serviceIp;
        }
        // listToAttrs (map (peer: nameValuePair peer.name peer.ip) allNodeList);
      };
    };

  mkNodeMachine = machineName: node: {
    spin = "vpsadminos";
    disks = [
      {
        type = "file";
        device = "${machineName}-tank.img";
        size = "20G";
      }
    ];
    networks = machineNetworks machineName;
    sharedFileSystems = sharedFileSystems;
    config = {
      _module.args = {
        vpsadminRev = if vpsadminRevision == "" then null else vpsadminRevision;
        vpsadminRevisionDirty = vpsadminRevisionDirty;
      };

      imports = [
        (vpsadminos.outPath + "/tests/configs/vpsadminos/pool-tank.nix")
        (vpsadmin.outPath + "/tests/configs/vpsadminos/node.nix")
        (nodeModule node)
      ];

      boot.qemu.memory = if node.role == "storage" then 4096 else 8192;
      boot.qemu.cpus = 4;

      system.vpsadminos = {
        revision = if vpsadminosRevision == "" then null else vpsadminosRevision;
        revisionDirty = vpsadminosRevisionDirty;
      };

      vpsadmin.test.node = {
        socketAddress = node.ip;
        servicesAddress = serviceIp;
        nodeId = node.id;
        nodeName = node.name;
        inherit locationDomain;
        socketPeers = {
          vpsadmin-services = serviceIp;
        }
        // listToAttrs (map (peer: nameValuePair peer.name peer.ip) allNodeList);
      };

      vpsadmin.nodectld.settings.vpsadmin.queues = {
        zfs_send.start_delay = zfsTransferStartDelay;
        zfs_recv.start_delay = zfsTransferStartDelay;
      };
    };
  };

  mkDnsMachine = _machineName: dnsServer: {
    spin = "nixos";
    memory = dnsServer.memoryMiB;
    cpus = dnsServer.cpus;
    cores = dnsServer.cpus;
    networks = machineNetworks dnsServer.machineName;
    sharedFileSystems = sharedFileSystems;
    config = dnsServerModule dnsServer;
  };
in
{
  name = "vpsadmin-devcluster-${slug}";

  description = ''
    Branch-selected vpsAdmin development cluster for ${slug}.
  '';

  machines = {
    services = {
      spin = "nixos";
      memory = 8192;
      cpus = 4;
      cores = 4;
      diskSize = serviceRootDiskMiB;
      networks = machineNetworks "services";
      sharedFileSystems = sharedFileSystems;
      config = {
        _module.args = {
          vpsadminRev = if vpsadminRevision == "" then null else vpsadminRevision;
          vpsadminRevisionDirty = vpsadminRevisionDirty;
        };

        imports = [
          (vpsadmin.outPath + "/tests/configs/nixos/vpsadmin-services.nix")
          servicesModule
        ];

        vpsadmin.test = {
          socketAddress = serviceIp;
          socketPeers = mapAttrs (_: node: node.ip) (selectedNodes // dnsServers);
          seedFiles = [
            "test.nix"
            "${devSeed}"
          ];
          mailpit.enable = false;
          inherit rabbitmqNodeUsers;
        };
      };
    };
  }
  // mapAttrs mkNodeMachine selectedNodes
  // mapAttrs mkDnsMachine dnsServers;

  testScript = ''
    # This config is consumed by devcluster-runner, not by the test runner.
  '';
}
