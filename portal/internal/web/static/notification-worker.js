"use strict";
importScripts("/static/session-attention-store.js");

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

async function display(payload) {
  if (!valid(payload)) {
    await self.registration.showNotification("Workspace update", {body: "Open the workspace to check its current status.", tag: "workspace-update", renotify: false, data: {route: "/"}});
    return;
  }
  let record, trusted = payload.kind === "test";
  try {
    record = await workspaceAttentionStore.update(payload.identity, value => {
      if (payload.revision > value.revision) { value.revision = payload.revision; value.event = payload; }
      return value;
    });
    if (payload.kind !== "test") {
      const controller = new AbortController(), deadline = setTimeout(() => controller.abort(), 3500);
      try {
        const response = await fetch(`/api/sessions/${encodeURIComponent(payload.slug)}/attention`, {credentials: "same-origin", cache: "no-store", signal: controller.signal});
        if (response.status === 404 || response.status === 403 || response.status === 401) {
          await self.registration.showNotification("Workspace update", {body: "Open the workspace to check its current status.", tag: `session-${payload.identity}`, renotify: false, data: {route: "/"}});
          return;
        }
        const status = response.ok ? await response.json() : null;
        trusted = status?.fresh && status.enabled && status.identity === payload.identity && status.event?.id === payload.id &&
          status.revision === payload.revision && payload.expiresAt > Date.now() && record.revision === payload.revision && !record.seen.includes(payload.id);
      } finally { clearTimeout(deadline); }
    }
  } catch (_) { trusted = false; }
  // A delivered push always results in a visible notification. Duplicate,
  // reordered or unverifiable delivery replaces the same entry with neutral copy.
  const current = record?.event || payload;
  await self.registration.showNotification(current.slug || "Workspace", {
    body: trusted ? `${current.member && current.member !== "lead" ? `${current.member}: ` : ""}${reason(current.kind)}` : "Session update available",
    tag: `session-${payload.identity}`, renotify: false,
    data: {route: safeRoute(current.route)?.pathname + (safeRoute(current.route)?.search || "") + (safeRoute(current.route)?.hash || ""), identity: payload.identity},
  });
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
    const existing = windows.find(client => {
      const url = new URL(client.url);
      return url.origin === route.origin && url.pathname === route.pathname;
    });
    if (existing) { await existing.navigate(route.href); await existing.focus(); }
    else await self.clients.openWindow(route.href);
  })());
});
self.addEventListener("message", event => {
  if (event.data?.type !== "session-attention-ack" || !/^[0-9a-f]{64}$/.test(event.data.identity)) return;
  event.waitUntil((async () => {
    const notifications = await self.registration.getNotifications({tag: `session-${event.data.identity}`});
    notifications.forEach(notification => notification.close());
  })());
});
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));
