{ pkgs, codexNative }:
let
  lib = pkgs.lib;
  expected = ./codex-provider-enforcement-tests.json;
  binding = pkgs.writeText "codex-provider-enforcement-binding.json" (builtins.toJSON {
    native = toString codexNative;
    # This is receipt text, not a derivation-file input. Keep the native output
    # dependency above without requiring its .drv to exist during read-only eval.
    nativeDerivation = builtins.unsafeDiscardStringContext codexNative.drvPath;
    source = toString codexNative.src;
    provenance = codexNative.codexProvenance;
    preparation = "Inherited recipe with daemon @out@ anchored to the production native output";
  });
  verify = pkgs.writeShellScript "verify-codex-provider-enforcement" ''
    set -euo pipefail
    export PATH=${lib.makeBinPath [ pkgs.coreutils pkgs.gnused pkgs.gnugrep pkgs.jq ]}:"$PATH"
    harness=$1
    test_server=$2
    exec_server=$3
    receipt=$4
    mkdir -p "$receipt"
    scratch=$(mktemp -d)
    trap 'rm -rf "$scratch"' EXIT
    mkdir "$scratch/home" "$scratch/tmp"
    export HOME="$scratch/home" TMPDIR="$scratch/tmp" CODEX_TEST_ENVIRONMENT=local
    unset CODEX_HOME CODEX_SQLITE_HOME CODEX_TEST_REMOTE_ENV CODEX_TEST_REMOTE_ENV_CONTAINER_NAME
    selector='suite::v2::model_provider_enforcement::'
    env "CARGO_BIN_EXE_codex-app-server=$test_server" "CARGO_BIN_EXE_exec-server=$exec_server" \
      "$harness" "$selector" --list --format terse > "$receipt/list.txt"
    sed -n 's/: test$//p' "$receipt/list.txt" | LC_ALL=C sort > "$receipt/selected.txt"
    jq -r '.[]' ${expected} | LC_ALL=C sort > "$receipt/expected.txt"
    test "$(wc -l < "$receipt/selected.txt")" -eq 12
    cmp "$receipt/expected.txt" "$receipt/selected.txt"
    env "CARGO_BIN_EXE_codex-app-server=$test_server" "CARGO_BIN_EXE_exec-server=$exec_server" \
      "$harness" "$selector" --color never --test-threads "''${NIX_BUILD_CORES:-8}" \
      2>&1 | tee "$receipt/results.txt"
    grep -Eq '^test result: ok\. 12 passed; 0 failed; 0 ignored; ' "$receipt/results.txt"
    sed -n 's/^test \(suite::v2::model_provider_enforcement::.*\) \.\.\. ok$/\1/p' \
      "$receipt/results.txt" | LC_ALL=C sort > "$receipt/passed.txt"
    cmp "$receipt/expected.txt" "$receipt/passed.txt"
    cp ${binding} "$receipt/binding.json"
    sha256sum ${codexNative}/libexec/codex/bin/codex > "$receipt/native.sha256"
  '';
in
assert lib.assertMsg (lib.hasInfix "--subst-var out" codexNative.postPatch)
  "provider check requires the reviewed recipe's explicit daemon output substitution";
codexNative.overrideAttrs (old: {
  pname = "codex-provider-enforcement";
  cargoDeps = codexNative.cargoDeps;
  nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.jq pkgs.makeWrapper pkgs.python3 ];
  # Preserve production preparation exactly, including the embedded daemon
  # package path. The test-only output must not become the native authority.
  postPatch = lib.replaceStrings [ "--subst-var out" ]
    [ "--subst-var-by out ${codexNative}" ] old.postPatch;
  buildPhase = ''
    runHook preBuild
    # The vendor/source lock consistency hook has already passed. The release
    # tag leaves local package versions at 0.0.0; the producer's first build
    # normalizes these implicitly, before its locked tests.
    ${pkgs.python3}/bin/python3 ${./codex-provider-lock.py} "$PWD" ${codexNative.version} prepared-lock.json
    export RUST_TEST_THREADS="$NIX_BUILD_CORES"
    cargo test --release --offline --locked --target ${pkgs.stdenv.hostPlatform.rust.rustcTarget} \
      -j "$NIX_BUILD_CORES" --config 'profile.release.package.codex-app-server.debug-assertions=true' \
      -p codex-app-server --test all --no-run --message-format=json > test-artifacts.json
    mapfile -t harnesses < <(jq -r \
      'select(.reason == "compiler-artifact" and .target.name == "all" and .executable != null) | .executable' \
      test-artifacts.json | sort -u)
    test "''${#harnesses[@]}" -eq 1
    harness=''${harnesses[0]}
    test_server="$PWD/target/${pkgs.stdenv.hostPlatform.rust.rustcTarget}/release/codex-app-server"
    exec_server="$PWD/target/${pkgs.stdenv.hostPlatform.rust.rustcTarget}/release/exec-server"
    test -x "$harness" -a -x "$test_server" -a -x "$exec_server"
    sha256sum app-server-daemon/src/prepare_install.rs chatgpt/src/lib.rs Cargo.toml \
      > prepared-source.sha256
  '';
  doCheck = true;
  checkPhase = ''
    ${verify} "$harness" "$test_server" "$exec_server" "$TMPDIR/provider-enforcement"
  '';
  installPhase = ''
    mkdir -p "$out/libexec/codex-provider-enforcement" "$out/share/codex-provider-enforcement"
    cp "$harness" "$out/libexec/codex-provider-enforcement/all"
    cp "$test_server" "$exec_server" "$out/libexec/codex-provider-enforcement/"
    ln -s ${codexNative}/libexec/codex/bin/codex-code-mode-host \
      "$out/libexec/codex-provider-enforcement/codex-code-mode-host"
    cp -r "$TMPDIR/provider-enforcement/." "$out/share/codex-provider-enforcement/"
    cp prepared-source.sha256 "$out/share/codex-provider-enforcement/"
    cp prepared-lock.json "$out/share/codex-provider-enforcement/"
    makeWrapper ${verify} "$out/bin/codex-provider-enforcement" \
      --add-flags "$out/libexec/codex-provider-enforcement/all" \
      --add-flags "$out/libexec/codex-provider-enforcement/codex-app-server" \
      --add-flags "$out/libexec/codex-provider-enforcement/exec-server"
  '';
  # Retain only this harness and its helpers, without the producer's install
  # layout, completion generation, or a general Cargo target cache.
  postInstall = "";
  postFixup = "";
  doInstallCheck = false;
  nativeInstallCheckInputs = [ ];
  dontStrip = true;
  passthru = {
    native = codexNative;
    inherit verify;
    expectedTests = builtins.fromJSON (builtins.readFile expected);
  };
})
