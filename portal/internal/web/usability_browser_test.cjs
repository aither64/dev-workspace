"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];

(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    const context = await browser.newContext({ignoreHTTPSErrors: true,
      permissions: ["clipboard-read", "clipboard-write"]});
    const page = await context.newPage(), errors = [], attempts = [];
    page.on("pageerror", error => errors.push(error.message));
    const accountScope = "a".repeat(64);
    let count = 3, consumed = false;
    const snapshot = () => ({windows: [{windowDurationMins: 10080, usedPercent: 20, resetsAt: 1900000000},
      {windowDurationMins: 300, usedPercent: 35, resetsAt: 1900000300}],
      updatedAt: Date.now(), credits: {hasCredits: true, unlimited: false, balance: "12.5"},
      accountScope, canReset: true, rateLimitResetCredits: {availableCount: count, credits: [
        {id: "reset-1", title: "Available fixture reset", resetType: "codexRateLimits", status: consumed ? "redeemed" : "available", expiresAt: Math.floor(Date.now() / 1000) + 86400},
        {id: "reset-2", title: "Expired fixture reset", resetType: "codexRateLimits", status: "available", expiresAt: 1},
        {id: "reset-3", title: "Unknown fixture reset", resetType: "unknown", status: "available", expiresAt: null},
      ]}});
    // Every account read and mutation is intercepted. This fixture never uses a live reset.
    await context.route("**/api/codex-limits", route => route.fulfill({json: snapshot()}));
    await context.route("**/api/codex-limits/reset", route => {
      attempts.push(route.request().postDataJSON());
      if (!consumed) {
        consumed = true; count--;
        return route.fulfill({status: 503, json: {error: "Fixture lost acknowledgement"}});
      }
      return route.fulfill({json: {outcome: "alreadyRedeemed"}});
    });
    await page.goto(baseURL + "/example/");
    await expect(page.locator(".limits-account-summary")).toContainText("12.5");
    await expect(page.locator(".limits-account-summary")).toContainText("3 banked resets");
    await page.getByRole("button", {name: "Account details", exact: true}).click();
    const dialog = page.locator("#codex-limits-dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("Expires");
    await page.setViewportSize({width:390, height:844});
    await expect(dialog.locator('[data-limit-duration="300"]')).toContainText('65% left');
    await expect(dialog.locator('[data-limit-duration="10080"]')).toContainText('80% left');
    await expect(dialog.locator('time')).toHaveCount(2);
    await dialog.getByRole("button", {name:"Close", exact:true}).click();
    await page.locator('.codex-limits-toggle').click();
    await expect(dialog.locator('[data-limit-duration="300"]')).toBeVisible();
    await page.setViewportSize({width:1280, height:900});
    await expect(dialog.getByRole("button", {name: "Use reset…"}).nth(0)).toBeDisabled();
    const available = dialog.locator(".reset-credit").filter({hasText: "Available fixture reset"}).getByRole("button");
    // Two already-open pages share the native lock and durable pending record.
    const peer = await context.newPage();
    await peer.goto(baseURL + "/example/");
    await expect(peer.locator(".limits-account-summary")).toContainText("3 banked resets");
    await peer.evaluate(() => new Promise(resolve => {
      navigator.locks.request(`workspace-portal.reset-attempt.${location.origin}`, async () => {
        resolve(); await new Promise(release => {window.releaseResetLock = release;});
      });
    }));
    await page.bringToFront();
    await available.click();
    await expect(page.locator("[data-reset-status]")).toHaveText("A reset attempt is already running in another tab.");
    assert.equal(attempts.length,0);
    await peer.evaluate(() => window.releaseResetLock());
    await Promise.all([page.waitForEvent("dialog").then(prompt => prompt.dismiss()), available.click()]);
    await expect(available).toBeEnabled(); assert.equal(attempts.length, 0);
    await Promise.all([page.waitForEvent("dialog").then(prompt => prompt.accept()), available.click()]);
    await expect(page.locator("[data-reset-status]")).toHaveText("Fixture lost acknowledgement");
    assert.equal(attempts.length, 1);
    await peer.bringToFront();
    await peer.getByRole("button", {name:"Account details", exact:true}).click();
    await expect(peer.getByRole("button", {name:"Retry saved attempt", exact:true})).toBeEnabled();
    for (const button of await peer.locator("#codex-limits-dialog").getByRole("button", {name:"Use reset…"}).all()) await expect(button).toBeDisabled();
    await peer.close(); await page.bringToFront();
    await page.reload(); await page.getByRole("button", {name: "Account details", exact: true}).click();
    await expect(dialog.getByRole("button", {name: "Retry saved attempt"})).toBeEnabled();
    await dialog.getByRole("button", {name: "Retry saved attempt"}).click();
    await expect(page.locator("[data-reset-status]")).toHaveText("This reset attempt already completed.");
    assert.equal(attempts.length, 2); assert.deepEqual(attempts[1], attempts[0]);
    assert.equal(attempts[0].creditId, "reset-1");
    await expect(page.locator(".limits-account-summary")).toContainText("2 banked resets");
    await dialog.getByRole("button", {name: "Close", exact: true}).click();

    const input = page.locator("#message-form textarea");
    await input.fill("Existing text ");
    await page.evaluate(() => navigator.clipboard.writeText("native clipboard text"));
    await input.focus(); await page.keyboard.press("Control+End"); await page.keyboard.press("Control+V");
    await expect(input).toHaveValue("Existing text native clipboard text");
    assert.equal(await page.evaluate(() => {
      const data = new DataTransfer();
      data.items.add(new File([new Uint8Array([137, 80, 78, 71])], "clipboard.png", {type: "image/png"}));
      return document.querySelector("#message-form textarea").dispatchEvent(new ClipboardEvent("paste", {clipboardData: data, bubbles: true, cancelable: true}));
    }), false, "binary paste is handled as files");
    await expect(page.locator("#message-uploads")).toContainText("clipboard.png");
    await expect(page.locator("#message-uploads")).toContainText("1 file");
    await expect(input).toHaveValue("Existing text native clipboard text");
    assert.equal(await page.evaluate(() => {
      const data = new DataTransfer(); data.setData("text/plain", "mixed native text");
      data.items.add(new File(["binary file"], "clipboard.bin", {type: "application/octet-stream"}));
      return document.querySelector("#message-form textarea").dispatchEvent(new ClipboardEvent("paste", {clipboardData: data, bubbles: true, cancelable: true}));
    }), true, "mixed paste leaves native text handling enabled");
    await expect(page.locator("#message-uploads")).toContainText("clipboard.bin");
    await expect(page.locator("#message-uploads")).toContainText("2 files");
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
