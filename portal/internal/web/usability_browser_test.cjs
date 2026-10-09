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
    let credits = {hasCredits: true, unlimited: false, balance: "12.5"};
    const snapshot = () => ({windows: [{windowDurationMins: 10080, usedPercent: 20, resetsAt: 1900000000},
      {windowDurationMins: 300, usedPercent: 35, resetsAt: 1900000300}],
      updatedAt: Date.now(), credits,
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
    await expect(page.locator(".limits-account-summary")).toContainText("13 credits");
    await expect(page.locator(".limits-account-summary")).toContainText("3 banked resets");
    const summaryLayout = await page.locator("[data-limits-content]").evaluate(content => {
      const summary = content.querySelector(".limits-account-summary");
      const windows = content.querySelectorAll(".limits-window");
      const summaryStyle = getComputedStyle(summary);
      return {tag: summary.tagName, lines: summary.children.length,
        gap: summary.getBoundingClientRect().top - windows[windows.length - 1].getBoundingClientRect().bottom,
        color: summaryStyle.color, metadataColor: getComputedStyle(content.querySelector(".limits-reset")).color,
        weight: summaryStyle.fontWeight};
    });
    assert.equal(summaryLayout.tag, "DIV"); assert.equal(summaryLayout.lines, 2);
    assert(summaryLayout.gap >= 12, JSON.stringify(summaryLayout));
    assert.equal(summaryLayout.color, summaryLayout.metadataColor); assert.equal(summaryLayout.weight, "400");
    const detailsButton = page.getByRole("button", {name: "Account details", exact: true});
    await expect(detailsButton).toHaveText("");
    assert.equal(await detailsButton.locator("svg").count(), 1);
    await page.getByRole("button", {name: "Account details", exact: true}).click();
    const dialog = page.locator("#codex-limits-dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("Credits: 13");
    await expect(dialog).toContainText("Expires");
    await dialog.getByRole("button", {name: "Close", exact: true}).click();
    for (const [balance, expected] of [["1234.567890123456", "1,235"], [null, "Available"]]) {
      credits = {hasCredits: true, unlimited: false, balance};
      await page.reload();
      await expect(page.locator(".limits-account-summary")).toContainText(balance == null ? "Credits available" : `${expected} credits`);
      await detailsButton.click(); await expect(dialog).toContainText(`Credits: ${expected}`);
      await dialog.getByRole("button", {name: "Close", exact: true}).click();
    }
    credits = {hasCredits: true, unlimited: true};
    await page.reload(); await expect(page.locator(".limits-account-summary")).toContainText("Unlimited credits");
    await detailsButton.click(); await expect(dialog).toContainText("Unlimited credits");
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

    // The index uses the same upload handler before a session exists. Uploads
    // reach only the disposable server; every session creation POST is mocked.
    let creationBody, releaseChunk;
    const draftWrites = [];
    context.on("request", request => {
      if (new URL(request.url()).pathname.startsWith("/uploads/d-") && request.method() !== "GET") {
        draftWrites.push(request.method());
      }
    });
    await page.route("**/sessions", route => {
      creationBody = route.request().postData();
      return route.fulfill({status: 503, json: {error: "Fixture creation remains pending"}});
    });
    await page.route("**/api/session-creations/*", route => route.fulfill({status: 404, json: {error: "Not found"}}));
    await page.route("**/uploads/d-*/**", async route => {
      if (route.request().method() === "PATCH" && !releaseChunk) {
        await new Promise(resolve => { releaseChunk = resolve; });
      }
      await route.continue();
    });
    await page.goto(baseURL + "/");
    const creationForm = page.locator("#new-session-form");
    const goal = creationForm.locator("[name=goal]");
    const createButton = creationForm.getByRole("button", {name: "Create session", exact: true});
    const creationFiles = page.locator("#creation-uploads");
    await expect(creationForm.getByRole("button", {name: "Add attachments"})).toBeEnabled();
    await goal.fill("Existing text ");
    await page.evaluate(() => navigator.clipboard.writeText("initial clipboard text"));
    await goal.focus(); await page.keyboard.press("Control+End"); await page.keyboard.press("Control+V");
    await expect(goal).toHaveValue("Existing text initial clipboard text");
    await page.keyboard.press("Control+Z");
    await expect(goal).toHaveValue("Existing text ");
    assert.equal(await goal.evaluate(field => {
      const data = new DataTransfer();
      data.items.add(new File([new Uint8Array([137, 80, 78, 71])], "initial.png", {type: "image/png"}));
      return field.dispatchEvent(new ClipboardEvent("paste", {clipboardData: data, bubbles: true, cancelable: true}));
    }), false, "initial binary paste is handled as files");
    await expect(creationFiles.locator(".codex-attachment")).toHaveCount(1);
    await expect(creationFiles).toContainText("initial.png");
    await expect.poll(() => Boolean(releaseChunk)).toBe(true);
    await expect(createButton).toBeDisabled();
    await expect(goal).toHaveValue("Existing text ");
    releaseChunk();
    await expect(creationFiles).toContainText("Ready");
    assert.equal(await goal.evaluate(field => {
      const data = new DataTransfer(); data.setData("text/plain", "mixed initial text");
      data.items.add(new File(["initial binary file"], "initial.bin", {type: "application/octet-stream"}));
      return field.dispatchEvent(new ClipboardEvent("paste", {clipboardData: data, bubbles: true, cancelable: true}));
    }), true, "initial mixed paste preserves native text handling");
    await expect(creationFiles.locator(".codex-attachment")).toHaveCount(2);
    await expect(creationFiles.locator(".codex-attachment-detail")).toHaveText([/Ready/, /Ready/]);
    await goal.fill("");
    assert.equal(await goal.getAttribute("required"), null, "file-only initial requests are valid");
    await expect(createButton).toBeEnabled();
    const scope = await creationForm.locator("[name=uploadScope]").inputValue();
    const selectedIDs = await page.evaluate(scope => JSON.parse(
      sessionStorage.getItem(`workspace-portal.upload-draft.${scope}`)).map(file => file.id), scope);
    assert.equal(selectedIDs.length, 2); assert.equal(new Set(selectedIDs).size, 2);
    await page.reload();
    await expect(creationFiles.locator(".codex-attachment")).toHaveCount(2);
    await expect(creationFiles.locator(".codex-attachment-detail")).toHaveText([/Ready/, /Ready/]);
    await expect(goal).toHaveValue("");
    await expect(createButton).toBeEnabled();
    await expect(creationForm.locator("[name=uploadScope]")).toHaveValue(scope);
    await createButton.click();
    await expect.poll(() => Boolean(creationBody)).toBe(true);
    const frozenCreationBody = creationBody;
    const submitted = new URLSearchParams(frozenCreationBody);
    assert.equal(submitted.get("goal"), ""); assert.equal(submitted.get("uploadScope"), scope);
    assert.deepEqual(submitted.getAll("attachmentIds"), selectedIDs);
    await expect(page.locator("#new-session-progress")).toHaveText("Fixture creation remains pending");
    await expect(goal).toBeDisabled(); await expect(createButton).toBeDisabled();
    const writesBeforeLockedPaste = draftWrites.length;
    await goal.evaluate(field => {
      const data = new DataTransfer(); data.items.add(new File(["locked"], "locked.bin"));
      field.dispatchEvent(new ClipboardEvent("paste", {clipboardData: data, bubbles: true, cancelable: true}));
    });
    await expect(creationFiles.locator(".codex-attachment")).toHaveCount(2);
    await page.reload();
    await expect(page.locator("#new-session-recovery")).toBeVisible();
    await expect(goal).toBeDisabled(); await expect(createButton).toBeDisabled();
    assert.equal(await page.evaluate(() => JSON.parse(sessionStorage.getItem(
      "workspace-portal.creation-draft")).body), frozenCreationBody);
    assert.equal(creationBody, frozenCreationBody, "recovery reuses the exact submitted body");
    assert.equal(draftWrites.length, writesBeforeLockedPaste, "locked recovery cannot upload more files");
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
