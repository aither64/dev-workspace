"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const fs = require("node:fs/promises");
(async () => {
  // Chromium's download manager also needs to trust the disposable TLS server.
  const browser = await chromium.launch({headless: true, channel: "chromium", args: ["--ignore-certificate-errors"]});
  try {
    const context = await browser.newContext({ignoreHTTPSErrors: true, permissions: ["clipboard-read", "clipboard-write"], viewport: {width: 1440, height: 850}});
    const page = await context.newPage(), errors = [], dialogs = [], sends = [], additions = [];
    page.on("pageerror", error => errors.push(error.message));
    page.on("dialog", dialog => { dialogs.push(dialog.message()); void dialog.dismiss(); });
    let filesReads = 0, failFiles = false, releaseAdd, turns = 0;
    let loseResponse = true, showReceipt = false, observedEntry = null, releaseObserved = null;
    const transcript = await (await context.request.get(baseURL + "/fixture/presentation")).json();
    const teamHTML = (await (await context.request.get(baseURL + "/fixture/team")).text()).replace('<div id="team-role-defaults" hidden>', '<div id="team-role-defaults" hidden><span data-role="implementer" data-model="model-1" data-effort="high"></span>');
    await page.route("**/api/models**", route => route.fulfill({json: [{model: "model-1", displayName: "Fixture model", isDefault: true, defaultReasoningEffort: "low", supportedReasoningEfforts: [{reasoningEffort: "low"}, {reasoningEffort: "high"}]}]}));
    await page.route("**/api/collaboration-modes", route => route.fulfill({json: [{mode:"default", name:"Default"}]}));
    await page.route("**/api/sessions/example/**", async route => {
      const url = new URL(route.request().url()), operation = url.pathname.split("/").at(-1);
      if (["thread", "page"].includes(operation)) return route.fulfill({json: {...transcript, ...(showReceipt ? {entries: [...transcript.entries, observedEntry]} : {})}});
      if (operation === "details") return route.fulfill({json: {repositoriesHTML:"", teamHTML}});
      if (operation === "files") {
        filesReads++;
        if (failFiles) return route.fulfill({status:503, json:{error:"Session files could not be listed. Refresh to try again."}});
        return route.continue();
      }
      if (operation === "team" && route.request().method() === "POST") {
        additions.push(route.request().postDataJSON());
        if (additions.length === 1) await new Promise(resolve => {releaseAdd = resolve;});
        return route.fulfill({json:{ok:true}});
      }
      if (operation === "message") {
        const body = route.request().postDataJSON(); sends.push(body);
        if (body.message === "Fail before dispatch" && !body.retry) return route.fulfill({status:502, body:"fixture before dispatch"});
        if (body.message === "Observed before failure") {
          const digest = await require("node:crypto").webcrypto.subtle.digest("SHA-256", new TextEncoder().encode(body.message));
          observedEntry = {kind:"userMessage", itemId:"observed-before-failure", text:body.message, clientUserMessageId:body.clientUserMessageId, clientUserMessageDigest:Buffer.from(digest).toString("hex")};
          showReceipt = true;
          await new Promise(resolve => {releaseObserved = resolve;});
          return route.fulfill({status:502, body:"fixture response lost after transcript confirmation"});
        }
        if (!sends.slice(0, -1).some(previous => previous.clientUserMessageId === body.clientUserMessageId)) turns++;
        if (loseResponse) { loseResponse = false; return route.fulfill({status:502, body:"upstream unavailable"}); }
        return route.fulfill({json:{turnId:"turn-once", clientUserMessageId:body.clientUserMessageId}});
      }
      if (["queue", "pending"].includes(operation)) return route.fulfill({json:[]});
      if (operation === "team-stats") return route.fulfill({json:{members:[]}});
      return route.continue();
    });
    await page.goto(baseURL + "/example/");
    await expect(page.locator("#transcript h1")).toHaveText("User heading");
    await expect(page.locator("#transcript")).toContainText("fixture.txt");
    await expect(page.locator("#transcript")).not.toContainText("Generated attachment envelope");
    await expect(page.locator("#transcript")).not.toContainText("Internal work.");
    const citation = page.locator('#transcript a[href="https://example.test/source"]');
    await expect(citation).toHaveText("[1]");
    await expect(citation).toHaveAttribute("title", 'Český "title"');
    await expect(page.locator("#transcript code")).toContainText("\ue200cite\ue202ref\ue201");
    await citation.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " message ")][1]').getByRole("button", {name: "Copy message", exact: true}).click();
    assert((await page.evaluate(() => navigator.clipboard.readText())).includes("https://example.test/source"));
    await page.locator('[data-transcript-filter="team"]').click();
    await expect(page.locator("#transcript .message.team")).toContainText("lead → implementer0");
    await expect(page.locator("#transcript")).toContainText("Internal work.");
    await page.locator('[data-transcript-filter="messages"]').click();
    const link = page.locator('#transcript a').filter({hasText:"Plan"});
    await expect(link).toHaveAttribute("target", "_blank");
    await expect(link).toHaveAttribute("rel", /noopener/);
    const [popup] = await Promise.all([context.waitForEvent("page"), link.click({button:"middle"})]);
    await popup.waitForLoadState();
    await expect(popup.locator("#source-file-copy-content")).toBeEnabled();
    await popup.locator("#source-file-copy-content").click();
    const source = await (await context.request.get(baseURL + "/api/sessions/example/file?artifact=plan.md")).json();
    assert.equal(await popup.evaluate(() => navigator.clipboard.readText()), source.content.text);
    const [download] = await Promise.all([popup.waitForEvent("download"), popup.locator("#source-file-download").click()]);
    assert.equal(download.suggestedFilename(), "plan.md");
    assert.equal(await fs.readFile(await download.path(), "utf8"), source.content.text);
    await popup.close(); await page.bringToFront();

    const input = page.locator('#message-form textarea');
    await input.fill("Keep this exact input"); await page.locator("#message-send").click();
    await expect(page.locator("#message-submission-status")).toBeVisible();
    await expect(input).toHaveValue("Keep this exact input");
    await expect(page.locator("#message-receipts")).toContainText("Outcome unknown");
    await page.reload();
    await page.getByRole("button", {name:"Retry original", exact:true}).click();
    await expect.poll(() => sends.length).toBe(2);
    assert.equal(sends[0].clientUserMessageId, sends[1].clientUserMessageId); assert.equal(turns, 1);
    await expect(page.locator("#message-submission-status")).toBeHidden();
    await expect(page.getByRole("button", {name:"Dismiss message", exact:true})).toHaveText("×");
    await expect(page.getByRole("button", {name:"Dismiss message", exact:true})).toHaveAttribute("title", "Dismiss message");
    await page.getByRole("button", {name:"Dismiss message", exact:true}).click();
    const acceptedKey = await page.evaluate(() => Object.keys(sessionStorage).find(key => key.startsWith("workspace-portal.send-attempts")));
    // Dismissal changes presentation while retry identity remains in storage.
    if (acceptedKey) assert(await page.evaluate(key => sessionStorage.getItem(key), acceptedKey));
    await input.fill("Fail before dispatch"); await page.locator("#message-send").click();
    await expect(input).toHaveValue("Fail before dispatch");
    await page.getByRole("button", {name:"Retry original", exact:true}).click();
    await expect.poll(() => sends.length).toBe(4);
    assert.equal(sends[2].clientUserMessageId,sends[3].clientUserMessageId);
    await page.getByRole("button", {name:"Dismiss message", exact:true}).click();
    await input.fill("Observed before failure"); await page.locator("#message-send").click();
    await expect.poll(() => Boolean(releaseObserved)).toBe(true);
    await page.request.post(baseURL + "/fixture/refresh");
    await expect(page.locator("#transcript")).toContainText("Observed before failure");
    releaseObserved(); await expect(page.locator("#message-send")).toBeEnabled();
    await expect(page.locator("#message-receipts")).not.toContainText("Outcome unknown");
    await expect(page.locator("#message-submission-status")).toBeHidden();

    assert.equal(filesReads, 0, "Unopened Files tab must not enumerate files");
    await input.fill("Draft retained while browsing files");
    await page.evaluate(() => {location.hash = "artifacts";});
    await expect(page.locator("#files")).toBeVisible();
    await expect(page.locator(".session-file")).toHaveCount(503);
    assert.equal(filesReads, 1);
    const bottom = page.locator('.session-file a[href$="reports%2Fdeep%2F499.md"]');
    await expect(bottom).toHaveText("499.md");
    await expect(bottom).toHaveAttribute("target", "_blank");
    await expect(bottom).toHaveAttribute("rel", /noopener/);
    await expect(bottom.locator("..")).toContainText("Report 499");
    await expect(bottom.locator("..")).toContainText("Registered");
    const [fileViewer] = await Promise.all([context.waitForEvent("page"), bottom.click()]);
    await fileViewer.waitForLoadState();
    await expect(fileViewer.locator("#source-file-copy-content")).toBeEnabled();
    await fileViewer.locator("#source-file-copy-content").click();
    assert.equal(await fileViewer.evaluate(() => navigator.clipboard.readText()), "# Original artifact bytes\r\n");
    const [artifactDownload] = await Promise.all([fileViewer.waitForEvent("download"), fileViewer.locator("#source-file-download").click()]);
    assert.equal(artifactDownload.url(), baseURL + "/api/sessions/example/file/download?artifact=reports%2Fdeep%2F499.md");
    assert.equal(artifactDownload.suggestedFilename(), "499.md");
    assert.equal(await artifactDownload.failure(), null, `Artifact download failed: ${artifactDownload.url()}`);
    assert.equal(await fs.readFile(await artifactDownload.path(), "utf8"), "# Original artifact bytes\r\n");
    await fileViewer.close(); await page.bringToFront();
    await expect(input).toHaveValue("Draft retained while browsing files");
    const directory = page.locator('[data-file-directory="reports/deep"]');
    await directory.evaluate(node => {node.open = false;});
    await directory.evaluate(node => {node.open = true;});
    await bottom.scrollIntoViewIfNeeded();
    const scrollBefore = await page.locator("#files").evaluate(node => node.scrollTop);
    assert(scrollBefore > 0, "500-file tree must overflow the Files panel");
    const directoriesBefore = await page.locator("[data-file-directory]").evaluateAll(nodes => nodes.map(node => [node.dataset.fileDirectory, node.open]));
    await page.locator("#files-refresh").evaluate(node => node.click());
    await expect.poll(() => filesReads).toBe(2);
    await expect(page.locator("#files-status")).toBeHidden();
    assert.equal(await page.locator("#files").evaluate(node => node.scrollTop), scrollBefore);
    assert.deepEqual(await page.locator("[data-file-directory]").evaluateAll(nodes => nodes.map(node => [node.dataset.fileDirectory, node.open])), directoriesBefore);
    failFiles = true;
    await page.locator("#files-refresh").evaluate(node => node.click());
    await expect(page.locator("#files-status")).toContainText("Session files could not be listed");
    await expect(page.locator(".session-file")).toHaveCount(503);
    failFiles = false;
    await page.locator("#files-refresh").evaluate(node => node.click());
    await expect(page.locator("#files-status")).toBeHidden();
    await page.setViewportSize({width:390, height:844});
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.setViewportSize({width:1440, height:850});

    await page.locator('[data-session-tab="team"]').click();
    const form = page.locator('[data-direct-team-form][data-action="add"]');
    await form.locator('[name="role"]').selectOption("implementer");
    await expect(form.locator('[name="effort"]')).toHaveValue("high");
    await form.evaluate(form => {form.requestSubmit(); form.requestSubmit();});
    await expect.poll(() => additions.length).toBe(1);
    await expect(form.getByRole("button", {name:"Add member"})).toBeDisabled();
    assert.deepEqual(additions[0], {action:"add", role:"implementer", model:"model-1", reasoningEffort:"high"});
    releaseAdd();
    await expect(form.getByRole("button", {name:"Add member"})).toBeEnabled();
    await expect(form.locator('[name="effort"]')).toHaveValue("high");
    await form.getByRole("button", {name:"Add member"}).click();
    await expect.poll(() => additions.length).toBe(2); assert.deepEqual(additions[1], additions[0]);
    await expect(form.locator('[name="effort"]')).toHaveValue("high");
    assert.deepEqual(dialogs, []); assert.deepEqual(errors, []);
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exitCode = 1;});
