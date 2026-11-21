{ pkgs, self, mkPackage, checkedPackage }:
let
  inherit (pkgs) lib;
  system = pkgs.stdenv.hostPlatform.system;
  package = self.packages.${system}.default;
  shell = self.devShells.${system}.default;
  assets = pkgs.callPackage ../review-ui.nix { };
  checkedAssets = assets.override { doCheck = true; };
  # String contexts are the evaluated derivation's direct input references.
  references = value: builtins.getContext (builtins.toJSON value.drvAttrs);
  hasInput = value: input: builtins.hasAttr (builtins.unsafeDiscardStringContext input.drvPath) (references value);
  rubyChecks = builtins.attrValues package.rubyChecks;
  testOutputs = rubyChecks ++ [ checkedPackage checkedAssets ];
  noTestInputs = value: builtins.all (input: !(hasInput value input)) testOutputs;
  composed = (mkPackage {
    inherit pkgs;
    extensions.commands.example = "${pkgs.hello}/bin/hello";
    piProviderConfig = { schema = 1; catalog = { }; default = { }; };
  }).overrideAttrs (old: { postInstall = old.postInstall + "\n# downstream installation\n"; });
  checkedComposed = composed.override { doCheck = true; };
  checkNames = builtins.attrNames self.checks.${system};
  vmChecks = [
    "host-module-idempotency" "session-recovery-boot"
    "session-archive-lifecycle" "session-creation" "pi-runtime-host"
  ];
  # Execute the workflow's actual selector against the current output names.
  selector = lib.findFirst (line: lib.hasInfix "jq -r '.[]" line) (throw "missing CI check selector")
    (lib.splitString "\n" (builtins.readFile ../../.github/workflows/check.yml));
  fastChecks = builtins.filter (name: !(builtins.elem name vmChecks)) checkNames;
in
assert builtins.all (name: self.packages.${system}.${name}.doCheck == false) [
  "default" "dev-workspace" "workspace-host" "workspace-portal"
];
assert self.apps.${system}.workspace-host.program == "${package}/bin/workspace-host";
assert checkedPackage.doCheck && self.checks.${system}.package.drvPath == checkedPackage.drvPath;
assert package ? rubyChecks && builtins.all (name: builtins.elem name checkNames)
  (builtins.attrNames package.rubyChecks);
assert !(package ? preCheck) && (package.nativeCheckInputs or [ ]) == [ ];
assert noTestInputs package && noTestInputs assets && noTestInputs shell && noTestInputs composed;
assert assets.doCheck == false && checkedAssets.doCheck;
assert hasInput package assets && !(hasInput package checkedAssets);
assert hasInput checkedPackage checkedAssets && hasInput checkedPackage package.rubyChecks.ruby-verification;
assert lib.hasInfix "node --test editor.test.mjs" checkedAssets.checkPhase;
assert builtins.all (command: lib.hasInfix command checkedPackage.checkPhase) [
  "go test ./..." "node --test" "codex_web_pin_test.sh" "codex_protocol_contract.py"
];
assert builtins.all (input: builtins.elem input checkedPackage.nativeBuildInputs)
  [ pkgs.nodejs pkgs.playwright-test pkgs.ruby pkgs.tmux ];
assert composed.doCheck == false && checkedComposed.doCheck;
assert hasInput checkedComposed checkedAssets && hasInput checkedComposed package.rubyChecks.ruby-verification;
assert lib.hasInfix "# downstream installation" checkedComposed.postInstall;
assert builtins.all (input: builtins.elem (toString (lib.getDev input)) (map toString shell.nativeBuildInputs))
  (checkedPackage.nativeBuildInputs ++ [ pkgs.go pkgs.ruby pkgs.git pkgs.nix pkgs.nodejs pkgs.jq ]);
assert shell.GOFLAGS == "-mod=readonly";
pkgs.runCommand "dev-workspace-package-check-separation" { nativeBuildInputs = [ pkgs.jq ]; } ''
  check_names=${lib.escapeShellArg (builtins.toJSON checkNames)}
  selected=$(${selector})
  expected=${lib.escapeShellArg (lib.concatMapStringsSep "\n" (name: ".#checks.${system}." + name) fastChecks)}
  test "$selected" = "$expected"
  touch "$out"
''
