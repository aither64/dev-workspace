{ pkgs, codex }:
let
  version = "0.172.0";
  sourceHash = "sha256-MDLBCNRb/1DNPZWegnF2yFDQU4IdSsgQcYOh0JV3EN8=";
  selected = codex.override {
    inherit version;
    hash = sourceHash;
    srcOverride = pkgs.fetchFromGitHub {
      name = "source";
      owner = "openai";
      repo = "codex";
      rev = "3d86e43b075adeba2193ec5c22ac92f55afd82e6";
      hash = sourceHash;
    };
    sourceRoot = "source/codex-rs";
    cargoVendor = {
      cargoHash = "sha256-baArx7G/DmmT+1k4ykHoBnVU73ta0F+Hai4hWBkHV/Q=";
    };
    librusty_v8 = codex.mkRustyV8Archive {
      version = "152.2.0";
      profile = "ptrcomp_sandbox_release";
      baseUrl = "https://github.com/openai/codex/releases/download/rusty-v8-v152.2.0";
      hashes.x86_64-linux = "sha256-s7SQ8qqAntp/ascn+FH7AM6DVb5ygnV3z6rCjHE3280=";
      srcBindingHashes.x86_64-linux = "sha256-Pk1f7Nvg+YpgF+adfKglGad/sDLF615M2+zdPi7FGdU=";
    };
  };
in
selected.overrideAttrs (old: {
  cargoBuildFlags = old.cargoBuildFlags ++ [
    "--package"
    "codex-exec-server-cli"
  ];
  nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ pkgs.jq ];
  postFixup = (old.postFixup or "") + ''
    mv "$out/bin/codex-exec-server" "$out/libexec/codex/bin/"
    ln -s ../libexec/codex/bin/codex-exec-server "$out/bin/codex-exec-server"
    jq '. + {execServer: "bin/codex-exec-server"}' \
      "$out/libexec/codex/codex-package.json" > "$out/libexec/codex/codex-package.json.new"
    mv "$out/libexec/codex/codex-package.json.new" "$out/libexec/codex/codex-package.json"
  '';
})
