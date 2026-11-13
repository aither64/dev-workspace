{ pkgs, self }:
let
  capability = builtins.toJSON {
    schema = 1;
    host_launcher_contract = 1;
    launch_request_schema = 1;
    handshake_schema = 1;
    supervisor_entrypoint = "bin/workspace-portal";
    supervisor_command = [ "pi-runtime" "supervise" ];
  };
  fixture = pkgs.runCommand "pi-host-fixture-application" { } ''
    mkdir -p "$out/bin" "$out/share/dev-workspace"
    cp ${pkgs.writeText "pi-runtime.json" capability} "$out/share/dev-workspace/pi-runtime.json"
    cat > "$out/bin/workspace-portal" <<'PY'
    #!${pkgs.python3}/bin/python3
    import argparse, fcntl, hashlib, json, os, pathlib, signal, subprocess, sys, time
    assert sys.argv[1:3] == ["pi-runtime", "supervise"]
    parser = argparse.ArgumentParser()
    for name in ["launch-request", "instance", "request", "digest"]:
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args(sys.argv[3:])
    launch = pathlib.Path(args.launch_request)
    assert hashlib.sha256(launch.read_bytes()).hexdigest() == args.digest
    data = json.loads(launch.read_bytes())
    assert data["instance_id"] == args.instance and data["request_id"] == args.request
    auth_root = pathlib.Path("/home/developer/fake-auth")
    with (auth_root / "auth.lock").open("a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        lock.write(args.instance + "\n")
        lock.flush()
        auth_identity = [auth_root.stat().st_dev, auth_root.stat().st_ino,
                         os.fstat(lock.fileno()).st_dev, os.fstat(lock.fileno()).st_ino]
    children = [subprocess.Popen([sys.executable, "-c", "import time; time.sleep(600)"])
                for role in ["lead", "member", "tool", "service"]]
    pids = [os.getpid()] + [child.pid for child in children]
    status = pathlib.Path("/proc/self/status").read_text().splitlines()
    sudo = subprocess.run(["/run/wrappers/bin/sudo", "-n", "true"], capture_output=True, text=True)
    result = {
        "uid": os.getuid(), "gid": os.getgid(), "groups": os.getgroups(),
        "pids": pids, "mounts": [os.readlink(f"/proc/{pid}/ns/mnt") for pid in pids],
        "users": [os.readlink(f"/proc/{pid}/ns/user") for pid in pids],
        "cgroups": [pathlib.Path(f"/proc/{pid}/cgroup").read_text() for pid in pids],
        "capabilities": [line for line in status if line.startswith(("CapEff:", "CapAmb:"))],
        "sudo_exit": sudo.returncode, "sudo_error": sudo.stderr,
        "invocation_id": os.environ.get("INVOCATION_ID"), "digest": args.digest,
        "scratch_before": pathlib.Path("/tmp/retained").exists(),
        "auth": pathlib.Path("/home/developer/fake-auth/auth.json").read_text(),
        "auth_identity": auth_identity,
    }
    pathlib.Path("/tmp/retained").write_text(args.instance)
    pathlib.Path("/var/tmp/retained").write_text(args.instance)
    (launch.parent / "observed.json").write_text(json.dumps(result))
    while True:
        (launch.parent / "auth-latest.json").write_text((auth_root / "auth.json").read_text())
        time.sleep(0.1)
    PY
    chmod 0555 "$out/bin/workspace-portal"
  '';
in
pkgs.testers.runNixOSTest {
  name = "dev-workspace-pi-runtime-host";
  nodes.machine = {
    imports = [ self.nixosModules.host ];
    users.groups.fixture-access = { };
    users.users.developer = {
      isNormalUser = true;
      uid = 1000;
      extraGroups = [ "fixture-access" "wheel" ];
    };
    services.dev-workspaces = {
      enable = true;
      owner = "developer";
      hostName = "workspace.example.test";
      piRuntime.enable = true;
    };
    environment.systemPackages = [ pkgs.python3 fixture ];
    system.stateVersion = "26.05";
  };
  testScript = ''
    import hashlib, json, shlex, uuid

    machine.start()
    machine.wait_for_unit("multi-user.target")
    root = "/home/developer/.local/state/dev-workspaces/pi-runtime-host"
    machine.succeed("install -d -o developer -g users /home/developer/fake-auth")
    machine.succeed("printf '{\"fake\":1}' > /home/developer/fake-auth/auth.json; chown developer:users /home/developer/fake-auth/auth.json")
    boot = machine.succeed("cat /proc/sys/kernel/random/boot_id").strip()
    host_userns = machine.succeed("readlink /proc/1/ns/user").strip()
    uid = int(machine.succeed("id -u developer").strip())
    gid = int(machine.succeed("id -g developer").strip())
    groups = sorted(map(int, machine.succeed("id -G developer").split()))
    helper = machine.succeed("readlink -f /run/current-system/sw/bin/workspace-pi-host").strip()

    def invoke(verb, instance, request, digest, fence=""):
        command = f"sudo -u developer sudo -n {helper} {verb} --instance {instance} --request {request} --digest {digest} {fence}"
        status, output = machine.execute(command)
        return status, json.loads(output)

    def publish(instance, request, generation, predecessor=None):
        directory = f"{root}/instances/{instance}"
        data = {
            "schema": 1, "host_launcher_contract": 1,
            "scope": {"workspace": "/home/developer", "slug": "2026-11-12-fixture",
                      "session_incarnation": instance.replace("-", "") * 2,
                      "backend": "pi", "backend_contract": 1, "binding_revision": 1},
            "instance_id": instance, "request_id": request, "generation": generation,
            "app_store_path": "${fixture}", "profile_path": "/home/developer/profile",
            "profile_generation": "fixture-1",
        }
        if predecessor:
            data["replaces_request_id"] = predecessor
        payload = json.dumps(data) + "\n"
        machine.succeed(f"install -d -o developer -g users {root} {root}/instances {directory} {directory}/tmp {directory}/var-tmp {directory}/receipts")
        machine.succeed(f"touch {directory}/launch.lock; chown developer:users {directory}/launch.lock")
        machine.succeed(f"ln -sfn ${fixture} {directory}/package-root")
        machine.succeed(f"printf %s {shlex.quote(payload)} > {directory}/launch.json; chown developer:users {directory}/launch.json")
        return hashlib.sha256(payload.encode()).hexdigest()

    instances = [str(uuid.uuid4()), str(uuid.uuid4())]
    requests = [str(uuid.uuid4()), str(uuid.uuid4())]
    digests = [publish(i, r, "a" * 64) for i, r in zip(instances, requests)]
    reports = []
    for instance, request, digest in zip(instances, requests, digests):
        status, reply = invoke("start", instance, request, digest)
        assert status == 0 and reply["outcome"] in ["starting", "running", "unknown"], reply
        machine.wait_until_succeeds(f"test -f {root}/instances/{instance}/observed.json")
        report = json.loads(machine.succeed(f"cat {root}/instances/{instance}/observed.json"))
        reports.append(report)
        assert report["uid"] == uid and report["gid"] == gid
        assert sorted(report["groups"]) == groups
        assert len(set(report["mounts"])) == 1 and len(set(report["cgroups"])) == 1
        assert set(report["users"]) == {host_userns}
        assert all(int(line.split()[1], 16) == 0 for line in report["capabilities"])
        assert report["sudo_exit"] != 0 and "password" in report["sudo_error"].lower(), report
        assert report["auth"] == '{"fake":1}' and not report["scratch_before"]
        assert report["invocation_id"] and report["digest"] == digest
        status, reconciled = invoke("start", instance, request, digest)
        assert status == 0 and reconciled["outcome"] == "running", reconciled
    assert reports[0]["mounts"][0] != reports[1]["mounts"][0]
    assert reports[0]["cgroups"][0] != reports[1]["cgroups"][0]
    assert reports[0]["auth_identity"] == reports[1]["auth_identity"]
    machine.succeed("printf '{\"fake\":2}' > /home/developer/fake-auth/next.json; chown developer:users /home/developer/fake-auth/next.json; mv /home/developer/fake-auth/next.json /home/developer/fake-auth/auth.json")
    for instance in instances:
        machine.wait_until_succeeds(f"grep -q '2' {root}/instances/{instance}/auth-latest.json")

    instance, request, digest = instances[0], requests[0], digests[0]
    invocation = reports[0]["invocation_id"]
    status, rejected = invoke("stop", instance, request, digest, f"--boot-id {boot} --invocation-id {'f' * 32}")
    assert status != 0 and rejected["error_code"] == "stop_fence_mismatch"
    machine.succeed(f"test -e /proc/{reports[0]['pids'][0]}")
    for i, r, d, report in zip(instances, requests, digests, reports):
        fence = f"--boot-id {boot} --invocation-id {report['invocation_id']}"
        status, reply = invoke("stop", i, r, d, fence)
        assert status == 0, reply
        machine.wait_until_succeeds(f"test $(systemctl show -p ActiveState --value workspace-pi-session@{i}.service) = inactive")
        status, terminal = invoke("start", i, r, d)
        assert status == 0 and terminal["outcome"] == "stopped", terminal
        cgroup = terminal["control_group"]
        machine.succeed(f"test ! -d /sys/fs/cgroup{cgroup} || grep -qx 'populated 0' /sys/fs/cgroup{cgroup}/cgroup.events")
        for pid in report["pids"]:
            machine.succeed(f"test ! -e /proc/{pid}")
        machine.succeed(f"test -f {root}/instances/{i}/tmp/retained; test -f {root}/instances/{i}/var-tmp/retained")

    successor = str(uuid.uuid4())
    new_digest = publish(instance, successor, "b" * 64, request)
    machine.succeed(f"rm {root}/instances/{instance}/observed.json")
    status, reply = invoke("start", instance, successor, new_digest)
    assert status == 0, reply
    machine.wait_until_succeeds(f"test -f {root}/instances/{instance}/observed.json")
    report = json.loads(machine.succeed(f"cat {root}/instances/{instance}/observed.json"))
    assert report["scratch_before"] and report["invocation_id"] != invocation
    status, old = invoke("stop", instance, request, digest, f"--boot-id {boot} --invocation-id {invocation}")
    assert status == 0 and old["outcome"] == "stopped"
    machine.succeed(f"test -e /proc/{report['pids'][0]}")
    status, reply = invoke("stop", instance, successor, new_digest,
                           f"--boot-id {boot} --invocation-id {report['invocation_id']}")
    assert status == 0, reply
    machine.wait_until_succeeds(f"test ! -e /proc/{report['pids'][0]}")
  '';
}
