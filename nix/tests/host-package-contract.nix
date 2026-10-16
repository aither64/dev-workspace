{
  devWorkspace,
  hostPaths,
  pkgs,
}:
pkgs.runCommand "dev-workspace-host-package-contract" { nativeBuildInputs = [ pkgs.jq ]; } ''
  metadata=${devWorkspace}/share/dev-workspace/package.json
  test "$(jq -r .routerSocket "$metadata")" = ${pkgs.lib.escapeShellArg hostPaths.routerSocket}
  jq -e '.integration == {journalSchema: 1, certificationSchema: 1}' "$metadata" >/dev/null
  jq -e '. == {schema: 1, journalSchemas: [1, 2], certificationSchema: 1}' \
    ${devWorkspace}/share/dev-workspace/integration-contract.json >/dev/null
  touch "$out"
''
