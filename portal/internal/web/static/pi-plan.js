// Mode controls remain absent unless the server supplies real policy support.
// The owner injects the current Plan panel's DOM hooks; this module owns identity.
export function mountPiPlan({root, api, storage, namespace, onChange = () => {},
  render = () => {}, randomUUID = () => globalThis.crypto.randomUUID()}) {
  let snapshot = {revision: -1, proposals: [], acceptances: [], canSelectMode: false};
  let destroyed = false, busy = false, error = '', pending = null;
  let modeUncertain = false, modeEpoch = 0, readSequence = 0, latestRead = 0;
  const validPolicy = value => ['plan', 'default'].includes(value.mode) &&
    Number.isSafeInteger(value.toolPolicyRevision) && value.toolPolicyRevision > 0 &&
    /^[0-9a-f]{64}$/.test(value.toolPolicyDigest || '');
  const canSelectMode = () => typeof api.setMode === 'function' && snapshot.canSelectMode &&
    validPolicy(snapshot) && !busy && !pending && !modeUncertain;
  const key = `pi-plan:${namespace}:acceptance`;
  try { pending = JSON.parse(storage?.getItem(key) || 'null'); } catch (_) {}
  const emit = () => { if (!destroyed) { render(root, {snapshot, busy, error, pending, modeUncertain,
    canSelectMode: canSelectMode(), accept, check, selectMode, refresh}); onChange(); } };
  const save = value => {
    if (!storage) throw Error('Browser storage is unavailable. The plan has not been submitted.');
    const data = JSON.stringify(value); storage.setItem(key, data);
    if (storage.getItem(key) !== data) throw Error('Browser storage did not retain the plan acceptance.');
  };
  function update(value) {
    if (destroyed || !value || !Number.isSafeInteger(value.revision) || value.revision < snapshot.revision) return false;
    snapshot = value;
    if (pending && snapshot.acceptances.some(a => a.acceptanceId === pending.acceptanceId)) {
      pending = null; try { save(null); } catch (e) { error = e.message; }
    }
    emit();
    return true;
  }
  async function readSnapshot(clearUncertainty) {
    if (destroyed) return false;
    const epoch = modeEpoch, sequence = ++readSequence;
    const canClear = clearUncertainty && modeUncertain && !busy;
    const value = await api.read({cache: 'no-store'});
    // A policy may change without changing the domain snapshot revision. An old
    // request cannot restore the pre-mutation observation or clear uncertainty.
    if (destroyed || epoch !== modeEpoch || sequence < latestRead) return false;
    latestRead = sequence;
    if (!update(value)) return false;
    if (canClear && modeUncertain && snapshot.canSelectMode && validPolicy(snapshot)) {
      modeUncertain = false; error = ''; emit();
    }
    return true;
  }
  async function refresh() { return readSnapshot(true); }
  async function check() {
    if (destroyed || !pending || busy) return;
    busy = true; emit();
    try { await api.acceptanceStatus(pending.acceptanceId); await readSnapshot(false); }
    catch (_) { error = 'Could not check the plan status.'; }
    finally { busy = false; emit(); }
  }
  async function accept(planId) {
    const p = snapshot.proposals.find(p => p.planId === planId);
    if (destroyed || busy || pending || modeUncertain || !p?.complete || !p.canAccept) return;
    const payload = {acceptanceId: randomUUID(), planId: p.planId, revision: p.revision, textSha256: p.textSha256};
    try { save(payload); } catch (e) { error = e.message; emit(); return; }
    pending = payload; busy = true; error = ''; emit();
    try { await api.accept(payload); await readSnapshot(false); }
    catch (_) { error = 'Your plan request is saved in this browser. Check its status before continuing.'; }
    finally { busy = false; emit(); }
  }
  async function selectMode(mode) {
    if (destroyed || !['plan', 'default'].includes(mode) || !canSelectMode()) return;
    const payload = {mode, expectedRevision: snapshot.toolPolicyRevision, expectedDigest: snapshot.toolPolicyDigest};
    const originalMode = snapshot.mode;
    modeEpoch++; busy = true; error = ''; emit();
    try {
      const result = await api.setMode(payload);
      const proved = result?.outcome === 'applied' && result.policy?.mode === mode &&
        result.policy.revision === payload.expectedRevision + (mode === originalMode ? 0 : 1) &&
        /^[0-9a-f]{64}$/.test(result.policy.digest || '');
      if (!proved && !(result?.outcome === 'rejected' && !result.policy)) throw Error('Unconfirmed mode');
      if (result.outcome === 'rejected') error = 'The mode change was rejected.';
      if (!await readSnapshot(false)) throw Error('Current policy is unavailable');
    } catch (_) {
      modeUncertain = true;
      error = 'Could not confirm the mode change. Refresh the current policy before selecting a mode again.';
    } finally { busy = false; emit(); }
  }
  return {update, refresh, accept, check, selectMode, destroy() { destroyed = true; }};
}
