"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];

(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [], sends = [];
      let reads = 0, threadId = "thread-1", metadataPending = true;
      let failSend = true, holdRetry = false, releaseRetry;
      let entry = {turnId: "turn-1", itemId: "message-1", kind: "agentMessage", text: "Timestamp fixture",
        timestamp: "2026-10-18T11:00:00Z", timestampApproximate: true};
      page.on("pageerror", error => { errors.push(error.message); console.error("Page error:", error.message); });
      page.on("dialog", dialog => dialog.dismiss());
      await page.addInitScript(() => {
        window.indexSnapshotReads = 0;
        document.addEventListener("session-list-updated", () => { window.indexSnapshotReads++; });
        window.EventSource = class {
          constructor() { window.recoveryDisplayEvents = this; queueMicrotask(() => this.onopen?.()); }
          addEventListener() {}
          close() {}
        };
      });
      await page.route("**/api/sessions/example/**", async route => {
        const operation = new URL(route.request().url()).pathname.split("/").at(-1);
        if (operation === "thread" || operation === "page") {
          reads++;
          return route.fulfill({json: {threadId, metadataPending, status: "idle", latestTurnId: "turn-1",
            collaborationMode: "default", model: "model-1", reasoningEffort: "medium", entries: [entry], hasOlder: false}});
        }
        if (operation === "message") {
          const body = route.request().postDataJSON();
          sends.push(body);
          if (failSend) { failSend = false; return route.abort("failed"); }
          if (holdRetry && body.retry) {
            holdRetry = false;
            await new Promise(resolve => { releaseRetry = resolve; });
          }
          return route.fulfill({status: 202, json: {clientUserMessageId: body.clientUserMessageId, turnId: "turn-2"}});
        }
        if (operation === "pending" || operation === "queue") return route.fulfill({json: []});
        if (operation === "reconcile") return route.fulfill({json: {ok: true}});
        if (operation === "activity") return route.fulfill({json: {currentState: "idle", workingMs: 0, waitingMs: 0, coverageComplete: true}});
        if (operation === "details") return route.fulfill({json: {repositoriesHTML: "", artifactsHTML: ""}});
        return route.continue();
      });
      const time = new Date("2026-10-18T12:00:00Z");
      await page.clock.install({time});
      await page.clock.pauseAt(new Date(time.getTime() + 1000));
      const flushStartup = async (count, previous = 0) => {
        await expect.poll(async () => {
          await page.clock.runFor(0);
          return count();
        }).toBeGreaterThan(previous);
      };
      await page.goto(baseURL + "/example/#codex");
      await flushStartup(() => reads);
      const message = page.locator('#message-form textarea[name="message"]');
      const receipt = text => page.locator(".message-receipt").filter({has: page.locator("span", {hasText: text})});
      const timestamp = page.locator('#transcript [data-transcript-entry-key] .message-time').first();
      await expect(page.locator("#transcript")).toContainText("Timestamp fixture");
      await expect(timestamp).toBeEmpty();
      await page.locator('#transcript [data-transcript-entry-key]').first().evaluate(node => {
        window.timestampNode = node;
        window.timestampTextNode = node.firstChild;
        const range = document.createRange();
        range.selectNodeContents(node.firstChild);
        getSelection().removeAllRanges();
        getSelection().addRange(range);
      });
      await page.clock.runFor(15_000);
      await expect(timestamp).toBeEmpty();
      await page.clock.runFor(15_000);
      await expect(timestamp).toHaveText(/^~/);
      assert.equal(await page.evaluate(() => window.timestampNode.isConnected &&
        window.timestampNode.firstChild === window.timestampTextNode && getSelection().toString() === "Timestamp fixture"), true);
      const refresh = async () => {
        const before = reads;
        await page.evaluate(() => window.recoveryDisplayEvents.onmessage());
        await page.clock.runFor(300);
        await expect.poll(() => reads).toBeGreaterThan(before);
      };
      metadataPending = false;
      entry = {...entry, timestamp: "2026-10-18T11:04:00Z", timestampApproximate: false};
      await refresh();
      await expect(timestamp).not.toHaveText(/^~/);
      assert.equal(await page.evaluate(() => window.timestampNode.isConnected &&
        window.timestampNode.firstChild === window.timestampTextNode && getSelection().toString() === "Timestamp fixture"), true);
      const exactText = await timestamp.textContent();
      metadataPending = true;
      entry = {...entry, timestamp: "2026-10-18T11:00:00Z", timestampApproximate: true};
      await refresh();
      await expect(timestamp).toHaveText(exactText);

      await message.fill("Original request");
      await page.locator("#message-send").click();
      await expect(receipt("Original request")).toContainText("Outcome unknown.");
      await expect(message).toBeEmpty(); // Original intent lives in its recoverable receipt.
      await message.fill("Revised request");
      await page.locator("#message-send").click();
      await expect(message).toBeEmpty();
      await expect(receipt("Original request")).toContainText("Outcome unknown.");
      await message.fill("Keep this revised draft");
      holdRetry = true;
      const retry = receipt("Original request").getByRole("button", {name: "Retry original", exact: true});
      await retry.evaluate(button => { button.click(); button.click(); });
      await expect.poll(() => sends.length).toBe(3);
      assert.equal(sends[2].clientUserMessageId, sends[0].clientUserMessageId);
      assert.equal(sends[2].message, "Original request");
      assert.equal(sends[2].retry, true);
      releaseRetry();
      await expect(receipt("Original request")).toContainText("Sent to Codex");
      await expect(message).toHaveValue("Keep this revised draft");

      const savedID = "00000000-0000-4000-8000-000000000099";
      const attachmentID = "00000000-0000-4000-8000-000000000098";
      await page.evaluate(({savedID, attachmentID}) => sessionStorage.setItem(
        `workspace-portal.send-attempt.example.thread-1.${savedID}`,
        JSON.stringify({message: "Attachment retry", attachmentIds: [attachmentID], context: ""}),
      ), {savedID, attachmentID});
      const beforeAttachmentReload = reads;
      await page.reload();
      await flushStartup(() => reads, beforeAttachmentReload);
      await receipt("Attachment retry").getByRole("button", {name: "Retry original"}).click();
      await expect(receipt("Attachment retry")).toContainText("Sent to Codex");
      assert.equal(sends.at(-1).clientUserMessageId, savedID);
      assert.deepEqual(sends.at(-1).attachmentIds, [attachmentID]);
      await page.evaluate(() => {
        window.restoreStorage = Storage.prototype.setItem;
        Storage.prototype.setItem = () => { throw new Error("Storage unavailable"); };
      });
      await receipt("Attachment retry").getByRole("button", {name: "Dismiss message", exact: true}).click();
      await expect(receipt("Attachment retry")).toBeVisible();
      await page.evaluate(() => { Storage.prototype.setItem = window.restoreStorage; });
      await receipt("Attachment retry").getByRole("button", {name: "Dismiss message", exact: true}).click();
      await expect(receipt("Attachment retry")).toHaveCount(0);
      assert.equal(await page.evaluate(savedID => JSON.parse(sessionStorage.getItem(
        `workspace-portal.send-attempt.example.thread-1.${savedID}`,
      )).dismissed, savedID), true);
      const beforeDismissedReload = reads;
      await page.reload();
      await flushStartup(() => reads, beforeDismissedReload);
      await expect(page.locator("#transcript")).toContainText("Timestamp fixture");
      await expect(receipt("Attachment retry")).toHaveCount(0);

      threadId = "thread-2";
      await refresh();
      await expect(timestamp).toBeEmpty();
      metadataPending = false;
      await refresh();
      await expect(timestamp).toHaveText(/^~/);

      let indexReads = 0, warning = "Session status is temporarily unavailable; showing the last known state.";
      let loading = false, statusCode = 200;
      await page.route("**/api/index-status", route => {
        indexReads++;
        return route.fulfill({status: statusCode, json: {sessions: [], operations: [], authoritative: false,
          generatedAt: "2099-01-01T00:00:00Z", warning, loading, error: "Access denied"}});
      });
      await page.goto(baseURL + "/");
      await flushStartup(() => indexReads);
      const indexWarning = page.locator("#index-status-warning");
      const indexSnapshotReads = () => page.evaluate(() => window.indexSnapshotReads);
      await expect.poll(indexSnapshotReads).toBeGreaterThan(0);
      const advanceIndexRefresh = async (milliseconds) => {
        const before = await indexSnapshotReads();
        await page.clock.runFor(milliseconds);
        await expect.poll(indexSnapshotReads).toBeGreaterThan(before);
      };
      await expect.poll(() => indexReads).toBeGreaterThan(0);
      await expect(indexWarning).toBeHidden();
      await advanceIndexRefresh(15_000);
      await page.clock.runFor(14_999);
      await expect(indexWarning).toBeHidden();
      await advanceIndexRefresh(1);
      await expect(indexWarning).toHaveText(warning);
      warning = "";
      await advanceIndexRefresh(15_000);
      await expect(indexWarning).toBeHidden();
      loading = true; warning = "Session status is loading.";
      indexReads = 0;
      await page.reload();
      await flushStartup(() => indexReads);
      await expect.poll(indexSnapshotReads).toBeGreaterThan(0);
      await expect.poll(() => indexReads).toBeGreaterThan(0);
      await page.clock.runFor(749); await expect(indexWarning).toBeHidden();
      await page.clock.runFor(1); await expect(indexWarning).toHaveText("");
      await expect(indexWarning).toBeHidden();
      loading = false; statusCode = 403;
      await page.clock.runFor(15_000);
      await expect(indexWarning).toHaveText("Access denied");
      assert.deepEqual(errors, []);
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
