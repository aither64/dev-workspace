"use strict";

const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const item = number => ({
  turnId: "turn-1", itemId: `item-${number}`,
  kind: number === 200 ? "plan" : "agentMessage",
  text: number === 200 ? "A completed implementation plan" : `Message ${number}`,
  turnStatus: "completed",
});
const items = (first, last) => Array.from({length: last - first + 1}, (_, index) => item(first + index));

(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1280, height: 720}});
    const errors = [];
    page.on("pageerror", error => errors.push(error.message));
    let newest = items(101, 200), mode = "", metadataPending = true, pageReads = 0;
    let releaseQueue;
    await page.route("**/api/sessions/example/**", async route => {
      const path = new URL(route.request().url());
      const operation = path.pathname.split("/").at(-1);
      const json = (value, status = 200) => route.fulfill({status, contentType: "application/json", body: JSON.stringify(value)});
      if (path.pathname.endsWith("/thread/page")) {
        pageReads++;
        const older = path.searchParams.has("cursor");
        return json({threadId: "thread-1", status: "idle", latestTurnId: "turn-1",
          entries: older ? items(1, 100) : newest,
          hasOlder: !older, olderCursor: older ? null : "older-page",
          collaborationMode: mode, metadataPending});
      }
      if (operation === "thread") return json({error: "Unexpected full transcript read"}, 503);
      if (operation === "reconcile") {
        await new Promise(resolve => { releaseQueue = resolve; });
        return json({ok: true});
      }
      if (operation === "queue") return json([]);
      if (operation === "pending") return json({error: "Pending read unavailable"}, 503);
      if (operation === "activity") return json({currentState: "idle", workingMs: 0, waitingMs: 0, coverageComplete: true});
      return route.continue();
    });
    await page.goto(baseURL + "/example/#codex");
    await expect(page.locator("#transcript .message")).toHaveCount(100);
    await expect(page.locator("#codex-status")).toHaveText("idle");
    await expect(page.locator("#codex-mode-status")).toHaveText("Checking mode…");
    await expect(page.locator("#plan-actions")).toBeHidden();
    await expect(page.locator("#pending-status")).toContainText("Requests could not be refreshed");
    await expect.poll(() => Boolean(releaseQueue)).toBe(true);
    releaseQueue();
    await expect(page.locator("#load-older")).toBeVisible();
    await page.evaluate(() => {
      const log = document.getElementById("transcript");
      const anchor = [...log.querySelectorAll(".message")].find(node => node.textContent.includes("Message 150"));
      log.scrollTop = anchor.offsetTop - log.offsetTop - 50;
      window.pagingAnchor = {node: anchor, top: anchor.getBoundingClientRect().top};
    });
    await page.locator("#load-older").click();
    await expect(page.locator("#transcript .message")).toHaveCount(200);
    const anchorShift = await page.evaluate(() => ({
      same: [...document.querySelectorAll("#transcript .message")].some(node => node === window.pagingAnchor.node),
      shift: Math.abs(window.pagingAnchor.node.getBoundingClientRect().top - window.pagingAnchor.top),
    }));
    assert.equal(anchorShift.same, true);
    assert(anchorShift.shift < 8, `older page moved the scroll anchor by ${anchorShift.shift}px`);
    newest = items(102, 201); mode = "plan"; metadataPending = false;
    await page.evaluate(() => fetch("/fixture/refresh"));
    await expect(page.locator("#transcript .message")).toHaveCount(201);
    await expect(page.locator("#codex-mode-status")).toBeHidden();
    assert.equal(await page.evaluate(() => [...document.querySelectorAll("#transcript .message")]
      .some(node => node === window.pagingAnchor.node)), true);
    assert(pageReads >= 3);
    assert.deepEqual(errors, []);
    console.log("paged transcript, retained DOM, mode recovery and independent lanes passed");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
