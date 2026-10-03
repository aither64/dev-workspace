// New session's tab-local request record. Plan and fork drafts use their
// existing stores. Once a POST might have reached the server, only this exact
// serialized body may be sent again, including after a status lookup says 404.
(() => {
  "use strict";
  const draftKey = "workspace-portal.creation-draft";
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
  const preparationURL = (id) => uuid.test(id) ? `/creations/${id}/` : "";
  const uploadScope = (scope) => Boolean(scope && typeof scope.id === "string" && uuid.test(scope.id) && scope.url === `/uploads/d-${scope.id}`);
  const acceptedStatus = (status, id, receiptId = "") => Boolean(
    status && status.requestId === id && preparationURL(id) && status.url === preparationURL(id) &&
    /^[0-9a-f]{64}$/.test(status.receiptId) && (!receiptId || status.receiptId === receiptId) &&
    Number.isSafeInteger(status.attempt) && status.attempt > 0,
  );
  const sessionURL = (status) => {
    if (status?.state !== "ready" || typeof status.slug !== "string" ||
        !/^\d{4}-\d{2}-\d{2}-[A-Za-z0-9][A-Za-z0-9_-]{0,47}$/.test(status.slug)) return "";
    return status.canonicalUrl === `/${status.slug}/` ? status.canonicalUrl : "";
  };
  const storageError = () => new Error("Unable to verify browser storage. Keep this tab open and retry recovery after enabling storage.");
  const verifyWrite = (storage, key, value) => {
    try {
      if (!storage) throw storageError();
      storage.setItem(key, value);
      if (storage.getItem(key) !== value) throw storageError();
    } catch (_) { throw storageError(); }
  };
  const verifyRemove = (storage, key) => {
    try {
      if (!storage) throw storageError();
      storage.removeItem(key);
      if (storage.getItem(key) !== null) throw storageError();
    } catch (_) { throw storageError(); }
  };
  const fieldNames = ["name", "goal", "date", "model", "effort", "team", "catalogDigest"];
  const validFields = (value) => fieldNames.every((key) => typeof value[key] === "string") &&
    /^\d{4}-\d{2}-\d{2}$/.test(value.date);
  const normalizeDraft = (value) => {
    if (!value || value.schema !== 2 || typeof value.requestId !== "string" || !uuid.test(value.requestId) || !validFields(value) ||
        (value.migratedUploads !== undefined && typeof value.migratedUploads !== "boolean") ||
        (value.scope !== null && !uploadScope(value.scope)) ||
        !Array.isArray(value.attachmentIds) || value.attachmentIds.length > 10 ||
        value.attachmentIds.some((id) => typeof id !== "string" || !uuid.test(id)) ||
        new Set(value.attachmentIds).size !== value.attachmentIds.length ||
        (value.body !== null && typeof value.body !== "string") ||
        (value.accepted !== null && !acceptedStatus(value.accepted, value.requestId))) return null;
    if (value.body !== null) {
      if (value.attachmentIds.length && !value.scope) return null;
      const fields = new URLSearchParams(value.body);
      const scalars = ["clientRequestId", "name", "goal", "creation_date", "model", "effort", "uploadScope"];
      const optional = ["team", "catalogDigest"];
      if (scalars.some((key) => fields.getAll(key).length !== 1) ||
          optional.some((key) => fields.getAll(key).length > 1) ||
          [...fields.keys()].some((key) => ![...scalars, ...optional, "attachmentIds"].includes(key)) ||
          fields.get("clientRequestId") !== value.requestId ||
          fields.get("uploadScope") !== (value.scope?.id || "") ||
          fieldNames.some((key) => (fields.get(key === "date" ? "creation_date" : key) || "") !== value[key]) ||
          JSON.stringify(fields.getAll("attachmentIds")) !== JSON.stringify(value.attachmentIds)) return null;
    } else if (value.accepted || value.attachmentIds.length) return null;
    return value;
  };
  const createRequestDraft = ({storage, fields, legacyStorage, randomUUID = () => globalThis.crypto.randomUUID(), fetch: fetchRequest}) => {
    let saved;
    try {
      if (!storage) throw storageError();
      saved = JSON.parse(storage.getItem(draftKey) || "null");
    } catch (_) { throw storageError(); }
    let draft = normalizeDraft(saved);
    if (saved && !draft && (saved.schema !== undefined || !validFields({...saved,
      model: saved.model || "", effort: saved.effort || "", team: saved.team || "", catalogDigest: saved.catalogDigest || ""}))) {
      throw new Error("The saved session request is unreadable. Keep this tab's storage for recovery; no request was sent.");
    }
    if (!draft) {
      let migratedUploads = false;
      // Only an actual older text/settings draft may inspect the ambiguous
      // shared selection. This is a notice, never proof of upload ownership.
      if (saved && legacyStorage) {
        try {
          const oldStorage = legacyStorage();
          const oldScope = JSON.parse(oldStorage.getItem("workspace-portal.creation-upload-scope") || "null");
          if (uploadScope(oldScope)) {
            const selected = JSON.parse(oldStorage.getItem(`workspace-portal.upload-draft.${oldScope.id}`) || "null");
            migratedUploads = Array.isArray(selected) && selected.length > 0 && selected.length <= 100 &&
              selected.every((entry) => entry && typeof entry.name === "string" && entry.name.length > 0);
          }
        } catch (_) { /* Unreadable legacy storage cannot block this tab's draft. */ }
      }
      const original = saved ? {...fields, ...saved} : fields;
      draft = {schema: 2, requestId: randomUUID(), ...Object.fromEntries(fieldNames.map((key) => [key, original[key] || ""])),
        scope: null, attachmentIds: [], body: null, accepted: null, migratedUploads};
      if (!normalizeDraft(draft)) throw new Error("Unable to allocate a valid session request identity.");
    }
    const persist = () => verifyWrite(storage, draftKey, JSON.stringify(draft));
    try { persist(); } catch (error) { error.requestId = draft.requestId; throw error; }
    let busy = null;
    const receive = async (response, decoded) => {
      const status = decoded ?? await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(status.error || `Unable to confirm session request (${response.status}).`);
      if (!acceptedStatus(status, draft.requestId, draft.accepted?.receiptId)) {
        throw new Error("Unable to confirm the saved session request identity. Retry recovery.");
      }
      draft.accepted = status;
      persist();
      return status;
    };
    const recover = async () => {
      persist(); // Readback must succeed again before any recovery request.
      if (draft.accepted) return draft.accepted;
      const response = await fetchRequest(`/api/session-creations/${draft.requestId}`, {credentials: "same-origin", cache: "no-store"});
      if (response.status === 503) {
        const failure = await response.json().catch(() => ({}));
        if (failure?.requestId !== draft.requestId || failure?.code !== "preparation_persistence_unconfirmed") {
          return receive(response, failure);
        }
      } else if (response.status !== 404) return receive(response);
      return receive(await fetchRequest("/sessions", {
        method: "POST", credentials: "same-origin",
        headers: {Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded"}, body: draft.body,
      }));
    };
    return {
      get draft() { return JSON.parse(JSON.stringify(draft)); },
      update(fields) {
        if (draft.body !== null) throw new Error("Recover the saved request before starting another session.");
        const next = {...draft, ...Object.fromEntries(fieldNames.map((key) => [key, fields[key] ?? draft[key]]))};
        if (!normalizeDraft(next)) throw new Error("The session draft is invalid.");
        draft = next; persist();
      },
      bindScope(scope) {
        if (draft.body !== null || !uploadScope(scope)) throw new Error("Unable to bind files to this session draft.");
        draft.scope = {id: scope.id, url: scope.url}; persist();
      },
      submit(attachmentIds, includeTeam) {
        if (busy) return busy;
        busy = (async () => {
          if (draft.body === null) {
            if (Boolean(draft.model) !== Boolean(draft.effort)) throw new Error("Choose both a model and reasoning effort, or use the team defaults.");
            const body = new URLSearchParams({clientRequestId: draft.requestId, name: draft.name, goal: draft.goal,
              creation_date: draft.date, model: draft.model, effort: draft.effort, uploadScope: draft.scope?.id || ""});
            if (includeTeam) { body.set("team", draft.team); body.set("catalogDigest", draft.catalogDigest); }
            for (const id of attachmentIds) body.append("attachmentIds", id);
            draft = {...draft, attachmentIds: [...attachmentIds], body: body.toString()};
            if (!normalizeDraft(draft)) throw new Error("The session request snapshot is invalid.");
            persist(); // Lock and save the exact bytes before the first POST.
            return receive(await fetchRequest("/sessions", {
              method: "POST", credentials: "same-origin",
              headers: {Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded"}, body: draft.body,
            }));
          }
          return recover();
        })().finally(() => { busy = null; });
        return busy;
      },
      clearAccepted() {
        if (!draft.accepted || !acceptedStatus(draft.accepted, draft.requestId)) throw new Error("The session request has not been confirmed.");
        persist();
        try {
          if (draft.scope) verifyRemove(storage, `workspace-portal.upload-draft.${draft.scope.id}`);
          verifyRemove(storage, draftKey);
        } catch (error) {
          // A failed removal/readback must not erase the only recoverable ID.
          try { persist(); } catch (_) {}
          throw error;
        }
        return preparationURL(draft.requestId);
      },
    };
  };
  const api = {draftKey, preparationURL, sessionURL, uploadScope, acceptedStatus, normalizeDraft, createRequestDraft};
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else globalThis.sessionPreparation = api;
})();
