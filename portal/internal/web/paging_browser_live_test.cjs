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
  const browser = await chromium.launch({headless: true, channel: "chromium",
    ignoreDefaultArgs: ["--hide-scrollbars"]});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1280, height: 720}});
    const errors = [];
    page.on("pageerror", error => errors.push(error.message));
    let newest = items(201, 300), mode = "", metadataPending = true, pageReads = 0;
    let olderReads = 0, failSecond = true, smallPage = false, holdFirstOlder = true;
    let repairScenario = false, failRepair = true, repairReads = 0, concurrentGap = false;
    let releaseQueue, releaseFirstOlder, releaseSecondError, holdQueue = true;
    await page.route("**/api/sessions/example/**", async route => {
      const path = new URL(route.request().url());
      const operation = path.pathname.split("/").at(-1);
      const json = (value, status = 200) => route.fulfill({status, contentType: "application/json", body: JSON.stringify(value)});
      if (path.pathname.endsWith("/thread/page")) {
        pageReads++;
        const cursor = path.searchParams.get("cursor");
        if (cursor === "repair-one") {
          repairReads++;
          if (failRepair) {
            failRepair = false;
            return json({error: "Repair read failed"}, 503);
          }
          return json({threadId: "thread-1", status: "idle", latestTurnId: "turn-1",
            entries: concurrentGap ? items(300, 399) : items(301, 400),
            hasOlder: true, olderCursor: "repair-two",
            collaborationMode: mode, metadataPending});
        }
        if (cursor) olderReads++;
        if (cursor === "older-one" && holdFirstOlder) {
          await new Promise(resolve => { releaseFirstOlder = resolve; });
        }
        if (cursor === "older-one" && concurrentGap) return json({error: "Older read failed"}, 503);
        if (cursor === "older-two" && failSecond) {
          await new Promise(resolve => { releaseSecondError = resolve; });
          failSecond = false;
          return json({error: "History read failed"}, 503);
        }
        return json({threadId: "thread-1", status: "idle", latestTurnId: "turn-1",
          entries: cursor === "older-one" ? items(101, 200) : cursor === "older-two" ? items(1, 100) :
            smallPage ? items(201, 201) : newest,
          hasOlder: cursor !== "older-two", olderCursor: cursor === "older-one" ? "older-two" :
            cursor === "older-two" ? null : repairScenario ? "repair-one" : "older-one",
          collaborationMode: mode, metadataPending});
      }
      if (operation === "thread") return json({error: "Unexpected full transcript read"}, 503);
      if (operation === "reconcile") {
        if (holdQueue) {
          await new Promise(resolve => { releaseQueue = resolve; });
          holdQueue = false;
        }
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
    assert.equal(await page.locator("#load-older").count(), 0);
    assert.equal(olderReads, 0);
    await page.evaluate(() => {
      const log = document.getElementById("transcript");
      log.scrollTop = 150;
    });
    await page.waitForTimeout(150);
    assert.equal(olderReads, 0, "programmatic scroll must not load history");
    await page.locator('[data-transcript-filter="all"]').click();
    await page.locator('[data-transcript-filter="messages"]').click();
    await page.setViewportSize({width: 1270, height: 710});
    await page.waitForTimeout(50);
    await page.locator("#transcript").hover();
    await page.evaluate(() => {
      const log = document.getElementById("transcript");
      log.scrollTop = 260;
      window.pagingViewport = log.getBoundingClientRect().top;
    });
    await page.waitForTimeout(50);
    assert.equal(olderReads, 0, "filter and resize restoration must not load history");
    const wheelStart = await page.locator("#transcript").evaluate(node => ({
      top: node.scrollTop, height: node.clientHeight, content: node.scrollHeight,
    }));
    assert(wheelStart.content > wheelStart.height, "wheel fixture must be scrollable");
    assert.equal(wheelStart.top, 260, "wheel fixture must start above the top threshold");
    await page.mouse.wheel(0, -120);
    await expect.poll(() => olderReads).toBe(1);
    assert((await page.locator("#transcript").evaluate(node => node.scrollTop)) <= 200,
      "native wheel scrolling must cross the top threshold");
    await expect(page.locator("#history-status")).toHaveText("Loading earlier messages…");
    assert.equal(await page.locator("#history-controls").evaluate(node => getComputedStyle(node).justifyContent), "center");
    assert.equal(await page.locator("#transcript").evaluate(node => node.getBoundingClientRect().top),
      await page.evaluate(() => window.pagingViewport), "loading status must not move the transcript");
    await page.mouse.wheel(0, -120);
    assert.equal(olderReads, 1, "one in-flight older read is allowed");
    await page.locator("#transcript").evaluate(node => new Promise(resolve => {
      let quiet;
      const settled = () => { node.removeEventListener("scroll", onScroll); resolve(); };
      const onScroll = () => { clearTimeout(quiet); quiet = setTimeout(settled, 150); };
      node.addEventListener("scroll", onScroll, {passive: true});
      quiet = setTimeout(settled, 150);
    }));
    assert.equal(olderReads, 1, "settling the held scroll must not queue another read");
    await page.evaluate(() => {
      const log = document.getElementById("transcript");
      const anchor = [...log.querySelectorAll(".message")].find(node =>
        node.getBoundingClientRect().bottom > log.getBoundingClientRect().top);
      window.pagingAnchor = {node: anchor, top: anchor.getBoundingClientRect().top};
    });
    await page.waitForTimeout(50);
    assert.equal(await page.evaluate(() => window.pagingAnchor.node.getBoundingClientRect().top),
      await page.evaluate(() => window.pagingAnchor.top), "held loading must preserve the anchor");
    releaseFirstOlder();
    holdFirstOlder = false;
    await expect(page.locator("#transcript .message")).toHaveCount(200);
    const anchorShift = await page.evaluate(() => ({
      same: [...document.querySelectorAll("#transcript .message")].some(node => node === window.pagingAnchor.node),
      shift: Math.abs(window.pagingAnchor.node.getBoundingClientRect().top - window.pagingAnchor.top),
    }));
    assert.equal(anchorShift.same, true);
    assert(anchorShift.shift < 8, `older page moved the scroll anchor by ${anchorShift.shift}px`);
    await page.waitForTimeout(150);
    assert.equal(olderReads, 1, "prepending must not start another read");
    await page.evaluate(() => { document.getElementById("transcript").scrollTop = 0; });
    await page.waitForTimeout(150);
    assert.equal(olderReads, 1, "programmatic return to top must not load history");
    await page.mouse.wheel(0, -120);
    await expect.poll(() => olderReads).toBe(2);
    await expect(page.locator("#history-status")).toHaveText("Loading earlier messages…");
    await page.evaluate(() => {
      const log = document.getElementById("transcript");
      const anchor = [...log.querySelectorAll(".message")].find(node =>
        node.getBoundingClientRect().bottom > log.getBoundingClientRect().top);
      window.retryAnchor = {node: anchor, top: anchor.getBoundingClientRect().top};
    });
    releaseSecondError();
    await expect(page.locator("#history-status")).toContainText("Earlier messages could not be loaded");
    assert((await page.evaluate(() => Math.abs(window.retryAnchor.node.getBoundingClientRect().top -
      window.retryAnchor.top))) < 8, "error status moved the scroll anchor");
    await expect(page.locator("#repair-history")).toBeVisible();
    await page.mouse.wheel(0, -120);
    assert.equal(olderReads, 2, "failure must pause automatic reads");
    newest = items(202, 301); mode = "plan"; metadataPending = false;
    await page.evaluate(() => fetch("/fixture/refresh"));
    await expect(page.locator("#transcript .message")).toHaveCount(201);
    await expect(page.locator("#codex-mode-status")).toBeHidden();
    await expect(page.locator("#repair-history")).toBeVisible();
    assert.equal(olderReads, 2, "newest refresh must not retry a failed older page");
    await page.locator("#repair-history").click();
    await expect(page.locator("#transcript .message")).toHaveCount(301);
    assert.equal(olderReads, 3, "Retry must repeat the failed older cursor");
    assert((await page.evaluate(() => Math.abs(window.retryAnchor.node.getBoundingClientRect().top -
      window.retryAnchor.top))) < 8, "retry prepend moved the scroll anchor");
    await expect(page.locator("#repair-history")).toBeHidden();
    await page.evaluate(() => { document.getElementById("transcript").scrollTop = 0; });
    await page.mouse.wheel(0, -120);
    assert.equal(olderReads, 3, "exhausted history must not request another page");
    assert.equal(await page.evaluate(() => [...document.querySelectorAll("#transcript .message")]
      .some(node => node === window.pagingAnchor.node)), true);
    repairScenario = true; newest = items(401, 500);
    await page.evaluate(() => fetch("/fixture/refresh"));
    await expect.poll(() => repairReads).toBe(1);
    await expect(page.locator("#repair-history")).toBeHidden();
    await page.evaluate(() => {
      Object.defineProperty(document, "hidden", {configurable: true, value: true});
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await page.waitForTimeout(2500);
    assert.equal(repairReads, 1, "background tabs must pause failed automatic repair");
    await page.evaluate(() => {
      Object.defineProperty(document, "hidden", {configurable: true, value: false});
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await expect.poll(() => repairReads).toBe(2);
    await expect(page.locator("#repair-history")).toBeHidden();
    await expect(page.locator("#transcript .message")).toHaveCount(500);
    repairScenario = false;
    smallPage = true;
    await page.reload();
    await expect(page.locator("#transcript .message")).toHaveCount(1);
    assert.equal(await page.locator("#transcript").evaluate(node => node.scrollHeight <= node.clientHeight), true);
    const beforeKeyboard = olderReads;
    await page.locator("#transcript").focus();
    await page.keyboard.press("ArrowUp");
    await expect(page.locator("#transcript .message")).toHaveCount(101);
    assert.equal(olderReads, beforeKeyboard + 1, "upward key at a non-scrollable top loads history");
    await page.reload();
    await expect(page.locator("#transcript .message")).toHaveCount(1);
    const beforeTouch = olderReads;
    const touchBox = await page.locator("#transcript").boundingBox();
    const cdp = await page.context().newCDPSession(page);
    const touchX = Math.round(touchBox.x + touchBox.width / 2);
    const touchY = Math.round(touchBox.y + touchBox.height / 2);
    await cdp.send("Input.dispatchTouchEvent", {type: "touchStart", touchPoints: [{x: touchX, y: touchY, id: 1}]});
    await cdp.send("Input.dispatchTouchEvent", {type: "touchMove", touchPoints: [{x: touchX, y: touchY + 60, id: 1}]});
    await cdp.send("Input.dispatchTouchEvent", {type: "touchEnd", touchPoints: []});
    await expect(page.locator("#transcript .message")).toHaveCount(101);
    assert.equal(olderReads, beforeTouch + 1, "upward touch movement loads one page");
    await cdp.detach();
    smallPage = false; holdFirstOlder = true;
    await page.reload();
    await expect(page.locator("#transcript .message")).toHaveCount(100);
    const beforeScrollbar = olderReads;
    const scrollbar = await page.locator("#transcript").evaluate(node => {
      node.scrollTop = 400;
      const box = node.getBoundingClientRect();
      const thumbHeight = Math.max(20, node.clientHeight * node.clientHeight / node.scrollHeight);
      const fraction = node.scrollTop / (node.scrollHeight - node.clientHeight);
      return {x: box.right - 2, y: box.top + fraction * (node.clientHeight - thumbHeight) + thumbHeight / 2,
        top: box.top, gutter: node.offsetWidth - node.clientWidth};
    });
    assert(scrollbar.gutter > 0, "native scrollbar must be visible for the drag test");
    await page.mouse.move(scrollbar.x, scrollbar.y);
    await page.mouse.down();
    await page.mouse.move(scrollbar.x, scrollbar.top + 2, {steps: 8});
    await page.mouse.up();
    await expect.poll(() => page.locator("#transcript").evaluate(node => node.scrollTop)).toBeLessThanOrEqual(200);
    await expect.poll(() => olderReads).toBe(beforeScrollbar + 1);
    releaseFirstOlder();
    holdFirstOlder = false;
    await expect(page.locator("#transcript .message")).toHaveCount(200);
    concurrentGap = true; repairScenario = false; newest = items(201, 300); holdFirstOlder = true;
    await page.reload();
    await expect(page.locator("#transcript .message")).toHaveCount(100);
    const beforeGapOlder = olderReads, beforeGapRepair = repairReads;
    await page.locator("#transcript").hover();
    await page.waitForTimeout(50);
    await page.evaluate(() => { document.getElementById("transcript").scrollTop = 0; });
    await page.waitForTimeout(50);
    const gapStart = await page.locator("#transcript").evaluate(node => ({
      top: node.scrollTop, height: node.clientHeight, content: node.scrollHeight,
    }));
    assert(gapStart.height > 0 && gapStart.content > gapStart.height,
      "concurrent-gap fixture needs a visible, scrollable transcript");
    assert.equal(gapStart.top, 0, "concurrent-gap wheel must start at the settled transcript top");
    await page.mouse.wheel(0, -120);
    await expect.poll(() => olderReads).toBe(beforeGapOlder + 1);
    repairScenario = true; newest = items(400, 499);
    await page.evaluate(() => fetch("/fixture/refresh"));
    await expect(page.locator("#transcript .message")).toHaveCount(200);
    releaseFirstOlder();
    await expect(page.locator("#history-status")).toContainText("Earlier messages could not be loaded");
    await page.waitForTimeout(250);
    assert.equal(repairReads, beforeGapRepair, "queued repair must stop after the older read fails");
    assert.equal(olderReads, beforeGapOlder + 1, "failed older read must not restart automatically");
    await expect(page.locator("#repair-history")).toBeVisible();
    await page.locator("#repair-history").click();
    await expect.poll(() => repairReads).toBe(beforeGapRepair + 1);
    await expect(page.locator("#transcript .message")).toHaveCount(299);
    assert(pageReads >= 5);
    assert.deepEqual(errors, []);
    console.log("automatic paging, retry, anchor, exhaustion, keyboard and independent lanes passed");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
