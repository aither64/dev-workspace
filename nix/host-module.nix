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
      internal_managed_directories=(
        ${lib.concatMapStringsSep "\n        " lib.escapeShellArg internalManagedDirectories}
      )

      path_contains() {
        local parent child
        parent=$1
        child=$2
        [ "$parent" = "$child" ] || [ -z "$parent" ] || [[ "$child" == "$parent/"* ]]
      }

      path_relation() {
        local left right
        left=$1
        right=$2
        if [ "$left" = "$right" ]; then
          relation=equal
        elif path_contains "$left" "$right"; then
          relation=left_parent
        elif path_contains "$right" "$left"; then
          relation=right_parent
        else
          relation=disjoint
        fi
      }

      preflight_managed_directory() {
        local target probe metadata owner mode expected_owner
        target=$1
        probe=$target
        while true; do
          if [ -L "$probe" ]; then
            echo "unsafe symlink in workspace substrate directory path: $probe" >&2
            return 1
          fi
          if [ -e "$probe" ]; then
            if [ ! -d "$probe" ]; then
              echo "non-directory in workspace substrate directory path: $probe" >&2
              return 1
            fi
            metadata=$(stat -c '%u:%a' "$probe") || return 1
            owner=''${metadata%%:*}
            mode=''${metadata#*:}
            expected_owner=0
            if [ "$probe" = "$router_directory" ]; then
              expected_owner=$(id -u ${lib.escapeShellArg cfg.owner}) || return 1
            fi
            if [ "$owner" != "$expected_owner" ] ||
               (( (8#$mode & 0002) != 0 )) ||
               { [ "$probe" != "$router_directory" ] && (( (8#$mode & 0020) != 0 )); }; then
              echo "unsafe ownership or mode in workspace substrate directory path: $probe" >&2
              return 1
            fi
          fi
          [ "$probe" = / ] && break
          probe=''${probe%/*}
          [ -n "$probe" ] || probe=/
        done
      }

      assert_managed_directory_layout() {
        local directory index target probe suffix part identity record_count
        local left right left_directory right_directory expected actual
        local -a record_directories record_identities record_suffixes
        for directory in "''${managed_directories[@]}"; do
          preflight_managed_directory "$directory"
        done
        for directory in "''${internal_managed_directories[@]}"; do
          if [ -e "$directory" ] && mountpoint -q "$directory"; then
            echo "unsafe mount at internal workspace substrate directory: $directory" >&2
            return 1
          fi
        done

        record_count=0
        for ((index = 0; index < ''${#managed_directories[@]}; index++)); do
          target=''${managed_directories[$index]}
          probe=$target
          suffix=
          while true; do
            if [ -e "$probe" ]; then
              identity=$(stat -c '%d:%i' "$probe") || return 1
              record_directories[record_count]=$index
              record_identities[record_count]=$identity
              record_suffixes[record_count]=$suffix
              record_count=$((record_count + 1))
            fi
            [ "$probe" = / ] && break
            part=''${probe##*/}
            suffix="$part''${suffix:+/$suffix}"
            probe=''${probe%/*}
            [ -n "$probe" ] || probe=/
          done
        done

        for ((left = 0; left < record_count; left++)); do
          for ((right = left + 1; right < record_count; right++)); do
            left_directory=''${record_directories[$left]}
            right_directory=''${record_directories[$right]}
            [ "''${record_identities[$left]}" != "''${record_identities[$right]}" ] && continue
            if [ "$left_directory" = "$right_directory" ]; then
              echo "workspace substrate directory path revisits a physical ancestor: ''${managed_directories[$left_directory]}" >&2
              return 1
            fi
            path_relation \
              "''${managed_directories[$left_directory]}" \
              "''${managed_directories[$right_directory]}"
            expected=$relation
            path_relation "''${record_suffixes[$left]}" "''${record_suffixes[$right]}"
            actual=$relation
            if [ "$actual" != "$expected" ]; then
              echo "workspace substrate directories have an unexpected physical relationship: ''${managed_directories[$left_directory]} and ''${managed_directories[$right_directory]}" >&2
              return 1
            fi
          done
        done
      }

      install_managed_directory() {
        local target owner group mode
        target=$1
        owner=$2
        group=$3
        mode=$4
        preflight_managed_directory "$target"
        install -d -o "$owner" -g "$group" -m "$mode" "$target"
        if [ -L "$target" ] || [ ! -d "$target" ] ||
           [ "$(stat -c '%U:%G:%a' "$target")" != "$owner:$group:$mode" ]; then
          echo "could not establish safe workspace substrate directory: $target" >&2
          return 1
        fi
      }

      assert_existing_ca_state_structure() {
        if [ ! -e "$authority" ] && [ ! -L "$authority" ]; then
          return 0
        fi
        if [ -L "$authority" ] || [ ! -d "$authority" ] ||
           [ -L "$ca_key" ] || [ -L "$ca_cert" ] ||
           [ ! -f "$ca_key" ] || [ ! -f "$ca_cert" ] ||
           mountpoint -q "$ca_key" || mountpoint -q "$ca_cert" ||
           [ "$(stat -c '%u:%g:%a:%h' "$ca_key")" != "0:0:600:1" ] ||
           [ "$(stat -c '%u:%g:%a:%h' "$ca_cert")" != "0:0:644:1" ]; then
          echo "incomplete or unsafe workspace CA state" >&2
          return 1
        fi
      }

      assert_managed_directory_layout
      install_managed_directory "$lock_directory" root root 755
      if [ ! -e "$lock_file" ] && [ ! -L "$lock_file" ]; then
        ( set -o noclobber; : > "$lock_file" ) 2>/dev/null || true
      fi
      if [ -L "$lock_file" ] || [ ! -f "$lock_file" ] ||
         [ "$(stat -c '%U:%G:%a' "$lock_file")" != "root:root:600" ]; then
        echo "unsafe workspace substrate lock file: $lock_file" >&2
        exit 1
      fi
      exec 9<>"$lock_file"
      if [ ! "$lock_file" -ef /proc/self/fd/9 ]; then
        echo "workspace substrate lock file changed while opening: $lock_file" >&2
        exit 1
      fi
      flock 9

      current="$tls_dir/current"
      renew=0
      current_target=
      current_pair=
      if [ -e "$current" ] && mountpoint -q "$current"; then
        echo "unsafe mount at workspace TLS current path: $current" >&2
        exit 1
      elif [ ! -L "$current" ]; then
        renew=1
      elif current_target_marker=$(readlink -n "$current" && printf /); then
        current_target=''${current_target_marker%/}
        if [[ "$current_target" =~ ^pairs/pair-[0-9]+-[0-9]+$ ]]; then
          current_pair="$tls_dir/$current_target"
          if ! preflight_managed_directory "$current_pair"; then
            exit 1
          fi
          if [ -e "$current_pair" ] && mountpoint -q "$current_pair"; then
            echo "unsafe mount at selected workspace TLS pair: $current_pair" >&2
            exit 1
          fi
          managed_directories+=("$current_pair")
        else
          renew=1
        fi
      else
        renew=1
      fi

      assert_managed_directory_layout
      assert_existing_ca_state_structure
      install_managed_directory "$router_directory" ${lib.escapeShellArg cfg.owner} ${lib.escapeShellArg cfg.proxyGroup} 2770
      install_managed_directory ${lib.escapeShellArg passwordDirectory} root ${lib.escapeShellArg cfg.ownerGroup} 750
      install_managed_directory ${lib.escapeShellArg authDirectory} root ${lib.escapeShellArg nginxGroup} 750
      install_managed_directory "$pki_state" root root 700
      install_managed_directory "$tls_dir" root ${lib.escapeShellArg nginxGroup} 750
      install_managed_directory "$tls_dir/pairs" root ${lib.escapeShellArg nginxGroup} 750
      install_managed_directory ${lib.escapeShellArg publicCaDirectory} root root 755
      assert_managed_directory_layout

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
      assert_managed_directory_layout
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

      if [ "$renew" -eq 0 ]; then
        if ! leaf_pair_valid "$current_pair"; then
          renew=1
        elif current_target_after_marker=$(readlink -n "$current" && printf /); then
          current_target_after=''${current_target_after_marker%/}
          if [ "$current_target_after" != "$current_target" ] ||
             [ "$(stat -Lc '%d:%i' "$current")" != "$(stat -c '%d:%i' "$current_pair")" ]; then
            renew=1
          fi
        else
          renew=1
        fi
      fi

      if [ "$renew" -eq 1 ]; then
        build=$(mktemp -d "$tls_dir/.pair.XXXXXX")
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
      description = "User that owns workspace runtime sockets and generated portal credentials.";
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
