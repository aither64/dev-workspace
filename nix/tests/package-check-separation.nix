{ pkgs, self, mkPackage, checkedPackage, rubyCheckSet }:
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
  rubyChecks = builtins.attrValues rubyCheckSet.checks ++ [ rubyCheckSet.all rubyCheckSet.essential ];
  testOutputs = rubyChecks ++ [ checkedPackage checkedAssets ];
  noTestInputs = value: builtins.all (input: !(hasInput value input)) testOutputs;
  composed = (mkPackage {
    inherit pkgs;
    extensions.commands.example = "${pkgs.hello}/bin/hello";
    piProviderConfig = { schema = 1; catalog = { }; default = { }; };
  }).overrideAttrs (old: { postInstall = old.postInstall + "\n# downstream installation\n"; });
  checkedComposed = composed.override { doCheck = true; };
  checkNames = builtins.attrNames self.checks.${system};
  essentialNames = [
    "package" "ruby-essential-verification" "package-check-separation"
    "codex-package" "pi-worker" "pi-runtime-host-unit" "pi-runtime-host-environment"
    "ruby-source-filter" "host-module" "host-auth-benchmark" "host-package-contract" "generic-source"
  ];
  compositionNames = [ "ruby-verification" "extension-catalog" "agent-team-catalog" "user-namespace" ];
  hostNames = [ "host-module-idempotency" "session-recovery-boot" "pi-runtime-host" ];
  essentialShards = [
    "ruby-workspace_host-0" "ruby-coordination_claims-0" "ruby-machine_identity-0"
    "ruby-coordination_admission-0" "ruby-integration_certification-0"
  ];
  shardNames = builtins.attrNames rubyCheckSet.checks;
  aggregateCovers = aggregate: names:
    lib.hasInfix "check_ruby_shards.py" aggregate.buildCommand
    && builtins.filter (name: hasInput aggregate rubyCheckSet.checks.${name}) shardNames
      == builtins.filter (name: builtins.elem name names) shardNames
    && builtins.all (name:
      builtins.length (lib.splitString (builtins.unsafeDiscardStringContext "${rubyCheckSet.checks.${name}}/timings.json") aggregate.buildCommand) == 2
    ) names;
  workflow = builtins.readFile ../../.github/workflows/check.yml;
  # Inspect literal installables from the actual job, not a parallel CI list.
  jobSection = name:
    let sections = lib.splitString "\n  ${name}:\n" workflow;
    in assert builtins.length sections == 2;
      builtins.head (builtins.split "\n  [[:alnum:]_-]+:\n" (lib.last sections));
  installables = section: builtins.filter (lib.hasPrefix ".#")
    (lib.concatMap (lib.splitString " ") (lib.splitString "\n" section));
  targets = names: map (name: ".#checks.${system}.${name}") names;
  buildCount = section: lib.count (lib.hasInfix "nix build ") (lib.splitString "\n" section);
  fast = jobSection "fast";
  extended = jobSection "extended";
  archive = jobSection "archive";
  host = jobSection "host";
  creation = jobSection "creation";
  dispatchOnly = section: lib.hasInfix "\n    if: github.event_name == 'workflow_dispatch'\n" section;
in
assert builtins.all (name: self.packages.${system}.${name}.doCheck == false) [
  "default" "dev-workspace" "workspace-host" "workspace-portal"
];
assert self.apps.${system}.workspace-host.program == "${package}/bin/workspace-host";
assert checkedPackage.doCheck && self.checks.${system}.package.drvPath == checkedPackage.drvPath;
assert package ? rubyChecks && builtins.all (name: builtins.elem name checkNames)
  (builtins.attrNames package.rubyChecks);
assert package.rubyChecks.ruby-verification.drvPath == rubyCheckSet.all.drvPath;
assert package.rubyChecks.ruby-essential-verification.drvPath == rubyCheckSet.essential.drvPath;
assert aggregateCovers rubyCheckSet.all shardNames && aggregateCovers rubyCheckSet.essential essentialShards;
assert builtins.length (lib.unique essentialShards) == 5;
assert builtins.all (name: lib.hasSuffix "-0-of-1" rubyCheckSet.checks.${name}.name) essentialShards;
assert !(hasInput rubyCheckSet.essential rubyCheckSet.all);
assert !(package ? preCheck) && (package.nativeCheckInputs or [ ]) == [ ];
assert noTestInputs package && noTestInputs assets && noTestInputs shell && noTestInputs composed;
assert assets.doCheck == false && checkedAssets.doCheck;
assert hasInput package assets && !(hasInput package checkedAssets);
assert hasInput checkedPackage checkedAssets && !(hasInput checkedPackage rubyCheckSet.all);
assert lib.hasInfix "node --test editor.test.mjs" checkedAssets.checkPhase;
assert builtins.all (command: lib.hasInfix command checkedPackage.checkPhase) [
  "go test ./..." "node --test" "codex_web_pin_test.sh" "codex_protocol_contract.py"
];
assert builtins.all (input: builtins.elem input checkedPackage.nativeBuildInputs)
  [ pkgs.nodejs pkgs.playwright-test pkgs.ruby pkgs.tmux ];
assert composed.doCheck == false && checkedComposed.doCheck;
assert hasInput checkedComposed checkedAssets && !(hasInput checkedComposed rubyCheckSet.all);
assert lib.hasInfix "# downstream installation" checkedComposed.postInstall;
assert builtins.all (input: builtins.elem (toString (lib.getDev input)) (map toString shell.nativeBuildInputs))
  (checkedPackage.nativeBuildInputs ++ [ pkgs.go pkgs.ruby pkgs.git pkgs.nix pkgs.nodejs pkgs.jq ]);
assert shell.GOFLAGS == "-mod=readonly";
assert builtins.length essentialNames == 12 && builtins.length (lib.unique essentialNames) == 12;
assert builtins.all (name: builtins.elem name checkNames)
  (essentialNames ++ compositionNames ++ hostNames ++ shardNames ++ [ "session-creation" "session-archive-lifecycle" ]);
assert installables fast == [ ".#pi-sdk-source" ] ++ targets essentialNames && buildCount fast == 2;
assert lib.hasInfix "run: nix build --no-link --print-build-logs .#pi-sdk-source\n      - name: Evaluate all checks\n        run: nix flake check --no-build --print-build-logs" fast;
assert !(lib.hasInfix "\n    if:" fast) && !(lib.hasInfix "\n    needs:" fast);
assert dispatchOnly extended && dispatchOnly archive && dispatchOnly host;
assert installables extended == targets compositionNames && buildCount extended == 1;
assert installables archive == targets [ "session-archive-lifecycle" ] && buildCount archive == 1;
assert installables host == targets hostNames && buildCount host == 1;
assert installables creation == targets [ "session-creation" ] && buildCount creation == 1;
assert !(lib.hasInfix "\n    if:" creation) && !(lib.hasInfix "\n    needs:" creation);
assert !(builtins.elem "session-creation-extended" checkNames)
  && self.packages.${system} ? session-creation-extended;
pkgs.runCommand "dev-workspace-package-check-separation" { } ''
  echo "12 essential targets; full Ruby and extended checks preserved separately"
  touch "$out"
''
