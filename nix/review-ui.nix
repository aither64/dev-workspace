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
      (src + "/editor-model.js")
      (src + "/highlight.js")
      (src + "/highlight-client.js")
      (src + "/highlight-worker.js")
      (src + "/editor.test.mjs")
      (src + "/syntax.NOTICE")
      (src + "/build.mjs")
    ];
  };
  npmDepsHash = "sha256-1FxpNmLkv0o9MXQG/qbH7f59sYJkHHmsfW1gytAd0VI=";
  npmRebuildFlags = [ "--ignore-scripts" ];
  doCheck = true;
  checkPhase = ''
    runHook preCheck
    node --test editor.test.mjs
    runHook postCheck
  '';
  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp dist/review-*.js dist/review-*.txt dist/review-editor.dependencies.json "$out/"
    runHook postInstall
  '';
}
