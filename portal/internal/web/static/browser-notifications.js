(() => {
  "use strict";
  const key = "workspace-browser-notifications.v1";
  const cleanupKey = `${key}.cleanup`;
  const panel = document.getElementById("browser-notifications");
  if (!panel) return;
  const status = panel.querySelector("[data-notification-status]"), enable = panel.querySelector("[data-notification-enable]"),
    disable = panel.querySelector("[data-notification-disable]"), test = panel.querySelector("[data-notification-test]");
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
  async function refresh() {
    if (!supported) { status.textContent = "This browser build does not support background Web Push. Use a browser with Push API support."; buttons(); return; }
    try {
      const local = await navigator.serviceWorker.getRegistration("/");
      localSubscription = await local?.pushManager.getSubscription();
      localKnown = true;
      const state = await api(`?registrationId=${encodeURIComponent(registrationId)}`);
      registered = Boolean(!pendingCleanup && state.registered && localSubscription && Notification.permission === "granted");
      status.textContent = pendingCleanup ? cleanupStatus() : localSubscription && !registered ? incomplete : Notification.permission === "denied" ? "Notifications are blocked. Change this site's notification permission in your browser settings." :
        !state.available ? "Notification delivery is disabled on the server." : state.result === "expired" ? "The browser subscription expired. Enable notifications again." : registered ?
        ({expired: "The browser subscription expired. Enable notifications again.", retrying: "The provider is unavailable. Delivery will retry.", delivery_failed: "Delivery failed. Send a test notification to check it.", provider_refused: "The provider refused delivery. Enable notifications again.", provider_accepted: "The push service accepted the last notification. Check this browser to confirm it appeared."})[state.result] || "Enabled on this browser." : "Disabled on this browser.";
    } catch (error) { registered = false; status.textContent = pendingCleanup ? cleanupStatus() : localSubscription ? incomplete : error.message; }
    buttons();
  }
  enable.addEventListener("click", async () => {
    if (busy || !supported || Notification.permission === "denied") return;
    readCleanup();
    if (pendingCleanup) { status.textContent = cleanupStatus(); buttons(); return; }
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
    } catch (error) { registered = false; status.textContent = pendingCleanup ? disablingIncomplete : localSubscription ? incomplete : `Registration failed: ${error.message}`; }
    finally { busy = false; buttons(); }
  });
  disable.addEventListener("click", async () => {
    if (busy) return;
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
    busy = true; buttons();
    try { await api("/test", {registrationId}); status.textContent = "A test notification will be sent in 10 seconds. Close portal tabs or switch windows to check background delivery."; }
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
  void refresh();
})();
