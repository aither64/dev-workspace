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
    let inFlight = 0, maxInFlight = 0, firstBatchesHeld = true, failedOnce = false, worktreeCaptures = 0, expireWorktree = false, failCapture = false;
    let historyMode = "zero";
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
        if (historyMode === "request-error") return route.fulfill({status: 503, json: {error: "Fixture history failed"}});
        if (historyMode === "batch-error" && operation === "repository-histories") {
          return route.fulfill({json: {repositories: url.searchParams.getAll("repository").map(repository =>
            ({repository, error: "Fixture batch history failed"}))}});
        }
        const review = "review-a";
        const commitCount = historyMode === "one" ? 1 : historyMode === "multi" ? 83 : 0;
        const result = repository => ({repository, snapshot: "snapshot-a", review, pair: pair(review),
          history: {page: 0, hasMore: historyMode === "multi", commits: commitCount ? [{id: "first", sha: pair(review).head, subject: "First-page commit"}] : []},
          ...(historyMode === "missing" ? {summaryError: "Fixture totals unavailable"} :
            {summary: {commitCount, stats: {files: historyMode === "zero" ? 0 : 12,
              additions: historyMode === "zero" ? 0 : 24012, deletions: 0, binaryFiles: historyMode === "multi" ? 1 : 0}}}),
          ...(historyMode === "conflict" ? {summaryError: "Fixture contradictory totals"} : {})});
        return route.fulfill({json: operation === "repository-histories" ?
          {repositories: url.searchParams.getAll("repository").map(result)} : result(url.searchParams.get("repository"))});
      }
      if (operation === "repository-states") return route.fulfill({json: {repositories: [{repository: "fixture", head: pair("review-a").head}]}});
      if (operation === "repository-comparison") {
        if (route.request().method() === "POST") {
          const {kind} = route.request().postDataJSON();
          assert.equal(kind, "unstaged");
          if (failCapture) return route.fulfill({status: 503, json: {error: "Fixture capture failed"}});
          worktreeCaptures++;
          return route.fulfill({json: {snapshot: `ephemeral-${worktreeCaptures}`, kind, ephemeral: true}});
        }
        const ephemeral = url.searchParams.get("snapshot");
        if (ephemeral) {
          if (expireWorktree) return route.fulfill({status: 409, json: {error: "This snapshot has expired. Capture the changes again."}});
          const changedFiles = ["ephemeral-2", "ephemeral-4"].includes(ephemeral) ? [] : ephemeral === "ephemeral-3" ? null : [file(0)];
          return route.fulfill({json: {snapshot: ephemeral, review: "", kind: "unstaged", ephemeral: true,
            capturedAt: "2026-09-30T12:00:00Z", sourceHead: "a".repeat(40), pair: pair("review-a"),
            stats: {files: changedFiles?.length || 0, additions: changedFiles?.length || 0, deletions: changedFiles?.length || 0, binaryFiles: 0}, files: changedFiles,
            unverifiedSubmodules: [{path: "nested/module", mode: "160000", object: "f".repeat(40)}]}});
        }
        const review = url.searchParams.get("review") || "review-a";
        if (review === "review-error") return route.fulfill({status: 503, json: {error: "Fixture comparison failed"}});
        const count = review === "review-a" ? 12 : review === "review-empty" || review === "review-null" ? 0 : 1;
        return route.fulfill({json: {snapshot: review.replace("review", "snapshot"), review,
          pair: pair(review), historyHead: pair(review).head,
          stats: {files: count, additions: count * 2001, deletions: 0, binaryFiles: 0},
          files: review === "review-null" ? null : review === "review-bad" ? {} : Array.from({length: count}, (_, index) => file(index))}});
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
          <div data-repository-status></div><div class="repository-review-actions">
            <button data-review-branch disabled>Compare</button><button data-review-worktree="staged">Staged changes</button><button data-review-worktree="unstaged">Unstaged changes</button><button data-review-refresh>Refresh commits</button>
          </div><p class="repository-head-change" hidden></p><details class="repository-history"><summary>Local commits <span class="repository-history-summary muted" data-repository-history-summary>Loading totals…</span></summary><div data-repository-commits></div></details>
        </article></div>`;
      document.body.append(section);
      const {mount} = await import("/static/repository-review.js?v=9");
      window.fixtureReview = mount({slug: "example", element: section,
        createCopyButton: () => document.createElement("button")});
    });
    const fixture = page.locator("#fixture-repositories");
    await expect(fixture.locator(".repository-history")).not.toHaveAttribute("open");
    const history = fixture.locator(".repository-history");
    const summary = history.locator("summary");
    const totals = summary.locator("[data-repository-history-summary]");
    await expect(totals).toContainText("0 commits · 0 changed files");
    await expect(history.locator("[data-repository-commits] .repository-history-summary")).toHaveCount(0);
    await summary.click();
    await page.evaluate(() => {
      window.fixtureHistory = document.querySelector("#fixture-repositories .repository-history");
      window.fixtureHistorySummary = window.fixtureHistory.querySelector("summary");
    });
    historyMode = "one";
    await fixture.locator("[data-review-refresh]").click();
    await expect(totals).toContainText("1 commit · 12 changed files");
    historyMode = "multi";
    await fixture.locator("[data-review-refresh]").click();
    await expect(totals).toContainText("83 commits · 12 changed files");
    await expect(totals).toContainText("1 binary file");
    await expect(history.locator(".repository-commit")).toHaveCount(1);
    assert(await page.evaluate(() => document.querySelector("#fixture-repositories .repository-history") === window.fixtureHistory &&
      document.querySelector("#fixture-repositories .repository-history > summary") === window.fixtureHistorySummary));
    await expect(history).toHaveAttribute("open", "");
    historyMode = "missing";
    await fixture.locator("[data-review-refresh]").click();
    await expect(totals).toHaveText("Totals unavailable");
    await expect(history.locator("[data-repository-commits] .notice.warning")).toHaveText("Fixture totals unavailable");
    historyMode = "conflict";
    await fixture.locator("[data-review-refresh]").click();
    await expect(totals).toHaveText("Totals unavailable");
    await expect(history.locator("[data-repository-commits] .notice.warning")).toHaveText("Fixture contradictory totals");
    historyMode = "request-error";
    await fixture.locator("[data-review-refresh]").click();
    await expect(totals).toHaveText("Totals unavailable");
    await expect(history.locator("[data-repository-commits] .notice.warning")).toHaveText("Fixture history failed");
    historyMode = "zero";
    await summary.click();
    await fixture.locator("[data-review-refresh]").click();
    await expect(totals).toContainText("0 commits · 0 changed files");
    await expect(history).not.toHaveAttribute("open");
    await expect(fixture.locator("[data-review-branch]")).toHaveAttribute("href", /review=review-a/);
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
      url.searchParams.delete("snapshot"); url.searchParams.delete("kind");
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
    await fixture.getByRole("button", {name: "← Repositories"}).first().click();
    await fixture.locator('[data-review-worktree="unstaged"]').click();
    await expect(fixture.locator(".repository-review-title").last()).toContainText("Unstaged changes");
    const submoduleNotice = fixture.locator(".repository-comparison > .notice.warning:has(> details):visible");
    await expect(submoduleNotice).toContainText("did not inspect the working state of 1 submodule");
    await expect(fixture.locator(".repository-comparison-stats")).toContainText("1 changed file");
    assert.equal(new URL(page.url()).searchParams.get("snapshot"), "ephemeral-1");
    assert.equal(new URL(page.url()).searchParams.has("review"), false);
    await fixture.getByRole("button", {name: "Recapture unstaged changes"}).click();
    await expect.poll(() => new URL(page.url()).searchParams.get("snapshot")).toBe("ephemeral-2");
    assert.equal(worktreeCaptures, 2);
    await expect(fixture.locator(".repository-file-section")).toHaveCount(0);
    await expect(fixture.locator(".repository-file-scroll > .empty")).toHaveText("No changes in this snapshot.");
    await expect(fixture.locator(".repository-review-heading button").filter({hasText: "Load all diffs"})).toBeHidden();
    await expect(submoduleNotice).toContainText("did not inspect the working state of 1 submodule");
    await expect(fixture.locator(".repository-comparison-stats")).toContainText("0 changed files");
    const expiredURL = page.url();
    expireWorktree = true;
    await navigate("review-b");
    await page.evaluate(href => { history.pushState(null, "", href); dispatchEvent(new PopStateEvent("popstate")); }, expiredURL);
    await expect(fixture.getByRole("button", {name: "Recapture unstaged changes"})).toBeVisible();
    const beforeEmptyBatches = batches.length;
    for (const review of ["review-empty", "review-null"]) {
      await navigate(review);
      await expect(fixture.locator(".repository-file-section")).toHaveCount(0);
      await expect(fixture.locator(".repository-file-scroll > .empty")).toHaveText("No changed files between these revisions.");
      await expect(fixture.locator(".repository-review-heading button").filter({hasText: "Load all diffs"})).toBeHidden();
      await fixture.getByRole("button", {name: "← Repositories"}).first().click();
      await expect(fixture.locator(".repository-overview")).toBeVisible();
    }
    assert.equal(batches.length, beforeEmptyBatches, "empty committed reviews requested no file previews");
    for (const [review, message] of [["review-bad", "Invalid comparison files."], ["review-error", "Fixture comparison failed"]]) {
      await navigate(review);
      await expect(fixture.locator(".repository-comparison > .notice.warning")).toHaveText(message);
      await expect(fixture.locator(".repository-file-scroll > .empty")).toHaveCount(0);
      await fixture.getByRole("button", {name: "← Repositories"}).first().click();
    }
    failCapture = true;
    await fixture.locator('[data-review-worktree="unstaged"]').click();
    await expect(fixture.locator("[data-worktree-capture-error]")).toHaveText("Fixture capture failed");
    await expect(fixture.locator("[data-worktree-capture-error]")).toBeVisible();
    await expect(fixture.locator(".repository-history")).not.toHaveAttribute("open");
    await expect(fixture.locator(".repository-history [data-worktree-capture-error]")).toHaveCount(0);
    failCapture = false; expireWorktree = false;
    await fixture.locator('[data-review-worktree="unstaged"]').click();
    await expect.poll(() => new URL(page.url()).searchParams.get("snapshot")).toBe("ephemeral-3");
    await expect(fixture.locator("[data-worktree-capture-error]")).toHaveCount(0);
    await expect(fixture.locator(".repository-file-section")).toHaveCount(0);
    await expect(fixture.locator(".repository-file-scroll > .empty")).toHaveText("No changes in this snapshot.");
    await expect(fixture.locator(".repository-review-heading button").filter({hasText: "Load all diffs"})).toBeHidden();
    await fixture.getByRole("button", {name: "Recapture unstaged changes"}).click();
    await expect.poll(() => new URL(page.url()).searchParams.get("snapshot")).toBe("ephemeral-4");
    await expect(fixture.locator(".repository-file-scroll > .empty")).toHaveText("No changes in this snapshot.");
    assert.equal(batches.length, beforeEmptyBatches, "empty working reviews requested no file previews");
    const addHistoryCard = id => page.evaluate(value => {
      const overview = document.querySelector("#fixture-repositories .repository-overview");
      const candidate = overview.cloneNode(true);
      const card = candidate.querySelector('[data-repository-id="fixture"]').cloneNode(true);
      card.dataset.repositoryId = value;
      card.dataset.repositoryName = value;
      card.querySelector("[data-repository-history-summary]").textContent = "Loading totals…";
      card.querySelector("[data-repository-commits]").replaceChildren();
      candidate.querySelector(".repo-grid").append(card);
      window.fixtureReview.updateHTML(candidate.innerHTML);
    }, id);
    historyMode = "batch-error";
    await addHistoryCard("fixture-batch");
    const batchCard = fixture.locator('[data-repository-id="fixture-batch"]');
    await expect(batchCard.locator("[data-repository-history-summary]")).toHaveText("Totals unavailable");
    await expect(batchCard.locator("[data-repository-commits] .notice.warning")).toHaveText("Fixture batch history failed");
    historyMode = "request-error";
    await addHistoryCard("fixture-whole");
    const wholeCard = fixture.locator('[data-repository-id="fixture-whole"]');
    await expect(wholeCard.locator("[data-repository-history-summary]")).toHaveText("Totals unavailable");
    await expect(wholeCard.locator("[data-repository-commits] .notice.warning")).toHaveText("Fixture history failed");
    assert.deepEqual(errors, []);
  } finally {
    releaseFirstBatches(); releaseSlow();
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
