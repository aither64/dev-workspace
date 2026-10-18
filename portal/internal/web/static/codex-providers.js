// Preview state is independent of the active sidebar limits reader.
export function createProviderPreview({request, onChange, session = "", now = Date.now, freshnessMs = 30000}) {
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
      const scope = session;
      const next = await request(`/api/codex-providers${scope ? `?session=${encodeURIComponent(scope)}` : ""}`);
      if (stopped) return;
      if (scope !== session) return;
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
    setSession(value) {session = value || ""; catalog = null; selected = ""; revision++; snapshot = null; changed(); return refresh();},
    invalidate() {cache.clear(); revision++; snapshot = null;},
    stop() {stopped = true; revision++;}};
}

export function providerOperationText(operation, providers, feedbackOperationId = "") {
  if (!operation) return "";
  const target = providers.find(provider => provider.id === operation.target)?.label || operation.target;
  const scope = operation.session ? `Session ${operation.session}` : "Sessions using the workspace default";
  const stages = {accepted:"Request accepted",preparing:"Preparing the switch","waiting-package":"Waiting for the workspace package update","waiting-creation":"Waiting for session creation","starting-provider-check":"Starting the provider check","checking-provider":"Checking provider authentication and models","locking-sessions":"Waiting for session access","capturing-settings":"Saving thread settings",quiescing:"Pausing terminal clients",pausing:"Pausing work",closing:"Closing threads",reopening:"Applying the provider",resuming:"Restoring work","resuming-work":"Resuming work","restoring-terminals":"Restoring terminal clients",complete:"Threads restored"};
  const progress = `${stages[operation.stage] || operation.stage || "Starting"}${operation.total > 0 ? ` (${operation.completed}/${operation.total})` : ""}${operation.thread ? ` · ${operation.thread}` : ""}`;
  const elapsed = operation.createdAt ? ` · ${Math.max(0,Math.floor((Date.now()-Date.parse(operation.createdAt))/1000))}s elapsed` : "";
  const reasons = {session_identity_changed:"The retained session conversation changed",session_runtime_busy:"Another operation still holds this session",session_lifecycle_unfinished:"An earlier session operation needs recovery",target_model_catalog_unavailable:"The target model catalog could not be reached",target_model_unavailable:"A retained model is unavailable on the target provider",target_model_or_effort_unavailable:"A retained model or reasoning effort is unavailable",subscription_authentication_unavailable:"The subscription account could not be verified",cold_permissions_unsupported:"Saved permissions cannot be restored safely",named_permission_profile_unsupported:"This named permission profile is not supported for switching",retained_policy_mismatch:"Effective permissions differ from the saved team policy",native_metadata_unavailable:"Native thread metadata could not be read",thread_identity_unproved:"Thread ownership could not be verified",native_settings_resume_failed:"Native thread settings could not be loaded"};
  const reason = operation.reason ? ` · ${reasons[operation.reason] || operation.reason.replaceAll("_", " ")}` : "";
  if (operation.phase === "waiting") return `${scope}: switching to ${target}. ${progress}${elapsed}.${operation.blockers?.length ? ` ${operation.blockers.join(" ")}` : ""}`;
  if (operation.phase === "applying") return `${scope}: switching to ${target}. ${progress}${elapsed}${reason}`;
  if (operation.phase === "applied") return operation.message || `${scope} now ${operation.session ? "uses" : "use"} ${target}.`;
  // Terminal cancellation receipts outlive the popup that initiated the action.
  if (operation.phase === "canceled") return feedbackOperationId && operation.id === feedbackOperationId ? "Provider switch canceled." : "";
  return `${operation.message || "Provider switch failed. Retry after checking its progress."}${operation.stage ? ` ${progress}${elapsed}${reason}` : ""}`;
}
