(async () => {
  "use strict";
  const phaseLabel = (next) => ({
    accepting: "Saving the request and files…",
    naming: "Choosing a session name…",
    reserving: "Reserving the session name…",
    initializing: "Initializing the session…",
    stopped: "Session preparation stopped.",
  }[next.phase] || "Preparing the session…");
  const preparationProgress = (next, identity, helpers) => {
    if (!helpers.acceptedStatus(next, identity.requestId, identity.receiptId) ||
        next.startedAt !== identity.startedAt) throw new Error("The saved session request identity changed. Keep this page for recovery.");
    const destination = helpers.sessionURL(next);
    if (next.state === "ready" && !destination) throw new Error("Unable to confirm the recorded session destination.");
    return {
      destination,
      phase: next.phase === "initializing" && typeof next.detail === "string" && next.detail ? next.detail : phaseLabel(next),
      retry: ["failed", "paused"].includes(next.state),
      stopped: ["gone", "conflict", "cancelled"].includes(next.state),
    };
  };
  if (typeof module !== "undefined" && module.exports) {
    module.exports = {phaseLabel, preparationProgress}; return;
  }
  const slug = document.body.dataset.creation;
  const requestId = document.body.dataset.preparation;
  if (!slug && !requestId) return;
  const identity = {requestId, receiptId: document.body.dataset.receiptId, startedAt: document.body.dataset.acceptedAt};
  const endpoint = requestId ? `/api/session-creations/${requestId}` : `/api/sessions/${encodeURIComponent(slug)}/creation`;
  const title = document.getElementById("creation-title");
  const phase = document.getElementById("creation-phase");
  const error = document.getElementById("creation-error");
  const retry = document.getElementById("creation-retry");
  const elapsed = document.getElementById("creation-elapsed");
  const source = document.getElementById("creation-source");
  const leaveNote = document.getElementById("creation-leave-note");
  const initialRequest = document.getElementById("creation-request");
  const requestPanel = document.getElementById("creation-request-panel");
  // The prompt is server-rendered, so asset or polling failures cannot hide it.
  void import("/codex/assets/conversation.js?v=12").then(({createCopyButton}) => {
    document.getElementById("creation-request-copy").append(createCopyButton({
      getText: () => initialRequest.textContent, label: "Copy initial request",
    }));
  }).catch(() => {});
  let receipt, retrying = false, stopped = false;
  async function request(url, options) {
    const response = await fetch(url, {credentials: "same-origin", cache: "no-store", ...options});
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Unable to read initialization status.");
    return result;
  }
  function render(next) {
    const presentation = requestId ? preparationProgress(next, identity, sessionPreparation) : null;
    if (receipt && next.attempt < receipt.attempt) return;
    receipt = next;
    if (typeof next.initialRequest === "string") {
      if (initialRequest.textContent !== next.initialRequest) initialRequest.textContent = next.initialRequest;
      requestPanel.hidden = !next.initialRequest;
    }
    const destination = requestId ? presentation.destination :
      (["ready", "conflict"].includes(next.state) ? `/${encodeURIComponent(slug)}/` : "");
    if (destination) { stopped = true; window.location.replace(destination); return; }
    stopped = Boolean(presentation?.stopped);
    title.textContent = ["running", "accepting", "handed_off"].includes(next.state) ? "Creating session" :
      next.state === "gone" ? "Session is no longer available" :
      next.state === "conflict" ? "Session request needs attention" :
      next.state === "cancelled" ? "Session was not created" : "Initialization stopped";
    leaveNote.hidden = stopped || next.state === "cancelled";
    phase.textContent = requestId ? presentation.phase : next.phase;
    const startedAt = requestId ? identity.startedAt : next.startedAt;
    const seconds = Math.max(0, Math.floor((Date.now() - Date.parse(startedAt)) / 1000));
    elapsed.textContent = Number.isFinite(seconds) ? `Elapsed: ${Math.floor(seconds / 60)}m ${seconds % 60}s` : "";
    const sourceURL = !requestId && /^\/[A-Za-z0-9][A-Za-z0-9_-]*\/$/.test(next.sourceUrl || "") ? next.sourceUrl : "";
    source.hidden = !sourceURL || next.state === "running";
    if (sourceURL) source.href = sourceURL;
    else source.removeAttribute("href");
    error.textContent = next.error || "";
    error.hidden = !next.error;
    retry.hidden = requestId ? !presentation.retry : !["failed", "paused"].includes(next.state);
    retry.disabled = retrying;
  }
  async function poll() {
    try { render(await request(endpoint)); }
    catch (failure) { error.textContent = failure.message; error.hidden = false; }
    finally { if (!stopped) window.setTimeout(poll, 1000); }
  }
  retry.addEventListener("click", async () => {
    if (!receipt || retrying || stopped) return;
    retrying = true; retry.disabled = true;
    try {
      render(await request(`${endpoint}/retry`, {
        method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({receiptId: receipt.receiptId, attempt: receipt.attempt}),
      }));
    } catch (failure) { error.textContent = failure.message; error.hidden = false; }
    finally { retrying = false; retry.disabled = false; }
  });
  poll();
})();
