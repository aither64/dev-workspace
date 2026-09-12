(() => {
  "use strict";
  const slug = document.body.dataset.creation;
  if (!slug) return;
  const endpoint = `/api/sessions/${encodeURIComponent(slug)}/creation`;
  const title = document.getElementById("creation-title");
  const phase = document.getElementById("creation-phase");
  const error = document.getElementById("creation-error");
  const retry = document.getElementById("creation-retry");
  const elapsed = document.getElementById("creation-elapsed");
  const source = document.getElementById("creation-source");
  let receipt;
  let retrying = false;
  async function request(url, options) {
    const response = await fetch(url, {credentials: "same-origin", cache: "no-store", ...options});
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Unable to read initialization status.");
    return result;
  }
  function render(next) {
    receipt = next;
    if (next.state === "ready" || next.state === "conflict") {
      window.location.replace(`/${encodeURIComponent(slug)}/`);
      return;
    }
    title.textContent = next.state === "running" ? "Creating session" : "Initialization stopped";
    phase.textContent = next.phase;
    const seconds = Math.max(0, Math.floor((Date.now() - Date.parse(next.startedAt)) / 1000));
    elapsed.textContent = Number.isFinite(seconds) ? `Elapsed: ${Math.floor(seconds / 60)}m ${seconds % 60}s` : "";
    source.hidden = !next.sourceUrl || next.state === "running";
    if (next.sourceUrl && /^\/[A-Za-z0-9][A-Za-z0-9_-]*\/$/.test(next.sourceUrl)) source.href = next.sourceUrl;
    error.textContent = next.error || "";
    error.hidden = !next.error;
    retry.hidden = !["failed", "paused"].includes(next.state);
    retry.disabled = retrying;
  }
  async function poll() {
    try { render(await request(endpoint)); }
    catch (failure) {
      error.textContent = failure.message;
      error.hidden = false;
    }
    finally { window.setTimeout(poll, 1000); }
  }
  retry.addEventListener("click", async () => {
    if (!receipt || retrying) return;
    retrying = true;
    retry.disabled = true;
    try {
      render(await request(`${endpoint}/retry`, {
        method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({receiptId: receipt.receiptId, attempt: receipt.attempt}),
      }));
    } catch (failure) {
      error.textContent = failure.message;
      error.hidden = false;
    } finally { retrying = false; retry.disabled = false; }
  });
  poll();
})();
