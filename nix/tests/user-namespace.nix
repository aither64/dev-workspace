{
  pkgs,
  mkPackage,
}:
let
  package = mkPackage {
    inherit pkgs;
    userNamespace = "previous-workspaces";
    routerSocket = "/run/previous-workspaces/router.sock";
  };
in
pkgs.runCommand "dev-workspace-user-namespace" { nativeBuildInputs = [ pkgs.jq ]; } ''
  units=${package}/share/systemd/user
  metadata=${package}/share/dev-workspace/package.json
  grep -RF 'ExecStart=%h/.local/state/previous-workspaces/profile/bin/workspace-host' "$units"
  if grep -RF '%h/.local/state/dev-workspaces/profile' "$units"; then
    echo 'compatibility package retained the default profile path' >&2
    exit 1
  fi
  grep -F 'DEV_WORKSPACES_NAMESPACE' ${package}/bin/workspace-host
  grep -F 'previous-workspaces' ${package}/bin/workspace-host
  grep -F 'DEV_WORKSPACES_ROUTER_SOCKET' ${package}/bin/workspace-host
  grep -F '/run/previous-workspaces/router.sock' ${package}/bin/workspace-host
  test "$(jq -r .userNamespace "$metadata")" = previous-workspaces
  test "$(jq -r .routerSocket "$metadata")" = /run/previous-workspaces/router.sock
  touch "$out"
''
