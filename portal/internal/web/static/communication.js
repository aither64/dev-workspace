"use strict";

(() => {
  const panel = document.getElementById("communication");
  if (!panel) return;
  const slug = document.body.dataset.session;
  const url = `/api/sessions/${encodeURIComponent(slug)}/communication`;
  const warning = panel.querySelector("[data-communication-warning]");
  const selector = panel.querySelector("select");
  const grants = panel.querySelector("[data-communication-grants]");
  const counters = panel.querySelector("[data-communication-counts]");
  const transcriptWarning = document.getElementById("session-communication-warning");
  let filter = document.querySelector("[data-transcript-filter][aria-selected=true]")?.dataset.transcriptFilter;
  const transcriptVisible = () => !document.body.dataset.selectedMember && ["all", "sessions"].includes(filter) && document.getElementById("codex")?.classList.contains("active");
  const settingsVisible = () => !panel.hidden && document.getElementById("settings")?.classList.contains("active");
  const showWarning = message => {
    warning.textContent = transcriptWarning.textContent = message;
    warning.hidden = !message;
    transcriptWarning.hidden = !message || !transcriptVisible();
  };
  const archived = document.body.dataset.archived === "true";
  let pending = false;
  const node = (tag, text, className) => {
    const element = document.createElement(tag);
    element.textContent = text;
    if (className) element.className = className;
    return element;
  };

  async function load() {
    if (pending || document.hidden || !settingsVisible() && !transcriptVisible()) return;
    pending = true;
    try {
      const response = await fetch(url, {cache: "no-store"});
      const value = await response.json();
      if (!response.ok) throw new Error(value.error || "Communication could not be read.");
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
        const peer = grant.a.slug === slug ? grant.b.slug : grant.a.slug;
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
      const sent = value.messages.filter(message => message.from.slug === slug).length;
      const received = value.messages.filter(message => message.to.slug === slug && message.status === "delivered").length;
      counters.textContent = `${sent} sent · ${received} received`;
      document.dispatchEvent(new CustomEvent("session-communication-updated", {detail: {messages: value.messages}}));
      showWarning("");
    } catch (error) {
      showWarning(error.message);
    } finally { pending = false; }
  }

  async function mutate(request, button) {
    button.disabled = true;
    try {
      const response = await fetch(url, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(request)});
      const value = await response.json();
      if (!response.ok) throw new Error(value.error || "Communication could not be updated.");
      await load();
    } catch (error) { warning.textContent = error.message; warning.hidden = false; }
    finally { button.disabled = false; }
  }
  const allow = panel.querySelector("[data-communication-allow]");
  allow.disabled = archived;
  selector.disabled = archived;
  allow.addEventListener("click", () => { if (selector.value) mutate({action: "allow", peer: selector.value}, allow); });
  new MutationObserver(load).observe(panel, {attributes: true, attributeFilter: ["hidden"]});
  document.addEventListener("session-section-change", () => {transcriptWarning.hidden = !warning.textContent || !transcriptVisible(); void load();});
  document.addEventListener("session-transcript-filter-change", event => {
    filter = event.detail; transcriptWarning.hidden = !warning.textContent || !transcriptVisible(); void load();
  });
  document.addEventListener("visibilitychange", load);
  setInterval(load, 5000);
  load();
})();
