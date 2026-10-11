"use strict";

const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];

(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true});
    // Deliver sync hints synchronously so the fake clock owns debounce timing.
    await page.addInitScript(() => {
      window.EventSource = class {
        constructor() { window.modeRefreshEvents = this; queueMicrotask(() => this.onopen?.()); }
        addEventListener() {}
        close() {}
      };
      const digest = crypto.subtle.digest.bind(crypto.subtle);
      crypto.subtle.digest = async (algorithm, data) => {
        const result = await digest(algorithm, data);
        if (window.holdNextPlanDigest && new TextDecoder().decode(data) === "A completed plan") {
          window.holdNextPlanDigest = false;
          return new Promise(resolve => { window.releasePlanDigest = () => resolve(result); });
        }
        return result;
      };
    });
    const errors = [], writes = [];
    page.on("pageerror", error => errors.push(error.message));
    let threadId = "thread-1", mode = "", metadataPending = true, reads = 0;
    let holdRead = false, releaseRead = null;
    let holdSave = false, releaseSave = null;
    const pair = {model: "model-1", reasoningEffort: "medium"};
    await page.route("**/api/models", route => route.fulfill({json: [{
      model: "model-1", displayName: "Model 1", isDefault: true,
      defaultReasoningEffort: "medium", supportedReasoningEfforts: [{reasoningEffort: "medium"}],
    }]}));
    await page.route("**/api/collaboration-modes", route => route.fulfill({json: [
      {mode: "default", name: "Default"}, {mode: "plan", name: "Plan"},
    ]}));
    await page.route("**/api/sessions/example/**", async route => {
      const operation = new URL(route.request().url()).pathname.split("/").at(-1);
      if (operation === "thread" || operation === "page") {
        const number = ++reads;
        const snapshot = {threadId, collaborationMode: mode, metadataPending,
          ...pair, status: "idle", latestTurnId: "turn-1", hasOlder: false, entries: [
            {turnId: "turn-1", itemId: "plan-1", kind: "plan", turnStatus: "completed", text: "A completed plan"},
            {turnId: "turn-1", itemId: "marker", kind: "agentMessage", text: `Read ${number}`},
          ]};
        if (holdRead) {
          holdRead = false;
          await new Promise(resolve => { releaseRead = resolve; });
        }
        return route.fulfill({json: snapshot});
      }
      if (operation === "settings" && route.request().method() === "POST") {
        const settings = route.request().postDataJSON();
        writes.push(settings);
        if (holdSave) {
          holdSave = false;
          await new Promise(resolve => { releaseSave = resolve; });
        }
        return route.fulfill({json: {...pair, collaborationMode: settings.collaborationMode}});
      }
      if (operation === "pending" || operation === "queue") return route.fulfill({json: []});
      if (operation === "reconcile") return route.fulfill({json: {ok: true}});
      if (operation === "activity") return route.fulfill({json: {currentState: "idle", workingMs: 0, waitingMs: 0, coverageComplete: true}});
      if (operation === "details") return route.fulfill({json: {repositoriesHTML: "", artifactsHTML: ""}});
      return route.continue();
    });
    const time = new Date("2026-10-11T12:00:00Z");
    await page.clock.install({time});
    await page.clock.pauseAt(time);
    await page.goto(baseURL + "/example/#codex");
    const notice = page.locator("#codex-mode-status");
    const buttons = page.locator("#codex-mode");
    const plan = buttons.locator('[data-codex-mode="plan"]');
    const normal = buttons.locator('[data-codex-mode="default"]');
    const refresh = async () => {
      const previous = reads;
      await page.evaluate(() => window.modeRefreshEvents.onmessage());
      await page.clock.runFor(300);
      await expect.poll(() => reads).toBeGreaterThan(previous);
      await expect(page.locator("#transcript")).toContainText(`Read ${reads}`);
    };
    const visible = async value => {
      await page.evaluate(value => {
        Object.defineProperty(document, "hidden", {configurable: true, value: !value});
        document.dispatchEvent(new Event("visibilitychange"));
      }, value);
    };
    await expect(plan).toHaveCount(1);
    await page.clock.runFor(0);
    await expect(page.locator("#transcript")).toContainText("Read 1");
    await expect(notice).toBeHidden();
    await page.clock.runFor(749);
    await expect(notice).toBeHidden();
    await page.clock.runFor(1);
    await expect(notice).toBeVisible();
    await expect(notice).toHaveText("Checking mode…");
    await expect(page.locator("#plan-actions")).toBeHidden();

    mode = "plan"; metadataPending = false;
    await refresh();
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await expect(notice).toBeHidden();
    await expect(page.locator("#plan-actions")).toBeVisible();
    await plan.evaluate(button => { window.retainedModeButton = button; });
    mode = ""; metadataPending = true;
    await refresh();
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await expect(plan).toBeEnabled();
    await expect(notice).toBeHidden();
    await expect(page.locator("#plan-actions")).toBeHidden();
    assert(await plan.evaluate(button => button === window.retainedModeButton));
    await visible(false);
    const hiddenReads = reads;
    await page.clock.runFor(120_000);
    assert.equal(reads, hiddenReads, "hidden tab continued metadata reads");
    await visible(true);
    await page.clock.runFor(300);
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await expect(notice).toBeHidden();
    await normal.click();
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    assert.deepEqual(writes.at(-1), {collaborationMode: "default"});
    await page.clock.runFor(300);
    await expect(notice).toBeHidden();

    // A known response begun before the user's save must not undo the save.
    await plan.click();
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await expect(plan).toBeEnabled();
    await page.clock.runFor(300);
    mode = "plan"; metadataPending = false; holdRead = true;
    await page.evaluate(() => window.modeRefreshEvents.onmessage());
    await page.clock.runFor(300);
    await expect.poll(() => Boolean(releaseRead)).toBe(true);
    const staleNumber = reads;
    await normal.click();
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    await expect(normal).toBeEnabled();
    mode = ""; metadataPending = true;
    releaseRead(); releaseRead = null;
    await expect(page.locator("#transcript")).toContainText(`Read ${staleNumber}`);
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    await expect(page.locator("#plan-actions")).toBeHidden();
    await page.clock.runFor(300);
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    mode = "plan"; metadataPending = false;
    await refresh();
    await expect(plan).toHaveAttribute("aria-pressed", "true");

    // A plan hash already in flight must not restore actions after a save.
    mode = ""; metadataPending = true;
    await refresh();
    await expect(page.locator("#plan-actions")).toBeHidden();
    await page.evaluate(() => { window.holdNextPlanDigest = true; });
    mode = "plan"; metadataPending = false;
    await refresh();
    await expect.poll(() => page.evaluate(() => Boolean(window.releasePlanDigest))).toBe(true);
    await normal.click();
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    await expect(normal).toBeEnabled();
    await page.evaluate(async () => {
      window.releasePlanDigest(); window.releasePlanDigest = null;
      for (let i = 0; i < 10; ++i) await Promise.resolve();
    });
    // Keep timers paused so a newer read cannot conceal a stale continuation.
    await expect(page.locator("#plan-actions")).toBeHidden();

    // A read can display a plan while a mode save is waiting for its response.
    mode = ""; metadataPending = true;
    await refresh();
    await expect(page.locator("#plan-actions")).toBeHidden();
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    await plan.click();
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await expect(plan).toBeEnabled();
    await page.clock.runFor(300);
    holdSave = true;
    await normal.click();
    await expect.poll(() => Boolean(releaseSave)).toBe(true);
    mode = "plan"; metadataPending = false;
    await refresh();
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await expect(page.locator("#plan-actions")).toBeVisible();
    mode = ""; metadataPending = true;
    releaseSave(); releaseSave = null;
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    await expect(normal).toBeEnabled();
    await expect(page.locator("#plan-actions")).toBeHidden();
    mode = "plan"; metadataPending = false;
    await refresh();
    await expect(plan).toHaveAttribute("aria-pressed", "true");

    // Unavailable metadata retains the buttons and uses continuous visible grace.
    mode = "";
    await refresh();
    await page.clock.runFor(29_000);
    await expect(notice).toBeHidden();
    await visible(false);
    await page.clock.runFor(120_000);
    await visible(true);
    await page.clock.runFor(29_999);
    await expect(notice).toBeHidden();
    await page.clock.runFor(1);
    await expect(notice).toHaveText("Mode could not be refreshed.");
    await expect(notice).toBeVisible();
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await page.evaluate(() => { dispatchEvent(new Event("pagehide")); dispatchEvent(new Event("pageshow")); });
    await expect(notice).toBeHidden();
    mode = "default";
    await page.clock.runFor(300);
    await refresh();
    await expect(normal).toHaveAttribute("aria-pressed", "true");
    await expect(notice).toBeHidden();

    threadId = "thread-2"; mode = ""; metadataPending = true;
    await refresh();
    await expect(buttons).toBeHidden();
    await expect(notice).toBeHidden();
    await page.clock.runFor(750);
    await expect(notice).toBeVisible();
    await expect(notice).toHaveText("Checking mode…");
    mode = "plan"; metadataPending = false;
    await refresh();
    await expect(plan).toHaveAttribute("aria-pressed", "true");
    await expect(notice).toBeHidden();
    assert.deepEqual(errors, []);
    console.log("Mode retention, shared notices, stale reads/renders, saves and thread identity passed");
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
