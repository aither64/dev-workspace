"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const checkSidebarMenu = async page => {
  const toggle = page.getByRole("button", {name: "Workspace menu", exact: true});
  const menu = page.locator("#sidebar-menu");
  await toggle.focus();
  await page.keyboard.press("Enter");
  await expect(menu).toBeVisible();
  await expect(menu.getByRole("link", {name: "Automatic archival", exact: true})).toBeFocused();
  await expect.poll(async () => {
    const button = await toggle.boundingBox(), bounds = await menu.boundingBox(), viewport = page.viewportSize();
    return bounds.x >= 0 && bounds.x + bounds.width <= viewport.width &&
      bounds.y >= button.y + button.height && bounds.y + bounds.height <= viewport.height;
  }).toBe(true);
  assert(await menu.evaluate(element => {
    const bounds = element.getBoundingClientRect();
    return element.contains(document.elementFromPoint(bounds.right - 10, bounds.top + 15));
  }), "popover is not clipped by the sidebar");
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(toggle).toBeFocused();
  await toggle.click();
  await expect(menu).toBeVisible();
  await page.locator("main").click({position: {x: 5, y: 5}});
  await expect(menu).toBeHidden();
};
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium", args: ["--ignore-certificate-errors"]} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const errors = [], notices = [];
      let details = 0, activity = 0, failActivity = false, holdDetails = false, releaseDetails;
      page.on("pageerror", error => errors.push(error.message));
      await page.exposeFunction("recordNotice", value => notices.push(value));
      await page.addInitScript(() => {
        document.addEventListener("DOMContentLoaded", () => {
          const warning = document.getElementById("session-details-warning");
          if (warning) new MutationObserver(() => {
            if (!warning.hidden) window.recordNotice(warning.textContent);
          }).observe(warning, {attributes: true, childList: true, subtree: true});
        });
      });
      await page.route("**/api/sessions/example/**", async route => {
        const operation = new URL(route.request().url()).pathname.split("/").at(-1);
        if (operation === "details") {
          details++;
          if (holdDetails) {
            holdDetails = false;
            await new Promise(resolve => { releaseDetails = resolve; });
            return route.abort("failed").catch(() => {});
          }
        }
        if (operation === "activity") {
          activity++;
          if (failActivity) return route.fulfill({status: 503, body: "Temporary read failure"});
          return route.fulfill({contentType: "application/json", body: JSON.stringify({
            currentState: "working", workingMs: 30000, waitingMs: 10000,
            startedAtMs: Date.now() - 30000, observedAtMs: Date.now(), coverageComplete: true,
          })});
        }
        return route.continue();
      });
      await page.route("**/fixture/next", async route => {
        releaseDetails?.();
        await new Promise(resolve => setTimeout(resolve, 250));
        return route.fulfill({contentType: "text/html", body: "<p>Next page</p>"});
      });
      await page.goto(baseURL + "/example/");
      await expect(page.locator("#codex-duration")).toContainText("Working 30s");
      await page.setViewportSize({width: 420, height: 720});
      await checkSidebarMenu(page);
      await page.setViewportSize({width: 1280, height: 720});
      // Use the real artifact control and its download attribute.
      assert.equal(await page.locator("#artifact-download").getAttribute("download"), "");
      await page.evaluate(() => {
        const link = document.getElementById("artifact-download");
        link.href = "/fixture/download"; link.hidden = false;
      });
      const downloaded = page.waitForEvent("download");
      await page.locator("#artifact-download").evaluate(link => link.click());
      await (await downloaded).delete();
      const beforeDownload = details;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect.poll(() => details).toBeGreaterThan(beforeDownload);

      // A hidden document retains the last display while it obtains fresh timing.
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: true});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      failActivity = true;
      const beforeWake = activity;
      await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {configurable: true, value: false});
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect.poll(() => activity).toBeGreaterThan(beforeWake);
      await page.waitForTimeout(1500);
      await expect(page.locator("#codex-work-elapsed")).not.toContainText("unavailable");
      failActivity = false;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(page.locator("#codex-duration")).toContainText("Working 30s");

      // Firefox can reject an in-flight read while the next document is loading.
      holdDetails = true;
      // A preceding focus refresh may still be in flight and coalesce this
      // event. Ask again once it finishes so the held read is actually started.
      await expect.poll(async () => {
        await page.evaluate(() => dispatchEvent(new Event("focus")));
        return Boolean(releaseDetails);
      }).toBe(true);
      await page.evaluate(() => {
        const link = document.createElement("a"); link.id = "fixture-next";
        link.href = "/fixture/next"; link.textContent = "Next page"; document.body.append(link);
      });
      await page.locator("#fixture-next").click();
      await page.waitForURL("**/fixture/next");
      const beforeBack = details;
      await page.goBack();
      await expect.poll(() => details).toBeGreaterThan(beforeBack);
      await expect(page.locator("#session-details-warning")).toBeHidden();
      await page.goto(baseURL + "/");
      await checkSidebarMenu(page);
      await page.getByRole("button", {name: "Workspace menu", exact: true}).click();
      await page.locator('#sidebar-menu a[href="/automatic-archival"]').click();
      await page.waitForURL("**/automatic-archival");
      assert.deepEqual(notices, []);
      assert.deepEqual(errors, []);
      console.log(engine.name() + ": sidebar menu, download, timing wake and navigation restoration passed");
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
