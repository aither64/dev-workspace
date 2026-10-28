"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true});
      const mutations = [], errors = [];
      let grants = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.route("**/api/sessions/example/communication", async route => {
        const request = route.request();
        if (request.method() === "POST") {
          const body = request.postDataJSON(); mutations.push(body);
          if (body.action === "allow") grants = [{id: "grant", a: {slug: "example"}, b: {slug: "peer"}, approvedBy: "example", approval: "User approved this pair in the portal"}];
          else grants[0].revokedAt = "2026-10-28T12:00:00Z";
        }
        await route.fulfill({json: {candidates: ["peer"], grants, messages: [{id: "id", from: {slug: "peer"}, to: {slug: "example"}, status: "held", detail: "Recipient is stopped", createdAt: "2026-10-28T12:00:00Z", text: '<img src=x onerror="window.peerExecuted=true">'}]}});
      });
      await page.goto(process.argv[2] + "/example/");
      await page.locator('[data-session-tab="communication"]').click();
      const panel = page.locator("#communication");
      await expect(panel).toContainText("No sessions have permission");
      await panel.locator("select").selectOption("peer");
      await panel.locator("[data-communication-allow]").click();
      await expect(panel).toContainText("Communication allowed in both directions");
      await panel.getByRole("button", {name: "Revoke access"}).click();
      await expect(panel).toContainText("Closed:");
      assert.deepEqual(mutations, [{action: "allow", peer: "peer"}, {action: "revoke", peer: "peer"}]);
      await expect(panel.locator("pre")).toContainText("<img src=x");
      assert.equal(await page.evaluate(() => window.peerExecuted), undefined);
      assert.deepEqual(errors, []);
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
