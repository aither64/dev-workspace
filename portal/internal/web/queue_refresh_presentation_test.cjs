"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const {messageReceiptLabel} = require("./static/app.js");
const source = fs.readFileSync(__dirname + "/static/app.js", "utf8");
const tick = () => new Promise(setImmediate);

class Element {
  constructor(tagName = "div") {
    this.tagName = tagName; this.hidden = false; this.disabled = false;
    this.children = []; this.dataset = {}; this.handlers = new Map(); this.text = "";
  }
  set textContent(value) { this.text = value; this.children = []; }
  get textContent() { return this.text + this.children.map(child => child.textContent).join(" "); }
  get childElementCount() { return this.children.length; }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.text = ""; this.children = children; }
  setAttribute() {}
  querySelector(selector) {
    return this.children.find(child => child.tagName === selector) ||
      this.children.map(child => child.querySelector(selector)).find(Boolean) || null;
  }
  addEventListener(name, callback) { this.handlers.set(name, callback); }
  async click() { if (!this.disabled) await this.handlers.get("click")?.(); await tick(); }
}

// Mount the actual portal renderer and refresh lane, as in read_cycle_browser_test.
// Only transport, DOM and time are synthetic; the shared notice policy is shipped code.
function mount(assets) {
  let now = 100_000, sequence = 0, automaticError = null;
  const timers = new Map(), listeners = new Map(), notices = [], reads = [], alerts = [];
  const ids = new Map(["queue-panel", "queue-list", "queue-start", "queue-status", "queue-retry",
    "message-receipts", "message-submission-status"].map(id => [id, new Element()]));
  for (const id of ["queue-panel", "queue-status", "queue-retry", "message-receipts", "message-submission-status"]) ids.get(id).hidden = true;
  ids.get("queue-status").append(new Element("span"));
  const page = {
    hidden: false, getElementById: id => ids.get(id), createElement: tag => new Element(tag),
    addEventListener(name, callback) {
      if (!listeners.has(name)) listeners.set(name, new Set());
      listeners.get(name).add(callback);
    },
    removeEventListener: (name, callback) => listeners.get(name)?.delete(callback),
  };
  const later = (run, delay) => { const id = ++sequence; timers.set(id, {run, at: now + delay}); return id; };
  const clear = id => timers.delete(id);
  const context = {
    Error, AbortController, DOMException, Date: class extends Date { static now() { return now; } },
    document: page, setTimeout: later, clearTimeout: clear, refreshPolicy: assets.refreshPolicy,
    interactive: true, executionHeld: false, threadActive: false, liveSettingsSaving: false,
    conversationID: "example", currentThreadId: "thread-1", messageReceiptLabel,
    slug: "example", sendAttemptStorage: {}, loadSendAttempts: () => [],
    pendingMessages: new Map(), inFlightMessageIDs: new Set(), messageDispatchInFlight: false, planImplementationInFlight: false, messageDispatcher: null,
    alert: message => alerts.push(message), scheduleRefresh() {}, followTranscript() {},
    pendingRetryTimer: null, pendingDueTimer: null, historyRetryTimer: null, historyRead: null, pendingRerun: false,
    pendingNotice: {cancel() {}}, detailsNotice: {cancel() {}}, historyNotice: {cancel() {}}, refreshPending() {},
    conversationAssets: {...assets, createRefreshNotice(options) {
      const notice = assets.createRefreshNotice({...options, document: page, window: page,
        now: () => now, setTimeout: later, clearTimeout: clear});
      notices.push(notice); return notice;
    }},
    client: {
      reconcileQueue: async () => {},
      queue({signal}) {
        if (automaticError) return Promise.reject(automaticError);
        let resolve, reject;
        const promise = new Promise((a, b) => { resolve = a; reject = b; });
        signal.addEventListener("abort", () => reject(signal.reason), {once: true});
        reads.push({resolve, reject, signal}); return promise;
      },
      deleteQueued: async () => { throw Error("Queue removal failed"); },
      startQueue: async () => { throw Error("Queue start failed"); },
    },
  };
  const section = (start, end) => {
    const first = source.indexOf(start), last = source.indexOf(end, first);
    assert(first > 0 && last > first, `shipped section ${start}`);
    return source.slice(first, last);
  };
  const api = vm.runInNewContext(
    section("  const createReadScope =", "  const createTimingClock =") +
    "\nconst pageReads = createReadScope();\n" +
    section("  const renderMessageReceipts =", "  loadMessageReceipts();") +
    section("  const queuePanel =", "  function scheduleRefresh(") +
    section("  const submissionStatus =", "  const form = document.getElementById(\"message-form\")") +
    section("  queueStart?.addEventListener", "  const planActions =") +
    "\n({refreshQueue, renderMessageReceipts, showSubmissionFailure, submissionIdentity, pageReads})", context);
  const advance = async milliseconds => {
    const target = now + milliseconds;
    while (true) {
      const next = [...timers].filter(([, timer]) => timer.at <= target).sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      now = next[1].at; timers.delete(next[0]); next[1].run(); await tick();
    }
    now = target; await tick();
  };
  return {
    ...api, reads, alerts, ids, context, advance,
    failAutomatically(error) { automaticError = error; },
    async settle(entries) { reads.at(-1).resolve(entries); await tick(); },
    async reject(error = Error("Queue unavailable")) { reads.at(-1).reject(error); await tick(); },
    async begin(force = true, manual = false) { void api.refreshQueue(force, manual); await tick(); },
    pause() {
      page.hidden = true; listeners.get("visibilitychange")?.forEach(callback => callback());
      api.pageReads.pause(); context.pauseConversationReads();
    },
    async resume() {
      page.hidden = false; listeners.get("visibilitychange")?.forEach(callback => callback());
      api.pageReads.resume(); context.resumeConversationReads(); await tick();
    },
    destroy() { api.pageReads.pause(); notices.forEach(notice => notice.destroy()); timers.clear(); },
  };
}

module.exports = async assets => {
  const h = mount(assets), {initialLoadingMs, manualLoadingMs, warningMs, queueMs, queueDeadlineMs} = assets.refreshPolicy;
  const status = h.ids.get("queue-status"), retry = h.ids.get("queue-retry"), panel = h.ids.get("queue-panel"), list = h.ids.get("queue-list");
  const rows = [{id: "first", text: "Keep this confirmed queued prompt"}];
  try {
    await h.begin();
    await h.advance(initialLoadingMs + 1);
    assert(status.hidden, "a delayed initial read must remain silent");
    assert(panel.hidden, "an unconfirmed queue must remain unassertive");
    assert.equal(list.children.length, 0);
    await h.settle(rows);
    assert(!panel.hidden); assert.match(list.textContent, /confirmed queued prompt/);
    await h.advance(queueMs);
    await h.begin(false);
    const confirmedRow = list.children[0];
    await h.advance(initialLoadingMs + 1);
    assert(status.hidden, "regular background reads must remain silent");
    assert.equal(list.children[0], confirmedRow, "a delayed read must retain the displayed row");
    await h.reject();
    h.failAutomatically(Error("Still unavailable"));
    await h.advance(warningMs - 1);
    assert(status.hidden, "warnings must keep the shared grace measured from failure");
    assert.equal(list.children[0], confirmedRow);
    await h.advance(1);
    assert(!status.hidden); assert(!retry.hidden);
    assert.match(status.textContent, /last result|out of date/i);
    assert.equal(list.children[0], confirmedRow);

    h.failAutomatically(null);
    await retry.click();
    assert(!status.hidden, "Retry must retain the warning until confirmation");
    assert(retry.disabled, "an active retry must prevent repeated clicks");
    const readCount = h.reads.length;
    await retry.click(); assert.equal(h.reads.length, readCount);
    await h.advance(manualLoadingMs + 1);
    assert(!status.hidden); assert.doesNotMatch(status.textContent, /checking/i);
    await h.settle(rows);
    assert(status.hidden); assert(retry.hidden);

    // Silent observation cannot hide or resolve actual pending delivery/mutation failures.
    for (const [id, state] of [["sending", "sending"], ["unknown", "unknown"]]) {
      h.context.pendingMessages.set(id, {id, state, message: `${state} message`});
    }
    h.renderMessageReceipts();
    const receipts = h.ids.get("message-receipts");
    assert(!receipts.hidden); assert.match(receipts.textContent, /Sending/); assert.match(receipts.textContent, /Outcome unknown/);
    await list.children[0].children[1].click();
    assert.deepEqual(h.alerts, ["Queue removal failed"]);
    assert(!list.children[0].children[1].disabled);
    h.showSubmissionFailure(Error("Delivery rejected"), h.submissionIdentity("thread-1", "send-failed"));
    await h.begin(); await h.settle(rows);
    assert(!h.ids.get("message-submission-status").hidden);
    assert.match(h.ids.get("message-submission-status").textContent, /Delivery rejected/);
    const starting = h.ids.get("queue-start").click(); await tick();
    assert.match(h.ids.get("message-submission-status").textContent, /Queue start failed/);
    await h.settle(rows); await starting;
    assert(!receipts.hidden); assert.match(receipts.textContent, /Outcome unknown/);
    assert(!h.ids.get("message-submission-status").hidden, "successful observation cannot clear a mutation failure");
  } finally { h.destroy(); }

  const first = mount(assets), initialStatus = first.ids.get("queue-status");
  try {
    await first.begin();
    // A real request deadline is a failed read, not permission to show checking copy.
    await first.advance(queueDeadlineMs);
    first.failAutomatically(Error("Still unavailable"));
    await first.advance(warningMs - 1); assert(initialStatus.hidden);
    await first.advance(1); assert(!initialStatus.hidden);
    assert.match(initialStatus.textContent, /could not be loaded|could not load|unable to load/i);
    assert.doesNotMatch(initialStatus.textContent, /last result|previous|out of date/i);
    assert(first.ids.get("queue-panel").hidden);

    first.pause(); await first.advance(warningMs * 2);
    await first.resume();
    assert(initialStatus.hidden, "restoration starts a fresh visible warning grace");
    await first.advance(warningMs - 1); assert(initialStatus.hidden);
    await first.advance(1); assert(!initialStatus.hidden);
    first.failAutomatically(null);
    await first.ids.get("queue-retry").click(); await first.settle([]);
    assert(initialStatus.hidden); assert(first.ids.get("queue-panel").hidden);
  } finally { first.destroy(); }

  for (const kind of ["access", "manual"]) {
    const failed = mount(assets);
    try {
      await failed.begin(true, kind === "manual");
      const error = Object.assign(Error("Queue unavailable"), kind === "access" ? {status: 403} : {});
      await failed.reject(error);
      assert(!failed.ids.get("queue-status").hidden, `${kind} failures must remain immediate`);
      assert(!failed.ids.get("queue-retry").hidden);
      assert.doesNotMatch(failed.ids.get("queue-status").textContent, /last result|previous|out of date/i);
    } finally { failed.destroy(); }
  }
};
