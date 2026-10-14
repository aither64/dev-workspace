(() => {
  "use strict";
  const title = (base, event, seen, machines) => [event && !seen.includes(event.id) ? (event.kind === "done" ? "✓" : "?") : "",
    machines ? `R${machines === 1 ? "" : machines}` : "", base].filter(Boolean).join(" ");
  const reservations = (payload, slug, identity) => {
    if (!Array.isArray(payload?.reservationHolders)) throw new Error("Reservation status is unavailable.");
    const machines = new Set();
    payload.reservationHolders.filter(holder => holder.slug === slug && holder.observationIdentity === identity).forEach(holder => {
      if (!Array.isArray(holder.machines)) throw new Error("Reservation status is unavailable.");
      holder.machines.forEach(machine => { if (machine.state !== "requested" && machine.state !== "released" && typeof machine.machine === "string") machines.add(machine.machine); });
    });
    return machines.size;
  };
  globalThis.workspaceSessionAttention = {title, reservations};
  if (typeof document === "undefined" || !document.body.dataset.session) return;
  const slug = document.body.dataset.session, baseTitle = document.title;
  const renderedIdentity = document.body.dataset.sessionObservation, renderedRoot = document.body.dataset.rootThreadId || "";
  const status = document.getElementById("session-attention-status"), enabled = document.getElementById("session-notifications-enabled");
  let saving = false, changed = false;
  let snapshot, seen = [], machines = 0, staleReservations = true, stopped = false, running = false, timer;
  const channel = typeof BroadcastChannel === "function" ? new BroadcastChannel("workspace-session-attention") : null;
  const focused = () => document.visibilityState === "visible" && document.hasFocus();
  function matchesDocument(value) {
    return value?.identity === renderedIdentity && value?.rootThread === renderedRoot;
  }
  function requireReload() {
    changed = true; stopped = true; clearTimeout(timer);
    snapshot = null; staleReservations = true;
    if (enabled) enabled.disabled = true;
  }
  function render() {
    document.title = title(baseTitle, snapshot?.event, seen, machines);
    if (changed) { if (status) status.textContent = "This session changed. Reload this page to continue."; return; }
    if (status) status.textContent = [snapshot?.event && !seen.includes(snapshot.event.id) ?
      ({input: "Your answer is needed", approval: "Approval needed", done: "Response finished"})[snapshot.event.kind] : "",
      machines ? `${machines} machine${machines === 1 ? "" : "s"} reserved` : "", staleReservations ? "Reservation status is stale" : "",
      snapshot && !snapshot.fresh ? "Session activity is unavailable" : ""].filter(Boolean).join(" · ");
  }
  async function acknowledge() {
    const event = snapshot?.event, identity = snapshot?.identity;
    if (changed || !matchesDocument(snapshot) || !event || !focused()) return;
    const id = event.id;
    try {
      const record = await workspaceAttentionStore.update(identity, value => {
        if (!changed && focused() && matchesDocument(snapshot) && snapshot?.event?.id === id) value.seen.push(id);
        return value;
      });
      if (changed || !matchesDocument(snapshot) || snapshot?.identity !== identity) return;
      seen = record.seen; render();
      if (seen.includes(id)) {
        channel?.postMessage({identity});
        const registration = await navigator.serviceWorker?.getRegistration("/");
        registration?.active?.postMessage({type: "session-attention-ack", identity});
      }
    } catch (_) { if (status) status.textContent = "Your browser could not save the read status."; }
  }
  async function read(url) {
    const controller = new AbortController(), deadline = setTimeout(() => controller.abort(), 6000);
    try { const response = await fetch(url, {cache: "no-store", signal: controller.signal}); if (!response.ok) throw new Error(); return await response.json(); }
    finally { clearTimeout(deadline); }
  }
  async function refresh() {
    if (changed || stopped || running) return;
    running = true;
    try {
      const next = await read(`/api/sessions/${slug}/attention`);
      if (next.version !== 1 || !/^[0-9a-f]{64}$/.test(next.identity)) throw new Error();
      if (!matchesDocument(next)) { requireReload(); return; }
      snapshot = next;
      try { seen = (await workspaceAttentionStore.update(next.identity, value => {
        if (next.revision > value.revision) { value.revision = next.revision; value.event = next.event; }
        return value;
      })).seen; } catch (_) { seen = []; }
      if (enabled && !saving) { enabled.checked = next.enabled; enabled.disabled = false; }
      await acknowledge();
      if (changed) return;
      await globalThis.workspaceBrowserNotifications?.presence(next, focused(), seen);
      try { machines = reservations(await read(`/api/sessions/${slug}/coordination`), slug, next.identity); staleReservations = false; }
      catch (_) { staleReservations = true; }
    } catch (_) { if (snapshot) { snapshot.fresh = false; snapshot.event = null; } staleReservations = true; }
    finally { running = false; render(); if (!stopped) timer = setTimeout(refresh, 5000); }
  }
  enabled?.addEventListener("change", async () => {
    if (changed || !matchesDocument(snapshot)) return;
    const previousEnabled = snapshot.enabled;
    saving = true; enabled.disabled = true;
    try {
      const response = await fetch(`/api/sessions/${slug}/attention`, {method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({identity: snapshot.identity, enabled: enabled.checked})});
      const saved = await response.json(); if (!response.ok) throw new Error(saved.error || "Settings could not be saved.");
      if (!matchesDocument(saved)) { requireReload(); render(); return; }
      snapshot = saved;
    } catch (error) { if (!changed) { enabled.checked = previousEnabled; if (status) status.textContent = error.message; } }
    finally { saving = false; enabled.disabled = changed; }
  });
  const revisit = () => { if (focused()) { void acknowledge(); clearTimeout(timer); void refresh(); } };
  document.addEventListener("visibilitychange", revisit); window.addEventListener("focus", revisit);
  if (channel) channel.onmessage = async event => { if (!changed && event.data.identity === renderedIdentity && matchesDocument(snapshot)) { seen = (await workspaceAttentionStore.update(renderedIdentity)).seen; render(); } };
  navigator.serviceWorker?.addEventListener("message", event => { if (event.data?.identity === snapshot?.identity) { clearTimeout(timer); void refresh(); } });
  window.addEventListener("pagehide", () => { stopped = true; clearTimeout(timer); });
  window.addEventListener("pageshow", () => { if (stopped) { stopped = false; void refresh(); } });
  void refresh();
})();
