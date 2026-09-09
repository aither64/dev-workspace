{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "dev-workspace-host-module-idempotency";
  nodes.machine = {
    imports = [ self.nixosModules.host ];

    users.users.developer.isNormalUser = true;
    i18n.defaultLocale = "en_US.UTF-8";

    services.dev-workspaces = {
      enable = true;
      owner = "developer";
      hostName = "workspace.example.test";
      wildcardHost = "*.workspace.example.test";
      aliases = [ "legacy-workspace.example.test" ];
      auth = {
        passwordFile = "/var/lib/dev-workspaces/password-state/managed/password";
        htpasswdFile = "/var/lib/dev-workspaces/auth-state/managed/htpasswd";
      };
    };

    system.stateVersion = "26.05";
  };
  testScript = ''
    import base64
    import re
    import shlex

    machine.start()
    machine.wait_for_unit("multi-user.target")

    reconcile = "/run/current-system/sw/bin/workspace-portal-substrate-reconcile"
    password = "/var/lib/dev-workspaces/password-state/managed/password"
    auth = "/var/lib/dev-workspaces/auth-state/managed/htpasswd"
    lock = "/run/lock/dev-workspace-substrate.lock"
    password_parent = "/var/lib/dev-workspaces/password-state"
    auth_parent = "/var/lib/dev-workspaces/auth-state"
    password_directory = f"{password_parent}/managed"
    auth_directory = f"{auth_parent}/managed"
    pki_directory = "/var/lib/dev-workspaces/pki"
    tls_directory = "/var/lib/dev-workspaces/tls"
    public_ca_directory = "/var/lib/dev-workspaces/public"
    tracked = " ".join([
        password,
        auth,
        "/var/lib/dev-workspaces/pki/authority/ca-key.pem",
        "/var/lib/dev-workspaces/pki/authority/ca.pem",
        "/var/lib/dev-workspaces/public/ca.pem",
        "/var/lib/dev-workspaces/tls/current/server-key.pem",
        "/var/lib/dev-workspaces/tls/current/server.pem",
    ])

    def hashes():
        return machine.succeed(f"sha256sum {tracked}")

    def current_target():
        encoded = machine.succeed(
            "readlink -n /var/lib/dev-workspaces/tls/current | base64 -w0"
        ).strip()
        return base64.b64decode(encoded).decode()

    def reconcile_non_c_locale():
        machine.succeed(f"LC_ALL=en_US.UTF-8 {reconcile}")

    def assert_password_valid():
        machine.succeed(f"test $(stat -c %U:%G:%a {password}) = root:workspace-portal-owner:640")
        encoded = machine.succeed(f"base64 -w0 {password}").strip()
        contents = base64.b64decode(encoded)
        assert re.fullmatch(rb"[0-9a-f]{64}\n", contents)

    def assert_password_rejected(command):
        machine.succeed(f"cp -a {password} /tmp/password.good")
        machine.succeed(command)
        broken_hash = machine.succeed(f"sha256sum {password}")
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        assert machine.succeed(f"sha256sum {password}") == broken_hash
        machine.succeed(f"mv -T /tmp/password.good {password}")
        reconcile_non_c_locale()
        assert_password_valid()

    def assert_directory_symlink_rejected(directory, label):
        backup = f"/tmp/{label}-directory-backup"
        target = f"/tmp/{label}-directory-target"
        machine.succeed(
            f"rm -rf {backup} {target}; mv {directory} {backup}; "
            f"install -d -o root -g root -m 0711 {target}; "
            f"printf 'do-not-touch\n' > {target}/sentinel; "
            f"ln -s {target} {directory}"
        )
        before = machine.succeed(
            f"stat -c %U:%G:%a {target}; sha256sum {target}/sentinel"
        )
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %U:%G:%a {target}; sha256sum {target}/sentinel"
        )
        assert after == before
        machine.succeed(f"rm {directory}; mv {backup} {directory}")
        reconcile_non_c_locale()

    def assert_directory_bind_rejected(source, target):
        before = machine.succeed(
            f"stat -c %U:%G:%a {source}; sha256sum {password}"
        )
        machine.succeed(f"mount --bind {source} {target}")
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %U:%G:%a {source}; sha256sum {password}"
        )
        assert after == before
        machine.succeed(f"umount {target}")
        reconcile_non_c_locale()

    def assert_missing_directory_alias_rejected():
        password_backup = "/tmp/password-state-backup"
        auth_backup = "/tmp/auth-state-backup"
        machine.succeed(
            f"mv {password_parent} {password_backup}; "
            f"mv {auth_parent} {auth_backup}; "
            f"install -d -o root -g root -m 0711 {password_parent} {auth_parent}; "
            f"mount --bind {password_parent} {auth_parent}"
        )
        before = machine.succeed(
            f"stat -c %U:%G:%a {password_parent}; "
            f"test ! -e {password_directory}; test ! -e {auth_directory}"
        )
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %U:%G:%a {password_parent}; "
            f"test ! -e {password_directory}; test ! -e {auth_directory}"
        )
        assert after == before
        machine.succeed(
            f"umount {auth_parent}; rmdir {password_parent} {auth_parent}; "
            f"mv {password_backup} {password_parent}; "
            f"mv {auth_backup} {auth_parent}"
        )
        reconcile_non_c_locale()

    def assert_ancestor_reentry_rejected():
        root_directory = "/var/lib/dev-workspaces"
        escaped_directory = f"{root_directory}/managed"
        backup = "/tmp/password-state-reentry-backup"
        machine.succeed(
            f"mv {password_parent} {backup}; "
            f"install -d -o root -g root -m 0711 {password_parent}; "
            f"mount --bind {root_directory} {password_parent}"
        )
        before = machine.succeed(
            f"stat -c %U:%G:%a {root_directory}; "
            f"sha256sum {backup}/managed/password {auth} "
            f"{pki_directory}/authority/ca-key.pem "
            f"{pki_directory}/authority/ca.pem; "
            f"test ! -e {escaped_directory}"
        )
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %U:%G:%a {root_directory}; "
            f"sha256sum {backup}/managed/password {auth} "
            f"{pki_directory}/authority/ca-key.pem "
            f"{pki_directory}/authority/ca.pem; "
            f"test ! -e {escaped_directory}"
        )
        assert after == before
        machine.succeed(
            f"umount {password_parent}; rmdir {password_parent}; "
            f"mv {backup} {password_parent}"
        )
        reconcile_non_c_locale()

    def assert_ca_rejected(command):
        ca_cert = f"{pki_directory}/authority/ca.pem"
        public_ca = "/var/lib/dev-workspaces/public/ca.pem"
        machine.succeed(f"cp -a {ca_cert} /tmp/ca.pem.good")
        public_before = machine.succeed(f"sha256sum {public_ca}")
        machine.succeed(command)
        broken = machine.succeed(f"sha256sum {ca_cert}")
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        assert machine.succeed(f"sha256sum {ca_cert}") == broken
        assert machine.succeed(f"sha256sum {public_ca}") == public_before
        machine.succeed(f"mv -T /tmp/ca.pem.good {ca_cert}")
        reconcile_non_c_locale()

    def assert_ca_alias_rejected(alias_kind):
        ca_cert = f"{pki_directory}/authority/ca.pem"
        public_ca = "/var/lib/dev-workspaces/public/ca.pem"
        source = f"/tmp/aliased-ca-{alias_kind}.pem"
        backup = f"/tmp/ca-{alias_kind}.pem.good"
        machine.succeed(
            f"cp -a {ca_cert} {backup}; cp {ca_cert} {source}; "
            f"chown root:root {source}; chmod 0644 {source}; "
            f"rm {ca_cert}"
        )
        if alias_kind == "hardlink":
            auth_backup = "/tmp/auth-ca-hardlink.good"
            machine.succeed(
                f"ln {source} {ca_cert}; cp -a {auth} {auth_backup}; "
                f"printf 'malformed\\n' > {auth}; "
                f"chown developer:nginx {auth}; chmod 0600 {auth}"
            )
            state_files = (
                f"{source} {pki_directory}/authority/ca-key.pem "
                f"{public_ca} {password} {auth}"
            )
            state_checks = "true"
        elif alias_kind == "bind":
            password_backup = "/tmp/password-ca-bind.good"
            machine.succeed(
                f"install -o root -g root -m 0644 /dev/null {ca_cert}; "
                f"mount --bind {source} {ca_cert}; "
                f"mv {password} {password_backup}"
            )
            state_files = (
                f"{source} {pki_directory}/authority/ca-key.pem "
                f"{public_ca} {password_backup} {auth}"
            )
            state_checks = f"test ! -e {password}"
        else:
            raise AssertionError(f"unknown CA alias kind: {alias_kind}")
        before = machine.succeed(
            f"stat -c %u:%g:%a:%h {source} "
            f"{pki_directory}/authority/ca-key.pem {auth}; "
            f"sha256sum {state_files}; {state_checks}"
        )
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %u:%g:%a:%h {source} "
            f"{pki_directory}/authority/ca-key.pem {auth}; "
            f"sha256sum {state_files}; {state_checks}"
        )
        assert after == before
        if alias_kind == "bind":
            machine.succeed(
                f"umount {ca_cert}; mv {password_backup} {password}"
            )
        else:
            machine.succeed(f"mv -T {auth_backup} {auth}")
        machine.succeed(
            f"rm {ca_cert} {source}; mv -T {backup} {ca_cert}"
        )
        reconcile_non_c_locale()

    def assert_ca_metadata_rejected_before_auth_mutation():
        ca_cert = f"{pki_directory}/authority/ca.pem"
        auth_backup = "/tmp/auth-ca-metadata.good"
        machine.succeed(
            f"cp -a {auth} {auth_backup}; "
            f"printf 'malformed\\n' > {auth}; "
            f"chown developer:nginx {auth}; chmod 0600 {auth}; "
            f"chmod 0600 {ca_cert}"
        )
        before = machine.succeed(
            f"stat -c %u:%g:%a:%h {ca_cert} {auth}; "
            f"sha256sum {ca_cert} {public_ca_directory}/ca.pem "
            f"{password} {auth}"
        )
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %u:%g:%a:%h {ca_cert} {auth}; "
            f"sha256sum {ca_cert} {public_ca_directory}/ca.pem "
            f"{password} {auth}"
        )
        assert after == before
        machine.succeed(
            f"chmod 0644 {ca_cert}; mv -T {auth_backup} {auth}"
        )
        reconcile_non_c_locale()

    def assert_leaf_valid():
        target = current_target()
        assert re.fullmatch(r"pairs/pair-[0-9]+-[0-9]+", target)
        pair = f"{tls_directory}/{target}"
        machine.succeed(f"test $(stat -c %U:%G:%a {pair}) = root:nginx:750")
        machine.succeed(f"test $(stat -c %U:%G:%a {pair}/server.pem) = root:nginx:644")
        machine.succeed(f"test $(stat -c %U:%G:%a {pair}/server-key.pem) = root:nginx:640")
        machine.succeed(
            "${pkgs.openssl}/bin/openssl verify -purpose sslserver "
            f"-CAfile {pki_directory}/authority/ca.pem {pair}/server.pem"
        )
        basic_constraints = machine.succeed(
            "${pkgs.openssl}/bin/openssl x509 -noout -ext basicConstraints "
            f"-in {pair}/server.pem"
        ).rstrip()
        assert basic_constraints == (
            "X509v3 Basic Constraints: critical\n    CA:FALSE"
        )
        key_usage = machine.succeed(
            "${pkgs.openssl}/bin/openssl x509 -noout -ext keyUsage "
            f"-in {pair}/server.pem"
        ).rstrip()
        assert key_usage == "X509v3 Key Usage: critical\n    Digital Signature"
        extended_key_usage = machine.succeed(
            "${pkgs.openssl}/bin/openssl x509 -noout -ext extendedKeyUsage "
            f"-in {pair}/server.pem"
        ).rstrip()
        assert extended_key_usage == (
            "X509v3 Extended Key Usage: \n    TLS Web Server Authentication"
        )
        names = machine.succeed(
            "${pkgs.openssl}/bin/openssl x509 -noout -ext subjectAltName "
            f"-in {pair}/server.pem"
        )
        assert set(re.findall(r"DNS:([^,\s]+)", names)) == {
            "workspace.example.test",
            "*.workspace.example.test",
            "legacy-workspace.example.test",
        }
        key_public = machine.succeed(
            "${pkgs.openssl}/bin/openssl pkey "
            f"-in {pair}/server-key.pem -pubout -outform DER | sha256sum"
        ).split()[0]
        cert_public = machine.succeed(
            "${pkgs.openssl}/bin/openssl x509 "
            f"-in {pair}/server.pem -pubkey -noout | "
            "${pkgs.openssl}/bin/openssl pkey -pubin -outform DER | sha256sum"
        ).split()[0]
        assert key_public == cert_public

    def assert_leaf_replaced_once(command):
        before_target = current_target()
        preserved_before = "\n".join(hashes().splitlines()[:5])
        machine.succeed(command)
        reconcile_non_c_locale()
        after_target = current_target()
        assert after_target != before_target
        assert_leaf_valid()
        preserved_after = "\n".join(hashes().splitlines()[:5])
        assert preserved_after == preserved_before
        reconcile_non_c_locale()
        assert current_target() == after_target

    def replace_current_leaf(extensions):
        extension_arguments = " ".join(shlex.quote(value) for value in extensions)
        return (
            f"pair={tls_directory}/$(readlink {tls_directory}/current); "
            "${pkgs.openssl}/bin/openssl req -new -sha256 "
            ' -subj "/CN=workspace.example.test" '
            ' -key "$pair/server-key.pem" -out /tmp/retained-leaf.csr; '
            f"printf '%s\\n' {extension_arguments} > /tmp/retained-leaf.cnf; "
            "${pkgs.openssl}/bin/openssl x509 -req -sha256 -days 397 "
            " -in /tmp/retained-leaf.csr "
            f" -CA {pki_directory}/authority/ca.pem "
            f" -CAkey {pki_directory}/authority/ca-key.pem "
            " -set_serial \"0x$("
            "${pkgs.openssl}/bin/openssl rand -hex 16)"
            '" -extfile /tmp/retained-leaf.cnf -out "$pair/server.pem"; '
            'chown root:nginx "$pair/server.pem"; chmod 0644 "$pair/server.pem"; '
            "rm /tmp/retained-leaf.csr /tmp/retained-leaf.cnf"
        )

    def assert_foreign_trust_does_not_retain_leaf():
        before_target = current_target()
        preserved_before = "\n".join(hashes().splitlines()[:5])
        machine.succeed(
            "rm -rf /tmp/foreign-trust /tmp/foreign-ca-key.pem; "
            "mkdir /tmp/foreign-trust; "
            "${pkgs.openssl}/bin/openssl genpkey -algorithm EC "
            "-pkeyopt ec_paramgen_curve:P-256 "
            "-out /tmp/foreign-ca-key.pem; "
            "${pkgs.openssl}/bin/openssl req -x509 -new -sha256 -days 30 "
            "-key /tmp/foreign-ca-key.pem -subj '/CN=Foreign test CA' "
            "-addext 'basicConstraints=critical,CA:TRUE,pathlen:0' "
            "-addext 'keyUsage=critical,keyCertSign,cRLSign' "
            "-out /tmp/foreign-trust/ca.pem; "
            "${pkgs.openssl}/bin/openssl rehash /tmp/foreign-trust"
        )
        extension_arguments = " ".join(shlex.quote(value) for value in [
            "basicConstraints=critical,CA:FALSE",
            "keyUsage=critical,digitalSignature",
            "extendedKeyUsage=serverAuth",
            "subjectAltName=DNS:workspace.example.test,DNS:*.workspace.example.test,DNS:legacy-workspace.example.test",
            "subjectKeyIdentifier=hash",
            "authorityKeyIdentifier=keyid,issuer",
        ])
        machine.succeed(
            f"pair={tls_directory}/$(readlink {tls_directory}/current); "
            "${pkgs.openssl}/bin/openssl req -new -sha256 "
            " -subj '/CN=workspace.example.test' "
            " -key \"$pair/server-key.pem\" -out /tmp/foreign-leaf.csr; "
            f"printf '%s\\n' {extension_arguments} > /tmp/foreign-leaf.cnf; "
            "${pkgs.openssl}/bin/openssl x509 -req -sha256 -days 30 "
            " -in /tmp/foreign-leaf.csr "
            " -CA /tmp/foreign-trust/ca.pem "
            " -CAkey /tmp/foreign-ca-key.pem "
            " -set_serial \"0x$("
            "${pkgs.openssl}/bin/openssl rand -hex 16)\" "
            " -extfile /tmp/foreign-leaf.cnf -out \"$pair/server.pem\"; "
            "chown root:nginx \"$pair/server.pem\"; chmod 0644 \"$pair/server.pem\"; "
            "rm /tmp/foreign-leaf.csr /tmp/foreign-leaf.cnf"
        )
        machine.succeed(
            f"SSL_CERT_DIR=/tmp/foreign-trust LC_ALL=en_US.UTF-8 {reconcile}"
        )
        after_target = current_target()
        assert after_target != before_target
        assert_leaf_valid()
        preserved_after = "\n".join(hashes().splitlines()[:5])
        assert preserved_after == preserved_before
        reconcile_non_c_locale()
        assert current_target() == after_target

    def assert_selected_pair_preflight_precedes_mutation():
        pair = f"{tls_directory}/{current_target()}"
        auth_backup = "/tmp/auth-selected-pair.good"
        machine.succeed(
            f"cp -a {auth} {auth_backup}; "
            f"printf 'malformed\\n' > {auth}; "
            f"chown developer:nginx {auth}; chmod 0600 {auth}; "
            f"mount --bind {password_directory} {pair}"
        )
        before = machine.succeed(
            f"stat -c %U:%G:%a {auth} {password_directory}; "
            f"sha256sum {auth} {password} "
            f"{pki_directory}/authority/ca-key.pem "
            f"{pki_directory}/authority/ca.pem "
            f"/var/lib/dev-workspaces/public/ca.pem"
        )
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %U:%G:%a {auth} {password_directory}; "
            f"sha256sum {auth} {password} "
            f"{pki_directory}/authority/ca-key.pem "
            f"{pki_directory}/authority/ca.pem "
            f"/var/lib/dev-workspaces/public/ca.pem"
        )
        assert after == before
        machine.succeed(
            f"umount {pair}; mv -T {auth_backup} {auth}"
        )
        reconcile_non_c_locale()

    def assert_current_mount_preflight_precedes_mutation():
        target = current_target()
        source = "/tmp/mounted-current"
        auth_backup = "/tmp/auth-current-mount.good"
        machine.succeed(
            f"cp -a {auth} {auth_backup}; "
            f"printf 'malformed\\n' > {auth}; "
            f"chown developer:nginx {auth}; chmod 0600 {auth}; "
            f"rm {tls_directory}/current; "
            f"printf '%s\\n' {shlex.quote(target)} > {source}; "
            f"install -o root -g root -m 0644 /dev/null {tls_directory}/current; "
            f"mount --bind {source} {tls_directory}/current"
        )
        before = machine.succeed(
            f"stat -c %u:%g:%a:%h {source} {auth}; "
            f"sha256sum {source} {auth} {password} "
            f"{pki_directory}/authority/ca-key.pem "
            f"{pki_directory}/authority/ca.pem "
            f"{public_ca_directory}/ca.pem; "
            f"LC_ALL=C ls -1 {tls_directory}/pairs"
        )
        machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
        after = machine.succeed(
            f"stat -c %u:%g:%a:%h {source} {auth}; "
            f"sha256sum {source} {auth} {password} "
            f"{pki_directory}/authority/ca-key.pem "
            f"{pki_directory}/authority/ca.pem "
            f"{public_ca_directory}/ca.pem; "
            f"LC_ALL=C ls -1 {tls_directory}/pairs"
        )
        assert after == before
        machine.succeed(
            f"umount {tls_directory}/current; "
            f"rm {tls_directory}/current {source}; "
            f"ln -s {shlex.quote(target)} {tls_directory}/current; "
            f"mv -T {auth_backup} {auth}"
        )
        reconcile_non_c_locale()

    def assert_compatible_leaf_retained():
        before_target = current_target()
        machine.succeed(
            replace_current_leaf([
                "basicConstraints=critical,CA:FALSE",
                "keyUsage=critical,digitalSignature",
                "extendedKeyUsage=serverAuth",
                "subjectAltName=DNS:workspace.example.test,DNS:*.workspace.example.test,DNS:legacy-workspace.example.test",
                "subjectKeyIdentifier=hash",
                "authorityKeyIdentifier=keyid,issuer",
            ])
        )
        reconcile_non_c_locale()
        assert current_target() == before_target
        assert_leaf_valid()

    def assert_current_target_replaced_once(target):
        machine.succeed(
            f"rm {tls_directory}/current; "
            f"ln -s {shlex.quote(target)} {tls_directory}/current"
        )
        reconcile_non_c_locale()
        repaired_target = current_target()
        assert repaired_target != target
        assert_leaf_valid()
        reconcile_non_c_locale()
        assert current_target() == repaired_target

    def assert_auth_valid():
        machine.succeed(f"test $(stat -c %U:%G:%a {auth}) = root:nginx:640")
        encoded = machine.succeed(f"base64 -w0 {auth}").strip()
        contents = base64.b64decode(encoded)
        assert re.fullmatch(
            rb"developer:\$2[aby]\$12\$[./A-Za-z0-9]{53}\n\n?",
            contents,
        )
        machine.succeed(
            "${pkgs.apacheHttpd}/bin/htpasswd -vi "
            f"{auth} developer < {password} >/dev/null"
        )

    def assert_repaired_once(command):
        machine.succeed(command)
        broken_hash = machine.succeed(f"sha256sum {auth}")
        reconcile_non_c_locale()
        repaired_hash = machine.succeed(f"sha256sum {auth}")
        assert repaired_hash != broken_hash
        assert_auth_valid()
        reconcile_non_c_locale()
        assert machine.succeed(f"sha256sum {auth}") == repaired_hash

    assert_auth_valid()
    assert_password_valid()
    initial_hashes = hashes()
    initial_target = current_target()
    reconcile_non_c_locale()
    reconcile_non_c_locale()
    reconciled_hashes = hashes()
    assert reconciled_hashes == initial_hashes, (
        f"initial hashes:\n{initial_hashes}reconciled hashes:\n{reconciled_hashes}"
    )
    assert current_target() == initial_target
    assert_auth_valid()
    machine.succeed(
        "test $(stat -c %U:%G:%a /run/lock/dev-workspace-substrate.lock) "
        "= root:root:600"
    )

    machine.succeed(
        f"rm {lock}; printf 'lock-target\n' > /tmp/lock-target; "
        f"ln -s /tmp/lock-target {lock}"
    )
    machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed("test $(cat /tmp/lock-target) = lock-target")
    machine.succeed(f"rm {lock}; LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed(f"rm {lock}; mkdir {lock}")
    machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed(f"rmdir {lock}; LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed(f"chmod 0644 {lock}")
    machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed(f"chmod 0600 {lock}; LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed(f"chown developer:root {lock}")
    machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed(f"chown root:root {lock}; LC_ALL=en_US.UTF-8 {reconcile}")
    machine.succeed(f"printf 'lock-sentinel\n' > {lock}; chmod 0600 {lock}")
    reconcile_non_c_locale()
    machine.succeed(f"test $(cat {lock}) = lock-sentinel")

    assert_directory_symlink_rejected(password_directory, "password")
    assert_directory_symlink_rejected(auth_directory, "auth")
    assert_directory_symlink_rejected(pki_directory, "pki")
    assert_directory_symlink_rejected(tls_directory, "tls")
    assert_directory_symlink_rejected(public_ca_directory, "public-ca")

    assert_directory_bind_rejected(password_directory, auth_directory)
    assert_directory_bind_rejected(password_directory, f"{tls_directory}/pairs")
    assert_directory_bind_rejected(password_directory, "/run/dev-workspaces")
    assert_missing_directory_alias_rejected()
    assert_ancestor_reentry_rejected()

    assert_password_rejected(
        f"value=$(tr -d '\\n' < {password}); printf '\\n%s' \"$value\" > {password}; "
        f"chown root:workspace-portal-owner {password}; chmod 0640 {password}"
    )
    assert_password_rejected(
        f"printf '\\n' >> {password}; chown root:workspace-portal-owner {password}; "
        f"chmod 0640 {password}"
    )
    assert_password_rejected(
        f"value=$(tr -d '\\n' < {password}); printf '%s\\0\\n' \"$value\" > {password}; "
        f"chown root:workspace-portal-owner {password}; chmod 0640 {password}"
    )
    assert_password_rejected(
        f"truncate -s -1 {password}; chown root:workspace-portal-owner {password}; "
        f"chmod 0640 {password}"
    )

    assert_ca_rejected(
        f"cat /tmp/ca.pem.good >> "
        f"{pki_directory}/authority/ca.pem"
    )
    assert_ca_rejected(
        f"cat {pki_directory}/authority/ca-key.pem >> "
        f"{pki_directory}/authority/ca.pem"
    )
    assert_ca_alias_rejected("hardlink")
    assert_ca_alias_rejected("bind")
    assert_ca_metadata_rejected_before_auth_mutation()

    assert_selected_pair_preflight_precedes_mutation()
    assert_current_mount_preflight_precedes_mutation()

    assert_repaired_once(
        f"printf 'malformed\\n' > {auth}; chown root:nginx {auth}; chmod 0640 {auth}"
    )
    assert_repaired_once(f"chmod 0600 {auth}")
    assert_repaired_once(f"chown developer:nginx {auth}")
    assert_repaired_once(
        "printf 'wrong-secret\\n' | ${pkgs.apacheHttpd}/bin/htpasswd "
        f"-niBC 12 developer > {auth}; chown root:nginx {auth}; chmod 0640 {auth}"
    )
    assert_repaired_once(
        rf"""entry=$(sed -n '1p' {auth}); printf '%s\0\n' "$entry" > {auth}; chown root:nginx {auth}; chmod 0640 {auth}"""
    )
    assert_repaired_once(
        rf"""hash=$(sed -n '1s/^[^:]*://p' {auth}); printf 'developer:\0%s\n' "$hash" > {auth}; chown root:nginx {auth}; chmod 0640 {auth}"""
    )
    assert_repaired_once(
        rf"""entry=$(sed -n '1p' {auth}); printf '%s\n%s\n' "$entry" "$entry" > {auth}; chown root:nginx {auth}; chmod 0640 {auth}"""
    )
    assert_repaired_once(
        rf"""entry=$(sed -n '1p' {auth}); printf '%s\n\n\n' "$entry" > {auth}; chown root:nginx {auth}; chmod 0640 {auth}"""
    )

    assert hashes().splitlines()[0] == initial_hashes.splitlines()[0]
    assert hashes().splitlines()[2:] == initial_hashes.splitlines()[2:]
    assert current_target() == initial_target

    current_pair = f"{tls_directory}/{current_target()}"
    assert_compatible_leaf_retained()
    assert_foreign_trust_does_not_retain_leaf()
    valid_target = current_target()
    assert_current_target_replaced_once(f"{valid_target}\n")
    assert_current_target_replaced_once("pairs/.")
    assert_current_target_replaced_once("pairs/..")

    assert_leaf_replaced_once(
        replace_current_leaf([
            "basicConstraints=critical,CA:FALSE",
            "keyUsage=critical,digitalSignature",
            "extendedKeyUsage=clientAuth",
            "subjectAltName=DNS:workspace.example.test,DNS:*.workspace.example.test,DNS:legacy-workspace.example.test",
            "subjectKeyIdentifier=hash",
            "authorityKeyIdentifier=keyid,issuer",
        ])
    )
    assert_leaf_replaced_once(
        replace_current_leaf([
            "basicConstraints=critical,CA:TRUE,pathlen:0",
            "keyUsage=critical,keyCertSign,cRLSign",
            "extendedKeyUsage=serverAuth",
            "subjectAltName=DNS:workspace.example.test,DNS:*.workspace.example.test,DNS:legacy-workspace.example.test",
            "subjectKeyIdentifier=hash",
            "authorityKeyIdentifier=keyid,issuer",
        ])
    )

    assert_leaf_replaced_once(
        f"pair=$(readlink {tls_directory}/current); chmod 0644 "
        f"{tls_directory}/$pair/server-key.pem"
    )
    assert_leaf_replaced_once(
        f"pair=$(readlink {tls_directory}/current); chown developer:nginx "
        f"{tls_directory}/$pair/server-key.pem"
    )
    assert_leaf_replaced_once(
        f"pair=$(readlink {tls_directory}/current); rm {tls_directory}/$pair/server.pem; "
        f"ln -s /etc/passwd {tls_directory}/$pair/server.pem"
    )
    assert_leaf_replaced_once(
        f"pair=$(readlink {tls_directory}/current); rm {tls_directory}/$pair/server-key.pem"
    )
    assert_leaf_replaced_once(
        f"pair=$(readlink {tls_directory}/current); printf 'not-a-key\\n' > "
        f"{tls_directory}/$pair/server-key.pem; chown root:nginx "
        f"{tls_directory}/$pair/server-key.pem; chmod 0640 "
        f"{tls_directory}/$pair/server-key.pem"
    )
    assert_leaf_replaced_once(
        f"pair=$(readlink {tls_directory}/current); "
        "${pkgs.openssl}/bin/openssl genpkey -algorithm EC "
        "-pkeyopt ec_paramgen_curve:P-256 "
        f"-out {tls_directory}/$pair/server-key.pem; chown root:nginx "
        f"{tls_directory}/$pair/server-key.pem; chmod 0640 "
        f"{tls_directory}/$pair/server-key.pem"
    )
    selected_target = current_target()
    selected_hashes = hashes()
    machine.succeed(
        f"mv {tls_directory}/{selected_target} /tmp/symlinked-leaf-pair; "
        f"ln -s /tmp/symlinked-leaf-pair {tls_directory}/{selected_target}"
    )
    machine.fail(f"LC_ALL=en_US.UTF-8 {reconcile}")
    assert current_target() == selected_target
    assert hashes() == selected_hashes
    machine.succeed(
        f"rm {tls_directory}/{selected_target}; "
        f"mv /tmp/symlinked-leaf-pair {tls_directory}/{selected_target}"
    )
    reconcile_non_c_locale()
    assert current_target() == selected_target
    assert hashes() == selected_hashes
  '';
}
