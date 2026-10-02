{ pkgs, codex }:
let
  inherit (pkgs) lib;
  manifest = pkgs.writeText "codex-package.json" (
    builtins.toJSON {
      layoutVersion = 1;
      version = codex.version;
      target = pkgs.stdenv.hostPlatform.rust.rustcTarget;
      variant = "codex";
      entrypoint = "bin/codex";
      resourcesDir = "codex-resources";
      pathDir = "codex-path";
    }
  );
  assemblyScript = pkgs.writeText "assemble-codex-package" ''
    set -euo pipefail
    source=${codex}/libexec/codex
    runtime="$out/libexec/codex"

    require_executable() {
      if [ ! -f "$1" ] || [ ! -x "$1" ]; then
        echo "codex-package: missing required executable: $1" >&2
        exit 1
      fi
    }

    for executable in codex codex-code-mode-host logs_client; do
      require_executable "$source/bin/$executable"
    done
    require_executable ${pkgs.ripgrep}/bin/rg
    ${lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
      require_executable ${pkgs.bubblewrap}/bin/bwrap
    ''}

    mkdir -p "$out/bin" "$runtime"
    # Daemon package copies reject escaping links, including directory links.
    cp -RL --preserve=mode,timestamps "$source/." "$runtime/"
    find "$runtime" -type d -exec chmod u+w {} +
    mkdir -p "$runtime/codex-resources" "$runtime/codex-path"
    cp -L --remove-destination ${pkgs.ripgrep}/bin/rg "$runtime/codex-path/rg"
    ${lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
      cp -L --remove-destination ${pkgs.bubblewrap}/bin/bwrap "$runtime/codex-resources/bwrap"
    ''}
    cp --remove-destination ${manifest} "$runtime/codex-package.json"
    if [ -n "$(find "$runtime" -type l -print -quit)" ]; then
      echo "codex-package: runtime root still contains a symlink" >&2
      exit 1
    fi

    makeWrapper "$runtime/bin/codex" "$out/bin/codex" \
      ${lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
        --prefix PATH : ${lib.makeBinPath [ pkgs.bubblewrap ]}
      ''}
    for executable in codex-code-mode-host logs_client; do
      ln -s "$runtime/bin/$executable" "$out/bin/$executable"
    done
    ln -s ${codex}/share "$out/share"
  '';
in
assert lib.assertMsg (pkgs.stdenv.hostPlatform.system == "x86_64-linux")
  "mkCodexPackage currently supports the x86_64-linux native Codex layout only";
pkgs.runCommand "codex-package-${codex.version}" {
  inherit (codex) version;
  nativeBuildInputs = [ pkgs.makeWrapper ];
  passthru = { inherit assemblyScript; };
  meta = (codex.meta or { }) // { platforms = [ "x86_64-linux" ]; };
} ''
  source ${assemblyScript}
''
