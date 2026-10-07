const assert = require("node:assert/strict");
const fs = require("node:fs");

(async () => {
  const source = fs.readFileSync("static/source-file.js", "utf8");
  const {sourceFileURL, sourceFileLine, sourceFileVersion} = await import("data:text/javascript;base64," + Buffer.from(source).toString("base64"));
  const original = "https://workspace.example.test/files/example?repository=abc&path=a%20%23%25.rb";
  for (const line of [1, 10, 12000, Number.MAX_SAFE_INTEGER]) {
    const href = sourceFileURL(original, line);
    assert.equal(sourceFileLine(href), line);
    assert.equal(new URL(href).search, new URL(original).search);
  }
  for (const hash of ["", "#L0", "#L-1", "#L01", "#L1x", "#old-L1", "#L9007199254740992"]) {
    assert.equal(sourceFileLine(original + hash), null, hash);
  }
  assert.equal(sourceFileURL(original + "#L10", null), original);
  assert.match(sourceFileVersion({source: "archive", revision: "exact-final-head"}), /exact-final-head/);
  assert.match(sourceFileVersion({source: "worktree"}), /uncommitted/);
  assert.match(sourceFileVersion({source: "archived-artifact"}), /Archived/);
  assert.equal(sourceFileVersion({source: "artifact"}), "Current session artifact");
  assert.match(sourceFileVersion({source: "workspace"}), /Current workspace.*uncommitted/);
  assert.throws(() => sourceFileVersion({source: "unknown"}), /source is unknown/);

  // Keep the mounted UI and event handlers intact; replace only the browser's
  // absolute editor import with a controllable asynchronous editor boundary.
  const editorImport = 'await import("/static/review-editor.js")';
  assert(source.includes(editorImport));
  const mountedSource = source.replace(editorImport, '({createReviewEditor: () => globalThis.sourceTestEditor})');
  const {mountSourceFile} = await import("data:text/javascript;base64," + Buffer.from(mountedSource).toString("base64"));
  const elements = Object.fromEntries(["notice", "content", "title", "version", "copy"].map(name =>
    [`source-file-${name}`, {textContent: "", hidden: false, addEventListener() {}}]));
  const events = {};
  globalThis.document = {getElementById: id => elements[id], querySelector: () => ({content: "nonce"})};
  globalThis.location = new URL(original + "#L10");
  globalThis.addEventListener = (name, callback) => { events[name] = callback; };
  let fetchedURL;
  globalThis.fetch = async url => {
    fetchedURL = url;
    return {ok: true, json: async () => ({path: "example.nix", source: "worktree", content: {text: "line\n"}})};
  };
  let finishOldReveal;
  globalThis.sourceTestEditor = {clearLine() {}, revealLine: (_side, line) => line === 10
    ? new Promise(resolve => { finishOldReveal = resolve; }) : Promise.resolve(false)};
  const mounting = mountSourceFile({dataset: {slug: "example"}});
  for (let tries = 0; !finishOldReveal && tries < 50; tries++) await new Promise(setImmediate);
  assert.equal(typeof finishOldReveal, "function");
  assert.equal(fetchedURL, `/api/sessions/example/file${location.search}`);
  globalThis.location = new URL(original + "#L999");
  await events.hashchange();
  assert.match(elements["source-file-notice"].textContent, /Line 999 is not present/);
  finishOldReveal(true);
  await mounting;
  assert.match(elements["source-file-notice"].textContent, /Line 999 is not present/);
  assert.equal(elements["source-file-notice"].hidden, false);

  // During a rollback the fetched editor may be the previously deployed bundle.
  globalThis.sourceTestEditor = {revealLine: async () => true};
  globalThis.location = new URL(original + "#L1");
  await mountSourceFile({dataset: {slug: "example"}});
  assert.equal(elements["source-file-notice"].textContent, "");
  globalThis.location = new URL("https://workspace.example.test/workspace-files?path=docs%2Fguide.md#L93");
  let revealedLine;
  globalThis.sourceTestEditor = {clearLine() {}, revealLine: async (_side, line) => { revealedLine = line; return true; }};
  globalThis.fetch = async url => {
    fetchedURL = url;
    return {ok: true, json: async () => ({path: "docs/guide.md", source: "workspace", content: {text: "line\n"}})};
  };
  await mountSourceFile({dataset: {fileApi: "/api/workspace/file"}});
  assert.equal(fetchedURL, "/api/workspace/file?path=docs%2Fguide.md");
  assert.equal(revealedLine, 93);
  assert.equal(elements["source-file-title"].textContent, "docs/guide.md");
  assert.match(elements["source-file-version"].textContent, /Current workspace/);
  console.log("Source file URL and source identity contracts passed.");
})().catch(error => { console.error(error); process.exitCode = 1; });
