// Preview state is independent of the active sidebar limits reader.
export function createProviderPreview({request, onChange, now = Date.now, freshnessMs = 30000}) {
  let catalog = null, selected = "", snapshot = null, failed = false, loading = false;
  let stopped = false, revision = 0;
  const cache = new Map(), pending = new Map();
  const state = () => ({catalog, selected, snapshot, failed, loading});
  const changed = () => { if (!stopped) onChange(state()); };
  const refreshPreview = async (force = false) => {
    const id = selected, generation = revision;
    if (!id || stopped) return;
    const saved = cache.get(id);
    if (!force && saved && now() - saved.at < freshnessMs) {
      snapshot = saved.value; failed = false; loading = false; changed(); return;
    }
    loading = true; changed();
    let read = pending.get(id);
    if (!read) {
      read = request(`/api/codex-limits?provider=${encodeURIComponent(id)}`).then(value => {
        if (value.provider?.id !== id) throw new Error("Provider limits identity changed.");
        cache.set(id, {at: now(), value}); return value;
      }).finally(() => pending.delete(id));
      pending.set(id, read);
    }
    try {
      const value = await read;
      if (stopped || selected !== id || revision !== generation) return;
      snapshot = value; failed = false;
    } catch (_) {
      if (stopped || selected !== id || revision !== generation) return;
      snapshot = saved?.value || null; failed = true;
    } finally {
      if (!stopped && selected === id && revision === generation) { loading = false; changed(); }
    }
  };
  const select = id => {
    if (!catalog?.providers.some(provider => provider.id === id)) return;
    selected = id; revision++; snapshot = cache.get(id)?.value || null; failed = false;
    changed(); void refreshPreview();
  };
  const refresh = async () => {
    try {
      const next = await request("/api/codex-providers");
      if (stopped) return;
      if (!Array.isArray(next.providers) || !next.providers.some(provider => provider.id === next.active)) throw new Error("Invalid provider catalog");
      const wasActive = catalog?.active;
      catalog = next;
      if (!catalog.providers.some(provider => provider.id === selected)) select(catalog.active);
      else changed();
      if (wasActive && wasActive !== catalog.active) { cache.clear(); revision++; snapshot = null; changed(); }
      await refreshPreview();
    } catch (_) { if (!stopped) { failed = true; changed(); } }
  };
  return {state, select, refresh, refreshPreview,
    invalidate() {cache.clear(); revision++; snapshot = null;},
    stop() {stopped = true; revision++;}};
}

export function providerOperationText(operation, providers, feedbackOperationId = "") {
  if (!operation) return "";
  const target = providers.find(provider => provider.id === operation.target)?.label || operation.target;
  if (operation.phase === "waiting") return `Waiting to switch to ${target} when all workspace threads are idle.${operation.blockers?.length ? ` ${operation.blockers.join(" ")}` : ""}`;
  if (operation.phase === "applying") return `Switching workspace threads to ${target}…`;
  if (operation.phase === "applied") return operation.message || `Workspace now uses ${target}.`;
  // Terminal cancellation receipts outlive the popup that initiated the action.
  if (operation.phase === "canceled") return feedbackOperationId && operation.id === feedbackOperationId ? "Provider switch canceled." : "";
  return operation.message || "Provider switch failed. Refresh its status before retrying.";
}
