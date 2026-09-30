{ pkgs, self }:
pkgs.testers.runNixOSTest {
  name = "dev-workspace-host-module-idempotency";
  nodes.machine =
    { lib, ... }:
    {
      imports = [ self.nixosModules.host ];
      users.users.developer.isNormalUser = true;
      i18n.defaultLocale = "en_US.UTF-8";
      services.dev-workspaces = {
        enable = true;
        owner = "developer";
        hostName = "workspace.example.test";
        wildcardHost = "*.workspace.example.test";
        aliases = [ "legacy-workspace.example.test" ];
      };
      specialisation.extraAlias.configuration.services.dev-workspaces.aliases = lib.mkForce [
        "legacy-workspace.example.test"
        "extra-workspace.example.test"
      ];
      specialisation.cost5.configuration.services.dev-workspaces.auth.bcryptCost = lib.mkForce 5;
      systemd.services.fixture-router = {
        wantedBy = [ "multi-user.target" ];
        serviceConfig = {
          User = "developer";
          Group = "workspace-portal-proxy";
          ExecStart = "${pkgs.python3}/bin/python3 ${pkgs.writeText "fixture-router.py" ''
            import os
            import socket
            path = "/run/dev-workspaces/router.sock"
            if os.path.exists(path):
                os.unlink(path)
            server = socket.socket(socket.AF_UNIX)
            server.bind(path)
            os.chmod(path, 0o660)
            server.listen()
            while True:
                connection, _ = server.accept()
                with connection:
                    connection.recv(65536)
                    connection.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\nready\n")
          ''}";
        };
      };
      environment.systemPackages = [
        pkgs.apacheHttpd
        pkgs.curl
        pkgs.openssl
      ];
      system.stateVersion = "26.05";
    };

  testScript = ''
    import re
    import shlex

    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("nginx.service")
    machine.wait_for_unit("fixture-router.service")
    machine.wait_until_succeeds("test -S /run/dev-workspaces/router.sock")
    reconcile = "/run/current-system/sw/bin/workspace-portal-substrate-reconcile"
    password = "/var/lib/dev-workspaces/password/password"
    auth = "/var/lib/dev-workspaces/auth/htpasswd"
    authority = "/var/lib/dev-workspaces/pki/authority"
    tls = "/var/lib/dev-workspaces/tls"
    public_ca = "/var/lib/dev-workspaces/public/ca.pem"
    preserved = f"{password} {auth} {authority}/ca-key.pem {authority}/ca.pem {public_ca}"
    curl = f"curl -sS --cacert {public_ca} --resolve workspace.example.test:443:127.0.0.1"
    endpoint = "https://workspace.example.test/"

    def hashes(paths):
        return machine.succeed(f"sha256sum {paths}")

    def current_target():
        return machine.succeed(f"readlink {tls}/current").strip()

    def reconcile_once():
        machine.succeed(f"LC_ALL=en_US.UTF-8 {reconcile}")

    def assert_endpoint():
        assert machine.succeed(f"{curl} -o /dev/null -w '%{{http_code}}' {endpoint}").strip() == "401"
        assert machine.succeed(
            f"{curl} --user developer:incorrect -o /dev/null -w '%{{http_code}}' {endpoint}"
        ).strip() == "401"
        assert machine.succeed(f'{curl} --user "developer:$(cat {password})" {endpoint}').strip() == "ready"

    def assert_auth_cost(cost):
        machine.succeed(
            "grep -Eq '^developer:\\$2[aby]\\$" + cost +
            f"\\$[./A-Za-z0-9]{{53}}$' {auth}; "
            f"test $(grep -c . {auth}) -eq 1; "
            f"test $(stat -c %U:%G:%a {auth}) = root:nginx:640; "
            f"htpasswd -vi {auth} developer < {password} >/dev/null 2>&1"
        )

    def auth_state():
        return machine.succeed(f"sha256sum {auth}; stat -c %i {auth}")

    def assert_leaf_valid():
        assert re.fullmatch(r"pairs/pair-[0-9]+-[0-9]+", current_target())
        machine.succeed(
            f"openssl verify -purpose sslserver -verify_hostname workspace.example.test "
            f"-no-CApath -no-CAstore -CAfile {public_ca} {tls}/current/server.pem; "
            f"openssl x509 -checkend 2592000 -noout -in {tls}/current/server.pem"
        )

    with subtest("activation and a stable second reconciliation"):
        initial_system = machine.succeed("readlink -f /run/current-system").strip()
        initial_preserved = hashes(preserved)
        initial_leaf = hashes(f"{tls}/current/server.pem {tls}/current/server-key.pem")
        initial_target = current_target()
        machine.succeed(
            f"test $(stat -c %U:%G:%a {password}) = root:workspace-portal-owner:640; "
            f"test $(stat -c %U:%G:%a {auth}) = root:nginx:640; "
            f"test $(stat -c %U:%G:%a {authority}/ca-key.pem) = root:root:600; "
            f"test $(stat -c %U:%G:%a {tls}/current/server-key.pem) = root:nginx:640"
        )
        assert_endpoint()
        assert_auth_cost("12")
        reconcile_once()
        assert hashes(preserved) == initial_preserved
        assert hashes(f"{tls}/current/server.pem {tls}/current/server-key.pem") == initial_leaf
        assert current_target() == initial_target
        assert_leaf_valid()

    with subtest("malformed persistent state is preserved for repair"):
        for path in [password, f"{authority}/ca.pem"]:
            machine.succeed(f"cp -a {path} /tmp/original; printf 'incomplete\\n' > {path}")
            broken = hashes(path)
            machine.fail(reconcile)
            assert hashes(path) == broken
            assert current_target() == initial_target
            machine.succeed(f"mv -T /tmp/original {path}")
        machine.succeed(f"mv {authority} /tmp/authority; ln -s /tmp/authority {authority}")
        machine.fail(reconcile)
        machine.succeed(f"rm {authority}; mv /tmp/authority {authority}")
        reconcile_once()
        assert hashes(preserved) == initial_preserved

    with subtest("recover an interrupted leaf publication and retain legacy state"):
        machine.succeed(
            f"mkdir -p {tls}/.pair.interrupted /var/lib/dev-workspaces/pki/leaves; "
            f"printf 'unfinished\\n' > {tls}/.pair.interrupted/server-key.pem; "
            "printf 'retained legacy state\\n' > /var/lib/dev-workspaces/pki/leaves/retained; "
            f"ln -s pairs/pair-1-1 {tls}/.current.interrupted"
        )
        reconcile_once()
        assert current_target() == initial_target
        machine.succeed("test -f /var/lib/dev-workspaces/pki/leaves/retained")

    with subtest("renew an expiring leaf through the system service"):
        machine.succeed(
            f"openssl x509 -x509toreq -in {tls}/current/server.pem "
            f"-signkey {tls}/current/server-key.pem -copy_extensions copy -out /tmp/leaf.csr; "
            f"openssl x509 -req -in /tmp/leaf.csr -CA {authority}/ca.pem "
            f"-CAkey {authority}/ca-key.pem -set_serial 123 -days 1 -copy_extensions copy "
            f"-out {tls}/current/server.pem; "
            "systemctl start workspace-portal-certificate-renewal.service"
        )
        renewed_target = current_target()
        assert renewed_target != initial_target
        assert hashes(preserved) == initial_preserved
        assert_leaf_valid()
        assert_endpoint()
        machine.succeed("systemctl is-active workspace-portal-certificate-renewal.timer")
        reconcile_once()
        assert current_target() == renewed_target

    with subtest("replace a mismatched leaf key without changing the CA or password"):
        machine.succeed(
            "openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 "
            f"-out {tls}/current/server-key.pem"
        )
        reconcile_once()
        assert current_target() != renewed_target
        assert hashes(preserved) == initial_preserved
        assert_leaf_valid()

    with subtest("serialize concurrent activation and renewal"):
        machine.succeed(
            "status=0; flock /run/lock/dev-workspace-substrate.lock "
            f"timeout 1 {reconcile} || status=$?; test $status -eq 124"
        )
        assert hashes(preserved) == initial_preserved
        concurrent = reconcile + ' & first=$!; ' + reconcile + '; wait "$first"'
        machine.succeed("bash -e -c " + shlex.quote(concurrent))
        assert hashes(preserved) == initial_preserved

    with subtest("configuration switch and rollback preserve credentials and old pairs"):
        before_switch = current_target()
        machine.succeed(f"{initial_system}/specialisation/extraAlias/bin/switch-to-configuration test")
        assert current_target() != before_switch
        machine.succeed(
            f"openssl x509 -in {tls}/current/server.pem -noout -checkhost extra-workspace.example.test"
        )
        assert hashes(preserved) == initial_preserved
        assert_endpoint()
        machine.succeed(f"{initial_system}/bin/switch-to-configuration test")
        assert hashes(preserved) == initial_preserved
        machine.succeed(f"test -f {tls}/{before_switch}/server.pem")
        assert "extra-workspace.example.test" not in machine.succeed(
            f"openssl x509 -in {tls}/current/server.pem -noout -ext subjectAltName"
        )
        assert_leaf_valid()
        assert_endpoint()
        after_rollback = current_target()
        reconcile_once()
        assert current_target() == after_rollback

    with subtest("cost-only system switches regenerate once without changing credentials or TLS"):
        stable_outputs = hashes(
            f"{password} {authority}/ca-key.pem {authority}/ca.pem {public_ca} "
            f"{tls}/current/server.pem {tls}/current/server-key.pem"
        )
        stable_target = current_target()
        password_inode = machine.succeed(f"stat -c %i {password}").strip()
        assert_auth_cost("12")
        before_cost5 = auth_state()
        machine.succeed(f"{initial_system}/specialisation/cost5/bin/switch-to-configuration test")
        assert_auth_cost("05")
        assert_endpoint()
        assert auth_state() != before_cost5
        assert hashes(
            f"{password} {authority}/ca-key.pem {authority}/ca.pem {public_ca} "
            f"{tls}/current/server.pem {tls}/current/server-key.pem"
        ) == stable_outputs
        assert current_target() == stable_target
        assert machine.succeed(f"stat -c %i {password}").strip() == password_inode
        at_cost5 = auth_state()
        reconcile_once()
        assert auth_state() == at_cost5

        machine.succeed(f"{initial_system}/bin/switch-to-configuration test")
        assert_auth_cost("12")
        assert_endpoint()
        at_cost12 = auth_state()
        assert at_cost12 != at_cost5
        reconcile_once()
        assert auth_state() == at_cost12

        machine.succeed(f"{initial_system}/specialisation/cost5/bin/switch-to-configuration test")
        assert_auth_cost("05")
        assert_endpoint()
        at_cost5_again = auth_state()
        assert at_cost5_again != at_cost12
        reconcile_once()
        assert auth_state() == at_cost5_again
        assert hashes(
            f"{password} {authority}/ca-key.pem {authority}/ca.pem {public_ca} "
            f"{tls}/current/server.pem {tls}/current/server-key.pem"
        ) == stable_outputs
        assert current_target() == stable_target
        assert machine.succeed(f"stat -c %i {password}").strip() == password_inode

    with subtest("wrong or malformed current-cost hashes are replaced through the same password"):
        for variant in ["2a", "2b", "2y"]:
            machine.succeed(
                "sed 's/\\$2[aby]\\$/\\$" + variant +
                f"\\$/' {auth} > /tmp/variant-auth; "
                "chown root:nginx /tmp/variant-auth; chmod 0640 /tmp/variant-auth"
            )
            supported, _ = machine.execute(
                f"htpasswd -vi /tmp/variant-auth developer < {password} >/dev/null 2>&1"
            )
            if supported == 0:
                machine.succeed(f"mv -T /tmp/variant-auth {auth}")
                accepted = auth_state()
                reconcile_once()
                assert_auth_cost("05")
                assert auth_state() == accepted
            else:
                machine.succeed("rm /tmp/variant-auth")
        for mutation in [
            f"sed 's/^developer:/other:/' {auth} > /tmp/changed-auth",
            f"sed 's/\\$2[aby]\\$/\\$5\\$/' {auth} > /tmp/changed-auth",
            f"cp {auth} /tmp/changed-auth; printf 'other:invalid\\n' >> /tmp/changed-auth",
            f"htpasswd -niBC 04 developer < {password} > /tmp/changed-auth",
            f"htpasswd -niBC 12 developer < {password} > /tmp/changed-auth",
            "printf '%s\\n' '0000000000000000000000000000000000000000000000000000000000000000' "
            "| htpasswd -niBC 05 developer > /tmp/changed-auth",
        ]:
            machine.succeed(mutation)
            machine.succeed(
                f"chown root:nginx /tmp/changed-auth; chmod 0640 /tmp/changed-auth; "
                f"mv -T /tmp/changed-auth {auth}"
            )
            reconcile_once()
            assert_auth_cost("05")
            assert_endpoint()
        machine.succeed(f"cp -a {auth} /tmp/original-auth; chmod 0644 {auth}")
        reconcile_once()
        assert_auth_cost("05")
        machine.succeed(f"mv {auth} /tmp/replaced-auth; ln -s /tmp/replaced-auth {auth}")
        machine.fail(reconcile)
        machine.succeed(f"rm {auth}; mv /tmp/replaced-auth {auth}")
        reconcile_once()
        assert_auth_cost("05")

    with subtest("failed generation preserves the old complete auth file"):
        machine.succeed(f"htpasswd -niBC 12 developer < {password} > /tmp/changed-auth")
        machine.succeed(
            f"chown root:nginx /tmp/changed-auth; chmod 0640 /tmp/changed-auth; "
            f"mv -T /tmp/changed-auth {auth}"
        )
        before_failure = auth_state()
        machine.fail(f"bash -c 'ulimit -f 0; {reconcile}'")
        assert auth_state() == before_failure
        assert_auth_cost("12")
        reconcile_once()
        assert_auth_cost("05")

    with subtest("concurrent readers see complete old or new auth files"):
        machine.succeed(f"htpasswd -niBC 12 developer < {password} > /tmp/changed-auth")
        machine.succeed(
            f"chown root:nginx /tmp/changed-auth; chmod 0640 /tmp/changed-auth; "
            f"mv -T /tmp/changed-auth {auth}"
        )
        reader = (
            "stop=/tmp/auth-reader-stop; ready=/tmp/auth-reader-ready; "
            "rm -f $stop $ready; trap 'touch $stop' EXIT; ("
            "touch $ready; "
            "while test ! -e $stop; do "
            f"grep -Eq '^developer:\\$2[aby]\\$(05|12)\\$[./A-Za-z0-9]{{53}}$' {auth} "
            "|| exit 1; "
            "done) & reader=$!; "
            "while test ! -e $ready; do sleep 0.01; done; "
            f"{reconcile}; touch $stop; wait $reader; rm $stop $ready"
        )
        machine.succeed("bash -e -c " + shlex.quote(reader))
        assert_auth_cost("05")
        assert_endpoint()
  '';
}
