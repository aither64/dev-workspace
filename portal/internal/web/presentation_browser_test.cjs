"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];
const head = "b".repeat(40), base = "a".repeat(40);
const cards = '<div class="repo-grid"><article class="panel repo-card" data-repository-id="project" data-repository-name="project" data-repository-head="' + head + '"><div data-repository-status>project</div><section class="repository-history"><div class="repository-review-actions"><button data-review-branch disabled>Compare</button><button data-review-refresh>Refresh commits</button></div><p class="repository-head-change" hidden></p><div data-repository-commits></div></section></article></div>';
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const page = await browser.newPage({ignoreHTTPSErrors: true, viewport: {width: 1440, height: 720}});
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.route("**/api/codex-limits", route => route.fulfill({json: {windows: [{windowDurationMins: 10080, usedPercent: 20}], updatedAt: Date.now()}}));
      await page.route("**/api/sessions/example/**", async route => {
        const operation = new URL(route.request().url()).pathname.split("/").at(-1);
        const pair = {base, head, baseLabel: "Merge base"};
        switch (operation) {
          case "details": return route.fulfill({json: {repositoriesHTML: cards, artifactsHTML: "", repositoryCount: 1, artifactCount: 0, clusterCount: 0}});
          case "repository-histories": return route.fulfill({json: {repositories: [{repository: "project", pair, review: "frozen", history: {commits: [], page: 0, hasMore: false}}]}});
          case "repository-states": return route.fulfill({json: {repositories: [{repository: "project", head}]}});
          case "repository-comparison": return route.fulfill({json: {pair, review: "frozen", files: [], stats: {files: 0}}});
          default: return route.continue();
        }
      });
      const sidebar = page.locator(".workspace-sidebar");
      const width = async value => expect.poll(async () => Math.round((await sidebar.boundingBox()).width)).toBe(value);
      await page.goto(baseURL + "/example/");
      await width(250);
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).click();
      await width(250);
      await page.locator("[data-review-branch]").click();
      await expect(page.locator(".repository-review-heading")).toBeVisible();
      await width(58);
      const limits = page.locator(".codex-limits-toggle");
      await limits.focus(); await page.keyboard.press("Enter");
      await expect(limits).toHaveAttribute("aria-expanded", "true");
      await page.keyboard.press("Escape");
      await expect(limits).toHaveAttribute("aria-expanded", "false");
      for (const name of ["Workspace", "Delete session"]) await expect(sidebar.getByRole(name === "Workspace" ? "link" : "button", {name, exact: true})).toBeVisible();
      await page.getByRole("tab", {name: "Session settings", exact: true}).click();
      await width(250);
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).click();
      await width(58);
      await page.getByRole("button", {name: "← Repositories", exact: true}).click();
      await width(250);
      await page.goBack(); await width(58);
      await page.goForward(); await width(250);
      await page.setViewportSize({width: 600, height: 720}); await width(58);
      await page.getByRole("tab", {name: "Codex", exact: true}).click(); await width(58);
      await page.setViewportSize({width: 1440, height: 450}); await width(250);
      await page.goto(baseURL + "/example/?tab=repositories&repository=project&review=frozen&view=file");
      await width(58);
      await page.getByRole("tab", {name: /^Repositories(?: \(\d+\))?$/}).focus();
      await page.keyboard.press("End");
      await expect(page.getByRole("tab", {name: "Session settings", exact: true})).toBeFocused();
      const historyLength = await page.evaluate(() => history.length);
      await page.keyboard.press("End");
      assert.equal(await page.evaluate(() => history.length), historyLength, "same-tab key added a history entry");
      await width(250);
      await page.goto(baseURL + "/");
      await width(310);
      await page.setViewportSize({width: 600, height: 720}); await width(180);
      await limits.focus(); await page.keyboard.press("Enter");
      await expect(limits).toHaveAttribute("aria-expanded", "true");
      const limitsBox = await page.locator("#codex-limits-panel").boundingBox();
      assert(limitsBox.x >= 180 && limitsBox.x + limitsBox.width <= 600, "index limits popover escaped the viewport");
      await page.keyboard.press("Escape");
      await expect(limits).toHaveAttribute("aria-expanded", "false");
      await page.setViewportSize({width: 1440, height: 720}); await width(310);
      await expect(page.locator("#codex-limits-panel")).toBeVisible();
      assert.deepEqual(errors, []);
      console.log(engine.name() + ": comparison-only compact sidebar, limits and keyboard navigation passed");
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
