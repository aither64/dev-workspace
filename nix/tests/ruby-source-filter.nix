{ pkgs }:
let
  suite =
    variant:
    (pkgs.callPackage ../ruby-checks.nix {
      src = ../../test/fixtures/ruby-source-filter + "/${variant}";
    }).checks.ruby-dev_session-0;
in
assert (suite "base").drvPath == (suite "portal-change").drvPath;
assert (suite "base").drvPath != (suite "ruby-change").drvPath;
pkgs.runCommand "dev-workspace-ruby-source-filter" { } ''touch "$out"''
