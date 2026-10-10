"use strict";
const assert = require("node:assert/strict");
const {chromium, expect} = require("@playwright/test");
const baseURL = process.argv[2];

(async () => {
  const browser = await chromium.launch({headless: true, channel: "chromium"});
  try {
    const page = await browser.newPage({ignoreHTTPSErrors: true});
    const errors = [];
    const saved = [];
    const savedSettings = [];
    let serverPair = {model: "model-1", reasoningEffort: "medium"};
    let threadStatus = "idle";
    let stopped = false;
    let detailsVersion = 0;
    let detailsReads = 0;
    let threadReads = 0;
    let holdStalePoll = false, releaseStalePoll = () => {};
    let stalePollStarted = false, stalePollFinished = false;
    let failSettingsWrite = false, failSettingsRead = false;
    let holdSettingsWrite = false, releaseSettingsWrite = () => {};
    const writeFailure = "Fixture write failure: " + "diagnostic-".repeat(80);
    page.on("pageerror", error => errors.push(error.message));
    await page.route("**/api/models", route => route.fulfill({json: [
      {model: "model-1", displayName: "gpt-6.1-sol", isDefault: true,
        defaultReasoningEffort: "medium", supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
      {model: "model-2", displayName: "gpt-6-astra", defaultReasoningEffort: "medium",
        supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
    ]}));
    await page.route("**/api/collaboration-modes", route => route.fulfill({json: [
      {mode: "default", name: "Default"}, {mode: "plan", name: "Plan"},
    ]}));
    await page.route("**/api/sessions/example/**", async route => {
      const operation = new URL(route.request().url()).pathname.split("/").at(-1);
      if (operation === "thread" || operation === "page") {
        threadReads++;
        if (holdStalePoll) {
          holdStalePoll = false;
          stalePollStarted = true;
          const oldPair = {...serverPair};
          await new Promise(resolve => { releaseStalePoll = resolve; });
          await route.fulfill({json: {threadId: "thread-1", status: threadStatus, collaborationMode: "default", ...oldPair, entries: [], hasOlder: false}});
          stalePollFinished = true;
          return;
        }
        if (failSettingsRead) return route.fulfill({status: 503, json: {error: "Fixture read failure"}});
        return route.fulfill({json: {threadId: "thread-1", status: threadStatus, collaborationMode: "default", ...serverPair, entries: [], hasOlder: false}});
      }
      if (operation === "settings" && route.request().method() === "POST") {
        const pair = route.request().postDataJSON();
        savedSettings.push(pair);
        if (holdSettingsWrite) {
          holdSettingsWrite = false;
          await new Promise(resolve => { releaseSettingsWrite = resolve; });
        }
        if (failSettingsWrite) return route.fulfill({status: 503, json: {error: writeFailure}});
        serverPair = pair;
        return route.fulfill({json: pair});
      }
      if (operation === "team-stats") return route.fulfill({json:{members:[{address:"architect0",state:"ready",snapshot:{currentState:"working",sentMessages:37,receivedMessages:12,totalToolCalls:81,workingMs:9000,idleMs:3000,waitingMs:1000}}]}});
      if (operation === "details") {
        detailsReads++;
        const teamHTML = `<form data-direct-team-form data-action="configure" data-address="architect0">
          <select name="model" data-model-select data-current-value="model-1" aria-label="architect0 model" required><option>Loading models</option></select>
          <select name="effort" data-effort-select data-current-value="medium" aria-label="architect0 reasoning" required><option>Loading efforts</option></select>
          <button type="submit">Save</button><p role="status" hidden></p></form><span data-version="${detailsVersion}"></span><span data-team-stats="architect0"></span>`;
        return route.fulfill({json: {repositoriesHTML: "", artifactsHTML: "", teamHTML, ...(stopped ? {interactive:false, recovery:{state:"stopped"}} : {})}});
      }
      if (operation === "team" && route.request().method() === "POST") {
        saved.push(route.request().postDataJSON());
        return route.fulfill(saved.length < 3 ? {status: 503, json: {error: "Fixture save failure"}} : {json: {ok: true}});
      }
      if (operation === "pending") return route.fulfill({json: []});
      if (operation === "queue") return route.fulfill({json: []});
      return route.continue();
    });
    await page.route("**/codex/conversations/example~architect0/settings", async route => {
      saved.push(route.request().postDataJSON());
      return route.fulfill(saved.length < 3 ? {status: 409, json: {error: "Fixture save failure"}} : {json: {ok: true}});
    });
    const liveModel = page.locator("#codex-model"), liveEffort = page.locator("#codex-effort");
    const edit = page.locator("#codex-settings-open"), save = page.locator("#codex-settings-save");
    const close = page.locator("#codex-settings-close"), dialog = page.locator("#codex-settings-dialog");
    const summary = page.locator("#codex-settings-summary"), feedback = page.locator("#codex-settings-status");
    const assertLayout = async () => {
      const layout = await page.evaluate(() => {
        const bounds = element => {
          const box = element.getBoundingClientRect();
          return {x: box.x, y: box.y, right: box.right, bottom: box.bottom, width: box.width, height: box.height};
        };
        const actions = document.querySelector("#message-form .chat-actions");
        const popup = document.getElementById("codex-settings-dialog");
        return {form: bounds(document.getElementById("message-form")), popup: popup.open ? bounds(popup) : null,
          summary: bounds(document.getElementById("codex-settings-summary")),
          edit: bounds(document.getElementById("codex-settings-open")),
          mode: bounds(document.getElementById("codex-mode")),
          modeBesideSend: document.getElementById("codex-mode").parentElement === document.getElementById("message-send").parentElement,
          controls: [...actions.querySelectorAll("button, select")].filter(el => el.getBoundingClientRect().width > 0)
            .map(el => ({id: el.id, ...bounds(el)})), width: innerWidth, height: innerHeight,
          documentWidth: document.documentElement.scrollWidth};
      });
      await expect(page.locator("#codex-mode")).toBeVisible();
      const diagnostic = JSON.stringify(layout);
      assert(layout.documentWidth <= layout.width, diagnostic);
      assert(layout.modeBesideSend, diagnostic);
      assert(Math.abs(layout.summary.y + layout.summary.height / 2 - layout.edit.y - layout.edit.height / 2) <= 1, diagnostic);
      if (layout.width >= 981) assert(layout.mode.right <= layout.summary.x, diagnostic);
      for (const control of layout.controls) {
        assert(control.x >= layout.form.x - 1 && control.right <= layout.form.right + 1, diagnostic);
        assert(control.width >= 28 && control.height >= 28, diagnostic);
      }
      for (const id of ["codex-settings-open", "message-send", "interrupt"]) {
        assert(layout.controls.some(control => control.id === id), "missing visible control: " + id);
      }
      assert(!layout.controls.some(control => ["codex-model", "codex-effort"].includes(control.id)));
      if (layout.popup) {
        assert(layout.popup.x >= 0 && layout.popup.right <= layout.width + 1, diagnostic);
        assert(layout.popup.y >= 0 && layout.popup.bottom <= layout.height + 1, diagnostic);
      }
    };
    for (const width of [981, 1024, 1100, 1200, 1280, 1440, 390]) {
      await page.setViewportSize({width, height: width >= 981 ? 900 : 844});
      serverPair = {model: "model-1", reasoningEffort: "medium"}; savedSettings.length = 0;
      stalePollStarted = false; stalePollFinished = false;
      await page.goto(baseURL + "/example/");
      await expect(summary).toContainText("medium");
      await expect(page.getByRole("button", {name: "Edit Codex settings", exact: true})).toHaveText("");
      assert.equal(await edit.locator("svg").count(), 1);
      await expect(edit).toBeEnabled(); await expect(dialog).toBeHidden();
      await assertLayout();
      await edit.click(); await expect(liveModel).toHaveValue("model-1");
      await expect(save).toBeDisabled();
      await liveModel.selectOption("model-2"); await liveEffort.selectOption("high");
      await expect(save).toBeEnabled(); await assertLayout();
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(liveModel).toHaveValue("model-2"); await expect(liveEffort).toHaveValue("high");
      await page.keyboard.press("Escape"); await expect(dialog).toBeHidden();
      assert.equal(savedSettings.length, 0);
      await edit.click(); await expect(liveModel).toHaveValue("model-1");
      await expect(liveEffort).toHaveValue("medium");
      await liveModel.selectOption("model-2"); await close.click(); await edit.click();
      await expect(liveModel).toHaveValue("model-1");
      await liveModel.selectOption("model-2"); await liveEffort.selectOption("high");
      await page.reload(); await expect(dialog).toBeHidden(); await edit.click();
      await expect(liveModel).toHaveValue("model-1"); await expect(liveEffort).toHaveValue("medium");
      await liveModel.selectOption("model-2"); await liveEffort.selectOption("high");
      threadStatus = "active"; await page.request.post(baseURL + "/fixture/refresh");
      await expect(save).toBeEnabled(); await expect(close).toBeEnabled();
      await expect(liveModel).toHaveValue("model-2");
      for (const state of ["systemError", "notLoaded", "idle"]) {
         threadStatus = state; await page.request.post(baseURL + "/fixture/refresh");
        await expect(save).toBeEnabled();
      }
      await expect(save).toBeEnabled();
      holdStalePoll = true; await page.request.post(baseURL + "/fixture/refresh");
      await expect.poll(() => stalePollStarted).toBe(true);
      holdSettingsWrite = true; await save.click();
      await expect.poll(() => savedSettings.length).toBe(1);
      assert.deepEqual(savedSettings[0], {model: "model-2", reasoningEffort: "high"});
      await expect(feedback).toHaveText("Saving Codex settings…");
      await expect(close).toBeDisabled(); await expect(save).toBeDisabled();
      await page.keyboard.press("Escape"); await expect(dialog).toBeVisible();
      releaseSettingsWrite(); await expect(dialog).toBeHidden();
      releaseStalePoll(); await expect.poll(() => stalePollFinished).toBe(true);
      await expect(summary).toContainText("high");
      await page.reload(); await expect(summary).toContainText("high"); await edit.click();
      await expect(liveModel).toHaveValue("model-2"); await expect(liveEffort).toHaveValue("high");
      await liveModel.selectOption("model-1"); await liveEffort.selectOption("medium");
      failSettingsWrite = true; failSettingsRead = true; await save.click();
      await expect.poll(() => savedSettings.length).toBe(2);
      await expect(feedback).toHaveText("The settings update could not be confirmed: " + writeFailure);
      await expect(dialog).toBeVisible(); await expect(close).toBeEnabled(); await assertLayout();
      failSettingsWrite = false; failSettingsRead = false;
      await close.click(); await page.reload(); await edit.click();
      await expect(liveModel).toHaveValue("model-2"); await expect(liveEffort).toHaveValue("high");
      await expect(save).toBeDisabled(); await close.click(); await assertLayout();
    }
    await page.setViewportSize({width: 1280, height: 720});
    await page.getByRole("tab", {name: "Team"}).click();
    const model = page.getByLabel("architect0 model");
    const effort = page.getByLabel("architect0 reasoning");
    await expect(model).toHaveValue("model-1");
    detailsVersion++;
    const idleReads = detailsReads;
    await page.evaluate(() => dispatchEvent(new Event("focus")));
    await expect.poll(() => detailsReads).toBeGreaterThan(idleReads);
    await expect(page.locator("[data-version]")).toHaveAttribute("data-version", "1");
    await page.getByRole("button", {name: "Save"}).click();
    await expect.poll(() => saved.length).toBe(1);
    await expect(page.getByText("Fixture save failure")).toBeVisible();
    assert.equal(saved[0].model, "model-1");
    assert.equal(saved[0].reasoningEffort, "medium");
    detailsVersion++;
    const failedReads = detailsReads;
    await page.getByRole("tab", {name: "Team"}).click();
    await expect.poll(() => detailsReads).toBeGreaterThan(failedReads);
    await expect(page.getByText("Fixture save failure")).toBeVisible();
    await expect(page.locator("[data-version]")).toHaveAttribute("data-version", "1");
    await model.selectOption("model-2");
    await effort.selectOption("high");
    await model.focus();
    await expect(page.locator('[data-team-stats="architect0"]')).toContainText("37 sent · 12 received · 81 tool calls");
    detailsVersion++;
    const editingReads = detailsReads;
    await page.evaluate(() => dispatchEvent(new Event("focus")));
    await expect.poll(() => detailsReads).toBeGreaterThan(editingReads);
    await expect(model).toBeFocused();
    await page.request.post(baseURL + "/fixture/refresh");
    await expect.poll(() => threadReads).toBeGreaterThan(1);
    await expect(model).toHaveValue("model-2");
    await expect(effort).toHaveValue("high");
    await expect(page.locator("[data-version]")).toHaveAttribute("data-version", "1");
    await page.getByRole("button", {name: "Save"}).click();
    await expect.poll(() => saved.length).toBe(2);
    await expect(page.getByText("Fixture save failure")).toBeVisible();
    assert.equal(saved[1].model, "model-2");
    assert.equal(saved[1].reasoningEffort, "high");
    await expect(model).toHaveValue("model-2");
    stopped = true;
    const stoppedReads = detailsReads;
    await page.evaluate(() => dispatchEvent(new Event("focus")));
    await expect.poll(() => detailsReads).toBeGreaterThan(stoppedReads);
    await expect(page.locator("#message-send")).toBeDisabled();
    await expect(edit).toBeEnabled();
    await page.getByRole("button", {name: "Save"}).click();
    await expect.poll(() => saved.length).toBe(3);
    assert.deepEqual(saved[2], saved[1]);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
