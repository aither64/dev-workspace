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
    // The real browser API and shared panel persist the choice without any
    // registration, permission or subscription action. This is option plumbing,
    // not evidence that the OS played or suppressed an audible sound.
    assert.equal(await controls.evaluate(() => "silent" in Notification.prototype), true);
    const sound = controls.locator("[data-notification-sound]");
    await expect(sound).toBeEnabled(); await expect(sound).toBeChecked();
    let registrationWrites = 0;
    const trackRegistration = request => {
      if (request.method() === "POST" && ["/api/browser-notifications/register", "/api/browser-notifications/disable"].includes(new URL(request.url()).pathname)) registrationWrites++;
    };
    context.on("request", trackRegistration);
    const beforeNativeRead = await controls.evaluate(async () => {
      globalThis.fixtureReadSoundSubscription = async registration => {
        let subscription;
        try {subscription = await registration.pushManager.getSubscription();}
        catch (error) {
          if (!(error instanceof DOMException) || !["AbortError", "InvalidStateError", "NotAllowedError", "NotSupportedError", "SecurityError", "UnknownError"].includes(error.name)) {
            throw new Error("Native subscription read failed unexpectedly.");
          }
          return {read: "unavailable", category: error.name};
        }
        // Serialization is deliberately outside the native-read catch. Keep
        // this identity solely in page memory, including actual readable null.
        const identity = subscription === null ? null : JSON.stringify(subscription.toJSON());
        if (subscription !== null && typeof identity !== "string") throw new Error("Native subscription identity could not be serialized.");
        return {read: "readable", category: null, identity};
      };
      globalThis.fixtureSoundRegistration = await navigator.serviceWorker.getRegistration("/");
      if (!fixtureSoundRegistration) throw new Error("Sound fixture requires an existing native registration.");
      globalThis.fixtureSoundSubscription = await fixtureReadSoundSubscription(fixtureSoundRegistration);
      return {read: fixtureSoundSubscription.read, category: fixtureSoundSubscription.category};
    });
    if (engine === chromium) assert.equal(beforeNativeRead.read, "readable", "Chromium native subscription is readable before sound save");
    await sound.click();
    await expect.poll(() => controls.evaluate(() => workspaceAttentionStore.readSound())).toBe(false);
    await expect(sound).toBeEnabled(); await expect(sound).not.toBeChecked();
    const afterNativeRead = await controls.evaluate(async () => {
      const registration = await navigator.serviceWorker.getRegistration("/");
      if (!registration) throw new Error("Sound fixture lost its native registration.");
      const after = await fixtureReadSoundSubscription(registration), before = fixtureSoundSubscription;
      return {registrationSame: registration === fixtureSoundRegistration, read: after.read, category: after.category,
        identitySame: before.read === "readable" && after.read === "readable" && before.identity === after.identity};
    });
    assert.equal(afterNativeRead.registrationSame, true, "sound preference retains the same native registration");
    assert.equal(afterNativeRead.read, beforeNativeRead.read, "native subscription-read availability is unchanged after sound save");
    assert.equal(afterNativeRead.category, beforeNativeRead.category, "native subscription-read category is unchanged after sound save");
    if (afterNativeRead.read === "readable") assert.equal(afterNativeRead.identitySame, true, "sound preference retains readable native subscription identity");
    else assert.equal(engine === firefox, true, "only Firefox may leave native subscription identity unverified");
    // Two recognized unavailable reads verify category consistency, never
    // subscription identity or absence. Only finite observations leave the page.
    console.log(JSON.stringify({nativeSoundSubscription: {engine: engine.name(), read: afterNativeRead.read, category: afterNativeRead.category,
      registrationSame: afterNativeRead.registrationSame, identityVerified: afterNativeRead.read === "readable" && afterNativeRead.identitySame}}));
    await controls.reload();
    await expect(controls.locator("[data-notification-sound]")).toBeEnabled();
    await expect(controls.locator("[data-notification-sound]")).not.toBeChecked();
    const sessionSound = retained.locator("#browser-notifications [data-notification-sound]");
    assert.equal(navigations, 0, "retained page has no unexpected navigation before Settings setup");
    const settingsTransition = retained.waitForEvent("framenavigated", {
      predicate: frame => frame === retained.mainFrame() && frame.url() === `${base}/example/#settings`, timeout: 5000,
    });
    await Promise.all([settingsTransition, retained.locator("#session-tab-settings").click()]);
    assert.equal(navigations, 1, "Settings setup causes exactly one retained main-frame transition");
    assert.equal(await retained.evaluate(() => location.hash === "#settings" && document.getElementById("settings")?.classList.contains("active")), true,
      "retained page is in Settings after the intended transition");
    navigations = 0;
    await expect(sessionSound).toBeEnabled(); await expect(sessionSound).not.toBeChecked();
    await sessionSound.click();
    await expect.poll(() => retained.evaluate(() => workspaceAttentionStore.readSound())).toBe(true);
    await expect(sessionSound).toBeEnabled(); await expect(sessionSound).toBeChecked();
    await expect(retained.locator("#sidebar-menu [data-notification-sound]")).toBeEnabled();
    await expect(retained.locator("#sidebar-menu [data-notification-sound]")).toBeChecked();
    await controls.reload();
    await expect(controls.locator("[data-notification-sound]")).toBeEnabled();
    await expect(controls.locator("[data-notification-sound]")).toBeChecked();
    assert.equal(registrationWrites, 0, "sound controls perform no subscription mutation");
    context.off("request", trackRegistration);
    // Reloads above are explicit fixture actions. The application still never
    // reloads either page to install the worker or change this preference.
    controlsNavigations = 0;
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
    await successor.evaluate(() => {
      self.fixtureSoundDisplays = [];
      self.registration.showNotification = async (_title, options) => self.fixtureSoundDisplays.push(options);
    });
    const queued = controls.waitForResponse(response => new URL(response.url()).pathname === "/api/browser-notifications/test" && response.request().method() === "POST");
    await controls.locator("[data-notification-test]").click();
    const descriptor = (await (await queued).json()).test;
    await successor.evaluate(descriptor => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify({...descriptor, registrationId:descriptor.identity.slice(5), version: 1, kind: "test", slug: "", route: "/"})})), descriptor);
    await expect(controls.locator("[data-notification-test-status]")).toContainText("browser accepted the display request");
    assert.equal(await successor.evaluate(() => Object.hasOwn(self.fixtureSoundDisplays[0], "silent")), false);
    await expect(sound).toBeEnabled(); await expect(sound).toBeChecked();
    await sound.click();
    await expect.poll(() => controls.evaluate(() => workspaceAttentionStore.readSound())).toBe(false);
    await expect(sound).toBeEnabled(); await expect(sound).not.toBeChecked();
    const queuedSilent = controls.waitForResponse(response => new URL(response.url()).pathname === "/api/browser-notifications/test" && response.request().method() === "POST");
    await controls.locator("[data-notification-test]").click();
    const silentDescriptor = (await (await queuedSilent).json()).test;
    await successor.evaluate(descriptor => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify({...descriptor, registrationId:descriptor.identity.slice(5), version: 1, kind: "test", slug: "", route: "/"})})), silentDescriptor);
    await expect(controls.locator("[data-notification-test-status]")).toContainText("browser accepted the display request");
    assert.equal(await successor.evaluate(() => self.fixtureSoundDisplays.at(-1).silent), true);
    const sessionPayload = await controls.evaluate(async () => {
      const status = await (await fetch("/api/sessions/example/attention")).json();
      return {version: 1, registrationId:(await workspaceAttentionStore.readDeliveryGate()).registrationId, identity: status.identity, id: "b".repeat(64), revision: 1, slug: "example", kind: "input",
        member: "implementer0", route: "/example/?member=implementer0#codex", expiresAt: Date.now() + 60000};
    });
    await successor.evaluate(payload => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify(payload)})), sessionPayload);
    await expect.poll(() => successor.evaluate(() => self.fixtureSoundDisplays.length)).toBe(3);
    assert.equal(await successor.evaluate(() => self.fixtureSoundDisplays.at(-1).silent), true);
    // Invoke the actual click listener with a synthetic event. Native focus may
    // reject without trusted user activation; only no-navigation/draft plumbing
    // is asserted here, never physical Firefox/macOS selection.
    const retainedURL = retained.url(), pageCount = context.pages().length;
    await controls.bringToFront();
    const click = await successor.evaluate(async payload => {
      let work, closed = false;
      const event = new Event("notificationclick");
      Object.defineProperties(event, {
        notification: {value: {data: {route: payload.route, identity: payload.identity}, close() {closed = true;}}},
        waitUntil: {value: promise => {work = promise;}},
      });
      self.dispatchEvent(event);
      let timer;
      const completion = await Promise.race([Promise.resolve(work).then(() => "resolved", () => "rejected"),
        new Promise(resolve => {timer = setTimeout(() => resolve("timeout"), 1500);})]);
      clearTimeout(timer);
      return {closed, dispatched: Boolean(work), completion};
    }, sessionPayload);
    assert.equal(click.closed, true); assert.equal(click.dispatched, true);
    assert.ok(["resolved", "rejected"].includes(click.completion), "synthetic focus settles within its bound");
    assert.equal(context.pages().length, pageCount); assert.equal(retained.url(), retainedURL);
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
      const menu = page.locator("#sidebar-menu");
      const menuDelivery = menu.locator("[data-notification-delivery]");
      await expect(menuDelivery).toBeEnabled(); await expect(menuDelivery).not.toBeChecked();
      assert.equal(await menuDelivery.evaluate(element => element.indeterminate), false, "menu confirms initial browser setup off");
      await expect(menu.locator("[data-notification-menu-error]")).toBeHidden();
      await expect(menu.locator("input[type=checkbox]")).toHaveCount(2);
      await expect(menu.locator("button, [data-notification-status], [data-notification-worker-status], [data-notification-sound-status]")).toHaveCount(0);
      const menuSound = menu.locator("[data-notification-sound]");
      await expect(menuSound).toBeEnabled(); await expect(menuSound).toBeChecked();
      await menuSound.click();
      await expect.poll(() => page.evaluate(() => workspaceAttentionStore.readSound())).toBe(false);
      await expect(menuSound).toBeEnabled(); await expect(menuSound).not.toBeChecked();
      await page.setViewportSize({width: 375, height: 812});
      for (const row of await menu.locator(".checkbox, .menu-notifications > a").all()) {
        const bounds = await row.boundingBox();
        assert.ok(bounds && bounds.x >= 0 && bounds.x + bounds.width <= 375, "quick controls fit a narrow menu");
      }
      await page.getByRole("link", {name: "Notification settings", exact: true}).click();
      await page.waitForURL("**/browser-notifications");
      await expect(page.locator("#browser-notifications [data-notification-status]")).toHaveText("Disabled on this browser.");
      await expect(page.locator("#browser-notifications [data-notification-sound]")).not.toBeChecked();
      await page.reload();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).toBeEnabled();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).not.toBeChecked();
      await checkControlSpacing(page);
      await page.setViewportSize({width: 375, height: 812});
      await checkControlSpacing(page);
      await page.getByRole("link", {name: "Back to workspace", exact: true}).click();
      await expect(page.locator("#browser-notifications")).toHaveCount(0);
      await toggle.click();
      await page.getByRole("link", {name: "Notification settings", exact: true}).click();
      await expect(page.locator("#browser-notifications [data-notification-status]")).toHaveText("Disabled on this browser.");
      await page.goto(`${base}/example/#codex`);
      await expect(page.locator("#browser-notifications")).toBeHidden();
      await expect(page.locator("#browser-notifications [data-notification-status]")).toHaveText("Disabled on this browser.");
      await page.locator("#session-tab-settings").click();
      await expect(page.locator("#browser-notifications")).toBeVisible();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).not.toBeChecked();
      await toggle.click();
      await expect(menuSound).toBeEnabled(); await expect(menuSound).not.toBeChecked();
      await menuSound.click();
      await expect.poll(() => page.evaluate(() => workspaceAttentionStore.readSound())).toBe(true);
      await expect(menuSound).toBeEnabled(); await expect(menuSound).toBeChecked();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).toBeEnabled();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).toBeChecked();
      // Pointer-driven saving may move focus when the input is disabled. Check
      // dismissal here, then keyboard restoration with known focus conditions.
      await page.keyboard.press("Escape");
      await expect.poll(() => menu.evaluate(element => element.matches(":popover-open"))).toBe(false);
      await toggle.focus(); await page.keyboard.press("Enter");
      await expect(page.getByRole("link", {name: "Operations", exact: true})).toBeFocused();
      assert.equal(await menu.evaluate(element => element.matches(":popover-open") && element.contains(document.activeElement)), true,
        "keyboard-open menu contains focus before Escape");
      await page.keyboard.press("Escape");
      await expect.poll(() => menu.evaluate(element => element.matches(":popover-open"))).toBe(false);
      await expect(toggle).toBeFocused();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).toBeEnabled();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).toBeChecked();
      await page.locator("#browser-notifications [data-notification-sound]").click();
      await expect.poll(() => page.evaluate(() => workspaceAttentionStore.readSound())).toBe(false);
      await expect(page.locator("#browser-notifications [data-notification-sound]")).toBeEnabled();
      await expect(page.locator("#browser-notifications [data-notification-sound]")).not.toBeChecked();
      await toggle.click();
      await expect(menuSound).toBeEnabled(); await expect(menuSound).not.toBeChecked();
      await page.keyboard.press("Escape");
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
        await toggle.click();
        await expect(menuDelivery).toBeEnabled(); await expect(menuDelivery).not.toBeChecked();
        await menuDelivery.click();
        const registerResponse = await registerReply;
        assert.ok(registerResponse.status() >= 200 && registerResponse.status() < 300, "register response must succeed");
        trace.record({event: "register_accepted"});
        await expect(menuDelivery).toBeEnabled(); await expect(menuDelivery).toBeChecked();
        await page.keyboard.press("Escape");
        await expect(page.locator("#browser-notifications [data-notification-status]")).toContainText("Enabled on this browser");
        const worker = context.serviceWorkers()[0]; assert.ok(worker, "real root service worker registered through CSP");
        await worker.evaluate(() => {
          self.fixtureDisplays = [];
          self.registration.showNotification = async (title, options) => self.fixtureDisplays.push({title, ...options});
        });
        const queued = page.waitForResponse(response => response.url().endsWith("/api/browser-notifications/test") && response.request().method() === "POST");
        await page.locator("#browser-notifications [data-notification-test]").click();
        const descriptor = (await (await queued).json()).test;
        await expect(page.locator("#browser-notifications [data-notification-status]")).toContainText("A test notification will be sent in 10 seconds.");
        await page.close(); assert.equal(context.pages().length, 0);
        await worker.evaluate(descriptor => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify({...descriptor, registrationId:descriptor.identity.slice(5), version: 1, kind: "test", slug: "", route: "/"})})), descriptor);
        await expect.poll(() => worker.evaluate(async descriptor => {
          const outcome = (await workspaceAttentionStore.read(descriptor.identity))?.testOutcome;
          return workspaceAttentionStore.testMatches(outcome, descriptor) && outcome.display;
        }, descriptor)).toBe("resolved");
        const testReopened = await context.newPage(); await testReopened.goto(`${base}/browser-notifications`);
        await expect(testReopened.locator("[data-notification-test-status]")).toContainText("browser accepted the display request");
        await expect(testReopened.locator("[data-notification-test-status]")).toContainText("unconfirmed");
        await testReopened.close();
        const payload = {version: 1, registrationId:await worker.evaluate(async()=>(await workspaceAttentionStore.readDeliveryGate()).registrationId), identity: await worker.evaluate(async () => (await (await fetch("/api/sessions/example/attention")).json()).identity), id: "b".repeat(64), revision: 1, slug: "example", kind: "input", member: "implementer0", route: "/example/?member=implementer0#codex", expiresAt: Date.now() + 60000};
        assert.equal(context.pages().length, 0);
        await worker.evaluate(payload => self.dispatchEvent(new PushEvent("push", {data: JSON.stringify(payload)})), payload);
        await expect.poll(() => worker.evaluate(() => self.fixtureDisplays.length)).toBe(2);
        assert.equal((await worker.evaluate(() => self.fixtureDisplays[0])).renotify, false);
        const reopened = await context.newPage(); await reopened.goto(`${base}/example/#settings`);
        await expect(reopened.locator("#browser-notifications [data-notification-disable]")).toBeEnabled();
        await reopened.getByRole("button", {name: "Workspace menu", exact: true}).click();
        const reopenedDelivery = reopened.locator("#sidebar-menu [data-notification-delivery]");
        await expect(reopenedDelivery).toBeEnabled(); await expect(reopenedDelivery).toBeChecked();
        await reopenedDelivery.click();
        await expect(reopenedDelivery).toBeEnabled(); await expect(reopenedDelivery).not.toBeChecked();
        await expect(reopened.locator("#browser-notifications [data-notification-status]")).toContainText("Disabled on this browser");
        const beforeOff = await worker.evaluate(()=>self.fixtureDisplays.length);
        await worker.evaluate(payload=>display(payload),payload);
        assert.equal(await worker.evaluate(()=>self.fixtureDisplays.length),beforeOff,"off epoch has no neutral popup");
        await reopenedDelivery.click();
        await expect(reopenedDelivery).toBeEnabled(); await expect(reopenedDelivery).toBeChecked();
        const freshId=await reopened.evaluate(async()=>(await workspaceAttentionStore.readDeliveryGate()).registrationId);
        assert.notEqual(freshId,payload.registrationId,"reenabling establishes a fresh transport epoch");
        await worker.evaluate(payload=>display(payload),payload);
        assert.equal(await worker.evaluate(()=>self.fixtureDisplays.length),beforeOff,"old provider delivery stays dropped after reenable");
        await worker.evaluate(payload=>self.dispatchEvent(new PushEvent("push",{data:JSON.stringify(payload)})),{...payload,registrationId:freshId});
        await expect.poll(()=>worker.evaluate(()=>self.fixtureDisplays.length)).toBe(beforeOff+1);
        await reopenedDelivery.click();
        await expect(reopenedDelivery).toBeEnabled(); await expect(reopenedDelivery).not.toBeChecked();
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
