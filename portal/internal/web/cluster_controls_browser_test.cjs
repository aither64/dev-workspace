"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const baseURL = process.argv[2];

(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium"} : {})});
    try {
      const context = await browser.newContext({ignoreHTTPSErrors: true});
      const page = await context.newPage(), errors = [], attempts = [];
      page.on("pageerror", error => errors.push(error.message));
      let state = "running", busy = false, legacy = false, finish, failStart = true;
      await page.addInitScript(() => {
        window.clusterPageMarker = Math.random();
        window.copiedValues = [];
        Object.defineProperty(navigator, "clipboard", {value: {writeText: async value => {
          if (window.failCopy) throw new Error("fixture failure");
          window.copiedValues.push(value);
        }}});
      });
      await context.route("**/api/sessions/example/details", async route => {
        const response = await route.fetch(), payload = await response.json();
        const html = await page.request.get(`${baseURL}/fixture/clusters?state=${state}&busy=${Number(busy)}&legacy=${Number(legacy)}`);
        await route.fulfill({json: {...payload, clustersHTML: await html.text(), clusterCount: state === "absent" ? 0 : 1}});
      });
      await context.route("**/api/sessions/example/*-cluster", async route => {
        const action = route.request().url().split("/").pop().replace("-cluster", "");
        assert.equal(route.request().postDataJSON().kind, "alpha");
        attempts.push(action); busy = true;
        await new Promise(resolve => { finish = resolve; });
        busy = false;
        if (action === "start" && failStart) {
          failStart = false;
          return route.fulfill({status: 409, json: {error: "Fixture build failed"}});
        }
        state = {start: "running", stop: "stopped", release: "absent"}[action];
        return route.fulfill({json: {ok: true}});
      });
      await page.goto(baseURL + "/example/#clusters");
      const marker = await page.evaluate(() => window.clusterPageMarker);
      const card = page.locator('[data-cluster="alpha"]');
      await expect(card).toBeVisible();
      const password = card.locator('[data-secret-field]');
      await expect(password).toHaveAttribute("type", "password");
      const reveal = card.getByRole("button", {name: "Reveal Password", exact: true});
      await reveal.focus(); await page.keyboard.press("Enter");
      await expect(password).toHaveAttribute("type", "text");
      await expect(card.getByRole("button", {name: "Hide Password", exact: true})).toHaveAttribute("aria-pressed", "true");
      await card.getByRole("button", {name: "Hide Password", exact: true}).click();
      await expect(password).toHaveAttribute("type", "password");
      await card.getByRole("button", {name: "Copy Password", exact: true}).click();
      assert.deepEqual(await page.evaluate(() => window.copiedValues), ["fixture-secret"]);
      await expect(card.getByRole("button", {name: "Copy Password", exact: true}).locator("svg")).toHaveCount(1);
      await page.evaluate(() => { window.failCopy = true; });
      await card.getByRole("button", {name: "Copy Login", exact: true}).first().click();
      await expect(card.locator(".copy-feedback").first()).toHaveText("Could not copy Login");
      await expect(card.getByRole("button", {name: "Copy Login", exact: true}).first().locator("svg")).toHaveCount(1);
      await page.evaluate(() => { window.failCopy = false; });
      const longSecret = "fixture-secret-".repeat(20);
      await password.evaluate((input, value) => { input.value = value; }, longSecret);
      for (const width of [1280, 375]) {
        await page.setViewportSize({width, height: 850});
        const loginCopy = await card.getByRole("button", {name: "Copy Login", exact: true}).first().boundingBox();
        const secretCopy = await card.getByRole("button", {name: "Copy Password", exact: true}).boundingBox();
        assert(Math.abs(loginCopy.x - secretCopy.x) < 1, "copy buttons are misaligned");
        assert(secretCopy.x + secretCopy.width <= width, "credential controls overflow");
        const loginInput = await card.locator('.cluster-account input:not([data-secret-field])').first().boundingBox();
        const secretInput = await password.boundingBox();
        assert(Math.abs(loginInput.x + loginInput.width - secretInput.x - secretInput.width) < 1, "input edges are misaligned");
        for (const row of await card.locator('.cluster-copy-row:visible').all()) {
          const inputBounds = await row.locator('input').boundingBox();
          const copyBounds = await row.locator('[data-copy]').boundingBox();
          const gap = await row.evaluate(element => parseFloat(getComputedStyle(element).columnGap));
          assert(Math.abs(copyBounds.x - inputBounds.x - inputBounds.width - gap) < 1, "empty space before Copy");
        }
        for (const type of ["password", "text"]) {
          if (await password.getAttribute("type") !== type) await card.locator('[data-reveal-secret]').click();
          const eye = await card.locator('[data-reveal-secret]').boundingBox();
          assert(eye.x >= secretInput.x && eye.x + eye.width <= secretInput.x + secretInput.width, "Reveal is outside the input");
          assert(eye.y >= secretInput.y && eye.y + eye.height <= secretInput.y + secretInput.height, "Reveal overflows vertically");
          const padding = await password.evaluate(input => parseFloat(getComputedStyle(input).paddingRight));
          assert(secretInput.x + secretInput.width - padding < eye.x, "credential text overlaps Reveal");
        }
        assert(await card.locator(".cluster-actions").evaluate(element => {
          const bounds = element.getBoundingClientRect();
          return bounds.left >= 0 && bounds.right <= innerWidth;
        }), "toolbar overflows");
      }
      await card.locator('[data-reveal-secret]').click();
      await expect(password).toHaveAttribute("type", "password");
      await password.focus(); await page.keyboard.press("Tab");
      await expect(card.locator('[data-reveal-secret]')).toBeFocused();
      await page.keyboard.press("Space");
      await expect(password).toHaveAttribute("type", "text");
      await page.keyboard.press("Tab");
      await expect(card.getByRole("button", {name: "Copy Password", exact: true})).toBeFocused();
      await card.getByRole("button", {name: "Copy Password", exact: true}).click();
      assert.deepEqual(await page.evaluate(() => window.copiedValues), ["fixture-secret", longSecret]);
      await page.setViewportSize({width: 1280, height: 850});
      await card.getByRole("tab", {name: "Database", exact: true}).click();
      await card.getByRole("button", {name: "Stop", exact: true}).click();
      await expect.poll(() => Boolean(finish)).toBe(true);
      await expect(card.locator("[data-cluster-control-status]")).toContainText("Stopping");
      await expect(card.getByRole("button", {name: "Release", exact: true})).toBeDisabled();
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(card.getByRole("tab", {name: "Database", exact: true})).toHaveAttribute("aria-selected", "true");
      finish(); finish = null;
      await expect(card.getByRole("button", {name: "Start", exact: true})).toBeEnabled();
      await card.getByRole("button", {name: "Start", exact: true}).click();
      await expect.poll(() => Boolean(finish)).toBe(true);
      finish(); finish = null;
      await expect(card.locator("[data-cluster-control-status]")).toContainText("Fixture build failed");
      await expect(card.getByRole("button", {name: "Start", exact: true})).toBeEnabled();
      assert.deepEqual(attempts, ["stop", "start"]);
      await card.getByRole("button", {name: "Start", exact: true}).click();
      await expect.poll(() => Boolean(finish)).toBe(true);
      finish(); finish = null;
      await expect(card.getByRole("button", {name: "Stop", exact: true})).toBeEnabled();
      legacy = true;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(card.locator('[data-cluster-action="stop"]')).toHaveCount(0);
      legacy = false;
      await page.evaluate(() => dispatchEvent(new Event("focus")));
      await expect(card.getByRole("button", {name: "Stop", exact: true})).toBeVisible();
      page.once("dialog", dialog => dialog.dismiss());
      await card.getByRole("button", {name: "Release", exact: true}).click();
      assert.equal(attempts.length, 3);
      page.once("dialog", dialog => {
        assert(dialog.message().includes("persistent disks"));
        return dialog.accept();
      });
      await card.getByRole("button", {name: "Release", exact: true}).click();
      await expect.poll(() => Boolean(finish)).toBe(true);
      finish(); finish = null;
      await expect(card).toHaveCount(0);
      assert.equal(await page.evaluate(() => window.clusterPageMarker), marker, "cluster control reloaded the page");
      assert.deepEqual(errors, []);
      await context.close();
    } finally { await browser.close(); }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
