(async () => {
  const apiPath = (slug, operation) => `/api/sessions/${encodeURIComponent(slug)}/${operation}`;
  const createRequest = (fetchRequest) => async (path, options = {}) => {
    const response = await fetchRequest(path, {
      ...options,
      headers: {"Content-Type": "application/json", ...(options.headers || {})},
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) { const error = new Error(data.error || `Request failed (${response.status})`); error.status = response.status; throw error; }
    return data;
  };
  const createSessionClient = (slug, request, conversation) => ({
    thread: conversation?.thread,
    activity: conversation?.activity,
    pending: conversation?.pending,
    modes: () => request("/api/collaboration-modes"),
    queue: conversation?.queue,
    reconcileQueue: conversation?.reconcileQueue,
    message: conversation?.message,
    acknowledgeMessages: conversation?.acknowledgeMessages,
    queueMessage: conversation?.queueMessage,
    deleteQueued: conversation?.deleteQueued,
    startQueue: conversation?.startQueue,
    settings: conversation?.settings,
    fork: (name, creationDate, model, reasoningEffort) => request(apiPath(slug, "fork"), {
      method: "POST", body: JSON.stringify({name, creationDate, model, reasoningEffort}),
    }),
    releaseCluster: (kind) => request(apiPath(slug, "release-cluster"), {
      method: "POST", body: JSON.stringify({kind}),
    }),
    details: () => request(apiPath(slug, "details")),
    artifactPreview: (path) => request(`${apiPath(slug, "artifact-preview")}?path=${encodeURIComponent(path)}`),
    archive: (mode, targetId) => request(apiPath(slug, "archive"), {
      method: "POST", body: JSON.stringify({mode, targetId}),
    }),
    revive: (allowAbandoned, targetId) => request(apiPath(slug, "revive"), {
      method: "POST", body: JSON.stringify({allowAbandoned, targetId}),
    }),
    deleteSession: (force, targetId) => request(apiPath(slug, "delete"), {
      method: "POST", body: JSON.stringify({force, targetId}),
    }),
    operation: () => request(apiPath(slug, "operation")),
    retryOperation: (receiptId, journalId, force) => {
      const body = {receiptId, journalId};
      if (force !== undefined) body.force = force;
      return request(`${apiPath(slug, "operation")}/retry`, {
        method: "POST", body: JSON.stringify(body),
      });
    },
    dismissOperation: (receiptId) => request(apiPath(slug, "operation"), {
      method: "DELETE", body: JSON.stringify({receiptId}),
    }),
    interrupt: conversation?.interrupt,
    implementPlan: (payload) => request(apiPath(slug, "implement-plan"), {
      method: "POST", body: JSON.stringify(payload),
    }),
    respond: conversation?.respond,
    snooze: conversation?.snooze,
    eventsPath: conversation?.eventsPath,
  });
  const currentCompletedPlan = (payload) => payload.latestTurnId ?
    [...(payload.entries || [])].reverse().find((entry) => (
      entry.turnId === payload.latestTurnId && entry.kind === "plan" &&
      entry.turnStatus === "completed" && (entry.text || "").trim()
    )) : undefined;
  const planIdentity = (turnId, digest) => JSON.stringify([turnId, digest]);
  const pendingPlanImplementation = (attempts, digest) => [...attempts].some((entry) => (
    entry.message === "Implement the plan." && entry.context === `plan:${digest}`
  ));
  const setPlanDecisionVisible = (panel, form, visible, focusComposer = false) => {
    if (!panel || !form) return;
    const document = form.ownerDocument;
    const moveFocus = visible ? form.contains(document.activeElement) : panel.contains(document.activeElement);
    if (visible) {
      form.querySelectorAll(":popover-open").forEach((menu) => menu.hidePopover());
    }
    panel.hidden = !visible;
    form.hidden = visible;
    panel.closest(".chat-panel")?.classList.toggle("plan-decision", visible);
    if (visible && moveFocus) panel.querySelector("#plan-keep-planning")?.focus();
    if (!visible && (focusComposer || moveFocus)) form.querySelector("textarea")?.focus();
  };
  const setControlLabel = (control, label) => {
    const target = control.querySelector(".rail-label") || control;
    target.textContent = label;
    if (target !== control) {
      control.title = label;
      control.setAttribute("aria-label", label);
    }
  };
  const automaticReasoningLabel = () => "Automatic";
  const messageActionLabel = (active) => active ? "Steer now" : "Send";
  const shouldSubmitMessage = (event) => (
    event.key === "Enter" && !event.shiftKey && !event.isComposing
  );
  const shouldFollowTranscript = (element, threshold = 48) => (
    element.scrollHeight - element.clientHeight - element.scrollTop <= threshold
  );
  const transcriptFollowOnScroll = (follow, element, userInitiated) => (
    userInitiated ? shouldFollowTranscript(element) : follow
  );
  const sendAcknowledgementCandidates = (entries, attempts, excludedIDs = new Set(), limit = 100) => {
    const observed = new Map((entries || []).filter((entry) => (
      entry.clientUserMessageId && /^[0-9a-f]{64}$/.test(entry.clientUserMessageDigest || "")
    )).map((entry) => [entry.clientUserMessageId, entry.clientUserMessageDigest]));
    return (attempts || []).filter((attempt) => (
      observed.has(attempt.id) && !excludedIDs.has(attempt.id)
    )).slice(0, limit).map((attempt) => ({
      ...attempt, transcriptDigest: observed.get(attempt.id),
    }));
  };
  const sha256Hex = async (text) => {
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
    return Array.from(new Uint8Array(digest), (value) => value.toString(16).padStart(2, "0")).join("");
  };
  const markTranscriptMessagesObserved = async (pending, entries) => {
    const changed = [];
    await Promise.all((entries || []).map(async (entry) => {
      const receipt = pending.get(entry.clientUserMessageId);
      if (!receipt || receipt.state === "observed" ||
          !/^[0-9a-f]{64}$/.test(entry.clientUserMessageDigest || "")) return;
      // The HTTP handler trims Unicode White_Space, as Go strings.TrimSpace
      // does. JavaScript trim differs for U+0085 and U+FEFF.
      const text = receipt.message.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
      const ids = receipt.attachmentIds || [];
      if (ids.length && (entry.displayText !== text ||
          JSON.stringify((entry.attachments || []).map((file) => file.id)) !== JSON.stringify(ids))) return;
      const digest = await sha256Hex(ids.length ? entry.text : text);
      if (digest !== entry.clientUserMessageDigest) return;
      const current = pending.get(receipt.id);
      if (!current || current.message !== receipt.message || JSON.stringify(current.attachmentIds || []) !== JSON.stringify(ids) || current.state === "observed") return;
      const observed = {...current, state: "observed", transcriptDigest: digest};
      pending.set(receipt.id, observed);
      changed.push(observed);
    }));
    return changed;
  };
  const messageReceiptLabel = (entry) => entry.state === "observed" ? "" :
    entry.state === "accepted" ? "Sent to Codex" : entry.state === "sending" ? "Sending…" :
      "Outcome unknown. Retry to check.";
  const transcriptEntryKey = (entry, index, entries = []) => {
    const turnID = entry?.turnId || "";
    const itemID = entry?.itemId || "";
    if (turnID && itemID) return JSON.stringify([turnID, itemID]);
    let occurrence = 0;
    for (let priorIndex = 0; priorIndex < index; priorIndex += 1) {
      const prior = entries[priorIndex];
      if (!prior?.itemId && (prior?.turnId || "") === turnID &&
          (prior?.kind || "") === (entry?.kind || "")) occurrence += 1;
    }
    return JSON.stringify([turnID, itemID, entry?.kind || "", occurrence]);
  };
  const captureTranscriptDisclosureState = (container) => {
    const states = new Map();
    for (const element of container.querySelectorAll("[data-transcript-entry-key]")) {
      const disclosure = element.querySelector("details");
      if (disclosure) states.set(element.dataset.transcriptEntryKey, disclosure.open);
    }
    return states;
  };
  const wrapMarkdownTables = (container) => {
    for (const table of container.querySelectorAll("table")) {
      if (table.parentElement?.classList.contains("table-scroll")) continue;
      const wrapper = table.ownerDocument.createElement("div");
      wrapper.className = "table-scroll";
      table.before(wrapper);
      wrapper.append(table);
    }
  };
  const sessionTabFromHash = (hash, panelIDs, fallback) => {
    let target = "";
    try {
      target = decodeURIComponent(String(hash || "").replace(/^#/, ""));
    } catch (_error) {
      return fallback;
    }
    return panelIDs.includes(target) ? target : fallback;
  };
  const sessionTabFromLocation = ({hash, search}, panelIDs, fallback) => {
    const queryTab = new URLSearchParams(search).get("tab");
    return sessionTabFromHash(hash, panelIDs, panelIDs.includes(queryTab) ? queryTab : fallback);
  };
  const transcriptEntryVisible = (entry, filter) => {
    if (filter === "all" || entry?.kind === "error") return true;
    const messageKinds = new Set(["userMessage", "agentMessage", "reasoning", "plan"]);
    if (filter === "messages") return messageKinds.has(entry?.kind);
    if (filter === "activity") return !messageKinds.has(entry?.kind);
    return true;
  };
  const transcriptEntriesForFilter = (entries, filter) => (
    (entries || []).filter((entry) => transcriptEntryVisible(entry, filter))
  );
  const transcriptErrorPresentation = (entry = {}) => {
    const heading = String(entry.summary || "Codex error").trim() || "Codex error";
    return {
      heading,
      message: String(entry.text || "").trim(),
      details: String(entry.details || "").trim(),
    };
  };
  const captureTranscriptViewState = (container) => ({
    disclosures: captureTranscriptDisclosureState(container),
    follow: shouldFollowTranscript(container),
    scrollTop: container.scrollTop,
  });
  const formatElapsed = (elapsedMilliseconds) => {
    const seconds = Math.max(0, Math.floor(Number(elapsedMilliseconds || 0) / 1000));
    if (seconds < 60) return `${seconds}s`;
    const minutes = Math.floor(seconds / 60);
    const remainder = String(seconds % 60).padStart(2, "0");
    if (minutes < 60) return `${minutes}m ${remainder}s`;
    const hours = Math.floor(minutes / 60);
    return `${hours}h ${String(minutes % 60).padStart(2, "0")}m`;
  };
  const activityPresentation = (snapshot, elapsedSinceRead = 0) => {
    const elapsed = Math.max(0, elapsedSinceRead);
    const stale = elapsed > 15_000;
    const extension = stale ? 0 : elapsed;
    const state = snapshot.currentState || "unclassified";
    const working = Number(snapshot.workingMs || 0) + (state === "working" ? extension : 0);
    const waiting = Number(snapshot.waitingMs || 0) + Number(snapshot.betweenTurnsMs || 0);
    const openWait = Number(snapshot.openWaitingMs || 0) +
      (["waiting", "idle"].includes(state) && snapshot.stateSinceMs ? extension : 0);
    const messages = Number(snapshot.messages || 0);
    const tools = Number(snapshot.toolCalls || 0);
    return {
      stale, state,
      working: formatElapsed(working), waiting: formatElapsed(waiting),
      unclassified: Number(snapshot.unclassifiedMs || 0),
      openWait: formatElapsed(openWait),
      counts: `${messages} ${messages === 1 ? "message" : "messages"} · ${tools} ${tools === 1 ? "tool call" : "tool calls"}`,
      turnElapsed: snapshot.startedAtMs ? formatElapsed(
        Math.max(0, Number(snapshot.completedAtMs || snapshot.observedAtMs) - snapshot.startedAtMs) +
          (!snapshot.completedAtMs ? extension : 0),
      ) : "",
    };
  };

  const timedProgress = (element, label) => {
    const startedAt = Date.now();
    const update = () => {
      if (!element) return;
      element.hidden = false;
      element.className = "operation-progress";
      element.textContent = `${label} · ${formatElapsed(Date.now() - startedAt)} elapsed`;
    };
    update();
    const timer = setInterval(update, 1000);
    return {
      fail: (message) => {
        clearInterval(timer);
        if (!element) return;
        element.hidden = false;
        element.className = "operation-progress error";
        element.textContent = message;
      },
      stop: () => clearInterval(timer),
    };
  };
  const indexStatusOrder = (statuses) => [...(statuses || [])].sort((left, right) => {
    const leftTime = Date.parse(left?.updatedAt || "") || 0;
    const rightTime = Date.parse(right?.updatedAt || "") || 0;
    if (leftTime !== rightTime) return rightTime - leftTime;
    return String(left?.slug || "").localeCompare(String(right?.slug || ""));
  });
  const activityAge = (updatedAt, now = Date.now()) => {
    const timestamp = Date.parse(updatedAt || "");
    if (!Number.isFinite(timestamp)) return "unknown";
    const seconds = Math.max(0, Math.floor((now - timestamp) / 1000));
    if (seconds < 60) return "just now";
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m ago`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    if (days < 30) return `${days}d ago`;
    return new Date(timestamp).toLocaleDateString();
  };
  const indexStatusFreshForPage = (pageGeneratedAt, statusGeneratedAt) => {
    const pageTime = Date.parse(pageGeneratedAt || "");
    const statusTime = Date.parse(statusGeneratedAt || "");
    return Number.isFinite(pageTime) && Number.isFinite(statusTime) && statusTime >= pageTime;
  };
  const lifecycleKindLabel = (kind) => ({
    archive: "Archive", delete: "Delete", revive: "Revive",
  }[kind] || "Session operation");
  const lifecyclePhaseLabel = (phase) => {
    const label = String(phase || "starting").replaceAll("_", " ");
    return label.charAt(0).toUpperCase() + label.slice(1);
  };
  const lifecyclePresentation = (operation = {}, pendingKind = "", elapsedMilliseconds = 0) => {
    const kind = operation.kind || pendingKind;
    const label = lifecycleKindLabel(kind);
    if (operation.state === "running") {
      return {
        detail: `Running · ${formatElapsed(elapsedMilliseconds)} elapsed`,
        retry: false,
        title: `${label}: ${lifecyclePhaseLabel(operation.phase)}`,
        tone: "running",
      };
    }
    if (operation.state === "failed") {
      return {
        detail: operation.error || "The operation did not finish.",
        retry: true,
        title: `${label} failed${operation.phase ? ` during ${operation.phase.replaceAll("_", " ")}` : ""}`,
        tone: "failed",
      };
    }
    if (operation.state === "paused") {
      return {
        detail: `Paused · ${formatElapsed(elapsedMilliseconds)} since the last recorded update`,
        retry: true,
        title: `${label}: ${lifecyclePhaseLabel(operation.phase)}`,
        tone: "pending",
      };
    }
    if (operation.state === "complete") {
      return {
        detail: operation.updatedAt ? `Finished ${activityAge(operation.updatedAt)}` : "Finished",
        retry: false,
        title: `${label} complete`,
        tone: "complete",
      };
    }
    if (pendingKind) {
      return {
        detail: `Retry ${pendingKind} to continue.`,
        retry: true,
        title: `${lifecycleKindLabel(pendingKind)} needs attention`,
        tone: "pending",
      };
    }
    return {detail: "", retry: false, title: "", tone: "idle"};
  };
  const lifecycleRecoveryAction = (kind, expected = {}, operation = {}) => {
    if (operation.kind !== kind || !operation.receiptId) return "none";
    if (expected.journalId) {
      if (operation.options?.journalId !== expected.journalId) return "none";
      if (expected.receiptId && operation.receiptId === expected.receiptId &&
          operation.state !== "complete") return "unchanged";
    } else {
      if (!expected.targetId || operation.options?.targetId !== expected.targetId) return "none";
      if (kind === "archive" && operation.options?.mode !== expected.mode) return "none";
      if (kind === "revive" &&
          Boolean(operation.options?.allowAbandoned) !== Boolean(expected.allowAbandoned)) return "none";
      if (kind === "delete" &&
          Boolean(operation.options?.force) !== Boolean(expected.force)) return "none";
    }
    if (operation.state === "complete") return "complete";
    if (operation.state === "running") return "monitor";
    if (operation.state === "failed" || operation.state === "paused") return "retry";
    return "none";
  };
  const lifecycleOperationMatches = (kind, targetId, pendingKind, operation = {}) => {
    if (operation.kind !== kind || !operation.receiptId) return false;
    if (targetId && operation.options?.targetId === targetId) return true;
    return pendingKind === kind && Boolean(operation.options?.journalExpected) &&
      Boolean(operation.options?.journalId);
  };
  const safeDiffPath = (value) => String(value || "unknown-file").replace(/[\r\n\t]/g, " ");
  const diffLineKind = (line) => {
    if (/^(diff --git |index |--- |\+\+\+ )/.test(line)) return "header";
    if (line.startsWith("@@")) return "hunk";
    if (line.startsWith("+")) return "addition";
    if (line.startsWith("-")) return "deletion";
    return "context";
  };
  const fileChangeDiffs = (details) => {
    let changes;
    try { changes = JSON.parse(details); } catch (_error) { return []; }
    if (!Array.isArray(changes)) return [];
    return changes.map((change) => {
      if (!change || typeof change !== "object" || Array.isArray(change)) return null;
      const path = safeDiffPath(change.path);
      const kindValue = typeof change.kind === "object" ? change.kind?.type : change.kind;
      const kind = typeof kindValue === "string" ? kindValue.toLowerCase() : "update";
      const diff = typeof change.diff === "string" ? change.diff.replace(/\r\n?/g, "\n") : "";
      const lines = diff ? diff.replace(/\n$/, "").split("\n") : [];
      if (!lines.some((line) => line.startsWith("--- ")) ||
          !lines.some((line) => line.startsWith("+++ "))) {
        const oldPath = kind === "add" || kind === "create" ? "/dev/null" : `a/${path}`;
        const newPath = kind === "delete" || kind === "remove" ? "/dev/null" : `b/${path}`;
        lines.unshift(`--- ${oldPath}`, `+++ ${newPath}`);
      }
      if (!diff) lines.push(" No diff was provided.");
      return {kind, path, lines: lines.map((text) => ({kind: diffLineKind(text), text}))};
    }).filter(Boolean);
  };
  const encodeQuestionAnswer = (question, draft = {}) => {
    if (draft.kind === "option" && draft.choice) {
      const values = [draft.choice];
      if ((draft.note || "").trim()) values.push(`user_note: ${draft.note.trim()}`);
      return values;
    }
    if (draft.kind === "other") {
      if ((draft.note || "").trim()) return [`user_note: ${draft.note.trim()}`];
      return [];
    }
    if (draft.kind === "freeform" && (draft.note || "").trim()) {
      return [`user_note: ${draft.note.trim()}`];
    }
    return [];
  };
  const renderCollaborationModes = (container, modes, selectMode = () => {}) => {
    if (!container) return [];
    const ownerDocument = container.ownerDocument || document;
    const buttons = (modes || []).filter((mode) => (
      mode && typeof mode.mode === "string" && mode.mode
    )).map((mode) => {
      const button = ownerDocument.createElement("button");
      button.type = "button";
      button.className = mode.mode === "plan" ? "quiet plan-mode" : "quiet";
      button.dataset.codexMode = mode.mode;
      button.textContent = mode.name || mode.mode;
      if (mode.description) button.title = mode.description;
      button.addEventListener("click", () => selectMode(mode.mode));
      return button;
    });
    container.replaceChildren(...buttons);
    return buttons;
  };
  const attemptStoragePrefix = (kind, slug, threadId) => (
    `workspace-portal.${kind}-attempt.${encodeURIComponent(slug)}.${encodeURIComponent(threadId)}.`
  );
  const queueAttemptStoragePrefix = (slug, threadId) => attemptStoragePrefix("queue", slug, threadId);
  const sendAttemptStoragePrefix = (slug, threadId) => attemptStoragePrefix("send", slug, threadId);
  const queueAttemptStorageKey = (slug, threadId, id) => (
    `${queueAttemptStoragePrefix(slug, threadId)}${id}`
  );
  let durableAttemptStoreFactory = null;
  const attemptAttachmentFields = (value) => {
    if (value.attachmentIds === undefined) return {};
    if (!Array.isArray(value.attachmentIds) || value.attachmentIds.length > 100 || value.attachmentIds.some((id) => typeof id !== "string")) throw new Error("Stored attachment IDs are invalid");
    return value.attachmentIds.length ? {attachmentIds: [...value.attachmentIds]} : {};
  };
  const sameAttemptAttachments = (left, right = []) => JSON.stringify(left || []) === JSON.stringify(right);
  const configureDurableAttemptStore = (factory) => {
    if (typeof factory !== "function") {
      throw new TypeError("durable attempt store factory is required");
    }
    durableAttemptStoreFactory = factory;
  };
  const queueAttemptStore = (storage, slug, threadId) => {
    if (!durableAttemptStoreFactory || !threadId) return null;
    return durableAttemptStoreFactory({
      storage,
      prefix: queueAttemptStoragePrefix(slug, threadId),
      decode: (id, value) => (
        value && typeof value.message === "string" && (value.message || value.attachmentIds?.length) ?
          {id, message: value.message, ...attemptAttachmentFields(value)} : null
      ),
      encode: (attempt) => ({message: attempt.message, ...attemptAttachmentFields(attempt)}),
    });
  };
  const loadQueueAttempts = (storage, slug, threadId) => {
    return queueAttemptStore(storage, slug, threadId)?.load() ?? null;
  };
  const requireQueueAttempts = (storage, slug, threadId) => {
    const store = queueAttemptStore(storage, slug, threadId);
    const attempts = store?.load();
    if (!store?.available() || attempts === null || attempts === undefined) {
      throw new Error("Browser storage is unavailable; messages cannot be submitted safely.");
    }
    return attempts;
  };
  const storeQueueAttempt = (storage, slug, threadId, attempt) => {
    return queueAttemptStore(storage, slug, threadId)?.store(attempt) === true;
  };
  const deleteQueueAttempt = (storage, slug, threadId, id) => {
    return queueAttemptStore(storage, slug, threadId)?.remove(id) === true;
  };
  const sendAttemptStorageKey = (slug, threadId, id) => (
    `${sendAttemptStoragePrefix(slug, threadId)}${id}`
  );
  const sendAttemptStore = (storage, slug, threadId) => {
    if (!durableAttemptStoreFactory || !threadId) return null;
    return durableAttemptStoreFactory({
      storage,
      prefix: sendAttemptStoragePrefix(slug, threadId),
      decode: (id, value) => {
        if (!value || typeof value.message !== "string" || (!value.message && !value.attachmentIds?.length) ||
            (value.context !== undefined && typeof value.context !== "string")) return null;
        return {
          id, message: value.message, steered: Boolean(value.steered), ...attemptAttachmentFields(value),
          context: value.context || "",
          ...(["accepted", "observed"].includes(value.state) ? {state: value.state} : {}),
          ...(/^[0-9a-f]{64}$/.test(value.transcriptDigest || "") ? {transcriptDigest: value.transcriptDigest} : {}),
        };
      },
      encode: (attempt) => ({
        message: attempt.message, steered: Boolean(attempt.steered), ...attemptAttachmentFields(attempt),
        context: typeof attempt.context === "string" ? attempt.context : "",
        ...(["accepted", "observed"].includes(attempt.state) ? {state: attempt.state} : {}),
        ...(/^[0-9a-f]{64}$/.test(attempt.transcriptDigest || "") ? {transcriptDigest: attempt.transcriptDigest} : {}),
      }),
    });
  };
  const loadSendAttempts = (storage, slug, threadId) => {
    return sendAttemptStore(storage, slug, threadId)?.load() ?? null;
  };
  const storeSendAttempt = (storage, slug, threadId, attempt) => {
    return sendAttemptStore(storage, slug, threadId)?.store(attempt) === true;
  };
  const deleteSendAttempt = (storage, slug, threadId, id) => {
    return sendAttemptStore(storage, slug, threadId)?.remove(id) === true;
  };
  const matchingSendAttempt = (attempts, message, context = "", attachments = []) => (
    attempts.find((candidate) => (
      candidate.message === message && (candidate.context || "") === context && sameAttemptAttachments(candidate.attachmentIds, attachments)
    ))
  );
  const autoResolutionLabel = (now, visibleAt, dueAt, snoozed) => {
    if (snoozed) return "Auto-resolution paused while you answer.";
    if (!dueAt || now < visibleAt) return "";
    const seconds = Math.max(0, Math.ceil((dueAt - now) / 1000));
    return `Auto-resolves unanswered in ${seconds}s.`;
  };
  const beforeRequestInputAction = async (snooze) => {
    await snooze();
  };
  const requestInputDraftStorageKey = (slug, threadId, requestId) => (
    `workspace-portal.request-input.${encodeURIComponent(slug)}.${encodeURIComponent(threadId)}.${encodeURIComponent(requestId)}`
  );
  const clearThreadStorage = (storage, slug, threadId) => {
    if (!storage || !threadId || typeof storage.length !== "number" ||
        typeof storage.key !== "function") return false;
    const queueStore = queueAttemptStore(storage, slug, threadId);
    const sendStore = sendAttemptStore(storage, slug, threadId);
    if (!queueStore?.clear() || !sendStore?.clear()) return false;
    const prefix =
      `workspace-portal.request-input.${encodeURIComponent(slug)}.${encodeURIComponent(threadId)}.`;
    try {
      const keys = [];
      for (let index = 0; index < storage.length; index += 1) {
        const key = storage.key(index);
        if (typeof key === "string" && key.startsWith(prefix)) keys.push(key);
      }
      keys.forEach((key) => storage.removeItem(key));
      return keys.every((key) => storage.getItem(key) === null);
    } catch (_error) {
      return false;
    }
  };
  const cleanupCompletedDeleteStorage = (operations, storages) => {
    for (const operation of operations || []) {
      if (operation.state !== "complete" || operation.kind !== "delete" || !operation.slug) continue;
      const threadId = operation.options?.deletedThreadId || "";
      if (!threadId) continue;
      for (const storage of storages || []) clearThreadStorage(storage, operation.slug, threadId);
    }
  };
  const loadRequestInputDraft = (storage, slug, threadId, requestId, questions) => {
    if (!storage || !threadId || !requestId || !Array.isArray(questions)) return null;
    try {
      const encoded = storage.getItem(requestInputDraftStorageKey(slug, threadId, requestId));
      if (encoded === null) return null;
      const value = JSON.parse(encoded);
      if (!value || !Number.isInteger(value.page) || value.page < 0 ||
          !Array.isArray(value.drafts) || value.drafts.length !== questions.length) return null;
      const drafts = value.drafts.map((draft, index) => {
        if (questions[index]?.isSecret) return {};
        if (!draft || !["option", "other", "freeform"].includes(draft.kind)) return {};
        if (typeof draft.choice !== "string" || typeof draft.note !== "string") return {};
        return {kind: draft.kind, choice: draft.choice, note: draft.note};
      });
      return {page: Math.min(value.page, Math.max(0, questions.length - 1)), drafts};
    } catch (_error) {
      return null;
    }
  };
  const storeRequestInputDraft = (storage, slug, threadId, requestId, questions, state) => {
    if (!storage || !threadId || !requestId || !Array.isArray(questions) || !state) return false;
    try {
      const drafts = questions.map((question, index) => {
        if (question.isSecret) return {};
        const draft = state.drafts[index] || {};
        return {
          kind: typeof draft.kind === "string" ? draft.kind : "",
          choice: typeof draft.choice === "string" ? draft.choice : "",
          note: typeof draft.note === "string" ? draft.note : "",
        };
      });
      storage.setItem(
        requestInputDraftStorageKey(slug, threadId, requestId),
        JSON.stringify({page: state.page, drafts}),
      );
      return true;
    } catch (_error) {
      return false;
    }
  };
  const deleteRequestInputDraft = (storage, slug, threadId, requestId) => {
    if (!storage || !threadId || !requestId) return false;
    try {
      storage.removeItem(requestInputDraftStorageKey(slug, threadId, requestId));
      return true;
    } catch (_error) {
      return false;
    }
  };
  const createCodexLimitsReader = (request, render) => {
    let snapshot = null;
    let pending = null;
    return {
      refresh() {
        if (pending) return pending;
        pending = (async () => {
          try {
            snapshot = await request("/api/codex-limits");
            render(snapshot, false);
          } catch (_error) {
            render(snapshot, true);
          } finally {
            pending = null;
          }
        })();
        return pending;
      },
    };
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      automaticReasoningLabel, createRequest, createSessionClient, createCodexLimitsReader,
      currentCompletedPlan, planIdentity, pendingPlanImplementation, setPlanDecisionVisible,
      autoResolutionLabel, beforeRequestInputAction, clearThreadStorage,
      configureDurableAttemptStore,
      deleteQueueAttempt, deleteRequestInputDraft, deleteSendAttempt,
      loadQueueAttempts, loadRequestInputDraft, loadSendAttempts, messageActionLabel,
      markTranscriptMessagesObserved, matchingSendAttempt, messageReceiptLabel,
      queueAttemptStorageKey, sendAttemptStorageKey, queueAttemptStoragePrefix,
      requestInputDraftStorageKey, requireQueueAttempts, shouldFollowTranscript, transcriptFollowOnScroll,
      renderCollaborationModes,
      sendAcknowledgementCandidates, shouldSubmitMessage,
      storeQueueAttempt, storeRequestInputDraft, storeSendAttempt,
      captureTranscriptDisclosureState, captureTranscriptViewState, cleanupCompletedDeleteStorage,
      encodeQuestionAnswer,
      activityAge, activityPresentation, fileChangeDiffs, formatElapsed, indexStatusFreshForPage,
      indexStatusOrder, lifecycleOperationMatches, lifecyclePresentation, lifecycleRecoveryAction,
      sessionTabFromHash, sessionTabFromLocation,
      transcriptEntriesForFilter, transcriptEntryKey, transcriptEntryVisible,
      transcriptErrorPresentation, wrapMarkdownTables,
    };
    return;
  }

  const body = document.body;
  document.querySelectorAll(".document").forEach(wrapMarkdownTables);
  const slug = body.dataset.session;
  const lifecycleTargetId = body.dataset.lifecycleTargetId || "";
  const interactive = body.dataset.interactive === "true";
  const request = createRequest(fetch.bind(globalThis));
  const conversationAssets = await import("/codex/assets/conversation.js?v=6");
  let composerUploads = null;
  let composerUploadReady = true;
  configureDurableAttemptStore(conversationAssets.createDurableAttemptStore);

  const limitsPanel = document.getElementById("codex-limits-panel");
  if (limitsPanel) {
    const toggle = document.querySelector(".codex-limits-toggle");
    const content = limitsPanel.querySelector("[data-limits-content]");
    const compactLabel = toggle.querySelector("[data-limits-compact-label]");
    const compactValue = toggle.querySelector("[data-limits-compact-value]");
    const formatLimitDate = (date) => date.toLocaleString(undefined, {
      month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
    });
    const renderLimits = (snapshot, failed) => {
      content.replaceChildren();
      const summaries = [];
      const windows = snapshot?.windows || [];
      for (const window of windows) {
        const remaining = Math.max(0, Math.min(100, 100 - window.usedPercent));
        const label = window.windowDurationMins === 10080 ? "Weekly" : "5h";
        summaries.push(`${label}: ${remaining}% left`);
        const item = document.createElement("div");
        item.className = "limits-window";
        item.dataset.limitDuration = String(window.windowDurationMins);
        const heading = document.createElement("div");
        heading.className = "limits-window-heading";
        const name = document.createElement("strong");
        name.textContent = label;
        const amount = document.createElement("span");
        amount.textContent = `${remaining}% left`;
        heading.append(name, amount);
        const meter = document.createElement("meter");
        meter.min = 0;
        meter.max = 100;
        meter.value = remaining;
        meter.setAttribute("aria-label", `${label} remaining`);
        item.append(heading, meter);
        if (window.resetsAt != null) {
          const reset = new Date(window.resetsAt * 1000);
          const resetLine = document.createElement("p");
          resetLine.className = "limits-reset";
          const time = document.createElement("time");
          time.dateTime = reset.toISOString();
          time.textContent = formatLimitDate(reset);
          time.title = reset.toLocaleString();
          resetLine.append("Resets ", time);
          item.append(resetLine);
        }
        content.append(item);
      }
      if (!windows.length) {
        const status = document.createElement("p");
        status.className = "limits-status";
        status.textContent = snapshot ? "No limits reported." : "Limits unavailable.";
        content.append(status);
      }
      if (failed && snapshot) {
        const stale = document.createElement("p");
        stale.className = "limits-status stale";
        stale.textContent = `Update failed. Last updated ${formatLimitDate(new Date(snapshot.updatedAt))}.`;
        content.append(stale);
      }
      const compactWindow = windows.find((window) => window.windowDurationMins === 10080) || windows[0];
      compactLabel.textContent = compactWindow ? (compactWindow.windowDurationMins === 10080 ? "Weekly" : "5h") : "Limits";
      compactValue.textContent = compactWindow ? `${Math.max(0, Math.min(100, 100 - compactWindow.usedPercent))}%` : "?";
      toggle.dataset.stale = String(failed);
      const summary = summaries.length ? summaries.join(". ") : content.textContent;
      toggle.title = `Codex limits. ${summary}${failed && snapshot ? ". Update failed." : ""}`;
      toggle.setAttribute("aria-label", toggle.title);
    };
    const narrow = globalThis.matchMedia("(max-width: 760px)");
    const updateLimitsPopover = () => {
      if (narrow.matches) limitsPanel.setAttribute("popover", "auto");
      else limitsPanel.removeAttribute("popover");
      toggle.setAttribute("aria-expanded", "false");
    };
    limitsPanel.addEventListener("toggle", (event) => {
      toggle.setAttribute("aria-expanded", String(event.newState === "open"));
    });
    narrow.addEventListener("change", updateLimitsPopover);
    updateLimitsPopover();
    const limits = createCodexLimitsReader(request, renderLimits);
    const refreshVisibleLimits = () => {
      if (!document.hidden) void limits.refresh();
    };
    document.addEventListener("visibilitychange", refreshVisibleLimits);
    globalThis.addEventListener("focus", refreshVisibleLimits);
    globalThis.setInterval(refreshVisibleLimits, 60_000);
    refreshVisibleLimits();
  }

  let indexNavigationPending = false;
  let indexRefreshTimer = null;
  const renderIndexOperations = (operations) => {
    const panel = document.getElementById("operations");
    const list = document.getElementById("operation-list");
    const count = document.getElementById("operation-count");
    if (!panel || !list || !count) return false;
    const records = [...(operations || [])].sort((left, right) => (
      (Date.parse(right.updatedAt || right.startedAt || "") || 0) -
      (Date.parse(left.updatedAt || left.startedAt || "") || 0)
    ));
    list.replaceChildren();
    count.textContent = String(records.length);
    panel.hidden = records.length === 0;
    let localStorage = null;
    let sessionStorage = null;
    try { localStorage = globalThis.localStorage; } catch (_error) {}
    try { sessionStorage = globalThis.sessionStorage; } catch (_error) {}
    cleanupCompletedDeleteStorage(records, [localStorage, sessionStorage]);
    for (const operation of records) {
      const item = document.createElement("article");
      item.className = `operation-item ${operation.state || "paused"}`;

      const summary = document.createElement("div");
      summary.className = "operation-item-summary";
      const identity = document.createElement("div");
      const name = operation.state === "complete" && operation.kind === "delete" ?
        document.createElement("strong") : document.createElement("a");
      name.textContent = operation.slug || "Unknown session";
      if (name instanceof HTMLAnchorElement) name.href = `/${encodeURIComponent(operation.slug)}/`;
      const phase = document.createElement("span");
      phase.className = "muted";
      phase.textContent = `${lifecycleKindLabel(operation.kind)} · ${lifecyclePhaseLabel(operation.phase)}`;
      identity.append(name, phase);

      const elapsedStart = Date.parse(operation.startedAt || operation.updatedAt || "");
      const presentation = lifecyclePresentation(
        operation, "", Number.isFinite(elapsedStart) ? Date.now() - elapsedStart : 0,
      );
      const state = document.createElement("span");
      state.className = `operation-state ${presentation.tone}`;
      state.textContent = operation.state === "running" ? presentation.detail : presentation.title;
      summary.append(identity, state);
      item.append(summary);

      if (operation.state === "failed" || operation.state === "paused") {
        const detail = document.createElement("p");
        detail.className = operation.state === "failed" ? "operation-error" : "muted";
        detail.textContent = presentation.detail;
        item.append(detail);
      }

      const actions = document.createElement("div");
      actions.className = "operation-item-actions";
      if (operation.state === "failed" || operation.state === "paused") {
        const retry = document.createElement("button");
        retry.type = "button";
        retry.textContent = `Retry ${operation.kind}`;
        retry.addEventListener("click", async () => {
          retry.disabled = true;
          retry.textContent = "Retrying…";
          try {
            await request(`${apiPath(operation.slug, "operation")}/retry`, {
              method: "POST", body: JSON.stringify({
                receiptId: operation.receiptId || "",
                journalId: operation.options?.journalId || "",
              }),
            });
            if (indexRefreshTimer !== null) clearTimeout(indexRefreshTimer);
            indexRefreshTimer = setTimeout(refreshIndexStatus, 0);
          } catch (error) {
            retry.disabled = false;
            retry.textContent = `Retry ${operation.kind}`;
            const warning = document.getElementById("index-status-warning");
            if (warning) {
              warning.textContent = `Unable to retry ${operation.kind}: ${error.message}`;
              warning.hidden = false;
            }
          }
        });
        actions.append(retry);
        if (operation.kind === "delete" && !operation.options?.force) {
          const forceRetry = document.createElement("button");
          forceRetry.type = "button";
          forceRetry.className = "danger";
          forceRetry.textContent = "Force delete";
          forceRetry.addEventListener("click", async () => {
            if (!globalThis.confirm(
              "Force deletion may discard dirty worktrees or interrupt an active Codex turn. Continue?",
            )) return;
            forceRetry.disabled = true;
            forceRetry.textContent = "Forcing…";
            try {
              await request(`${apiPath(operation.slug, "operation")}/retry`, {
                method: "POST", body: JSON.stringify({
                  receiptId: operation.receiptId || "",
                  journalId: operation.options?.journalId || "",
                  force: true,
                }),
              });
              if (indexRefreshTimer !== null) clearTimeout(indexRefreshTimer);
              indexRefreshTimer = setTimeout(refreshIndexStatus, 0);
            } catch (error) {
              forceRetry.disabled = false;
              forceRetry.textContent = "Force delete";
              const warning = document.getElementById("index-status-warning");
              if (warning) {
                warning.textContent = `Unable to force deletion: ${error.message}`;
                warning.hidden = false;
              }
            }
          });
          actions.append(forceRetry);
        }
      }
      if (operation.state === "failed" || operation.state === "complete") {
        const dismiss = document.createElement("button");
        dismiss.type = "button";
        dismiss.className = "quiet";
        dismiss.textContent = "Dismiss";
        dismiss.addEventListener("click", async () => {
          dismiss.disabled = true;
          try {
            await request(apiPath(operation.slug, "operation"), {
              method: "DELETE",
              body: JSON.stringify({receiptId: operation.receiptId || ""}),
            });
            item.remove();
            const remaining = list.childElementCount;
            count.textContent = String(remaining);
            panel.hidden = remaining === 0;
          } catch (error) {
            dismiss.disabled = false;
            const warning = document.getElementById("index-status-warning");
            if (warning) {
              warning.textContent = `Unable to dismiss operation: ${error.message}`;
              warning.hidden = false;
            }
          }
        });
        actions.append(dismiss);
      }
      if (actions.childElementCount) item.append(actions);
      list.append(item);
    }
    return records.some((operation) => operation.state === "running");
  };
  const refreshIndexStatus = async () => {
    if (!body.hasAttribute("data-index")) return;
    if (indexNavigationPending) return;
    const warning = document.getElementById("index-status-warning");
    let nextRefresh = 15_000;
    try {
      const payload = await request("/api/index-status");
      if (indexNavigationPending) return;
      if (renderIndexOperations(payload.operations)) nextRefresh = 1000;
      const statuses = indexStatusOrder(payload.sessions);
      if (!indexStatusFreshForPage(body.dataset.indexGeneratedAt, payload.generatedAt)) {
        nextRefresh = 1000;
        return;
      }
      const sidebar = document.querySelector(".index-sidebar");
      const previousSidebarTop = sidebar?.scrollTop || 0;
      const existing = new Map(Array.from(document.querySelectorAll("[data-session-slug]"))
        .map((card) => [card.dataset.sessionSlug, card]));
      const present = new Set(statuses.map((item) => item.slug));
      if (payload.authoritative) {
        for (const [slug, card] of existing) if (!present.has(slug)) card.remove();
      }
      for (const item of statuses) {
        let card = existing.get(item.slug);
        if (!card) {
          card = document.getElementById("session-card-template").content.firstElementChild.cloneNode(true);
          card.dataset.sessionSlug = item.slug;
          card.href = `/${encodeURIComponent(item.slug)}/`;
          card.querySelector("strong").textContent = item.slug;
        }
        card.classList.toggle("archived", item.archived);
        card.querySelector(".status-dot").classList.toggle("active", !item.archived);
        document.querySelector(`[data-session-list="${item.archived ? "archived" : "active"}"]`).append(card);
      }
      for (const [kind, counter] of [["active", "active-count"], ["archived", "archive-count"]]) {
        const list = document.querySelector(`[data-session-list="${kind}"]`);
        const count = list.querySelectorAll("[data-session-slug]").length;
        document.getElementById(counter).textContent = String(count);
        list.querySelectorAll(".empty").forEach((element) => element.remove());
        if (!count) {
          const empty = document.createElement("p");
          empty.className = "empty";
          empty.textContent = kind === "active" ? "No work sessions." : "No archived sessions.";
          list.append(empty);
        }
      }
      const cards = Array.from(document.querySelectorAll("[data-session-slug]"));
      const bySlug = new Map(statuses.map((item) => [item.slug, item]));
      for (const card of cards) {
        const item = bySlug.get(card.dataset.sessionSlug);
        if (!item) continue;
        const updated = card.querySelector("[data-session-updated]");
        if (updated) {
          const prefix = card.classList.contains("archived") ? "Finalized" : "Updated";
          updated.textContent = `${prefix} ${activityAge(item.updatedAt)}`;
          updated.dataset.updatedAt = item.updatedAt;
          updated.title = new Date(item.updatedAt).toLocaleString();
        }
        const repositories = card.querySelector("[data-session-repositories]");
        if (repositories) repositories.textContent = `${item.repositoryCount} ${item.repositoryCount === 1 ? "repository" : "repositories"}`;
        const clusters = card.querySelector("[data-session-clusters]");
        if (clusters) {
          clusters.textContent = `${item.runningClusters} running ${item.runningClusters === 1 ? "cluster" : "clusters"}`;
          clusters.hidden = item.runningClusters === 0;
        }
        const lifecycle = card.querySelector("[data-session-lifecycle]");
        if (lifecycle) {
          lifecycle.textContent = item.pendingLifecycle ? `${item.pendingLifecycle} pending` : "";
          lifecycle.hidden = !item.pendingLifecycle;
        }
      }
      for (const grid of document.querySelectorAll(".session-grid")) {
        const gridCards = Array.from(grid.querySelectorAll(":scope > [data-session-slug]"));
        const ordered = indexStatusOrder(gridCards.map((card) => bySlug.get(card.dataset.sessionSlug)).filter(Boolean))
          .map((item) => gridCards.find((card) => card.dataset.sessionSlug === item.slug))
          .filter(Boolean);
        ordered.forEach((card) => grid.append(card));
      }
      if (warning) {
        warning.textContent = payload.warning || "";
        warning.hidden = !warning.textContent;
      }
      if (sidebar) sidebar.scrollTop = previousSidebarTop;
    } catch (error) {
      if (warning) {
        warning.textContent = `Live session status is temporarily unavailable: ${error.message}`;
        warning.hidden = false;
      }
    } finally {
      if (!indexNavigationPending) indexRefreshTimer = setTimeout(refreshIndexStatus, nextRefresh);
    }
  };
  if (body.hasAttribute("data-index")) {
    const form = document.getElementById("new-session-form");
    let creationUploads = null;
    if (form) {
      const uploadRoot = document.getElementById("creation-uploads");
      const storage = globalThis.localStorage;
      const scopeKey = "workspace-portal.creation-upload-scope";
      const initCreationUploads = async () => {
        try {
          let scope = JSON.parse(storage.getItem(scopeKey) || "null");
          if (scope) {
            try { await request(scope.url); } catch (error) { if (error.status === 404) scope = null; else throw error; }
          }
          if (!scope) {
            scope = await request("/api/upload-drafts", {method: "POST", body: "{}"});
            storage.setItem(scopeKey, JSON.stringify(scope));
          }
          creationUploads = conversationAssets.mountUploads(uploadRoot, {
            basePath: scope.url, dropTarget: form, storage,
            controlsRoot: document.getElementById("creation-upload-controls"),
            storageKey: `workspace-portal.upload-draft.${scope.id}`,
            onChange: ({ready, count}) => { form.elements.goal.required = !count; form.querySelector('button[type="submit"]').disabled = !ready; },
          });
          form.elements.uploadScope.value = scope.id;
          await creationUploads.initialized;
        } catch (error) { uploadRoot.textContent = error.message; uploadRoot.hidden = false; }
      };
      void initCreationUploads();
    }
    form?.addEventListener("submit", (event) => {
      if (creationUploads && !creationUploads.ready()) { event.preventDefault(); return; }
      form.querySelectorAll('input[name="attachmentIds"]').forEach((input) => input.remove());
      for (const id of creationUploads?.ids() || []) {
        const input = document.createElement("input"); input.type = "hidden"; input.name = "attachmentIds"; input.value = id; form.append(input);
      }
      creationUploads?.lock(true);
      indexNavigationPending = true;
      if (indexRefreshTimer !== null) clearTimeout(indexRefreshTimer);
      const button = form.querySelector('button[type="submit"]');
      if (button) button.disabled = true;
      const progress = document.getElementById("new-session-progress");
      timedProgress(progress, "Creating session");
    });
    void refreshIndexStatus();
  }

  let models = [];
  let collaborationModes = [];
  let currentModel = "";
  let currentEffort = "";
  let currentMode = "";
  let currentThreadId = body.dataset.threadId || "";
  let threadActive = false;
  const modelSelects = Array.from(document.querySelectorAll("[data-model-select]"));
  const effortSelects = Array.from(document.querySelectorAll("[data-effort-select]"));

  const populateEfforts = (modelSelect, effortSelect, selected = "") => {
    if (!effortSelect) return;
    const model = models.find((candidate) => candidate.model === modelSelect.value);
    const existingSettings = modelSelect.dataset.existingSettings === "true";
    effortSelect.replaceChildren();
    if (!existingSettings) {
      const fallback = document.createElement("option");
      fallback.value = "";
      fallback.textContent = automaticReasoningLabel(model);
      effortSelect.append(fallback);
    }
    for (const option of model?.supportedReasoningEfforts || []) {
      const element = document.createElement("option");
      element.value = option.reasoningEffort;
      element.textContent = option.reasoningEffort;
      if (option.description) element.title = option.description;
      effortSelect.append(element);
    }
    const desired = selected || (existingSettings ? model?.defaultReasoningEffort : "") || "";
    if (Array.from(effortSelect.options).some((option) => option.value === desired)) {
      effortSelect.value = desired;
    }
    effortSelect.disabled = !model || threadActive;
  };

  const applyCurrentSettings = () => {
    modelSelects.forEach((modelSelect, index) => {
      const effortSelect = effortSelects[index];
      if (currentModel && Array.from(modelSelect.options).some((option) => option.value === currentModel)) {
        modelSelect.value = currentModel;
      }
      populateEfforts(modelSelect, effortSelect, currentEffort);
      if (modelSelect.dataset.existingSettings === "true") {
        modelSelect.disabled = threadActive || !models.some((model) => model.model === modelSelect.value);
      }
    });
    document.getElementById("fork-open")?.toggleAttribute("disabled", threadActive);
    const modeToggle = document.getElementById("codex-mode");
    const availableModes = new Set(collaborationModes.map((mode) => mode.mode));
    const hasModeToggle = availableModes.size > 0 && currentMode;
    if (modeToggle) modeToggle.hidden = !hasModeToggle;
    document.querySelectorAll("[data-codex-mode]").forEach((button) => {
      const active = button.dataset.codexMode === currentMode;
      button.classList.toggle("active", active);
      button.setAttribute("aria-pressed", active ? "true" : "false");
    });
  };

  const loadModels = async () => {
    try {
      models = await request("/api/models");
      for (const modelSelect of modelSelects) {
        const allowDefault = !modelSelect.required;
        modelSelect.replaceChildren();
        if (allowDefault) {
          const fallback = document.createElement("option");
          fallback.value = "";
          fallback.textContent = "GPT-6 Astra (default)";
          modelSelect.append(fallback);
        }
        for (const model of models) {
          const option = document.createElement("option");
          option.value = model.model;
          option.textContent = model.displayName;
          option.title = model.description || "";
          modelSelect.append(option);
        }
        if (!currentModel && modelSelect.required) {
          const defaultModel = models.find((model) => model.isDefault);
          if (defaultModel) modelSelect.value = defaultModel.model;
        }
      }
      applyCurrentSettings();
    } catch (_error) {
      for (const modelSelect of modelSelects) {
        modelSelect.replaceChildren();
        const option = document.createElement("option");
        option.value = "";
        option.textContent = "Model catalog unavailable";
        modelSelect.append(option);
        modelSelect.disabled = true;
      }
      for (const effortSelect of effortSelects) effortSelect.disabled = true;
    }
  };

  modelSelects.forEach((modelSelect, index) => {
    modelSelect.addEventListener("change", () => populateEfforts(modelSelect, effortSelects[index]));
  });
  if (modelSelects.length) loadModels();

  document.querySelectorAll("[data-copy]").forEach((button) => {
    button.addEventListener("click", async () => {
      const input = button.parentElement.querySelector("input");
      await navigator.clipboard.writeText(input.value);
      const old = button.textContent;
      button.textContent = "Copied";
      setTimeout(() => { button.textContent = old; }, 1000);
    });
  });

  const sessionTabBar = document.querySelector('[aria-label="Session sections"]');
  const sessionTabs = Array.from(sessionTabBar?.children || []).filter((element) => (
    element.matches("a[data-session-tab]")
  ));
  const sessionPanels = Array.from(document.querySelectorAll(".session-tabs > .tab-panel"));
  const defaultSessionTab = sessionTabs.find((tab) => tab.classList.contains("active"))?.dataset.sessionTab ||
    sessionTabs[0]?.dataset.sessionTab || "";
  const activateSessionTab = (target) => {
    if (!sessionTabs.some((tab) => tab.dataset.sessionTab === target)) return;
    sessionTabs.forEach((tab) => {
      const selected = tab.dataset.sessionTab === target;
      tab.classList.toggle("active", selected);
      tab.setAttribute("aria-selected", selected ? "true" : "false");
      tab.tabIndex = selected ? 0 : -1;
    });
    sessionPanels.forEach((panel) => panel.classList.toggle("active", panel.id === target));
    document.dispatchEvent(new CustomEvent("session-section-change", {detail: target}));
  };
  sessionTabs.forEach((tab, index) => {
    tab.addEventListener("click", () => activateSessionTab(tab.dataset.sessionTab));
    tab.addEventListener("keydown", (event) => {
      const next = event.key === "ArrowDown" ? (index + 1) % sessionTabs.length :
        event.key === "ArrowUp" ? (index + sessionTabs.length - 1) % sessionTabs.length :
          event.key === "Home" ? 0 : event.key === "End" ? sessionTabs.length - 1 : -1;
      if (next < 0) return;
      event.preventDefault();
      sessionTabs[next].focus();
      sessionTabs[next].click();
    });
  });
  const activateSessionHash = () => activateSessionTab(sessionTabFromLocation(
    location, sessionTabs.map((tab) => tab.dataset.sessionTab), defaultSessionTab,
  ));
  activateSessionHash();
  addEventListener("hashchange", activateSessionHash);
  addEventListener("popstate", activateSessionHash);

  if (!slug) return;
  const conversation = slug ? conversationAssets.createConversationClient({
    id: slug,
    basePath: "/codex",
    conversationPath: `/api/sessions/${encodeURIComponent(slug)}`,
  }) : null;
  const client = createSessionClient(slug, request, conversation);
  const lifecycleStatus = document.getElementById("lifecycle-operation-status");
  const lifecycleTitle = document.getElementById("lifecycle-operation-title");
  const lifecycleDetail = document.getElementById("lifecycle-operation-detail");
  const lifecycleRetry = document.getElementById("lifecycle-operation-retry");
  const pendingLifecycle = body.dataset.pendingLifecycle || "";
  let lifecycleKind = pendingLifecycle;
  let lifecycleObservedAt = 0;
  let lifecycleClock = null;
  let lifecyclePollTimer = null;
  let lastLifecycleOperation = {state: pendingLifecycle ? "idle" : "idle"};
  const lifecycleOperationBelongsToPage = (kind, operation) => lifecycleOperationMatches(
    kind, lifecycleTargetId, pendingLifecycle, operation,
  );
  const operationForRetry = async (kind) => {
    let operation = lastLifecycleOperation;
    if (!lifecycleOperationBelongsToPage(kind, operation)) operation = await client.operation();
    if (!lifecycleOperationBelongsToPage(kind, operation)) {
      throw new Error("This operation belongs to an older session. Reload the page before continuing.");
    }
    return operation;
  };

  const setLifecycleActionsDisabled = (disabled) => {
    for (const id of ["archive-session-open", "revive-session-open", "revive-session-retry", "delete-session-open"]) {
      document.getElementById(id)?.toggleAttribute("disabled", disabled);
    }
  };
  const stopLifecycleTimers = () => {
    if (lifecycleClock !== null) clearInterval(lifecycleClock);
    if (lifecyclePollTimer !== null) clearTimeout(lifecyclePollTimer);
    lifecycleClock = null;
    lifecyclePollTimer = null;
  };
  const clearBrowserThreadStorage = (deletedThreadId) => {
    if (!deletedThreadId) return;
    let localStorage = null;
    let sessionStorage = null;
    try { localStorage = globalThis.localStorage; } catch (_error) {}
    try { sessionStorage = globalThis.sessionStorage; } catch (_error) {}
    clearThreadStorage(localStorage, slug, deletedThreadId);
    clearThreadStorage(sessionStorage, slug, deletedThreadId);
  };
  const showLifecycle = (operation = lastLifecycleOperation, pendingKind = "") => {
    if (!lifecycleStatus || !lifecycleTitle || !lifecycleDetail || !lifecycleRetry) return;
    lastLifecycleOperation = operation;
    if (!lifecycleObservedAt && operation.startedAt) {
      const startedAt = Date.parse(operation.startedAt);
      if (Number.isFinite(startedAt)) lifecycleObservedAt = startedAt;
    }
    const elapsed = lifecycleObservedAt ? Date.now() - lifecycleObservedAt : 0;
    const presentation = lifecyclePresentation(operation, pendingKind, elapsed);
    lifecycleStatus.hidden = presentation.tone === "idle";
    lifecycleStatus.className = `operation-status notice full ${presentation.tone}`;
    lifecycleTitle.textContent = presentation.title;
    lifecycleDetail.textContent = presentation.detail;
    lifecycleRetry.hidden = !presentation.retry;
    lifecycleRetry.textContent = `Retry ${(operation.kind || pendingKind || "operation")}`;
  };
  const failLifecycle = (
    kind, error, resetControls = () => {}, phase = "", failedOperation = lastLifecycleOperation,
  ) => {
    stopLifecycleTimers();
    lifecycleKind = kind;
    setLifecycleActionsDisabled(false);
    resetControls();
    showLifecycle({
      ...failedOperation,
      kind,
      state: "failed",
      phase,
      error: error || "The operation did not finish.",
    });
  };
  const adoptLifecycleAfterRequestFailure = async (kind, expected, error, resetControls) => {
    try {
      const operation = await client.operation();
      switch (lifecycleRecoveryAction(kind, expected, operation)) {
        case "complete":
          if (kind === "delete") clearBrowserThreadStorage(operation.options?.deletedThreadId);
          location.assign(operation.redirect || "/");
          return;
        case "monitor":
          monitorLifecycle(kind, operation, resetControls);
          return;
        case "retry":
          failLifecycle(
            kind,
            operation.error || error,
            resetControls,
            operation.phase,
            operation,
          );
          return;
        case "unchanged":
          failLifecycle(kind, error, resetControls, operation.phase, operation);
          return;
        default:
          break;
      }
    } catch (_statusError) {
      // Keep the initiating request's error when status recovery is unavailable.
    }
    failLifecycle(kind, error, resetControls, "", {});
  };
  const monitorLifecycle = (kind, firstOperation, resetControls = () => {}) => {
    stopLifecycleTimers();
    lifecycleKind = kind;
    const startedAt = Date.parse(firstOperation?.startedAt || "");
    lifecycleObservedAt = Number.isFinite(startedAt) ? startedAt : Date.now();
    setLifecycleActionsDisabled(true);
    showLifecycle(firstOperation || {kind, state: "running"});
    lifecycleClock = setInterval(() => showLifecycle(), 1000);
    const poll = async () => {
      try {
        const operation = await client.operation();
        if (!firstOperation?.receiptId || operation.receiptId !== firstOperation.receiptId ||
            operation.kind !== kind) {
          failLifecycle(
            kind,
            "The lifecycle operation changed. Reload the page before continuing.",
            resetControls,
          );
          return;
        }
        if (operation.state === "complete") {
          stopLifecycleTimers();
          if (operation.kind === "delete") {
            clearBrowserThreadStorage(operation.options?.deletedThreadId);
          }
          location.assign(operation.redirect || "/");
          return;
        }
        if (operation.state === "failed") {
          failLifecycle(
            operation.kind || kind,
            operation.error,
            resetControls,
            operation.phase,
            operation,
          );
          return;
        }
        if (operation.state !== "running") {
          failLifecycle(kind, "The operation stopped before it finished.", resetControls);
          return;
        }
        showLifecycle(operation);
        lifecyclePollTimer = setTimeout(poll, 1200);
      } catch (error) {
        failLifecycle(kind, error.message, resetControls);
      }
    };
    lifecyclePollTimer = setTimeout(poll, 1200);
  };
  const deleteDialog = document.getElementById("delete-session-dialog");
  const deleteForm = document.getElementById("delete-session-form");
  const deleteOpen = document.getElementById("delete-session-open");
  let deleteRetryForce = false;
  const openDeleteDialog = async () => {
    let operation = lastLifecycleOperation;
    if (pendingLifecycle === "delete" &&
        (operation.kind !== "delete" || !operation.receiptId)) {
      try {
        operation = await client.operation();
        if (operation.kind === "delete") showLifecycle(operation, "delete");
      } catch (_error) {}
    }
    deleteForm.elements.force.checked = lifecycleOperationBelongsToPage("delete", operation) &&
      Boolean(operation.options?.force);
    deleteDialog.showModal();
  };
  deleteOpen?.addEventListener("click", () => void openDeleteDialog());
  deleteDialog?.querySelector("[data-dialog-close]")?.addEventListener("click", () => deleteDialog.close());
  deleteForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const controls = Array.from(deleteForm.querySelectorAll("button, input"));
    const resetControls = () => controls.forEach((control) => { control.disabled = false; });
    controls.forEach((control) => { control.disabled = true; });
    deleteDialog.close();
    const operation = lastLifecycleOperation;
    const isRetry = lifecycleOperationBelongsToPage("delete", operation) &&
      (operation.state === "failed" || operation.state === "paused");
    deleteRetryForce = deleteForm.elements.force.checked ||
      Boolean(isRetry && operation.options?.force);
    try {
      if (isRetry) {
        await client.retryOperation(
          operation.receiptId,
          operation.options?.journalId || "",
          deleteRetryForce,
        );
      } else {
        await client.deleteSession(deleteRetryForce, lifecycleTargetId);
      }
      location.assign("/");
    } catch (error) {
      const expected = isRetry ? {
        journalId: operation.options?.journalId || "",
        receiptId: operation.receiptId,
      } : {
        targetId: lifecycleTargetId,
        force: deleteRetryForce,
      };
      await adoptLifecycleAfterRequestFailure("delete", expected, error.message, resetControls);
    }
  });

  const artifactList = document.querySelector(".artifact-list");
  const artifactButtons = () => Array.from(artifactList?.querySelectorAll("[data-artifact-path]") || []);
  const artifactPreview = document.getElementById("artifact-preview");
  const artifactTitle = document.getElementById("artifact-title");
  const artifactDownload = document.getElementById("artifact-download");
  let selectedArtifactPath = "";
  const showArtifact = async (button) => {
    const path = button.dataset.artifactPath;
    if (!path || !artifactPreview) return;
    selectedArtifactPath = path;
    artifactButtons().forEach((candidate) => candidate.classList.toggle("active", candidate === button));
    artifactTitle.textContent = button.dataset.artifactLabel || path;
    artifactDownload.href = `/artifacts/${encodeURIComponent(slug)}/${path.split("/").map(encodeURIComponent).join("/")}`;
    artifactDownload.hidden = false;
    artifactPreview.replaceChildren();
    const loading = document.createElement("p");
    loading.className = "empty";
    loading.textContent = "Loading artifact…";
    artifactPreview.append(loading);
    try {
      const result = await client.artifactPreview(path);
      if (selectedArtifactPath !== path) return;
      artifactPreview.replaceChildren();
      if (result.kind === "markdown") {
        artifactPreview.innerHTML = result.html || "";
        wrapMarkdownTables(artifactPreview);
      } else if (result.kind === "image") {
        const image = document.createElement("img");
        image.src = result.url;
        image.alt = button.dataset.artifactLabel || path;
        artifactPreview.append(image);
      } else {
        const pre = document.createElement("pre");
        pre.textContent = result.text || "";
        artifactPreview.append(pre);
      }
    } catch (error) {
      if (selectedArtifactPath !== path) return;
      const notice = document.createElement("p");
      notice.className = "notice error";
      notice.textContent = `Unable to preview artifact: ${error.message}`;
      artifactPreview.replaceChildren(notice);
    }
  };
  artifactList?.addEventListener("click", (event) => {
    const button = event.target.closest("[data-artifact-path]");
    if (button) showArtifact(button);
  });
  document.querySelector('[data-session-tab="artifacts"]')?.addEventListener("click", () => {
    if (!selectedArtifactPath && artifactButtons()[0]) showArtifact(artifactButtons()[0]);
  });

  let repositoryReview = null;
  let repositoryReviewLoading = null;
  const loadRepositoryReview = async () => {
    if (repositoryReview || repositoryReviewLoading) return repositoryReviewLoading;
    const element = document.getElementById("repositories");
    if (!element) return;
    repositoryReviewLoading = import("/static/repository-review.js").then((module) => {
      repositoryReview = module.mount({
        slug, element, nonce: document.querySelector('meta[name="style-nonce"]')?.content || "",
        createCopyButton: conversationAssets.createCopyButton,
      });
    }).catch((error) => {
      const notice = document.createElement("p");
      notice.className = "notice error";
      notice.textContent = `Unable to load repository review: ${error.message}`;
      element.prepend(notice);
    }).finally(() => { repositoryReviewLoading = null; });
    return repositoryReviewLoading;
  };
  document.addEventListener("session-section-change", (event) => {
    if (event.detail === "repositories") void loadRepositoryReview();
  });
  if (document.getElementById("repositories")?.classList.contains("active")) void loadRepositoryReview();

  let detailsTimer = null;
  let detailsRunning = false;
  let lastRepositoriesHTML = "";
  let lastArtifactsHTML = "";
  const refreshSessionDetails = async () => {
    if (detailsRunning || document.hidden || !artifactList) return;
    if (detailsTimer !== null) clearTimeout(detailsTimer);
    detailsRunning = true;
    const warning = document.getElementById("session-details-warning");
    try {
      const payload = await client.details();
      if (payload.repositoriesHTML !== lastRepositoriesHTML) {
        const repositories = document.getElementById("repositories");
        if (repositoryReviewLoading) await repositoryReviewLoading;
        if (repositoryReview) {
          repositoryReview.updateHTML(payload.repositoriesHTML);
        } else {
          const previousTop = repositories.scrollTop;
          repositories.innerHTML = payload.repositoriesHTML;
          repositories.scrollTop = previousTop;
        }
        lastRepositoriesHTML = payload.repositoriesHTML;
      }
      if (payload.artifactsHTML !== lastArtifactsHTML) {
        artifactList.innerHTML = payload.artifactsHTML;
        lastArtifactsHTML = payload.artifactsHTML;
        const selected = artifactButtons().find((button) => button.dataset.artifactPath === selectedArtifactPath);
        if (selected) {
          selected.classList.add("active");
          artifactTitle.textContent = selected.dataset.artifactLabel || selectedArtifactPath;
        } else if (selectedArtifactPath) {
          selectedArtifactPath = "";
          artifactTitle.textContent = "Artifacts";
          artifactDownload.hidden = true;
          artifactPreview.textContent = "Choose an artifact to preview it.";
        }
      }
      for (const [section, label, count] of [
        ["repositories", "Repositories", payload.repositoryCount], ["artifacts", "Artifacts", payload.artifactCount],
      ]) {
        const tab = document.querySelector(`[data-session-tab="${section}"]`);
        const title = `${label} (${count})`;
        if (tab) {
          tab.querySelector(".rail-label").textContent = title;
          tab.title = title;
          tab.setAttribute("aria-label", title);
        }
      }
      warning.hidden = true;
    } catch (error) {
      warning.textContent = `Session details could not be refreshed: ${error.message}`;
      warning.hidden = false;
    } finally {
      detailsRunning = false;
      detailsTimer = setTimeout(refreshSessionDetails, 15_000);
    }
  };
  sessionTabs.forEach((tab) => tab.addEventListener("click", refreshSessionDetails));
  addEventListener("focus", refreshSessionDetails);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) refreshSessionDetails();
  });
  refreshSessionDetails();

  const archiveDialog = document.getElementById("archive-session-dialog");
  const archiveForm = document.getElementById("archive-session-form");
  const archiveOpen = document.getElementById("archive-session-open");
  const retryArchive = async () => {
    const resetControl = () => {
      archiveOpen.disabled = false;
      setControlLabel(archiveOpen, "Retry archive");
    };
    archiveOpen.disabled = true;
    setControlLabel(archiveOpen, "Archiving…");
    let operation = null;
    try {
      operation = await operationForRetry("archive");
      await client.retryOperation(
        operation.receiptId || "",
        operation.options?.journalId || "",
      );
      location.assign("/");
    } catch (error) {
      if (operation?.options?.journalId) {
        await adoptLifecycleAfterRequestFailure("archive", {
          journalId: operation.options.journalId,
          receiptId: operation.receiptId,
        }, error.message, resetControl);
      } else {
        failLifecycle("archive", error.message, resetControl);
      }
    }
  };
  archiveOpen?.addEventListener("click", () => {
    if (pendingLifecycle === "archive") void retryArchive();
    else archiveDialog.showModal();
  });
  archiveDialog?.querySelector("[data-dialog-close]")?.addEventListener("click", () => archiveDialog.close());
  archiveForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const mode = event.submitter?.value === "abandoned" ? "abandoned" : "complete";
    const button = event.submitter;
    const controls = Array.from(archiveForm.querySelectorAll("button"));
    const idleLabel = mode === "abandoned" ? "Archive as abandoned" : "Archive completed session";
    const resetControls = () => {
      controls.forEach((control) => { control.disabled = false; });
      button.textContent = idleLabel;
    };
    controls.forEach((control) => { control.disabled = true; });
    button.textContent = "Archiving…";
    try {
      await client.archive(mode, lifecycleTargetId);
      archiveDialog.close();
      location.assign("/");
    } catch (error) {
      archiveDialog.close();
      await adoptLifecycleAfterRequestFailure("archive", {
        targetId: lifecycleTargetId,
        mode,
      }, error.message, resetControls);
    }
  });

  const reviveDialog = document.getElementById("revive-session-dialog");
  const reviveForm = document.getElementById("revive-session-form");
  document.getElementById("revive-session-open")?.addEventListener("click", () => reviveDialog.showModal());
  reviveDialog?.querySelector("[data-dialog-close]")?.addEventListener("click", () => reviveDialog.close());
  reviveForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = event.submitter;
    const controls = Array.from(reviveForm.querySelectorAll("button"));
    const resetControls = () => {
      controls.forEach((control) => { control.disabled = false; });
      button.textContent = "Revive session";
    };
    controls.forEach((control) => { control.disabled = true; });
    button.textContent = "Reviving…";
    try {
      await client.revive(body.dataset.lifecycle === "abandoned", lifecycleTargetId);
      reviveDialog.close();
      location.assign("/");
    } catch (error) {
      reviveDialog.close();
      await adoptLifecycleAfterRequestFailure("revive", {
        targetId: lifecycleTargetId,
        allowAbandoned: body.dataset.lifecycle === "abandoned",
      }, error.message, resetControls);
    }
  });
  const reviveRetry = document.getElementById("revive-session-retry");
  const retryRevive = async (control = reviveRetry || lifecycleRetry) => {
    const resetControl = () => {
      if (!control) return;
      control.disabled = false;
      setControlLabel(control, "Retry revive");
    };
    if (control) {
      control.disabled = true;
      setControlLabel(control, "Reviving…");
    }
    let operation = null;
    try {
      operation = await operationForRetry("revive");
      await client.retryOperation(
        operation.receiptId || "",
        operation.options?.journalId || "",
      );
      location.assign("/");
    } catch (error) {
      if (operation?.options?.journalId) {
        await adoptLifecycleAfterRequestFailure("revive", {
          journalId: operation.options.journalId,
          receiptId: operation.receiptId,
        }, error.message, resetControl);
      } else {
        failLifecycle("revive", error.message, resetControl);
      }
    }
  };
  reviveRetry?.addEventListener("click", () => void retryRevive(reviveRetry));
  lifecycleRetry?.addEventListener("click", () => {
    const needsOptions = !pendingLifecycle &&
      !lifecycleOperationBelongsToPage(lifecycleKind, lastLifecycleOperation);
    if (lifecycleKind === "archive" && needsOptions) archiveDialog.showModal();
    else if (lifecycleKind === "archive") void retryArchive();
    else if (lifecycleKind === "delete") void openDeleteDialog();
    else if (lifecycleKind === "revive" && needsOptions) reviveDialog.showModal();
    else if (lifecycleKind === "revive") void retryRevive(lifecycleRetry);
  });
  client.operation().then((operation) => {
    if (!pendingLifecycle && !lifecycleOperationBelongsToPage(operation.kind, operation)) return;
    if (operation.state === "running") {
      monitorLifecycle(operation.kind || pendingLifecycle, operation);
    } else if (operation.state !== "complete" && (operation.state !== "idle" || pendingLifecycle)) {
      lifecycleKind = operation.kind || pendingLifecycle;
      showLifecycle(operation, pendingLifecycle);
    }
  }).catch((error) => {
    if (pendingLifecycle) failLifecycle(pendingLifecycle, error.message);
  });
  const transcript = document.getElementById("transcript");
  const pending = document.getElementById("pending");
  const status = document.getElementById("codex-status");
  if (!transcript) return;

  let refreshTimer = null;
  let refreshRunning = false;
  let refreshDirty = false;
  let transcriptInitialized = false;
  let transcriptSignature = "";
  let transcriptFilter = "messages";
  let transcriptEntries = [];
  const transcriptViews = new Map(["all", "messages", "activity"].map((filter) => [filter, {
    disclosures: new Map(), follow: true, initialized: false, scrollTop: 0,
  }]));
  let planRenderGeneration = 0;
  let dismissedPlanIdentity = "";
  let planImplementationInFlight = false;
  const pendingMessages = new Map();
  const inFlightMessageIDs = new Set();
  let sendReceiptAcknowledgementActive = false;
  const requestInputDrafts = new Map();
  const codexWork = document.getElementById("codex-work");
  const codexWorkElapsed = document.getElementById("codex-work-elapsed");
  const codexWorkLabel = document.getElementById("codex-work-label");
  const codexWorkCounts = document.getElementById("codex-work-counts");
  const durationSummary = document.getElementById("codex-duration");
  let activitySnapshot = null;
  let activityReceivedAt = 0;
  let activityRead = null;
  let activityFailed = false;
  let sendAttemptStorage = null;
  try { sendAttemptStorage = globalThis.sessionStorage; } catch (_error) {}
  let requestInputDraftStorage = null;
  try { requestInputDraftStorage = globalThis.sessionStorage; } catch (_error) {}

  const updateCodexWork = (active = threadActive) => {
    if (!codexWork || !codexWorkElapsed) return;
    if (!activitySnapshot) {
      codexWork.hidden = !active;
      codexWorkLabel.textContent = "Codex is working";
      codexWorkCounts.textContent = "";
      codexWorkElapsed.textContent = "";
      durationSummary.textContent = activityFailed ? "Timing unavailable" : "Loading timing…";
      return;
    }
    const elapsed = interactive ? Date.now() - activityReceivedAt : 0;
    const view = activityPresentation(activitySnapshot, activityFailed ? 15_001 : elapsed);
    const stale = view.stale || activityFailed;
    const waiting = !stale && ["waiting", "idle"].includes(view.state) && activitySnapshot.stateSinceMs;
    codexWork.hidden = !active && !(waiting && interactive);
    codexWork.classList.toggle("waiting", Boolean(waiting));
    codexWorkLabel.textContent = waiting ? "Waiting for instructions" : active ? "Codex is working" : "";
    codexWorkCounts.textContent = active ? view.counts : "";
    codexWorkElapsed.textContent = stale ? "Timing update unavailable" : waiting ?
      `${view.openWait} waiting` : view.turnElapsed ? `${view.turnElapsed} this turn` : "";
    const prefix = activitySnapshot.scope === "sinceFork" ? "Since fork · " : "";
    durationSummary.textContent = `${prefix}Working ${view.working} · Waiting ${view.waiting}`;
    if (view.unclassified > 0 || activitySnapshot.coverageComplete === false) {
      durationSummary.textContent += view.unclassified > 0 ?
        ` · ${formatElapsed(view.unclassified)} unclassified` : " · Partial timing";
    }
    if (stale) durationSummary.textContent += " · Update unavailable";
    durationSummary.title = activitySnapshot.coverageReason ||
      "Waiting includes answered blocking requests and gaps between turns. The current wait is shown separately.";
  };
  const refreshActivity = () => {
    if (!client.activity || document.hidden) return Promise.resolve();
    if (!interactive && activitySnapshot) return Promise.resolve();
    if (activityRead) return activityRead;
    activityRead = client.activity().then((snapshot) => {
      activitySnapshot = snapshot;
      activityReceivedAt = Date.now();
      activityFailed = false;
    }).catch(() => { activityFailed = true; }).finally(() => {
      activityRead = null;
      updateCodexWork();
    });
    return activityRead;
  };
  setInterval(() => { if (!document.hidden) updateCodexWork(); }, 1000);
  setInterval(() => { void refreshActivity(); }, 5000);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) void refreshActivity(); });
  addEventListener("focus", () => { void refreshActivity(); });

  const loadMessageReceipts = () => {
    if (!currentThreadId || pendingMessages.size) return;
    const entries = loadSendAttempts(sendAttemptStorage, slug, currentThreadId);
    if (!entries) return;
    for (const entry of entries) {
      pendingMessages.set(entry.id, {...entry, state: entry.state || "unknown"});
    }
  };

  const renderMessageReceipts = () => {
    const container = document.getElementById("message-receipts");
    if (!container) return;
    container.replaceChildren();
    for (const entry of pendingMessages.values()) {
      const label = messageReceiptLabel(entry);
      if (!label) continue;
      const item = document.createElement("div");
      item.className = "message-receipt";
      item.dataset.receiptState = entry.state;
      const statusText = document.createElement("strong");
      statusText.textContent = label;
      const messageText = document.createElement("span");
      messageText.textContent = entry.message;
      item.append(statusText, messageText);
      container.append(item);
    }
    container.hidden = container.childElementCount === 0;
  };
  loadMessageReceipts();
  renderMessageReceipts();

  const acknowledgeTranscriptMessages = (entries) => {
    if (sendReceiptAcknowledgementActive) return;
    const attempts = loadSendAttempts(sendAttemptStorage, slug, currentThreadId);
    if (!attempts?.length) return;
    const observedAttempts = attempts.filter((attempt) => pendingMessages.get(attempt.id)?.state === "observed");
    const candidates = sendAcknowledgementCandidates(entries, observedAttempts, inFlightMessageIDs, attempts.length)
      .filter((attempt) => pendingMessages.get(attempt.id)?.transcriptDigest === attempt.transcriptDigest);
    const batch = candidates.slice(0, 100);
    if (!batch.length) return;
    sendReceiptAcknowledgementActive = true;
    let continueAcknowledging = false;
    let retryAcknowledgement = false;
    void client.acknowledgeMessages(batch.map((attempt) => ({
      clientUserMessageId: attempt.id, digest: attempt.transcriptDigest,
    }))).then((result) => {
      const acknowledged = new Set(result.acknowledgedClientUserMessageIds || []);
      let removedAll = true;
      for (const attempt of batch) {
        if (!acknowledged.has(attempt.id)) {
          removedAll = false;
          continue;
        }
        if (deleteSendAttempt(sendAttemptStorage, slug, currentThreadId, attempt.id)) {
          pendingMessages.delete(attempt.id);
          const composer = document.getElementById("message-form")?.elements.message;
          if (composer && composer.value.trim() === attempt.message && composerUploads?.ready() &&
              conversationAssets.sameAttachments(composerUploads.ids(), attempt.attachmentIds)) {
            composer.value = "";
            composerUploads.clear();
          }
        } else {
          removedAll = false;
        }
      }
      renderMessageReceipts();
      continueAcknowledging = removedAll && candidates.length > batch.length;
      retryAcknowledgement = !removedAll;
    }).catch(() => {
      retryAcknowledgement = true;
    }).finally(() => {
      sendReceiptAcknowledgementActive = false;
      if (continueAcknowledging) queueMicrotask(() => acknowledgeTranscriptMessages(entries));
      if (retryAcknowledgement) setTimeout(() => acknowledgeTranscriptMessages(entries), 2000);
    });
  };

  const renderPlanActions = async (payload) => {
    const panel = document.getElementById("plan-actions");
    if (!panel) return;
    const generation = ++planRenderGeneration;
    const plan = currentCompletedPlan(payload);
    const eligibleMode = ["plan", "default"].includes(payload.collaborationMode);
    if (!plan || payload.status === "active" || !eligibleMode || planImplementationInFlight) {
      setPlanDecisionVisible(panel, document.getElementById("message-form"), false);
      return;
    }
    // Hide an obsolete decision immediately while the new content is hashed.
    if (panel.dataset.planTurnId !== plan.turnId || panel.planText !== plan.text) {
      setPlanDecisionVisible(panel, document.getElementById("message-form"), false);
    }
    const digest = await sha256Hex(plan.text);
    if (generation !== planRenderGeneration) return;
    const pendingImplementation = pendingPlanImplementation(pendingMessages.values(), digest);
    if (payload.collaborationMode === "default" && !pendingImplementation) {
      setPlanDecisionVisible(panel, document.getElementById("message-form"), false);
      return;
    }
    panel.dataset.planTurnId = plan.turnId;
    panel.dataset.planSha256 = digest;
    panel.planText = plan.text;
    const sameButton = document.getElementById("plan-implement-same");
    if (sameButton) sameButton.textContent = pendingImplementation ? "Check request" : "Implement here";
    setPlanDecisionVisible(panel, document.getElementById("message-form"),
      planIdentity(plan.turnId, digest) !== dismissedPlanIdentity);
  };

  let transcriptUserScroll = false;
  let transcriptScrollTimer = null;
  let transcriptPointerDown = false;
  let transcriptTouchY = null;
  const followTranscript = () => {
    const view = transcriptViews.get(transcriptFilter);
    view.follow = true;
    transcriptUserScroll = false;
    if (transcriptScrollTimer !== null) clearTimeout(transcriptScrollTimer);
    transcript.scrollTop = transcript.scrollHeight;
    document.getElementById("new-output").hidden = true;
  };
  const beginTranscriptScroll = (pause = false) => {
    transcriptUserScroll = true;
    if (pause) transcriptViews.get(transcriptFilter).follow = false;
    if (transcriptScrollTimer !== null) clearTimeout(transcriptScrollTimer);
    transcriptScrollTimer = setTimeout(() => { transcriptUserScroll = false; }, 800);
  };
  transcript.tabIndex = 0;
  transcript.addEventListener("wheel", (event) => beginTranscriptScroll(event.deltaY < 0), {passive: true});
  transcript.addEventListener("touchstart", (event) => {
    transcriptTouchY = event.touches[0]?.clientY ?? null;
    beginTranscriptScroll();
  }, {passive: true});
  transcript.addEventListener("touchmove", (event) => {
    const position = event.touches[0]?.clientY ?? null;
    beginTranscriptScroll(position !== null && transcriptTouchY !== null && position > transcriptTouchY);
    transcriptTouchY = position;
  }, {passive: true});
  transcript.addEventListener("pointerdown", () => {
    transcriptPointerDown = true;
    beginTranscriptScroll();
  });
  addEventListener("pointerup", () => { transcriptPointerDown = false; });
  addEventListener("pointercancel", () => { transcriptPointerDown = false; });
  transcript.addEventListener("keydown", (event) => {
    if (["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "].includes(event.key)) {
      beginTranscriptScroll(["ArrowUp", "PageUp", "Home"].includes(event.key));
    }
  });
  document.getElementById("new-output")?.addEventListener("click", followTranscript);
  transcript.addEventListener("scroll", () => {
    if (!transcript.clientHeight) return;
    const view = transcriptViews.get(transcriptFilter);
    view.follow = transcriptFollowOnScroll(view.follow, transcript, transcriptUserScroll || transcriptPointerDown);
    view.scrollTop = transcript.scrollTop;
    view.initialized = true;
    if (shouldFollowTranscript(transcript)) document.getElementById("new-output").hidden = true;
  });
  const transcriptResize = new ResizeObserver(() => {
    if (transcript.clientHeight && transcriptViews.get(transcriptFilter).follow) transcript.scrollTop = transcript.scrollHeight;
  });
  transcriptResize.observe(transcript);
  document.addEventListener("session-section-change", (event) => {
    if (event.detail !== "codex") return;
    const view = transcriptViews.get(transcriptFilter);
    transcript.scrollTop = view.follow ? transcript.scrollHeight : view.scrollTop;
  });

  const updateMessageActions = () => {
    const sendButton = document.getElementById("message-send");
    const queueButton = document.getElementById("message-queue");
    const interruptButton = document.getElementById("interrupt");
    if (sendButton) { sendButton.textContent = messageActionLabel(threadActive); sendButton.disabled = !composerUploadReady; }
    if (queueButton) queueButton.disabled = !composerUploadReady;
    if (queueButton) queueButton.hidden = !threadActive;
    if (interruptButton) interruptButton.disabled = !threadActive;
  };

  const saveCollaborationMode = async (mode) => {
    const controls = Array.from(document.querySelectorAll(
      "[data-codex-mode], #message-form button, #message-form input, #message-form select, #message-form textarea",
    ));
    controls.forEach((control) => { control.disabled = true; });
    try {
      const saved = await client.settings(undefined, undefined, mode);
      currentModel = saved.model;
      currentEffort = saved.reasoningEffort;
      currentMode = saved.collaborationMode || currentMode;
      applyCurrentSettings();
      scheduleRefresh(0);
    } catch (error) { alert(error.message); }
    finally {
      controls.forEach((control) => { control.disabled = false; });
      applyCurrentSettings();
      updateMessageActions();
    }
  };

  const loadCollaborationModes = async () => {
    try {
      collaborationModes = await client.modes();
      renderCollaborationModes(
        document.getElementById("codex-mode"), collaborationModes, saveCollaborationMode,
      );
      applyCurrentSettings();
    } catch (_error) {
      collaborationModes = [];
      renderCollaborationModes(document.getElementById("codex-mode"), []);
      applyCurrentSettings();
    }
  };

  const appendFileChanges = (element, entry, entryKey, disclosureStates) => {
    const disclosure = document.createElement("details");
    disclosure.open = disclosureStates.get(entryKey) === true;
    const summary = document.createElement("summary");
    summary.textContent = entry.summary || "File changes";
    disclosure.append(summary);
    const changes = fileChangeDiffs(entry.details || "");
    if (!changes.length) {
      const notice = document.createElement("p");
      notice.className = "muted";
      notice.textContent = "File change details are unavailable.";
      disclosure.append(notice);
    }
    for (const change of changes) {
      const section = document.createElement("section");
      section.className = "file-diff";
      const heading = document.createElement("div");
      heading.className = "file-diff-heading";
      const path = document.createElement("code");
      path.textContent = change.path;
      const kind = document.createElement("span");
      kind.textContent = change.kind;
      heading.append(path, kind);
      const pre = document.createElement("pre");
      for (const line of change.lines) {
        const row = document.createElement("span");
        row.className = `diff-line ${line.kind}`;
        row.textContent = line.text || " ";
        pre.append(row);
      }
      section.append(heading, pre);
      disclosure.append(section);
    }
    element.append(disclosure);
  };

  const appendError = (element, entry, entryKey, disclosureStates) => {
    const presentation = transcriptErrorPresentation(entry);
    const heading = document.createElement("strong");
    heading.className = "message-error-heading";
    heading.textContent = presentation.heading;
    element.append(heading);
    if (presentation.message && presentation.message !== presentation.heading) {
      const message = document.createElement("p");
      message.className = "message-error-text";
      message.textContent = presentation.message;
      element.append(message);
    }
    if (presentation.details) {
      const disclosure = document.createElement("details");
      disclosure.className = "message-error-details";
      disclosure.open = disclosureStates.get(entryKey) === true;
      const summary = document.createElement("summary");
      summary.textContent = "Error details";
      const pre = document.createElement("pre");
      pre.textContent = presentation.details;
      disclosure.append(summary, pre);
      element.append(disclosure);
    }
  };

  const appendMessage = (entry, index, entries, disclosureStates) => {
    const kind = entry.kind === "userMessage" ? "user" : ["agentMessage", "reasoning", "plan"].includes(entry.kind) ? "agent" : entry.kind === "error" ? "error" : "event";
    const text = entry.displayText ?? entry.text ?? entry.summary ?? "Codex event";
    const details = entry.details || "";
    const html = entry.html || "";
    const entryKey = transcriptEntryKey(entry, index, entries);
    const element = document.createElement("div");
    const activityElement = conversationAssets.createTranscriptActivity(entry);
    element.className = `message ${kind}`;
    element.dataset.transcriptEntryKey = entryKey;
    if (entry.kind === "error") {
      appendError(element, entry, entryKey, disclosureStates);
    } else if (entry.kind === "fileChange") {
      appendFileChanges(element, entry, entryKey, disclosureStates);
    } else if (activityElement) {
      const disclosure = activityElement.matches("details") ? activityElement : activityElement.querySelector("details");
      if (disclosure) disclosure.open = disclosureStates.get(entryKey) === true;
      element.append(activityElement);
    } else if (details) {
      const disclosure = document.createElement("details");
      disclosure.open = disclosureStates.get(entryKey) === true;
      const summary = document.createElement("summary");
      summary.textContent = text;
      const pre = document.createElement("pre");
      pre.textContent = details;
      disclosure.append(summary, pre);
      element.append(disclosure);
    } else if (html) {
      element.classList.add("markdown");
      element.innerHTML = html;
      wrapMarkdownTables(element);
    } else {
      element.textContent = text;
    }
    if (entry.attachments?.length) element.append(conversationAssets.renderAttachments(entry.attachments, {onRemove: async (file) => {
      await request(`${file.deleteUrl}?confirmed=true`, {method: "DELETE"}); scheduleRefresh(0);
    }}));
    const timestamp = conversationAssets.formatTranscriptTimestamp(entry);
    const time = document.createElement("time");
    time.className = "message-time";
    time.textContent = timestamp.text;
    if (timestamp.dateTime) time.dateTime = timestamp.dateTime;
    time.title = timestamp.title;
    const footer = document.createElement("div");
    footer.className = "message-footer";
    footer.append(conversationAssets.createTranscriptCopyButton(entry), time);
    element.append(footer);
    transcript.append(element);
  };

  const renderTranscriptEntries = (entries, disclosureStates) => {
    transcript.replaceChildren();
    const visibleEntries = transcriptEntriesForFilter(entries, transcriptFilter);
    let dateKey = "";
    entries.forEach((entry, index) => {
      if (transcriptEntryVisible(entry, transcriptFilter)) {
        const timestamp = conversationAssets.formatTranscriptTimestamp(entry);
        if (timestamp.dateKey && timestamp.dateKey !== dateKey) {
          dateKey = timestamp.dateKey;
          const separator = document.createElement("div");
          separator.className = "transcript-date";
          separator.textContent = timestamp.dateLabel;
          transcript.append(separator);
        }
        appendMessage(entry, index, entries, disclosureStates);
      }
    });
    if (!visibleEntries.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = transcriptFilter === "all" ? "No conversation entries yet." :
        `No ${transcriptFilter} in this conversation.`;
      transcript.append(empty);
    }
  };

  const saveTranscriptView = () => {
    const view = transcriptViews.get(transcriptFilter);
    if (!view) return;
    Object.assign(view, captureTranscriptViewState(transcript), {follow: view.follow, initialized: true});
  };

  document.querySelectorAll("[data-transcript-filter]").forEach((button) => {
    button.addEventListener("click", () => {
      const nextFilter = button.dataset.transcriptFilter;
      if (!transcriptViews.has(nextFilter) || nextFilter === transcriptFilter) return;
      saveTranscriptView();
      transcriptFilter = nextFilter;
      document.querySelectorAll("[data-transcript-filter]").forEach((candidate) => {
        const selected = candidate.dataset.transcriptFilter === transcriptFilter;
        candidate.classList.toggle("active", selected);
        candidate.setAttribute("aria-selected", selected ? "true" : "false");
      });
      const view = transcriptViews.get(transcriptFilter);
      renderTranscriptEntries(transcriptEntries, view.disclosures);
      transcript.scrollTop = !view.initialized || view.follow ? transcript.scrollHeight : view.scrollTop;
      view.scrollTop = transcript.scrollTop;
      view.initialized = true;
      const newOutput = document.getElementById("new-output");
      if (newOutput) newOutput.hidden = true;
    });
  });

  const renderThread = async (payload) => {
    if (!payload.threadId) throw new Error("Codex returned no thread");
    currentThreadId = payload.threadId;
    loadMessageReceipts();
    const follow = !transcriptInitialized || transcriptViews.get(transcriptFilter).follow;
    const previousTop = transcript.clientHeight ? transcript.scrollTop : transcriptViews.get(transcriptFilter).scrollTop;
    const entries = payload.entries || [];
    const nextSignature = JSON.stringify(entries);
    const transcriptChanged = !transcriptInitialized || nextSignature !== transcriptSignature;
    for (const observed of await markTranscriptMessagesObserved(pendingMessages, entries)) {
      storeSendAttempt(sendAttemptStorage, slug, currentThreadId, observed);
    }
    renderMessageReceipts();
    acknowledgeTranscriptMessages(entries);
    if (transcriptChanged) {
      const view = transcriptViews.get(transcriptFilter);
      view.disclosures = captureTranscriptDisclosureState(transcript);
      view.follow = follow;
      view.scrollTop = previousTop;
      view.initialized = transcriptInitialized;
      transcriptEntries = entries;
      renderTranscriptEntries(entries, view.disclosures);
    }
    const threadStatus = payload.status;
    threadActive = threadStatus === "active";
    updateCodexWork(threadActive);
    currentModel = payload.model || currentModel;
    currentEffort = payload.reasoningEffort || currentEffort;
    currentMode = payload.collaborationMode || currentMode;
    applyCurrentSettings();
    updateMessageActions();
    status.textContent = threadStatus || "Connected";
    status.className = `badge ${threadStatus === "active" ? "active" : ""}`;
    const newOutput = document.getElementById("new-output");
    if (transcriptChanged) {
      if (follow) {
        transcript.scrollTop = transcript.scrollHeight;
        if (newOutput) newOutput.hidden = true;
      } else {
        transcript.scrollTop = previousTop;
        if (newOutput && transcriptInitialized) newOutput.hidden = false;
      }
      const view = transcriptViews.get(transcriptFilter);
      view.follow = follow;
      view.scrollTop = transcript.clientHeight ? transcript.scrollTop : previousTop;
      view.initialized = true;
    }
    transcriptInitialized = true;
    transcriptSignature = nextSignature;
    renderMessageReceipts();
    renderPlanActions(payload);
  };

  const refreshThread = async () => {
    try {
      await renderThread(await client.thread());
    } catch (error) {
      updateCodexWork(false);
      status.textContent = "Offline";
      status.className = "badge warning";
      if (!transcript.children.length || transcript.querySelector(".empty")) transcript.innerHTML = `<p class="notice error"></p>`;
      const notice = transcript.querySelector(".notice");
      if (notice) notice.textContent = error.message;
    }
  };

  const respond = async (id, payload, container) => {
    const controls = Array.from(container.querySelectorAll("button, input, select, textarea"));
    controls.forEach((control) => { control.disabled = true; });
    try {
      await client.respond(id, payload);
      requestInputDrafts.delete(id);
      deleteRequestInputDraft(requestInputDraftStorage, slug, currentThreadId, id);
      scheduleRefresh(0);
    } catch (error) {
      alert(error.message);
    } finally {
      controls.forEach((control) => { control.disabled = false; });
    }
  };

  const renderApproval = (entry) => {
    const box = document.createElement("article");
    box.className = "approval";
    const title = document.createElement("strong");
    title.textContent = entry.kind === "userInput" ? "Codex needs your input" : entry.method.split("/").slice(-2).join(" · ");
    box.append(title);

    if (entry.kind !== "userInput") {
      const pre = document.createElement("pre");
      pre.textContent = JSON.stringify({request: entry.params, item: entry.item || null}, null, 2);
      box.append(pre);
    }

    if (entry.kind === "terminalOnly") {
      const notice = document.createElement("p");
      notice.className = "notice warning";
      notice.textContent = "This permission request is waiting for an answer in the attached terminal.";
      box.append(notice);
      return box;
    }

    if (entry.error) {
      const notice = document.createElement("p");
      notice.className = "notice error";
      notice.textContent = entry.error;
      box.append(notice);
      return box;
    }
    if (!interactive) return box;
    if (!entry.authorityAvailable) {
      const notice = document.createElement("p");
      notice.className = "notice warning";
      notice.textContent = "The matching thread item is unavailable. Review and answer this request in the terminal.";
      box.append(notice);
      return box;
    }

    if (entry.kind === "userInput") {
      const form = document.createElement("form");
      form.className = "input-wizard stack";
      const questions = entry.questions || [];
      let wizardState = requestInputDrafts.get(entry.id);
      if (!wizardState || wizardState.drafts.length !== questions.length) {
        const saved = loadRequestInputDraft(
          requestInputDraftStorage, slug, currentThreadId, entry.id, questions,
        );
        wizardState = saved ? {...saved, snoozed: false} : {
          drafts: questions.map(() => ({})), page: 0, snoozed: false,
        };
        requestInputDrafts.set(entry.id, wizardState);
      }
      const drafts = wizardState.drafts;
      let page = Math.min(wizardState.page, questions.length - 1);
      box.classList.add("question-approval");
      const questionPanel = document.createElement("div");
      questionPanel.className = "wizard-content";
      const progress = document.createElement("p");
      progress.className = "eyebrow";
      const autoResolution = document.createElement("p");
      autoResolution.className = "muted auto-resolution";
      const actions = document.createElement("div");
      actions.className = "approval-actions wizard-actions";
      const back = document.createElement("button");
      back.type = "button";
      back.className = "quiet";
      back.textContent = "Back";
      const next = document.createElement("button");
      next.type = "button";
      const answerField = (question, rows, placeholder) => {
        const field = document.createElement(question.isSecret ? "input" : "textarea");
        field.name = "answer-note";
        field.placeholder = placeholder;
        field.autocomplete = "off";
        if (question.isSecret) {
          field.type = "password";
          field.className = "secret-answer";
          field.spellcheck = false;
        } else {
          field.rows = rows;
        }
        return field;
      };
      const saveDraft = () => {
        const question = questions[page];
        const selected = form.querySelector('input[name="answer-choice"]:checked');
        const note = form.querySelector('[name="answer-note"]')?.value || "";
        if ((question.options || []).length) {
          drafts[page] = selected ? {
            kind: selected.value === "__other__" ? "other" : "option",
            choice: selected.value === "__other__" ? "" : selected.value,
            note,
          } : {};
        } else {
          drafts[page] = {kind: "freeform", note};
        }
        wizardState.page = page;
        storeRequestInputDraft(
          requestInputDraftStorage, slug, currentThreadId, entry.id, questions, wizardState,
        );
      };
      const renderAutoResolution = () => {
        const dueAt = Number(entry.autoResolutionAtMs || 0);
        autoResolution.textContent = autoResolutionLabel(
          Date.now(), Number(entry.autoResolutionVisibleAtMs || 0), dueAt,
          wizardState.snoozed || Boolean(entry.autoResolveSnoozed),
        );
        autoResolution.hidden = !autoResolution.textContent;
      };
      const snoozeAutoResolution = async () => {
        if (entry.isBlocking || wizardState.snoozed || entry.autoResolveSnoozed) return;
        wizardState.snoozed = true;
        renderAutoResolution();
        try {
          await client.snooze(entry.id);
        } catch (error) {
          wizardState.snoozed = false;
          renderAutoResolution();
          alert(`Unable to pause automatic resolution: ${error.message}`);
          scheduleRefresh(0);
        }
      };
      const renderQuestion = () => {
        const question = questions[page];
        const draft = drafts[page] || {};
        progress.textContent = `${question.header} · ${page + 1} of ${questions.length}`;
        questionPanel.replaceChildren();
        questionPanel.scrollTop = 0;
        const prompt = document.createElement("p");
        prompt.className = "wizard-question";
        prompt.textContent = question.question;
        questionPanel.append(prompt);
        const options = question.options || [];
        if (options.length) {
          const choices = document.createElement("div");
          choices.className = "wizard-options";
          for (const option of options) {
            const label = document.createElement("label");
            label.className = "wizard-option";
            const input = document.createElement("input");
            input.type = "radio";
            input.name = "answer-choice";
            input.value = option.label;
            input.checked = draft.kind === "option" && draft.choice === option.label;
            const text = document.createElement("span");
            const heading = document.createElement("strong");
            heading.textContent = option.label;
            const description = document.createElement("small");
            description.textContent = option.description || "";
            text.append(heading, description);
            label.append(input, text);
            choices.append(label);
          }
          if (question.isOther) {
            const label = document.createElement("label");
            label.className = "wizard-option";
            const input = document.createElement("input");
            input.type = "radio";
            input.name = "answer-choice";
            input.value = "__other__";
            input.checked = draft.kind === "other";
            const text = document.createElement("span");
            const heading = document.createElement("strong");
            heading.textContent = "None of the above";
            text.append(heading);
            label.append(input, text);
            choices.append(label);
          }
          questionPanel.append(choices);
          const note = answerField(question, 2, "Add an optional note…");
          note.value = draft.note || "";
          questionPanel.append(note);
        } else {
          const note = answerField(question, 3, "Type your answer, or leave it unanswered…");
          note.value = draft.note || "";
          questionPanel.append(note);
        }
        back.disabled = page === 0;
        next.textContent = page + 1 === questions.length ? "Submit answers" : "Next";
      };
      back.addEventListener("click", async () => {
        await beforeRequestInputAction(snoozeAutoResolution);
        saveDraft();
        page -= 1;
        wizardState.page = page;
        storeRequestInputDraft(
          requestInputDraftStorage, slug, currentThreadId, entry.id, questions, wizardState,
        );
        renderQuestion();
      });
      next.addEventListener("click", () => form.requestSubmit());
      actions.append(back, next);
      questionPanel.addEventListener("input", () => {
        saveDraft();
        void snoozeAutoResolution();
      });
      questionPanel.addEventListener("change", () => {
        saveDraft();
        void snoozeAutoResolution();
      });
      form.append(progress, autoResolution, questionPanel, actions);
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        await beforeRequestInputAction(snoozeAutoResolution);
        saveDraft();
        if (drafts[page].kind === "other" && !(drafts[page].note || "").trim()) {
          alert("Type your own answer for None of the above, or clear that selection.");
          return;
        }
        if (page + 1 < questions.length) {
          page += 1;
          wizardState.page = page;
          storeRequestInputDraft(
            requestInputDraftStorage, slug, currentThreadId, entry.id, questions, wizardState,
          );
          renderQuestion();
          return;
        }
        const answers = {};
        let unanswered = 0;
        questions.forEach((question, index) => {
          const values = encodeQuestionAnswer(question, drafts[index]);
          if (!values.length) unanswered += 1;
          answers[question.id] = {answers: values};
        });
        if (unanswered && !confirm(
          `${unanswered} ${unanswered === 1 ? "question is" : "questions are"} unanswered. Submit anyway?`,
        )) {
          return;
        }
        await respond(entry.id, {answers}, form);
      });
      renderQuestion();
      renderAutoResolution();
      if (!entry.isBlocking && !wizardState.snoozed && !entry.autoResolveSnoozed) {
        const timer = setInterval(() => {
          if (!box.isConnected) {
            clearInterval(timer);
            return;
          }
          renderAutoResolution();
        }, 1000);
      }
      box.append(form);
    } else {
      const actions = document.createElement("div");
      actions.className = "approval-actions";
      for (const decision of entry.availableDecisions || []) {
        const button = document.createElement("button");
        button.type = "button";
        button.textContent = decision === "acceptForSession" ? "Approve for session" : decision[0].toUpperCase() + decision.slice(1);
        if (decision === "decline" || decision === "cancel") button.className = "danger";
        button.addEventListener("click", () => respond(entry.id, {decision}, actions));
        actions.append(button);
      }
      box.append(actions);
    }
    return box;
  };

  const refreshPending = async () => {
    if (!interactive) return;
    try {
      const entries = await client.pending();
      const currentInputIDs = new Set(entries.filter((entry) => entry.kind === "userInput").map((entry) => entry.id));
      for (const id of requestInputDrafts.keys()) {
        if (!currentInputIDs.has(id)) {
          requestInputDrafts.delete(id);
          deleteRequestInputDraft(requestInputDraftStorage, slug, currentThreadId, id);
        }
      }
      pending.replaceChildren(...entries.map(renderApproval));
    } catch (error) {
      const notice = document.createElement("p");
      notice.className = "notice error";
      notice.textContent = `Unable to load pending requests: ${error.message}`;
      pending.replaceChildren(notice);
    }
  };

  const queuePanel = document.getElementById("queue-panel");
  const queueList = document.getElementById("queue-list");
  const queueStart = document.getElementById("queue-start");
  let queueStartInFlight = false;

  const renderQueue = (entries) => {
    if (!queuePanel || !queueList) return;
    queueList.replaceChildren();
    for (const entry of entries) {
      const item = document.createElement("li");
      const text = document.createElement("span");
      text.textContent = entry.displayText ?? entry.text ?? "Queued input";
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "quiet queue-remove";
      remove.textContent = "Remove";
      remove.addEventListener("click", async () => {
        remove.disabled = true;
        try {
          await client.deleteQueued(entry.id);
          scheduleRefresh(0);
        } catch (error) {
          alert(error.message);
          remove.disabled = false;
        }
      });
      item.append(text, remove);
      if (entry.attachments?.length) item.append(conversationAssets.renderAttachments(entry.attachments));
      queueList.append(item);
    }
    queuePanel.hidden = entries.length === 0;
    if (queueStart) {
      queueStart.dataset.queuedSubmissionId = entries[0]?.id || "";
      queueStart.hidden = threadActive;
      queueStart.disabled = threadActive || queueStartInFlight;
    }
  };

  const refreshQueue = async () => {
    if (!interactive) return;
    try {
      await client.reconcileQueue();
      renderQueue(await client.queue());
    } catch (error) {
      if (!queuePanel || !queueList) return;
      const notice = document.createElement("li");
      notice.className = "notice error";
      notice.textContent = `Unable to load queued messages: ${error.message}`;
      queueList.replaceChildren(notice);
      queuePanel.hidden = false;
      if (queueStart) queueStart.hidden = true;
    }
  };

  const refreshAll = async () => {
    if (refreshRunning) {
      refreshDirty = true;
      return;
    }
    refreshRunning = true;
    do {
      refreshDirty = false;
      await refreshThread();
      await Promise.all([refreshPending(), refreshQueue(), refreshActivity()]);
    } while (refreshDirty);
    refreshRunning = false;
  };

  function scheduleRefresh(delay = 200) {
    refreshDirty = true;
    if (refreshTimer !== null) return;
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      refreshAll();
    }, delay);
  }

  const markMessageOutcomeUnknown = (id) => {
    const entry = pendingMessages.get(id);
    if (!entry || entry.state === "observed" || entry.state === "accepted") return;
    pendingMessages.set(id, {...entry, state: "unknown"});
    renderMessageReceipts();
    scheduleRefresh(0);
  };
  const acceptMessageReceipt = (attempt, receipt) => {
    const current = pendingMessages.get(attempt.id);
    const accepted = {
      ...attempt, ...current,
      state: current?.state === "observed" || attempt.state === "observed" ? "observed" : "accepted",
      steered: Boolean(receipt.steered),
    };
    pendingMessages.set(attempt.id, accepted);
    // The original retry identity remains durable even when this metadata
    // update fails. Keep the known acceptance visible for this page lifetime.
    storeSendAttempt(sendAttemptStorage, slug, currentThreadId, accepted);
    renderMessageReceipts();
  };

  const form = document.getElementById("message-form");
  if (form && interactive) {
    const textarea = form.elements.message;
    composerUploads = conversationAssets.mountUploads(document.getElementById("message-uploads"), {
      basePath: `/uploads/s-${encodeURIComponent(slug)}`, dropTarget: form,
      controlsRoot: document.getElementById("message-upload-controls"),
      storageKey: `workspace-portal.upload-draft.${slug}.${currentThreadId}`,
      onChange: ({ready, count}) => { composerUploadReady = ready; textarea.required = !count; updateMessageActions(); },
    });
    const queueButton = document.getElementById("message-queue");
    const interruptButton = document.getElementById("interrupt");
    let queueAttemptStorage = null;
    try { queueAttemptStorage = globalThis.localStorage; } catch (_error) {}
    const submitMessage = async (queue) => {
      const message = textarea.value.trim();
      if (!message && !composerUploads.count()) return;
      let attachments;
      try { attachments = composerUploads.ids(); } catch (error) { alert(error.message); return; }
      composerUploads.lock(true);
      const controls = Array.from(form.querySelectorAll("button, input, select, textarea"));
      controls.forEach((control) => { control.disabled = true; });
      try {
        if (queue) {
          let queueAttempts = requireQueueAttempts(queueAttemptStorage, slug, currentThreadId);
          let attempt = queueAttempts.find((candidate) => candidate.message === message && sameAttemptAttachments(candidate.attachmentIds, attachments));
          if (!attempt) {
            attempt = {message, id: crypto.randomUUID(), ...(attachments.length ? {attachmentIds: attachments} : {})};
            if (!storeQueueAttempt(queueAttemptStorage, slug, currentThreadId, attempt)) {
              throw new Error("Browser storage is unavailable; queued messages cannot be submitted safely.");
            }
          }
          await client.queueMessage(message, attempt.id, attachments);
          if (!deleteQueueAttempt(queueAttemptStorage, slug, currentThreadId, attempt.id)) {
            throw new Error("The queued message was accepted, but its retry record could not be cleared.");
          }
        } else {
          const queueAttempts = requireQueueAttempts(queueAttemptStorage, slug, currentThreadId);
          if (queueAttempts.some((candidate) => candidate.message === message && sameAttemptAttachments(candidate.attachmentIds, attachments))) {
            throw new Error("This message has an unresolved queue submission. Use Queue next to reconcile it before sending.");
          }
          const sendAttempts = loadSendAttempts(sendAttemptStorage, slug, currentThreadId);
          if (sendAttempts === null) {
            throw new Error("Browser storage is unavailable; messages cannot be submitted safely.");
          }
          let attempt = matchingSendAttempt(sendAttempts, message, "", attachments);
          const retry = Boolean(attempt);
          if (!attempt) {
            attempt = {id: crypto.randomUUID(), message, steered: threadActive, context: "", ...(attachments.length ? {attachmentIds: attachments} : {})};
            if (!storeSendAttempt(sendAttemptStorage, slug, currentThreadId, attempt)) {
              throw new Error("Browser storage is unavailable; messages cannot be submitted safely.");
            }
          }
          const id = attempt.id;
          inFlightMessageIDs.add(id);
          pendingMessages.set(id, {...attempt, state: attempt.state || "sending"});
          renderMessageReceipts();
          try {
            acceptMessageReceipt(attempt, await client.message(message, id, retry, attachments));
          } finally {
            inFlightMessageIDs.delete(id);
            scheduleRefresh(0);
          }
        }
        textarea.value = "";
        composerUploads.clear();
        if (!queue) followTranscript();
        else {
          await refreshQueue();
          queuePanel.scrollIntoView({block: "nearest"});
        }
        scheduleRefresh(0);
      } catch (error) {
        for (const [id, entry] of pendingMessages) {
          if (entry.state === "sending" && entry.message === message) {
            markMessageOutcomeUnknown(id);
          }
        }
        alert(error.message);
      }
      finally {
        controls.forEach((control) => { control.disabled = false; });
        composerUploads.lock(false);
        applyCurrentSettings();
        updateMessageActions();
        textarea.focus();
      }
    };
    textarea.addEventListener("keydown", (event) => {
      if (!shouldSubmitMessage(event)) return;
      event.preventDefault();
      if (!textarea.value.trim() && !composerUploads.count()) return;
      form.requestSubmit();
    });
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      await submitMessage(false);
    });
    queueButton.addEventListener("click", () => submitMessage(true));
    interruptButton.addEventListener("click", async () => {
      try {
        await client.interrupt();
        scheduleRefresh(0);
      } catch (error) { alert(error.message); }
    });
    updateMessageActions();
  }

  queueStart?.addEventListener("click", async () => {
    const queuedSubmissionId = queueStart.dataset.queuedSubmissionId;
    if (!queuedSubmissionId) return;
    queueStartInFlight = true;
    queueStart.disabled = true;
    try {
      await client.startQueue(queuedSubmissionId);
      followTranscript();
    } catch (error) {
      alert(error.message);
    } finally {
      queueStartInFlight = false;
      await refreshQueue();
      scheduleRefresh(0);
    }
  });

  const planActions = document.getElementById("plan-actions");
  document.getElementById("plan-keep-planning")?.addEventListener("click", () => {
    dismissedPlanIdentity = planIdentity(planActions.dataset.planTurnId, planActions.dataset.planSha256);
    ++planRenderGeneration;
    setPlanDecisionVisible(planActions, document.getElementById("message-form"), false, true);
  });
  document.getElementById("plan-implement-same")?.addEventListener("click", async (event) => {
    const button = event.currentTarget;
    const message = "Implement the plan.";
    const context = `plan:${planActions.dataset.planSha256}`;
    const attempts = loadSendAttempts(sendAttemptStorage, slug, currentThreadId);
    if (attempts === null) {
      alert("Browser storage is unavailable; messages cannot be submitted safely.");
      return;
    }
    let attempt = matchingSendAttempt(attempts, message, context);
    if (!attempt) {
      attempt = {id: crypto.randomUUID(), message, steered: false, context};
      if (!storeSendAttempt(sendAttemptStorage, slug, currentThreadId, attempt)) {
        alert("Browser storage is unavailable; messages cannot be submitted safely.");
        return;
      }
    }
    const id = attempt.id;
    inFlightMessageIDs.add(id);
    pendingMessages.set(id, {...attempt, state: attempt.state || "sending"});
    renderMessageReceipts();
    button.disabled = true;
    planImplementationInFlight = true;
    ++planRenderGeneration;
    const implementingIdentity = planIdentity(planActions.dataset.planTurnId, planActions.dataset.planSha256);
    try {
      const receipt = await client.implementPlan({
        action: "same",
        planTurnId: planActions.dataset.planTurnId,
        planSha256: planActions.dataset.planSha256,
        clientUserMessageId: id,
      });
      acceptMessageReceipt(attempt, receipt);
      currentMode = "default";
      followTranscript();
      dismissedPlanIdentity = implementingIdentity;
      ++planRenderGeneration;
      setPlanDecisionVisible(planActions, document.getElementById("message-form"), false);
      renderMessageReceipts();
      applyCurrentSettings();
      scheduleRefresh(0);
    } catch (error) {
      markMessageOutcomeUnknown(id);
      button.disabled = false;
      alert(error.message);
    } finally {
      planImplementationInFlight = false;
      button.disabled = false;
      inFlightMessageIDs.delete(id);
      scheduleRefresh(0);
    }
  });

  const planSessionDialog = document.getElementById("plan-session-dialog");
  const planSessionForm = document.getElementById("plan-session-form");
  let planSessionSnapshot = null;
  document.getElementById("plan-implement-new")?.addEventListener("click", () => {
    planSessionSnapshot = {
      planTurnId: planActions.dataset.planTurnId,
      planSha256: planActions.dataset.planSha256,
      planText: planActions.planText,
      model: currentModel,
      reasoningEffort: currentEffort,
    };
    planSessionDialog.showModal();
  });
  planSessionDialog?.querySelector("[data-dialog-close]")?.addEventListener("click", () => planSessionDialog.close());
  planSessionForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const controls = Array.from(planSessionForm.querySelectorAll("button, input"));
    controls.forEach((control) => { control.disabled = true; });
    const progress = timedProgress(
      document.getElementById("plan-session-progress"), "Creating session",
    );
    try {
      const result = await client.implementPlan({
        action: "new",
        ...planSessionSnapshot,
        name: planSessionForm.elements.name.value,
        creationDate: planSessionForm.elements.creationDate.value,
      });
      progress.stop();
      location.assign(result.url);
    } catch (error) {
      progress.fail(error.message);
      controls.forEach((control) => { control.disabled = false; });
    }
  });

  const liveModelSelect = document.getElementById("codex-model");
  const liveEffortSelect = document.getElementById("codex-effort");
  const liveSettingsStatus = document.getElementById("codex-settings-status");
  const saveLiveSettings = async () => {
    if (!interactive || !liveModelSelect || !liveEffortSelect || threadActive) return;
    liveModelSelect.disabled = true;
    liveEffortSelect.disabled = true;
    if (liveSettingsStatus) liveSettingsStatus.textContent = "Saving Codex settings";
    try {
      const saved = await client.settings(liveModelSelect.value, liveEffortSelect.value);
      currentModel = saved.model;
      currentEffort = saved.reasoningEffort;
      if (liveSettingsStatus) liveSettingsStatus.textContent = "Codex settings saved";
      scheduleRefresh(0);
    } catch (error) {
      if (liveSettingsStatus) liveSettingsStatus.textContent = "Codex settings were not saved";
      alert(error.message);
    } finally {
      applyCurrentSettings();
    }
  };
  liveModelSelect?.addEventListener("change", () => { void saveLiveSettings(); });
  liveEffortSelect?.addEventListener("change", () => { void saveLiveSettings(); });

  const forkDialog = document.getElementById("fork-dialog");
  const forkForm = document.getElementById("fork-form");
  document.getElementById("fork-open")?.addEventListener("click", () => forkDialog.showModal());
  forkDialog?.querySelector("[data-dialog-close]")?.addEventListener("click", () => forkDialog.close());
  if (forkForm && interactive) {
    forkForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const controls = Array.from(forkForm.querySelectorAll("button, input, select"));
      controls.forEach((control) => { control.disabled = true; });
      const progress = timedProgress(document.getElementById("fork-progress"), "Creating fork");
      try {
        const result = await client.fork(
          forkForm.elements.name.value,
          forkForm.elements.creationDate.value,
          forkForm.elements.model.value,
          forkForm.elements.effort.value,
        );
        progress.stop();
        location.assign(result.url);
      } catch (error) {
        progress.fail(error.message);
        controls.forEach((control) => { control.disabled = false; });
      }
    });
  }

  document.querySelectorAll("[data-release-cluster]").forEach((button) => {
    button.addEventListener("click", async () => {
      const kind = button.dataset.releaseCluster;
      if (!confirm("Stop this development cluster and remove its temporary state?")) return;
      button.disabled = true;
      button.textContent = "Releasing…";
      try {
        await client.releaseCluster(kind);
        location.reload();
      } catch (error) {
        alert(error.message);
        button.disabled = false;
        button.textContent = "Release cluster";
      }
    });
  });

  document.querySelectorAll("[data-cluster]").forEach((clusterCard) => {
    const tabs = Array.from(clusterCard.querySelectorAll("[data-cluster-service-tab]"));
    const panels = Array.from(clusterCard.querySelectorAll("[data-cluster-service-panel]"));
    tabs.forEach((tab) => tab.addEventListener("click", () => {
      tabs.forEach((candidate) => {
        const selected = candidate === tab;
        candidate.classList.toggle("active", selected);
        candidate.setAttribute("aria-selected", selected ? "true" : "false");
        candidate.tabIndex = selected ? 0 : -1;
      });
      panels.forEach((panel) => panel.classList.toggle(
        "active", panel.dataset.clusterServicePanel === tab.dataset.clusterServiceTab,
      ));
    }));
  });
  document.querySelectorAll("[data-reveal-secret]").forEach((button) => {
    button.addEventListener("click", () => {
      const input = button.parentElement.querySelector("[data-secret-field]");
      if (!input) return;
      const reveal = input.type === "password";
      input.type = reveal ? "text" : "password";
      button.textContent = reveal ? "Hide" : "Reveal";
    });
  });

  scheduleRefresh(0);
  if (interactive) loadCollaborationModes();
  if (interactive) {
    const events = new EventSource(client.eventsPath());
    events.onopen = () => { scheduleRefresh(0); };
    events.onmessage = () => { scheduleRefresh(); };
    events.onerror = () => { status.textContent = "Reconnecting"; status.className = "badge warning"; };
  }
})();
