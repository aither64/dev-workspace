{
  bash,
  buildGoModule,
  coreutils,
  git,
  gh,
  gnugrep,
  lib,
  makeWrapper,
  nodejs,
  nix,
  openssl,
  jq,
  python3,
  ruby,
  systemd,
  tmux,
  util-linux,
  src,
}:
let
  contractPython = python3.withPackages (pythonPackages: [ pythonPackages.jsonschema ]);
in
buildGoModule {
  pname = "dev-workspace";
  version = "0.1.0";

  inherit src;
  modRoot = "portal";
  vendorHash = "sha256-Hz8lcK5n0rrcHcyO7lkegl1xK8AhcDOOlrBtyCNFk/8=";

  subPackages = [ "cmd/workspace-portal" ];
  nativeBuildInputs = [ makeWrapper ];
  nativeCheckInputs = [
    bash
    coreutils
    git
    jq
    nodejs
    openssl
    contractPython
    ruby
    tmux
    util-linux
  ];

  checkPhase = ''
    runHook preCheck
    export HOME="$TMPDIR/home"
    export LANG=C.UTF-8
    export LC_ALL=C.UTF-8
    export SHELL=${bash}/bin/bash
    export TMUX_TMPDIR="$TMPDIR/tmux"
    export VPSFREE_DEV_SESSION_SKIP_REAL_TMUX_TESTS=1
    mkdir -p "$HOME" "$TMUX_TMPDIR"
    patchShebangs \
      ../dev-clusters/vpsadmin/bin/devcluster \
      ../dev-clusters/vpsadminos/bin/devcluster
    ${contractPython}/bin/python3 ../test/codex_protocol_contract.py \
      --coverage-only internal/codex/client.go
    go test ./...
    node --check internal/web/static/app.js
    (
      cd ..
      ruby test/dev_session_test.rb
      ruby test/devcluster_status_test.rb
      ruby test/workspace_host_test.rb
    )
    runHook postCheck
  '';

  postInstall = ''
    install -Dm755 ${src}/libexec/dev-session \
      "$out/libexec/workspace-portal/dev-session"
    install -Dm755 ${src}/libexec/workspace-host \
      "$out/libexec/workspace-host"
    install -Dm644 ${src}/libexec/workspace-profile-identity.rb \
      "$out/libexec/workspace-profile-identity.rb"
    ln -s ../workspace-profile-identity.rb \
      "$out/libexec/workspace-portal/workspace-profile-identity.rb"
    install -Dm644 ${src}/test/codex_protocol_contract.py \
      "$out/share/workspace-portal/codex_protocol_contract.py"
    install -Dm644 ${src}/portal/internal/codex/client.go \
      "$out/share/workspace-portal/codex-client.go"
    install -Dm644 ${src}/portal/internal/session/runtime-contract.json \
      "$out/share/workspace-portal/runtime-contract.json"
    install -Dm644 ${src}/nix/systemd/workspace-* \
      -t "$out/share/systemd/user"
    cp -R ${src}/dev-clusters/vpsadmin "$out/share/workspace-portal/vpsadmin-devcluster"
    cp -R ${src}/dev-clusters/vpsadminos "$out/share/workspace-portal/vpsadminos-devcluster"
    cp -R ${src}/dev-clusters/lib "$out/share/workspace-portal/lib"
    substituteInPlace "$out/libexec/workspace-portal/dev-session" \
      --replace-fail '#!/usr/bin/env ruby' '#!${ruby}/bin/ruby'
    substituteInPlace "$out/libexec/workspace-host" \
      --replace-fail '#!/usr/bin/env ruby' '#!${ruby}/bin/ruby'
    substituteInPlace \
      "$out/share/workspace-portal/vpsadmin-devcluster/bin/devcluster" \
      "$out/share/workspace-portal/vpsadminos-devcluster/bin/devcluster" \
      --replace-fail '#!/usr/bin/env bash' '#!${bash}/bin/bash'

    runtimePath=${
      lib.makeBinPath [
        coreutils
        git
        gnugrep
        ruby
        tmux
      ]
    }
    wrapProgram "$out/libexec/workspace-portal/dev-session" \
      --prefix PATH : "$runtimePath"
    hostRuntimePath=${
      lib.makeBinPath [
        coreutils
        gh
        git
        nix
        contractPython
        ruby
        systemd
        tmux
      ]
    }
    wrapProgram "$out/libexec/workspace-host" \
      --prefix PATH : "$hostRuntimePath"
    clusterRuntimePath=${lib.makeBinPath [ bash coreutils git gnugrep jq openssl util-linux ]}
    makeWrapper "$out/share/workspace-portal/vpsadmin-devcluster/bin/devcluster" \
      "$out/libexec/workspace-portal/vpsadmin-devcluster" --prefix PATH : "$clusterRuntimePath"
    makeWrapper "$out/share/workspace-portal/vpsadminos-devcluster/bin/devcluster" \
      "$out/libexec/workspace-portal/vpsadminos-devcluster" --prefix PATH : "$clusterRuntimePath"
    for command in workspace-host dev-session vpsadmin-devcluster vpsadminos-devcluster; do
      makeWrapper "$out/libexec/workspace-host" "$out/bin/$command" \
        --set VPSFREE_WORKSPACE_HOST_MODE "$command"
    done
  '';

  postFixup = ''
    mkdir -p "$TMPDIR/workspace"
    test -x "$out/bin/dev-session"
    test -x "$out/bin/workspace-host"
    test -f "$out/share/workspace-portal/runtime-contract.json"
    wrapped="$out/libexec/workspace-portal/.dev-session-wrapped"
    if ! head -n 1 "$wrapped" | grep -Eq '^#! */nix/store/'; then
      echo "wrapped helper has a non-store interpreter: $wrapped" >&2
      exit 1
    fi
    wrapped="$out/libexec/.workspace-host-wrapped"
    if ! head -n 1 "$wrapped" | grep -Eq '^#! */nix/store/'; then
      echo "wrapped helper has a non-store interpreter: $wrapped" >&2
      exit 1
    fi
    for program in vpsadmin-devcluster vpsadminos-devcluster; do
      if ! head -n 1 "$out/bin/$program" | grep -Eq '^#! */nix/store/'; then
        echo "cluster helper has a non-store interpreter: $out/bin/$program" >&2
        exit 1
      fi
      ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
        VPSFREE_DEVCLUSTER_WORKSPACE="$TMPDIR/workspace" \
        "$out/libexec/workspace-portal/$program" --help >/dev/null
    done

    ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
      "$out/libexec/workspace-portal/dev-session" --help >/dev/null
    ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
      "$out/bin/workspace-host" --help >/dev/null
    ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
      "$out/bin/dev-session" --help >/dev/null
  '';

  meta = {
    description = "Persistent development workspaces backed by Codex App Server";
    mainProgram = "workspace-portal";
    platforms = lib.platforms.linux;
  };
}
