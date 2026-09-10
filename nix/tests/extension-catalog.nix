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
  rejectsProgramCollision =
    !(evaluate {
      commands.alpha-devcluster = immutableCommand;
      clusterProviders.alpha = {
        label = "Alpha";
        command = immutableCommand;
      };
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
in
assert rejectsLongName;
assert rejectsMutableTarget;
assert rejectsProgramCollision;
assert rejectsUnsafeUserNamespace;
assert rejectsUnsafeRouterSocket;
pkgs.runCommand "dev-workspace-extension-catalog-validation" { } ''
  touch "$out"
''
