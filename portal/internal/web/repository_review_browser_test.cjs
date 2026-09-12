const assert = require("node:assert/strict");
const fs = require("node:fs");

(async () => {
  const source = fs.readFileSync("static/repository-review.js", "utf8");
  const {reviewRoute, reviewURL, fullFileVersion, fileStatus, changeCounts} =
    await import("data:text/javascript;base64," + Buffer.from(source).toString("base64"));
  const original = "https://workspace.example.test/example/?unrelated=kept#codex";
  const route = {
    repository: "repository-id", review: "frozen-review", commit: "a".repeat(40),
    file: "file-id", view: "file", version: "old", layout: "unified",
    line: {side: "old", number: 12000},
  };
  const href = reviewURL(original, route);
  const parsed = new URL(href);
  assert.equal(parsed.searchParams.get("unrelated"), "kept");
  assert.equal(parsed.searchParams.get("tab"), "repositories");
  assert.equal(parsed.hash, "#old-L12000");
  assert.deepEqual(reviewRoute(href), route);
  assert.equal(reviewRoute(original, "unified").layout, "unified");
  assert.equal(reviewRoute(original + "-L1").line, null);
  for (const bad of ["#old-L0", "#old-L-1", "#old-L1x", "#new-L9007199254740992"]) {
    assert.equal(reviewRoute("https://workspace.example.test/" + bad).line, null);
  }
  const overview = reviewURL(href);
  for (const key of ["repository", "review", "commit", "file", "view", "layout", "version"]) {
    assert.equal(new URL(overview).searchParams.has(key), false, key);
  }
  assert.equal(new URL(overview).hash, "");
  assert.equal(fullFileVersion({oldMode: "100644", newMode: "000000"}, "new"), "old");
  assert.equal(fullFileVersion({oldMode: "000000", newMode: "100644"}, "old"), "new");
  assert.equal(fullFileVersion({oldMode: "100644", newMode: "100644"}, ""), "new");
  assert.equal(fullFileVersion({oldMode: "100644", newMode: "100644"}, "old"), "old");
  assert.deepEqual(fileStatus("R100"), ["renamed", "Renamed"]);
  assert.deepEqual(fileStatus("T"), ["type", "Type changed"]);
  assert.equal(changeCounts({additions: null, deletions: null}), "Binary");
  assert.equal(changeCounts({files: 3, additions: 7, deletions: 2, binaryFiles: 1}, true),
    "3 changed files · +7 · −2 · 1 binary file");
  console.log("Repository URL, file-version and metadata contracts passed.");
})().catch(error => { console.error(error); process.exitCode = 1; });
