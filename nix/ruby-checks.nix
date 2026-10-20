{
  bash,
  coreutils,
  git,
  jq,
  lib,
  nodejs,
  openssl,
  openssh,
  python3,
  ruby,
  runCommand,
  src,
  tmux,
  util-linux,
}:
let
  # These suites exercise the Ruby commands with disposable fixtures. Their
  # source includes contracts and packaging fixtures, but no portal implementation.
  rubySource = lib.cleanSourceWith {
    inherit src;
    name = "dev-workspace-ruby-source";
    filter =
      path: type:
      let
        relative = lib.removePrefix (toString src + "/") path;
      in
      type == "directory"
      || lib.any (prefix: lib.hasPrefix prefix relative) [
        "libexec/"
        "test/"
        "nix/"
      ]
      || lib.elem relative [
        "docs/coordination.md"
        "docs/coordination-operations.md"
        "docs/integration.md"
        "docs/coordination-legacy.md"
      ]
      || relative == "portal/internal/session/runtime-contract.json";
  };
  suites = {
    dev_session = 4;
    workspace_coordination = 2;
    auto_archive = 1;
    workspace_host = 1;
    coordination_claims = 1;
    coordination_admission = 1;
    coordination_gc = 1;
    integration = 1;
    integration_certification = 1;
  };
  shard =
    suite: count: index:
    runCommand "dev-workspace-${suite}-${toString index}-of-${toString count}"
      {
        nativeBuildInputs = [
          bash
          coreutils
          git
          jq
          nodejs
          openssl
          openssh
          python3
          ruby
          tmux
          util-linux
        ];
      }
      ''
        export HOME="$TMPDIR/home"
        export LANG=C.UTF-8 LC_ALL=C.UTF-8 SHELL=${bash}/bin/bash
        export TMUX_TMPDIR="$TMPDIR/tmux" DEV_SESSION_SKIP_REAL_TMUX_TESTS=1
        mkdir -p "$HOME" "$TMUX_TMPDIR" "$out"
        cp -R ${rubySource} source
        chmod -R u+w source
        ruby source/test/run_ruby_suite.rb \
          --shard ${toString index}/${toString count} \
          --timings "$out/timings.json" ${suite}_test.rb
      '';
  checks = lib.listToAttrs (
    lib.concatLists (
      lib.mapAttrsToList (
        suite: count:
        lib.genList (index: {
          name = "ruby-${suite}-${toString index}";
          value = shard suite count index;
        }) count
      ) suites
    )
  );
  all = runCommand "dev-workspace-ruby-verification" { nativeBuildInputs = [ python3 ]; } ''
    python3 ${../test/check_ruby_shards.py} \
      ${lib.concatMapStringsSep " " (check: "${check}/timings.json") (builtins.attrValues checks)}
    touch "$out"
  '';
in
{
  inherit checks all rubySource;
}
