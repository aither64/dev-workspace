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
  let snapshot, seen = [], machines = 0, staleReservations = true, stopped = false, running = null, generation = 0, timer;
  const channel = typeof BroadcastChannel === "function" ? new BroadcastChannel("workspace-session-attention") : null;
  const visible = () => document.visibilityState !== "hidden" && !document.hidden;
  const retire = () => {++generation; running?.abort.abort(); running = null; clearTimeout(timer);};
  const focused = () => document.visibilityState === "visible" && document.hasFocus();
  function matchesDocument(value) {
    return value?.identity === renderedIdentity && value?.rootThread === renderedRoot;
  }
  function requireReload() {
    changed = true; stopped = true; retire();
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
  async function acknowledge(current = () => !stopped && visible()) {
    const event = snapshot?.event, identity = snapshot?.identity;
    if (!current() || changed || !matchesDocument(snapshot) || !event || !focused()) return;
    const id = event.id;
    try {
      const record = await workspaceAttentionStore.update(identity, value => {
        if (current() && !changed && focused() && matchesDocument(snapshot) && snapshot?.event?.id === id) value.seen.push(id);
        return value;
      });
      if (!current() || changed || !matchesDocument(snapshot) || snapshot?.identity !== identity) return;
      seen = record.seen; render();
      if (seen.includes(id)) {
        channel?.postMessage({identity});
        const registration = await navigator.serviceWorker?.getRegistration("/");
        registration?.active?.postMessage({type: "session-attention-ack", identity});
      }
    } catch (_) { if (current() && status) status.textContent = "Your browser could not save the read status."; }
  }
  async function read(url, cycle) {
    const deadline = setTimeout(() => cycle.abort.abort(), 6000);
    try {
      const response = await fetch(url, {cache: "no-store", signal: cycle.abort.signal});
      cycle.abort.signal.throwIfAborted();
      if (!response.ok) throw new Error();
      const result = await response.json(); cycle.abort.signal.throwIfAborted(); return result;
    } finally { clearTimeout(deadline); }
  }
  async function refresh() {
    if (changed || stopped || !visible() || running) return;
    const cycle = {abort: new AbortController(), ticket: generation}; running = cycle;
    const current = () => running === cycle && cycle.ticket === generation && !stopped && visible();
    try {
      const next = await read(`/api/sessions/${slug}/attention`, cycle);
      if (!current()) return;
      if (next.version !== 1 || !/^[0-9a-f]{64}$/.test(next.identity)) throw new Error();
      if (!matchesDocument(next)) { requireReload(); render(); return; }
      snapshot = next;
      try {
        const record = await workspaceAttentionStore.update(next.identity, value => {
          if (current() && next.revision > value.revision) {value.revision = next.revision; value.event = next.event;}
          return value;
        });
        if (!current()) return;
        seen = record.seen;
      } catch (_) { if (!current()) return; seen = []; }
      if (enabled && !saving) { enabled.checked = next.enabled; enabled.disabled = false; }
      await acknowledge(current);
      if (!current() || changed) return;
      await globalThis.workspaceBrowserNotifications?.presence(next, focused(), seen);
      if (!current()) return;
      try {
        const coordination = await read(`/api/sessions/${slug}/coordination`, cycle);
        if (!current()) return;
        machines = reservations(coordination, slug, next.identity); staleReservations = false;
      } catch (_) { if (current()) staleReservations = true; }
    } catch (_) {
      if (current()) { if (snapshot) { snapshot.fresh = false; snapshot.event = null; } staleReservations = true; }
    } finally {
      if (current()) {running = null; render(); timer = setTimeout(refresh, 5000);}
    }
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
  const revisit = () => { if (!visible()) {retire(); return;} if (!stopped) {if (focused()) void acknowledge(); clearTimeout(timer); void refresh();} };
  document.addEventListener("visibilitychange", revisit); window.addEventListener("focus", revisit);
  if (channel) channel.onmessage = async event => { if (!changed && !stopped && visible() && event.data.identity === renderedIdentity && matchesDocument(snapshot)) { const ticket = generation, record = await workspaceAttentionStore.update(renderedIdentity); if (ticket === generation && visible() && !stopped) {seen = record.seen; render();} } };
  navigator.serviceWorker?.addEventListener("message", event => { if (event.data?.identity === snapshot?.identity) { clearTimeout(timer); void refresh(); } });
  window.addEventListener("pagehide", () => { stopped = true; retire(); });
  window.addEventListener("pageshow", () => { if (stopped && !changed) { stopped = false; void refresh(); } });
  void refresh();
})();
