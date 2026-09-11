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
        assert machine.succeed(f'{curl} --user "developer:$(cat {password})" {endpoint}').strip() == "ready"

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
  '';
}
