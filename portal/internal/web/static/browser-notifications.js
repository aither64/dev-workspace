(() => {
  "use strict";
  const key = "workspace-browser-notifications.v1";
  const cleanupKey = `${key}.cleanup`;
  const panel = document.getElementById("browser-notifications");
  if (!panel) return;
  const status = panel.querySelector("[data-notification-status]"), enable = panel.querySelector("[data-notification-enable]"),
    disable = panel.querySelector("[data-notification-disable]"), test = panel.querySelector("[data-notification-test]");
  const testStatus = panel.querySelector("[data-notification-test-status]"), providerStatus = panel.querySelector("[data-notification-provider-status]"),
    workerStatus = panel.querySelector("[data-notification-worker-status]");
  const store = globalThis.workspaceAttentionStore;
  let pendingTest = null, pollTimer = null, pollGeneration = 0, pageClosed = false, updatingWorker = false, resumeRetired = false;
  let refreshGeneration = 0, observationRetired = false;
  const setText = (element, text) => { if (element) { element.textContent = text; element.hidden = !text; } };
  const localResult = async promise => {
    let timer;
    try { return await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error()), 1000); })]); }
    finally { clearTimeout(timer); }
  };
  const stopTest = () => { pollGeneration++; clearTimeout(pollTimer); pollTimer = null; };
  const cancelTest = () => {
    stopTest();
    resumeRetired = true;
    const descriptor = pendingTest;
    pendingTest = null;
    setText(testStatus, "");
    if (!descriptor || !store) return Promise.resolve();
    const retirement = store.update(descriptor.identity, value => {
      if (store.testMatches(value.testPending, descriptor)) delete value.testPending;
      return value;
    });
    // Enable/Disable compensation must not wait on diagnostic storage. A new
    // explicit Test does await retirement before submitting another request.
    void retirement.catch(() => {});
    return retirement;
  };
  const lastProvider = value => ({provider_accepted: "The push service accepted a notification.", retrying: "The push service is unavailable; delivery will retry.",
    provider_refused: "The push service refused a notification.", delivery_failed: "A notification could not be sent.", expired: "A notification expired."})[value];
  async function workerReadiness(registration, update = false, current = () => true) {
    if (!registration) return;
    if (update && !updatingWorker) {
      updatingWorker = true;
      try { await registration.update?.(); } catch (_) { if (current()) setText(workerStatus, "The notification update could not be checked. Try reopening this page."); }
    }
    if (!current()) return;
    if (registration.waiting || registration.installing) {
      setText(workerStatus, "A notification update is waiting. Close portal tabs, then reopen this page.");
      return;
    }
    if (!registration.active || !globalThis.MessageChannel) {
      setText(workerStatus, "The notification update could not be confirmed. Test results may be limited.");
      return;
    }
    const supported = await new Promise(resolve => {
      const channel = new MessageChannel();
      const timer = setTimeout(() => { channel.port1.close(); resolve(false); }, 1500);
      channel.port1.onmessage = event => { clearTimeout(timer); channel.port1.close(); resolve(event.data?.version === 1); };
      try { registration.active.postMessage({type: "notification-test-capabilities"}, [channel.port2]); }
      catch (_) { clearTimeout(timer); channel.port1.close(); resolve(false); }
    });
    if (current()) setText(workerStatus, supported ? "" : "Test results are limited until the notification update is active. Close portal tabs, then reopen this page.");
  }
  async function pollTest() {
    stopTest();
    const generation = pollGeneration, descriptor = pendingTest;
    if (!descriptor || pageClosed || !registered || descriptor.identity !== `test-${registrationId}`) return;
    let record, storageFailed = false;
    try { record = await localResult(store?.read(descriptor.identity)); } catch (_) { storageFailed = true; }
    if (generation !== pollGeneration || pageClosed) return;
    const current = store?.testDescriptor?.(record?.testPending), latest = store?.testDescriptor?.(record?.testOutcome);
    if ([current, latest].some(item => item && item.revision >= descriptor.revision && !store.testMatches(item, descriptor))) {
      setText(testStatus, "Another test replaced this check. Send a new test to check this page.");
      pendingTest = null;
      return;
    }
    const outcome = record?.testOutcome, matches = store?.testMatches?.(outcome, descriptor);
    let text = "Waiting for this test to reach the browser.";
    let complete = false;
    if (matches && outcome.received === true) {
      if (outcome.display === "rejected") {
        text = `This test reached the browser, but the browser rejected the display request (${store.testError({name: outcome.error})}).`;
        complete = true;
      } else if (outcome.display === "resolved") {
        text = "This test reached the browser and the browser accepted the display request. ";
        text += outcome.browserRecord === "present" ? "A matching notification is recorded. Check your screen to confirm it appeared." :
          outcome.browserRecord === "absent" ? "No matching notification is recorded. Appearance on your screen is unconfirmed." :
          "The notification record could not be checked. Appearance on your screen is unconfirmed.";
        complete = true;
      } else text = "This test reached the browser. Waiting for its display result.";
    }
    if (storageFailed || !store) text = "This browser could not read the test result. Appearance on your screen is unconfirmed.";
    const expired = Date.now() >= descriptor.expiresAt + 10000;
    if (expired && !complete && !storageFailed) text = matches && outcome.received ?
      "This test reached the browser, but its display result was not confirmed before the check ended." :
      "This test was not confirmed by the browser before the check ended. Appearance on your screen is unconfirmed.";
    setText(testStatus, text);
    status.textContent = "Enabled on this browser.";
    // The provider status is registration-wide. It never certifies this test.
    try {
      const state = await api(`?registrationId=${encodeURIComponent(registrationId)}`);
      if (generation !== pollGeneration || pageClosed) return;
      setText(providerStatus, lastProvider(state.result) ? `Last push-service result; it may be from another notification: ${lastProvider(state.result)}` : "");
    } catch (_) {}
    if (complete || expired || !store) return;
    if (generation === pollGeneration && !pageClosed) pollTimer = setTimeout(() => void pollTest(), 2000);
  }
  async function resumeTest() {
    if (!store?.read || !registered || busy || pageClosed || pendingTest || resumeRetired) return;
    const generation = pollGeneration, identity = `test-${registrationId}`;
    try {
      const record = await localResult(store.read(identity)), saved = store.testDescriptor(record?.testPending);
      if (generation !== pollGeneration || pageClosed || !registered || busy || resumeRetired || identity !== `test-${registrationId}` || pendingTest) return;
      if (saved && saved.identity === identity && saved.expiresAt <= Date.now() + 70000) {
        pendingTest = saved;
        void pollTest();
      }
    } catch (_) {}
  }
  const supported = globalThis.isSecureContext && "serviceWorker" in navigator && "PushManager" in globalThis && "Notification" in globalThis;
  let registrationId = "", busy = false, registered = false, localSubscription = null, localKnown = false, pendingCleanup = null;
  const incomplete = "Browser setup is incomplete. Disable it or enable notifications again.";
  const disablingIncomplete = "Disabling is incomplete. Retry Disable.";
  const serverPending = "Browser subscription removed; server cleanup is still pending. Retry Disable.";
  const digestPattern = /^[0-9a-f]{64}$/;
  const readCleanup = () => {
    try {
      const saved = localStorage.getItem(cleanupKey);
      if (saved && saved.length <= 128) {
        const value = JSON.parse(saved), fields = value && Object.keys(value);
        if (fields?.length === 1 && ["registrationId", "endpointDigest"].includes(fields[0]) &&
            typeof value[fields[0]] === "string" && digestPattern.test(value[fields[0]])) pendingCleanup = value;
      }
    } catch (_) {}
  };
  const saveCleanup = selector => {
    pendingCleanup = selector;
    try { localStorage.setItem(cleanupKey, JSON.stringify(selector)); } catch (_) {}
  };
  const subscriptionSelector = async subscription => {
    const endpoint = subscription.endpoint || subscription.toJSON().endpoint;
    if (typeof endpoint !== "string" || !endpoint) throw new Error();
    const sum = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(endpoint));
    return {endpointDigest: [...new Uint8Array(sum)].map(byte => byte.toString(16).padStart(2, "0")).join("")};
  };
  const clearRegistration = id => {
    if (!id || registrationId !== id) return;
    try {
      localStorage.removeItem(key);
      if (!localStorage.getItem(key)) registrationId = "";
    } catch (_) {}
  };
  const clearCleanup = () => {
    try {
      localStorage.removeItem(cleanupKey);
      if (localStorage.getItem(cleanupKey)) return false;
      pendingCleanup = null;
      return true;
    } catch (_) { return false; }
  };
  const cleanupStatus = () => localKnown && !localSubscription ? serverPending : disablingIncomplete;
  try { registrationId = localStorage.getItem(key) || ""; } catch (_) {}
  readCleanup();
  const api = async (path, body) => {
    const controller = new AbortController(), deadline = setTimeout(() => controller.abort(), 8000);
    try {
      const response = await fetch(`/api/browser-notifications${path}`, {cache: "no-store", credentials: "same-origin", signal: controller.signal,
        ...(body ? {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body)} : {})});
      const value = await response.json(); if (!response.ok) throw new Error(value.error || "Browser notifications are unavailable."); return value;
    } finally { clearTimeout(deadline); }
  };
  const buttons = () => {
    enable.disabled = busy || !supported || Notification.permission === "denied" || Boolean(pendingCleanup);
    disable.disabled = busy || (!localSubscription && !registrationId && !pendingCleanup);
    test.disabled = busy || !registered || Boolean(pendingCleanup);
  };
  async function refresh(resume = true) {
    if (busy || pageClosed || observationRetired) return;
    const generation = ++refreshGeneration, identity = registrationId;
    const current = () => generation === refreshGeneration && identity === registrationId && !busy && !pageClosed && !observationRetired;
    if (!supported) { status.textContent = "This browser build does not support background Web Push. Use a browser with Push API support."; buttons(); return; }
    let observedSubscription = localSubscription, observedKnown = localKnown;
    try {
      const local = await navigator.serviceWorker.getRegistration("/");
      observedSubscription = await local?.pushManager.getSubscription();
      observedKnown = true;
      if (!current()) return;
      const state = await api(`?registrationId=${encodeURIComponent(identity)}`);
      if (!current()) return;
      localSubscription = observedSubscription; localKnown = observedKnown;
      setText(providerStatus, lastProvider(state.result) ? `Last push-service result; it may be from another notification: ${lastProvider(state.result)}` : "");
      registered = Boolean(!pendingCleanup && state.registered && localSubscription && Notification.permission === "granted");
      status.textContent = pendingCleanup ? cleanupStatus() : localSubscription && !registered ? incomplete : Notification.permission === "denied" ? "Notifications are blocked. Change this site's notification permission in your browser settings." :
        !state.available ? "Notification delivery is disabled on the server." : state.result === "expired" ? "The browser subscription expired. Enable notifications again." : registered ?
        "Enabled on this browser." : "Disabled on this browser.";
      if (resume) { void workerReadiness(local, true, current); void resumeTest(); }
    } catch (error) {
      if (!current()) return;
      localSubscription = observedSubscription; localKnown = observedKnown;
      registered = false; status.textContent = pendingCleanup ? cleanupStatus() : localSubscription ? incomplete : error.message;
    }
    if (current()) buttons();
  }
  enable.addEventListener("click", async () => {
    if (busy || !supported || Notification.permission === "denied") return;
    readCleanup();
    if (pendingCleanup) { status.textContent = cleanupStatus(); buttons(); return; }
    refreshGeneration++; observationRetired = false;
    cancelTest();
    busy = true; buttons();
    try {
      const permission = await Notification.requestPermission();
      if (permission !== "granted") throw new Error(permission === "denied" ? "Notifications are blocked. Change this site's notification permission in your browser settings." : "Notification permission was not granted.");
      const state = await api("");
      if (!state.available) throw new Error("Notification delivery is disabled on the server.");
      const registration = await navigator.serviceWorker.register("/notification-worker.js", {scope: "/", updateViaCache: "none"});
      await navigator.serviceWorker.ready;
      let subscription = await registration.pushManager.getSubscription();
      localSubscription = subscription;
      const applicationServerKey = Uint8Array.from(atob(state.publicKey.replace(/-/g, "+").replace(/_/g, "/")), char => char.charCodeAt(0));
      if (subscription && subscription.options.applicationServerKey &&
          [...new Uint8Array(subscription.options.applicationServerKey)].join() !== [...applicationServerKey].join()) {
        throw new Error("The server's push key changed. Disable this browser subscription, then enable it again.");
      }
      subscription ||= await registration.pushManager.subscribe({userVisibleOnly: true, applicationServerKey});
      localSubscription = subscription;
      const value = subscription.toJSON();
      const saved = await api("/register", {subscription: {endpoint: value.endpoint, keys: value.keys}, expirationTime: value.expirationTime ?? null});
      const replacement = registrationId && registrationId !== saved.registrationId;
      if (replacement) cancelTest();
      if (replacement) {
        // Do not overwrite an older deletion obligation with the new ID.
        saveCleanup(await subscriptionSelector(subscription));
        await api("/disable", {registrationId});
      }
      registrationId = saved.registrationId;
      localStorage.setItem(key, registrationId);
      if (localStorage.getItem(key) !== registrationId) throw new Error("Browser registration could not be saved. Try enabling notifications again.");
      if (replacement && !clearCleanup()) throw new Error();
      registered = true; status.textContent = "Enabled on this browser. Send a test notification to check delivery.";
      void workerReadiness(registration, true);
    } catch (error) { registered = false; status.textContent = pendingCleanup ? disablingIncomplete : localSubscription ? incomplete : `Registration failed: ${error.message}`; }
    finally { busy = false; buttons(); }
  });
  disable.addEventListener("click", async () => {
    if (busy) return;
    refreshGeneration++;
    cancelTest();
    busy = true; registered = false; buttons();
    let registration = null, subscription = localSubscription, selector = null, currentBound = false;
    readCleanup();
    try {
      registration = await navigator.serviceWorker.getRegistration("/");
      subscription = await registration?.pushManager.getSubscription() || subscription;
    } catch (_) {}
    selector = pendingCleanup;
    if (!registrationId && selector?.registrationId) {
      registrationId = selector.registrationId;
      try { localStorage.setItem(key, registrationId); } catch (_) {}
    }
    currentBound = !subscription;
    try {
      if (subscription) {
        selector = await subscriptionSelector(subscription);
        currentBound = true;
      }
    } catch (_) {}
    selector ||= registrationId ? {registrationId} : null;
    // The saved ID may predate a replacement whose register reply was lost.
    // Keep its obligation separately from the current subscription's digest.
    if (selector) saveCleanup(selector);
    const knownId = registrationId;
    const selectorRemoval = selector ? api("/disable", selector).then(() => true, () => false) : Promise.resolve(true);
    const idRemoval = knownId ? selector?.registrationId === knownId ? selectorRemoval :
      api("/disable", {registrationId: knownId}).then(() => true, () => false) : Promise.resolve(true);
    const [selectorAbsent, idAbsent] = await Promise.all([selectorRemoval, idRemoval]);
    if (idAbsent && currentBound) clearRegistration(knownId);
    const serverAbsent = currentBound && Boolean(selector || knownId) && selectorAbsent && idAbsent;
    // Local removal is independent of the server response. A false/throwing
    // unsubscribe is followed by a read, which establishes actual absence.
    try { if (subscription) await subscription.unsubscribe(); } catch (_) {}
    localKnown = false;
    try {
      registration ||= await navigator.serviceWorker.getRegistration("/");
      localSubscription = await registration?.pushManager.getSubscription() || null;
      localKnown = true;
    } catch (_) {}
    if (serverAbsent && localKnown && !localSubscription && !registrationId) clearCleanup();
    const complete = serverAbsent && localKnown && !localSubscription && !pendingCleanup && !registrationId;
    status.textContent = complete ? "Disabled on this browser. Site permission remains in your browser settings." :
      localKnown && !localSubscription && !serverAbsent ? serverPending : disablingIncomplete;
    busy = false; buttons();
  });
  test.addEventListener("click", async () => {
    if (busy || !registered) return;
    refreshGeneration++;
    busy = true; buttons();
    const retirement = cancelTest(), generation = pollGeneration, identity = registrationId;
    setText(testStatus, "A matching test result is unavailable. Appearance on your screen is unconfirmed.");
    try {
      await localResult(retirement);
      if (generation !== pollGeneration || pageClosed || identity !== registrationId || !registered) return;
      const response = await api("/test", {registrationId});
      if (generation !== pollGeneration || pageClosed || identity !== registrationId || !registered) return;
      stopTest();
      let descriptor = store?.testDescriptor?.(response.test);
      if (descriptor && (descriptor.identity !== `test-${registrationId}` || descriptor.expiresAt > Date.now() + 70000 || descriptor.expiresAt <= Date.now())) descriptor = null;
      if (!descriptor) cancelTest();
      pendingTest = descriptor;
      status.textContent = "A test notification will be sent in 10 seconds. Close portal tabs or switch windows to check background delivery.";
      setText(testStatus, pendingTest ? "Waiting for this test to reach the browser." : "A matching test result is unavailable. Appearance on your screen is unconfirmed.");
      if (pendingTest) {
        try { await localResult(store.saveTest(pendingTest)); } catch (_) { setText(testStatus, "This browser could not save the test check. Keep this page open to check the result."); }
        pollTimer = setTimeout(() => void pollTest(), 2000);
      }
    }
    catch (error) { status.textContent = error.message; }
    finally { busy = false; buttons(); }
  });
  globalThis.workspaceBrowserNotifications = {
    async presence(snapshot, focused, seen) {
      if (!focused || !registrationId || !registered) return;
      const eventId = snapshot.event && seen.includes(snapshot.event.id) ? snapshot.event.id : "";
      try { await fetch(`/api/sessions/${document.body.dataset.session}/attention`, {method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({identity: snapshot.identity, registrationId, eventId})}); } catch (_) {}
    },
  };
  navigator.serviceWorker?.addEventListener?.("message", event => {
    if (event.data?.type === "notification-test-update" && event.data.identity === pendingTest?.identity) void pollTest();
  });
  navigator.serviceWorker?.addEventListener?.("controllerchange", () => void refresh());
  document.addEventListener?.("visibilitychange", () => { if (document.visibilityState === "visible") { void refresh(); if (pendingTest) void pollTest(); } });
  globalThis.addEventListener?.("pagehide", () => { pageClosed = true; refreshGeneration++; stopTest(); });
  globalThis.addEventListener?.("pageshow", () => { pageClosed = false; void refresh(); if (pendingTest) void pollTest(); });
  globalThis.addEventListener?.("storage", event => {
    if ((event.key === key && event.newValue !== registrationId) || (event.key === cleanupKey && event.newValue)) {
      refreshGeneration++; observationRetired = true;
      cancelTest(); registered = false; buttons();
    }
  });
  void refresh();
})();
