/* Shared by documents and the service worker; identity binds workspace, retained
 * directory and root thread. IndexedDB and BroadcastChannel are origin scoped. */
(() => {
  "use strict";
  let opening;
  const open = () => opening ||= new Promise((resolve, reject) => {
    const request = indexedDB.open("workspace-session-attention", 1);
    request.onupgradeneeded = () => request.result.createObjectStore("sessions", {keyPath: "identity"});
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => { opening = null; reject(new Error("Attention storage is unavailable.")); };
  });
  const update = async (identity, change = value => value) => {
    const db = await open();
    return new Promise((resolve, reject) => {
      const tx = db.transaction("sessions", "readwrite"), store = tx.objectStore("sessions");
      let result;
      const read = store.get(identity);
      read.onsuccess = () => {
        const value = read.result || {identity, seen: [], revision: 0, event: null};
        result = change(value);
        result.identity = identity; result.at = Date.now();
        result.seen = [...new Set(result.seen || [])].slice(-32);
        store.put(result);
        const all = store.getAll();
        all.onsuccess = () => all.result.sort((a,b) => b.at-a.at).slice(512).forEach(item => store.delete(item.identity));
      };
      tx.oncomplete = () => resolve(result);
      tx.onerror = tx.onabort = () => reject(new Error("Attention storage is unavailable."));
    });
  };
  const read = async identity => {
    const db = await open();
    return new Promise((resolve, reject) => {
      const tx = db.transaction("sessions", "readonly"), request = tx.objectStore("sessions").get(identity);
      request.onsuccess = () => resolve(request.result || null);
      request.onerror = () => reject(new Error("Attention storage is unavailable."));
    });
  };
  const testDescriptor = value => value && typeof value.id === "string" && /^[0-9a-f]{64}$/.test(value.id) &&
    typeof value.identity === "string" && /^test-[0-9a-f]{64}$/.test(value.identity) &&
    Number.isSafeInteger(value.revision) && value.revision > 0 && Number.isSafeInteger(value.expiresAt) && value.expiresAt > 0 ?
    {id: value.id, identity: value.identity, revision: value.revision, expiresAt: value.expiresAt} : null;
  const testMatches = (a, b) => Boolean(a && b && a.identity === b.identity && a.id === b.id && a.revision === b.revision && a.expiresAt === b.expiresAt);
  const testError = error => ["NotAllowedError", "NotSupportedError", "SecurityError", "InvalidStateError", "TypeError", "AbortError"].includes(error?.name) ? error.name : "UnknownError";
  const saveTest = (input, outcome) => {
    const descriptor = testDescriptor(input);
    if (!descriptor) return Promise.reject(new Error("Invalid test descriptor."));
    return update(descriptor.identity, value => {
      const current = value.testPending, latest = value.testOutcome;
      // Equal revisions with another ID cannot prove ordering. Never let a late
      // document or old push replace a newer test's descriptor or outcome.
      if ([current, latest].some(item => item && (item.revision > descriptor.revision ||
          (item.revision === descriptor.revision && !testMatches(item, descriptor))))) return value;
      if (!outcome) value.testPending = descriptor;
      else if (!testMatches(latest, descriptor) || latest.display === "pending") value.testOutcome = {...descriptor, received: true,
        display: ["pending", "resolved", "rejected"].includes(outcome.display) ? outcome.display : "pending",
        browserRecord: ["present", "absent", "unavailable"].includes(outcome.browserRecord) ? outcome.browserRecord : "unavailable",
        ...(outcome.display === "rejected" ? {error: testError({name: outcome.error})} : {})};
      return value;
    });
  };
  globalThis.workspaceAttentionStore = {update, read, testDescriptor, testMatches, testError, saveTest};
})();
