"use strict";
importScripts("/static/session-attention-store.js?v=4");

const reason = kind => ({input: "Your answer is needed", approval: "Approval needed", done: "Response finished", test: "Browser notification test"})[kind];
const safeRoute = value => {
  if (typeof value !== "string" || !value.startsWith("/") || value.startsWith("//")) return null;
  const route = new URL(value, self.location.origin);
  if (route.origin !== self.location.origin || !/^\/(?:[A-Za-z0-9][A-Za-z0-9_-]*\/)?$/.test(route.pathname) ||
      (route.search && !/^\?member=[a-z][a-z0-9]{0,31}[0-9]{1,20}$/.test(route.search)) || (route.hash && route.hash !== "#codex")) return null;
  return route;
};
const valid = payload => payload && payload.version === 1 && /^[0-9a-f]{64}$/.test(payload.id) &&
  /^(?:[0-9a-f]{64}|test-[0-9a-f]{64})$/.test(payload.identity) && Number.isSafeInteger(payload.revision) &&
  Number.isSafeInteger(payload.expiresAt) && payload.revision > 0 && typeof payload.slug === "string" && payload.slug.length <= 128 &&
  reason(payload.kind) && safeRoute(payload.route);

async function notifyTestPages(identity) {
  try {
    const windows = await self.clients.matchAll({type: "window", includeUncontrolled: true});
    windows.forEach(client => client.postMessage({type: "notification-test-update", identity}));
  } catch (_) {}
}
async function boundedTestRead(promise, fallback) {
  let timer;
  try { return await Promise.race([promise, new Promise(resolve => { timer = setTimeout(() => resolve(fallback), 750); })]); }
  catch (_) { return fallback; }
  finally { clearTimeout(timer); }
}
const deliveryGate = () => boundedTestRead(Promise.resolve().then(() => workspaceAttentionStore.readDeliveryGate()), false);
const gateAccepts = (gate, payload) => gate === null || Boolean(gate && gate.enabled && payload?.registrationId === gate.registrationId);
async function show(title, options, payload, initialGate) {
  // Read at the display attempt, after status/receipt work, so a newly saved
  // visual-only choice also applies to delivery already waiting in this worker.
  const playSound = await boundedTestRead(Promise.resolve().then(() => workspaceAttentionStore.readSound()), false);
  const gate = await deliveryGate();
  if (!gateAccepts(gate, payload) || (initialGate && (!gate || gate.actionToken !== initialGate.actionToken))) return false;
  await self.registration.showNotification(title, {...options, ...(playSound === true ? {} : {silent: true})});
  return true;
}
async function displayTest(payload, gate) {
  const descriptor = workspaceAttentionStore.testDescriptor?.(payload);
  const save = async outcome => {
    if (!descriptor) return;
    await boundedTestRead(Promise.resolve().then(() => workspaceAttentionStore.saveTest(descriptor, outcome)), null);
    await boundedTestRead(notifyTestPages(payload.identity), null);
  };
  await save({received: true, display: "pending", browserRecord: "unavailable"});
  const tag = `test-${payload.identity}-${payload.id}`;
  try {
    // Only older tests for this registration are retired. Session notifications
    // and a newer explicit test remain untouched by a late/duplicate push.
    const previous = await boundedTestRead(self.registration.getNotifications(), []);
    previous.filter(item => item.data?.testIdentity === payload.identity && item.tag !== tag &&
      Number.isSafeInteger(item.data.testRevision) && item.data.testRevision < payload.revision).slice(0, 32).forEach(item => item.close());
  } catch (_) {}
  try {
    if (!await show("Workspace", {body: reason("test"), tag, renotify: false,
      data: {route: "/", testIdentity: payload.identity, testRevision: payload.revision}}, payload, gate)) return;
  } catch (error) {
    await save({received: true, display: "rejected", error: workspaceAttentionStore.testError?.(error) || "UnknownError", browserRecord: "unavailable"});
    return;
  }
  let browserRecord = "unavailable";
  const notifications = await boundedTestRead(self.registration.getNotifications({tag}), null);
  if (notifications) browserRecord = notifications.some(item => item.tag === tag) ? "present" : "absent";
  await save({received: true, display: "resolved", browserRecord});
}

async function display(payload) {
  // Reject old epochs before every branch, including invalid/Test/fallback data.
  const gate = await deliveryGate();
  if (!gateAccepts(gate, payload)) return;
  if (!valid(payload)) {
    await show("Workspace update", {body: "Open the workspace to check its current status.", tag: "workspace-update", renotify: false, data: {route: "/"}}, payload, gate);
    return;
  }
  if (payload.kind === "test" && /^test-[0-9a-f]{64}$/.test(payload.identity)) {
    await displayTest(payload, gate);
    return;
  }
  let record, trusted = payload.kind === "test";
  try {
    record = await workspaceAttentionStore.update(payload.identity, value => {
      if (payload.revision > value.revision) { const {registrationId, ...event} = payload; value.revision = payload.revision; value.event = event; }
      return value;
    });
    if (payload.kind !== "test") {
      const controller = new AbortController(), deadline = setTimeout(() => controller.abort(), 3500);
      try {
        const response = await fetch(`/api/sessions/${encodeURIComponent(payload.slug)}/attention`, {credentials: "same-origin", cache: "no-store", signal: controller.signal});
        if (response.status === 404 || response.status === 403 || response.status === 401) {
          await show("Workspace update", {body: "Open the workspace to check its current status.", tag: `session-${payload.identity}`, renotify: false, data: {route: "/"}}, payload, gate);
          return;
        }
        const status = response.ok ? await response.json() : null;
        trusted = status?.fresh && status.enabled && status.identity === payload.identity && status.event?.id === payload.id &&
          status.revision === payload.revision && payload.expiresAt > Date.now() && record.revision === payload.revision && !record.seen.includes(payload.id);
      } finally { clearTimeout(deadline); }
    }
  } catch (_) { trusted = false; }
  // A still-enabled current epoch retains neutral delivery fallbacks.
  const current = record?.event || payload;
  if (!await show(current.slug || "Workspace", {
    body: trusted ? `${current.member && current.member !== "lead" ? `${current.member}: ` : ""}${reason(current.kind)}` : "Session update available",
    tag: `session-${payload.identity}`, renotify: false,
    data: {route: safeRoute(current.route)?.pathname + (safeRoute(current.route)?.search || "") + (safeRoute(current.route)?.hash || ""), identity: payload.identity},
  }, payload, gate)) return;
  const windows = await self.clients.matchAll({type: "window", includeUncontrolled: true});
  windows.forEach(client => client.postMessage({type: "session-attention-update", identity: payload.identity}));
}
self.addEventListener("push", event => {
  let payload;
  try { payload = event.data?.json(); } catch (_) { payload = null; }
  event.waitUntil(display(payload));
});
self.addEventListener("notificationclick", event => {
  event.notification.close();
  event.waitUntil((async () => {
    const route = safeRoute(event.notification.data?.route);
    if (!route) return;
    if (route.pathname !== "/" && /^[0-9a-f]{64}$/.test(event.notification.data?.identity)) route.searchParams.set("attentionIdentity", event.notification.data.identity);
    const windows = await self.clients.matchAll({type: "window", includeUncontrolled: true});
    const matching = windows.filter(client => {
      const url = new URL(client.url);
      return url.origin === route.origin && (url.pathname === route.pathname ||
        (route.pathname !== "/" && url.pathname === route.pathname.slice(0, -1)));
    });
    const member = route.searchParams.get("member") || "lead";
    const existing = matching.find(client => (new URL(client.url).searchParams.get("member") || "lead") === member) || matching[0];
    if (existing) await existing.focus();
    else await self.clients.openWindow(route.href);
  })());
});
self.addEventListener("message", event => {
  if (event.data?.type === "notification-test-capabilities") {
    event.ports?.[0]?.postMessage({version: 1, deliveryEpoch: 1});
    return;
  }
  if (event.data?.type !== "session-attention-ack" || !/^[0-9a-f]{64}$/.test(event.data.identity)) return;
  event.waitUntil((async () => {
    const notifications = await self.registration.getNotifications({tag: `session-${event.data.identity}`});
    notifications.forEach(notification => notification.close());
  })());
});
self.addEventListener("install", event => event.waitUntil(self.skipWaiting()));
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));
