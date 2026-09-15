{
  pkgs,
  mkPackage,
}:
let
  evaluate =
    extensions: builtins.tryEval (builtins.deepSeq (mkPackage { inherit pkgs extensions; }) true);
  immutableCommand = "${pkgs.hello}/bin/hello";
  longName = builtins.concatStringsSep "" (builtins.genList (_: "a") 64);
  rejectsLongName =
    !(evaluate {
      commands.${longName} = immutableCommand;
    }).success;
  rejectsMutableTarget =
    !(evaluate {
      commands.example = "/bin/true";
    }).success;
  rejectsUnknownSection =
    !(evaluate {
      misspelledProviders.example = immutableCommand;
    }).success;
  rejectsUnknownProviderField =
    !(evaluate {
      clusterProviders.example = {
        command = immutableCommand;
        label = "Example";
        stateDirectory = "example";
      };
    }).success;
  rejectsProgramCollision =
    !(evaluate {
      commands.alpha-devcluster = immutableCommand;
      clusterProviders.alpha = {
        label = "Alpha";
        command = immutableCommand;
      };
    }).success;
  rejectsCoreProgramCollision =
    !(evaluate {
      commands.workspace-portal = immutableCommand;
    }).success;
  rejectsUnsafeUserNamespace =
    !(builtins.tryEval (
      builtins.deepSeq (mkPackage {
        inherit pkgs;
        userNamespace = "../state";
      }) true
    )).success;
  rejectsCoreSkillCollision =
    !(evaluate {
      skills.dev-session-documentation = "${pkgs.hello}";
    }).success;
  corePackage = mkPackage { inherit pkgs; };
  extraSkill = pkgs.runCommand "example-workspace-skill" { } ''
    mkdir -p "$out"
    echo '# Example skill' > "$out/SKILL.md"
  '';
  extendedPackage = mkPackage {
    inherit pkgs;
    extensions.skills.example = extraSkill;
  };
  rejectsUnsafeRouterSocket =
    !(builtins.tryEval (
      builtins.deepSeq (mkPackage {
        inherit pkgs;
        routerSocket = "/tmp/router.sock";
      }) true
    )).success;
  rejectsUnsafeActivationAlias =
    !(builtins.tryEval (
      builtins.deepSeq (mkPackage {
        activationEnvironmentAliases = [ "unsafe-name" ];
        inherit pkgs;
      }) true
    )).success;
in
assert rejectsLongName;
assert rejectsMutableTarget;
assert rejectsUnknownSection;
assert rejectsUnknownProviderField;
assert rejectsProgramCollision;
assert rejectsCoreProgramCollision;
assert rejectsCoreSkillCollision;
assert rejectsUnsafeUserNamespace;
assert rejectsUnsafeRouterSocket;
assert rejectsUnsafeActivationAlias;
pkgs.runCommand "dev-workspace-extension-catalog-validation" { nativeBuildInputs = [ pkgs.jq ]; } ''
  for package in ${corePackage} ${extendedPackage}; do
    skill=$(jq -er '.skills[] | select(.name == "dev-session-documentation") | .path' \
      "$package/share/dev-workspace/extensions.json")
    test -f "$skill/SKILL.md"
    test -f "$package/share/codex/skills/dev-session-documentation/SKILL.md"
  done
  jq -e '[.skills[].name] == ["dev-session-documentation"]' \
    ${corePackage}/share/dev-workspace/extensions.json >/dev/null
  jq -e '[.skills[].name] == ["dev-session-documentation", "example"]' \
    ${extendedPackage}/share/dev-workspace/extensions.json >/dev/null
  test -f ${extendedPackage}/share/codex/skills/example/SKILL.md
  touch "$out"
''
