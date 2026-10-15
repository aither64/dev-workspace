"use strict";
const assert = require("node:assert/strict");
const {chromium, firefox, expect} = require("@playwright/test");
const base = process.argv[2];
const safeError = error => ["Error", "TimeoutError", "AbortError", "NotAllowedError", "SecurityError", "InvalidStateError", "TypeError"].includes(error?.name) ? error.name : "UnknownError";
function failureTrace(context) {
  const rows = [], requests = new WeakMap(), pending = new Set();
  let order = 0, requestNumber = 0, dropped = 0;
  const record = value => {rows.push({order: ++order, ...value}); if (rows.length > 48) {rows.shift(); dropped++;}};
  const category = request => {
    const pathname = new URL(request.url()).pathname;
    return pathname === "/api/browser-notifications" && request.method() === "GET" ? "get" :
      pathname === "/api/browser-notifications/register" ? "register" : null;
  };
  const track = promise => {pending.add(promise); void promise.finally(() => pending.delete(promise));};
  const queryIdentity = async request => {
    const queried = new URL(request.url()).searchParams.get("registrationId");
    if (!queried) return "absent";
    try {return await request.frame().evaluate(value => value === localStorage.getItem("workspace-browser-notifications.v1"), queried) ? "current" : "stale";}
    catch (_) {return "unavailable";}
  };
  context.on("request", request => {
    const action = category(request); if (!action) return;
    const number = ++requestNumber; requests.set(request, number);
    record({event: "request", action, request: number});
    if (action === "get") track(queryIdentity(request).then(identity => record({event: "query_identity", request: number, identity})));
  });
  context.on("requestfailed", request => {
    if (requests.has(request)) record({event: "request_failed", request: requests.get(request)});
  });
  context.on("response", response => {
    const request = response.request(), number = requests.get(request);
    if (!number) return;
    record({event: "response", request: number, status: response.status()});
    track((async () => {
      try {
        const value = await response.json();
        record({event: "response_shape", request: number, registered: value.registered === true,
          available: value.available === true, replyIdPresent: typeof value.registrationId === "string" && Boolean(value.registrationId)});
      } catch (error) {record({event: "response_unavailable", request: number, category: safeError(error)});}
      if (category(request) === "get") record({event: "completion_identity", request: number, identity: await queryIdentity(request)});
    })());
  });
  return {record, async dump(page) {
    let snapshotDeadline;
    try {
      const snapshot = await Promise.race([page.evaluate(async () => {
        const text = document.querySelector("[data-notification-status]")?.textContent || "";
        const state = text.startsWith("Enabled on this browser") ? "enabled" : text.startsWith("Disabled on this browser") ? "disabled" :
          text.startsWith("Browser setup is incomplete") ? "incomplete" : text.startsWith("Registration failed:") ? "registration_failed" :
          text.startsWith("Disabling is incomplete") ? "disable_incomplete" : "other";
        const worker = await navigator.serviceWorker.getRegistration("/");
        return {state, savedIdPresent: Boolean(localStorage.getItem("workspace-browser-notifications.v1")),
          testDisabled: Boolean(document.querySelector("[data-notification-test]")?.disabled),
          controller: ["installing", "installed", "activating", "activated", "redundant"].includes(navigator.serviceWorker.controller?.state) ? navigator.serviceWorker.controller.state : "none",
          active: Boolean(worker?.active), waiting: Boolean(worker?.waiting), installing: Boolean(worker?.installing)};
      }), new Promise(resolve => {snapshotDeadline = setTimeout(() => resolve(null), 1500);})]);
      if (snapshot) record({event: "failure_state", ...snapshot});
      else record({event: "failure_state_unavailable", category: "TimeoutError"});
    } catch (error) {record({event: "failure_state_unavailable", category: safeError(error)});}
    finally {clearTimeout(snapshotDeadline);}
    let timer;
    await Promise.race([Promise.allSettled([...pending]), new Promise(resolve => {timer = setTimeout(resolve, 1000);})]); clearTimeout(timer);
    while (JSON.stringify({notificationFailureTrace: rows, dropped}).length > 8192) {rows.shift(); dropped++;}
    console.log(JSON.stringify({notificationFailureTrace: rows, dropped}));
  }};
}
async function checkControlSpacing(page) {
  const buttons = await page.locator("#browser-notifications button").all();
  const bounds = await Promise.all(buttons.map(button => button.boundingBox()));
  const viewport = page.viewportSize();
  for (let i = 0; i < bounds.length; i++) {
    assert.ok(bounds[i] && bounds[i].x >= 0 && bounds[i].x + bounds[i].width <= viewport.width, "notification control fits the viewport");
    if (i) assert.ok(bounds[i].y >= bounds[i - 1].y + bounds[i - 1].height + 8 ||
      bounds[i].x >= bounds[i - 1].x + bounds[i - 1].width + 8, "notification controls have space between them");
  }
}
async function checkWorkerUpgrade(browser, engine) {
  const context = await browser.newContext({ignoreHTTPSErrors: true});
  const trace = failureTrace(context);
  let retained, controls;
  const stage = async name => {
    const response = await context.request.post(`${base}/fixture/worker-stage/${name}`, {headers: {Origin: base}});
    assert.equal(response.status(), 200, "fixture stage selection succeeds");
  };
  try {
    await stage("old");
    retained = await context.newPage(); await retained.goto(`${base}/example/#codex`);
    await retained.evaluate(async () => {
      await navigator.serviceWorker.register("/notification-worker.js", {scope: "/", updateViaCache: "none"});
      await navigator.serviceWorker.ready;
    });
    await retained.waitForFunction(() => navigator.serviceWorker.controller?.state === "activated");
    await retained.locator('#message-form textarea[name="message"]').fill("Retain this unsent draft");
    await retained.evaluate(() => {globalThis.fixtureRetainedSentinel = true;});
    let navigations = 0;
    retained.on("framenavigated", frame => {if (frame === retained.mainFrame()) navigations++;});
    await stage("waiting");
    await retained.evaluate(async () => (await navigator.serviceWorker.getRegistration("/")).update());
    await retained.waitForFunction(async () => Boolean((await navigator.serviceWorker.getRegistration("/"))?.waiting));
    const priorController = await retained.evaluateHandle(() => navigator.serviceWorker.controller);
    // The already-waiting predecessor cannot handle a new activation message.
    // Fetch the actual successor at the same URL with its own install handler.
    await stage("current");
    controls = await context.newPage(); await controls.goto(`${base}/browser-notifications`);
    await expect(controls.locator("[data-notification-worker-status]")).toBeHidden({timeout: 12000});
    await expect.poll(() => controls.evaluate(async () => {
      const registration = await navigator.serviceWorker.getRegistration("/");
      return Boolean(registration?.active && !registration.waiting && !registration.installing &&
        navigator.serviceWorker.controller === registration.active && registration.active.state === "activated");
    })).toBe(true);
    await expect(controls.locator("[data-notification-worker-status]")).toBeHidden();
    assert.equal(await retained.evaluate(prior => navigator.serviceWorker.controller !== prior, priorController), true);
    assert.equal(retained.isClosed(), false); assert.equal(navigations, 0);
    assert.equal(await retained.evaluate(() => globalThis.fixtureRetainedSentinel), true);
    await expect(retained.locator('#message-form textarea[name="message"]')).toHaveValue("Retain this unsent draft");
    let controlsNavigations = 0;
    controls.on("framenavigated", frame => {if (frame === controls.mainFrame()) controlsNavigations++;});
    const capability = await controls.evaluate(() => new Promise(resolve => {
      const channel = new MessageChannel(), controller = navigator.serviceWorker.controller;
      const timer = setTimeout(() => {channel.port1.close(); resolve(false);}, 1500);
      channel.port1.onmessage = event => {clearTimeout(timer); channel.port1.close(); resolve(event.data?.version === 1);};
      controller.postMessage({type: "notification-test-capabilities"}, [channel.port2]);
    }));
    assert.equal(capability, true, "current document controller supports detailed Test outcomes");
    if (engine !== chromium) {
      console.log(`${engine.name()}: successor activated over old active/waiting workers; both tabs, draft and current-controller capability retained; no Playwright worker delivery claim`);
      return;
    }
    const subscription = await (await context.request.get(`${base}/fixture/subscription`)).json();
    await context.grantPermissions(["notifications"], {origin: base});
    await controls.evaluate(sub => {
      let subscribed = true;
      const fake = {options: {}, toJSON: () => ({...sub, expirationTime: null}), unsubscribe: async () => {subscribed = false; return true;}};
      PushManager.prototype.subscribe = async () => fake;
      PushManager.prototype.getSubscription = async () => subscribed ? fake : null;
    }, subscription);
    const registerReply = controls.waitForResponse(response => new URL(response.url()).pathname === "/api/browser-notifications/register" && response.request().method() === "POST");
    await controls.locator("[data-notification-enable]").click();
    assert.equal((await registerReply).status(), 200);
    await expect(controls.locator("[data-notification-test]")).toBeEnabled();
    // Document controller equality already proved takeover. Select the worker
    // carrying the actual successor's test-display implementation.
    const successor = await Promise.any(context.serviceWorkers().map(async candidate => {
      assert.equal(await candidate.evaluate(() => typeof displayTest), "function"); return candidate;
    }));
    await successor.evaluate(() => {self.registration.showNotification = async () => {};});
    const queued = controls.waitForResponse(response => new URL(response.url()).pathname === "/api/browser-notifications/test" && response.request().method() === "POST");
    await controls.locator("[data-notification-test]").click();
    const descriptor = (await (await queued).json()).test;
    await successor.evaluate(descriptor => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify({...descriptor, version: 1, kind: "test", slug: "", route: "/"})})), descriptor);
    await expect(controls.locator("[data-notification-test-status]")).toContainText("browser accepted the display request");
    assert.equal(controlsNavigations, 0); assert.equal(retained.isClosed(), false); assert.equal(navigations, 0);
    assert.equal(await retained.evaluate(() => globalThis.fixtureRetainedSentinel), true);
    await expect(retained.locator('#message-form textarea[name="message"]')).toHaveValue("Retain this unsent draft");
    await controls.locator("[data-notification-disable]").click();
    await expect(controls.locator("[data-notification-status]")).toContainText("Disabled on this browser");
    console.log(`${engine.name()}: successor activated over an old active and waiting worker with both tabs and draft retained; synthetic Test outcome correlated`);
  } catch (error) {trace.record({event: "upgrade_fixture_failure", category: safeError(error)}); await trace.dump(controls || retained); throw error;}
  finally {await stage("current"); await context.close();}
}
(async () => {
  for (const engine of [chromium, firefox]) {
    const browser = await engine.launch({headless: true, ...(engine === chromium ? {channel: "chromium", args: ["--ignore-certificate-errors"]} : {})});
    let page, trace;
    try {
      const context = await browser.newContext({ignoreHTTPSErrors: true});
      trace = failureTrace(context);
      page = await context.newPage();
      const errors = [], mutations = [];
      context.on("page", next => next.on("pageerror", error => {errors.push(safeError(error)); trace.record({event: "local_exception", category: safeError(error)});}));
      page.on("pageerror", error => {errors.push(safeError(error)); trace.record({event: "local_exception", category: safeError(error)});});
      page.on("request", request => {if (request.method() === "POST" && new URL(request.url()).pathname.startsWith("/api/browser-notifications")) mutations.push("notification_post");});
      await page.goto(base);
      await expect(page.locator("#browser-notifications")).toHaveCount(0);
      const toggle = page.getByRole("button", {name: "Workspace menu", exact: true});
      await toggle.focus(); await page.keyboard.press("Enter");
      await expect(page.getByRole("link", {name: "Operations", exact: true})).toBeFocused();
      await page.keyboard.press("Escape"); await expect(toggle).toBeFocused();
      await toggle.click();
      await page.getByRole("link", {name: "Browser notifications", exact: true}).click();
      await page.waitForURL("**/browser-notifications");
      await expect(page.locator("[data-notification-status]")).toHaveText("Disabled on this browser.");
      await checkControlSpacing(page);
      await page.setViewportSize({width: 375, height: 812});
      await checkControlSpacing(page);
      await page.getByRole("link", {name: "Back to workspace", exact: true}).click();
      await expect(page.locator("#browser-notifications")).toHaveCount(0);
      await toggle.click();
      await page.getByRole("link", {name: "Browser notifications", exact: true}).click();
      await expect(page.locator("[data-notification-status]")).toHaveText("Disabled on this browser.");
      await page.goto(`${base}/example/#codex`);
      await expect(page.locator("#browser-notifications")).toBeHidden();
      await expect(page.locator("[data-notification-status]")).toHaveText("Disabled on this browser.");
      await page.locator("#session-tab-settings").click();
      await expect(page.locator("#browser-notifications")).toBeVisible();
      await checkControlSpacing(page);
      await page.setViewportSize({width: 1280, height: 720});
      await checkControlSpacing(page);
      await expect(page.locator("#session-notifications-enabled")).toBeChecked();
      await expect(page.locator("#session-attention-status")).toHaveCount(0);
      assert.equal(await page.evaluate(() => Notification.permission), "default", "page load did not ask for permission");
      assert.deepEqual(mutations, [], "opening or revealing controls never changes a subscription");
      assert.equal(await page.evaluate(async () => Boolean(await navigator.serviceWorker.getRegistration("/"))), false, "opening controls did not register a worker");
      const workerAsset = await context.request.get(`${base}/notification-worker.js`);
      assert.equal(workerAsset.status(), 200);
      assert.equal(workerAsset.headers()["service-worker-allowed"], "/");
      assert.ok((await workerAsset.text()).includes('event.waitUntil(display(payload))'));
      if (engine === chromium) {
        const sub = await (await context.request.get(`${base}/fixture/subscription`)).json();
        await context.grantPermissions(["notifications"], {origin: base});
        const installSubscription = sub => {
          let subscribed = true;
          const fake = {options: {}, toJSON: () => ({...sub, expirationTime: null}), unsubscribe: async () => {subscribed = false; return true;}};
          PushManager.prototype.subscribe = async () => fake;
          PushManager.prototype.getSubscription = async () => subscribed ? fake : null;
        };
        await context.addInitScript(installSubscription, sub);
        await page.evaluate(installSubscription, sub);
        const registerReply = page.waitForResponse(response => new URL(response.url()).pathname === "/api/browser-notifications/register" && response.request().method() === "POST");
        trace.record({event: "enable_click"});
        await page.locator("[data-notification-enable]").click();
        const registerResponse = await registerReply;
        assert.ok(registerResponse.status() >= 200 && registerResponse.status() < 300, "register response must succeed");
        trace.record({event: "register_accepted"});
        await expect(page.locator("[data-notification-status]")).toContainText("Enabled on this browser");
        const worker = context.serviceWorkers()[0]; assert.ok(worker, "real root service worker registered through CSP");
        await worker.evaluate(() => {
          self.fixtureDisplays = [];
          self.registration.showNotification = async (title, options) => self.fixtureDisplays.push({title, ...options});
        });
        const queued = page.waitForResponse(response => response.url().endsWith("/api/browser-notifications/test") && response.request().method() === "POST");
        await page.locator("[data-notification-test]").click();
        const descriptor = (await (await queued).json()).test;
        await expect(page.locator("[data-notification-status]")).toContainText("A test notification will be sent in 10 seconds.");
        await page.close(); assert.equal(context.pages().length, 0);
        await worker.evaluate(descriptor => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify({...descriptor, version: 1, kind: "test", slug: "", route: "/"})})), descriptor);
        await expect.poll(() => worker.evaluate(async descriptor => {
          const outcome = (await workspaceAttentionStore.read(descriptor.identity))?.testOutcome;
          return workspaceAttentionStore.testMatches(outcome, descriptor) && outcome.display;
        }, descriptor)).toBe("resolved");
        const testReopened = await context.newPage(); await testReopened.goto(`${base}/browser-notifications`);
        await expect(testReopened.locator("[data-notification-test-status]")).toContainText("browser accepted the display request");
        await expect(testReopened.locator("[data-notification-test-status]")).toContainText("unconfirmed");
        await testReopened.close();
        const payload = {version: 1, identity: await worker.evaluate(async () => (await (await fetch("/api/sessions/example/attention")).json()).identity), id: "b".repeat(64), revision: 1, slug: "example", kind: "input", member: "implementer0", route: "/example/?member=implementer0#codex", expiresAt: Date.now() + 60000};
        assert.equal(context.pages().length, 0);
        await worker.evaluate(payload => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify(payload)})), payload);
        await expect.poll(() => worker.evaluate(() => self.fixtureDisplays.length)).toBe(2);
        assert.equal((await worker.evaluate(() => self.fixtureDisplays[0])).renotify, false);
        const reopened = await context.newPage(); await reopened.goto(`${base}/example/#settings`);
        await expect(reopened.locator("[data-notification-disable]")).toBeEnabled();
        await reopened.locator("[data-notification-disable]").click();
        await expect(reopened.locator("[data-notification-status]")).toContainText("Disabled on this browser");
      }
      assert.deepEqual(errors, [], `${engine.name()} loaded real notification UI without script errors`);
      console.log(`${engine.name()}: real template, CSP, permission separation and worker route passed${engine === chromium ? "; registered worker received synthetic push with zero tabs" : ""}`);
      await context.close();
      await checkWorkerUpgrade(browser, engine);
    } catch (error) {
      if (trace) {trace.record({event: "fixture_failure", category: safeError(error)}); await trace.dump(page);}
      throw error;
    } finally {await browser.close();}
  }
})().catch(error => {console.error(JSON.stringify({fixtureFailure: safeError(error)})); process.exitCode = 1;});
