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
  globalThis.workspaceAttentionStore = {update};
})();
