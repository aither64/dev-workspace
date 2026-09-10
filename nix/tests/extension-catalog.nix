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
assert rejectsUnsafeUserNamespace;
assert rejectsUnsafeRouterSocket;
assert rejectsUnsafeActivationAlias;
pkgs.runCommand "dev-workspace-extension-catalog-validation" { } ''
  touch "$out"
''
