// Real installed portal/shipped assets. No synthetic API replies or model turns.
// Execute only in a supervised private service fixture after independent review.
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {randomUUID} = require("node:crypto");
const {chromium} = require(process.env.PLAYWRIGHT_MODULE);

const [root, mode] = process.argv.slice(2);
assert(root && /^\/tmp\/archive-acceptance-[a-z0-9]{1,12}$/.test(root));
assert.equal(fs.realpathSync(root), root);
assert(["stable", "replacement"].includes(mode));
const spec = JSON.parse(fs.readFileSync(path.join(root, "fixture.json"), "utf8"));
assert.equal(spec.root, root);
assert.equal(spec.workspace, path.join(root, "workspace"));
assert.equal(spec.uid, process.getuid());
assert.equal(spec.purpose, "disposable-archive-acceptance");
assert.equal(spec.status, "prepared");
const {origin} = JSON.parse(fs.readFileSync(path.join(root, "proxy.json"), "utf8"));
assert(/^https:\/\/127\.0\.0\.1:[0-9]+$/.test(origin));
const api = `/api/sessions/${spec.slug}`;
const tracking = path.join(spec.workspace, "work", spec.slug);
const logs = path.join(root, "logs");
const sleep = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));

(async () => {
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_EXECUTABLE,
    headless: true, args: ["--no-sandbox"]});
  const context = await browser.newContext({ignoreHTTPSErrors: true});
  const errors = [], forbidden = [], mutations = [];
  try {
    await context.route("**/*", async route => {
      const request = route.request(), url = new URL(request.url());
      if (url.origin !== origin || (request.method() !== "GET" && request.method() !== "HEAD" &&
          !(request.method() === "POST" && url.pathname === api + "/archive"))) {
        forbidden.push({method: request.method(), url: request.url()});
        return route.abort();
      }
      if (request.method() === "POST") mutations.push(JSON.parse(request.postData()));
      return route.continue();
    });
    const page = await context.newPage();
    page.on("pageerror", error => errors.push(error.message));
    await page.goto(origin + "/" + spec.slug + "/");
    assert.equal(await page.locator('script[src="/static/app.js?v=23"]').count(), 1);
    const pageIdentity = randomUUID();
    await page.evaluate(id => { window.acceptanceDocumentIdentity = id; }, pageIdentity);
    const operation = async () => {
      const response = await context.request.get(origin + api + "/operation");
      assert.equal(response.status(), 200);
      return response.json();
    };
    const before = await operation();
    assert.equal(before.currentTarget.targetIdentityVersion, 2);
    assert.equal(before.currentTarget.archived, false);
    assert.equal(before.currentTarget.lifecycle, "complete");
    // The actual page refreshes its confirmation without reloading its bundle.
    if (mode === "stable") {
      fs.writeFileSync(path.join(tracking, "open-page-artifact.txt"), "Synthetic artifact added while page stays open.\n", {flag: "wx"});
      const manifest = path.join(tracking, "portal.yml"), temporary = manifest + ".acceptance.tmp";
      fs.writeFileSync(temporary, fs.readFileSync(manifest) + "\n# Semantically equivalent atomic rewrite.\n", {flag: "wx"});
      fs.renameSync(temporary, manifest);
      const after = await operation();
      assert.equal(after.currentTarget.targetId, before.currentTarget.targetId);
      assert.equal(after.currentTarget.targetIdentityVersion, 2);
    }
    await page.locator("#archive-session-open").click();
    await page.waitForFunction(() => document.getElementById("archive-session-dialog").open);
    if (mode === "replacement") {
      // An old confirmation cannot select this different tracking directory.
      // Original remains outside work/archive for inspection; no root adoption.
      const retained = path.join(root, "browser", "original-tracking");
      fs.renameSync(tracking, retained);
      fs.cpSync(retained, tracking, {recursive: true, errorOnExist: true, force: false});
    }
    assert.equal(await page.evaluate(() => window.acceptanceDocumentIdentity), pageIdentity);
    const archiveResponse = page.waitForResponse(response => new URL(response.url()).pathname === api + "/archive" && response.request().method() === "POST");
    await page.locator('#archive-session-form button[value="complete"]').click();
    const response = await archiveResponse, accepted = await response.json();
    assert.equal(mutations.length, 1);
    assert.equal(mutations[0].targetId, before.currentTarget.targetId);
    if (mode === "replacement") {
      assert.equal(response.status(), 409);
      assert.equal(accepted.code, "target_changed");
      assert.notEqual(accepted.currentTarget.targetId, before.currentTarget.targetId);
      assert(!fs.existsSync(path.join(spec.workspace, "archive", spec.slug)));
      assert(!fs.existsSync(path.join(spec.workspace, "worktrees", ".locks", spec.slug + ".archive.json")));
      assert.equal(await page.evaluate(() => window.acceptanceDocumentIdentity), pageIdentity);
    } else {
      assert(response.ok(), JSON.stringify(accepted));
      assert.equal(accepted.kind, "archive");
      assert(accepted.receiptId);
      const deadline = Date.now() + 240_000;
      let completed;
      while (Date.now() < deadline) {
        completed = await operation();
        assert.equal(completed.receiptId, accepted.receiptId);
        if (completed.state === "complete") break;
        assert(!["failed", "paused"].includes(completed.state), JSON.stringify(completed));
        await sleep(250);
      }
      assert.equal(completed.state, "complete");
      assert.equal(completed.currentTarget.archived, true);
      assert(fs.existsSync(path.join(spec.workspace, "archive", spec.slug, "open-page-artifact.txt")));
    }
    assert.deepEqual(errors, []);
    assert.deepEqual(forbidden, []);
    fs.writeFileSync(path.join(logs, "browser-" + mode + ".json"), JSON.stringify({mode, before,
      accepted, mutations, errors, forbidden, evidence: "real portal response and shipped new bundle; native order not observed"}, null, 2) + "\n", {mode: 0o600, flag: "wx"});
  } finally {
    await context.close();
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
