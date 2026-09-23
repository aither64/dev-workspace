{
  lib,
  teamConfig ? null,
  writeText,
}:
let
  catalogPath = "share/dev-workspace/agent-teams.json";
  nativeRoot = "share/dev-workspace/agent-teams";
  canonicalization = "nix-builtins-toJSON-attrset-v1";
  generator = {
    identity = "dev-workspace-nix-agent-teams";
    version = 1;
    inherit canonicalization;
  };
  nativeAdapter = {
    identity = "codex-custom-agent-toml";
    version = 1;
  };
  supportedEfforts = [
    "minimal"
    "low"
    "medium"
    "high"
    "xhigh"
    "max"
    "ultra"
  ];
  roleBehaviors = [
    "team_lead"
    "designer"
    "implementer"
    "reviewer"
    "general"
  ];
  rolePurposes = [
    "lead"
    "design"
    "implementation"
    "review"
    "general"
  ];
  roleLifetimes = [
    "session"
    "initiative"
    "work_unit"
    "review_cycle"
  ];
  accesses = [
    "read_only"
    "workspace_write"
  ];
  isNonemptyString = value: builtins.isString value && value != "";
  isIdentifier =
    value:
    isNonemptyString value
    && builtins.stringLength value <= 63
    && builtins.match "[a-z][a-z0-9_]*" value != null;
  isRoleName =
    value:
    value == "team_lead"
    || (
      value != "lead"
      && isNonemptyString value
      && builtins.stringLength value <= 32
      && builtins.match "[a-z][a-z0-9]*" value != null
    );
  isPositiveInt = value: builtins.isInt value && value > 0;
  isUnique = values: builtins.length values == builtins.length (lib.unique values);
  hasExactFields = fields: value: builtins.isAttrs value && builtins.attrNames value == fields;
  hasOnlyFields =
    fields: value:
    builtins.isAttrs value
    && builtins.all (field: builtins.elem field fields) (builtins.attrNames value);
  validEffort = value: builtins.elem value supportedEfforts;
  validModel = isNonemptyString;
  validStringList =
    values:
    builtins.isList values && values != [ ] && isUnique values && builtins.all isNonemptyString values;
  validIdentifierList =
    values:
    builtins.isList values && values != [ ] && isUnique values && builtins.all isIdentifier values;
  validEffortList = values: validStringList values && builtins.all validEffort values;
  validRole =
    role:
    hasExactFields [
      "access"
      "allowed_efforts"
      "behavior"
      "effort"
      "fresh_context"
      "instructions"
      "lifetime"
      "model"
      "purpose"
    ] role
    && validModel role.model
    && validEffort role.effort
    && validEffortList role.allowed_efforts
    && builtins.elem role.effort role.allowed_efforts
    && builtins.elem role.behavior roleBehaviors
    && builtins.elem role.purpose rolePurposes
    &&
      role.purpose == {
        team_lead = "lead";
        designer = "design";
        implementer = "implementation";
        reviewer = "review";
        general = "general";
      }
      .${role.behavior}
    && isNonemptyString role.instructions
    && builtins.stringLength role.instructions <= 4096
    && builtins.elem role.lifetime roleLifetimes
    && builtins.elem role.access accesses
    && (role.purpose != "lead" || role.access == "workspace_write")
    && (role.purpose != "implementation" || role.access == "workspace_write")
    && (role.purpose != "review" || role.access == "read_only")
    && builtins.isBool role.fresh_context;
  validWorkPolicy =
    policy:
    hasExactFields [
      "allowed"
      "default"
      "followup"
      "simple"
      "simple_requires_reason"
    ] policy
    && validEffortList policy.allowed
    && builtins.elem policy.default policy.allowed
    && builtins.elem policy.simple policy.allowed
    && builtins.isBool policy.simple_requires_reason
    && policy.followup == "retain";
  validLifecycle =
    lifecycle:
    hasExactFields [ "communication" "reviewer_reuse" "startup" ] lifecycle
    && lifecycle.startup == "on_demand"
    && lifecycle.communication == "lead_mediated"
    && lifecycle.reviewer_reuse == "same_change";
  validUtility =
    utility:
    hasExactFields [
      "access"
      "behavior"
      "effort"
      "lifetime"
      "max_concurrent"
      "model"
      "required_for"
      "startup"
    ] utility
    && validModel utility.model
    && validEffort utility.effort
    && utility.behavior == "verification_watcher"
    && utility.lifetime == "operation"
    && builtins.elem utility.access accesses
    && utility.startup == "on_demand"
    && isPositiveInt utility.max_concurrent
    && validIdentifierList utility.required_for;
  roleMatchesWorkPolicy =
    role: policy: role.effort == policy.default && role.allowed_efforts == policy.allowed;
  roleSupportsWorkPolicy =
    role: policy:
    builtins.elem policy.default role.allowed_efforts
    && builtins.elem policy.simple role.allowed_efforts;
  validRouting =
    team: workPolicy:
    hasOnlyFields [
      "design_simple_effort"
      "implementer_simple_effort"
      "low_risk_review_role"
    ] team.routing
    && (
      !team.routing ? design_simple_effort
      || (
        validEffort team.routing.design_simple_effort
        && team.routing.design_simple_effort == workPolicy.design.simple
      )
    )
    && (
      !team.routing ? implementer_simple_effort
      || (
        validEffort team.routing.implementer_simple_effort
        && team.routing.implementer_simple_effort == workPolicy.implementation.simple
      )
    )
    && (
      !team.routing ? low_risk_review_role
      || (
        isRoleName team.routing.low_risk_review_role
        && builtins.hasAttr team.routing.low_risk_review_role team.roles
        && (builtins.getAttr team.routing.low_risk_review_role team.roles).purpose == "review"
        && (builtins.getAttr team.routing.low_risk_review_role team.roles).fresh_context
      )
    );
  validTeam =
    team: capacity: workPolicy:
    hasExactFields [
      "description"
      "design_owner"
      "lifecycle"
      "max_open_agents"
      "mode"
      "roles"
      "routing"
      "service_policy"
    ] team
    && isNonemptyString team.description
    && builtins.elem team.mode [
      "solo"
      "development"
    ]
    && builtins.elem team.service_policy [
      "unmanaged"
      "non_priority"
    ]
    && builtins.isAttrs team.roles
    && team.roles != { }
    && builtins.length (builtins.attrNames team.roles) <= 8
    && !(team.roles ? designer && team.roles ? architect)
    && builtins.all isRoleName (builtins.attrNames team.roles)
    && builtins.all validRole (builtins.attrValues team.roles)
    && builtins.all (
      name:
      let
        role = team.roles.${name};
      in
      (name == "team_lead" || role.purpose != "lead")
      && (name != "designer" || role.purpose == "design")
      && (name != "implementer" || role.purpose == "implementation")
      && (name != "reviewer" || role.purpose == "review")
    ) (builtins.attrNames team.roles)
    && validLifecycle team.lifecycle
    && builtins.isAttrs team.routing
    && validRouting team workPolicy
    && team.roles ? team_lead
    && team.roles.team_lead.behavior == "team_lead"
    && builtins.hasAttr team.design_owner team.roles
    && builtins.elem team.roles.${team.design_owner}.purpose [
      "lead"
      "design"
    ]
    && (
      if team.mode == "solo" then
        builtins.attrNames team.roles == [ "team_lead" ]
        && team.design_owner == "team_lead"
        && team.routing == { }
        && team.max_open_agents == 0
        && roleSupportsWorkPolicy team.roles.team_lead workPolicy.design
        && roleSupportsWorkPolicy team.roles.team_lead workPolicy.implementation
      else
        roleMatchesWorkPolicy (builtins.getAttr team.design_owner team.roles) workPolicy.design
        && builtins.any (
          role: role.purpose == "implementation" && roleMatchesWorkPolicy role workPolicy.implementation
        ) (builtins.attrValues team.roles)
        && builtins.any (role: role.purpose == "review" && role.fresh_context) (
          builtins.attrValues team.roles
        )
        && builtins.all (role: role.purpose != "review" || role.fresh_context) (
          builtins.attrValues team.roles
        )
        && isPositiveInt team.max_open_agents
        && team.max_open_agents <= capacity.required_native_child_threads
        && team.routing ? design_simple_effort
        && team.routing ? implementer_simple_effort
    );
  validTeamConfig =
    builtins.isAttrs teamConfig
    && hasExactFields [
      "capacity"
      "default_development_team"
      "default_team"
      "schema_version"
      "teams"
      "utilities"
      "work_policy"
    ] teamConfig
    && teamConfig.schema_version == 4
    && hasExactFields [ "required_native_child_threads" ] teamConfig.capacity
    && isPositiveInt teamConfig.capacity.required_native_child_threads
    && hasExactFields [ "design" "implementation" ] teamConfig.work_policy
    && validWorkPolicy teamConfig.work_policy.design
    && validWorkPolicy teamConfig.work_policy.implementation
    && builtins.isAttrs teamConfig.teams
    && teamConfig.teams != { }
    && builtins.all isIdentifier (builtins.attrNames teamConfig.teams)
    && builtins.all (team: validTeam team teamConfig.capacity teamConfig.work_policy) (
      builtins.attrValues teamConfig.teams
    )
    && isIdentifier teamConfig.default_team
    && builtins.hasAttr teamConfig.default_team teamConfig.teams
    && (
      teamConfig.default_development_team == null
      || (
        isIdentifier teamConfig.default_development_team
        && builtins.hasAttr teamConfig.default_development_team teamConfig.teams
        && (builtins.getAttr teamConfig.default_development_team teamConfig.teams).mode == "development"
      )
    )
    && hasExactFields [ "verification_watcher" ] teamConfig.utilities
    && validUtility teamConfig.utilities.verification_watcher;
  canonicalJSON = value: builtins.toJSON value;
  catalogDigest =
    if teamConfig == null then null else builtins.hashString "sha256" (canonicalJSON teamConfig);
  teamDigest =
    teamName: team:
    builtins.hashString "sha256" (canonicalJSON {
      schema_version = teamConfig.schema_version;
      inherit (teamConfig) capacity;
      work_policy = teamConfig.work_policy;
      team_name = teamName;
      inherit team;
    });
  nativeIdentity = developerInstructions: {
    inherit generator;
    native_adapter = nativeAdapter;
    behavior_digest = builtins.hashString "sha256" developerInstructions;
  };
  nativeName =
    data: "dw_${builtins.substring 0 48 (builtins.hashString "sha256" (canonicalJSON data))}";
  roleVariant =
    team: roleName: role: effort:
    let
      developerInstructions = role.instructions;
      identity = nativeIdentity developerInstructions;
      name = nativeName {
        catalog_digest = catalogDigest;
        kind = "role";
        inherit identity team effort;
        role = roleName;
      };
      path = "${nativeRoot}/${team}/${roleName}-${effort}.toml";
    in
    {
      kind = "role";
      inherit
        team
        effort
        identity
        name
        path
        ;
      role = roleName;
      source = writeText "${name}.toml" ''
        name = ${builtins.toJSON name}
        description = ${builtins.toJSON "Managed ${role.behavior} for team ${team}, effort ${effort}."}
        developer_instructions = ${builtins.toJSON developerInstructions}
      '';
    };
  roleVariants =
    if teamConfig == null then
      [ ]
    else
      lib.concatMap (
        teamName:
        lib.concatMap (
          roleName:
          let
            role = teamConfig.teams.${teamName}.roles.${roleName};
          in
          map (effort: roleVariant teamName roleName role effort) role.allowed_efforts
        ) (builtins.attrNames teamConfig.teams.${teamName}.roles)
      ) (builtins.attrNames teamConfig.teams);
  utilityVariant =
    if teamConfig == null then
      null
    else
      let
        utility = teamConfig.utilities.verification_watcher;
        developerInstructions = "Run only the assigned verification operation, retain its result, and do not edit application source, diagnose failures, retry, approve, or deploy.";
        identity = nativeIdentity developerInstructions;
        name = nativeName {
          catalog_digest = catalogDigest;
          kind = "utility";
          inherit identity;
          utility = "verification_watcher";
          effort = utility.effort;
        };
      in
      {
        kind = "utility";
        utility = "verification_watcher";
        effort = utility.effort;
        inherit identity name;
        path = "${nativeRoot}/utilities/verification_watcher-${utility.effort}.toml";
        source = writeText "${name}.toml" ''
          name = ${builtins.toJSON name}
          description = "Managed transient verification watcher."
          developer_instructions = ${builtins.toJSON developerInstructions}
        '';
      };
  nativeRoleConfigs = map (variant: builtins.removeAttrs variant [ "source" ]) roleVariants;
  nativeUtilityConfigs =
    if utilityVariant == null then [ ] else [ (builtins.removeAttrs utilityVariant [ "source" ]) ];
  modelInventory =
    if teamConfig == null then
      [ ]
    else
      lib.sort builtins.lessThan (
        lib.unique (
          (lib.concatMap (team: map (role: role.model) (builtins.attrValues team.roles)) (
            builtins.attrValues teamConfig.teams
          ))
          ++ [ teamConfig.utilities.verification_watcher.model ]
        )
      );
  catalogData =
    if teamConfig == null then
      null
    else
      teamConfig
      // {
        catalog_digest = catalogDigest;
        model_inventory = modelInventory;
        teams = lib.mapAttrs (name: team: team // { team_digest = teamDigest name team; }) teamConfig.teams;
        native_agent_configs = {
          adapter = nativeAdapter;
          roles = nativeRoleConfigs;
          utilities = nativeUtilityConfigs;
        };
      };
  catalog =
    if catalogData == null then
      null
    else
      writeText "dev-workspace-agent-teams.json" (canonicalJSON catalogData);
in
{
  configured = teamConfig != null;
  valid = teamConfig == null || validTeamConfig;
  inherit
    catalog
    catalogData
    catalogDigest
    catalogPath
    generator
    nativeAdapter
    nativeRoleConfigs
    nativeRoot
    nativeUtilityConfigs
    roleVariants
    utilityVariant
    ;
}
