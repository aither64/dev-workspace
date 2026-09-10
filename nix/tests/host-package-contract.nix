{
  devWorkspace,
  hostPaths,
  pkgs,
}:
pkgs.runCommand "dev-workspace-host-package-contract" { nativeBuildInputs = [ pkgs.jq ]; } ''
  metadata=${devWorkspace}/share/dev-workspace/package.json
  test "$(jq -r .routerSocket "$metadata")" = ${pkgs.lib.escapeShellArg hostPaths.routerSocket}
  touch "$out"
''
