{
  activationEnvironmentAliases ? [ ],
  bash,
  buildGoModule,
  coreutils,
  codex,
  codexWebSrc,
  codexWebRev,
  git,
  gh,
  jq,
  lib,
  makeWrapper,
  nodejs,
  nix,
  openssl,
  python3,
  ruby,
  routerSocket,
  systemd,
  tmux,
  userNamespace ? "dev-workspaces",
  util-linux,
  writeText,
  extensions ? { },
  src,
}:
let
  contractPython = python3.withPackages (pythonPackages: [ pythonPackages.jsonschema ]);
  validActivationEnvironmentAliases =
    builtins.isList activationEnvironmentAliases
    &&
      builtins.length activationEnvironmentAliases
      == builtins.length (lib.unique activationEnvironmentAliases)
    && builtins.all (
      name:
      builtins.isString name
      && builtins.match "[A-Z][A-Z0-9_]*" name != null
      && name != "DEV_WORKSPACE_ACTIVATION"
    ) activationEnvironmentAliases;
  validUserNamespace =
    builtins.stringLength userNamespace <= 63
    && builtins.match "[a-z0-9][a-z0-9-]*" userNamespace != null;
  validRouterSocket =
    let
      components = lib.splitString "/" routerSocket;
    in
    lib.hasPrefix "/run/" routerSocket
    && lib.all (component: component != "" && component != "." && component != "..") (
      builtins.tail components
    );
  extensionCommands = extensions.commands or { };
  extensionSkills = extensions.skills or { };
  clusterProviders = extensions.clusterProviders or { };
  extensionsValid = builtins.all (
    name:
    builtins.elem name [
      "clusterProviders"
      "commands"
      "skills"
    ]
  ) (builtins.attrNames extensions);
  validName =
    name: builtins.stringLength name <= 63 && builtins.match "[a-z0-9][a-z0-9-]*" name != null;
  validTarget =
    value:
    (builtins.isString value || builtins.isPath value || lib.isDerivation value)
    && (
      let
        string = toString value;
      in
      builtins.hasContext string && lib.hasPrefix "${builtins.storeDir}/" string
    );
  commandNames = builtins.attrNames extensionCommands;
  skillNames = builtins.attrNames extensionSkills;
  providerNames = builtins.attrNames clusterProviders;
  providerPrograms = map (name: "${name}-devcluster") providerNames;
  allNamesValid = builtins.all validName (commandNames ++ skillNames ++ providerNames);
  extensionTargets =
    builtins.attrValues extensionCommands
    ++ builtins.attrValues extensionSkills
    ++ map (name: clusterProviders.${name}.command or "") providerNames;
  invalidTargets = builtins.filter (value: !validTarget value) extensionTargets;
  targetsValid = invalidTargets == [ ];
  programNames = commandNames ++ providerPrograms;
  programsValid =
    builtins.length programNames == builtins.length (lib.unique programNames)
    && builtins.all (
      name:
      !(builtins.elem name [
        "workspace-host"
        "workspace-portal"
        "dev-session"
      ])
    ) programNames;
  providersValid = builtins.all (
    name:
    let
      provider = clusterProviders.${name};
    in
    builtins.isAttrs provider
    &&
      builtins.attrNames provider == [
        "command"
        "label"
      ]
    && provider ? label
    && provider ? command
    && builtins.isString provider.label
    && provider.label != ""
    && builtins.stringLength provider.label <= 128
  ) providerNames;
  extensionCatalogData = {
    schema = 1;
    commands = map (name: {
      inherit name;
      path = toString extensionCommands.${name};
    }) commandNames;
    skills = map (name: {
      inherit name;
      path = toString extensionSkills.${name};
    }) skillNames;
    clusterProviders = map (name: {
      id = name;
      inherit (clusterProviders.${name}) label;
      command = toString clusterProviders.${name}.command;
    }) providerNames;
  };
  extensionCatalog = writeText "dev-workspace-extensions.json" (builtins.toJSON extensionCatalogData);
  packageConfiguration = writeText "dev-workspace-package.json" (
    builtins.toJSON {
      schema = 1;
      inherit activationEnvironmentAliases routerSocket userNamespace;
    }
  );
in
assert lib.assertMsg validActivationEnvironmentAliases
  "dev-workspace activation environment aliases must be unique uppercase names";
assert lib.assertMsg validUserNamespace
  "dev-workspace user namespace must be a lowercase identifier";
assert lib.assertMsg validRouterSocket
  "dev-workspace router socket must be an absolute path below /run";
assert lib.assertMsg extensionsValid "dev-workspace extensions contain an unknown section";
assert lib.assertMsg allNamesValid "dev-workspace extension names must be lowercase identifiers";
assert lib.assertMsg targetsValid
  "dev-workspace extension targets must be immutable Nix store references: ${builtins.toJSON (map toString invalidTargets)}";
assert lib.assertMsg programsValid "dev-workspace extension commands must not collide";
assert lib.assertMsg providersValid "dev-workspace cluster providers require label and command";
buildGoModule {
  pname = "dev-workspace";
  version = "0.2.0";

  inherit src;
  modRoot = "portal";
  vendorHash = "sha256-8NzgurFJHLPL2ITTeXbXuD33afPxj3i+hmQKIQcWOVE=";

  postPatch = ''
    expected=${lib.escapeShellArg (builtins.substring 0 12 codexWebRev)}
    module_file=portal/go.mod
    [ -f "$module_file" ] || module_file=go.mod
    if ! grep -Eq "github.com/aither64/codex-web v0.0.0-[0-9]{14}-$expected" "$module_file"; then
      echo "portal/go.mod does not pin codex-web revision $expected" >&2
      exit 1
    fi
  '';

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
    export DEV_SESSION_SKIP_REAL_TMUX_TESTS=1
    mkdir -p "$HOME" "$TMUX_TMPDIR"
    ${contractPython}/bin/python3 ${codexWebSrc}/test/codex_protocol_contract.py \
      --coverage-only ${codexWebSrc}/codex/client.go
    go test ./...
    node --check internal/web/static/app.js
    (
      cd ..
      ruby test/dev_session_test.rb
      ruby test/workspace_host_test.rb
    )
    runHook postCheck
  '';

  postInstall = ''
    mkdir -p "$out/libexec/workspace-portal" "$out/share/dev-workspace"
    ln -s ${codex} "$out/libexec/codex"
    install -Dm755 ${src}/libexec/dev-session \
      "$out/libexec/workspace-portal/dev-session"
    install -Dm755 ${src}/libexec/workspace-host \
      "$out/libexec/workspace-host"
    install -Dm644 ${src}/libexec/workspace-profile-identity.rb \
      "$out/libexec/workspace-profile-identity.rb"
    ln -s ../workspace-profile-identity.rb \
      "$out/libexec/workspace-portal/workspace-profile-identity.rb"
    install -Dm644 ${codexWebSrc}/test/codex_protocol_contract.py \
      "$out/share/workspace-portal/codex_protocol_contract.py"
    install -Dm644 ${codexWebSrc}/codex/client.go \
      "$out/share/workspace-portal/codex-client.go"
    install -Dm644 ${src}/portal/internal/session/runtime-contract.json \
      "$out/share/workspace-portal/runtime-contract.json"
    install -Dm644 ${extensionCatalog} "$out/share/dev-workspace/extensions.json"
    install -Dm644 ${packageConfiguration} "$out/share/dev-workspace/package.json"
    install -Dm644 ${src}/nix/systemd/workspace-* \
      -t "$out/share/systemd/user"
    substituteInPlace "$out/share/systemd/user/"workspace-*.service \
      --replace-fail '%h/.local/state/dev-workspaces/profile' \
      '%h/.local/state/${userNamespace}/profile'

    ${lib.concatMapStringsSep "\n" (name: ''
      ln -s ${lib.escapeShellArg (toString extensionCommands.${name})} \
        "$out/bin/${name}"
    '') commandNames}
    mkdir -p "$out/share/codex/skills"
    ${lib.concatMapStringsSep "\n" (name: ''
      ln -s ${lib.escapeShellArg (toString extensionSkills.${name})} \
        "$out/share/codex/skills/${name}"
    '') skillNames}
    ${lib.concatMapStringsSep "\n" (name: ''
      ln -s ${lib.escapeShellArg (toString clusterProviders.${name}.command)} \
        "$out/libexec/workspace-portal/${name}-devcluster"
    '') providerNames}

    substituteInPlace "$out/libexec/workspace-portal/dev-session" \
      --replace-fail '#!/usr/bin/env ruby' '#!${ruby}/bin/ruby'
    substituteInPlace "$out/libexec/workspace-host" \
      --replace-fail '#!/usr/bin/env ruby' '#!${ruby}/bin/ruby'

    runtimePath=${
      lib.makeBinPath [
        coreutils
        git
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
    for command in workspace-host dev-session ${lib.concatStringsSep " " providerPrograms}; do
      makeWrapper "$out/libexec/workspace-host" "$out/bin/$command" \
        --set DEV_WORKSPACE_HOST_MODE "$command" \
        --set DEV_WORKSPACES_EXTENSION_CATALOG "$out/share/dev-workspace/extensions.json" \
        --set DEV_WORKSPACE_ACTIVATION_ALIASES \
          ${lib.escapeShellArg (lib.concatStringsSep ":" activationEnvironmentAliases)} \
        --set-default DEV_WORKSPACES_NAMESPACE ${lib.escapeShellArg userNamespace} \
        --set-default DEV_WORKSPACES_ROUTER_SOCKET ${lib.escapeShellArg routerSocket}
    done
  '';

  postFixup = ''
    mkdir -p "$TMPDIR/workspace"
    test -x "$out/bin/dev-session"
    test -x "$out/bin/workspace-host"
    test -f "$out/share/workspace-portal/runtime-contract.json"
    test -f "$out/share/dev-workspace/extensions.json"
    test -f "$out/share/dev-workspace/package.json"
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
    ${lib.concatMapStringsSep "\n" (name: ''
      test -x "$out/bin/${name}-devcluster"
      ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
        DEVCLUSTER_WORKSPACE="$TMPDIR/workspace" \
        "$out/bin/${name}-devcluster" --help >/dev/null
    '') providerNames}
    ${lib.concatMapStringsSep "\n" (name: ''
      test -f "$out/bin/${name}"
      test -x "$out/bin/${name}"
    '') commandNames}
    ${lib.concatMapStringsSep "\n" (name: ''
      test -d "$out/share/codex/skills/${name}"
    '') skillNames}
    ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
      "$out/libexec/workspace-portal/dev-session" --help >/dev/null
    ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
      "$out/bin/workspace-host" --help >/dev/null
    ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
      "$out/bin/dev-session" --help >/dev/null
    if ${coreutils}/bin/env -i PATH=/empty HOME="$TMPDIR" \
      DEV_WORKSPACE_ACTIVATION_ALIASES=HOSTILE_WORKSPACE_ACTIVATION \
      HOSTILE_WORKSPACE_ACTIVATION=1 \
      "$out/bin/workspace-host" _activate >"$TMPDIR/hostile-activation" 2>&1; then
      echo "ambient activation alias bypassed the packaged wrapper" >&2
      exit 1
    fi
    grep -q 'workspace activation is private' "$TMPDIR/hostile-activation"
  '';

  meta = {
    description = "Persistent development workspaces backed by Codex App Server";
    mainProgram = "workspace-portal";
    platforms = lib.platforms.linux;
  };
}
