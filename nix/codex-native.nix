# The fork contains the provider compatibility change. Reuse the frozen
# llm-agents recipe for its independent daemon/layout/V8/build adjustments.
{ pkgs, upstreamCodex, source, recipeRevision }:
let
  nativeSource = pkgs.fetchFromGitHub {
    owner = "aither64";
    repo = "codex";
    rev = source.revision;
    hash = source.hash;
  };
  selected = upstreamCodex.override {
    version = source.version;
    srcOverride = nativeSource;
    cargoVendor = { cargoHash = source.cargoHash; };
  };
in
assert pkgs.lib.assertMsg (builtins.match "[0-9a-f]{40}" source.revision != null)
  "codex-native: pin a complete immutable fork revision";
assert pkgs.lib.assertMsg (upstreamCodex.version == source.version)
  "codex-native: the frozen packaging recipe must match the selected release";
selected.overrideAttrs (old: {
  # Reuse release dependencies for the patch tests. Only the test app-server
  # needs debug assertions for upstream's managed-config/test-startup hooks.
  doCheck = true;
  # The dedicated codex-provider-enforcement check owns its exact twelve cases.
  checkPhase = ''
    runHook preCheck
    export RUST_TEST_THREADS="$NIX_BUILD_CORES"
    sha256sum target/${pkgs.stdenv.hostPlatform.rust.rustcTarget}/release/{codex,codex-code-mode-host,logs_client} > "$TMPDIR/native-release-binaries.sha256"
    test_flags=(--release --offline --locked --target ${pkgs.stdenv.hostPlatform.rust.rustcTarget} -j "$NIX_BUILD_CORES"
      --config 'profile.release.package.codex-app-server.debug-assertions=true')
    cargo test "''${test_flags[@]}" -p codex-app-server --lib config_manager::provider_tests
    cargo test "''${test_flags[@]}" -p codex-app-server --bin codex-app-server host_provider_switching_requires_startup_flag
    cargo test "''${test_flags[@]}" -p codex-cli --bin codex app_server_host_provider_switching_requires_startup_flag
    cargo test "''${test_flags[@]}" -p codex-app-server --test all thread_resume_returns_provider_error_even_with_provider_override
    cargo test "''${test_flags[@]}" -p codex-app-server --test all thread_resume_recovers_after_provider_configuration_is_restored
    sha256sum --check "$TMPDIR/native-release-binaries.sha256"
    runHook postCheck
  '';
  passthru = (old.passthru or { }) // {
    codexProvenance = source // {
      repository = "github.com/aither64/codex";
      inherit recipeRevision;
      sourcePath = toString nativeSource;
    };
  };
})
