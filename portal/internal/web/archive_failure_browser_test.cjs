"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [];
      page.on("pageerror", error => { errors.push(error.message); console.error("Page error:", error.message); });
      page.on("requestfailed", request => console.error("Request failed:", request.url(), request.failure()));
      await page.addInitScript(() => {
        const realNow = Date.now;
        window.archiveTestOffset = 0;
        Date.now = () => realNow() + window.archiveTestOffset;
      });
      let reads = 0, offline = false, held = false, release;
      let operation = {kind: "archive", state: "paused", phase: "tracking_committed", receiptId: "receipt-1",
        startedAt: "2026-09-16T13:02:17Z", updatedAt: "2026-09-16T13:02:17Z",
        options: {journalId: "a".repeat(64), journalExpected: true}};
      const failure = {identity: "thread-1", operation: {id: "a".repeat(64), identity: "thread-1"}, result: "deferred",
        checked_at: "2026-09-16T20:02:12Z", blockers: ["command failed with exit 124: /nix/store/fixture/bin/workspace-portal thread retire"]};
      await page.route("**/api/sessions/example/**", async route => {
        const method = new URL(route.request().url()).pathname.split("/").at(-1);
        const json = (value, status = 200) => route.fulfill({status, contentType: "application/json", body: JSON.stringify(value)});
        if (method === "operation") return json(operation);
        if (method === "auto-archive") {
          reads++;
          if (held) await new Promise(resolve => { release = resolve; });
          if (offline) return json({error: "Fixture unavailable"}, 503);
          return json(failure);
        }
        if (method === "thread") return json({threadId: "thread-1", status: "idle", entries: []});
        if (["pending", "queue"].includes(method)) return json([]);
        return route.continue();
      });
      await page.goto(process.argv[2] + "/example/");
      const banner = page.locator("#archive-last-failure");
      await expect(banner).toContainText("Closing the conversation timed out.");
      await expect(page.locator("#lifecycle-operation-detail")).toContainText("since the last completed step");
      await expect(page.locator("#message-form")).toHaveCount(0);
      const initial = await banner.textContent();
      const wake = async () => page.evaluate(() => {
        window.archiveTestOffset += 31_000;
        document.dispatchEvent(new Event("visibilitychange"));
      });
      // A settings visit shares the pending-page read, and focus does not evade throttling.
      await banner.getByRole("link", {name: "Technical details"}).click();
      await expect(page.locator("#settings")).toBeVisible();
      assert.equal(reads, 1);
      offline = true;
      await wake();
      await expect.poll(() => reads).toBe(2);
      await expect(page.locator("#auto-archive-status")).toContainText("Could not refresh");
      assert.equal(await banner.textContent(), initial);
      offline = false; held = true;
      await wake();
      await expect.poll(() => reads).toBe(3);
      await page.evaluate(() => {
        document.dispatchEvent(new Event("visibilitychange"));
        window.dispatchEvent(new Event("pageshow"));
      });
      assert.equal(reads, 3);
      held = false; release();
      await expect(page.locator("#auto-archive-status")).toBeHidden();
      failure.operation.id = "b".repeat(64);
      await wake();
      await expect(banner).toBeHidden();
      failure.operation.id = "a".repeat(64);
      // Hidden pages do not poll. Waking after the deadline refreshes the warning.
      await page.evaluate(() => Object.defineProperty(document, "hidden", {configurable: true, value: true}));
      const hiddenReads = reads;
      await wake();
      assert.equal(reads, hiddenReads);
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: false});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect(banner).toBeVisible();
      operation = {...operation, state: "running"};
      await wake();
      await expect(page.locator("#lifecycle-operation-detail")).toContainText("Running");
      await expect(banner).toContainText("Last automatic attempt failed");
      // Completion must clear the historical warning and reload the final page.
      operation = {...operation, state: "complete"};
      const nextNavigation = page.waitForEvent("framenavigated", {predicate: frame => frame === page.mainFrame()});
      await nextNavigation;
      await expect(banner).toBeHidden();
      assert.deepEqual(errors, []);
      console.log(engine.name() + ": archive diagnostics, throttling, recovery and read-only state passed");
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
