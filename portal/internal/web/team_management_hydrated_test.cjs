"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const input = JSON.parse(process.argv[2]);

(async () => {
  for (const [engine, type] of Object.entries({chromium, firefox})) {
    const browser = await type.launch({headless: true});
    try {
      const page = await browser.newPage();
      const errors = [], writes = [], settings = [];
      let editing = "", teamReads = 0;
      const idleSince = Date.now() - 120000;
      const models = [{model: "model", displayName: "Model", isDefault: true,
        defaultReasoningEffort: "high", supportedReasoningEfforts: [
          {reasoningEffort: "high"}, {reasoningEffort: "low"}]}];
      page.on("pageerror", error => errors.push(error.stack));
      // Reuse the browser fixtures' notification-only EventSource port. HTTP
      // reads, rows and settings use the production page and module.
      await page.addInitScript(() => {
        window.EventSource = class {
          constructor() { queueMicrotask(() => this.onopen?.()); }
          addEventListener() {}
          removeEventListener() {}
          close() {}
        };
      });
      await page.route("**/*", async route => {
        const request = route.request(), url = new URL(request.url()), path = url.pathname;
        const json = value => route.fulfill({json: value});
        if (path.startsWith("/static/") || path.startsWith("/codex/assets/") || path === "/fixture/team-page") return route.continue();
        const operation = path.split("/").at(-1);
        if (operation === "team" && request.method() !== "GET") {
          writes.push(request.postDataJSON());
          if (input.scopes.lead) {
            assert.equal(request.headers()["x-workspace-conversation-scope"], input.scopes.lead);
            assert.equal(request.headers()["x-workspace-pi-generation"], input.generation);
          }
          return json({ok: true});
        }
        if (path.startsWith("/codex/conversations/")) {
          const address = decodeURIComponent(path.split("/")[3]).endsWith("~reviewer0") ? "reviewer0" : "lead";
          if (input.scopes[address] && editing === address) {
            assert.equal(request.headers()["x-workspace-conversation-scope"], input.scopes[address]);
            assert.equal(request.headers()["x-workspace-pi-generation"], input.generation);
          }
          if (operation === "settings" && request.method() === "POST") {
            settings.push(request.postDataJSON());
            return json(settings.at(-1));
          }
        }
        if (["thread", "page"].includes(operation)) return json({threadId: input.nativeId || "root",
          status: "idle", model: "model", reasoningEffort: "high", entries: [], hasOlder: false});
        if (operation === "models") return json(models);
        if (operation === "details") return json({repositoriesHTML: "", artifactsHTML: "",
          sessionBackend: input.kind === "codex" ? "codex" : "pi", conversationScope: input.scopes.lead || "",
          conversationInteractive: true, conversationHeld: false, readyMembers: input.hasMember ? ["reviewer0"] : []});
        if (operation === "pending" || operation === "queue") return json([]);
        if (operation === "reconcile") return json({ok: true});
        if (operation === "activity") return json({currentState: "idle", idleMs: Date.now() - idleSince, observedAtMs: Date.now(), stateSinceMs: idleSince, coverageComplete: false});
        if (operation === "collaboration-modes") return json([{mode: "default", name: "Default"}]);
        if (operation === "pi-questions") return json({revision: 0, questions: []});
        if (operation === "pi-plan") return json({revision: 0, proposals: [], acceptances: []});
        if (operation === "mode") return json({mode: "default", revision: 0, pending: false});
        if (operation === "operation") return json({state: "idle", options: {}});
        if (operation === "progress") return json({available: false});
        if (operation === "retry-policy") return json({revision: "fixture", capacity: false, cybersecurity: false});
        if (operation === "auto-archive") return json({enabled: false, hold: true, blockers: []});
        if (operation === "communication") return json({candidates: [], grants: [], messages: []});
        if (operation === "coordination") return json({workspaces: [], reservationHolders: [], jobs: []});
        if (operation === "attention") return json({version: 1, identity: "", rootThread: input.kind === "codex" ? "root" : "", enabled: false, revision: 0, event: null, fresh: true});
        if (operation === "provider") return json({threadId: input.nativeId, generation: input.generation,
          providerId: "fixture", providerLabel: "Fixture", usage: {state: "unavailable", reason: "usage_unavailable"}});
        if (operation === "codex-providers") return json({providers: [], inherited: true});
        if (operation === "codex-limits") return json({windows: [], canReset: false});
        if (operation === "browser-notifications") return json({version: 1, deliveryEpoch: 1, available: false});
        if (operation === "team-stats") {
          teamReads++;
          if (input.scopes.lead) {
            assert.equal(request.headers()["x-workspace-conversation-scope"], input.scopes.lead);
            assert.equal(request.headers()["x-workspace-pi-generation"], input.generation);
          }
          return json({members: Object.entries(input.threads).filter(([, thread]) => thread).map(([address, threadId]) => ({address, state: "ready",
            snapshot: {threadId, currentState: "idle", observedAtMs: Date.now(), stateSinceMs: idleSince,
              sentMessages: address === "lead" ? 3 : 0, receivedMessages: 2, totalToolCalls: 7,
              workingMs: 20000, waitingMs: 4000, idleMs: 120000, countsComplete: true, coverageComplete: false}}))});
        }
        return json({});
      });
      await page.goto(input.origin + "/fixture/team-page#team");
      await expect(page.locator("#team .team-table").first()).toBeVisible();
      await page.locator('[data-session-tab="team"]').click();
      const leadRow = page.locator('[data-team-stats="lead"]');
      await expect(leadRow.locator('[data-team-metric="sent"]')).toHaveText("3");
      await expect(leadRow.locator('[data-team-metric="received"]')).toHaveText("2");
      await expect(leadRow.locator('[data-team-metric="tools"]')).toHaveText("7");
      await expect(leadRow.locator('[data-team-metric="working"]')).toHaveText("20s");
      await expect(leadRow.locator('[data-team-metric="waiting"]')).toHaveText("4s");
      await expect(leadRow.locator('[data-team-metric="idle"]')).toHaveText("2m 00s");
      await expect(leadRow.locator(".team-coverage")).toBeVisible();
      if (input.hasMember) await expect(page.locator('[data-team-stats="reviewer0"] [data-team-metric="sent"]')).toHaveText("0");
      await page.locator('[data-session-tab="codex"]').click();
      await expect(page.locator("#codex-work-elapsed")).toContainText("2m");
      await page.locator('[data-session-tab="team"]').click();
      await page.locator('[data-session-tab="codex"]').click();
      await expect(page.locator("#codex-work-elapsed")).toContainText("2m");
      await page.reload();
      await expect(page.locator("#codex-work-elapsed")).toContainText("2m");
      await page.locator('[data-session-tab="team"]').click();
      assert(teamReads > 0); assert.deepEqual(writes, []); assert.deepEqual(settings, []);

      assert.equal(await page.locator('[data-direct-team-form][data-action="assign"]').count(), 0);
      assert.equal(await page.locator("#team th").filter({hasText: "Actions"}).count() > 0, true);
      const form = page.locator('[data-direct-team-form][data-action="add"]');
      await expect(form).toBeVisible();
      await expect(form.locator('[name="model"]')).toBeEnabled();
      await form.locator('[name="model"]').selectOption("model");
      await form.locator('[name="effort"]').selectOption("low");
      await form.locator('button[type="submit"]').click();
      await expect.poll(() => writes.length).toBe(1);
      assert.equal(writes[0].reasoningEffort, "low");
      await expect(form.locator('[name="effort"]')).toHaveValue("low");
      if (input.hasMember) {
        editing = "reviewer0";
        await page.locator('[data-team-settings="reviewer0"]').click();
        await expect(page.locator("#team-model")).toBeEnabled();
        await page.locator("#team-effort").selectOption("low");
        await page.locator("#team-settings-save").click();
        await expect.poll(() => settings.length).toBe(1);
        assert.equal(settings[0].reasoningEffort, "low");
        page.once("dialog", dialog => dialog.accept());
        await page.locator('[data-team-remove="reviewer0"]').click();
        await expect.poll(() => writes.some(write => write.action === "remove")).toBe(true);
      }
      assert.deepEqual(errors, []);
      console.log(`${engine} ${input.kind} shared Team PASS`);
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
