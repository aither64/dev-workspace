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
    details: (options) => request(apiPath(slug, "details"), options),
    autoArchive: () => request(apiPath(slug, "auto-archive")),
    autoArchiveHold: (hold, targetId) => request(apiPath(slug, "auto-archive"), {
      method: "POST", body: JSON.stringify({hold, targetId}),
    }),
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
  const planActionContext = (turnId, digest) => `plan:${turnId}:${digest}`;
  const pendingPlanImplementation = (attempts, turnId, digest) => Boolean(
    matchingSendAttempt([...attempts], "Implement the plan.", planActionContext(turnId, digest))
  );
  const planRecoveryRequest = (attempt, latestTurnId) => {
    if (attempt.message !== "Implement the plan.") return null;
    const legacy = /^plan:([0-9a-f]{64})$/.exec(attempt.context || "");
    if (legacy) return {action: "recover", planSha256: legacy[1], clientUserMessageId: attempt.id};
    const current = /^plan:(.+):([0-9a-f]{64})$/.exec(attempt.context || "");
    if (!current || !latestTurnId || current[1] === latestTurnId) return null;
    return {action: "recover", planContextVersion: 2, planTurnId: current[1], planSha256: current[2], clientUserMessageId: attempt.id};
  };
  // Plan rendering and pending-request refreshes share one owner for the composer.
  const createComposerView = (panel, form, pending) => {
    const document = form.ownerDocument;
    const chat = form.closest(".chat-panel");
    const interrupt = form.querySelector("#interrupt");
    const interruptHome = interrupt.parentElement;
    let planVisible = false;

    const update = ({focusComposer = false, previousQuestion = null, previousFocus = document.activeElement} = {}) => {
      const question = pending.querySelector(".question-approval");
      const showPlan = planVisible && !question;
      const hideComposer = Boolean(question || showPlan);
      const moveFocus = focusComposer || previousQuestion ||
        (hideComposer && form.contains(previousFocus)) ||
        (!showPlan && panel.contains(previousFocus));
      if (hideComposer && !form.hidden) {
        form.querySelectorAll(":popover-open").forEach((menu) => menu.hidePopover());
      }
      const interruptParent = question ? question.querySelector(".question-heading") : interruptHome;
      if (interrupt.parentElement !== interruptParent) interruptParent.append(interrupt);
      panel.hidden = !showPlan;
      form.hidden = hideComposer;
      chat.classList.toggle("plan-decision", showPlan);
      if (previousFocus === interrupt && !interrupt.disabled && (question || !hideComposer)) {
        if (document.activeElement !== interrupt) interrupt.focus();
      } else if (moveFocus) {
        const matchingQuestion = previousQuestion && [...pending.querySelectorAll(".question-approval")]
          .find((entry) => previousQuestion.dataset.promptKey ? entry.dataset.promptKey === previousQuestion.dataset.promptKey :
            entry.dataset.requestId === previousQuestion.dataset.requestId);
        const matchingControl = matchingQuestion && [...matchingQuestion.querySelectorAll("input, textarea, button")]
          .find((control) => !control.disabled && control.name && control.name === previousFocus.name &&
            (previousFocus.type !== "radio" || control.value === previousFocus.value));
        const target = matchingControl || (question ?
          question.querySelector(".input-wizard input:checked:not(:disabled)") ||
            question.querySelector(".input-wizard input:not(:disabled), .input-wizard textarea:not(:disabled), .input-wizard button:not(:disabled)") :
          showPlan ? panel.querySelector("#plan-keep-planning") : form.querySelector("textarea"));
        target?.focus({preventScroll: true});
        if (matchingControl && previousFocus.selectionStart !== null && previousFocus.selectionStart !== undefined) {
          matchingControl.setSelectionRange(previousFocus.selectionStart, previousFocus.selectionEnd);
        }
      }
    };

    return {
      restoreFocus(previousFocus, previousQuestion) {
        if (document.activeElement === document.body) update({previousFocus, previousQuestion});
      },
      setInterruptEnabled(enabled) {
        const wasFocused = document.activeElement === interrupt;
        interrupt.disabled = !enabled;
        if (wasFocused && !enabled) update({focusComposer: true});
      },
      setPlanVisible(visible, focusComposer = false) {
        planVisible = visible;
        update({focusComposer});
      },
      replacePending(entries) {
        const previousFocus = document.activeElement;
        const previousQuestion = pending.contains(previousFocus) ? previousFocus.closest(".question-approval") : null;
        pending.replaceChildren(...entries);
        update({previousFocus, previousQuestion});
      },
    };
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

  // A read may settle after abort (for example from a cache or test transport).
  // Consumers must check its generation before applying either data or errors.
  const createReadScope = () => {
    let paused = false, generation = 0;
    const reads = new Set();
    return {
      get paused() { return paused; },
      begin(timeout = 10_000) {
        const abort = new AbortController(), ticket = generation;
        const timer = setTimeout(() => abort.abort(), timeout);
        reads.add(abort);
        if (paused) abort.abort();
        return {signal: abort.signal, isCurrent: () => !paused && generation === ticket,
          finish() { clearTimeout(timer); reads.delete(abort); }};
      },
      pause() { paused = true; ++generation; for (const abort of reads) abort.abort(); },
      resume() { paused = false; },
    };
  };
  const createTimingClock = (now = Date.now) => {
    let received = null, elapsed = 0, paused = false, needsFresh = true, recovery = now();
    return {
      received() { received = now(); elapsed = 0; needsFresh = false; recovery = null; },
      pause() { this.view(); paused = true; needsFresh = true; recovery = null; },
      resume() { paused = false; recovery = now(); },
      failed() { needsFresh = true; if (!paused && recovery === null) recovery = now(); },
      view() {
        const age = received === null ? Infinity : Math.max(0, now() - received);
        const stale = needsFresh || age > 15_000;
        if (!paused && !stale) elapsed = age;
        if (!paused && stale && recovery === null) recovery = now();
        return {elapsed, stale, unavailable: !paused && stale && recovery !== null && now() - recovery >= 10_000};
      },
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
        detail: `Paused · ${formatElapsed(elapsedMilliseconds)} since the last completed step`,
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
  const createPromptSnooze = (token, send) => {
    const state = {token, paused: false, failed: false, pending: null, onChange: null,
      pause(retry = false) {
        if (state.pending) return state.pending;
        if (state.paused || state.failed && !retry) return Promise.resolve(state.paused);
        state.failed = false; state.paused = true;
        state.onChange?.();
        state.pending = Promise.resolve().then(send).then(() => true, () => {
          state.paused = false; state.failed = true; return false;
        }).finally(() => { state.pending = null; state.onChange?.(); });
        return state.pending;
      },
    };
    return state;
  };
  const promptIdentity = entry => {
    const turn = entry.turnId || entry.params?.turnId;
    if (!entry.threadId || !turn || !entry.itemId) return null;
    return JSON.stringify([entry.threadId, turn, entry.itemId, entry.method, entry.kind,
      entry.isBlocking, entry.questions || [], entry.availableDecisions || []]);
  };
  const promptDraftKey = entry => promptIdentity(entry) || JSON.stringify([entry.threadId, entry.id, entry.token || ""]);
  const respondWithRecovery = async (entry, payload, send, recover, onRecovery = () => {}) => {
    try { await send(entry, payload); return entry; }
    catch (error) {
      if (["invalid_response", "reload_required"].includes(error.code)) throw error;
      onRecovery(error);
      let restored;
      try { restored = await recover(entry); } catch (_) { /* Preserve the original delivery outcome. */ }
      const identity = promptIdentity(entry);
      if (error.notSent === true && ["prompt_changed", "prompt_transport"].includes(error.code) &&
          entry.kind === "userInput" && identity && restored?.token && restored.authorityAvailable &&
          promptIdentity(restored) === identity) {
        // Exactly one resend, only after an authoritative restoration. A second
        // failure escapes to the caller even when it was also definitely unsent.
        await send(restored, payload);
        return restored;
      }
      throw error;
    }
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
      if (!value || value.questions !== JSON.stringify(questions) || !Number.isInteger(value.page) || value.page < 0 ||
          !Array.isArray(value.drafts) || value.drafts.length !== questions.length) return null;
      const drafts = value.drafts.map((draft, index) => {
        if (questions[index]?.isSecret) return {};
        if (!draft || !["", "option", "other", "freeform"].includes(draft.kind)) return {};
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
        JSON.stringify({page: state.page, drafts, questions: JSON.stringify(questions)}),
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
  const autoArchivePresentation = (state) => {
    const date = value => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString() : "";
    const rules = {
      complete: "After 1 inactive day for completed sessions.",
      merged: "After 7 inactive days, once all registered branches are merged.",
      empty: "After 14 inactive days without registered repositories or owned worktrees; archived as abandoned.",
    };
    const fields = [["Workspace policy", state.enabled ? "Enabled" : "Disabled"],
      ["Rule", rules[state.tier] || "No automatic archival rule applies."],
      ["Last check", date(state.checked_at) || "Waiting for the first scan."]];
    if (state.enabled && !state.hold && date(state.eligible_at)) fields.push(["Not before", date(state.eligible_at)]);
    if (state.result) fields.push(["Last result", ({archived: "Archived", deferred: "Deferred", error: "Check failed"})[state.result] || "Unknown result"]);
    const blockers = [], diagnostics = [];
    const plain = {
      "Session has uncommitted worktree changes.": "The session has uncommitted worktree changes.",
      "Abandoned sessions require manual archival.": "Abandoned sessions must be archived manually.",
      "Session has no automatic archive rule.": "No automatic archival rule applies to this session.",
    };
    for (const raw of state.blockers || []) {
      if (raw === "Automatic archival is disabled." || raw === "Keep open is enabled.") continue;
      if (Object.hasOwn(plain, raw)) { blockers.push(plain[raw]); continue; }
      let message = "An archival check could not be completed. See Technical details.";
      if (/Codex thread \S+ is not idle \(latest turn \S+ has status "inProgress"\)/.test(raw)) message = "Codex has an active turn.";
      else if (/Codex thread \S+ has \d+ pending request\(s\)/.test(raw)) message = "Codex has pending requests.";
      else if (/Codex thread \S+ has \d+ queued message\(s\)/.test(raw)) message = "Codex has queued messages.";
      else if (/thread retire/.test(raw) && /exit 124|context deadline exceeded/.test(raw)) message = "Closing the conversation timed out.";
      else if (/thread retire/.test(raw)) message = "The conversation could not be closed.";
      blockers.push(message); diagnostics.push(raw);
    }
    return {fields, blockers: [...new Set(blockers)], diagnostics};
  };

  const archiveFailurePresentation = (operation, state, threadId) => {
    if (operation?.kind !== "archive" || !["paused", "failed", "running"].includes(operation.state) ||
        !operation.options?.journalId || operation.options.journalId !== state?.operation?.id ||
        !threadId || state.identity !== threadId || state.operation.identity !== threadId ||
        !["deferred", "error"].includes(state.result) || !Number.isFinite(Date.parse(state.checked_at))) return null;
    const presentation = autoArchivePresentation(state);
    if (!presentation.blockers.length) return null;
    return {message: presentation.blockers.join(" "), attemptedAt: state.checked_at};
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

  // Creation drafts are deliberately small browser-local retry records. The
  // catalog digest binds their team policy to the catalog that rendered it.
  // A later catalog is never combined with a saved team or lead override.
  const normalizeCreationDraft = (value, kind) => {
    if (!value || typeof value !== "object" || Array.isArray(value) ||
        typeof value.name !== "string" || typeof value.date !== "string" ||
        !/^\d{4}-\d{2}-\d{2}$/.test(value.date) ||
        (kind === "new" && typeof value.goal !== "string")) return null;
    const draft = {
      name: value.name, date: value.date,
      model: typeof value.model === "string" ? value.model : "",
      effort: typeof value.effort === "string" ? value.effort : "",
      team: typeof value.team === "string" ? value.team : "",
      catalogDigest: typeof value.catalogDigest === "string" ? value.catalogDigest : "",
    };
    if (kind === "new") draft.goal = value.goal;
    return draft;
  };
  const loadCreationDraft = (storage, key, kind) => {
    try { return normalizeCreationDraft(JSON.parse(storage?.getItem(key) || "null"), kind); }
    catch (_error) { return null; }
  };
  const storeCreationDraft = (storage, key, value, kind) => {
    const draft = normalizeCreationDraft(value, kind);
    if (!draft) return false;
    try { storage?.setItem(key, JSON.stringify(draft)); return true; }
    catch (_error) { return false; }
  };
  const planSessionDraftKey = (slug, snapshot) => (
    `workspace-portal.plan-session-draft.${slug}.${snapshot.planTurnId}.${snapshot.planSha256}`
  );
  const creationDraftCatalogRecovery = (draft, currentCatalogDigest, acknowledged = false) => {
    const current = typeof currentCatalogDigest === "string" ? currentCatalogDigest : "";
    const changed = Boolean(draft && current && draft.catalogDigest !== current);
    return {
      catalogChanged: changed,
      requiresAcknowledgement: changed && !acknowledged,
      persistedCatalogDigest: changed && !acknowledged ? draft.catalogDigest : (current || draft?.catalogDigest || ""),
    };
  };
  const planSessionCreationSettings = (managed, snapshot, draft) => (
    managed ? {
      team: draft.team, catalogDigest: draft.catalogDigest,
      model: draft.model, reasoningEffort: draft.effort,
    } : {
      model: snapshot.model, reasoningEffort: snapshot.reasoningEffort,
    }
  );
  const effortSelectionForModelRefresh = (managedOverride, selectedEffort, currentEffort) => (
    managedOverride ? selectedEffort : currentEffort
  );
  const creationSubmitEligible = ({uploadReady = true, recoveryPending = false, inFlight = false} = {}) => (
    Boolean(uploadReady) && !recoveryPending && !inFlight
  );
  const shellQuote = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;
  const creationCLICommand = ({name, team, model, effort}) => {
    if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,47}$/.test(name || "") || !model || !effort) return "";
    const args = ["dev-session start", shellQuote(name)];
    if (team) args.push("--team", shellQuote(team));
    args.push("--model", shellQuote(model), "--effort", shellQuote(effort));
    return args.join(" ");
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      automaticReasoningLabel, createRequest, createSessionClient, createCodexLimitsReader, autoArchivePresentation, archiveFailurePresentation,
      currentCompletedPlan, planIdentity, planActionContext, pendingPlanImplementation, planRecoveryRequest, createComposerView,
      createPromptSnooze, promptIdentity, promptDraftKey, respondWithRecovery, autoResolutionLabel, beforeRequestInputAction, clearThreadStorage,
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
      createReadScope, createTimingClock, activityAge, activityPresentation, fileChangeDiffs, formatElapsed, indexStatusFreshForPage,
      indexStatusOrder, lifecycleOperationMatches, lifecyclePresentation, lifecycleRecoveryAction,
      sessionTabFromHash, sessionTabFromLocation,
      transcriptEntriesForFilter, transcriptEntryKey, transcriptEntryVisible,
      transcriptErrorPresentation, wrapMarkdownTables,
      creationSubmitEligible, effortSelectionForModelRefresh, loadCreationDraft, normalizeCreationDraft,
      creationDraftCatalogRecovery, planSessionCreationSettings, planSessionDraftKey, storeCreationDraft,
      creationCLICommand,
    };
    return;
  }

  const body = document.body;
  const narrowSidebar = globalThis.matchMedia("(max-width: 760px)");
  let comparisonOpen = false, updateLimitsPopover = () => {};
  const updateSidebar = () => {
    const reviewing = comparisonOpen && document.getElementById("repositories")?.classList.contains("active");
    body.classList.toggle("compact-sidebar", narrowSidebar.matches || Boolean(reviewing));
    updateLimitsPopover();
  };
  narrowSidebar.addEventListener("change", updateSidebar);
  document.addEventListener("session-section-change", updateSidebar);
  updateSidebar();
  document.querySelectorAll(".document").forEach(wrapMarkdownTables);
  const slug = body.dataset.session;
  const lifecycleTargetId = body.dataset.lifecycleTargetId || "";
  const interactive = body.dataset.interactive === "true";
  const request = createRequest(fetch.bind(globalThis));
  const conversationAssets = await import("/codex/assets/conversation.js?v=9");
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
    updateLimitsPopover = () => {
      const compact = body.classList.contains("compact-sidebar");
      if (compact === limitsPanel.hasAttribute("popover")) return;
      if (compact) limitsPanel.setAttribute("popover", "auto");
      else {
        if (limitsPanel.matches(":popover-open")) limitsPanel.hidePopover();
        limitsPanel.removeAttribute("popover");
      }
      toggle.setAttribute("aria-expanded", "false");
    };
    limitsPanel.addEventListener("toggle", (event) => {
      toggle.setAttribute("aria-expanded", String(event.newState === "open"));
    });
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
          if (item.clusterNotice) clusters.textContent = item.runningClusters ? `${clusters.textContent} · Status updating` : "Cluster status updating";
          clusters.title = item.clusterNotice || "";
          clusters.hidden = item.runningClusters === 0 && !item.clusterNotice;
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
  let creationDraft = null;
  let planSessionDraft = null;
  let persistCreationDraft = null;
  let persistPlanSessionDraft = null;
  let updateCreationCLI = () => {};
  const managedDraftRecoveries = new WeakMap();
  const managedFormSubmissionState = new WeakMap();
  const managedFormSubmitButtons = (form) => Array.from(form.querySelectorAll("button")).filter((button) => (
    (button.type || "submit") === "submit"
  ));
  const managedFormRecoveryPending = (form) => Boolean(
    managedDraftRecoveries.get(form)?.requiresAcknowledgement,
  );
  const updateManagedFormSubmitEligibility = (form, changes = {}) => {
    if (!form) return false;
    const state = managedFormSubmissionState.get(form) || {uploadReady: true, inFlight: false};
    Object.assign(state, changes);
    managedFormSubmissionState.set(form, state);
    const eligible = creationSubmitEligible({
      uploadReady: state.uploadReady,
      recoveryPending: managedFormRecoveryPending(form),
      inFlight: state.inFlight,
    });
    managedFormSubmitButtons(form).forEach((button) => { button.disabled = !eligible; });
    return eligible;
  };
  if (body.hasAttribute("data-index")) {
    const form = document.getElementById("new-session-form");
    let creationUploads = null;
    const updateCreationSubmitEligibility = (changes = {}) => updateManagedFormSubmitEligibility(form, changes);
    let creationDraftStorage;
    const creationDraftKey = "workspace-portal.creation-draft";
    try { creationDraftStorage = globalThis.sessionStorage; } catch (_) {}
    const saveCreationDraft = () => {
      const policy = managedDraftPolicyForStorage(form, {
        model: form.elements.model?.value || "", effort: form.elements.effort?.value || "",
        team: form.elements.team?.value || "", catalogDigest: form.elements.catalogDigest?.value || "",
      });
      const draft = normalizeCreationDraft({
        name: form.elements.name.value, goal: form.elements.goal.value,
        date: form.elements.creation_date.value,
        ...policy,
      }, "new");
      if (draft) {
        creationDraft = draft;
        storeCreationDraft(creationDraftStorage, creationDraftKey, draft, "new");
      }
    };
    if (form) {
      updateCreationCLI = () => {
        const output = form.querySelector("[data-cli-command]");
        if (!output) return;
        const command = creationCLICommand({
          name: form.elements.name.value.trim(), team: form.elements.team?.value || "",
          model: form.elements.model?.value || "", effort: form.elements.effort?.value || "",
        });
        output.value = command || "Choose a short name, model and reasoning effort.";
        output.parentElement.querySelector("[data-copy]").disabled = !command || managedFormRecoveryPending(form);
      };
      persistCreationDraft = saveCreationDraft;
      creationDraft = loadCreationDraft(creationDraftStorage, creationDraftKey, "new");
      if (creationDraft) {
        form.elements.name.value = creationDraft.name;
        form.elements.goal.value = creationDraft.goal;
        form.elements.creation_date.value = creationDraft.date;
      }
      form.addEventListener("input", () => { saveCreationDraft(); updateCreationCLI(); });
      form.addEventListener("change", () => { saveCreationDraft(); updateCreationCLI(); });
      updateCreationCLI();
    }
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
            onChange: ({ready, count}) => {
              form.elements.goal.required = !count;
              updateCreationSubmitEligibility({uploadReady: ready});
            },
          });
          form.elements.uploadScope.value = scope.id;
          await creationUploads.initialized;
          updateCreationSubmitEligibility({uploadReady: creationUploads.ready()});
        } catch (error) { uploadRoot.textContent = error.message; uploadRoot.hidden = false; }
      };
      void initCreationUploads();
    }
    form?.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (managedFormRecoveryPending(form)) return;
      saveCreationDraft();
      if (!updateCreationSubmitEligibility({uploadReady: !creationUploads || creationUploads.ready()})) return;
      form.querySelectorAll('input[name="attachmentIds"]').forEach((input) => input.remove());
      for (const id of creationUploads?.ids() || []) {
        const input = document.createElement("input"); input.type = "hidden"; input.name = "attachmentIds"; input.value = id; form.append(input);
      }
      creationUploads?.lock(true);
      indexNavigationPending = true;
      if (indexRefreshTimer !== null) clearTimeout(indexRefreshTimer);
      updateCreationSubmitEligibility({inFlight: true});
      const progress = document.getElementById("new-session-progress");
      const creationProgress = timedProgress(progress, "Creating session");
      try {
        const response = await fetch("/sessions", {
          method: "POST", credentials: "same-origin",
          headers: {Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded"},
          body: new URLSearchParams(new FormData(form)),
        });
        const result = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(result.error || `Unable to create session (${response.status}).`);
        if (!result.receiptId || !/^\/[A-Za-z0-9][A-Za-z0-9_-]*\/$/.test(result.url)) {
          throw new Error("Unable to confirm session creation. Retry with the saved request.");
        }
        try { creationDraftStorage?.removeItem(creationDraftKey); } catch (_) {}
        creationProgress.stop();
        window.location.assign(result.url);
      } catch (failure) {
        creationProgress.fail(failure.message);
        indexNavigationPending = false;
        creationUploads?.lock(false);
        updateCreationSubmitEligibility({
          inFlight: false, uploadReady: !creationUploads || creationUploads.ready(),
        });
        void refreshIndexStatus();
      }
    });
    void refreshIndexStatus();
  }

  let models = [];
  let collaborationModes = [];
  let currentModel = "";
  let currentEffort = "";
  let currentMode = "";
  let currentThreadId = body.dataset.threadId || "";
  const conversationID = body.dataset.conversationId || body.dataset.session || "";
  const selectedMember = body.dataset.selectedMember || "";
  let threadActive = false;
  const teamSelects = Array.from(document.querySelectorAll("[data-team-select]"));

  const updateTeamDescription = (teamSelect) => {
    const description = teamSelect.closest("form")?.querySelector("[data-team-description]");
    const selected = teamSelect.selectedOptions[0];
    if (!description || !selected) return;
    const roles = selected.dataset.roles ? ` Roles: ${selected.dataset.roles}.` : "";
    const lead = selected.dataset.leadModel && selected.dataset.leadEffort ?
      ` Lead: ${selected.dataset.leadModel} / ${selected.dataset.leadEffort}.` : "";
    description.textContent = `${selected.dataset.description || ""}${roles}${lead}`;
  };

  const applyTeamLeadSettings = (teamSelect) => {
    const form = teamSelect.closest("form");
    const selected = teamSelect.selectedOptions[0];
    const modelSelect = form?.elements.model;
    const effortSelect = form?.elements.effort;
    if (!selected || !modelSelect || !effortSelect || !models.length) return;
    const model = selected.dataset.leadModel || "";
    if (Array.from(modelSelect.options).some((option) => option.value === model)) modelSelect.value = model;
    else modelSelect.selectedIndex = -1;
    populateEfforts(modelSelect, effortSelect, selected.dataset.leadEffort || "");
    modelSelect.dataset.userEdited = "false";
    effortSelect.dataset.userEdited = "false";
  };

  const restoreDraftSelect = (select, value, unavailableLabel) => {
    if (!select || typeof value !== "string") return;
    if (!Array.from(select.options).some((option) => option.value === value) && value !== "") {
      const option = document.createElement("option");
      option.value = value;
      option.textContent = unavailableLabel;
      option.dataset.savedDraftValue = "true";
      select.append(option);
    }
    if (Array.from(select.options).some((option) => option.value === value)) select.value = value;
  };

  const managedCatalogDigest = (form) => (
    typeof form?.elements?.catalogDigest?.value === "string" ? form.elements.catalogDigest.value : ""
  );
  const managedDraftPolicyForStorage = (form, policy) => {
    const recovery = managedDraftRecoveries.get(form);
    if (!recovery?.requiresAcknowledgement) return policy;
    return {...policy, ...recovery.savedPolicy};
  };
  const updateManagedCatalogRecoveryUI = (form, recovery) => {
    const notice = form.querySelector("[data-team-catalog-changed]");
    const acknowledgement = form.querySelector("[data-team-catalog-acknowledgement]");
    const checkbox = form.querySelector("[data-team-catalog-acknowledge]");
    const pending = recovery.requiresAcknowledgement;
    if (notice) notice.hidden = !pending;
    if (acknowledgement) acknowledgement.hidden = !pending;
    // A newly pending recovery must start unacknowledged.  Do not clear an
    // acknowledgement while its change handler is completing: that would
    // immediately undo the user's explicit confirmation.
    if (checkbox && pending) checkbox.checked = false;
    updateManagedFormSubmitEligibility(form);
  };
  const resetManagedDraftPolicy = (form) => {
    const team = form.elements.team;
    if (team) {
      team.querySelectorAll("[data-saved-draft-value]").forEach((option) => option.remove());
      const defaultTeam = Array.from(team.options).find((option) => option.defaultSelected) || team.options[0];
      if (defaultTeam) team.value = defaultTeam.value;
      updateTeamDescription(team);
      applyTeamLeadSettings(team);
    }
    const model = form.elements.model;
    const effort = form.elements.effort;
    if (model) {
      model.querySelectorAll("[data-saved-draft-value]").forEach((option) => option.remove());
      if (!form.elements.team && Array.from(model.options).some((option) => option.value === "")) model.value = "";
    }
    if (effort) {
      effort.querySelectorAll("[data-saved-draft-value]").forEach((option) => option.remove());
      if (!form.elements.team && Array.from(effort.options).some((option) => option.value === "")) effort.value = "";
    }
  };
  const restoreManagedCatalogRecovery = (form, draft) => {
    if (!form?.elements.team || !form?.elements.catalogDigest || !draft) return null;
    const currentDigest = managedCatalogDigest(form);
    const key = `${draft.catalogDigest}\n${currentDigest}`;
    let recovery = managedDraftRecoveries.get(form);
    if (!recovery || recovery.key !== key) {
      const decision = creationDraftCatalogRecovery(draft, currentDigest);
      recovery = {
        ...decision, key, acknowledged: false, reset: false,
        savedPolicy: {
          team: draft.team, catalogDigest: draft.catalogDigest,
          model: draft.model, effort: draft.effort,
        },
      };
      managedDraftRecoveries.set(form, recovery);
    }
    recovery.requiresAcknowledgement = recovery.catalogChanged && !recovery.acknowledged;
    if (recovery.requiresAcknowledgement && !recovery.reset) {
      resetManagedDraftPolicy(form);
      recovery.reset = true;
    }
    updateManagedCatalogRecoveryUI(form, recovery);
    return recovery;
  };
  const acknowledgeManagedCatalogRecovery = (form) => {
    const recovery = managedDraftRecoveries.get(form);
    if (!recovery?.requiresAcknowledgement) return;
    recovery.acknowledged = true;
    recovery.requiresAcknowledgement = false;
    updateManagedCatalogRecoveryUI(form, recovery);
    if (form.id === "new-session-form") persistCreationDraft?.();
    if (form.id === "plan-session-form") persistPlanSessionDraft?.();
  };

  const restoreManagedCreationDraft = (form, draft) => {
    if (!form || !draft) return;
    const catalogDigest = form.elements.catalogDigest;
    const recovery = restoreManagedCatalogRecovery(form, draft);
    if (recovery?.catalogChanged) return;
    if (catalogDigest && typeof draft.catalogDigest === "string") catalogDigest.value = draft.catalogDigest;
    restoreDraftSelect(form.elements.team, draft.team, "Saved team is unavailable");
    if (form.elements.team) updateTeamDescription(form.elements.team);
    if (!models.length) return;
    const model = form.elements.model;
    const effort = form.elements.effort;
    if (!draft.model || !draft.effort) {
      applyTeamLeadSettings(form.elements.team);
      return;
    }
    restoreDraftSelect(model, draft.model, "Saved lead model is unavailable");
    populateEfforts(model, effort, draft.effort);
    restoreDraftSelect(effort, draft.effort, "Saved lead reasoning effort is unavailable");
    const selected = form.elements.team?.selectedOptions?.[0];
    model.dataset.userEdited = String(draft.model !== (selected?.dataset.leadModel || ""));
    effort.dataset.userEdited = String(draft.effort !== (selected?.dataset.leadEffort || ""));
  };

  const restoreManagedCreationDrafts = () => {
    if (creationDraft) restoreManagedCreationDraft(document.getElementById("new-session-form"), creationDraft);
    if (planSessionDraft) restoreManagedCreationDraft(document.getElementById("plan-session-form"), planSessionDraft);
  };

  const populateEfforts = (modelSelect, effortSelect, selected = "") => {
    if (!effortSelect) return;
    const model = models.find((candidate) => candidate.model === modelSelect.value);
    const existingSettings = modelSelect.dataset.existingSettings === "true";
    effortSelect.replaceChildren();
    if (!existingSettings && !effortSelect.required) {
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
    const desired = selected || effortSelect.dataset.currentValue || model?.defaultReasoningEffort || "";
    if (Array.from(effortSelect.options).some((option) => option.value === desired)) {
      effortSelect.value = desired;
    }
    effortSelect.disabled = !model || (threadActive && existingSettings);
  };

  const pairedEffortSelect = (modelSelect) => modelSelect.id === "codex-model" ?
    document.getElementById("codex-effort") : modelSelect.closest("form")?.elements.effort;
  const applyCurrentSettings = (root = document, includeTeamForms = false) => {
    root.querySelectorAll("[data-model-select]").forEach((modelSelect) => {
      if (!includeTeamForms && modelSelect.closest("[data-direct-team-form]")) return;
      const effortSelect = pairedEffortSelect(modelSelect);
      const team = modelSelect.closest("form")?.elements.team?.selectedOptions?.[0];
      const retainedValue = modelSelect.dataset.currentValue || "";
      if (retainedValue && Array.from(modelSelect.options).some((option) => option.value === retainedValue)) {
        modelSelect.value = retainedValue;
      }
      if (team && modelSelect.dataset.userEdited !== "true") {
        if (Array.from(modelSelect.options).some((option) => option.value === team.dataset.leadModel)) {
          modelSelect.value = team.dataset.leadModel;
        } else modelSelect.selectedIndex = -1;
      } else if (!team && !retainedValue && !modelSelect.closest("[data-direct-team-form]") &&
                 currentModel && Array.from(modelSelect.options).some((option) => option.value === currentModel)) {
        modelSelect.value = currentModel;
      }
      // A plan/new-session override is a pending creation choice, not a view
      // of the source conversation. Preserve both its explicit value and its
      // intentional empty default across background settings refreshes.
      const retainedEffort = modelSelect.closest('[data-action="configure"]') ? effortSelect.dataset.currentValue : "";
      populateEfforts(modelSelect, effortSelect, team && effortSelect.dataset.userEdited !== "true" ?
        team.dataset.leadEffort : retainedEffort || effortSelectionForModelRefresh(false, effortSelect.value, currentEffort));
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
    restoreManagedCreationDrafts();
    updateCreationCLI();
  };

  const loadModels = async (root = document) => {
    const modelSelects = Array.from(root.querySelectorAll("[data-model-select]"));
    const effortSelects = Array.from(root.querySelectorAll("[data-effort-select]"));
    try {
      if (!models.length) models = await request("/api/models");
      for (const modelSelect of modelSelects) {
        const allowDefault = !modelSelect.required;
        modelSelect.replaceChildren();
        if (allowDefault) {
          const fallback = document.createElement("option");
          fallback.value = "";
          fallback.textContent = "Configured default";
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
      applyCurrentSettings(root, true);
      applyMemberDefaults();
      restoreManagedCreationDrafts();
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

  const bindModelSelects = (root = document) => {
    root.querySelectorAll("[data-model-select]").forEach((modelSelect) => {
      modelSelect.addEventListener("change", () => {
        modelSelect.dataset.userEdited = "true";
        populateEfforts(modelSelect, pairedEffortSelect(modelSelect));
      });
      pairedEffortSelect(modelSelect)?.addEventListener("change", (event) => {
        event.currentTarget.dataset.userEdited = "true";
      });
    });
  };
  bindModelSelects();
  teamSelects.forEach((teamSelect) => {
    updateTeamDescription(teamSelect);
    teamSelect.addEventListener("change", () => {
      updateTeamDescription(teamSelect);
      applyTeamLeadSettings(teamSelect);
    });
  });
  const applyMemberDefaults = () => {
    const memberDefaults = document.getElementById("team-role-defaults");
    const addMemberForm = document.querySelector('[data-direct-team-form][data-action="add"]');
    if (!memberDefaults || !addMemberForm || !models.length) return;
    const preset = memberDefaults.dataset.currentPreset || memberDefaults.dataset.defaultPreset;
    const role = addMemberForm.elements.role.value.trim();
    const defaults = Array.from(memberDefaults.querySelectorAll("[data-preset][data-role]"))
      .find((entry) => entry.dataset.preset === preset && entry.dataset.role === role);
    const modelSelect = addMemberForm.elements.model;
    const effortSelect = addMemberForm.elements.effort;
    if (defaults && Array.from(modelSelect.options).some((option) => option.value === defaults.dataset.model)) {
      modelSelect.value = defaults.dataset.model;
      populateEfforts(modelSelect, effortSelect, defaults.dataset.effort);
      return;
    }
    // A role absent from this preset has no preset settings: require an explicit
    // model choice instead of silently taking Codex's global default.
    modelSelect.selectedIndex = -1;
    populateEfforts(modelSelect, effortSelect);
  };
  document.addEventListener("change", (event) => {
    if (event.target.matches('[data-direct-team-form][data-action="add"] [name="role"]')) applyMemberDefaults();
  });
  document.querySelectorAll("[data-team-catalog-acknowledge]").forEach((checkbox) => {
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) acknowledgeManagedCatalogRecovery(checkbox.closest("form"));
    });
  });
  restoreManagedCreationDrafts();
  if (document.querySelector("[data-model-select]")) loadModels();

  document.addEventListener("click", async (event) => {
    const button = event.target.closest("[data-copy]");
    if (!button) return;
    const input = button.parentElement.querySelector("input");
    await navigator.clipboard.writeText(input.value);
    const old = button.textContent;
    button.textContent = "Copied";
    setTimeout(() => { button.textContent = old; }, 1000);
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
      // Native fragment navigation can move focus away after the key handler.
      const oldURL = location.href;
      if (oldURL !== sessionTabs[next].href) history.pushState(null, "", sessionTabs[next].href);
      dispatchEvent(new HashChangeEvent("hashchange", {oldURL, newURL: location.href}));
      sessionTabs[next].focus();
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
    id: conversationID,
    basePath: "/codex",
    ...(selectedMember ? {} : {conversationPath: `/api/sessions/${encodeURIComponent(slug)}`}),
  }) : null;
  const client = createSessionClient(slug, request, conversation);
  document.getElementById("codex-member")?.addEventListener("change", (event) => {
    const target = new URL(location.href);
    const member = event.target.value;
    if (member === "lead") target.searchParams.delete("member");
    else target.searchParams.set("member", member);
    target.hash = "codex";
    location.assign(target.href);
  });
  const directTeamRequest = async (body, method = "POST") => request(
    `/api/sessions/${encodeURIComponent(slug)}/team`, {
      method, headers: {"content-type": "application/json"}, body: JSON.stringify(body),
    },
  );
  const showTeamActionStatus = (message) => {
    const teamActionStatus = document.getElementById("team-action-status");
    if (!teamActionStatus) return;
    teamActionStatus.textContent = message;
    teamActionStatus.hidden = false;
  };
  document.addEventListener("submit", async (event) => {
    const form = event.target.closest("[data-direct-team-form]");
    if (!form || !interactive) return;
    event.preventDefault();
    const action = form.dataset.action;
    const status = form.querySelector("[role=status]");
    const controls = Array.from(form.querySelectorAll("button, input, select, textarea"));
    const value = name => form.elements[name]?.value?.trim() || "";
    const body = {action, model: value("model"), reasoningEffort: value("effort")};
    if (action === "preset") body.preset = value("preset");
    if (action === "add") body.role = value("role");
    if (action === "configure") body.address = form.dataset.address;
    form.dataset.teamSubmitting = "true";
    controls.forEach(control => { control.disabled = true; });
    if (status) { status.textContent = "Saving team…"; status.hidden = false; }
    try {
      await directTeamRequest(body);
      if (status) status.textContent = "Saved.";
      setTimeout(() => location.reload(), 250);
    } catch (error) {
      delete form.dataset.teamSubmitting;
      form.dataset.teamDirty = "true";
      if (status) status.textContent = error.message;
      controls.forEach(control => { control.disabled = false; });
    }
  });
  document.addEventListener("change", (event) => {
    const form = event.target.closest("[data-direct-team-form]");
    if (form) form.dataset.teamDirty = "true";
  });
  document.addEventListener("click", async (event) => {
    const button = event.target.closest("[data-team-thread]");
    if (!button) return;
    const panel = document.getElementById("team-transcript");
    const output = panel?.querySelector(".transcript");
    if (!panel || !output) return;
    button.disabled = true;
    output.replaceChildren();
    const loading = document.createElement("p"); loading.className = "empty"; loading.textContent = "Loading member messages…";
    output.append(loading); panel.hidden = false;
    try {
      const result = await request(`/api/sessions/${encodeURIComponent(slug)}/team-thread?member=${encodeURIComponent(button.dataset.teamThread)}`);
      output.replaceChildren();
      for (const entry of result.transcript.entries || []) {
        if (!entry.text && !entry.displayText) continue;
        const item = document.createElement("article"); item.className = "message";
        const heading = document.createElement("strong"); heading.textContent = entry.kind || "message";
        const text = document.createElement("pre"); text.textContent = entry.text || entry.displayText || "";
        const timestamp = conversationAssets.formatTranscriptTimestamp(entry);
        const time = document.createElement("time"); time.className = "message-time";
        time.textContent = timestamp.text;
        time.title = timestamp.title;
        if (timestamp.dateTime) time.dateTime = timestamp.dateTime;
        item.append(heading, text, time); output.append(item);
      }
      if (!output.childElementCount) output.append(Object.assign(document.createElement("p"), {className: "empty", textContent: "This member has no visible messages yet."}));
    } catch (error) {
      output.replaceChildren(Object.assign(document.createElement("p"), {className: "notice error", textContent: error.message}));
    } finally { button.disabled = false; }
  });
  document.addEventListener("click", (event) => {
    if (event.target.closest("[data-team-transcript-close]")) document.getElementById("team-transcript").hidden = true;
  });
  document.addEventListener("click", async (event) => {
    const button = event.target.closest("[data-team-retry]");
    if (!button || !interactive) return;
    button.disabled = true;
    showTeamActionStatus(`Retrying ${button.dataset.teamRetry}…`);
    try {
      await directTeamRequest({action: "retry", address: button.dataset.teamRetry});
      location.reload();
    } catch (error) {
      showTeamActionStatus(error.message);
      button.disabled = false;
    }
  });
  document.addEventListener("click", async (event) => {
    const button = event.target.closest("[data-team-remove]");
    if (!button || !interactive) return;
    if (!confirm(`Archive ${button.dataset.teamRemove}'s Codex thread and remove it from the active team?`)) return;
    button.disabled = true;
    try { await directTeamRequest({action: "remove", address: button.dataset.teamRemove}, "DELETE"); location.reload(); }
    catch (error) { showTeamActionStatus(error.message); button.disabled = false; }
  });
  const autoArchiveStatus = document.getElementById("auto-archive-status");
  const autoArchiveHold = document.getElementById("auto-archive-hold");
  const autoArchiveValues = document.getElementById("auto-archive-values");
  const autoArchiveDetails = document.getElementById("auto-archive-details");
  let lastAutoArchive = null, autoArchiveRead = 0, autoArchiveSaving = false;
  let autoArchiveLoading = null, autoArchiveReadAt = -Infinity;
  const showAutoArchiveDetails = (diagnostics) => {
    autoArchiveDetails.hidden = !diagnostics.length;
    autoArchiveDetails.querySelector("pre").textContent = diagnostics.join("\n\n");
  };
  const renderAutoArchive = (state) => {
    lastAutoArchive = state;
    renderArchiveFailure();
    const presentation = autoArchivePresentation(state);
    const fields = document.createElement("dl"); fields.className = "auto-archive-fields";
    for (const [label, value] of presentation.fields) {
      const term = document.createElement("dt"); term.textContent = label;
      const description = document.createElement("dd"); description.textContent = value;
      fields.append(term, description);
    }
    autoArchiveValues.replaceChildren(fields); autoArchiveValues.hidden = false;
    if (presentation.blockers.length) {
      const heading = document.createElement("h4"); heading.textContent = "Blockers at last check";
      const list = document.createElement("ul");
      for (const text of presentation.blockers) {
        const item = document.createElement("li"); item.textContent = text; list.append(item);
      }
      autoArchiveValues.append(heading, list);
    }
    showAutoArchiveDetails(presentation.diagnostics);
    autoArchiveStatus.textContent = ""; autoArchiveStatus.hidden = true;
    if (autoArchiveHold) { autoArchiveHold.checked = state.hold; autoArchiveHold.disabled = autoArchiveSaving; }
  };
  const failAutoArchive = (error, message) => {
    autoArchiveStatus.textContent = message;
    autoArchiveStatus.hidden = false;
    showAutoArchiveDetails([...(lastAutoArchive ? autoArchivePresentation(lastAutoArchive).diagnostics : []), error.message]);
  };
  const loadAutoArchive = (force = false) => {
    if (autoArchiveLoading) return autoArchiveLoading;
    if (autoArchiveSaving || (!force && Date.now() - autoArchiveReadAt < 30_000)) return Promise.resolve();
    autoArchiveReadAt = Date.now();
    const read = ++autoArchiveRead;
    autoArchiveLoading = (async () => {
      try {
        const state = await client.autoArchive();
        if (read === autoArchiveRead) renderAutoArchive(state);
      } catch (error) {
        if (read === autoArchiveRead) failAutoArchive(error, lastAutoArchive ?
          "Could not refresh archival settings. Showing the last available settings." : "Could not load archival settings.");
      } finally { autoArchiveLoading = null; }
    })();
    return autoArchiveLoading;
  };
  if (autoArchiveStatus) {
    if (document.getElementById("settings")?.classList.contains("active")) void loadAutoArchive();
    document.addEventListener("session-section-change", event => {
      if (event.detail === "settings") void loadAutoArchive();
    });
    autoArchiveHold?.addEventListener("change", async () => {
      const held = autoArchiveHold.checked;
      let saveError = null;
      autoArchiveSaving = true; ++autoArchiveRead;
      autoArchiveHold.disabled = true;
      try {
        const saved = await client.autoArchiveHold(held, lifecycleTargetId);
        renderAutoArchive({...lastAutoArchive, ...saved});
      } catch (error) {
        autoArchiveHold.checked = lastAutoArchive?.hold || false;
        saveError = error;
      } finally {
        autoArchiveSaving = false; autoArchiveHold.disabled = false;
      }
      await autoArchiveLoading;
      await loadAutoArchive(true);
      if (saveError) failAutoArchive(saveError, "Could not confirm the Keep open change. The checkbox shows the last confirmed setting.");
    });
  }
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
  const archiveFailure = document.getElementById("archive-last-failure");
  function renderArchiveFailure() {
    if (!archiveFailure) return;
    const failure = archiveFailurePresentation(lastLifecycleOperation, lastAutoArchive, body.dataset.rootThreadId);
    archiveFailure.hidden = !failure;
    archiveFailure.querySelector("span").textContent = failure ?
      `Last automatic attempt failed (${new Date(failure.attemptedAt).toLocaleString()}): ${failure.message}` : "";
  }
  const showLifecycle = (operation = lastLifecycleOperation, pendingKind = "") => {
    if (!lifecycleStatus || !lifecycleTitle || !lifecycleDetail || !lifecycleRetry) return;
    lastLifecycleOperation = operation;
    renderArchiveFailure();
    if (!lifecycleObservedAt && operation.startedAt) {
      const startedAt = Date.parse(operation.startedAt);
      if (Number.isFinite(startedAt)) lifecycleObservedAt = startedAt;
    }
    const observedAt = operation.state === "paused" ? Date.parse(operation.updatedAt) : lifecycleObservedAt;
    const elapsed = Number.isFinite(observedAt) && observedAt ? Math.max(0, Date.now() - observedAt) : 0;
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

  const pageReads = createReadScope();
  let pageLeaving = false, pauseTiming = () => {}, resumeTiming = () => {};
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
        onComparisonChange(open) { comparisonOpen = open; updateSidebar(); },
      });
      if (pageReads.paused) repositoryReview.suspend();
    }).catch((error) => {
      if (pageReads.paused) return;
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
  let lastClustersHTML = "";
  let lastTeamHTML = "";
  let releasingCluster = false;
  const refreshSessionDetails = async () => {
    if (detailsRunning || pageReads.paused || document.hidden || !artifactList) return;
    if (detailsTimer !== null) clearTimeout(detailsTimer);
    detailsRunning = true;
    const read = pageReads.begin(15_000);
    const warning = document.getElementById("session-details-warning");
    try {
      const payload = await client.details({signal: read.signal});
      if (!read.isCurrent()) return;
      if (!releasingCluster && typeof payload.clustersHTML === "string" && payload.clustersHTML !== lastClustersHTML) {
        const clusters = document.getElementById("clusters");
        const selected = Array.from(clusters.querySelectorAll("[data-cluster]")).map(card => [card.dataset.cluster, card.querySelector('[data-cluster-service-tab][aria-selected="true"]')?.dataset.clusterServiceTab]);
        clusters.innerHTML = payload.clustersHTML;
        lastClustersHTML = payload.clustersHTML;
        for (const [kind, service] of selected) {
          const card = Array.from(clusters.querySelectorAll("[data-cluster]")).find(card => card.dataset.cluster === kind);
          Array.from(card?.querySelectorAll("[data-cluster-service-tab]") || []).find(tab => tab.dataset.clusterServiceTab === service)?.click();
        }
      }
      if (payload.repositoriesHTML !== lastRepositoriesHTML) {
        const repositories = document.getElementById("repositories");
        if (repositoryReviewLoading) await repositoryReviewLoading;
        if (!read.isCurrent()) return;
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
      if (typeof payload.teamHTML === "string" && payload.teamHTML !== lastTeamHTML) {
        const teamStatus = document.getElementById("team-member-status");
        const editingTeam = teamStatus?.querySelector("[data-direct-team-form]:focus-within, [data-direct-team-form][data-team-dirty], [data-direct-team-form][data-team-submitting]");
        if (teamStatus && !editingTeam) {
          const removedOpen = teamStatus.querySelector(".removed-members")?.open || false;
          teamStatus.innerHTML = payload.teamHTML;
          lastTeamHTML = payload.teamHTML;
          const removed = teamStatus.querySelector(".removed-members");
          if (removed) removed.open = removedOpen;
          bindModelSelects(teamStatus);
          void loadModels(teamStatus);
        }
      }
      if (Array.isArray(payload.readyMembers)) {
        const selector = document.getElementById("codex-member");
        if (selector) {
          const addresses = ["lead", ...payload.readyMembers];
          if (selectedMember && !addresses.includes(selectedMember)) {
            const target = new URL(location.href);
            target.searchParams.delete("member");
            target.hash = "codex";
            location.replace(target.href);
            return;
          }
          if (JSON.stringify([...selector.options].map((option) => option.value)) !== JSON.stringify(addresses)) {
            selector.replaceChildren(...addresses.map((address) => new Option(address, address)));
            selector.value = selectedMember || "lead";
          }
        }
      }
      for (const [section, label, count] of [
        ["repositories", "Repositories", payload.repositoryCount], ["artifacts", "Artifacts", payload.artifactCount],
        ["clusters", "Clusters", payload.clusterCount],
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
      if (!read.isCurrent()) return;
      warning.textContent = `Session details could not be refreshed: ${error.message}`;
      warning.hidden = false;
    } finally {
      read.finish(); detailsRunning = false;
      if (!pageReads.paused) detailsTimer = setTimeout(refreshSessionDetails, 15_000);
    }
  };
  const suspendPageReads = (leaving = false) => {
    pageLeaving ||= leaving;
    pageReads.pause(); clearTimeout(detailsTimer); detailsTimer = null;
    repositoryReview?.suspend(); pauseTiming();
  };
  const resumePageReads = () => {
    if (document.hidden || pageLeaving) return;
    const wasPaused = pageReads.paused;
    pageReads.resume();
    if (wasPaused) { resumeTiming(); repositoryReview?.resume(); }
    void refreshSessionDetails();
  };
  // Firefox can reject old-document fetches before pagehide. Stop these reads
  // as soon as ordinary document navigation starts.
  document.addEventListener("click", event => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const anchor = event.target.closest?.("a[href]");
    if (!anchor || anchor.hasAttribute("download") || anchor.target && anchor.target !== "_self") return;
    const destination = new URL(anchor.href, location.href);
    if (["http:", "https:"].includes(destination.protocol) &&
        (destination.origin !== location.origin || destination.pathname !== location.pathname || destination.search !== location.search)) suspendPageReads(true);
  });
  addEventListener("beforeunload", () => suspendPageReads(true));
  addEventListener("pagehide", () => suspendPageReads(true));
  addEventListener("pageshow", () => { pageLeaving = false; resumePageReads(); });
  sessionTabs.forEach(tab => tab.addEventListener("click", refreshSessionDetails));
  addEventListener("focus", resumePageReads);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) suspendPageReads(); else resumePageReads();
  });
  if (document.hidden) suspendPageReads(); else void refreshSessionDetails();

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
  // Worker diagnostics are historical evidence; they never replace a live retry.
  let archiveRefreshRunning = false, archiveRefreshAt = -Infinity;
  const refreshPendingArchive = async () => {
    if (pendingLifecycle !== "archive" || document.hidden || archiveRefreshRunning ||
        Date.now() - archiveRefreshAt < 30_000 || lastLifecycleOperation.state === "complete") return;
    archiveRefreshRunning = true;
    archiveRefreshAt = Date.now();
    const observedOperation = lastLifecycleOperation;
    try {
      await Promise.all([loadAutoArchive(), (async () => {
        const operation = await client.operation();
        if (lastLifecycleOperation === observedOperation && observedOperation.state !== "running") {
          if (operation.state === "running") monitorLifecycle("archive", operation);
          else showLifecycle(operation, operation.state === "complete" ? "" : pendingLifecycle);
          if (operation.state === "complete") location.reload();
        }
      })()]);
    } catch (_error) {
      // Keep the last confirmed attempt and its timestamp when a read fails.
    } finally { archiveRefreshRunning = false; }
  };
  if (pendingLifecycle === "archive") {
    let timer = setInterval(() => void refreshPendingArchive(), 30_000);
    document.addEventListener("visibilitychange", () => void refreshPendingArchive());
    window.addEventListener("pageshow", () => {
      if (timer === null) timer = setInterval(() => void refreshPendingArchive(), 30_000);
      void refreshPendingArchive();
    });
    window.addEventListener("pagehide", () => { clearInterval(timer); timer = null; });
  }
  client.operation().then((operation) => {
    if (!pendingLifecycle && !lifecycleOperationBelongsToPage(operation.kind, operation)) return;
    if (operation.state === "running") {
      monitorLifecycle(operation.kind || pendingLifecycle, operation);
    } else if (operation.state !== "complete" && (operation.state !== "idle" || pendingLifecycle)) {
      lifecycleKind = operation.kind || pendingLifecycle;
      showLifecycle(operation, pendingLifecycle);
    }
    void refreshPendingArchive();
  }).catch((error) => {
    if (pendingLifecycle) failLifecycle(pendingLifecycle, error.message);
  });
  const transcript = document.getElementById("transcript");
  const pending = document.getElementById("pending");
  const status = document.getElementById("codex-status");
  if (!transcript) return;

  const composerView = interactive ? createComposerView(
    document.getElementById("plan-actions"), document.getElementById("message-form"), pending,
  ) : null;
  let sync = null;
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
  const requestInputDrafts = new Map(), promptStatuses = new Map(), respondingPrompts = new Set(), answeredOffers = new Set();
  let currentPrompts = [];
  const codexWork = document.getElementById("codex-work");
  const codexWorkElapsed = document.getElementById("codex-work-elapsed");
  const codexWorkLabel = document.getElementById("codex-work-label");
  const codexWorkCounts = document.getElementById("codex-work-counts");
  const durationSummary = document.getElementById("codex-duration");
  const codexTab = document.getElementById("session-tab-codex");
  const codexWaitingIndicator = document.getElementById("codex-waiting-indicator");
  let activitySnapshot = null;
  let activityAvailable = false;
  const timingClock = createTimingClock();
  let activityRead = null;

  let sendAttemptStorage = null;
  try { sendAttemptStorage = globalThis.sessionStorage; } catch (_error) {}
  let requestInputDraftStorage = null;
  try { requestInputDraftStorage = globalThis.sessionStorage; } catch (_error) {}

  const actionablePrompts = () => currentPrompts.filter(entry => !answeredOffers.has(entry.token));
  const updateCodexWaitingIndicator = () => {
    const state = activitySnapshot?.currentState;
    const blockingRequest = actionablePrompts().some((entry) => entry.isBlocking);
    const timing = timingClock.view();
    const waiting = interactive && activityAvailable && !timing.stale && Boolean(activitySnapshot?.stateSinceMs) &&
      ((state === "idle" && !threadActive) || (state === "waiting" && blockingRequest));
    codexTab?.classList.toggle("waiting", waiting);
    if (codexWaitingIndicator) codexWaitingIndicator.hidden = !waiting;
    const label = waiting ? "Codex: waiting for instructions" : "Codex";
    codexTab?.setAttribute("aria-label", label);
    codexTab?.setAttribute("title", label);
  };
  const updateCodexWork = (active = threadActive) => {
    if (!codexWork || !codexWorkElapsed) return;
    const timing = timingClock.view();
    if (!activitySnapshot) {
      codexWork.hidden = !active;
      codexWorkLabel.textContent = "Codex is working";
      codexWorkCounts.textContent = "";
      codexWorkElapsed.textContent = "";
      durationSummary.textContent = timing.unavailable ? "Timing unavailable" : "Loading timing…";
      updateCodexWaitingIndicator();
      return;
    }
    const view = activityPresentation(activitySnapshot, interactive ? timing.elapsed : 0);
    const stale = interactive && timing.unavailable;
    const waiting = ["waiting", "idle"].includes(view.state) && activitySnapshot.stateSinceMs;
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
    updateCodexWaitingIndicator();
  };
  const refreshActivity = () => {
    if (!client.activity || document.hidden || pageReads.paused) return Promise.resolve();
    if (!interactive && activitySnapshot) return Promise.resolve();
    if (activityRead) return activityRead;
    const read = pageReads.begin();
    activityRead = client.activity({signal: read.signal}).then(snapshot => {
      if (!read.isCurrent()) return;
      activitySnapshot = snapshot; activityAvailable = true; timingClock.received();
    }).catch(() => { if (read.isCurrent()) { activityAvailable = false; timingClock.failed(); } }).finally(() => {
      read.finish(); activityRead = null;
      if (!pageReads.paused) updateCodexWork();
    });
    return activityRead;
  };
  pauseTiming = () => { timingClock.pause(); updateCodexWaitingIndicator(); };
  resumeTiming = () => { timingClock.resume(); updateCodexWaitingIndicator(); void refreshActivity(); };
  if (pageReads.paused) pauseTiming();
  setInterval(() => { if (!document.hidden && !pageReads.paused) updateCodexWork(); }, 1000);
  setInterval(() => { void refreshActivity(); }, 5000);
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
    if (selectedMember) { composerView.setPlanVisible(false); return; }
    const generation = ++planRenderGeneration;
    const plan = currentCompletedPlan(payload);
    const eligibleMode = ["plan", "default"].includes(payload.collaborationMode);
    if (!plan || payload.status === "active" || !eligibleMode || planImplementationInFlight) {
      composerView.setPlanVisible(false);
      return;
    }
    // Hide an obsolete decision immediately while the new content is hashed.
    if (panel.dataset.planTurnId !== plan.turnId || panel.planText !== plan.text) {
      composerView.setPlanVisible(false);
    }
    const digest = await sha256Hex(plan.text);
    if (generation !== planRenderGeneration) return;
    const pendingImplementation = pendingPlanImplementation(pendingMessages.values(), plan.turnId, digest);
    if (payload.collaborationMode === "default" && !pendingImplementation) {
      composerView.setPlanVisible(false);
      return;
    }
    panel.dataset.planTurnId = plan.turnId;
    panel.dataset.planSha256 = digest;
    panel.planText = plan.text;
    const sameButton = document.getElementById("plan-implement-same");
    if (sameButton) sameButton.textContent = pendingImplementation ? "Check request" : "Implement here";
    composerView.setPlanVisible(
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
    if (sendButton) { sendButton.textContent = messageActionLabel(threadActive); sendButton.disabled = !composerUploadReady; }
    if (queueButton) queueButton.disabled = !composerUploadReady;
    if (queueButton) queueButton.hidden = !threadActive;
    composerView?.setInterruptEnabled(threadActive);
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

  const renderThread = async (payload, isCurrent = () => true) => {
    if (!payload.threadId) throw new Error("Codex returned no thread");
    if (currentThreadId && currentThreadId !== payload.threadId) {
      requestInputDrafts.clear(); promptStatuses.clear(); answeredOffers.clear();
    }
    currentThreadId = payload.threadId;
    loadMessageReceipts();
    recoverPlanAttempts(payload.latestTurnId);
    const follow = !transcriptInitialized || transcriptViews.get(transcriptFilter).follow;
    const previousTop = transcript.clientHeight ? transcript.scrollTop : transcriptViews.get(transcriptFilter).scrollTop;
    const entries = payload.entries || [];
    const nextSignature = JSON.stringify(entries);
    const transcriptChanged = !transcriptInitialized || nextSignature !== transcriptSignature;
    const observedMessages = await markTranscriptMessagesObserved(pendingMessages, entries);
    if (!isCurrent()) return;
    for (const observed of observedMessages) {
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

  const showPromptStatus = (entry, message) => {
    const key = promptDraftKey(entry);
    if (message) promptStatuses.set(key, {entry, message}); else promptStatuses.delete(key);
    for (const box of document.querySelectorAll(".approval")) if (box.dataset.promptKey === key) {
      const notice = box.querySelector(".prompt-response-status");
      if (notice) { notice.textContent = message; notice.hidden = !message; }
    }
  };
  const recoverPrompt = async entry => {
    if (pageReads.paused) return null;
    sync?.retry();
    const read = pageReads.begin(), deadline = Date.now() + 9000;
    try {
      do {
        const thread = await client.thread({signal: read.signal});
        const pending = await client.pending({signal: read.signal});
        if (!read.isCurrent() || read.signal.aborted || thread.threadId !== entry.threadId ||
            thread.latestTurnId !== (entry.turnId || entry.params?.turnId)) return null;
        renderPendingEntries(pending);
        const restored = pending.find(candidate => promptIdentity(candidate) === promptIdentity(entry));
        if (restored || pending.some(candidate => candidate.kind === "userInput") || thread.status !== "active") return restored;
        await new Promise(resolve => {
          const done = () => { clearTimeout(timer); read.signal.removeEventListener("abort", done); resolve(); };
          const timer = setTimeout(done, 400); read.signal.addEventListener("abort", done, {once: true});
        });
      } while (Date.now() < deadline && !read.signal.aborted);
      return null;
    } finally { read.finish(); scheduleRefresh(0); }
  };
  const respond = async (entry, payload, container) => {
    const key = promptDraftKey(entry);
    if (respondingPrompts.has(key)) return;
    if (!entry.token) { showPromptStatus(entry, "Reload this page to update the question controls."); return; }
    respondingPrompts.add(key);
    const activeElement = document.activeElement;
    const controls = Array.from(container.querySelectorAll("button, input, select, textarea"));
    controls.forEach(control => { control.disabled = true; });
    showPromptStatus(entry, entry.kind === "userInput" ? "Submitting answers…" : "Submitting response…");
    try {
      const accepted = await respondWithRecovery(entry, payload,
        (offer, answer) => client.respond(offer.id, {...answer, token: offer.token}), recoverPrompt,
        () => showPromptStatus(entry, entry.kind === "userInput" ? "Reconnecting to Codex. Your answers are saved." : "Reconnecting to Codex…"));
      answeredOffers.add(entry.token); answeredOffers.add(accepted.token);
      if (answeredOffers.size > 100) answeredOffers.delete(answeredOffers.values().next().value);
      requestInputDrafts.delete(key); promptStatuses.delete(key);
      deleteRequestInputDraft(requestInputDraftStorage, slug, entry.threadId, key);
      pendingSignature = null; renderPendingEntries(currentPrompts); scheduleRefresh(0);
    } catch (error) {
      const message = entry.kind !== "userInput" ? (error.notSent ? "Your response was not sent. Refresh the request and try again." : "Delivery could not be confirmed. Check the conversation before responding again.") :
        error.code === "invalid_response" || error.code === "reload_required" ? error.message :
        error.notSent ? "Your answers were not sent. They are saved here; refresh the question and try again." :
          "Delivery could not be confirmed. Your answers are saved. Check the conversation before submitting again.";
      showPromptStatus(entry, message);
    } finally {
      respondingPrompts.delete(key);
      pendingSignature = null; renderPendingEntries(currentPrompts);
      controls.forEach(control => { control.disabled = false; });
      if (controls.includes(activeElement)) composerView.restoreFocus(activeElement, activeElement.closest(".question-approval"));
    }
  };

  const renderApproval = (entry) => {
    const box = document.createElement("article");
    box.className = "approval";
    const draftKey = promptDraftKey(entry);
    box.dataset.promptKey = draftKey;
    const responseStatus = document.createElement("p");
    responseStatus.className = "notice warning prompt-response-status";
    responseStatus.setAttribute("role", "status");
    responseStatus.textContent = promptStatuses.get(draftKey)?.message || "";
    responseStatus.hidden = !responseStatus.textContent;
    box.append(responseStatus);
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
      let wizardState = requestInputDrafts.get(draftKey);
      if (!wizardState || wizardState.drafts.length !== questions.length) {
        const saved = loadRequestInputDraft(
          requestInputDraftStorage, slug, currentThreadId, draftKey, questions,
        );
        wizardState = saved || {
          drafts: questions.map(() => ({})), page: 0,
        };
        requestInputDrafts.set(draftKey, wizardState);
      }
      if (!wizardState.snooze || wizardState.snooze.token !== entry.token) {
        wizardState.snooze = createPromptSnooze(entry.token, () => {
          if (!entry.token) throw new Error("Reload this page to update the question controls.");
          return client.snooze(entry.id, entry.token);
        });
      }
      const snoozeState = wizardState.snooze;
      const drafts = wizardState.drafts;
      let page = Math.min(wizardState.page, questions.length - 1);
      box.classList.add("question-approval");
      box.dataset.requestId = entry.id;
      const heading = document.createElement("div");
      heading.className = "question-heading";
      heading.append(title);
      box.prepend(heading);
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
      back.name = "question-back";
      back.className = "quiet";
      back.textContent = "Back";
      const next = document.createElement("button");
      next.type = "button";
      next.name = "question-next";
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
          } : {note};
        } else {
          drafts[page] = {kind: "freeform", note};
        }
        wizardState.page = page;
        storeRequestInputDraft(
          requestInputDraftStorage, slug, currentThreadId, draftKey, questions, wizardState,
        );
      };
      const renderAutoResolution = () => {
        const dueAt = Number(entry.autoResolutionAtMs || 0);
        autoResolution.textContent = autoResolutionLabel(
          Date.now(), Number(entry.autoResolutionVisibleAtMs || 0), dueAt,
          snoozeState.paused || Boolean(entry.autoResolveSnoozed),
        );
        autoResolution.hidden = !autoResolution.textContent;
      };
      snoozeState.onChange = () => {
        // A pending operation belongs to its captured offer, even when the
        // same logical question retains its draft on a replacement offer.
        if (wizardState.snooze !== snoozeState) return;
        renderAutoResolution();
        if (snoozeState.failed) {
          showPromptStatus(entry, "Automatic resolution could not be paused. Your answers are saved; refresh the question to try again.");
          sync?.retry(); scheduleRefresh(0);
        }
      };
      const snoozeAutoResolution = async () => {
        if (entry.isBlocking || entry.recovering || entry.autoResolveSnoozed) return;
        await snoozeState.pause();
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
          requestInputDraftStorage, slug, currentThreadId, draftKey, questions, wizardState,
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
            requestInputDraftStorage, slug, currentThreadId, draftKey, questions, wizardState,
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
        await respond(entry, {answers}, form);
      });
      renderQuestion();
      renderAutoResolution();
      if (!entry.isBlocking && !snoozeState.paused && !entry.autoResolveSnoozed) {
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
        button.addEventListener("click", () => respond(entry, {decision}, actions));
        actions.append(button);
      }
      box.append(actions);
    }
    if (respondingPrompts.has(draftKey)) box.querySelectorAll("button, input, select, textarea").forEach(control => { control.disabled = true; });
    if (entry.recovering) {
      box.querySelectorAll("button").forEach(control => { control.disabled = true; });
      responseStatus.hidden = false;
      responseStatus.textContent = promptStatuses.get(draftKey)?.message || "Refreshing this question. Your answers are saved.";
    }
    if (promptStatuses.has(draftKey) && !respondingPrompts.has(draftKey)) {
      const refresh = document.createElement("button"); refresh.type = "button"; refresh.className = "quiet"; refresh.textContent = entry.kind === "userInput" ? "Refresh question" : "Refresh request";
      refresh.addEventListener("click", async () => {
        refresh.disabled = true;
        try {
          const restored = await recoverPrompt(entry);
          const state = restored && requestInputDrafts.get(promptDraftKey(restored))?.snooze;
          if (restored && !restored.isBlocking && state?.token === restored.token && await state.pause(true)) showPromptStatus(restored, "");
        } catch (_) {} finally { refresh.disabled = false; }
      });
      box.append(refresh);
      if (entry.recovering) {
        const dismiss = document.createElement("button"); dismiss.type = "button"; dismiss.className = "quiet"; dismiss.textContent = "Hide saved question";
        dismiss.addEventListener("click", () => { promptStatuses.delete(draftKey); pendingSignature = null; renderPendingEntries(currentPrompts); });
        box.append(dismiss);
      }
    }
    return box;
  };

  let pendingSignature = null;
  const renderPendingEntries = (entries) => {
    if (!interactive) return;
    currentPrompts = entries;
    entries = actionablePrompts();
    updateCodexWaitingIndicator();
    const keys = new Set(entries.map(promptDraftKey));
    // An empty transient snapshot cannot prove that saved answers are obsolete.
    // Keep recovery controls until the question returns or the user hides them.
    for (const [key, status] of promptStatuses) if (!keys.has(key) && status.entry.threadId === currentThreadId &&
        !entries.some(entry => entry.kind === status.entry.kind)) entries = [...entries, {...status.entry, recovering: true}];
    const signature = JSON.stringify([entries, [...promptStatuses].map(([key, status]) => [key, status.message]), [...respondingPrompts]]);
    if (signature === pendingSignature) return;
    pendingSignature = signature;
    composerView.replacePending(entries.map(renderApproval));
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

  const refreshQueue = () => sync.refresh();

  function scheduleRefresh(delay = 200) {
    sync?.scheduleRefresh(delay);
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

  const recoverPlanAttempts = (latestTurnId) => {
    for (const attempt of pendingMessages.values()) {
      if (["accepted", "observed"].includes(attempt.state) || inFlightMessageIDs.has(attempt.id)) continue;
      const request = planRecoveryRequest(attempt, latestTurnId);
      if (!request) continue;
      inFlightMessageIDs.add(attempt.id);
      void client.implementPlan(request).then((result) => {
        if (result.clientUserMessageId !== attempt.id) throw new Error("receipt identity changed");
        if (result.retired === true) {
          if (deleteSendAttempt(sendAttemptStorage, slug, currentThreadId, attempt.id)) pendingMessages.delete(attempt.id);
        } else if (result.turnId) {
          acceptMessageReceipt(attempt, result);
        }
      }).catch(() => {
        // Keep the original retry record visible until recovery is available.
      }).finally(() => {
        inFlightMessageIDs.delete(attempt.id);
        renderMessageReceipts();
        acknowledgeTranscriptMessages(transcriptEntries);
      });
    }
  };

  const form = document.getElementById("message-form");
  if (form && interactive) {
    const textarea = form.elements.message;
    composerUploads = conversationAssets.mountUploads(document.getElementById("message-uploads"), {
      basePath: `/uploads/s-${encodeURIComponent(conversationID)}`, dropTarget: form,
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
    queueButton?.addEventListener("click", () => submitMessage(true));
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
    composerView.setPlanVisible(false, true);
  });
  document.getElementById("plan-implement-same")?.addEventListener("click", async (event) => {
    const button = event.currentTarget;
    const message = "Implement the plan.";
    const context = planActionContext(planActions.dataset.planTurnId, planActions.dataset.planSha256);
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
        planContextVersion: 2,
        planTurnId: planActions.dataset.planTurnId,
        planSha256: planActions.dataset.planSha256,
        clientUserMessageId: id,
      });
      acceptMessageReceipt(attempt, receipt);
      currentMode = "default";
      followTranscript();
      dismissedPlanIdentity = implementingIdentity;
      ++planRenderGeneration;
      composerView.setPlanVisible(false);
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
  let planSessionDraftStorage = null;
  let planSessionCreationInFlight = false;
  try { planSessionDraftStorage = globalThis.sessionStorage; } catch (_) {}
  const planSessionIsManaged = () => Boolean(planSessionForm?.elements.team);
  const updatePlanSessionSubmitEligibility = () => updateManagedFormSubmitEligibility(
    planSessionForm, {uploadReady: true, inFlight: planSessionCreationInFlight},
  );
  const savePlanSessionDraft = () => {
    if (!planSessionForm || !planSessionSnapshot) return;
    const policy = managedDraftPolicyForStorage(planSessionForm, {
      model: planSessionForm.elements.model?.value || "",
      effort: planSessionForm.elements.effort?.value || "",
      team: planSessionForm.elements.team?.value || "",
      catalogDigest: planSessionForm.elements.catalogDigest?.value || "",
    });
    const draft = normalizeCreationDraft({
      name: planSessionForm.elements.name.value,
      date: planSessionForm.elements.creationDate.value,
      ...policy,
    }, "plan");
    if (draft) {
      planSessionDraft = draft;
      storeCreationDraft(planSessionDraftStorage, planSessionDraftKey(slug, planSessionSnapshot), draft, "plan");
    }
  };
  persistPlanSessionDraft = savePlanSessionDraft;
  document.getElementById("plan-implement-new")?.addEventListener("click", () => {
    planSessionSnapshot = {
      planTurnId: planActions.dataset.planTurnId,
      planSha256: planActions.dataset.planSha256,
      planText: planActions.planText,
    };
    if (!planSessionIsManaged()) {
      planSessionSnapshot.model = currentModel;
      planSessionSnapshot.reasoningEffort = currentEffort;
    }
    planSessionDraft = loadCreationDraft(
      planSessionDraftStorage, planSessionDraftKey(slug, planSessionSnapshot), "plan",
    );
    managedDraftRecoveries.delete(planSessionForm);
    if (planSessionDraft) {
      planSessionForm.elements.name.value = planSessionDraft.name;
      planSessionForm.elements.creationDate.value = planSessionDraft.date;
      restoreManagedCreationDrafts();
    }
    updatePlanSessionSubmitEligibility();
    planSessionDialog.showModal();
  });
  planSessionDialog?.querySelector("[data-dialog-close]")?.addEventListener("click", () => planSessionDialog.close());
  planSessionForm?.addEventListener("input", savePlanSessionDraft);
  planSessionForm?.addEventListener("change", savePlanSessionDraft);
  planSessionForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (managedFormRecoveryPending(planSessionForm)) return;
    savePlanSessionDraft();
    if (!updatePlanSessionSubmitEligibility()) return;
    const controls = Array.from(planSessionForm.querySelectorAll("button, input, select"));
    controls.forEach((control) => { control.disabled = true; });
    planSessionCreationInFlight = true;
    updatePlanSessionSubmitEligibility();
    const progress = timedProgress(
      document.getElementById("plan-session-progress"), "Creating session",
    );
    try {
      const managed = planSessionIsManaged();
      const settings = planSessionCreationSettings(managed, planSessionSnapshot, {
        team: planSessionForm.elements.team?.value || "",
        catalogDigest: planSessionForm.elements.catalogDigest?.value || "",
        model: planSessionForm.elements.model?.value || "",
        effort: planSessionForm.elements.effort?.value || "",
      });
      const result = await client.implementPlan({
        action: "new",
        ...planSessionSnapshot,
        name: planSessionForm.elements.name.value,
        creationDate: planSessionForm.elements.creationDate.value,
        ...settings,
      });
      try { planSessionDraftStorage?.removeItem(planSessionDraftKey(slug, planSessionSnapshot)); } catch (_) {}
      progress.stop();
      location.assign(result.url);
    } catch (error) {
      progress.fail(error.message);
      controls.forEach((control) => { control.disabled = false; });
      planSessionCreationInFlight = false;
      updatePlanSessionSubmitEligibility();
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

  document.getElementById("clusters")?.addEventListener("click", async (event) => {
    const release = event.target.closest("[data-release-cluster]");
    if (release) {
      if (releasingCluster || !confirm("Stop this development cluster and remove its temporary state?")) return;
      releasingCluster = true;
      release.disabled = true;
      release.textContent = "Releasing…";
      try { await client.releaseCluster(release.dataset.releaseCluster); location.reload(); }
      catch (error) { alert(error.message); release.disabled = false; release.textContent = "Release cluster"; }
      finally { releasingCluster = false; }
      return;
    }
    const tab = event.target.closest("[data-cluster-service-tab]");
    if (tab) {
      const card = tab.closest("[data-cluster]");
      card.querySelectorAll("[data-cluster-service-tab]").forEach(candidate => {
        const selected = candidate === tab;
        candidate.classList.toggle("active", selected);
        candidate.setAttribute("aria-selected", selected ? "true" : "false");
        candidate.tabIndex = selected ? 0 : -1;
      });
      card.querySelectorAll("[data-cluster-service-panel]").forEach(panel => panel.classList.toggle("active", panel.dataset.clusterServicePanel === tab.dataset.clusterServiceTab));
    }
    const reveal = event.target.closest("[data-reveal-secret]");
    if (reveal) {
      const input = reveal.parentElement.querySelector("[data-secret-field]");
      const visible = input.type === "password";
      input.type = visible ? "text" : "password";
      reveal.textContent = visible ? "Hide" : "Reveal";
    }
  });

  sync = conversationAssets.createConversationSync({
    live: interactive, eventsPath: interactive ? client.eventsPath() : null,
    read: signal => Promise.all([
      client.thread({signal}),
      interactive ? client.pending({signal}) : Promise.resolve([]),
      interactive ? client.reconcileQueue({signal}).then(() => client.queue({signal})) : Promise.resolve([]),
    ]),
    apply: async ([thread, pendingEntries, queuedEntries], {isCurrent}) => {
      await renderThread(thread, isCurrent);
      if (!isCurrent()) return;
      renderPendingEntries(pendingEntries);
      if (interactive) renderQueue(queuedEntries);
      void refreshActivity();
    },
    onStateChange: state => conversationAssets.renderConnectionStatus(
      document.getElementById("conversation-connection"), state, () => sync.retry(),
    ),
  });
  if (interactive) loadCollaborationModes();
})();
