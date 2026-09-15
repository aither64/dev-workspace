import test from "node:test";
import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {mkdtempSync, writeFileSync, rmSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {readFile} from "node:fs/promises";
import {normalizeSource, sourceLines, languageForPath, unifiedProjection, splitProjection, contextRegions, projectTokens} from "./editor-model.js";
import {initializeHighlighter, tokenizeSource} from "./highlight.js";

function gitDiff(before, after) {
  const dir = mkdtempSync(join(tmpdir(), "review-git-"));
  try {
    writeFileSync(join(dir, "old"), before); writeFileSync(join(dir, "new"), after);
    let patch;
    try { patch = execFileSync("git", ["diff", "--no-index", "--no-ext-diff", "--no-textconv", "--diff-algorithm=myers", "--indent-heuristic", "--unified=0", "old", "new"], {cwd: dir, encoding: "utf8"}); }
    catch (error) { if (error.status !== 1) throw error; patch = error.stdout; }
    const changes = [...patch.matchAll(/^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/gm)].map(match => {
      const oldLines = Number(match[2] ?? 1), newLines = Number(match[4] ?? 1);
      return {oldStart: Number(match[1]) - (oldLines ? 1 : 0), oldLines, newStart: Number(match[3]) - (newLines ? 1 : 0), newLines};
    });
    return {changes};
  } finally { rmSync(dir, {recursive: true}); }
}
function validateProjection(before, after, diff = gitDiff(before, after)) {
  before = normalizeSource(before); after = normalizeSource(after);
  const projected = unifiedProjection(before, after, diff);
  const split = splitProjection(before, after, diff);
  assert.equal(split.old.rows.length, split.new.rows.length);
  for (const [side, kind, count] of [["old", "deletion", "oldLines"], ["new", "addition", "newLines"]]) {
    assert.equal(projected.rows.filter(row => row.kind === kind).length, diff.changes.reduce((sum, change) => sum + change[count], 0));
    assert.equal(split[side].rows.filter(row => row.kind === kind).length, projected.rows.filter(row => row.kind === kind).length);
    assert.deepEqual(split[side].rows.filter(row => row.kind !== "empty").map(row => row.text), sourceLines(side === "old" ? before : after).map(line => line.text));
  }
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
    ["a\r\nb\rc\n", "a\r\nnew\rc\n"],
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

for (const layout of ["unified", "split"]) {
  function changedRows(before, after) {
    const ranges = gitDiff(before, after);
    validateProjection(before, after, ranges);
    const model = layout === "unified" ? unifiedProjection(before, after, ranges) : splitProjection(before, after, ranges);
    const rows = layout === "unified" ? model.rows : [...model.old.rows, ...model.new.rows];
    return rows.filter(row => ["deletion", "addition"].includes(row.kind));
  }

  test(`${layout} character highlights keep replaced identifiers together`, () => {
    const rows = changedRows("keep\n    old_name = 123\nend\n", "keep\n    new_value = true\nend\n");
    assert.deepEqual(rows.map(row => row.changes.map(([from, to]) => row.text.slice(from - row.from, to - row.from))),
      [["old_name", "123"], ["new_value", "true"]]);
  });

  test(`${layout} character highlights do not scatter through an unrelated replacement block`, () => {
    const before = "keep\n      return [] if addr_str.nil? || time.nil?\nend\n";
    const after = "keep\n      body_ips = entries.map { |entry| entry[:ip] }.compact.uniq\n" +
      "      conflict = subject_ip && body_ips.any? && !body_ips.include?(subject_ip)\n" +
      "      log(\"subject IP #{subject_ip} contradicts body entries\") if conflict\nend\n";
    const rows = changedRows(before, after);
    assert.equal(rows.length, 4);
    for (const row of rows) {
      assert.equal(row.changes.length, 1);
      const [[from, to]] = row.changes;
      assert.equal(row.text.slice(from - row.from, to - row.from).trim(), row.text.trim());
    }
  });
}

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
  assert.equal(normalizeSource("a\r\nb\rc\n"), "a\nb\rc\n");
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


test("reported OAuth2 comparison has exactly 116 added and six removed lines in both layouts", async () => {
  const fixture = JSON.parse(await readFile(new URL("fixtures/oauth2.json", import.meta.url), "utf8"));
  assert.deepEqual(gitDiff(fixture.before, fixture.after), fixture.diff);
  const projected = validateProjection(fixture.before, fixture.after, fixture.diff);
  assert.equal(projected.rows.filter(row => row.kind === "addition").length, 116);
  assert.equal(projected.rows.filter(row => row.kind === "deletion").length, 6);
});

test("missing or inconsistent Git ranges fail without falling back to approximate changed lines", () => {
  assert.throws(() => unifiedProjection("a", "b"), /Exact Git diff/);
  assert.throws(() => unifiedProjection("a", "b", {changes: []}), /context/);
  for (const changes of [
    [{oldStart: -1, oldLines: 1, newStart: 0, newLines: 1}],
    [{oldStart: 0, oldLines: 2, newStart: 0, newLines: 1}],
    [{oldStart: 0, oldLines: 0, newStart: 0, newLines: 0}],
  ]) assert.throws(() => splitProjection("a", "b", {changes}), /ranges/);
});
