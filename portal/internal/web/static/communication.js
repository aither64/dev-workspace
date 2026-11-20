"use strict";

(() => {
  const body = document.body;
  const {session: slug, sessionBackend: backend, selectedMember: member = ""} = body.dataset;
  const scopeKey = backend === "pi" ? "conversationScope" : "sessionObservation";
  const scope = body.dataset[scopeKey];
  if (!slug || !scope || !["codex", "pi"].includes(backend) || member && member !== "lead") return;
  const panel = document.getElementById("communication");
  const url = `/api/sessions/${encodeURIComponent(slug)}/communication`;
  const warning = panel?.querySelector("[data-communication-warning]");
  const selector = panel?.querySelector("select");
  const grants = panel?.querySelector("[data-communication-grants]");
  const counters = panel?.querySelector("[data-communication-counts]");
  const transcriptWarning = document.getElementById("session-communication-warning");
  let filter = document.querySelector("[data-transcript-filter][aria-selected=true]")?.dataset.transcriptFilter;
  const transcriptVisible = () => (!document.body.dataset.selectedMember || document.body.dataset.selectedMember === "lead") && ["all", "sessions"].includes(filter) && document.getElementById("codex")?.classList.contains("active");
  const settingsVisible = () => panel && !panel.hidden && document.getElementById("settings")?.classList.contains("active");
  let active = true;
  const current = () => active && body.dataset.session === slug && body.dataset.sessionBackend === backend &&
    (body.dataset.selectedMember || "") === member && body.dataset[scopeKey] === scope;
  const visible = () => current() && !document.hidden && (settingsVisible() || transcriptVisible());
  const showWarning = message => {
    if (warning) { warning.textContent = message; warning.hidden = !message; }
    if (transcriptWarning) { transcriptWarning.textContent = message; transcriptWarning.hidden = !message || !transcriptVisible(); }
  };
  const archived = document.body.dataset.archived === "true";
  let pending = null;
  let generation = "";
  const endpointSlug = endpoint => endpoint?.slug || endpoint?.session?.slug || "";
  const sourceHeaders = () => ({
    "X-Workspace-Conversation-Scope": scope,
    ...(backend === "pi" && generation ? {"X-Workspace-Pi-Generation": generation} : {}),
  });
  const node = (tag, text, className) => {
    const element = document.createElement(tag);
    element.textContent = text;
    if (className) element.className = className;
    return element;
  };

  async function load() {
    if (pending || !visible()) return;
    const read = new AbortController();
    pending = read;
    try {
      const response = await fetch(url, {cache: "no-store", headers: sourceHeaders(), signal: read.signal});
      const value = await response.json();
      if (read.signal.aborted || !visible()) return;
      if (!response.ok) throw new Error(value.error || "Communication could not be read.");
      generation = /^[0-9a-f]{64}$/.test(value.generation || "") ? value.generation : "";
      if (panel) {
        const selected = selector.value;
        selector.replaceChildren(node("option", "Choose a session"));
        selector.firstChild.value = "";
        for (const peer of value.candidates) {
          const option = node("option", peer); option.value = peer;
          selector.append(option);
        }
        selector.value = selected;
        const rows = [];
        for (const grant of value.grants) {
          const peer = endpointSlug(grant.a) === slug ? endpointSlug(grant.b) : endpointSlug(grant.a);
          const row = node("div", "", "panel");
          const heading = node("h3", peer);
          row.append(heading, node("p", grant.revokedAt ? `Closed: ${grant.reason}` : "Communication allowed in both directions."),
            node("p", `Approved from ${grant.approvedBy}: ${grant.approval}`, "muted"));
          if (!grant.revokedAt && !archived) {
            const revoke = node("button", "Revoke access", "quiet");
            revoke.type = "button";
            revoke.addEventListener("click", () => mutate({action: "revoke", peer}, revoke));
            row.append(revoke);
          }
          rows.push(row);
        }
        grants.replaceChildren(...(rows.length ? rows : [node("p", "No sessions have permission to communicate.", "muted")]));
        const sent = value.messages.filter(message => endpointSlug(message.from) === slug).length;
        const received = value.messages.filter(message => endpointSlug(message.to) === slug && message.status === "delivered").length;
        counters.textContent = `${sent} sent · ${received} received`;
      }
      document.dispatchEvent(new CustomEvent("session-communication-updated", {detail: {messages: value.messages}}));
      showWarning("");
    } catch (error) {
      if (!read.signal.aborted && visible()) showWarning(error.message);
    } finally { if (pending === read) pending = null; }
  }

  async function mutate(request, button) {
    if (!current()) return;
    button.disabled = true;
    try {
      const response = await fetch(url, {method: "POST", headers: {"Content-Type": "application/json", ...sourceHeaders()}, body: JSON.stringify(request)});
      const value = await response.json();
      if (!current()) return;
      if (!response.ok) throw new Error(value.error || "Communication could not be updated.");
      await load();
    } catch (error) { if (current()) showWarning(error.message); }
    finally { if (current()) button.disabled = false; }
  }
  const allow = panel?.querySelector("[data-communication-allow]");
  if (allow && selector) {
    allow.disabled = selector.disabled = archived;
    allow.addEventListener("click", () => { if (selector.value) mutate({action: "allow", peer: selector.value}, allow); });
  }
  const observer = panel ? new MutationObserver(() => refresh()) : null;
  observer?.observe(panel, {attributes: true, attributeFilter: ["hidden"]});
  const refresh = () => {
    if (!visible()) pending?.abort();
    if (transcriptWarning) transcriptWarning.hidden = !transcriptWarning.textContent || !transcriptVisible();
    void load();
  };
  document.addEventListener("session-section-change", refresh);
  document.addEventListener("session-transcript-filter-change", event => { filter = event.detail; refresh(); });
  document.addEventListener("visibilitychange", refresh);
  let timer = setInterval(refresh, 5000);
  addEventListener("pagehide", () => {
    active = false;
    const read = pending;
    pending = null;
    read?.abort();
    clearInterval(timer); timer = null;
    observer?.disconnect();
  });
  addEventListener("pageshow", () => {
    if (active) return;
    active = true;
    observer?.observe(panel, {attributes: true, attributeFilter: ["hidden"]});
    timer = setInterval(refresh, 5000);
    refresh();
  });
  load();
})();
