// Preview state is independent of the active sidebar limits reader.
export function createProviderPreview({request, onChange, session = "", now = Date.now, freshnessMs = 30000, deadlineMs = 10000}) {
  let catalog = null, selected = "", snapshot = null, failed = false, loading = false;
  let stopped = false, paused = false, generation = 0, revision = 0, catalogRead = null, previewRead = null;
  const cache = new Map();
  const state = () => ({catalog, selected, snapshot, failed, loading});
  const changed = () => { if (!stopped && !paused) onChange(state()); };
  const begin = () => {
    const abort = new AbortController(), ticket = generation;
    const timer = setTimeout(() => abort.abort(), deadlineMs);
    return {signal: abort.signal, cancel: () => {clearTimeout(timer); abort.abort();},
      current: () => !stopped && !paused && ticket === generation,
      finish: () => clearTimeout(timer), promise: null};
  };
  const retire = () => {
    ++generation; ++revision;
    catalogRead?.cancel(); previewRead?.cancel();
    catalogRead = null; previewRead = null; loading = false;
  };
  const refreshPreview = (force = false) => {
    const id = selected, selection = revision;
    if (!id || stopped || paused) return Promise.resolve();
    const saved = cache.get(id);
    if (!force && saved && now() - saved.at < freshnessMs) {
      snapshot = saved.value; failed = false; loading = false; changed(); return Promise.resolve();
    }
    if (previewRead?.current()) return previewRead.promise;
    const read = begin(); previewRead = read; loading = true; changed();
    read.promise = (async () => {
      try {
        const value = await request(`/api/codex-limits?provider=${encodeURIComponent(id)}`, {signal: read.signal});
        if (!read.current() || read.signal.aborted || selection !== revision || selected !== id) return;
        if (value.provider?.id !== id) throw new Error("Provider limits identity changed.");
        cache.set(id, {at: now(), value}); snapshot = value; failed = false;
      } catch (_) {
        if (read.current() && selection === revision && selected === id) {snapshot = saved?.value || null; failed = true;}
      } finally {
        read.finish();
        if (previewRead === read && read.current()) {previewRead = null; loading = false; changed();}
      }
    })();
    return read.promise;
  };
  const select = id => {
    if (!catalog?.providers.some(provider => provider.id === id)) return;
    previewRead?.cancel(); previewRead = null;
    selected = id; revision++; snapshot = cache.get(id)?.value || null; failed = false;
    changed(); void refreshPreview();
  };
  const refresh = () => {
    if (stopped || paused) return Promise.resolve();
    if (catalogRead?.current()) return catalogRead.promise;
    const read = begin(), scope = session; catalogRead = read;
    read.promise = (async () => {
      try {
        const next = await request(`/api/codex-providers${scope ? `?session=${encodeURIComponent(scope)}` : ""}`, {signal: read.signal});
        if (!read.current() || read.signal.aborted || scope !== session) return;
        if (!Array.isArray(next.providers) || !next.providers.some(provider => provider.id === next.active)) throw new Error("Invalid provider catalog");
        const wasActive = catalog?.active;
        catalog = next;
        if (wasActive && wasActive !== catalog.active) {cache.clear(); revision++; previewRead?.cancel(); previewRead = null; snapshot = null;}
        if (!catalog.providers.some(provider => provider.id === selected)) select(catalog.active);
        else changed();
        await refreshPreview();
      } catch (_) { if (read.current()) {failed = true; changed();} }
      finally {read.finish(); if (catalogRead === read && read.current()) catalogRead = null;}
    })();
    return read.promise;
  };
  return {state, select, refresh, refreshPreview,
    setSession(value) {retire(); session = value || ""; catalog = null; selected = ""; snapshot = null; changed(); return refresh();},
    invalidate() {retire(); cache.clear(); snapshot = null;},
    pause() {paused = true; retire();},
    resume() {if (!paused || stopped) return Promise.resolve(); paused = false; return refresh();},
    stop() {stopped = true; retire();}};
}

const stages = {accepted:"Request accepted",preparing:"Preparing the switch","waiting-package":"Waiting for the workspace package update","waiting-creation":"Waiting for session creation","starting-provider-check":"Starting the provider check","checking-provider":"Checking provider authentication and models","locking-sessions":"Waiting for session access","capturing-settings":"Saving thread settings",quiescing:"Pausing terminal clients",pausing:"Pausing work",closing:"Closing threads",reopening:"Applying the provider",resuming:"Restoring work","resuming-work":"Resuming work","restoring-terminals":"Restoring terminal clients",complete:"Threads restored"};

const reasons = {session_identity_changed:"The retained session conversation changed",session_runtime_busy:"Another operation still holds this session",session_lifecycle_unfinished:"An earlier session operation needs recovery",target_model_catalog_unavailable:"The target model catalog could not be reached",target_model_unavailable:"A retained model is unavailable on the target provider",target_model_or_effort_unavailable:"A retained model or reasoning effort is unavailable",subscription_authentication_unavailable:"The subscription account could not be verified",cold_permissions_unsupported:"Saved permissions cannot be restored safely",named_permission_profile_unsupported:"This named permission profile is not supported for switching",retained_policy_mismatch:"Effective permissions differ from the saved team policy",native_metadata_unavailable:"Native thread metadata could not be read",thread_identity_unproved:"Thread ownership could not be verified",native_settings_resume_failed:"Native thread settings could not be loaded"};

export function providerOperationText(operation, providers, feedbackOperationId = "") {
  if (!operation || operation.dismissed) return "";
  const target = providers.find(provider => provider.id === operation.target)?.label || operation.target;
  const scope = operation.session ? `Session ${operation.session}` : "Sessions using the workspace default";
  const progress = `${stages[operation.stage] || operation.stage || "Starting"}${operation.total > 0 ? ` (${operation.completed}/${operation.total})` : ""}${operation.thread ? ` · ${operation.thread}` : ""}`;
  const running = ["waiting", "applying"].includes(operation.phase);
  const end = running ? Date.now() : Date.parse(operation.finishedAt || operation.updatedAt || "");
  const seconds = Math.max(0, Math.floor((end - Date.parse(operation.createdAt || "")) / 1000));
  const elapsed = Number.isFinite(seconds) ? ` · ${seconds}s elapsed` : "";
  if (operation.cancelRequested && running) return `Canceling provider switch. Restoring paused terminal clients${elapsed}.`;
  const reason = operation.reason ? ` · ${reasons[operation.reason] || operation.reason.replaceAll("_", " ")}` : "";
  if (operation.phase === "waiting") return `${scope}: switching to ${target}. ${progress}${elapsed}.${operation.blockers?.length ? ` ${operation.blockers.join(" ")}` : ""}`;
  if (operation.phase === "applying") return `${scope}: switching to ${target}. ${progress}${elapsed}${reason}`;
  if (operation.phase === "applied") return operation.message || `${scope} now ${operation.session ? "uses" : "use"} ${target}.`;
  // Terminal cancellation receipts outlive the popup that initiated the action.
  if (operation.phase === "canceled") return feedbackOperationId && operation.id === feedbackOperationId ? "Provider switch canceled." : "";
  return `${operation.message || "Provider switch failed. Retry after checking its progress."}${operation.stage ? ` ${progress}${elapsed}${reason}` : ""}`;
}

export function providerOperationView(operation, providers, feedbackOperationId = "") {
  const summary = providerOperationText(operation, providers, feedbackOperationId);
  if (!summary) return null;
  const running = ["waiting", "applying"].includes(operation.phase);
  const finish = running ? Date.now() : Date.parse(operation.finishedAt || operation.updatedAt || "");
  const seconds = Math.max(0, Math.floor((finish - Date.parse(operation.createdAt || "")) / 1000));
  const elapsed = Number.isFinite(seconds) ? (seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`) : "";
  const target = providers.find(provider => provider.id === operation.target)?.label || operation.target;
  const title = operation.cancelRequested && running ? "Canceling provider switch" :
    ({failed:"Provider switch failed", "recovery-required":"Provider switch paused", applied:"Provider changed", canceled:"Provider switch canceled."}[operation.phase] || `Switching to ${target}`);
  return {
    title, target, scope: operation.session || "Sessions using the workspace default",
    message: operation.message || "", stage: stages[operation.stage] || operation.stage || "",
    count: operation.total > 0 ? `${operation.completed || 0} of ${operation.total}` : "",
    thread: operation.thread || "", threadLabel: operation.threadLabel || "",
    reason: reasons[operation.reason] || operation.reason?.replaceAll("_", " ") || "",
    blockers: operation.blockers || [], elapsed, phase: operation.phase,
  };
}

export function renderProviderOperation(container, operation, providers, feedbackOperationId, createElement) {
  const view = providerOperationView(operation, providers, feedbackOperationId);
  const key = JSON.stringify(view && {...view, elapsed: ""});
  container.hidden = !view;
  if (container.providerViewKey === key) {
    if (container.providerElapsed) container.providerElapsed.textContent = view.elapsed;
    return;
  }
  container.providerViewKey = key;
  container.providerElapsed = null;
  container.replaceChildren();
  if (!view) return;
  container.className = `provider-operation ${["failed", "recovery-required"].includes(view.phase) ? "error" : ""}`;
  const node = (tag, text) => { const element = createElement(tag); element.textContent = text; return element; };
  container.append(node("strong", view.title));
  if (view.message && view.message !== view.title) container.append(node("p", view.message));
  const facts = createElement("dl");
  const fact = (label, value) => {
    if (!value) return null;
    const definition = node("dd", value);
    facts.append(node("dt", label), definition);
    return definition;
  };
  fact("Applies to", view.scope);
  fact("Provider", view.target);
  fact("Step", view.stage);
  fact("Threads completed", view.count);
  fact("Current thread", view.threadLabel || (view.thread ? "Retained thread" : ""));
  container.providerElapsed = fact("Elapsed", view.elapsed);
  if (container.providerElapsed) container.providerElapsed.setAttribute("aria-live", "off");
  container.append(facts);
  if (view.reason) container.append(node("p", view.reason));
  if (view.blockers.length) {
    const blockers = createElement("ul");
    for (const blocker of view.blockers) blockers.append(node("li", blocker));
    container.append(blockers);
  }
  if (view.thread) {
    const details = createElement("details");
    details.append(node("summary", "Technical details"), node("code", view.thread));
    container.append(details);
  }
}
