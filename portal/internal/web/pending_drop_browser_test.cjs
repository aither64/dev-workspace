"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 390, height: 844}});
      const sends = [], errors = [];
      const original = "653332e7-a404-46cf-a8d3-743730a3dd61", later = "653332e7-a404-46cf-a8d3-743730a3dd62";
      page.on("pageerror", error => errors.push(error.message));
      await page.addInitScript(({original, later}) => {
        if (sessionStorage.getItem("drop-fixture")) return;
        const entries = [
          {id: original, message: "Unknown original", action: "send", state: "unknown", attachmentIds: []},
          {id: later, message: "Later message", action: "send", state: "waiting", attachmentIds: []},
        ];
        for (const entry of entries) sessionStorage.setItem(`workspace-portal.send-attempt.example.thread-1.${entry.id}`, JSON.stringify(entry));
        sessionStorage.setItem("workspace-portal.message-dispatch.example.thread-1", JSON.stringify({schema: 1, entries}));
        sessionStorage.setItem("drop-fixture", "1");
      }, {original, later});
      await page.route(/\/(?:api\/sessions|codex\/conversations)\/example\/(?:thread(?:\/page)?|message)(?:\?.*)?$/, async route => {
        if (route.request().method() === "POST") {
          const body = route.request().postDataJSON(); sends.push(body);
          await route.fulfill({json: {clientUserMessageId: body.clientUserMessageId, turnId: "fixture-turn"}});
        } else await route.fulfill({json: {threadId: "thread-1", status: "idle", collaborationMode: "default", entries: [], hasOlder: false}});
      });
      await page.goto(process.argv[2] + "/example/");
      const receipts = page.locator("#message-receipts");
      const unknown = receipts.locator(".message-receipt").filter({hasText: "Unknown original"});
      await expect(unknown).toContainText("Outcome unknown");
      const prompt = page.locator('#message-form textarea[name="message"]');
      await prompt.fill("Keep my draft");
      page.once("dialog", dialog => {assert.match(dialog.message(), /may already have received/); void dialog.dismiss();});
      await unknown.getByRole("button", {name: "Drop message"}).click();
      await expect(unknown).toBeVisible();
      page.once("dialog", dialog => void dialog.accept());
      await unknown.getByRole("button", {name: "Drop message"}).click();
      await expect(unknown).toHaveCount(0);
      await expect(prompt).toHaveValue("Keep my draft");
      assert.equal(sends.length, 0);
      await page.reload();
      await expect(unknown).toHaveCount(0);
      await expect(receipts).toContainText("Later message");
      await receipts.getByRole("button", {name: "Resume sending"}).click();
      await expect.poll(() => sends.length).toBe(1);
      assert.equal(sends[0].clientUserMessageId, later);
      assert.equal(sends[0].message, "Later message");
      assert.deepEqual(errors, []);
    } finally {await browser.close();}
  }
})().catch(error => {console.error(error); process.exitCode = 1;});
