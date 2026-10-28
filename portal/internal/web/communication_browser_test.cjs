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
      await page.route(/\/api\/sessions\/example\/thread(?:\/page)?(?:\?.*)?$/, route => route.fulfill({json: {
        threadId: "thread-1", status: "idle", latestTurnId: "turn-1", collaborationMode: "default", hasOlder: false,
        entries: [
          {kind: "userMessage", itemId: "ordinary", text: "Ordinary request"},
          {kind: "userMessage", itemId: "peer-message", text: "Shared session result", communication: {kind: "session", from: "peer", to: "example"}},
          {kind: "mcpToolCall", itemId: "peer-send", summary: "Send to peer", communication: {kind: "session"}},
          {kind: "userMessage", itemId: "team-message", text: "Internal team result", communication: {kind: "team", from: "lead", to: "implementer0"}},
        ],
      }}));
      await page.goto(process.argv[2] + "/example/");
      const transcript = page.locator("#transcript");
      await expect(transcript).toContainText("Ordinary request");
      await expect(transcript).not.toContainText("Shared session result");
      await page.locator('[data-transcript-filter="sessions"]').click();
      await expect(transcript).toContainText("Shared session result");
      await expect(transcript).toContainText("Send to peer");
      await expect(transcript).not.toContainText("Ordinary request");
      await expect(transcript).not.toContainText("Internal team result");
      await expect(transcript.locator(".message.session")).toHaveCount(2);
      await expect(transcript.locator(".message-session-label").first()).toHaveText("Session · peer → example");
      const sessionColor = await transcript.locator(".message.session").first().evaluate(node => getComputedStyle(node).backgroundColor);
      await page.locator('[data-transcript-filter="all"]').click();
      await expect(transcript).toContainText("Ordinary request");
      const teamColor = await transcript.locator(".message.team").evaluate(node => getComputedStyle(node).backgroundColor);
      assert.notEqual(sessionColor, teamColor);
      await page.locator('[data-transcript-filter="team"]').click();
      await expect(transcript).toContainText("Internal team result");
      await expect(transcript).not.toContainText("Shared session result");
      await expect(page.locator('[data-session-tab="communication"]')).toHaveCount(0);
      await page.locator('[data-session-tab="settings"]').click();
      await expect(page.locator("#settings-general")).toBeVisible();
      await page.locator('[data-settings-tab="general"]').click();
      await page.locator('[data-settings-tab="automation"]').click();
      await expect(page.locator("#auto-archive-panel")).toBeVisible();
      await expect(page.locator("#settings-general")).toBeHidden();
      await expect(page.locator(".settings-tabs")).toBeInViewport({ratio: 1});
      await page.goBack();
      await expect(page.locator("#settings-general")).toBeVisible();
      await page.goForward();
      await expect(page.locator("#settings-automation")).toBeVisible();
      await expect(page.locator(".settings-tabs")).toBeInViewport({ratio: 1});
      await page.locator('[data-settings-tab="automation"]').press("ArrowRight");
      await expect(page.locator('[data-settings-tab="notifications"]')).toBeFocused();
      await expect(page.locator("#session-notifications-enabled")).toBeVisible();
      await page.locator('[data-settings-tab="communication"]').click();
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
      const gap = await panel.locator(".communication-form").evaluate(node => parseFloat(getComputedStyle(node).gap));
      assert(gap >= 12, "selector and submit need visible spacing");
      await page.locator('[data-session-tab="codex"]').click();
      await page.evaluate(() => {location.hash = "communication";});
      await expect(panel).toBeVisible();
      await expect(page.locator('[data-session-tab="settings"]')).toHaveAttribute("aria-selected", "true");
      assert.deepEqual(errors, []);
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
