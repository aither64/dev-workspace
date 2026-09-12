// Component acceptance against bounded fixture APIs. Run using Nix Node,
// playwright-driver and Chromium; point REVIEW_ASSETS_DIRECTORY at the built assets.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const assert = require("node:assert/strict");
const root = process.cwd();
const bundle = process.env.REVIEW_ASSETS_DIRECTORY || path.join(root, "portal/review-ui/dist");
const provider = process.env.CODEX_WEB_SOURCE;
const base = "a".repeat(40), originalHead = "b".repeat(40);
let head = originalHead, shortFiles = false;
const calls = [], assets = [];
const cards = '<div class="section-heading"><h2>Repositories</h2><span>4</span></div><div class="repo-grid">' +
  ["project", "second", "third", "fourth"].map(name => '<article class="panel repo-card" data-repository-id="' + name +
    '" data-repository-name="' + name + '" data-repository-head="' + head +
    '"><div data-repository-status><h3>' + name + '</h3></div><section class="repository-history"><div class="repository-review-actions">' +
    '<button data-review-branch disabled>Compare</button><button data-review-refresh>Refresh commits</button></div>' +
    '<p class="repository-head-change" hidden>Branch changed</p><div data-repository-commits></div></section></article>').join("") + "</div>";
const files = Array.from({length: 30}, (_, index) => ({id: String(index), path: "src/file-" + index + ".nix",
  status: index === 1 ? "A" : index === 2 ? "D" : "M", oldMode: index === 1 ? "000000" : "100644",
  newMode: index === 2 ? "000000" : "100644", additions: index === 2 ? 0 : 1, deletions: index === 1 ? 0 : 1}));
const source = Array.from({length: 100}, (_, i) => '  value' + i + ' = "before ' + i + '";').join("\n") + "\n";
const content = id => {
  const file = files[Number(id)];
  const original = shortFiles ? '  value = "before 35";\n' : source;
  const before = file.oldMode === "000000" ? "" : "{\n" + original + "}\n";
  const after = file.newMode === "000000" ? "" : "{\n" + original.replace("before 35", "after 35") + "}\n";
  return {before: {text: before, bytes: before.length, kind: file.oldMode === "000000" ? "absent" : "file"},
    after: {text: after, bytes: after.length, kind: file.newMode === "000000" ? "absent" : "file"}};
};
const commit = {id: "commit", sha: originalHead, subject: "Improve repository review", body: "Complete body\n\nSecond paragraph.",
  message: "Improve repository review\n\nComplete body\n\nSecond paragraph.\n", author: "Example Author",
  date: "2026-09-12T12:00:00Z", url: "https://github.com/example/project/commit/" + originalHead};
const history = repository => ({repository, review: "frozen", snapshot: "snapshot", pair: {base, head: originalHead, baseLabel: "Merge base"},
  history: {commits: [commit], page: 0, hasMore: false}});
const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://fixture");
  const send = (type, data) => {res.setHeader("Content-Type", type); res.end(data);};
  const json = value => send("application/json", JSON.stringify(value));
  if (url.pathname === "/example/") {
    res.setHeader("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-teststyle123'; connect-src 'self'");
    return send("text/html", '<!doctype html><link rel="stylesheet" href="/static/style.css"><link rel="stylesheet" href="/copy.css">' +
      '<link rel="stylesheet" href="/harness.css"><section id="repositories" class="tab-panel active">' + cards +
      '</section><script type="module" src="/entry.js"></script>');
  }
  if (url.pathname === "/entry.js") return send("text/javascript", "import {mount} from '/static/repository-review.js';" +
    "import {createCopyButton} from '/conversation.js';window.cardMarkup=" + JSON.stringify(cards) +
    ";window.review=mount({slug:'example',nonce:'teststyle123',createCopyButton,element:document.getElementById('repositories')});");
  if (url.pathname === "/harness.css") return send("text/css", "html,body{height:100%;margin:0}#repositories{height:100%;}");
  if (url.pathname === "/conversation.js") return send("text/javascript", fs.readFileSync(path.join(provider, "conversation/assets/conversation.js")));
  if (url.pathname === "/copy.css") return send("text/css", fs.readFileSync(path.join(provider, "conversation/assets/conversation.css")));
  if (url.pathname.startsWith("/static/")) {
    const name = path.basename(url.pathname);
    assets.push(name);
    const file = path.join(name.startsWith("review-") ? bundle : path.join(root, "portal/internal/web/static"), name);
    if (!fs.existsSync(file)) {res.statusCode = 404; return res.end(file);}
    return send(name.endsWith(".css") ? "text/css" : "text/javascript", fs.readFileSync(file));
  }
  calls.push({operation: url.pathname.split("/").at(-1), files: url.searchParams.getAll("file")});
  if (url.pathname.endsWith("repository-histories")) return json({repositories: url.searchParams.getAll("repository").map(history)});
  if (url.pathname.endsWith("repository-history")) return json(history(url.searchParams.get("repository")));
  if (url.pathname.endsWith("repository-states")) return json({repositories: url.searchParams.getAll("repository").map(repository => ({repository, head}))});
  if (url.pathname.endsWith("repository-comparison")) return json({review: "frozen", snapshot: "snapshot",
    name: "project", pair: {base, head: originalHead, baseLabel: "Merge base"}, historyHead: originalHead,
    commit: url.searchParams.get("commit") ? commit : null, files,
    stats: {files: 30, additions: 29, deletions: 29, binaryFiles: 0},
    preview: {file: url.searchParams.get("file") || "0", content: content(url.searchParams.get("file") || "0")}});
  if (url.pathname.endsWith("repository-files")) return json({files: url.searchParams.getAll("file").map(file => ({file, content: content(file)}))});
  res.statusCode = 404; res.end();
});
(async () => {
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  const origin = "http://127.0.0.1:" + server.address().port;
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_EXECUTABLE, headless: true, args: ["--no-sandbox"]});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 900}});
    const errors = [];
    page.on("pageerror", error => errors.push(error.message));
    await page.addInitScript(() => {
      window.violations = [];
      document.addEventListener("securitypolicyviolation", event => window.violations.push(event.violatedDirective));
      Object.defineProperty(navigator, "clipboard", {value: {writeText: async value => {window.copiedText = value;}}});
    });
    await page.goto(origin + "/example/");
    await page.locator(".repository-commit").first().waitFor();
    await page.waitForLoadState("networkidle");
    assert(!assets.includes("review-editor.js"), "overview eagerly loaded the editor");
    assert(!assets.includes("review-highlight-worker.js"), "overview eagerly loaded syntax grammars");
    assert.equal(calls.filter(call => call.operation === "repository-histories").length, 1);
    assert.equal(await page.locator(".repo-grid").evaluate(el => getComputedStyle(el).gridTemplateColumns.split(" ").length), 2);
    await page.getByRole("button", {name: "Show commit message", exact: true}).first().click();
    assert(await page.locator(".repository-commit-message").first().isVisible());
    await page.evaluate(() => {window.originalCommit = document.querySelector(".repository-commit"); window.review.updateHTML(window.cardMarkup);});
    assert(await page.evaluate(() => document.querySelector(".repository-commit") === window.originalCommit));
    await page.getByRole("button", {name: "Copy commit hash", exact: true}).first().click();
    assert.equal(await page.evaluate(() => window.copiedText), originalHead);
    const commitLink = await page.locator(".repository-commit-subject").first().getAttribute("href");
    assert(new URL(commitLink).searchParams.get("commit") === originalHead);
    await page.locator(".repository-commit-subject").first().click();
    await page.locator(".cm-mergeView").first().waitFor();
    await page.waitForFunction(() => {
      const colors = [...document.querySelectorAll('.repository-file-section[data-file-id="0"] [class*="review-token-"]')]
        .map(element => getComputedStyle(element).color);
      return new Set(colors).size >= 3;
    });
    assert.equal(await page.locator(".repository-commit-full-message").textContent(), commit.message);
    assert((await page.locator(".repository-comparison-stats").textContent()).includes("30 changed files"));
    assert.equal(calls.filter(call => call.operation === "repository-comparison").length, 1);
    assert(!calls.some(call => call.operation === "repository-files" && call.files.includes("0")), "inline preview was fetched twice");
    await page.locator('.repository-file-section[data-file-id="0"]').getByRole("link", {name: "View file", exact: true}).click();
    await page.locator('.repository-file-section[data-file-id="0"] .cm-editor').waitFor();
    assert.equal(new URL(page.url()).searchParams.get("view"), "file");
    await page.getByRole("button", {name: "Before", exact: true}).first().click();
    assert.equal(new URL(page.url()).searchParams.get("version"), "old");
    await page.locator('.repository-file[data-file-id="1"]').click();
    assert.equal(new URL(page.url()).searchParams.get("version"), "new", "added file URL must name the displayed version");
    await page.locator('.repository-file[data-file-id="2"]').click();
    assert.equal(new URL(page.url()).searchParams.get("version"), "old", "deleted file URL must name the displayed version");
    await page.locator('.repository-file[data-file-id="0"]').click();
    const permalink = new URL(page.url()); permalink.hash = "old-L90";
    await page.goto(permalink.href);
    await page.locator('.repository-file-section[data-file-id="0"] .cm-editor').waitFor();
    await page.waitForFunction(() => document.querySelector('.repository-file-section[data-file-id="0"] .cm-content')?.textContent.includes("before 88"));
    assert.equal(new URL(page.url()).hash, "#old-L90");
    await page.locator('.repository-file-section[data-file-id="0"]').getByRole("link", {name: "View diff", exact: true}).click();
    await page.getByRole("button", {name: "Unified", exact: true}).click();
    await page.waitForFunction(() => !document.querySelector(".cm-mergeView") && document.querySelector(".cm-editor"));
    const lineLink = new URL(page.url()); lineLink.hash = "old-L90";
    await page.goto(lineLink.href);
    await page.waitForFunction(() => document.querySelector('.repository-file-section[data-file-id="0"] .cm-content')?.textContent.includes("before 88"));
    const anchors = await page.locator('.repository-file-section[data-file-id="0"] .cm-gutters a').evaluateAll(nodes => nodes.map(node => node.href));
    assert(anchors.some(href => href.endsWith("#old-L90")), "old-side line anchor absent");
    assert(anchors.some(href => /#new-L[0-9]+$/.test(href)), "new-side line anchors absent");
    await page.locator('.repository-file[data-file-id="2"]').click();
    await page.locator('.repository-file-section[data-file-id="2"]').getByRole("link", {name: "View file", exact: true}).click();
    assert.equal(new URL(page.url()).searchParams.get("version"), "old");
    assert(await page.locator('.repository-file-section[data-file-id="2"]').getByRole("button", {name: "After", exact: true}).isDisabled());
    await page.goBack();
    await page.waitForFunction(() => new URL(location.href).searchParams.get("view") === "diff");
    await page.locator('.repository-file[data-file-id="29"]').click();
    await page.waitForFunction(() => document.querySelector('.repository-file-section[data-file-id="29"] .cm-editor'));
    head = "c".repeat(40);
    await page.evaluate(() => document.dispatchEvent(new CustomEvent("session-section-change", {detail: "repositories"})));
    await page.locator(".repository-comparison-changed:not([hidden])").waitFor();
    assert((await page.locator(".repository-pair").textContent()).includes("bbbbbbbbbb"));
    assert.equal(await page.locator('[contenteditable="true"]').count(), 0);
    await page.setViewportSize({width: 390, height: 844});
    await page.getByRole("button", {name: "← Repositories", exact: true}).click();
    assert.equal(await page.locator(".repo-grid").evaluate(el => getComputedStyle(el).gridTemplateColumns.split(" ").length), 1);
    await page.locator("[data-review-branch]").first().click();
    await page.locator(".cm-editor").first().waitFor();
    assert.deepEqual(await page.evaluate(() => window.violations), []);
    assert.deepEqual(errors, []);
    shortFiles = true;
    await page.setViewportSize({width: 1600, height: 5000});
    await page.reload();
    await page.waitForFunction(() => document.querySelectorAll(".review-code-view").length >= 6);
    await page.waitForFunction(() => [...document.querySelectorAll(".review-syntax-status")].every(el => el.textContent !== "Highlighting…"));
    const retained = await page.evaluate(() => document.querySelectorAll(".review-code-view").length);
    assert(retained <= 8, "many short diffs exceeded the eight-file mount bound: " + retained);
    assert.equal(await page.locator(".review-syntax-status").count(), 0, "bounded editors lost syntax highlighting");
    assert(await page.locator('.repository-file-section[data-file-id="0"] .review-code-view').count(), "selected file was evicted");
    console.log(JSON.stringify({result: "passed", calls: calls.length, checks: ["batched histories", "message and copy controls",
      "inline first file", "full file versions", "cold immutable links", "both line anchor sides", "unified collapsed target",
      "browser history", "file statuses and counts", "branch movement", "responsive layout", "readonly", "strict CSP", "eight-file mount bound", "lazy editor and syntax assets"]}));
  } finally {await browser.close(); await new Promise(resolve => server.close(resolve));}
})().catch(error => {console.error(error); process.exitCode = 1; server.close();});
