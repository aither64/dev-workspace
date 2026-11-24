{
  codex,
  mkCodexPackage,
  pkgs,
}:
let
  inherit (pkgs) lib;
  hasExecServer = lib.versionAtLeast codex.version "0.172.0";
  executables = [
    "codex"
    "codex-code-mode-host"
    "logs_client"
  ]
  ++ lib.optional hasExecServer "codex-exec-server";
  assembled = mkCodexPackage { inherit pkgs codex; };
  linkedInput = pkgs.runCommand "codex-linked-input" { version = "0.171.0"; } ''
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
  executorInput = linkedInput.overrideAttrs (old: {
    version = "0.172.0";
    buildCommand = old.buildCommand + ''
      ln -s ${pkgs.coreutils}/bin/true "$out/payload/bin/codex-exec-server"
    '';
  });
  executorPackage = mkCodexPackage {
    inherit pkgs;
    codex = executorInput;
  };
  missingExecutorInput = linkedInput.overrideAttrs { version = "0.172.0"; };
  missingExecutorPackage = mkCodexPackage {
    inherit pkgs;
    codex = missingExecutorInput;
  };
  missingInput = pkgs.runCommand "codex-missing-input" { version = "0.171.0"; } ''
    mkdir -p "$out/libexec/codex/bin"
    ln -s ${pkgs.coreutils}/bin/true "$out/libexec/codex/bin/codex"
    ln -s ${pkgs.coreutils}/bin/true "$out/libexec/codex/bin/codex-code-mode-host"
  '';
  missingPackage = mkCodexPackage {
    inherit pkgs;
    codex = missingInput;
  };
  fixture =
    pkgs.runCommand "dev-workspace-codex-package-fixture"
      {
        nativeBuildInputs = [
          pkgs.diffutils
          pkgs.findutils
          pkgs.jq
        ];
      }
      ''
        for runtime in ${linkedPackage}/libexec/codex ${executorPackage}/libexec/codex; do
          test -z "$(find "$runtime" -type l -print -quit)"
          for executable in codex codex-code-mode-host logs_client; do
            cmp "$runtime/bin/$executable" ${pkgs.coreutils}/bin/true
          done
          cmp "$runtime/codex-resources/bundled" ${linkedInput}/payload/resources/bundled
          cmp "$runtime/resource-link" ${linkedInput}/payload/resources/bundled
        done
        jq -e '. == {layoutVersion: 1, version: "0.171.0", target: "x86_64-unknown-linux-gnu",
          variant: "codex", entrypoint: "bin/codex", resourcesDir: "codex-resources", pathDir: "codex-path"}' \
          ${linkedPackage}/libexec/codex/codex-package.json >/dev/null
        test ! -e ${linkedPackage}/bin/codex-exec-server
        jq -e '. == {layoutVersion: 1, version: "0.172.0", target: "x86_64-unknown-linux-gnu",
          variant: "codex", entrypoint: "bin/codex", execServer: "bin/codex-exec-server",
          resourcesDir: "codex-resources", pathDir: "codex-path"}' \
          ${executorPackage}/libexec/codex/codex-package.json >/dev/null
        cmp ${executorPackage}/libexec/codex/bin/codex-exec-server ${pkgs.coreutils}/bin/true
        test "$(readlink -f ${executorPackage}/bin/codex-exec-server)" = \
          ${executorPackage}/libexec/codex/bin/codex-exec-server

        # Execute the real assembly script without depending on failed derivations.
        if out="$TMPDIR/missing-output" ${pkgs.bash}/bin/bash ${missingPackage.assemblyScript} > missing.log 2>&1; then
          echo "assembly accepted an input without logs_client" >&2
          exit 1
        fi
        grep -F 'codex-package: missing required executable:' missing.log
        grep -F '/bin/logs_client' missing.log
        if out="$TMPDIR/missing-executor-output" ${pkgs.bash}/bin/bash ${missingExecutorPackage.assemblyScript} > missing-executor.log 2>&1; then
          echo "assembly accepted a new-layout input without codex-exec-server" >&2
          exit 1
        fi
        grep -F 'codex-package: missing required executable:' missing-executor.log
        grep -F '/bin/codex-exec-server' missing-executor.log
        touch "$out"
      '';
in
pkgs.runCommand "dev-workspace-codex-package"
  {
    passthru = { inherit fixture; };
    nativeBuildInputs = [
      pkgs.diffutils
      pkgs.findutils
      pkgs.jq
    ];
  }
  ''
    test -e ${fixture}
    runtime=${assembled}/libexec/codex
    jq -e --arg version ${pkgs.lib.escapeShellArg codex.version} \
      --argjson hasExecServer ${lib.boolToString hasExecServer} \
      --arg target ${pkgs.lib.escapeShellArg pkgs.stdenv.hostPlatform.rust.rustcTarget} '
      . == ({layoutVersion: 1, version: $version, target: $target, variant: "codex",
        entrypoint: "bin/codex", resourcesDir: "codex-resources", pathDir: "codex-path"}
        + (if $hasExecServer then {execServer: "bin/codex-exec-server"} else {} end))
    ' "$runtime/codex-package.json" >/dev/null
    test ${pkgs.lib.escapeShellArg assembled.version} = ${pkgs.lib.escapeShellArg codex.version}
    test -z "$(find "$runtime" -type l -print -quit)"

    for executable in ${lib.escapeShellArgs executables}; do
      test -x "$runtime/bin/$executable"
      cmp "$runtime/bin/$executable" ${codex}/libexec/codex/bin/$executable
      test "$(od -An -tx1 -N4 "$runtime/bin/$executable" | tr -d ' \n')" = 7f454c46
    done
    for executable in ${lib.escapeShellArgs (lib.tail executables)}; do
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

    touch "$out"
  ''
