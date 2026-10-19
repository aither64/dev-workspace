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
      canSetName: next.canSetName === true && ["failed", "paused"].includes(next.state),
      stopped: ["gone", "conflict", "cancelled"].includes(next.state),
    };
  };
  const ignorePreparationStatus = (next, previous) => {
    if (!previous) return false;
    if (Number.isSafeInteger(next.preparationAttempt) && Number.isSafeInteger(previous.preparationAttempt) &&
        next.preparationAttempt !== previous.preparationAttempt) return next.preparationAttempt < previous.preparationAttempt;
    if (previous.phase === "initializing" && next.phase !== "initializing") return true;
    if (next.phase === "initializing" && previous.phase !== "initializing") return false;
    if (["ready", "gone", "conflict", "cancelled"].includes(next.state)) return false;
    return next.attempt < previous.attempt;
  };
  const createNameSelection = ({storage, identity, fetch: fetchRequest, helpers}) => {
    const key = `workspace-portal.creation-name.${identity.requestId}`;
    const storageError = () => new Error("Unable to save the name choice. Enable browser storage and try again.");
    let saved;
    try {
      saved = JSON.parse(storage.getItem(key) || "null");
      if (saved && (saved.receiptId !== identity.receiptId || !Number.isSafeInteger(saved.attempt) || saved.attempt < 1 ||
          typeof saved.name !== "string" || !/^[A-Za-z0-9][A-Za-z0-9_-]{0,47}$/.test(saved.name))) throw storageError();
    } catch (_) { throw storageError(); }
    const clear = () => {
      try {
        storage.removeItem(key);
        if (storage.getItem(key) !== null) throw storageError();
        saved = null;
      } catch (_) { throw storageError(); }
    };
    return {
      get pending() { return saved ? {...saved} : null; },
      async submit(attempt, name) {
        const body = saved || {receiptId: identity.receiptId, attempt, name: name.trim()};
        try {
          const encoded = JSON.stringify(body);
          storage.setItem(key, encoded);
          if (storage.getItem(key) !== encoded) throw storageError();
        } catch (_) { throw storageError(); }
        saved = body;
        const response = await fetchRequest(`/api/session-creations/${identity.requestId}/name`, {
          method: "POST", credentials: "same-origin", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body),
        });
        const next = await response.json();
        if (!response.ok) {
          if ([400, 403, 404, 409].includes(response.status)) clear();
          throw new Error(next.error || "Unable to confirm the name choice. Retry the saved name.");
        }
        if (!helpers.acceptedStatus(next, identity.requestId, identity.receiptId) || next.startedAt !== identity.startedAt || next.preparationAttempt !== body.attempt + 1) {
          throw new Error("Unable to confirm the name choice. Retry the saved name.");
        }
        clear();
        return next;
      },
    };
  };
  if (typeof module !== "undefined" && module.exports) {
    module.exports = {phaseLabel, preparationProgress, ignorePreparationStatus, createNameSelection}; return;
  }
  const slug = document.body.dataset.creation;
  const requestId = document.body.dataset.preparation;
  if (!slug && !requestId) return;
  const identity = {requestId, receiptId: document.body.dataset.receiptId, startedAt: document.body.dataset.acceptedAt};
  const endpoint = requestId ? `/api/session-creations/${requestId}` : `/api/sessions/${encodeURIComponent(slug)}/creation`;
  const title = document.getElementById("creation-title");
  const phase = document.getElementById("creation-phase");
  const error = document.getElementById("creation-error");
  const errorCopy = document.getElementById("creation-error-copy");
  const retry = document.getElementById("creation-retry");
  const elapsed = document.getElementById("creation-elapsed");
  const source = document.getElementById("creation-source");
  const leaveNote = document.getElementById("creation-leave-note");
  const initialRequest = document.getElementById("creation-request");
  const requestPanel = document.getElementById("creation-request-panel");
  const nameForm = document.getElementById("creation-name-form");
  const nameInput = document.getElementById("creation-name");
  const nameSubmit = document.getElementById("creation-name-submit");
  let nameSelection, nameStorageError;
  if (requestId) {
    try { nameSelection = createNameSelection({storage: window.sessionStorage, identity, fetch: globalThis.fetch, helpers: sessionPreparation}); }
    catch (failure) { nameStorageError = failure.message; }
  }
  // The prompt is server-rendered, so asset or polling failures cannot hide it.
  void import("/codex/assets/conversation.js?v=12").then(({createCopyButton}) => {
    document.getElementById("creation-request-copy").append(createCopyButton({
      getText: () => initialRequest.textContent, label: "Copy initial request",
    }));
    errorCopy.append(createCopyButton({getText: () => error.textContent, label: "Copy error"}));
  }).catch(() => {});
  let receipt, retrying = false, stopped = false;
  function showError(message) {
    // Replacing an unchanged text node clears the user's selection each poll.
    if (error.textContent !== message) error.textContent = message;
    error.hidden = !message;
    errorCopy.hidden = !message;
  }
  async function request(url, options) {
    const response = await fetch(url, {credentials: "same-origin", cache: "no-store", ...options});
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Unable to read initialization status.");
    return result;
  }
  function render(next) {
    const presentation = requestId ? preparationProgress(next, identity, sessionPreparation) : null;
    if (requestId ? ignorePreparationStatus(next, receipt) : receipt && next.attempt < receipt.attempt) return;
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
    leaveNote.hidden = stopped || next.state === "cancelled" || presentation?.canSetName;
    phase.textContent = requestId ? presentation.phase : next.phase;
    const startedAt = requestId ? identity.startedAt : next.startedAt;
    const seconds = Math.max(0, Math.floor((Date.now() - Date.parse(startedAt)) / 1000));
    elapsed.textContent = Number.isFinite(seconds) ? `Elapsed: ${Math.floor(seconds / 60)}m ${seconds % 60}s` : "";
    const sourceURL = !requestId && /^\/[A-Za-z0-9][A-Za-z0-9_-]*\/$/.test(next.sourceUrl || "") ? next.sourceUrl : "";
    source.hidden = !sourceURL || next.state === "running";
    if (sourceURL) source.href = sourceURL;
    else source.removeAttribute("href");
    showError(nameStorageError || next.error || "");
    retry.hidden = requestId ? !presentation.retry : !["failed", "paused"].includes(next.state);
    retry.textContent = presentation?.canSetName ? "Retry naming" : requestId ? "Retry creation" : "Retry initialization";
    retry.disabled = retrying || Boolean(nameSelection?.pending);
    if (nameForm) {
      const pending = nameSelection?.pending;
      nameForm.hidden = !presentation.canSetName && !pending;
      if (pending) nameInput.value = pending.name;
      nameInput.disabled = retrying || Boolean(pending) || Boolean(nameStorageError);
      nameInput.required = !pending;
      nameSubmit.disabled = retrying || Boolean(nameStorageError);
      nameSubmit.textContent = pending ? "Retry saved name" : "Use name and continue";
    }
  }
  async function poll() {
    try { render(await request(endpoint)); }
    catch (failure) { showError(failure.message); }
    finally { if (!stopped) window.setTimeout(poll, 1000); }
  }
  retry.addEventListener("click", async () => {
    if (!receipt || retrying || stopped || nameSelection?.pending) return;
    retrying = true; retry.disabled = true;
    try {
      render(await request(`${endpoint}/retry`, {
        method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({receiptId: receipt.receiptId, attempt: receipt.attempt}),
      }));
    } catch (failure) { showError(failure.message); }
    finally { retrying = false; retry.disabled = false; }
  });
  nameForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!receipt || retrying || stopped || !nameSelection) return;
    retrying = true; render(receipt);
    let nameFailure;
    try { render(await nameSelection.submit(receipt.attempt, nameInput.value)); }
    catch (failure) { nameFailure = failure.message; }
    finally {
      retrying = false; render(receipt);
      if (nameFailure) showError(nameFailure);
    }
  });
  poll();
})();
