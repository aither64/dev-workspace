{ buildNpmPackage, lib }:
let src = ../portal/review-ui; in
buildNpmPackage {
  pname = "workspace-repository-review-assets";
  version = "1.0.0";
  src = lib.fileset.toSource {
    root = src;
    fileset = lib.fileset.unions [
      (src + "/package.json")
      (src + "/package-lock.json")
      (src + "/editor.js")
      (src + "/build.mjs")
    ];
  };
  npmDepsHash = "sha256-DWVz5vMA4KfiqcpfCrhO2aFKweM34RNxLjP6e82fcoA=";
  npmRebuildFlags = [ "--ignore-scripts" ];
  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp dist/review-editor.js dist/review-editor.LICENSE.txt \
      dist/review-editor.dependencies.json "$out/"
    runHook postInstall
  '';
}
