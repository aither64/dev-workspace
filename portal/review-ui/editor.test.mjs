import test from "node:test";
import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import {normalizeSource, sourceLines, languageForPath, unifiedProjection, contextRegions, projectTokens} from "./editor-model.js";
import {initializeHighlighter, tokenizeSource} from "./highlight.js";

function validateProjection(before, after) {
  const projected = unifiedProjection(before, after);
  for (const [side, source] of [["old", before], ["new", after]]) {
    const key = side === "old" ? "oldLine" : "newLine";
    const rows = projected.rows.filter(row => row[key] != null);
    assert.deepEqual(rows.map(row => row.text), sourceLines(source).map(line => line.text));
    assert.deepEqual(rows.map(row => row[key]), rows.map((_, index) => index + 1));
    for (const row of rows) assert.equal(projected.positions[side].get(row[key]), row.from);
  }
  for (const row of projected.rows) {
    assert.equal(projected.text.slice(row.from, row.from + row.text.length), row.text);
    for (const [from, to] of row.changes) assert.ok(from >= row.from && to <= row.from + row.text.length && to > from);
  }
  return projected;
}

test("unified projections preserve complete old/new line identities and unusual contents", () => {
  for (const [before, after] of [
    ["", ""], ["", "new\n"], ["deleted\n", ""], ["same\n", "same\n"],
    ["same", "same\n"], ["same\n", "same"], ["\n", "\n\n"],
    ["a\nb\nc", "before\na\nc\nafter\n"], ["😀 = '<tag>';\n\told\n", "😀 = '&';\n\tnew\n"],
    ["dup\ndup\nold\ndup\n", "dup\nnew\ndup\ndup\n"],
  ]) validateProjection(before, after);
  // Exercise insertions/removals/repetition without mirroring the diff algorithm.
  let seed = 81;
  const random = size => ((seed = (seed * 1664525 + 1013904223) >>> 0) % size);
  for (let example = 0; example < 80; example++) {
    const oldLines = Array.from({length: random(40)}, () => `line ${random(12)}`);
    const newLines = [...oldLines];
    for (let edit = 0; edit < 5; edit++) newLines.splice(random(newLines.length + 1), random(3), `changed ${random(8)}`);
    validateProjection(oldLines.join("\n"), newLines.join("\n"));
  }
});

test("collapse regions keep change context and locate links within hidden lines", () => {
  const before = Array.from({length: 100}, (_, i) => `line ${i + 1}`).join("\n");
  const after = before.replace("line 50", "changed 50");
  const projected = validateProjection(before, after);
  const regions = contextRegions(projected.rows);
  assert.equal(regions.length, 2);
  const hidden = projected.positions.old.get(12);
  assert.equal(regions.filter(region => region.from <= hidden && hidden < region.to).length, 1);
  for (const row of projected.rows.filter(row => row.kind !== "context")) {
    assert.ok(regions.every(region => row.from < region.from || row.from >= region.to));
  }
  assert.deepEqual(contextRegions(validateProjection("a\nb\nc", "a\nnew\nc").rows), []);
});

test("language selection recognizes workspace files and every bundled grammar", async () => {
  const fixtures = [["flake.nix", "nix"], ["Gemfile.lock", "ruby"], ["Rakefile", "ruby"],
    ["server.go", "go"], ["page.tsx", "tsx"], ["test.sh", "shellscript"], ["Dockerfile.dev", "dockerfile"],
    ["config.yml", "yaml"], ["main.tf", "hcl"], ["data.json", "json"], ["Makefile", "makefile"],
    ["page.jsx", "jsx"], ["app.js", "javascript"], ["types.ts", "typescript"], ["config.jsonc", "jsonc"],
    ["main.py", "python"], ["page.php", "php"], ["page.html", "html"], ["data.xml", "xml"],
    ["style.css", "css"], ["style.scss", "scss"], ["README.md", "markdown"], ["config.toml", "toml"],
    ["config.ini", "ini"], ["query.sql", "sql"], ["main.rs", "rust"], ["main.c", "c"], ["main.cpp", "cpp"],
    ["changes.patch", "diff"]];
  const loaded = (await initializeHighlighter()).getLoadedLanguages();
  for (const [path, language] of fixtures) {
    assert.equal(languageForPath(path), language);
    assert(loaded.includes(language), `${path} resolves to an unbundled grammar`);
  }
  assert.equal(languageForPath("run", "#!/usr/bin/env ruby\nputs 1"), "ruby");
  assert.equal(languageForPath("notes.unknown"), null);
  assert.notEqual(languageForPath("before.rb"), languageForPath("after.ts"));
  assert.equal(normalizeSource("a\r\nb\rc\n"), "a\nb\nc\n");
});

test("selected maintained grammars highlight complete source with stable palette indices", async () => {
  const fixtures = {
    nix: `{ pkgs, ... }: {\n message = ''\n# inside string\n\nmore\n'';\n package = pkgs.git;\n}`,
    ruby: `class Example\n  text = <<~EOF\n# inside string\n\nmore\nEOF\nend`,
    go: `package main\nfunc main() { println("hello") }`,
    javascript: `const data = {value: "😀"};`, typescript: `const message: string = \`foo\n\nbar\`;`,
    shellscript: `#!/bin/sh\ncat <<'EOF'\n# text\n\nworld\nEOF`,
    yaml: `services:\n  enabled: true\n`, hcl: `resource "x" "name" { value = true }`,
    json: `{"number": 12, "yes": true}`, php: `<?php echo "hello";`,
  };
  let palette;
  for (const [language, text] of Object.entries(fixtures)) {
    const result = await tokenizeSource(text, language);
    assert.ok(result.tokens.length > 0, language);
    assert.ok(new Set(result.tokens.map(token => token[2])).size >= 2, language);
    palette ??= result.styles;
    assert.deepEqual(result.styles, palette, language);
    for (const [from, to, style] of result.tokens) {
      assert.ok(Number.isInteger(from) && Number.isInteger(to) && from >= 0 && to > from && to <= text.length);
      assert.match(result.styles[style].color, /^#[0-9a-f]{6}(?:[0-9a-f]{2})?$/);
    }
  }
  assert.deepEqual(await tokenizeSource("<script>alert(1)</script>", "unknown"), {tokens: [], styles: []});
  await assert.rejects(tokenizeSource("x".repeat(512 * 1024 + 1), "javascript"), /limit/);
  for (const text of ["x\n".repeat(12000), "x\n".repeat(11999) + "x"]) await tokenizeSource(text, "unknown");
  for (const text of ["x\n".repeat(12001), "x\n".repeat(12000) + "x"]) {
    await assert.rejects(tokenizeSource(text, "unknown"), /limit/);
  }
});

test("unified highlighting uses full original grammar context and each renamed language", async () => {
  const before = `value = <<~EOF\n# old string, not a comment\n\nEOF\n`;
  const after = `const value: string = \`new text\`;\n`;
  const oldResult = await tokenizeSource(before, "ruby"), newResult = await tokenizeSource(after, "typescript");
  const projected = validateProjection(before, after);
  const tokens = projectTokens(projected, oldResult.tokens, newResult.tokens);
  for (const row of projected.rows) {
    const sourceFrom = row.newLine == null ? row.oldFrom : row.newFrom;
    const sourceTokens = row.newLine == null ? oldResult.tokens : newResult.tokens;
    const sourceStyles = Array(row.text.length), projectedStyles = Array(row.text.length);
    for (const [from, to, style] of sourceTokens) for (let i = Math.max(sourceFrom, from); i < Math.min(sourceFrom + row.text.length, to); i++) sourceStyles[i - sourceFrom] = style;
    for (const [from, to, style] of tokens) for (let i = Math.max(row.from, from); i < Math.min(row.from + row.text.length, to); i++) projectedStyles[i - row.from] = style;
    assert.deepEqual(projectedStyles, sourceStyles);
  }
  const stringOffset = before.indexOf("# old");
  const contextual = oldResult.tokens.find(([from, to]) => from <= stringOffset && to > stringOffset);
  const isolated = await tokenizeSource("# old string, not a comment", "ruby");
  assert.notEqual(contextual[2], isolated.tokens[0][2], "deleted heredoc stays a string rather than becoming a comment");
});

test("editor and worker bundles are self-contained and exclude the WASM engine", async () => {
  const metadata = JSON.parse(await readFile("dist/review-build.json", "utf8"));
  for (const output of Object.values(metadata.outputs)) assert.deepEqual(output.imports, []);
  assert.ok(!Object.keys(metadata.inputs).some(path => /engine-oniguruma|\.wasm$/.test(path)));
  assert.ok(Object.keys(metadata.outputs).includes("dist/review-highlight-worker.js"));
});

test("worker client coalesces immutable sources and cancels only abandoned work", async () => {
  const instances = [];
  const original = globalThis.Worker;
  class WorkerFixture {
    messages = [];
    constructor() { instances.push(this); }
    postMessage(message) { this.messages.push(message); }
    terminate() { this.terminated = true; }
    answer(result = {tokens: [], styles: []}) { this.onmessage({data: {id: this.messages.at(-1).id, result}}); }
  }
  globalThis.Worker = WorkerFixture;
  try {
    const {highlightSource} = await import("./highlight-client.js?worker-contract");
    const abort = new AbortController();
    const first = highlightSource("same", "ruby", abort.signal).catch(error => error.message);
    const second = highlightSource("same", "ruby");
    assert.equal(instances.length, 1);
    assert.equal(instances[0].messages.length, 1);
    abort.abort();
    assert.match(await first, /unavailable/);
    assert.ok(!instances[0].terminated, "another editor still needs the shared job");
    instances[0].answer();
    await second;
    const stale = new AbortController();
    const abandoned = highlightSource("stale", "ruby", stale.signal).catch(error => error.message);
    const next = highlightSource("next", "ruby");
    stale.abort();
    assert.match(await abandoned, /unavailable/);
    assert.ok(instances[0].terminated);
    assert.equal(instances.length, 2);
    // A late failure from a retired worker cannot kill its replacement.
    instances[0].onerror({preventDefault() {}});
    assert.ok(!instances[1].terminated);
    instances[1].answer();
    await next;
    const requests = Array.from({length: 20}, (_, index) => highlightSource(`source ${index}`, "ruby").then(() => "ok", () => "full"));
    for (let i = 0; i < 17; i++) instances[1].answer();
    const results = await Promise.all(requests);
    assert.equal(results.filter(result => result === "ok").length, 17);
    assert.equal(results.filter(result => result === "full").length, 3);
  } finally { globalThis.Worker = original; }
});
