"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];

const pair = review => ({base: "a".repeat(40), head: (review === "review-a" ? "b" : review === "review-slow" ? "c" : "d").repeat(40), baseLabel: "master"});
const file = index => ({id: `file-${index}`, path: `file-${String(index).padStart(2, "0")}.txt`,
  status: "M", oldMode: "100644", newMode: "100644", additions: 2001, deletions: 0});
const content = (index, snapshot) => ({before: {kind: "file", text: `before ${index}\n`, bytes: 9},
  after: {kind: "file", text: `${snapshot} file ${index}\n`, bytes: 24},
  diff: {changes: [{oldStart: 0, oldLines: 1, newStart: 0, newLines: 1}]}});

(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  let releaseFirstBatches = () => {}, releaseSlow = () => {};
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true});
    const errors = [], batches = [];
    let inFlight = 0, maxInFlight = 0, firstBatchesHeld = true, failedOnce = false;
    page.on("pageerror", error => errors.push(error.message));
    await page.route("**/static/review-editor.js", route => route.fulfill({contentType: "text/javascript", body: `
      export function createReviewEditor({parent, after}) {
        const editor = document.createElement("div");
        editor.className = "fixture-editor";
        editor.textContent = after;
        parent.append(editor);
        return {ready: Promise.resolve(), destroy() { editor.remove(); }, revealLine: async () => true};
      }
    `}));
    await page.route("**/api/sessions/example/repository-*", async route => {
      const url = new URL(route.request().url());
      const operation = url.pathname.split("/").at(-1);
      if (operation === "repository-histories" || operation === "repository-history") {
        const review = "review-a";
        const result = {repository: "fixture", snapshot: "snapshot-a", review, pair: pair(review),
          history: {page: 0, hasMore: false, commits: []},
          summary: {commitCount: 0, stats: {files: 12, additions: 24012, deletions: 0}}};
        return route.fulfill({json: operation === "repository-histories" ? {repositories: [result]} : result});
      }
      if (operation === "repository-states") return route.fulfill({json: {repositories: [{repository: "fixture", head: pair("review-a").head}]}});
      if (operation === "repository-comparison") {
        const review = url.searchParams.get("review") || "review-a";
        const count = review === "review-a" ? 12 : 1;
        return route.fulfill({json: {snapshot: review.replace("review", "snapshot"), review,
          pair: pair(review), historyHead: pair(review).head,
          stats: {files: count, additions: count * 2001, deletions: 0, binaryFiles: 0},
          files: Array.from({length: count}, (_, index) => file(index))}});
      }
      if (operation !== "repository-files") return route.continue();
      const snapshot = url.searchParams.get("snapshot");
      const ids = url.searchParams.getAll("file");
      batches.push({snapshot, ids});
      inFlight++; maxInFlight = Math.max(maxInFlight, inFlight);
      try {
        if (snapshot === "snapshot-a" && firstBatchesHeld && batches.filter(batch => batch.snapshot === "snapshot-a").length <= 2) {
          await new Promise(resolve => { const previous = releaseFirstBatches; releaseFirstBatches = () => { previous(); resolve(); }; });
        }
        if (snapshot === "snapshot-slow") await new Promise(resolve => { releaseSlow = resolve; });
        const files = ids.map(id => {
          if (snapshot === "snapshot-a" && id === "file-11" && !failedOnce) {
            failedOnce = true;
            return {file: id, error: "Fixture preview failed"};
          }
          return {file: id, content: content(Number(id.slice(5)), snapshot)};
        });
        await route.fulfill({json: {files}});
      } catch (error) {
        // A comparison change aborts its old request; Playwright may reject its late fulfillment.
        if (snapshot !== "snapshot-slow") throw error;
      } finally { inFlight--; }
    });
    await page.goto(baseURL + "/example/");
    await page.evaluate(async () => {
      const section = document.createElement("section");
      section.id = "fixture-repositories"; section.className = "active";
      section.style.cssText = "position:fixed;inset:0;z-index:1000;background:white;overflow:auto";
      section.innerHTML = `<div class="section-heading"><span>1</span></div><div class="repo-grid">
        <article class="panel repo-card" data-repository-id="fixture" data-repository-name="Fixture" data-repository-head="${"b".repeat(40)}">
          <div data-repository-status></div><div class="repository-history"><div class="repository-review-actions">
            <button data-review-branch>Compare</button><button data-review-refresh>Refresh commits</button>
          </div><p class="repository-head-change" hidden></p><div data-repository-commits></div></div>
        </article></div>`;
      document.body.append(section);
      const {mount} = await import("/static/repository-review.js?v=5");
      window.fixtureReview = mount({slug: "example", element: section,
        createCopyButton: () => document.createElement("button")});
    });
    const fixture = page.locator("#fixture-repositories");
    await expect(fixture.locator("[data-review-branch]")).toHaveText("Compare");
    await fixture.locator("[data-review-branch]").click();
    await expect(fixture.locator(".repository-file-section")).toHaveCount(12);
    const loadAll = fixture.getByRole("button", {name: "Load all diffs"});
    await loadAll.click();
    await expect.poll(() => batches.filter(batch => batch.snapshot === "snapshot-a").length).toBe(2);
    await expect(fixture.locator(".repository-load-all-status")).toHaveText("0/12 diffs loaded");
    await expect(loadAll).toBeDisabled();
    firstBatchesHeld = false; releaseFirstBatches();
    await expect.poll(() => failedOnce).toBe(true);
    await expect(fixture.locator(".repository-load-all-status")).toHaveText("11/12 diffs loaded · 1 failed");
    await expect(fixture.getByRole("button", {name: "Retry failed diffs"})).toBeEnabled();
    await expect(fixture.locator('.repository-file-section[data-file-id="file-11"] .notice.warning')).toHaveText("Fixture preview failed");
    const initialBatches = batches.filter(batch => batch.snapshot === "snapshot-a");
    assert.deepEqual(initialBatches.map(batch => batch.ids.length), [4, 4, 4]);
    assert.deepEqual(initialBatches.flatMap(batch => batch.ids).sort(), Array.from({length: 12}, (_, index) => `file-${index}`).sort());
    assert.ok(maxInFlight <= 2, `at most two file batches, observed ${maxInFlight}`);
    assert.ok(maxInFlight === 2, "the fixture should exercise parallel batches");
    await fixture.getByRole("button", {name: "Retry failed diffs"}).click();
    await expect(fixture.locator(".repository-load-all-status")).toHaveText("12/12 diffs loaded");
    assert.deepEqual(batches.filter(batch => batch.snapshot === "snapshot-a").slice(3).map(batch => batch.ids), [["file-11"]]);
    await expect(fixture.locator(".fixture-editor")).toHaveCount(12);
    await page.evaluate(() => { window.fixtureFirstEditor = document.querySelector('#fixture-repositories [data-file-id="file-0"] .fixture-editor'); });
    await fixture.locator('.repository-file-section[data-file-id="file-11"]').scrollIntoViewIfNeeded();
    await fixture.locator('.repository-file-section[data-file-id="file-0"]').scrollIntoViewIfNeeded();
    const firstLoaded = await page.evaluate(() => window.fixtureFirstEditor === document.querySelector('#fixture-repositories [data-file-id="file-0"] .fixture-editor'));
    assert.equal(firstLoaded, true, "scrolling keeps the first editor mounted");
    await expect(fixture.locator('[data-file-id="file-0"] .fixture-editor')).toContainText("snapshot-a file 0");
    await page.evaluate(() => {
      const overview = document.querySelector("#fixture-repositories .repository-overview");
      window.fixtureReview.updateHTML(overview.innerHTML);
      window.fixtureReview.suspend(); window.fixtureReview.resume();
    });
    await expect(fixture.locator(".fixture-editor")).toHaveCount(12);
    assert.equal(await page.evaluate(() => window.fixtureFirstEditor === document.querySelector('#fixture-repositories [data-file-id="file-0"] .fixture-editor')), true,
      "details refresh and suspension keep loaded editors");
    await fixture.getByRole("button", {name: "Toggle diff for file-00.txt"}).click();
    await fixture.getByRole("button", {name: "Toggle diff for file-00.txt"}).click();
    assert.equal(await page.evaluate(() => window.fixtureFirstEditor === document.querySelector('#fixture-repositories [data-file-id="file-0"] .fixture-editor')), true);
    assert.equal(batches.filter(batch => batch.snapshot === "snapshot-a").length, 4, "retained content is not fetched again");

    const navigate = review => page.evaluate(value => {
      const url = new URL(location.href); url.searchParams.set("tab", "repositories");
      url.searchParams.set("repository", "fixture"); url.searchParams.set("review", value);
      history.pushState(null, "", url); dispatchEvent(new PopStateEvent("popstate"));
    }, review);
    await navigate("review-slow");
    await expect(fixture.locator(".repository-file-section")).toHaveCount(1);
    await fixture.getByRole("button", {name: "Load all diffs"}).click();
    await expect.poll(() => batches.filter(batch => batch.snapshot === "snapshot-slow").length).toBe(1);
    await navigate("review-b");
    await expect(fixture.locator(".repository-file-section")).toHaveCount(1);
    await fixture.getByRole("button", {name: "Load all diffs"}).click();
    await expect(fixture.locator(".fixture-editor")).toContainText("snapshot-b file 0");
    releaseSlow();
    await expect.poll(() => inFlight).toBe(0);
    await expect(fixture.locator(".fixture-editor")).toContainText("snapshot-b file 0");
    assert.equal(await fixture.locator(".repository-file-section").count(), 1);
    assert.deepEqual(errors, []);
  } finally {
    releaseFirstBatches(); releaseSlow();
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
