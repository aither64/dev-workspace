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
      if (operation === "details") {
        detailsReads++;
        const teamHTML = `<form data-direct-team-form data-action="configure" data-address="architect0">
          <select name="model" data-model-select data-current-value="model-1" aria-label="architect0 model" required><option>Loading models</option></select>
          <select name="effort" data-effort-select data-current-value="medium" aria-label="architect0 reasoning" required><option>Loading efforts</option></select>
          <button type="submit">Save</button><p role="status" hidden></p></form><span data-version="${detailsVersion}"></span>`;
        return route.fulfill({json: {repositoriesHTML: "", artifactsHTML: "", teamHTML}});
      }
      if (operation === "team" && route.request().method() === "POST") {
        saved.push(route.request().postDataJSON());
        return route.fulfill(saved.length < 3 ? {status: 503, json: {error: "Fixture save failure"}} : {json: {ok: true}});
      }
      if (operation === "pending") return route.fulfill({json: []});
      if (operation === "queue") return route.fulfill({json: []});
      return route.continue();
    });
    const liveModel = page.locator("#codex-model"), liveEffort = page.locator("#codex-effort");
    const apply = page.locator("#codex-settings-apply"), cancel = page.locator("#codex-settings-cancel");
    const feedback = page.locator("#codex-settings-status");
    const geometry = () => page.evaluate(() => {
      const bounds = element => {
        const box = element.getBoundingClientRect();
        return {id: element.id || element.className, x: box.x, y: box.y, width: box.width,
          height: box.height, right: box.right, bottom: box.bottom};
      };
      const actions = document.querySelector("#message-form .chat-actions");
      const status = document.getElementById("codex-settings-status");
      const controls = [...actions.querySelectorAll("button, select")]
        .filter(element => element.getBoundingClientRect().width > 0);
      return {actions: bounds(actions), composer: bounds(actions.querySelector(".composer-controls")),
        form: bounds(document.getElementById("message-form")),
        controls: controls.map(element => ({...bounds(element), inComposer: Boolean(element.closest(".composer-controls"))})),
        status: {...bounds(status), text: status.textContent,
          scrollWidth: status.scrollWidth, clientWidth: status.clientWidth,
          scrollHeight: status.scrollHeight, clientHeight: status.clientHeight},
        viewport: {width: innerWidth, height: innerHeight}, documentWidth: document.documentElement.scrollWidth};
    });
    const assertLayout = async (baseline, hasFeedback = false) => {
      await expect(page.locator("#codex-mode")).toBeVisible();
      const layout = await geometry();
      const diagnostic = JSON.stringify(layout);
      assert(layout.documentWidth <= layout.viewport.width, diagnostic);
      for (const control of layout.controls) {
        assert(control.x >= layout.form.x - 1 && control.right <= layout.form.right + 1, diagnostic);
        assert(control.y >= 0 && control.bottom <= layout.viewport.height + 1, diagnostic);
        assert(control.width >= 28 && control.height >= 28, diagnostic);
        if (control.inComposer) {
          assert(control.x >= layout.composer.x - 1 && control.right <= layout.composer.right + 1 &&
            control.y >= layout.composer.y - 1 && control.bottom <= layout.composer.bottom + 1,
            "control escaped its composer group: " + diagnostic);
        }
      }
      for (let index = 0; index < layout.controls.length; index++) {
        const first = layout.controls[index];
        for (const second of layout.controls.slice(index + 1)) {
          const overlapWidth = Math.min(first.right, second.right) - Math.max(first.x, second.x);
          const overlapHeight = Math.min(first.bottom, second.bottom) - Math.max(first.y, second.y);
          assert(overlapWidth <= 1 || overlapHeight <= 1,
            `controls overlap (${first.id}, ${second.id}): ` + diagnostic);
        }
      }
      for (const id of ["codex-model", "codex-effort", "codex-settings-apply", "codex-settings-cancel", "message-send", "interrupt"]) {
        assert(layout.controls.some(control => control.id === id), "missing visible control: " + id);
      }
      const top = Math.max(...layout.controls.map(control => control.y));
      const bottom = Math.min(...layout.controls.map(control => control.bottom));
      if (layout.viewport.width >= 1280) assert(top < bottom, "desktop controls must share one row: " + diagnostic);
      else if (layout.viewport.width <= 760) assert(top >= bottom, "mobile controls must retain wrapping: " + diagnostic);
      if (baseline) {
        assert(Math.abs(layout.actions.height - baseline.actions.height) <= 1, "feedback changed the control row height: " + diagnostic);
        for (const control of layout.controls) {
          const before = baseline.controls.find(candidate => candidate.id === control.id);
          assert(before && Math.abs(control.x - before.x) <= 1 && Math.abs(control.width - before.width) <= 1,
            "feedback changed control widths or positions: " + diagnostic);
        }
      }
      if (hasFeedback) {
        assert(layout.status.y >= layout.actions.bottom - 1, "feedback overlaps controls: " + diagnostic);
        assert(layout.status.bottom <= layout.form.bottom + 1 && layout.status.bottom <= layout.viewport.height + 1, diagnostic);
        assert(layout.status.scrollWidth <= layout.status.clientWidth + 1 &&
          layout.status.scrollHeight <= layout.status.clientHeight + 1, "feedback is clipped: " + diagnostic);
      } else {
        await expect(feedback).toBeEmpty();
        await expect(feedback).toBeHidden();
        assert.equal(layout.status.height, 0, "empty feedback must occupy no space");
      }
      return layout;
    };
    for (const width of [981, 1024, 1100, 1200, 1280, 1440, 390]) {
      await page.setViewportSize({width, height: width >= 981 ? 900 : 844});
      serverPair = {model: "model-1", reasoningEffort: "medium"};
      savedSettings.length = 0;
      stalePollStarted = false; stalePollFinished = false;
      await page.goto(baseURL + "/example/");
      await expect(liveModel).toHaveValue("model-1");
      await expect(liveModel).toBeEnabled();
      await expect(page.locator("#codex-mode")).toBeVisible();
      await expect(apply).toBeDisabled();
      await expect(cancel).toBeDisabled();
      const baseline = await assertLayout();
      await liveModel.selectOption("model-2");
      await expect(liveEffort).toHaveValue("medium");
      await expect(apply).toBeEnabled();
      await expect(cancel).toBeEnabled();
      await assertLayout(baseline);
      await cancel.click();
      await expect(liveModel).toHaveValue("model-1");
      await liveEffort.selectOption("high");
      await expect(liveModel).toHaveValue("model-1");
      await expect(apply).toBeEnabled();
      await assertLayout(baseline);
      await cancel.click();
      await expect(liveEffort).toHaveValue("medium");
      await liveModel.selectOption("model-2");
      await liveEffort.selectOption("high");
      assert.equal(savedSettings.length, 0);
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(liveModel).toHaveValue("model-2");
      await expect(liveEffort).toHaveValue("high");
      await page.reload();
      await expect(liveModel).toHaveValue("model-2");
      await expect(liveEffort).toHaveValue("high");
      await expect(apply).toBeEnabled();
      await assertLayout(baseline);
      threadStatus = "active";
      await page.request.post(baseURL + "/fixture/refresh");
      await expect(page.locator("#message-queue")).toBeVisible();
      await expect(liveModel).toBeDisabled();
      await expect(apply).toBeDisabled();
      await expect(cancel).toBeEnabled();
      await expect(liveModel).toHaveValue("model-2");
      await expect(liveEffort).toHaveValue("high");
      await assertLayout();
      threadStatus = "idle";
      await page.request.post(baseURL + "/fixture/refresh");
      await expect(apply).toBeEnabled();
      await expect(page.locator("#message-queue")).toBeHidden();
      await cancel.click();
      await expect(liveModel).toHaveValue("model-1");
      await expect(liveEffort).toHaveValue("medium");
      await liveModel.selectOption("model-2");
      await liveEffort.selectOption("high");
      holdStalePoll = true;
      await page.request.post(baseURL + "/fixture/refresh");
      await expect.poll(() => stalePollStarted).toBe(true);
      holdSettingsWrite = true;
      await apply.click();
      await expect.poll(() => savedSettings.length).toBe(1);
      assert.deepEqual(savedSettings[0], {model: "model-2", reasoningEffort: "high"});
      await expect(feedback).toHaveText("Saving Codex settings…");
      await expect(feedback).toHaveAttribute("aria-live", "polite");
      await expect(apply).toBeDisabled();
      await expect(cancel).toBeDisabled();
      await assertLayout(baseline, true);
      releaseSettingsWrite();
      await expect(feedback).toBeEmpty();
      await expect(apply).toBeDisabled();
      await expect(cancel).toBeDisabled();
      releaseStalePoll();
      await expect.poll(() => stalePollFinished).toBe(true);
      await page.waitForTimeout(50);
      await expect(liveModel).toHaveValue("model-2");
      await expect(liveEffort).toHaveValue("high");
      await expect(apply).toBeDisabled();
      await expect(cancel).toBeDisabled();
      await assertLayout(baseline);
      await page.reload();
      await expect(liveModel).toHaveValue("model-2");
      await expect(liveEffort).toHaveValue("high");
      await liveModel.selectOption("model-1");
      await liveEffort.selectOption("medium");
      failSettingsWrite = true; failSettingsRead = true;
      await apply.click();
      await expect.poll(() => savedSettings.length).toBe(2);
      assert.deepEqual(savedSettings[1], {model: "model-1", reasoningEffort: "medium"});
      await expect(feedback).toHaveText("The settings update could not be confirmed: " + writeFailure);
      await expect(feedback).toBeVisible();
      await expect(liveModel).toHaveValue("model-1");
      await expect(liveEffort).toHaveValue("medium");
      await expect(cancel).toBeEnabled();
      await assertLayout(baseline, true);
      failSettingsWrite = false; failSettingsRead = false;
      await page.reload();
      await expect(liveModel).toHaveValue("model-1");
      await expect(liveEffort).toHaveValue("medium");
      await expect(apply).toBeEnabled();
      await assertLayout(baseline);
      await cancel.click();
      await expect(liveModel).toHaveValue("model-2");
      await expect(liveEffort).toHaveValue("high");
      await expect(apply).toBeDisabled();
      await expect(cancel).toBeDisabled();
      await assertLayout(baseline);
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
    await page.getByRole("button", {name: "Save"}).click();
    await expect.poll(() => saved.length).toBe(3);
    assert.deepEqual(saved[2], saved[1]);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
