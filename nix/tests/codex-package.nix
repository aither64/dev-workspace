{
  codex,
  mkCodexPackage,
  pkgs,
}:
let
  assembled = mkCodexPackage { inherit pkgs codex; };
  linkedInput = pkgs.runCommand "codex-linked-input" { version = "link-fixture"; } ''
    mkdir -p "$out/libexec/codex" "$out/payload/bin" "$out/payload/resources" \
      "$out/share/bash-completion/completions" "$out/share/fish/vendor_completions.d" \
      "$out/share/zsh/site-functions"
    for executable in codex codex-code-mode-host logs_client; do
      ln -s ${pkgs.coreutils}/bin/true "$out/payload/bin/$executable"
    done
    printf 'bundled resource\n' > "$out/payload/resources/bundled"
    ln -s "$out/payload/bin" "$out/libexec/codex/bin"
    ln -s "$out/payload/resources" "$out/libexec/codex/codex-resources"
    ln -s "$out/payload/resources/bundled" "$out/libexec/codex/resource-link"
    touch "$out/share/bash-completion/completions/codex.bash" \
      "$out/share/fish/vendor_completions.d/codex.fish" "$out/share/zsh/site-functions/_codex"
  '';
  linkedPackage = mkCodexPackage {
    inherit pkgs;
    codex = linkedInput;
  };
  missingInput = pkgs.runCommand "codex-missing-input" { version = "missing-fixture"; } ''
    mkdir -p "$out/libexec/codex/bin"
    ln -s ${pkgs.coreutils}/bin/true "$out/libexec/codex/bin/codex"
    ln -s ${pkgs.coreutils}/bin/true "$out/libexec/codex/bin/codex-code-mode-host"
  '';
  missingPackage = mkCodexPackage {
    inherit pkgs;
    codex = missingInput;
  };
in
pkgs.runCommand "dev-workspace-codex-package" {
  nativeBuildInputs = [
    pkgs.diffutils
    pkgs.findutils
    pkgs.jq
  ];
} ''
  runtime=${assembled}/libexec/codex
  jq -e --arg version ${pkgs.lib.escapeShellArg codex.version} \
    --arg target ${pkgs.lib.escapeShellArg pkgs.stdenv.hostPlatform.rust.rustcTarget} '
    . == {layoutVersion: 1, version: $version, target: $target, variant: "codex",
      entrypoint: "bin/codex", resourcesDir: "codex-resources", pathDir: "codex-path"}
  ' "$runtime/codex-package.json" >/dev/null
  test ${pkgs.lib.escapeShellArg assembled.version} = ${pkgs.lib.escapeShellArg codex.version}
  test -z "$(find "$runtime" -type l -print -quit)"

  for executable in codex codex-code-mode-host logs_client; do
    test -x "$runtime/bin/$executable"
    cmp "$runtime/bin/$executable" ${codex}/libexec/codex/bin/$executable
    test "$(od -An -tx1 -N4 "$runtime/bin/$executable" | tr -d ' \n')" = 7f454c46
  done
  for executable in codex-code-mode-host logs_client; do
    test "$(readlink -f ${assembled}/bin/$executable)" = "$runtime/bin/$executable"
  done
  test -x "$runtime/codex-path/rg"
  test -x "$runtime/codex-resources/bwrap"
  cmp "$runtime/codex-path/rg" ${pkgs.ripgrep}/bin/rg
  cmp "$runtime/codex-resources/bwrap" ${pkgs.bubblewrap}/bin/bwrap
  for executable in codex-path/rg codex-resources/bwrap; do
    test "$(od -An -tx1 -N4 "$runtime/$executable" | tr -d ' \n')" = 7f454c46
  done
  test "$(readlink -f ${assembled}/bin/codex)" = ${assembled}/bin/codex
  grep -F "$runtime/bin/codex" ${assembled}/bin/codex
  grep -F ${pkgs.lib.escapeShellArg (pkgs.lib.makeBinPath [ pkgs.bubblewrap ])} ${assembled}/bin/codex
  if grep -F ${pkgs.lib.escapeShellArg (toString codex)} ${assembled}/bin/codex; then
    echo "public launcher still refers to the incomplete input package" >&2
    exit 1
  fi
  for completion in bash-completion/completions/codex.bash fish/vendor_completions.d/codex.fish zsh/site-functions/_codex; do
    test -f ${assembled}/share/$completion
    cmp ${assembled}/share/$completion ${codex}/share/$completion
  done
  export CODEX_HOME="$TMPDIR/codex-home"
  mkdir -m 700 "$CODEX_HOME"
  input_version=$(${codex}/libexec/codex/bin/codex --version)
  output_version=$(${assembled}/bin/codex --version)
  test "$input_version" = "codex-cli ${codex.version}"
  test "$output_version" = "$input_version"

  linked_runtime=${linkedPackage}/libexec/codex
  test -z "$(find "$linked_runtime" -type l -print -quit)"
  for executable in codex codex-code-mode-host logs_client; do
    cmp "$linked_runtime/bin/$executable" ${pkgs.coreutils}/bin/true
  done
  cmp "$linked_runtime/codex-resources/bundled" ${linkedInput}/payload/resources/bundled
  cmp "$linked_runtime/resource-link" ${linkedInput}/payload/resources/bundled
  jq -e '.version == "link-fixture"' "$linked_runtime/codex-package.json" >/dev/null

  # Exercise the same assembly script without making a failed derivation a dependency.
  if out="$TMPDIR/missing-output" ${pkgs.bash}/bin/bash ${missingPackage.assemblyScript} > missing.log 2>&1; then
    echo "assembly accepted an input without logs_client" >&2
    exit 1
  fi
  grep -F 'codex-package: missing required executable:' missing.log
  grep -F '/bin/logs_client' missing.log
  touch "$out"
''
