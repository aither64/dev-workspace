{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.dev-workspaces;
  hostPaths = (import ./host-paths.nix { inherit lib; }).defaults;
  nginxUser = config.services.nginx.user;
  nginxGroup = config.services.nginx.group;
  virtualHost = if cfg.wildcardHost == null then cfg.hostName else cfg.wildcardHost;
  wildcardCoversHost =
    if cfg.wildcardHost == null then
      false
    else
      let
        wildcardLabels = lib.splitString "." cfg.wildcardHost;
        hostLabels = lib.splitString "." cfg.hostName;
      in
      builtins.length wildcardLabels == builtins.length hostLabels
      && builtins.head wildcardLabels == "*"
      && builtins.tail wildcardLabels == builtins.tail hostLabels;
  certificateNames = lib.unique (
    lib.optional (!wildcardCoversHost) cfg.hostName
    ++ lib.optional (cfg.wildcardHost != null) cfg.wildcardHost
    ++ cfg.aliases
  );
  serverAliases = lib.filter (name: name != virtualHost) (
    lib.unique ([ cfg.hostName ] ++ cfg.aliases)
  );
  subjectAltNames = lib.concatMapStringsSep "," (name: "DNS:${name}") certificateNames;
  expectedSubjectAltNames = lib.concatStringsSep "\n" (
    lib.sort builtins.lessThan (map (name: "DNS:${name}") certificateNames)
  );
  expectedLeafBasicConstraints = lib.concatStringsSep "\n" [
    "X509v3 Basic Constraints: critical"
    "    CA:FALSE"
  ];
  expectedLeafKeyUsage = lib.concatStringsSep "\n" [
    "X509v3 Key Usage: critical"
    "    Digital Signature"
  ];
  expectedLeafExtendedKeyUsage = lib.concatStringsSep "\n" [
    "X509v3 Extended Key Usage:"
    "    TLS Web Server Authentication"
  ];
  hostPathValidation = import ./host-paths.nix {
    inherit lib;
    paths = {
      inherit (cfg) routerSocket lockFile;
      inherit (cfg.auth) passwordFile htpasswdFile;
      inherit (cfg.tls) caStateDirectory certificateDirectory publicCaFile;
    };
  };
  managedDirectories = hostPathValidation.managedDirectories;
  routerDirectory = managedDirectories.router;
  lockDirectory = managedDirectories.lock;
  passwordDirectory = managedDirectories.password;
  authDirectory = managedDirectories.authentication;
  publicCaDirectory = managedDirectories.publicCa;
  internalManagedDirectories = [
    "${cfg.tls.caStateDirectory}/authority"
    "${cfg.tls.certificateDirectory}/pairs"
  ];
  managedDirectoryInventory = builtins.attrValues managedDirectories ++ internalManagedDirectories;
  managedOutputDirectoryDescription = " Its owning directory must be distinct from and non-nested with every other configured output directory.";
  reconcile = pkgs.writeShellApplication {
    name = "workspace-portal-substrate-reconcile";
    runtimeInputs = with pkgs; [
      apacheHttpd
      coreutils
      diffutils
      gnugrep
      gnused
      openssl
      util-linux
    ];
    text = ''
      set -euo pipefail
      umask 077

      router_directory=${lib.escapeShellArg routerDirectory}
      lock_directory=${lib.escapeShellArg lockDirectory}
      lock_file=${lib.escapeShellArg cfg.lockFile}
      password_file=${lib.escapeShellArg cfg.auth.passwordFile}
      auth_file=${lib.escapeShellArg cfg.auth.htpasswdFile}
      auth_user=${lib.escapeShellArg (if cfg.auth.user == null then cfg.owner else cfg.auth.user)}
      pki_state=${lib.escapeShellArg cfg.tls.caStateDirectory}
      tls_dir=${lib.escapeShellArg cfg.tls.certificateDirectory}
      public_ca=${lib.escapeShellArg cfg.tls.publicCaFile}
      canonical=${lib.escapeShellArg cfg.hostName}
      authority="$pki_state/authority"
      ca_key="$authority/ca-key.pem"
      ca_cert="$authority/ca.pem"
      managed_directories=(
        ${lib.concatMapStringsSep "\n        " lib.escapeShellArg managedDirectoryInventory}
      )
      # The local operator administers this host. Check the configured outputs
      # for ordinary mistakes; do not try to contain that operator's filesystem.
      assert_directory_type() {
        if [ -L "$1" ] || { [ -e "$1" ] && [ ! -d "$1" ]; }; then
          echo "workspace substrate path must be a directory: $1" >&2
          return 1
        fi
      }

      assert_file_type() {
        if [ -L "$1" ] || { [ -e "$1" ] && [ ! -f "$1" ]; }; then
          echo "workspace substrate path must be a regular file: $1" >&2
          return 1
        fi
      }

      install_managed_directory() {
        assert_directory_type "$1"
        install -d -o "$2" -g "$3" -m "$4" "$1"
      }

      assert_existing_ca_state_structure() {
        if [ ! -e "$authority" ] && [ ! -L "$authority" ]; then
          return 0
        fi
        if [ -L "$authority" ] || [ ! -d "$authority" ] ||
           [ -L "$ca_key" ] || [ -L "$ca_cert" ] ||
           [ ! -f "$ca_key" ] || [ ! -f "$ca_cert" ] ||
           [ "$(stat -c '%u:%g:%a' "$ca_key")" != "0:0:600" ] ||
           [ "$(stat -c '%u:%g:%a' "$ca_cert")" != "0:0:644" ]; then
          echo "incomplete or unsafe workspace CA state" >&2
          return 1
        fi
      }

      install_managed_directory "$lock_directory" root root 755
      assert_file_type "$lock_file"
      exec 9<>"$lock_file"
      flock 9
      chown root:root "$lock_file"
      chmod 0600 "$lock_file"

      for directory in "''${managed_directories[@]}"; do
        assert_directory_type "$directory"
      done
      for file in "$password_file" "$auth_file" "$public_ca"; do
        assert_file_type "$file"
      done
      assert_existing_ca_state_structure

      current="$tls_dir/current"
      current_pair=
      if [ -L "$current" ]; then
        current_target=$(readlink "$current")
        if [[ "$current_target" =~ ^pairs/pair-[0-9]+-[0-9]+$ ]]; then
          current_pair="$tls_dir/$current_target"
          assert_directory_type "$current_pair"
        fi
      elif [ -e "$current" ]; then
        echo "workspace TLS current path must be a generation symlink: $current" >&2
        exit 1
      fi

      install_managed_directory "$router_directory" ${lib.escapeShellArg cfg.owner} ${lib.escapeShellArg cfg.proxyGroup} 2770
      install_managed_directory ${lib.escapeShellArg passwordDirectory} root ${lib.escapeShellArg cfg.ownerGroup} 750
      install_managed_directory ${lib.escapeShellArg authDirectory} root ${lib.escapeShellArg nginxGroup} 750
      install_managed_directory "$pki_state" root root 700
      install_managed_directory "$tls_dir" root ${lib.escapeShellArg nginxGroup} 750
      install_managed_directory "$tls_dir/pairs" root ${lib.escapeShellArg nginxGroup} 750
      install_managed_directory ${lib.escapeShellArg publicCaDirectory} root root 755

      # shellcheck disable=SC2016
      auth_pattern='^[^:]+:\$2[aby]\$12\$[./A-Za-z0-9]{53}$'

      auth_file_valid() {
        local candidate entry shape_tmp
        candidate=$1
        if [ -L "$candidate" ] || [ ! -f "$candidate" ] ||
           [ "$(stat -c '%U:%G:%a' "$candidate")" != "root:${nginxGroup}:640" ]; then
          return 1
        fi
        shape_tmp=$(mktemp "$auth_directory/.htpasswd-shape.XXXXXX") || return 1
        if ! entry=$(LC_ALL=C sed -n '1p' "$candidate"); then
          rm -f "$shape_tmp"
          return 1
        fi
        if [ "''${entry%%:*}" != "$auth_user" ] ||
           ! printf '%s\n' "$entry" | grep -Eq "$auth_pattern"; then
          rm -f "$shape_tmp"
          return 1
        fi
        if ! printf '%s\n' "$entry" > "$shape_tmp"; then
          rm -f "$shape_tmp"
          return 1
        fi
        if ! cmp -s "$candidate" "$shape_tmp"; then
          if ! printf '\n' >> "$shape_tmp"; then
            rm -f "$shape_tmp"
            return 1
          fi
          if ! cmp -s "$candidate" "$shape_tmp"; then
            rm -f "$shape_tmp"
            return 1
          fi
        fi
        rm -f "$shape_tmp" || return 1
        htpasswd -vi "$candidate" "$auth_user" < "$password_file" >/dev/null 2>&1
      }

      password_file_valid() {
        local candidate value shape_tmp
        candidate=$1
        if [ -L "$candidate" ] || [ ! -f "$candidate" ] ||
           [ "$(stat -c '%U:%G:%a' "$candidate")" != "root:${cfg.ownerGroup}:640" ]; then
          return 1
        fi
        if ! IFS= read -r value < "$candidate" ||
           ! [[ "$value" =~ ^[0-9a-f]{64}$ ]]; then
          return 1
        fi
        shape_tmp=$(mktemp "$(dirname "$candidate")/.password-shape.XXXXXX") || return 1
        if ! printf '%s\n' "$value" > "$shape_tmp" ||
           ! cmp -s "$candidate" "$shape_tmp"; then
          rm -f "$shape_tmp"
          return 1
        fi
        rm -f "$shape_tmp"
      }

      canonical_certificate_file() {
        local candidate temporary_directory shape_tmp
        candidate=$1
        temporary_directory=$2
        shape_tmp=$(mktemp "$temporary_directory/.certificate-shape.XXXXXX") || return 1
        if ! openssl x509 -in "$candidate" -outform PEM -out "$shape_tmp" 2>/dev/null ||
           ! cmp -s "$candidate" "$shape_tmp"; then
          rm -f "$shape_tmp"
          return 1
        fi
        rm -f "$shape_tmp"
      }

      if [ ! -e "$password_file" ] && [ ! -L "$password_file" ]; then
        password_tmp=$(mktemp "$(dirname "$password_file")/.password.XXXXXX")
        openssl rand -hex 32 > "$password_tmp"
        chown root:${lib.escapeShellArg cfg.ownerGroup} "$password_tmp"
        chmod 0640 "$password_tmp"
        mv -T "$password_tmp" "$password_file"
      fi
      if ! password_file_valid "$password_file"; then
        echo "invalid workspace portal password file: $password_file" >&2
        exit 1
      fi

      auth_directory=${lib.escapeShellArg authDirectory}
      if ! auth_file_valid "$auth_file"; then
        auth_tmp=$(mktemp "$(dirname "$auth_file")/.htpasswd.XXXXXX")
        htpasswd -niBC 12 "$auth_user" < "$password_file" > "$auth_tmp"
        chown root:${lib.escapeShellArg nginxGroup} "$auth_tmp"
        chmod 0640 "$auth_tmp"
        if ! auth_file_valid "$auth_tmp"; then
          rm -f "$auth_tmp"
          echo "generated an invalid workspace portal authentication file" >&2
          exit 1
        fi
        mv -T "$auth_tmp" "$auth_file"
      fi

      if [ ! -e "$authority" ] && [ ! -L "$authority" ]; then
        authority_tmp=$(mktemp -d "$pki_state/.authority.XXXXXX")
        trap 'rm -rf -- "$authority_tmp"' EXIT INT TERM
        openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
          -out "$authority_tmp/ca-key.pem"
        openssl req -x509 -new -sha256 -days ${toString cfg.tls.caValidityDays} \
          -key "$authority_tmp/ca-key.pem" \
          -subj ${lib.escapeShellArg "/CN=${cfg.tls.caCommonName}"} \
          -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
          -addext 'keyUsage=critical,keyCertSign,cRLSign' \
          -addext 'subjectKeyIdentifier=hash' \
          -out "$authority_tmp/ca.pem"
        chown root:root "$authority_tmp" "$authority_tmp/ca-key.pem" "$authority_tmp/ca.pem"
        chmod 0700 "$authority_tmp"
        chmod 0600 "$authority_tmp/ca-key.pem"
        chmod 0644 "$authority_tmp/ca.pem"
        openssl verify -no-CApath -no-CAstore \
          -CAfile "$authority_tmp/ca.pem" "$authority_tmp/ca.pem" >/dev/null
        mv -T "$authority_tmp" "$authority"
        trap - EXIT INT TERM
      fi
      assert_existing_ca_state_structure
      if ! canonical_certificate_file "$ca_cert" "$pki_state"; then
        echo "workspace CA certificate is not one canonical PEM certificate" >&2
        exit 1
      fi
      openssl verify -no-CApath -no-CAstore \
        -CAfile "$ca_cert" "$ca_cert" >/dev/null
      ca_key_public=$(openssl pkey -in "$ca_key" -pubout -outform DER | openssl dgst -sha256)
      ca_cert_public=$(openssl x509 -in "$ca_cert" -pubkey -noout | \
        openssl pkey -pubin -outform DER | openssl dgst -sha256)
      if [ "$ca_key_public" != "$ca_cert_public" ]; then
        echo "workspace CA certificate and key do not match" >&2
        exit 1
      fi

      leaf_pair_valid() {
        local candidate leaf_cert leaf_key actual_names leaf_key_public leaf_cert_public
        local basic_constraints key_usage extended_key_usage
        candidate=$1
        leaf_cert="$candidate/server.pem"
        leaf_key="$candidate/server-key.pem"
        if [ -L "$candidate" ] || [ ! -d "$candidate" ] ||
           [ "$(stat -c '%U:%G:%a' "$candidate")" != "root:${nginxGroup}:750" ] ||
           [ -L "$leaf_cert" ] || [ ! -f "$leaf_cert" ] ||
           [ "$(stat -c '%U:%G:%a' "$leaf_cert")" != "root:${nginxGroup}:644" ] ||
           [ -L "$leaf_key" ] || [ ! -f "$leaf_key" ] ||
           [ "$(stat -c '%U:%G:%a' "$leaf_key")" != "root:${nginxGroup}:640" ]; then
          return 1
        fi
        if ! canonical_certificate_file "$leaf_cert" "$tls_dir" ||
           ! openssl x509 -checkend ${toString cfg.tls.renewBeforeSeconds} \
          -noout -in "$leaf_cert" >/dev/null 2>&1 ||
           ! openssl verify -purpose sslserver -no-CApath -no-CAstore \
             -CAfile "$ca_cert" \
             "$leaf_cert" >/dev/null 2>&1; then
          return 1
        fi
        if ! basic_constraints=$(LC_ALL=C openssl x509 -noout \
          -ext basicConstraints -in "$leaf_cert" 2>/dev/null | sed 's/[[:space:]]*$//') ||
           [ "$basic_constraints" != ${lib.escapeShellArg expectedLeafBasicConstraints} ] ||
           ! key_usage=$(LC_ALL=C openssl x509 -noout \
             -ext keyUsage -in "$leaf_cert" 2>/dev/null | sed 's/[[:space:]]*$//') ||
           [ "$key_usage" != ${lib.escapeShellArg expectedLeafKeyUsage} ] ||
           ! extended_key_usage=$(LC_ALL=C openssl x509 -noout \
             -ext extendedKeyUsage -in "$leaf_cert" 2>/dev/null | sed 's/[[:space:]]*$//') ||
           [ "$extended_key_usage" != ${lib.escapeShellArg expectedLeafExtendedKeyUsage} ]; then
          return 1
        fi
        if ! actual_names=$(openssl x509 -noout -ext subjectAltName -in "$leaf_cert" | \
          tail -n +2 | tr ',' '\n' | sed 's/^[[:space:]]*//' | LC_ALL=C sort) ||
           [ "$actual_names" != ${lib.escapeShellArg expectedSubjectAltNames} ]; then
          return 1
        fi
        if ! leaf_key_public=$(openssl pkey -in "$leaf_key" -pubout -outform DER 2>/dev/null | \
          openssl dgst -sha256) ||
           ! leaf_cert_public=$(openssl x509 -in "$leaf_cert" -pubkey -noout 2>/dev/null | \
          openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256); then
          return 1
        fi
        [ "$leaf_key_public" = "$leaf_cert_public" ]
      }

      if [ -z "$current_pair" ] || ! leaf_pair_valid "$current_pair"; then
        build=$(mktemp -d "$tls_dir/.pair.XXXXXX")
        trap 'rm -rf -- "$build"; rm -f -- "$tls_dir/.current.$$"' EXIT INT TERM
        openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
          -out "$build/server-key.pem"
        openssl req -new -key "$build/server-key.pem" -subj "/CN=$canonical" \
          -out "$build/server.csr"
        {
          echo 'basicConstraints=critical,CA:FALSE'
          echo 'keyUsage=critical,digitalSignature'
          echo 'extendedKeyUsage=serverAuth'
          echo ${lib.escapeShellArg "subjectAltName=${subjectAltNames}"}
          echo 'subjectKeyIdentifier=hash'
          echo 'authorityKeyIdentifier=keyid,issuer'
        } > "$build/extensions.cnf"
        serial=$(openssl rand -hex 16)
        openssl x509 -req -sha256 -days ${toString cfg.tls.certificateValidityDays} \
          -in "$build/server.csr" -CA "$ca_cert" -CAkey "$ca_key" \
          -set_serial "0x$serial" -extfile "$build/extensions.cnf" \
          -out "$build/server.pem"
        rm "$build/server.csr" "$build/extensions.cnf"
        chown root:${lib.escapeShellArg nginxGroup} "$build" "$build/server.pem" "$build/server-key.pem"
        chmod 0750 "$build"
        chmod 0644 "$build/server.pem"
        chmod 0640 "$build/server-key.pem"
        if ! leaf_pair_valid "$build"; then
          rm -rf -- "$build"
          echo "generated an invalid workspace portal TLS pair" >&2
          exit 1
        fi
        pair="$tls_dir/pairs/pair-$(date +%s)-$$"
        mv -T "$build" "$pair"
        ln -s "pairs/$(basename "$pair")" "$tls_dir/.current.$$"
        mv -Tf "$tls_dir/.current.$$" "$current"
        trap - EXIT INT TERM
      fi

      ca_tmp=$(mktemp "$(dirname "$public_ca")/.ca.XXXXXX")
      install -o root -g root -m 0644 "$ca_cert" "$ca_tmp"
      mv -T "$ca_tmp" "$public_ca"
    '';
  };
  firewallRules = lib.concatMapStringsSep "\n" (
    source:
    lib.concatMapStringsSep "\n" (
      port:
      "${pkgs.iptables}/bin/iptables -A nixos-fw -p tcp --dport ${toString port} -s ${lib.escapeShellArg source} -j ACCEPT"
    ) cfg.firewall.ports
  ) cfg.firewall.sourceRanges;
  firewallStopRules = lib.concatMapStringsSep "\n" (
    source:
    lib.concatMapStringsSep "\n" (
      port:
      "${pkgs.iptables}/bin/iptables -D nixos-fw -p tcp --dport ${toString port} -s ${lib.escapeShellArg source} -j ACCEPT 2>/dev/null || true"
    ) cfg.firewall.ports
  ) cfg.firewall.sourceRanges;
in
{
  options.services.dev-workspaces = {
    enable = lib.mkEnableOption "the shared host substrate for development workspaces";
    owner = lib.mkOption {
      type = lib.types.str;
      description = "User that owns runtime sockets and can read the generated portal password.";
    };
    ownerGroup = lib.mkOption {
      type = lib.types.str;
      default = "workspace-portal-owner";
      description = "Group allowed to read the generated portal password.";
    };
    proxyGroup = lib.mkOption {
      type = lib.types.str;
      default = "workspace-portal-proxy";
      description = "Group shared by the workspace owner and the reverse proxy.";
    };
    hostName = lib.mkOption {
      type = lib.types.str;
      description = "Canonical workspace portal hostname.";
    };
    wildcardHost = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Optional wildcard virtual host and certificate name.";
    };
    aliases = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Additional portal hostnames included in the certificate.";
    };
    listenAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = "Address on which nginx accepts portal traffic.";
    };
    routerSocket = lib.mkOption {
      type = lib.types.str;
      default = hostPaths.routerSocket;
      description =
        "Unix socket provided by the user-profile workspace router." + managedOutputDirectoryDescription;
    };
    lockFile = lib.mkOption {
      type = lib.types.str;
      default = hostPaths.lockFile;
      description = "Regular root-owned lock file directly below /run/lock.";
    };
    auth = {
      user = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Basic authentication username. The workspace owner is used when unset.";
      };
      passwordFile = lib.mkOption {
        type = lib.types.str;
        default = hostPaths.passwordFile;
        description = "Persistent generated portal password." + managedOutputDirectoryDescription;
      };
      htpasswdFile = lib.mkOption {
        type = lib.types.str;
        default = hostPaths.htpasswdFile;
        description = "Generated nginx basic authentication file." + managedOutputDirectoryDescription;
      };
    };
    tls = {
      caStateDirectory = lib.mkOption {
        type = lib.types.str;
        default = hostPaths.caStateDirectory;
        description = "Persistent local certificate authority state." + managedOutputDirectoryDescription;
      };
      certificateDirectory = lib.mkOption {
        type = lib.types.str;
        default = hostPaths.certificateDirectory;
        description = "Persistent leaf certificate generations." + managedOutputDirectoryDescription;
      };
      publicCaFile = lib.mkOption {
        type = lib.types.str;
        default = hostPaths.publicCaFile;
        description = "Public copy of the local CA certificate." + managedOutputDirectoryDescription;
      };
      caCommonName = lib.mkOption {
        type = lib.types.str;
        default = "Development Workspace CA";
        description = "Common name used when the local CA is first created.";
      };
      caValidityDays = lib.mkOption {
        type = lib.types.ints.positive;
        default = 3650;
      };
      certificateValidityDays = lib.mkOption {
        type = lib.types.ints.positive;
        default = 397;
      };
      renewBeforeSeconds = lib.mkOption {
        type = lib.types.ints.positive;
        default = 2592000;
      };
    };
    firewall = {
      sourceRanges = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "Source networks allowed to reach the portal. Empty keeps the firewall closed.";
      };
      ports = lib.mkOption {
        type = lib.types.listOf lib.types.port;
        default = [
          80
          443
        ];
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.owner != "" && cfg.hostName != "" && certificateNames != [ ];
        message = "services.dev-workspaces requires non-empty owner and hostName values";
      }
      {
        assertion = hostPathValidation.allAbsolute;
        message = "services.dev-workspaces managed paths must be normalized absolute paths";
      }
      {
        assertion = hostPathValidation.lockInRunLock;
        message = "services.dev-workspaces.lockFile must be directly below /run/lock";
      }
      {
        assertion = hostPathValidation.routerInRun;
        message = "services.dev-workspaces.routerSocket must be below /run";
      }
      {
        assertion = hostPathValidation.stateInVarLib;
        message = "services.dev-workspaces persistent state paths must be below /var/lib";
      }
      {
        assertion = hostPathValidation.directoriesDisjoint;
        message = "services.dev-workspaces managed directories must be distinct and non-nested";
      }
    ];

    users.groups.${cfg.proxyGroup}.members = [ nginxUser ];
    users.groups.${cfg.ownerGroup}.members = [ cfg.owner ];

    system.activationScripts.devWorkspaceCredentials = {
      deps = [ "users" ];
      text = ''
        ${reconcile}/bin/workspace-portal-substrate-reconcile
      '';
    };

    environment.systemPackages = [ reconcile ];

    systemd.services.workspace-portal-certificate-renewal = {
      description = "Renew the development workspace portal TLS certificate";
      after = [ "nginx.service" ];
      serviceConfig = {
        Type = "oneshot";
        UMask = "0077";
      };
      script = ''
        ${reconcile}/bin/workspace-portal-substrate-reconcile
        ${pkgs.systemd}/bin/systemctl reload nginx.service
      '';
    };

    systemd.timers.workspace-portal-certificate-renewal = {
      description = "Periodically check the development workspace portal TLS certificate";
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnCalendar = "weekly";
        Persistent = true;
        RandomizedDelaySec = "6h";
      };
    };

    systemd.services.nginx = {
      restartTriggers = [
        (pkgs.writeText "workspace-portal-nginx-group-v1" "${cfg.proxyGroup}\n")
      ];
      serviceConfig.SupplementaryGroups = [ cfg.proxyGroup ];
    };

    services.nginx = {
      enable = true;
      recommendedProxySettings = true;
      recommendedTlsSettings = true;
      upstreams.dev-workspace.servers."unix:${cfg.routerSocket}" = { };
      virtualHosts.${virtualHost} = {
        serverAliases = serverAliases;
        forceSSL = true;
        listen = [
          {
            addr = cfg.listenAddress;
            port = 80;
          }
          {
            addr = cfg.listenAddress;
            port = 443;
            ssl = true;
          }
        ];
        sslCertificate = "${cfg.tls.certificateDirectory}/current/server.pem";
        sslCertificateKey = "${cfg.tls.certificateDirectory}/current/server-key.pem";
        basicAuthFile = cfg.auth.htpasswdFile;
        extraConfig = ''
          add_header Strict-Transport-Security "max-age=31536000" always;
        '';
        locations."/" = {
          proxyPass = "http://dev-workspace";
          extraConfig = ''
            proxy_buffering off;
            proxy_read_timeout 1h;
            client_max_body_size 16m;
            proxy_set_header Authorization "";
            proxy_hide_header Strict-Transport-Security;
          '';
        };
      };
    };

    networking.firewall.extraCommands = lib.mkAfter firewallRules;
    networking.firewall.extraStopCommands = lib.mkAfter firewallStopRules;
  };
}
