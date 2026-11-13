// Mode controls remain absent unless the server supplies real policy support.
// The owner injects the current Plan panel's DOM hooks; this module owns identity.
export function mountPiPlan({root, api, storage, namespace, onChange = () => {},
  render = () => {}, randomUUID = () => globalThis.crypto.randomUUID()}) {
  let snapshot = {revision: -1, proposals: [], acceptances: [], canSelectMode: false};
  let destroyed = false, busy = false, error = '', pending = null;
  const key = `pi-plan:${namespace}:acceptance`;
  try { pending = JSON.parse(storage?.getItem(key) || 'null'); } catch (_) {}
  const emit = () => { if (!destroyed) { render(root, {snapshot, busy, error, pending, accept, check}); onChange(); } };
  const save = value => {
    if (!storage) throw Error('Browser storage is unavailable. The plan has not been submitted.');
    const data = JSON.stringify(value); storage.setItem(key, data);
    if (storage.getItem(key) !== data) throw Error('Browser storage did not retain the plan acceptance.');
  };
  function update(value) {
    if (destroyed || !value || !Number.isSafeInteger(value.revision) || value.revision < snapshot.revision) return;
    snapshot = value;
    if (pending && snapshot.acceptances.some(a => a.acceptanceId === pending.acceptanceId)) {
      pending = null; try { save(null); } catch (e) { error = e.message; }
    }
    emit();
  }
  async function refresh() { update(await api.read()); }
  async function check() {
    if (destroyed || !pending || busy) return;
    busy = true; emit();
    try { await api.acceptanceStatus(pending.acceptanceId); await refresh(); }
    catch (_) { error = 'Could not check the plan status.'; }
    finally { busy = false; emit(); }
  }
  async function accept(planId) {
    const p = snapshot.proposals.find(p => p.planId === planId);
    if (destroyed || busy || pending || !p?.complete || !p.canAccept) return;
    const payload = {acceptanceId: randomUUID(), planId: p.planId, revision: p.revision, textSha256: p.textSha256};
    try { save(payload); } catch (e) { error = e.message; emit(); return; }
    pending = payload; busy = true; error = ''; emit();
    try { await api.accept(payload); await refresh(); }
    catch (_) { error = 'Your plan request is saved in this browser. Check its status before continuing.'; }
    finally { busy = false; emit(); }
  }
  return {update, refresh, accept, check, destroy() { destroyed = true; }};
}
