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
    let detailsVersion = 0;
    let detailsReads = 0;
    let threadReads = 0;
    page.on("pageerror", error => errors.push(error.message));
    await page.route("**/api/models", route => route.fulfill({json: [
      {model: "model-1", displayName: "Model 1", isDefault: true,
        defaultReasoningEffort: "medium", supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
      {model: "model-2", displayName: "Model 2", defaultReasoningEffort: "medium",
        supportedReasoningEfforts: [{reasoningEffort: "medium"}, {reasoningEffort: "high"}]},
    ]}));
    await page.route("**/api/sessions/example/**", route => {
      const operation = new URL(route.request().url()).pathname.split("/").at(-1);
      if (operation === "thread") {
        threadReads++;
        return route.fulfill({json: {threadId: "thread-1", status: "idle", model: "model-1", reasoningEffort: "medium", entries: []}});
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
