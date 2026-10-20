{
  pkgs,
  self,
  mkPackage,
}:
let
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
  python = pkgs.python3.withPackages (p: [
    p.websockets
    p.pyyaml
  ]);
in
pkgs.testers.runNixOSTest {
  name = "dev-workspace-session-creation";
  nodes.machine = {
    imports = [ self.nixosModules.host ];
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
            tokenFile = "/home/developer/.codex/fixture.token";
          })
          [
            "primary"
            "alternate"
          ];
    };
    networking.extraHosts = "127.0.0.1 fixture.workspace.example.test";
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
      additionalPaths = [
        package
        pkgs.playwright-driver
      ];
    };
    system.stateVersion = "26.05";
  };
  testScript = ''
    import hashlib
    import json
    import shlex

    machine.start()
    machine.wait_for_unit("user@1000.service")
    package = "${package}"
    root = "/home/developer/workspace"
    state = "/home/developer/.local/state/dev-workspaces"
    runtime = "/run/user/1000/dev-workspaces/fixture"
    portal_state = state + "/portal/workspace-" + hashlib.sha256(root.encode()).hexdigest()[:16]
    user_prefix = "runuser -u developer -- env XDG_RUNTIME_DIR=/run/user/1000 "

    def user(command):
        return machine.succeed(user_prefix + "sh -c " + shlex.quote(command))

    def browser(action, name, **options):
        result = json.loads(user("PLAYWRIGHT_MODULE=${pkgs.playwright-driver} "
            "CHROMIUM_EXECUTABLE=${pkgs.chromium}/bin/chromium "
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

    def native(identity):
        return json.loads(user("${python}/bin/python ${../../test/creation_transport.py} read "
            + runtime + "/app-server.sock " + shlex.quote(identity)))

    def naming_proof(name, provider, attempts=1):
        marker = checkpoint(name)["marker"]
        # Keep large request bodies inside the guest. Ignore an incomplete last
        # line from unrelated inference still being logged after naming finished.
        script = ("import json; marker=" + repr(marker) + "; "
            + "rows=[json.loads(line) for line in open('/tmp/creation-model/requests.jsonl') if line.endswith('\\n')]; "
            + "print(json.dumps([row['provider'] for row in rows if row['body'].get('model')=='gpt-6-luna' "
            + "and marker in json.dumps(row['body'].get('input',[]))]))")
        routes = json.loads(user("${python}/bin/python -c " + shlex.quote(script)))
        assert routes == [provider] * attempts, routes

    def proof(name, provider, members=0, sensitive=False):
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
        roster = json.loads(user("cd " + root + " && " + package + "/bin/dev-session team list " + slug + " --as-is"))
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
            "files=list((pathlib.Path.home()/'.codex').glob('state_*.sqlite')); "
            "assert len(files)==1,files; "
            "db=sqlite3.connect(files[0].as_uri()+'?mode=ro',uri=True); "
            "print(json.dumps(db.execute('SELECT id FROM threads WHERE cwd=? AND archived=0',"
            + repr((directory,)) + ").fetchall()))")))
        assert {row[0] for row in persisted} == set(identities), persisted
        if sensitive:
            user("git -C " + root + " check-ignore work/" + slug + "/state.md")
        # The real Go ready proof has already passed; retain independent native
        # identities and durable files so a green browser cannot mask duplicates.
        user("mkdir -p /tmp/creation-evidence/" + name + " && cp " + receipt_path + " " + evidence_path + " "
            + directory + "/portal.yml /tmp/creation-evidence/" + name)
        if record["kind"] != "fork":
            user("cp " + journal_path + " /tmp/creation-evidence/" + name)
        user("printf %s " + shlex.quote(json.dumps(metadata)) + " > /tmp/creation-evidence/" + name + "/native.json")

    with subtest("install shipped portal, stock Codex and private external providers"):
        user("mkdir -p ~/.codex " + state + " " + root + "/repos " + root + "/work " + root + "/worktrees ~/origins /tmp/creation-evidence")
        user("printf fixture-private-token > ~/.codex/fixture.token && chmod 0600 ~/.codex/fixture.token")
        # Alternate exists only in the host catalog. Naming must import that
        # definition rather than relying on this global primary configuration.
        config = 'model_provider="primary"\nmodel="gpt-5.5"\nmodel_reasoning_effort="xhigh"\napproval_policy="never"\n[model_providers.primary]\nname="Primary fixture"\nbase_url="http://127.0.0.1:2455/primary/v1"\nwire_api="responses"\nrequires_openai_auth=false\nsupports_websockets=false\n'
        user("printf %s " + shlex.quote(config) + " > ~/.codex/config.toml")
        user("systemd-run --user --unit=creation-model ${python}/bin/python ${../../test/creation_model.py}")
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
                "--codex-command", state + "/codex/current/bin/codex", "--codex-version", "0.163.0",
                "--portal-command", package + "/bin/workspace-portal", "--model", "gpt-5.5", "--effort", "high"]
            wrong = args.copy()
            wrong[wrong.index("--cwd") + 1] += "-foreign"
            machine.fail(user_prefix + "sh -c " + shlex.quote(shlex.join(wrong)))
            assert native(record["threadId"])["thread"]["reasoningEffort"] == "xhigh"
            restored = json.loads(user(shlex.join(args)))
            assert restored == {"model": "gpt-5.5", "reasoningEffort": "high"}, restored
            proof("explicit-fork", "alternate")

        with subtest("failed naming creates usable sessions with distinct fallback names"):
            user("touch /tmp/creation-model/bad-name")
            browser("create", "naming-error", auto=True, provider="alternate", attachment=True)
            proof("naming-error", "alternate")
            naming_proof("naming-error", "alternate")
            first = checkpoint("naming-error")
            assert first["slug"].endswith("-development-session"), first
            initial = [entry for entry in first["transcript"]["entries"] if first["marker"] in entry.get("text", "")]
            assert any("creation-evidence.txt" in json.dumps(entry) for entry in initial), initial
            browser("create", "naming-collision", auto=True)
            proof("naming-collision", "primary")
            naming_proof("naming-collision", "primary")
            second = checkpoint("naming-collision")
            assert second["slug"].startswith(first["slug"] + "-"), second
            assert first["threadId"] != second["threadId"], (first, second)
            user("rm /tmp/creation-model/bad-name")

        # Transport faults forward only real native responses. The application
        # binaries, protocol and ordinary service endpoints remain unchanged.
        socket = runtime + "/app-server.sock"
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
            proof("before", "alternate", members=2)

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
            proof("after", "alternate", members=2)
            user("systemctl --user stop creation-transport && rm -f " + socket + " && mv " + socket + ".native " + socket)

        with subtest("ordinary deletion fences the stale creation request"):
            record = checkpoint("alternate")
            browser("delete-stale", "alternate")
            user("test ! -e " + root + "/work/" + record["slug"])
    except Exception:
        print(user("journalctl --user --no-pager -n 180 -u workspace-portal@fixture -u workspace-codex@fixture -u creation-transport"))
        print(user("find /tmp -maxdepth 1 -name 'creation-*.json' -exec cat {} \\;"))
        raise
    finally:
        machine.copy_from_vm("/tmp/creation-evidence", "creation-evidence")
        machine.copy_from_vm("/tmp/creation-model", "creation-model")
  '';
}
