"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const base = process.argv[2];
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium", args: ["--ignore-certificate-errors"]} : {})});
    try {
      const context = await browser.newContext({ignoreHTTPSErrors: true});
      const page = await context.newPage();
      const errors = []; page.on("pageerror", error => errors.push(error.message));
      await page.goto(`${base}/example/#settings`);
      await expect(page.locator("#browser-notifications")).toBeVisible();
      await expect(page.locator("#session-notifications-enabled")).toBeChecked();
      await expect(page.locator("#session-attention-status")).toContainText(/^(?!.*unavailable)/);
      assert.equal(await page.evaluate(() => Notification.permission), "default", "page load did not ask for permission");
      const workerAsset = await context.request.get(`${base}/notification-worker.js`);
      assert.equal(workerAsset.status(), 200);
      assert.equal(workerAsset.headers()["service-worker-allowed"], "/");
      assert.ok((await workerAsset.text()).includes('event.waitUntil(display(payload))'));
      if (engine === chromium) {
        const sub = await (await context.request.get(`${base}/fixture/subscription`)).json();
        await context.grantPermissions(["notifications"], {origin: base});
        await page.evaluate(sub => {
          let subscribed = true;
          const fake = {options: {}, toJSON: () => ({...sub, expirationTime: null}), unsubscribe: async () => {subscribed = false; return true;}};
          PushManager.prototype.subscribe = async () => fake;
          PushManager.prototype.getSubscription = async () => subscribed ? fake : null;
        }, sub);
        await page.locator("[data-notification-enable]").click();
        await expect(page.locator("[data-notification-status]")).toContainText("Enabled on this browser");
        const worker = context.serviceWorkers()[0]; assert.ok(worker, "real root service worker registered through CSP");
        await worker.evaluate(() => {
          self.fixtureDisplays = [];
          self.registration.showNotification = async (title, options) => self.fixtureDisplays.push({title, ...options});
        });
        const payload = {version: 1, identity: await worker.evaluate(async () => (await (await fetch("/api/sessions/example/attention")).json()).identity), id: "b".repeat(64), revision: 1, slug: "example", kind: "input", member: "implementer0", route: "/example/?member=implementer0#codex", expiresAt: Date.now() + 60000};
        await page.close(); assert.equal(context.pages().length, 0);
        await worker.evaluate(payload => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify(payload)})), payload);
        await expect.poll(() => worker.evaluate(() => self.fixtureDisplays.length)).toBe(1);
        assert.equal((await worker.evaluate(() => self.fixtureDisplays[0])).renotify, false);
        const reopened = await context.newPage(); await reopened.goto(`${base}/example/#settings`);
        await expect(reopened.locator("[data-notification-disable]")).toBeEnabled();
        await reopened.locator("[data-notification-disable]").click();
        await expect(reopened.locator("[data-notification-status]")).toContainText("Disabled on this browser");
      }
      assert.deepEqual(errors, [], `${engine.name()} loaded real notification UI without script errors`);
      console.log(`${engine.name()}: real template, CSP, permission separation and worker route passed${engine === chromium ? "; registered worker received synthetic push with zero tabs" : ""}`);
      await context.close();
    } finally {await browser.close();}
  }
})().catch(error => {console.error(error); process.exitCode = 1;});
