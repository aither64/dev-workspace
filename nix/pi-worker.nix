{ lib, runCommand, findutils, nodejs, piPackage, src }:
let
  productionSource = lib.cleanSourceWith {
    src = src + "/pi-runtime/src";
    name = "dev-workspace-pi-worker-source";
    filter = path: type:
      type == "directory"
      || (lib.hasSuffix ".mjs" path && !(lib.hasSuffix ".test.mjs" path));
  };
in
assert lib.assertMsg (builtins.pathExists (src + "/pi-runtime/src/worker.mjs"))
  "Pi packaging requires the production worker entrypoint";
runCommand "dev-workspace-pi-worker" {
  passthru = { inherit nodejs piPackage; };
} ''
  mkdir -p "$out/libexec/pi-runtime" "$out/share/dev-workspace"
  cp -R ${productionSource}/. "$out/libexec/pi-runtime/"
  test -f "$out/libexec/pi-runtime/worker.mjs"
  ${findutils}/bin/find "$out/libexec/pi-runtime" -type f -name '*.mjs' -print0 |
  while IFS= read -r -d "" module; do
    ${nodejs}/bin/node --check "$module"
  done
  cat > "$out/share/dev-workspace/pi-worker.json" <<EOF
  {"schema":1,"worker_protocol":1,"node_executable":"${nodejs}/bin/node","worker_entrypoint":"$out/libexec/pi-runtime/worker.mjs","pi_package_root":"${piPackage}"}
  EOF
  test "$(wc -c < "$out/share/dev-workspace/pi-worker.json")" -le 4096
''
