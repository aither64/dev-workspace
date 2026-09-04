{
  bash,
  apacheHttpd,
  buildGoModule,
  codex,
  coreutils,
  git,
  gnugrep,
  lib,
  makeWrapper,
  nodejs,
  openssl,
  python3,
  python3Packages,
  ruby,
  tmux,
  src,
}:
buildGoModule {
  pname = "workspace-portal";
  version = "0.1.0";

  inherit src;
  modRoot = "portal";
  vendorHash = "sha256-W8qZsxbBqSEeaUcdu8wYfjhepn94v4T6xX0WQlgOWuE=";

  subPackages = [ "cmd/workspace-portal" ];
  nativeBuildInputs = [ makeWrapper ];
  nativeCheckInputs = [
    bash
    codex
    coreutils
    git
    nodejs
    openssl
    python3
    python3Packages.jsonschema
    ruby
    tmux
    apacheHttpd
  ];

  checkPhase = ''
    runHook preCheck
    export HOME="$TMPDIR/home"
    export SHELL=${bash}/bin/bash
    export TMUX_TMPDIR="$TMPDIR/tmux"
    export VPSFREE_DEV_SESSION_SKIP_REAL_TMUX_TESTS=1
    mkdir -p "$HOME" "$TMUX_TMPDIR"
    schema_dir="$TMPDIR/codex-schema"
    codex app-server generate-json-schema --experimental --out "$schema_dir"
    export VPSFREE_CODEX_SCHEMA_DIR="$schema_dir"
    python3 ../test/codex_protocol_contract.py "$schema_dir"
    go test ./...
    node --check internal/web/static/app.js
    (
      cd ..
      ruby test/dev_session_test.rb
      ruby test/workspace_pki_test.rb
      ruby test/workspace_portal_password_test.rb
    )
    runHook postCheck
  '';

  postInstall = ''
    install -Dm755 ${src}/bin/dev-session "$out/bin/dev-session"
    install -Dm755 ${src}/bin/workspace-pki "$out/bin/workspace-pki"
    install -Dm755 ${src}/bin/workspace-portal-password-hash \
      "$out/bin/workspace-portal-password-hash"
    install -Dm644 ${src}/portal/runtime-contract.json \
      "$out/share/workspace-portal/runtime-contract.json"

    runtimePath=${
      lib.makeBinPath [
        coreutils
        git
        gnugrep
        apacheHttpd
        openssl
        ruby
        tmux
      ]
    }
    for program in dev-session workspace-pki workspace-portal-password-hash; do
      wrapProgram "$out/bin/$program" \
        --prefix PATH : "$runtimePath"
    done
  '';

  passthru.codexPackage = codex;

  meta = {
    description = "Browser interface for vpsFree.cz development sessions";
    mainProgram = "workspace-portal";
    platforms = lib.platforms.linux;
  };
}
