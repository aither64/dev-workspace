// Every tab coordinates through the same origin-scoped lock. Reload the durable
// attempt under that lock before creating, retrying or clearing a redemption.
export function createResetCreditAction({request, store, withLock, refresh, onChange, confirm, randomUUID}) {
  let unavailable = false, pending = null, busy = false;
  const read = () => {
    const loaded = store.load();
    unavailable = typeof withLock !== "function" || !loaded || loaded.length > 1;
    pending = loaded?.[0] || null;
  };
  read();
  const state = () => ({pending, busy, unavailable});
  const publish = () => onChange?.(state());
  const coordinated = async operation => {
    if (busy || typeof withLock !== "function") return;
    busy = true; publish();
    try {
      return await withLock(async () => {
        read(); publish();
        if (unavailable) throw new Error("The saved reset attempt could not be read. The reset was not sent.");
        return operation();
      });
    } finally { busy = false; read(); publish(); }
  };
  const submit = async () => {
    if (!pending) return;
    const attempt = pending;
    const result = await request("/api/codex-limits/reset", {method: "POST", body: JSON.stringify({
      idempotencyKey: attempt.id, creditId: attempt.creditId, accountScope: attempt.accountScope,
    })});
    if (!["reset", "alreadyRedeemed", "nothingToReset", "noCredit"].includes(result.outcome)) {
      throw new Error("The reset outcome could not be confirmed. Retry the saved attempt.");
    }
    if (!store.remove(attempt.id)) throw new Error("The completed reset attempt could not be cleared. Retry the saved attempt.");
    pending = null;
    await refresh();
    return result.outcome;
  };
  return {
    state,
    reload() { if (!busy) { read(); publish(); } },
    use({creditId = "", accountScope, description}) {
      return coordinated(async () => {
        if (pending) throw new Error("Resolve the saved reset attempt before using another reset.");
        if (!accountScope) throw new Error("The current Codex account could not be verified.");
        if (!confirm(`Use ${description || "the next available reset"}? This consumes one banked reset if a limit is eligible.`)) return;
        if (!store.available()) throw new Error("Browser storage is unavailable. The reset was not sent.");
        const attempt = {id: randomUUID(), creditId, accountScope};
        if (!store.store(attempt)) throw new Error("The reset attempt could not be saved. The reset was not sent.");
        pending = attempt; publish();
        return submit();
      });
    },
    retry() { return coordinated(submit); },
  };
}
