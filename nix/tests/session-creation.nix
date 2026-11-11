{
  pkgs,
  self,
  mkPackage,
  focusedTui ? false,
  maintenanceAdmissionOnly ? false,
}:
let
  pythonBlock = text: builtins.replaceStrings [ "\n" ] [ "\n    " ] ("    " + text);
  policy = {
    default = "xhigh";
    simple = "high";
    allowed = [
      "high"
      "xhigh"
    ];
    simple_requires_reason = true;
    followup = "retain";
  };
  lifecycle = {
    startup = "on_demand";
    communication = "lead_mediated";
    reviewer_reuse = "same_change";
  };
  lead = {
    model = "gpt-5.5";
    effort = "xhigh";
    behavior = "team_lead";
    purpose = "lead";
    instructions = "CREATION_LEAD_INSTRUCTIONS: return the fixture reply; do not run tools.";
    lifetime = "session";
    allowed_efforts = [
      "high"
      "xhigh"
    ];
    access = "workspace_write";
    fresh_context = false;
  };
  package = mkPackage {
    inherit pkgs;
    teamConfig = {
      schema_version = 6;
      execution_access = "full_access";
      default_team = "solo";
      default_development_team = "development";
      capacity.required_native_child_threads = 4;
      work_policy = {
        design = policy;
        implementation = policy;
      };
      teams = {
        solo = {
          description = "Creation acceptance solo";
          mode = "solo";
          design_owner = "team_lead";
          service_policy = "unmanaged";
          max_open_agents = 0;
          inherit lifecycle;
          routing = { };
          roles.team_lead = lead;
        };
        development = {
          description = "Creation acceptance retained team";
          mode = "development";
          design_owner = "team_lead";
          service_policy = "non_priority";
          max_open_agents = 2;
          initial_member_counts.reviewer = 2;
          inherit lifecycle;
          routing = {
            design_simple_effort = "high";
            implementer_simple_effort = "high";
            low_risk_review_role = "reviewer";
          };
          roles = {
            team_lead = lead;
            reviewer = lead // {
              effort = "high";
              behavior = "reviewer";
              purpose = "review";
              instructions = "CREATION_MEMBER_INSTRUCTIONS: inspect only when assigned.";
              lifetime = "review_cycle";
              access = "read_only";
              fresh_context = true;
            };
          };
        };
      };
      utilities.verification_watcher = {
        model = "gpt-5.5";
        effort = "low";
        behavior = "verification_watcher";
        lifetime = "operation";
        access = "workspace_write";
        startup = "on_demand";
        max_concurrent = 1;
        required_for = [
          "long_check"
          "ci_wait"
        ];
      };
    };
  };
  maintenanceTarget = package.override (previous: {
    codex = previous.codex.overrideAttrs (old: {
      name = old.name + "-maintenance-fixture";
    });
  });
  python = pkgs.python3.withPackages (p: [
    p.websockets
    p.pyyaml
  ]);
in
assert !(focusedTui && maintenanceAdmissionOnly);
pkgs.testers.runNixOSTest {
  name = "dev-workspace-session-creation";
  # Full serial coverage needs a separate outer budget; local limits stay unchanged.
  globalTimeout = if focusedTui || maintenanceAdmissionOnly then 3600 else 7200;
  nodes.machine = {
    imports = [ self.nixosModules.host ];
    # Public package preparation uses the ordinary flake-enabled Nix CLI.
    nix.settings.experimental-features = [
      "nix-command"
      "flakes"
    ];
    users.users.developer = {
      isNormalUser = true;
      uid = 1000;
      linger = true;
    };
    services.dev-workspaces = {
      enable = true;
      owner = "developer";
      hostName = "workspace.example.test";
      wildcardHost = "*.workspace.example.test";
      codexInitialProvider = "primary";
      codexProviders =
        map
          (id: {
            inherit id;
            label = "Creation fixture " + id;
            baseURL = "http://127.0.0.1:2455/" + id + "/v1";
            tokenFile = "/home/developer/.local/state/dev-workspaces/codex-homes/fixture/" + id + ".token";
          })
          [
            "primary"
            "alternate"
          ];
    };
    services.nginx.commonHttpConfig = pkgs.lib.mkAfter ''
      log_format creation_timing escape=json
        '{"msec":"$msec","method":"$request_method","uri":"$uri","status":"$status",'
        '"request_time":"$request_time","protocol":"$server_protocol","connection":"$connection",'
        '"connection_requests":"$connection_requests","request_id":"$request_id",'
        '"upstream_status":"$upstream_status","upstream_connect_time":"$upstream_connect_time",'
        '"upstream_header_time":"$upstream_header_time","upstream_response_time":"$upstream_response_time",'
        '"x_portal_read":"$upstream_http_x_portal_read","server_timing":"$upstream_http_server_timing"}';
    '';
    services.nginx.virtualHosts."*.workspace.example.test".extraConfig = pkgs.lib.mkAfter ''
      access_log /var/log/nginx/creation-timing.jsonl creation_timing;
    '';
    networking.extraHosts = "127.0.0.1 fixture.workspace.example.test";
    # The production-policy helper refuses before writing /etc unless this
    # root-owned, disposable-guest marker and stopped fixture services agree.
    environment.etc."dev-workspaces/creation-policy-fixture".text =
      "session-creation-production-policy-v1\n";
    environment.systemPackages = [
      pkgs.git
      pkgs.curl
      pkgs.nodejs
      pkgs.chromium
      python
    ];
    virtualisation = {
      memorySize = 6144;
      diskSize = 4096;
      cores = 4;
      # Public preparation selects prebuilt immutable package installables,
      # without exporting their build-recipe graphs into the guest.
      additionalPaths = [
        package
        maintenanceTarget
        pkgs.playwright-driver
      ];
    };
    system.stateVersion = "26.05";
  };
  testScript = ''
    import hashlib
    import json
    import shlex
    import time
    import uuid

    machine.start()
    machine.wait_for_unit("user@1000.service")
    package = "${package}"
    maintenance_packages = {"original": package, "target": "${maintenanceTarget}"}
    root = "/home/developer/workspace"
    state = "/home/developer/.local/state/dev-workspaces"
    runtime = "/run/user/1000/dev-workspaces/fixture"
    portal_state = state + "/portal/workspace-" + hashlib.sha256(root.encode()).hexdigest()[:16]
    user_prefix = "runuser -u developer -- env XDG_RUNTIME_DIR=/run/user/1000 "

    def user(command):
        return machine.succeed(user_prefix + "sh -c " + shlex.quote(command))

    def sync_source(package_path, slug):
        # This fixture ceiling retries only the refusal before sync admission.
        command = user_prefix + "sh -c " + shlex.quote("cd " + root + " && " + shlex.join([
            package_path + "/bin/dev-session", "sync", slug, "--as-is"])) + " 2>&1"
        refusal = "error: another dev-session command is changing " + slug
        deadline = time.monotonic() + 20
        attempts = 0
        last = None
        while True:
            remaining = deadline - time.monotonic()
            # The driver takes integer seconds; zero would disable its timeout.
            timeout_seconds = int(remaining)
            if timeout_seconds < 1:
                raise AssertionError(("sync admission deadline", slug, attempts, last))
            attempts += 1
            try:
                status, output = machine.execute(command, timeout=timeout_seconds)
            except Exception as error:
                raise AssertionError(("sync execution failed", slug, attempts, last)) from error
            last = {"status": status, "output": output[-2048:] if isinstance(output, str) else repr(output)[:2048]}
            if time.monotonic() >= deadline:
                raise AssertionError(("sync admission deadline", slug, attempts, last))
            if type(status) is not int or not isinstance(output, str):
                raise AssertionError(("invalid sync result", slug, attempts, last))
            if status == 0:
                return output
            if status != 1 or output not in (refusal, refusal + "\n"):
                raise AssertionError(("sync failed", slug, attempts, last))
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AssertionError(("sync admission deadline", slug, attempts, last))
            time.sleep(min(0.25, remaining))

    def browser(action, name, **options):
        result = json.loads(user("PLAYWRIGHT_MODULE=${pkgs.playwright-driver} "
            "CHROMIUM_EXECUTABLE=${pkgs.chromium}/bin/chromium "
            "CREATION_RELOAD_READINESS=${../../test/creation_reload_readiness.cjs} "
            "${pkgs.nodejs}/bin/node ${../../test/creation_acceptance_browser.cjs} "
            + action + " " + name + " " + shlex.quote(json.dumps(options))))
        print(json.dumps(result))
        return result

    def checkpoint(name):
        return json.loads(user("cat /tmp/creation-" + name + ".json"))

    def status(name):
        request = checkpoint(name)["requestId"]
        return json.loads(user("curl -fsS --unix-socket " + runtime + "/portal.sock "
            + "http://localhost/api/session-creations/" + request))

    def restart():
        user("systemctl --user kill --signal=SIGKILL --kill-whom=all workspace-portal@fixture.service")
        user("systemctl --user restart workspace-portal@fixture.service")
        machine.wait_until_succeeds(user_prefix + "curl -fsS --unix-socket " + runtime
            + "/portal.sock http://localhost/ >/dev/null")

    def native_json(identity, timeout=None):
        # Retain each complete history through the driver's ordinary file interface.
        filename = "native-read-" + str(uuid.uuid4()) + ".json"
        guest = "/tmp/creation-evidence/" + filename
        target_dir = "native-reads"
        copied = machine.out_dir / target_dir / filename
        command = ["${python}/bin/python", "${../../test/creation_transport.py}",
            "read", runtime + "/app-server.sock", identity]
        if timeout is not None:
            command = ["${pkgs.coreutils}/bin/timeout", str(timeout)] + command
        data = None
        try:
            user("mkdir -p /tmp/creation-evidence && " + shlex.join(command) + " > " + shlex.quote(guest))
            machine.copy_from_machine(guest, target_dir)
            data = copied.read_bytes()
            return json.loads(data)
        except Exception as error:
            print("native JSON file read failed", json.dumps({"identity": identity,
                "guest": guest, "copied": str(copied), "error": type(error).__name__,
                "length": len(data) if data is not None else None,
                "sha256": hashlib.sha256(data).hexdigest() if data is not None else None,
                "position": error.pos if isinstance(error, json.JSONDecodeError)
                    else error.start if isinstance(error, UnicodeDecodeError) else None}))
            raise

    def native(identity):
        return native_json(identity)

    def managed_input(record, text, identity):
        body = json.dumps({"message": text, "clientUserMessageId": identity})
        return json.loads(user("curl -fsS --unix-socket " + runtime + "/portal.sock "
            + "-H 'Origin: https://fixture.workspace.example.test' -H 'Content-Type: application/json' "
            + "--data-binary " + shlex.quote(body) + " http://localhost/api/sessions/" + record["slug"] + "/message"))

    def maintenance_input(record, text, identity, observations):
        body = {"message": text, "clientUserMessageId": identity}
        command = ("curl -sS --max-time 30 --unix-socket " + runtime + "/portal.sock "
            + "-H 'Origin: https://fixture.workspace.example.test' -H 'Content-Type: application/json' "
            + "--write-out '\\n%{http_code}' --data-binary ")
        url = "http://localhost/api/sessions/" + record["slug"] + "/message"
        def request():
            try:
                raw = user(command + shlex.quote(json.dumps(body)) + " " + shlex.quote(url))
                payload, status = raw.rsplit("\n", 1)
                observed = {"status": status, "body": payload}
                observations.append(observed)
                response = json.loads(payload)
                observed["body"] = response
                assert isinstance(response, dict), observations
                return status, response
            except Exception as error:
                observations.append({"failure": str(error)})
                raise AssertionError(("maintenance input transport/JSON failure", observations)) from error
        def accepted(status, response):
            assert (status == "202" and response.get("clientUserMessageId") == identity
                and isinstance(response.get("turnId"), str) and response["turnId"]
                and not response.get("queued") and not response.get("queuedSubmission")), observations
            return response
        status, response = request()
        if status == "202":
            return accepted(status, response)
        # This is one deliberate same-identity attempt, never an automatic retry loop.
        assert (status == "503" and response.get("code") == "message_not_sent"
            and response.get("notSent") is True and response.get("retryable") is False), observations
        body["retry"] = True
        status, response = request()
        return accepted(status, response)

    def held_model_request(marker, text, after=0):
        # Match this turn's exact user text, never the latest unrelated request.
        script = ("import json,runpy; marker=" + repr(marker) + "; text=" + repr(text) + "; "
            + "rows=runpy.run_path('${../../test/creation_model.py}')['request_rows']; "
            + "users=lambda row: [part.get('text') for item in row['body'].get('input',[]) "
            + "if item.get('role')=='user' for part in item.get('content',[]) if part.get('type')=='input_text']; "
            + "matches=[]\nfor row in rows():\n"
            + " if row['index']>" + str(after)
            + " and row['body'].get('model')=='gpt-5.5' and marker in users(row) and text in users(row): matches.append(row['index'])\n"
            + "print(json.dumps(matches))")
        matches = []
        def observed(_last):
            nonlocal matches
            matches = json.loads(user("${python}/bin/python -c " + shlex.quote(script)))
            assert isinstance(matches, list) and all(type(index) is int for index in matches), matches
            assert len(matches) <= 1, {"matchingRequestCount": len(matches)}
            return bool(matches)
        retry(observed)
        return matches[0]

    def advance_model_response(index, marker, text, permits):
        permit = "/tmp/creation-model/release-turn-" + str(index)
        permits.append(permit)
        user("test -e /tmp/creation-model/hold-turn && test ! -e " + shlex.quote(permit)
            + " && touch " + shlex.quote(permit))
        following = held_model_request(marker, text, after=index)
        user("test -e /tmp/creation-model/hold-turn && test ! -e " + shlex.quote(permit))
        return following

    def prefix_proof(record, turn, expected):
        path = (state + "/turn-retries/" + hashlib.sha256(root.encode()).hexdigest() + "/" + record["slug"]
            + "/" + hashlib.sha256(record["threadId"].encode()).hexdigest() + ".json")
        script = ("import json,sqlite3; db=sqlite3.connect('file:' + " + repr(path + ".sqlite") + " + '?mode=ro', uri=True); "
            + "s=json.loads(db.execute('SELECT body FROM retry_state WHERE id=1').fetchone()[0]); m=s['managedTurn']; "
            + "assert m['turn']==" + repr(turn) + " and m['count']==" + str(len(expected)) + "; "
            + "assert s['handled']!=" + repr(turn) + " and s['pending'] is None")
        command = user_prefix + "${python}/bin/python -c " + shlex.quote(script)

        def diagnostic(stage):
            try:
                evidence = "/tmp/creation-evidence/native-prefix-" + str(len(expected)) + "-" + stage + ".json"
                projected = [{"clientId": row["clientId"], "textSha256": hashlib.sha256(row["text"].encode()).hexdigest(),
                              "textLength": len(row["text"].encode())} for row in expected]
                status, output = machine.execute(user_prefix + shlex.join([
                    "${pkgs.coreutils}/bin/timeout", "--kill-after=1s", "10s", "${python}/bin/python",
                    "${../../test/creation_transport.py}", "prefix-diagnostic", runtime + "/app-server.sock",
                    record["threadId"], turn, path, json.dumps(projected), evidence]), timeout=12)
                if status == 0:
                    print(json.dumps({"prefixDiagnostic": stage, "evidence": evidence, "summary": json.loads(output)}))
                else:
                    print(json.dumps({"prefixDiagnostic": stage, "errorType": "Timeout" if status == 124 else "ProcessFailure",
                                      "exitStatus": status}))
            except Exception as error:
                print(json.dumps({"prefixDiagnostic": stage, "errorType": type(error).__name__}))

        try:
            initial_status, _ = machine.execute(command, timeout=10)
        except Exception:
            initial_status = 1
        if initial_status != 0:
            diagnostic("initial")
        try:
            machine.wait_until_succeeds(command)
            return json.loads(user(shlex.join(["${python}/bin/python", "${../../test/creation_transport.py}",
                "items-proof", runtime + "/app-server.sock", record["threadId"], turn, path, json.dumps(expected),
                "/tmp/creation-items-proof.json"])))
        except Exception:
            diagnostic("failure")
            raise

    def naming_proof(name, provider, attempts=1):
        marker = checkpoint(name)["marker"]
        # Keep only matching providers, while validating the complete prefix.
        script = ("import json,runpy; marker=" + repr(marker) + "; "
            + "rows=runpy.run_path('${../../test/creation_model.py}')['request_rows']; routes=[]\n"
            + "for row in rows():\n"
            + " if row['body'].get('model')=='gpt-6-luna' and marker in json.dumps(row['body'].get('input',[])): routes.append(row['provider'])\n"
            + "print(json.dumps(routes))")
        routes = json.loads(user("${python}/bin/python -c " + shlex.quote(script)))
        assert routes == [provider] * attempts, routes

    def proof(name, provider, members=0, sensitive=False, evidence_name=None):
        record = checkpoint(name)
        slug = record["slug"]
        directory = root + "/work/" + slug
        manifest = json.loads(user("${python}/bin/python -c " + shlex.quote(
            "import yaml,json;print(json.dumps(yaml.safe_load(open(" + repr(directory + "/portal.yml") + "))))")))
        assert manifest["codex"]["thread_id"] == record["threadId"], manifest
        assert manifest["creation"]["state"] == "ready", manifest
        if record["kind"] != "fork":
            assert manifest["creation"]["initial_goal_sent"] is True, manifest
        journal_path = root + "/worktrees/.locks/" + slug + ".creation.json"
        if record["kind"] == "fork":
            user("test ! -e " + root + "/worktrees/.locks/" + slug + ".fork.json")
        else:
            journal = json.loads(user("cat " + journal_path))
            assert journal["state"] == "ready", journal
            if record["options"].get("provider"):
                assert journal["provider"] == record["options"]["provider"], journal
        receipt_path = portal_state + "/creations/" + slug + ".json"
        receipt = json.loads(user("cat " + receipt_path))
        assert receipt["receiptId"] == record["receiptId"] and receipt["state"] == "ready", receipt
        evidence_path = portal_state + "/creations/" + slug + "." + record["receiptId"] + ".complete.json"
        evidence = json.loads(user("cat " + evidence_path))
        assert evidence["slug"] == slug and evidence["receiptId"] == record["receiptId"], evidence
        assert evidence["threadId"] == record["threadId"], evidence
        metadata = native(record["threadId"])["thread"]
        assert metadata["modelProvider"] == provider, metadata
        assert metadata["cwd"] == directory, metadata
        assert metadata["model"] == "gpt-5.5" and metadata["reasoningEffort"] == "high", metadata
        assert sum(item.get("type") == "userMessage" and record["marker"] in json.dumps(item)
            for turn in metadata["turns"] for item in turn["items"]) == 1, metadata
        assert len({turn["id"] for turn in metadata["turns"]}) == len(metadata["turns"]), metadata
        roster = json.loads(user("cd " + root + " && " + profile + "/bin/dev-session team list " + slug + " --as-is"))
        saved = roster["roster"]
        retained = saved["members"] or []
        assert len(retained) == members, saved
        assert saved["rootThreadId"] == record["threadId"] and saved["slug"] == slug, saved
        assert saved["leadModel"] == "gpt-5.5" and saved["leadEffort"] == "high", saved
        assert saved["leadInstructions"] == "CREATION_LEAD_INSTRUCTIONS: return the fixture reply; do not run tools.", saved
        identities = [record["threadId"]]
        for member in retained:
            assert member["state"] == "ready", member
            assert member["model"] == "gpt-5.5" and member["reasoningEffort"] == "high", member
            assert member["instructions"] == "CREATION_MEMBER_INSTRUCTIONS: inspect only when assigned.", member
            identities.append(member["threadId"])
            member_native = native(member["threadId"])["thread"]
            assert member_native["modelProvider"] == provider, member_native
            assert member_native["cwd"] == directory, member_native
            assert member_native["projectId"] == member["projectId"], member_native
        assert len(set(identities)) == len(identities), identities
        inventory = native("inventory")
        assert not inventory.get("nextCursor"), inventory
        listed = {item["id"] for item in inventory["data"] if item["cwd"] == directory}
        assert record["threadId"] in listed and listed <= set(identities), inventory
        # Native thread/list omits empty previews, including unassigned members
        # with only the durable developer bootstrap. Read the pinned native
        # database without writes to inventory those identities and orphan starts.
        persisted = json.loads(user("${python}/bin/python -c " + shlex.quote(
            "import json,pathlib,sqlite3; "
            "files=list((pathlib.Path.home()/'.local/state/dev-workspaces/codex-homes/fixture').glob('state_*.sqlite')); "
            "assert len(files)==1,files; "
            "db=sqlite3.connect(files[0].as_uri()+'?mode=ro',uri=True); "
            "print(json.dumps(db.execute('SELECT id FROM threads WHERE cwd=? AND archived=0',"
            + repr((directory,)) + ").fetchall()))")))
        assert {row[0] for row in persisted} == set(identities), persisted
        if sensitive:
            user("git -C " + root + " check-ignore work/" + slug + "/state.md")
        # The real Go ready proof has already passed; retain independent native
        # identities and durable files so a green browser cannot mask duplicates.
        destination = "/tmp/creation-evidence/" + (evidence_name or name)
        user("mkdir -p " + destination + " && cp " + receipt_path + " " + evidence_path + " "
            + directory + "/portal.yml " + destination)
        if record["kind"] != "fork":
            user("cp " + journal_path + " " + destination)
        user("printf %s " + shlex.quote(json.dumps(metadata)) + " > " + destination + "/native.json")
        return {"checkpoint": record, "creation": manifest["creation"], "receipt": receipt, "completion": evidence,
            "team": {key: saved[key] for key in ("slug", "rootThreadId", "presetId", "leadModel", "leadEffort", "leadInstructions")},
            "members": retained, "thread": {key: metadata.get(key) for key in
                ("id", "cwd", "modelProvider", "model", "reasoningEffort", "projectId")},
            "turnIds": [turn["id"] for turn in metadata["turns"]],
            "messages": [(turn["id"], item) for turn in metadata["turns"] for item in turn["items"]
                if item.get("type") == "userMessage"],
            "incarnation": user("stat -Lc '%d:%i' " + directory).strip()}

    # Packaged maintenance acceptance uses the same VM, model and managed team.
    # Generic fixture packages have no profile resource: this is not claim proof.
    profile = state + "/profile"
    native_unit = "workspace-codex@fixture.service"
    portal_unit = "workspace-portal@fixture.service"
    hold = "/tmp/creation-model/hold-turn"
    target_hold = "/tmp/creation-model/hold-target-start"
    target_entered = "/tmp/creation-model/target-start-entered"
    router_unit = "workspace-router.service"
    target_dropin = "/home/developer/.config/systemd/user/" + router_unit + ".d/maintenance-fixture.conf"
    failure_dropin = "/home/developer/.config/systemd/user/" + native_unit + ".d/maintenance-fixture.conf"
    resume_text = "Resume the work paused for the workspace update. Inspect your saved checkpoint, current state and partial effects before retrying any action. Restart an interrupted child only if you decide it is needed."

    def host(arguments):
        return user("cd " + root + " && " + shlex.join([profile + "/bin/workspace-host"] + arguments))

    def native_request(method, **params):
        return json.loads(user(shlex.join(["${python}/bin/python", "${../../test/creation_transport.py}",
            "request", runtime + "/app-server.sock", method, json.dumps(params)])))

    def managed_client_state(record, native, source=None, operation=None, state_name="running", provider_operation=False):
        expected = {"workspace": root, "slug": record["slug"], "root": record["threadId"],
            "socket": runtime + "/app-server.sock", "tmuxSocket": runtime + "/tmux.sock",
            "native": native, "state": state_name, "source": source, "operation": operation,
            "providerOperation": provider_operation}
        # Read-only tmux/proc observation; this helper never restores a client.
        command = shlex.join(["${pkgs.coreutils}/bin/env", "PATH=${pkgs.tmux}/bin",
            "${python}/bin/python", "${../../test/creation_transport.py}", "managed-client",
            runtime + "/tmux.sock", record["slug"], json.dumps(expected)])
        return json.loads(machine.wait_until_succeeds(user_prefix + command, timeout=20))

    def recovery_proof(record):
        path = state + "/session-recovery/" + hashlib.sha256(root.encode()).hexdigest() + "/" + record["slug"] + ".json"
        saved = json.loads(user("cat " + shlex.quote(path)))
        assert saved["schema"] == 1 and saved["workspace"] == root and saved["slug"] == record["slug"], saved
        assert saved["thread_id"] == record["threadId"] and saved["socket_path"] == runtime + "/app-server.sock", saved
        assert saved["automatic"] is True and saved["socket_identity"], saved
        current = json.loads(user(shlex.join([package + "/bin/workspace-portal", "recovery", "--operation", "status",
            "--workspace", root, "--user-state-root", state, "--slug", record["slug"],
            "--thread-id", record["threadId"], "--socket", runtime + "/app-server.sock"])))
        assert set(current) == {"held"} and isinstance(current["held"], bool), current
        return {"record": saved, "status": current}

    def public_post(path, body):
        raw = user(shlex.join(["curl", "--max-time", "20", "-sS", "--unix-socket", runtime + "/portal.sock",
            "-H", "Origin: https://fixture.workspace.example.test", "-H", "Content-Type: application/json",
            "--write-out", "\\n%{http_code}", "--data-binary", json.dumps(body), "http://localhost" + path]))
        assert "\n" in raw, ("public POST response framing", path, raw)
        payload, status = raw.rsplit("\n", 1)
        # Preserve refusal bytes before JSON decoding can obscure their reason.
        assert status in ("200", "202"), ("public POST failed", path, status, payload)
        return json.loads(payload)

    def unit_state(unit):
        return dict(line.split("=", 1) for line in user("systemctl --user show " + unit
            + " --property=ActiveState,SubState,MainPID,ControlPID,InvocationID,Result,ExecMainStatus").splitlines())

    def maintenance_status(identity=None):
        response = json.loads(user(shlex.join(["${pkgs.coreutils}/bin/timeout", "10", profile + "/bin/workspace-host",
            "maintenance", "status"] + ([identity] if identity else []))))
        assert response["schema"] == 1, response
        return response.get("operation")

    def maintenance_wait(label, predicate, identity=None, recovering=False, predecessor=None):
        deadline = time.monotonic() + 300
        while time.monotonic() < deadline:
            current = maintenance_status(identity)
            # An accepted launch can still expose the completed predecessor.
            if current and predecessor and current["operationId"] == predecessor:
                assert current["phase"] == "complete", (label, current)
            elif predicate(current):
                return current
            elif current and (current["phase"] in ["complete", "cancelled"]
                    or current["phase"] == "recovery-required" and not recovering):
                raise AssertionError((label, current))
            time.sleep(0.25)
        raise AssertionError((label, "fixture deadline", maintenance_status(identity)))

    def resume_requests(marker):
        script = ("import json,runpy; rows=runpy.run_path('${../../test/creation_model.py}')['request_rows']; "
            + "users=lambda row:[item for item in row['body'].get('input',[]) if item.get('role')=='user']; matches=[]\n"
            + "for row in rows():\n"
            + " if users(row) and " + repr(resume_text) + " in json.dumps(users(row)[-1]) and "
            + repr(marker) + " in json.dumps(row['body'].get('input',[])): matches.append(row['index'])\n"
            + "print(json.dumps(matches))")
        return json.loads(user("${python}/bin/python -c " + shlex.quote(script)))

    def user_texts(metadata):
        return [item for turn in metadata["turns"] for item in turn["items"] if item.get("type") == "userMessage"]

    def continuation_items(thread, client_id):
        return [item for item in user_texts(native(thread)["thread"]) if item.get("clientId") == client_id]

    def package_native_identity(package):
        launcher = user("readlink -f " + package + "/libexec/codex/bin/codex").strip()
        executable = user("readlink -f " + package + "/libexec/codex/libexec/codex/bin/codex").strip()
        user("test -x " + executable)
        digest = user("sha256sum " + executable).split()[0]
        assert len(digest) == 64, digest
        return {"package": package, "launcher": launcher, "executable": executable, "sha256": digest}

    def assert_native_variant(source, target):
        assert all(source[key] != target[key] for key in ("package", "launcher", "executable")), (source, target)
        assert source["sha256"] == target["sha256"], (source, target)

    def switch_start(target, label, pause=2):
        previous = maintenance_status()
        assert not previous or previous["phase"] == "complete", previous
        # Distinct packaged paths require restart; executable bytes stay stock.
        prepared = json.loads(host(["prepare", "--source", maintenance_packages[target],
            "--enable-maintenance", "--pause-timeout", str(pause), "--json"]))
        assert prepared["source"] == maintenance_packages[target], prepared
        assert prepared["candidatePackage"] == maintenance_packages[target], prepared
        target_native = package_native_identity(prepared["candidatePackage"])
        source_native = package_native_identity(prepared["sourcePackage"])
        assert prepared["candidateCodex"] == target_native["launcher"], prepared
        assert prepared["sourceCodex"] == source_native["launcher"], prepared
        assert_native_variant(source_native, target_native)
        assert prepared["sourcePlansDigest"] != prepared["plansDigest"], prepared
        command_unit = "creation-maintenance-" + label
        user(shlex.join(["systemd-run", "--user", "--unit=" + command_unit, "--property=Type=exec",
            profile + "/bin/workspace-host", "switch", "--source", maintenance_packages[target],
            "--enable-maintenance", "--pause-timeout", str(pause)]))
        current = maintenance_wait("new " + label, lambda o: o and
            o["phase"] not in ["cancelled", "recovery-required"] and
            (not previous or o["operationId"] != previous["operationId"]),
            predecessor=previous["operationId"] if previous else None)
        assert current["preparation"]["source"] == prepared["source"], current
        assert current["preparation"]["sourceIdentity"] == prepared["sourceIdentity"], current
        assert current["sourcePackage"] == prepared["sourcePackage"], current
        assert current["candidatePackage"] == maintenance_packages[target], current
        assert current["preparation"]["pauseTimeout"] == pause, current
        assert current["preparation"]["switchOperationId"] == current["operationId"], current
        return current, command_unit + ".service", target_native

    def switch_done(identity, command_unit=None):
        current = maintenance_wait("completed " + identity, lambda o: o and o["phase"] == "complete", identity)
        machine.wait_until_succeeds(user_prefix + "systemctl --user is-active " + portal_unit)
        machine.wait_until_succeeds(user_prefix + "systemctl --user is-active " + native_unit)
        assert current["sourcesStopped"] and current["activationIntent"] and current["continuationsSettled"], current
        assert current["restoration"]["completed"], current
        # Completion may precede the worker process's final return very briefly.
        for unit in ["workspace-maintenance@" + identity + ".service"] + ([command_unit] if command_unit else []):
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(
                "test \"$(systemctl --user show " + unit + " -p MainPID --value)\" = 0"))
            state_row = unit_state(unit)
            assert state_row["Result"] == "success" and state_row["ExecMainStatus"] == "0", state_row
        user("mkdir -p /tmp/creation-evidence/maintenance && " + profile + "/bin/workspace-host maintenance status "
            + identity + " > /tmp/creation-evidence/maintenance/" + identity + ".json")
        user("journalctl --user --no-pager -u workspace-maintenance@" + identity
            + ".service > /tmp/creation-evidence/maintenance/" + identity + ".log")
        return current

    def target_barrier(fail=False):
        router = unit_state(router_unit)
        assert router["ActiveState"] == "active" and router["ControlPID"] == "0", router
        user("rm -f " + target_entered)
        if fail:
            path = failure_dropin
            contents = "[Service]\nExecStart=\nExecStart=${pkgs.coreutils}/bin/false\nRestart=no\n"
        else:
            path = target_dropin
            user("touch " + target_hold)
            command = ("echo \"$MAINPID\" \"$INVOCATION_ID\" > " + target_entered
                + "; while test -e " + target_hold + "; do sleep 0.1; done")
            contents = "[Service]\nExecStartPost=${pkgs.coreutils}/bin/timeout 60 ${pkgs.runtimeShell} -c " + shlex.quote(command) + "\n"
        user("mkdir -p " + shlex.quote(path.rsplit("/", 1)[0]))
        user("printf %s " + shlex.quote(contents) + " > " + shlex.quote(path))
        user("systemctl --user daemon-reload")
        # Only _activate's existing synchronous try-restart may enter this hold.
        assert unit_state(router_unit) == router, "router restarted before target activation"
        return router

    def release_target():
        # Only this fixture's marker/drop-ins; never a source portal resurrection.
        user("rm -f " + target_hold + " " + target_dropin + " " + failure_dropin + " && systemctl --user daemon-reload")

    def stable_goal_fields(goal):
        return {key: goal[key] for key in ("threadId", "objective", "status", "tokenBudget", "createdAt")}

    def retained_state(record, queue, goal, before, queue_text):
        thread = native(record["threadId"])["thread"]
        assert thread["id"] == before["id"] and thread["cwd"] == before["cwd"], thread
        assert {t["id"] for t in before["turns"]} <= {t["id"] for t in thread["turns"]}, thread
        queued = native_request("thread/queue/list", threadId=record["threadId"])
        assert queued.get("nextCursor") is None, queued
        def canonical_text(parts):
            assert isinstance(parts, list) and parts, parts
            assert all(isinstance(part, dict) and part.get("type") == "text"
                and isinstance(part.get("text"), str) for part in parts), parts
            return "\n".join(part["text"] for part in parts)
        assert isinstance(queue.get("id"), str) and queue["id"], queue
        assert isinstance(queue.get("clientUserMessageId"), str) and queue["clientUserMessageId"], queue
        assert isinstance(queue_text, str) and queue_text, queue_text
        assert canonical_text(queue.get("input")) == queue_text, queue
        rows = [row for row in queued["data"] if row.get("id") == queue["id"]]
        consumed = [(turn, item) for turn in thread["turns"] for item in turn["items"]
            if item.get("type") == "userMessage" and item.get("clientId") == queue["clientUserMessageId"]]
        assert len(rows) + len(consumed) == 1, (queued, consumed)
        if rows:
            assert rows[0].get("clientUserMessageId") == queue["clientUserMessageId"], rows
            assert canonical_text(rows[0].get("input")) == queue_text, rows
        else:
            turn, item = consumed[0]
            assert isinstance(turn.get("id"), str) and turn["id"], turn
            assert isinstance(item.get("id"), str) and item["id"], item
            assert canonical_text(item.get("content")) == queue_text, item
        current_goal = native_request("thread/goal/get", threadId=record["threadId"])["goal"]
        assert current_goal["threadId"] == record["threadId"] and current_goal["objective"] == goal["objective"], current_goal
        assert stable_goal_fields(current_goal) == stable_goal_fields(goal), current_goal
        if current_goal.get("status") != "active":
            try:
                # Returned array order does not establish native turn recency.
                turns = thread["turns"]
                last = len(turns) - 1
                failed = next((index for index in range(last, -1, -1) if turns[index].get("status") == "failed"), -1)
                def turn_evidence(index):
                    if index < 0:
                        return None
                    turn = turns[index]
                    error = json.dumps(turn.get("error"))
                    return {"arrayIndex": index, "id": turn.get("id"), "status": turn.get("status"),
                        "errorAvailable": "error" in turn, "errorJson": error[:1000], "errorTruncated": len(error) > 1000}
                fields = ("threadId", "objective", "status", "tokenBudget", "tokensUsed",
                    "timeBudgetSeconds", "timeUsedSeconds", "createdAt", "updatedAt")
                print(json.dumps({"retainedGoalFailure": {"threadId": thread["id"],
                    "savedGoal": {key: goal[key] for key in fields if key in goal},
                    "currentGoal": {key: current_goal[key] for key in fields if key in current_goal},
                    "turnCount": len(turns), "turnOrder": "returned-array", "recency": "unavailable",
                    "returnedLastTurn": turn_evidence(last),
                    "returnedLastFailedTurn": turn_evidence(failed) if failed != last else None,
                    "failedTurnSameAsLast": failed >= 0 and failed == last}}))
            except Exception as diagnostic_error:
                try:
                    print(json.dumps({"retainedGoalDiagnosticUnavailable": type(diagnostic_error).__name__}))
                except Exception:
                    pass
        assert current_goal["status"] == "active", current_goal
        return {"queue": queued, "goal": current_goal, "threadId": thread["id"]}

    def maintenance_model_hold(rules, install=False):
        # Atomic publication and exact-content removal keep this case's ownership.
        script = ("import json,pathlib; p=pathlib.Path('/tmp/creation-model/hold-maintenance.json'); rules="
            + repr(rules) + "\n")
        if install:
            script += ("assert not p.exists(), 'maintenance hold already owned'\n"
                "temporary=p.with_suffix('.tmp'); temporary.write_text(json.dumps(rules)); temporary.replace(p)\n")
        else:
            script += "if p.exists():\n assert json.loads(p.read_text())==rules, 'maintenance hold owner changed'\n p.unlink()\n"
        user("${python}/bin/python -c " + shlex.quote(script))

    def maintenance_model_requests(rules, ready=False):
        script = ("import json,runpy; rules=" + repr(rules) + "; "
            + "helpers=runpy.run_path('${../../test/creation_model.py}'); match=helpers['maintenance_request_matches']; "
            + "parts=lambda row:[part.get('text') for item in row['body'].get('input',[]) "
            + "if item.get('role')=='user' for part in item.get('content',[]) "
            + "if part.get('type')=='input_text' and isinstance(part.get('text'),str)]; "
            + "metadata=lambda row:row['body'].get('client_metadata') if isinstance(row['body'].get('client_metadata'),dict) else {}; "
            + "project=lambda row,rule:{'index':row['index'],'model':row['body'].get('model'),"
            + "'threadId':metadata(row).get('thread_id'),"
            + "'turnId':metadata(row).get('turn_id'),"
            + "'exactParts':sum(text==rule['marker'] for text in parts(row)),"
            + "'markerLines':sum(text.splitlines().count(rule['marker']) for text in parts(row))}; "
            + "matches=[[] for rule in rules]; candidates=[[] for rule in rules]; count=0; maximum=0\n"
            + "for row in helpers['request_rows']():\n"
            + " count+=1; maximum=max(maximum,row['index'])\n"
            + " for i,rule in enumerate(rules):\n"
            + "  if match(row['body'],rule['threadId'],rule['marker']): matches[i].append(project(row,rule))\n"
            + "  if any(rule['marker'] in text.splitlines() for text in parts(row)) or metadata(row).get('thread_id')==rule['threadId']:\n"
            + "   candidates[i].append(project(row,rule)); candidates[i]=candidates[i][-20:]\n"
            + "print(json.dumps({'completeRows':count,'maxIndex':maximum,'matches':matches,'candidates':candidates}))")
        command = "${python}/bin/python -c " + shlex.quote(script)
        summary = {}
        def observed(_last):
            nonlocal summary
            summary = json.loads(user(command))
            assert (isinstance(summary, dict) and set(summary) == {"completeRows", "maxIndex", "matches", "candidates"}
                and type(summary["completeRows"]) is int and type(summary["maxIndex"]) is int
                and isinstance(summary["matches"], list) and isinstance(summary["candidates"], list)
                and len(summary["matches"]) == len(rules) and len(summary["candidates"]) == len(rules)
                and all(isinstance(rows, list) for rows in summary["matches"] + summary["candidates"])
                and all(isinstance(row, dict) and set(row) == {"index", "model", "threadId", "turnId", "exactParts", "markerLines"}
                    and all(type(row[key]) is int for key in ("index", "exactParts", "markerLines"))
                    for rows in summary["matches"] + summary["candidates"] for row in rows)), summary
            assert all(len(rows) <= 1 for rows in summary["matches"]), summary
            return all(len(rows) == 1 for rows in summary["matches"])
        if ready:
            retry(observed)
        else:
            observed(False)
        return summary

    def maintenance_member_snapshot(record, member, assignment_id, marker):
        expected_marker = marker + "_member"
        envelope = ("Team assignment from lead to " + member["address"] + ":\n\n" + expected_marker
            + "\n\nSend results or blocking questions to lead with report_to_lead. Use a new message_id for each message; reuse it only to retry that message.")
        snapshot = {"root": record["threadId"], "member": member["address"], "threadId": member["threadId"],
            "assignmentId": assignment_id, "marker": expected_marker,
            "envelopeSha256": hashlib.sha256(envelope.encode()).hexdigest()}
        def text_matches(text):
            if not isinstance(text, str):
                raise ValueError("message text unavailable")
            return {"envelopeExact": text == envelope, "markerLine": expected_marker in text.splitlines()}
        try:
            queued: list[dict[str, object]] = json.loads(user(shlex.join(["curl", "--max-time", "20", "-fsS", "--unix-socket",
                runtime + "/portal.sock", "http://localhost/codex/conversations/" + record["slug"] + "~" + member["address"] + "/queue"])))
            assert isinstance(queued, list), "queue is not a list"
            rows = []
            for index, row in enumerate(queued):
                assert isinstance(row, dict), "queue entry malformed"
                assert isinstance(row["id"], str) and isinstance(row["clientUserMessageId"], str), "queue identity malformed"
                rows.append({"order": index, "id": row["id"], "clientId": row["clientUserMessageId"],
                    "assignmentExact": row["clientUserMessageId"] == assignment_id, **text_matches(row["text"])})
            snapshot["queue"] = {"available": True, "entries": rows}
        except Exception as error:
            snapshot["queue"] = {"available": False, "error": type(error).__name__}
        try:
            thread = native_json(member["threadId"], timeout=10)["thread"]
            assert (isinstance(thread["id"], str) and isinstance(thread["turns"], list)
                and isinstance(thread["status"], (str, dict))), "native thread malformed"
            turns = []
            for turn in thread["turns"]:
                assert (isinstance(turn["id"], str) and isinstance(turn["status"], str)
                    and isinstance(turn["items"], list)), "native turn malformed"
                items = []
                for item in turn["items"]:
                    if item.get("type") != "userMessage":
                        continue
                    texts = [part["text"] for part in item["content"] if part.get("type") == "text"]
                    matches = [text_matches(text) for text in texts]
                    if item.get("clientId") == assignment_id or any(row["markerLine"] for row in matches):
                        items.append({"id": item["id"], "clientId": item.get("clientId"),
                            "assignmentExact": item.get("clientId") == assignment_id,
                            "envelopeExact": texts == [envelope], "markerLine": any(row["markerLine"] for row in matches)})
                error = turn["error"]
                if error is not None:
                    assert isinstance(error, dict), "native error malformed"
                    error = {key: str(error[key])[:256] for key in ("message", "codexErrorInfo", "additionalDetails") if key in error}
                turns.append({"id": turn["id"], "status": turn["status"], "error": error, "assignmentItems": items})
            snapshot["native"] = {"available": True, "threadId": thread["id"],
                "identityMatches": thread["id"] == member["threadId"], "status": thread["status"], "turns": turns}
        except Exception as error:
            snapshot["native"] = {"available": False, "error": type(error).__name__}
        try:
            path = state + "/session-recovery/" + hashlib.sha256(root.encode()).hexdigest() + "/" + record["slug"] + ".json"
            script = ("import json,pathlib,os,stat; saved=json.load(open(" + repr(path) + ")); "
                + "s=os.stat(" + repr(runtime + "/app-server.sock") + "); "
                + "assert stat.S_ISSOCK(s.st_mode), 'endpoint is not a socket'; "
                + "boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip(); "
                + "epoch=f'{boot}:{s.st_dev}:{s.st_ino}:{s.st_ctime_ns//1000000000}:{s.st_ctime_ns%1000000000}'; "
                + "print(json.dumps({'saved':saved,'epoch':epoch}))")
            permission = json.loads(user(shlex.join(["${pkgs.coreutils}/bin/timeout", "10", "${python}/bin/python", "-c", script])))
            saved = permission["saved"]
            assert isinstance(permission["epoch"], str) and permission["epoch"], "current socket epoch unavailable"
            assert saved["schema"] == 1 and isinstance(saved["automatic"], bool), "recovery shape malformed"
            assert isinstance(saved["active_threads"], list) and all(isinstance(value, str) for value in saved["active_threads"]), "active threads malformed"
            assert all(isinstance(saved[key], str) and saved[key] for key in ("workspace", "slug", "thread_id", "socket_path", "socket_identity")), "recovery identity malformed"
            snapshot["permission"] = {"available": True, "workspace": saved["workspace"], "slug": saved["slug"],
                "root": saved["thread_id"], "socket": saved["socket_path"], "savedEpoch": saved["socket_identity"],
                "currentEpoch": permission["epoch"], "automatic": saved["automatic"],
                "activeThreads": saved["active_threads"], "memberActive": member["threadId"] in saved["active_threads"],
                "identityMatches": saved["workspace"] == root and saved["slug"] == record["slug"]
                    and saved["thread_id"] == record["threadId"] and saved["socket_path"] == runtime + "/app-server.sock",
                "epochMatches": saved["socket_identity"] == permission["epoch"]}
        except Exception as error:
            snapshot["permission"] = {"available": False, "error": type(error).__name__}
        return snapshot

    def maintenance_case(label, target, cooperative=False, stop=False, fail=False):
        record = checkpoint("automatic")
        # Source setup only: restoration below must be independently observed.
        sync_source(profile, record["slug"])
        source_package = user("readlink -f " + profile).strip()
        source_native = package_native_identity(source_package)
        assert user("readlink -f " + state + "/codex/current/bin/codex").strip() == source_native["launcher"]
        source_client = managed_client_state(record, source_native["executable"])
        failed_client = None
        roster = json.loads(user("cd " + root + " && " + profile + "/bin/dev-session team list " + record["slug"] + " --as-is"))["roster"]
        member = next(row for row in roster["members"] if row["state"] == "ready")
        before = native(record["threadId"])["thread"]
        source_service = unit_state(native_unit)
        source_socket = user("stat -Lc '%d:%i' " + runtime + "/app-server.sock").strip()
        marker = "CREATION_MAINTENANCE_" + label + "_" + str(uuid.uuid4())
        client_id, assignment_id = str(uuid.uuid4()), str(uuid.uuid4())
        rules = [{"threadId": record["threadId"], "marker": marker},
            {"threadId": member["threadId"], "marker": marker + "_member"}]
        user("test ! -e " + hold)
        maintenance_model_hold(rules, install=True)
        setup = {"root": record["threadId"], "clientId": client_id, "member": member["address"],
            "memberThread": member["threadId"], "assignmentId": assignment_id, "rules": rules}
        fault_installed = False
        try:
            try:
                setup["inputResponses"] = []
                receipt = maintenance_input(record, marker, client_id, setup["inputResponses"])
                setup["receipt"] = receipt
                assert receipt["turnId"] and receipt["clientUserMessageId"] == client_id, receipt
                assignment = public_post("/api/sessions/" + record["slug"] + "/team", {"action": "assign", "from": "lead",
                    "to": member["address"], "message": marker + "_member", "messageId": assignment_id})
                roster = assignment["roster"]
                member = next(row for row in roster["members"] if row["address"] == member["address"])
                setup["returnedMember"] = {"address": member.get("address"), "threadId": member.get("threadId")}
                try:
                    setup["memberAfterAssignment"] = maintenance_member_snapshot(record,
                        {"address": setup["member"], "threadId": setup["memberThread"]}, assignment_id, marker)
                except Exception as snapshot_error:
                    setup["memberAfterAssignment"] = {"available": False, "error": type(snapshot_error).__name__}
                print("maintenance member after assignment", json.dumps(setup["memberAfterAssignment"]))
                assert member["threadId"] == rules[1]["threadId"], {"address": member["address"],
                    "expectedThread": rules[1]["threadId"], "observedThread": member["threadId"]}
                root_turn = receipt["turnId"]
                setup["rootTurn"] = root_turn
                root_index = held_model_request(marker, marker)
                selected = maintenance_model_requests(rules, ready=True)
                root_request, member_request = [rows[0] for rows in selected["matches"]]
                setup["requests"] = selected
                assert root_request["index"] == root_index, selected
                assert root_request["threadId"] == record["threadId"] and root_request["turnId"] == root_turn, selected
                member_thread = native(member["threadId"])["thread"]
                member_turn = next(t["id"] for t in member_thread["turns"] if t["status"] == "inProgress")
                setup["rootTurn"], setup["memberTurn"] = root_turn, member_turn
                assert member_request["threadId"] == member["threadId"] and member_request["turnId"] == member_turn, selected
                print("maintenance request admission", json.dumps(setup))
            except Exception as error:
                try:
                    setup["memberOnFailure"] = maintenance_member_snapshot(record,
                        {"address": setup["member"], "threadId": setup["memberThread"]}, assignment_id, marker)
                    print("maintenance member on failure", json.dumps(setup["memberOnFailure"]))
                except Exception as snapshot_error:
                    print("maintenance member snapshot unavailable", type(snapshot_error).__name__)
                try:
                    print("maintenance request admission failed", str(error), json.dumps(setup),
                        json.dumps(maintenance_model_requests(rules)))
                except Exception as diagnostic_error:
                    print("maintenance request diagnostic unavailable", str(diagnostic_error), json.dumps(setup))
                raise
            queue_id = str(uuid.uuid4())
            queue = native_request("thread/queue/add", threadId=record["threadId"], clientUserMessageId=queue_id,
                input=[{"type": "text", "text": marker + "_queue"}])["queuedSubmission"]
            assert queue["id"] and queue["clientUserMessageId"] == queue_id, queue
            native_request("thread/goal/set", threadId=record["threadId"], objective=marker + " goal", status="active", origin="user")
            goal = native_request("thread/goal/get", threadId=record["threadId"])["goal"]
            fault_installed = True
            barrier_source = target_barrier(fail)
            # The finite Stop/cooperative window belongs to this fixture only.
            operation, command_unit, target_native = switch_start(target, label, pause=30 if cooperative or stop else 2)
            assert operation["sourcePackage"] == source_native["package"], operation
            assert_native_variant(source_native, target_native)
            identity = operation["operationId"]
            captured = {p["thread"]: p for p in operation["participants"]}
            assert captured[record["threadId"]]["sourceTurn"] == root_turn, operation
            assert captured[member["threadId"]]["sourceTurn"] == member_turn, operation
            assert captured[member["threadId"]]["member"] == member["address"], operation
            if stop:
                public_post("/codex/conversations/" + record["slug"] + "~" + member["address"] + "/interrupt", {})
                operation = maintenance_wait("public Stop saved", lambda o: o and any(
                    p["thread"] == member["threadId"] and p["stopRevision"] > 0 for p in o["participants"]), identity)
                assert unit_state(portal_unit)["ActiveState"] == "active", operation
                assert not operation["sourcesStopped"], operation
            if cooperative:
                maintenance_wait("pause notices accepted", lambda o: o and all(p["noticeDisposition"] == "accepted"
                    for p in o["participants"] if p["thread"] in captured), identity)
                maintenance_model_hold(rules)
            if fail:
                operation = maintenance_wait("fixture target startup failed", lambda o: o and o["phase"] == "recovery-required",
                    identity, recovering=True)
                assert operation["activationIntent"] and operation["sourcesStopped"], operation
                assert user("readlink -f " + profile).strip() == target_native["package"]
                assert operation["activationNative"] == target_native["launcher"], operation
                assert all(p["continuationDisposition"] == "pending" for p in operation["participants"]), operation
                assert unit_state(portal_unit)["ActiveState"] == "inactive"
                assert unit_state(native_unit)["ActiveState"] == "failed"
                assert not resume_requests(marker), "resume input before successful recovery"
                failed_client = managed_client_state(record, target_native["executable"], source_client, operation, "stopped")
                # The failed switch command and worker must have returned before
                # recovery is requested under the same durable operation UUID.
                for unit in [command_unit, "workspace-maintenance@" + identity + ".service"]:
                    machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(
                        "test \"$(systemctl --user show " + unit + " -p MainPID --value)\" = 0"))
                    assert unit_state(unit)["Result"] != "success", unit_state(unit)
                saved_invocation = operation["workerInvocation"]
                release_target()
                barrier_source = target_barrier()
                host(["maintenance", "recover", identity])
                operation = maintenance_wait("same operation recovering", lambda o: o and o["workerInvocation"] != saved_invocation,
                    identity, recovering=True)
            def target_ready(current):
                result, _ = machine.execute(user_prefix + "test -s " + target_entered
                    + " && test -S " + runtime + "/app-server.sock", timeout=5)
                return current and result == 0
            operation = maintenance_wait("owned target startup barrier", target_ready, identity)
            target_service = unit_state(native_unit)
            router = unit_state(router_unit)
            entered = user("cat " + target_entered).split()
            assert router["ActiveState"] == "activating" and entered == [router["MainPID"], router["InvocationID"]], router
            assert router["InvocationID"] != barrier_source["InvocationID"], router
            assert target_service["ActiveState"] == "active" and target_service["ControlPID"] == "0", target_service
            assert int(target_service["MainPID"]) > 0 and target_service["InvocationID"] != source_service["InvocationID"], target_service
            assert operation["activationPackage"] == target_native["package"], operation
            assert operation["activationNative"] == target_native["launcher"], operation
            assert user("readlink /proc/" + target_service["MainPID"] + "/exe").strip() == target_native["executable"]
            assert user("stat -Lc '%d:%i' " + runtime + "/app-server.sock").strip() != source_socket
            assert user("readlink -f " + profile).strip() == target_native["package"]
            assert operation["sourcesStopped"] and operation["activationIntent"] and not operation["continuationsSettled"], operation
            user("test ! -e /proc/" + source_service["MainPID"])
            assert unit_state(portal_unit)["ActiveState"] == "inactive"
            for p in operation["participants"]:
                assert not continuation_items(p["thread"], p["continuationId"]), p
            barrier_clock = time.monotonic()
            stopped_client = managed_client_state(record, target_native["executable"], source_client, operation, "stopped")
            held_state = retained_state(record, queue, goal, before, marker + "_queue")
            source = native(record["threadId"])["thread"]
            source_status = next(t["status"] for t in source["turns"] if t["id"] == root_turn)
            assert source_status == ("completed" if cooperative else "interrupted"), source
            member_history = native(member["threadId"])["thread"]
            member_status = next(t["status"] for t in member_history["turns"] if t["id"] == member_turn)
            assert member_status == ("completed" if cooperative else "interrupted"), member_history
            # Source completion/interruption comes from actual retained native history.
            assert time.monotonic() - barrier_clock < 40, "bounded target inspection exceeded fixture budget"
            assert unit_state(router_unit)["ActiveState"] == "activating", "target barrier expired before release"
            release_target()
            fault_installed = False
            maintenance_model_hold(rules)
            completed = switch_done(identity, None if fail else command_unit)
            restored_client = managed_client_state(record, target_native["executable"], source_client, completed)
            for p in completed["participants"]:
                items = continuation_items(p["thread"], p["continuationId"])
                stopped = stop and p["thread"] == member["threadId"]
                assert p["continuationDisposition"] == ("suppressed" if stopped else "accepted"), p
                assert len(items) == (0 if stopped else 1), (p, items)
                if items:
                    assert resume_text in json.dumps(items[0]), items
            # Exhaust the prefix even after a match, so later corruption is fatal.
            resume_script = ("import json,runpy; rows=runpy.run_path('${../../test/creation_model.py}')['request_rows']; found=False\n"
                + "for row in rows():\n"
                + " if " + repr(resume_text) + " in json.dumps(row['body'].get('input',[])) and "
                + repr(marker) + " in json.dumps(row['body'].get('input',[])): found=True\n"
                + "print(json.dumps(found))")
            def resume_observed(_last):
                found = json.loads(user("${python}/bin/python -c " + shlex.quote(resume_script)))
                assert type(found) is bool, found
                return found
            retry(resume_observed)
            assert resume_requests(marker), "native resume input not observed by the existing model"
            assert json.loads(user("cd " + root + " && " + profile + "/bin/dev-session team list " + record["slug"] + " --as-is"))["roster"] == roster
            retained_state(record, queue, goal, before, marker + "_queue")
            # Re-running public recover on a completed UUID must not duplicate resume.
            expected = {p["thread"]: len(continuation_items(p["thread"], p["continuationId"])) for p in completed["participants"]}
            host(["maintenance", "recover", identity])
            replayed = switch_done(identity)
            replay_client = managed_client_state(record, target_native["executable"], source_client, replayed)
            assert replay_client["client"] == restored_client["client"], "completed recovery replaced the restored client"
            assert {p["thread"]: len(continuation_items(p["thread"], p["continuationId"])) for p in completed["participants"]} == expected
            user("printf %s " + shlex.quote(json.dumps({"heldState": held_state, "sourceStatus": source_status,
                "memberSourceStatus": member_status, "sourceService": source_service, "targetService": target_service,
                "clientSource": source_client, "clientStopped": stopped_client, "clientFailure": failed_client,
                "clientRestored": restored_client, "clientReplay": replay_client,
                "routerBarrier": router, "targetNative": target_native, "operationId": identity,
                "requestAdmission": setup}))
                + " > /tmp/creation-evidence/maintenance/" + label + ".json")
            # Preservation is proved above. Quiesce this case's retained goal
            # before the next case sends into the same conversation: a goal
            # continuation can otherwise finish between observation and steer.
            native_request("thread/goal/set", threadId=record["threadId"],
                objective=goal["objective"], status="paused", origin="user")
            for thread_id in (record["threadId"], member["threadId"]):
                def settled(_last):
                    thread = native(thread_id)["thread"]
                    return all(turn["status"] != "inProgress" for turn in thread["turns"])
                retry(settled)
        finally:
            maintenance_model_hold(rules)
            if fault_installed:
                release_target()

    def provider_status():
        return json.loads(host(["provider", "status", "fixture"]))

    def provider_done(identity):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            current = provider_status()
            operation = current.get("operation")
            assert operation and operation["id"] == identity, current
            assert operation["phase"] not in ("failed", "canceled", "recovery-required"), current
            if operation["phase"] == "applied":
                assert not operation.get("message"), operation
                return current
            time.sleep(0.1)
        raise AssertionError(("provider restoration deadline", provider_status()))

    def provider_worker_settled(operation):
        script = ("import pathlib; operation=" + repr(operation) + "; found=[]\n"
            + "for p in pathlib.Path('/proc').iterdir():\n"
            + " if not p.name.isdigit(): continue\n"
            + " try: args=(p/'cmdline').read_bytes().rstrip(b'\\0').decode().split('\\0')\n"
            + " except (OSError,UnicodeError): continue\n"
            + " if '--operation' in args and operation in args and 'codex-provider' in args: found.append(p.name)\n"
            + "assert not found, 'exact provider helper remains in flight'\n")
        machine.wait_until_succeeds(user_prefix + "${python}/bin/python -c " + shlex.quote(script), timeout=20)
        assert unit_state("workspace-codex-provider@fixture.service")["ActiveState"] == "active"

    def model_routes(thread=None, marker=None, after=0):
        # Exhaust the finite journal prefix, including wrong auth/route attempts.
        script = ("import json,runpy; h=runpy.run_path('${../../test/creation_model.py}'); rows=list(h['request_rows']()); "
            + "assert all(r.get('authMatch') is True and r.get('path')=='/'+r['provider']+'/v1/responses' for r in rows); "
            + "matches=[{k:r[k] for k in ('index','provider','authMatch','path')} for r in rows[" + str(after) + ":] "
            + "if " + repr(thread) + " is not None and h['maintenance_request_matches'](r['body']," + repr(thread) + "," + repr(marker) + ")]; "
            + "print(json.dumps({'count':len(rows),'matches':matches}))")
        return json.loads(user("${python}/bin/python -c " + shlex.quote(script)))

    def tui_evidence(label, evidence):
        user("printf %s " + shlex.quote(json.dumps(evidence))
            + " > /tmp/creation-evidence/tui-" + label + ".json")

    def tui_state(record):
        thread = native(record["threadId"])["thread"]
        return {"threadId": thread["id"], "status": thread["status"], "turns": thread["turns"],
            "goal": native_request("thread/goal/get", threadId=record["threadId"])["goal"],
            "queue": native_request("thread/queue/list", threadId=record["threadId"])}

    def tui_settled(observed, goal=None, queue=None):
        if goal is not None:
            assert stable_goal_fields(observed["goal"]) == {**stable_goal_fields(goal), "status": "paused"}, observed
        if queue is not None:
            queued = observed["queue"]
            assert queued.get("nextCursor") is None, queued
            delivered = [item for turn in observed["turns"] for item in turn["items"]
                if item.get("type") == "userMessage" and item.get("clientId") == queue["clientUserMessageId"]]
            assert len(delivered) <= 1, observed
            if queued["data"] or not delivered:
                return False
        return observed["status"]["type"] == "idle" and not any(turn["status"] == "inProgress" for turn in observed["turns"])

    def terminal_send(record, client, provider, goal=None, queue=None, trace=False):
        marker = "CREATION_TUI_" + str(uuid.uuid4())
        panes = []
        observation = {"marker": marker, "threadId": record["threadId"], "client": client, "panes": panes}
        # Observation only: original transport bytes and admission stay native.
        if trace:
            user("printf %s " + shlex.quote(json.dumps({"threadId": record["threadId"], "markers": [marker]}))
                + " > /tmp/creation-model/observe-tui.new && mv /tmp/creation-model/observe-tui.new /tmp/creation-model/observe-tui.json")
        tmux = ["${pkgs.tmux}/bin/tmux", "-S", runtime + "/tmux.sock", "send-keys", "-t", client["pane"]["id"]]
        def pane(stage):
            observed = json.loads(user(shlex.join(["${pkgs.coreutils}/bin/env", "PATH=${pkgs.tmux}/bin",
                "${python}/bin/python", "${../../test/creation_transport.py}", "pane-observation",
                runtime + "/tmux.sock", client["pane"]["id"]])))
            panes.append({"stage": stage, **observed})
            tui_evidence(marker, observation)
            return observed
        try:
            observation["before"] = tui_state(record)
            ready = pane("before-keys")
            assert ready["composerReady"], ("empty TUI composer unavailable", ready)
            assert tui_settled(observation["before"], goal, queue), ("TUI input work not settled", observation["before"])
            before = model_routes()["count"]
            # Empty composer proved above; never clear an unexpected draft.
            user(shlex.join(tmux + ["C-u"]))
            user(shlex.join(tmux + ["-l", marker]))
            literal = pane("after-literal-before-enter")
            literal_lines = literal["visible"].splitlines()
            cursor = int(literal["pane"]["cursor_y"])
            start = cursor
            while start > 0 and literal_lines[start].startswith("  "):
                start -= 1
            assert literal_lines[start].startswith("› ") and "".join(line[2:] for line in literal_lines[start:cursor + 1]) == marker, ("literal input not visible in composer", literal)
            user(shlex.join(tmux + ["Enter"]))
            pane("after-enter")
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                current = tui_state(record)
                observation["last"] = current
                if goal is not None:
                    assert stable_goal_fields(current["goal"]) == {**stable_goal_fields(goal), "status": "paused"}, current
                items = [(turn, item) for turn in current["turns"] for item in turn["items"]
                    if item.get("type") == "userMessage" and any(part.get("text") == marker for part in item.get("content", []))]
                routes = model_routes(record["threadId"], marker, before)
                observation["routes"] = routes
                assert len(items) <= 1 and len(routes["matches"]) <= 1, (marker, items, routes)
                if len(items) == len(routes["matches"]) == 1 and items[0][0]["status"] == "completed":
                    assert routes["matches"][0]["provider"] == provider, routes
                    observation["receipt"] = {"marker": marker, "threadId": record["threadId"], "turnId": items[0][0]["id"],
                        "itemId": items[0][1]["id"], "route": routes["matches"][0]}
                    if trace:
                        rows = [json.loads(line) for line in user("cat /tmp/creation-model/tui-rpc.jsonl").splitlines()]
                        assert rows and not any("observationError" in row for row in rows), rows
                        admissions = [row for row in rows if row.get("direction") == "request" and marker in json.dumps(row.get("params", {}))]
                        assert len(admissions) == 1, ("TUI input RPC count", admissions)
                        admitted = admissions[0]
                        replies = [row for row in rows if row.get("connection") == admitted["connection"]
                            and row.get("direction") == "response" and row.get("id") == admitted["id"]
                            and row["sequence"] > admitted["sequence"] and row.get("requestMethod") == admitted["method"]]
                        assert len(replies) == 1 and "error" not in replies[0], ("TUI input reply", admitted, replies)
                        observation["admission"] = {"request": admitted, "reply": replies[0]}
                    return observation["receipt"]
                time.sleep(0.1)
            raise AssertionError(("actual tmux TUI submission deadline", record["threadId"], marker))
        finally:
            try:
                observation["final"] = tui_state(record)
                pane("final")
            except Exception as error:
                observation["finalObservationError"] = type(error).__name__
            tui_evidence(marker, observation)

    def provider_terminal_case():
        started = time.monotonic()
        record, control = checkpoint("automatic"), checkpoint("explicit")
        sync_source(profile, record["slug"])
        sync_source(profile, control["slug"])
        identity = json.loads(user(shlex.join(["${python}/bin/python", "${../../test/creation_transport.py}",
            "production-native", package])))
        source_native = package_native_identity(package)
        assert source_native["sha256"] == identity["sha256"], (source_native, identity)
        client = managed_client_state(record, source_native["executable"])
        control_client = managed_client_state(control, source_native["executable"])
        daemon, portal = unit_state(native_unit), unit_state(portal_unit)
        roster_command = "cd " + root + " && " + profile + "/bin/dev-session team list " + record["slug"] + " --as-is"
        roster = json.loads(user(roster_command))["roster"]
        assert roster["members"] and all(member["state"] == "ready" for member in roster["members"]), roster
        retained = [record["threadId"]] + [member["threadId"] for member in roster["members"]]
        settings = {thread: {key: native(thread)["thread"].get(key) for key in ("id", "cwd", "model", "reasoningEffort", "projectId")} for thread in retained}
        control_marker = "CREATION_UNRELATED_" + str(uuid.uuid4())
        rules = [{"threadId": control["threadId"], "marker": control_marker}]
        maintenance_model_hold(rules, install=True)
        receipts = []
        try:
            control_input = managed_input(control, control_marker, str(uuid.uuid4()))
            held_model_request(control_marker, control_marker)
            control_before = native(control["threadId"])["thread"]
            assert any(turn["id"] == control_input["turnId"] and turn["status"] == "inProgress" for turn in control_before["turns"]), control_before
            before_send = terminal_send(record, client, "primary")
            for target in ("alternate", "primary"):
                old_client = client
                operation_id = str(uuid.uuid4())
                admitted = json.loads(host(["provider", "request", "fixture", target, operation_id, "--session", record["slug"]]))
                assert admitted["operation"]["id"] == operation_id, admitted
                applied = provider_done(operation_id)
                assert applied["sessions"][record["slug"]] == {"provider": target, "rootThreadId": record["threadId"]}, applied
                client = managed_client_state(record, source_native["executable"], old_client,
                    applied["operation"], provider_operation=True)
                sent = terminal_send(record, client, target)
                assert unit_state(native_unit) == daemon and unit_state(portal_unit) == portal, "scoped switch restarted shared services"
                assert managed_client_state(control, source_native["executable"]) == control_client, "unrelated terminal changed"
                assert native(control["threadId"])["thread"] == control_before, "unrelated active turn interrupted/changed"
                assert json.loads(user(roster_command))["roster"] == roster, "retained team changed"
                for thread in retained:
                    current = native(thread)["thread"]
                    assert {key: current.get(key) for key in settings[thread]} == settings[thread], current
                    assert current["modelProvider"] == target, current
                # Completed-current-UUID recovery is idempotency proof only.
                # The separate lost-archive-reply case owns interrupted recovery.
                count = model_routes()["count"]
                saved = user("cat " + state + "/providers/fixture/" + operation_id + ".snapshot.json")
                history = {thread: native(thread)["thread"] for thread in retained}
                host(["provider", "recover", "fixture", operation_id])
                provider_worker_settled(operation_id)
                replayed = provider_done(operation_id)
                assert replayed == applied, (applied, replayed)
                replay_client = managed_client_state(record, source_native["executable"])
                assert replay_client["session"] == client["session"] and replay_client["client"] == client["client"], "completed recovery restored client twice"
                assert user("cat " + state + "/providers/fixture/" + operation_id + ".snapshot.json") == saved
                assert model_routes()["count"] == count, "completed recovery submitted/inferred again"
                assert {thread: native(thread)["thread"] for thread in retained} == history, "completed recovery inserted messages"
                receipts.append({"operation": operation_id, "target": target, "status": applied,
                    "oldClient": old_client, "restoredClient": client, "sent": sent, "completedRecovery": "idempotent"})
            user("printf %s " + shlex.quote(json.dumps({"native": identity, "daemon": daemon, "beforeSend": before_send,
                "directions": receipts, "retained": retained, "controlTurn": control_input["turnId"],
                "elapsedSeconds": time.monotonic() - started})) + " > /tmp/creation-evidence/provider-terminal.json")
        finally:
            maintenance_model_hold(rules)
        deadline = time.monotonic() + 20
        while any(turn["status"] == "inProgress" for turn in native(control["threadId"])["thread"]["turns"]):
            assert time.monotonic() < deadline, "unrelated turn did not finish after fixture release"
            time.sleep(0.1)

    def interrupted_provider_case():
        started = time.monotonic()
        record, control = checkpoint("before"), checkpoint("after")
        # Both roots/teams were materialized through this existing proxy epoch.
        # First use an ordinary completed request to select A for the affected
        # session; its accepted creation provider was B.
        initial = str(uuid.uuid4())
        host(["provider", "request", "fixture", "primary", initial, "--session", record["slug"]])
        provider_done(initial)
        source = package_native_identity(package)
        identity = json.loads(user(shlex.join(["${python}/bin/python", "${../../test/creation_transport.py}",
            "production-native", package])))
        assert source["sha256"] == identity["sha256"], (source, identity)
        client = managed_client_state(record, source["executable"])
        control_client = managed_client_state(control, source["executable"])
        baseline = terminal_send(record, client, "primary", trace=True)
        daemon = unit_state(native_unit)
        assert user("readlink /proc/" + daemon["MainPID"] + "/exe").strip() == source["executable"], daemon
        tui_evidence("native-identity", {"native": identity, "package": source, "client": client, "daemon": daemon})
        roster_command = "cd " + root + " && " + profile + "/bin/dev-session team list " + record["slug"] + " --as-is"
        roster = json.loads(user(roster_command))["roster"]
        retained = [record["threadId"]] + [member["threadId"] for member in roster["members"]]
        settings = {thread: {key: native(thread)["thread"].get(key) for key in ("id", "cwd", "model", "reasoningEffort", "projectId")} for thread in retained}
        marker, control_marker = "CREATION_LOST_ARCHIVE_" + str(uuid.uuid4()), "CREATION_ARCHIVE_CONTROL_" + str(uuid.uuid4())
        rules = [{"threadId": record["threadId"], "marker": marker},
            {"threadId": control["threadId"], "marker": control_marker}]
        maintenance_model_hold(rules, install=True)
        operation = str(uuid.uuid4())
        path = state + "/providers/fixture/" + operation + ".snapshot.json"
        rule_path = "/tmp/creation-model/drop-archive-reply.json"
        receipt_path = "/tmp/creation-model/archive-reply-receipt.json"
        try:
            active = managed_input(record, marker, str(uuid.uuid4()))
            control_turn = managed_input(control, control_marker, str(uuid.uuid4()))
            held_model_request(marker, marker)
            held_model_request(control_marker, control_marker)
            before = native(record["threadId"])["thread"]
            control_before = native(control["threadId"])["thread"]
            assert any(turn["id"] == control_turn["turnId"] and turn["status"] == "inProgress" for turn in control_before["turns"])
            queue_text = marker + "_queue"
            queue = native_request("thread/queue/add", threadId=record["threadId"], clientUserMessageId=str(uuid.uuid4()),
                input=[{"type": "text", "text": queue_text}])["queuedSubmission"]
            native_request("thread/goal/set", threadId=record["threadId"], objective=marker + " goal", status="active", origin="user")
            goal = native_request("thread/goal/get", threadId=record["threadId"])["goal"]
            # Arm only after ordinary source admission; UUID is correlation in
            # this fixture control, not an invented archive RPC parameter.
            rule = {"operation": operation, "threadId": record["threadId"]}
            user("test ! -e " + rule_path + " && test ! -e " + receipt_path + " && printf %s "
                + shlex.quote(json.dumps(rule)) + " > " + rule_path)
            host(["provider", "request", "fixture", "alternate", operation, "--session", record["slug"]])
            machine.wait_until_succeeds(user_prefix + "test -s " + receipt_path, timeout=20)
            receipt = json.loads(user("cat " + receipt_path))
            assert receipt["operation"] == operation and receipt["threadId"] == record["threadId"] and receipt["nativeSuccess"] is True, receipt
            assert "result" in receipt["response"] and "error" not in receipt["response"], receipt
            deadline = time.monotonic() + 60
            while True:
                failed = provider_status()["operation"]
                assert failed["id"] == operation and failed["phase"] not in ("applied", "failed", "canceled"), failed
                if failed["phase"] == "recovery-required":
                    break
                assert time.monotonic() < deadline, ("lost archive reply did not reach recovery-required", failed)
                time.sleep(0.1)
            snapshot = json.loads(user("cat " + path))
            intent = snapshot["intents"][record["threadId"]]
            assert snapshot["operation"] == operation and snapshot["target"] == "alternate" and intent["closeIntent"] and not intent["closed"], snapshot
            archived = native(record["threadId"])["thread"]
            assert archived["path"].rsplit("/", 2)[-2] == "archived_sessions", archived
            assert unit_state(native_unit) == daemon and native(control["threadId"])["thread"] == control_before, "unrelated control/daemon changed"
            assert managed_client_state(control, source["executable"]) == control_client
            failure_copy = {"operation": failed, "snapshot": snapshot, "archiveReceipt": receipt, "archivedPath": archived["path"]}
            user("printf %s " + shlex.quote(json.dumps(failure_copy)) + " > /tmp/creation-evidence/provider-lost-archive-failure.json")
            # Exactly one public recovery, same operation and accepted B target.
            host(["provider", "recover", "fixture", operation])
            applied = provider_done(operation)
            completed = json.loads(user("cat " + path))
            assert completed["operation"] == operation and completed["target"] == "alternate", completed
            assert set(completed["intents"]) == set(snapshot["intents"]), completed
            for thread, old in snapshot["intents"].items():
                current = completed["intents"][thread]
                assert current["continuation"] == old["continuation"] and current["closed"] and current["verified"], current
                assert len(continuation_items(thread, current["continuation"])) <= 1, current
            assert json.loads(user(roster_command))["roster"] == roster
            for thread in retained:
                current = native(thread)["thread"]
                assert {key: current.get(key) for key in settings[thread]} == settings[thread] and current["modelProvider"] == "alternate", current
            preserved = retained_state(record, queue, goal, before, queue_text)
            assert {(turn["id"], item["id"]) for turn in before["turns"] for item in turn["items"] if item.get("type") == "userMessage"} <= {
                (turn["id"], item["id"]) for turn in native(record["threadId"])["thread"]["turns"] for item in turn["items"] if item.get("type") == "userMessage"}
            restored_client = managed_client_state(record, source["executable"], client, applied["operation"], provider_operation=True)
            assert unit_state(native_unit) == daemon and native(control["threadId"])["thread"] == control_before
            assert managed_client_state(control, source["executable"]) == control_client
            # Preserve the active goal first, then deliberately pause this case's
            # goal before an actual terminal input to avoid synthetic auto-work.
            paused = native_request("thread/goal/set", threadId=record["threadId"], status="paused")
            pause_proof = {"reply": paused, "state": tui_state(record)}
            tui_evidence("goal-pause", pause_proof)
            assert stable_goal_fields(pause_proof["state"]["goal"]) == {**stable_goal_fields(goal), "status": "paused"}, pause_proof
            maintenance_model_hold(rules)
            deadline = time.monotonic() + 20
            while True:
                settled = tui_state(record)
                pause_proof["last"] = settled
                tui_evidence("goal-pause", pause_proof)
                if tui_settled(settled, goal, queue) and not any(turn["status"] == "inProgress"
                        for turn in native(control["threadId"])["thread"]["turns"]):
                    break
                assert time.monotonic() < deadline, ("recovered/control turns or queue did not settle", settled)
                time.sleep(0.1)
            sent = terminal_send(record, restored_client, "alternate", goal, queue, trace=True)
            model_routes()
            user("printf %s " + shlex.quote(json.dumps({"operation": applied, "snapshot": completed,
                "preserved": preserved, "client": restored_client, "terminalSend": sent,
                "baseline": baseline, "pause": pause_proof,
                "rootTurn": active["turnId"], "controlTurn": control_turn["turnId"],
                "elapsedSeconds": time.monotonic() - started})) + " > /tmp/creation-evidence/provider-lost-archive-recovered.json")
        finally:
            maintenance_model_hold(rules)
            user("rm -f " + rule_path)

    def maintenance_bootstrap(captured_roots):
        assert captured_roots, "maintenance bootstrap requires captured terminal roots"
        policy = json.loads(user("cat " + package + "/share/dev-workspace/machine-identity-policy.json"))
        catalog = json.loads(user("cat " + package + "/share/dev-workspace/coordination.json"))
        assert policy["profileTarget"] is None and catalog.get("profileMachine") is None, (policy, catalog)
        original_marker = json.loads(user("cat " + runtime + "/registration.json"))
        source_native = package_native_identity(package)
        assert original_marker["launch"]["required_native_child_threads"] == 0, original_marker
        assert original_marker["launch"]["codex_path"] == source_native["launcher"], original_marker
        # Ordinary same-package enable creates its own startup authority.
        # The fixture fault is only timer restoration, after native-ready
        # evidence; no synthetic startup ID/grant or new native is staged.
        before_enable = unit_state(native_unit)
        before_marker = user("cat " + runtime + "/registration.json")
        startup_path = state + "/maintenance/startup.json"
        timer = "workspace-tracking-sync@fixture.timer"
        user("systemctl --user mask --force " + timer)
        enable_argv = [profile + "/bin/workspace-host", "switch", "--source", package, "--enable-maintenance"]
        enable_status, enable_output = machine.execute(user_prefix + shlex.join(enable_argv) + " 2>&1")
        assert enable_status == 1 and "command failed: systemctl --user enable --now " + timer in enable_output, (enable_status, enable_output)
        startup = json.loads(user("cat " + startup_path))
        startup_id = startup["operationId"]
        public_startup = json.loads(host(["maintenance", "startup-status", startup_id]))
        assert public_startup["startup"] == startup and public_startup["status"] == "pending", public_startup
        assert startup["schema"] == 1 and startup["pending"] and not startup["enabled"] and startup["targetEnabled"], startup
        assert startup["candidatePackage"] == package and startup["sourceProfileToken"] == startup["authorizedProfileToken"], startup
        assert len(startup["launchGrants"]) == 1, startup
        grant = startup["launchGrants"][0]
        assert grant["sequence"] == 1 and grant["invocation"] == before_enable["InvocationID"], grant
        assert len(grant["attempts"]) == 1 and grant["attempts"][0]["disposition"] == "ready", grant
        assert grant["attempts"][0]["pid"] == int(before_enable["MainPID"]), grant
        user("test ! -e " + state + "/maintenance/" + startup_id)
        assert maintenance_status() is None, "same-package enable fabricated a full operation"
        assert user("readlink -f " + profile).strip() == package
        assert unit_state(native_unit)["InvocationID"] == before_enable["InvocationID"]
        assert unit_state(native_unit)["MainPID"] == before_enable["MainPID"]
        assert user("cat " + runtime + "/registration.json") == before_marker
        user("systemctl --user unmask " + timer + " && systemctl --user daemon-reload")
        recovery_argv = ["maintenance", "recover-startup", startup_id, "--source", package]
        recovered = json.loads(host(recovery_argv))
        assert recovered["status"] == "enabled" and recovered["startup"]["enabled"] and not recovered["startup"]["pending"], recovered
        assert recovered["startup"]["launchGrants"] == startup["launchGrants"], recovered
        user("test ! -e " + state + "/codex/pending && test ! -e " + state + "/maintenance/" + startup_id)
        for family in ("auto-archive", "tracking-sync"):
            user("systemctl --user is-enabled workspace-" + family + "@fixture.timer")
            user("systemctl --user is-active workspace-" + family + "@fixture.timer")
        assert json.loads(host(recovery_argv)) == recovered, "completed startup recovery replay changed evidence"
        host(["switch", "--source", package, "--enable-maintenance"])
        assert json.loads(host(["maintenance", "startup-status", startup_id])) == recovered
        assert unit_state(native_unit)["InvocationID"] == before_enable["InvocationID"]
        assert unit_state(native_unit)["MainPID"] == before_enable["MainPID"]
        assert user("cat " + runtime + "/registration.json") == before_marker
        print("same-package startup recovery", json.dumps({"operationId": startup_id,
            "initialFailure": enable_output, "startup": recovered, "nativeBefore": before_enable,
            "nativeAfter": unit_state(native_unit)}))
        operation, command_unit, target_native = switch_start("target", "bootstrap")
        assert operation["sourcePackage"] == package, operation
        completed = switch_done(operation["operationId"], command_unit)
        assert completed["sourcePackage"] == package, completed
        for name, retained in captured_roots.items():
            record = checkpoint(name)
            intent = {"workspace": root, "slug": record["slug"], "root": record["threadId"],
                "incarnation": retained["identity"]["incarnation"]}
            assert [item for item in completed["terminalIntents"] if item["slug"] == record["slug"]] == [intent], completed
            assert intent in completed["restoration"]["terminals"] and completed["restoration"]["completed"], completed
            # These idle roots have terminal intents, not pause participants.
            client = managed_client_state(record, target_native["executable"])
            source_client = retained["sourceClient"]
            assert client["session"] == source_client["session"], (name, client)
            assert all(client["pane"][key] == source_client["pane"][key] for key in ("session", "id")), (name, client)
            old = (source_client["client"]["pid"], source_client["client"]["startTime"])
            assert all((process["pid"], process["startTime"]) != old for process in client["processes"]), (name, client)
            identity = proof(name, "alternate", members=2, evidence_name=name + "-target-restored")
            assert identity == retained["identity"], (name, retained["identity"], identity)
            user("printf %s " + shlex.quote(json.dumps({"intent": intent, "client": client}))
                + " > /tmp/creation-evidence/" + name + "-target-restored/terminal-client.json")
        current_marker = json.loads(user("cat " + runtime + "/registration.json"))
        assert current_marker["launch"]["required_native_child_threads"] == 0, current_marker
        assert current_marker["launch"]["codex_path"] == target_native["launcher"], current_marker

    with subtest("install shipped portal, stock Codex and private external providers"):
        user("mkdir -p ~/.local/state/dev-workspaces/codex-homes/fixture " + state + " " + root + "/repos " + root + "/work " + root + "/worktrees ~/origins /tmp/creation-evidence")
        for provider in ("primary", "alternate"):
            token = state + "/codex-homes/fixture/" + provider + ".token"
            user("printf %s " + shlex.quote("fixture-" + provider + "-token") + " > " + token + " && chmod 0600 " + token)
        # Alternate exists only in the host catalog. Naming must import that
        # definition rather than relying on this global primary configuration.
        config = 'model_provider="primary"\nmodel="gpt-5.5"\nmodel_reasoning_effort="xhigh"\napproval_policy="never"\n[features]\ngoals=true\n[model_providers.primary]\nname="Primary fixture"\nbase_url="http://127.0.0.1:2455/primary/v1"\nwire_api="responses"\nrequires_openai_auth=false\nsupports_websockets=false\nstream_max_retries=2\nrequest_max_retries=0\n[model_providers.alternate]\nname="Alternate fixture"\nstream_max_retries=0\nrequest_max_retries=0\n[notice.model_migrations]\n"gpt-5.5" = "gpt-6-sol"\n'
        user("printf %s " + shlex.quote(config) + " > ~/.local/state/dev-workspaces/codex-homes/fixture/config.toml")
        user("systemd-run --user --unit=creation-model --setenv=CREATION_DISTINCT_PROVIDER_AUTH=1 ${python}/bin/python ${../../test/creation_model.py}")
        machine.wait_until_succeeds("curl -fsS http://127.0.0.1:2455/primary/v1/models")
        user("git config --global user.name 'Creation fixture' && git config --global user.email fixture@example.invalid")
        user("git config --global url.file:///home/developer/origins/.insteadOf git@github.com:fixture/")
        user("git init --bare --initial-branch=master ~/origins/workspace.git")
        user("git init --initial-branch=master " + root + " && cd " + root
            + " && printf '/worktrees/\\n' > .gitignore && git add .gitignore && git commit -m 'fixture: workspace' "
            + "&& git remote add origin git@github.com:fixture/workspace.git && git push origin master")
        user("nix-env --profile " + state + "/profile --set " + package)
        # Reproduce an old systemctl-created instance alias whose package was
        # collected. Ordinary registration must repair it before creation starts.
        unit_dir = "/home/developer/.config/systemd/user"
        provider_unit = "workspace-codex-provider@fixture.service"
        stale_template = "/nix/store/00000000000000000000000000000000-dev-workspace-0.2.0/share/systemd/user/workspace-codex-provider@.service"
        user("mkdir -p " + unit_dir + "/default.target.wants && ln -s " + stale_template + " " + unit_dir + "/" + provider_unit
            + " && ln -s " + stale_template + " " + unit_dir + "/default.target.wants/" + provider_unit)
        user(package + "/bin/workspace-host register fixture " + root + " --hostname fixture.workspace.example.test")
        assert user("systemctl --user show " + provider_unit + " -p LoadState --value").strip() == "loaded"
        user("systemctl --user is-enabled " + provider_unit)
        user("systemctl --user start " + provider_unit)
        user("systemctl --user is-active " + provider_unit)
        assert user("readlink -f " + unit_dir + "/" + provider_unit).strip() == package + "/share/systemd/user/workspace-codex-provider@.service"
        for service in ["workspace-codex@fixture", "workspace-portal@fixture", "workspace-tmux@fixture", "workspace-router"]:
            machine.wait_until_succeeds(user_prefix + "systemctl --user is-active " + service)

    try:
    ${pythonBlock (if focusedTui then ''
        with subtest("fresh retained team and unrelated control on the transparent socket epoch"):
            socket = runtime + "/app-server.sock"
            source_native = package_native_identity(package)
            assert source_native["sha256"] == "8880a0384e3020872fe9635098772d10bb436b4fbe4c931ad8625a56dca73e58", source_native
            materialization_cwd = root + "/work/" + user("date +%F").strip() + "-before"
            materialization_rule = {"cwd": materialization_cwd, "active": True}
            user("printf %s " + shlex.quote(json.dumps(materialization_rule)) + " > /tmp/creation-model/observe-materialization.json.new && mv /tmp/creation-model/observe-materialization.json.new /tmp/creation-model/observe-materialization.json")
            user("mv " + socket + " " + socket + ".native")
            user("systemd-run --user --unit=creation-transport ${python}/bin/python ${../../test/creation_transport.py} proxy "
                + socket + " " + socket + ".native")
            machine.wait_until_succeeds(user_prefix + "test -S " + socket)
            for name in ("before", "after"):
                browser("admit", name, provider="alternate", team="development")
                admitted = checkpoint(name)
                if name == "before":
                    binding = {"cwd": materialization_cwd, "requestId": admitted["requestId"], "receiptId": admitted["receiptId"],
                        "slug": admitted.get("slug") or None, "threadId": admitted.get("threadId") or None}
                    user("printf %s " + shlex.quote(json.dumps(binding)) + " > /tmp/creation-model/materialization-admission.json.new && mv /tmp/creation-model/materialization-admission.json.new /tmp/creation-model/materialization-admission.json")
                deadline = time.monotonic() + 180
                while True:
                    ready = status(name)
                    user("printf %s " + shlex.quote(json.dumps(ready))
                        + " > /tmp/creation-evidence/ready-before-followup-" + name + ".json")
                    assert ready["requestId"] == admitted["requestId"] and ready["receiptId"] == admitted["receiptId"], ready
                    if ready["state"] == "ready":
                        break
                    assert ready["state"] not in ("failed", "paused", "conflict", "gone", "cancelled"), ready
                    assert time.monotonic() < deadline, ("creation ready before followup", ready)
                    time.sleep(0.25)
                # Preparation status intentionally has no threadId. CLI completion
                # and the ready manifest independently own the root identity.
                authority_path = "/tmp/creation-evidence/ready-authorities-" + name + ".json"
                expected = {"workspace": root, "cwd": root + "/work/" + ready["slug"], "model": "gpt-5.5", "effort": "high",
                    **{key: ready[key] for key in ("requestId", "receiptId", "slug")}}
                script = ("import json,pathlib,os,yaml; expected=" + repr(expected) + "; "
                    + "base=pathlib.Path(" + repr(portal_state + "/creations") + "); "
                    + "receipt=json.loads((base/(expected['slug']+'.json')).read_text()); "
                    + "complete=json.loads((base/(expected['slug']+'.'+expected['receiptId']+'.complete.json')).read_text()); "
                    + "cwd=pathlib.Path(expected['cwd']); m=yaml.safe_load((cwd/'portal.yml').read_text()); st=os.lstat(cwd); "
                    + "manifest={'slug':m['slug'],'sensitive':m.get('sensitive',False),'codex':{'threadId':m['codex']['thread_id']},"
                    + "'creation':{'state':m['creation']['state'],'initialGoalSent':m['creation'].get('initial_goal_sent',False),'goalSha256':m['creation'].get('goal_sha256')}}; "
                    + "receipt.pop('agentTeamBinding',None); "
                    + "json.dump({'status':" + repr(ready) + ", 'receipt':receipt,'completion':complete,'manifest':manifest,'expected':expected,"
                    + "'tracking':{'cwd':str(cwd),'device':st.st_dev,'inode':st.st_ino}},open(" + repr(authority_path) + ",'w'))")
                user("${python}/bin/python -c " + shlex.quote(script))
                machine.copy_from_machine(authority_path, "materialization")
                binding = json.loads(user(shlex.join(["${python}/bin/python", "${../../test/creation_transport.py}",
                    "ready-identity", authority_path])))
                ready_thread = binding["threadId"]
                if name == "before":
                    user("printf %s " + shlex.quote(json.dumps(binding)) + " > /tmp/creation-model/materialization-admission.json.new && mv /tmp/creation-model/materialization-admission.json.new /tmp/creation-model/materialization-admission.json")
                    try:
                        initial_response = native(ready_thread)
                    finally:
                        materialization_rule["active"] = False
                        user("printf %s " + shlex.quote(json.dumps(materialization_rule)) + " > /tmp/creation-model/observe-materialization.json.new && mv /tmp/creation-model/observe-materialization.json.new /tmp/creation-model/observe-materialization.json")
                        user("cp /tmp/creation-model/materialization-rpc.jsonl /tmp/creation-evidence/materialization-rpc.jsonl")
                        machine.copy_from_machine("/tmp/creation-evidence/materialization-rpc.jsonl", "materialization")
                    expected = {**binding, "model": "gpt-5.5", "modelProvider": "alternate", "effort": "high"}
                    validation_command = shlex.join(["${python}/bin/python", "${../../test/creation_transport.py}",
                        "materialization-proof", "/tmp/creation-model", json.dumps(expected)])
                    validation_status, validation_output = machine.execute(user_prefix + "sh -c " + shlex.quote(
                        validation_command + " > /tmp/creation-evidence/materialization-validation.json"))
                    machine.copy_from_machine("/tmp/creation-evidence/materialization-validation.json", "materialization")
                    assert validation_status == 0, ("materialization boundary", validation_output)
                else:
                    initial_response = native(ready_thread)
                accepted = json.loads(user("cat " + portal_state + "/creations/" + ready["slug"] + ".json"))
                selection = {key: accepted.get(key) for key in ("receiptId", "state", "validated", "model", "effort")}
                request_selection, direct_selection = accepted.get("request"), accepted.get("directTeam")
                selection["request"] = {key: request_selection.get(key) for key in ("slug", "model", "effort", "team", "catalogDigest", "provider")} if isinstance(request_selection, dict) else None
                selection["directTeam"] = {key: direct_selection.get(key) for key in ("id", "leadModel", "leadEffort", "catalogDigest", "teamDigest")} if isinstance(direct_selection, dict) else None
                user("printf %s " + shlex.quote(json.dumps(selection))
                    + " > /tmp/creation-evidence/accepted-before-followup-" + name + ".json")
                assert selection["receiptId"] == ready["receiptId"] and selection["state"] == "ready" and selection["validated"] is True, selection
                assert selection["model"] == "gpt-5.5" and selection["effort"] == "high", selection
                assert isinstance(selection["request"], dict) and isinstance(selection["directTeam"], dict), selection
                assert selection["request"]["slug"] == ready["slug"] and selection["request"]["provider"] == "alternate", selection
                assert selection["request"]["team"] == "development" and selection["request"]["model"] == "gpt-5.5" and selection["request"]["effort"] == "high", selection
                assert selection["directTeam"]["id"] == "development" and selection["directTeam"]["leadModel"] == "gpt-5.5" and selection["directTeam"]["leadEffort"] == "high", selection
                assert selection["request"]["catalogDigest"] and selection["request"]["catalogDigest"] == selection["directTeam"]["catalogDigest"], selection
                initial_native = initial_response["thread"]
                assert initial_native["id"] == ready_thread and initial_native["cwd"] == root + "/work/" + ready["slug"], initial_native
                assert initial_native["modelProvider"] == "alternate" and initial_native["model"] == "gpt-5.5" and initial_native["reasoningEffort"] == "high", initial_native
                browser("finish", name)
                proof(name, "alternate", members=2)
                record = checkpoint(name)
                sync_source(profile, record["slug"])
                permission = recovery_proof(record)
                assert permission["status"]["held"] is False and record["threadId"] in permission["record"]["active_threads"], permission
                managed_client_state(record, source_native["executable"])
        with subtest("lost successful scoped archive reply recovers the same operation on the live daemon"):
            interrupted_provider_case()
    '' else if maintenanceAdmissionOnly then ''
        with subtest("fresh retained team for packaged maintenance admission"):
            browser("create", "explicit", provider="alternate", team="development")
            record = checkpoint("explicit")
            identity = proof("explicit", "alternate", members=2)
            history = native(record["threadId"])["thread"]
            assert history["turns"] and all(turn["status"] == "completed" for turn in history["turns"]), history
            source_native = package_native_identity(package)
            assert source_native["sha256"] == "8880a0384e3020872fe9635098772d10bb436b4fbe4c931ad8625a56dca73e58", source_native
            permission = recovery_proof(record)
            assert permission["status"]["held"] is False, permission
            assert permission["record"]["thread_id"] == record["threadId"], permission
            assert permission["record"]["socket_path"] == runtime + "/app-server.sock", permission
            assert record["threadId"] in permission["record"]["active_threads"], permission
            source_client = managed_client_state(record, source_native["executable"])
            captured_roots = {"explicit": {"identity": identity, "sourceClient": source_client}}
        with subtest("public maintenance bootstrap uses a packaged native identity change"):
            maintenance_bootstrap(captured_roots)
    '' else ''
        with subtest("custom explicit default provider, solo, notifications, attachment and follow-up"):
            browser("create", "explicit", provider="primary", attachment=True)
            proof("explicit", "primary")
            record = checkpoint("explicit")
            initial = [entry for entry in record["transcript"]["entries"] if record["marker"] in entry.get("text", "")]
            assert any("creation-evidence.txt" in json.dumps(entry) for entry in initial), initial
            initial_native = [item for turn in native(record["threadId"])["thread"]["turns"] for item in turn["items"]
                if item.get("type") == "userMessage" and record["marker"] in json.dumps(item)]
            assert len(initial_native) == 1, initial_native
            text = "\n".join(part["text"] for part in initial_native[0]["content"] if part["type"] == "text")
            attachments = json.loads(text.split("Attached local files (inputs; keep these files outside version control):\n", 1)[1])
            assert len(attachments) == 1 and attachments[0]["name"] == "creation-evidence.txt", attachments
            assert attachments[0]["path"].startswith(portal_state + "/uploads/files/"), attachments
            assert user("cat " + shlex.quote(attachments[0]["path"])) == "CREATION_ATTACHMENT_CONTENT"

        with subtest("native ascending managed input proof survives active tail and portal process restart"):
            record = checkpoint("explicit")
            before = native(record["threadId"])["thread"]
            prior_turns = {turn["id"] for turn in before["turns"]}
            assert prior_turns, before
            expected = []
            permits = []
            user("rm -f /tmp/creation-model/turn-entered /tmp/creation-items-proof.json && touch /tmp/creation-model/hold-turn")
            try:
                turn = None
                for index in range(2):
                    identity = str(uuid.uuid4())
                    text = "CREATION_MANAGED_PREFIX_" + str(index) + "_" + identity
                    receipt = managed_input(record, text, identity)
                    assert receipt["clientUserMessageId"] == identity, receipt
                    if turn is None:
                        turn = receipt["turnId"]
                        assert turn not in prior_turns and not receipt.get("steered", False), receipt
                        marker = text
                        held = held_model_request(marker, text)
                    else:
                        assert receipt["turnId"] == turn and receipt["steered"], receipt
                        held = advance_model_response(held, marker, text, permits)
                    expected.append({"clientId": identity, "text": text})
                first = prefix_proof(record, turn, expected)
                identity = str(uuid.uuid4())
                text = "CREATION_MANAGED_PREFIX_2_" + identity
                receipt = managed_input(record, text, identity)
                assert receipt["turnId"] == turn and receipt["clientUserMessageId"] == identity and receipt["steered"], receipt
                expected.append({"clientId": identity, "text": text})
                held = advance_model_response(held, marker, text, permits)
                tail = prefix_proof(record, turn, expected)
                assert tail["count"] == first["count"] + 1, tail
                old_pid = user("systemctl --user show workspace-portal@fixture.service -p MainPID --value").strip()
                restart()
                new_pid = user("systemctl --user show workspace-portal@fixture.service -p MainPID --value").strip()
                assert new_pid != old_pid and int(new_pid) > 0, (old_pid, new_pid)
                identity = str(uuid.uuid4())
                text = "CREATION_MANAGED_PREFIX_3_" + identity
                receipt = managed_input(record, text, identity)
                assert receipt["turnId"] == turn and receipt["clientUserMessageId"] == identity and receipt["steered"], receipt
                expected.append({"clientId": identity, "text": text})
                held = advance_model_response(held, marker, text, permits)
                recovered = prefix_proof(record, turn, expected)
                assert recovered["count"] == 4 and recovered["users"][:3] == tail["users"], recovered
                print(json.dumps({"nativeInputPrefix": recovered, "oldPortalPID": old_pid, "newPortalPID": new_pid}))
            finally:
                user("${python}/bin/python -c " + shlex.quote("from pathlib import Path; "
                    + "[Path(path).unlink(missing_ok=True) for path in "
                    + repr(["/tmp/creation-model/hold-turn", *permits]) + "]"))
            # Poll the native state afresh, rather than a captured JSON string.
            machine.wait_until_succeeds(user_prefix + "sh -c " + shlex.quote(
                "${python}/bin/python ${../../test/creation_transport.py} read " + runtime + "/app-server.sock "
                + shlex.quote(record["threadId"]) + " | ${python}/bin/python -c " + shlex.quote(
                    "import sys,json; t=json.load(sys.stdin)['thread']; assert next(v for v in t['turns'] if v['id']==" + repr(turn) + ")['status']=='completed'")))
            after = native(record["threadId"])["thread"]
            assert after["model"] == before["model"] == "gpt-5.5", after
            assert after["reasoningEffort"] == before["reasoningEffort"] == "high", after
            assert {item["id"] for item in after["turns"]} == prior_turns | {turn}, after
            users = [item for value in after["turns"] if value["id"] == turn for item in value["items"] if item["type"] == "userMessage"]
            assert [item["clientId"] for item in users] == [item["clientId"] for item in expected], users

        with subtest("automatic naming, inherited default provider, sensitive retained team and notifications off"):
            browser("create", "automatic", auto=True, team="development", sensitive=True, notifications=False)
            proof("automatic", "primary", members=2, sensitive=True)
            naming_proof("automatic", "primary")
            assert checkpoint("automatic")["slug"].endswith("accepted-session")

        with subtest("alternate provider, fork and approved-plan destinations preserve source"):
            browser("create", "alternate", provider="alternate")
            proof("alternate", "alternate")
            browser("fork", "explicit", provider="alternate")
            browser("plan", "explicit", provider="alternate", team="development")
            proof("explicit", "primary")
            proof("explicit-fork", "alternate")
            proof("explicit-plan", "alternate", members=2)
            for kind in ["fork", "plan"]:
                record = checkpoint("explicit-" + kind)
                assert native(record["threadId"])["thread"]["modelProvider"] == "alternate", record

        with subtest("frozen fork settings can be restored while its terminal is subscribed"):
            browser("change-effort", "explicit-fork")
            record = checkpoint("explicit-fork")
            assert native(record["threadId"])["thread"]["reasoningEffort"] == "xhigh"
            slug = record["slug"]
            args = [package + "/bin/workspace-portal", "thread", "ensure-settings", "--require-runtime",
                "--thread-id", record["threadId"], "--socket", runtime + "/app-server.sock",
                "--user-state-root", state, "--workspace", root, "--session-slug", slug,
                "--cwd", root + "/work/" + slug, "--worktrees-dir", root + "/worktrees/" + slug,
                "--portal-base-url", "https://fixture.workspace.example.test",
                "--portal-url", "https://fixture.workspace.example.test/" + slug + "/",
                "--authority-dir", runtime + "/authority", "--tmux-socket", runtime + "/tmux.sock",
                "--codex-command", state + "/codex/current/bin/codex", "--codex-version", "0.167.0",
                "--portal-command", package + "/bin/workspace-portal", "--model", "gpt-5.5", "--effort", "high"]
            wrong = args.copy()
            wrong[wrong.index("--cwd") + 1] += "-foreign"
            machine.fail(user_prefix + "sh -c " + shlex.quote(shlex.join(wrong)))
            assert native(record["threadId"])["thread"]["reasoningEffort"] == "xhigh"
            restored = json.loads(user(shlex.join(args)))
            assert restored == {"model": "gpt-5.5", "reasoningEffort": "high"}, restored
            proof("explicit-fork", "alternate")

        with subtest("native naming retry reaches success without manual choice"):
            user("printf 2 > /tmp/creation-model/naming-overloads")
            user("touch /tmp/creation-model/naming-retry-advice")
            browser("create", "naming-native-retry", auto=True, provider="primary")
            proof("naming-native-retry", "primary")
            naming_proof("naming-native-retry", "primary", attempts=3)
            assert checkpoint("naming-native-retry")["preparationAttempt"] == 1
            user("rm /tmp/creation-model/naming-overloads")
            user("rm /tmp/creation-model/naming-retry-advice")

        with subtest("settled terminal transient naming error permits one fresh call"):
            user("printf 1 > /tmp/creation-model/naming-overloads")
            browser("create", "naming-fresh-retry", auto=True, provider="alternate")
            proof("naming-fresh-retry", "alternate")
            naming_proof("naming-fresh-retry", "alternate", attempts=2)
            assert checkpoint("naming-fresh-retry")["preparationAttempt"] == 1
            user("rm /tmp/creation-model/naming-overloads")

        with subtest("persistent transient naming error stops after two utility calls"):
            user("printf 100 > /tmp/creation-model/naming-overloads")
            browser("admit", "naming-persistent", auto=True, provider="alternate")
            browser("failed", "naming-persistent")
            stopped = status("naming-persistent")
            assert stopped["canSetName"] and not stopped.get("slug"), stopped
            naming_proof("naming-persistent", "alternate", attempts=2)
            assert user("cat /tmp/creation-model/naming-overloads").strip() == "98"
            browser("name", "naming-persistent", customName="provider-recovery")
            proof("naming-persistent", "alternate")
            user("rm /tmp/creation-model/naming-overloads")

        with subtest("failed naming stops before reservation and keeps manual recovery"):
            user("touch /tmp/creation-model/bad-name")
            browser("admit", "naming-error", auto=True, provider="alternate", attachment=True)
            browser("failed", "naming-error")
            stopped = status("naming-error")
            assert stopped["canSetName"] and not stopped.get("slug"), stopped
            assert not stopped.get("canonicalUrl"), stopped
            mapping = json.loads(user("cat " + portal_state + "/session-preparations/"
                + checkpoint("naming-error")["requestId"] + ".json"))
            assert not mapping.get("base") and not mapping.get("slug") and not mapping.get("handoff"), mapping
            browser("name", "naming-error", customName="php-upload-progress-mitigation")
            proof("naming-error", "alternate")
            naming_proof("naming-error", "alternate")
            first = checkpoint("naming-error")
            assert first["slug"].endswith("-php-upload-progress-mitigation"), first
            initial = [entry for entry in first["transcript"]["entries"] if first["marker"] in entry.get("text", "")]
            assert any("creation-evidence.txt" in json.dumps(entry) for entry in initial), initial
            user("rm /tmp/creation-model/bad-name")

        # Transport faults forward only real native responses. The application
        # binaries, protocol and ordinary service endpoints remain unchanged.
        socket = runtime + "/app-server.sock"
        source_native = package_native_identity(package)
        source_permission = recovery_proof(checkpoint("explicit"))
        assert source_permission["status"]["held"] is False, source_permission
        source_epoch = source_permission["record"]["socket_identity"]
        assert checkpoint("explicit")["threadId"] in source_permission["record"]["active_threads"], source_permission
        source_listener = user("readlink -f " + socket).strip()
        source_service = unit_state(native_unit)
        user("printf %s " + shlex.quote(json.dumps({"permission": source_permission, "native": source_native,
            "listener": source_listener, "service": source_service})) + " > /tmp/creation-evidence/proxy-source.json")
        proxy_roots = {}
        user("mv " + socket + " " + socket + ".native")
        user("systemd-run --user --unit=creation-transport ${python}/bin/python ${../../test/creation_transport.py} proxy "
            + socket + " " + socket + ".native")
        machine.wait_until_succeeds(user_prefix + "test -S " + socket)

        with subtest("reload, interruption and retry before native creation preserve admitted settings"):
            user("touch /tmp/creation-model/hold-root-request")
            browser("admit", "before", provider="alternate", team="development")
            machine.wait_until_succeeds("test -e /tmp/creation-model/root-entered")
            restart()
            user("rm /tmp/creation-model/hold-root-request")
            browser("retry", "before")
            proxy_roots["before"] = {"identity": proof("before", "alternate", members=2),
                "client": managed_client_state(checkpoint("before"), source_native["executable"])}

        with subtest("lost accepted initial-send reply and portal restart do not repeat the user turn"):
            user("rm -f /tmp/creation-model/send-accepted")
            user("touch /tmp/creation-model/hold-send-reply /tmp/creation-model/hold-turn")
            browser("admit", "after", provider="alternate", team="development")
            machine.wait_until_succeeds("test -e /tmp/creation-model/send-accepted")
            accepted = json.loads(user("cat /tmp/creation-model/send-accepted"))
            accepted_native = native(accepted["threadId"])["thread"]
            assert accepted_native["cwd"] == root + "/work/" + status("after")["slug"], accepted_native
            assert any(turn["id"] == accepted["turnId"] and any(item.get("type") == "userMessage" and
                "CREATION_INITIAL_after" in json.dumps(item) for item in turn["items"])
                for turn in accepted_native["turns"]), accepted_native
            restart()
            user("rm /tmp/creation-model/hold-send-reply /tmp/creation-model/hold-turn")
            browser("retry", "after")
            proxy_roots["after"] = {"identity": proof("after", "alternate", members=2),
                "client": managed_client_state(checkpoint("after"), source_native["executable"])}
            for name, retained in proxy_roots.items():
                retained["proxyPermission"] = recovery_proof(checkpoint(name))
                assert retained["proxyPermission"]["status"]["held"] is False, retained["proxyPermission"]
                assert checkpoint(name)["threadId"] in retained["proxyPermission"]["record"]["active_threads"], retained["proxyPermission"]
                assert retained["proxyPermission"]["record"]["socket_identity"] != source_epoch, retained["proxyPermission"]
                user("printf %s " + shlex.quote(json.dumps(retained))
                    + " > /tmp/creation-evidence/" + name + "/proxy-permission-client.json")

        with subtest("lost successful scoped archive reply recovers the same operation on the live daemon"):
            interrupted_provider_case()
            # Retain ordinary creation identities for the source-epoch proof;
            # added recovery turns have their independent receipts above.
            for name in proxy_roots:
                proxy_roots[name]["identity"] = proof(name, "alternate", members=2,
                    evidence_name=name + "-after-provider-recovery")
                # Provider recovery replaced the affected terminal client.
                proxy_roots[name]["client"] = managed_client_state(checkpoint(name), source_native["executable"])
        user("systemctl --user stop creation-transport && rm -f " + socket + " && mv " + socket + ".native " + socket)

        with subtest("proxy-created roots regain source permission and actual managed clients"):
            # The proxy was a different permission epoch. Ordinary start publishes
            # root permission after its first reconcile; one sync then ensures the client.
            assert user("readlink -f " + profile).strip() == package
            assert user("readlink -f " + socket).strip() == source_listener
            current_service = unit_state(native_unit)
            assert current_service["ActiveState"] == "active" and current_service["ControlPID"] == "0", current_service
            assert all(current_service[key] == source_service[key] for key in ("MainPID", "InvocationID")), current_service
            assert user("readlink /proc/" + current_service["MainPID"] + "/exe").strip() == source_native["executable"]
            anchored = recovery_proof(checkpoint("explicit"))
            assert anchored["status"]["held"] is False and anchored["record"]["socket_identity"] == source_epoch, anchored
            for name, retained in proxy_roots.items():
                record = checkpoint(name)
                before = recovery_proof(record)
                assert before["record"]["socket_identity"] in (
                    retained["proxyPermission"]["record"]["socket_identity"], source_epoch), before
                user("printf %s " + shlex.quote(json.dumps(before))
                    + " > /tmp/creation-evidence/" + name + "/restored-listener-permission.json")
                started = json.loads(user("cd " + root + " && " + shlex.join([
                    package + "/bin/dev-session", "start", record["slug"], "--as-is", "--no-attach", "--json"])))
                assert started["slug"] == record["slug"] and started["threadId"] == record["threadId"], started
                sync_source(package, record["slug"])
                permission = recovery_proof(record)
                assert permission["status"]["held"] is False and permission["record"]["socket_identity"] == source_epoch, permission
                assert record["threadId"] in permission["record"]["active_threads"], permission
                identity = proof(name, "alternate", members=2, evidence_name=name + "-source-restored")
                assert identity == retained["identity"], (name, retained["identity"], identity)
                client = managed_client_state(record, source_native["executable"])
                assert client["session"] == retained["client"]["session"], (name, client)
                assert all(client["pane"][key] == retained["client"]["pane"][key] for key in ("session", "id", "pid")), (name, client)
                retained["sourceClient"] = client
                user("printf %s " + shlex.quote(json.dumps({"permission": permission, "client": client, "start": started}))
                    + " > /tmp/creation-evidence/" + name + "-source-restored/permission-client.json")

        with subtest("ordinary deletion fences the stale creation request"):
            record = checkpoint("alternate")
            browser("delete-stale", "alternate")
            user("test ! -e " + root + "/work/" + record["slug"])

        with subtest("public scoped provider round trip restores actual tmux input and preserves unrelated work"):
            provider_terminal_case()

        with subtest("public maintenance bootstrap uses a packaged native identity change"):
            maintenance_bootstrap(proxy_roots)

        with subtest("packaged cooperative pause selects profile and restores the captured team once"):
            maintenance_case("cooperative", "original", cooperative=True)

        with subtest("packaged deadline interrupts while public Stop suppresses only its member"):
            maintenance_case("deadline-stop", "target", stop=True)

        with subtest("target startup failure retains the same operation and never resumes before recovery"):
            maintenance_case("startup-failure", "original", fail=True)

        with subtest("production native enforces guest system provider policy and recovers the same histories"):
            # Last, serial phase: no ordinary fixture client/worker can observe
            # temporary /etc policy. The external synthetic model reads no policy.
            user("systemctl --user stop 'workspace-*' creation-transport.service")
            policy_started = time.monotonic()
            command = ["${python}/bin/python", "${../../test/creation_transport.py}", "production-policy",
                package, "${../../test/creation_model.py}", "/tmp/creation-evidence/production-policy"]
            result = json.loads(machine.succeed(shlex.join(command), timeout=600))
            assert result["scenarios"] == ["strict-mismatch", "missing-definition", "malformed-current",
                "managed-selection", "managed-definition"], result
            print(json.dumps({"productionPolicy": result, "elapsedSeconds": time.monotonic() - policy_started}))

    '')}
    except Exception:
        try:
            # Later failure-only allocation sample, before exports write more files.
            storage_sample = {"sampledAtUnix": time.time(), "stage": "after-failure-before-exports"}
            storage_paths = ["/tmp/creation-model", "/tmp/creation-evidence",
                "/home/developer/.local/state/dev-workspaces/codex-homes/fixture", portal_state, "/var/log"]
            storage_script = ("${pkgs.coreutils}/bin/df -P -B1 -- \"$@\"; storage_blocks=$?; "
                "${pkgs.coreutils}/bin/df -P -i -- \"$@\"; storage_inodes=$?; "
                "${pkgs.coreutils}/bin/du -s -B1 -- \"$@\"; storage_allocated=$?; "
                "printf 'dfBlocksStatus=%s dfInodesStatus=%s duAllocatedStatus=%s\\n' \"$storage_blocks\" \"$storage_inodes\" \"$storage_allocated\"; "
                "test \"$storage_blocks:$storage_inodes:$storage_allocated\" = 0:0:0")
            storage_status, storage_output = machine.execute(shlex.join(["${pkgs.coreutils}/bin/timeout", "10",
                "sh", "-c", storage_script, "failure-storage"] + storage_paths) + " 2>&1", timeout=10)
            storage_sample.update(status=storage_status, available=storage_status == 0, output=storage_output)
            print(json.dumps({"failureStorageSample": storage_sample}))
        except Exception as storage_error:
            try:
                print(json.dumps({"failureStorageSample": {"sampledAtUnix": time.time(),
                    "stage": "after-failure-before-exports", "available": False, "error": type(storage_error).__name__}}))
            except Exception:
                pass
        try:
            import textwrap
            timing_export = textwrap.dedent("""
                import datetime, json, os, pathlib
                root = pathlib.Path('/tmp/creation-evidence')
                limit = 1024 * 1024
                metadata = {'capturedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                            'limitBytes': limit, 'originalBytes': None, 'exportedBytes': 0, 'truncated': None}
                try:
                    with open('/var/log/nginx/creation-timing.jsonl', 'rb') as source:
                        size = os.fstat(source.fileno()).st_size
                        metadata.update(originalBytes=size, truncated=size > limit)
                        source.seek(max(0, size - limit))
                        data = source.read(min(size, limit))
                except FileNotFoundError:
                    metadata['missing'] = True
                except OSError as error:
                    metadata['readError'] = str(error)[:500]
                else:
                    try:
                        (root / 'nginx-timing.jsonl').write_bytes(data)
                        metadata['exportedBytes'] = len(data)
                    except OSError as error:
                        metadata['exportError'] = str(error)[:500]
                (root / 'nginx-timing-metadata.json').write_text(json.dumps(metadata, indent=2))
            """)
            timing_status, timing_output = machine.execute("${python}/bin/python -c " + shlex.quote(timing_export))
            print(json.dumps({"timingExportStatus": timing_status, "output": timing_output[:1000]}))
        except Exception as timing_error:
            print(json.dumps({"timingExportError": str(timing_error)[:1000]}))
        try:
            # These reads occur after the browser failure, never as readiness proof.
            diagnostic_status, diagnostic_output = machine.execute(user_prefix + "sh -c " + shlex.quote(
                "${python}/bin/python ${../../test/creation_passive_diagnostic.py} " + shlex.quote(runtime)
                + " ${python}/bin/python ${../../test/creation_transport.py}"))
            # Relay one whole safe line; never fall back to raw child output.
            passive_summary = {"outcome": "unavailable"}
            try:
                import datetime, math, re, uuid
                lines = [line for line in diagnostic_output.splitlines()
                    if re.match(r'^\s*\{.*"postFailurePassiveSummary"', line)]
                if len(lines) != 1 or len(lines[0].encode()) > 8192:
                    raise ValueError()
                envelope = json.loads(lines[0])
                candidate = envelope["postFailurePassiveSummary"]
                if set(envelope) != {"postFailurePassiveSummary"} or set(candidate) != {"identity", "native", "portal", "timing"}:
                    raise ValueError()
                # Fixed groups and scalar vocabularies also reject accidental payload output.
                fields = {
                    "identity": {"state", "requestId", "receiptId", "threadId", "slug", "provider"},
                    "native": {"outcome", "exit", "rootMatches", "providerMatches", "entryCount", "initialCount",
                        "initialTurnCount", "initialTurnStatus", "replyInInitialTurnCount"},
                    "timing": {"failureAt", "capturedAt", "nativeStartedAt", "nativeEndedAt", "portalStartedAt",
                        "portalEndedAt", "nativeElapsedSeconds", "portalElapsedSeconds"}}
                fields["portal"] = fields["native"] | {"httpStatus", "hasOlder"}
                for group, keys in fields.items():
                    if set(candidate[group]) != keys:
                        raise ValueError()
                    for key, value in candidate[group].items():
                        if value is None:
                            continue
                        if key in ("requestId", "receiptId", "threadId"):
                            valid = isinstance(value, str) and str(uuid.UUID(value)) == value
                        elif key == "slug":
                            valid = isinstance(value, str) and len(value) <= 256 and re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9-]+", value)
                        elif key.endswith("At"):
                            valid = isinstance(value, str) and len(value) <= 64 and re.fullmatch(r"[0-9T:.+Z-]+", value) and datetime.datetime.fromisoformat(value).tzinfo
                        elif key.endswith("ElapsedSeconds"):
                            valid = type(value) in (int, float) and math.isfinite(value) and value >= 0
                        elif key in ("rootMatches", "providerMatches", "hasOlder"):
                            valid = type(value) is bool
                        elif key in ("exit", "httpStatus") or key.endswith("Count"):
                            valid = type(value) is int and (key == "exit" or value >= 0)
                        else:
                            valid = value in {"state": ("valid", "invalid", "unavailable"),
                                "provider": ("primary", "alternate"),
                                "initialTurnStatus": ("completed", "inProgress", "interrupted", "failed"),
                                "outcome": ("ok", "skipped", "timeout", "command-error", "invalid-json",
                                    "invalid-shape", "identity-mismatch", "unavailable")}[key]
                        if not valid:
                            raise ValueError()
                passive_summary = candidate
            except (ValueError, KeyError, TypeError, AttributeError):
                pass
            print(json.dumps({"postFailureDiagnosticStatus": diagnostic_status, "postFailurePassiveSummary": passive_summary}), flush=True)
            print(user("journalctl --user --no-pager -n 180 -u workspace-portal@fixture -u workspace-codex@fixture -u creation-transport"))
            print(user("systemctl --user show " + native_unit + " " + portal_unit + " " + router_unit
                + " --property=ActiveState,SubState,MainPID,ControlPID,InvocationID,Result"))
            maintenance_journal_status, maintenance_journal_output = machine.execute(user_prefix + "sh -c " + shlex.quote(
                "journalctl --user --no-pager -n 120 -u 'workspace-maintenance@*' -u 'creation-maintenance-*'"))
            print(json.dumps({"maintenanceJournalStatus": maintenance_journal_status, "output": maintenance_journal_output}))
            print(user("find /tmp -maxdepth 1 -name 'creation-*.json' -exec cat {} \\;"))
            naming_script = ("import json,pathlib,runpy; root=pathlib.Path('/tmp/creation-model'); "
                "rows=runpy.run_path('${../../test/creation_model.py}')['request_rows']; naming=[]\n"
                "for row in rows():\n"
                " if row['body'].get('model')=='gpt-6-luna': naming.append({k:row.get(k) for k in ['index','provider','overload','retryAdvice']})\n"
                "print(json.dumps({'namingResponses':naming, "
                "'overloadsRemaining':(root/'naming-overloads').read_text() if (root/'naming-overloads').exists() else None, "
                "'retryAdviceEnabled':(root/'naming-retry-advice').exists()}))")
            print(user("${python}/bin/python -c " + shlex.quote(naming_script)))
        except Exception as diagnostic_error:
            print(json.dumps({"failureDiagnosticError": str(diagnostic_error)[:1000]}))
        raise
    finally:
    ${pythonBlock (pkgs.lib.optionalString focusedTui ''
        teardown_error = None
        try:
            user("systemctl --user stop creation-transport.service")
            transport = unit_state("creation-transport.service")
            assert transport["MainPID"] == "0" and transport["ActiveState"] == "inactive", transport
            user("if test -f /tmp/creation-model/materialization-rpc.jsonl; then cp /tmp/creation-model/materialization-rpc.jsonl /tmp/creation-evidence/materialization-rpc.jsonl; fi")
            user("rm -f " + socket + " && mv " + socket + ".native " + socket)
        except Exception as error:
            teardown_error = str(error)
    '')}
        machine.copy_from_vm("/tmp/creation-evidence", "creation-evidence")
        machine.copy_from_vm("/tmp/creation-model", "creation-model")
    ${pythonBlock (pkgs.lib.optionalString focusedTui ''
        assert teardown_error is None, ("focused transport teardown failed", teardown_error)
    '')}
  '';
}
