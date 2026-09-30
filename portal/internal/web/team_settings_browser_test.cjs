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
    let detailsVersion = 0;
    let detailsReads = 0;
    let threadReads = 0;
    let holdStalePoll = false, releaseStalePoll = () => {};
    let stalePollStarted = false, stalePollFinished = false;
    let failSettingsWrite = false, failSettingsRead = false;
    page.on("pageerror", error => errors.push(error.message));
    await page.route("**/api/models", route => route.fulfill({json: [
      {model: "model-1", displayName: "Model 1", isDefault: true,
        defaultReasoningEffort: "medium", supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
      {model: "model-2", displayName: "Model 2", defaultReasoningEffort: "medium",
        supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
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
          await route.fulfill({json: {threadId: "thread-1", status: "idle", ...oldPair, entries: [], hasOlder: false}});
          stalePollFinished = true;
          return;
        }
        if (failSettingsRead) return route.fulfill({status: 503, json: {error: "Fixture read failure"}});
        return route.fulfill({json: {threadId: "thread-1", status: "idle", ...serverPair, entries: [], hasOlder: false}});
      }
      if (operation === "settings" && route.request().method() === "POST") {
        const pair = route.request().postDataJSON();
        savedSettings.push(pair);
        if (failSettingsWrite) return route.fulfill({status: 503, json: {error: "Fixture write failure"}});
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
    await page.goto(baseURL + "/example/");
    const liveModel = page.locator("#codex-model"), liveEffort = page.locator("#codex-effort");
    await expect(liveModel).toHaveValue("model-1");
    await liveModel.selectOption("model-2");
    await liveEffort.selectOption("high");
    assert.equal(savedSettings.length, 0);
    await page.evaluate(() => dispatchEvent(new Event("focus")));
    await expect(liveModel).toHaveValue("model-2");
    await expect(liveEffort).toHaveValue("high");
    await page.reload();
    await expect(liveModel).toHaveValue("model-2");
    await expect(liveEffort).toHaveValue("high");
    await page.locator("#codex-settings-cancel").click();
    await expect(liveModel).toHaveValue("model-1");
    await expect(liveEffort).toHaveValue("medium");
    await liveModel.selectOption("model-2");
    await liveEffort.selectOption("high");
    holdStalePoll = true;
    await page.request.post(baseURL + "/fixture/refresh");
    await expect.poll(() => stalePollStarted).toBe(true);
    await page.locator("#codex-settings-apply").click();
    await expect.poll(() => savedSettings.length).toBe(1);
    assert.deepEqual(savedSettings[0], {model: "model-2", reasoningEffort: "high"});
    await expect(page.locator("#codex-settings-status")).toBeEmpty();
    await expect(page.locator("#codex-settings-apply")).toBeDisabled();
    await expect(page.locator("#codex-settings-cancel")).toBeDisabled();
    releaseStalePoll();
    await expect.poll(() => stalePollFinished).toBe(true);
    await page.waitForTimeout(50);
    await expect(liveModel).toHaveValue("model-2");
    await expect(liveEffort).toHaveValue("high");
    await expect(page.locator("#codex-settings-apply")).toBeDisabled();
    await expect(page.locator("#codex-settings-cancel")).toBeDisabled();
    await page.reload();
    await expect(liveModel).toHaveValue("model-2");
    await expect(liveEffort).toHaveValue("high");
    await liveModel.selectOption("model-1");
    await liveEffort.selectOption("medium");
    failSettingsWrite = true; failSettingsRead = true;
    await page.locator("#codex-settings-apply").click();
    await expect.poll(() => savedSettings.length).toBe(2);
    assert.deepEqual(savedSettings[1], {model: "model-1", reasoningEffort: "medium"});
    await expect(page.locator("#codex-settings-status")).toContainText("The settings update could not be confirmed");
    await expect(liveModel).toHaveValue("model-1");
    await expect(liveEffort).toHaveValue("medium");
    await expect(page.locator("#codex-settings-cancel")).toBeEnabled();
    failSettingsWrite = false; failSettingsRead = false;
    await page.reload();
    await expect(liveModel).toHaveValue("model-1");
    await expect(liveEffort).toHaveValue("medium");
    await expect(page.locator("#codex-settings-apply")).toBeEnabled();
    await page.locator("#codex-settings-cancel").click();
    await expect(liveModel).toHaveValue("model-2");
    await expect(liveEffort).toHaveValue("high");
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
