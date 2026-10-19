"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");

(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.addInitScript(() => {
        const now = Date.now;
        window.testTime = 0;
        Date.now = () => now() + window.testTime;
      });
      const failed = {slug: "failed", kind: "archive", state: "failed", phase: "starting",
        receiptId: "a".repeat(64), startedAt: "2026-10-19T12:00:00Z", updatedAt: "2026-10-19T12:00:01Z",
        error: "tracking files have no prior commit: plan.md, state.md", options: {journalId: "b".repeat(64)}};
      const running = {slug: "running", kind: "archive", state: "running", phase: "prepared",
        receiptId: "c".repeat(64), startedAt: "2026-10-19T12:00:02Z", updatedAt: "2026-10-19T12:00:02Z"};
      let operations = [failed, running], reads = 0, retries = [], releaseRetry;
      const sessions = ["first", "second"].map((slug, i) => ({slug, archived: true,
        updatedAt: `2020-10-19T12:00:0${2 - i}Z`, repositoryCount: 0, runningClusters: 0}));
      await page.route("**/api/index-status*", route => {
        reads++;
        return route.fulfill({json: {generatedAt: new Date(Date.now() + 86400_000).toISOString(),
          authoritative: true, sessions, operations}});
      });
      await page.route("**/api/sessions/failed/operation/retry", async route => {
        retries.push(route.request().postDataJSON());
        await new Promise(resolve => { releaseRetry = resolve; });
        return route.fulfill({json: {}});
      });
      await page.route("**/api/sessions/failed/operation", route => {
        assert.equal(route.request().method(), "DELETE");
        operations = operations.filter(operation => operation.slug !== "failed");
        return route.fulfill({json: {}});
      });
      await page.goto(process.argv[2] + "/");
      const detail = page.locator(".operation-error");
      await expect(detail).toContainText("tracking files have no prior commit");
      await page.locator(".archive-list summary").click();
      await expect(page.locator('[data-session-slug="first"]')).toBeVisible();
      await detail.evaluate(element => {
        window.savedError = element;
        window.savedText = element.firstChild;
        const range = document.createRange(); range.selectNodeContents(element);
        getSelection().removeAllRanges(); getSelection().addRange(range);
        window.savedSelection = getSelection().toString();
        window.savedCard = document.querySelector('[data-session-slug="first"]');
        window.savedCardText = window.savedCard.querySelector("strong").firstChild;
        window.cardMutations = [];
        new MutationObserver(records => window.cardMutations.push(...records.filter(record => record.type === "childList")))
          .observe(window.savedCard.parentElement, {childList: true, subtree: true});
      });
      const refresh = async () => {
        const before = reads;
        await page.evaluate(() => {
          window.dispatchEvent(new Event("pagehide"));
          window.testTime += 31000;
          window.dispatchEvent(new Event("pageshow"));
        });
        await expect.poll(() => reads).toBeGreaterThan(before);
        await page.waitForTimeout(100);
      };
      // Full refreshes, plus the independent fast progress poll, must leave the
      // failed row intact while another operation changes.
      for (let i = 0; i < 3; i++) {
        running.phase = i % 2 ? "prepared" : "quiesced";
        await refresh();
        assert.deepEqual(await page.evaluate(() => ({
          element: window.savedError === document.querySelector(".operation-error"),
          text: window.savedText === window.savedError.firstChild,
          selection: getSelection().toString() === window.savedSelection,
          card: window.savedCard === document.querySelector('[data-session-slug="first"]'),
          cardText: window.savedCardText === window.savedCard.querySelector("strong").firstChild,
          mutations: window.cardMutations.length,
        })), {element: true, text: true, selection: true, card: true, cardText: true, mutations: 0});
      }
      const beforeProgress = reads;
      await expect.poll(() => reads, {timeout: 10000}).toBeGreaterThan(beforeProgress);
      assert.equal(await page.evaluate(() => getSelection().toString() === window.savedSelection), true);
      // Existing controls must use the newest journal data, and stay disabled
      // across a refresh while the request is in flight.
      failed.options.journalId = "d".repeat(64);
      await refresh();
      const retry = detail.locator("..").getByRole("button", {name: /^Retry/});
      await retry.click();
      await expect.poll(() => retries.length).toBe(1);
      assert.equal(retries[0].journalId, "d".repeat(64));
      await refresh(); await expect(retry).toBeDisabled();
      releaseRetry();
      // The retry may have already failed again before polling sees running.
      failed.attempt = 2;
      await expect(retry).toHaveText("Retry archive");
      await expect(retry).toBeEnabled();
      failed.error = "Updated diagnostic";
      await refresh(); await expect(detail).toHaveText("Updated diagnostic");
      await page.getByRole("button", {name: "Dismiss", exact: true}).click();
      await expect(detail).toHaveCount(0);
      failed.kind = "delete";
      operations.push(failed);
      await refresh();
      const force = page.getByRole("button", {name: /^(Force delete|Forcing…)/});
      page.once("dialog", dialog => dialog.accept());
      await force.click();
      await expect.poll(() => retries.length).toBe(2);
      assert.equal(retries[1].force, true);
      await refresh(); await expect(force).toBeDisabled();
      releaseRetry();
      failed.attempt = 3;
      await expect(force).toHaveText("Force delete");
      await expect(force).toBeEnabled();
      await refresh(); await expect(force).toBeEnabled();
      await page.getByRole("button", {name: "Dismiss", exact: true}).click();
      await expect(detail).toHaveCount(0);
      sessions.reverse(); sessions[0].updatedAt = "2026-10-19T12:10:00Z";
      await refresh();
      assert.deepEqual(await page.locator('[data-session-list="archived"] [data-session-slug]').evaluateAll(cards => cards.map(card => card.dataset.sessionSlug)), ["second", "first"]);
      assert.deepEqual(errors, []);
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exit(1); });
