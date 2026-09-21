{
  mkPackage,
  pkgs,
}:
let
  designPolicy = {
    default = "high";
    simple = "high";
    allowed = [ "high" "xhigh" ];
    simple_requires_reason = true;
    followup = "retain";
  };
  implementationPolicy = {
    default = "xhigh";
    simple = "high";
    allowed = [ "high" "xhigh" ];
    simple_requires_reason = true;
    followup = "retain";
  };
  lifecycle = {
    startup = "on_demand";
    communication = "lead_mediated";
    reviewer_reuse = "same_change";
  };
  validTeamConfig = {
    schema_version = 3;
    default_team = "development";
    default_development_team = "development";
    capacity.required_native_child_threads = 4;
    work_policy = {
      design = designPolicy;
      implementation = implementationPolicy;
    };
    teams = {
      solo = {
        description = "One agent for investigation or discussion.";
        mode = "solo";
        design_owner = "team_lead";
        service_policy = "unmanaged";
        max_open_agents = 0;
        inherit lifecycle;
        routing = { };
        roles.team_lead = {
          model = "model_lead";
          effort = "high";
          behavior = "team_lead";
          lifetime = "session";
          allowed_efforts = [ "high" "xhigh" ];
          access = "workspace_write";
          fresh_context = false;
        };
      };
      development = {
        description = "A coordinated development team.";
        mode = "development";
        design_owner = "designer";
        service_policy = "non_priority";
        max_open_agents = 4;
        inherit lifecycle;
        routing = {
          design_simple_effort = "high";
          implementer_simple_effort = "high";
          low_risk_review_role = "reviewer";
        };
        roles = {
          team_lead = {
            model = "model_lead";
            effort = "high";
            behavior = "team_lead";
            lifetime = "session";
            allowed_efforts = [ "high" "xhigh" ];
            access = "workspace_write";
            fresh_context = false;
          };
          designer = {
            model = "model_designer";
            effort = "high";
            behavior = "designer";
            lifetime = "initiative";
            allowed_efforts = [ "high" "xhigh" ];
            access = "read_only";
            fresh_context = false;
          };
          implementer = {
            model = "model_implementer";
            effort = "xhigh";
            behavior = "implementer";
            lifetime = "work_unit";
            allowed_efforts = [ "high" "xhigh" ];
            access = "workspace_write";
            fresh_context = false;
          };
          reviewer = {
            model = "model_reviewer";
            effort = "high";
            behavior = "reviewer";
            lifetime = "review_cycle";
            allowed_efforts = [ "high" "xhigh" ];
            access = "read_only";
            fresh_context = true;
          };
        };
      };
    };
    utilities.verification_watcher = {
      model = "model_watcher";
      effort = "low";
      behavior = "verification_watcher";
      lifetime = "operation";
      access = "workspace_write";
      startup = "on_demand";
      max_concurrent = 1;
      required_for = [ "long_check" "ci_wait" ];
    };
  };
  evaluate =
    teamConfig:
    builtins.tryEval (builtins.deepSeq (mkPackage {
      inherit pkgs teamConfig;
    }) true);
  invalidExtraField = validTeamConfig // { unexpected = true; };
  invalidCapacity = validTeamConfig // {
    capacity.required_native_child_threads = 0;
  };
  invalidFreshContext = validTeamConfig // {
    teams = validTeamConfig.teams // {
      development = validTeamConfig.teams.development // {
        roles = validTeamConfig.teams.development.roles // {
          reviewer = builtins.removeAttrs validTeamConfig.teams.development.roles.reviewer [ "fresh_context" ];
        };
      };
    };
  };
  invalidUtilityMember = validTeamConfig // {
    teams = validTeamConfig.teams // {
      development = validTeamConfig.teams.development // {
        roles = validTeamConfig.teams.development.roles // {
          verification_watcher = {
            model = "model_watcher";
            effort = "low";
            behavior = "verification_watcher";
            lifetime = "operation";
            allowed_efforts = [ "low" ];
            access = "workspace_write";
            fresh_context = true;
          };
        };
      };
    };
  };
  invalidDesignRouting = validTeamConfig // {
    teams = validTeamConfig.teams // {
      development = validTeamConfig.teams.development // {
        routing = validTeamConfig.teams.development.routing // {
          design_simple_effort = "xhigh";
        };
      };
    };
  };
  renamedTeamConfig = validTeamConfig // {
    default_team = "development_clone";
    default_development_team = "development_clone";
    teams = {
      development_clone = validTeamConfig.teams.development;
    };
  };
  configuredPackage = mkPackage {
    inherit pkgs;
    teamConfig = validTeamConfig;
  };
  renamedPackage = mkPackage {
    inherit pkgs;
    teamConfig = renamedTeamConfig;
  };
  unmanagedPackage = mkPackage { inherit pkgs; };
in
assert !((evaluate invalidExtraField).success);
assert !((evaluate invalidCapacity).success);
assert !((evaluate invalidFreshContext).success);
assert !((evaluate invalidUtilityMember).success);
assert !((evaluate invalidDesignRouting).success);
pkgs.runCommand "dev-workspace-agent-team-catalog" {
  nativeBuildInputs = [
    pkgs.coreutils
    pkgs.jq
  ];
} ''
  catalog=${configuredPackage}/share/dev-workspace/agent-teams.json
  metadata=${configuredPackage}/share/dev-workspace/package.json
  renamed_catalog=${renamedPackage}/share/dev-workspace/agent-teams.json

  test -f "$catalog"
  test ! -e ${unmanagedPackage}/share/dev-workspace/agent-teams.json
  test "$(jq -r '.agent_teams.managed' ${unmanagedPackage}/share/dev-workspace/package.json)" = false
  test "$(jq -r '.schema_version' "$catalog")" = 3
  test "$(jq -r '.agent_teams.catalog.schema_version' "$metadata")" = 3
  test "$(jq -r '.agent_teams.capacity.required_native_child_threads' "$metadata")" = 4
  test "$(jq -r '.agent_teams.native_capacity.config_key' "$metadata")" = agents.max_concurrent_threads_per_session
  test "$(jq -r '.agent_teams.native_capacity.required_value' "$metadata")" = 4
  test "$(jq -r '.agent_teams.generator.identity' "$metadata")" = dev-workspace-nix-agent-teams
  test "$(jq -r '.agent_teams.native_adapter.identity' "$metadata")" = codex-custom-agent-toml
  test "$(jq -r '.teams.development.roles.implementer.effort' "$catalog")" = xhigh
  jq -e '.teams.development.roles.implementer.allowed_efforts == ["high", "xhigh"]' \
    "$catalog" >/dev/null
  jq -e '.teams.development.roles.reviewer.fresh_context == true' "$catalog" >/dev/null
  jq -e '(.teams.development.roles | has("verification_watcher")) | not' "$catalog" >/dev/null
  jq -e '.utilities.verification_watcher.behavior == "verification_watcher"' "$catalog" >/dev/null
  jq -e '.native_agent_configs.utilities | length == 1' "$catalog" >/dev/null
  test "$(jq -r '.teams.development.team_digest' "$catalog")" != \
    "$(jq -r '.teams.development_clone.team_digest' "$renamed_catalog")"
  jq -e '.model_inventory == ["model_designer", "model_implementer", "model_lead", "model_reviewer", "model_watcher"]' \
    "$catalog" >/dev/null

  canonical=$(jq -cS '
    del(.catalog_digest, .model_inventory, .native_agent_configs)
    | .teams |= with_entries(.value |= del(.team_digest))
  ' "$catalog")
  expected_digest=$(printf '%s' "$canonical" | sha256sum | cut -d' ' -f1)
  test "$expected_digest" = "$(jq -r '.catalog_digest' "$catalog")"

  native_catalog_digest=$(jq -r '.agent_teams.catalog.digest' "$metadata")
  jq -cS --arg catalog_digest "$native_catalog_digest" '
    .agent_teams.native_role_configs[] |
    {
      name: .name,
      native_name_input: {
        catalog_digest: $catalog_digest,
        kind: "role",
        identity: .identity,
        team: .team,
        effort: .effort,
        role: .role
      }
    }
  ' "$metadata" | while IFS= read -r role_variant; do
    actual_name=$(printf '%s' "$role_variant" | jq -r '.name')
    native_name_input=$(printf '%s' "$role_variant" | jq -cS '.native_name_input')
    expected_name="dw_$(printf '%s' "$native_name_input" | sha256sum | cut -c1-48)"
    test "$expected_name" = "$actual_name"
  done
  utility_variant=$(jq -cS --arg catalog_digest "$native_catalog_digest" '
    .agent_teams.native_utility_configs[0] |
    {
      name: .name,
      native_name_input: {
        catalog_digest: $catalog_digest,
        kind: "utility",
        identity: .identity,
        utility: .utility,
        effort: .effort
      }
    }
  ' "$metadata")
  actual_utility_name=$(printf '%s' "$utility_variant" | jq -r '.name')
  utility_name_input=$(printf '%s' "$utility_variant" | jq -cS '.native_name_input')
  expected_utility_name="dw_$(printf '%s' "$utility_name_input" | sha256sum | cut -c1-48)"
  test "$expected_utility_name" = "$actual_utility_name"

  jq -e 'all(.agent_teams.native_role_configs[];
    .identity.generator.identity == "dev-workspace-nix-agent-teams"
    and .identity.generator.version == 1
    and .identity.generator.canonicalization == "nix-builtins-toJSON-attrset-v1"
    and .identity.native_adapter.identity == "codex-custom-agent-toml"
    and .identity.native_adapter.version == 1
    and (.identity.behavior_digest | test("^[0-9a-f]{64}$")))' "$metadata" >/dev/null
  jq -r '.agent_teams.native_role_configs[] | [.path, .identity.behavior_digest] | @tsv' \
    "$metadata" | while IFS="$(printf '\t')" read -r path digest; do
    config=${configuredPackage}/$path
    test -f "$config"
    grep -Eq '^name = "dw_[0-9a-f]{48}"$' "$config"
    if grep -Eq '^(model|model_reasoning_effort|sandbox_mode) =' "$config"; then
      echo "native role configuration overrides runtime model, effort, or permissions" >&2
      exit 1
    fi
    instructions=$(sed -n 's/^developer_instructions = //p' "$config" | jq -r .)
    test "$(printf '%s' "$instructions" | sha256sum | cut -d' ' -f1)" = "$digest"
  done
  utility_path=$(jq -r '.agent_teams.native_utility_configs[0].path' "$metadata")
  utility_config=${configuredPackage}/$utility_path
  test -f "$utility_config"
  jq -e '.agent_teams.native_utility_configs[0].identity.generator.identity == "dev-workspace-nix-agent-teams"
    and .agent_teams.native_utility_configs[0].identity.native_adapter.identity == "codex-custom-agent-toml"
    and (.agent_teams.native_utility_configs[0].identity.behavior_digest | test("^[0-9a-f]{64}$"))' \
    "$metadata" >/dev/null
  if grep -Eq '^(model|model_reasoning_effort|sandbox_mode) =' "$utility_config"; then
    echo "native utility configuration overrides runtime model, effort, or permissions" >&2
    exit 1
  fi
  utility_instructions=$(sed -n 's/^developer_instructions = //p' "$utility_config" | jq -r .)
  test "$(printf '%s' "$utility_instructions" | sha256sum | cut -d' ' -f1)" = \
    "$(jq -r '.agent_teams.native_utility_configs[0].identity.behavior_digest' "$metadata")"
  if jq -e '.. | strings | select(test("astra"; "i"))' "$catalog" >/dev/null; then
    echo "generic fixture selected a forbidden fallback model" >&2
    exit 1
  fi
  touch "$out"
''
