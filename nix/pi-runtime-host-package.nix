{
  lib,
  runCommand,
  ruby,
  bash,
  coreutils,
  systemd,
  writeText,
  ownerUid,
  ownerHome,
  ownerName,
  stateRoot,
}:
let
  configuration = writeText "workspace-pi-host.json" (builtins.toJSON {
    owner_uid = ownerUid;
    state_root = stateRoot;
    systemctl = "${systemd}/bin/systemctl";
  });
  wrapper = writeText "workspace-pi-host-wrapper" ''
    #!${bash}/bin/bash
    exec ${coreutils}/bin/env -i LANG=C.UTF-8 LC_ALL=C.UTF-8 \
      PATH=/run/wrappers/bin:/run/current-system/sw/bin \
      NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
      SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
      HOME=${lib.escapeShellArg ownerHome} TMPDIR=/tmp \
      USER=${lib.escapeShellArg ownerName} LOGNAME=${lib.escapeShellArg ownerName} \
      SUDO_UID="''${SUDO_UID-}" INVOCATION_ID="''${INVOCATION_ID-}" \
      ${ruby}/bin/ruby "@out@/libexec/@command@" "$@"
  '';
in
runCommand "workspace-pi-host" { } ''
  mkdir -p "$out/libexec" "$out/bin"
  substitute ${../libexec/workspace-pi-host} "$out/libexec/workspace-pi-host" \
    --replace-fail '@piHostConfig@' ${lib.escapeShellArg (toString configuration)}
  cp ${../libexec/workspace-pi-session-entry} "$out/libexec/workspace-pi-session-entry"
  for command in workspace-pi-host workspace-pi-session-entry; do
    substitute ${wrapper} "$out/bin/$command" \
      --replace-fail '@out@' "$out" --replace-fail '@command@' "$command"
    chmod 0555 "$out/bin/$command"
  done
''
