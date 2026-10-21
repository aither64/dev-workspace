/* Shared by documents and the service worker; identity binds workspace, retained
 * directory and root thread. IndexedDB and BroadcastChannel are origin scoped. */
(() => {
  "use strict";
  const soundIdentity = "browser-notification-sound";
  const deliveryIdentity = "browser-notification-delivery";
  let deliveryManaged = false;
  let opening;
  const open = () => opening ||= new Promise((resolve, reject) => {
    const request = indexedDB.open("workspace-session-attention", 1);
    request.onupgradeneeded = () => request.result.createObjectStore("sessions", {keyPath: "identity"});
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => { opening = null; reject(new Error("Attention storage is unavailable.")); };
  });
  const update = async (identity, change = value => value) => {
    if ([soundIdentity, deliveryIdentity].includes(identity)) throw new Error("Reserved attention identity.");
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
        all.onsuccess = () => all.result.filter(item => ![soundIdentity, deliveryIdentity].includes(item.identity)).sort((a,b) => b.at-a.at).slice(512).forEach(item => store.delete(item.identity));
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
  // Preference reads and writes resolve only after commit. Bound the whole
  // operation, including opening storage, and abort an overdue write if possible.
  const sound = (write, playSound) => new Promise((resolve, reject) => {
    let tx, finished = false, result;
    const finish = (error) => {
      if (finished) return;
      finished = true; clearTimeout(timer);
      if (error) reject(new Error("Notification sound storage is unavailable."));
      else resolve(result);
    };
    const timer = setTimeout(() => {
      finish(true);
      try { tx?.abort(); } catch (_) {}
    }, 750);
    open().then(db => {
      if (finished) return;
      try {
        tx = db.transaction("sessions", write ? "readwrite" : "readonly");
        tx.oncomplete = () => finish(false);
        tx.onerror = tx.onabort = () => finish(true);
        const store = tx.objectStore("sessions");
        if (write) {
          // A retention rank, not a timestamp: already-open predecessor pages
          // also sort this record ahead of ordinary Date.now() attention data.
          store.put({identity: soundIdentity, playSound, at: Number.MAX_SAFE_INTEGER});
          result = playSound;
        } else {
          const request = store.get(soundIdentity);
          request.onerror = () => finish(true);
          request.onsuccess = () => {
            const value = request.result;
            if (value === undefined) result = true;
            else if (value && value.identity === soundIdentity && Object.keys(value).length === 3 && value.at === Number.MAX_SAFE_INTEGER && typeof value.playSound === "boolean") result = value.playSound;
            else finish(true);
          };
        }
      } catch (_) { finish(true); }
    }, () => finish(true));
  });
  const readSound = () => sound(false);
  const writeSound = playSound => typeof playSound === "boolean" ? sound(true, playSound) : Promise.reject(new Error("Invalid sound preference."));
  // One reserved v1 record fences transport epochs, independently of attention.
  // A write with expectedToken is a compare-and-set, committed in the same tx.
  const validGate = value => value && value.identity === deliveryIdentity &&
    Object.keys(value).length === 5 && value.at === Number.MAX_SAFE_INTEGER &&
    typeof value.enabled === "boolean" && typeof value.actionToken === "string" &&
    value.actionToken.length > 0 && value.actionToken.length <= 128 &&
    typeof value.registrationId === "string" && (value.enabled ? /^[0-9a-f]{64}$/.test(value.registrationId) : value.registrationId === "");
  const deliveryGate = (next, expectedToken) => new Promise((resolve, reject) => {
    let tx, finished = false, result;
    const finish = error => {
      if (finished) return;
      finished = true; clearTimeout(timer);
      if (error) reject(new Error("This browser cannot save notification settings."));
      else {if (result?.identity === deliveryIdentity) deliveryManaged = true; resolve(result);}
    };
    const timer = setTimeout(() => {finish(true); try {tx?.abort();} catch (_) {}}, 750);
    open().then(db => {
      if (finished) return;
      try {
        tx = db.transaction("sessions", next ? "readwrite" : "readonly");
        tx.oncomplete = () => finish(false);
        tx.onerror = tx.onabort = () => finish(true);
        const store = tx.objectStore("sessions"), request = store.get(deliveryIdentity);
        request.onerror = () => finish(true);
        request.onsuccess = () => {
          const prior = request.result;
          if (!next) {
            if (prior === undefined && !deliveryManaged) result = null; // Legacy installation.
            else if (prior === undefined) finish(true); // Unexpected loss cannot unmanage this context.
            else if (validGate(prior)) result = prior;
            else finish(true);
            return;
          }
          if (expectedToken !== undefined && (expectedToken === null ? prior !== undefined : !validGate(prior) || prior.actionToken !== expectedToken)) {result = null; return;}
          result = {...next, identity: deliveryIdentity, at: Number.MAX_SAFE_INTEGER};
          if (!validGate(result)) {finish(true); try {tx.abort();} catch (_) {} return;}
          store.put(result);
        };
      } catch (_) {finish(true);}
    }, () => finish(true));
  });
  const readDeliveryGate = () => deliveryGate();
  const writeDeliveryGate = (value, expectedToken) => deliveryGate(value, expectedToken);
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
  globalThis.workspaceAttentionStore = {update, read, readSound, writeSound, readDeliveryGate, writeDeliveryGate, testDescriptor, testMatches, testError, saveTest};
})();
