"use strict";

(() => {
  const panel = document.querySelector("[data-coordination]");
  if (!panel && !document.querySelector("[data-session-list]")) return;
  const warning = document.querySelector("[data-coordination-warning]");
  const refreshed = document.querySelector("[data-coordination-refreshed]");
  const sessionSlug = panel?.dataset.coordinationSession;
  const observation = panel?.dataset.coordinationObservation;
  const reservationTab = document.querySelector('[data-session-tab="reservations"]');
  const terminal = new Set(["succeeded", "failed", "canceled"]);
  const labels = {waiting_approval: "Waiting for approval", waiting_dependency: "Waiting for an earlier operation",
    waiting_resource: "Queued", ready: "Ready to run", claimed: "Starting", running: "Running",
    needs_refresh: "Needs preparation", reconciling: "Recovery required", succeeded: "Completed",
    failed: "Failed", canceled: "Canceled", requested: "Waiting for reservation", held: "Reserved for deployment",
    deployed_review: "Held for review", blocked_recovery: "Held for recovery",
    closing: "Checking completion", released: "Released", runnable: "Continuing automatically",
    waiting: "Waiting automatically", paused: "Paused",
    needs_attention: "Agent action required", complete: "Completed"};
  const kinds = {merge: "Merge feature branch", machine_activate: "Deploy system configuration", profile_activate: "Update workspace application"};
  const node = (tag, text = "", className = "") => {
    const element = document.createElement(tag); element.textContent = text;
    if (className) element.className = className;
    return element;
  };
  const link = (url, text) => {
    const element = node("a", text);
    try { if (new URL(url).protocol === "https:") element.href = url; } catch (_) { /* malformed status link */ }
    return element;
  };
  const shortName = machine => machine.shortName || machine.machine.split(":").at(-1);
  const stateLabel = state => labels[state] || "Status unavailable";
  const badge = state => node("span", stateLabel(state), `badge ${["failed", "blocked_recovery"].includes(state) ? "danger" : ["succeeded", "held", "deployed_review"].includes(state) ? "active" : "warning"}`);
  const date = value => {
    const parsed = typeof value === "string" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(value) ? new Date(value) : null;
    if (!parsed || !Number.isFinite(parsed.getTime())) return node("span", "Time unavailable", "muted");
    const element = node("time", parsed.toLocaleString()); element.dateTime = value; element.title = value;
    return element;
  };
  let lastPayload = null, lastDisplay = null, unavailable = false, paused = true, active = null, timer = null;
  let reservationCount = null;
  const panelVisible = () => !sessionSlug || panel.closest(".tab-panel")?.classList.contains("active");

  function countSessionReservations(payload) {
    const machines = new Set();
    for (const workspace of payload.workspaces) for (const holder of workspace.reservationHolders) {
      if (!observation || holder.slug !== sessionSlug || holder.observationIdentity !== observation) continue;
      if (!Array.isArray(holder.machines)) throw new Error("Reservation status is unavailable.");
      for (const machine of holder.machines) {
        if (!["requested", "released"].includes(machine.state)) machines.add(machine.machine);
      }
    }
    return machines.size;
  }

  function renderReservationTab() {
    if (!reservationTab) return;
    const label = reservationCount === null ? "Reservations" : `Reservations (${reservationCount})`;
    reservationTab.querySelector(".rail-label").textContent = label;
    reservationTab.title = label + (unavailable ? reservationCount === null
      ? ". Reservation status is unavailable."
      : ". Reservation status is unavailable. Last known count is shown." : "");
    reservationTab.setAttribute("aria-label", reservationTab.title);
  }

  function renderBadges() {
    if (!lastPayload) return;
    const holders = lastPayload.workspaces.flatMap(workspace => workspace.reservationHolders);
    for (const card of document.querySelectorAll("[data-session-slug]")) {
      const element = card.querySelector("[data-session-reservation]");
      if (!element) continue;
      const holder = holders.find(holder => holder.slug === card.dataset.sessionSlug
        && holder.observationIdentity === card.dataset.sessionObservation);
      const machines = holder && holder.observationIdentity && holder.observationIdentity === card.dataset.sessionObservation
        ? [...new Map(holder.machines.map(machine => [machine.machine, machine])).values()] : [];
      element.hidden = machines.length === 0;
      element.textContent = machines.length === 1 ? `${shortName(machines[0])} reserved` : `${machines.length} machines reserved`;
      element.title = machines.map(machine => `${shortName(machine)} (${machine.machine}): ${stateLabel(machine.state)}`).join("\n")
        + (unavailable ? "\nReservation status is unavailable. Last known reservations are shown." : "");
      element.setAttribute("aria-label", element.title);
    }
  }

  function renderOperations(payload) {
    if (!panel || !panelVisible()) return;
    const display = JSON.stringify(payload);
    if (display === lastDisplay) return;
    const expanded = new Set([...panel.querySelectorAll("details[data-coordination-key][open]")].map(detail => detail.dataset.coordinationKey));
    const focused = document.activeElement?.closest?.("details[data-coordination-key]")?.dataset.coordinationKey;
    const content = document.createDocumentFragment();
    const owners = new Map(), reservations = new Map(), held = new Map(), jobs = [], claims = [], integrations = [], requests = [], alerts = [];
    let limited = false;
    for (const workspace of payload.workspaces) {
      for (const owner of workspace.owners) {
        if (!sessionSlug || observation && owner.observationIdentity === observation) owners.set(owner.id, owner);
      }
      const own = item => !sessionSlug || owners.has(item.ownerId);
      if (workspace.suspended) alerts.push("Operation scheduling is paused. Existing reservations remain held.");
      for (const holder of workspace.reservationHolders) {
        if (sessionSlug && (!observation || holder.observationIdentity !== observation)) continue;
        owners.set(holder.ownerId, {...holder, id: holder.ownerId});
        for (const machine of holder.machines) {
          const item = {...machine, id: machine.id || machine.machine, ownerId: holder.ownerId};
          held.set(item.machine, item); reservations.set(item.id, item);
        }
      }
      for (const item of workspace.reservations.filter(own)) {
        reservations.set(item.id, item);
        if (item.state === "requested") requests.push(item);
        else if (item.state !== "released") held.set(item.machine, item);
      }
      if (!sessionSlug) for (const item of workspace.foreignReservations) {
        if (!["requested", "released"].includes(item.state) && !held.has(item.machine)) held.set(item.machine, {...item, id: item.machine, foreign: true});
      }
      jobs.push(...workspace.jobs.filter(own));
      claims.push(...(workspace.claims || []).filter(own));
      integrations.push(...(workspace.integrations || []).filter(own));
      const notices = workspace.notifications.filter(own).filter(event => event.state !== "delivered");
      if (notices.length) alerts.push(`${notices.length} agent ${notices.length === 1 ? "notification is" : "notifications are"} awaiting delivery or confirmation.`);
      if (workspace.counts && Object.entries(workspace.counts).some(([key, total]) => total > (workspace[key]?.length || 0))) limited = true;
    }
    const details = (key, values) => {
      const element = node("details", "", "coordination-details"); element.dataset.coordinationKey = key;
      element.append(node("summary", "Technical details"), node("pre", JSON.stringify(values, null, 2)));
      element.open = expanded.has(key);
      return element;
    };
    const ownerLink = (item, reservation = false) => {
      const owner = owners.get(item.ownerId);
      if (owner?.observationIdentity) return link(owner.portalUrl + (reservation ? "#reservations" : ""), owner.slug);
      if (item.foreign) return link(item.portalUrl + "#reservations", "Owning session");
      return node("span", owner ? `${owner.slug} (retained record)` : "Session details unavailable", "muted");
    };
    const section = (title, count) => {
      const element = node("section", "", "panel coordination-section");
      const heading = node("div", "", "section-heading"); heading.append(node("h2", title), node("span", String(count)));
      element.append(heading); content.append(element); return element;
    };
    for (const text of alerts) content.append(node("p", text, "notice warning"));
    if (integrations.length) {
      const runs = section("Background integrations", integrations.length);
      for (const run of integrations) {
        const row = node("article", "", "coordination-machine");
        const heading = node("p"); heading.append(ownerLink(run), node("span", ": "), badge(run.outcome === "canceled" ? "canceled" : run.continuation?.state === "held" ? "paused" : run.continuation?.state));
        row.append(heading);
        if (run.outcome === "canceled") row.append(node("p", "Canceled before any operation was submitted."));
        else if (run.reason?.message) row.append(node("p", run.reason.message));
        else row.append(node("p", run.continuation?.state === "complete"
          ? "The requested operations have finished."
          : ["paused", "held"].includes(run.continuation?.state)
            ? "Continuation is paused. Operations already running may finish."
            : "The worker continues approved operations while the session is idle."));
        if (run.handoff?.state === "pending") row.append(node("p", "Waiting for an agent to respond. Sending a message does not confirm a response.", "notice warning"));
        else if (run.handoff?.state === "acknowledged") row.append(node("p", "An agent acknowledged this action. A reminder follows if its acknowledgement expires."));
        else if (run.handoff?.state === "supervised") row.append(node("p", "Continuation is supervised from an external conversation."));
        row.append(details(`integration:${run.id}`, run)); runs.append(row);
      }
    }
    const machineSection = section("Reserved machines", held.size);
    const grid = node("div", "", "coordination-machines"); machineSection.append(grid);
    for (const item of [...held.values()].sort((a, b) => shortName(a).localeCompare(shortName(b)))) {
      const card = node("article", "", "coordination-machine");
      const title = node("div", "", "coordination-machine-heading"); title.append(node("h3", shortName(item)), badge(item.state));
      card.append(title);
      const owner = node("p"); owner.append(node("span", "Held by ", "muted"), ownerLink(item, true)); card.append(owner);
      card.append(node("p", item.mode === "claim"
        ? "Reserved until the owning session explicitly releases this machine."
        : item.state === "deployed_review"
        ? "Other sessions cannot redeploy this machine while review, feedback and integration are pending."
        : item.state === "blocked_recovery" ? "The owning session must resolve deployment recovery before another session can deploy."
        : "Other sessions wait until the owning session finishes its deployment work."));
      const since = node("p", "", "muted"); since.append(node("span", "Reserved since: "), date(item.timestamps?.heldAt)); card.append(since);
      if (typeof item.deployments === "number") card.append(node("p", `${item.deployments} recorded deployment ${item.deployments === 1 ? "step" : "steps"}`, "muted"));
      card.append(details(`reservation:${item.id}`, item)); grid.append(card);
    }
    if (!held.size) machineSection.append(node("p", sessionSlug ? "This session does not hold any machines." : "No machines are reserved.", "empty"));
    if (requests.length) {
      const pending = section("Requested reservations", requests.length);
      for (const item of requests) {
        const row = node("p", `${shortName(item)}: ${stateLabel(item.state)}. `); row.append(ownerLink(item)); pending.append(row);
      }
    }
    if (claims.length) {
      const claimSection = section("Resource claims", claims.length);
      for (const item of claims) {
        const row = node("article", "", "coordination-machine");
        const title = node("p"); title.append(node("strong", item.label), node("span", ": "),
          node("span", item.state === "held" ? "Held until released" : item.state === "waiting" ? "Waiting for resources" : stateLabel(item.state)));
        const owner = node("p"); owner.append(ownerLink(item));
        row.append(title, owner, node("p", [...item.repositories, ...item.machines].join(", ")),
          details(`claim:${item.id}`, item)); claimSection.append(row);
      }
    }
    const target = job => job.kind === "merge" ? `${job.source?.repository || "Repository"} → ${(job.source?.target || "default branch").replace(/^refs\/heads\//, "")}`
      : reservations.has(job.reservationId) ? shortName(reservations.get(job.reservationId)) : job.source?.target || "Workspace";
    const jobTable = (title, rows, empty) => {
      const container = section(title, rows.length);
      if (!rows.length) { container.append(node("p", empty, "empty")); return; }
      const scroll = node("div", "", "table-scroll"), table = node("table", "", "coordination-table");
      const head = node("thead"), headings = node("tr"), body = node("tbody");
      for (const text of ["Operation", "Target", "Session", "Status", "Last change"]) {
        const cell = node("th", text); cell.scope = "col"; headings.append(cell);
      }
      head.append(headings); table.append(head, body); scroll.append(table); container.append(scroll);
      for (const job of rows) {
        const row = node("tr"), operation = node("td");
        operation.append(node("strong", kinds[job.kind] || "Coordinated operation"), details(`job:${job.id}`, job));
        const owner = node("td"); owner.append(ownerLink(job));
        const status = node("td"); status.append(badge(job.reason?.code === "integration_paused" ? "paused" : job.state));
        if (job.state === "needs_refresh") status.append(node("p", "The owning session needs to prepare an updated operation.", "muted"));
        if (job.reason?.portalUrl) {
          const blocker = node("p", "Waiting for "); blocker.append(link(job.reason.portalUrl + "#reservations", "another session")); status.append(blocker);
        } else if (job.state === "waiting_dependency") status.append(node("p", "An earlier step must finish first.", "muted"));
        const changed = node("td"); changed.append(date(job.timestamps?.stateChangedAt));
        row.append(operation, node("td", target(job)), owner, status, changed); body.append(row);
      }
    };
    const current = jobs.filter(job => !terminal.has(job.state)).sort((a, b) => {
      const rank = job => ["running", "claimed", "reconciling"].includes(job.state) ? 0 : 1;
      return rank(a) - rank(b) || (b.sequence || 0) - (a.sequence || 0);
    });
    jobTable("Running and waiting", current, "No operations are running or waiting.");
    jobTable("Recent results", jobs.filter(job => terminal.has(job.state)).sort((a, b) => (b.sequence || 0) - (a.sequence || 0)), "No completed operations yet.");
    if (limited) content.append(node("p", "This is a limited view of operation history. The session command provides the complete records.", "notice"));
    panel.replaceChildren(content); lastDisplay = display;
    if (focused) [...panel.querySelectorAll("details[data-coordination-key]")].find(detail => detail.dataset.coordinationKey === focused)?.querySelector("summary")?.focus({preventScroll: true});
  }

  async function refresh() {
    if (paused || active) return;
    const controller = new AbortController(); active = controller;
    const deadline = setTimeout(() => controller.abort(), 10000);
    try {
      const endpoint = sessionSlug ? `/api/sessions/${encodeURIComponent(sessionSlug)}/coordination` : "/api/coordination";
      const response = await fetch(endpoint, {cache: "no-store", signal: controller.signal});
      if (!response.ok) throw new Error("Operations are unavailable.");
      const raw = await response.json();
      if (active !== controller || controller.signal.aborted) return;
      const payload = sessionSlug ? {workspaces: [raw]} : raw;
      if (!Array.isArray(payload.workspaces) || payload.workspaces.some(workspace =>
        !["owners", "jobs", "reservations", "foreignReservations", "notifications", "reservationHolders"].every(key => Array.isArray(workspace[key])))) {
        throw new Error("Reservation status is unavailable.");
      }
      const count = sessionSlug ? countSessionReservations(payload) : null;
      renderOperations(payload); lastPayload = payload; reservationCount = count; unavailable = false;
      renderReservationTab(); renderBadges();
      if (warning) { warning.hidden = true; warning.textContent = ""; }
      if (refreshed) refreshed.textContent = `Last checked: ${new Date().toLocaleTimeString()}`;
    } catch (_) {
      if (active !== controller || paused) return;
      unavailable = true; renderReservationTab(); renderBadges();
      if (warning) {
        warning.textContent = lastPayload ? "Operation status is unavailable. Last known status remains shown." : "Operation status is unavailable.";
        warning.hidden = false;
      }
    } finally {
      clearTimeout(deadline);
      if (active === controller) { active = null; if (!paused) timer = setTimeout(refresh, 5000); }
    }
  }
  function pause() {
    paused = true; clearTimeout(timer); if (active) active.abort(); active = null;
  }
  function resume() {
    if (document.hidden) { pause(); return; }
    if (lastPayload) renderOperations(lastPayload);
    if (paused) { paused = false; clearTimeout(timer); refresh(); }
  }
  document.addEventListener("session-list-updated", renderBadges);
  document.addEventListener("visibilitychange", resume);
  if (sessionSlug) {
    document.addEventListener("session-section-change", resume);
    document.addEventListener("click", resume);
    window.addEventListener("hashchange", resume);
  }
  window.addEventListener("pagehide", pause); window.addEventListener("pageshow", resume);
  resume();
})();
